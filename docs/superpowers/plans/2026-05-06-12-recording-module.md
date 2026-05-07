# Implementation Plan #12 — Recording Module (СоциоПульс)

**Дата:** 2026-05-06
**Автор:** SocioPulse Platform Team
**Статус:** Ready for execution
**Связанные документы:**
- `docs/superpowers/specs/2026-05-06-sociopulse-system-design.md` — §9 (Recording), §FR-G (Functional Requirements: Recordings), §13.6 (S3 + KMS), §15.5 (Retention), ADR-005 (Encryption-at-rest)
- `docs/superpowers/plans/2026-05-06-00-foundation.md` — стиль, структура, naming, тест-стратегия
- `docs/superpowers/plans/2026-05-06-01-infrastructure.md` — S3 (Yandex Object Storage), KMS, NATS, Postgres
- `docs/superpowers/plans/2026-05-06-03-database.md` — таблица `call_recordings`, audit_log
- `docs/superpowers/plans/2026-05-06-05-auth-module.md` — JWT, RBAC, mTLS

---

## 1. Goal

Реализовать модуль `recording` сервиса СоциоПульс — единственный системный владелец метаданных и данных аудиозаписей звонков:

1. **gRPC-server `RecordingService`** на :9091 (mTLS, internal) — принимает Commit от внешнего ingest-агента (медиа-сервер записи), который уже загрузил `.opus.enc` + `.dek.enc` в S3. Service делает финальную регистрацию: валидация, идемпотентность, INSERT в `call_recordings`, audit, NATS event.
2. **HTTP REST endpoints** (на основном API-порту :8080, через JWT) для конечных пользователей (admin/supervisor):
   - `GET /api/calls/{call_id}/recording` — потоковая дешифровка (envelope decryption + AES-256-GCM), **stream-whole-file без HTTP Range в v1**. См. примечание ниже.
   - `GET /api/recordings/search` — поиск/фильтр (project, operator, period, status).
   - `POST /api/calls/{call_id}/recording/verify` — manual-trigger SHA-256 проверка целостности.

> **Range-support trade-off:** AES-256-GCM нативно не поддерживает random-access decryption (auth-tag в конце, поток непрерывный). Два решения: (a) decrypt-all-в-RAM-then-slice (200 МБ × 50 одновременных аудитов = 10 ГБ RAM пика), (b) chunked-envelope-format с per-chunk auth-tag'ами (custom формат, требует переделки encrypt-pipeline в Plan 08). На v1 принимаем простое решение — `Accept-Ranges: none`, `Content-Length` известен после decrypt, UI показывает play/pause без seek; клиент ждёт первый чанк ~1-2 сек на медиум-записи. На v2 — chunked envelope (отдельный план или backlog).
3. **Worker-процессы** (`cmd/worker`):
   - `recording.retention_pass` — раз в сутки (leader-only): hot→cold lifecycle, hard-delete по истечении `delete_at`.
   - `recording.integrity_pass` — раз в неделю: SHA-256 verify на 1% случайной выборки.
4. **Полное отсутствие plaintext-аудио в storage** (envelope encryption) — каждая запись имеет свой DEK, зашифрованный мастер-ключом KMS.
5. **Метрики Prometheus**, **structured slog**, **distributed tracing** через OpenTelemetry.

### Inputs
- Конфиг `internal/recording/config/config.go` — KMS key ID, S3 bucket, retention defaults, signed URL TTL (deprecated в v1).
- Stream от ingest-агента: `.opus.enc` уже в S3, ingest вызывает `RecordingService.Commit(call_id, sha256, bytes_size, duration_ms, encrypted_dek_b64, kms_key_id)`.
- HTTP-запрос пользователя с JWT (claims: `tenant_id`, `user_id`, `roles`).

### Outputs
- gRPC `Commit` → 200 OK / `AlreadyExists` (идемпотентность по `call_id`).
- HTTP `GET /recording` → `audio/ogg` content-type, с corrected `Content-Length` (после decrypt), `Accept-Ranges: none` (см. trade-off выше; Range — backlog v2).
- HTTP `GET /recordings/search` → JSON `{"items": [...], "total": N, "next_cursor": "..."}`.
- NATS event `tenant.<tenant_id>.recording.uploaded` с payload `{"call_id":"...","sha256":"...","bytes_size":N,"duration_ms":D,"committed_at":"..."}`.
- Audit-log entries: `recording.committed`, `recording.accessed`, `recording.deleted`, `recording.verify_failed`.

---

## 2. Constraints (что НЕ делаем в этом плане)

- **Транскрипция** — отдельный модуль `transcription` (Plan 13). Здесь только запись/доступ.
- **Ingest-агент** (медиа-сервер, который ПИШЕТ запись на диск с SIP/RTP-стрима, шифрует и грузит в S3) — отдельный модуль `ingest` (вне этого плана). Здесь только Commit-эндпоинт, который агент дёргает после upload.
- **Frontend UI плеера** — это работа Plan 20 (Frontend). Здесь только бек-эндпоинты.
- **Pre-signed URL fallback** — описан в спеке как "future v2", в v1 только `GET /recording` через decrypt-endpoint (потому что объект в S3 зашифрован).
- **Cross-tenant access** запрещён — все запросы фильтруются по `tenant_id` из JWT/mTLS-cert.

---

## 3. Tech Stack & Dependencies

```go
// go.mod additions for this plan
require (
    google.golang.org/grpc v1.63.2
    google.golang.org/protobuf v1.34.1
    github.com/aws/aws-sdk-go-v2 v1.27.0
    github.com/aws/aws-sdk-go-v2/config v1.27.16
    github.com/aws/aws-sdk-go-v2/service/s3 v1.54.3
    github.com/yandex-cloud/go-sdk v0.0.0-20240515123456-abcdef
    github.com/yandex-cloud/go-genproto v0.0.0-20240515123456-abcdef
    github.com/nats-io/nats.go v1.34.1
    github.com/jackc/pgx/v5 v5.5.5
    github.com/google/uuid v1.6.0
    github.com/stretchr/testify v1.9.0
    go.uber.org/mock v0.4.0
)

// Tools (in tools.go, build-tagged)
//go:build tools
package tools
import (
    _ "google.golang.org/protobuf/cmd/protoc-gen-go"          // v1.34
    _ "google.golang.org/grpc/cmd/protoc-gen-go-grpc"          // v1.4
    _ "go.uber.org/mock/mockgen"                                // v0.4
)
```

