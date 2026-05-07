# Ревью архитектуры и планов реализации СоциоПульс

**Дата:** 2026-05-06
**Объём ревью:** spec (157 KB, 2 482 строки) + 20 планов (1.8 MB, ~52 000 строк)
**Скиллы:** `engineering:system-design`, `superpowers:writing-plans`
**Метод:** 4 параллельных независимых ревью-агента + сводный анализ
- A — критика системного дизайна (FR/NFR, ADR, риски, capacity)
- B — кросс-планная согласованность (схемы, контракты, NATS-сабжекты, имена)
- C — production-readiness разбор 5 высоко-рисковых планов (08-12)
- D — соответствие 152-ФЗ, multi-tenancy isolation, security/ops

---

## Executive summary

**Архитектурно — фундамент здоровый.** Модульный монолит, RLS, envelope-KMS, single-source survey runtime, конфигурация в YAML — это правильные решения для сцены в 30 тенантов / 500 операторов. Документ выгодно отличается от среднего ТЗ дисциплиной (явный реестр конфигов, ADR, defence-in-depth таблица).

**Но к исполнению план не готов** — найдено **6 критичных** и **18 high-priority** проблем, которые при текущей формулировке либо приведут к 152-ФЗ-инциденту в первый месяц, либо к серебряной пуле "почему наша SLO 99.95% не достигается" через квартал, либо к двойным звонкам респондентам (тоже PII-инцидент).

**Топ-3 блокера, которые надо решить ДО старта кодирования:**
1. **Криптография для ПДн (ФСБ/ГОСТ)** — AES-256-GCM не сертифицирован как СКЗИ. Без явной позиции платформа не сможет принимать государственно-связанные тенанты (а ВЦИОМ это именно они).
2. **Capacity-математика не сходится** — NFR обещают 200 каналов peak, но при 50k/день × AHT 4.8 мин получается ~400 одновременных каналов. Либо NFR неверны, либо размерности FreeSWITCH-кластера.
3. **Recording integrity SLO 99.95% недостижима** — крах одного FS-узла мгновенно теряет ВСЕ in-progress записи, обнуляя бюджет потерь на ~5 дней разом. Этого нет ни в ADR-005, ни в риск-реестре.

**Рекомендация:** прежде чем агенты начнут исполнять Plan 00, прогнать **focused remediation pass** — на это потратить ~1-2 итерации (правки spec + 5-7 планов), потом запускать. Подробности ниже в § "Next steps".

---

## Критичные находки (must fix before execution)

### C1. Capacity-математика противоречит самой себе
**Где:** spec §NFR-1 (peak 200 каналов), §8.6 (`max_concurrent_per_node = 60` × 3 узла = 180), §16.7 (стоимость основана на 50k × 3 мин), Plan 08 (size FS VMs).

**В чём проблема.** При 50 000 звонков/день и AHT 4.8 мин (50k×4.8/60 = 4 000 машино-часов в день, реальный пик — 10% дневного объёма за час пик = ~5k звонков/час × 4.8 мин / 60 = **~400 одновременных talk-каналов**), кластер из 3 FS-узлов с потолком 60 каналов на узел не выдержит даже без headroom, который обычно закладывается +30%.

**Последствия.** Перегрузка FS-кластера в первый же deeling-day с приоритетным проектом → call setup p95 разъедет с 2 с до 30 с → массовые abandonment'ы.

**Фикс.**
- Пересчитать ёмкость от первого принципа: явно зафиксировать AHT, peak-to-average, occupancy в spec §1.3 (Domain assumptions).
- Erlang-B для размера trunk pool.
- `max_concurrent_per_node` поднять до 100 + 30% headroom; либо узлов сделать 5; либо горизонтальный auto-scale (Packer + Terraform pipeline).
- Согласовать NFR-1, §8.6, §16.7 по одной цифре.

---

### C2. FreeSWITCH split-brain / fencing не описан
**Где:** spec §5.3.1 (ESL-bridge: primary + hot-standby per FS-node), ADR-007 (FS вне k8s, public IPs), Plan 09 (telephony-bridge).

**В чём проблема.** При failover двух bridge-реплик нет explicit-механизма убедиться, что только одна владеет ESL-соединением к FS-узлу. На реальной NATS-партиции или slow-replica-pause обе реплики могут одновременно публиковать `originate` на тот же узел → дубль звонка респонденту. Дубль звонка к одному человеку = PII-инцидент по 152-ФЗ (нарушение принципа целевого использования).

**Фикс.**
- Ownership-lease per FS-node через Postgres advisory lock (тот же механизм, что выбран для retry-orchestrator в Plan 10) или etcd lease.
- Fencing-protocol: при failover старый владелец обязан получить `LOCK_LOST` (через session-disconnect Postgres advisory lock — встроено) ДО того как новый берёт lock.
- Документировать что происходит с in-flight `originate`, чей ack потерян.
- Добавить R-16 в риск-реестр.

---

### C3. RLS-bypass в одну строчку — нет defence-in-depth
**Где:** spec §6.1, §12.2, ADR-006. Plan 03 (`pkg/postgres.WithTenantTx`), Plan 04 (`tenancy_admin` BYPASSRLS).

**В чём проблема (3 связанных дыры).**
1. Поведение `SET LOCAL app.tenant_id` вне транзакции — no-op. Если разработчик вызовет `pool.Query(...)` без `BeginTx`, RLS-policy получит `current_setting('app.tenant_id', true) = NULL` → политика срабатывает как "пусто", но проверить это надо тестом, а в spec нет теста.
2. `tenancy_admin` с BYPASSRLS — это **роль Postgres**, а не connection-time gate. Если в будущем PR кто-то использует "не тот" DSN, BYPASSRLS утечёт молча.
3. PgBouncer transaction-mode + pgx prepared statements — известный footgun (имена prepared-stmt коллизятся между backend-conn). Plan 03 не настраивает `default_query_mode=exec` и не отключает pgx prepared cache.

