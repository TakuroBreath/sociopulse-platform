# Plan 15 — Frontend Foundation — Reference

> Per-plan curated reading list, canonical specs, reference impls, gotchas, open questions, and **wire-format reality** for the Plan 15 implementation in [`sociopulse-web`](https://github.com/TakuroBreath/sociopulse-web).
>
> **Read this BEFORE writing any code for Plan 15.** Subagent prompts MUST cite this file.

---

## 0. Cross-repo context — read first

This plan executes in **`sociopulse-web`** (separate repo at `/Users/user/call-center/sociopulse-web`), not in platform. The plan file (`docs/superpowers/plans/2026-05-06-15-frontend-foundation.md`) was authored before the FE/BE split — it assumes `web/` as a subdir inside platform. Adapt accordingly.

### Path adaptation table

| Plan says | Reality (separate repo) |
|---|---|
| `web/package.json` | `package.json` (at web repo root) |
| `web/src/...` | `src/...` (at web repo root) |
| `web/tsconfig.json` | `tsconfig.json` |
| `web/vite.config.ts` | `vite.config.ts` |
| `web/index.html` | `index.html` |
| `web/dist/` (build output) | `dist/` (at web repo root) |
| `web/public/surveys-runtime.wasm` | `public/surveys-runtime.wasm` |
| `cd web && npm ...` | `npm ...` (we're already at root) |
| `git add web/...` | `git add ...` |
| `/Users/user/call-center/social-pulse/social-pulse-maket/project/<x>.jsx` | Same path (canonical sibling location per web README); equivalent in-web copy at `social-pulse-maket/project/<x>.jsx` is the working duplicate (greenfield decision below: gitignored). |

### Prototype directory decision (Plan 15 Task 1)

**Decision: gitignore `social-pulse-maket/` in web repo.**

Rationale:
- README already references the canonical sibling at `../social-pulse/social-pulse-maket/` — single source of truth.
- The in-repo copy is a local convenience duplicate; committing it would bloat the repo and risk confusion ("which is the canon?").
- Adding to `.gitignore` (vs leaving untracked) signals intent — no accidental commit later.
- Subagent prompts use the in-repo path `social-pulse-maket/project/<x>.jsx` because it resolves locally; if the dir disappears, fall back to `/Users/user/call-center/social-pulse/social-pulse-maket/project/<x>.jsx`.

### Task 12 split (CRITICAL — embed.FS bridge)

Plan 15 Task 12 ("Build integration with cmd/api (embed.FS)") spans both repos:

- **Web-side (this plan):** ensure `npm run build` produces `dist/` with stable layout. Add `.gitignore` for `dist/`. Document the artefact contract.
- **Platform-side (NOT this plan):** `cmd/api/embed.go`, `Makefile` web-build target, multi-stage `Dockerfile`. **DO NOT commit to platform from this skill.**

Instead, **file a GitHub issue in `TakuroBreath/sociopulse-platform`** with `ready-for-agent` label so the backend pipeline picks it up. Issue body: link this reference, link the plan, list exact file changes needed (embed.go + Makefile diff + Dockerfile diff per plan §Task 12).

---

## 1. Canonical specs (read these once)

| Spec section | Topic | Location |
|---|---|---|
| §FR-L | Theme + font-size persistence | platform `docs/superpowers/specs/2026-05-06-sociopulse-system-design.md` |
| §NFR-8 | Browser support (Chrome ≥ 110, Firefox ≥ 110, Safari ≥ 16) | same |
| §NFR-9 | i18n — Russian-only at v1, harness must support more | same |
| §NFR-12 | Idempotency-Key header on all mutating requests | same |
| §10.1 | WebSocket protocol (auth/subscribe/event/ping/refresh frames) | same |
| §11.5 | WASM survey runtime contract | same |
| §13 | Operator workflow narrative (FSM context) | same |
| §14 | Admin workflow narrative | same |

| ADR | Topic | Location |
|---|---|---|
| **ADR-0001** | mod_verto WebRTC (Operator audio path — FE will integrate verto.js in Plan 16+) | platform `docs/adr/0001-mod-verto-sip-wss.md` |
| **ADR-0008** | WASM survey runtime (B-fallback to TS port if TinyGo can't compile) | platform `docs/adr/0008-survey-runtime-wasm.md` |
| **ADR-0009** | Hand-rolled CSS port (no Tailwind / no CSS-in-JS) | platform `docs/adr/0009-handwritten-css.md` |

---

## 2. Wire-format reality (Bruno + dto.go are the canon)

**Verify against `docs/api/collections/sociopulse/<module>/*.bru` + `internal/<module>/transport/http/dto.go`. The plan's prose often diverges.**

### 2.1 Auth — `internal/auth/transport/http/dto.go` + `docs/api/collections/sociopulse/auth/*.bru`

#### POST /api/auth/login

**Request:**
```json
{ "org_id": "string", "login": "string", "password": "string" }
```

**Response (success, no TOTP):**
```json
{
  "access_token": "string",
  "refresh_token": "string",
  "access_expires_at": "RFC3339 timestamp",
  "refresh_expires_at": "RFC3339 timestamp",
  "user": {
    "id": "uuid string",
    "tenant_id": "uuid string",
    "login": "string",
    "full_name": "string",
    "email": "string (omitempty)",
    "roles": ["operator" | "supervisor" | "admin"],
    "totp_enabled": false,
    "must_change_pwd": false,
    "created_at": "RFC3339",
    "updated_at": "RFC3339",
    "archived_at": "RFC3339 (nullable, omitempty)"
  }
}
```

**Response (TOTP required):**
```json
{
  "access_token": "<partial_token (5-min)>",
  "totp_required": true,
  "user": { ... }
}
```
- `refresh_token` is empty.
- The `access_token` field carries the partial token; client must pass it as `partial_token` in `/login/totp`.

> **Plan 15 drift:** the plan says `claims: AuthClaims` with camelCase fields (`userId`, `tenantId`, `fullName`). **Wrong.** Real shape is `user: UserDTO` with snake_case (`id`, `tenant_id`, `full_name`). Rename `AuthClaims` → `User` in the auth store. **Verified by:** `internal/auth/transport/http/dto.go:78-113`, `docs/api/collections/sociopulse/auth/01_login.bru:30-58`.

> **Plan 15 drift:** TOTP request field is `partial_token`, not `temp_session_id`. **Verified by:** `dto.go:29-32`.

#### POST /api/auth/login/totp

**Request:**
```json
{ "partial_token": "<from initial login>", "code": "<6-digit>" }
```

**Response:** same as login-success.

#### POST /api/auth/refresh

**Request:**
```json
{ "refresh_token": "string" }
```

**Response:**
```json
{
  "access_token": "string",
  "refresh_token": "string",
  "access_expires_at": "RFC3339",
  "refresh_expires_at": "RFC3339"
}
```

> **Plan 15 drift:** Refresh response has **no `claims`/`user`** — the FE keeps the existing user object in store, only rotates tokens + expiry. **Verified by:** `dto.go:88-93`, `02_refresh.bru:30-44`.

#### POST /api/auth/logout

**Request:** `{ "refresh_token": "string" }`
**Response:** 204 No Content.

#### Error envelope (auth module)

```json
{ "error": "machine_code_snake", "message": "human-readable" }
```

Both fields populated. The plan's APIError stores `body?: unknown` — fine, but display should prefer `body.message ?? body.error`. **Verified by:** `dto.go:157-160`.

### 2.2 WebSocket — `docs/api/realtime/` (when seeded)

Plan 15 Task 4 implements a generic hub with frames: `auth | auth.ok | auth.error | refresh | refresh.ok | subscribe | subscribe.ok | subscribe.error | unsubscribe | event | ping | pong | force.event`.

**Status:** The platform's `internal/realtime/transport/ws/*` is the source of truth. The hub's protocol surface in Plan 15 is forward-compatible — actual subject names + payload shapes come in Plan 16 (operator FSM) and Plan 17 (admin live). **No verification needed for Plan 15** since we ship only the transport-level frame contract, not domain subjects.

For Plan 16+ readers: cross-check subject names against `docs/api/realtime/*.md` and the WS smoke tests in `cmd/api/smoke_operator_ws_test.go`.

### 2.3 Endpoints NOT used by Plan 15

Plan 15 sets up the scaffolding. Domain endpoints (`/api/projects`, `/api/calls`, etc.) are consumed by Plans 16-19. **Don't pre-create API client modules with assumed shapes** — wait until each plan needs them. The scaffolded `src/api/{projects,surveys,calls,operators,reports,recording}.ts` files exist as stubs only.

---

## 3. Library docs — use `context7` BEFORE writing code

The plan pins versions; library APIs drift between minors.

| Library | Pinned version | Why context7 it |
|---|---|---|
| react | ^18.3.1 | React 19 dropped some APIs; ensure you use 18.3 syntax. |
| @tanstack/react-query | ^5.30 | v5 was a rewrite (`useQueries` shape, suspense API). v4 examples won't compile. |
| zustand | ^4.5 | `persist` middleware API; `create()()` curried form. |
| @radix-ui/react-* | latest stable | Each primitive has its own version; portal z-index semantics are version-specific. |
| react-router-dom | ^6.23 | v6 router API; `useRoutes` / `lazy` patterns. |
| msw | ^2.3 | v2 dropped `rest.*` for `http.*` — old guides reference removed API. |
| @playwright/test | ^1.45 | Auto-waiting locator API (`toBeVisible`); avoid `waitForSelector`. |
| vitest | ^1.6 | `vi.fn()` / `vi.spyOn()` API; setup/teardown vs Jest. |
| @testing-library/react | ^16 | v16 requires React 18.3+; queries unchanged from v14. |
| @testing-library/user-event | ^14.5 | `userEvent.setup()` pattern; `await` everything. |
| i18next / react-i18next | ^23 / ^14 | `useTranslation` hook; namespacing. |
| date-fns | ^3.6 | v3 is ESM-only; locale imports are tree-shakable. |
| @typescript-eslint | ^7.10 | Flat-config v9 wiring is different from v8 .eslintrc. |
| eslint | ^9.3 | Flat-config required; legacy config still works with `--config`. |

> **Rule:** if a subagent is about to write code against any library above, **invoke `context7` first** (`resolve-library-id` → `query-docs`) to verify current API. Don't guess from training data — it's stale.

---

## 4. Reference implementations (URLs)

- [TanStack Query — testing with MSW](https://tanstack.com/query/v5/docs/framework/react/guides/testing)
- [Radix Dialog — controlled state + portal](https://www.radix-ui.com/primitives/docs/components/dialog)
- [React Router v6 — protected routes pattern](https://reactrouter.com/en/main/start/concepts#index-routes-and-layout-routes)
- [zustand — persist middleware](https://github.com/pmndrs/zustand/blob/main/docs/integrations/persisting-store-data.md)
- [Vite — alias resolution + manualChunks](https://vitejs.dev/config/build-options.html#build-rollupoptions)
- [MSW — Node setup for Vitest](https://mswjs.io/docs/integrations/node)
- [Playwright config — webServer auto-start](https://playwright.dev/docs/test-webserver)
- [React 18 — StrictMode double-invoke effects](https://react.dev/reference/react/StrictMode)
- [react-i18next — i18next.use(initReactI18next).init()](https://react.i18next.com/getting-started)
- [Idempotency-Key — IETF Internet-Draft](https://www.ietf.org/archive/id/draft-ietf-httpapi-idempotency-key-header-06.html) (the platform implements server-side caching of responses; FE just sends the header).

---

## 5. Gotchas (known traps)

### 5.1 Radix portal z-index inheritance
`<Dialog.Portal>` does NOT inherit stacking context from the trigger — it portals to `document.body`. Stack order:
- `--z-tooltip: 50`
- `--z-toast: 55`
- `--z-modal: 60` (Dialog portal)
- `--z-popover: 65`

Set explicitly via CSS class on the portal root; don't rely on DOM order. Document in `tokens.css`.

### 5.2 MSW server lifecycle in Vitest
- Start server in `beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))`.
- Reset handlers in `afterEach(() => server.resetHandlers())`.
- Close in `afterAll(() => server.close())`.
- Use `onUnhandledRequest: 'error'` to catch missing mocks early.
- For Node 20+, MSW v2 works without polyfills; older guides may say otherwise.

### 5.3 React Query + zustand state interaction
- React Query caches server state; zustand caches client state. **Don't duplicate.**
- Auth tokens → zustand (rehydrated from localStorage on app boot).
- Server response data → React Query cache (invalidate on mutation).
- DO subscribe to zustand from inside queries via the store's `getState()` (sync read) — don't pass tokens as React Query args (causes re-fetch on token rotation).

### 5.4 TypeScript `exactOptionalPropertyTypes: true`
Plan 15's tsconfig enables this. Common breakage: passing `undefined` explicitly to optional fields.
- `{ foo: undefined }` → invalid for `{ foo?: string }`.
- Use `{}` (omit the key) or `{ foo: "" as const }` (if "" is meaningful).
- Also: object spreads `{ ...obj }` may leak `undefined` keys. Filter explicitly.

### 5.5 zustand `persist` rehydration timing
- Rehydration is async (reads `localStorage` on first store access).
- Don't read store state in module-init code — wrap in `useEffect` or use `onRehydrateStorage`.
- For theme persistence, also bootstrap `<html data-theme>` in `index.html` BEFORE React mounts (via inline `<script>`) to avoid FOUC.

### 5.6 Vite proxy + WebSocket
- `server.proxy['/api']` proxies HTTP only by default.
- For WS: explicit `{ target: 'ws://...', ws: true }`.
- Plan 15 Task 1 has this config; verify it works against a running `cmd/api` (port 8080) before declaring DONE.

### 5.7 React-i18next + Vite HMR
- Importing JSON locales triggers full reloads on save (Vite re-runs i18n init).
- Acceptable for dev; in production the locale is bundle-inlined.
- Don't lazy-load locales until plan 19 (we ship ru-only).

### 5.8 `useEffect` cleanup + WebSocket subscriptions
- Plan 15 Task 4's `useWSSubscription` hook returns a cleanup that calls `unsub()`. **Critical:** the cleanup must fire BEFORE the effect re-runs (React 18 StrictMode double-invokes effects in dev — verify cleanup is symmetric).
- The hub's `subscribe` returns an unsub function; the hook must capture it correctly across re-renders (use `useRef` for the callback to avoid re-subscribing on every render).

### 5.9 Embed.FS path constraints (cmd/api side)
- Go's `//go:embed` cannot traverse `..` — embed must be a sibling/descendant of the embed.go file.
- Plan 15 Task 12 copies `web/dist/` → `cmd/api/webdist/` for this reason. Verify the copy at platform-side (NOT this skill).

### 5.10 Vite dev server proxy headers
- `Authorization` header NOT forwarded by default in some Vite versions. If you see 401s in dev despite a valid token in localStorage, check:
  - `Network` tab — is `Authorization` in the actual request?
  - If not, add explicit `configure` callback in `vite.config.ts` proxy.
- Production (cmd/api serves both API + SPA from same origin) doesn't have this issue.

### 5.11 `ESLint flat config + TypeScript project: true`
- Flat config doesn't auto-detect tsconfig like legacy `.eslintrc.cjs` did. Explicit `parserOptions.project: "./tsconfig.json"`.
- For monorepos: array of tsconfigs. For us (single config): single path.
- If you see "Cannot find tsconfig" — the path is relative to where `eslint` runs, not the config file. Make it absolute or run from repo root.

### 5.12 `npm create vite@5.2 . -- --template react-ts -y`
- The `.` argument means "current dir"; `-y` auto-confirms.
- It WILL refuse if dir has unrelated files. Workaround: `npm create vite@5.2 web-temp -- --template react-ts -y && mv web-temp/{*,.*} . && rmdir web-temp`.
- Or: scaffold the files by hand (we know what they look like — see Plan 15 Task 1 Step 3).

### 5.13 Standing FE rules (web-repo-wide — apply to every task)

1. **TS strict mode** — no `any`. Use `unknown` + narrowing.
2. **No raw `fetch`** — always through `src/api/client.ts` (handles auth refresh + idempotency).
3. **No raw `WebSocket`** — always through `src/api/ws.ts` (hub handles reconnect + auth refresh).
4. **Hand-rolled CSS only** — port from prototype's `styles.css` per ADR-0009.
5. **Radix primitives for headless components** — no MUI/Chakra/HeadlessUI.
6. **TanStack Query for server state, zustand for ephemeral client state.**
7. **Idempotency-Key on mutating requests** — auto-injected by client wrapper.
8. **WS subscribe is route-scoped** — `useEffect` mounts, cleanup unmounts.
9. **i18n via react-i18next** — all user-facing strings through `t('...')`. RU-only at v1.
10. **Theme stored in localStorage** — bootstrap via inline `<script>` in `index.html` to avoid FOUC.

---

## 6. Open questions (resolved during execution → move to "Production lessons")

- [ ] Does `npm create vite@5.2` succeed in a non-empty repo with `social-pulse-maket/` already present? (Tests Task 1 Step 2.)
- [ ] Does `import.meta.env.VITE_*` resolve correctly in `src/api/client.ts` when the build is embedded into cmd/api?
- [ ] Does the FOUC-prevention `<script>` in `index.html` work with Vite's dev-mode HMR?
- [ ] Confirm the `auth.ok` frame from WS shape — does the platform's realtime hub send it before any subscribe-ack, or batched?
- [ ] Verify `vitest` + MSW works with Node 20 LTS (some older guides mention polyfill issues).

---

## 7. Production lessons (post-execution 2026-05-17)

Captured during execution. Plan 16+ readers — these are the rakes we already stepped on.

### Tooling / dependency surface

- **ESLint v9 flat-config requires `typescript-eslint` v8 umbrella**, NOT the plan's pinned v7 split packages (`@typescript-eslint/eslint-plugin` + `@typescript-eslint/parser`). v7 capped at eslint v8. Modern path: `import tseslint from "typescript-eslint"; export default tseslint.config(...)` (see typescript-eslint.io). The `tseslint.config(...)` signature shows as deprecated in TS diagnostics — switch to `defineConfig` from `eslint/config` in Plan 16+ if convenient.
- **`eslint-plugin-react-hooks` must be v5** for eslint v9 (plan pinned v4 which caps at v8).
- **`@eslint/js` must be pinned to v9** (`^9.3.0`). v10 requires eslint v10.
- **`@types/node` must be added** explicitly — `vite.config.ts` uses `node:path` + `__dirname`. The plan's package.json omitted it.
- **`tsc -b` (composite project mode) emits `*.tsbuildinfo`, `vite.config.js`, `vite.config.d.ts`** as build artefacts. Gitignore them. `noEmit: true` doesn't apply to composite — `composite: true` requires emit.
- **`uuid` package is in `package.json` but unused** — `crypto.randomUUID()` is the built-in. Consider dropping in Plan 16 to shave dependencies (the package is still in `package-lock.json`).

### Wire format

- **`/api/auth/login` response is `{ user: UserDTO, ... }` (snake_case interior)**, NOT `{ claims: AuthClaims }` (camelCase). The plan's prose was wrong; verified against `internal/auth/transport/http/dto.go:78-113` and `docs/api/collections/sociopulse/auth/01_login.bru`. The web auth store mirrors snake_case — do NOT introduce a camelCase alias layer.
- **TOTP partial flow uses `partial_token`** (request field name in `/api/auth/login/totp`), NOT `temp_session_id`. The partial token rides on the `access_token` field of the login response when `totp_required: true`. Plan 16 reads this for the 2FA screen.
- **`/api/auth/refresh` response has NO `user`/`claims`** — only `access_token`, `refresh_token`, `access_expires_at`, `refresh_expires_at`. The store keeps the existing user object across refreshes; the `rotateTokens(...)` action takes ONLY the 4 token fields.
- **Error envelope is `{ error: string, message: string }`** with both fields populated. APIError stores `body?: unknown`; display via `body.message ?? body.error ?? 'HTTP {status}'`.

### Testing / harness

- **jsdom's `localStorage` is read-only when Node is launched with `--localstorage-file` without a path** — zustand `persist` then throws `setItem is not a function`. Resolution: install a `Map`-backed `Storage` shim in `src/test/setup.ts` (conditional on the live `localStorage` lacking a working `setItem`). Production unaffected.
- **`globalThis.WebSocket` is a read-only property on jsdom** — direct assignment throws. Use `vi.stubGlobal("WebSocket", MockSocket)` + `vi.unstubAllGlobals()`.
- **Vitest 1.6's `vi.fn<T>()` generic expects `[args, return]` tuples**, not a function signature. Plain `vi.fn()` (untyped) + `expect.toHaveBeenCalledWith(...)` is the path of least friction.
- **MSW v2 handlers** use plain path strings (`/api/...`), host-agnostic. Don't write `http.post("http://localhost/api/...")` — it won't match the relative `fetch("/api/...")`. Use `onUnhandledRequest: 'error'` to catch missing mocks early.
- **Module-singleton state leaks across tests** (e.g. `useWS`'s hub singleton). Reset with `vi.resetModules()` in `beforeEach` for tests that exercise it.
- **`react-router-dom` v6 types `location.state` as `unknown`** — narrow with a type guard, don't blind-cast.
- **`@testing-library`'s `getByRole("button", { name })`** uses ARIA priority: `aria-label` → `aria-labelledby` → text content → `title`. A button with visible label + separate `title` is NOT findable by the title string; add `aria-label` for an accessible name AND for testability.
- **`@testing-library`'s `getByLabelText(/Пароль/)`** matches both the input label and the show/hide button's `aria-label="Показать пароль"`. Anchor with `^...$` or use the full literal.
- **In-flight test holding MSW open**: a `vi.useFakeTimers()` approach fights with MSW's internal `setTimeout`. Inject a `sleep` override in production code OR keep the handler open with a `Promise` released after assertions.

### React 18 + TS

- **`JSX` namespace import** under `verbatimModuleSyntax`: `import type { CSSProperties, JSX } from "react"`. The global `JSX` namespace from React 17 is no longer in scope with the React 18 typings + this flag.
- **`exactOptionalPropertyTypes: true` forbids passing `undefined` to optional fields explicitly.** Conditional spread: `{ ...(condition ? { propName } : {}) }`. Affects component props (`style`, `right`-slot patterns), object spreads, and React Query option blocks.
- **zustand's `UseBoundStore` overload set makes `ReturnType<typeof useStore>` collapse to `unknown`** under TS overload-resolution. Idiomatic fix: subscribe with selectors (`useStore((s) => s.field)`) rather than destructuring the whole state.
- **React StrictMode double-invokes effects in dev.** WS hub `useEffect` cleanup must be symmetric — verified by `useWS.test.tsx` (subscribe-on-mount + unsubscribe-on-unmount + no-token short-circuit).

### Build / CSS

- **CSS bundle 17.51 kB (4.12 kB gzip)** — matches ADR-0009's "~22KB" estimate. Hand-rolled CSS port pays off in payload size vs Tailwind (~30-50KB) and CSS-in-JS (runtime cost).
- **Vite manualChunks split**: `react` (134KB) + `query` (33KB) + `radix` (placeholder, expands as Radix components land in Plan 16+) + `index` (100KB app code + i18next) + lazy `Login.tsx` (8.6KB) + lazy `NotFound.tsx` (0.3KB). Good for Plan 16's TTI budget.
- **FOUC-prevention** via inline `<script>` in `index.html` reading `localStorage.getItem("sociopulse.theme")` and setting `data-theme` + `data-fs` BEFORE React mounts. Tested by toggling theme + hard-refreshing dev server.

### Architecture decisions made (no ADR needed — internal conventions)

- **Wire format: snake_case all the way to the store.** Avoids a mapping layer with no user; the `User` type is the wire shape verbatim. Plan 16+ readers: do NOT introduce camelCase shadowing.
- **Stores: zustand `persist` + version field**. Storage keys:`sociopulse.auth` (v1), `sociopulse.theme` (v1). Bump version + write migration if shape changes; coordinate with `index.html`'s FOUC script if `sociopulse.theme` key/shape changes.
- **Sidebar takes `user` as prop** (not pulled from store internally) — testable in isolation; AppShell is the integration seam.
- **Routing layout: `RequireAuth → RequireRole → AppShell`** as nested elements. Children inherit guards; siblings of the auth tree (`/login`) skip them.

### Open questions resolved during execution

- ✅ **Does `npm create vite@5.2` succeed in a non-empty repo?** No (refuses on non-empty dir). Scaffolded the files manually instead — Plan 15 Task 1 prescription needs amending for separate-repo case.
- ✅ **Does `import.meta.env.VITE_*` resolve correctly?** Not exercised in Plan 15 (no env vars consumed yet). Plan 16 will test under platform's embed.FS — `import.meta.env.MODE === "production"` should be the canonical check for "running under cmd/api" vs "vite dev".
- ✅ **Does the FOUC-prevention `<script>` work with Vite HMR?** Yes — verified via dev-server reload.
- ✅ **Confirm `auth.ok` frame timing** — Plan 15 doesn't exercise live WS; the hub's contract is `auth → auth.ok before any subscribe`. Plan 16's first WS test will confirm against `internal/realtime/transport/ws/*`.
- ✅ **MSW v2 + Node 20 (and Node 25)** — works without polyfills.

### Deferred to Plan 16 (per final review)

- **MINOR**: `api/client.ts:100` — the refresh-token POST bypasses `fetchWithRetry`, so a transient 503 during refresh kicks the user out. Route refresh through `fetchWithRetry` (with no auth header). 1-line change.
- **MINOR**: `useWS.ts` — `hub.connect(accessToken)` on each token rotation could race with the previous connect's auth handshake in StrictMode dev. The `cancelled` flag guards against double-callback but a stale subscribe could leak. Plan 16 will exercise this under real token rotation; add a smoke test then.
- **MINOR**: `api/ws.ts` — inline open/message/close listeners aren't removed when `connect()` is called again. Old socket gets GC'd so it's not a leak, but listeners retain `resolve`/`reject` until GC. Cosmetic.
- **NIT**: Sidebar header shows raw `user.tenant_id` UUID instead of an org name. Org-name endpoint lands later; placeholder UX acceptable.

---

## 8. References to other plans

- **Plan 05 (auth)** — owner of `/api/auth/*` endpoints. Verify any change here against `internal/auth/transport/http/dto.go`.
- **Plan 11 (realtime)** — owner of `/ws` endpoint. Subject naming + payload shapes come from `internal/realtime/`.
- **Plan 07 (surveys WASM)** — Plan 15 ships the WASM loader; the .wasm artefact is built by `internal/surveys/wasm/build-wasm.sh`. CI copies it into `public/`.
- **Plan 16 (operator)** — first FE plan to consume the scaffold. Uses auth store + WS hub + API client.
- **Plan 17/18/19 (admin + survey-builder + admin part 2)** — likewise.

---

## 9. Verification checklist BEFORE declaring Plan 15 done

- [ ] `npm run typecheck` green
- [ ] `npm run lint` green
- [ ] `npm run test` green (all Vitest tests pass)
- [ ] `npm run build` succeeds; `dist/` contains `index.html` + chunked JS + CSS
- [ ] `npm run dev` boots; navigate to `/login`; submit form against MSW-mocked endpoint → success
- [ ] Theme toggle persists across reload (no FOUC)
- [ ] Cross-repo issue filed in platform for Task 12 (embed.FS bridge)
- [ ] `PROJECT_STATUS.md` bootstrapped in web repo with milestone row + standing rules
- [ ] Plan amendments filled in platform repo's plan file
- [ ] Tag `v0.0.1-frontend-foundation` pushed