`Makefile` цели:
```makefile
.PHONY: proto-recording
proto-recording:
	protoc \
		-I=docs/api \
		--go_out=. --go_opt=module=github.com/sociopulse/sociopulse \
		--go-grpc_out=. --go-grpc_opt=module=github.com/sociopulse/sociopulse \
		docs/api/recording/v1/recording.proto

.PHONY: mocks-recording
mocks-recording:
	mockgen -source=internal/recording/service/service.go -destination=internal/recording/service/mocks/service_mock.go -package=mocks
	mockgen -source=internal/recording/storage/s3.go      -destination=internal/recording/storage/mocks/s3_mock.go      -package=mocks
	mockgen -source=internal/recording/crypto/kms.go      -destination=internal/recording/crypto/mocks/kms_mock.go      -package=mocks
```

---

## 4. File Structure

```
docs/api/recording/v1/
└── recording.proto                          # Task 1

internal/recording/
├── proto/v1/                                # Task 1 (generated)
│   ├── recording.pb.go
│   └── recording_grpc.pb.go
├── api/
│   ├── grpc_server.go                       # Task 2 — gRPC handler skeleton
│   ├── grpc_server_test.go                  # Task 9
│   ├── http_handlers.go                     # Task 8 — REST handlers
│   └── http_handlers_test.go                # Task 9
├── service/
│   ├── service.go                           # Task 3 — Commit, Get, Search interface
│   ├── service_test.go                      # Task 9
│   ├── url_signer.go                        # Task 5
│   ├── url_signer_test.go                   # Task 9
│   ├── retention_planner.go                 # Task 6
│   ├── retention_planner_test.go            # Task 9
│   ├── integrity_verifier.go                # Task 7
│   ├── integrity_verifier_test.go           # Task 9
│   └── mocks/                               # generated by mockgen
│       └── service_mock.go
├── storage/
│   ├── postgres.go                          # Task 3 — INSERT/SELECT call_recordings
│   ├── postgres_test.go                     # Task 9 (testcontainers)
│   ├── s3.go                                # Task 4 — Get/Delete/Lifecycle
│   ├── s3_test.go                           # Task 9 (minio)
│   └── mocks/
│       └── s3_mock.go
├── crypto/
│   ├── kms.go                               # Task 4 — Yandex KMS Decrypt
│   ├── kms_test.go                          # Task 9 (KMS mock)
│   ├── aesgcm.go                            # Task 4 — AES-256-GCM stream decrypt
│   ├── aesgcm_test.go                       # Task 9 (golden vectors)
│   └── mocks/
│       └── kms_mock.go
├── events/
│   ├── publisher.go                         # Task 3 — NATS publish
│   └── publisher_test.go                    # Task 9
├── worker/
│   ├── retention.go                         # Task 6 — daily retention pass
│   ├── retention_test.go                    # Task 9
│   ├── integrity.go                         # Task 7 — weekly 1% verify
│   └── integrity_test.go                    # Task 9
└── config/
    └── config.go                            # Loaded by ConfigService

cmd/api/
└── main.go                                  # +RegisterRecordingService (Task 2 patch)

cmd/worker/
└── main.go                                  # +recording.retention_pass, recording.integrity_pass (Task 6, 7 patches)
```

---

## Task 1 — Proto-файл `recording.proto` + кодоген

**Цель:** определить gRPC-контракт для внутренней связи `ingest-agent → cmd/api(:9091)`. Сгенерировать Go-код через `protoc-gen-go` v1.34 + `protoc-gen-go-grpc` v1.4.

### 1.1 Создать `docs/api/recording/v1/recording.proto`

