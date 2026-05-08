# Plan 07 — Surveys Module references

> **Plan source**: [`docs/superpowers/plans/2026-05-06-07-surveys-module.md`](../superpowers/plans/2026-05-06-07-surveys-module.md).
> **Module path**: `internal/surveys/`.
> **Depends on**: Plan 03 (`surveys`/`survey_versions` migrations), Plan 05 (audit), Plan 06 (project linkage).

Status: **shipped (`v0.0.9-surveys`, 2026-05-08)**.

## Plan source caveat

The plan source under `docs/superpowers/plans/` only fleshes out **Task 1 (skeleton + JSON-Schema)** and **Task 2 (schema validator)**. Self-review claims full coverage including DSL evaluator, service CRUD, store, runtime, HTTP transport, and WASM build. **Tasks 3-8 must be derived from the file structure and self-review section** — they aren't enumerated as `## Task N:` headings.

Pragmatic split for our pipeline:
1. **Task 1**: skeleton + JSON-Schema embed.
2. **Task 2**: schema validator (JSON-Schema + graph; stub DSL).
3. **Task 3**: DSL evaluator real impl (replaces stub).
4. **Task 4**: SurveyService CRUD + Postgres store.
5. **Task 5**: Runtime (NextNode/ValidateAnswer/CalculateProgress).
6. **Task 6**: HTTP transport + activation flow.
7. **Deferred**: WASM build pipeline (frontend not yet built; Plan 16 owns).

---

## Canonical specs (must-read)

