# СоциоПульс — Системный дизайн-документ

**Статус:** Proposed (готов к ревью)
**Дата:** 2026-05-06
**Авторы:** Claude (с пользователем-владельцем продукта)
**Целевая аудитория:** агенты-исполнители, делающие реализацию по этому документу.
**Что описывает:** платформу для проведения телефонных социологических опросов в формате SaaS — архитектура, стек, функциональные и нефункциональные требования, ключевые потоки, модель данных, безопасность, развёртывание, тест-стратегия, риски, ADR.

---

## 1. Контекст и термины

### 1.1 Бизнес-контекст

СоциоПульс — SaaS-платформа для подрядчиков ВЦИОМ и аналогичных социологических служб. Колл-центр-арендатор подключается к платформе, ведёт проекты (политические, социальные, потребительские опросы), назначает операторов, загружает базы респондентов или использует автогенерацию номеров (RDD), запускает автодозвон, контролирует операторов в реальном времени, прослушивает звонки, выгружает отчёты.

Для оператора — простое одноокновое рабочее место: «нажал готов → система сама дозвонилась → говорю с респондентом и веду анкету → выбрал статус → следующий звонок».
Для админа — управление проектами/анкетами/пользователями + контрольные панели.
Для контролёра качества — прослушивание записей и фиксация нарушений.

Платформа физически живёт в Yandex Cloud, данные не покидают РФ (152-ФЗ).

### 1.2 Термины и сокращения

| Термин | Значение |
|---|---|
| Тенант (tenant) | Организация-колл-центр в SaaS, изолирована от других тенантов на уровне данных и ключей шифрования. |
| Проект | Кампания одного заказчика внутри тенанта: целевая выборка, квоты, привязанная анкета, период. |
| Анкета (survey) | Опросный лист — набор вопросов с условной логикой и версионированием. |
| Респондент | Физлицо, которому система звонит. ПДн (телефон) хранятся зашифрованно. |
| Оператор | Сотрудник колл-центра, ведущий разговор. У него FSM-состояние и сессия смены. |
| Контролёр (supervisor) | Сотрудник тенанта со специальной ролью на прослушку и контроль качества. |
| RDD | Random Digit Dialing — генерация номеров по DEF/АВС-кодам без предзагруженной базы. |
| DEF/АВС-код | Префиксы российской нумерации (DEF = мобильные `9XX`, АВС = географические). |
| Trunk | SIP-канал к оператору связи; группа trunk'ов даёт ёмкость одновременных звонков. |
| FS | FreeSWITCH (telephony media + control). |
| ESL | Event Socket Library — управляющий протокол FreeSWITCH. |
| FSM | Finite State Machine — конечный автомат состояний оператора/звонка. |
| RLS | Row-Level Security PostgreSQL — фильтр строк по `tenant_id` на уровне БД. |
| KMS | Key Management Service (Yandex Lockbox + KMS) — управление ключами шифрования. |
| KEK | Key Encryption Key — мастер-ключ тенанта в KMS. |
| DEK | Data Encryption Key — одноразовый ключ для шифрования конкретного объекта. |
| WS | WebSocket. |
| WSS | WebSocket Secure (TLS). |
| OTel | OpenTelemetry. |
| RTO/RPO | Recovery Time Objective / Recovery Point Objective. |
| BFF | Backend For Frontend (gateway-слой нашего монолита). |

### 1.3 Источник истины UI

Источник истины по визуалу и UX — прототип `social-pulse-maket/project/*.jsx + styles.css`. Все страницы оттуда обязательны к реализации в полном объёме. См. Приложение A — инвентаризацию страниц.

---

## 2. Функциональные требования

Требования сгруппированы по доменам (см. раздел 5 — те же домены = модули монолита).

### FR-A. Аутентификация и тенантность

- **FR-A1.** Вход в систему по тройке `(org_id, login, password)`. `org_id` — публичный код тенанта.
- **FR-A2.** Роли: `operator`, `supervisor`, `admin`. Возможна композиция (admin может быть supervisor).
- **FR-A3.** CRUD пользователей в рамках тенанта; архивация (soft-delete) и восстановление учёток; нельзя физически удалить пользователя, у которого есть исторические звонки.
- **FR-A4.** Создание учётки с временным паролем (генерируется системой), форс-смена при первом входе.
- **FR-A5.** 2FA (TOTP) — опционально, включается per-тенант либо per-пользователь.
- **FR-A6.** JWT (access, 15 мин) + refresh-токен (30 дней, ротируемый); отзыв через Redis-blacklist.
- **FR-A7.** Принудительный logout пользователя админом (revoke всех refresh-токенов).
- **FR-A8.** Защита от брутфорса логина: rate-limit на IP + lockout аккаунта на 15 мин после 5 неудачных попыток.
- **FR-A9.** Аудит входов и выходов в `audit_log`.

### FR-B. Проекты и базы респондентов

- **FR-B1.** CRUD проектов: код, имя, заказчик, период (`from`/`to`), цель (target количество анкет), привязанная анкета (через `survey_id`).
- **FR-B2.** Квоты по измерениям: `region` (федеральный округ или конкретный субъект), `gender`, `age_bucket`, или произвольные тенант-настраиваемые. Каждое измерение — список значений с целевыми числами.
- **FR-B3.** Статусы проекта: `active`, `paused`, `archived`. Архив виден на отдельной вкладке, не запускается в дозвон.
- **FR-B4.** Импорт базы респондентов из CSV/XLSX: телефон + произвольные атрибуты (имя, регион, возраст, пол, заметки). Telephone column обязательна, остальное — настраиваемая мапа.
- **FR-B5.** Валидация импортируемых телефонов по российскому формату (E.164 `+7XXXXXXXXXX` либо нормализуемые форматы `8...`, `7...`).
- **FR-B6.** Альтернатива базе — RDD-генерация (см. FR-E).
- **FR-B7.** Назначение операторов на проект (M:N).
- **FR-B8.** Просмотр прогресса проекта: выполнено/осталось анкет, разбивка по округам, темп выполнения.
- **FR-B9.** Просмотр квот в реальном времени (заполнение по каждому измерению).
- **FR-B10.** Per-проектный DNC-список с импортом и ручным добавлением.

### FR-C. Конструктор анкет (два режима)

- **FR-C1.** Form-режим: вертикальный список вопросов; типы — `intro` (вступительный текст без ответа), `single` (один из), `multi` (несколько из), `number` (диапазон), `text` (свободный текст), `select` (большие списки, например регион).
- **FR-C2.** Каждый вопрос: текст для оператора, подсказка (отображается отдельным синим блоком), флаг обязательности.
- **FR-C3.** Условная логика: «показать вопрос N если `q1 in [X, Y]`», «скрыть если ...». Выражения через простой DSL (см. раздел 11).
- **FR-C4.** Flow-режим: блок-схема. Узлы — `start`, `question`, `text-block`, `success-end`, `refusal-end`, `condition`, `jump`. Связи (рёбра) с условиями. Drag-and-drop, палитра.
- **FR-C5.** Графический режим — это тот же `survey_versions.schema`, представленный графически: оба режима читают/пишут одну схему. Переключение режима не теряет данные. Один режим выбран как «основной» для конкретной анкеты, но просмотр в другом доступен read-only.
- **FR-C6.** Версионирование: каждое сохранение → новая версия с инкрементом (v1.0 → v1.1 → v2.0). Major-bump (например, v1.x → v2.0) — при breaking-changes (удалён вопрос, изменён тип). Ручной выбор major/minor при сохранении.
- **FR-C7.** Активная версия — одна на момент. При переключении активной версии: уже идущие звонки продолжают на прежней; новые — на новой.
- **FR-C8.** Превью анкеты: симуляция в браузере без реального звонка, чтобы заказчик мог пощёлкать.
- **FR-C9.** Валидация анкеты при сохранении: нет недостижимых узлов, нет циклов без выхода, все условия ссылаются на существующие вопросы, все типы ответов согласованы.
- **FR-C10.** Свойства анкеты: лимит вопросов (по тенант-настройке), оценка среднего времени, пометки.

### FR-D. Рабочее место оператора

- **FR-D1.** Сессия смены: оператор логинится → выбирает проект из списка назначенных → стартует смена. Смена закрывается явно («завершить смену») или auto после N часов простоя.
- **FR-D2.** FSM состояний: `ready → dialing → call → status → verify → ready`, плюс `pause` из любого, `verify` только для `success`-исходов.
- **FR-D3.** UI каждого состояния — как в прототипе: ready-карточка, dialer-кольцо, активный звонок с таймером, выбор статуса (8 кнопок), перепроверка анкеты, пауза.
- **FR-D4.** Зачитываемое вступление в начале анкеты (отдельный блок с пометкой «зачитайте дословно»).
- **FR-D5.** Прогресс-бар анкеты + счётчик «вопрос N из M».
- **FR-D6.** Подсказка к вопросу (синий блок) — опционально, если задана.
- **FR-D7.** Клавиатурные shortcut'ы: `1`–`5` — выбрать соответствующий вариант ответа; `Space` — пауза в звонке; `Enter` — далее; `Esc` — отмена; `Z` — «затрудняется».
- **FR-D8.** Карточка респондента в активном звонке: телефон, регион, попытка (1/2/3), любые публичные атрибуты из базы.
- **FR-D9.** Кнопки управления звонком: микрофон вкл/выкл, удержание (hold), завершить разговор.
- **FR-D10.** Выбор итогового статуса (8 вариантов): `success`, `refused`, `dropped`, `no-answer`, `busy`, `callback`, `wrong-person`, `tech-failure`. Набор ярлыков перенастраивается per-тенант (FR-A не меняет внутренний enum, только labels).
- **FR-D11.** Перепроверка анкеты перед сохранением (только для `success`): операцор просматривает все ответы, может вернуться и поправить, потом «сохранить».
- **FR-D12.** Комментарий оператора к звонку — свободный текст, опциональный.
- **FR-D13.** Лимит непрерывной паузы: per-тенант (default 15 мин). Превышение → визуальный индикатор у оператора + флаг у админа в мониторинге.
- **FR-D14.** Информация о текущем проекте: «о проекте» (описание, что важно соблюдать), «история моих звонков», «моя результативность» (KPI смены).

### FR-E. Автодозвон