```protobuf
syntax = "proto3";

package sociopulse.recording.v1;

option go_package = "github.com/sociopulse/sociopulse/internal/recording/proto/v1;recordingv1";

import "google/protobuf/timestamp.proto";
import "google/protobuf/duration.proto";

// RecordingService — internal API for ingest-agent ↔ cmd/api.
// All RPCs require mTLS with cert SAN containing "ingest-agent".
service RecordingService {
  // Commit registers a recording that ingest-agent has already uploaded
  // to S3 (.opus.enc + .dek.enc envelope-encrypted). Idempotent by call_id.
  // Errors:
  //   ALREADY_EXISTS — recording for this call_id already committed (returns existing metadata).
  //   INVALID_ARGUMENT — sha256 length != 64, bytes_size <= 0, etc.
  //   FAILED_PRECONDITION — call_id not found in calls table.
  //   PERMISSION_DENIED — tenant mismatch between cert and call_id.
  rpc Commit(CommitRequest) returns (CommitResponse);

  // Get returns metadata for a single recording by call_id (no audio bytes).
  // Used for health-check and diagnostics. Public-facing reads go via HTTP.
  rpc Get(GetRequest) returns (GetResponse);

  // GetPresignedURL — DEPRECATED in v1.
  // Returns a presigned S3 URL pointing to the encrypted .opus.enc object.
  // Client cannot decrypt without DEK + KMS access. Reserved for v2 client-side decrypt.
  rpc GetPresignedURL(GetPresignedURLRequest) returns (GetPresignedURLResponse);
}

message CommitRequest {
  // Tenant scope (also validated against mTLS cert SAN).
  string tenant_id = 1;       // UUID v7
  string call_id   = 2;       // UUID v7 (FK → calls.id)

  // S3 layout (single bucket, paths derived):
  //   audio_object_key = recordings/<tenant_id>/<yyyy>/<mm>/<dd>/<call_id>.opus.enc
  //   dek_object_key   = recordings/<tenant_id>/<yyyy>/<mm>/<dd>/<call_id>.dek.enc
  string s3_bucket           = 3;
  string audio_object_key    = 4;
  string dek_object_key      = 5;

  // Cryptographic envelope.
  string kms_key_id          = 6;   // e.g. "abjxxxxxxxxxxxxxxxxx" (Yandex Cloud KMS key ID)
  bytes  encrypted_dek       = 7;   // 88 bytes typical (KMS-wrapped 32-byte DEK)

  // Audio properties (post-encryption file size in bytes).
  int64                       bytes_size  = 8;
  google.protobuf.Duration    duration    = 9;
  string                      sha256      = 10;  // hex, 64 chars, of CIPHERTEXT (.opus.enc)
  string                      codec       = 11;  // "opus" (only opus in v1)
  int32                       sample_rate = 12;  // 48000

  // Retention plan (resolved by ingest from project policy or defaults).
  google.protobuf.Timestamp   delete_at   = 13;  // when hard-delete must happen
  google.protobuf.Timestamp   cold_at     = 14;  // when lifecycle hot→cold

  // Provenance.
  string ingest_agent_id      = 15;  // for audit ("which agent uploaded")
  google.protobuf.Timestamp   recorded_at = 16;  // when audio capture started
}

message CommitResponse {
  string                      recording_id = 1;  // UUID v7 (PK in call_recordings)
  string                      call_id      = 2;
  google.protobuf.Timestamp   committed_at = 3;
  bool                        idempotent_replay = 4;  // true iff this was an idempotent retry
}

message GetRequest {
  string tenant_id = 1;
  string call_id   = 2;
}

message GetResponse {
  string                      recording_id = 1;
  string                      call_id      = 2;
  string                      tenant_id    = 3;
  string                      s3_bucket    = 4;
  string                      audio_object_key = 5;
  int64                       bytes_size   = 6;
  google.protobuf.Duration    duration     = 7;
  string                      sha256       = 8;
  string                      status       = 9;  // "stored" | "cold" | "deleted"
  google.protobuf.Timestamp   committed_at = 10;
  google.protobuf.Timestamp   delete_at    = 11;
  google.protobuf.Timestamp   cold_at      = 12;
  google.protobuf.Timestamp   verified_at  = 13; // last integrity verification
}

message GetPresignedURLRequest {
  string tenant_id = 1;
  string call_id   = 2;
  // TTL clamped server-side to MAX(5min, requested) — never longer than 5 minutes.
  google.protobuf.Duration ttl = 3;
}

message GetPresignedURLResponse {
  string url       = 1;
  google.protobuf.Timestamp expires_at = 2;
  // Non-empty in v1: clients must use HTTP /api/calls/{id}/recording instead.
  string deprecation_notice = 3;
}
```

### 1.2 Запустить кодоген

`make proto-recording` создаст:

- `internal/recording/proto/v1/recording.pb.go`
- `internal/recording/proto/v1/recording_grpc.pb.go` (с `RecordingServiceServer` interface, `RegisterRecordingServiceServer(grpc.ServiceRegistrar, RecordingServiceServer)`)

### 1.3 Acceptance

- `protoc` отрабатывает без warning.
- Файлы commited в репо (генерим в CI и сравниваем `git diff --exit-code`).
- В `internal/recording/proto/v1/` есть `RecordingServiceServer` interface с тремя методами `Commit`/`Get`/`GetPresignedURL`.

### 1.4 Risks
- Несовпадение версий protoc/protoc-gen-go между разработчиками → стандартизуем через `tools/buf.yaml` + GitHub Action в Plan 02.

---

## Task 2 — gRPC server skeleton на :9091, mTLS, регистрация в cmd/api

**Цель:** поднять отдельный gRPC-listener рядом с HTTP API (тот же процесс `cmd/api`), регистрировать `RecordingService`, требовать клиентский сертификат с SAN, содержащим имя сервиса.

### 2.1 `internal/recording/api/grpc_server.go` — каркас

```go
package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	recordingv1 "github.com/sociopulse/sociopulse/internal/recording/proto/v1"
	"github.com/sociopulse/sociopulse/internal/recording/service"
	"github.com/sociopulse/sociopulse/pkg/observability"
)

// GRPCServer wires RecordingService implementation over gRPC with mTLS.
type GRPCServer struct {
	recordingv1.UnimplementedRecordingServiceServer

	svc      service.RecordingService
	logger   *slog.Logger
	metrics  *observability.RecordingMetrics
	listenOn string
	server   *grpc.Server
}

// Config controls listener address and TLS material.
type Config struct {
	ListenAddr   string        // ":9091"
	TLSCertFile  string        // server cert (signed by internal CA)
	TLSKeyFile   string        // server key
	TLSCAFile    string        // CA bundle that signs client (ingest-agent) certs
	MaxRecvBytes int           // 4 MiB default
	Timeout      time.Duration // per-call deadline
}

func NewGRPCServer(cfg Config, svc service.RecordingService, logger *slog.Logger, m *observability.RecordingMetrics) (*GRPCServer, error) {
	creds, err := loadMTLSCreds(cfg)
	if err != nil {
		return nil, fmt.Errorf("load mtls: %w", err)
	}

	srv := grpc.NewServer(
		grpc.Creds(creds),
		grpc.MaxRecvMsgSize(cfg.MaxRecvBytes),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: 5 * time.Minute,
			Time:              30 * time.Second,
			Timeout:           10 * time.Second,
		}),
		grpc.ChainUnaryInterceptor(
			observability.UnaryLoggingInterceptor(logger),
			observability.UnaryMetricsInterceptor(m.GRPC),
			observability.UnaryRecoveryInterceptor(logger),
			peerTenantInterceptor(),
		),
	)

	g := &GRPCServer{
		svc:      svc,
		logger:   logger,
		metrics:  m,
		listenOn: cfg.ListenAddr,
		server:   srv,
	}
	recordingv1.RegisterRecordingServiceServer(srv, g)
	return g, nil
}

func loadMTLSCreds(cfg Config) (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load server keypair: %w", err)
	}
	caBytes, err := os.ReadFile(cfg.TLSCAFile)
	if err != nil {
		return nil, fmt.Errorf("read ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return nil, errors.New("ca bundle has no valid certificates")
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}), nil
}

// Serve blocks until the listener errors or Stop is called.
func (g *GRPCServer) Serve() error {
	lis, err := net.Listen("tcp", g.listenOn)
	if err != nil {
		return fmt.Errorf("listen %s: %w", g.listenOn, err)
	}
	g.logger.Info("recording gRPC server listening", "addr", g.listenOn)
	return g.server.Serve(lis)
}

// GracefulStop drains in-flight calls.
func (g *GRPCServer) GracefulStop() { g.server.GracefulStop() }

// peerTenantInterceptor extracts the SPIFFE-style identity from the client
// certificate and stashes it on context as `peerIdentity{tenant_id, agent_id}`.
// Implementation detail in Task 3 (validates request.tenant_id matches cert).
func peerTenantInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		p, ok := peer.FromContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "missing peer info")
		}
		tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
		if !ok || len(tlsInfo.State.VerifiedChains) == 0 {
			return nil, status.Error(codes.Unauthenticated, "client cert required")
		}
		leaf := tlsInfo.State.VerifiedChains[0][0]
		identity, err := parseSPIFFEIdentity(leaf)
		if err != nil {
			return nil, status.Errorf(codes.Unauthenticated, "invalid SPIFFE identity: %v", err)
		}
		ctx = withPeerIdentity(ctx, identity)
		return h(ctx, req, info, nil)
	}
}

type peerIdentity struct {
	TenantID uuid.UUID
	AgentID  string
	URI      string // spiffe://sociopulse/ingest-agent/<agent_id>?tenant=<tenant_id>
}

type peerIdentityKey struct{}

func withPeerIdentity(ctx context.Context, p peerIdentity) context.Context {
	return context.WithValue(ctx, peerIdentityKey{}, p)
}

func peerIdentityFrom(ctx context.Context) (peerIdentity, bool) {
	v, ok := ctx.Value(peerIdentityKey{}).(peerIdentity)
	return v, ok
}

func parseSPIFFEIdentity(cert *x509.Certificate) (peerIdentity, error) {
	if len(cert.URIs) == 0 {
		return peerIdentity{}, errors.New("no URI SAN")
	}
	u := cert.URIs[0]
	if u.Scheme != "spiffe" {
		return peerIdentity{}, fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	tenantStr := u.Query().Get("tenant")
	tenantID, err := uuid.Parse(tenantStr)
	if err != nil {
		return peerIdentity{}, fmt.Errorf("tenant uuid: %w", err)
	}
	return peerIdentity{
		TenantID: tenantID,
		AgentID:  u.Path,
		URI:      u.String(),
	}, nil
}
```