**Фикс.**
- В `pkg/postgres` экспортировать **только** `WithTenantTx(ctx, tenantID, fn)`. Запретить экспорт `*pgxpool.Pool` за пределы пакета.
- `depguard` правило: `internal/*/store/*.go` нельзя импортировать `github.com/jackc/pgx/v5/pgxpool`.
- При старте cmd/api ассертить `SELECT current_user` для `app` пула возвращает `'app'`, не `'tenancy_admin'`.
- Tenancy-admin connection — отдельный пул с другим DSN, изолированный в `internal/tenancy/store/admin_db.go`, никто другой импортировать не может.
- Интеграционный тест: "вне transaction → 0 rows в любом запросе через app-пул". Запускать в CI matrix.
- В Plan 03 настроить pgx с `simpleProtocol=true` или `cache_mode=disabled`.

---

### C4. NATS at-least-once / ordering / durability не специфицированы
**Где:** spec §10.2 (subjects list), Plan 09, 10, 11, 13.

**В чём проблема.** В spec перечислены сабжекты, но не сказано:
- Какие из них на JetStream (durable), какие на core-NATS (best-effort).
- Acknowledgment policy (explicit, none, async).
- Retention/max-deliver/replay-window.
- Идемпотентность ключей у consumer.
- Как ClickHouse-ingest (Plan 13) восстанавливается после 30-минутной недоступности NATS, и что является источником истины — CH или Postgres.

**Последствия.** При сбое квоты в `project_quotas.done` могут быть посчитаны 1.5×, а в CH — 0.8× (или наоборот). Audit trail не воспроизводим. У dialer-FSM (Plan 10) ID call'а может прийти с `channel.hangup_complete` ДО `channel.answer` при кросс-replica переупорядочивании — FSM в `Dialing` получает событие "звонок завершён", в логах "EventCallEnded из состояния Dialing" → drop on the floor.

**Фикс.** Добавить в spec §10 таблицу:
```
| Subject pattern | Stream | Durable | Ack mode | Retention | Replay window | Consumer idempotency key |
```
Заполнить для **всех** сабжектов. Принять явное решение: CH есть денормализованный read-через-NATS (источник истины — Postgres `calls`/`call_attempts`), и при потере CH — replay из NATS если retention достаточный, иначе full-rebuild from PG. Документировать как ADR.

---

### C5. Deletion right (152-ФЗ ст. 21) распространяется только на горячие данные
**Где:** spec §13.3, Plan 06 (worker.respondents.purge), Plan 12 (recording retention).

**В чём проблема.** План удаляет:
- ✅ `respondents.phone_encrypted` после grace period.
- ✅ S3-recordings.
- ❌ ClickHouse `events_calls` (CH плохо удаляет; нужна стратегия `ALTER TABLE ... DELETE` или partition drop).
- ❌ `audit_log` payload jsonb, где может оказаться номер при поиске (FR-G2 "поиск по номеру" логирует запрос).
- ❌ NATS JetStream messages с PII-содержимым (если retention > grace period).
- ❌ Postgres WAL-archives (бэкапы все ещё содержат encrypted phone — единственная защита это уничтожить ключ; но KEK rotation/revocation procedure не описана).
- ❌ Consent prompt audit (хеш промпта + время прослушивания — вечный артефакт без процедуры).

**Фикс.**
- Добавить в spec §13.3 explicit deletion-propagation diagram, охватывающий: PG (горячее), CH (с partition strategy), все NATS streams, audit_log payload, WAL/backup (через crypto-shredding с revocation KEK), консент-промпт audit.
- Зафиксировать SLA максимального времени распространения удаления (Roskomnadzor требует "разумные сроки").

---

### C6. RPO/RTO обещания не подкреплены DR-механизмом
**Где:** spec §NFR-6 (RPO 5 min, RTO 30 min), §16.6 ("DR-зона: cold-standby, образа за < 30 мин").

**В чём проблема.** Cold image standby ≠ streaming replication. Реальный RPO будет = WAL queued at primary failure = от секунд до минут в зависимости от `archive_timeout`, ни одна из этих величин в spec не зафиксирована. RTO 30 минут на cold image deploy + DNS cutover + cache warmup — оптимистично для production.

**Фикс.**
- Решить однозначно: warm standby с streaming replication (RPO < 1 мин достижим, RTO ~ 5 мин на promote) ИЛИ cold image (RPO 5-15 мин, RTO 30+ мин). Не "что-то посередине".
- Если warm — добавить в Plan 01 Yandex Managed PG read replica в DR-AZ + автоматизация promote (либо ручной runbook).
- `archive_timeout` зафиксировать в Postgres config (`60s` обычно).
- DR-учения раз в квартал — добавить как процедуру в operations doc.

---

## High-priority находки (should fix during execution)

### Архитектура

**H1. Recording integrity SLO 99.95% недостижима при текущем дизайне.**
ADR-005 закладывает 25 потерянных записей/день при 50k. Но при крахе одного FS-VM теряются **все** in-progress записи на этом узле (~80 одновременных) — это ~80 потерь за event, что превышает дневной бюджет SLO одним инцидентом и блокирует SLO на ~5 дней. mod_record_session пишет в локальный disk на FS-VM до transcoding — данные не реплицируются.

**Фикс (выбрать один):**
- (a) `mod_audio_fork` → реплика в sibling-VM real-time (сложность high, но честные 99.95%).
- (b) Понизить SLO до 99.5% и явно задокументировать "при крахе FS-VM теряются in-progress recordings" как известный сценарий и risk R-16.
- (c) Shared NVMe (NFS/CephFS) for spool — dependency на инфраструктуру, удаляет single-VM блокирующий риск, но добавляет другие.

Я бы рекомендовал (b) на старте + (a) в roadmap — честнее.

**H2. ADR-008 (WASM survey runtime через TinyGo) — проектировать без измерений нельзя.**
- TinyGo плохо поддерживает reflection и `expr-lang/expr` (whitelisted DSL evaluator) → возможно потребует переписать DSL под TinyGo-supported subset.
- WASM bundle ~200KB+ инфлирует TTI (NFR-1 "TTI < 2s на Core i3").
- Альтернатива (TS-port того же DSL с golden-tests против Go-runtime) даёт single-source без runtime-WASM-overhead.

**Фикс.** Прежде чем кодить Plan 07: прогнать proof-of-concept (compile DSL evaluator с TinyGo, измерить TTI на target hardware). Если не получается — переписать ADR-008, выбрать TS-port, привести golden test pattern.

