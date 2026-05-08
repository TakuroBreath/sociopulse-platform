# Plan 05 — Auth Module references

> **Plan source**: [`docs/superpowers/plans/2026-05-06-05-auth-module.md`](../superpowers/plans/2026-05-06-05-auth-module.md).
> **Module path**: `internal/auth/`.
> **Depends on**: Plan 04 (TenantService for tenant resolution; PhoneHasher for phone-based identifiers).

Status: **ready, plan not yet started**.

---

## Canonical specs (must-read)

### Password hashing
- [**RFC 9106 — Argon2 Password Hashing**](https://datatracker.ietf.org/doc/rfc9106/) — каноническое описание Argon2id, включая RECOMMENDED parameters.
- [**OWASP Password Storage Cheat Sheet**](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html) — runtime guidance с актуальными minimum-параметрами.
  Note:
  - **Используем Argon2id**, не Argon2i и не Argon2d (баланс устойчивости к GPU и side-channel).
  - **Default params (locked in commit 66e35c3)**: m=19 MiB, t=2, p=1 — это OWASP "Argon2id minimum" (Aug 2024 revision).
  - **Почему не 64 MiB / t=3 / p=4** (что советует RFC): главный риск для нас — DoS в виде memory pressure от concurrent логинов, а не offline-атака против утёкшей БД. 64 MiB × N concurrent = OOM-kill на 256 MiB pod'е до того, как rate-limit сработает. С m=19 MiB и `BoundedHasher` (semaphore cap = NumCPU) worst-case ≈ 76 MiB — предсказуемо и безопасно.
  - **Defense layers**: (1) Argon2id с bounded memory; (2) `BoundedHasher` через `golang.org/x/sync/semaphore`; (3) per-IP rate limit (Plan 05 Task 5, 30/h); (4) lockout after 5 failures (Plan 05 Task 5).
  - Salt: 16 random bytes from `crypto/rand` (NOT math/rand).
  - Output: 32 bytes hex.
  - **Если threat model изменится** (high-value tenant, real attack target) — bump `auth.password.{memory,iterations}` через config, существующие хеши работают (cost params в каждом PHC).

### JWT
- [**RFC 7519 — JSON Web Token**](https://datatracker.ietf.org/doc/rfc7519/) — спецификация JWT.
- [**RFC 7515 — JSON Web Signature**](https://datatracker.ietf.org/doc/rfc7515/) — JWS (HS256, RS256, и т.д.).
- [**OWASP JSON Web Token Cheat Sheet**](https://cheatsheetseries.owasp.org/cheatsheets/JSON_Web_Token_for_Java_Cheat_Sheet.html) — практическое руководство по безопасному использованию.
  Note:
  - **HS256** для нас (single-tenant secret per environment, не нужно key distribution).
  - Claims: `iss` (issuer), `sub` (user_id), `tenant_id`, `roles`, `exp`, `iat`, `jti`.
  - **`exp` обязательно** — без TTL это session token, не JWT.
  - **Проверять `alg` claim** на сервере — иначе уязвимость "alg=none" attack.
  - **НЕ хранить sensitive data в JWT** — payload видим всем. Только идентификаторы.

### TOTP (2FA)
- [**RFC 6238 — TOTP**](https://datatracker.ietf.org/doc/rfc6238/) — алгоритм.
- [**RFC 4226 — HOTP**](https://datatracker.ietf.org/doc/rfc4226/) — базовый HOTP, на котором TOTP построен.
- [**Google Authenticator key URI format**](https://github.com/google/google-authenticator/wiki/Key-Uri-Format) — формат `otpauth://...` URL для QR-кода.
  Note:
  - Period: **30 секунд** (стандарт).
  - Digits: **6** (стандарт; не 8 — будет несовместимо с большинством authenticator apps).
  - Window: **±1** (принимать предыдущий и следующий код для clock skew). НЕ принимать ±2 — это снижает энтропию.
  - Secret: 20 bytes from `crypto/rand`, base32-encoded для display.
  - **Backup codes**: 10 single-use codes, each 8-10 chars, hex or numeric. Хранить только хеши.

### Refresh token rotation + replay detection
- [**OAuth 2.0 — Refresh Token Rotation**](https://www.rfc-editor.org/rfc/rfc6749) (RFC 6749 + RFC 7636 PKCE) — каноника.
- [**OAuth 2.0 Security Best Current Practice**](https://datatracker.ietf.org/doc/draft-ietf-oauth-security-topics/) — IETF draft.
- [**Auth0 — Refresh Token Rotation**](https://auth0.com/docs/secure/tokens/refresh-tokens/refresh-token-rotation) — практическое описание (хорошо объяснено).
  Note:
  - **Каждый refresh выдаёт новую пару (access, refresh)**. Старый refresh инвалидируется.
  - **Если кто-то использовал старый refresh после rotation → revoke entire family** (security incident: refresh-replay detected).
  - В нашем `internal/auth/api.errors.go` есть `ErrRefreshReplay` именно для этого случая.
  - Хранить: `session_id` + `family_id` + `previous_jti` chain.

### RBAC
- [**NIST RBAC Standard**](https://csrc.nist.gov/projects/role-based-access-control) — формальная модель.
- [**OWASP — Access Control**](https://owasp.org/www-community/Access_Control) — практика.
  Note:
  - **Roles** в нашем кейсе: `service_owner`, `tenant_admin`, `supervisor`, `operator`. См. spec §13.
  - **Resource-action pairs**: `recordings:read`, `tenants:write`, etc.
  - **Hierarchical roles** — `tenant_admin` ⊃ `supervisor` ⊃ `operator`. Service-owner — сверху, cross-tenant.

---

## Reference implementations

- [**ory/kratos**](https://github.com/ory/kratos) — open-source identity server на Go. Production-quality, но **много** features. Нам не нужно всё; смотреть как референс на конкретные подзадачи (TOTP enroll flow, recovery codes).
  Files of interest: `selfservice/strategy/totp/`, `session/manager.go`.

- [**dexidp/dex**](https://github.com/dexidp/dex) — OIDC provider на Go. Не наш кейс (мы не OIDC), но JWT validation + key rotation = хороший референс.
  Files of interest: `server/handlers.go`.

- [**casbin/casbin**](https://github.com/casbin/casbin) — RBAC/ABAC engine. Готовая библиотека для policy enforcement.
  Note: для нашего scope (4 роли + ~20 actions) overkill. Hand-rolled `func (c Claims) HasRole(...)` достаточно. Но если кто-то завтра захочет ABAC — casbin готов.

- [**alexedwards/argon2id**](https://github.com/alexedwards/argon2id) — micro-library для Argon2id с разумными defaults.
  Note: или используем `golang.org/x/crypto/argon2` напрямую (он есть в stdlib-extended). Зависит от того, хочется ли тонкая обвязка.

- [**golang-jwt/jwt v5**](https://github.com/golang-jwt/jwt) — JWT library. Активно поддерживается.
  Note: v5 — текущая стабильная. v4 deprecated. Мы пинимся к v5.

- [**pquerna/otp**](https://github.com/pquerna/otp) — TOTP/HOTP на Go. Зрелая, простая.

---

## Production lessons (blog posts, talks)

- [**Auth0 — A Concise Guide to JSON Web Tokens**](https://auth0.com/blog/a-look-at-the-latest-draft-for-jwt-bcp/) — конкретные attacks (alg=none, key confusion, etc.) и как от них защититься.
- [**Stytch blog — Refresh Token Rotation**](https://stytch.com/blog/refresh-token-rotation/) — production-tested подход.
- [**Habr — "Argon2: что это и почему он лучше bcrypt"**](https://habr.com/ru/articles/) — поиск; есть несколько хороших статей с бенчмарками на CPU российских серверов.

### Russian-language (152-ФЗ context)
- [**Habr — "Хранение паролей: best practices"**](https://habr.com/ru/articles/) — поиск; статьи варьируются по качеству.
- 152-ФЗ требования к authentication: сами по себе не описывают, но **Постановление ФСТЭК 21** (упоминается в `COMMON.md`) требует "защита от перебора" — у нас это login rate-limit + lockout.

---

## Lessons learned from Plan 05 implementation (2026-05-08)

After 9 sub-tasks and ~15 commits the auth module is shipped. These are the things subagents repeatedly tripped on — capture them so future plans (Plan 06 CRM, Plan 07 surveys) avoid the same cycles.

1. **Timing-safety is _everywhere_, not just on password verify.** Three rounds of code review caught timing oracles on (a) `ChangePassword` missing-user branch, (b) `Authenticator.Login` per-account rate-limit branch, (c) `Authenticator.Login` archived/locked/missing-tenant branches. Pattern: any path that returns BEFORE `hasher.Verify` runs is a latency oracle. Fix: pre-bake a dummy hash in the constructor and always run Verify against it on the early-exit branches. Document this prominently in any service that takes a password.

2. **OWASP-min Argon2id beats max-security for our threat model.** Default 64 MiB / t=3 / p=4 burns 64 MiB per concurrent login — a flood OOM-kills small pods before the rate-limiter trips. m=19 MiB / t=2 / p=1 + `BoundedHasher` (semaphore = NumCPU) caps worst-case at ~76 MiB. Code path documented in `pkg/passwords/doc.go`.

3. **JWT JTI MUST be unique per token, not per session.** First implementation reused `claims.JTI = jti` for both `IssueAccess` and `IssueRefresh` calls — the issuer happily produced two tokens with the same JTI (a passive RFC 7519 §4.1.7 violation, plus a real collision risk in any future jti-keyed store). Fix: pre-mint two distinct JTIs in the caller. Eliminated a wasted re-Validate round-trip as a side effect.

4. **Down migrations need data-loss guards.** `users.role text` → `roles text[]` was a one-way move: any user with 2+ roles loses everything except `roles[1]` on rollback. Fix: PL/pgSQL `RAISE EXCEPTION` guard in down.sql when multi-role data exists. Future migrations should check for irreversibility BEFORE the actual destructive ALTER.

5. **Redis whitelist + Lua atomic Rotate is the canonical refresh-rotation pattern.** Reads-modify-writes in one EVAL with 3-way return ({not-found, already-rotated, success}) — strictly better than the spec's Lookup-then-Rotate pattern. The 3-way return prevented a race-window where a valid token could be marked rotated for a transient lookup miss.

6. **`gopls` cache is THE single biggest noise source.** After every subagent dispatch, IDE shows phantom errors (undefined symbols, GOPROXY=off, "method unused"). Reality: `go build && go test -race` are clean. CLAUDE.md's "verify directly" rule saves real time — never trust IDE diagnostics fresh off a subagent.

7. **`gocognit` linter trips when a single function has both happy + timing-safe + transactional branches.** Refactor pattern: extract the inner-tx closure into a helper (`applyPasswordChange`). Keeps the public method readable and the linter quiet.

8. **`forcetypeassert` + `errorlint` (project-enforced) ban two patterns the plan source uses casually**: bare `mc["x"].(string)` and `fmt.Errorf("%w: %v", sentinel, err)`. Use comma-ok and `errors.Join` (or `%w: %w` on Go 1.20+) respectively. Sabagents need this in their prompts to avoid bouncing through fix-up loops.

9. **`miniredis` + `t.Cleanup(release)` + idempotent `Release()`**: the gateHasher pattern in `pkg/passwords/bounded_test.go` had to be made idempotent (`sync.Once`) so explicit Release() inside the test and `t.Cleanup(Release)` for safety could coexist without panic. Reusable pattern for any test that gates goroutines.

10. **Composition root needs explicit nil-checks**, not just lazy ones. `NewUserService` originally accepted nil deps; in production a misconfigured root would silently swallow audit rows forever. Fix: panic-on-nil at construction with a clear message. Same fix applied to TOTP and Authenticator.

11. **Audit module is still a stub** (`internal/audit/Module.Register` is a no-op as of v0.0.7). The auth module's composition root falls back to a `noopAuditLogger{}` with `logger.Warn` to surface this loudly. When Plan 03 / a future plan wires real audit, the fallback silently bypasses. **Future plans depending on audit should follow this same fallback pattern.**

12. **Redis client missing from `cmd/api/main.go`**. `internal/modules.Deps.Redis` is plumbed (UniversalClient), but `cmd/api` doesn't yet construct one. `auth.Module.Register` returns a structured error if called without it. The `registry.Modules` map in cmd/api is still empty per the existing Plan 02 comment, so this is a forward-compatible slot — first module to actually register will need cmd/api wiring.

---

## Gotchas (do-not-do list)

1. **НЕ использовать `bcrypt`** — устарел (нет protect-against-side-channel, нет memory-hardness). Argon2id — современный стандарт. (Note Aug 2024: bcrypt всё ещё OK для совсем low-stakes deployments; мы выбрали Argon2id с OWASP-минимум params + BoundedHasher.)
2. **НЕ генерировать JWT secret detrministically** — всегда из `crypto/rand`. И НЕ из ENV var с дефолтом — обязательно из Lockbox в production.
3. **НЕ хранить пароли в plaintext** для recovery — используем "user types new password + old password" flow. Forgot-password — email/sms link, не passive recovery.
4. **НЕ путать `access` и `refresh` token TTL**: access = 15 мин (короткий), refresh = 30 дней (длинный, но revocable). Не наоборот.
5. **НЕ сохранять JWT в localStorage у фронта** — XSS-vulnerable. Сохранять в `httpOnly` cookie или памяти SPA.
6. **НЕ выдавать access-token в URL query params** — попадает в server logs, browser history, referrer headers. Только в `Authorization: Bearer`.
7. **НЕ принимать TOTP с window > ±1**. Window > ±1 снижает энтропию атаки в N раз.
8. **НЕ забыть constant-time compare** для password hashes, TOTP codes. `crypto/subtle.ConstantTimeCompare`.
9. **НЕ logging password / TOTP / JWT** — даже в debug. PII redaction в `pkg/observability` маскирует, но лучше изначально не передавать в zap.Field.
10. **`forcetypeassert` linter** — все type assertions с comma-ok. JWT claims при unmarshal — частая жертва.

---

## Open questions (что узнаем в реализации)

1. **Argon2id parameters**: ~~какие m/t/p оптимальны?~~ ✅ **RESOLVED**: m=19 MiB, t=2, p=1 (OWASP min). Защита от DoS-OOM важнее offline-резистентности для нашей модели угроз. Worst-case резидентка с `BoundedHasher` (NumCPU concurrency) ≈ 76 MiB. См. `pkg/passwords/doc.go` и `params.go`.
2. **Rate-limiting strategy**: ✅ **RESOLVED**: и per-IP (30/h), и per-account (10/h). Sliding window через Redis ZADD/ZCARD pipeline (count-before-add). См. `internal/auth/service/ratelimit.go`. Известный edge case: pipeline-interleaving может пропустить ±1 запрос; для нашего масштаба приемлемо.
3. **Session storage**: ✅ **RESOLVED**: refresh tokens — Redis whitelist (`auth:refresh:<jti>`); revocation — Redis kill keys (`auth:revoke:sid:<sid>`) + per-user cutoff (`auth:revoke:user:<uid>:cutoff`). Postgres не используется для сессий — TTL Redis достаточно.
4. **MFA for service_owner**: остаётся открытым — `service-owner` роль ещё не добавлена в код. Появится в Plan 06+ (cross-tenant ops).
5. **Backup codes UX**: ✅ **RESOLVED**: 10 кодов по 10 hex chars, returned ONCE at Enroll, hashed via cheap Argon2 (`passwords.BackupCodeParams()` — m=1 MiB, t=1, p=1). Single-use enforced через atomic `array_remove` в SQL (race-safe). Re-generate доступно через `Disable` + `Enroll`.
6. **Logout-all-devices**: ✅ **RESOLVED**: `SessionRevoker.RevokeAllForUser` пишет cutoff timestamp; `IsRevokedClaims` инвалидирует любой access token, выпущенный до этого момента (`iat <= cutoff`). 30-дневный TTL cutoff-ключа.

---

## Workflow note

Subagent dispatching Plan 05 Task N MUST:
1. Read this file before starting.
2. Read [`COMMON.md`](COMMON.md) for cross-cutting concerns.
3. Read the actual plan task from `docs/superpowers/plans/2026-05-06-05-auth-module.md`.
4. **Use `context7` MCP** to verify current API of `golang-jwt/jwt/v5`, `pquerna/otp`, `golang.org/x/crypto/argon2` before writing code that uses them. Don't guess — query.
5. **Use `WebSearch`** if you hit unfamiliar errors or unknown territory.
6. Apply skill discipline (samber/cc-skills-golang) — особенно `golang-security` (`crypto/rand`, AES-GCM, ConstantTimeCompare).
7. TDD per `superpowers:test-driven-development`.

Failure to read references / use runtime tools = high probability of repeating known mistakes (alg=none, bcrypt-not-Argon2, wrong API signatures, etc.).