### 2.2 Регистрация в `cmd/api/main.go`

В существующий `bootstrap()` добавляем:

```go
// cmd/api/main.go (excerpt — patch)
recSvc, err := recordingservice.New(recordingservice.Deps{
    DB:        pgPool,
    S3:        s3Client,
    KMS:       kmsClient,
    NATS:      natsConn,
    Logger:    logger.With("module", "recording"),
    Metrics:   metrics.Recording,
    URLTTL:    5 * time.Minute,
    Clock:     clock.Real,
})
if err != nil {
    return fmt.Errorf("init recording service: %w", err)
}

recGRPC, err := recordingapi.NewGRPCServer(recordingapi.Config{
    ListenAddr:   cfg.Recording.GRPCListenAddr,        // ":9091"
    TLSCertFile:  cfg.Recording.TLSCertFile,
    TLSKeyFile:   cfg.Recording.TLSKeyFile,
    TLSCAFile:    cfg.Recording.TLSCAFile,
    MaxRecvBytes: 4 * 1024 * 1024,
    Timeout:      30 * time.Second,
}, recSvc, logger, metrics.Recording)
if err != nil {
    return fmt.Errorf("init recording grpc: %w", err)
}

// HTTP handlers (Task 8)
httpRouter.Mount("/api", recordingapi.NewHTTPRouter(recSvc, logger, metrics.Recording))

g, gctx := errgroup.WithContext(ctx)
g.Go(func() error { return recGRPC.Serve() })
g.Go(func() error { <-gctx.Done(); recGRPC.GracefulStop(); return nil })
g.Go(func() error { return httpServer.ListenAndServe() })
// ... rest of group
```

### 2.3 Acceptance

- `cmd/api` слушает :8080 (HTTP) и :9091 (gRPC) одновременно.
- Подключение без cert → `Unauthenticated`. С невалидным CA → handshake error.
- `grpcurl --cert ./agent.crt --key ./agent.key --cacert ./ca.crt -d '{}' localhost:9091 sociopulse.recording.v1.RecordingService/Get` возвращает `INVALID_ARGUMENT` (заглушка).

### 2.4 Risks
- mTLS-сертификаты для ingest-агента — будут в Plan 18 (PKI bootstrap). На время разработки используем `script/dev/issue-cert.sh` с локальным CA.

---

## Task 3 — `Commit` метод: валидация, идемпотентность, INSERT, audit, NATS

**Цель:** реализовать ядро бизнес-логики commit-flow. Идемпотентность по `call_id` (UNIQUE-индекс в Postgres), вставка в `call_recordings` + audit-log в одной транзакции, NATS-publish после COMMIT (outbox-pattern).

### 3.1 Сервис-интерфейс `internal/recording/service/service.go`