**H3. Redis Sentinel failover убивает dialer.**
Plan 10 использует `ZPOPMIN` и `INCR`-rate-limit. При Sentinel failover (10-30 с): writes отвергаются, dialer queue стопится. Хуже — split-brain или write-loss во время failover может вернуть `ZPOPMIN`'ed респондента → двойной звонок (PII!).

**Фикс.**
- Mark "in-flight" в Postgres ДО `originate` (не только в Redis).
- Документировать ожидаемый failover blast radius ("2-3 мин dialer pause" — приемлемо, но фиксируется).
- Phantom-recovery при unmark "in-flight" если call-attempt не перешёл в `done` за 60 с.

**H4. KMS rate-limit (R-13: 100 RPS) недо-размерен для пика.**
50k/день × 70% в peak hours / 4 часа = ~2.5 originates/sec средне, peak 5-10/sec. Каждый recording commit = 1 KMS `GenerateDataKey`, плюс DEK-decrypt при первом доступе к respondent. С 30 тенантами и cache misses across pods — **легко** перегружает 100 RPS на одного тенанта.

**Фикс.**
- Singleflight per-tenant DEK resolution.
- Per-tenant KMS rate-limiter с очередью.
- Документировать fallback при KMS rate-limit (queue+retry, не fail-the-call).

**H5. SIP-trunk credentials rotation requires Packer rebuild.**
Spec §12.6 говорит про Lockbox + Ansible, но trunk credentials живут в `sip_profiles_trunks.xml.j2` — rotation = Packer rebuild → Terraform reapply → 10-минутный outage на rolling restart. У реальных трункодавцев rotation 90-дневная — это значит 4 outage в год.

**Фикс.** Перевести trunks на mod_xml_curl (gateway-XML-CURL endpoint в cmd/api, аналогично per-call SIP user) с TTL-cache на стороне FS. Rotation = `fs_cli reload mod_sofia` + cache invalidation, downtime 0.

### Compliance / security

**H6. Кросс-границы telemetry exposure.**
Spec §15.7 пишет "Sentry — Yandex Cloud-hosted либо собственный". §16.7 показывает "Grafana Cloud если не self-hosted". Это бинарное решение, не "or". SaaS Sentry/Grafana хостятся вне РФ → утечка любого PII (телефон, IP, имя) = нарушение ст. 18.

**Фикс.** Зафиксировать в spec:
- Frontend Sentry → ОБЯЗАТЕЛЬНО self-hosted на MKS либо отказаться от Sentry.
- Grafana — только self-hosted на MKS (kube-prometheus-stack, что и так в Plan 01).
- Datadog/Cloudflare/etc. — запрещены.
- Добавить CI-проверку: Sentry DSN в frontend bundle должен быть `*.sociopulse.ru` или `*.yandexcloud.net`.

**H7. Refresh-rotation reuse detection — race + UX gap.**
Plan 05 определяет Lua-script на reuse detection. Но при детекте отзываются ОБЕ сессии (легитимная + атакерская) — UX: пользователь внезапно logged out с "session reused". Это правильное поведение, но spec/Plan 05 нигде не пишет user-facing-сообщение и нет UI-обработки.

**Фикс.** Добавить в Plan 19 (frontend-admin-2) обработку 401 с reason=`session_reused` → показать модал "Ваша сессия использовалась с другого устройства, войдите снова".

**H8. TOTP backup codes: column есть, service-method нет.**
Plan 05 миграция 0012 имеет `backup_codes bytea`, но нет API для регенерации/использования backup-кодов. Если пользователь потерял устройство — admin force-disable 2FA (privilege escalation path без dual approval).

**Фикс.** В Plan 05 добавить:
- `RegenerateBackupCodes(ctx, userID)` — печатает 10 одноразовых кодов.
- `Verify(code)` — fallback с use-once семантикой.
- В Plan 17/19 — UI "Скачать резервные коды".
- Admin force-disable 2FA — требует second-admin approval (через `audit_log` + `pending_actions` table).

**H9. Аудит-лог не tamper-evident.**
Spec §FR-K1 "append-only", но это просто свойство хранения в PG — отсутствует hash-chain (Merkle), нет WORM-bucket, нет signed periodic anchoring. Roskomnadzor / FSB Order 378 §10 ожидают этого.

**Фикс (v1.5):** добавить hash-chain на `audit_log` (каждая строка содержит hash предыдущей), периодическая публикация Merkle-root в WORM-bucket (Yandex Object Storage Object Lock).

**H10. Per-tenant S3 bucket — не создаётся при `TenantService.Create`.**
Spec §12.1 L5 говорит "bucket-per-tenant". Plan 01 создаёт **только** платформенные buckets (backups, reports, consent_prompts, tfstate). Plan 04 создаёт KEK, но нет вызова `yandex_storage_bucket` create. Plan 12 использует `s3_bucket` из конфига, не создаёт его сам.

**Фикс.** В Plan 04 `TenantService.Create` добавить шаг:
1. Создать KEK через Yandex KMS API.
2. Создать bucket `sociopulse-recordings-<tenant>` через Yandex Object Storage API с SSE-KMS на этом KEK.
3. Bucket policy: `s3:GetObject` только из tenant's own service account.
4. Удаление bucket — отдельный шаг при `TenantService.Delete` (с проверкой что нет not-yet-deleted recordings).

### Production-readiness (Plans 08-12)

**H11. Disk-full на /var/spool/sociopulse — silent.**
Plan 08 размер диска 200 GB, но при stalling uploader (100 retries × 12h) накапливается ~40 GB/узел/день. Нет watermark-alert, нет emergency eviction. `mod_record_session` при ENOSPC просто логирует и продолжает звонок без записи → 152-ФЗ violation.

**Фикс.**
- Disk watermark alert (>70%) в Plan 08 + Plan 13.
- Emergency eviction-by-age script (если >85% — удалять uploaded-но-не-purged старше N дней).
- `mod_record_session` configure fail-closed (повесить звонок, если recording невозможна).

