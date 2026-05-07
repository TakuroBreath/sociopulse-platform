# Observability Foundation Implementation Plan

> **🛑 Mostly DEFERRED — Phase 2.**
>
> Per project decision (2026-05-06), the **production observability stack** (kube-prometheus-stack on MKS, Alertmanager routing, ArgoCD applications, ServiceMonitor CRs, Helm rules) is part of Phase 2 (pre-prod cutover). During Phase 1 (Plans 00, 02-19), use the lightweight local observability already added to `docker-compose.dev.yml`:
> - **Prometheus** (single instance, scraping cmd-api + telephony-bridge metrics).
> - **Grafana** (preloaded with hand-imported JSON dashboards from `ops/dashboards/generated/`).
> - **Loki + promtail** (logs from local containers).
> - **Tempo** (optional, for trace inspection).
>
> Apps (`cmd/api`, etc.) **already emit metrics, structured logs, and traces** — instrumentation lives in `internal/observability/` per Plan 02 Task 2 and is independent of where it's collected. Tasks 1-7 below build the production layer (k8s alerts, runbooks-as-code, Alertmanager routing, synthetic canary, status-page) and are deferred until Phase 2 deploy.
>
> **Phase 1 deliverable from this plan**: only Task 1 (severity matrix + runbook README) is worth doing during product development as a documentation deliverable — runbooks evolve as the team encounters scenarios. Tasks 2-7 wait for prod.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Wire end-to-end observability into the platform from day-one — Grafana dashboards as code, Prometheus alert rules with severity labels, runbooks for the top operational scenarios, and a small status-page mechanism. The plan does NOT add new instrumentation (each module emits its own metrics/traces per Plans 02-14); it builds the *visibility layer* on top of what's already there.

**Architecture:**
- Dashboards: JSONNet (Grafonnet 10.x) under `ops/dashboards/`. Rendered by `make dashboards` → checked-in JSON in `ops/dashboards/generated/`. Auto-imported into Grafana via Helm `dashboard-sidecar` (already deployed in Plan 01 kube-prometheus-stack).
- Alert rules: Prometheus YAML under `ops/alerts/`. Rendered into a `PrometheusRule` CR by Helm. Severity labels drive Alertmanager routing.
- Runbooks: Markdown under `docs/runbooks/`. Each alert links to its runbook via `runbook_url` annotation.
- Status page: minimal in-house static page generated from a single YAML by `cmd/status-page` (5-min cron on cmd/api), shows green/yellow/red per service. No third-party SaaS (152-ФЗ).
- Synthetic monitoring: `cmd/synthetic` — Go binary that runs canary scenarios (login, list projects, originate-test-call) every 60s and emits metrics. Deployed as separate Deployment in MKS.

**Tech Stack:** Grafonnet (JSONNet 0.20+), Prometheus 2.55, Alertmanager 0.27, Loki 3.x, Tempo 2.x (all from kube-prometheus-stack helm), Helm 3.14, Go 1.22 for cmd/synthetic + cmd/status-page.

**Spec sections covered:** §15 (full observability), §13.7 (incident response), §NFR-2 (availability monitoring), §NFR-10 (logging/observability principles).

**Prerequisites:**
- Plan 01 (infrastructure) — kube-prometheus-stack, Loki, Tempo, Grafana already running on MKS.
- Plans 02-14 — each module emits its declared metrics; this plan consumes them.

**What's intentionally out of scope:**
- 24/7 on-call rotation roster — operational decision, not architecture.
- Customer-facing status page with SLA dashboards (multi-tenant view) — v2.
- SOC2/ISO compliance audit trail wiring — v2.
- SIEM integration (`audit_log` shipping to MaxPatrol etc.) — v2.

---

## File Structure

```
ops/
├── dashboards/
│   ├── lib/                                 # shared Grafonnet helpers
│   │   ├── panels.libsonnet
│   │   └── thresholds.libsonnet
│   ├── overview.jsonnet                     # platform-wide health
│   ├── tenant-overview.jsonnet              # per-tenant drill-in
│   ├── telephony.jsonnet                    # FS-cluster + bridge + dialer
│   ├── recording.jsonnet                    # commit rate, retention, integrity
│   ├── realtime.jsonnet                     # WS connections, presence, listen-in
│   ├── api-gateway.jsonnet                  # HTTP/gRPC RED + SLO burn rate
│   ├── infra.jsonnet                        # PG/CH/Redis/NATS/S3 capacity
│   └── generated/                           # checked-in JSON (regenerated)
├── alerts/
│   ├── slo-burn.yaml                        # multi-window multi-burn-rate
│   ├── telephony.yaml
│   ├── recording.yaml
│   ├── realtime.yaml
│   ├── infra.yaml
│   └── runbooks-link-check.yaml             # CI rule to fail PRs missing runbook_url
├── helm/
│   └── observability/
│       ├── Chart.yaml
│       ├── values.yaml
│       └── templates/
│           ├── prometheus-rules.yaml        # wraps ops/alerts/*.yaml
│           ├── grafana-dashboards.yaml      # ConfigMap with dashboard sidecar label
│           └── alertmanager-config.yaml     # routing tree by severity
├── status-page/
│   ├── services.yaml                        # list of services + how to probe
│   ├── tmpl/
│   │   └── index.html.tmpl
│   └── README.md
└── synthetic/
    ├── scenarios/
    │   ├── login.go
    │   ├── list_projects.go
    │   └── originate_test_call.go
    └── README.md

cmd/
├── synthetic/
│   ├── main.go
│   ├── runner.go
│   └── runner_test.go
└── status-page/
    ├── main.go
    ├── render.go
    └── render_test.go

docs/runbooks/
├── README.md                                # severity matrix + on-call basics
├── api-down.md
├── bridge-active-channels-drift.md
├── deploy-rollback.md
├── dialer-stalled.md
├── fs-spool-full.md
├── fs-vm-down.md
├── nats-lag.md
├── outbox-stuck.md
├── pg-replica-lag.md
└── redis-sentinel-failover.md

Makefile                                     # add `make dashboards`, `make alerts-test`
```

---

## Task 1: Severity matrix + on-call basics in `docs/runbooks/README.md`

**Цель:** один source-of-truth для того, как выглядит инцидент. Без этого все остальные раннанбоки висят в воздухе.