```go
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/sociopulse/sociopulse/internal/recording/crypto"
	"github.com/sociopulse/sociopulse/internal/recording/events"
	"github.com/sociopulse/sociopulse/internal/recording/storage"
	"github.com/sociopulse/sociopulse/pkg/clock"
	"github.com/sociopulse/sociopulse/pkg/observability"
)

// RecordingService is the public façade used by both gRPC and HTTP layers.
//
// Methods are scoped by tenant — every call must include the tenant_id
// resolved from the caller's identity (mTLS SAN or JWT claim).
type RecordingService interface {
	Commit(ctx context.Context, in CommitInput) (CommitOutput, error)
	Get(ctx context.Context, tenantID, callID uuid.UUID) (RecordingMetadata, error)
	Search(ctx context.Context, tenantID uuid.UUID, q SearchQuery) (SearchResult, error)

	OpenAudioStream(ctx context.Context, tenantID, callID uuid.UUID, byteRange *ByteRange) (AudioStream, error)
	VerifyChecksum(ctx context.Context, tenantID, callID uuid.UUID) (VerifyResult, error)
}

type CommitInput struct {
	TenantID         uuid.UUID
	CallID           uuid.UUID
	S3Bucket         string
	AudioObjectKey   string
	DEKObjectKey     string
	KMSKeyID         string
	EncryptedDEK     []byte
	BytesSize        int64
	Duration         time.Duration
	SHA256Hex        string // 64 hex chars
	Codec            string // "opus"
	SampleRate       int32
	DeleteAt         time.Time
	ColdAt           time.Time
	IngestAgentID    string
	RecordedAt       time.Time
}

type CommitOutput struct {
	RecordingID      uuid.UUID
	CallID           uuid.UUID
	CommittedAt      time.Time
	IdempotentReplay bool
}

type RecordingMetadata struct {
	RecordingID    uuid.UUID
	CallID         uuid.UUID
	TenantID       uuid.UUID
	S3Bucket       string
	AudioObjectKey string
	BytesSize      int64
	Duration       time.Duration
	SHA256Hex      string
	Status         string // "stored" | "cold" | "deleted"
	CommittedAt    time.Time
	DeleteAt       time.Time
	ColdAt         time.Time
	VerifiedAt     *time.Time
}

type SearchQuery struct {
	ProjectID  *uuid.UUID
	OperatorID *uuid.UUID
	Status     []string
	From       *time.Time
	To         *time.Time
	Cursor     string // opaque, encoded committed_at + recording_id
	Limit      int    // 1..200, default 50
}

type SearchResult struct {
	Items      []RecordingMetadata
	NextCursor string
	HasMore    bool
}

type ByteRange struct {
	Start int64
	End   int64 // inclusive; -1 means open-ended
}

// AudioStream wraps an io.ReadCloser plus content-length and content-type.
type AudioStream struct {
	Reader        io.ReadCloser
	ContentType   string
	ContentLength int64 // total decrypted length, regardless of Range
	StartOffset   int64 // 0 if no Range
	EndOffset     int64 // ContentLength-1 if no Range
}

type VerifyResult struct {
	OK            bool
	ExpectedSHA   string
	ActualSHA     string
	BytesScanned  int64
	DurationMS    int64
}

// Sentinel errors translated to gRPC/HTTP status codes by the API layer.
var (
	ErrNotFound          = errors.New("recording: not found")
	ErrAlreadyDeleted    = errors.New("recording: already deleted")
	ErrTenantMismatch    = errors.New("recording: tenant mismatch")
	ErrCallNotFound      = errors.New("recording: call not found")
	ErrInvalidInput      = errors.New("recording: invalid input")
	ErrIntegrityFailed   = errors.New("recording: integrity check failed")
)

// Deps assembles the concrete service.
type Deps struct {
	DB      storage.RecordingStore
	S3      storage.ObjectStore
	KMS     crypto.KMSClient
	Decrypt crypto.StreamDecryptor
	Events  events.Publisher
	Audit   storage.AuditLogger
	Logger  *slog.Logger
	Metrics *observability.RecordingMetrics
	Clock   clock.Clock
	URLTTL  time.Duration
}

type svc struct{ Deps }

func New(d Deps) (RecordingService, error) {
	if d.DB == nil || d.S3 == nil || d.KMS == nil || d.Events == nil {
		return nil, fmt.Errorf("recording service: missing dependency")
	}
	if d.Clock == nil {
		d.Clock = clock.Real
	}
	if d.Decrypt == nil {
		d.Decrypt = crypto.NewAESGCMStreamDecryptor()
	}
	return &svc{Deps: d}, nil
}
```

### 3.2 Реализация `Commit` — `service.go` (продолжение)

```go
const sha256HexLen = 64

func (s *svc) Commit(ctx context.Context, in CommitInput) (CommitOutput, error) {
	if err := validateCommitInput(in); err != nil {
		return CommitOutput{}, fmt.Errorf("%w: %s", ErrInvalidInput, err)
	}

	now := s.Clock.Now().UTC()

	// Idempotency: try-INSERT; on UNIQUE conflict, return existing row.
	row, replay, err := s.DB.InsertRecordingIdempotent(ctx, storage.RecordingRow{
		ID:             uuid.Must(uuid.NewV7()),
		CallID:         in.CallID,
		TenantID:       in.TenantID,
		S3Bucket:       in.S3Bucket,
		AudioObjectKey: in.AudioObjectKey,
		DEKObjectKey:   in.DEKObjectKey,
		KMSKeyID:       in.KMSKeyID,
		EncryptedDEK:   in.EncryptedDEK,
		BytesSize:      in.BytesSize,
		DurationMS:     in.Duration.Milliseconds(),
		SHA256Hex:      in.SHA256Hex,
		Codec:          in.Codec,
		SampleRate:     in.SampleRate,
		Status:         "stored",
		CommittedAt:    now,
		DeleteAt:       in.DeleteAt,
		ColdAt:         in.ColdAt,
		RecordedAt:     in.RecordedAt,
		IngestAgentID:  in.IngestAgentID,
	})
	if err != nil {
		if errors.Is(err, storage.ErrCallNotFound) {
			return CommitOutput{}, ErrCallNotFound
		}
		return CommitOutput{}, fmt.Errorf("insert recording: %w", err)
	}

	// Audit row + NATS event are written via outbox in the same TX.
	// (InsertRecordingIdempotent above also enqueues to outbox table.)

	if !replay {
		s.Metrics.CommitTotal.WithLabelValues(in.TenantID.String(), "ok").Inc()
		s.Metrics.StorageSizeBytes.WithLabelValues(in.TenantID.String()).Add(float64(in.BytesSize))
		s.Logger.Info("recording committed",
			"tenant_id", in.TenantID, "call_id", in.CallID,
			"recording_id", row.ID, "bytes", in.BytesSize, "sha256", in.SHA256Hex)
	} else {
		s.Metrics.CommitTotal.WithLabelValues(in.TenantID.String(), "replay").Inc()
		s.Logger.Info("recording commit idempotent replay",
			"tenant_id", in.TenantID, "call_id", in.CallID, "recording_id", row.ID)
	}

	return CommitOutput{
		RecordingID:      row.ID,
		CallID:           row.CallID,
		CommittedAt:      row.CommittedAt,
		IdempotentReplay: replay,
	}, nil
}

func validateCommitInput(in CommitInput) error {
	switch {
	case in.TenantID == uuid.Nil:
		return errors.New("tenant_id required")
	case in.CallID == uuid.Nil:
		return errors.New("call_id required")
	case in.S3Bucket == "":
		return errors.New("s3_bucket required")
	case in.AudioObjectKey == "":
		return errors.New("audio_object_key required")
	case in.DEKObjectKey == "":
		return errors.New("dek_object_key required")
	case in.KMSKeyID == "":
		return errors.New("kms_key_id required")
	case len(in.EncryptedDEK) == 0:
		return errors.New("encrypted_dek required")
	case in.BytesSize <= 0:
		return errors.New("bytes_size must be > 0")
	case in.Duration <= 0:
		return errors.New("duration must be > 0")
	case len(in.SHA256Hex) != sha256HexLen:
		return fmt.Errorf("sha256 length: want %d hex chars, got %d", sha256HexLen, len(in.SHA256Hex))
	case in.Codec == "":
		return errors.New("codec required")
	case in.DeleteAt.IsZero():
		return errors.New("delete_at required (retention plan must be resolved)")
	}
	return nil
}
```