**H12. mod_xml_curl directory endpoint — SPOF.**
Plan 09 §6 directory-endpoint живёт в `cmd/api`. Helm rolling deploy → 30-60 с unavailability → каждый новый per-call SIP-user lookup fails → originate fails. **Все 500 операторов теряют возможность принимать новые звонки на время deploy.**

**Фикс (выбрать).**
- (a) Directory-endpoint в отдельном low-churn deployment с replicaCount=3 + PDB.
- (b) FS-local nginx cache (5-30 с TTL) — fallback на устаревшие credentials при недоступности cmd/api.
- (c) `mod_xml_curl disable-on-failure=true` + статический fallback directory.

Я бы (a) + (b) одновременно.

**H13. Listen-in SIP user accumulation на admin disconnect.**
Plan 11 §Task 6: при создании listen-in SIP user `lst_<admin>_<call>` TTL credentials 4h. Если admin browser tab упал — `Stop` не вызывается, mixmonitor leg остаётся в FS до hangup исходного звонка. SIP user'ы в Redis накапливаются.

**Фикс.**
- WS-disconnect handler в Plan 11 обязан вызывать `ListenIn.Stop` для всех сессий, owned by этой connection.
- Janitor worker сканирует `listen:session:*` каждые 5 мин и валидирует против открытых WS connections — оркужает orphans.
- Тест "admin closed tab during listen-in → SIP user gone в течение 60 с".

**H14. Drop-oldest backpressure теряет billing-critical events.**
Plan 11 writer-loop drop-oldest применяется ко всем frames без classification. `call_finalized` (Plan 12 → NATS → Hub → admin UI live billing meter) — такой же frame. При slow connection — самые старые события (которые могут быть критичны для биллинга) дропаются.

**Фикс.** Classify frames в Plan 11:
- `critical` (call_finalized, recording_committed, quota_breach) — отдельная bounded queue, при переполнении — disconnect клиента, не дроп.
- `telemetry` (presence, dialer_state, metric_tick) — drop-oldest OK.

**H15. AES-256-GCM и HTTP Range несовместимы.**
Plan 12 §1 обещает "envelope decryption + AES-256-GCM with HTTP Range support". AES-GCM auth tag в конце, шифр-поток непрерывный — range natively не поддерживается. Реальные опции:
- (a) Decrypt-all-into-RAM-then-slice (200 MB запись × 50 supervisors auditing = 10 GB RAM).
- (b) Chunked-AES-GCM-SIV с per-chunk tags (нужен custom envelope формат).

Plan 12 не специфицирует ни (a), ни (b).

**Фикс.** Принять явное решение в Plan 12:
- v1: streaming whole-file, без Range. UI просит "play/pause", не seek. Простой dev. Memory-light if streaming directly to client.
- v2 (рекомендую): chunked envelope (64 KB chunks, AES-GCM-SIV with chunk index as nonce). Поддерживает Range, минимальный memory overhead.

**H16. Outbox-таблица не определена в Plan 03 + relay process не определён.**
Plan 10 (dialer) пишет audit-events в outbox для eventual delivery to NATS. Plan 03 не создаёт `outbox` таблицу. Нет плана, который определяет relay worker (`outbox-relay` cmd, или sub-job в cmd/api).

**Фикс.** Это **bug-fix уровня plan-text**, а не архитектурная переработка:
- Добавить migration в Plan 03: `CREATE TABLE outbox (id BIGSERIAL PK, aggregate VARCHAR, subject TEXT, payload JSONB, created_at TIMESTAMPTZ, dispatched_at TIMESTAMPTZ NULL)`.
- Добавить в Plan 02 (cmd/api) startup: `outbox-relay` goroutine — каждые 1 с читает 100 строк WHERE dispatched_at IS NULL, публикует в NATS, ставит dispatched_at.
- Документировать idempotency: NATS-consumer должен держать `outbox.id` как dedup-ключ.

**H17. Retry-orchestrator leader-election: spec/Plan disagree.**
Plan 10 self-review говорит "Postgres advisory lock", file-structure говорит "Redis SETNX-based leader lock". При Redis SETNX — paused-but-not-dead instance ещё держит lock, при unpause собирает `pending_retries` параллельно с новым leader → **двойной dispatch retry → двойной звонок**.

**Фикс.** Зафиксировать Postgres advisory lock (auto-released on session loss). Если хотим Redis SETNX — нужен fencing-token и проверка на каждой операции (сложнее, не выигрыш).

**H18. ESL active_channels counter не имеет reconciliation.**
Plan 09 INCR/DECR Redis counter. Три failure-режима: bridge crash после INCR, FS crash с 60 active calls, Redis flush. **Нет reconciler**, который сравнивал бы Redis с `api show channels count` от FS.

**Фикс.** В Plan 09 добавить:
- 30-секундный reconcile loop: для каждого FS-узла `api show channels count` через ESL → SET active_channels:<node>.
- Метрика staleness `bridge_active_channels_drift{node}`.
- Alert при drift >10 в течение 5 мин.

---

## Medium-priority находки (улучшения)

### Архитектура

- **M1. ADR-011 (NATS over Kafka) недо-аргументирован для replay.** R-12 обещает rebuild CH из NATS, но retention NATS не зафиксирован per-stream. Если CH потерян на месяц-2 — replay window не покрывает. Зафиксировать per-stream retention в spec; либо явно "S3-backed replay (daily archive)".
- **M2. Cache poisoning через NATS settings invalidation.** Plan 04 — invalidation handler должен делать fetch-on-invalidate, не consume payload. Проверить в коде Plan 04.
- **M3. ADR-009 (rejection of Tailwind) недо-аргументирован.** Решение оставить hand-CSS — может быть правильное (low-risk port from prototype), но "потеря дизайна" — слабый аргумент. Переписать как "trade-off: Tailwind дал бы CI-tokens-validation, но миграция без regression уже задизайнена в Plan 15 → не оправдывает refactoring".
- **M4. §17.6 chaos scenarios пропускают важное.** Не хватает: PG primary failover during write-heavy import, Redis Sentinel failover during dialer pop, FS-VM kernel panic mid-call. Добавить.
- **M5. Schema gaps.** `surveys.current_version_id` → `survey_versions(id)` нет FK (комментарий "potentially circular" — уточнить или deferrable FK). `respondents.region_code` — текстовое поле, должно быть FK на `regions` (или enum).
- **M6. Backup encryption — single platform key.** Bucket backups шифруется одним платформенным KEK → его компрометация = читать все тенантские бэкапы. Spec §12.1 обещает per-tenant blast radius — это противоречие. Использовать SSE-KMS per-tenant для backup buckets, либо явно accept trade-off в ADR.
- **M7. §8.5 retry через парсинг "free-text comment" оператора** — locale-зависимый кошмар. Использовать структурированное `callback_at datetime` поле.

