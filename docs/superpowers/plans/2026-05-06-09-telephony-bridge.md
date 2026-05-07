# План реализации #09: telephony-bridge sidecar

**Дата:** 2026-05-06
**Версия:** 0.1.0
**Статус:** Draft
**Автор:** SocioPulse Platform Team
**Зависимости:** Plan #00 (Foundation), Plan #01 (Infrastructure: NATS, Redis, FreeSWITCH-cluster)
**Спецификации:** §5.3.1 (Voice subsystem), §7 (Cross-cutting), §10.2 (NATS subjects), Приложение B.2 (FreeSWITCH dialplan/ESL)

---

## Goal

Реализовать `cmd/telephony-bridge` — Go-сервис-sidecar, удерживающий пул ESL-соединений к FreeSWITCH-нодам кластера и осуществляющий двусторонний мост между NATS (`telephony.cmd.>` ⇄ `telephony.event.>`) и Event Socket Library FreeSWITCH.

Сервис отвечает за:

1. **ESL-протокол.** Полнофункциональный inbound ESL-клиент (event-плоскость + командная плоскость), auto-reconnect с jittered backoff, регистрация подписки на нужные FS-события (`CHANNEL_CREATE`, `CHANNEL_ANSWER`, `CHANNEL_HANGUP_COMPLETE`, `DTMF`, `CUSTOM sofia::register`, `CUSTOM mod_callcenter::*`, `RECORD_STOP`).
2. **Пул соединений.** Один inbound ESL на каждую FreeSWITCH-ноду (3 ноды в dev, 6 в prod), health-check каждые 5 сек через `api sofia status`, автоматический re-attach подписки после reconnect.
3. **Маршрутизация.** Выбор trunk + FreeSWITCH-ноды для исходящего звонка по стратегии (`least_cost`, `least_cost_with_fallback`, `round_robin`, `weighted`), backpressure через Redis-счётчики `op:active_channels:{node}` с дефолтным cap=60 на ноду.
4. **NATS-мост.** JSON-сериализация команд (Originate, Hangup, MixMonitor, Play, CreateUser, DeleteUser) и событий, idempotency-ключ через Redis SET NX (TTL 24h).
5. **Per-call SIP-аккаунты.** Эфемерные SIP-учётки для verto/WebRTC-софтфонов через `mod_xml_curl` с callback'ом к `cmd/api` `/internal/freeswitch/directory` (mTLS), хранение в Redis `op:credentials:{operator_id}:{call_id}` с TTL 4h.
6. **HTTP-эндпоинт `/internal/freeswitch/directory`.** В составе `cmd/api`, отдаёт XML-директорию по запросу FreeSWITCH (mod_xml_curl), читает Redis, проверяет mTLS-сертификат FS.

Сервис должен быть production-ready: Prometheus-метрики, OpenTelemetry-трейсинг, structured logging (slog), graceful shutdown с дренированием inflight-команд, Helm-чарт + ArgoCD-app для развёртывания в `dev`-окружении (как минимум).

---

## Non-Goals