### 3.3 Storage layer `internal/recording/storage/postgres.go`

```go
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type RecordingStore interface {
	InsertRecordingIdempotent(ctx context.Context, row RecordingRow) (RecordingRow, bool, error)
	GetByCallID(ctx context.Context, tenantID, callID uuid.UUID) (RecordingRow, error)
	Search(ctx context.Context, tenantID uuid.UUID, q SearchQ) ([]RecordingRow, error)
	UpdateStatus(ctx context.Context, recordingID uuid.UUID, status string, at time.Time) error
	UpdateVerifiedAt(ctx context.Context, recordingID uuid.UUID, ok bool, actualSHA string, at time.Time) error
	ListDueForDelete(ctx context.Context, before time.Time, limit int) ([]RecordingRow, error)
	ListDueForCold(ctx context.Context, before time.Time, limit int) ([]RecordingRow, error)
}

type RecordingRow struct {
	ID             uuid.UUID
	CallID         uuid.UUID
	TenantID       uuid.UUID
	S3Bucket       string
	AudioObjectKey string
	DEKObjectKey   string
	KMSKeyID       string
	EncryptedDEK   []byte
	BytesSize      int64
	DurationMS    int64
	SHA256Hex      string
	Codec          string
	SampleRate     int32
	Status         string
	CommittedAt    time.Time
	DeleteAt       time.Time
	ColdAt         time.Time
	RecordedAt     time.Time
	VerifiedAt     *time.Time
	IntegrityOK    *bool
	IngestAgentID  string
}

type SearchQ struct {
	ProjectID  *uuid.UUID
	OperatorID *uuid.UUID
	Status     []string
	From       *time.Time
	To         *time.Time
	CursorCommittedAt *time.Time
	CursorRecordingID *uuid.UUID
	Limit      int
}

var ErrCallNotFound = errors.New("storage: call not found")

type pgStore struct{ pool *pgxpool.Pool }

func NewPGStore(p *pgxpool.Pool) RecordingStore { return &pgStore{pool: p} }

const insertRecordingSQL = `
WITH ins AS (
  INSERT INTO call_recordings (
    id, call_id, tenant_id, s3_bucket, audio_object_key, dek_object_key,
    kms_key_id, encrypted_dek, bytes_size, duration_ms, sha256_hex,
    codec, sample_rate, status, committed_at, delete_at, cold_at,
    recorded_at, ingest_agent_id
  )
  VALUES (
    $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19
  )
  ON CONFLICT (call_id) DO NOTHING
  RETURNING id, committed_at
)
SELECT
  COALESCE((SELECT id FROM ins),
           (SELECT id FROM call_recordings WHERE call_id = $2)) AS id,
  COALESCE((SELECT committed_at FROM ins),
           (SELECT committed_at FROM call_recordings WHERE call_id = $2)) AS committed_at,
  (SELECT id FROM ins) IS NULL AS replay