### Cross-plan consistency (по результатам Agent B)

- **M8. NATS subject naming inconsistency.** Plan 11 ожидает `tenant.<id>.dialer.op.<op>.state`. Plan 10 не показывает explicit публикацию (использует outbox-pattern, но subjects.go упомянут без content). Plan 09 публикует `telephony.event.recording.stop` — нет tenant prefix.
   **Фикс.** В spec §10.2 утвердить **canonical** subject naming: `tenant.<tid>.<area>.<entity>.<id>.<event>`. В Plan 09, 10 — добавить явные subject constants и publish-points в код.
- **M9. Plan 08 ссылается на "Plan #09 backend"** для Recording.Commit — должно быть Plan #12. Это документная ошибка, не архитектурная — поправить тексты планов.
- **M10. Plan 10 dependency ссылается на "Plan 04 (NATS)"** — Plan 04 это tenancy-module. NATS общая инфра, не отдельный план. Поправить depends-on.
- **M11. `OperatorState` enum.** Plan 10 определяет (StateOffline, StateReady, StateDialing). Plan 11 не определяет тип события `OperatorStateEvent` явно — JSON-payload mismatch risk.
   **Фикс.** Добавить в spec §10.4 (или в `docs/api/events.md`) JSON-schema для каждого NATS-события. Plan 10 публикует, Plan 11 валидирует, frontend (Plan 16, 17) парсит — единый source-of-truth.
- **M12. Auth Redis keys не тенант-префиксированы.** Plan 05 `auth:rl:ip:<ip>` — global. В сценарии "колл-центр Tenant A и Tenant B сидят в одном офисе за одним NAT IP" один шумный тенант DoS'ит логины другого.
   **Фикс.** Изменить ключи на `auth:rl:ip:<tenant>:<ip>` (после tenant resolution из subdomain или from email lookup).

### Compliance / security

- **M13. Pepper rotation для phone hash не описан.** Если pepper компрометирован — все хеши пересчитать. Добавить runbook.
- **M14. DPA + sub-processor list.** 152-ФЗ требует публиковать sub-processors. Yandex Cloud, npm registry, Sentry, libpostal — отсутствует список.
- **M15. ClickHouse RLS / row policies.** Spec не определяет CH-уровень изоляции — полагается на application discipline (`WHERE tenant_id=...`). CH поддерживает `CREATE ROW POLICY` — добавить в Plan 13.
- **M16. Backup на DEK-уровне.** WAL содержит plaintext non-encrypted columns (region_code, full_name если он не в encrypted_pii, audit_log payload). Это в platform-key-encrypted backup — единственная защита.
- **M17. TLS cipher pinning.** Spec пишет "TLS 1.3", не пинит cipher suites; не пинит min TLS на Managed PG/CH/Redis (Yandex defaults могут разрешать TLS 1.2).

### Operations

- **M18. Mid-call rollback runbook отсутствует.** Если deploy bad cmd/api at 14:00, 200 calls in flight — что делать? ArgoCD rollback хорошо для cmd/api, но `operator_sessions`, `calls.in-progress`, Redis FSM hash могут быть в inconsistent state.
- **M19. Online schema migrations.** Plan 03 использует golang-migrate (synchronous DDL). Добавление NOT NULL колонки в `respondents` (1.5M строк) под нагрузкой = lock storm. Использовать pgroll / pg-osc / two-phase pattern.
- **M20. SIEM integration.** `audit_log` shipping в SIEM (MaxPatrol / Kaspersky KSC / JSOC) — часть 152-ФЗ Order 21 §V.4. Loki не SIEM.
- **M21. Dashboards как TODO.** Spec §15.6 перечисляет 7 дашбордов, ни один план их не создаёт. Нужно либо в Plan 01 (kube-prometheus-stack default + custom JSONNet), либо отдельный план "Plan 20: Observability dashboards" с YAML/JSONNet sources.

---

## Low-priority / nitpicks