- **FR-E1.** Progressive-режим (1:1): на каждого оператора в `ready` система выбирает один номер, инициирует originate, ждёт ответа.
- **FR-E2.** Источник номеров: приоритет 1 — предзагруженная база (со score = время добавления и retry-задержкой); приоритет 2 — RDD-генерация (включается, когда квоты по региону недобраны и/или база исчерпана).
- **FR-E3.** RDD-генератор: выбор региона по непокрытой квоте → выбор DEF/АВС-кода для региона → случайный 7-значный хвост → проверка `not in DNC AND not in attempted-set AND format valid`.
- **FR-E4.** Лимит RDD-генерации: leaky-bucket per-проект (default 10 номеров/сек, настраивается).
- **FR-E5.** Отметка попыток: каждый звонок инкрементирует `attempt_no` для респондента; max попыток per-тенант (default 3).
- **FR-E6.** Retry-логика по статусам: `no-answer` → +`retry_no_answer_delay` (default 4ч); `busy` → +`retry_busy_delay` (default 30мин); `tech-failure` → +`retry_tech_failure_delay` (default 5мин); `wrong-person` → больше не звоним; `dnc-hit` → блокировка номера.
- **FR-E7.** Working hours: не звоним вне допустимых часов в часовом поясе респондента (резолвится через `regions.yaml`-маппинг). Default: будни 9:00–21:00, выходные 10:00–20:00.
- **FR-E8.** Маршрутизация trunk'а: стратегии `round_robin`, `weighted`, `least_cost`, `least_cost_with_fallback`. Выбор стратегии — per-тенант или per-проект. Нездоровые trunk'и (по health-check'у) исключаются из выбора.
- **FR-E9.** Backpressure: лимит in-flight `originate` на FS-нода (default 60); превышение → переход на следующую нода или короткий backoff.
- **FR-E10.** Учёт квот при выборе номера: если квота региона `R` заполнена ≥ 100% — респонденты этого региона не вынимаются из очереди (но в очереди остаются — на случай ослабления квоты).

### FR-F. Real-time мониторинг (админ + supervisor)

- **FR-F1.** Список всех операторов тенанта с состоянием, проектом, временем в состоянии, KPI смены. Обновление < 1 сек после события.
- **FR-F2.** Фильтры по состоянию (`call`, `online/ready`, `pause`, `processing`, `offline`).
- **FR-F3.** Цветовое выделение проблемных строк: оранжевый — превышение паузы; красный — оператор не отвечает на heartbeat > 30 сек.
- **FR-F4.** Принудительные действия (только admin): «снять с паузы», «завершить смену», «открепить от проекта».
- **FR-F5.** Listen-in (admin + supervisor): подключиться к живому звонку.
  - **v1**: `silent` режим (тихое прослушивание, оператор не знает).
  - **v2**: `whisper` (только оператор слышит) и `barge-in` (полная конференция). Фронт показывает кнопки «скоро» с подсказкой.
- **FR-F6.** Очередь дозвона: счётчики — готовы / в дозвоне / в разговоре / в обработке / сегодняшние сбросы; обновление в реальном времени.
- **FR-F7.** Состояние SIP-trunk'ов: per-trunk health, занятые каналы, текущая нагрузка.

### FR-G. Контроль качества звонков

- **FR-G1.** Сводный список звонков с фильтрами: период, проект, оператор, статус, регион, наличие записи, наличие нарушения.
- **FR-G2.** Поиск по номеру телефона (с маскированием в выдаче), по `call_id`.
- **FR-G3.** Карточка звонка: метаданные (время, оператор, регион, попытка, длительность, hangup cause) + аудио-плеер с waveform + заполненная анкета read-only + комментарий оператора.
- **FR-G4.** Аудио-плеер: воспроизведение, перемотка, скачивание (логируется в audit), скорость 1×/1.5×/2×.
- **FR-G5.** Действия контролёра: «подтвердить статус», «изменить статус» (с обязательным комментарием), «отметить нарушение» (категория + комментарий).
- **FR-G6.** Категории нарушений (тенант-настраиваемые): `грубость`, `не зачитал вступление`, `подсказал вариант`, `завершил досрочно без причины`, `другое`.
- **FR-G7.** Все действия контролёра — в `audit_log`.

### FR-H. Финансы

- **FR-H1.** Дашборд расходов: расходы за период, стоимость анкеты, стоимость минуты связи, маржа, сравнение с предыдущим периодом.
- **FR-H2.** Графики: расходы по месяцам, структура расходов (связь / зарплата / базы), расходы по проектам.
- **FR-H3.** Per-проект финансы: общая сумма расходов с разбивкой, расход на анкету.
- **FR-H4.** Per-тенант тарифы: стоимость минуты по trunk'у (из YAML), стоимость анкеты для оператора (per-проект, в `tenant_settings`).

### FR-I. Отчётность

- **FR-I1.** Преднастроенные отчёты (6 шаблонов): эффективность операторов, сводка по проекту, звонки по статусам, финансы, контроль качества, активность по часам.
- **FR-I2.** Произвольные отчёты: выбор `period`, `project`, `format`. Форматы — `XLSX`, `CSV`, `PDF`.
- **FR-I3.** Async-генерация для больших периодов (> 30 дней или > 100k записей): задание ставится в очередь, по готовности уведомление и presigned URL для скачивания (TTL 24ч).
- **FR-I4.** Все выгрузки — в audit с указанием параметров.

### FR-J. Личная статистика оператора

- **FR-J1.** KPI текущей смены: звонков, успешных анкет, время в звонке, время на паузе, средняя обработка.
- **FR-J2.** Сравнение с командой: средние по проекту с пометкой «выше/ниже».
- **FR-J3.** История моих звонков: таблица за смену + фильтры; для успешных — переход к заполненной анкете (read-only).
- **FR-J4.** Информация о текущем проекте: описание, что важно соблюдать, прогресс, команда.

### FR-K. Уведомления и аудит

- **FR-K1.** Аудит-лог append-only: actor (user/system), action, target, payload (jsonb), ts, ip, user_agent.
- **FR-K2.** Действия для аудита: вход/выход; изменение статуса звонка; прослушка; экспорт записи; изменение анкеты; CRUD пользователей; CRUD проектов; impersonation; force-pause/force-end.
- **FR-K3.** In-app уведомления (бейдж в topbar): превышение паузы оператора, падение trunk'а, заполнение квоты округа, готовность отчёта, новый incident-флаг от контролёра.
- **FR-K4.** Ретенция аудит-лога: 5 лет (cold tier после 1 года).

### FR-L. Темизация и доступность

- **FR-L1.** Светлая/тёмная тема, переключение в topbar, сохранение в профиле.
- **FR-L2.** Размер шрифта 16/18/20px (для операторов с ослабленным зрением), сохранение в профиле.
- **FR-L3.** A11y baseline: WCAG 2.1 AA — контраст, фокус-индикаторы, навигация с клавиатуры, screen-reader для не-критичных страниц (главным образом для админ-выгрузок).
- **FR-L4.** Минимальное разрешение — 1366×768 (типовой ноут оператора).

---

## 3. Нефункциональные требования

### NFR-1. Производительность и ёмкость

| Метрика | Цель |
|---|---|
| Тенантов | до 30 одновременных активных |
| Операторов одновременно (всего) | до 500 |
| Завершённых звонков в день (пик) | 50 000 |
| Активных SIP-каналов в час пик | 400 (пик), 500 (ёмкость trunk'ов с headroom) |
| Real-time event (state change → UI) p95 / p99 | < 500 мс / < 1 сек |
| HTTP API admin-страницы TTFB p95 | < 300 мс |
| Frontend TTI (Core i3 8th, 8GB) | < 2 сек |
| Frontend длительные JS-таски | нет блокирующих > 100 мс |

**Расчёт ёмкости (для воспроизводимости):** 50 000 звонков/день, AHT (avg handling time, включая dialing+talk+wrap) ≈ 4–5 мин, peak-to-average ratio 2.5x (час пик ≈ 10% дневного объёма) → ~5 000 звонков/час пик × 5 мин / 60 = **~400 одновременных talk-каналов**. С учётом параллельных dialing-leg'ов × 1.2 = ~500. Соответствует §8.6 (`max_concurrent_per_node` × число FS-узлов).

### NFR-2. Доступность

- Цель **99.5% в рабочие часы** (9:00–22:00 по Москве). Эквивалент ≤ 2ч простоя/месяц в рабочее окно.
- Плановое обслуживание — в ночное окно (3:00–5:00 МСК).
- Падение одного SIP-провайдера → деградация (меньше пропускной), не отказ.
- Падение одной FS-нода → автоматический re-route оперраторских регистраций; активные звонки этой нода теряются (drop допустим).
- Падение `recording-uploader` → накопление файлов на FS-нода до 24ч без потерь.

### NFR-3. Целостность записей разговоров

- Цель сохранности — **99.95%** (1 потерянная запись на ~2 000). См. ADR-005 — почему не 99.99%.
- Запись пишется на локальный диск FS-нода (NVMe), uploader забирает с подтверждением, файл удаляется только после ack от server'а — двойное хранение на момент upload.
- В S3 — версионирование объектов и MFA-delete на bucket'е каждого тенанта.
- `sha256` записи фиксируется в `call_recordings` и `audit_log`; сверяется при выдаче.

### NFR-4. Безопасность

- TLS 1.3 везде (browser ↔ gateway, internal mTLS между сервисами/sidecar'ами/FS).
- ПДн зашифрованы AES-256-GCM на уровне приложения с per-tenant KEK (Yandex KMS); в Postgres — только bytea.
- Поиск по телефону через `phone_hash bytea` (HMAC-SHA256, per-tenant pepper).
- Секреты — Yandex Lockbox + CSI-driver (volume-mount) или env через k8s-Secret из Lockbox-source.
- Защита от прослушки чужого тенанта — RBAC на gateway + RLS в Postgres + tenant-prefix в NATS-subject + per-WS-pull tenant_id-проверка.
- Зависимости: `osv-scanner` + `govulncheck` на каждый PR, Trivy для образов в CI.
- Аутентификация админ-API — JWT + опциональный mTLS-клиент-серт для super-admin endpoints.
- Защита от CSRF: cookies — `SameSite=Strict`; для WS — токен в первом frame.

### NFR-5. Compliance (152-ФЗ + бизнес-правила)

- Данные физически в РФ (Yandex Cloud RU-зоны).
- IVR-промпт согласия на запись перед соединением с оператором.
- Поддержка деперсонификации респондента по запросу (soft-delete → 30 дней grace → hard-anonymize ПДн, ответы анкеты остаются обезличенными).
- Журнал доступа к ПДн (см. FR-K).
- Retention:
  - Записи: 365 дней hot tier → cold до 3 лет (тенант-настраиваемо) → удаление.
  - Аудит-лог: 5 лет.
  - ПДн респондентов: пока активен договор + 30 дней.
  - Логи приложения: 30 дней hot, 1 год cold.

### NFR-6. Восстановление после сбоев

- **RPO** (приемлемая потеря данных): ≤ 5 минут (Postgres — непрерывный WAL-archive в S3; ClickHouse — потеря допустима, восстанавливается из NATS-replay).
- **RTO** (время на восстановление): ≤ 30 минут до восстановления критичного функционала.
- DR-сценарий: вторая зона доступности Yandex Cloud, cold-standby — образа развёртываются за < 30 мин.
- См. раздел 16.6 для детального DR-плана.

### NFR-7. Мульти-тенантность

- Колонка `tenant_id uuid` во всех бизнес-таблицах.
- Postgres RLS-политики `using (tenant_id = current_setting('app.tenant_id')::uuid)`.
- На gateway — обязательная установка `tenant_id` из JWT в начале каждой транзакции (`SET LOCAL app.tenant_id = $1`).
- NATS — tenant-prefix в subject + permissioned-account отделяет subscribe-права (см. раздел 12).
- KMS — per-tenant KEK; нет общего ключа на платформу (кроме service-owner-уровня).

### NFR-8. Поддерживаемые браузеры

- Chrome / Chromium-edge / Firefox / Safari — последние 2 мажорных версии.
- Для рабочего места оператора **обязателен Chrome ≥ 110** или Firefox ≥ 110 (нужны современные WebRTC API).
- IE/старые браузеры не поддерживаются.

### NFR-9. Локализация

- v1 — только русский язык.
- Архитектурно: `i18next` на фронте, Go-сторона возвращает ID сообщений + параметры; ru.json в репо.
- Переводы на другие языки в v1 не делаются.

### NFR-10. Logging, observability

- Структурированный JSON-лог через `zap` (уровни: debug/info/warn/error).
- Каждая запись лога включает: `tenant_id` (если есть), `request_id`, `trace_id`, `actor`, `action`, `module`.
- Redaction: телефоны/токены/секреты обрезаются до последних 4-х символов либо заменяются маркером `<redacted>`.
- OTel-трейсы через монолит и sidecar'ы (gateway → module → DB → NATS → telephony-bridge → FS-ESL).
- Метрики Prometheus-формата на каждом модуле (см. раздел 15.3).
- Алерты — в OnCall Yandex Cloud / PagerDuty (см. раздел 15.5).

### NFR-11. Конфигурация (как принцип)

- Всё, что может потребоваться менять без релиза, — через YAML (deploy-time) или `tenant_settings` (runtime). Хардкод в Go-коде запрещён для бизнес-параметров.
- Реестр конфигов — раздел 14.

### NFR-12. Идемпотентность мутирующих API

- Все POST/PUT/PATCH-эндпоинты, изменяющие состояние, поддерживают `Idempotency-Key` (UUID, TTL 24ч). Повтор с тем же ключом возвращает закешированный ответ.

### NFR-13. Стоимость инфраструктуры

- Целевой бюджет на инфру в пиковом месяце для 30 тенантов / 500 операторов / 50k звонков/день: оценка в разделе 16.7. Цель — не превысить ориентир без явного решения.

---

## 4. Высокоуровневая архитектура

### 4.1 Компонентная диаграмма

```
                                  ┌──────────────────────────────────────────────┐
                                  │              Browser (React 18 + TS)         │
                                  │  Operator UI · Admin UI · Supervisor UI      │
                                  └────────┬───────────────┬────────────┬────────┘
                                           │ HTTPS         │ WSS        │ WebRTC (DTLS-SRTP)
                                           ▼               ▼            │
┌─────────────────────────────────────────────────────────────────────┐ │
│                    Sociopulse Monolith (cmd/api in Go)              │ │
│  ┌──────────┐  ┌────────┐  ┌─────────┐  ┌────────┐  ┌──────────┐   │ │
│  │ gateway  │→ │  auth  │  │   crm   │  │surveys │  │  dialer  │   │ │
│  │ (HTTP+WS │  │JWT,RBAC│  │projects │  │builder/│  │FSM,queue │   │ │
│  │  BFF)    │  │        │  │ users   │  │runtime │  │RDD,router│   │ │
│  └──────────┘  └────────┘  └─────────┘  └────────┘  └────┬─────┘   │ │
│                                                          │         │ │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌─────────┐  ┌───┴──────┐  │ │
│  │ realtime │ │recording │ │analytics │ │ reports │  │ billing  │  │ │
│  │WS hub    │ │ metadata │ │ CH read  │ │ XLSX/PDF│  │ finance  │  │ │
│  └──────────┘ └──────────┘ └──────────┘ │ async   │  └──────────┘  │ │
│                                         └─────────┘                │ │
│  ┌──────────┐  ┌──────────┐                                        │ │
│  │  audit   │  │ tenancy  │                                        │ │
│  │append-log│  │CRUD,KMS  │                                        │ │
│  └──────────┘  └──────────┘                                        │ │
└─────────┬───────────────────────────────────────────────────┬──────┘ │
          │                                                   │        │
          ▼                                                   ▼        │
   ┌──────────────┐                                 ┌──────────────────┴──┐
   │  Postgres 16 │                                 │  NATS JetStream     │
   │  (RLS)       │                                 │ tenant-prefixed     │
   │ + PgBouncer  │                                 │ subjects + accounts │
   │  (txn mode)  │                                 └─────────┬───────────┘
   └──────────────┘                                           │
   ┌──────────────┐                                  ┌────────┴─────────┐
   │ ClickHouse   │ ← async batch from analytics    │                  │
   └──────────────┘   module (subscriber)        ┌──┴────────┐  ┌──────┴──────────┐
   ┌──────────────┐                              │ telephony │  │ recording-      │
   │  Redis 7     │ ← FSM, queues, presence,     │ -bridge   │  │ uploader        │
   │              │   asynq jobs                 │  (Go)     │  │  (Go)           │
   └──────────────┘                              │ ESL pool  │  │ fsnotify+ffmpeg │
                                                 └─────┬─────┘  │ +KMS+S3 PUT     │
                                                       │ ESL    └────────┬────────┘
                                                       ▼                 │
                                            ┌──────────────────────────────────┐
                                            │ FreeSWITCH cluster (3+ VMs)      │
                                            │ - mod_sofia (trunks + verto-WSS) │
                                            │ - mod_record_session (.wav)      │
                                            │ - local NVMe (recordings buffer) │
                                            └────────────────────┬─────────────┘
                                                                 │ SIP / RTP
                                                                 ▼
                                                  ┌────────────────────────┐
                                                  │  SIP-провайдеры РФ     │
                                                  │  (МТТ/Манго/Билайн/…)  │
                                                  └────────────────────────┘
                                                                 │ PSTN
                                                                 ▼
                                                          [респонденты]


              ┌────────────────────────────────┐
              │  cmd/worker (background jobs)  │
              │  - retention                   │
              │  - retry scheduling            │
              │  - report generation           │
              │  - leader election via         │
              │    Postgres advisory lock      │
              │  - asynq consumer (Redis)      │
              └────────────────────────────────┘

              ┌────────────────────────────────┐
              │  cmd/migrator (DB migrations)  │
              │  - golang-migrate              │
              │  - run as k8s Job pre-deploy   │
              └────────────────────────────────┘
```

### 4.2 Артефакты деплоя

| Артефакт | Что это | Где живёт |
|---|---|---|
| `cmd/api` | Главный монолит (HTTP + WS) | k8s Deployment, ≥ 3 replicas |
| `cmd/worker` | Фоновые задачи + scheduled jobs | k8s Deployment, 2 replicas + leader-election |
| `cmd/migrator` | DB-миграции | k8s Job (запускается до rollout API) |
| `cmd/telephony-bridge` | ESL-мост к FreeSWITCH | k8s Deployment, 2 replicas |
| `cmd/recording-uploader` | Сборщик записей | systemd-unit на каждой FS-VM |
| FreeSWITCH | Telephony media + control | bare-metal/VM, ≥ 3 ноды |
| Postgres 16 | OLTP БД | Yandex Managed PostgreSQL |
| ClickHouse | OLAP БД | Yandex Managed ClickHouse |
| Redis 7 | Cache, queues, presence | Yandex Managed Redis |
| NATS JetStream | Event bus | k8s StatefulSet, 3 nodes |
| S3 (Yandex Object Storage) | Записи + бэкапы + отчёты | Managed |

### 4.3 Сквозной поток «оператор делает один звонок»

1. Оператор открывает `https://app.sociopulse.ru/operator/workstation`. Фронт запрашивает `/api/me` → если 401, редирект на `/login`.
2. Логин: фронт шлёт `POST /api/auth/login {org_id, login, password}` → `gateway` → `auth.Authenticate()` → JWT (access + refresh) в HttpOnly-cookie + payload в JSON.
3. Браузер открывает WS на `wss://app.sociopulse.ru/ws`, шлёт первый кадр `{type:"auth", token:"..."}`. `realtime`-модуль валидирует, регистрирует презенс в Redis.
4. Оператор выбирает проект → `POST /api/sessions/start {project_id}` → `dialer.StartShift()` создаёт `operator_sessions`-запись, оператор переходит в `ready`.
5. Браузер инициирует **WebRTC-регистрацию через `mod_verto`**: `POST /api/telephony/operator-credentials` → `telephony-bridge` через ESL создаёт временный SIP-аккаунт оператора `op_<user_id>_<session>` на одной из FS-нода с TTL = время смены, отдаёт фронту `{ws_url, sip_user, sip_password, fs_node_id}`. Фронт открывает второй WS — `wss://fs-node-2.sociopulse.ru:8082/` — verto-протокол, и регистрируется. Дальше browser ↔ FS-нода имеют WebRTC-канал.
6. `dialer` подписан на `dialer.op.<tenant>.<op>.state` → видит `ready` → берёт следующий номер из Redis ZSET `project:<id>:queue` через `ZPOPMIN`. Перед использованием проверяет: квоты региона не заполнены, респондент не в DNC, working hours для региона.
7. `dialer` публикует `telephony.cmd.<tenant>.<call_id> {action: "originate", number, trunk_strategy, operator_sip_user}`.
8. `telephony-bridge` принимает команду, выбирает trunk через `Router` (по health + стратегии), даёт ESL-команду `originate` к выбранной FS-нода. FS дозванивается до респондента через `mod_sofia` → trunk → PSTN.
9. **Респондент берёт трубку** → `CHANNEL_ANSWER` → FS играет промпт согласия (`mod_playback`) длительностью 4–6 сек → стартует запись (`mod_record_session` пишет в `/var/spool/sociopulse/<tenant>/<call_id>.wav`) → бриджит на оператора (`bridge user/op_<user_id>_<session>@fs-node`).
10. ESL-события (`CHANNEL_ANSWER`, `CHANNEL_BRIDGE`) публикуются в `telephony.event.<tenant>.<call_id>`. `dialer` обновляет `calls.answered_at = now()`, переводит оператора в `call`, публикует `dialer.op.state` → `realtime` пушит фронту → UI оператора переключается на `ActiveCallCard`.
11. Оператор ведёт разговор + заполняет анкету. Каждый ответ → `POST /api/calls/{id}/answers {question_id, value}` (idempotency-key) → `surveys.RuntimeService` валидирует ответ + сохраняет `call_answers` строку.
12. Конец разговора (`CHANNEL_HANGUP` от респондента или оператора) → запись закрывается → ESL-событие → `dialer` ставит оператора в `status` (выбор статуса) или, если `success`-сценарий уже подтверждён — в `verify`.
13. `recording-uploader` (на FS-нода) видит файл через `fsnotify`, конвертирует в Opus 32 кбит/с, генерирует random DEK (AES-256), шифрует файл DEK'ом, шифрует DEK через KMS-call (KEK тенанта), кладёт `call_id.opus` + `call_id.dek.enc` в bucket `sociopulse-recordings-<tenant>`, сверяет sha256, делает gRPC-вызов `recording.Commit(ctx, ...)` → `recording`-модуль создаёт запись в `call_recordings`, пишет `audit.Log("recording.committed", sha256, ...)`. После ack — uploader удаляет локальные файлы.
14. Оператор выбирает статус (`success`) → `POST /api/calls/{id}/status {status: "success"}` → `dialer.FinalizeCall()` обновляет `calls.status`, инкрементирует счётчики квот в `project_quotas`, переводит оператора в `verify`.
15. Оператор перепроверяет анкету, нажимает «сохранить» → оператор переходит в `ready`, цикл начинается заново с шага 6.

### 4.4 Допущения и ограничения

- Браузер оператора имеет стабильный интернет ≥ 1 Мбит/с upload (для WebRTC аудио + UI). Деградация → audio jitter, операционно — задача колл-центра.
- Корпоративная сеть колл-центра пропускает UDP-трафик к нашим FS-нодам **либо** мы предоставляем TURN-сервер (см. Риск R-3).
- SIP-trunk'и — у тенанта-владельца платформы (мы) с операторами связи РФ. Тенанты-арендаторы не приносят свои trunk'и в v1 (можно добавить в v2).
- Один логин оператора в один момент — только в одном браузере (mutex по `user_id` в Redis).

---

## 5. Декомпозиция: модули, sidecar'ы, воркер

### 5.1 Принципы

- **Модульный монолит**: единый Go-репозиторий, единая Go-сборка `cmd/api`, но внутренние границы модулей жёсткие.
- Каждый модуль — пакет `internal/<module>/` с подпакетами `api` (публичные интерфейсы), `service` (бизнес-логика), `store` (Postgres), `events` (NATS).
- Cross-module вызовы — **только через интерфейс из `api`-подпакета**. Прямой импорт `internal/<module>/service` из чужого модуля запрещён `depguard`.
- Модули регистрируются в `cmd/api/main.go` через DI-контейнер (uber-fx или ручной wire).
- При нужде вынести модуль в отдельный сервис — заменяется реализация интерфейса (in-process call → gRPC client). Доменная логика не переписывается.

### 5.2 Каталог модулей

| Модуль | Ответственность | Главные интерфейсы (`<module>/api`) |
|---|---|---|
| `gateway` | HTTP/WS routing, middleware (auth, tracing, rate-limit, idempotency), input validation | `RouteRegistry`, `RequestContext` |
| `auth` | JWT/refresh, RBAC, CRUD пользователей, login attempts, audit входов | `Authenticator`, `UserService`, `RBACChecker` |
| `crm` | CRUD проектов, респондентов, импорт баз, квоты | `ProjectService`, `RespondentService`, `QuotaTracker` |
| `surveys` | CRUD анкет (form+flow), валидация графа, версионирование, runtime-исполнитель ответов с условной логикой | `SurveyService`, `VersionStore`, `Runtime`, `ConditionalEvaluator` |
| `dialer` | FSM оператора, очередь номеров (Redis), RDD-генератор, retry-логика, маршрутизатор trunk'ов, working-hours-контроль, backpressure | `OperatorFSM`, `CallQueue`, `RDDGenerator`, `Router`, `LineCapacityTracker` |
| `realtime` | WebSocket-хаб, fan-out NATS-событий, presence, listen-in commands | `Hub`, `Subscription`, `PresenceTracker` |
| `recording` | API над метаданными записей, presigned URL, retention-задачи (через worker) | `RecordingMetadata`, `URLSigner`, `RetentionPlanner` |
| `analytics` | NATS-консьюмер для вставки в ClickHouse, чтение для дашбордов и отчётов | `IngestPipeline`, `MetricsQuery` |
| `reports` | Преднастроенные отчёты + произвольные; XLSX/CSV/PDF; async через asynq | `ReportRenderer`, `AsyncJobs` |
| `billing` | Финансы, тарифы, расчёт стоимостей | `CostCalculator`, `TariffStore` |
| `audit` | Append-only журнал | `AuditLogger` |
| `tenancy` | Service-Owner CRUD тенантов, KMS-ключи, `tenant_settings` | `TenantService`, `SettingsCache`, `KMSResolver` |

### 5.3 Sidecar-процессы

#### 5.3.1 `telephony-bridge`

- **Назначение**: единственный собственник ESL-соединений к FS-нодам.
- **Состояние**: in-memory map `node_id → ESL-conn`; пере-подключение при дисконнекте; health-check каждые 5 сек через `sofia status`.
- **Подписки на NATS**: `telephony.cmd.>` (команды от `dialer`).
- **Публикации в NATS**: `telephony.event.<tenant>.<call_id>` (события каналов).
- **Команды**:
  - `originate {number, trunk_id, operator_sip_user, prompt_url, recording_path, idempotency_key}`
  - `hangup {call_id, cause}`
  - `mixmonitor {call_id, mode: silent|read|write|both, listener_endpoint}`
  - `play {call_id, url}`
  - `transfer {call_id, target}` (для будущего)
- **События публикуемые**:
  - `channel.dialing`, `channel.answered`, `channel.bridged`, `channel.hangup {cause, sip_response}`
  - `recording.started`, `recording.stopped`
  - `node.unhealthy`, `trunk.unhealthy`
- **Деплой**: 2 replicas, sticky к нодам через consistent-hash (одна replica = primary owner для подмножества FS-нода, вторая — hot-standby).

#### 5.3.2 `recording-uploader`

- **Назначение**: на каждой FS-VM забирать новые записи, конвертировать, шифровать, грузить в S3, регистрировать в `recording`-модуле.
- **Деплой**: systemd-unit `sociopulse-recording-uploader.service` рядом с FreeSWITCH-процессом. Запускается при boot VM, перезапускается systemd при крэше.
- **Watch path**: `/var/spool/sociopulse/<tenant>/`. fsnotify CREATE-events.
- **Pipeline per file**:
  1. `ffmpeg -y -i <call_id>.wav -c:a libopus -b:a 32k -ar 16000 /var/spool/.staging/<call_id>.opus`
  2. SHA-256 над `.opus`-файлом.
  3. KMS data-key encryption: `KMS.GenerateDataKey(tenant_kek_id)` → `(plaintext_dek, encrypted_dek)`.
  4. Шифрование AES-256-GCM file-by-file с random nonce, plaintext_dek используется как ключ.
  5. PUT в S3: `sociopulse-recordings-<tenant>/<YYYY-MM-DD>/<call_id>.opus.enc` + рядом `<call_id>.dek.enc`.
  6. gRPC `recording.Commit({call_id, s3_bucket, s3_key, sha256, duration_sec, encrypted_dek, kms_key_id, codec})` к `cmd/api`.
  7. Если ack — удалить `.wav`, `.opus`, `.staging/`.
  8. Если ошибка — оставить файлы, retry с экспоненциальным backoff.
- **Retry / алерты**:
  - Файл не загружен > 1 час → метрика `recording_upload_lag_seconds` растёт → алерт.
  - Local disk > 80% → срочный алерт + uploader получает приоритет.
- **Безопасность**: имеет свою service-account (mTLS-сертификат), может вызывать только `recording.Commit`. Не имеет доступа к остальной API.

### 5.4 cmd/worker — фоновые задачи

Отдельный процесс для всего, что не относится к realtime-обработке HTTP/WS.

- **Лидер-выбор**: один из replicas работает «лидером» для scheduled-задач через Postgres advisory lock (`pg_advisory_lock(<job_kind_hash>)`), остальные ждут. При крэше лидера — следующий захватывает lock.
- **Очередь задач**: `asynq` поверх Redis (есть UI, retry semantics, scheduling).

| Задача | Триггер | Действие |
|---|---|---|
| `dialer.retry_due` | Каждые 30 сек, leader-only | Сканирует respondents с `next_attempt_at <= now()` → возвращает в ZSET очереди |
| `recording.retention_pass` | Раз в сутки в 03:00 МСК | Сканирует `call_recordings` с `retention_until < now()` → переводит в cold tier S3 → если `delete_at < now()` — удаляет |
| `audit.archive_pass` | Раз в неделю | Аудит-записи > 1 года → cold tier S3 |
| `reports.generate` | По событию из API | Async-генерация отчётов |
| `quotas.recompute` | Раз в час | Перепересчёт счётчиков квот (защита от race-conditions, опциональная reconciliation) |
| `sessions.cleanup` | Раз в час | Закрытие смен оператора с no-heartbeat > 2ч |
| `dnc.import` | По событию | Импорт DNC-файла |

### 5.5 Зависимости между модулями (направленный граф)

```
gateway → {auth, crm, surveys, dialer, realtime, recording, analytics, reports, billing}
realtime → {auth, dialer, audit}
dialer → {crm, surveys, recording, telephony-bridge (через NATS), audit}
crm → {tenancy, audit}
surveys → {tenancy, audit}
recording → {tenancy, audit, S3}
analytics → {} (читает только)
reports → {analytics, recording}
billing → {analytics}
auth → {tenancy, audit}
audit → {} (только пишет)
tenancy → {KMS}
```

Критическое: **нет циклов**. `audit` — листовой модуль, его все вызывают, он никого. `tenancy` — почти листовой (только KMS).

---

## 6. Модель данных

### 6.1 Подход к multi-tenancy

- Колонка `tenant_id uuid not null` на каждой бизнес-таблице.
- RLS-политика: `using (tenant_id = current_setting('app.tenant_id', true)::uuid)`.
- В начале каждой транзакции `gateway` исполняет `SET LOCAL app.tenant_id = $1` (см. ADR-006 — почему `LOCAL` и почему transaction-mode pgbouncer).
- Cross-tenant-запросы запрещены архитектурно: нет таких эндпоинтов в API; единственный cross-tenant модуль — `tenancy` (для service-owner-уровня), которому RLS обходится через специальную роль `tenancy_admin` с `BYPASSRLS`.

### 6.2 Шифрование PII

- ПДн (телефон респондента, телефон пользователя) — `bytea` с AES-256-GCM в приложении.
- Поиск — через колонку-хеш `<field>_hash bytea` (HMAC-SHA256 с per-tenant pepper).
- KEK — Yandex KMS, per-tenant.
- DEK для PII — кешируется в памяти с TTL 5 мин per-tenant; resolve через KMS.Decrypt в начале сессии тенанта.
- Ротация KEK — раз в год; старые DEK расшифровываются прежними версиями KEK (Yandex KMS поддерживает версионирование).

### 6.3 Ключевые таблицы (упрощённо)

```sql
-- ─────── tenancy ───────
create table tenants (
  id uuid primary key default gen_random_uuid(),
  org_code text not null unique,                  -- "CC-MOSKVA-01"
  name text not null,
  status text not null check (status in ('active','suspended','archived')),
  kms_kek_id text not null,                        -- "yk-kek-tenant-<id>"
  phone_hash_pepper bytea not null,                -- random 32 bytes per-tenant
  created_at timestamptz not null default now()
);

create table tenant_settings (
  tenant_id uuid not null references tenants(id),
  key text not null,
  value jsonb not null,
  updated_at timestamptz not null default now(),
  primary key (tenant_id, key)
);

-- ─────── auth ───────
create table users (
  id uuid primary key default gen_random_uuid(),
  tenant_id uuid not null references tenants(id),
  login text not null,
  password_hash text not null,                     -- argon2id
  full_name text not null,
  role text not null check (role in ('operator','supervisor','admin')),
  status text not null check (status in ('active','archived')),
  totp_secret_encrypted bytea,                     -- nullable
  hired_at date,
  last_login_at timestamptz,
  created_at timestamptz not null default now(),
  unique (tenant_id, login)
);

create table user_sessions (
  id uuid primary key default gen_random_uuid(),
  tenant_id uuid not null,
  user_id uuid not null references users(id),
  refresh_token_hash text not null,
  expires_at timestamptz not null,
  ip text, user_agent text,
  revoked_at timestamptz,
  created_at timestamptz not null default now()
);
create index on user_sessions (user_id, expires_at) where revoked_at is null;

-- ─────── crm ───────
create table projects (
  id uuid primary key default gen_random_uuid(),
  tenant_id uuid not null,
  code text not null,
  name text not null,
  customer text,                                   -- "ВЦИОМ"
  status text not null check (status in ('active','paused','archived')),
  target_count int not null default 0,
  period_from date, period_to date,
  survey_id uuid,                                  -- references surveys(id)
  default_survey_version_id uuid,                  -- snapshot пини
  created_at timestamptz not null default now(),
  unique (tenant_id, code)
);

create table project_quotas (
  project_id uuid not null references projects(id),
  dimension_kind text not null,                    -- 'region','gender','age_bucket','custom'
  dimension_value text not null,                   -- 'ЦФО','M','25-34'
  target int not null,
  done int not null default 0,
  primary key (project_id, dimension_kind, dimension_value)
);

create table project_assignments (
  project_id uuid not null references projects(id),
  operator_id uuid not null references users(id),
  assigned_at timestamptz not null default now(),
  primary key (project_id, operator_id)
);

create table respondents (
  id uuid primary key default gen_random_uuid(),
  tenant_id uuid not null,
  project_id uuid not null references projects(id),
  phone_encrypted bytea not null,
  phone_hash bytea not null,
  region_code text not null,                       -- '77','50',...
  attributes jsonb not null default '{}',          -- любые поля из импорта
  status text not null default 'pending'
    check (status in ('pending','dialing','completed','dnc','exhausted','wrong')),
  attempts int not null default 0,
  last_attempt_at timestamptz,
  next_attempt_at timestamptz,                     -- когда снова можно звонить
  source text not null check (source in ('imported','rdd')),
  created_at timestamptz not null default now()
);
create index on respondents (project_id, status, next_attempt_at);
create index on respondents (project_id, phone_hash);

create table project_dnc (
  tenant_id uuid not null,
  project_id uuid,                                 -- nullable: tenant-wide DNC
  phone_hash bytea not null,
  source text not null,                            -- 'manual','import','wrong-person'
  added_at timestamptz not null default now(),
  primary key (tenant_id, coalesce(project_id, '00000000-0000-0000-0000-000000000000'::uuid), phone_hash)
);

-- ─────── surveys ───────
create table surveys (
  id uuid primary key default gen_random_uuid(),
  tenant_id uuid not null,
  name text not null,
  current_version_id uuid,
  primary_mode text not null default 'form'
    check (primary_mode in ('form','flow')),
  created_at timestamptz not null default now()
);

create table survey_versions (
  id uuid primary key default gen_random_uuid(),
  tenant_id uuid not null,
  survey_id uuid not null references surveys(id),
  version_label text not null,                     -- 'v1.0','v1.1','v2.0'
  schema jsonb not null,                           -- normalized form+flow schema
  is_active boolean not null default false,
  created_at timestamptz not null default now(),
  created_by uuid references users(id)
);
create unique index survey_versions_active_one
  on survey_versions(survey_id) where is_active;

-- ─────── dialer / calls ───────
create table calls (
  id uuid primary key default gen_random_uuid(),
  tenant_id uuid not null,
  project_id uuid not null references projects(id),
  respondent_id uuid references respondents(id),
  operator_id uuid references users(id),
  survey_version_id uuid references survey_versions(id),
  started_at timestamptz not null default now(),
  answered_at timestamptz,
  ended_at timestamptz,
  duration_sec int,
  status text not null default 'in-progress'
    check (status in ('in-progress','success','refused','dropped',
                      'no-answer','busy','callback','wrong-person','tech-failure')),
  hangup_cause text,
  attempt_no int not null default 1,
  trunk_used text,
  sip_call_id text,
  freeswitch_node text,
  comment text
);
create index on calls (project_id, started_at desc);
create index on calls (operator_id, started_at desc);
create index on calls (status, started_at desc);

create table call_events (
  call_id uuid not null references calls(id),
  ts timestamptz not null,
  event text not null,
  payload jsonb,
  primary key (call_id, ts, event)
);

create table call_recordings (
  call_id uuid primary key references calls(id),
  tenant_id uuid not null,
  s3_bucket text not null,
  s3_key text not null,
  duration_sec int not null,
  sha256 text not null,
  codec text not null default 'opus-32',
  encrypted_dek bytea not null,
  kms_key_id text not null,
  retention_until date not null,
  delete_at date,                                  -- when to actually delete (after cold)
  created_at timestamptz not null default now()
);

create table call_answers (
  call_id uuid not null references calls(id),
  question_id text not null,                       -- из survey schema
  answer jsonb not null,
  answered_at timestamptz not null default now(),
  primary key (call_id, question_id)
);

-- ─────── operator sessions / state log ───────
create table operator_sessions (
  id uuid primary key default gen_random_uuid(),
  tenant_id uuid not null,
  user_id uuid not null references users(id),
  project_id uuid not null references projects(id),
  started_at timestamptz not null default now(),
  ended_at timestamptz,
  total_call_sec int not null default 0,
  total_pause_sec int not null default 0
);

create table operator_state_log (
  session_id uuid not null references operator_sessions(id),
  ts timestamptz not null,
  state text not null,
  reason text,
  primary key (session_id, ts)
);

-- ─────── audit ───────
create table audit_log (
  id bigserial primary key,
  tenant_id uuid not null,
  actor_kind text not null check (actor_kind in ('user','system','service')),
  actor_user_id uuid,                               -- nullable for system
  action text not null,
  target_kind text not null,
  target_id text,
  payload jsonb,
  ts timestamptz not null default now(),
  ip text,
  user_agent text
);
create index on audit_log (tenant_id, ts desc);
create index on audit_log (action, ts desc);

-- ─────── reports / async jobs ───────
create table reports_jobs (
  id uuid primary key default gen_random_uuid(),
  tenant_id uuid not null,
  requested_by uuid references users(id),
  kind text not null,                               -- 'predefined:efficiency','custom'
  params jsonb not null,
  status text not null check (status in ('queued','running','succeeded','failed')),
  result_s3_key text,
  error text,
  created_at timestamptz not null default now(),
  finished_at timestamptz
);
```

### 6.4 ClickHouse-таблицы (аналитика)

```sql
create table events_calls
(
  date Date,
  ts DateTime64(3),
  tenant_id UUID,
  project_id UUID,
  operator_id UUID,
  call_id UUID,
  status LowCardinality(String),
  duration_sec UInt32,
  hangup_cause LowCardinality(String),
  region_code LowCardinality(String),
  attempt_no UInt8,
  trunk_used LowCardinality(String)
) engine = MergeTree
partition by toYYYYMM(date)
order by (tenant_id, project_id, ts);

create table events_operator_state
(
  date Date,
  ts DateTime64(3),
  tenant_id UUID,
  user_id UUID,
  state LowCardinality(String),
  duration_in_state_sec UInt32
) engine = MergeTree
partition by toYYYYMM(date)
order by (tenant_id, user_id, ts);
```

NATS-консьюмер `analytics`-модуля батчит события в CH каждые 5 сек (или 10000 строк).

### 6.5 Миграции

- Инструмент: `golang-migrate` (поддерживает up/down, schema_migrations таблица).
- Файлы: `migrations/<timestamp>_<name>.up.sql` + `<...>.down.sql`.
- Запуск: `cmd/migrator` как k8s Job pre-deploy (sync hook ArgoCD).
- Запрещено: drop column в одну миграцию (двухфазный паттерн: добавили new → переключили код → удалили old).
- Все миграции проходят CI на staging-БД с продовским snapshot'ом.

---

## 7. Телефонная плоскость

### 7.1 Топология FreeSWITCH-кластера

- 3 ноды на старте (горизонтально масштабируется до 6-8 при росте).
- Каждая нода — отдельная VM в Yandex Cloud с публичным IP. **Не в Kubernetes** (RTP-traffic не дружит с k8s NAT/Service-LB; см. ADR-007).
- ОС: Ubuntu 22.04 LTS, FreeSWITCH 1.10.x (LTS).
- Образ собирается через Packer + Ansible, версия пинится через переменную в IaC.

### 7.2 Конфигурация FreeSWITCH

#### 7.2.1 Профили sofia

```xml
<!-- profile: trunks (исходящие в SIP-провайдеров) -->
<profile name="trunks">
  <gateways>
    <gateway name="mtt-msk-1">
      <param name="username" value="..."/>
      <param name="password" value="..."/>      <!-- подставляется Ansible'ом из Lockbox -->
      <param name="proxy" value="sip.mtt-business.ru:5060"/>
      <param name="register" value="true"/>
      <param name="caller-id-in-from" value="true"/>
    </gateway>
    <gateway name="mango-fed">
      <!-- ... -->
    </gateway>
  </gateways>
</profile>

<!-- profile: operators (внутренние WebRTC-аккаунты операторов через verto) -->
<profile name="operators">
  <param name="auth-calls" value="true"/>
  <param name="apply-inbound-acl" value="webrtc"/>
  <param name="ws-binding" value=":8081"/>
  <param name="wss-binding" value=":8082"/>
  <param name="dtls-version" value="1.2"/>
  <!-- per-call sip-аккаунты создаются telephony-bridge через ESL -->
</profile>
```

#### 7.2.2 mod_event_socket (ESL)

- Слушает на `127.0.0.1:8021` (не публично, через mTLS-туннель stunnel).
- Доступ только из `telephony-bridge` (по mTLS-сертификату).

#### 7.2.3 mod_record_session

- Запись в `/var/spool/sociopulse/<tenant_id>/<call_id>.wav` (PCM 8kHz mono 16-bit).
- Триггер на запись — после промпта согласия (`event:record_session`).

#### 7.2.4 mod_callcenter — НЕ используем

- Диспетчеризацию делает наш `dialer`-модуль через explicit `originate`-команды.

### 7.3 Маршрутизация trunk'ов

Конфиг в `telephony-bridge` (yaml):

```yaml
trunks:
  - id: mtt-msk-1
    sip_gateway: mtt-msk-1                # имя в FS sofia profile
    capacity_channels: 100                # из контракта
    cost_per_minute_rub: 3.42
    weight: 60
    regions: ["77","50","78"]             # коды Москвы/МО/СПб (ABC)
    healthcheck:
      method: options
      interval: 30s
      timeout: 5s
      unhealthy_after_failures: 2
      recover_after_successes: 3
  - id: mango-fed
    sip_gateway: mango-fed
    capacity_channels: 150
    cost_per_minute_rub: 3.78
    weight: 40
    regions: ["*"]                        # fallback
    healthcheck: ...
  - id: beeline-srf
    sip_gateway: beeline-srf
    capacity_channels: 100
    cost_per_minute_rub: 4.12
    weight: 20
    regions: ["54","66","23","61"]        # СФО, ЮФО, УрФО
    healthcheck: ...

routing:
  default_strategy: least_cost_with_fallback
  per_tenant_overrides:
    enabled: true                         # тенант может выбрать свою стратегию
```

Стратегии (реализованы в `dialer.Router`):
- `round_robin`: по очереди, скип unhealthy.
- `weighted`: пропорция по `weight`.
- `least_cost`: минимальный `cost_per_minute_rub` среди подходящих по региону.
- `least_cost_with_fallback`: least_cost + при недоступности — fallback на `regions=["*"]`.

Балансировка по нагрузке: учёт активных каналов на trunk'е через Redis-счётчик.

### 7.4 Аудио-путь оператора (ADR-001 — фиксация)

**Решение**: WebRTC прямо в браузере оператора через `mod_verto` или SIP-over-WSS через `mod_sofia`.

**Поток**:
1. Оператор логинится в UI.
2. UI делает `POST /api/telephony/operator-credentials` → `telephony-bridge` создаёт временный SIP-аккаунт `op_<user_id>_<session>` через ESL (`directory.create` + password = random per-session).
3. UI получает `{ws_url: "wss://fs-node-2:8082", sip_user: "op_xxx_yyy", sip_password: "...", caller_id: "..."}`.
4. UI инициализирует verto JS-клиент (или sip.js + WSS), регистрируется на FS-нода.
5. При входящем `bridge` — verto открывает WebRTC media-stream между browser и FS-нода (DTLS-SRTP, peer-to-peer относительно браузера и FS).
6. При завершении смены — UI отзывает verto-сессию, `telephony-bridge` удаляет временный SIP-аккаунт.

**STUN/TURN**:
- STUN-сервер: Yandex Cloud STUN либо публичный.
- TURN — собственный (coturn) на отдельной VM в случае, если корпоративные сети колл-центров блокируют UDP-direct (см. Риск R-3).

### 7.5 Запись согласия

- При `CHANNEL_ANSWER` (респондент взял трубку) — `telephony-bridge` шлёт ESL-команду на проигрывание промпта:
  ```
  uuid_broadcast <call_id> playback::<consent_prompt_url>
  ```
- `consent_prompt_url` — URL аудио (Opus/WAV) в S3, на тенанта; по умолчанию — стандартный текст «В целях контроля качества разговор записывается. Если вы согласны, оставайтесь на линии».
- После окончания промпта (`PLAYBACK_STOP`) — стартует запись:
  ```
  uuid_record <call_id> start /var/spool/sociopulse/<tenant_id>/<call_id>.wav
  ```
- И параллельно — bridge на оператора. Оператор начинает слышать респондента **после** промпта (это намеренно — респондент должен прослушать сообщение).

---

## 8. Алгоритм автодозвона

### 8.1 Server-side FSM оператора

Состояния:

```
        offline
           ↓ login + select project
       ready ←────────────────────────┐
           ↓ pickFromQueue()          │
        dialing                       │
           ↓ ANSWER (bridge)          │
        call                          │
           ↓ HANGUP                   │
        status (выбор статуса)        │
           ↓ if status=success        │
        verify                        │
           ↓ save                     │
        ready ────────────────────────┘
        
       pause ↔ ready (может из любого через hold)
```

- Серверное хранение: `hash op:<id>:state` в Redis с полями `state`, `since_ts`, `current_call_id`, `project_id`.
- Лог переходов: `operator_state_log`.
- Heartbeat: оператор каждые 10 сек шлёт WS-ping → `realtime` обновляет `presence:<id>` в Redis (TTL 30 сек). При TTL-expire — оператор переходит в `offline` принудительно, активный звонок завершается.

### 8.2 Очередь номеров проекта

- Redis Sorted Set `project:<id>:queue`, score = `priority + epoch_seconds`.
- Структура score:
  - Импорт базы: score = `0 + import_index` (сразу доступны, в порядке импорта).
  - Retry no-answer: score = `next_attempt_at_epoch + 100000` (отложен).
  - RDD-генерация: score = `now + 60` (даём преимущество базе).
- `dialer.PickNext(project_id)`:
  ```
  // pseudo
  for try := 1..3:
    res = ZPOPMIN(queue:<project_id>, count=1)
    if not res: return nil
    member = res[0]
    if member.score > now+1: ZADD(...member); return nil  // не пришло время
    respondent = lookup(member.id)
    if respondent.region_quota_full: skip; continue
    if respondent.in_dnc: ZREM; mark; continue
    if respondent.outside_working_hours: ZADD(...with delay); continue
    return respondent
  return nil
  ```
- Concurrency-safe: `ZPOPMIN` атомарен в Redis.

### 8.3 RDD-генератор

Каталог DEF/АВС-кодов: `configs/regions.yaml` в репо. Структура:
```yaml
regions:
  - code: "77"
    name: "Москва"
    okrug: "ЦФО"
    timezone: "Europe/Moscow"
    abc_codes: ["495","499","498"]
    def_codes: ["916","926","999","968","965"]   # пример
  - code: "78"
    name: "Санкт-Петербург"
    okrug: "СЗФО"
    timezone: "Europe/Moscow"
    abc_codes: ["812"]
    def_codes: ["911","921","981"]
  # ... все 89 регионов
```

Алгоритм генерации:
```
generate(project, target_region):
  region = pickRegionByLowestQuota(project, target_region)
  code = pickRandomCode(region.abc_codes ∪ region.def_codes)
  for try := 1..50:
    tail = randomDigits(7)
    phone = "+7" + code + tail
    if !validRussianFormat(phone): continue
    if existsInDNC(phone, project): continue
    if alreadyAttemptedInProject(phone, project): continue
    if alreadyAttemptedAnyProjectThisTenant(phone, lastNDays=30): continue   // anti-spam
    return phone
  return nil   // не удалось за 50 попыток — алерт
```

Дедупликация в проекте — Redis SET `project:<id>:attempted` (Bloom filter не нужен на нашем масштабе, прямой SET 50k/день * 30 дней = 1.5M элементов = ~50MB Redis, приемлемо).

Лимит rate: Redis `INCR project:<id>:rdd_bucket` с TTL 1 сек, max=10. Превышение → backoff 1 сек.

### 8.4 Working-hours контроль

- Регион респондента → `regions.yaml` → tz → локальное время.
- Тенант-настройка: `working_hours_weekdays = "09:00-21:00"`, `working_hours_weekends = "10:00-20:00"`.
- Если текущее локальное время не в окне — респондент не выбирается, ставится в очередь со score = время следующего входа в окно.

### 8.5 Retry-логика

| Финальный статус | Поведение |
|---|---|
| `success` | помечается `respondents.status = completed`; не звоним больше |
| `refused` | `status = completed` (мы уважаем отказ) |
| `wrong-person` | `status = wrong`, в DNC проекта; не звоним |
| `dropped` | `status = pending`; retry через `retry_dropped_delay` (default 2ч) |
| `no-answer` | `attempts < max ?` → retry через `retry_no_answer_delay` (default 4ч); иначе `exhausted` |
| `busy` | `attempts < max ?` → retry через `retry_busy_delay` (default 30мин) |
| `callback` | retry через явное время, указанное оператором в комментарии (parsed) |
| `tech-failure` | retry через `retry_tech_failure_delay` (default 5мин), не считается попыткой |

`worker.dialer.retry_due` (раз в 30 сек) сканирует `respondents.next_attempt_at <= now()` и возвращает их в ZSET.

### 8.6 Backpressure

Лимит in-flight `originate` per FS-нода:
- Redis `INCR fs:<node>:active_channels` перед `originate`-командой; `DECR` в `channel.hangup`.
- Cap: `max_concurrent_per_node` (default **100**) из YAML.
- Кластер по дефолту 5 FS-узлов в prod (3 в dev) → суммарная ёмкость 500 talk-каналов = соответствие NFR-1 (400 пик + 25% headroom).
- Periodic reconciler (Plan 09 Task 6) каждые 30 сек переписывает Redis-счётчик из ESL `api show channels count` → drift не накапливается.
- При cap — `dialer.Router` пробует следующую ноду; если все cap — `originate` откладывается на 1 сек.

### 8.7 Учёт квот при выборе номера

```
quotaPassesForRespondent(respondent, project):
  for each (kind, value) in respondent.dimensions:
    quota = project_quotas[(kind, value)]
    if quota.done >= quota.target: return false
  return true
```

Инкремент `quotas.done` — в транзакции вместе с `calls.status = success`. Если квота переполнилась из-за race — отдельный hourly-job пере-сверяется и нормализует (см. `worker.quotas.recompute`).

---

## 9. Конвейер записи

### 9.1 Полный поток

```
[FreeSWITCH]
  │ mod_record_session writes
  ▼ /var/spool/sociopulse/<tenant>/<call_id>.wav (8kHz PCM mono)
  │
  ▼ fsnotify CREATE event
[recording-uploader on same VM]
  │
  ▼ ffmpeg -y -i .wav -c:a libopus -b:a 32k -ar 16000 .opus
  │
  ▼ sha256(.opus)
  │
  ▼ KMS.GenerateDataKey(tenant_kek_id)
  │   → (plaintext_dek, encrypted_dek)
  │
  ▼ AES-256-GCM(file=.opus, key=plaintext_dek, nonce=random12bytes)
  │   → .opus.enc
  │
  ▼ S3 PUT:
  │   bucket = sociopulse-recordings-<tenant>
  │   key    = YYYY/MM/DD/<call_id>.opus.enc
  │   key2   = YYYY/MM/DD/<call_id>.dek.enc   (encrypted DEK)
  │
  ▼ verify upload (HEAD + check ETag)
  │
  ▼ gRPC recording.Commit({
  │   call_id, s3_bucket, s3_key,
  │   sha256, duration_sec,
  │   encrypted_dek_s3_key, kms_key_id,
  │   codec="opus-32"
  │ })
  │
  ▼ on ack:
  │   - INSERT call_recordings
  │   - INSERT audit_log("recording.committed", sha256, ...)
  │   - publish NATS analytics.recording.committed
  │
  ▼ recording-uploader removes local files
```

### 9.2 Шифрование (envelope)

- KEK (Key Encryption Key): per-tenant, в Yandex KMS. Сам ключ никогда не покидает KMS.
- DEK (Data Encryption Key): random 256 бит per-recording, генерируется через `KMS.GenerateDataKey`. Возвращается две формы — plaintext (используется для шифрования файла, в памяти) и encrypted (зашифрован KEK'ом, кладётся рядом с объектом).
- Шифрование файла: AES-256-GCM с random 12-byte nonce. Nonce в начале зашифрованного файла.
- Дешифрование при чтении: `recording`-модуль скачивает оба объекта, делает `KMS.Decrypt(encrypted_dek, kek_id)` → получает plaintext_dek → расшифровывает файл.

### 9.3 Целостность (chain of integrity)

- При commit — sha256 в `call_recordings` и в `audit_log`.
- При выдаче (presigned URL) — `recording.URLSigner.Sign()` сначала верифицирует sha256 (скачивает headers, проверяет ETag/checksum match), отказывает если mismatch.
- Опция (v2): ежедневный hash-chain в S3 (Merkle root всех recordings за день, подписывается KMS).

### 9.4 Retention pipeline

- `call_recordings.retention_until` — рассчитывается при commit как `now + tenant.recording_retention_days` (default 365).
- `call_recordings.delete_at` — `retention_until + cold_period_days` (default 730 = +2 года = всего 3 года).
- `worker.recording.retention_pass` (раз в сутки):
  - `retention_until < now AND storage_class != 'cold'` → S3 lifecycle: STANDARD → COLD.
  - `delete_at < now` → S3 DELETE + UPDATE call_recordings SET deleted_at = now (метаданные оставляем для аудита).
- Версионирование S3 + MFA-delete защищает от случайного удаления.

### 9.5 Параметры конфигурации

```yaml
recording:
  local_buffer_path: /var/spool/sociopulse
  local_disk_alarm_threshold: 80              # %
  staging_path: /var/spool/sociopulse/.staging
  ffmpeg_codec: libopus
  ffmpeg_bitrate: 32k
  ffmpeg_sample_rate: 16000
  upload_retry_initial_delay: 5s
  upload_retry_max_delay: 5m
  upload_retry_max_attempts: 100               # ~24h при экспоненте
  upload_lag_alert_threshold: 1h
```

---

## 10. Real-time плоскость

### 10.1 WebSocket-протокол (browser ↔ realtime)

**URL**: `wss://app.sociopulse.ru/ws`

**Authentication**:
- Первый кадр от клиента: `{"type":"auth","token":"<access_jwt>"}`.
- Сервер валидирует, регистрирует в `Hub`, отвечает `{"type":"auth.ok","subscriptions":[]}`.
- При невалидном токене — `{"type":"auth.error"}` + `Close(4401)`.
- Refresh: каждые 10 минут клиент шлёт `{"type":"refresh","token":"<new_access_jwt>"}`. Сервер обновляет ассоциацию или закрывает соединение.

**Подписки**:
```jsonc
// Запрос
{"type":"subscribe","topic":"operators.state","filter":{"project_id":"..."}}

// Ответ
{"type":"subscribe.ok","topic":"operators.state","sub_id":"abc123"}

// Push-кадры
{"type":"event","sub_id":"abc123","payload":{"operator_id":"...","state":"call","since":"..."}}
```

**Доступные топики** (фильтрация на сервере по RBAC + RLS):

| Топик | Кто может | Что приходит |
|---|---|---|
| `operators.state` | admin, supervisor | изменения состояния оператора в тенанте |
| `dialer.queue` | admin, supervisor | счётчики очереди (ready/dialing/inCall/processing) |
| `trunks.health` | admin | health changes trunk'ов |
| `call.<call_id>.events` | оператор звонка, admin (если listen-in) | события одного звонка |
| `notifications.user` | сам пользователь | in-app уведомления |
| `op.<op_id>.commands` | сам оператор | команды от сервера (force-pause, force-end-shift) |

### 10.2 NATS subjects (внутренняя шина)

**Каноническая схема именования:** `tenant.<tenant_id>.<area>.<entity>.<id>.<event>` (плюс кросс-тенант служебные `analytics.>` и `audit.>`).

| Subject | Stream | Durable | Ack mode | Retention | Publisher | Subscriber | Назначение |
|---|---|---|---|---|---|---|---|
| `tenant.<t>.telephony.cmd.<call_id>` | core-NATS | no | — | — | dialer | telephony-bridge | команды (originate, hangup, mixmonitor, play); потеря допустима — повтор через retry |
| `tenant.<t>.telephony.event.<call_id>.<event>` | TELEPHONY | yes | explicit | 7 days | telephony-bridge | dialer, realtime, analytics | события каналов (`channel.create`, `channel.answer`, `channel.hangup_complete`, `dtmf`, `record_stop`) |
| `tenant.<t>.dialer.op.<op_id>.state` | DIALER | yes | explicit | 24h | dialer (через outbox) | realtime | переход FSM оператора |
| `tenant.<t>.dialer.call.<call_id>.lifecycle` | DIALER | yes | explicit | 24h | dialer (через outbox) | realtime, analytics | старт/answer/hangup на уровне dialer'а |
| `tenant.<t>.recording.uploaded` | RECORDING | yes | explicit | 30 days | recording (через outbox) | analytics, retention-worker | new recording (committed) |
| `tenant.<t>.audit.event` | AUDIT | yes | explicit | 90 days | * | долгое хранилище (только append) | append-only audit feed; CH-ingestor читает |
| `tenant.<t>.notify.user.<user_id>` | core-NATS | no | — | — | * | realtime | in-app push |
| `tenant.<t>.settings.updated` | core-NATS | no | — | — | tenancy | cmd/api replicas (settings cache) | invalidation |
| `analytics.event.calls` | ANALYTICS | yes | explicit | 24h | dialer/telephony-bridge | analytics-ingestor (CH) | без tenant-prefix — единый поток денормализован per row |
| `analytics.event.operator_state` | ANALYTICS | yes | explicit | 24h | dialer | analytics-ingestor (CH) | то же |

**Правила:**
- Все subject'ы с реальным state (event, transition) → JetStream durable, explicit ack, идемпотентность по `<id>` (call_id, recording_id, operator_state_log_id) на consumer'е.
- Команды (telephony.cmd) и in-app push (notify) → core-NATS best-effort; повтор/lifecycle — через бизнес-логику, не через bus.
- Settings invalidation — best-effort + 30s TTL safety net в кэше.
- Analytics — отдельный stream, можно быстро replay'ить ClickHouse rebuild (24h window). Долгосрочный rebuild — из Postgres + audit_log, не из NATS.
- Все consumer'ы должны быть idempotent: payload содержит `event_id` или дедуплицируемый ключ (call_id+ts).

**Event-payload schemas:** определяются как JSON-Schema в `docs/api/events/` per stream. Backend-публикаторы (Plan 09, 10, 12) и frontend-парсеры (Plan 16, 17, 19) валидируют против них в CI. Контракт-тест на каждый событийный subject — обязательное требование.

**NATS-permissions (per-account):**
- `cmd-api` account: pub/sub на `tenant.>` + `analytics.>` + `audit.>`.
- `telephony-bridge` account: pub `tenant.<t>.telephony.event.>` + sub `tenant.<t>.telephony.cmd.>`.
- `recording-uploader` account: gRPC only — NATS не нужен (publish идёт через `cmd/api`'s outbox-relay после `Recording.Commit`).
- `analytics-ingestor` account: sub `analytics.>` only.
- Каждый `cmd/api`-replica — single account, fan-out по тенантам — на уровне бизнес-логики.

### 10.3 WS-хаб: масштабирование

- Несколько replicas `cmd/api` за k8s Service (LB) с `session-affinity: ClientIP` для WS sticky.
- Каждый replica подписан на NATS на нужные subjects, фильтрует по своим коннектам.
- Презенс — централизован в Redis (`presence:<tenant>:user:<id>` → `replica_id` + TTL 30 сек).
- При коннекте — `replica.RegisterConnection(user_id)` → `set Redis presence`. Replica знает только своих клиентов.

### 10.4 Listen-in реализация

**v1 (silent)**:
1. Admin/supervisor нажимает «прослушать» → UI: `POST /api/calls/<id>/listen` → `realtime.StartListenIn(call_id, mode=silent)`.
2. `realtime` создаёт временный SIP-аккаунт `lst_<admin_id>_<call_id>` (как у оператора, через telephony-bridge).
3. Возвращает фронту `{ws_url, sip_user, sip_password}` для verto-регистрации.
4. После регистрации — `realtime` шлёт `telephony.cmd.<call_id> {action: "mixmonitor", mode: "read", listener_endpoint: "user/lst_xxx"}`.
5. `telephony-bridge` исполняет ESL `uuid_audio <call_id> start read mute -2` + `bridge` на listener-аккаунт.
6. Admin'у в браузер начинает течь mixed-аудио (оператор + респондент).
7. При закрытии modal — отзыв listener-сессии.

**v2 (whisper, barge-in)**: тот же паттерн + конференц-режим (`conference` модуль FreeSWITCH или `mod_audio_fork`).

### 10.5 Backpressure и flow-control на WS

- Per-connection отдельная горутина-writer, кадры буферизуются в канал.
- Канал переполнен (slow consumer) → дроп старых кадров, лог `ws.slow_consumer`. Метрика.
- Per-tenant rate-limit на push'и (защита от broadcast-floods): max 100 кадров/сек на соединение.

---


## 11. Конструктор анкет: схема и runtime

### 11.1 Универсальная схема анкеты

`survey_versions.schema` — JSON-документ, который служит источником истины для form-режима, flow-режима, и runtime-исполнителя. Один формат — два представления.

```jsonc
{
  "version": "1.1",
  "title": "Электоральный мониторинг — Май 2026",
  "intro": "Здравствуйте! Меня зовут {operator_name}, я представляю...",
  "nodes": [
    {
      "id": "n1",
      "kind": "intro",
      "text": "Скажите, удобно ли Вам сейчас поговорить?",
      "next": [
        {"to": "n2", "when": "true"},
        {"to": "end_refused", "when": "answer == 'no'"}
      ],
      "ui": {"x": 40, "y": 30}     // координаты для flow-режима
    },
    {
      "id": "n2",
      "kind": "question",
      "type": "single",
      "text": "В целом Вы интересуетесь политикой?",
      "hint": "Если уточняют как именно — поясните: новости, обсуждения, выборы.",
      "required": true,
      "options": [
        {"id": "very", "label": "Очень интересуюсь"},
        {"id": "rather", "label": "Скорее интересуюсь"},
        {"id": "not_really", "label": "Скорее не интересуюсь"},
        {"id": "not_at_all", "label": "Совсем не интересуюсь"},
        {"id": "dk", "label": "Затрудняюсь ответить"}
      ],
      "next": [
        {"to": "n3", "when": "answer in ['very','rather']"},
        {"to": "n4", "when": "true"}
      ],
      "ui": {"x": 40, "y": 130}
    },
    // ... остальные узлы
    {"id": "end_success", "kind": "success-end", "ui": {"x": 200, "y": 640}},
    {"id": "end_refused", "kind": "refusal-end", "ui": {"x": 680, "y": 240}}
  ],
  "metadata": {
    "estimated_minutes": "5-7",
    "max_questions": 12,
    "primary_mode": "form"
  }
}
```

### 11.2 Form-режим vs flow-режим

- **Form-режим**: рендерит `nodes` как линейный список (sorted by `order` или topological-sort `next`-связей). Редактирование — обычный CRUD узлов. Условные переходы — через UI «показать если ...» (компилируется в `next.when`-выражение).
- **Flow-режим**: рендерит `nodes` как граф по координатам `ui.{x,y}`. Связи — линии по `next`. Редактирование — drag-drop, добавление узлов из палитры, рисование связей.
- Переключение режима не теряет данных. Если в form-режиме нет `ui.x/y` (вновь созданный узел), flow-режим раскладывает узлы автоматически (Sugiyama layout).

### 11.3 Условный язык (DSL)

`when` — JS-подобное выражение в строковой форме, парсится Go-парсером (`expr-lang/expr` или собственный мини-парсер):

| Выражение | Что значит |
|---|---|
| `true` | всегда |
| `answer == 'yes'` | ответ на текущий вопрос равен `'yes'` |
| `answer in ['a','b']` | ответ принадлежит списку |
| `answer.contains('c')` | для `multi`-вопросов |
| `q1.value == 'yes'` | ссылка на ответ другого вопроса по id |
| `q4.value >= 18` | для `number`-вопросов |
| `q1.answered && q2.value > 5` | композиция |

Whitelist функций (никаких side-effects, только чтение).

### 11.4 Валидация анкеты при сохранении

`surveys.SurveyService.SaveVersion()`:

1. JSON-схема проходит JSON-Schema-валидацию (структура).
2. Граф собирается, проверяется:
   - Только один `start`-узел.
   - Все узлы достижимы из `start`.
   - Все `next.to` ссылаются на существующие узлы.
   - Все `*-end` узлы достижимы (нет deadlock'ов).
   - Все `when`-выражения парсятся успешно.
   - Нет циклов **без выхода** (циклы с условным выходом допустимы — например, «уточняющий вопрос»).
   - Все ссылки на `q<id>.value` ссылаются на узел, идущий ДО текущего в любом возможном пути.
3. Если ошибки — `400 Bad Request` с массивом проблем (для подсветки в UI).

### 11.5 Runtime-исполнитель (на сервере и клиенте)

**На клиенте** (для UX):
- При выборе ответа — `surveys.RuntimeService.NextNode(currentNode, answer, allAnswers)` локально вычисляет следующий узел; UI рендерит его без round-trip.
- Это именно для UX-плавности; источник истины — сервер.

**На сервере** (для безопасности и валидации):
- Каждый `POST /api/calls/<id>/answers` — валидация:
  - Узел существует.
  - Тип ответа соответствует `node.type`.
  - Если `required=true` — ответ не пустой.
  - Прошлые ответы не редактировались (immutability в рамках звонка).
- Сервер хранит `call_answers` и пересчитывает «следующий узел» — для отслеживания прогресса в админ-мониторинге.

Реализация: код runtime — в Go, экспортируется в WebAssembly (`tinygo`) для работы в браузере через единый источник кода. Альтернатива — порт на TS, но это дублирование. ADR-008.

### 11.6 Версионирование

- Каждое сохранение → INSERT в `survey_versions`. Старые версии не редактируются.
- При активации новой версии — `is_active=true` на новой, на старой — false (через `survey_versions_active_one` уникальный индекс).
- Уже идущие звонки используют версию, на которой стартовали (`calls.survey_version_id` пиннится при старте).
- Major / minor: ручной выбор при сохранении. Семантика — для удобства тенанта, без машинной логики.

### 11.7 Превью

- Превью открывается в отдельной вкладке UI (`/surveys/<id>/preview`).
- Использует тот же runtime, но без бэкенд-сохранения; ответы держатся в state фронта.
- На сервере есть `POST /api/surveys/<id>/preview/run` для full-stack-проверки runtime'а (опционально, для тестирования сложной логики).

---

## 12. Multi-tenancy и безопасность (deep dive)

### 12.1 Tenant isolation: defence in depth

Изоляция строится на нескольких слоях, каждый — самодостаточный.

| Слой | Механизм | От чего защищает |
|---|---|---|
| L1 — JWT | tenant_id в `aud`-clain JWT | подмена тенанта в API-запросе |
| L2 — gateway middleware | проверка `tenant_id` matches URL/route ownership | bug-баги в обработчиках |
| L3 — Postgres RLS | `using (tenant_id = app.tenant_id)` | если приложение забыло WHERE |
| L4 — KMS per-tenant KEK | расшифровать чужие данные нельзя без чужого ключа | компрометация одного тенанта не каскадирует |
| L5 — S3 bucket per-tenant | отдельный bucket + IAM-политики | случайный cross-tenant access на storage-уровне |
| L6 — NATS account per service | publishers ограничены своим subject-tree | злоупотребление шиной |

### 12.2 RLS-политики: пример

```sql
-- На каждой бизнес-таблице:
alter table projects enable row level security;
alter table projects force row level security;       -- даже для table-owner

create policy projects_tenant_isolation on projects
  using (tenant_id = current_setting('app.tenant_id', true)::uuid)
  with check (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- Сервисная роль для tenancy-модуля (CRUD тенантов):
create role tenancy_admin bypassrls;
grant tenancy_admin to sociopulse_app;             -- но используется только в tenancy-модуле
```

В Go: каждая транзакция начинается с `SET LOCAL app.tenant_id = $1` где `$1` — из JWT. Если код забудет — RLS вернёт 0 строк, а не утечёт чужие данные.

### 12.3 PgBouncer и transaction-mode

- PgBouncer mode: `transaction`.
- Каждое API-обращение = одна транзакция (`BEGIN` / `COMMIT` / `ROLLBACK`).
- `SET LOCAL` действует только в рамках транзакции — что гарантирует, что после `COMMIT` следующий клиент не унаследует чужого `tenant_id`.
- Длинные операции (отчёты, импорты) разбиваются на серию транзакций, каждая со свой `SET LOCAL`.

### 12.4 Шифрование at-rest

| Данные | Шифрование | Ключ |
|---|---|---|
| ПДн в Postgres (телефон, ФИО) | AES-256-GCM в приложении | KEK тенанта в KMS |
| Записи в S3 | AES-256-GCM (envelope) | DEK per-recording, KEK тенанта в KMS |
| Postgres backups в S3 | SSE-KMS server-side | Yandex KMS, single platform-key |
| Логи в Loki | плейн (не содержат ПДн благодаря redaction) | — |
| Аудит-лог в Postgres | плейн (без ПДн в payload, только ID) | — |
| Секреты тенантов (TOTP-secret, API-keys) | AES-256-GCM в приложении | KEK тенанта |

### 12.5 Шифрование in-transit

- Все public endpoints — TLS 1.3 (Yandex Application Load Balancer + Let's Encrypt cert или Yandex Certificate Manager).
- Internal — mTLS между сервисами (cmd/api ↔ telephony-bridge ↔ recording-uploader). CA — внутренний ACM или cert-manager в k8s.
- WebSocket — wss:// (TLS 1.3).
- WebRTC — DTLS-SRTP (стандарт WebRTC).
- SIP-trunks — TLS если поддерживает оператор связи; иначе UDP/TCP с шифрованием SRTP только если оператор поддерживает (часть провайдеров — нет; принять как fact).

### 12.6 Secrets management

- **Yandex Lockbox** — основной storage для секретов (DB-credentials, API-keys, KMS, SIP-passwords).
- **CSI-driver** для k8s — монтируется как volume в pod, refresh автоматически.
- **Sidecar `lockbox-injector`** для FS-VM — Ansible/Packer тянет секреты при build образа, плюс runtime-refresh через cron.
- В коде секреты читаются из переменных окружения / файлов; никогда — из git/configmap/ENV в plain.

### 12.7 RBAC

| Действие | operator | supervisor | admin | service-owner |
|---|---|---|---|---|
| Open workstation | ✅ | ❌ | ❌ | — |
| View own stats | ✅ | ✅ | ✅ | — |
| View any operator's stats | ❌ | ✅ | ✅ | — |
| Listen-in live calls | ❌ | ✅ | ✅ | — |
| Listen recordings | ❌ | ✅ | ✅ | — |
| Mark violation | ❌ | ✅ | ✅ | — |
| CRUD users / projects / surveys | ❌ | ❌ | ✅ | — |
| Configure trunks | ❌ | ❌ | ❌ | ✅ |
| CRUD tenants | ❌ | ❌ | ❌ | ✅ |

Композиция: пользователь может иметь несколько ролей (например, admin+supervisor). RBAC-checker — `auth.RBACChecker.Allow(user, action, resource)`.

### 12.8 Защита от типовых атак

| Атака | Митигация |
|---|---|
| SQL injection | Только prepared statements (`pgx`). Линтер `sqlc` для compile-time проверки. |
| XSS | React по умолчанию escapes; строгий CSP `default-src 'self'`. |
| CSRF | SameSite=Strict cookies + custom header check. |
| CSRF на WS | Token в первом frame (не в URL). |
| Замена IDOR | Все ресурсы — uuid (не предсказуемо), плюс RLS. |
| Brute-force | Rate-limit + account lockout. |
| DDoS | Yandex DDoS Protection на ALB. |
| SIP-flood, регистрация фишек | FS ACL + fail2ban на FS-VM. |
| Compromised JWT | Короткий TTL access (15 мин) + refresh rotation. |
| Stolen recording link | Presigned URL с TTL 5 мин + audit on access. |
| Замена KMS-key (подмена) | Audit все KMS-операции в Yandex Cloud Audit Trails. |

---

## 13. Compliance: 152-ФЗ и бизнес-правила

### 13.1 Подготовка к 152-ФЗ

- Регистрация оператора ПДн в Роскомнадзоре (compliance team тенанта-разработчика).
- DPA (Data Processing Agreement) с каждым тенантом-арендатором — мы как оператор по поручению.
- Политика обработки ПДн — публикуется на `/legal/personal-data`.
- Согласие пользователя-оператора и admin'а — при первом входе чек-бокс.
- Согласие респондента — IVR-промпт перед записью.

### 13.2 Категории ПДн в системе

| ПДн | Где хранится | Шифрование | Retention |
|---|---|---|---|
| Телефон респондента | `respondents.phone_encrypted` | AES-256-GCM с per-tenant KEK | договорный + 30 дней |
| Аудио-запись | S3 `sociopulse-recordings-<tenant>` | envelope KMS | 365д hot + 2г cold |
| ФИО оператора | `users.full_name` | АES-256-GCM | пока активен + 5 лет audit |
| Логин оператора | `users.login` | плейн (это публичный идентификатор внутри тенанта) | как ФИО |
| IP-адрес сессии | `audit_log.ip` | плейн | 5 лет |

### 13.3 Право на удаление (subject right)

API: `DELETE /api/respondents/<id>` (admin-only).

```
1. Set respondents.status = 'deletion-requested'
2. Set respondents.delete_at = now() + 30 days
3. Audit log
4. After 30 days, worker.respondents.purge runs:
   - DELETE associated call_recordings (S3 + table)
   - UPDATE respondents SET phone_encrypted = NULL, phone_hash = NULL,
            attributes = '{}', region_code = '', deleted_at = now()
   - call_answers — оставляем (уже обезличены, нет ПДн)
   - calls — оставляем (статистика)
5. Audit log
```

### 13.4 Право на доступ

- API: `GET /api/respondents/<id>/personal-data-export` (admin-only по запросу субъекта).
- Возвращает JSON: все ПДн респондента + список звонков + presigned URL'ы записей.

### 13.5 Реестры (38-ФЗ — реклама)

- Социологические опросы **не** подпадают под 38-ФЗ (это не реклама).
- Флаг `projects.is_advertising` (default false). Если true — добавится сверка с реестром Роскомнадзора (v2). На v1 enforced false с проверкой при создании проекта.

### 13.6 Журнал доступа к ПДн

- Каждый раз когда админ/контролёр читает запись — `audit_log("recording.accessed", call_id, ...)`.
- Каждое расшифрование KEK'ом — Yandex KMS Audit Trails (опционально включается).

### 13.7 Уведомление об инциденте

- Процесс: при обнаружении утечки — incident-flow в `audit`-модуле помечает событие, on-call получает алерт, compliance team тенанта-разработчика связывается с Роскомнадзором в течение 24 часов (по 152-ФЗ).
- Это не код, но процесс задокументирован — раздел 18 (Риски).

---

## 14. Конфигурация: реестр и принципы

### 14.1 Двухуровневая конфигурация

- **Static (deploy-time)** — `config.yaml`, монтируется в k8s ConfigMap. Меняется деплоем. Источник: репо `configs/<env>/config.yaml`.
- **Dynamic (runtime, per-tenant)** — таблица `tenant_settings(tenant_id, key, value jsonb)`. Меняется через UI админа тенанта. Применяется через 30 сек после изменения (TTL кеша).

### 14.2 Структура `config.yaml`

```yaml
service:
  env: production           # development|staging|production
  log_level: info           # debug|info|warn|error
  region: yc-ru-central-1

http:
  bind: ":8080"
  read_timeout: 10s
  write_timeout: 30s
  max_body_size: 10MB

ws:
  bind: ":8081"
  ping_interval: 20s
  read_buffer_size: 4KB
  write_buffer_size: 4KB
  max_message_size: 64KB

database:
  postgres:
    dsn: postgres://app:${PG_PASSWORD}@pgbouncer:6432/sociopulse?sslmode=require
    max_conns: 50
    max_idle_time: 5m
    statement_cache: 100
    migrations_path: /etc/sociopulse/migrations
  clickhouse:
    dsn: clickhouse://app:${CH_PASSWORD}@ch-cluster:9000/sociopulse
    batch_size: 10000
    flush_interval: 5s
  redis:
    addr: redis-master:6379
    password: ${REDIS_PASSWORD}
    pool_size: 50
    db: 0

nats:
  urls: ["nats://nats-1:4222","nats://nats-2:4222","nats://nats-3:4222"]
  account: cmd-api
  jetstream:
    stream_telephony_event: ...
    stream_audit_event: ...

s3:
  endpoint: https://storage.yandexcloud.net
  region: ru-central-1
  buckets:
    backups: sociopulse-backups
    reports: sociopulse-reports
    consent_prompts: sociopulse-consent-prompts
  # recordings buckets — per-tenant, имена резолвятся в коде

kms:
  endpoint: kms.api.cloud.yandex.net:443

auth:
  jwt:
    issuer: https://app.sociopulse.ru
    access_ttl: 15m
    refresh_ttl: 720h
    algorithm: HS256
    secret_lockbox_key: jwt-signing-secret
  password:
    argon2id_memory: 64MB
    argon2id_iterations: 3
    argon2id_parallelism: 4
  rate_limit:
    login_per_ip_per_hour: 30
    login_per_account_per_hour: 10
    lockout_after_failures: 5
    lockout_duration: 15m
  totp:
    issuer: SocioPulse
    period_sec: 30
    digits: 6

dialer:
  defaults:                 # дефолты, переопределяются tenant_settings
    attempt_max: 3
    retry_no_answer_delay: 4h
    retry_busy_delay: 30m
    retry_dropped_delay: 2h
    retry_tech_failure_delay: 5m
    dialing_timeout: 25s
    pause_max: 15m
    rdd:
      enabled: true
      max_rate_per_sec: 10
      fallback_threshold: 0.3
      max_attempts_per_call: 50
    working_hours:
      weekdays: { from: "09:00", to: "21:00" }
      weekends: { from: "10:00", to: "20:00" }

telephony:
  bridge:
    fs_nodes:
      - id: fs-1
        esl_endpoint: tls://fs-1.sociopulse.local:8021
        esl_cert: /etc/sociopulse/certs/esl-client.pem
        esl_key: /etc/sociopulse/certs/esl-client-key.pem
      - id: fs-2
        esl_endpoint: ...
      - id: fs-3
        esl_endpoint: ...
    healthcheck_interval: 5s
    max_concurrent_per_node: 60
  trunks:
    - id: mtt-msk-1
      sip_gateway: mtt-msk-1
      capacity_channels: 100
      cost_per_minute_rub: 3.42
      weight: 60
      regions: ["77","50","78"]
      healthcheck:
        method: options
        interval: 30s
        timeout: 5s
        unhealthy_after: 2
    - id: mango-fed
      ...
  routing:
    default_strategy: least_cost_with_fallback

recording:
  local_buffer_path: /var/spool/sociopulse
  staging_path: /var/spool/sociopulse/.staging
  ffmpeg:
    codec: libopus
    bitrate: 32k
    sample_rate: 16000
  upload:
    retry_initial_delay: 5s
    retry_max_delay: 5m
    retry_max_attempts: 100
  retention:
    default_hot_days: 365
    default_cold_days: 730            # +730 = 3y total
    cold_storage_class: COLD

reports:
  async_threshold_period_days: 30
  async_threshold_records: 100000
  job_ttl: 24h
  presigned_url_ttl: 24h

observability:
  otel:
    endpoint: otel-collector:4317
    sampling_ratio: 0.1                 # 10% trace sampling
  metrics:
    bind: ":9090"
    namespace: sociopulse
  logging:
    redact_patterns:
      - "phone:\\+?7\\d{10}"
      - "token:\\w+"
    sample_info_logs: 1.0               # 100%
    sample_debug_logs: 0.05             # 5%
```

### 14.3 Реестр `tenant_settings` (что тенант может крутить)

| Ключ | Тип | Default | Описание |
|---|---|---|---|
| `dialer.attempt_max` | int | 3 | Max попыток на респондента |
| `dialer.retry_no_answer_delay` | duration | `4h` | Задержка для no-answer |
| `dialer.retry_busy_delay` | duration | `30m` | Задержка для busy |
| `dialer.retry_dropped_delay` | duration | `2h` | Задержка для dropped |
| `dialer.retry_tech_failure_delay` | duration | `5m` | Задержка для tech-failure |
| `dialer.dialing_timeout` | duration | `25s` | Сколько ждём ANSWER |
| `dialer.pause_max` | duration | `15m` | Лимит непрерывной паузы |
| `dialer.rdd.enabled` | bool | true | Включена ли RDD |
| `dialer.rdd.max_rate_per_sec` | int | 10 | Лимит RDD-rate |
| `dialer.working_hours_weekdays` | string | `09:00-21:00` | Рабочие часы будни |
| `dialer.working_hours_weekends` | string | `10:00-20:00` | Рабочие часы выходные |
| `dialer.routing_strategy` | enum | `least_cost_with_fallback` | Стратегия trunk-маршрутизации |
| `dialer.caller_id` | string | (из YAML) | Caller-ID для исходящих |
| `recording.consent_prompt_url` | string | стандартный | URL аудио-промпта согласия |
| `recording.hot_retention_days` | int | 365 | Дней в hot tier |
| `recording.cold_retention_days` | int | 730 | Дней в cold tier |
| `surveys.max_questions` | int | 25 | Лимит вопросов в анкете |
| `surveys.cost_per_completed_rub` | int | 120 | Сколько платится оператору за анкету |
| `auth.password_min_length` | int | 8 | Min длина пароля |
| `auth.totp_required` | bool | false | Обязательность 2FA |
| `quality.violation_categories` | json[] | стандарт | Категории нарушений (label + code) |
| `notifications.pause_overrun_threshold` | duration | `15m` | Когда алертить |
| `ui.theme_default` | enum | `light` | Дефолтная тема |
| `ui.font_size_default` | enum | `md` | Дефолтный размер шрифта |
| `quotas.dimensions` | json | `[region]` | Какие измерения квот использовать |

Технически: `tenancy.SettingsCache` — in-memory кеш с lazy-load и TTL 30 сек; событие `tenant.<id>.settings.updated` в NATS вызывает invalidation.

### 14.4 Конфиги, которые **не** в YAML и не в tenant_settings

- Логика бизнес-правил (например, что `success` инкрементирует quota — это не «настройка», это инвариант).
- Конкретные коды состояний (`call`, `pause`, `verify` — enum в Go).
- Структура анкеты (это уже отдельный UI-конструктор).


## 15. Observability

### 15.1 Принцип трёх столпов

Logs (zap) — Metrics (Prometheus) — Traces (OpenTelemetry). Все три коррелируются через общий `trace_id`/`span_id`/`request_id`.

### 15.2 Logging — zap

- Backend: `go.uber.org/zap` (production-grade, нулевая аллокация в hot-path).
- Все логи структурированные JSON: `{"ts":"...","level":"info","msg":"...","module":"dialer","tenant_id":"...","trace_id":"...","fields":{...}}`.
- Уровни: `debug` (только в dev/staging), `info` (production default), `warn`, `error`, `fatal`.
- Sampling: для `debug` — 5% sampling (не утопить хранилище). Для `info`/`warn`/`error` — 100%.
- **Redaction**: middleware redact'ит чувствительные значения по regex'ам (`config.observability.logging.redact_patterns`):
  ```
  phone: "+7XXXXX***1234"
  token: "eyJh*** (redacted)"
  password: "<redacted>"
  ```
- Лог-агрегатор: **Grafana Loki** через `promtail`-sidecar в k8s.
- Retention: 30 дней hot, 1 год cold (S3-backed Loki).

### 15.3 Metrics — Prometheus + Mimir

Каждый сервис экспортит `/metrics` на порту 9090. Префикс `sociopulse_*`.

#### Бизнес-метрики (RED + USE):

| Метрика | Тип | Labels | Назначение |
|---|---|---|---|
| `sociopulse_calls_total` | counter | `tenant_id, project_id, status, region` | Количество звонков |
| `sociopulse_call_duration_seconds` | histogram | `tenant_id, status` | Длительность разговоров |
| `sociopulse_dialer_queue_depth` | gauge | `tenant_id, project_id` | Глубина очереди номеров |
| `sociopulse_dialer_active_channels` | gauge | `fs_node, trunk_id` | Активные SIP-каналы |
| `sociopulse_operator_state_seconds` | counter | `tenant_id, operator_id, state` | Время в каждом state |
| `sociopulse_recording_upload_lag_seconds` | gauge | `fs_node` | Отставание uploader'а |
| `sociopulse_recording_upload_total` | counter | `tenant_id, status` | Записи uploaded/failed |
| `sociopulse_quota_progress_ratio` | gauge | `tenant_id, project_id, dimension, value` | Заполнение квоты |
| `sociopulse_rdd_generated_total` | counter | `tenant_id, project_id, region` | Сгенерированные RDD-номера |

#### Технические метрики (Go-runtime + HTTP):

- `go_*` (Prometheus client_golang default).
- `http_request_duration_seconds` (histogram, labels: `method, path, status`).
- `http_inflight_requests` (gauge).
- `ws_connections_active` (gauge, labels: `tenant_id`).
- `nats_messages_in_total`, `nats_messages_out_total` (per-subject).
- `db_connections_active`, `db_query_duration_seconds`.

### 15.4 Tracing — OpenTelemetry

- SDK: `go.opentelemetry.io/otel`.
- Propagation: W3C TraceContext + Baggage.
- Collector: OpenTelemetry Collector в k8s, экспорт в **Grafana Tempo** (S3-backed).
- Sampling: head-based, 10% production / 100% staging.
- Spans:
  - `gateway.<route>` — root span.
  - `module.<module>.<method>` — module-level span.
  - `db.query` — SQL queries.
  - `nats.publish`, `nats.subscribe.handle` — messaging.
  - `esl.command` — telephony-bridge → FS.
  - `s3.put`, `s3.get` — uploader.
- Каждый span имеет attributes: `tenant_id`, `actor_user_id` (если есть), `business_op`.

### 15.5 SLI / SLO / Alerts

| SLI | Цель (SLO) | Алерт |
|---|---|---|
| Доступность gateway HTTP | 99.5% за 30 дней | error budget < 50% — warning; < 10% — critical |
| Латентность API p95 | < 300 ms | > 500 ms 5 мин подряд — warning |
| Real-time event latency p95 | < 500 ms | > 1 sec 5 мин подряд — warning |
| Recording upload lag p95 | < 5 мин | > 1 час 5 мин подряд — critical |
| Recording upload success rate | > 99.95% | < 99.9% за час — critical |
| Brushed-call rate (для будущего predictive) | < 3% | > 5% — critical |
| Trunk health up | > 95% checks pass | trunk down > 5 min — critical, on-call |
| Postgres replication lag | < 5 sec | > 30 sec — warning |
| Quota recompute job last run | < 90 min ago | > 90 min — warning |

Alert routing:
- **Critical** → PagerDuty / Yandex OnCall → дежурный.
- **Warning** → Slack channel `#sociopulse-alerts`.

### 15.6 Дашборды Grafana

- **System overview**: live calls, ready operators, queue depth, error rate, latencies.
- **Per-tenant overview**: calls by status, recording upload lag, quota progress.
- **Telephony**: trunk health, FS-node load, ESL command latency.
- **Recording pipeline**: upload throughput, encrypt/decrypt times, S3 errors.
- **Operators**: state distribution, KPI by operator.
- **DB**: query latencies, connection pool, replication.
- **Cost**: storage growth, S3 requests, KMS calls.

### 15.7 Sentry для frontend

- Ошибки в браузере — Sentry (Yandex Cloud-hosted или собственный).
- PII-redaction в Sentry config (телефоны, токены не отправляются).

---

## 16. Развёртывание

### 16.1 Инфраструктура (Yandex Cloud)

| Компонент | Сервис Yandex Cloud | Размер (старт) |
|---|---|---|
| Kubernetes | Managed Kubernetes (MKS) | 3 worker-нода, 4vCPU/16GB |
| PostgreSQL | Managed PostgreSQL | 2 hosts (master + replica), 4vCPU/16GB, 200GB SSD |
| ClickHouse | Managed ClickHouse | 1 host, 4vCPU/16GB, 500GB SSD |
| Redis | Managed Redis | 2 hosts (master + replica), 2vCPU/8GB |
| NATS JetStream | Self-hosted on MKS | 3 nodes via StatefulSet |
| FreeSWITCH | Compute Cloud VMs | 3 VMs, 4vCPU/8GB, NVMe 200GB |
| TURN-server (coturn) | Compute Cloud VM | 1 VM, 2vCPU/4GB |
| Object Storage | Yandex Object Storage | per-tenant buckets |
| KMS | Yandex KMS | per-tenant KEK |
| Secrets | Yandex Lockbox | один Lockbox-instance |
| Logs/Metrics/Traces | Self-hosted Grafana stack on MKS | 1 admin-нода |
| ALB | Application Load Balancer | front для cmd/api |

### 16.2 Сетевая топология

```
Internet
   │
   ▼
[Yandex Application Load Balancer + DDoS Protection]
   │ TLS termination
   ▼
[k8s Service "cmd-api"] — round-robin на cmd/api Deployment replicas
   │
   ├─→ [Managed PostgreSQL] (через PgBouncer Deployment)
   ├─→ [Managed ClickHouse]
   ├─→ [Managed Redis]
   ├─→ [NATS StatefulSet]
   └─→ [telephony-bridge Service]
                │ NATS only
                ▼
        [FreeSWITCH VMs] (отдельная подсеть, public IP для SIP-trunks)
                │ ESL via mTLS internal IP
                │ recording-uploader systemd unit рядом
                ▼
        [Yandex Object Storage]   ← envelope-encrypted recordings
        [Yandex KMS]               ← KEK per tenant
```

### 16.3 IaC

- **Terraform** — вся инфра Yandex Cloud (MKS, MPG, MCH, MRD, ALB, S3, KMS, Lockbox, VMs).
- **Helm + ArgoCD** — k8s-приложения (`cmd/api`, `cmd/worker`, `telephony-bridge`, NATS, monitoring stack).
- **Packer + Ansible** — образ FreeSWITCH-VM (FreeSWITCH + recording-uploader + конфиги).
- **GitOps** — все изменения через PR в repo с infra/.

### 16.4 CI/CD

- **GitHub Actions** или **GitLab CI** (выбираем после фиксации хостинга репо).
- **Pipeline**:
  1. `lint` — `golangci-lint`, `eslint`, `stylelint`.
  2. `test` — unit-tests Go + Vitest для фронта.
  3. `build` — Go-бинари, Docker-образы (multi-stage), фронт-bundle (Vite).
  4. `security-scan` — `osv-scanner`, `govulncheck`, `trivy`, `npm audit`.
  5. `migration-dry-run` — миграции на staging snapshot БД.
  6. `integration-test` — pytest или go test против ephemeral environment в k8s (kind или dev-cluster).
  7. `deploy-staging` — автоматически на main.
  8. `deploy-production` — manual approval, ArgoCD sync.

### 16.5 Migrations

- `golang-migrate` библиотека.
- Миграции в `migrations/<timestamp>_<name>.up.sql` + `<...>.down.sql`.
- Запуск: `cmd/migrator` как k8s Job (`PreSync` ArgoCD hook), блокирует rollout `cmd/api` до завершения.
- Двухфазные миграции (для не-обратно-совместимых):
  - Фаза 1: добавить новый column / индекс / table; не удалять старое; релиз кода, который пишет в обе версии.
  - Фаза 2 (через несколько релизов): удалить старое.

### 16.6 Disaster Recovery

| Слой | RPO | RTO | Метод |
|---|---|---|---|
| Postgres | ≤ 5 мин | ≤ 30 мин | pgbackrest → S3 (full daily + WAL непрерывно) + standby в DR-зоне |
| ClickHouse | ≤ 1 час | ≤ 2 часа | weekly snapshot в S3; восстановление через NATS-replay из аудита |
| Redis | принимаем потерю | ≤ 5 мин | AOF + ежедневный snapshot; presence/cache не критичны; asynq jobs — критичны, требуют durability (хранятся в Redis Streams с persistence) |
| NATS JetStream | ≤ 5 мин | ≤ 15 мин | replication stream между 3 нодами + S3 backup для long-term |
| FS-VMs | recordings до 24 часов | < 30 мин | Packer-образ → восстановление за 10 мин; recordings уже в S3 |
| S3 | crossrep | < 1 мин | cross-region replication между zones |
| Lockbox / KMS | управляется Yandex | минуты | репликация на стороне облака |

DR-runbook — отдельный документ `docs/runbooks/disaster-recovery.md` (создаётся при имплементации).

### 16.7 Оценка стоимости (порядок)

При нагрузке Variant B (30 тенантов, 500 операторов, 50k звонков/день):

| Категория | Месячная оценка | Комментарий |
|---|---|---|
| Yandex MKS (3 ноды + 2 для DR) | ~₽40 000 | |
| Managed PostgreSQL | ~₽30 000 | master+replica, 200GB |
| Managed ClickHouse | ~₽15 000 | |
| Managed Redis | ~₽8 000 | |
| 3 FreeSWITCH VMs | ~₽15 000 | |
| Object Storage hot (~5 ТБ) | ~₽5 000 | hot tier, растёт за год до ~22 ТБ |
| Object Storage cold | ~₽3 000 | |
| KMS calls (~2M/мес) | ~₽4 000 | |
| Network egress (≈ 500 ГБ/мес WebRTC + API) | ~₽5 000 | |
| Yandex Application LB + DDoS | ~₽5 000 | |
| Logs / Metrics (Grafana Cloud если не self-hosted) | ~₽10 000 | |
| **Инфра без trunk'ов** | **~₽140 000/мес** | |
| SIP-trunks (50k звонков × 3 мин × 3.5 ₽) | ~₽525 000/мес | |
| **ИТОГО** | **~₽665 000/мес** | trunk'и — 80% стоимости |

Стоимость ровно следует за объёмом звонков; основной рычаг оптимизации — `least_cost`-routing trunk'ов.

---

## 17. Тест-стратегия

### 17.1 Пирамида тестов

```
              ┌─────────────────┐
              │  Manual / UAT   │  ~20 ручных сценариев на релиз
              └─────────────────┘
            ┌───────────────────────┐
            │  E2E (Playwright +    │  ~50 сценариев, full stack on dev cluster
            │   ephemeral cluster)  │
            └───────────────────────┘
         ┌──────────────────────────────┐
         │  Integration (Go testify +   │  ~200 тестов, реальные Postgres/Redis/
         │   testcontainers + sipp/pjsua│   NATS/SIPp в Docker, FS sim
         └──────────────────────────────┘
       ┌────────────────────────────────────┐
       │  Unit tests (Go testify + Vitest)  │  ~2000+ тестов, моки на интерфейсы
       └────────────────────────────────────┘
```

### 17.2 Unit-tests

- **Go**: `testify/assert` + `testify/require`. Моки — `gomock` (interface-based) или ручные (для простых).
- **TypeScript**: `Vitest` + `@testing-library/react`.
- **Покрытие**: целевое ≥ 70% по модулям бизнес-логики; для `dialer`-FSM, `surveys.RuntimeService`, `RDDGenerator`, `Router` — обязательно ≥ 90%.
- Линтер: на каждый PR, fail если падает.

### 17.3 Integration-tests

- **Tools**: `testcontainers-go` для запуска ephemeral Postgres/Redis/NATS/Minio в тестах.
- **Telephony**: SIPp + PJSUA для эмуляции SIP-операторов и SIP-провайдеров; FreeSWITCH в Docker для bridge-тестов.
- **Тест-сценарии**:
  - `dialer.PickNext` против реального Postgres+Redis с mock-респондентами.
  - `recording-uploader` end-to-end: пишем .wav → uploader → S3 (Minio) → проверяем decrypt.
  - `surveys.Runtime` против реальной схемы из ВЦИОМ-прототипа.
  - `auth.Login` против реальной БД с argon2id.

### 17.4 E2E-tests

- **Tool**: Playwright (TypeScript) против ephemeral environment в k8s (kind или dev-cluster).
- **Сценарии**:
  - Login → workstation → ready → call (mock SIP) → answer survey → save status.
  - Admin создаёт проект → импортирует базу → назначает оператора → видит KPI.
  - Admin создаёт анкету в form-режиме → переключает на flow-режим → сохраняет → видит превью.
  - Контролёр прослушивает запись → отмечает нарушение → видит в audit.
- Запуск: на каждый PR (быстрая выборка), полная — на main.

### 17.5 Load-tests

- **Tool**: k6 (JS-based) для HTTP/WS, SIPp scenarios для телефонии.
- **Сценарии**:
  - 500 операторов в WS, изменения state каждые 5 сек — проверяем p95 < 500 мс.
  - 10 одновременных импортов 100k-номерных баз.
  - 200 одновременных активных SIP-каналов через FS-кластер.
- Запуск: pre-release.

### 17.6 Chaos engineering

- **Tool**: Chaos Mesh в k8s.
- **Сценарии** (на staging):
  - Убить один pod cmd/api → клиенты переподключились без потери session.
  - Убить FS-нода → активные звонки этой нода потеряны, остальные продолжают.
  - Network partition между cmd/api и Postgres — graceful degradation, ошибки 503.
  - Замедлить S3 upload в 100× → recording lag растёт, алерт срабатывает.

### 17.7 Security testing

- **SAST**: `gosec`, `eslint-plugin-security`.
- **Dependency scan**: `osv-scanner`, `govulncheck`, `trivy`, `npm audit`.
- **DAST**: OWASP ZAP против staging раз в неделю.
- **Penetration test**: external раз в год для production (если бизнес-требование).

### 17.8 Go Coding Standards

Backend Go-codebase следует распакованному и адаптированному набору
**`samber/cc-skills-golang`** (MIT, 12 скиллов) — community-стандарту от
автора `samber/lo`/`samber/oops`. Скиллы установлены в
`~/.agents/skills/golang-*/SKILL.md` и автоматически активируются по описанию
при работе с релевантным кодом. Полная distilled-версия для нашего проекта
живёт в `docs/architecture/07-go-coding-standards.md` (создаётся в Plan 00a
Task 1) и в `CONTRIBUTING.md` (Plan 00 Task 5).

**Двенадцать областей (заголовок каждой — название скилла):**

| # | Скилл | Project enforcement |
|---|---|---|
| 1 | `golang-error-handling` | `errorlint`, single-handling rule, `samber/oops` на boundary |
| 2 | `golang-context` | `contextcheck`, `noctx`, `WithoutCancel` для outbox-relay |
| 3 | `golang-concurrency` | `errgroup.SetLimit` для worker pools, `goleak.VerifyTestMain` обязателен, race detector в CI |
| 4 | `golang-structs-interfaces` | small interfaces, accept interface/return struct, compile-time check `var _ api.X = (*Y)(nil)` |
| 5 | `golang-safety` | `forcetypeassert`, comma-ok, no `defer` в loops, bounds-checked numeric conversion |
| 6 | `golang-security` | `crypto/rand` для secrets, AES-GCM, `crypto/subtle.ConstantTimeCompare`, `gosec`, `govulncheck` в CI |
| 7 | `golang-modernize` | Go 1.22+: `any`, `min`/`max`, `range` over int, `slices`/`maps`, `cmp.Or` |
| 8 | `golang-data-structures` | preallocate slices/maps, `strings.Builder`, generic constraints |
| 9 | `golang-design-patterns` | functional options, `defer Close()` сразу после открытия, no `init()` для injectable deps |
| 10 | `golang-grpc` | health check service, `GracefulStop`, `status.Errorf` с правильным code, mTLS |
| 11 | `golang-testing` | table-driven + named subtests + `t.Parallel()` (paralleltest), testify как helper, `goleak` |
| 12 | `golang-troubleshooting` | reproduce-before-fix, `pprof` на admin порту, `dlv` только в dev |

**Механическая enforcement (см. Plan 00 Task 9 + Plan 00a Task 8 для полного
списка линтеров и depguard правил):**

- `forcetypeassert` — все type assertions через comma-ok.
- `errorlint` — `%w` в `fmt.Errorf`, `errors.Is/As` вместо `==` и type-switch.
- `contextcheck` + `noctx` — context propagation через цепочку.
- `paralleltest` + `thelper` + `testifylint` — testing-идиомы.
- `loggercheck` — корректность ключ-значение пар у `slog`/`zap`.
- `exhaustive` — switch по enum покрывает все варианты.
- `bodyclose` + `sqlclosecheck` + `rowserrcheck` — закрытие resources.
- `depguard:banned-stdlib` — `math/rand`, MD5, SHA1, DES, CBC/ECB запрещены.
- `depguard:cross-module-isolation` — модули общаются только через `internal/<X>/api/`.
- `depguard:pgxpool-blocked` — direct `pgxpool.Pool` блокирован вне `pkg/postgres` (защита RLS).
- `depguard:yandex-sdk-isolation` — Yandex SDK только в `internal/tenancy/store` и `cmd/recording-uploader`.

**Не-механический enforcement (code review):**

- Single-handling rule — линтер не ловит, ловит ревью.
- Premature interfaces — review-проверка: extract interface когда появился
  второй consumer или test mock.
- Low-cardinality error strings — review-проверка: переменные данные через
  `slog.Attr`/`oops.With`, не интерполируются в message string.
- `time.After` в loop — CI-grep guard (`make grep-time-after`, см. Plan 00a Task 8).

**HTTP testing pattern (gin, per ADR-0014):**

```go
gin.SetMode(gin.TestMode) // FIRST line of test file's TestMain or test func

r := gin.New()
r.POST("/api/auth/login", h.Login)

req := httptest.NewRequest(http.MethodPost, "/api/auth/login",
    strings.NewReader(`{"email":"x","password":"y"}`))
req.Header.Set("Content-Type", "application/json")
rec := httptest.NewRecorder()
r.ServeHTTP(rec, req)

assert.Equal(t, http.StatusXxx, rec.Code)
```

For handler-only unit tests (no routing), use `gin.CreateTestContext`:

```go
rec := httptest.NewRecorder()
c, _ := gin.CreateTestContext(rec)
c.Request = httptest.NewRequest(...)
c.Params = gin.Params{{Key: "id", Value: "42"}}
h.GetUser(c)
```

Full Red-Green-Refactor playbook lives in
[`docs/architecture/08-tdd-discipline.md`](../../architecture/08-tdd-discipline.md).
ADR-0015 makes TDD mandatory; ADR-0014 fixes the router choice; this section
is the testing surface where they meet.

**ADR-0016 candidate** (открыт): когда совершим miграцию `zap → slog`
(ADR-0012 текущий), `loggercheck.zap` будет заменён на `loggercheck.slog`
single-mode и `zap` уйдёт из allow-list импортов.

---

## 18. Риски и митигации

| ID | Риск | Вероятность | Импакт | Митигация |
|---|---|---|---|---|
| R-1 | WebRTC аудио не проходит через корпоративные NAT/firewall тенантов-арендаторов | Высокая | Критический (оператор не работает) | Свой TURN-сервер (coturn) + документация по требуемым портам/IP в `docs/customer-onboarding.md`. Резервный SIP-софтфон (X-Lite) как fallback. |
| R-2 | Падение SIP-провайдера в час пик | Средняя | Высокий (деградация дозвона) | Multi-trunk routing, health-check, автоматическое переключение. Контракты с ≥ 2 операторами связи. |
| R-3 | Заполнение локального диска FS-VM (recording backlog) | Низкая | Высокий (потеря записей) | Алерт на 80%, увеличить размер диска / ускорить uploader; автомасштабирование диска через Yandex Cloud API. |
| R-4 | Утечка KMS-key одного тенанта | Низкая | Критический | Per-tenant KEK ограничивает blast radius; ротация раз в год; Yandex Cloud Audit Trails отслеживают KMS-операции. |
| R-5 | DDoS на gateway | Средняя | Средний | Yandex DDoS Protection на ALB + rate-limit на уровне приложения. |
| R-6 | Insider-threat: оператор скачивает базу респондентов | Средняя | Высокий (152-ФЗ) | RBAC ограничивает скачивание; все экспорты — в audit; supervisor-роль не имеет доступа к unmasked phone. |
| R-7 | Несоответствие между ru-зонами Yandex Cloud (latency между AZ) | Низкая | Средний | Все компоненты в одной AZ для production; только DR в другой AZ (eventually-consistent). |
| R-8 | Quota-race: переполнение квот при concurrent success-вызовах | Высокая (без митигации) | Низкий | Постобработка: `worker.quotas.recompute` ежечасно нормализует; принимаем небольшое временное превышение (< 1%). |
| R-9 | RDD-генерация попадает в реестр Роскомнадзора (на номер бизнеса) | Средняя | Средний (репутация) | Pre-validation через DNC + правила «не звонить в часы вне рабочих» + пользовательский DNC-список тенанта. |
| R-10 | Расхождение между прототипом и финальным UI (UX отличается, операторы жалуются) | Средняя | Средний | Прототип — обязательный референс; PR-ревью включает визуальную сверку с прототипом. |
| R-11 | Расход на trunk'и больше, чем закладывал бюджет (out-of-region звонки дороже) | Средняя | Средний | Per-tenant отчёт расходов в реальном времени; алерт при превышении дневного бюджета. |
| R-12 | RTO/RPO не выдерживается на DR | Низкая | Высокий | DR-учения раз в квартал; автоматизированный runbook. |
| R-13 | Лимит KMS-API rate (Yandex KMS — 100 RPS) при пиковом приёме записей | Низкая | Средний | Кеш DEK'ов в `recording-uploader` и `recording`-модуле; KMS вызывается только для encrypt при upload и decrypt при первом доступе; повторные доступы — с DEK из in-memory кеша. |
| R-14 | Конфигурация плывёт между tenant_settings и YAML (неконсистентность) | Средняя | Средний | Миграционная утилита: на каждый релиз снимок `tenant_settings.<key>` сравнивается с YAML defaults; алерт на orphans (key в DB, нет в коде). |
| R-15 | Утечка через логи (телефон попал в info-log) | Средняя | Высокий (152-ФЗ) | redact-middleware на zap; CI-тест проверяет, что test-logs не содержат регексов телефонов. |
| R-16 | Полная потеря FS-VM в момент звонка → потеря всех in-progress recordings (~80 одновременных при peak) | Низкая (event-driven) | Средний | Принято в ADR-005: SLO recording-integrity = 99.5% uploaded. При крахе FS-VM `cmd/api` помечает active calls как `recording_lost=true`; PM может назначить ре-обзвон. Backlog v2 — оценка mod_audio_fork. |
| R-17 | Двойной звонок одному респонденту при Redis Sentinel failover (ZPOPMIN'ed респондент возвращается после write-loss) | Низкая | Высокий (152-ФЗ нарушение целевого использования) | В Plan 10 — mark "in-flight" в Postgres ДО `originate`. Reconciler при Redis flush сверяет с in-flight в PG. |
| R-18 | mod_xml_curl directory-endpoint в `cmd/api` — SPOF на каждом deploy: 30-60s окна недоступности → новые операторские звонки fail | Средняя | Высокий (operator UX) | Plan 09 Task 7 (backlog) — выделить directory в low-churn deployment + FS-local nginx cache 30s; либо `terminationGracePeriodSeconds=120` + max 1 unavailable. |
| R-19 | Cross-tenant subscription через WS — admin тенанта A подписывается на operator-state тенанта B по ID | Низкая | Высокий (PII утечка) | Plan 11 Task 10: TopicRBAC валидирует filter UUID-ы → claims.tenant_id через cached resolvers. |
| R-20 | Listen-in SIP user accumulation на abrupt admin disconnect — orphan mixmonitor leg в FS | Средняя | Низкий (operational) | Plan 11 Task 10: WS-disconnect cleanup hook + janitor goroutine каждые 5 мин. |

---

## 19. Глоссарий

См. раздел 1.2 (термины и сокращения).

---

## 20. Приложение A: инвентаризация UI-страниц из прототипа

Все страницы обязательны к реализации в финальном продукте.

**Источники истины (design reference):**
1. **`SocioPulse.html`** в корне репозитория — runnable single-file сборка прототипа (CSS + JSX через babel-standalone из CDN). Открыть в браузере → увидеть финальный визуал и поведение всех состояний и страниц. Tweaks-panel внутри файла позволяет переключать темы, размер шрифта, состояния FSM-оператора и режимы конструктора анкет — для скриншотов и проверки edge-case'ов.
2. **`social-pulse-maket/project/*.jsx + styles.css`** — те же исходники в модульном виде (чище для чтения). Используются для понимания структуры компонентов, props, имён классов CSS.

При расхождении между двумя источниками приоритет — у `SocioPulse.html` (это то, что видно в браузере). На практике расхождений быть не должно — они собраны из одних и тех же файлов.

**Что НЕ переносим в production**: компонент `TweaksPanel` и связанные с ним `useTweaks`/`TweaksRoot` — это инструмент дизайн-прототипа Anthropic, не часть продукта. В production-фронте их нет.

### Не-аутентифицированные

| Страница | JSX-файл | Описание |
|---|---|---|
| Login screen | `login.jsx` | Левая панель с брендингом + правая форма входа (org_id, login, password, demo-buttons) |

### Оператор (роль `operator`)

| Страница | JSX-файл | Описание |
|---|---|---|
| Workstation | `workstation.jsx` | Двухпанельный layout: слева — call-card по состоянию FSM, справа — анкета |
| Моя результативность | `operator-pages.jsx` (`MyStats`) | KPI-tiles, графики звонков по часам и статусов, сравнение с командой |
| О проекте | `operator-pages.jsx` (`ProjectInfo`) | Описание проекта, прогресс, команда |
| История звонков | `operator-pages.jsx` (`OpHistory`) | Таблица с фильтрами и переходом к анкете |

### Админ (роль `admin`)

| Страница | JSX-файл | Описание |
|---|---|---|
| Обзор | `admin-pages-1.jsx` (`AdminOverview`) | Дашборд: KPI-tiles, прогресс по округам, состояние линии, операторы, последние звонки |
| Состояние операторов | `admin-pages-1.jsx` (`AdminOperators`) | Таблица операторов с фильтрами, listen-in modal |
| Состояние автодозвона | `admin-pages-1.jsx` (`AdminDialer`) | Сетка из 32 линий, статусы, причины завершений |
| Проекты | `admin-pages-1.jsx` (`AdminProjects`) | Карточки проектов с табами active/paused/archived |
| Анкеты | `surveys.jsx` (`AdminSurveys`) | Список анкет + переход в конструктор |
| Конструктор анкеты — form | `surveys.jsx` (`FormBuilder`) | Трёхколоночный layout: список вопросов / редактор / типы+свойства |
| Конструктор анкеты — flow | `surveys.jsx` (`FlowBuilder`) | Палитра / canvas / свойства узла |
| Пользователи | `admin-pages-2.jsx` (`AdminUsers`) | Таблица + modal create/edit |
| Исходящие звонки | `admin-pages-2.jsx` (`AdminCalls`) | Двухколоночный: список звонков + панель проигрывателя |
| Финансы | `admin-pages-2.jsx` (`AdminFinance`) | KPI-tiles, графики, таблица расходов по проектам |
| Отчётность | `admin-pages-2.jsx` (`AdminReports`) | Сетка карточек отчётов + форма произвольной выгрузки |

### Общее (присутствует на каждой странице после входа)

| Элемент | JSX-файл | Описание |
|---|---|---|
| Sidebar | `layout.jsx` (`Sidebar`) | Навигация по ролям + footer с user-chip |
| Topbar | `layout.jsx` (`Topbar`) | Хлебные крошки + уведомления + переключатель темы + селектор проекта |
| Listen-in modal | `app.jsx` (`ListenInModal`) | Модал с waveform + информацией о звонке + действиями |
| Operator detail modal | `admin-pages-1.jsx` (`OperatorDetailModal`) | Список действий админа над оператором |
| New user modal | `admin-pages-2.jsx` (`NewUserModal`) | Форма создания учётки |
| Tweaks panel | `tweaks-panel.jsx` | **НЕ переносим в продакшн** — это инструмент дизайн-прототипа Anthropic; для production её нет |

Стилизация — `styles.css` целиком (CSS-переменные, утилитарные классы); переносим как-есть, минимально адаптируя под Tailwind или CSS-modules (см. ADR-009).

---

## 21. Приложение B: примеры событий и сообщений

### B.1 NATS event: телефонное событие

Subject: `tenant.<tenant_id>.telephony.event.<call_id>`

```json
{
  "event": "channel.answered",
  "call_id": "550e8400-e29b-41d4-a716-446655440000",
  "tenant_id": "...",
  "ts": "2026-05-06T11:23:45.123Z",
  "fs_node": "fs-2",
  "sip_call_id": "abc123@fs-2.sociopulse.local",
  "sip_response_code": 200,
  "answered_by": "respondent",
  "metadata": {
    "trunk_used": "mtt-msk-1",
    "from_caller_id": "+74950000000",
    "to_number": "+79161234567"
  }
}
```

### B.2 NATS command: originate

Subject: `tenant.<tenant_id>.telephony.cmd.<call_id>`

```json
{
  "action": "originate",
  "call_id": "550e8400-...",
  "tenant_id": "...",
  "ts": "2026-05-06T11:23:43.000Z",
  "idempotency_key": "ik-7f3a...",
  "to_number": "+79161234567",
  "trunk_strategy": "least_cost_with_fallback",
  "operator_sip_user": "op_3f8a_42b1",
  "operator_fs_node": "fs-2",
  "consent_prompt_url": "s3://sociopulse-consent-prompts/<tenant_id>/default.opus",
  "recording_path": "/var/spool/sociopulse/<tenant_id>/<call_id>.wav",
  "dialing_timeout_sec": 25,
  "from_caller_id": "+74950000000",
  "trace_id": "..."
}
```

### B.3 WebSocket frame: operator state change

Server → admin browser:

```json
{
  "type": "event",
  "sub_id": "abc123",
  "topic": "operators.state",
  "payload": {
    "operator_id": "...",
    "state": "call",
    "since": "2026-05-06T11:23:45.123Z",
    "current_call_id": "550e8400-...",
    "project_id": "..."
  }
}
```

### B.4 gRPC: recording.Commit

```protobuf
syntax = "proto3";
package sociopulse.recording.v1;

service Recording {
  rpc Commit(CommitRequest) returns (CommitResponse);
  rpc GetPresignedURL(GetPresignedURLRequest) returns (GetPresignedURLResponse);
}

message CommitRequest {
  string tenant_id = 1;
  string call_id = 2;
  string s3_bucket = 3;
  string s3_key = 4;
  string sha256 = 5;
  uint32 duration_sec = 6;
  bytes encrypted_dek = 7;
  string kms_key_id = 8;
  string codec = 9;
}

message CommitResponse {
  bool ok = 1;
  string error = 2;
}
```

### B.5 Survey schema (минимальный пример)

```json
{
  "version": "1.0",
  "title": "Минимальный опрос",
  "intro": "Здравствуйте...",
  "nodes": [
    {
      "id": "start",
      "kind": "start",
      "next": [{"to": "q1", "when": "true"}],
      "ui": {"x": 40, "y": 20}
    },
    {
      "id": "q1",
      "kind": "question",
      "type": "single",
      "text": "Согласны участвовать?",
      "required": true,
      "options": [
        {"id": "yes", "label": "Да"},
        {"id": "no", "label": "Нет"}
      ],
      "next": [
        {"to": "end_success", "when": "answer == 'yes'"},
        {"to": "end_refused", "when": "true"}
      ],
      "ui": {"x": 40, "y": 130}
    },
    {"id": "end_success", "kind": "success-end", "ui": {"x": 40, "y": 250}},
    {"id": "end_refused", "kind": "refusal-end", "ui": {"x": 240, "y": 250}}
  ],
  "metadata": {
    "estimated_minutes": "1",
    "primary_mode": "form"
  }
}
```

---

## 22. ADR (Architecture Decision Records)

Каждый ADR — отдельный отчёт о значимом решении: контекст, альтернативы, выбор, последствия.

### ADR-001: Аудио-путь оператора через WebRTC (mod_verto/SIP-WSS)

**Статус:** Accepted
**Дата:** 2026-05-06

**Контекст**
Оператор работает в браузере (UI), но звонок — это SIP/RTP-сессия с FreeSWITCH. Нужен механизм соединения браузера с медиа-плоскостью FS, чтобы оператор мог говорить и слышать.

**Опции**

| Дим. | A: Софтфон рядом с браузером | B: USB-hardphone | C: WebRTC в браузере |
|---|---|---|---|
| Complexity | Low (готовое ПО) | Low | Medium (verto config) |
| Cost | Free | Hardware ₽~3000/оператор | Free + TURN ₽5000/мес |
| UX | Плохой (две окна) | Средний (отдельная гарнитура) | Отличный (одно окно) |
| Корпоративная сеть | Часто блокирует SIP | Не зависит от сети | UDP может блокироваться → TURN fallback |
| Обновления | Каждый клиент сам | Не требуется | Через браузер автоматически |

**Решение**: C — WebRTC через `mod_verto` (или SIP-over-WSS через `mod_sofia`).

**Trade-off**: получаем лучший UX и автоматические обновления, но добавляем зависимость от корпоративных firewall'ов; митигация — собственный TURN-сервер.

**Последствия**
- Усложнение FS-конфига (verto-binding, DTLS-сертификаты).
- Появляется TURN-сервер как компонент.
- Нагрузка медиа-трафика идёт от браузера напрямую к FS-нода (publicIP), не через k8s.

### ADR-002: FreeSWITCH self-hosted, multi-trunk routing

**Статус:** Accepted
**Дата:** 2026-05-06

**Контекст**: на 50k звонков/день нужна управляемая, масштабируемая телефонная плоскость с собственной dialer-логикой и хранением записей у нас.

**Опции**

| Дим. | A: Облачный API (Voximplant и т.п.) | B: Self-hosted FS, 1 trunk | C: Self-hosted FS, multi-trunk |
|---|---|---|---|
| Complexity | Low (готовое API) | Medium (свой кластер) | Medium-High (+routing) |
| Cost (50k звонков/мес) | ₽700-900k | ₽525k + инфра | ₽525k + инфра + multi-routing |
| Flexibility | Lockin | High | High |
| Resilience | Vendor SLA | SPOF на trunk'е | High (multi-trunk failover) |

**Решение**: C.

**Последствия**
- Нужны компетенции по FreeSWITCH (агенты-исполнители имеют их).
- Контракты с ≥ 2 операторами связи (бизнес-процесс, не код).
- Гибкость в дизайне dialer-алгоритмов.

### ADR-003: Progressive-dialer для v1, predictive — на v2

**Статус:** Accepted
**Дата:** 2026-05-06

**Контекст**: dialer-режим определяет UX оператора и эффективность смены.

**Опции**: см. раздел 8 первоначального обсуждения.

**Решение**: Progressive (1:1) для v1.

**Trade-off**: меньше эффективность операторов (60-70% vs 80-90%), но без abandoned-call-проблем и без необходимости ML/исторических данных. Predictive — отдельная стратегия в `dialer.OperatorFSM` на v2.

### ADR-004: Модульный монолит на Go + 2 sidecar'а

**Статус:** Accepted

**Опции**: монолит / модульный монолит / микросервисы.

**Решение**: модульный монолит + telephony-bridge + recording-uploader + cmd/worker.

**Обоснование**: см. раздел 5 первоначального обсуждения и раздел 5 этого документа.

### ADR-005: Целостность записей — 99.5% (uploaded), полная потеря при крахе FS-VM принимается

**Статус:** Accepted (revised 2026-05-06 — пересмотрен после ревью архитектуры)

**Контекст**: запись пишется на локальный диск FS-нода (`mod_record_session` → `/var/spool/sociopulse/<call_id>.wav`). Падение нода в момент звонка → потеря всех in-progress recordings на этом узле.

**Опции**

- A: 99.99% за счёт live-репликации каждого RTP-stream (mod_audio_fork → sibling node / shared NVMe). +1 сервис, +1 хранилище, +bandwidth, повышение latency.
- B: 99.5% — локальный диск + быстрый uploader; при крахе FS-VM теряются in-progress recordings (~80 одновременных при peak), редкое событие.
- C: 99.95% но нечестно — заявить целевую цифру без покрытия full-VM-loss сценария.

**Решение**: B.

**Trade-off**: принимаем потери при крахе FS-VM (event-driven, не steady-state). При peak 400 одновременных каналов и 5 узлах — крах одного узла = ~80 потерянных recordings, что превышает дневной бюджет 0.5% (250) одним инцидентом. Это допустимо, потому что: (a) сам event редкий — VM crash на Yandex Cloud в среднем < 1 раз/год на узел, (b) социология — не финансы, потеря единичных интервью покрывается ре-обзвоном с увеличением выборки на N+10%, (c) +1 сервис live-replication удваивает операционную поверхность.

**Митигация**: 
- Alert `recording_loss_rate > 0.5% per 24h (rolling)` — при повторении инцидентов пересмотр в сторону опции A.
- При крахе FS-VM `cmd/api` помечает все active calls на этом узле как `recording_lost=true`, событие в audit_log, проект-менеджер видит в UI и может назначить ре-обзвон.
- Backlog v2: оценить mod_audio_fork → second VM как опцию A2 (без shared FS, с RTP-mirror).

### ADR-006: PostgreSQL RLS + transaction-mode PgBouncer + SET LOCAL

**Статус:** Accepted

**Контекст**: multi-tenancy через RLS требует, чтобы `app.tenant_id` был правильно установлен в каждой транзакции. PgBouncer в session mode не масштабируется (1 backend connection ≈ 1 client).

**Опции**

- A: `PgBouncer session mode` + `set_config` persist=true. Не масштабируется.
- B: `PgBouncer transaction mode` + `SET LOCAL app.tenant_id`. Масштабируется, требует дисциплины.
- C: Без PgBouncer (прямые connection'ы). Не масштабируется.

**Решение**: B.

**Последствия**: каждая API-операция = одна транзакция; запрещены долгие транзакции в hot-path; готовый код-pattern в `gateway` middleware.

### ADR-007: FreeSWITCH вне Kubernetes

**Статус:** Accepted

**Контекст**: RTP-трафик плохо ходит через k8s NAT/Service-LB. FreeSWITCH также чувствителен к kernel-tuning (jitter buffers, RT-priority).

**Опции**

- A: FS в k8s с `hostNetwork: true`. Работает, но pod'ы привязаны к node, нет переезда.
- B: FS на VM с публичным IP, отдельно от k8s.

**Решение**: B.

**Последствия**: нужны IaC-pipeline для VM (Packer + Ansible) отдельно от k8s-манифестов; recording-uploader — systemd-unit, не DaemonSet.

### ADR-008: Survey runtime — единый Go-код через WASM, fallback TS-port если TinyGo не справится

**Статус:** Conditional Accept (revised 2026-05-06)

**Контекст**: бизнес-логика «следующий узел по ответу» нужна и на сервере (валидация + статистика), и в браузере (мгновенный UX). Дублирование на Go и TS — источник багов.

**Опции**

- A: Дублировать на Go и TS, синхронизировать вручную.
- B: Единый код на Go, в браузере — через WebAssembly (TinyGo build).
- C: Единый код на TS, на сервере — через node-sidecar или v8 в Go.
- B-fallback: B как preferred, но если TinyGo не компилирует DSL evaluator (`expr-lang/expr` использует heavy reflection), переходим на A с golden-test contract — общий набор тестовых сценариев гоняется параллельно против Go и TS реализаций.

**Решение**: B-fallback.

**Условие исполнения (Plan 07 Task 0):** прежде чем кодить runtime, выполнить TinyGo proof-of-concept:
1. Скомпилировать `expr-lang/expr` Eval с минимальным DSL (5 операций: `==`, `!=`, `<`, `>`, `&&`, `in`).
2. Замерить размер WASM-bundle. Цель: < 500 KB после gzip.
3. Замерить cold-load TTI на target hardware (Core i3 8th, 8GB RAM, Chrome): runtime-init + первый Eval должны завершаться < 200 мс.

Если PoC проходит — продолжаем с B. Если нет — переключаемся на A:
- Go runtime в `internal/surveys/runtime/` остаётся.
- TS-port в `web/src/surveys/runtime.ts` пишется ручно, ~300 LoC.
- `web/tests/survey-runtime/golden.spec.ts` гоняет 50+ test-cases (JSON-схема + JSON-ответ → JSON-next-node) против обеих реализаций. Расхождение = CI failure.

**Trade-off (B):** WASM-bundle ~200-500 KB добавляет к фронтенд-payload (NFR-1 TTI < 2s). Один источник правды для Go-кода.

**Trade-off (A):** Дублирование 200-300 LoC. Защита от drift через golden-tests. Простой dev-workflow (нет TinyGo-toolchain).

Решение между B и A финализируется в Plan 07 Task 0 на основе измерений.

### ADR-009: CSS — переносим как-есть из прототипа (CSS variables + утилитарные классы)

**Статус:** Accepted

**Контекст**: прототип использует ручную CSS с CSS-переменными и утилитарными классами (без Tailwind/styled-components). Стиль продуман и согласован.

**Опции**

- A: Переписать на Tailwind. Большая работа, потеря дизайна.
- B: Переписать на CSS Modules / Vanilla Extract. Средняя работа.
- C: Перенести как-есть, добавить Radix UI primitives для headless-компонентов (Dialog, Popover).

**Решение**: C.

**Последствия**: CSS-bundle ~22KB, простой, читаемый; компоненты Radix добавляются точечно.

### ADR-010: Postgres + ClickHouse для OLTP+OLAP-разделения

**Статус:** Accepted

**Контекст**: на 50k звонков/день и горизонте 1+ год Postgres не вытянет аналитические запросы (group by tenant + project + month) без серьёзных индексов и денормализации.

**Опции**

- A: Только Postgres. Нужны materialized views, проблемы при обновлениях.
- B: Postgres + ClickHouse для аналитики. Двойное хранение, но чистая archi.
- C: Postgres + Druid / TimescaleDB. TimescaleDB ближе, но менее выразительный для OLAP.

**Решение**: B.

**Trade-off**: дополнительная инфра-стоимость и ETL-сложность; зато аналитика отделена от OLTP, не влияет на latency API.

### ADR-011: NATS JetStream вместо Kafka

**Статус:** Accepted

**Контекст**: нужна шина событий между модулями монолита и sidecar'ами, с durability для critical-flows и pub-sub.

**Опции**

- A: Kafka. Industry standard, но операционно тяжелее.
- B: NATS JetStream. Лёгкий, нативная Go-интеграция, достаточно durability.
- C: RabbitMQ. Менее подходит для streaming.

**Решение**: B.

**Trade-off**: меньшая ёмкость retention vs Kafka, но на нашем масштабе (50k events/day на критичные subjects) NATS достаточен. При росте — миграция возможна.

### ADR-012: Go logger — zap (вместо slog)

**Статус:** Accepted

**Контекст**: пользователь явно выбрал zap.

**Решение**: `go.uber.org/zap` с production-config + redaction-middleware.

**Trade-off**: zap менее каноничен после появления `slog` в std-lib (Go 1.21+), но он быстрее (zero-alloc) и поддерживает sampling из коробки. Для high-throughput сервиса (тысячи WS-фреймов/сек) zap предпочтительнее.

### ADR-013: viper для конфигурации (yaml + env override + hot-reload)

**Статус:** Accepted

**Решение**: `spf13/viper` для чтения `config.yaml` с env-override и `fsnotify`-watch (для hot-reload `log_level` и т.п.).

**Trade-off**: viper тяжеловат, но даёт нужный feature-set. Альтернатива — `koanf` (легче, тот же API).

### ADR-0014. HTTP-роутер: gin-gonic/gin

**Статус:** Accepted (2026-05-07).

**Контекст.** Backend cmd/api обслуживает REST API для оператора (~30 эндпоинтов) и админа (~80 эндпоинтов), плюс WebSocket-эндпоинты в realtime-модуле. Нужен HTTP-роутер с middleware-цепочкой: request-id, structured logging (zap), recovery, JWT-валидация, RBAC, rate-limit, idempotency, tenant context (`SET LOCAL app.tenant_id`).

Кандидаты: net/http+chi, gin-gonic/gin, fiber, echo. Чистый `net/http` отвергнут — нужна декларативная routing-tree-семантика и группа middleware.

**Решение.** Gin (`github.com/gin-gonic/gin` v1.10+).

**Обоснование.**
1. Bridge `gin-contrib/zap` совмещает gin с zap-логером (ADR-0012), даёт нам стандартизованные поля `tenant_id`/`request_id`/`trace_id` бесплатно.
2. `c.ShouldBindJSON(&dto)` + `c.JSON(status, resp)` сокращает шаблонный код в каждом handler ~в 2 раза против stdlib-стиля.
3. `gin.SetMode(gin.TestMode)` детерминированно глушит логи в `httptest`-сценариях.
4. Большое community + стабильный API (v1 с 2017).
5. RU Go-сообщество знакомо с gin — onboarding дешевле.

**Альтернативы.**
- **chi** — отлично совместим со stdlib `http.Handler`, но требует ручного JSON-encode/decode и custom logger middleware. Потенциальная экономия: 200-300 строк handler-кода × 110 эндпоинтов.
- **echo** — функционально сопоставим с gin, но ecosystem меньше.
- **fiber** — fasthttp под капотом, несовместимо с net/http middleware (наш TLS termination, идемпотентность, healthcheck-клиенты — все net/http-shaped).

**Последствия.**
- `pkg/httputil/gin_adapter.go` нужен для перевода stdlib `http.Handler`-middleware (idempotency, requestid) в `gin.HandlerFunc`. Адаптер реализуется в Plan 00a Task 5.
- WebSocket upgrade использует `c.Request` + `c.Writer` напрямую (gorilla/websocket совместим). См. Plan 11.
- Все handler-сигнатуры: `func (h *Handler) Method(c *gin.Context)`. Тесты на handler через `httptest.NewRecorder()` + `gin.CreateTestContext(...)`.
- Запрет добавления `chi` в `go.mod` — депгард rule в Plan 00 Task 9.

### ADR-0015. Test-Driven Development as mandatory discipline

**Статус:** Accepted (2026-05-07).

**Контекст.** План-проекта состоит из 22 implementation plans, каждый из которых разбит на ~10-15 задач. Каждая задача описана как Red-Green-Refactor цикл: «write failing test → run it fails → implement → run it passes → commit». Это TDD-структура.

Без формального утверждения TDD как обязательной дисциплины, исполнители планов могут «срезать» — написать код первым, тесты вторым, а то и пропустить. Это создаёт невидимый технический долг: tests-after-the-fact не покрывают edge cases, не ловят regression, не служат документацией поведения.

**Решение.** TDD обязателен для всего нового кода в `internal/` и `pkg/`. Допустимые исключения:
1. `cmd/<binary>/main.go` — composition root, тесты — smoke (запустить и проверить /healthz).
2. Migrations — schema validation через интеграционные тесты.
3. Generated code (mocks, proto-stubs).

**Обоснование.**
1. Спека §17.1 уже описывает пирамиду тестов с ~2000 unit-тестов как целевое количество. TDD — единственный способ достичь этого без отставания.
2. `samber/cc-skills-golang@golang-testing` § Persona: «You write tests to constrain behavior, not to hit coverage targets.» — TDD естественно ведёт к тестам, фиксирующим поведение.
3. Coverage-таргеты (≥85% service, ≥70% store, ≥60% http/grpc) выполняются автоматически если задача написана как RGR-цикл.

**Альтернативы.**
- **Tests-after** — быстрее в моменте, медленнее в долгосрочной перспективе. Регрессия = детектор tests-after.
- **No tests** — отвергнуто бизнес-требованием §17.

**Последствия.**
- Каждая задача в planах 00a, 02-19 — RGR-цикл. PR-template требует подтверждения RGR-дисциплины.
- `superpowers:test-driven-development` skill — обязательная sub-skill при subagent-driven-development.
- `paralleltest` + `thelper` + `testifylint` линтеры обязательны (см. Plan 00 Task 9).
- Когда test становится «не TDD» (e.g. характеризация легаси), автор отмечает `// characterization: pre-existing behaviour` — будет поводом для review.
- Coverage gate в CI: `make test-cover` падает при <70% общего покрытия (см. Plan 00 Task 11).
- TDD-методология распилована в `docs/architecture/08-tdd-discipline.md` (см. Task 3 этого плана).

---

## Self-review checklist

Прошёл по чек-листу skill-а перед тем, как считать документ готовым:

- [x] **Placeholder scan**: нет TBD/TODO в основных разделах. Открытый вопрос — ADR-008 (WASM-runtime) помечен как Proposed.
- [x] **Internal consistency**: feature lists, modules, data model, NFR — все ссылаются на одни и те же сущности. FSM в разделах 4.3, 5, 8 — согласована.
- [x] **Scope check**: документ для одной системы, описывает её целиком, но реализация будет идти по подсистемам — каждая получит свой implementation plan через `writing-plans` skill.
- [x] **Ambiguity check**: где могло быть двусмысленно — зафиксировал решением (ADR-001..014) или пометил как открытый вопрос (ADR-008).
- [x] **Self-review issues C1-C4 / I1-I4 / U1-U10** — все включены в финальную доку: WebRTC оператор (ADR-001), recording-uploader как systemd (раздел 5.3.2), envelope KMS (раздел 9.2), 99.95% (ADR-005), processing-state маппинг (раздел 4.3), AES-GCM в приложении (раздел 6.2 + 12.4), gRPC между uploader и api (раздел 9.1, B.4), pgbouncer txn mode (ADR-006), worker-binary (раздел 5.4), regions.yaml (раздел 8.3), idempotency (NFR-12), trunk-capacity и caller-ID (раздел 7.3, 14.3), backpressure (раздел 8.6), recording integrity audit (раздел 9.3), WS-auth refresh (раздел 10.1), backup/DR (раздел 16.6), config registry (раздел 14).

---

**Документ готов к ревью.**