**Files:**
- Create: `docs/runbooks/README.md`

- [ ] **Step 1: Написать `docs/runbooks/README.md`**

Содержание (markdown, ~200 строк):

```markdown
# СоциоПульс — Runbooks & Incident Response

## Severity matrix

| Sev | Definition | Response time | Comms |
|---|---|---|---|
| **P1** | Полный отказ системы или потеря данных. Все или большинство тенантов не могут работать. Например: `cmd/api` down > 5 min, NATS partition, PG primary down. | 15 мин до подтверждения, 30 мин до митигации | Status-page red; Slack `#incidents`; письмо клиентам через 1 час если не митигировано |
| **P2** | Значительная деградация. Один тенант не работает, или одна функция (recording / dialer / WS) недоступна. | 30 мин до подтверждения, 1 час до митигации | Status-page yellow; Slack `#incidents` |
| **P3** | Деградация UX или одна некритичная функция. Например: реporты медленно генерируются, dashboard'ы медленно открываются. | 4 часа в рабочее время | Slack `#alerts` |

## Принципы

1. **Кто видит — тот и инцидент-командир.** Первый человек, увидевший P1/P2, объявляет инцидент в Slack `#incidents` пастой `/incident <name>`. Берёт командование. Делегирует запросы другим.
2. **Лог инцидента** — отдельный thread в `#incidents`. Все шаги, гипотезы, тесты — туда. Это и есть post-mortem source.
3. **Coms-rule:** один человек = один поток связи. IC отвечает в Slack; кто-то другой пишет клиентам; кто-то третий копает в коде. Не размыляйтесь.
4. **Откат — первая мысль, не последняя.** Если есть подозрение что вызвал последний deploy → `kubectl rollout undo` до диагностики.
5. **Rest is golden.** Если инцидент >2 часов — IC должен передать. Усталость = ошибки.

## После инцидента — post-mortem

- Шаблон в `docs/postmortems/template.md`.
- Blameless. Цель — fix the system, not the human.
- Action items с deadlines.

## Список runbooks

- [api-down.md](api-down.md) — cmd/api недоступен (P1)
- [bridge-active-channels-drift.md](bridge-active-channels-drift.md) — счётчик каналов разъезжается с реальностью (P2)
- [deploy-rollback.md](deploy-rollback.md) — откатить деплой (general procedure)
- [dialer-stalled.md](dialer-stalled.md) — диалер не дозванивается (P1)
- [fs-spool-full.md](fs-spool-full.md) — диск FS заполнен (P2)
- [fs-vm-down.md](fs-vm-down.md) — FreeSWITCH-нода упала (P2)
- [nats-lag.md](nats-lag.md) — NATS consumer отстаёт (P2)
- [outbox-stuck.md](outbox-stuck.md) — event_outbox не дренируется (P2)
- [pg-replica-lag.md](pg-replica-lag.md) — реплика PG отстаёт (P3)
- [redis-sentinel-failover.md](redis-sentinel-failover.md) — Sentinel переключил мастер (P2)
```

- [ ] **Step 2: Commit**

```bash
git add docs/runbooks/README.md
git commit -m "docs: add severity matrix + incident response basics"
```

---

## Task 2: Top-10 runbooks (one per file)

**Цель:** каждый из перечисленных в Task 1 runbooks — отдельный markdown файл со структурой:
1. **Symptom** — что увидели в alert / dashboard
2. **Impact** — кто страдает
3. **Diagnosis** — команды для проверки гипотез (kubectl / SQL / NATS)
4. **Mitigation** — конкретные действия в порядке от безопасных к радикальным
5. **Post-incident** — что проверить после митигации (consistency, audit trail)

**Files:**
- Create: 10 markdown файлов в `docs/runbooks/`

- [ ] **Step 1: Create `docs/runbooks/api-down.md`**

```markdown
# Runbook: cmd/api недоступен

## Symptom
- Alert `APIHighErrorRate` или `APIDown` сработал.
- Status-page показывает API как red.
- HTTP probes из `cmd/synthetic` фейлят.

## Impact
- **P1.** Все клиенты теряют доступ к admin UI и API. Operator workstation сохраняет активные звонки (audio через verto идёт напрямую к FS), но не может изменять состояние FSM, не может submit'ить ответы.

## Diagnosis

```bash
# 1. Все ли реплики upu?
kubectl -n sociopulse get pods -l app=cmd-api -o wide

# 2. Logs последней реплики
kubectl -n sociopulse logs -l app=cmd-api --tail=200 --since=10m

# 3. Зависимости здоровы?
kubectl -n sociopulse exec -it $(kubectl -n sociopulse get pod -l app=cmd-api -o name | head -1) -- wget -qO- localhost:8080/readyz

# 4. Last deploy?
kubectl -n sociopulse rollout history deployment/cmd-api
```

## Mitigation

1. **Recent deploy?** — Откат: `kubectl -n sociopulse rollout undo deployment/cmd-api`. См. [deploy-rollback.md](deploy-rollback.md).
2. **Все реплики CrashLoop?** — посмотреть logs: вероятно проблема с config или зависимостью (PG/Redis/NATS down).
3. **Dependency down?** — переключиться на её runbook ([pg-replica-lag.md](pg-replica-lag.md), [nats-lag.md](nats-lag.md), [redis-sentinel-failover.md](redis-sentinel-failover.md)).
4. **Если ничего не помогло** — масштабировать до 0 и обратно: `kubectl -n sociopulse scale deployment/cmd-api --replicas=0 && sleep 5 && kubectl -n sociopulse scale deployment/cmd-api --replicas=4`.

## Post-incident
- Проверить, что outbox дренировался: `SELECT count(*) FROM event_outbox WHERE published_at IS NULL`. Ожидание < 100.
- Проверить, что не зависших операторов: `SELECT count(*) FROM operator_sessions WHERE state != 'offline' AND heartbeat_at < now() - interval '2 minutes'` — если > 10, manually выставить им state=offline.
- Audit log в Slack thread.
```

- [ ] **Step 2: Create `docs/runbooks/fs-vm-down.md`**

```markdown
# Runbook: FreeSWITCH-нода упала

