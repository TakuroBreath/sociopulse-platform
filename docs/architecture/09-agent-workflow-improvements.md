# Agent Workflow Improvements — для контроллера и сабагентов

> Дополнение к PROJECT_STATUS.md "Standing rules". Цель: закрыть слепые пятна процесса, проявившиеся на Plan 12.x. Применять начиная с Plan 13.

## Контекст

**Работает:**
- TDD + 2-stage review ловит ~95% дефектов до пуша.
- PROJECT_STATUS.md как ledger даёт continuity между сессиями.
- Compile-time interface checks (`var _ X = (*Y)(nil)`) ловят контрактные ломки на `go build`.
- Tag-per-plan + CI watch — жёсткий чекпоинт.

**Хрустит:**
- **Standing-rule-утверждения в плане протухают** между написанием и исполнением. Plan 12.4 утверждал "tenancy_admin has grants on call_recordings" — было неверно (поймали на Task 1, имплементер добавил миграцию 000011).
- **Кросс-модульные контракты не enforce'ятся.** Locator-key mismatch (recording→auth) ловится только в runtime; breaking-change на `internal/*/api/` ловится только когда потребитель попытается собраться.
- **Plan-файлы стареют.** PROJECT_STATUS обновляется в close-out, но исходный plan остаётся с устаревшими утверждениями. Следующая сессия снова "знает" неверный факт.
- **Standing rules растут монотонно.** ~15 правил, некоторые уже неактуальны.

---

## 7 улучшений

### 1. Verify-before-assert в Context-блоке плана

**Why:** план 12.4 ошибся в утверждении про гранты — поймал спецревьюер на Task 1.

**How:** для каждого assertion формата "X is true / X exists" указать рядом источник доказательства:
```markdown
- `tenancy_admin` has SELECT/UPDATE on `call_recordings`.
  Verified by: `grep tenancy_admin migrations/000001_init.up.sql migrations/000010*.sql`
- `RecordingService.VerifyChecksum` returns ciphertext sha256 (no decrypt).
  Verified by: `internal/recording/service/verify.go:42-58`
```

**Scope:** правило применяется к утверждениям про **state другого модуля / БД / внешней системы / миграции**. Не нужно verify-tag'ом обвешивать тривиальные in-package факты ("Go module path is X", "package P exports T"). Иначе план разбухнет до нечитаемости.

**Signal:** в любом plan-файле каждое нетривиальное cross-boundary assertion имеет inline-доказательство либо помечено `Assumed (not verified)`.

### 2. Plan amendments в исходном plan-файле

**Why:** PROJECT_STATUS обновляется в close-out, но plan-файл остаётся с устаревшим текстом. Через 6 месяцев агент откроет plan и снова "узнает" неверный факт.

**How:** в close-out каждого плана добавить блок в **исходный plan-файл**:
```markdown
## Amendments (post-execution)

- 2026-05-09 — Task 1 added migration 000011_admin_grants_call_recordings (originally
  "No migrations"). Standing rule был WRONG; verified missing в 000001_init.up.sql.
```

**Signal:** любой plan начинается либо с `## Amendments: none` либо содержит точный список отклонений.

### 3. Module dependency graph (split: auto + hand)

**Why:** прямой ответ на "не ломается ли связь модулей". Сейчас нет одного места с картой "кто кого импортирует + кто какие events публикует".

**How:** `docs/architecture/module-graph.md` с двумя диаграммами разной природы:

1. **Import graph — auto-generated.** Скрипт `make module-graph-imports` запускает `go list -deps -json ./internal/... ./pkg/... ./cmd/...`, фильтрует только cross-package рёбра внутри `github.com/sociopulse/platform/{internal,pkg,cmd}`, рендерит mermaid через `gomod-graph` или собственный 30-строчный helper. Hand-maintained вариант на 50+ модулей drift'нёт за 3 плана; авто-вариант **filtered** даёт ~30 рёбер и читается. Не запускается в CI — это диагностика для агентов, не gate.

2. **Event graph — hand-maintained.** NATS subjects (`tenant.<t>.X.Y`) и их producers/consumers. Их мало (≤25 на v1), drift минимальный, ценность очень высокая: например, Plan 11.4 ждёт `tenant.<t>.recording.call.deleted` от Plan 12.4 retention worker — без графа эта зависимость не видна нигде. Обновляется руками в close-out плана, который добавил/удалил event.

**Signal:** для произвольного PR за 30 секунд ответ "какие модули затронуты + какие events двигаются".

### 4. `apidiff` в CI на `internal/*/api/` и `pkg/`

**Why:** Plan 12.1 поменял `DeleteAt` с `time.Time` на `*time.Time`; потребители ловили только при `go build`. С `apidiff` подсвечивается в PR.

**How:** job `api-diff` в `.github/workflows/ci.yml` запускает `golang.org/x/exp/cmd/apidiff` против `origin/main` для каждого пакета в `internal/*/api/` и `pkg/`. Pre-1.0 эти пакеты намеренно эволюционируют, поэтому стратегия — **PR-title-gating, не hard-fail**:

