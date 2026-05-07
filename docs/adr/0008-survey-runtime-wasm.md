# ADR-0008: Survey runtime — единый Go-код через WASM, fallback TS-port если TinyGo не справится

**Статус:** Conditional Accept (revised 2026-05-06)
**Дата:** 2026-05-06
**Принимающий:** platform team

## Контекст

Бизнес-логика «следующий узел по ответу» нужна и на сервере (валидация + статистика), и в браузере (мгновенный UX). Дублирование на Go и TS — источник багов.

## Альтернативы

- A: Дублировать на Go и TS, синхронизировать вручную.
- B: Единый код на Go, в браузере — через WebAssembly (TinyGo build).
- C: Единый код на TS, на сервере — через node-sidecar или v8 в Go.
- B-fallback: B как preferred, но если TinyGo не компилирует DSL evaluator (`expr-lang/expr` использует heavy reflection), переходим на A с golden-test contract — общий набор тестовых сценариев гоняется параллельно против Go и TS реализаций.

## Решение

B-fallback.

**Условие исполнения (Plan 07 Task 0):** прежде чем кодить runtime, выполнить TinyGo proof-of-concept:
1. Скомпилировать `expr-lang/expr` Eval с минимальным DSL (5 операций: `==`, `!=`, `<`, `>`, `&&`, `in`).
2. Замерить размер WASM-bundle. Цель: < 500 KB после gzip.
3. Замерить cold-load TTI на target hardware (Core i3 8th, 8GB RAM, Chrome): runtime-init + первый Eval должны завершаться < 200 мс.

Если PoC проходит — продолжаем с B. Если нет — переключаемся на A:
- Go runtime в `internal/surveys/runtime/` остаётся.
- TS-port в `web/src/surveys/runtime.ts` пишется ручно, ~300 LoC.
- `web/tests/survey-runtime/golden.spec.ts` гоняет 50+ test-cases (JSON-схема + JSON-ответ → JSON-next-node) против обеих реализаций. Расхождение = CI failure.

## Последствия

**Trade-off (B):** WASM-bundle ~200-500 KB добавляет к фронтенд-payload (NFR-1 TTI < 2s). Один источник правды для Go-кода.

**Trade-off (A):** Дублирование 200-300 LoC. Защита от drift через golden-tests. Простой dev-workflow (нет TinyGo-toolchain).

Решение между B и A финализируется в Plan 07 Task 0 на основе измерений.

## Связанное

- Спека §22 (ADR-008)
- Plan 07 Task 0 (TinyGo proof-of-concept)
- NFR-1 (TTI < 2s)