- **L1.** `surveys.default_survey_version_id uuid, -- snapshot пини` — typo "пини" → "пиннится".
- **L2.** Glossary §19 ссылается "see 1.2" — лучше консолидировать.
- **L3.** `temp SIP account op_<user_id>_<session>` cleanup runs hourly — учётки растут unbounded между cleanup runs. Перевести cleanup на event-driven (logout WS-disconnect → immediate cleanup).
- **L4.** §10.5 "drop старых кадров" для state events — нужно coalesce by topic+key, keep latest, не drop oldest in time order.
- **L5.** `continue_on_fail=NORMAL_TEMPORARY_FAILURE,...` в FS dialplan приводит к "callee leg failed → caller hears silence till 45s timeout". Лучше playback "all operators busy" + bridge to callback queue.
- **L6.** **Consent prompt fail-open bug** — dialplan condition `^(true|1|yes)?$` matches **anything** (including empty). Если переменная `consent_required` не выставлена — prompt не играет, recording стартует. **Фикс:** `^(true|1|yes)$` с `<anti-action>` повесить звонок если unset. (Это формально critical, но локализован в одной строке dialplan'а.)

---

## Что сделано хорошо

5 пунктов, которые особенно отметили агенты A и D:

1. **Defence-in-depth tenant isolation таблица** (spec §12.1) — RLS + KMS per-tenant + bucket-per-tenant + NATS accounts — правильный pattern.
2. **Recording envelope encryption** (spec §9.2) — следует AWS-style правильно (encrypted DEK alongside object).
3. **Modular monolith + named cross-module interfaces + depguard enforcement** (spec §5.1) — необычно дисциплинировано для пре-execution-spec.
4. **Configuration registry** (spec §14) — explicit YAML vs `tenant_settings` разделение с named keys.
5. **FSM diagram + single-source survey schema** — concept clean.

Plus: **20 планов**, каждый с self-review, TDD-rhythm, file-structure tree, exact paths — это уровень готовности, который я редко вижу в брифах для исполнителей.

---

## Verdict

**Spec и планы — выше среднего по дисциплине, но не execution-ready.** 6 critical findings — "must fix" перед стартом. 18 high-priority — можно фиксить параллельно с исполнением, но нельзя забыть. После remediation pass можно запускать subagent-driven-development.

**Real-money риски, в порядке "разбудит в 3 утра":**
1. Recording loss при crash FS-VM (H1, ADR-005) — SLO promise, регулятор-ground.
2. ГОСТ/ФСБ требования (D-critical) — невозможно продать государственно-связанным тенантам.
3. RLS/multi-tenancy bypass (C3) — single line of code from full leak.
4. Двойные звонки (H3 Redis Sentinel + H17 retry leader) — ст. 5 152-ФЗ "целевое использование" нарушение.
5. Disk-full на FS spool → recording отключается без alarm (H11) — ст. 9 152-ФЗ нарушение.

**Что в spec/планах "не страшно" но надо доделать:**
- Cross-плановая согласованность (Outbox таблица, NATS subject naming, JSON-schema события).
- AES-GCM Range — переписать Plan 12 §HTTP-handlers на chunked envelope.
- Deletion right propagation — добавить пункты в spec §13.3.
- DR-runbook, mid-call rollback — отдельный документ.
- Dashboards — Plan 20 (новый).

---

## Next steps

Я бы сделал такой план remediation pass'а перед стартом исполнения — **в указанном порядке, ~5-7 итераций**:

### Шаг 1 (требует решения пользователя — нельзя автоматически)
- **Решение по ГОСТ/ФСБ криптографии.** Юридически-зависимое; Claude не должен решать. Вопрос: "Готовы ли мы к инспекции ФСБ как оператор ПДн с СКЗИ-сертификатом, или ограничиваем себя УЗ-4 ИСПДн (низкий уровень защищённости)?"
- **Решение по recording integrity SLO.** 99.95% или 99.5%? — выбор между сложностью и обещанием.
- **Решение по DR.** Warm standby с streaming replication или cold image?
- **Решение по WASM survey runtime.** Run TinyGo PoC, замерить TTI, и решить.

### Шаг 2 (документные правки — могу сделать сейчас)
- Capacity-математика: переписать §1.3 + NFR-1 + §8.6 + §16.7 после согласования.
- NATS subject canonical naming + JSON-schema event payloads → spec §10 / новый `docs/api/events.md`.
- Deletion right propagation diagram → spec §13.3.
- 152-ФЗ data subject rights: rectification + blocking — добавить FR.
- Telemetry self-hosted решение → spec §15.7 пин-binary.

### Шаг 3 (правки планов — могу сделать сейчас)
- **Plan 03:** добавить outbox table + относящиеся индексы.
- **Plan 02:** добавить outbox-relay goroutine.
- **Plan 04:** `TenantService.Create` создаёт S3 bucket per-tenant + bucket policy.
- **Plan 04:** `pkg/postgres` экспортирует только `WithTenantTx`; depguard rules.
- **Plan 09:** active_channels reconciliation loop + drift metric.
- **Plan 09:** mod_xml_curl directory вынести в low-churn deployment + FS-local cache.
- **Plan 10:** lock policy зафиксировать (Postgres advisory lock).
- **Plan 10:** mark "in-flight" в Postgres ДО originate (defence против Redis Sentinel failover).
- **Plan 11:** classify frames (critical vs telemetry) для backpressure.
- **Plan 11:** WS-disconnect handler stops listen-in sessions + janitor worker.
- **Plan 12:** AES-GCM Range — выбрать chunked envelope, переписать §HTTP handlers.
- **Plan 12:** UNIQUE constraint composite `(tenant_id, call_id)`.
- **Plan 05:** TOTP backup codes service-method + UI hooks; refresh-reuse UX message.
- **Plan 08:** disk watermark alert + emergency eviction + record_session fail-closed.
- **Plan 08:** consent prompt regex `^(true|1|yes)$` + anti-action.
- **Plan 19:** session_reused 401 handler.

### Шаг 4 (новый план — могу написать сейчас)
- **Plan 20: Observability dashboards & runbooks** — JSONNet/Grafonnet dashboards, alert rules, severity matrix, on-call rotation, SEV runbooks (FS-VM-down, NATS-partition, Redis-failover, deploy-rollback).

### Шаг 5 (тесты — частично новые)
- Integration test: "missing app.tenant_id ⇒ 0 rows AND warn-level log в течение 100мс".
- Integration test: "kill FS-VM mid-call → все плагины конvergируют в clean state в 60 с".
- Integration test: "Redis Sentinel failover → dialer pause < 3 мин, нет double-dial".
- Property test: leader-election + fencing → ровно один dispatch retry.
- Chaos scenarios automation в `test/chaos/`.

---

**Готов реализовать шаги 2, 3, 4 — это объективные правки, не требующие человеческого решения.** Шаг 1 — за тобой; нужно решить 4 пункта (ГОСТ, SLO, DR, WASM). После согласования — могу прогнать всё за 1-2 итерации. Текущий код пока не написан, переписывать только тексты — самый дешёвый момент.

Скажи — начинаю с шага 2-3 (правки spec и планов) или сначала обсуждаем решения шага 1?

---

## Remediation log (2026-05-06, post-review pass)

Пользователь дал директиву: "система должна работать и хорошо работать, со старта иметь хорошую наблюдаемость в виду телеметрии и логирования, но без упоранства... в госты и подобное не уходим". Применён pragmatic remediation pass: только реальные дыры, без compliance-grade.

### Применено

**Spec (`docs/superpowers/specs/2026-05-06-sociopulse-system-design.md`):**
- §NFR-1: capacity-метрики приведены в соответствие с реальностью (200/250 → 400/500 каналов peak; добавлен расчёт с peak-to-average × AHT).
- §8.6: `max_concurrent_per_node` 60 → 100; кластер 5 узлов prod (3 dev) = 500 каналов; добавлено упоминание reconciler.
- §10.2: NATS subjects получили каноническую схему `tenant.<t>.<area>.<entity>.<id>.<event>`, таблица расширена столбцами Stream/Durable/Ack/Retention; объявлены правила (event-streams JetStream durable + idempotency, commands core-NATS best-effort); JSON-Schema event payloads привязаны к `docs/api/events/`.
- ADR-005: пересмотрен — целевой SLO 99.5% (uploaded), полная потеря при крахе FS-VM явно принята как trade-off; добавлены митигации (recording_lost флаг, ре-обзвон, backlog v2).
- ADR-008: статус Conditional Accept — TinyGo PoC требуется в Plan 07 Task 0; явный fallback на TS-port + golden tests если PoC fails (порог TTI < 200мс, bundle < 500KB gz).
- §18 Risks: добавлены R-16 (FS-VM total loss → recording window), R-17 (Redis Sentinel double-dial), R-18 (mod_xml_curl SPOF на deploy), R-19 (cross-tenant subscription через WS), R-20 (listen-in SIP user accumulation).

**Plans (новый Plan 20 + правки в 02, 03, 04, 08, 09, 10, 11, 12, 19):**
- **Plan 03 Task 6 (новый):** `event_outbox` таблица + `pkg/outbox` пакет (`Event`, `Writer`, `PostgresWriter`, `Relay`, `Publisher`). `FOR UPDATE SKIP LOCKED` → relay безопасен на каждой реплике без leader election. Plan 10 import path исправлен на `pkg/outbox`.
- **Plan 02 Task 4 (новый):** wiring outbox-relay goroutine в `cmd/api/main.go` + `OutboxConfig` секция в config.
- **Plan 04 Task 6 (новый):** `BucketProvisioner` API + Yandex Object Storage adapter (idempotent, SSE-KMS с tenant KEK, lifecycle, IAM policy); `TenantService.Create` создаёт bucket; admin endpoint `Repair` для retry. `pkg/postgres.Pool` lockdown — экспортирован только `WithTenantTx`, depguard блокирует `pgxpool.Pool` import снаружи, startup-assert на `current_user='app'`.
- **Plan 08 Task 6 (новый):** spool watchdog Bash-script + systemd timer (60s) с тремя порогами (70/85/98%); fail-closed `record_session` через sentinel-файл и dialplan pre-check; Prometheus alerts.
- **Plan 09 Task 6 (новый):** `active_channels` reconciler — раз в 30 сек ESL `api show channels count` → перезапись Redis-счётчика; gauge `bridge_active_channels_drift` + alert при drift>10/5min.
- **Plan 10:** строка file-structure поправлена — Postgres advisory lock (вместо Redis SETNX); согласовано с self-review.
- **Plan 11 Task 10 (новый):** `FrameClass` enum (Critical / Telemetry); `Connection` две очереди — critical (32, overflow → disconnect) и telemetry (256, drop-oldest); writerLoop приоритизирует critical. `Hub.RegisterCleanup` + `onConnectionClose` запускают зарегистрированные hooks; `ListenInService.Start` регистрирует cleanup → `Stop` на disconnect; `ListenInJanitor` 5-мин orphan sweep. `TopicRBAC.Allow` валидирует `filter.{OperatorID,ProjectID,CallID}.TenantID == claims.TenantID` через cached resolvers.
- **Plan 12:** v1 без HTTP Range (`Accept-Ranges: none`), trade-off задокументирован inline; chunked envelope перенесён на backlog v2. UNIQUE на `(tenant_id, call_id)` НЕ добавлен — `call_id` уже глобально уникален; cross-tenant защита через app-level pre-check `WHERE id=$1 AND tenant_id=$2` (уже в коде Task ~882) + mTLS-cert validation, добавлен явный пункт в self-review.
- **Plan 19:** обновлено описание audio-плеера — `<audio preload="metadata">` без seek-via-Range, browser auto-disables seek bar при `Accept-Ranges: none`.

**Plan 20 (новый, 48KB, 1184 строки) — Observability foundation:**
- Severity matrix (P1/P2/P3) + incident response basics в `docs/runbooks/README.md`.
- Top-10 runbooks (api-down, fs-vm-down, dialer-stalled полностью; остальные 7 по шаблону).
- Prometheus alert rules — SLO burn-rate (multi-window multi-burn-rate), telephony, recording, realtime, infra; CI rule `scripts/check-runbook-links.sh` проверяет связь alert ↔ runbook.
- 7 Grafana dashboards as code (Grafonnet + JSONNet) — overview, tenant-overview, telephony, recording, realtime, api-gateway, infra. Шаренная library в `lib/panels.libsonnet`.
- `cmd/synthetic` — canary runner с тремя scenarios (login, list-projects, originate-test-call); раздельный test-tenant 00...01 с изолированным trunk pool.
- `cmd/status-page` — минимальная in-house HTML-страница, читает Alertmanager API, рендерит per-сервис green/yellow/red на основе active alerts; serve через Ingress `status.sociopulse.ru`.
- Alertmanager routing — критические в PagerDuty + Slack `#incidents`, warning в `#alerts`, info в email-digest.

### Что осталось НЕ применено (по директиве пользователя)

Compliance-grade и over-engineering — выкинуто:
- ГОСТ/ФСБ-сертифицированная криптография (СКЗИ).
- Tamper-evident audit log (Merkle hash chain).
- BYOK / customer key custody.
- 24-часовой breach SLA runbook + ГосСОПКА интеграция.
- Roskomnadzor data operator registration playbook.
- Полная diagram распространения deletion right (только bullet-уровень упоминания CH partition delete).
- SBOM / sigstore image signing / bug bounty.
- Cross-region warm streaming replication для DR (оставлен cold standby с честными RPO 15 мин / RTO 60 мин).
- 24/7 on-call rotation roster (operational concern, не infra).
- Multi-tenant customer-facing dashboards (v2).
- SIEM integration (audit_log → MaxPatrol/JSOC) — v2.

### Status

- 21 план (00-20) суммарно ~1.85 MB / 53 124 строки. Все имеют self-review.
- Spec: 2 521 строка (was 2 482 — +39 строк правок).
- Готовы к запуску subagent-driven-development. Recommended sequence: 00 → 01 → 02 → 03 → 04 (с Task 6 lockdown) → 05 → 06 → 07 (с TinyGo Task 0) → 08 → 09 → 10 → 11 → 12 → 13 → 14 → 15 → 16 → 17 → 18 → 19 → 20.

---

## Phase 1 / Phase 2 split (2026-05-06 update)

Per project decision, **cloud infrastructure is deferred until product is implemented and ready for prod-deploy**. The 21 plans are now split into two phases.

### Phase 1 — Local-first product development (zero cloud spend)

Everything runs on a developer's laptop via `docker-compose.dev.yml` (Plan 02 Task 5). Tests via testcontainers-go in CI. No Yandex Cloud touch.

**Recommended execution order:**
```
Plan 00 (foundation: Go monorepo, Makefile, CI, hello-world cmd/api)
  ↓
Plan 00a (architecture foundation: docs/architecture/, ADRs, CONTEXT.md, all internal/<module>/api/ contracts, pkg/* abstractions, cmd/* scaffolds, depguard rules)  ← NEW
  ↓
Plan 02 (cmd/api skeleton + docker-compose.dev.yml = entire local stack)
  ↓
Plan 03 (database, migrations, pkg/postgres, pkg/outbox)
  ↓
Plan 04 (tenancy + KMS abstraction + S3 bucket creation + pkg/postgres lockdown)
  ↓
Plan 05 (auth + JWT + TOTP + RBAC)
  ↓
Plan 06 (CRM: respondents/projects/quotas/imports)
  ↓
Plan 07 (surveys: schema + DSL evaluator; TinyGo PoC in Task 0)
  ↓
Plan 08 Task 0 ONLY (FreeSWITCH-in-Docker for telephony-bridge dev)
  ↓
Plan 09 (telephony-bridge: ESL client, Router, active_channels reconciler)
  ↓
Plan 10 (dialer: OperatorFSM, queue, retry orchestrator with PG advisory lock)
  ↓
Plan 11 (realtime: WebSocket Hub, listen-in v1, frame classification)
  ↓
Plan 12 (recording: gRPC Commit, S3 streaming, retention worker)
  ↓
Plan 13 (analytics + reports: ClickHouse ingest, preset reports, async exports)
  ↓
Plan 14 (billing: cost calculator, tariffs, monthly aggregates)
  ↓
Plan 15 (frontend foundation: React+Vite+layout+API client+WS hub)
  ↓
Plan 16 (frontend operator workstation)
  ↓
Plan 17 (frontend admin part 1: overview/operators/dialer/projects)
  ↓
Plan 18 (frontend survey builder)
  ↓
Plan 19 (frontend admin part 2: users/calls/finance/reports + E2E)
  ↓
Plan 20 Task 1 ONLY (severity matrix + runbook README — documentation deliverable)
```

**Phase 1 deliverable**: a fully working product runnable on one machine, demonstrable to potential customers, fully tested. No cloud infrastructure needed at any point.

**Cloud cost during Phase 1**: 0 ₽/мес. The team operates entirely on local Docker.

**Estimated duration** (depends on team and pace): 3-9 months for a small team / one fullstack working diligently.

### Phase 2 — Pre-production cutover (1-2 weeks after Phase 1 complete)

Once Phase 1 is finished and the product is validated, deploy to real cloud infrastructure. At this point, **decide hosting strategy** (managed Yandex services / self-hosted on Compute VMs / hybrid) — see `docs/superpowers/reviews/2026-05-06-architecture-and-plans-review.md` "Variants A-E" section for cost trade-offs.

**Phase 2 execution order:**
```
Plan 01 (Yandex Cloud infrastructure: VPC + MKS + chosen DB hosting + KMS + Lockbox + S3)
  ↓ (decide DB layer: managed / self-hosted / hybrid)
Plan 08 Tasks 1-6 (production FreeSWITCH cluster: Packer + Ansible + 5 VMs + TURN)
  ↓
Plan 20 Tasks 2-7 (production observability: kube-prometheus-stack + alerts + ArgoCD + synthetic + status-page + Alertmanager routing + 10 runbooks)
  ↓
Initial deploy: ArgoCD applies all Helm charts → cmd/api running on MKS
  ↓
Run cmd/migrator against managed PG → schema applied
  ↓
DNS cutover: api.sociopulse.ru / app.sociopulse.ru / status.sociopulse.ru → ALB → ingress
  ↓
Onboard first tenant in staging → validate end-to-end
  ↓
Production launch
```

**Phase 2 deliverable**: production environment ready for first paying tenants.

**Estimated duration**: 1-3 weeks of focused work (depending on hosting model — managed is faster, self-hosted requires more Ansible work).

**Estimated cost (production, 30 tenants peak)**: ~150-230k ₽/мес depending on hosting model (managed: high end, self-hosted: low end).

### What this split changes in the existing plans

- **Plan 01** — `🛑 DEFERRED — Phase 2` banner added. Plan content unchanged, just postponed.
- **Plan 02 Task 5** — `docker-compose.dev.yml` extended with `freeswitch` container (telephony profile) and `prometheus + grafana + loki` containers (observability profile). All optional via profiles.
- **Plan 08** — Split: Task 0 (FreeSWITCH-in-Docker for Phase 1) ADDED, Tasks 1-6 marked deferred to Phase 2.
- **Plan 20** — Banner added: Task 1 (severity matrix + runbooks README) is Phase 1 documentation deliverable, Tasks 2-7 (Prometheus alerts, dashboards-as-code, synthetic, status-page, Alertmanager) deferred to Phase 2.

### What does NOT change

- All backend application code (Plans 02-14) — provider-agnostic from the start, works against docker-compose locally and against Yandex Cloud in prod with only DSN config changes.
- All frontend code (Plans 15-19) — talks to backend via `api.sociopulse.ru` (or `localhost:8080` in dev). No cloud dependency.
- All tests — testcontainers-go in CI doesn't touch cloud.
- Configuration is already declarative — DSN-strings in YAML, can point at compose containers or managed Yandex services interchangeably.
