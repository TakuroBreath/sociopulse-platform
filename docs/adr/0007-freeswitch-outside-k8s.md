# ADR-0007: FreeSWITCH вне Kubernetes

**Статус:** Accepted
**Дата:** 2026-05-06
**Принимающий:** platform team

## Контекст

RTP-трафик плохо ходит через k8s NAT/Service-LB. FreeSWITCH также чувствителен к kernel-tuning (jitter buffers, RT-priority).

## Альтернативы

- A: FS в k8s с `hostNetwork: true`. Работает, но pod'ы привязаны к node, нет переезда.
- B: FS на VM с публичным IP, отдельно от k8s.

## Решение

B.

## Последствия

Нужны IaC-pipeline для VM (Packer + Ansible) отдельно от k8s-манифестов; recording-uploader — systemd-unit, не DaemonSet.

## Связанное

- Спека §22 (ADR-007)
- ADR-0001 (WebRTC требует публичного IP на FS-узле)
- ADR-0002 (multi-trunk routing — обусловлен self-hosted FS)
- ADR-0005 (модель отказов FS-VM лежит в основе решения о целостности записей)