- Дифф постится комментарием на PR (всегда — для visibility).
- Job fail'ится **только** если `Incompatible changes:` непустой И PR title не содержит префикса `breaking:` И в PR description нет блока с дифффом.
- Это превращает ломку из "случайной" (поймали на `go build` потребителя) в "осознанную" (автор явно пометил `breaking:` и описал миграционный путь).

**Signal:** breaking-PR явно помечен в title; CI краснеет если автор сломал API не отметив.

### 5. Smoke-тесты в `tests/smoke/`

**Why:** unit + per-module integration не покрывают "boot полного `cmd/api` против реального стэка". Locator-mismatch между recording и auth не покрыт.

**How:** `tests/smoke/recording_playback_test.go` — testcontainers Postgres+Redis+NATS, boot `cmd/api`, выпустить настоящий JWT, кинуть `GET /api/recordings/search`, assert 200.

**Trigger:** не только tag push (это поздно — regression уже в `main`):
- **Каждый push в `main` после CI green** — отдельный `smoke` job, запускается параллельно с deploy. Если падает, PR уже мерж-нут, но channel-уведомление и rollback быстрый.
- **Scheduled cron 1×/час** — ловим regressions от внешних изменений (image rebuilds, Yandex SDK pinning).
- **Tag push `v*`** — обязательный gate перед production rollout.

**Signal:** новый HTTP/gRPC endpoint в плане → автор обязан добавить smoke-кейс. Smoke-suite растёт линейно с public surface, не с числом тестов в кодовой базе.

### 6. Standing-rules pruning раз в 5 планов

**Why:** PROJECT_STATUS standing rules растут монотонно. К Plan 20 их 30+ и они перестанут помещаться в working memory.

**How:** каждый 5-й close-out (Plan 12.5, 13.5, ...) ИЛИ когда секция превышает 200 строк — обязательный шаг:
1. Прочитать секцию `## Standing rules / patterns` целиком.
2. Для каждого правила пометить `keep / merge / archive / delete`:
   - **keep** — правило ещё актуально и часто срабатывает.
   - **merge** — близко к другому правилу, объединить.
   - **archive** — правило больше не активно (модуль/тулинг ушли), но контекст ценен. **Переехать в `docs/architecture/agent-rules-archive.md`** с датой архивации + причиной + ссылкой на план который сделал правило неактуальным.
   - **delete** — правило было ошибочным с самого начала (см. Plan 12.4 "tenancy_admin grants на call_recordings" — было неверным утверждением, не правилом).
3. Apply. Цель — держать активные standing rules ≤ 200 строк.

**Why archive vs delete:** иногда "deleted" правило снова становится релевантным. Plan 12.4 заново открывал тему grants, которую Plan 04 уже знал (правило существовало где-то в истории, но потерялось). Архив сохраняет контекст без захламления working memory.

**Signal:** длина активных standing rules не растёт линейно с числом планов; архив монотонно растёт но никто его не читает по умолчанию (только при подозрении "это уже было раньше").

### 7. Re-review proportionality

**Why:** сейчас 2-stage review гоняется полностью даже на trivial nit-fix'ы. Plan 12.4 пропустил через полный цикл коммиты типа `fix(...): action labels past-participle` (1 строка переименована), `fix(...): drop vestigial loop-var capture` (3 строки), `fix(...): require.Positive over require.Greater(...,int64(0))` (1 строка). Каждый такой раунд — отдельный subagent dispatch + двойной review, время и стоимость без яркого выхлопа.

**How:** controller применяет эвристику **диff size + scope match**:

| Diff scope | Action |
|---|---|
| ≤ 5 lines И точно matches reviewer'ский tickbox | Controller fix'ит inline + commit'ит, **skip re-review**. |
| 6–30 lines ИЛИ затрагивает behavior/error path | Single re-review (spec only — code-quality уже approved'ил исходный дифф). |
| > 30 lines ИЛИ новые публичные symbols ИЛИ новые тесты | Full 2-stage re-review. |

**Edge case:** если controller'ский inline-fix ломает тест — это сигнал что fix НЕ был "точно matches tickbox", полный re-review обязателен.

**Signal:** мелкие nit'ы съедают единицы минут walltime, не десятки. Review-cost масштабируется с **scope изменения**, не с числом review-итераций.

---

## Что НЕ надо делать

- Не добавлять unit-тесты ради coverage-цифры. Каждый тест ловит конкретный класс багов.
- Не дробить работающие модули ради "чистоты".
- **Не генерировать unfiltered import graph** — на 50+ модулей будет нечитаемо. Auto-gen работает только с filtering на cross-package рёбра внутри проекта (см. #3).
- Не дублировать в CLAUDE.md правила из PROJECT_STATUS — single source of truth.
- Не делать full re-review на 1-line nit'ы (см. #7) — review-cost должен масштабироваться со scope изменения.
- Не использовать `delete` вместо `archive` в pruning — потерянный контекст возвращается через год.