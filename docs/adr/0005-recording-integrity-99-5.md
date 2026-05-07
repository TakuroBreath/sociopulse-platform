# ADR-0005: Целостность записей — 99.5% (uploaded), полная потеря при крахе FS-VM принимается

**Статус:** Accepted (revised 2026-05-06 — пересмотрен после ревью архитектуры)
**Дата:** 2026-05-06
**Принимающий:** platform team

## Контекст

Запись пишется на локальный диск FS-нода (`mod_record_session` → `/var/spool/sociopulse/<call_id>.wav`). Падение нода в момент звонка → потеря всех in-progress recordings на этом узле.

## Альтернативы

- A: 99.99% за счёт live-репликации каждого RTP-stream (mod_audio_fork → sibling node / shared NVMe). +1 сервис, +1 хранилище, +bandwidth, повышение latency.
- B: 99.5% — локальный диск + быстрый uploader; при крахе FS-VM теряются in-progress recordings (~80 одновременных при peak), редкое событие.
- C: 99.95% но нечестно — заявить целевую цифру без покрытия full-VM-loss сценария.

## Решение

B.

**Trade-off**: принимаем потери при крахе FS-VM (event-driven, не steady-state). При peak 400 одновременных каналов и 5 узлах — крах одного узла = ~80 потерянных recordings, что превышает дневной бюджет 0.5% (250) одним инцидентом. Это допустимо, потому что: (a) сам event редкий — VM crash на Yandex Cloud в среднем < 1 раз/год на узел, (b) социология — не финансы, потеря единичных интервью покрывается ре-обзвоном с увеличением выборки на N+10%, (c) +1 сервис live-replication удваивает операционную поверхность.

## Последствия

**Митигация**:
- Alert `recording_loss_rate > 0.5% per 24h (rolling)` — при повторении инцидентов пересмотр в сторону опции A.
- При крахе FS-VM `cmd/api` помечает все active calls на этом узле как `recording_lost=true`, событие в audit_log, проект-менеджер видит в UI и может назначить ре-обзвон.
- Backlog v2: оценить mod_audio_fork → second VM как опцию A2 (без shared FS, с RTP-mirror).

## Связанное

- Спека §22 (ADR-005)
- Спека §5.3.2 (recording-uploader как systemd-unit)
- Спека §9.3 (recording integrity audit)
- ADR-0007 (FreeSWITCH вне Kubernetes — связано с моделью отказов VM)