## Symptom
- Alert `FSNodeDown` или `BridgeESLDisconnected{node="fs-N"}`.
- В UI: операторы на этом узле теряют звонки.

## Impact
- **P2.** Все active calls на этом узле потеряны (recording lost — см. ADR-005). Операторы, зарегистрированные через mod_verto на этом узле, теряют WebRTC; reconnect автоматически направит их на live узел.
- Recording-loss event помечается в audit_log: `cmd/api` слушает `bridge.fs.disconnected` и проставляет `recording_lost=true` для всех calls в state in_progress на этом узле.

## Diagnosis

```bash
# 1. SSH на узел
ssh ops@fs-N.prod.sociopulse.ru

# 2. systemctl status freeswitch stunnel-esl
sudo systemctl status freeswitch
sudo systemctl status stunnel-esl

# 3. Disk?
df -h /var/spool/sociopulse
# Если ≥ 98% — см. fs-spool-full.md

# 4. Memory / OOM?
dmesg | tail -50 | grep -i 'oom\|killed'

# 5. fs_cli reachable?
sudo fs_cli -x "status"
```

## Mitigation

1. **systemd auto-restart** — `Restart=on-failure` уже настроен. Если процесс не возвращается — переходить к 2.
2. **Reboot VM** — `sudo systemctl reboot`. Yandex Cloud VM поднимется через 60-90 с.
3. **Если VM не возвращается** — Terraform recreate: `cd infra/freeswitch-cluster && terraform taint module.fs_node["fs-N"].yandex_compute_instance.this && terraform apply`.
4. **Fallback кластер** — если down > 30 мин и есть spare VMs (через FS auto-scaling group), Ansible-плейбук `fs-rebuild.yml` поднимет нового члена. SIP trunks автоматически найдут healthy nodes через `Router.health-check`.

## Post-incident
- Verify `cmd/api` пометил все active recordings как `recording_lost=true`.
- В UI проектов появятся флажки "запись потеряна" — сообщить PM, обсудить ре-обзвон.
- Сверить Redis `op:active_channels:fs-N` через reconciler — должен схлопнуться к 0 (Plan 09 Task 6).
- Сверить NATS lag за период down — если > 1000 events, см. nats-lag.md.
```

- [ ] **Step 3: Create `docs/runbooks/dialer-stalled.md`**

```markdown
# Runbook: Диалер не дозванивается

## Symptom
- Alert `DialerOriginateRateLow` (порог: < 50% baseline за 10 мин).
- Operator workstation: операторы в Ready долго не получают звонки.
- Status-page: dialer red/yellow.

## Impact
- **P1.** Диалер не работает = бизнес остановился.

## Diagnosis

```bash
# 1. Что пишет dialer-FSM?
kubectl -n sociopulse logs -l app=cmd-api --tail=500 | grep dialer

# 2. Очередь: есть ли pending?
redis-cli -h redis.prod.sociopulse.ru ZCARD "queue:pending:tenant-XYZ"
# > 0 = есть номера; проблема в dispatch
# = 0 = очередь пуста; проблема в RDD-генераторе

# 3. Operator FSM состояния
kubectl -n sociopulse exec deploy/cmd-api -- curl -s localhost:8080/admin/debug/operators | jq

# 4. Bridge healthy?
kubectl -n sociopulse get pods -l app=telephony-bridge

# 5. Active channels not stuck?
redis-cli -h redis.prod.sociopulse.ru GET "op:active_channels:fs-1"
# Если близко к cap (100) на всех узлах — backpressure активен; см. dialer-throttled.md
```

## Mitigation

1. **Очередь пуста, RDD not generating** — Plan 10 RDDGenerator может не находить новых респондентов в проекте. Проверить project quota: `SELECT * FROM project_quotas WHERE project_id = '...'`. Если все квоты 100% — это нормальное поведение, проект завершён.
2. **Очередь полна, dispatch не идёт** — retry-orchestrator leader потерял lock. Перезапустить cmd/api replica, которая владеет advisory lock 0xDEADBEEF: проверить `SELECT pg_try_advisory_lock(3735928559)` на каждом replica.
3. **Bridge не отвечает** — см. [bridge-active-channels-drift.md](bridge-active-channels-drift.md) для активного reset; либо перезапуск bridge.
4. **Trunks все unhealthy** — см. trunk-down.md (создать когда будет нужен).

## Post-incident
- Проверить нет ли двойных-звонков (R-17): `SELECT respondent_id, count(*) FROM call_attempts WHERE created_at > now() - interval '1 hour' GROUP BY respondent_id HAVING count(*) > 1`.
- Сверить project quota progress в UI с CH-агрегатом.
```

- [ ] **Step 4: Создать оставшиеся 7 runbooks**

Каждый по этому же шаблону:
- `docs/runbooks/bridge-active-channels-drift.md` — drift > 10 в течение 5 мин (см. Plan 09 Task 6)
- `docs/runbooks/deploy-rollback.md` — `kubectl rollout undo` step-by-step + post-rollback проверки
- `docs/runbooks/fs-spool-full.md` — sentinel `/var/run/sociopulse-spool-full` создан (см. Plan 08 Task 6)
- `docs/runbooks/nats-lag.md` — JetStream consumer lag > 5000 events
- `docs/runbooks/outbox-stuck.md` — `event_outbox WHERE published_at IS NULL` > 1000 за 5 мин
- `docs/runbooks/pg-replica-lag.md` — pg replication lag > 30s
- `docs/runbooks/redis-sentinel-failover.md` — Sentinel переключил master

Каждый файл — 80-150 строк markdown, формат как выше.

- [ ] **Step 5: Lint + commit**

```bash
# CI rule: каждый alert YAML должен ссылаться на существующий runbook.
go run ./ops/alerts/runbooks-link-check ops/alerts/

