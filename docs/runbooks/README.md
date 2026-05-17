# СоциоПульс — Runbooks & Incident Response

Operational руководство для реакции на инциденты в production. Каждый
runbook здесь связан с Prometheus-алертом через аннотацию `runbook_url`;
если ты пришёл сюда из Slack-нотификации — открывай файл по имени из
алерта.

> **Phase 1 deliverable.** Сейчас этот файл — единственный артефакт
> observability-плана в `sociopulse-platform`. Конкретные runbook'и
> (`api-down.md`, `fs-vm-down.md`, …) и alert-правила появятся в Phase 2
> через `sociopulse-infra` (kube-prometheus-stack + Grafonnet + Alertmanager).
> Ссылки ниже будут 404 до того, как Phase 2 закроется — намеренно: shape
> навигации фиксирован сейчас, leaves заполняются позже.

---

## Severity matrix

| Sev | Определение | Время реакции | Коммуникации |
|---|---|---|---|
| **P1** | Полный отказ системы или потеря данных. Все или большинство тенантов не могут работать. Примеры: `cmd/api` down > 5 min; NATS-partition; PG primary down; KMS region недоступен. | 15 мин до подтверждения, 30 мин до митигации. | Status-page red; Slack `#incidents`; письмо клиентам через 1 час, если не митигировано. |
| **P2** | Значительная деградация. Один тенант не работает, или одна функция (recording / dialer / WS / billing) недоступна. Примеры: FS-нода down (часть операторов теряют звонки); outbox stuck > 10 min; integrity worker отстаёт. | 30 мин до подтверждения, 1 час до митигации. | Status-page yellow; Slack `#incidents`. |
| **P3** | Деградация UX или одна некритичная функция. Примеры: отчёты медленно генерируются; dashboard'ы медленно открываются; PG-реплика отстаёт без влияния на write-path. | 4 часа в рабочее время; в нерабочее — следующее рабочее утро. | Slack `#alerts`. |

**Правило назначения severity:** smoke-level импакт = P1, single-tenant
импакт = P2, UX-импакт = P3. Если сомневаешься — поднимай уровень
(over-classify P2 в P1 безопасно, наоборот — нет).

---

## Принципы реагирования

1. **Кто видит — тот и инцидент-командир (IC).** Первый человек, увидевший
   P1/P2, объявляет инцидент в Slack `#incidents` через `/incident <name>`.
   Берёт командование. Делегирует помощь другим. Не ждёт «более опытного»
   — командует тот, кто первый, передаёт по необходимости.

2. **Лог инцидента — отдельный thread в `#incidents`.** Все шаги, гипотезы,
   проверенные команды, наблюдения — туда. Это и есть source для
   post-mortem; без лога восстановить хронологию через сутки невозможно.

3. **Comms-rule: один человек = один поток связи.** IC отвечает за Slack;
   кто-то другой — за переписку с клиентами; третий — за код / SQL /
   `kubectl`. Не размывайтесь: «multitasking IC» = пропущенный сигнал.

4. **Откат — первая мысль, не последняя.** Если есть подозрение, что
   последний deploy причина — `kubectl rollout undo deployment/<name>` ДО
   диагностики. Time-to-mitigation > time-to-root-cause. Корень разберёшь
   после восстановления сервиса.

5. **Rest is golden.** Если инцидент > 2 часов — IC обязан передать
   командование. Усталость = ошибки = повторный инцидент. Усталый IC
   принимает плохие решения по dependency-graph'у.

6. **Не паникуем по одному алерту.** Один алерт без визибл-импакта может
   быть flapping. Подтверди через второй источник (status-page / synthetic /
   user report) перед эскалацией до P1.

---

## Шаги для IC при объявлении инцидента

1. Сразу пиши в `#incidents`:
   ```
   /incident <короткое-имя>
   sev: P1 | P2 | P3
   what-i-see: <одна строка>
   investigating: yes
   ```

2. Status-page → выставить соответствующий цвет (`green` → `yellow`/`red`)
   для пострадавшего сервиса. Phase-2: автоматизировано через
   `cmd/status-page`; сейчас — вручную.

3. Открыть relevant runbook (по имени из алерта).