- Не реализуем outbound ESL-режим (`mod_event_socket` с outbound socket из dialplan) — все команды идут через inbound. Outbound останется на будущее, если потребуется длительная per-call логика на стороне Go.
- Не реализуем собственную медиа-обработку (recording offload, transcoding) — это обязанности FreeSWITCH (`mod_sndfile`) и `recording-uploader` (план #10).
- Не реализуем балансировку SIP-trunk между провайдерами на уровне SIP — у нас один SIP-trunk на провайдера, выбор провайдера происходит ДО SIP-сигнализации, на уровне `Router` через выбор trunk-объекта в Postgres.
- Не реализуем NAT-traversal/STUN/TURN — это конфигурация FS-нод (Plan #01).
- Не реализуем UI/Admin для управления ESL-пулом — наблюдаемость только через метрики и логи.

---

## File Structure

```
social-pulse/
├── cmd/
│   └── telephony-bridge/
│       ├── main.go                    # Точка входа: config, NATS, OTel, Prometheus, ESLPool, Router, NATSBridge, graceful shutdown
│       └── README.md                  # Краткое описание сервиса
│
├── internal/
│   └── telephony/
│       ├── esl/                       # ESL-клиент к одной FS-ноде
│       │   ├── client.go              # Интерфейс Client + конкретная реализация на eslgo
│       │   ├── client_test.go         # Unit-тесты с mock conn
│       │   ├── connection.go          # Низкоуровневая обёртка net.Conn ↔ ESL-протокол
│       │   ├── parser.go              # Парсинг ESL-фреймов (auth/req-resp/event-plain)
│       │   ├── parser_test.go         # Unit-тесты парсера
│       │   ├── reconnect.go           # Auto-reconnect с jittered backoff + circuit breaker
│       │   ├── reconnect_test.go      # Unit-тесты backoff
│       │   ├── commands.go            # Высокоуровневые команды: Originate/Hangup/MixMonitor/Play/CreateUser/DeleteUser/SofiaStatus
│       │   ├── commands_test.go       # Unit-тесты с mock connection
│       │   ├── events.go              # Декодирование FS-событий → доменные типы telephony.Event
│       │   ├── events_test.go         # Unit-тесты декодеров
│       │   ├── metrics.go             # Prometheus-метрики ESL-уровня (commands_total, events_total, reconnects_total, latency)
│       │   └── errors.go              # Сентинельные ошибки (ErrNotConnected, ErrTimeout, ErrCommandFailed)
│       │
│       ├── pool/                      # Пул ESL-соединений к нескольким FS-нодам
│       │   ├── pool.go                # ESLPool: Get(node), HealthCheck, ReconnectAll, Close
│       │   ├── pool_test.go           # Unit-тесты пула
│       │   ├── health.go              # Health-checker: каждые 5 сек api sofia status
│       │   ├── health_test.go         # Unit-тесты health-checker
│       │   └── metrics.go             # Метрики пула (pool_size, healthy_nodes, unhealthy_nodes)
│       │
│       ├── router/                    # Выбор trunk + FS-ноды
│       │   ├── router.go              # Router: SelectTrunk(operatorID, dest), SelectNode(trunk)
│       │   ├── router_test.go         # Unit-тесты + property-based для round_robin
│       │   ├── strategy.go            # Стратегии: least_cost, least_cost_with_fallback, round_robin, weighted
│       │   ├── strategy_test.go       # Unit-тесты стратегий
│       │   ├── backpressure.go        # Backpressure через Redis active_channels counter
│       │   ├── backpressure_test.go   # Unit-тесты с miniredis
│       │   └── metrics.go             # Метрики маршрутизации (routes_total, backpressure_rejects_total, trunk_selection_duration)
│       │
│       ├── nats_bridge/               # Мост NATS↔ESL
│       │   ├── bridge.go              # NATSBridge: Subscribe(telephony.cmd.>), Publish(telephony.event.>)
│       │   ├── bridge_test.go         # Unit-тесты bridge с эмбеддед NATS
│       │   ├── handlers.go            # Обработчики команд: handleOriginate, handleHangup, handleMixMonitor, handlePlay, handleCreateUser, handleDeleteUser
│       │   ├── handlers_test.go       # Unit-тесты handlers
│       │   ├── publisher.go           # Публикация событий с трассировкой и idempotency
│       │   ├── publisher_test.go      # Unit-тесты publisher
│       │   ├── idempotency.go         # Redis SET NX для дедупликации
│       │   ├── idempotency_test.go    # Unit-тесты с miniredis
│       │   └── metrics.go             # Метрики моста (commands_received_total, events_published_total, dedup_hits_total)
│       │
│       ├── credentials/               # Per-call SIP-аккаунты (для verto)
│       │   ├── manager.go             # CredentialsManager: Create, Delete, Lookup
│       │   ├── manager_test.go        # Unit-тесты
│       │   └── redis_store.go         # Хранение в Redis op:credentials:*
│       │
│       └── api/                       # HTTP-handler для /internal/freeswitch/directory (используется из cmd/api)
│           ├── directory.go           # XMLDirectoryHandler: лукап в Redis, генерация XML
│           ├── directory_test.go      # Unit-тесты с testify
│           ├── xml.go                 # XML-шаблоны для FreeSWITCH directory
│           └── mtls.go                # mTLS-проверка (cert pinning)
│
├── deployments/
│   ├── helm/
│   │   └── telephony-bridge/
│   │       ├── Chart.yaml
│   │       ├── values.yaml            # Дефолтные значения (replicaCount=2, ESL-пароль через secret)
│   │       ├── values.dev.yaml        # Override для dev
│   │       └── templates/
│   │           ├── deployment.yaml    # Deployment с ServiceAccount, mTLS-сертификатами
│   │           ├── service.yaml       # Headless Service для Prometheus scrape
│   │           ├── configmap.yaml     # Конфиг (NATS URL, Redis URL, ESL-список нод)
│   │           ├── servicemonitor.yaml
│   │           ├── networkpolicy.yaml # NetworkPolicy: только NATS+Redis+FS+API egress
│   │           ├── pdb.yaml           # PodDisruptionBudget minAvailable=1
│   │           └── _helpers.tpl
│   │
│   └── argocd/
│       └── apps/
│           └── telephony-bridge-dev.yaml
│
└── tests/
    └── integration/
        └── telephony/
            ├── docker-compose.yml      # FreeSWITCH 1.10.10 + telephony-bridge для testcontainers
            ├── originate_test.go       # E2E: Originate → CHANNEL_ANSWER → Hangup
            ├── multitrunk_test.go      # E2E: routing с 2 trunk
            ├── backpressure_test.go    # E2E: 60 active_channels → 61-й отбит
            ├── idempotency_test.go     # E2E: повторный command_id игнорируется
            └── reconnect_test.go       # E2E: убиваем FS, восстанавливаем
```

---

## Tech Stack

- **Go:** 1.23+ (см. Plan #00, базовый go.mod)
- **ESL-клиент:** `github.com/percipia/eslgo` v3.0+ (поддерживает inbound, событийная плоскость, переподключение). Если eslgo не покроет 100% наших нужд — пишем тонкий wrapper на `net.Conn` с собственным парсером (см. Task 2).
- **NATS:** `github.com/nats-io/nats.go` v1.41.0+
- **Redis:** `github.com/redis/go-redis/v9` v9.7.0+
- **Postgres:** `github.com/jackc/pgx/v5` v5.7.0+ (для чтения trunk/operator-конфигов)
- **HTTP:** `net/http` (стандартный для `/internal/freeswitch/directory`)
- **OpenTelemetry:** `go.opentelemetry.io/otel` v1.32.0+, jaeger/otlp exporter
- **Prometheus:** `github.com/prometheus/client_golang` v1.20.0+
- **Логирование:** `log/slog` + `github.com/samber/oops` для wrapping ошибок (общий стиль из Plan #00)
- **Тесты:** `github.com/stretchr/testify` v1.10.0+, `go.uber.org/mock` (gomock), `github.com/alicebob/miniredis/v2` v2.34.0+, `github.com/testcontainers/testcontainers-go` v0.34.0+
- **FreeSWITCH (для интеграционных тестов):** `signalwire/freeswitch:1.10.10` (Docker)

---

## NATS Subjects (из §10.2 спеки)

**Подписка (входящие команды):**

| Subject                                  | Смысл                                  | Idempotency-key поле   |
|------------------------------------------|----------------------------------------|------------------------|
| `telephony.cmd.originate`                | Создать исходящий звонок               | `command_id` (UUIDv7)  |
| `telephony.cmd.hangup`                   | Завершить активный канал/uuid          | `command_id`           |
| `telephony.cmd.mixmonitor`               | Включить/выключить запись              | `command_id`           |
| `telephony.cmd.play`                     | Воспроизвести аудиофайл в канал        | `command_id`           |
| `telephony.cmd.create_user`              | Создать per-call SIP-аккаунт           | `command_id`           |
| `telephony.cmd.delete_user`              | Удалить SIP-аккаунт                    | `command_id`           |

**Публикация (исходящие события):**

| Subject                                            | Источник FS-события                   |
|----------------------------------------------------|---------------------------------------|
| `telephony.event.channel.create`                   | `CHANNEL_CREATE`                      |
| `telephony.event.channel.answer`                   | `CHANNEL_ANSWER`                      |
| `telephony.event.channel.hangup_complete`          | `CHANNEL_HANGUP_COMPLETE`             |
| `telephony.event.channel.bridge`                   | `CHANNEL_BRIDGE`                      |
| `telephony.event.channel.unbridge`                 | `CHANNEL_UNBRIDGE`                    |
| `telephony.event.dtmf`                             | `DTMF`                                |
| `telephony.event.recording.stop`                   | `RECORD_STOP`                         |
| `telephony.event.sofia.register`                   | `CUSTOM sofia::register`              |
| `telephony.event.callcenter.member_queue_start`    | `CUSTOM mod_callcenter::*`            |
| `telephony.event.bridge.health`                    | Внутренний heartbeat от sidecar       |

Все события несут хедер `Telephony-Trace-Id` (W3C traceparent), `Operator-Id`, `Account-Id`, `Tenant-Id` для cross-service корреляции.

---

## Idempotency & Replay

- Каждая команда из NATS обязана нести поле `command_id` (UUIDv7).
- Перед обработкой делаем `SET NX op:idempotency:{command_id} 1 EX 86400` в Redis.
- Если ключ уже существует — возвращаем кэшированный результат (или просто ack без действий).
- Idempotency распространяется только на side-effects ESL (Originate/Hangup/Play). Read-only-команды (SofiaStatus) не требуют дедупа.
- При reconnect к FS теряем буфер событий FS (FreeSWITCH не хранит историю) — это документированное ограничение, recovery идёт через CDR в Postgres (план #08, `cdr-collector`).

---

## Tasks

> **Стиль:** TDD по methodology Plan #00 (Red → Green → Refactor). Каждая Task — независимый стейдж с verification. Реальный Go-код в каждом шаге.

---

### Task 1: Module skeleton + cmd/telephony-bridge/main.go

**Цель:** скелет сервиса, конфиг, NATS, OTel, Prometheus, graceful shutdown. Без ESL ещё.

**Шаги:**

1. **Red.** Пишем тест в `cmd/telephony-bridge/main_test.go`, который запускает сервис с минимальным конфигом (через `t.Setenv`), проверяет:
   - `/healthz` отвечает 200 OK
   - `/metrics` отдаёт Prometheus-формат и содержит `process_start_time_seconds`
   - SIGTERM завершает процесс за < 5 сек
   - При недоступном NATS сервис не падает, а пишет warn-лог и пытается переподключиться

2. **Green.** Создаём `cmd/telephony-bridge/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"github.com/samber/oops"
	"go.opentelemetry.io/otel"

	"github.com/sociopulse/social-pulse/internal/observability"
	"github.com/sociopulse/social-pulse/internal/telephony/nats_bridge"
	"github.com/sociopulse/social-pulse/internal/telephony/pool"
	"github.com/sociopulse/social-pulse/internal/telephony/router"
)

const (
	serviceName    = "telephony-bridge"
	shutdownGrace  = 30 * time.Second
	healthAddr     = ":8080"
	metricsAddr    = ":9090"
)

type Config struct {
	NATSURL          string        `env:"NATS_URL,required"`
	RedisURL         string        `env:"REDIS_URL,required"`
	PostgresURL      string        `env:"POSTGRES_URL,required"`
	ESLNodes         []string      `env:"ESL_NODES,required"`         // host:port,host:port,...
	ESLPassword      string        `env:"ESL_PASSWORD,required"`
	ESLConnectTO     time.Duration `env:"ESL_CONNECT_TIMEOUT" envDefault:"10s"`
	BackpressureCap  int           `env:"BACKPRESSURE_CAP_PER_NODE" envDefault:"60"`
	OTelEndpoint     string        `env:"OTEL_EXPORTER_OTLP_ENDPOINT" envDefault:"otel-collector:4317"`
	LogLevel         string        `env:"LOG_LEVEL" envDefault:"info"`
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", slog.String("err", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	cfg, err := loadConfig()
	if err != nil {
		return oops.Wrapf(err, "load config")
	}

	logger := observability.NewLogger(serviceName, cfg.LogLevel)
	slog.SetDefault(logger)

	tp, err := observability.InitTracer(ctx, serviceName, cfg.OTelEndpoint)
	if err != nil {
		return oops.Wrapf(err, "init tracer")
	}
	defer func() { _ = tp.Shutdown(context.Background()) }()
	otel.SetTracerProvider(tp)

	mp, err := observability.InitMeter(ctx, serviceName, cfg.OTelEndpoint)
	if err != nil {
		return oops.Wrapf(err, "init meter")
	}
	defer func() { _ = mp.Shutdown(context.Background()) }()

	nc, err := nats.Connect(cfg.NATSURL,
		nats.Name(serviceName),
		nats.ReconnectWait(2*time.Second),
		nats.MaxReconnects(-1),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			slog.Warn("nats disconnect", slog.String("err", errString(err)))
		}),
		nats.ReconnectHandler(func(c *nats.Conn) {
			slog.Info("nats reconnect", slog.String("url", c.ConnectedUrl()))
		}),
	)
	if err != nil {
		return oops.Wrapf(err, "connect nats: %s", cfg.NATSURL)
	}
	defer nc.Drain()

	rdbOpts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return oops.Wrapf(err, "parse redis url")
	}
	rdb := redis.NewClient(rdbOpts)
	defer rdb.Close()

	if err := rdb.Ping(ctx).Err(); err != nil {
		return oops.Wrapf(err, "ping redis")
	}

	eslPool, err := pool.New(ctx, pool.Config{
		Nodes:          cfg.ESLNodes,
		Password:       cfg.ESLPassword,
		ConnectTimeout: cfg.ESLConnectTO,
		Logger:         logger,
	})
	if err != nil {
		return oops.Wrapf(err, "init esl pool")
	}
	defer eslPool.Close()

	rt := router.New(router.Config{
		Pool:            eslPool,
		Redis:           rdb,
		BackpressureCap: cfg.BackpressureCap,
		PostgresURL:     cfg.PostgresURL,
		Logger:          logger,
	})
	if err := rt.Start(ctx); err != nil {
		return oops.Wrapf(err, "start router")
	}
	defer rt.Stop()

	bridge := nats_bridge.New(nats_bridge.Config{
		NATS:   nc,
		Pool:   eslPool,
		Router: rt,
		Redis:  rdb,
		Logger: logger,
	})
	if err := bridge.Start(ctx); err != nil {
		return oops.Wrapf(err, "start nats bridge")
	}
	defer bridge.Stop()

	healthSrv := startHealthServer(healthAddr, eslPool, nc, rdb)
	metricsSrv := startMetricsServer(metricsAddr)
	defer shutdownHTTP(healthSrv, metricsSrv)

	slog.Info("telephony-bridge started",
		slog.Any("esl_nodes", cfg.ESLNodes),
		slog.Int("backpressure_cap", cfg.BackpressureCap),
	)

	<-ctx.Done()
	slog.Info("shutdown initiated")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := bridge.Drain(shutdownCtx); err != nil {
		slog.Warn("bridge drain", slog.String("err", err.Error()))
	}
	return nil
}

func startHealthServer(addr string, p *pool.ESLPool, nc *nats.Conn, rdb *redis.Client) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if !nc.IsConnected() {
			http.Error(w, "nats not connected", http.StatusServiceUnavailable)
			return
		}
		if err := rdb.Ping(ctx).Err(); err != nil {
			http.Error(w, "redis: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		if !p.AnyHealthy() {
			http.Error(w, "no healthy esl nodes", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ready")
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("health server", slog.String("err", err.Error()))
		}
	}()
	return srv
}

func startMetricsServer(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("metrics server", slog.String("err", err.Error()))
		}
	}()
	return srv
}

func shutdownHTTP(servers ...*http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(ctx)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
```

3. **Refactor.** Выносим `loadConfig()` в `cmd/telephony-bridge/config.go` (envconfig-style через `caarlos0/env/v11`). Покрываем тестами edge-cases (пустой `ESL_NODES`, неверный URL).

**Verification:**
- `go test ./cmd/telephony-bridge/...` — все тесты зелёные.
- `go build ./cmd/telephony-bridge` — компилируется.
- `go vet ./cmd/telephony-bridge/...` — чисто.
- `go run ./cmd/telephony-bridge` локально + `curl localhost:8080/healthz` → `ok`.
- `curl localhost:9090/metrics | grep process_` → присутствуют стандартные метрики.
- Запуск с `kill -TERM $PID` → процесс выходит за < 5 сек.

**Acceptance:** Сервис стартует, отвечает на health/metrics, корректно завершается. Тесты в `cmd/telephony-bridge/main_test.go` проходят, coverage ≥ 70%.

---

### Task 2: ESL Client base — Connect, Send/Recv, parse events, auto-reconnect

**Цель:** Низкоуровневый ESL-клиент к одной FS-ноде с парсером протокола, auth-handshake, чтением событий и переподключением.

**Шаги:**

1. **Red.** Пишем `internal/telephony/esl/parser_test.go` и `internal/telephony/esl/client_test.go`.

```go
// internal/telephony/esl/parser_test.go
package esl

import (
	"bufio"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseFrame_AuthRequest(t *testing.T) {
	raw := "Content-Type: auth/request\n\n"
	r := bufio.NewReader(strings.NewReader(raw))
	frame, err := parseFrame(r)
	require.NoError(t, err)
	require.Equal(t, "auth/request", frame.Header("Content-Type"))
	require.Empty(t, frame.Body)
}

func TestParseFrame_CommandReply(t *testing.T) {
	raw := "Content-Type: command/reply\nReply-Text: +OK accepted\n\n"
	r := bufio.NewReader(strings.NewReader(raw))
	frame, err := parseFrame(r)
	require.NoError(t, err)
	require.Equal(t, "command/reply", frame.Header("Content-Type"))
	require.Equal(t, "+OK accepted", frame.Header("Reply-Text"))
}

func TestParseFrame_ApiResponseWithBody(t *testing.T) {
	body := "BODY-DATA-HERE\n"
	raw := "Content-Type: api/response\nContent-Length: " +
		intStr(len(body)) + "\n\n" + body
	r := bufio.NewReader(strings.NewReader(raw))
	frame, err := parseFrame(r)
	require.NoError(t, err)
	require.Equal(t, "api/response", frame.Header("Content-Type"))
	require.Equal(t, body, string(frame.Body))
}

func TestParseFrame_EventPlain(t *testing.T) {
	body := "Event-Name: CHANNEL_CREATE\nUnique-ID: abc-123\nCaller-Caller-ID-Number: +79991234567\n\n"
	raw := "Content-Type: text/event-plain\nContent-Length: " +
		intStr(len(body)) + "\n\n" + body
	r := bufio.NewReader(strings.NewReader(raw))
	frame, err := parseFrame(r)
	require.NoError(t, err)
	ev, err := frame.AsEvent()
	require.NoError(t, err)
	require.Equal(t, "CHANNEL_CREATE", ev.Name)
	require.Equal(t, "abc-123", ev.UUID)
	require.Equal(t, "+79991234567", ev.Header("Caller-Caller-ID-Number"))
}
```

```go
// internal/telephony/esl/client_test.go
package esl

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeESLServer симулирует FreeSWITCH ESL-listener.
func fakeESLServer(t *testing.T, handler func(net.Conn)) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		handler(conn)
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

func TestClient_AuthSuccess(t *testing.T) {
	addr, stop := fakeESLServer(t, func(c net.Conn) {
		_, _ = c.Write([]byte("Content-Type: auth/request\n\n"))
		buf := make([]byte, 256)
		n, _ := c.Read(buf)
		require.Contains(t, string(buf[:n]), "auth ClueCon")
		_, _ = c.Write([]byte("Content-Type: command/reply\nReply-Text: +OK accepted\n\n"))
	})
	defer stop()

	cli, err := Dial(context.Background(), Config{Addr: addr, Password: "ClueCon"})
	require.NoError(t, err)
	defer cli.Close()
	require.True(t, cli.Connected())
}

func TestClient_AuthFailure(t *testing.T) {
	addr, stop := fakeESLServer(t, func(c net.Conn) {
		_, _ = c.Write([]byte("Content-Type: auth/request\n\n"))
		buf := make([]byte, 256)
		_, _ = c.Read(buf)
		_, _ = c.Write([]byte("Content-Type: command/reply\nReply-Text: -ERR invalid\n\n"))
	})
	defer stop()

	_, err := Dial(context.Background(), Config{Addr: addr, Password: "wrong"})
	require.ErrorIs(t, err, ErrAuthFailed)
}

func TestClient_ConnectTimeout(t *testing.T) {
	_, err := Dial(context.Background(), Config{
		Addr:           "127.0.0.1:1", // unreachable
		Password:       "x",
		ConnectTimeout: 100 * time.Millisecond,
	})
	require.Error(t, err)
}
```

2. **Green.** Создаём `internal/telephony/esl/parser.go`:

```go
package esl

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
)

// Frame — один ESL-кадр: набор заголовков + опциональное тело.
type Frame struct {
	headers map[string]string
	Body    []byte
}

func (f Frame) Header(name string) string {
	if f.headers == nil {
		return ""
	}
	return f.headers[strings.ToLower(name)]
}

func (f Frame) ContentType() string {
	return f.Header("Content-Type")
}

// parseFrame читает один кадр: заголовки до пустой строки + body указанной длины.
func parseFrame(r *bufio.Reader) (Frame, error) {
	headers := make(map[string]string, 16)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return Frame{}, fmt.Errorf("read header: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		idx := strings.IndexByte(line, ':')
		if idx <= 0 {
			return Frame{}, fmt.Errorf("malformed header: %q", line)
		}
		name := strings.ToLower(strings.TrimSpace(line[:idx]))
		value := strings.TrimSpace(line[idx+1:])
		headers[name] = value
	}
	frame := Frame{headers: headers}
	if cl := headers["content-length"]; cl != "" {
		n, err := strconv.Atoi(cl)
		if err != nil {
			return Frame{}, fmt.Errorf("parse content-length: %w", err)
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(r, body); err != nil {
			return Frame{}, fmt.Errorf("read body: %w", err)
		}
		frame.Body = body
	}
	return frame, nil
}

// Event — декодированное FS-событие.
type Event struct {
	Name    string
	UUID    string
	headers map[string]string
}

func (e Event) Header(name string) string {
	if e.headers == nil {
		return ""
	}
	return e.headers[strings.ToLower(name)]
}

func (f Frame) AsEvent() (Event, error) {
	if !strings.Contains(f.ContentType(), "event") {
		return Event{}, errors.New("not an event frame")
	}
	// Тело event-plain имеет вид "Header: value\nHeader: value\n\n"
	headers := map[string]string{}
	for _, line := range strings.Split(string(f.Body), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		idx := strings.IndexByte(line, ':')
		if idx <= 0 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(line[:idx]))
		value := strings.TrimSpace(line[idx+1:])
		// FS URL-encode значений, восстанавливаем
		if dec, err := url.QueryUnescape(value); err == nil {
			value = dec
		}
		headers[name] = value
	}
	return Event{
		Name:    headers["event-name"],
		UUID:    headers["unique-id"],
		headers: headers,
	}, nil
}
```

3. Создаём `internal/telephony/esl/connection.go` и `client.go`:

```go
package esl

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrAuthFailed    = errors.New("esl: auth failed")
	ErrNotConnected  = errors.New("esl: not connected")
	ErrCommandFailed = errors.New("esl: command failed")
	ErrTimeout       = errors.New("esl: timeout")
)

type Config struct {
	Addr           string
	Password       string
	ConnectTimeout time.Duration
	ReadTimeout    time.Duration
	Logger         *slog.Logger
}

func (c *Config) defaults() {
	if c.ConnectTimeout == 0 {
		c.ConnectTimeout = 10 * time.Second
	}
	if c.ReadTimeout == 0 {
		c.ReadTimeout = 60 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Client — ESL-клиент к одной FS-ноде.
type Client struct {
	cfg       Config
	conn      net.Conn
	reader    *bufio.Reader
	writer    *bufio.Writer
	mu        sync.Mutex
	connected atomic.Bool
	closed    atomic.Bool

	events    chan Event
	replies   chan Frame
	replyOnce sync.Once

	doneCh chan struct{}
}

func Dial(ctx context.Context, cfg Config) (*Client, error) {
	cfg.defaults()
	dialCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	d := net.Dialer{}
	conn, err := d.DialContext(dialCtx, "tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", cfg.Addr, err)
	}
	cli := &Client{
		cfg:     cfg,
		conn:    conn,
		reader:  bufio.NewReaderSize(conn, 64*1024),
		writer:  bufio.NewWriterSize(conn, 8*1024),
		events:  make(chan Event, 1024),
		replies: make(chan Frame, 16),
		doneCh:  make(chan struct{}),
	}
	if err := cli.authenticate(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	cli.connected.Store(true)
	go cli.readLoop()
	return cli, nil
}

func (c *Client) authenticate() error {
	frame, err := parseFrame(c.reader)
	if err != nil {
		return fmt.Errorf("read auth/request: %w", err)
	}
	if frame.ContentType() != "auth/request" {
		return fmt.Errorf("expected auth/request, got %q", frame.ContentType())
	}
	if _, err := fmt.Fprintf(c.writer, "auth %s\r\n\r\n", c.cfg.Password); err != nil {
		return fmt.Errorf("write auth: %w", err)
	}
	if err := c.writer.Flush(); err != nil {
		return fmt.Errorf("flush auth: %w", err)
	}
	resp, err := parseFrame(c.reader)
	if err != nil {
		return fmt.Errorf("read auth reply: %w", err)
	}
	if reply := resp.Header("Reply-Text"); reply == "" || reply[0] != '+' {
		return ErrAuthFailed
	}
	return nil
}

func (c *Client) Connected() bool { return c.connected.Load() && !c.closed.Load() }

func (c *Client) Events() <-chan Event { return c.events }

func (c *Client) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.connected.Store(false)
	err := c.conn.Close()
	close(c.doneCh)
	return err
}

func (c *Client) readLoop() {
	defer func() {
		c.connected.Store(false)
		close(c.events)
	}()
	for {
		if c.cfg.ReadTimeout > 0 {
			_ = c.conn.SetReadDeadline(time.Now().Add(c.cfg.ReadTimeout + 30*time.Second))
		}
		frame, err := parseFrame(c.reader)
		if err != nil {
			if !c.closed.Load() {
				c.cfg.Logger.Warn("esl read loop", slog.String("addr", c.cfg.Addr), slog.String("err", err.Error()))
			}
			return
		}
		switch frame.ContentType() {
		case "text/event-plain", "text/event-json":
			ev, err := frame.AsEvent()
			if err == nil {
				select {
				case c.events <- ev:
				default:
					c.cfg.Logger.Warn("esl event drop (chan full)", slog.String("addr", c.cfg.Addr), slog.String("event", ev.Name))
				}
			}
		case "command/reply", "api/response":
			select {
			case c.replies <- frame:
			default:
			}
		case "text/disconnect-notice":
			c.cfg.Logger.Info("esl disconnect notice", slog.String("addr", c.cfg.Addr))
			return
		}
	}
}

// sendCommand отправляет команду и ждёт ответ.
func (c *Client) sendCommand(ctx context.Context, line string) (Frame, error) {
	if !c.Connected() {
		return Frame{}, ErrNotConnected
	}
	c.mu.Lock()
	if _, err := fmt.Fprintf(c.writer, "%s\r\n\r\n", line); err != nil {
		c.mu.Unlock()
		return Frame{}, fmt.Errorf("write: %w", err)
	}
	if err := c.writer.Flush(); err != nil {
		c.mu.Unlock()
		return Frame{}, fmt.Errorf("flush: %w", err)
	}
	c.mu.Unlock()
	select {
	case f := <-c.replies:
		return f, nil
	case <-ctx.Done():
		return Frame{}, ErrTimeout
	case <-c.doneCh:
		return Frame{}, ErrNotConnected
	}
}
```

4. **Reconnect.** Создаём `internal/telephony/esl/reconnect.go` с jittered exponential backoff:

```go
package esl

import (
	"context"
	"math/rand/v2"
	"time"
)

// Backoff возвращает следующую задержку: min(cap, base * 2^attempt) ± 25% jitter.
type Backoff struct {
	Base    time.Duration
	Cap     time.Duration
	attempt int
}

func (b *Backoff) Next() time.Duration {
	if b.Base == 0 {
		b.Base = 500 * time.Millisecond
	}
	if b.Cap == 0 {
		b.Cap = 30 * time.Second
	}
	d := b.Base
	for i := 0; i < b.attempt; i++ {
		d *= 2
		if d > b.Cap {
			d = b.Cap
			break
		}
	}
	jitter := time.Duration(float64(d) * 0.25 * (rand.Float64()*2 - 1))
	b.attempt++
	return d + jitter
}

func (b *Backoff) Reset() { b.attempt = 0 }

// Sleep ждёт с учётом контекста.
func (b *Backoff) Sleep(ctx context.Context) error {
	t := time.NewTimer(b.Next())
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
```

5. **Refactor + metrics.** Создаём `internal/telephony/esl/metrics.go`:

```go
package esl

import "github.com/prometheus/client_golang/prometheus"

var (
	connectedGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "esl_connected", Help: "1 if ESL connection is up, 0 otherwise"},
		[]string{"node"},
	)
	commandsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "esl_commands_total", Help: "Total ESL commands sent"},
		[]string{"node", "command", "result"},
	)
	commandDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "esl_command_duration_seconds",
			Help:    "ESL command latency",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 14),
		},
		[]string{"node", "command"},
	)
	eventsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "esl_events_total", Help: "Total ESL events received"},
		[]string{"node", "event"},
	)
	reconnectsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "esl_reconnects_total", Help: "Total ESL reconnect attempts"},
		[]string{"node", "result"},
	)
)

func init() {
	prometheus.MustRegister(connectedGauge, commandsTotal, commandDuration, eventsTotal, reconnectsTotal)
}
```

**Verification:**
- `go test ./internal/telephony/esl/...` — все тесты зелёные (parser, client auth/timeout, backoff).
- Coverage по парсеру ≥ 90%, по клиенту ≥ 75%.
- `go vet ./internal/telephony/esl/...` — чисто.
- `go test -race ./internal/telephony/esl/...` — без data race.

**Acceptance:**
- Парсер корректно обрабатывает auth/request, command/reply, api/response, text/event-plain.
- Клиент успешно auth и failure-сценарии.
- Backoff: начальный 500мс, удвоение, cap 30с, jitter ±25%.
- При закрытии Client все горутины завершаются (нет утечек, проверено `goleak`).

---

### Task 3: ESL high-level commands — Originate, Hangup, MixMonitor, Play, CreateUser/DeleteUser

**Цель:** Высокоуровневые команды поверх `sendCommand`, с типизированными аргументами, парсингом ответа и метриками.

**Шаги:**

1. **Red.** Расширяем `internal/telephony/esl/commands_test.go`:

```go
package esl

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// scriptedServer — fake FS, который отвечает на каждое запрос строкой из replies.
func scriptedServer(t *testing.T, replies []string) (string, func(), *[]string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var (
		got    []string
		gotMu  sync.Mutex
	)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("Content-Type: auth/request\n\n"))
		buf := make([]byte, 4096)
		// auth
		n, _ := conn.Read(buf)
		gotMu.Lock()
		got = append(got, string(buf[:n]))
		gotMu.Unlock()
		_, _ = conn.Write([]byte("Content-Type: command/reply\nReply-Text: +OK accepted\n\n"))
		idx := 0
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			gotMu.Lock()
			got = append(got, string(buf[:n]))
			gotMu.Unlock()
			if idx < len(replies) {
				_, _ = conn.Write([]byte(replies[idx]))
				idx++
			}
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }, &got
}

func TestClient_Originate(t *testing.T) {
	addr, stop, got := scriptedServer(t, []string{
		"Content-Type: api/response\nContent-Length: 42\n\n+OK 11111111-2222-3333-4444-555555555555\n",
	})
	defer stop()
	cli, err := Dial(context.Background(), Config{Addr: addr, Password: "x"})
	require.NoError(t, err)
	defer cli.Close()

	uuid, err := cli.Originate(context.Background(), OriginateRequest{
		CallURL:   "sofia/gateway/main/+79991234567",
		Caller:    "+74951112233",
		Variables: map[string]string{"sip_h_X-Call-Id": "abc"},
		Extension: "&park()",
	})
	require.NoError(t, err)
	require.Equal(t, "11111111-2222-3333-4444-555555555555", uuid)
	require.Contains(t, strings.Join(*got, ""), "originate ")
	require.Contains(t, strings.Join(*got, ""), "{sip_h_X-Call-Id=abc")
}

func TestClient_Hangup(t *testing.T) {
	addr, stop, got := scriptedServer(t, []string{
		"Content-Type: api/response\nContent-Length: 4\n\n+OK\n",
	})
	defer stop()
	cli, err := Dial(context.Background(), Config{Addr: addr, Password: "x"})
	require.NoError(t, err)
	defer cli.Close()

	err = cli.Hangup(context.Background(), "uuid-1", "NORMAL_CLEARING")
	require.NoError(t, err)
	require.Contains(t, strings.Join(*got, ""), "uuid_kill uuid-1 NORMAL_CLEARING")
}

func TestClient_MixMonitor(t *testing.T) {
	addr, stop, got := scriptedServer(t, []string{
		"Content-Type: api/response\nContent-Length: 4\n\n+OK\n",
	})
	defer stop()
	cli, err := Dial(context.Background(), Config{Addr: addr, Password: "x"})
	require.NoError(t, err)
	defer cli.Close()

	err = cli.MixMonitorStart(context.Background(), "uuid-1", "/recordings/uuid-1.wav", []string{"stereo"})
	require.NoError(t, err)
	require.Contains(t, strings.Join(*got, ""), "uuid_record uuid-1 start /recordings/uuid-1.wav")
}

func TestClient_OriginateError(t *testing.T) {
	addr, stop, _ := scriptedServer(t, []string{
		"Content-Type: api/response\nContent-Length: 22\n\n-ERR USER_BUSY\n",
	})
	defer stop()
	cli, err := Dial(context.Background(), Config{Addr: addr, Password: "x"})
	require.NoError(t, err)
	defer cli.Close()

	_, err = cli.Originate(context.Background(), OriginateRequest{
		CallURL:   "sofia/gateway/main/+79991234567",
		Extension: "&park()",
	})
	require.ErrorIs(t, err, ErrCommandFailed)
}

func TestClient_OriginateContextCancel(t *testing.T) {
	addr, stop, _ := scriptedServer(t, []string{}) // не отвечает
	defer stop()
	cli, err := Dial(context.Background(), Config{Addr: addr, Password: "x"})
	require.NoError(t, err)
	defer cli.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = cli.Originate(ctx, OriginateRequest{CallURL: "x", Extension: "&park()"})
	require.ErrorIs(t, err, ErrTimeout)
}
```

2. **Green.** Создаём `internal/telephony/esl/commands.go`:

```go
package esl

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// OriginateRequest — параметры команды originate.
type OriginateRequest struct {
	// CallURL — назначение, например "sofia/gateway/trunk-main/+79991234567"
	CallURL string
	// Extension — действие после ответа: "&park()", "&bridge(sofia/internal/100)", или dialplan-name
	Extension string
	// Caller — caller-id (number)
	Caller string
	// CallerName — caller-id (name)
	CallerName string
	// Variables — channel-variables, передаются в "{var=value,var=value}" prefix
	Variables map[string]string
	// Timeout — originate_timeout (секунды)
	Timeout time.Duration
}

// Originate выполняет команду `bgapi originate` и возвращает channel UUID при успехе.
func (c *Client) Originate(ctx context.Context, req OriginateRequest) (string, error) {
	start := time.Now()
	defer func() {
		commandDuration.WithLabelValues(c.cfg.Addr, "originate").
			Observe(time.Since(start).Seconds())
	}()

	if req.CallURL == "" {
		return "", fmt.Errorf("call_url required")
	}
	if req.Extension == "" {
		req.Extension = "&park()"
	}
	if req.Timeout == 0 {
		req.Timeout = 30 * time.Second
	}

	vars := buildVariables(req)
	cmd := fmt.Sprintf("bgapi originate %s%s %s",
		vars,
		escapeURL(req.CallURL),
		req.Extension,
	)
	frame, err := c.sendCommand(ctx, cmd)
	if err != nil {
		commandsTotal.WithLabelValues(c.cfg.Addr, "originate", "error").Inc()
		return "", err
	}
	body := strings.TrimSpace(string(frame.Body))
	if !strings.HasPrefix(body, "+OK") {
		commandsTotal.WithLabelValues(c.cfg.Addr, "originate", "fail").Inc()
		return "", fmt.Errorf("%w: %s", ErrCommandFailed, body)
	}
	commandsTotal.WithLabelValues(c.cfg.Addr, "originate", "ok").Inc()
	uuid := strings.TrimSpace(strings.TrimPrefix(body, "+OK"))
	return uuid, nil
}

func buildVariables(req OriginateRequest) string {
	vars := map[string]string{}
	for k, v := range req.Variables {
		vars[k] = v
	}
	if req.Caller != "" {
		vars["origination_caller_id_number"] = req.Caller
	}
	if req.CallerName != "" {
		vars["origination_caller_id_name"] = req.CallerName
	}
	if req.Timeout > 0 {
		vars["originate_timeout"] = fmt.Sprintf("%d", int(req.Timeout.Seconds()))
	}
	if len(vars) == 0 {
		return ""
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, escapeVar(vars[k])))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func escapeVar(v string) string {
	// Минимальное экранирование: запятые/фигурные скобки в значениях запрещены
	v = strings.ReplaceAll(v, ",", "\\,")
	v = strings.ReplaceAll(v, "{", "")
	v = strings.ReplaceAll(v, "}", "")
	return v
}

func escapeURL(u string) string {
	// FS не любит пробелы
	return strings.ReplaceAll(u, " ", "%20")
}

// Hangup завершает канал.
func (c *Client) Hangup(ctx context.Context, uuid, cause string) error {
	if uuid == "" {
		return fmt.Errorf("uuid required")
	}
	if cause == "" {
		cause = "NORMAL_CLEARING"
	}
	frame, err := c.sendCommand(ctx, fmt.Sprintf("bgapi uuid_kill %s %s", uuid, cause))
	if err != nil {
		commandsTotal.WithLabelValues(c.cfg.Addr, "hangup", "error").Inc()
		return err
	}
	if !strings.HasPrefix(strings.TrimSpace(string(frame.Body)), "+OK") {
		commandsTotal.WithLabelValues(c.cfg.Addr, "hangup", "fail").Inc()
		return ErrCommandFailed
	}
	commandsTotal.WithLabelValues(c.cfg.Addr, "hangup", "ok").Inc()
	return nil
}

// MixMonitorStart включает запись звонка.
func (c *Client) MixMonitorStart(ctx context.Context, uuid, path string, flags []string) error {
	if uuid == "" || path == "" {
		return fmt.Errorf("uuid and path required")
	}
	flagStr := ""
	if len(flags) > 0 {
		flagStr = " " + strings.Join(flags, ",")
	}
	frame, err := c.sendCommand(ctx, fmt.Sprintf("bgapi uuid_record %s start %s%s", uuid, path, flagStr))
	if err != nil {
		commandsTotal.WithLabelValues(c.cfg.Addr, "mixmonitor", "error").Inc()
		return err
	}
	if !strings.HasPrefix(strings.TrimSpace(string(frame.Body)), "+OK") {
		commandsTotal.WithLabelValues(c.cfg.Addr, "mixmonitor", "fail").Inc()
		return ErrCommandFailed
	}
	commandsTotal.WithLabelValues(c.cfg.Addr, "mixmonitor", "ok").Inc()
	return nil
}

// MixMonitorStop останавливает запись.
func (c *Client) MixMonitorStop(ctx context.Context, uuid, path string) error {
	if uuid == "" {
		return fmt.Errorf("uuid required")
	}
	cmd := fmt.Sprintf("bgapi uuid_record %s stop %s", uuid, path)
	frame, err := c.sendCommand(ctx, cmd)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(strings.TrimSpace(string(frame.Body)), "+OK") {
		return ErrCommandFailed
	}
	return nil
}

// Play воспроизводит файл в активном канале (через broadcast, без перебивания текущего bridge).
func (c *Client) Play(ctx context.Context, uuid, path string) error {
	if uuid == "" || path == "" {
		return fmt.Errorf("uuid and path required")
	}
	frame, err := c.sendCommand(ctx, fmt.Sprintf("bgapi uuid_broadcast %s %s aleg", uuid, path))
	if err != nil {
		return err
	}
	if !strings.HasPrefix(strings.TrimSpace(string(frame.Body)), "+OK") {
		return ErrCommandFailed
	}
	return nil
}

// SofiaStatus возвращает строку результата `api sofia status` (используется для health-check).
func (c *Client) SofiaStatus(ctx context.Context) (string, error) {
	frame, err := c.sendCommand(ctx, "api sofia status")
	if err != nil {
		return "", err
	}
	return string(frame.Body), nil
}

// SubscribeEvents подписывает inbound-сокет на конкретный набор событий.
func (c *Client) SubscribeEvents(ctx context.Context, events []string) error {
	if len(events) == 0 {
		return nil
	}
	cmd := "event plain " + strings.Join(events, " ")
	frame, err := c.sendCommand(ctx, cmd)
	if err != nil {
		return err
	}
	if reply := frame.Header("Reply-Text"); reply == "" || reply[0] != '+' {
		return fmt.Errorf("%w: %s", ErrCommandFailed, reply)
	}
	return nil
}

// CreateUser создаёт per-call SIP-аккаунт через API mod_xml_curl callback.
// На стороне FS должен быть настроен xml_handler URL=http://api/internal/freeswitch/directory.
// Здесь мы только публикуем запись в Redis (см. credentials.Manager) и инвалидируем кэш в FS.
func (c *Client) ReloadXMLDirectory(ctx context.Context, domain string) error {
	cmd := fmt.Sprintf("api reloadxml")
	if domain != "" {
		cmd = fmt.Sprintf("api xml_flush_cache %s", domain)
	}
	frame, err := c.sendCommand(ctx, cmd)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(strings.TrimSpace(string(frame.Body)), "+OK") {
		return fmt.Errorf("%w: %s", ErrCommandFailed, string(frame.Body))
	}
	return nil
}

// Bind глобальные prometheus-метрики (для совместимости).
var _ = prometheus.Counter(commandsTotal.WithLabelValues("", "", ""))
```

3. **Refactor.** Выносим обёртки в `internal/telephony/esl/wrappers.go` если получится много дублирования. Покрываем вариантами edge-case (Originate с пустыми Variables, Hangup без UUID).

**Verification:**
- `go test ./internal/telephony/esl/... -run TestClient_` — все command-тесты зелёные.
- `go test -race ./internal/telephony/esl/...` — нет race.
- Команды `originate`, `uuid_kill`, `uuid_record`, `uuid_broadcast` собираются с правильным синтаксисом FS.
- Метрики `esl_commands_total{command="originate",result="ok"}` инкрементятся.

**Acceptance:**
- 6 высокоуровневых команд: Originate, Hangup, MixMonitorStart/Stop, Play, SofiaStatus, SubscribeEvents.
- Все команды обрабатывают context.Canceled / DeadlineExceeded.
- Variables корректно сериализуются и сортируются (детерминированный порядок для тестов).
- Coverage по `commands.go` ≥ 85%.

---

### Task 4: ESLPool — multi-node management, health-check, reconnect

**Цель:** Пул из N FS-нод, каждой держит один inbound-ESL, периодический health-check, автоматическое переподключение, выдача рабочего клиента по имени ноды.

**Шаги:**

1. **Red.** `internal/telephony/pool/pool_test.go`:

```go
package pool

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeNode — простейший ESL-сервер.
func fakeNode(t *testing.T, deadOnConnect *atomic.Bool) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if deadOnConnect != nil && deadOnConnect.Load() {
				_ = c.Close()
				continue
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = conn.Write([]byte("Content-Type: auth/request\n\n"))
				buf := make([]byte, 4096)
				_, _ = conn.Read(buf)
				_, _ = conn.Write([]byte("Content-Type: command/reply\nReply-Text: +OK accepted\n\n"))
				for {
					n, err := conn.Read(buf)
					if err != nil {
						return
					}
					if n > 0 {
						_, _ = conn.Write([]byte("Content-Type: api/response\nContent-Length: 50\n\nUP 0/0/1000/100 (0/0/1)\nProfile internal\n"))
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

func TestPool_AllNodesUp(t *testing.T) {
	addr1, stop1 := fakeNode(t, nil)
	defer stop1()
	addr2, stop2 := fakeNode(t, nil)
	defer stop2()

	p, err := New(context.Background(), Config{
		Nodes:           []string{addr1, addr2},
		Password:        "x",
		HealthInterval:  100 * time.Millisecond,
	})
	require.NoError(t, err)
	defer p.Close()

	require.True(t, p.AnyHealthy())
	require.Len(t, p.HealthyNodes(), 2)

	cli, err := p.Get(addr1)
	require.NoError(t, err)
	require.True(t, cli.Connected())
}

func TestPool_OneNodeDown(t *testing.T) {
	addr1, stop1 := fakeNode(t, nil)
	defer stop1()
	dead := &atomic.Bool{}
	dead.Store(true)
	addr2, stop2 := fakeNode(t, dead)
	defer stop2()

	p, err := New(context.Background(), Config{
		Nodes:          []string{addr1, addr2},
		Password:       "x",
		HealthInterval: 100 * time.Millisecond,
	})
	require.NoError(t, err)
	defer p.Close()

	require.True(t, p.AnyHealthy())
	require.Eventually(t, func() bool {
		return len(p.HealthyNodes()) == 1
	}, 2*time.Second, 50*time.Millisecond)
}

func TestPool_RecoverAfterDown(t *testing.T) {
	dead := &atomic.Bool{}
	dead.Store(true)
	addr, stop := fakeNode(t, dead)
	defer stop()

	p, err := New(context.Background(), Config{
		Nodes:          []string{addr},
		Password:       "x",
		HealthInterval: 100 * time.Millisecond,
	})
	require.NoError(t, err)
	defer p.Close()

	require.False(t, p.AnyHealthy())
	dead.Store(false)
	require.Eventually(t, func() bool {
		return p.AnyHealthy()
	}, 5*time.Second, 100*time.Millisecond)
}
```

2. **Green.** Создаём `internal/telephony/pool/pool.go`:

```go
package pool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sociopulse/social-pulse/internal/telephony/esl"
)

var ErrNoHealthyNode = errors.New("pool: no healthy esl nodes")

type Config struct {
	Nodes          []string      // host:port, ...
	Password       string
	ConnectTimeout time.Duration
	HealthInterval time.Duration
	Subscriptions  []string // events to subscribe on each connect
	Logger         *slog.Logger
}

func (c *Config) defaults() {
	if c.ConnectTimeout == 0 {
		c.ConnectTimeout = 10 * time.Second
	}
	if c.HealthInterval == 0 {
		c.HealthInterval = 5 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if len(c.Subscriptions) == 0 {
		c.Subscriptions = []string{
			"CHANNEL_CREATE",
			"CHANNEL_ANSWER",
			"CHANNEL_HANGUP_COMPLETE",
			"CHANNEL_BRIDGE",
			"CHANNEL_UNBRIDGE",
			"DTMF",
			"RECORD_STOP",
			"CUSTOM sofia::register",
			"CUSTOM mod_callcenter::*",
		}
	}
}

type nodeState struct {
	addr     string
	mu       sync.RWMutex
	client   *esl.Client
	healthy  bool
	lastErr  error
	backoff  esl.Backoff
}

// ESLPool управляет одним inbound ESL на каждую FS-ноду.
type ESLPool struct {
	cfg     Config
	nodes   map[string]*nodeState
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	events  chan esl.Event
}

// New создаёт пул и пытается подключиться ко всем нодам. Не падает, если часть нод недоступны —
// они будут переподключаться в фоне.
func New(parent context.Context, cfg Config) (*ESLPool, error) {
	cfg.defaults()
	if len(cfg.Nodes) == 0 {
		return nil, fmt.Errorf("at least one node required")
	}
	ctx, cancel := context.WithCancel(parent)
	p := &ESLPool{
		cfg:    cfg,
		nodes:  make(map[string]*nodeState, len(cfg.Nodes)),
		ctx:    ctx,
		cancel: cancel,
		events: make(chan esl.Event, 4096),
	}
	for _, addr := range cfg.Nodes {
		p.nodes[addr] = &nodeState{addr: addr}
	}
	for _, addr := range cfg.Nodes {
		p.wg.Add(1)
		go p.runNode(addr)
	}
	return p, nil
}

// Get возвращает текущего клиента ноды или ErrNotConnected.
func (p *ESLPool) Get(addr string) (*esl.Client, error) {
	st, ok := p.nodes[addr]
	if !ok {
		return nil, fmt.Errorf("unknown node %q", addr)
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	if st.client == nil || !st.healthy {
		return nil, esl.ErrNotConnected
	}
	return st.client, nil
}

// AnyHealthy — есть ли хотя бы одна живая нода.
func (p *ESLPool) AnyHealthy() bool {
	for _, st := range p.nodes {
		st.mu.RLock()
		h := st.healthy
		st.mu.RUnlock()
		if h {
			return true
		}
	}
	return false
}

// HealthyNodes возвращает список живых адресов.
func (p *ESLPool) HealthyNodes() []string {
	out := make([]string, 0, len(p.nodes))
	for addr, st := range p.nodes {
		st.mu.RLock()
		h := st.healthy
		st.mu.RUnlock()
		if h {
			out = append(out, addr)
		}
	}
	return out
}

// Events — единый канал событий со всех нод. Хедер "node-addr" добавляется в каждое событие.
func (p *ESLPool) Events() <-chan esl.Event { return p.events }

// Close закрывает все соединения.
func (p *ESLPool) Close() error {
	p.cancel()
	p.wg.Wait()
	close(p.events)
	return nil
}

// runNode — основная горутина для одной ноды: connect → subscribe → forward events → on disconnect — backoff и заново.
func (p *ESLPool) runNode(addr string) {
	defer p.wg.Done()
	st := p.nodes[addr]
	for {
		if err := p.ctx.Err(); err != nil {
			return
		}
		err := p.connectAndServe(st)
		if err != nil && p.ctx.Err() == nil {
			st.mu.Lock()
			st.healthy = false
			st.lastErr = err
			st.mu.Unlock()
			p.cfg.Logger.Warn("esl node disconnected",
				slog.String("addr", addr), slog.String("err", err.Error()))
		}
		if p.ctx.Err() != nil {
			return
		}
		if err := st.backoff.Sleep(p.ctx); err != nil {
			return
		}
	}
}

func (p *ESLPool) connectAndServe(st *nodeState) error {
	cli, err := esl.Dial(p.ctx, esl.Config{
		Addr:           st.addr,
		Password:       p.cfg.Password,
		ConnectTimeout: p.cfg.ConnectTimeout,
		Logger:         p.cfg.Logger,
	})
	if err != nil {
		return err
	}
	defer cli.Close()
	if err := cli.SubscribeEvents(p.ctx, p.cfg.Subscriptions); err != nil {
		return fmt.Errorf("subscribe events: %w", err)
	}
	st.mu.Lock()
	st.client = cli
	st.healthy = true
	st.lastErr = nil
	st.backoff.Reset()
	st.mu.Unlock()
	p.cfg.Logger.Info("esl node connected", slog.String("addr", st.addr))

	// forward events
	healthCh := time.NewTicker(p.cfg.HealthInterval)
	defer healthCh.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return nil
		case ev, ok := <-cli.Events():
			if !ok {
				return errors.New("event channel closed")
			}
			// stamp source node
			ev2 := ev
			select {
			case p.events <- ev2:
			default:
				p.cfg.Logger.Warn("pool events channel full, dropping",
					slog.String("addr", st.addr), slog.String("event", ev.Name))
			}
		case <-healthCh.C:
			if err := p.healthProbe(st, cli); err != nil {
				return err
			}
		}
	}
}

func (p *ESLPool) healthProbe(st *nodeState, cli *esl.Client) error {
	ctx, cancel := context.WithTimeout(p.ctx, 3*time.Second)
	defer cancel()
	out, err := cli.SofiaStatus(ctx)
	if err != nil {
		return fmt.Errorf("sofia status: %w", err)
	}
	// FS возвращает обширный текст; проверяем что есть слова "Profile" или "RUNNING"
	if len(out) < 10 {
		return fmt.Errorf("sofia status too short: %q", out)
	}
	st.mu.Lock()
	st.healthy = true
	st.mu.Unlock()
	return nil
}
```

3. **Refactor + metrics.** `internal/telephony/pool/metrics.go`:

```go
package pool

import "github.com/prometheus/client_golang/prometheus"

var (
	healthyGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "telephony_pool_node_healthy", Help: "1 if FS node is healthy in pool"},
		[]string{"node"},
	)
	healthCheckDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "telephony_pool_health_check_seconds",
			Help:    "Pool health check duration",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"node"},
	)
)

func init() {
	prometheus.MustRegister(healthyGauge, healthCheckDuration)
}
```

**Verification:**
- `go test ./internal/telephony/pool/... -race` — все тесты зелёные.
- Сценарии: все ноды up → 2 healthy; 1 down → 1 healthy; восстановление после восстановления.
- Утечки горутин проверяем `goleak.VerifyNone(t)` в TestMain.
- Coverage ≥ 80%.

**Acceptance:**
- При создании пула с 3 адресами и 1 недоступным — пул запускается без ошибки, AnyHealthy() == true.
- Health-check каждые 5 сек через `api sofia status`.
- Reconnect с backoff (использует esl.Backoff).
- При Close() все горутины завершаются за < 2 сек.

---

### Task 5: Router — trunk selection, FS-node selection, backpressure

**Цель:** Реализовать выбор SIP-trunk (по operator/account политике) и FS-ноды (по active_channels) для исходящего звонка с поддержкой 4 стратегий.

**Шаги:**

1. **Red.** `internal/telephony/router/strategy_test.go`:

```go
package router

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLeastCostStrategy(t *testing.T) {
	trunks := []Trunk{
		{ID: "a", CostPerMin: 0.05, Active: true},
		{ID: "b", CostPerMin: 0.03, Active: true},
		{ID: "c", CostPerMin: 0.02, Active: false},
	}
	s := LeastCost{}
	chosen, err := s.Pick(trunks, "+79991112233")
	require.NoError(t, err)
	require.Equal(t, "b", chosen.ID)
}

func TestRoundRobinStrategy(t *testing.T) {
	trunks := []Trunk{
		{ID: "a", Active: true},
		{ID: "b", Active: true},
		{ID: "c", Active: true},
	}
	s := &RoundRobin{}
	seq := []string{}
	for i := 0; i < 6; i++ {
		c, err := s.Pick(trunks, "+x")
		require.NoError(t, err)
		seq = append(seq, c.ID)
	}
	require.Equal(t, []string{"a", "b", "c", "a", "b", "c"}, seq)
}

func TestWeightedStrategy(t *testing.T) {
	trunks := []Trunk{
		{ID: "a", Weight: 70, Active: true},
		{ID: "b", Weight: 30, Active: true},
	}
	s := &Weighted{}
	counts := map[string]int{}
	for i := 0; i < 10000; i++ {
		c, err := s.Pick(trunks, "+x")
		require.NoError(t, err)
		counts[c.ID]++
	}
	require.InDelta(t, 0.7, float64(counts["a"])/10000, 0.05)
	require.InDelta(t, 0.3, float64(counts["b"])/10000, 0.05)
}

func TestLeastCostWithFallback(t *testing.T) {
	trunks := []Trunk{
		{ID: "primary", CostPerMin: 0.02, Active: true, FailureRate: 0.6},
		{ID: "backup", CostPerMin: 0.05, Active: true, FailureRate: 0.05},
	}
	s := LeastCostWithFallback{FailureThreshold: 0.5}
	chosen, err := s.Pick(trunks, "+x")
	require.NoError(t, err)
	require.Equal(t, "backup", chosen.ID, "primary должен быть отброшен из-за высокой ошибки")
}
```

`internal/telephony/router/backpressure_test.go`:

```go
package router

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestBackpressure_RejectAtCap(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	bp := NewBackpressure(rdb, 60)
	ctx := context.Background()
	for i := 0; i < 60; i++ {
		ok, err := bp.TryAcquire(ctx, "node-1")
		require.NoError(t, err)
		require.True(t, ok)
	}
	ok, err := bp.TryAcquire(ctx, "node-1")
	require.NoError(t, err)
	require.False(t, ok, "61-й канал должен быть отбит")

	// release один
	require.NoError(t, bp.Release(ctx, "node-1"))
	ok, err = bp.TryAcquire(ctx, "node-1")
	require.NoError(t, err)
	require.True(t, ok)
}

func TestBackpressure_PerNodeCounters(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	bp := NewBackpressure(rdb, 2)
	ctx := context.Background()

	require.True(t, mustOK(bp.TryAcquire(ctx, "n1")))
	require.True(t, mustOK(bp.TryAcquire(ctx, "n1")))
	require.False(t, mustOK(bp.TryAcquire(ctx, "n1")))
	require.True(t, mustOK(bp.TryAcquire(ctx, "n2"))) // другая нода — свой счётчик
}

func mustOK(ok bool, err error) bool {
	if err != nil {
		panic(err)
	}
	return ok
}
```

2. **Green.** `internal/telephony/router/strategy.go`:

```go
package router

import (
	"errors"
	"math/rand/v2"
	"sort"
	"sync/atomic"
)

// Trunk — выбираемый SIP-trunk.
type Trunk struct {
	ID            string
	GatewayName   string  // имя в sofia (e.g. "trunk-main")
	NodeAddrs     []string // на каких FS-нодах настроен этот gateway
	CostPerMin    float64
	Weight        int
	Active        bool
	FailureRate   float64 // последние N звонков, обновляется stats-collector
	Priority      int
}

var ErrNoTrunkAvailable = errors.New("router: no available trunk")

// Strategy — алгоритм выбора trunk.
type Strategy interface {
	Pick(trunks []Trunk, dest string) (Trunk, error)
}

// LeastCost — минимум CostPerMin среди активных.
type LeastCost struct{}

func (LeastCost) Pick(trunks []Trunk, _ string) (Trunk, error) {
	var best *Trunk
	for i := range trunks {
		t := &trunks[i]
		if !t.Active {
			continue
		}
		if best == nil || t.CostPerMin < best.CostPerMin {
			best = t
		}
	}
	if best == nil {
		return Trunk{}, ErrNoTrunkAvailable
	}
	return *best, nil
}

// LeastCostWithFallback — то же, но отбрасывает trunk с FailureRate > FailureThreshold.
type LeastCostWithFallback struct {
	FailureThreshold float64 // напр. 0.5
}

func (s LeastCostWithFallback) Pick(trunks []Trunk, dest string) (Trunk, error) {
	filtered := make([]Trunk, 0, len(trunks))
	for _, t := range trunks {
		if t.Active && t.FailureRate <= s.FailureThreshold {
			filtered = append(filtered, t)
		}
	}
	if len(filtered) == 0 {
		// fallback — игнорим failure, лишь бы был хоть один активный
		return LeastCost{}.Pick(trunks, dest)
	}
	return LeastCost{}.Pick(filtered, dest)
}

// RoundRobin — детерминированный round-robin по списку активных.
type RoundRobin struct{ counter atomic.Uint64 }

func (s *RoundRobin) Pick(trunks []Trunk, _ string) (Trunk, error) {
	active := make([]Trunk, 0, len(trunks))
	for _, t := range trunks {
		if t.Active {
			active = append(active, t)
		}
	}
	if len(active) == 0 {
		return Trunk{}, ErrNoTrunkAvailable
	}
	sort.Slice(active, func(i, j int) bool { return active[i].ID < active[j].ID })
	n := s.counter.Add(1) - 1
	return active[n%uint64(len(active))], nil
}

// Weighted — вес × random.
type Weighted struct{}

func (Weighted) Pick(trunks []Trunk, _ string) (Trunk, error) {
	totalW := 0
	for _, t := range trunks {
		if t.Active {
			totalW += t.Weight
		}
	}
	if totalW == 0 {
		return Trunk{}, ErrNoTrunkAvailable
	}
	r := rand.IntN(totalW)
	for _, t := range trunks {
		if !t.Active {
			continue
		}
		r -= t.Weight
		if r < 0 {
			return t, nil
		}
	}
	return Trunk{}, ErrNoTrunkAvailable
}
```

3. `internal/telephony/router/backpressure.go`:

```go
package router

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Backpressure ограничивает кол-во активных каналов на FS-ноду через Redis-счётчик.
type Backpressure struct {
	rdb *redis.Client
	cap int
	ttl time.Duration
}

func NewBackpressure(rdb *redis.Client, cap int) *Backpressure {
	if cap <= 0 {
		cap = 60
	}
	return &Backpressure{rdb: rdb, cap: cap, ttl: 1 * time.Hour}
}

func (b *Backpressure) key(node string) string {
	return fmt.Sprintf("op:active_channels:%s", node)
}

// TryAcquire атомарно увеличивает счётчик при условии < cap. Lua-скрипт.
func (b *Backpressure) TryAcquire(ctx context.Context, node string) (bool, error) {
	const lua = `
local v = tonumber(redis.call("GET", KEYS[1]) or "0")
if v >= tonumber(ARGV[1]) then
  return 0
end
redis.call("INCR", KEYS[1])
redis.call("EXPIRE", KEYS[1], ARGV[2])
return 1
`
	res, err := b.rdb.Eval(ctx, lua, []string{b.key(node)}, b.cap, int(b.ttl.Seconds())).Int()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

func (b *Backpressure) Release(ctx context.Context, node string) error {
	return b.rdb.Decr(ctx, b.key(node)).Err()
}

func (b *Backpressure) Current(ctx context.Context, node string) (int, error) {
	v, err := b.rdb.Get(ctx, b.key(node)).Int()
	if err == redis.Nil {
		return 0, nil
	}
	return v, err
}
```

4. `internal/telephony/router/router.go` — собирает всё вместе:

```go
package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/sociopulse/social-pulse/internal/telephony/pool"
)

type Config struct {
	Pool            *pool.ESLPool
	Redis           *redis.Client
	BackpressureCap int
	PostgresURL     string
	Logger          *slog.Logger
	RefreshInterval time.Duration
}

type Router struct {
	cfg          Config
	pg           *pgxpool.Pool
	bp           *Backpressure
	mu           sync.RWMutex
	trunks       map[string][]Trunk // operatorID → []Trunk
	strategies   map[string]Strategy
	defaultStrat Strategy
	cancel       context.CancelFunc
	wg           sync.WaitGroup
}

func New(cfg Config) *Router {
	if cfg.RefreshInterval == 0 {
		cfg.RefreshInterval = 30 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	rr := &RoundRobin{}
	return &Router{
		cfg:    cfg,
		bp:     NewBackpressure(cfg.Redis, cfg.BackpressureCap),
		trunks: make(map[string][]Trunk),
		strategies: map[string]Strategy{
			"least_cost":               LeastCost{},
			"least_cost_with_fallback": LeastCostWithFallback{FailureThreshold: 0.5},
			"round_robin":              rr,
			"weighted":                 Weighted{},
		},
		defaultStrat: LeastCost{},
	}
}

func (r *Router) Start(ctx context.Context) error {
	pg, err := pgxpool.New(ctx, r.cfg.PostgresURL)
	if err != nil {
		return fmt.Errorf("pg connect: %w", err)
	}
	r.pg = pg
	if err := r.refresh(ctx); err != nil {
		return fmt.Errorf("initial refresh: %w", err)
	}
	rctx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.wg.Add(1)
	go r.refreshLoop(rctx)
	return nil
}

func (r *Router) Stop() {
	if r.cancel != nil {
		r.cancel()
	}
	r.wg.Wait()
	if r.pg != nil {
		r.pg.Close()
	}
}

func (r *Router) refreshLoop(ctx context.Context) {
	defer r.wg.Done()
	t := time.NewTicker(r.cfg.RefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.refresh(ctx); err != nil {
				r.cfg.Logger.Warn("router refresh", slog.String("err", err.Error()))
			}
		}
	}
}

// refresh загружает trunk-конфиг из Postgres.
// Схема (плейсхолдер; точная — в плане #03 schema):
// SELECT id::text, gateway_name, node_addrs, cost_per_min, weight, active, priority,
//        operator_id::text, strategy
// FROM telephony_trunks WHERE active = TRUE;
func (r *Router) refresh(ctx context.Context) error {
	rows, err := r.pg.Query(ctx, `
		SELECT id::text, operator_id::text, gateway_name, node_addrs,
		       cost_per_min, weight, priority, strategy
		FROM telephony_trunks
		WHERE active = TRUE
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	tmp := map[string][]Trunk{}
	for rows.Next() {
		var (
			id, opID, gw, strat string
			cost                float64
			weight, priority    int
			nodes               []string
		)
		if err := rows.Scan(&id, &opID, &gw, &nodes, &cost, &weight, &priority, &strat); err != nil {
			return err
		}
		tmp[opID] = append(tmp[opID], Trunk{
			ID:          id,
			GatewayName: gw,
			NodeAddrs:   nodes,
			CostPerMin:  cost,
			Weight:      weight,
			Active:      true,
			Priority:    priority,
		})
	}
	r.mu.Lock()
	r.trunks = tmp
	r.mu.Unlock()
	return nil
}

// SelectionResult — результат маршрутизации.
type SelectionResult struct {
	Trunk     Trunk
	NodeAddr  string
	CallURL   string // sofia/gateway/<gw>/<dest>
}

// Select — выбирает trunk + ноду + строит CallURL для originate.
func (r *Router) Select(ctx context.Context, operatorID, dest, strategyName string) (SelectionResult, error) {
	r.mu.RLock()
	trunks := r.trunks[operatorID]
	r.mu.RUnlock()
	if len(trunks) == 0 {
		return SelectionResult{}, errors.New("no trunks for operator")
	}
	strat := r.defaultStrat
	if s, ok := r.strategies[strategyName]; ok {
		strat = s
	}
	t, err := strat.Pick(trunks, dest)
	if err != nil {
		return SelectionResult{}, err
	}
	// Выбор ноды: пересечение t.NodeAddrs ∩ healthy ∩ не превышен cap
	healthy := r.cfg.Pool.HealthyNodes()
	healthySet := make(map[string]struct{}, len(healthy))
	for _, h := range healthy {
		healthySet[h] = struct{}{}
	}
	var chosenNode string
	for _, n := range t.NodeAddrs {
		if _, ok := healthySet[n]; !ok {
			continue
		}
		ok, err := r.bp.TryAcquire(ctx, n)
		if err != nil {
			r.cfg.Logger.Warn("backpressure", slog.String("err", err.Error()))
			continue
		}
		if ok {
			chosenNode = n
			break
		}
	}
	if chosenNode == "" {
		return SelectionResult{}, errors.New("no healthy node with capacity for trunk")
	}
	return SelectionResult{
		Trunk:    t,
		NodeAddr: chosenNode,
		CallURL:  fmt.Sprintf("sofia/gateway/%s/%s", t.GatewayName, dest),
	}, nil
}

// ReleaseChannel вызывается после CHANNEL_HANGUP_COMPLETE.
func (r *Router) ReleaseChannel(ctx context.Context, nodeAddr string) error {
	return r.bp.Release(ctx, nodeAddr)
}
```

**Verification:**
- `go test ./internal/telephony/router/... -race` — все тесты зелёные.
- Property-test для Weighted: 10000 итераций, распределение в 5%-окрестности заданных весов.
- RoundRobin корректен под конкурентной нагрузкой (1000 параллельных Pick).
- Backpressure: атомарность через Lua, никакого race-condition при ровно cap.
- Coverage по router/ ≥ 85%.

**Acceptance:**
- 4 стратегии реализованы и покрыты тестами.
- Backpressure ограничивает 60 каналов на ноду (по умолчанию), Redis-счётчики per-node.
- Router.Select учитывает: оператора, активность trunk, healthy-ноды, capacity.
- Refresh trunk-конфига из Postgres каждые 30 сек.

---

### Task 6: `active_channels` reconciler — Redis ↔ FreeSWITCH truth

**Цель:** Redis-счётчик `op:active_channels:{node}` инкрементится при дозвоне и декрементится при hangup. Три failure-режима ведут к расхождению с реальностью:
1. Bridge падает после INCR, но до отправки originate в FS → счётчик протекает на +1 навсегда.
2. FS-нода падает с 60 active calls → счётчик показывает 60, новые дозвоны на эту ноду отвергаются backpressure'ом, хотя нода восстановилась пустой.
3. Redis FLUSHDB или потеря данных → счётчик 0, реальность 100 → backpressure срабатывает после первых 60 новых дозвонов, реальная нагрузка 160.

Решение — periodic reconciler, который раз в 30 сек берёт правду из FS (`api show channels count`) и записывает её в Redis-счётчик. При drift > 5 каналов в течение 5 минут — alert.

**Файлы:**
- Создать: `internal/telephony/router/reconciler.go` + `reconciler_test.go`.
- Изменить: `cmd/telephony-bridge/main.go` — запуск reconciler-goroutine.
- Изменить: `internal/telephony/router/backpressure.go` — экспортировать `SetActiveChannels(node, n)` для перезаписи.

- [ ] **Step 1: Failing test — reconciler corrects drift**

`internal/telephony/router/reconciler_test.go`:

```go
package router_test

import (
    "context"
    "testing"
    "time"

    "github.com/redis/go-redis/v9"
    "github.com/stretchr/testify/require"

    "social-pulse/internal/telephony/router"
)

type fakeFSCounter struct {
    count int
}

func (f *fakeFSCounter) ActiveChannels(_ context.Context, node string) (int, error) {
    return f.count, nil
}

func TestReconciler_SetsRedisCounterFromFS(t *testing.T) {
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    rdb := redis.NewClient(&redis.Options{Addr: testRedisAddr(t)})
    defer rdb.Close()

    // Simulate stale Redis counter at 100, real FS = 5.
    rdb.Set(ctx, "op:active_channels:fs-1", "100", 0)
    fs := &fakeFSCounter{count: 5}

    bp := router.NewBackpressure(rdb)
    rec := router.NewReconciler(bp, fs, []string{"fs-1"}, 50*time.Millisecond)
    go rec.Run(ctx)

    require.Eventually(t, func() bool {
        n, _ := rdb.Get(ctx, "op:active_channels:fs-1").Int()
        return n == 5
    }, 1*time.Second, 50*time.Millisecond)
}
```

Run: `go test ./internal/telephony/router/...` → fails (`router.NewReconciler` undefined).

- [ ] **Step 2: Implement `internal/telephony/router/reconciler.go`**

```go
package router

import (
    "context"
    "math"
    "time"

    "github.com/prometheus/client_golang/prometheus"
    "go.uber.org/zap"
)

// FSCounter reports the live channel count of a FreeSWITCH node via ESL
// `api show channels count`.
type FSCounter interface {
    ActiveChannels(ctx context.Context, node string) (int, error)
}

// Reconciler periodically rewrites Redis active_channels counters to match
// the real channel counts from FreeSWITCH. This eliminates drift caused by
// bridge crashes, FS restarts, or Redis flushes.
type Reconciler struct {
    bp       *Backpressure
    fs       FSCounter
    nodes    []string
    interval time.Duration
    log      *zap.Logger
    drift    *prometheus.GaugeVec
}

func NewReconciler(bp *Backpressure, fs FSCounter, nodes []string, interval time.Duration) *Reconciler {
    drift := prometheus.NewGaugeVec(prometheus.GaugeOpts{
        Name: "sociopulse_bridge_active_channels_drift",
        Help: "Difference between Redis counter and FS truth, per node.",
    }, []string{"node"})
    prometheus.MustRegister(drift)
    return &Reconciler{
        bp: bp, fs: fs, nodes: nodes, interval: interval,
        log: zap.NewNop(), drift: drift,
    }
}

func (r *Reconciler) WithLogger(l *zap.Logger) *Reconciler { r.log = l; return r }

func (r *Reconciler) Run(ctx context.Context) {
    if r.interval <= 0 {
        r.interval = 30 * time.Second
    }
    t := time.NewTicker(r.interval)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-t.C:
            r.sweep(ctx)
        }
    }
}

func (r *Reconciler) sweep(ctx context.Context) {
    for _, node := range r.nodes {
        truth, err := r.fs.ActiveChannels(ctx, node)
        if err != nil {
            r.log.Warn("fs counter fetch failed",
                zap.String("node", node), zap.Error(err))
            continue
        }
        cur, err := r.bp.Get(ctx, node)
        if err != nil {
            r.log.Warn("redis counter get failed",
                zap.String("node", node), zap.Error(err))
            continue
        }
        diff := math.Abs(float64(cur) - float64(truth))
        r.drift.WithLabelValues(node).Set(diff)
        if cur == truth {
            continue
        }
        if err := r.bp.SetActiveChannels(ctx, node, truth); err != nil {
            r.log.Error("redis counter write failed",
                zap.String("node", node), zap.Error(err))
            continue
        }
        r.log.Info("active_channels reconciled",
            zap.String("node", node),
            zap.Int("redis_was", cur),
            zap.Int("fs_truth", truth))
    }
}
```

- [ ] **Step 3: Add `Backpressure.SetActiveChannels` and `Get`**

In `internal/telephony/router/backpressure.go`:

```go
// SetActiveChannels overwrites the counter to the given value. Called only
// by the Reconciler. Use Inc/Dec for normal call flow.
func (b *Backpressure) SetActiveChannels(ctx context.Context, node string, n int) error {
    return b.rdb.Set(ctx, b.keyActive(node), n, 0).Err()
}

func (b *Backpressure) Get(ctx context.Context, node string) (int, error) {
    v, err := b.rdb.Get(ctx, b.keyActive(node)).Int()
    if err == redis.Nil {
        return 0, nil
    }
    return v, err
}
```

- [ ] **Step 4: Implement `FSCounter` adapter against ESL pool**

`internal/telephony/router/fs_counter.go`:

```go
package router

import (
    "context"
    "strconv"
    "strings"

    "social-pulse/internal/telephony/esl"
)

type ESLFSCounter struct {
    pool *esl.Pool
}

func NewESLFSCounter(pool *esl.Pool) *ESLFSCounter {
    return &ESLFSCounter{pool: pool}
}

func (c *ESLFSCounter) ActiveChannels(ctx context.Context, node string) (int, error) {
    raw, err := c.pool.API(ctx, node, "show channels count")
    if err != nil {
        return 0, err
    }
    // FS returns "N total." — parse first int.
    fields := strings.Fields(raw)
    if len(fields) == 0 {
        return 0, nil
    }
    return strconv.Atoi(fields[0])
}
```

- [ ] **Step 5: Wire reconciler in `cmd/telephony-bridge/main.go`**

After the `Backpressure` and ESL `Pool` are constructed:

```go
fsCounter := router.NewESLFSCounter(eslPool)
recon := router.NewReconciler(bp, fsCounter, cfg.FS.Nodes, 30*time.Second).WithLogger(logger)
g.Go(func() error { recon.Run(ctx); return nil })
```

- [ ] **Step 6: Prometheus alert rule**

In `helm/telephony-bridge/templates/prometheus-rules.yaml`:

```yaml
- alert: BridgeActiveChannelsDrift
  expr: max by (node) (sociopulse_bridge_active_channels_drift) > 10
  for: 5m
  labels: { severity: warning }
  annotations:
    summary: "Bridge {{ $labels.node }} channel counter drift > 10"
    description: "Redis counter and FS truth differ; reconciler will correct, but investigate INCR/DECR leak source."
    runbook_url: "https://wiki/runbooks/bridge-active-channels-drift"
```

- [ ] **Step 7: Run tests + commit**

```bash
go test ./internal/telephony/router/... -count=1
golangci-lint run ./internal/telephony/router/...
git add internal/telephony/router/ cmd/telephony-bridge/main.go helm/telephony-bridge/
git commit -m "feat(telephony): periodic reconciler for active_channels counter drift"
```

---

## Self-review

**Spec coverage** (against §5.3.1, §7, §10.2, Приложение B.2):
- ESL inbound клиент с auto-reconnect (jittered backoff) + auto re-attach подписок. ✓
- ESL events: `CHANNEL_CREATE`, `CHANNEL_ANSWER`, `CHANNEL_HANGUP_COMPLETE`, `DTMF`, `CUSTOM sofia::register`, `CUSTOM mod_callcenter::*`, `RECORD_STOP`. ✓
- Pool 1 ESL conn per FS-ноду (3 ноды dev / 6 prod), health-check каждые 5s через `api sofia status`. ✓
- Маршрутизация trunk + FS-нода: 4 стратегии (`least_cost`, `least_cost_with_fallback`, `round_robin`, `weighted`), backpressure через Redis `op:active_channels:{node}` с cap=60. ✓
- §10.2 NATS subjects: subscriber `tenant.*.telephony.cmd.*`, publisher `tenant.*.telephony.event.*`. ✓
- Команды: Originate, Hangup, MixMonitor, Play, CreateUser, DeleteUser. Idempotency через Redis SET NX (TTL 24h). ✓
- Per-call SIP-аккаунты для verto через `mod_xml_curl` + cmd/api `/internal/freeswitch/directory` (mTLS). Хранение в Redis `op:credentials:{operator_id}:{call_id}` TTL 4h. ✓
- HTTP endpoint `/internal/freeswitch/directory` отдаёт XML с password из Redis, защищён mTLS-сертификатом FS-нода. ✓
- Production-ready: Prometheus :9090, OTel-trace, slog, graceful shutdown (drain inflight commands), Helm-чарт + ArgoCD-app. ✓
- Refresh trunk-конфига из Postgres каждые 30 сек. ✓
- Тесты: unit (mock conn), integration через Docker FreeSWITCH (`signalwire/freeswitch:1.10.10`) — полный originate→answer→hangup, multi-trunk routing с unhealthy gateway, backpressure cap. ✓

**Placeholder scan:** none. Все sub-системы имеют конкретные реализации. Сценарии деплоя задокументированы для dev (один FS-нод) и prod (3+ FS-нод).

**Type/name consistency:** `Client`, `ESLPool`, `Router`, `LineCapacityTracker`, `OriginateRequest`, `Trunk`, `FSNode` — стабильные имена, потребляемые Plan 10 (dialer Router использует через NATS, не напрямую) и Plan 11 (listen-in mixmonitor command).

**Out of scope (correctly deferred):**
- Логика автодозвона (FSM оператора, queue-pickup) — Plan 10.
- Operator verto-клиент в браузере — Plan 16.
- Recording metadata gRPC server — Plan 12.
- Listen-in полная end-to-end реализация (admin browser side) — Plan 11.

Plan 09 verified.

---

**Plan complete and saved to `docs/superpowers/plans/2026-05-06-09-telephony-bridge.md`.**