### JSON-Schema validation
- [**JSON Schema 2020-12**](https://json-schema.org/draft/2020-12) — current dialect.
- [**`santhosh-tekuri/jsonschema/v5`**](https://github.com/santhosh-tekuri/jsonschema) — Go implementation; supports 2020-12, 2019-09, draft-07. The plan pins this lib.
  Note:
  - Compile schema once at startup via `jsonschema.NewCompiler()` + `Compile`.
  - Use `Validate(jsonAny)` against parsed JSON (not raw bytes).
  - Errors are structured (`*jsonschema.ValidationError`) — surface them to the UI as field-pathed validation issues.
  - **Don't roll your own** — JSON-Schema's edge cases (anyOf/oneOf/if-then-else, type unions, regex pattern, format) are battle-tested in this lib.

### DSL evaluator (conditional logic)
- [**`expr-lang/expr`**](https://github.com/expr-lang/expr) v1.16+ — sandboxed Go expression language; safe-by-default (no I/O, no system calls).
  Note:
  - **MUST use whitelist environment** — restrict identifiers to `answer`, `q<id>.value`, `q<id>.answered`. Don't expose `os`, `time`, file I/O, or arbitrary functions.
  - **Compile once, eval many** — keep an LRU cache of compiled `expr.Program` keyed by expression string.
  - **No side effects** — every operator/function in our whitelist is pure. `expr.Eval` with our env is referentially transparent.
  - **Forward-reference detection**: parse the AST and walk identifiers; reject `q<id>.value` references where `<id>` is not reachable from the current node BEFORE the predicate evaluates. The validator does this at SaveVersion time (graph pass).

### Runtime / WASM (TinyGo)
- [**TinyGo**](https://tinygo.org/) — small-footprint Go compiler that targets WebAssembly. ADR-0008 commits us to TinyGo for the survey runtime browser bundle.
- [**ADR-0008**](../adr/0008-wasm-survey-runtime.md) — single-source Go runtime → WASM eliminates duplicating logic between server and browser.
- [**Go WASM canonical guide**](https://go.dev/wiki/WebAssembly) — base patterns (`syscall/js`, `wasm_exec.js` glue).
  Note:
  - **TinyGo limitations**: no full reflection, no `encoding/json` reflection-based decoder (use code-generated decoders), no goroutine-heavy stdlib. Our runtime is pure-function so this is OK.
  - **Bundle size**: TinyGo cuts WASM from ~6MB (vanilla Go) to ~200-500KB.
  - **Browser API surface**: `js.Global().Set("nextNode", func)` exposes Go functions to JS. Plan 16 wires `wasm_exec.js` glue.

### Survey schema design
- The JSON-Schema in `schemas/survey-1.0.json` defines the survey graph: nodes (start/intro/question/text-block/success-end/refusal-end/condition/jump), edges (`next` with optional `when` DSL).
- **Versioning**: every SaveVersion → INSERT new row; only one row per `(survey_id, is_active=true)` (partial UNIQUE index `survey_versions_active_one` — already in 000001_init).
- **Pinning**: `calls.survey_version_id` pins the version when a call starts (Plan 09 owns this hand-off).

---

## Reference implementations

- [**SurveyJS engine**](https://github.com/surveyjs/survey-library) — JS-only, but the JSON schema for question/conditions is a useful reference for our `survey-1.0.json` shape.
- [**Typeform conditional logic**](https://help.typeform.com/) — UX inspiration for flow-mode editor (Plan 18).
- Existing scaffolding in repo:
  - `internal/surveys/api/interfaces.go`, `dto.go`, `errors.go`, `events.go` — already defined; implement against.
  - `migrations/000001_init.up.sql` — `surveys`, `survey_versions` (with `is_active boolean` + partial unique index `survey_versions_active_one`).
  - `internal/auth/transport/http/` + `internal/crm/transport/http/` — gin transport pattern reference.
  - `internal/auth/service/` — composition pattern (panic-on-nil deps, audit-logger noop fallback).

---

## Production lessons (blog posts, talks)

- [**expr-lang docs — Security considerations**](https://expr.medv.io/) — explicit guidance on whitelisting envs.
- [**TinyGo blog**](https://tinygo.org/blog/) — periodic posts on WASM-Go interop quirks.
- [**Habr — "WASM на Go: что работает, что нет"**](https://habr.com/ru/articles/) — search; lots of war stories.

### Russian-language
- [**ВЦИОМ методология опросов**](https://wciom.ru/methodology) — вдохновение для valid fixtures (`valid-vciom-electoral.json`). Реальная анкета ВЦИОМ — 5-7 вопросов с условными переходами; используем как smoke-test.

---

## Gotchas (do-not-do list)

1. **DON'T expose unrestricted `expr` env** — every identifier MUST be in the whitelist. A leaked `os.Getenv` or `time.Now` in DSL = arbitrary RCE in the browser via WASM.
2. **DON'T compile `expr.Program` per-eval** — cache by expression string. ВЦИОМ-style surveys have 50+ expressions; recompiling burns CPU.
3. **DON'T trust schema bytes from the wire** — always run JSON-Schema validation FIRST, then graph validation. Skipping JSON-Schema means malformed `next.to` (e.g., null) crashes the graph walker.
4. **DON'T allow forward references** in DSL — `q5.value > 10` referenced from before q5 is reachable = always-undefined → branch chooses the default edge silently. Validator rejects at SaveVersion; runtime should NEVER see one.
5. **DON'T use `encoding/json` reflection in the WASM bundle** — TinyGo's reflection support is partial. Use code-gen (`easyjson` or hand-rolled marshalers) for the runtime DTOs.
6. **DON'T ship `wasm_exec.js` from the wrong Go version** — the JS file must match the compiler that produced the .wasm. Bundle them together via `embed.FS`.
7. **DON'T mutate `schema []byte` in Runtime methods** — schema is shared across goroutines (per-tenant cache). Treat as read-only; if a parse step needs an unmarshalled view, create a deep copy.
8. **DON'T forget RLS** — service-level CRUD writes go through `pool.WithTenant(tenantID, ...)`. The `surveys` table has RLS policies from 000001_init.
9. **DON'T create a new ServeMux for every endpoint** — gin transport pattern (see `internal/crm/transport/http/`) attaches handlers to a router group.
10. **DON'T audit reads** — only state-changing ops (`SaveVersion`, `Activate`, `Update`, `Archive`) emit audit rows. List/Get reads are noisy and audited at the access-log level by middleware.

---

## Open questions (to resolve during implementation)

1. **Schema versioning major bumps**: spec says `minor=true` is a backwards-compatible bump. What's the criterion for "compatible"? Same node IDs? Same question types? Defer to runtime — if old answers (referenced by `q<id>.value` in conditions) are still valid, it's compatible.
2. **DSL whitelist scope**: spec mentions `q<id>.value` and `q<id>.answered`. Do we need numeric helpers like `min`, `max`, `len(arr)` for multi-select? Plan source omits — we add them iff a fixture demands.
3. **Runtime WASM scope**: should `NextNode` accept the schema as `[]byte` (parse on every call) or pre-parsed `Schema struct`? Server-side prefer struct; WASM call-site prefer bytes (less marshal overhead). Decision: API takes `[]byte`, internal parse caches by sha256(schema).
4. **Active-version atomicity**: the partial unique index makes a two-row UPDATE in one tx the only way. Defer plan source's exact SQL until Task 4.
5. **Preview endpoint shape**: `POST /api/surveys/{id}/preview/run` accepts current_node + answers map, returns next-node prediction. Plan source mentions but doesn't shape the request — adopt the runtime's `NodeResult` type.
6. **Audit on Activate**: log {previous_version_id, new_version_id} so the audit row is reversible. Spec doesn't specify; implement.

---

## Workflow note

Subagent dispatching Plan 07 Task N MUST:
1. Read this file before starting.
2. Read [`COMMON.md`](COMMON.md) for cross-cutting concerns.
3. Read [`plan-05-auth.md`](plan-05-auth.md) and [`plan-06-crm.md`](plan-06-crm.md) — lessons learned (panic-on-nil, errorlint, gocognit refactor, gopls cache discipline) directly apply.
4. Read the actual plan task from `docs/superpowers/plans/2026-05-06-07-surveys-module.md`.
5. **Use `context7` MCP** to verify current API of `santhosh-tekuri/jsonschema/v5`, `expr-lang/expr`, `pgx/v5`. Don't guess.
6. **Use `WebSearch`** for unfamiliar errors (especially expr-lang AST walkers, JSON-Schema 2020-12 quirks).
7. Apply Plan 05/06 lessons: panic-on-nil deps, audit-logger noop fallback, eventbus.Publisher slot pattern (no nil dereference), constraint-name discrimination on unique violations, hand-rolled fakes over gomock.
8. TDD per `superpowers:test-driven-development`.

---

## Lessons learned from Plan 07 implementation (2026-05-08)

After 6 sub-tasks and ~10 commits the surveys module is shipped. These are the things subagents repeatedly tripped on — capture so future plans (Plan 08 freeswitch, Plan 16 frontend WASM) avoid the same cycles.

1. **Plan source incomplete**: only Tasks 1-2 were enumerated as `## Task N:`; Tasks 3-6 had to be derived from the File Structure + Self-review sections. The reference file's "Plan source caveat" pre-empted this — recommend doing the same for any plan whose body trails off mid-spec.

2. **JSON-Schema 2020-12 `if/then` matching nodes without the discriminator**: an `if {properties:{kind:{const:"question"}}}` matches `{}` (no kind) by default since schemas without keywords-on-missing-property are vacuously true. ALWAYS pin the `if` clause with `required:["kind"]` so the rule fires only on nodes that actually declare it.

3. **`additionalProperties: false` at every object level** — locks shape, prevents typo'd field names from silently passing schema validation. Easy to relax later if it causes friction.

4. **edge.to needs the node-id pattern constraint** — without it, `" q1"` (with leading space) passes JSON-Schema then surfaces as a "dangling edge" graph error. Add `pattern: "^[a-zA-Z0-9_-]+$"` to edge.to so the typo is caught at the structural pass with a clear message.

5. **expr-lang AST quirk**: `ast.Walk` visits children before parents, so a bare `IdentifierNode` (`q1` in `q1.value`) is reported before its enclosing `MemberNode`. Solution: pre-pass collector that records identifiers belonging to a member chain; main visitor consults the map and skips them, letting `MemberNode` enforce the full `q<id>.<prop>` whitelist.

6. **DSL whitelist must reject by default, allow by name** — opposite of `expr.AllowUndefinedVariables`. Walk every `IdentifierNode` and `MemberNode`; reject anything not in `{answer, q<id>.value, q<id>.answered}`. Without this, `os.Getenv` or `time.Now` in DSL = arbitrary RCE in the browser via WASM.

7. **DSL identifier extraction at the validator level**: regex like `\b([a-zA-Z][a-zA-Z0-9_-]*)\.(value|answered)\b` over-matches `answer.value`, `equality.value` etc. Pin to `q[a-zA-Z0-9_-]*` (project convention: every question node id is q-prefixed). Long-term fix: replace regex with AST walker via `dsl.Evaluator.ParseAndCheck`.

8. **Activate atomicity needs advisory lock**: the partial-unique-index `WHERE is_active` makes a "set both" UPDATE impossible. Pattern: open tx → `pg_advisory_xact_lock(hashtext(survey_id))` → DeactivateAll → Activate → commit. FNV-1a of UUID bytes → int64 is the deterministic key.

9. **`api.Runtime` interface is ctx-less per spec** — for cancellable callers (long-running NextNode batches in tests) add a sister method `NextNodeCtx(ctx, ...)` rather than widening the interface. Same pattern when a future task needs ctx-aware API: the public interface stays minimal; sister methods cover edge cases.

10. **Schema-cache `put()` is read-only on duplicate insert**: callers hold cached `*schemaDoc` pointers, and overwriting `entry.doc` while readers walk nodes is a real race (caught by `-race` during dev). Since the key is sha256-keyed by content, a duplicate-insert is by definition identical — skip the rewrite.

11. **NATS publisher slot pattern (Plan 06 lesson re-confirmed)** — declare `events eventbus.Publisher` as nil-tolerant; SaveVersion / Activate publish typed events when non-nil, no-op when nil. Plan 11 plumbs the real publisher; until then no NPE.

12. **gocognit refactor pattern (Plan 05/06 lesson re-confirmed)** — when `Activate` exceeded gocognit:20, extract `applyActivate` + `captureCurrentActive` helpers. Same trick for `processBatch` in CRM, `ChangePassword` in auth.

13. **Dead sentinel discipline**: `api.ErrAlreadyActive` was declared as "returned by Activate when already active" but Activate is idempotent (returns nil, no error). The sentinel was unreachable from outside; remove it. Same for `api.ErrNameTaken` (surveys.name not unique in schema). Document in Create's comment what to restore if a future migration adds the constraint.

14. **gopls cache lag during long subagent dispatches** — every long-running implementer leaves gopls reporting phantom errors (undefined symbols, GOPROXY=off, "method unused"). Reality: `go build` clean. Always verify `go build && go test -race` before reacting.