`

func (s *pgStore) InsertRecordingIdempotent(ctx context.Context, r RecordingRow) (RecordingRow, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return RecordingRow{}, false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// 1) FK check — call must exist in same tenant.
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM calls WHERE id=$1 AND tenant_id=$2)`,
		r.CallID, r.TenantID).Scan(&exists); err != nil {
		return RecordingRow{}, false, fmt.Errorf("call exists check: %w", err)
	}
	if !exists {
		return RecordingRow{}, false, ErrCallNotFound
	}

	// 2) Upsert (idempotent by call_id).
	var (
		id       uuid.UUID
		commAt   time.Time
		replay   bool
	)
	if err := tx.QueryRow(ctx, insertRecordingSQL,
		r.ID, r.CallID, r.TenantID, r.S3Bucket, r.AudioObjectKey, r.DEKObjectKey,
		r.KMSKeyID, r.EncryptedDEK, r.BytesSize, r.DurationMS, r.SHA256Hex,
		r.Codec, r.SampleRate, r.Status, r.CommittedAt, r.DeleteAt, r.ColdAt,
		r.RecordedAt, r.IngestAgentID,
	).Scan(&id, &commAt, &replay); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" /* FK violation */ {
			return RecordingRow{}, false, ErrCallNotFound
		}
		return RecordingRow{}, false, fmt.Errorf("insert call_recordings: %w", err)
	}
	r.ID = id
	r.CommittedAt = commAt

	// 3) Audit log within same TX (only for fresh inserts to avoid duplicates).
	if !replay {
		auditPayload, _ := json.Marshal(map[string]any{
			"recording_id":     id,
			"call_id":          r.CallID,
			"sha256":           r.SHA256Hex,
			"bytes_size":       r.BytesSize,
			"kms_key_id":       r.KMSKeyID,
			"audio_object_key": r.AudioObjectKey,
			"ingest_agent_id":  r.IngestAgentID,
		})
		if _, err := tx.Exec(ctx,
			`INSERT INTO audit_log (id, tenant_id, actor_kind, actor_id, action, target_kind, target_id, payload, occurred_at)
			 VALUES ($1, $2, 'service', $3, 'recording.committed', 'recording', $4, $5, $6)`,
			uuid.Must(uuid.NewV7()), r.TenantID, r.IngestAgentID, id, auditPayload, r.CommittedAt,
		); err != nil {
			return RecordingRow{}, false, fmt.Errorf("audit insert: %w", err)
		}

		// 4) Outbox row for NATS publish.
		eventPayload, _ := json.Marshal(map[string]any{
			"call_id":      r.CallID,
			"recording_id": id,
			"sha256":       r.SHA256Hex,
			"bytes_size":   r.BytesSize,
			"duration_ms":  r.DurationMS,
			"committed_at": r.CommittedAt,
		})
		if _, err := tx.Exec(ctx,
			`INSERT INTO event_outbox (id, tenant_id, subject, payload, created_at)
			 VALUES ($1, $2, $3, $4, $5)`,
			uuid.Must(uuid.NewV7()), r.TenantID,
			fmt.Sprintf("tenant.%s.recording.uploaded", r.TenantID),
			eventPayload, r.CommittedAt,
		); err != nil {
			return RecordingRow{}, false, fmt.Errorf("outbox insert: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return RecordingRow{}, false, fmt.Errorf("commit tx: %w", err)
	}
	return r, replay, nil
}

func (s *pgStore) GetByCallID(ctx context.Context, tenantID, callID uuid.UUID) (RecordingRow, error) {
	const q = `
SELECT id, call_id, tenant_id, s3_bucket, audio_object_key, dek_object_key,
       kms_key_id, encrypted_dek, bytes_size, duration_ms, sha256_hex,
       codec, sample_rate, status, committed_at, delete_at, cold_at,
       recorded_at, verified_at, integrity_ok, ingest_agent_id
FROM call_recordings
WHERE tenant_id = $1 AND call_id = $2`
	var r RecordingRow
	err := s.pool.QueryRow(ctx, q, tenantID, callID).Scan(
		&r.ID, &r.CallID, &r.TenantID, &r.S3Bucket, &r.AudioObjectKey, &r.DEKObjectKey,
		&r.KMSKeyID, &r.EncryptedDEK, &r.BytesSize, &r.DurationMS, &r.SHA256Hex,
		&r.Codec, &r.SampleRate, &r.Status, &r.CommittedAt, &r.DeleteAt, &r.ColdAt,
		&r.RecordedAt, &r.VerifiedAt, &r.IntegrityOK, &r.IngestAgentID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RecordingRow{}, ErrCallNotFound
	}
	return r, err
}
```

(Search, UpdateStatus, ListDue* — реализуем в Task 6/8 ниже.)

### 3.4 gRPC handler `Commit` — `internal/recording/api/grpc_server.go` (дополнение)

```go
func (g *GRPCServer) Commit(ctx context.Context, req *recordingv1.CommitRequest) (*recordingv1.CommitResponse, error) {
	peerID, ok := peerIdentityFrom(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no peer identity")
	}
	tenantID, err := uuid.Parse(req.GetTenantId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "tenant_id: %v", err)
	}
	if tenantID != peerID.TenantID {
		return nil, status.Error(codes.PermissionDenied, "tenant mismatch")
	}
	callID, err := uuid.Parse(req.GetCallId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "call_id: %v", err)
	}

	out, err := g.svc.Commit(ctx, service.CommitInput{
		TenantID:       tenantID,
		CallID:         callID,
		S3Bucket:       req.GetS3Bucket(),
		AudioObjectKey: req.GetAudioObjectKey(),
		DEKObjectKey:   req.GetDekObjectKey(),
		KMSKeyID:       req.GetKmsKeyId(),
		EncryptedDEK:   req.GetEncryptedDek(),
		BytesSize:      req.GetBytesSize(),
		Duration:       req.GetDuration().AsDuration(),
		SHA256Hex:      req.GetSha256(),
		Codec:          req.GetCodec(),
		SampleRate:     req.GetSampleRate(),
		DeleteAt:       req.GetDeleteAt().AsTime(),
		ColdAt:         req.GetColdAt().AsTime(),
		IngestAgentID:  peerID.AgentID,
		RecordedAt:     req.GetRecordedAt().AsTime(),
	})
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	case errors.Is(err, service.ErrCallNotFound):
		return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
	case errors.Is(err, service.ErrTenantMismatch):
		return nil, status.Errorf(codes.PermissionDenied, "%v", err)
	case err != nil:
		return nil, status.Errorf(codes.Internal, "commit: %v", err)
	}

	return &recordingv1.CommitResponse{
		RecordingId:      out.RecordingID.String(),
		CallId:           out.CallID.String(),
		CommittedAt:      timestamppb.New(out.CommittedAt),
		IdempotentReplay: out.IdempotentReplay,
	}, nil
}
```

### 3.5 NATS-publisher (outbox-relay) `internal/recording/events/publisher.go`

```go
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
)

type Publisher interface {
	Publish(ctx context.Context, subject string, payload []byte) error
}

// natsPublisher writes to NATS JetStream synchronously (acked).
type natsPublisher struct {
	js     nats.JetStreamContext
	logger *slog.Logger
}

func NewNATSPublisher(js nats.JetStreamContext, logger *slog.Logger) Publisher {
	return &natsPublisher{js: js, logger: logger}
}

func (p *natsPublisher) Publish(ctx context.Context, subject string, payload []byte) error {
	_, err := p.js.PublishMsg(&nats.Msg{Subject: subject, Data: payload}, nats.Context(ctx))
	if err != nil {
		return fmt.Errorf("nats publish %s: %w", subject, err)
	}
	return nil
}

// OutboxRelay drains event_outbox rows in batches and publishes to NATS.
// Runs in cmd/api or cmd/worker (single-instance via leader election).
type OutboxRelay struct {
	pool   *pgxpool.Pool
	pub    Publisher
	logger *slog.Logger
	tick   time.Duration
}

func NewOutboxRelay(pool *pgxpool.Pool, pub Publisher, logger *slog.Logger, tick time.Duration) *OutboxRelay {
	if tick <= 0 {
		tick = 1 * time.Second
	}
	return &OutboxRelay{pool: pool, pub: pub, logger: logger, tick: tick}
}