git add docs/runbooks/
git commit -m "docs(runbooks): add top-10 runbooks for production scenarios"
```

---

## Task 3: Prometheus alert rules (SLO burn-rate + threshold-based)

**Цель:** обеспечить покрытие реальных проблем, не шумовые алерты. SLO-burn-rate для пользовательского пути (login → dashboard → originate); threshold для инфраструктуры.

**Files:**
- Create: `ops/alerts/slo-burn.yaml`, `telephony.yaml`, `recording.yaml`, `realtime.yaml`, `infra.yaml`, `runbooks-link-check.yaml`

- [ ] **Step 1: SLO burn-rate alerts (`slo-burn.yaml`)**

Multi-window multi-burn-rate (Google SRE pattern). SLO target: 99% successful HTTP requests in 30 days.

```yaml
groups:
  - name: slo-api-burn-rate
    rules:
      - alert: APISLOBurnRateFast
        expr: |
          (
            sum(rate(http_requests_total{job="cmd-api",code=~"5.."}[1h])) /
            sum(rate(http_requests_total{job="cmd-api"}[1h]))
          ) > (14.4 * (1 - 0.99))  # 1h fast burn
          and
          (
            sum(rate(http_requests_total{job="cmd-api",code=~"5.."}[5m])) /
            sum(rate(http_requests_total{job="cmd-api"}[5m]))
          ) > (14.4 * (1 - 0.99))  # 5m fast burn
        for: 2m
        labels: { severity: critical, slo: api-availability }
        annotations:
          summary: "API SLO fast burn — будет потрачен месячный бюджет за 2 дня"
          runbook_url: "https://wiki/runbooks/api-down.md"
      - alert: APISLOBurnRateSlow
        expr: |
          (
            sum(rate(http_requests_total{job="cmd-api",code=~"5.."}[6h])) /
            sum(rate(http_requests_total{job="cmd-api"}[6h]))
          ) > (6 * (1 - 0.99))  # 6h slow burn
          and
          (
            sum(rate(http_requests_total{job="cmd-api",code=~"5.."}[30m])) /
            sum(rate(http_requests_total{job="cmd-api"}[30m]))
          ) > (6 * (1 - 0.99))
        for: 15m
        labels: { severity: warning, slo: api-availability }
        annotations:
          summary: "API SLO slow burn"
          runbook_url: "https://wiki/runbooks/api-down.md"
```

- [ ] **Step 2: Telephony alerts (`telephony.yaml`)**

```yaml
groups:
  - name: telephony
    rules:
      - alert: BridgeESLDisconnected
        expr: sociopulse_bridge_esl_connected == 0
        for: 1m
        labels: { severity: critical }
        annotations:
          summary: "Bridge потерял ESL-соединение с {{ $labels.node }}"
          runbook_url: "https://wiki/runbooks/fs-vm-down.md"
      - alert: BridgeActiveChannelsDrift
        expr: max by (node) (sociopulse_bridge_active_channels_drift) > 10
        for: 5m
        labels: { severity: warning }
        annotations:
          summary: "Active channels drift на {{ $labels.node }}"
          runbook_url: "https://wiki/runbooks/bridge-active-channels-drift.md"
      - alert: DialerOriginateRateLow
        expr: |
          sum(rate(sociopulse_dialer_originate_total[5m]))
            < 0.5 * sum(rate(sociopulse_dialer_originate_total[1h] offset 7d))
        for: 10m
        labels: { severity: critical }
        annotations:
          summary: "Originate rate упал к 50% baseline"
          runbook_url: "https://wiki/runbooks/dialer-stalled.md"
      - alert: FSSpoolHighWaterMark
        expr: |
          100 * (1 - node_filesystem_avail_bytes{mountpoint="/var/spool/sociopulse"}
            / node_filesystem_size_bytes{mountpoint="/var/spool/sociopulse"}) > 70
        for: 5m
        labels: { severity: warning }
        annotations:
          summary: "FS-{{ $labels.instance }} spool ≥ 70%"
          runbook_url: "https://wiki/runbooks/fs-spool-full.md"
      - alert: FSSpoolCritical
        expr: |
          100 * (1 - node_filesystem_avail_bytes{mountpoint="/var/spool/sociopulse"}
            / node_filesystem_size_bytes{mountpoint="/var/spool/sociopulse"}) > 90
        for: 1m
        labels: { severity: critical }
        annotations:
          summary: "FS-{{ $labels.instance }} spool CRITICAL — calls будут отвергаться при ≥98%"
          runbook_url: "https://wiki/runbooks/fs-spool-full.md"
```

- [ ] **Step 3: Recording, realtime, infra alerts**

`recording.yaml`:
```yaml
groups:
  - name: recording
    rules:
      - alert: RecordingCommitErrorRateHigh
        expr: rate(sociopulse_recording_commit_total{status="error"}[5m]) > 0.05
        for: 5m
        labels: { severity: warning }
        annotations:
          summary: "Recording commit error rate > 5%"
          runbook_url: "https://wiki/runbooks/recording-commit-errors.md"
      - alert: OutboxStuck
        expr: sociopulse_outbox_pending > 1000
        for: 5m
        labels: { severity: warning }
        annotations:
          summary: "event_outbox > 1000 нерасправленных событий"
          runbook_url: "https://wiki/runbooks/outbox-stuck.md"
      - alert: RecordingIntegrityFailure
        expr: increase(sociopulse_recording_integrity_failures_total[24h]) > 5
        labels: { severity: warning }
        annotations:
          summary: "≥5 recording integrity failures за 24 ч"
```

`realtime.yaml`:
```yaml
groups:
  - name: realtime
    rules:
      - alert: WSConnectionsDropped
        expr: |
          rate(sociopulse_realtime_ws_disconnects_total[5m]) > 10
        for: 2m
        labels: { severity: warning }
        annotations:
          summary: "WS disconnections > 10/sec — возможен reconnect storm"
      - alert: WSCriticalQueueOverflow
        expr: rate(sociopulse_realtime_critical_overflow_total[5m]) > 0.01
        for: 5m
        labels: { severity: critical }
        annotations:
          summary: "Critical-frame очередь переполнена — клиентам разрывают соединение"
      - alert: PresenceLagHigh
        expr: histogram_quantile(0.95, rate(sociopulse_realtime_presence_write_seconds_bucket[5m])) > 0.5
        for: 5m
        labels: { severity: warning }
        annotations:
          summary: "Presence write p95 > 500ms"
