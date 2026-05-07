# ADR-0015: Test-Driven Development as mandatory discipline

**Статус:** Accepted (2026-05-07)
**Дата:** 2026-05-07
**Принимающий:** platform team

## Контекст

План-проекта состоит из 22 implementation plans, каждый из которых разбит на ~10-15 задач. Каждая задача описана как Red-Green-Refactor цикл: «write failing test → run it fails → implement → run it passes → commit». Это TDD-структура.

Без формального утверждения TDD как обязательной дисциплины, исполнители планов могут «срезать» — написать код первым, тесты вторым, а то и пропустить. Это создаёт невидимый технический долг: tests-after-the-fact не покрывают edge cases, не ловят regression, не служат документацией поведения.

## Решение

TDD обязателен для всего нового кода в `internal/` и `pkg/`. Допустимые исключения:
1. `cmd/<binary>/main.go` — composition root, тесты — smoke (запустить и проверить /healthz).
2. Migrations — schema validation через интеграционные тесты.
3. Generated code (mocks, proto-stubs).

## Обоснование

1. Спека §17.1 уже описывает пирамиду тестов с ~2000 unit-тестов как целевое количество. TDD — единственный способ достичь этого без отставания.
2. `samber/cc-skills-golang@golang-testing` § Persona: «You write tests to constrain behavior, not to hit coverage targets.» — TDD естественно ведёт к тестам, фиксирующим поведение.
3. Coverage-таргеты (≥85% service, ≥70% store, ≥60% http/grpc) выполняются автоматически если задача написана как RGR-цикл.

## Альтернативы

- **Tests-after** — быстрее в моменте, медленнее в долгосрочной перспективе. Регрессия = детектор tests-after.
- **No tests** — отвергнуто бизнес-требованием §17.

## Последствия

- Каждая задача в planах 00a, 02-19 — RGR-цикл. PR-template требует подтверждения RGR-дисциплины.
- `superpowers:test-driven-development` skill — обязательная sub-skill при subagent-driven-development.
- `paralleltest` + `thelper` + `testifylint` линтеры обязательны (см. Plan 00 Task 9).
- Когда test становится «не TDD» (e.g. характеризация легаси), автор отмечает `// characterization: pre-existing behaviour` — будет поводом для review.
- Coverage gate в CI: `make test-cover` падает при <70% общего покрытия (см. Plan 00 Task 11).
- TDD-методология распилована в `docs/architecture/08-tdd-discipline.md` (см. Task 3 этого плана).

## Связанное

- Спека §22 (ADR-0015)
- Спека §17.1 (пирамида тестов)
- `docs/architecture/04-testing-strategy.md`
- `docs/architecture/08-tdd-discipline.md`
- Plan 00 Task 9 (линтеры), Plan 00 Task 11 (coverage gate)