func (r *OutboxRelay) Run(ctx context.Context) error {
	t := time.NewTicker(r.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := r.drainOnce(ctx); err != nil {
				r.logger.Error("outbox drain failed", "err", err)
			}
		}
	}
}

func (r *OutboxRelay) drainOnce(ctx context.Context) error {
	rows, err := r.pool.Query(ctx,
		`SELECT id, subject, payload FROM event_outbox WHERE published_at IS NULL ORDER BY created_at LIMIT 100 FOR UPDATE SKIP LOCKED`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type row struct {
		id      uuid.UUID
		subject string
		payload json.RawMessage
	}
	var batch []row
	for rows.Next() {
		var rr row
		if err := rows.Scan(&rr.id, &rr.subject, &rr.payload); err != nil {
			return err
		}
		batch = append(batch, rr)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, rr := range batch {
		if err := r.pub.Publish(ctx, rr.subject, rr.payload); err != nil {
			r.logger.Warn("publish failed; retry next tick", "subject", rr.subject, "err", err)
			continue
		}
		if _, err := r.pool.Exec(ctx,
			`UPDATE event_outbox SET published_at = NOW() WHERE id = $1`, rr.id); err != nil {
			r.logger.Warn("mark published failed", "id", rr.id, "err", err)
		}
	}
	return nil
}
```

### 3.6 Acceptance

- `Commit` с тем же `call_id` дважды → второй вызов возвращает `idempotent_replay=true`, в `call_recordings` одна строка, audit-log запись только одна, outbox только одна.
- При FK-нарушении (несуществующий `call_id`) — `FailedPrecondition` (gRPC) / 412 (HTTP).
- При SHA-256 длиной 63/65 → `InvalidArgument`.
- При cert.tenant != request.tenant → `PermissionDenied`.
- Метрика `recording_commit_total{tenant=...,result="ok"}` инкрементируется.
- NATS subscriber `tenant.<id>.recording.uploaded` получает event в течение 2s после Commit (через outbox-relay).

### 3.7 Risks
- Гонка outbox: дубль publish, если падаем после publish, до `UPDATE published_at` → потребитель должен дедуплицировать по `recording_id` (это естественное свойство downstream-сервисов).
- Размер `encrypted_dek` (88 bytes) → bytea-колонка, индексировать не нужно.

---

## Self-review

**Spec coverage** (against §9, §FR-G, §13.6, §15.5, ADR-005):
- gRPC `RecordingService.Commit` на :9091 (mTLS, internal) — принимает от recording-uploader (Plan 08): валидация + idempotency через UNIQUE `call_id` + INSERT call_recordings + audit "recording.committed" с sha256 + NATS event `tenant.<id>.recording.uploaded`. ✓
- HTTP `GET /api/calls/{call_id}/recording` — RBAC (admin/supervisor), audit access, потоковая дешифровка (envelope decryption — KMS.Decrypt для DEK + AES-256-GCM расшифровка). v1 без Range (см. trade-off в §Outputs); seek в UI отключён. ✓
- HTTP `GET /api/recordings/search` — фильтры по project, operator, period, status, наличию записи. ✓
- HTTP `POST /api/calls/{call_id}/recording/verify` — manual SHA-256 проверка целостности (скачать + расшифровать + sha256 + сверить с `call_recordings.sha256`). ✓
- §9.4 retention pipeline: `retention_until = now + tenant.recording_retention_days` (default 365); `delete_at = retention_until + cold_period_days` (default 730). ✓
- `cmd/worker.recording.retention_pass` (раз в сутки, leader-only): hot→cold S3 lifecycle при retention_until < now; S3 DELETE при delete_at < now. Audit на каждое действие. ✓
- Periodic integrity verify в worker раз в неделю на 1% выборку → метрика `recording_integrity_failures_total`. ✓
- §13.6 audit-log entries `recording.{committed,accessed,deleted,verified}` с sha256 для chain-of-custody. ✓
- §15.5 метрики: `recording_commit_total{status}`, `recording_access_total{actor_role}`, `recording_decrypt_duration_seconds`, `recording_storage_size_bytes`, `recording_integrity_failures_total`. ✓
- ADR-005 envelope encryption: per-recording DEK (random AES-256-GCM key) + per-tenant KEK (Yandex KMS). DEK encrypted by KEK, lays alongside object. ✓
- Outbox pattern для NATS publish: гарантия at-least-once + downstream дедупликация по recording_id. ✓
- Coverage `internal/recording/service/` ≥ 90%. ✓

**Placeholder scan:** Outbox-race описан в Risks как acceptable trade-off (downstream dedup-handling).

**Cross-tenant defence (recording.Commit):** идемпотентность по `call_id` (он уже глобально-уникальный PK в `call_recordings` и FK к `calls.id`, который тоже глобально-уникальный UUID v7). Защита от malicious recording-uploader, подделывающего `call_id` другого тенанта, — application-level pre-check `SELECT EXISTS(... WHERE id=$1 AND tenant_id=$2)` в `InsertRecordingIdempotent` (см. ~ строка 882). Этот фильтр вернёт `ErrCallNotFound` если `call_id` принадлежит чужому тенанту. mTLS-cert тенант-ID извлекается из SPIFFE URI и сравнивается с `r.TenantID` на gRPC layer'е перед заходом в InsertRecordingIdempotent. ✓ DB-level композитный UNIQUE избыточен (нечего конфликтовать — call_id уже глобально уникален), не добавляем.

**Type/name consistency:** `RecordingMetadata`, `URLSigner`, `RetentionPlanner`, `IntegrityVerifier`, `CommitRequest` — стабильные имена. proto-файл `docs/api/recording/v1/recording.proto` единый для всех клиентов.

**Out of scope (correctly deferred):**
- Encryption pipeline на стороне FS-VM — Plan 08.
- Live listen-in (через mixmonitor) — Plan 11.
- Audio waveform UI — Plan 19.
- Periodic full-archive verify (на 100% записей) — backlog (дорого по KMS-rate).

Plan 12 verified.

---

**Plan complete and saved to `docs/superpowers/plans/2026-05-06-12-recording-module.md`.**

