# ADR-0014: HTTP-роутер: gin-gonic/gin

**Статус:** Accepted (2026-05-07)
**Дата:** 2026-05-07
**Принимающий:** platform team

## Контекст

Backend cmd/api обслуживает REST API для оператора (~30 эндпоинтов) и админа (~80 эндпоинтов), плюс WebSocket-эндпоинты в realtime-модуле. Нужен HTTP-роутер с middleware-цепочкой: request-id, structured logging (zap), recovery, JWT-валидация, RBAC, rate-limit, idempotency, tenant context (`SET LOCAL app.tenant_id`).

Кандидаты: net/http+chi, gin-gonic/gin, fiber, echo. Чистый `net/http` отвергнут — нужна декларативная routing-tree-семантика и группа middleware.

## Решение

Gin (`github.com/gin-gonic/gin` v1.10+).

## Обоснование

1. Bridge `gin-contrib/zap` совмещает gin с zap-логером (ADR-0012), даёт нам стандартизованные поля `tenant_id`/`request_id`/`trace_id` бесплатно.
2. `c.ShouldBindJSON(&dto)` + `c.JSON(status, resp)` сокращает шаблонный код в каждом handler ~в 2 раза против stdlib-стиля.
3. `gin.SetMode(gin.TestMode)` детерминированно глушит логи в `httptest`-сценариях.
4. Большое community + стабильный API (v1 с 2017).
5. RU Go-сообщество знакомо с gin — onboarding дешевле.

## Альтернативы

- **chi** — отлично совместим со stdlib `http.Handler`, но требует ручного JSON-encode/decode и custom logger middleware. Потенциальная экономия: 200-300 строк handler-кода × 110 эндпоинтов.
- **echo** — функционально сопоставим с gin, но ecosystem меньше.
- **fiber** — fasthttp под капотом, несовместимо с net/http middleware (наш TLS termination, идемпотентность, healthcheck-клиенты — все net/http-shaped).

## Последствия

- `pkg/httputil/gin_adapter.go` нужен для перевода stdlib `http.Handler`-middleware (idempotency, requestid) в `gin.HandlerFunc`. Адаптер реализуется в Plan 00a Task 5.
- WebSocket upgrade использует `c.Request` + `c.Writer` напрямую (gorilla/websocket совместим). См. Plan 11.
- Все handler-сигнатуры: `func (h *Handler) Method(c *gin.Context)`. Тесты на handler через `httptest.NewRecorder()` + `gin.CreateTestContext(...)`.
- Запрет добавления `chi` в `go.mod` — депгард rule в Plan 00 Task 9.

## Связанное

- Спека §22 (ADR-0014)
- ADR-0012 (zap — интеграция через `gin-contrib/zap`)
- ADR-0006 (tenant context middleware зависит от gin middleware-цепочки)
- Plan 00a Task 5 (gin_adapter)
- Plan 00 Task 9 (depguard rule на chi)
- Plan 11 (WebSocket upgrade)