4. Если runbook не помогает за TTL митигации — escalate (`@oncall-platform`
   в `#incidents`) И записать в thread, что эскалация произошла.

5. После митигации — отметить в thread `mitigated at <timestamp>`,
   подтвердить через `cmd/synthetic` / probes, что система health.

6. В течение 24 часов — post-mortem (см. ниже).

---

## После инцидента — post-mortem

- Шаблон: `docs/postmortems/template.md` (TBD — создаётся при первом
  реальном инциденте).
- **Blameless.** Цель — fix the system, not the human. Если ошибся
  человек, вопрос: «почему система позволила ошибку?» — а не «кто
  виноват?».
- Action items с deadline'ами и owner'ами; tracked в Linear/GitHub-issues
  с label `incident-followup`.
- Post-mortem-thread остаётся в `#incidents` навсегда — references для
  будущих инцидентов того же класса.

---

## Список runbooks

Каждый файл следует структуре: **Symptom → Impact → Diagnosis →
Mitigation → Post-incident**. Файлы создаются в Phase 2 через
`sociopulse-infra` — ссылки ниже сейчас не работают, фиксируют будущее
дерево.

| Runbook | Severity | Trigger |
|---|---|---|
| [api-down.md](api-down.md) | P1 | `cmd/api` недоступен (HTTP probes фейлят, реплики CrashLoop) |
| [bridge-active-channels-drift.md](bridge-active-channels-drift.md) | P2 | Counter `telephony_router_active_channels` разъезжается с реальностью FS |
| [deploy-rollback.md](deploy-rollback.md) | procedure | Откатить последний deploy (general procedure для всех cmd-* deployment'ов) |
| [dialer-stalled.md](dialer-stalled.md) | P1 | `dialer_originate_rate` < 50% baseline за 10 минут |
| [fs-spool-full.md](fs-spool-full.md) | P2 | `/var/spool/sociopulse` ≥ 95% на FS-VM |
| [fs-vm-down.md](fs-vm-down.md) | P2 | `FSNodeDown` / `BridgeESLDisconnected{node=...}` |
| [nats-lag.md](nats-lag.md) | P2 | JetStream consumer lag > 1000 events |
| [outbox-stuck.md](outbox-stuck.md) | P2 | `sociopulse_outbox_parked_rows{tenant}` > 100 за 5 минут |
| [pg-replica-lag.md](pg-replica-lag.md) | P3 | `pg_replication_lag_seconds` > 30 |
| [redis-sentinel-failover.md](redis-sentinel-failover.md) | P2 | Sentinel переключил master; FSM-state / async-queues потенциально потеряны |

Phase 2 добавит:
- `kms-rate-limit.md` — Yandex KMS throttling (recording playback и
  envelope unwrap).
- `s3-presigned-expired.md` — массовые expired presigned URLs у клиентов.
- `clickhouse-ingest-lag.md` — analytics ingester отстаёт от NATS.

---

## Что НЕ в этом каталоге

- **Application-level debugging** (бизнес-логика, конкретные баги) — в
  GitHub Issues с label `bug`.
- **Architecture decisions** — в `docs/adr/`. Runbook ≠ ADR.
- **Performance tuning guides** — в `docs/architecture/`.
- **On-call rotation roster** — operational решение, не часть архитектуры;
  ведётся отдельно через HR/PM-инструменты.

---

## Связанное

- ADR registry: [`docs/adr/`](../adr/) — пятнадцать accepted решений
  (mod_verto, FreeSWITCH, dialer, recording, etc.) — контекст для
  каждого runbook'а.
- Спека: [`docs/superpowers/specs/2026-05-06-sociopulse-system-design.md`](../superpowers/specs/2026-05-06-sociopulse-system-design.md)
  § 15 (observability), § 13.7 (incident response), § NFR-2 (availability),
  § NFR-10 (logging principles).
- Domain glossary: [`CONTEXT.md`](../../CONTEXT.md) — терминология
  (operator, FSM, tenant, recording, outbox, RLS, audit log) каноническая;
  используйте её в новых runbook'ах.
- План Phase-2 деливераблов: [`docs/superpowers/plans/2026-05-06-20-observability-foundation.md`](../superpowers/plans/2026-05-06-20-observability-foundation.md)
  Tasks 2-7.