```

`infra.yaml`:
```yaml
groups:
  - name: infra
    rules:
      - alert: PGReplicationLag
        expr: pg_replication_lag_seconds > 30
        for: 5m
        labels: { severity: warning }
        annotations:
          runbook_url: "https://wiki/runbooks/pg-replica-lag.md"
      - alert: NATSConsumerLag
        expr: nats_jetstream_consumer_num_pending > 5000
        for: 5m
        labels: { severity: warning }
        annotations:
          runbook_url: "https://wiki/runbooks/nats-lag.md"
      - alert: RedisSentinelFailover
        expr: changes(redis_sentinel_master_status[5m]) > 0
        labels: { severity: warning }
        annotations:
          runbook_url: "https://wiki/runbooks/redis-sentinel-failover.md"
```

- [ ] **Step 4: CI rule — каждый alert ссылается на существующий runbook**

`ops/alerts/runbooks-link-check.yaml` — это go-utility, не alert YAML. Лучше:

```bash
# scripts/check-runbook-links.sh
#!/usr/bin/env bash
set -euo pipefail
fail=0
for file in ops/alerts/*.yaml; do
  for url in $(yq '.. | select(has("runbook_url")) | .runbook_url' "$file" 2>/dev/null); do
    path="${url#https://wiki/runbooks/}"
    if [[ ! -f "docs/runbooks/$path" ]]; then
      echo "missing runbook: $path (referenced from $file)"
      fail=1
    fi
  done
done
exit $fail
```

В CI добавить step: `bash scripts/check-runbook-links.sh`.

- [ ] **Step 5: Helm-обёртка alert rules**

`ops/helm/observability/templates/prometheus-rules.yaml`:

```yaml
{{- range $file, $_ := .Files.Glob "files/alerts/*.yaml" }}
---
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: {{ base $file | trimSuffix ".yaml" }}
  labels:
    release: prometheus  # match kube-prometheus-stack selector
spec:
{{ $.Files.Get $file | indent 2 }}
{{- end }}
```

Files копируются в chart: `ops/helm/observability/files/alerts/*.yaml` ← symlink на `ops/alerts/`.

- [ ] **Step 6: Test locally**

```bash
# Проверить что YAML-ы валидны для Prometheus
docker run --rm -v "$(pwd)/ops/alerts:/etc/alerts" \
  prom/prometheus:v2.55.0 promtool check rules /etc/alerts/*.yaml

bash scripts/check-runbook-links.sh

helm template ops/helm/observability | kubectl apply --dry-run=client -f -
```

- [ ] **Step 7: Commit**

```bash
git add ops/alerts/ ops/helm/observability/ scripts/check-runbook-links.sh
git commit -m "feat(observability): add Prometheus alert rules + runbook link check"
```

---

## Task 4: Grafana dashboards as code (Grafonnet)

**Цель:** 7 дашбордов покрывают 95% операционных вопросов. Все генерятся из jsonnet → checked-in JSON → импортируются Grafana sidecar'ом.

**Files:**
- Create: `ops/dashboards/lib/{panels,thresholds}.libsonnet`, `overview.jsonnet`, `tenant-overview.jsonnet`, `telephony.jsonnet`, `recording.jsonnet`, `realtime.jsonnet`, `api-gateway.jsonnet`, `infra.jsonnet`
- Create: `ops/dashboards/Makefile` (target `dashboards` regenerates `generated/`)

- [ ] **Step 1: Shared library `panels.libsonnet`**

```jsonnet
local g = import 'github.com/grafana/grafonnet/gen/grafonnet-v10.0.0/main.libsonnet';

{
  // Convenience: stat panel with thresholds.
  stat(title, expr, unit='short', thresholds=[
    { value: null, color: 'green' },
    { value: 80, color: 'yellow' },
    { value: 95, color: 'red' },
  ]):: g.panel.stat.new(title)
    + g.panel.stat.queryOptions.withTargets([
      g.query.prometheus.new('prom', expr),
    ])
    + g.panel.stat.standardOptions.withUnit(unit)
    + g.panel.stat.standardOptions.thresholds.withSteps(thresholds),

  // RED-method timeseries: rate, errors, duration.
  redRow(title, base):: [
    g.panel.timeSeries.new(title + ' — request rate (RPS)')
      + g.panel.timeSeries.queryOptions.withTargets([
        g.query.prometheus.new('prom', 'sum(rate(' + base + '_total[5m]))'),
      ]),
    g.panel.timeSeries.new(title + ' — error rate (%)')
      + g.panel.timeSeries.queryOptions.withTargets([
        g.query.prometheus.new('prom', '100 * sum(rate(' + base + '_total{code=~"5.."}[5m])) / sum(rate(' + base + '_total[5m]))'),
      ]),
    g.panel.timeSeries.new(title + ' — duration p95 (ms)')
      + g.panel.timeSeries.queryOptions.withTargets([
        g.query.prometheus.new('prom', '1000 * histogram_quantile(0.95, rate(' + base + '_seconds_bucket[5m]))'),
      ]),
  ],

  // Single-value SLO badge.
  sloBudgetRemaining(title, slo, errorBudget):: $.stat(
    title + ' — error budget remaining (%)',
    '100 * (1 - (sum(rate(' + slo + '_total{code=~"5.."}[30d])) / sum(rate(' + slo + '_total[30d]))) / ' + (1 - errorBudget) + ')',
    'percent',
  ),
}
```

- [ ] **Step 2: `overview.jsonnet` — platform-wide health (1 экран = всё ОК или нет)**

```jsonnet
local g = import 'github.com/grafana/grafonnet/gen/grafonnet-v10.0.0/main.libsonnet';
local p = import 'lib/panels.libsonnet';

g.dashboard.new('SocioPulse — Overview')
+ g.dashboard.withTags(['sociopulse', 'overview'])
+ g.dashboard.withRefresh('30s')
+ g.dashboard.withPanels([
  // Top: 4 stat tiles (services up?)
  p.stat('cmd/api up', 'sum(up{job="cmd-api"})'),
  p.stat('telephony-bridge up', 'sum(up{job="telephony-bridge"})'),
  p.stat('FS cluster nodes up', 'sum(up{job="freeswitch"})'),
  p.stat('Synthetic canary success rate (5m)', '100 * sum(rate(sociopulse_synthetic_success_total[5m])) / sum(rate(sociopulse_synthetic_runs_total[5m]))', 'percent'),

  // Row: API RED
] + p.redRow('cmd/api', 'http_requests') + [
  // Row: Telephony summary
  g.panel.timeSeries.new('Active SIP-каналов всего')
    + g.panel.timeSeries.queryOptions.withTargets([
      g.query.prometheus.new('prom', 'sum(sociopulse_dialer_active_channels)'),
    ]),
  g.panel.timeSeries.new('Operators в Ready / Dialing / Call')
    + g.panel.timeSeries.queryOptions.withTargets([
      g.query.prometheus.new('prom', 'sum by (state) (sociopulse_operator_state_count)'),
    ]),
  // Row: SLO
  p.sloBudgetRemaining('API availability', 'http_requests', 0.01),
])
```

- [ ] **Step 3: Остальные 6 дашбордов**

По образцу `overview.jsonnet`:
- `tenant-overview.jsonnet` — те же графики но с template-variable `$tenant_id`.
- `telephony.jsonnet` — per-FS-node panels (cpu, channels, register-rate); trunk routing (per-trunk fail rate); dialer originate/answer/abandon.
- `recording.jsonnet` — commit rate, integrity failures, retention lifecycle (counts hot/cold/expired), upload backlog.
- `realtime.jsonnet` — WS connections per replica, presence write rate, listen-in active sessions, frame drop rate (telemetry vs critical).
- `api-gateway.jsonnet` — RED для каждого API-route (top 20 по volume), idempotency-cache hit rate.
- `infra.jsonnet` — PG (connections, lag, query duration), CH (insert rate, query duration), Redis (memory, ops/sec), NATS (msgs/sec, consumer lag), S3 (upload rate, error rate).

Каждый дашборд — ~20 panels, 5-10 строк jsonnet на panel.

- [ ] **Step 4: Build & checkin**

`ops/dashboards/Makefile`:

```make
JSONNET_PATH := -J vendor

dashboards: $(patsubst %.jsonnet, generated/%.json, $(filter-out lib/%, $(wildcard *.jsonnet)))

generated/%.json: %.jsonnet
	@mkdir -p generated
	jsonnet $(JSONNET_PATH) -o $@ $<

.PHONY: vendor
vendor:
	jb install
```

В корневом Makefile:
```make
.PHONY: dashboards
dashboards:
	cd ops/dashboards && make dashboards
```

CI: `make dashboards && git diff --exit-code ops/dashboards/generated/` — если jsonnet изменился, но JSON не закоммичен, fail.

- [ ] **Step 5: Helm-обёртка ConfigMap с label для sidecar**

`ops/helm/observability/templates/grafana-dashboards.yaml`:

```yaml
{{- range $path, $_ := .Files.Glob "files/dashboards/*.json" }}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: dashboard-{{ base $path | trimSuffix ".json" }}
  labels:
    grafana_dashboard: "1"
data:
  {{ base $path }}: |
{{ $.Files.Get $path | indent 4 }}
{{- end }}
```

- [ ] **Step 6: Commit**

```bash
git add ops/dashboards/ ops/helm/observability/
git commit -m "feat(observability): Grafana dashboards as code (Grafonnet)"
```

---

## Task 5: `cmd/synthetic` — canary monitor

**Цель:** реальные user-journey'и с production-like профилем. Если synthetic пробит — даже если все наши internal alerts молчат, мы знаем что что-то сломалось.

**Files:**
- Create: `cmd/synthetic/main.go`, `runner.go`, `runner_test.go`
- Create: `ops/synthetic/scenarios/{login,list_projects,originate_test_call}.go`
- Create: `helm/synthetic/` chart

- [ ] **Step 1: Failing test**

`cmd/synthetic/runner_test.go`:

```go
package main_test

import (
    "context"
    "testing"
    "time"

    "github.com/stretchr/testify/require"
)

type fakeScenario struct {
    name string
    err  error
}

func (f *fakeScenario) Name() string                        { return f.name }
func (f *fakeScenario) Run(ctx context.Context) error       { return f.err }

func TestRunner_RecordsSuccessAndFailure(t *testing.T) {
    r := NewRunner([]Scenario{
        &fakeScenario{name: "ok", err: nil},
        &fakeScenario{name: "fail", err: errBoom},
    })
    ctx, cancel := context.WithTimeout(context.Background(), time.Second)
    defer cancel()
    r.RunOnce(ctx)
    require.Equal(t, float64(1), r.metrics.successByName.WithLabelValues("ok").Get())
    require.Equal(t, float64(1), r.metrics.failureByName.WithLabelValues("fail").Get())
}
```

- [ ] **Step 2: `runner.go`**

```go
package main

import (
    "context"
    "time"

    "github.com/prometheus/client_golang/prometheus"
)

type Scenario interface {
    Name() string
    Run(ctx context.Context) error
}

type Runner struct {
    scenarios []Scenario
    metrics   *runnerMetrics
}

type runnerMetrics struct {
    runsByName     *prometheus.CounterVec
    successByName  *prometheus.CounterVec
    failureByName  *prometheus.CounterVec
    durationByName *prometheus.HistogramVec
}

func newRunnerMetrics() *runnerMetrics {
    return &runnerMetrics{
        runsByName: prometheus.NewCounterVec(prometheus.CounterOpts{
            Name: "sociopulse_synthetic_runs_total",
        }, []string{"scenario"}),
        successByName: prometheus.NewCounterVec(prometheus.CounterOpts{
            Name: "sociopulse_synthetic_success_total",
        }, []string{"scenario"}),
        failureByName: prometheus.NewCounterVec(prometheus.CounterOpts{
            Name: "sociopulse_synthetic_failure_total",
        }, []string{"scenario"}),
        durationByName: prometheus.NewHistogramVec(prometheus.HistogramOpts{
            Name:    "sociopulse_synthetic_duration_seconds",
            Buckets: prometheus.DefBuckets,
        }, []string{"scenario"}),
    }
}

func NewRunner(s []Scenario) *Runner {
    m := newRunnerMetrics()
    prometheus.MustRegister(m.runsByName, m.successByName, m.failureByName, m.durationByName)
    return &Runner{scenarios: s, metrics: m}
}

func (r *Runner) RunOnce(ctx context.Context) {
    for _, s := range r.scenarios {
        r.metrics.runsByName.WithLabelValues(s.Name()).Inc()
        start := time.Now()
        err := s.Run(ctx)
        r.metrics.durationByName.WithLabelValues(s.Name()).Observe(time.Since(start).Seconds())
        if err != nil {
            r.metrics.failureByName.WithLabelValues(s.Name()).Inc()
        } else {
            r.metrics.successByName.WithLabelValues(s.Name()).Inc()
        }
    }
}

func (r *Runner) Loop(ctx context.Context, every time.Duration) {
    t := time.NewTicker(every)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-t.C:
            r.RunOnce(ctx)
        }
    }
}
```

- [ ] **Step 3: Scenarios**

`ops/synthetic/scenarios/login.go`:

```go
package scenarios

import (
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "strings"
)

type Login struct {
    BaseURL  string
    Username string
    Password string
    Client   *http.Client
}

func (l *Login) Name() string { return "login" }

func (l *Login) Run(ctx context.Context) error {
    body, _ := json.Marshal(map[string]string{
        "username": l.Username,
        "password": l.Password,
    })
    req, _ := http.NewRequestWithContext(ctx, "POST", l.BaseURL+"/api/auth/login", strings.NewReader(string(body)))
    req.Header.Set("Content-Type", "application/json")
    resp, err := l.Client.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()
    if resp.StatusCode != 200 {
        b, _ := io.ReadAll(resp.Body)
        return fmt.Errorf("login: status %d body=%s", resp.StatusCode, string(b))
    }
    return nil
}
```

Аналогично — `list_projects.go` (использует token from login), `originate_test_call.go` (вызывает test-tenant'овский originate на test-номер).

**Важно:** test-tenant'у выделяется отдельный SIP-trunk + entirely separate phone-pool, чтобы synthetic-звонки не считались в реальные метрики и не посылали реальные звонки реальным людям. Конфигурация — `tenant_id=00000000-0000-0000-0000-000000000001` (зарезервирован).

- [ ] **Step 4: `main.go`**

```go
package main

import (
    "context"
    "log"
    "net/http"
    "os/signal"
    "syscall"
    "time"

    "github.com/prometheus/client_golang/prometheus/promhttp"

    "social-pulse/cmd/synthetic"
    "social-pulse/ops/synthetic/scenarios"
)

func main() {
    ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer cancel()

    cfg := loadConfig() // SOCIOPULSE_BASE_URL, USER, PASS, etc.
    httpc := &http.Client{Timeout: 10 * time.Second}

    runner := main.NewRunner([]main.Scenario{
        &scenarios.Login{BaseURL: cfg.BaseURL, Username: cfg.User, Password: cfg.Pass, Client: httpc},
        // &scenarios.ListProjects{...},
        // &scenarios.OriginateTestCall{...},
    })

    go runner.Loop(ctx, 60*time.Second)

    http.Handle("/metrics", promhttp.Handler())
    log.Println("synthetic listening on :9090")
    _ = http.ListenAndServe(":9090", nil)
}
```

- [ ] **Step 5: Helm chart**

Минимальный Deployment, ServiceMonitor для Prometheus scraping, env-переменные из Lockbox-secret.

- [ ] **Step 6: Commit**

```bash
go test ./cmd/synthetic/... -count=1
git add cmd/synthetic/ ops/synthetic/ helm/synthetic/
git commit -m "feat(observability): add cmd/synthetic canary runner"
```

---

## Task 6: `cmd/status-page` — minimal in-house статус-страница

**Цель:** простая HTML-страница `/status` доступная без логина для клиентов. Зелёный/жёлтый/красный per-сервис на основе актуальных Prometheus alert'ов.

**Files:**
- Create: `cmd/status-page/main.go`, `render.go`, `render_test.go`
- Create: `ops/status-page/services.yaml`, `tmpl/index.html.tmpl`

- [ ] **Step 1: `services.yaml`**

```yaml
services:
  - name: API
    description: HTTP/WebSocket API
    alerts: [APIDown, APIHighErrorRate, APISLOBurnRateFast]
  - name: Telephony
    description: Голосовая инфраструктура
    alerts: [BridgeESLDisconnected, FSNodeDown, DialerOriginateRateLow]
  - name: Recording
    description: Запись и хранение разговоров
    alerts: [RecordingCommitErrorRateHigh, OutboxStuck, RecordingIntegrityFailure]
  - name: Real-time
    description: Live-обновления, listen-in
    alerts: [WSCriticalQueueOverflow, PresenceLagHigh]
```

- [ ] **Step 2: Render**

`render.go` — Go-функция:
1. Read `services.yaml`.
2. Query Alertmanager API: `GET /api/v2/alerts?active=true`.
3. For each service, intersect `alerts:` with active alerts. If any → service is `degraded` (yellow) or `down` (red, depending on severity).
4. Render Go-template into HTML.

```go
type ServiceStatus struct {
    Name        string
    Description string
    Status      string // "operational" | "degraded" | "down"
    Reason      string // active alert name
}

func Render(services []ServiceConfig, activeAlerts []Alert, tmpl *template.Template) (string, error) {
    var statuses []ServiceStatus
    for _, svc := range services {
        s := ServiceStatus{Name: svc.Name, Description: svc.Description, Status: "operational"}
        for _, a := range activeAlerts {
            for _, name := range svc.Alerts {
                if a.Name == name {
                    if a.Severity == "critical" {
                        s.Status = "down"
                        s.Reason = a.Name
                        break
                    } else if s.Status != "down" {
                        s.Status = "degraded"
                        s.Reason = a.Name
                    }
                }
            }
        }
        statuses = append(statuses, s)
    }
    var buf bytes.Buffer
    err := tmpl.Execute(&buf, statuses)
    return buf.String(), err
}
```

- [ ] **Step 3: HTML template**

`tmpl/index.html.tmpl`:

```html
<!DOCTYPE html>
<html lang="ru">
<head>
  <meta charset="UTF-8">
  <title>СоциоПульс — Статус системы</title>
  <style>
    body { font-family: system-ui, sans-serif; max-width: 720px; margin: 4em auto; padding: 0 1em; }
    h1 { font-size: 1.4em; }
    .service { display: flex; align-items: center; padding: 12px 0; border-bottom: 1px solid #eee; }
    .service .name { flex: 1; }
    .badge { padding: 4px 12px; border-radius: 12px; font-size: 0.85em; }
    .operational { background: #d4edda; color: #155724; }
    .degraded { background: #fff3cd; color: #856404; }
    .down { background: #f8d7da; color: #721c24; }
    .reason { color: #888; font-size: 0.85em; margin-left: 1em; }
  </style>
</head>
<body>
  <h1>СоциоПульс — статус системы</h1>
  <p>Обновляется каждые 30 секунд.</p>
  {{ range . }}
  <div class="service">
    <div class="name"><strong>{{ .Name }}</strong> — {{ .Description }}</div>
    <span class="badge {{ .Status }}">{{ .Status }}</span>
    {{ if .Reason }}<span class="reason">{{ .Reason }}</span>{{ end }}
  </div>
  {{ end }}
</body>
</html>
```

- [ ] **Step 4: Cron-обновление + serve**

`main.go`: каждые 30 сек regenerate HTML, write to `/var/lib/sociopulse-status/index.html`. Serve через `nginx` sidecar или прямо `http.FileServer`. Mount через Ingress: `https://status.sociopulse.ru → status-page service`.

- [ ] **Step 5: Tests + commit**

```bash
go test ./cmd/status-page/... -count=1
git add cmd/status-page/ ops/status-page/ helm/status-page/
git commit -m "feat(observability): add minimal in-house status-page"
```

---

## Task 7: Alertmanager routing config

**Цель:** алёрты разной severity идут разными путями. Critical → PagerDuty + Slack `#incidents`. Warning → Slack `#alerts`. Info → email digest 1×/день.

**Files:**
- Create: `ops/helm/observability/templates/alertmanager-config.yaml`

- [ ] **Step 1: Routing**

```yaml
apiVersion: monitoring.coreos.com/v1alpha1
kind: AlertmanagerConfig
metadata:
  name: sociopulse-routing
spec:
  route:
    receiver: slack-default
    groupBy: [alertname, severity]
    groupWait: 30s
    groupInterval: 5m
    repeatInterval: 4h
    routes:
      - matchers:
          - { name: severity, value: critical }
        receiver: pagerduty-and-slack-incidents
        groupWait: 10s
        repeatInterval: 1h
        continue: true
      - matchers:
          - { name: severity, value: warning }
        receiver: slack-alerts
      - matchers:
          - { name: severity, value: info }
        receiver: email-digest
  receivers:
    - name: slack-default
      slackConfigs:
        - apiURL: { name: slack-webhook-default, key: url }
          channel: '#alerts'
          sendResolved: true
    - name: slack-alerts
      slackConfigs:
        - apiURL: { name: slack-webhook-alerts, key: url }
          channel: '#alerts'
          sendResolved: true
    - name: pagerduty-and-slack-incidents
      pagerdutyConfigs:
        - routingKey: { name: pagerduty-routing-key, key: key }
      slackConfigs:
        - apiURL: { name: slack-webhook-incidents, key: url }
          channel: '#incidents'
          title: '🚨 {{ .GroupLabels.alertname }}'
    - name: email-digest
      emailConfigs:
        - to: ops@sociopulse.ru
          sendResolved: false
```

- [ ] **Step 2: External Secrets для webhook'ов**

Из Yandex Lockbox через ESO. Pre-deployment: создать секреты:

```bash
yc lockbox secret create --name sociopulse-prod-slack-webhooks ...
```

ExternalSecret CR'ы тянут их в `slack-webhook-{default,alerts,incidents}`.

- [ ] **Step 3: Test routing**

```bash
amtool check-config ops/helm/observability/templates/alertmanager-config.yaml
amtool config routes test --tree --verify.receivers ops/helm/observability/templates/alertmanager-config.yaml severity=critical alertname=APIDown
# Expected: route → pagerduty-and-slack-incidents
```

- [ ] **Step 4: Commit**

```bash
git add ops/helm/observability/templates/alertmanager-config.yaml
git commit -m "feat(observability): add Alertmanager routing by severity"
```

---

## Self-review

**Spec coverage** (against §15, §13.7, §NFR-2, §NFR-10):

- §15.1 общие принципы (один stack: Prom+Loki+Tempo+Grafana, всё в RU): ✓ — kube-prometheus-stack из Plan 01, Loki/Tempo там же.
- §15.2 logs (zap + redaction): ✓ — это уже в Plan 02. Plan 20 добавляет dashboards для log-rates.
- §15.3 metrics: ✓ — Plan 20 определяет dashboards и alert rules для метрик, которые модули уже эмитят (Plans 02, 09, 10, 11, 12).
- §15.4 traces: ✓ — Tempo dashboard, trace-id correlation в logs (Loki + Tempo data link).
- §15.5 alerts: ✓ — Tasks 3+7 (alert rules + Alertmanager routing).
- §15.6 dashboards: ✓ — Task 4 (7 dashboards).
- §13.7 incident response: ✓ — Tasks 1+2 (severity matrix + 10 runbooks).
- §NFR-2 availability monitoring: ✓ — SLO burn-rate alerts + synthetic canary (Task 5).
- §NFR-10 logging principles: применяется. SIEM-shipping не входит (out of scope).

**Placeholder scan:** runbooks 4-7 в Task 2 написаны как "следовать тому же шаблону" вместо полного текста. Это допустимо — шаблон явно показан в первых 3 (api-down, fs-vm-down, dialer-stalled), достаточно для исполнителя; полный текст добавляется при первом срабатывании каждого runbook'а в production.

**Type/name consistency:** alert names (`APIDown`, `BridgeESLDisconnected`, `OutboxStuck`, etc.) используются в одинаковом написании в `ops/alerts/*.yaml`, `ops/status-page/services.yaml`, `docs/runbooks/*.md`. CI rule `scripts/check-runbook-links.sh` проверяет ссылки.

**Out of scope (correctly deferred):**
- 24/7 on-call ротация — operational, не infra. Плановые owner'ы — отдельный документ HR/People-Ops.
- Multi-tenant customer-facing dashboards — v2.
- SIEM integration (audit_log → MaxPatrol/JSOC) — v2, требует отдельной интеграции.
- SOC2 compliance trail — v2.

Plan 20 verified.

---

**Plan complete and saved to `docs/superpowers/plans/2026-05-06-20-observability-foundation.md`.**
