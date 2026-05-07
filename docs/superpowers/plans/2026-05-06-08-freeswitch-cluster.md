# Implementation Plan #08 — FreeSWITCH cluster + recording-uploader

> **🛑 Task 1-6 (production cluster) DEFERRED — Phase 2.**
>
> Per project decision (2026-05-06), the **production FS-cluster** (Packer + Ansible + 5 Compute VMs + TURN server + recording-uploader systemd) is part of Phase 2 (pre-prod cutover) — not built during product development. **Task 0 (Local Dev — FreeSWITCH в Docker)** below covers Phase 1: a single-instance FS container in `docker-compose.dev.yml` is sufficient for developing and testing telephony-bridge (Plan 09), dialer (Plan 10), recording-uploader logic (Plan 12).
>
> When Phase 1 is finished and you're ready for prod-deploy, return to Tasks 1-6.

**Дата:** 2026-05-06
**Автор:** SocioPulse Engineering
**Статус:** Task 0 (dev) — Ready · Tasks 1-6 (prod) — Deferred to Phase 2
**Связанные документы:**
- Spec: `docs/superpowers/specs/2026-05-06-sociopulse-system-design.md` (§7 FS-кластер, §9 Запись и шифрование, §13.1-2 152-ФЗ/PCI compliance, §16.1 Безопасность инфраструктуры, §FR-E Запись разговоров, ADR-001 FreeSWITCH, ADR-002 Verto+mTLS, ADR-005 client-side AES-256-GCM, ADR-007 storage-tiered Object Storage)
- Plan #00: `docs/superpowers/plans/2026-05-06-00-foundation.md` (стиль, monorepo layout)
- Plan #01: `docs/superpowers/plans/2026-05-06-01-infrastructure.md` (Yandex Cloud, KMS, Lockbox, S3, VPC, security groups уже подняты)

---

## Goal

Развернуть production-ready FreeSWITCH-кластер из 3 нод в Yandex Cloud для обработки SIP-trunk вызовов от 8 операторов, c полным lifecycle записи разговоров: захват WAV на локальный диск → конвертация ffmpeg в Opus 32 kbps 16 kHz → client-side шифрование AES-256-GCM ключом из KMS → загрузка в S3 hot-tier → ack через gRPC `Recording.Commit` → удаление локальной копии.

В результате этого плана:
1. Готов **Packer-template** для immutable Yandex Compute Image: Ubuntu 22.04 + FreeSWITCH 1.10.10 + recording-uploader + node_exporter + blackbox_exporter.
2. Готовы **Ansible playbooks** для provisioning FS-нод: установка FS из официального пакета SignalWire, модули `mod_sofia/event_socket/record_session/verto/playback/dptools/mixmonitor`, ffmpeg, генерация DTLS-сертификатов, systemd-юниты, mount /var/spool/sociopulse, exporter'ы.
3. Готовы **боевые XML-конфиги FreeSWITCH**: SIP-профиль `trunks` для входящих от операторов, SIP-профиль `operators` для verto-клиентов с mTLS на :8082, dialplan с originate/bridge/mixmonitor + voice-prompt согласия записи, event_socket :8021 c mTLS, switch.conf с RT-prio и max-sessions=200.
4. Реализован **`cmd/recording-uploader`** — Go-бинарь с fsnotify-watcher, ffmpeg-конвертером, AES-256-GCM client-side encryption через KMS GenerateDataKey, S3-uploader, gRPC-клиентом `Recording.Commit`, retry-backoff (exponential 5s→5min, max 100 попыток), Prometheus :9091.
5. Готов **Terraform-модуль `freeswitch-cluster`** — count=3 VM (4 vCPU / 8 GB / 200 GB NVMe), public IP, security-group с whitelist'ом операторов, образ из Packer-build.
6. Готов **TURN-сервер** (mitigation R-1 `Failed call` из spec §17): отдельная VM с coturn 4.6+, listen :3478 UDP/TCP + :5349 TLS, long-term-creds в Lockbox, prometheus_exporter.
7. Развёрнуты **DNS-записи** через Yandex Cloud DNS: `fs-1.dev.sociopulse.ru`, `fs-2.dev.sociopulse.ru`, `fs-3.dev.sociopulse.ru`, `turn.dev.sociopulse.ru`.
8. Прогоны: Packer-build smoke, ansible-lint + molecule, VM boot test (`ssh systemctl status freeswitch`), SIPp-симуляция входящего вызова → запись пишется в `/var/spool/sociopulse/`. End-to-end (после Plan #09 `originate/bridge/Recording.Commit gRPC API`).

**Не делаем в этом плане:**
- Verto-клиент для оператор-WebApp (Plan #11).
- gRPC `Recording.Commit` server-side (Plan #09 backend).
- Шифрование per-tenant data-keys + ротация (Plan #10 KMS-rotator).
- Архивация в cold-tier через lifecycle policy (Plan #12 retention).

**Acceptance criteria:**
- [ ] `cd deployments/packer/freeswitch && packer build .` собирает образ за < 25 минут, итог в Yandex Cloud Image registry.
- [ ] `cd deployments/terraform/envs/dev && terraform apply -target=module.freeswitch_cluster` поднимает 3 VM за < 10 минут.
- [ ] `ssh ubuntu@fs-1.dev.sociopulse.ru "systemctl status freeswitch"` возвращает `active (running)` и `loaded`.
- [ ] `ssh ubuntu@fs-1.dev.sociopulse.ru "fs_cli -x 'sofia status'"` показывает оба профиля `trunks` и `operators` в state `RUNNING`.
- [ ] `sipp -sn uac fs-1.dev.sociopulse.ru:5060 -m 1` принимает 200 OK, в `/var/spool/sociopulse/` появляется WAV.
- [ ] `recording-uploader` лог: `level=info msg="recording uploaded" call_id=... s3_key=recordings/2026/05/06/...opus.enc bytes=...`.
- [ ] Метрики Prometheus `recording_uploader_uploads_total{status="success"} > 0`.
- [ ] `coturn` отвечает на `turnutils_uclient -v turn.dev.sociopulse.ru -u sociopulse -w <pass>` без ошибок.
- [ ] Все Go-тесты `cmd/recording-uploader` проходят: `go test ./... -race -coverprofile=cover.out`, coverage ≥ 80 %.
- [ ] `ansible-lint deployments/ansible/freeswitch/` без warnings уровня error.
- [ ] DNS `dig +short fs-1.dev.sociopulse.ru` возвращает public IP VM.

---

## File structure (что создаём в этом плане)

```
social-pulse/
├── cmd/
│   └── recording-uploader/
│       ├── main.go                              # entrypoint
│       ├── main_test.go
│       ├── config.go                            # ENV-парсинг
│       ├── config_test.go
│       └── README.md                            # runbook
├── internal/
│   └── uploader/
│       ├── watcher.go                           # fsnotify wrapper
│       ├── watcher_test.go
│       ├── transcoder.go                        # ffmpeg wav→opus
│       ├── transcoder_test.go
│       ├── encryptor.go                         # AES-256-GCM + KMS GenerateDataKey
│       ├── encryptor_test.go
│       ├── s3client.go                          # yandex-cloud S3 PUT
│       ├── s3client_test.go
│       ├── recorder_client.go                   # gRPC client to Recording.Commit
│       ├── recorder_client_test.go
│       ├── retry.go                             # exponential backoff
│       ├── retry_test.go
│       ├── pipeline.go                          # orchestration
│       ├── pipeline_test.go
│       ├── metrics.go                           # Prometheus
│       └── metrics_test.go
├── deployments/
│   ├── packer/
│   │   └── freeswitch/
│   │       ├── freeswitch.pkr.hcl              # Packer-template
│   │       ├── variables.pkr.hcl
│   │       └── README.md
│   ├── ansible/
│   │   └── freeswitch/
│   │       ├── playbook.yml                    # main entrypoint
│   │       ├── inventory/
│   │       │   └── packer.ini
│   │       ├── roles/
│   │       │   ├── common/
│   │       │   │   ├── tasks/main.yml
│   │       │   │   └── handlers/main.yml
│   │       │   ├── freeswitch/
│   │       │   │   ├── tasks/main.yml
│   │       │   │   ├── tasks/install.yml
│   │       │   │   ├── tasks/configure.yml
│   │       │   │   ├── tasks/dtls.yml
│   │       │   │   ├── tasks/systemd.yml
│   │       │   │   ├── handlers/main.yml
│   │       │   │   ├── templates/
│   │       │   │   │   ├── switch.conf.xml.j2
│   │       │   │   │   ├── event_socket.conf.xml.j2
│   │       │   │   │   ├── sip_profiles_trunks.xml.j2
│   │       │   │   │   ├── sip_profiles_operators.xml.j2
│   │       │   │   │   ├── dialplan_default.xml.j2
│   │       │   │   │   ├── modules.conf.xml.j2
│   │       │   │   │   ├── verto.conf.xml.j2
│   │       │   │   │   └── sofia_internal.xml.j2
│   │       │   │   └── files/
│   │       │   │       ├── sociopulse-fs.service
│   │       │   │       └── consent_prompt.wav
│   │       │   ├── recording_uploader/
│   │       │   │   ├── tasks/main.yml
│   │       │   │   ├── files/recording-uploader   # built Go binary copied here
│   │       │   │   └── templates/
│   │       │   │       ├── recording-uploader.service.j2
│   │       │   │       └── recording-uploader.env.j2
│   │       │   └── observability/
│   │       │       ├── tasks/main.yml
│   │       │       └── templates/
│   │       │           ├── node_exporter.service.j2
│   │       │           └── blackbox_exporter.yml.j2
│   │       ├── molecule/
│   │       │   └── default/
│   │       │       ├── molecule.yml
│   │       │       ├── converge.yml
│   │       │       └── verify.yml
│   │       └── README.md
│   └── terraform/
│       ├── modules/
│       │   ├── freeswitch-cluster/
│       │   │   ├── main.tf
│       │   │   ├── variables.tf
│       │   │   ├── outputs.tf
│       │   │   ├── security_group.tf
│       │   │   ├── instance.tf
│       │   │   └── README.md
│       │   ├── turn-server/
│       │   │   ├── main.tf
│       │   │   ├── variables.tf
│       │   │   ├── outputs.tf
│       │   │   ├── security_group.tf
│       │   │   ├── cloud-init.yaml
│       │   │   └── README.md
│       │   └── dns-records/
│       │       ├── main.tf
│       │       ├── variables.tf
│       │       └── outputs.tf
│       └── envs/
│           └── dev/
│               ├── freeswitch.tf               # uses module.freeswitch-cluster
│               ├── turn.tf                     # uses module.turn-server
│               └── dns.tf                      # uses module.dns-records
└── tests/
    ├── sipp/
    │   ├── uac_invite.xml                      # SIPp scenario
    │   └── README.md
    └── e2e/
        └── recording_pipeline_test.sh          # bash смок-тест
```

---

## Tasks

### Task 0 — Local Dev: FreeSWITCH в Docker (PHASE 1)

**Цель:** одиночный FreeSWITCH-контейнер в `docker-compose.dev.yml` (профиль `telephony`) с минимально достаточной конфигурацией для разработки telephony-bridge, dialer, и recording-uploader. Без HA, без TURN, без Packer/Ansible. Tasks 1-6 (production) запускаются позже.

**Что упрощаем по сравнению с production (Tasks 1-6):**
- Один контейнер вместо 3-5 VM.
- Plain ESL на 8021 (без stunnel/mTLS) — это локалхост, дев only.
- Без mod_xml_curl directory endpoint — статическая XML directory с парой тестовых users.
- Без TURN server — реальный WebRTC NAT-traversal не нужен на localhost.
- Без recording-uploader как отдельного процесса — вместо этого telephony-bridge напрямую читает WAV из shared volume и вызывает gRPC `Recording.Commit` (mock-S3 = MinIO).
- Без mod_verto TLS — verto на ws:// (не wss://).
- Single trunk в sofia profile, нацеленный на test SIP provider (либо тестовая SIP-станция типа dummy `linphonec`).

**Что остаётся production-grade (чтобы код приложения был тот же):**
- ESL события (CHANNEL_ANSWER, CHANNEL_HANGUP_COMPLETE, RECORD_STOP) — реальные.
- Dialplan structure — minimal copy of production dialplan.
- mod_record_session пишет реальные WAV.
- Consent prompt играется (в dev — фейковая подсказка).

**Files:**
- Create: `dev/freeswitch/conf/freeswitch.xml` (root config)
- Create: `dev/freeswitch/conf/sip_profiles/internal.xml` (verto + operator users)
- Create: `dev/freeswitch/conf/sip_profiles/external.xml` (single trunk for test calls)
- Create: `dev/freeswitch/conf/dialplan/default.xml` (minimal: trunk → consent prompt → record_session → bridge to operator)
- Create: `dev/freeswitch/conf/directory/default.xml` (test users: op001/op002 + admin)
- Create: `dev/freeswitch/conf/sounds/consent_prompt_dev.wav` (10-секундная фейковая подсказка)
- Modify: `docker-compose.dev.yml` (already added `freeswitch` service in Plan 02 Task 5 with `profiles: [telephony, full]`)

- [ ] **Step 1: Создать `dev/freeswitch/conf/freeswitch.xml`**

Минимальный root config: загружает 5 модулей (mod_sofia, mod_event_socket, mod_dptools, mod_dialplan_xml, mod_record_session). Без mod_verto на v1 — оператор будет звонить через REST API, а WebRTC опционально через verto можно включить позже.

```xml
<?xml version="1.0"?>
<document type="freeswitch/xml">
  <section name="configuration">
    <configuration name="event_socket.conf" description="ESL">
      <settings>
        <param name="listen-ip" value="0.0.0.0"/>
        <param name="listen-port" value="8021"/>
        <param name="password" value="ClueCon"/>
        <param name="apply-inbound-acl" value="any"/>
      </settings>
    </configuration>
    <configuration name="modules.conf">
      <modules>
        <load module="mod_console"/>
        <load module="mod_event_socket"/>
        <load module="mod_sofia"/>
        <load module="mod_dialplan_xml"/>
        <load module="mod_dptools"/>
        <load module="mod_commands"/>
        <load module="mod_loopback"/>
        <load module="mod_sndfile"/>
        <load module="mod_native_file"/>
      </modules>
    </configuration>
  </section>
  <section name="dialplan" description="Default Dialplan">
    <X-PRE-PROCESS cmd="include" data="dialplan/default.xml"/>
  </section>
  <section name="directory">
    <X-PRE-PROCESS cmd="include" data="directory/default.xml"/>
  </section>
</document>
```

- [ ] **Step 2: Создать `dev/freeswitch/conf/dialplan/default.xml`**

Минимальный dialplan: incoming SIP → play consent prompt → record_session → bridge to operator. Это почти полная копия Production dialplan (Plan 08 Task 4) с одним упрощением — directory статичный.

```xml
<include>
  <context name="default">
    <extension name="incoming_call_to_operator">
      <condition field="destination_number" expression="^op(\d+)$">
        <action application="set" data="sociopulse_call_id=${uuid}"/>
        <action application="set" data="sociopulse_tenant_id=tenant-dev"/>
        <action application="set" data="recording_path=/var/spool/sociopulse/${sociopulse_call_id}.wav"/>
        <action application="set" data="recording_required=true"/>
        <action application="set" data="consent_required=true"/>
        <action application="answer"/>

        <!-- Consent prompt -->
        <action application="execute_extension" data="play_consent_prompt"/>

        <!-- Start recording -->
        <action application="execute_extension" data="start_recording"/>

        <!-- Bridge to operator -->
        <action application="bridge" data="user/$1@${domain_name}"/>
      </condition>
    </extension>

    <extension name="play_consent_prompt" continue="true">
      <condition field="${consent_required}" expression="^(true|1|yes)?$">
        <action application="log" data="INFO consent_prompt for ${sociopulse_call_id}"/>
        <action application="playback" data="/etc/freeswitch/sounds/consent_prompt_dev.wav"/>
      </condition>
    </extension>

    <extension name="start_recording" continue="true">
      <condition field="${recording_required}" expression="^(true|1|yes)$">
        <action application="set" data="RECORD_STEREO=true"/>
        <action application="record_session" data="${recording_path}"/>
      </condition>
    </extension>
  </context>
</include>
```

- [ ] **Step 3: Создать `dev/freeswitch/conf/directory/default.xml`**

Два test-user'а (`op001`, `op002`) для разработки + admin. Никакого dynamic xml_curl на dev.

```xml
<include>
  <domain name="$${domain}">
    <params>
      <param name="dial-string" value="{presence_id=${dialed_user}@${dialed_domain}}${sofia_contact(${dialed_user}@${dialed_domain})}"/>
    </params>
    <groups>
      <group name="default">
        <users>
          <user id="op001"><params>
            <param name="password" value="dev_op_001"/>
            <param name="vm-password" value="dev_op_001"/>
          </params></user>
          <user id="op002"><params>
            <param name="password" value="dev_op_002"/>
            <param name="vm-password" value="dev_op_002"/>
          </params></user>
        </users>
      </group>
    </groups>
  </domain>
</include>
```

- [ ] **Step 4: Создать тестовый WAV для consent prompt**

```bash
# 10 sec mono 8 kHz silence + одно фразы "test consent prompt"
mkdir -p dev/freeswitch/conf/sounds
ffmpeg -f lavfi -i "anullsrc=channel_layout=mono:sample_rate=8000" -t 10 \
  dev/freeswitch/conf/sounds/consent_prompt_dev.wav
```

- [ ] **Step 5: Smoke-test**

```bash
make dev-up PROFILE=telephony
sleep 10
docker logs sp-freeswitch | tail -30   # видим "FreeSWITCH Started, Ready to Rock!"
docker exec sp-freeswitch fs_cli -x "status"
```

Expected: FS is running, ESL on 8021 reachable.

- [ ] **Step 6: Commit**

```bash
git add docker-compose.dev.yml dev/freeswitch/
git commit -m "dev: add FreeSWITCH container for local telephony development"
```

**Когда переходить к Tasks 1-6 (production cluster)**: после завершения Phase 1 и принятия решения о хостинге БД-слоя. Tasks 1-6 разворачивают real-FS на VMs через Packer + Ansible с полным TLS, mod_xml_curl directory endpoint, multi-trunk routing, TURN, и отдельным cmd/recording-uploader. Это 95% существующего содержимого Plan 08 ниже.

---

### Task 1 — Packer-template для FreeSWITCH-образа

**Зачем:** immutable infrastructure (ADR-001). Любое изменение FS-конфига или версии = новый Packer-build → новый Image ID → пересборка VM через Terraform. Никаких manual-edits на live-VM.

**Что делаем:**

#### 1.1. Создать `deployments/packer/freeswitch/freeswitch.pkr.hcl`

```hcl
packer {
  required_version = ">= 1.10.0"
  required_plugins {
    yandex = {
      source  = "github.com/hashicorp/yandex"
      version = ">= 0.13.0"
    }
    ansible = {
      source  = "github.com/hashicorp/ansible"
      version = ">= 1.1.0"
    }
  }
}

source "yandex" "freeswitch" {
  endpoint            = var.yc_endpoint
  service_account_key_file = var.yc_service_account_key_file
  folder_id           = var.yc_folder_id
  zone                = var.yc_zone

  source_image_family = "ubuntu-2204-lts"
  source_image_folder_id = "standard-images"

  image_name        = "sociopulse-freeswitch-${formatdate("YYYYMMDD-hhmmss", timestamp())}"
  image_family      = "sociopulse-freeswitch"
  image_description = "FreeSWITCH 1.10 + recording-uploader for SocioPulse — built ${timestamp()}"
  image_min_disk_size_gb = 30
  image_pooled       = false
  image_labels = {
    project   = "sociopulse"
    component = "freeswitch"
    env       = var.env
    git_sha   = var.git_sha
  }

  instance_cores  = 4
  instance_mem_gb = 8
  disk_type       = "network-ssd"
  disk_size_gb    = 30
  preemptible     = true

  subnet_id        = var.packer_subnet_id
  use_ipv4_nat     = true
  ssh_username     = "ubuntu"
  ssh_timeout      = "10m"

  metadata = {
    user-data = file("${path.root}/cloud-init.yaml")
  }
}

build {
  name    = "sociopulse-freeswitch"
  sources = ["source.yandex.freeswitch"]

  provisioner "shell" {
    inline = [
      "cloud-init status --wait",
      "sudo apt-get update -qq",
      "sudo apt-get install -y -qq python3 python3-pip",
    ]
  }

  provisioner "ansible" {
    playbook_file = "${path.root}/../../ansible/freeswitch/playbook.yml"
    user          = "ubuntu"
    extra_arguments = [
      "--extra-vars", "ansible_python_interpreter=/usr/bin/python3",
      "--extra-vars", "env=${var.env}",
      "--extra-vars", "git_sha=${var.git_sha}",
      "--extra-vars", "recording_uploader_binary=${var.recording_uploader_binary}",
      "-vv",
    ]
  }

  provisioner "shell" {
    inline = [
      "sudo systemctl enable freeswitch",
      "sudo systemctl enable recording-uploader",
      "sudo systemctl enable node_exporter",
      "sudo systemctl enable blackbox_exporter",
      "sudo apt-get autoremove -y",
      "sudo apt-get clean",
      "sudo rm -rf /var/log/* /tmp/* /var/tmp/*",
      "sudo cloud-init clean --logs",
    ]
  }

  post-processor "manifest" {
    output = "manifest.json"
    strip_path = true
  }
}
```

#### 1.2. `deployments/packer/freeswitch/variables.pkr.hcl`

```hcl
variable "yc_endpoint" {
  type    = string
  default = "api.cloud.yandex.net:443"
}

variable "yc_service_account_key_file" {
  type        = string
  description = "Path to YC service-account-key JSON (Packer builder SA)"
}

variable "yc_folder_id" {
  type        = string
  description = "YC folder for Packer build (typically: sociopulse-build)"
}

variable "yc_zone" {
  type    = string
  default = "ru-central1-a"
}

variable "packer_subnet_id" {
  type        = string
  description = "YC subnet ID for Packer builder VM (must have NAT)"
}

variable "env" {
  type    = string
  default = "dev"
  validation {
    condition     = contains(["dev", "staging", "prod"], var.env)
    error_message = "env must be one of: dev, staging, prod"
  }
}

variable "git_sha" {
  type        = string
  description = "Git SHA of the build, set by CI"
}

variable "recording_uploader_binary" {
  type        = string
  description = "Local path to pre-built recording-uploader binary"
  default     = "../../../bin/recording-uploader"
}
```

#### 1.3. `deployments/packer/freeswitch/cloud-init.yaml`

```yaml
#cloud-config
package_update: true
package_upgrade: true
packages:
  - python3
  - python3-pip
  - ca-certificates
  - curl
ssh_pwauth: false
disable_root: true
```

#### 1.4. `deployments/packer/freeswitch/README.md`

Документация с шагами:
1. `export YC_TOKEN=$(yc iam create-token)`.
2. `make build-recording-uploader` — сборка Go-бинаря в `bin/recording-uploader`.
3. `cd deployments/packer/freeswitch && packer init . && packer validate -var-file=dev.pkrvars.hcl .`.
4. `packer build -var-file=dev.pkrvars.hcl .`.
5. На выходе — Image в Yandex Cloud, ID в `manifest.json`.

#### 1.5. Пример `deployments/packer/freeswitch/dev.pkrvars.hcl`

```hcl
yc_service_account_key_file = "../../../.secrets/packer-builder-sa-key.json"
yc_folder_id                = "b1g0123456789packer"
yc_zone                     = "ru-central1-a"
packer_subnet_id            = "e9bxxxxxxxxxxxxxxx"
env                         = "dev"
git_sha                     = "DEVELOPMENT"
recording_uploader_binary   = "../../../bin/recording-uploader"
```

#### 1.6. Тесты

Smoke-test через CI (см. Task 9):

```bash
cd deployments/packer/freeswitch
packer init .
packer fmt -check .
packer validate -var-file=dev.pkrvars.hcl .
```

И on-demand build:

```bash
packer build -var-file=dev.pkrvars.hcl .
test -f manifest.json
jq -e '.builds[0].artifact_id' manifest.json
```

**Acceptance:**
- [ ] `packer validate` без ошибок.
- [ ] `packer build` завершается за < 25 минут.
- [ ] `manifest.json` содержит `artifact_id` (Image ID).
- [ ] Image появляется в `yc compute image list --folder-id <build-folder>`.
- [ ] Размер итогового образа ≤ 5 GB.

---

### Task 2 — Ansible-плейбуки

**Зачем:** Packer вызывает Ansible как provisioner. Все настройки FS, recording-uploader, exporter'ов — в Ansible-ролях для версионирования и переиспользования (например, для molecule-тестов и для re-provisioning standalone-инсталляции).

**Что делаем:**

#### 2.1. `deployments/ansible/freeswitch/playbook.yml`

```yaml
---
- name: Provision SocioPulse FreeSWITCH node
  hosts: all
  become: true
  gather_facts: true

  vars:
    freeswitch_version: "1.10.10"
    freeswitch_user: freeswitch
    freeswitch_group: freeswitch
    spool_dir: /var/spool/sociopulse
    consent_prompt_path: /usr/share/sociopulse/consent_prompt.wav
    recording_uploader_user: recording-uploader
    recording_uploader_group: recording-uploader
    recording_uploader_install_dir: /usr/local/bin
    recording_uploader_state_dir: /var/lib/recording-uploader

  pre_tasks:
    - name: Wait for cloud-init
      ansible.builtin.command: cloud-init status --wait
      changed_when: false

  roles:
    - role: common
      tags: [common]
    - role: freeswitch
      tags: [freeswitch]
    - role: recording_uploader
      tags: [uploader]
    - role: observability
      tags: [observability]

  post_tasks:
    - name: Final disk cleanup
      ansible.builtin.shell: |
        apt-get autoremove -y
        apt-get clean
        find /var/log -type f -name "*.log" -exec truncate -s 0 {} +
      changed_when: false
```

#### 2.2. Role: `common`

`deployments/ansible/freeswitch/roles/common/tasks/main.yml`:

```yaml
---
- name: Install base packages
  ansible.builtin.apt:
    name:
      - ca-certificates
      - curl
      - gnupg2
      - software-properties-common
      - python3-pip
      - jq
      - chrony
      - rsyslog
      - ufw
      - tcpdump
      - htop
      - iotop
      - sysstat
    update_cache: true
    state: present

- name: Set timezone to Europe/Moscow
  community.general.timezone:
    name: Europe/Moscow

- name: Configure chrony for accurate time (RTP timestamps depend on it)
  ansible.builtin.template:
    src: chrony.conf.j2
    dest: /etc/chrony/chrony.conf
    owner: root
    group: root
    mode: "0644"
  notify: restart chrony

- name: Set kernel network parameters for SIP/RTP
  ansible.posix.sysctl:
    name: "{{ item.name }}"
    value: "{{ item.value }}"
    state: present
    sysctl_set: true
    reload: true
  loop:
    - { name: "net.core.rmem_max", value: "26214400" }
    - { name: "net.core.wmem_max", value: "26214400" }
    - { name: "net.core.rmem_default", value: "26214400" }
    - { name: "net.core.wmem_default", value: "26214400" }
    - { name: "net.core.netdev_max_backlog", value: "5000" }
    - { name: "net.ipv4.udp_mem", value: "65536 131072 262144" }
    - { name: "net.ipv4.ip_local_port_range", value: "16384 65535" }
    - { name: "net.ipv4.tcp_keepalive_time", value: "60" }
    - { name: "fs.file-max", value: "1048576" }

- name: Increase open files limits for freeswitch
  community.general.pam_limits:
    domain: "*"
    limit_type: "{{ item }}"
    limit_item: nofile
    value: "1000000"
  loop: [soft, hard]

- name: Create system users — freeswitch and recording-uploader
  ansible.builtin.user:
    name: "{{ item }}"
    system: true
    shell: /usr/sbin/nologin
    create_home: false
    state: present
  loop:
    - "{{ freeswitch_user }}"
    - "{{ recording_uploader_user }}"

- name: Create spool directory
  ansible.builtin.file:
    path: "{{ spool_dir }}"
    state: directory
    owner: "{{ freeswitch_user }}"
    group: "{{ recording_uploader_group }}"
    mode: "2770"

- name: Configure ufw default policy
  community.general.ufw:
    state: enabled
    policy: deny
    direction: incoming
```

`roles/common/handlers/main.yml`:

```yaml
---
- name: restart chrony
  ansible.builtin.service:
    name: chrony
    state: restarted
```

`roles/common/templates/chrony.conf.j2`:

```jinja
# Managed by Ansible — SocioPulse common role
pool ntp.time.in.ua iburst maxsources 4
pool 0.ru.pool.ntp.org iburst maxsources 2
driftfile /var/lib/chrony/chrony.drift
makestep 1.0 3
rtcsync
logdir /var/log/chrony
```

#### 2.3. Role: `freeswitch`

`roles/freeswitch/tasks/main.yml`:

```yaml
---
- name: Install FreeSWITCH from SignalWire repo
  ansible.builtin.import_tasks: install.yml
  tags: [freeswitch, install]

- name: Configure FreeSWITCH XML
  ansible.builtin.import_tasks: configure.yml
  tags: [freeswitch, configure]

- name: Generate DTLS certificates
  ansible.builtin.import_tasks: dtls.yml
  tags: [freeswitch, tls]

- name: Install systemd unit
  ansible.builtin.import_tasks: systemd.yml
  tags: [freeswitch, systemd]
```

`roles/freeswitch/tasks/install.yml`:

```yaml
---
- name: Add SignalWire APT key
  ansible.builtin.get_url:
    url: https://files.freeswitch.org/repo/deb/debian-release/signalwire-freeswitch-repo.gpg
    dest: /usr/share/keyrings/signalwire-freeswitch.gpg
    mode: "0644"

- name: Get SignalWire access token from environment
  ansible.builtin.set_fact:
    signalwire_token: "{{ lookup('env', 'SIGNALWIRE_TOKEN') | default(signalwire_token_default, true) }}"

- name: Configure SignalWire APT auth
  ansible.builtin.copy:
    dest: /etc/apt/auth.conf.d/signalwire.conf
    content: |
      machine freeswitch.signalwire.com
      login signalwire
      password {{ signalwire_token }}
    owner: root
    group: root
    mode: "0600"
  no_log: true

- name: Add SignalWire FreeSWITCH apt repository
  ansible.builtin.apt_repository:
    repo: "deb [signed-by=/usr/share/keyrings/signalwire-freeswitch.gpg] https://freeswitch.signalwire.com/repo/deb/debian-release/ jammy main"
    filename: signalwire-freeswitch
    state: present
    update_cache: true

- name: Install FreeSWITCH and modules
  ansible.builtin.apt:
    name:
      - freeswitch={{ freeswitch_version }}-*
      - freeswitch-conf-vanilla
      - freeswitch-mod-sofia
      - freeswitch-mod-event-socket
      - freeswitch-mod-commands
      - freeswitch-mod-dialplan-xml
      - freeswitch-mod-dptools
      - freeswitch-mod-logfile
      - freeswitch-mod-console
      - freeswitch-mod-spandsp
      - freeswitch-mod-tone-stream
      - freeswitch-mod-local-stream
      - freeswitch-mod-native-file
      - freeswitch-mod-sndfile
      - freeswitch-mod-loopback
      - freeswitch-mod-verto
      - freeswitch-mod-rtc
      - freeswitch-mod-cdr-csv
      - freeswitch-mod-xml-cdr
      - freeswitch-music-8000
      - freeswitch-sounds-en-us-callie
      - freeswitch-sounds-ru
    state: present
    install_recommends: false

- name: Install ffmpeg for transcoding
  ansible.builtin.apt:
    name:
      - ffmpeg
      - libopus0
      - libopusenc0
    state: present

- name: Stop default freeswitch.service shipped by package
  ansible.builtin.systemd:
    name: freeswitch
    enabled: false
    state: stopped
  failed_when: false
```

`roles/freeswitch/tasks/configure.yml`:

```yaml
---
- name: Ensure /etc/freeswitch hierarchy exists
  ansible.builtin.file:
    path: "/etc/freeswitch/{{ item }}"
    state: directory
    owner: "{{ freeswitch_user }}"
    group: "{{ freeswitch_group }}"
    mode: "0750"
  loop:
    - autoload_configs
    - sip_profiles
    - sip_profiles/external
    - dialplan
    - dialplan/default
    - directory
    - directory/default

- name: Render switch.conf.xml
  ansible.builtin.template:
    src: switch.conf.xml.j2
    dest: /etc/freeswitch/autoload_configs/switch.conf.xml
    owner: "{{ freeswitch_user }}"
    group: "{{ freeswitch_group }}"
    mode: "0640"
  notify: restart freeswitch

- name: Render event_socket.conf.xml
  ansible.builtin.template:
    src: event_socket.conf.xml.j2
    dest: /etc/freeswitch/autoload_configs/event_socket.conf.xml
    owner: "{{ freeswitch_user }}"
    group: "{{ freeswitch_group }}"
    mode: "0640"
  notify: restart freeswitch

- name: Render modules.conf.xml
  ansible.builtin.template:
    src: modules.conf.xml.j2
    dest: /etc/freeswitch/autoload_configs/modules.conf.xml
    owner: "{{ freeswitch_user }}"
    group: "{{ freeswitch_group }}"
    mode: "0640"
  notify: restart freeswitch

- name: Render verto.conf.xml
  ansible.builtin.template:
    src: verto.conf.xml.j2
    dest: /etc/freeswitch/autoload_configs/verto.conf.xml
    owner: "{{ freeswitch_user }}"
    group: "{{ freeswitch_group }}"
    mode: "0640"
  notify: restart freeswitch

- name: Render trunks SIP profile
  ansible.builtin.template:
    src: sip_profiles_trunks.xml.j2
    dest: /etc/freeswitch/sip_profiles/trunks.xml
    owner: "{{ freeswitch_user }}"
    group: "{{ freeswitch_group }}"
    mode: "0640"
  notify: restart freeswitch

- name: Render operators SIP profile
  ansible.builtin.template:
    src: sip_profiles_operators.xml.j2
    dest: /etc/freeswitch/sip_profiles/operators.xml
    owner: "{{ freeswitch_user }}"
    group: "{{ freeswitch_group }}"
    mode: "0640"
  notify: restart freeswitch

- name: Render default dialplan
  ansible.builtin.template:
    src: dialplan_default.xml.j2
    dest: /etc/freeswitch/dialplan/default.xml
    owner: "{{ freeswitch_user }}"
    group: "{{ freeswitch_group }}"
    mode: "0640"
  notify: restart freeswitch

- name: Copy consent prompt WAV
  ansible.builtin.copy:
    src: consent_prompt.wav
    dest: "{{ consent_prompt_path }}"
    owner: root
    group: "{{ freeswitch_group }}"
    mode: "0644"

- name: Create FS log dir
  ansible.builtin.file:
    path: /var/log/freeswitch
    state: directory
    owner: "{{ freeswitch_user }}"
    group: "{{ freeswitch_group }}"
    mode: "0750"
```

`roles/freeswitch/tasks/dtls.yml`:

```yaml
---
- name: Ensure DTLS cert directory exists
  ansible.builtin.file:
    path: /etc/freeswitch/tls
    state: directory
    owner: "{{ freeswitch_user }}"
    group: "{{ freeswitch_group }}"
    mode: "0750"

- name: Generate self-signed DTLS cert (replaced by Lockbox-issued cert in prod)
  ansible.builtin.command:
    cmd: >
      openssl req -x509 -newkey rsa:2048 -nodes -days 365
      -keyout /etc/freeswitch/tls/dtls-srtp.key
      -out /etc/freeswitch/tls/dtls-srtp.crt
      -subj "/C=RU/ST=Moscow/O=SocioPulse/CN={{ inventory_hostname }}"
    creates: /etc/freeswitch/tls/dtls-srtp.crt

- name: Combine cert+key into agent.pem (FS expects single PEM)
  ansible.builtin.shell: |
    cat /etc/freeswitch/tls/dtls-srtp.key /etc/freeswitch/tls/dtls-srtp.crt > /etc/freeswitch/tls/agent.pem
    chmod 0640 /etc/freeswitch/tls/agent.pem
    chown {{ freeswitch_user }}:{{ freeswitch_group }} /etc/freeswitch/tls/agent.pem
  args:
    creates: /etc/freeswitch/tls/agent.pem

- name: Generate WSS/Verto TLS material (overridden by Lockbox in prod via cloud-init drop-ins)
  ansible.builtin.command:
    cmd: >
      openssl req -x509 -newkey rsa:2048 -nodes -days 365
      -keyout /etc/freeswitch/tls/wss.key
      -out /etc/freeswitch/tls/wss.crt
      -subj "/C=RU/ST=Moscow/O=SocioPulse/CN={{ inventory_hostname }}"
    creates: /etc/freeswitch/tls/wss.crt
```

`roles/freeswitch/tasks/systemd.yml`:

```yaml
---
- name: Install custom systemd unit for FreeSWITCH
  ansible.builtin.copy:
    src: sociopulse-fs.service
    dest: /etc/systemd/system/freeswitch.service
    owner: root
    group: root
    mode: "0644"
  notify:
    - daemon-reload
    - restart freeswitch

- name: Enable freeswitch service
  ansible.builtin.systemd:
    name: freeswitch
    enabled: true
    daemon_reload: true
```

`roles/freeswitch/handlers/main.yml`:

```yaml
---
- name: daemon-reload
  ansible.builtin.systemd:
    daemon_reload: true

- name: restart freeswitch
  ansible.builtin.systemd:
    name: freeswitch
    state: restarted
  listen: restart freeswitch
```

`roles/freeswitch/files/sociopulse-fs.service`:

```ini
[Unit]
Description=SocioPulse FreeSWITCH
After=network-online.target chrony.service
Wants=network-online.target

[Service]
Type=forking
PIDFile=/run/freeswitch/freeswitch.pid
RuntimeDirectory=freeswitch
RuntimeDirectoryMode=0755
User=freeswitch
Group=freeswitch
LimitNOFILE=1000000
LimitNPROC=60000
LimitRTPRIO=99
LimitRTTIME=-1
LimitSTACK=240
LimitCORE=infinity
IOSchedulingClass=realtime
IOSchedulingPriority=2
CPUSchedulingPolicy=rr
CPUSchedulingPriority=89
CPUSchedulingResetOnFork=true
ExecStart=/usr/bin/freeswitch -ncwait -nonat -reincarnate -rp -nf -u freeswitch -g freeswitch
ExecReload=/usr/bin/fs_cli -x reloadxml
TimeoutStopSec=45
Restart=on-failure
RestartSec=5
ProtectSystem=full
PrivateTmp=true
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
```

`roles/freeswitch/files/consent_prompt.wav` — реальный аудиофайл (PCM 16-bit, 8 kHz, mono, ~10 секунд):

> "Здравствуйте! Вы дозвонились в SocioPulse. В целях контроля качества обслуживания этот разговор может быть записан. Если вы не согласны на запись — пожалуйста, повесьте трубку. Сейчас вы будете соединены с оператором."

(Файл хранится в репо как binary asset; в плане-файле прописываем требования: 16-bit PCM mono 8 kHz; см. <https://files.freeswitch.org/sounds/> формат.)

#### 2.4. Role: `recording_uploader`

`roles/recording_uploader/tasks/main.yml`:

```yaml
---
- name: Create recording-uploader state directory
  ansible.builtin.file:
    path: "{{ recording_uploader_state_dir }}"
    state: directory
    owner: "{{ recording_uploader_user }}"
    group: "{{ recording_uploader_group }}"
    mode: "0750"

- name: Add recording-uploader to spool group (read access)
  ansible.builtin.user:
    name: "{{ recording_uploader_user }}"
    groups: "{{ recording_uploader_group }}"
    append: true

- name: Copy recording-uploader binary
  ansible.builtin.copy:
    src: recording-uploader
    dest: "{{ recording_uploader_install_dir }}/recording-uploader"
    owner: root
    group: root
    mode: "0755"

- name: Render recording-uploader env file
  ansible.builtin.template:
    src: recording-uploader.env.j2
    dest: /etc/default/recording-uploader
    owner: root
    group: "{{ recording_uploader_group }}"
    mode: "0640"
  notify: restart recording-uploader

- name: Render recording-uploader systemd unit
  ansible.builtin.template:
    src: recording-uploader.service.j2
    dest: /etc/systemd/system/recording-uploader.service
    owner: root
    group: root
    mode: "0644"
  notify:
    - daemon-reload
    - restart recording-uploader

- name: Enable recording-uploader
  ansible.builtin.systemd:
    name: recording-uploader
    enabled: true
    daemon_reload: true
```

`roles/recording_uploader/templates/recording-uploader.env.j2`:

```bash
# Managed by Ansible
RECORDING_UPLOADER_SPOOL_DIR={{ spool_dir }}
RECORDING_UPLOADER_STATE_DIR={{ recording_uploader_state_dir }}
RECORDING_UPLOADER_S3_ENDPOINT=https://storage.yandexcloud.net
RECORDING_UPLOADER_S3_BUCKET={{ s3_bucket_recordings | default("sociopulse-recordings-" + env) }}
RECORDING_UPLOADER_S3_PREFIX=recordings
RECORDING_UPLOADER_KMS_KEY_ID={{ kms_key_recordings_id }}
RECORDING_UPLOADER_RECORDER_GRPC_ADDR={{ recorder_grpc_addr | default("recorder.svc.sociopulse.internal:443") }}
RECORDING_UPLOADER_RECORDER_GRPC_TLS_CA=/etc/freeswitch/tls/internal_ca.crt
RECORDING_UPLOADER_RECORDER_GRPC_TLS_CERT=/etc/freeswitch/tls/uploader.crt
RECORDING_UPLOADER_RECORDER_GRPC_TLS_KEY=/etc/freeswitch/tls/uploader.key
RECORDING_UPLOADER_FFMPEG_PATH=/usr/bin/ffmpeg
RECORDING_UPLOADER_OPUS_BITRATE=32k
RECORDING_UPLOADER_OPUS_SAMPLE_RATE=16000
RECORDING_UPLOADER_RETRY_INITIAL=5s
RECORDING_UPLOADER_RETRY_MAX=5m
RECORDING_UPLOADER_RETRY_ATTEMPTS=100
RECORDING_UPLOADER_METRICS_ADDR=:9091
RECORDING_UPLOADER_LOG_LEVEL=info
RECORDING_UPLOADER_LOG_FORMAT=json
RECORDING_UPLOADER_DELETE_AFTER_ACK=true
RECORDING_UPLOADER_INSTANCE_ID={{ ansible_hostname }}
```

`roles/recording_uploader/templates/recording-uploader.service.j2`:

```ini
[Unit]
Description=SocioPulse Recording Uploader
After=network-online.target freeswitch.service
Wants=network-online.target
PartOf=freeswitch.service

[Service]
Type=simple
User={{ recording_uploader_user }}
Group={{ recording_uploader_group }}
EnvironmentFile=/etc/default/recording-uploader
WorkingDirectory={{ recording_uploader_state_dir }}
ExecStart={{ recording_uploader_install_dir }}/recording-uploader
Restart=on-failure
RestartSec=10
LimitNOFILE=65536
ProtectSystem=full
ProtectHome=true
PrivateTmp=true
NoNewPrivileges=true
ReadWritePaths={{ spool_dir }} {{ recording_uploader_state_dir }}

[Install]
WantedBy=multi-user.target
```

`roles/recording_uploader/handlers/main.yml`:

```yaml
---
- name: daemon-reload
  ansible.builtin.systemd:
    daemon_reload: true

- name: restart recording-uploader
  ansible.builtin.systemd:
    name: recording-uploader
    state: restarted
```

#### 2.5. Role: `observability`

`roles/observability/tasks/main.yml`:

```yaml
---
- name: Install node_exporter
  ansible.builtin.apt:
    name: prometheus-node-exporter
    state: present

- name: Render node_exporter override
  ansible.builtin.template:
    src: node_exporter.service.j2
    dest: /etc/systemd/system/prometheus-node-exporter.service.d/override.conf
    owner: root
    group: root
    mode: "0644"
  notify:
    - daemon-reload
    - restart node_exporter

- name: Install blackbox_exporter
  ansible.builtin.apt:
    name: prometheus-blackbox-exporter
    state: present

- name: Render blackbox_exporter config
  ansible.builtin.template:
    src: blackbox_exporter.yml.j2
    dest: /etc/prometheus/blackbox.yml
    owner: root
    group: root
    mode: "0644"
  notify: restart blackbox_exporter

- name: Enable exporters
  ansible.builtin.systemd:
    name: "{{ item }}"
    enabled: true
    daemon_reload: true
  loop:
    - prometheus-node-exporter
    - prometheus-blackbox-exporter
```

`roles/observability/templates/blackbox_exporter.yml.j2`:

```yaml
modules:
  sip_options_udp:
    prober: tcp
    timeout: 5s
    tcp:
      preferred_ip_protocol: ip4
  freeswitch_esl:
    prober: tcp
    timeout: 5s
    tcp:
      preferred_ip_protocol: ip4
  http_2xx:
    prober: http
    timeout: 5s
    http:
      preferred_ip_protocol: ip4
      valid_status_codes: [200]
```

#### 2.6. Molecule-тест

`deployments/ansible/freeswitch/molecule/default/molecule.yml`:

```yaml
---
dependency:
  name: galaxy
driver:
  name: docker
platforms:
  - name: fs-test
    image: geerlingguy/docker-ubuntu2204-ansible:latest
    pre_build_image: true
    privileged: true
    volumes:
      - /sys/fs/cgroup:/sys/fs/cgroup:rw
    cgroupns_mode: host
    command: /lib/systemd/systemd
provisioner:
  name: ansible
  inventory:
    group_vars:
      all:
        signalwire_token_default: "test-token"
        kms_key_recordings_id: "test-kms-id"
        env: dev
        git_sha: test
        recording_uploader_binary: /tmp/recording-uploader
verifier:
  name: ansible
```

`molecule/default/converge.yml`:

```yaml
---
- name: Converge
  hosts: all
  pre_tasks:
    - name: Pre-create dummy uploader binary so role idempotency tests pass
      ansible.builtin.copy:
        content: "#!/bin/sh\nexit 0\n"
        dest: /tmp/recording-uploader
        mode: "0755"
  tasks:
    - name: Include playbook
      ansible.builtin.import_playbook: ../../playbook.yml
```

`molecule/default/verify.yml`:

```yaml
---
- name: Verify
  hosts: all
  tasks:
    - name: Check freeswitch installed
      ansible.builtin.command: dpkg -s freeswitch
      changed_when: false

    - name: Check freeswitch service file exists
      ansible.builtin.stat:
        path: /etc/systemd/system/freeswitch.service
      register: fs_unit
      failed_when: not fs_unit.stat.exists

    - name: Check recording-uploader service file
      ansible.builtin.stat:
        path: /etc/systemd/system/recording-uploader.service
      register: ru_unit
      failed_when: not ru_unit.stat.exists

    - name: Check sip_profiles trunks file
      ansible.builtin.stat:
        path: /etc/freeswitch/sip_profiles/trunks.xml
      register: trunks_xml
      failed_when: not trunks_xml.stat.exists

    - name: Validate XML syntax of dialplan
      ansible.builtin.command: xmllint --noout /etc/freeswitch/dialplan/default.xml
      changed_when: false
```

#### 2.7. Тесты

```bash
cd deployments/ansible/freeswitch
ansible-lint .
yamllint .
molecule test
```

**Acceptance:**
- [ ] `ansible-lint` без ошибок уровня `error`.
- [ ] `yamllint` чистый.
- [ ] `molecule test` проходит за < 5 минут.
- [ ] После molecule-converge: `dpkg -s freeswitch` shows installed.
- [ ] `xmllint` валидирует все рендеры шаблонов.
- [ ] Idempotency check (Molecule встроенный) — второй прогон без `changed`.

---

### Task 3 — FreeSWITCH XML-конфиги

**Зачем:** реальные конфиги, без placeholder-ов. Покрывают: switch.conf (RT-prio, max-sessions=200), event_socket :8021 mTLS, два SIP-профиля (`trunks` для входящих от телеком-провайдеров, `operators` для verto-клиентов), modules.conf, verto.conf.

**Что делаем:**

#### 3.1. `roles/freeswitch/templates/switch.conf.xml.j2`

```xml
<configuration name="switch.conf" description="Core Configuration — managed by Ansible">
  <cli-keybindings>
    <key name="1" value="help"/>
    <key name="2" value="status"/>
    <key name="3" value="show channels"/>
    <key name="4" value="show calls"/>
    <key name="5" value="sofia status"/>
    <key name="6" value="reloadxml"/>
    <key name="7" value="console loglevel 0"/>
    <key name="8" value="console loglevel 7"/>
  </cli-keybindings>

  <default-ptimes/>

  <settings>
    <param name="colorize-console" value="false"/>

    <!-- Core thread RT scheduling -->
    <param name="run-as" value="freeswitch"/>
    <param name="enable-monotonic-timing" value="true"/>
    <param name="rtp-enable-zrtp" value="false"/>
    <param name="enable-clock-nanosleep" value="true"/>
    <param name="rtp-start-port" value="16384"/>
    <param name="rtp-end-port" value="32768"/>
    <param name="max-sessions" value="200"/>
    <param name="sessions-per-second" value="50"/>
    <param name="loglevel" value="info"/>
    <param name="dump-cores" value="yes"/>
    <param name="mailer-app" value="false"/>
    <param name="mailer-app-args" value=""/>
    <param name="dialplan-timestamps" value="true"/>
    <param name="max-db-handles" value="50"/>
    <param name="db-handle-timeout" value="10"/>
    <param name="multiple-registrations" value="contact"/>
    <param name="rtp-port-usage-robustness" value="true"/>

    <!-- 152-ФЗ + storage paths -->
    <param name="recordings-dir" value="/var/spool/sociopulse"/>
    <param name="sound-prefix" value="/usr/share/freeswitch/sounds"/>
    <param name="storage-dir" value="/var/lib/freeswitch/storage"/>
    <param name="cache-dir" value="/var/cache/freeswitch"/>
    <param name="log-dir" value="/var/log/freeswitch"/>

    <!-- Variables for dialplan templating (read by ${var}) -->
    <variable name="default_areacode" value="495"/>
    <variable name="default_country" value="RU"/>
    <variable name="hold_music" value="local_stream://moh"/>
    <variable name="recording_consent_prompt_url" value="file://{{ consent_prompt_path }}"/>
    <variable name="bind_server_ip" value="auto-nat"/>
    <variable name="external_rtp_ip" value="autonat:auto-nat"/>
    <variable name="external_sip_ip" value="autonat:auto-nat"/>
  </settings>
</configuration>
```

#### 3.2. `roles/freeswitch/templates/modules.conf.xml.j2`

```xml
<configuration name="modules.conf" description="Modules — managed by Ansible">
  <modules>
    <load module="mod_console"/>
    <load module="mod_logfile"/>
    <load module="mod_xml_cdr"/>
    <load module="mod_cdr_csv"/>

    <!-- SIP -->
    <load module="mod_sofia"/>

    <!-- WebRTC operator clients -->
    <load module="mod_verto"/>
    <load module="mod_rtc"/>

    <!-- ESL -->
    <load module="mod_event_socket"/>

    <!-- Codecs -->
    <load module="mod_g711"/>
    <load module="mod_g729"/>
    <load module="mod_opus"/>

    <!-- Dialplan -->
    <load module="mod_dialplan_xml"/>
    <load module="mod_commands"/>
    <load module="mod_dptools"/>
    <load module="mod_loopback"/>

    <!-- Codec apps & file IO -->
    <load module="mod_sndfile"/>
    <load module="mod_native_file"/>
    <load module="mod_local_stream"/>
    <load module="mod_tone_stream"/>
    <load module="mod_spandsp"/>

    <!-- Recording -->
    <load module="mod_record"/>
    <load module="mod_say_en"/>
    <load module="mod_say_ru"/>
  </modules>
</configuration>
```

#### 3.3. `roles/freeswitch/templates/event_socket.conf.xml.j2`

```xml
<configuration name="event_socket.conf" description="Event Socket — managed by Ansible">
  <settings>
    <!-- Listen only on VPC subnet, never public -->
    <param name="nat-map" value="false"/>
    <param name="listen-ip" value="{{ ansible_default_ipv4.address }}"/>
    <param name="listen-port" value="8021"/>
    <param name="password" value="{{ esl_password }}"/>
    <param name="apply-inbound-acl" value="vpc-internal"/>

    <!-- mTLS via stunnel sidecar (FS event_socket itself does not natively do TLS;
         systemd unit стартует stunnel перед freeswitch.service для wrap-а на :8821) -->
    <param name="stop-on-bind-error" value="true"/>
  </settings>
</configuration>
```

> Примечание: ESL TLS-обёртка делается через `stunnel` sidecar — добавляем в Ansible role:

`roles/freeswitch/tasks/configure.yml` (append):

```yaml
- name: Install stunnel for ESL TLS termination
  ansible.builtin.apt:
    name: stunnel4
    state: present

- name: Render stunnel config for ESL
  ansible.builtin.copy:
    dest: /etc/stunnel/esl.conf
    content: |
      foreground = no
      pid = /var/run/stunnel-esl.pid
      [esl]
      accept = 0.0.0.0:8821
      connect = 127.0.0.1:8021
      cert = /etc/freeswitch/tls/agent.pem
      verify = 4
      CAfile = /etc/freeswitch/tls/internal_ca.crt
    owner: root
    group: root
    mode: "0640"
  notify: restart stunnel
```

И ACL `vpc-internal` — в `acl.conf.xml`:

`roles/freeswitch/templates/acl.conf.xml.j2`:

```xml
<configuration name="acl.conf" description="ACLs">
  <network-lists>
    <list name="vpc-internal" default="deny">
      <node type="allow" cidr="{{ vpc_cidr | default('10.128.0.0/9') }}"/>
      <node type="allow" cidr="127.0.0.1/32"/>
    </list>
    <list name="operator-trunks" default="deny">
      {% for ip in operator_sip_trunk_ips | default([]) %}
      <node type="allow" cidr="{{ ip }}/32"/>
      {% endfor %}
    </list>
  </network-lists>
</configuration>
```

(Этот рендер тоже включаем в `configure.yml` — записывается в `/etc/freeswitch/autoload_configs/acl.conf.xml`.)

#### 3.4. `roles/freeswitch/templates/sip_profiles_trunks.xml.j2`

```xml
<profile name="trunks">
  <aliases>
    <alias name="trunks"/>
  </aliases>

  <gateways>
    {% for trunk in sip_trunks | default([]) %}
    <gateway name="{{ trunk.id }}">
      <param name="username" value="{{ trunk.username }}"/>
      <param name="realm" value="{{ trunk.realm }}"/>
      <param name="password" value="{{ trunk.password }}"/>
      <param name="proxy" value="{{ trunk.proxy }}"/>
      <param name="register" value="{{ trunk.register | default('true') }}"/>
      <param name="register-transport" value="{{ trunk.register_transport | default('udp') }}"/>
      <param name="expire-seconds" value="3600"/>
      <param name="retry-seconds" value="30"/>
      <param name="ping" value="60"/>
      <variables>
        <variable name="sip_trunk_id" value="{{ trunk.id }}" direction="inbound"/>
        <variable name="trunk_provider" value="{{ trunk.provider | default('unknown') }}" direction="inbound"/>
      </variables>
    </gateway>
    {% endfor %}
  </gateways>

  <settings>
    <param name="user-agent-string" value="SocioPulse-FS/1.0"/>
    <param name="debug" value="0"/>
    <param name="sip-trace" value="no"/>
    <param name="context" value="default"/>
    <param name="rfc2833-pt" value="101"/>
    <param name="sip-port" value="5060"/>
    <param name="dialplan" value="XML"/>
    <param name="dtmf-duration" value="2000"/>
    <param name="rtp-timer-name" value="soft"/>
    <param name="rtp-ip" value="$${external_rtp_ip}"/>
    <param name="sip-ip" value="$${external_sip_ip}"/>
    <param name="ext-rtp-ip" value="auto-nat"/>
    <param name="ext-sip-ip" value="auto-nat"/>
    <param name="hold-music" value="$${hold_music}"/>
    <param name="local-network-acl" value="vpc-internal"/>
    <param name="apply-inbound-acl" value="operator-trunks"/>
    <param name="apply-register-acl" value="operator-trunks"/>
    <param name="manage-presence" value="false"/>
    <param name="inbound-codec-prefs" value="OPUS,PCMA,PCMU,G729"/>
    <param name="outbound-codec-prefs" value="OPUS,PCMA,PCMU,G729"/>
    <param name="rtp-rewrite-timestamps" value="true"/>
    <param name="rtp-autoflush-during-bridge" value="true"/>
    <param name="auth-calls" value="true"/>
    <param name="auth-all-packets" value="false"/>
    <param name="inbound-reg-force-matching-username" value="true"/>
    <param name="disable-transcoding" value="false"/>
    <param name="disable-transfer" value="true"/>
  </settings>
</profile>
```

#### 3.5. `roles/freeswitch/templates/sip_profiles_operators.xml.j2`

> Для операторов используется отдельный профиль на :8082 c WSS+verto. mTLS обеспечивается на уровне verto.conf.xml (см. ниже) и реверс-прокси не требуется — verto держит DTLS-SRTP сам.

```xml
<profile name="operators">
  <aliases>
    <alias name="operators"/>
  </aliases>

  <settings>
    <param name="user-agent-string" value="SocioPulse-FS-OP/1.0"/>
    <param name="debug" value="0"/>
    <param name="context" value="operator"/>
    <param name="dialplan" value="XML"/>
    <param name="rfc2833-pt" value="101"/>
    <!-- Operators connect via Verto (WSS) — see verto.conf.xml. SIP profile here exists
         only as a fallback for SIP-OPTIONS keepalive from operator-side endpoints. -->
    <param name="sip-port" value="5061"/>
    <param name="tls" value="true"/>
    <param name="tls-only" value="true"/>
    <param name="tls-bind-params" value="transport=tls"/>
    <param name="tls-sip-port" value="5061"/>
    <param name="tls-cert-dir" value="/etc/freeswitch/tls"/>
    <param name="tls-passphrase" value=""/>
    <param name="tls-verify-policy" value="all"/>
    <param name="tls-verify-depth" value="3"/>
    <param name="tls-version" value="tlsv1.2,tlsv1.3"/>
    <param name="rtp-ip" value="$${external_rtp_ip}"/>
    <param name="sip-ip" value="$${external_sip_ip}"/>
    <param name="rtp-timer-name" value="soft"/>
    <param name="hold-music" value="$${hold_music}"/>
    <param name="local-network-acl" value="vpc-internal"/>
    <param name="apply-inbound-acl" value="vpc-internal"/>
    <param name="manage-presence" value="false"/>
    <param name="inbound-codec-prefs" value="OPUS,PCMA"/>
    <param name="outbound-codec-prefs" value="OPUS,PCMA"/>
    <param name="auth-calls" value="true"/>
  </settings>
</profile>
```

#### 3.6. `roles/freeswitch/templates/verto.conf.xml.j2`

```xml
<configuration name="verto.conf" description="Verto — managed by Ansible">
  <settings>
    <param name="debug" value="0"/>
    <param name="enable-presence" value="false"/>
    <param name="detach-on-park" value="true"/>
  </settings>

  <profiles>
    <profile name="default-v4">
      <param name="bind-local" value="$${external_sip_ip}:8081"/>
      <param name="bind-local" value="$${external_sip_ip}:8082" secure="true"/>
      <param name="secure-combined" value="/etc/freeswitch/tls/wss.crt"/>
      <param name="secure-key" value="/etc/freeswitch/tls/wss.key"/>
      <param name="userauth" value="true"/>
      <param name="context" value="operator"/>
      <param name="outbound-codec-string" value="opus,pcma,pcmu"/>
      <param name="inbound-codec-string" value="opus,pcma,pcmu"/>
      <param name="apply-candidate-acl" value="vpc-internal"/>
      <param name="apply-candidate-acl" value="operator-clients"/>
      <param name="rtp-ip" value="$${external_rtp_ip}"/>
      <param name="ext-rtp-ip" value="auto-nat"/>
      <param name="local-network" value="vpc-internal"/>
      <param name="dtls-version" value="auto"/>
      <param name="enable-3pcc" value="true"/>
      <param name="enable-fs-ws-uds" value="false"/>
      <param name="mtls" value="true"/>
      <param name="mtls-ca-bundle" value="/etc/freeswitch/tls/operator_ca.crt"/>
    </profile>
  </profiles>
</configuration>
```

> ACL `operator-clients` дополняется в `acl.conf.xml.j2` через `operator_client_cidrs` Ansible-переменную, накатывается через Lockbox.

#### 3.7. Тесты для XML-конфигов

В Molecule добавляем (в verify.yml):

```yaml
- name: Validate switch.conf XML
  ansible.builtin.command: xmllint --noout /etc/freeswitch/autoload_configs/switch.conf.xml
  changed_when: false

- name: Validate trunks profile XML
  ansible.builtin.command: xmllint --noout /etc/freeswitch/sip_profiles/trunks.xml
  changed_when: false

- name: Validate operators profile XML
  ansible.builtin.command: xmllint --noout /etc/freeswitch/sip_profiles/operators.xml
  changed_when: false

- name: Run freeswitch syntax check (--check-config)
  ansible.builtin.command: freeswitch -syntax
  register: fs_syntax
  changed_when: false
  failed_when: fs_syntax.rc != 0

- name: Verify event_socket port bound to local IP
  ansible.builtin.shell: grep listen-ip /etc/freeswitch/autoload_configs/event_socket.conf.xml | grep -v 0.0.0.0
  changed_when: false
```

**Acceptance:**
- [ ] `xmllint` валидирует все 6 файлов конфигов.
- [ ] `freeswitch -syntax` возвращает 0.
- [ ] После старта `fs_cli -x "sofia status"` показывает оба профиля в `RUNNING`.
- [ ] `fs_cli -x "show modules"` содержит mod_sofia, mod_event_socket, mod_record, mod_dptools, mod_verto.
- [ ] `ss -tlnp | grep 5060` показывает freeswitch (UDP/TCP).
- [ ] `ss -tlnp | grep 5061` показывает freeswitch с TLS.
- [ ] `ss -tlnp | grep 8082` показывает verto WSS.
- [ ] `ss -tlnp | grep 8021` показывает event_socket bound только на VPC IP.

---

### Task 4 — Dialplan: согласие на запись + маршрутизация trunk → оператор

**Зачем:** реализация требований §FR-E (запись с уведомлением абонента) и §13.1 (152-ФЗ — согласие на запись). Логика:

1. Входящий вызов через `gateway/${sip_trunk_id}` попадает в context `default`.
2. По CallerID определяется `tenant_id` (из переменной gateway или из dialplan-DB lookup; в этом плане — через переменную из originate-команды backend'a).
3. Проигрывается WAV-промпт согласия (10 сек, `${recording_consent_prompt_url}`).
4. Запускается `record_session` (write to `/var/spool/sociopulse/${call_uuid}.wav`) или `mixmonitor` (для bridged-mixed audio).
5. `bridge user/${operator_sip_user}@${operator_fs_node}` — звонок оператору. Operator-target выбирается backend'ом и передаётся через originate-переменные.
6. После hangup — record_session останавливается, файл закрывается, fsnotify видит CLOSE_WRITE → recording-uploader забирает.

**Что делаем:**

#### 4.1. `roles/freeswitch/templates/dialplan_default.xml.j2`

```xml
<include>
  <context name="default">

    <!-- ============================================================
         INBOUND from trunk (provider) → consent prompt → bridge to operator
         Triggered by gateway in trunks SIP profile.
         Required headers/variables (set by backend via originate):
           - sip_trunk_id
           - tenant_id
           - call_id (UUID from CRM)
           - operator_sip_user
           - operator_fs_node
           - recording_required (true/false; default true)
           - consent_required (true/false; default true)
         ============================================================ -->

    <extension name="inbound_trunk_to_operator">
      <condition field="${sip_h_X-SocioPulse-Direction}" expression="^inbound$">
        <action application="set" data="hangup_after_bridge=true"/>
        <action application="set" data="continue_on_fail=NORMAL_TEMPORARY_FAILURE,USER_NOT_REGISTERED,NO_USER_RESPONSE,NORMAL_CLEARING"/>
        <action application="set" data="ringback=%(2000,4000,440,480)"/>
        <action application="set" data="transfer_ringback=$${hold_music}"/>
        <action application="set" data="call_timeout=45"/>
        <action application="set" data="origination_caller_id_name=${caller_id_name}"/>
        <action application="set" data="origination_caller_id_number=${caller_id_number}"/>

        <!-- Persist call meta to channel variables for ESL/CDR -->
        <action application="set" data="sociopulse_tenant_id=${tenant_id}"/>
        <action application="set" data="sociopulse_call_id=${call_id}"/>
        <action application="set" data="sociopulse_trunk_id=${sip_trunk_id}"/>
        <action application="set" data="sociopulse_node=${hostname}"/>

        <!-- Consent prompt only when required (default true).
             Customers in jurisdictions where consent prompt is regulator-required
             (RF 152-ФЗ) get it; backend may set consent_required=false for
             contexts already covered by signed contract (e.g. B2B). -->
        <action application="answer"/>
        <action application="sleep" data="500"/>
        <action application="execute_extension" data="play_consent_prompt XML default"/>

        <!-- Recording: file path = /var/spool/sociopulse/<call_id>.wav
             record_session captures all audio of channel; for mixed-leg recording
             after bridge мы используем mixmonitor (см. ниже). -->
        <action application="execute_extension" data="start_recording XML default"/>

        <!-- Originate to operator's verto endpoint. Bridge handle has its own UUID. -->
        <action application="set" data="bridge_pre_execute_aleg_app=preprocess_a_leg"/>
        <action application="bridge" data="user/${operator_sip_user}@${operator_fs_node}"/>

        <!-- Post-bridge hangup hook: stop recording, fire ESL event for backend ack -->
        <action application="execute_extension" data="post_bridge_cleanup XML default"/>
      </condition>
    </extension>

    <!-- ============================================================
         play_consent_prompt: only if consent_required is true
         ============================================================ -->
    <extension name="play_consent_prompt" continue="true">
      <condition field="${consent_required}" expression="^(true|1|yes)?$">
        <action application="log" data="INFO consent_prompt: playing for call=${sociopulse_call_id} tenant=${sociopulse_tenant_id}"/>
        <action application="playback" data="${recording_consent_prompt_url}"/>
        <action application="sleep" data="500"/>
      </condition>
    </extension>

    <!-- ============================================================
         start_recording: enable record_session OR mixmonitor.
         We use mixmonitor because we need both inbound (caller) and outbound
         (operator) audio mixed into a single file.
         ============================================================ -->
    <extension name="start_recording" continue="true">
      <condition field="${recording_required}" expression="^(true|1|yes)?$">
        <action application="set" data="RECORD_STEREO=true"/>
        <action application="set" data="RECORD_BRIDGE_REQ=true"/>
        <action application="set" data="RECORD_TITLE=SocioPulse-${sociopulse_call_id}"/>
        <action application="set" data="RECORD_COPYRIGHT=SocioPulse"/>
        <action application="set" data="RECORD_SOFTWARE=FreeSWITCH"/>
        <action application="set" data="RECORD_ARTIST=SocioPulse"/>
        <action application="set" data="RECORD_COMMENT=tenant=${sociopulse_tenant_id} trunk=${sociopulse_trunk_id} node=${sociopulse_node}"/>
        <action application="set" data="RECORD_DATE=${strftime(%Y-%m-%dT%H:%M:%S%z)}"/>
        <action application="set" data="recording_path=$${recordings_dir}/${sociopulse_call_id}.wav"/>
        <action application="set" data="api_hangup_hook=uuid_record ${uuid} stop ${recording_path}"/>
        <action application="log" data="INFO recording: starting mixmonitor path=${recording_path}"/>
        <action application="record_session" data="${recording_path}"/>
      </condition>
    </extension>

    <!-- ============================================================
         post_bridge_cleanup: ensure recording stopped, emit custom event
         ============================================================ -->
    <extension name="post_bridge_cleanup" continue="true">
      <condition field="${recording_path}" expression="^(.+)$">
        <action application="log" data="INFO recording: stopping path=${recording_path} reason=post_bridge"/>
        <action application="stop_record_session" data="${recording_path}"/>
        <action application="event" data="Event-Subclass=sociopulse::recording_finished,Event-Name=CUSTOM,call_id=${sociopulse_call_id},tenant_id=${sociopulse_tenant_id},path=${recording_path},node=${sociopulse_node}"/>
      </condition>
    </extension>

    <!-- ============================================================
         fallback: unknown destination
         ============================================================ -->
    <extension name="unknown">
      <condition field="destination_number" expression="^.*$">
        <action application="log" data="WARN dialplan: unknown destination=${destination_number}"/>
        <action application="respond" data="404 Not Found"/>
      </condition>
    </extension>

  </context>

  <!-- ============================================================
       OPERATOR context — used when operator-side leg connects (verto).
       Originate from backend → bridge to here → eventually back-to-back
       call leg into default context.
       ============================================================ -->
  <context name="operator">
    <extension name="operator_outbound_to_trunk">
      <condition field="destination_number" expression="^outbound:(.+)$">
        <action application="set" data="hangup_after_bridge=true"/>
        <action application="set" data="effective_caller_id_number=${operator_did_number}"/>
        <action application="set" data="effective_caller_id_name=${operator_did_name}"/>
        <action application="bridge" data="sofia/gateway/${sip_trunk_id}/$1"/>
      </condition>
    </extension>

    <extension name="operator_to_operator">
      <condition field="destination_number" expression="^op-(.+)$">
        <action application="bridge" data="user/$1@${operator_fs_node}"/>
      </condition>
    </extension>
  </context>
</include>
```

#### 4.2. Originate flow (для контракта c backend'ом, который пилится в Plan #09)

Backend через ESL/gRPC выполняет:

```
originate {sip_trunk_id=msk-megaphone,tenant_id=00000000-...,call_id=<uuid>,operator_sip_user=op42,operator_fs_node=fs-2.dev.sociopulse.ru,recording_required=true,consent_required=true,origination_uuid=<uuid>}sofia/gateway/msk-megaphone/74951234567 &transfer(inbound_trunk_to_operator XML default)
```

Это требование к Plan #09; здесь мы фиксируем контракт переменных и проверяем dialplan через SIPp-сценарий (Task 9).

#### 4.3. Тест dialplan через SIPp

`tests/sipp/uac_invite.xml`:

```xml
<?xml version="1.0" encoding="ISO-8859-1" ?>
<!DOCTYPE scenario SYSTEM "sipp.dtd">
<scenario name="SocioPulse inbound trunk smoke">
  <send retrans="500">
    <![CDATA[
      INVITE sip:5551234@[remote_ip]:[remote_port] SIP/2.0
      Via: SIP/2.0/[transport] [local_ip]:[local_port];branch=[branch]
      From: "Test Caller" <sip:74951234567@[local_ip]:[local_port]>;tag=[call_number]
      To: "SocioPulse" <sip:5551234@[remote_ip]:[remote_port]>
      Call-ID: [call_id]
      CSeq: 1 INVITE
      Contact: sip:74951234567@[local_ip]:[local_port]
      Max-Forwards: 70
      X-SocioPulse-Direction: inbound
      Subject: Performance Test
      Content-Type: application/sdp
      Content-Length: [len]

      v=0
      o=user1 53655765 2353687637 IN IP[local_ip_type] [local_ip]
      s=-
      c=IN IP[media_ip_type] [media_ip]
      t=0 0
      m=audio [media_port] RTP/AVP 0 8 101
      a=rtpmap:0 PCMU/8000
      a=rtpmap:8 PCMA/8000
      a=rtpmap:101 telephone-event/8000
    ]]>
  </send>

  <recv response="100" optional="true"/>
  <recv response="180" optional="true"/>
  <recv response="183" optional="true"/>
  <recv response="200" rtd="true"/>

  <send>
    <![CDATA[
      ACK sip:5551234@[remote_ip]:[remote_port] SIP/2.0
      Via: SIP/2.0/[transport] [local_ip]:[local_port];branch=[branch]
      From: "Test Caller" <sip:74951234567@[local_ip]:[local_port]>;tag=[call_number]
      To: <sip:5551234@[remote_ip]:[remote_port]>[peer_tag_param]
      Call-ID: [call_id]
      CSeq: 1 ACK
      Contact: sip:74951234567@[local_ip]:[local_port]
      Max-Forwards: 70
      Content-Length: 0
    ]]>
  </send>

  <pause milliseconds="11000"/>

  <send retrans="500">
    <![CDATA[
      BYE sip:5551234@[remote_ip]:[remote_port] SIP/2.0
      Via: SIP/2.0/[transport] [local_ip]:[local_port];branch=[branch]
      From: "Test Caller" <sip:74951234567@[local_ip]:[local_port]>;tag=[call_number]
      To: <sip:5551234@[remote_ip]:[remote_port]>[peer_tag_param]
      Call-ID: [call_id]
      CSeq: 2 BYE
      Contact: sip:74951234567@[local_ip]:[local_port]
      Max-Forwards: 70
      Content-Length: 0
    ]]>
  </send>

  <recv response="200"/>
  <ResponseTimeRepartition value="10, 20, 30, 40, 50, 100, 150, 200"/>
  <CallLengthRepartition value="10, 50, 100, 500, 1000, 5000, 10000"/>
</scenario>
```

После прогона `sipp -sn uac fs-1.dev.sociopulse.ru:5060 -m 1 -sf tests/sipp/uac_invite.xml` ожидаем в `/var/spool/sociopulse/` файл `<call_id>.wav` ≥ 80 KB (10 секунд ≈ 80 KB при 8kHz mono PCM).

**Acceptance:**
- [ ] Dialplan валидирует `freeswitch -syntax`.
- [ ] SIPp-сценарий проходит, 200 OK получен.
- [ ] WAV-файл создан в `/var/spool/sociopulse/`.
- [ ] Custom event `sociopulse::recording_finished` ловится через `fs_cli -x "/event plain CUSTOM sociopulse::recording_finished"`.
- [ ] При `consent_required=false` промпт не проигрывается (проверка через сценарий с переменной).
- [ ] При `recording_required=false` файл не создаётся.

---

### Task 5 — `cmd/recording-uploader` (Go-бинарь)

**Зачем:** §9 spec — после hangup WAV-файл должен попасть в S3 hot-tier зашифрованным. Делаем это **рядом** с FS, не на самом FS-процессе, чтобы не блокировать call-обработку. Сервис: fsnotify watcher → ffmpeg → AES-256-GCM → S3 PUT → gRPC `Recording.Commit` → удаление локальной копии после ack. TDD; тесты сначала.

**Что делаем:**

#### 5.1. `cmd/recording-uploader/main.go`

```go
// Package main — SocioPulse recording-uploader.
//
// Watches /var/spool/sociopulse for new WAV files written by FreeSWITCH,
// transcodes to Opus, encrypts client-side with KMS-wrapped data keys,
// uploads to Yandex Object Storage, and acknowledges via gRPC Recording.Commit.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/sociopulse/social-pulse/internal/uploader"
)

func main() {
	logger := uploader.NewLogger(os.Stdout, os.Getenv("RECORDING_UPLOADER_LOG_LEVEL"))
	slog.SetDefault(logger)

	cfg, err := uploader.LoadConfigFromEnv()
	if err != nil {
		slog.Error("load config", slog.String("err", err.Error()))
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	pipeline, err := uploader.NewPipeline(ctx, cfg, logger)
	if err != nil {
		slog.Error("init pipeline", slog.String("err", err.Error()))
		os.Exit(2)
	}
	defer pipeline.Close()

	slog.Info("recording-uploader starting",
		slog.String("spool", cfg.SpoolDir),
		slog.String("bucket", cfg.S3Bucket),
		slog.String("metrics_addr", cfg.MetricsAddr),
		slog.String("instance", cfg.InstanceID),
	)

	if err := pipeline.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("pipeline exit", slog.String("err", err.Error()))
		os.Exit(1)
	}
	slog.Info("recording-uploader stopped")
}
```

#### 5.2. `cmd/recording-uploader/config.go`

```go
package main // тесты: cmd/recording-uploader/config_test.go

// (delegated to internal/uploader.LoadConfigFromEnv — main.go re-exports nothing)
```

> Конфиг живёт в `internal/uploader/config.go`. Переменные из ENV (см. Ansible `recording-uploader.env.j2`).

#### 5.3. `internal/uploader/config.go`

```go
package uploader

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"time"
)

// Config governs runtime behaviour of recording-uploader.
type Config struct {
	SpoolDir     string
	StateDir     string
	S3Endpoint   string
	S3Bucket     string
	S3Prefix     string
	S3Region     string
	KMSKeyID     string
	RecorderAddr string
	TLSCAPath    string
	TLSCertPath  string
	TLSKeyPath   string
	FFmpegPath   string
	OpusBitrate  string
	OpusSampleRate int

	RetryInitial  time.Duration
	RetryMax      time.Duration
	RetryAttempts int

	MetricsAddr     string
	LogLevel        string
	LogFormat       string
	DeleteAfterAck  bool
	InstanceID      string
}

// LoadConfigFromEnv reads RECORDING_UPLOADER_* environment variables.
func LoadConfigFromEnv() (Config, error) {
	get := func(k, def string) string {
		if v, ok := os.LookupEnv(k); ok {
			return v
		}
		return def
	}
	getDur := func(k string, def time.Duration) (time.Duration, error) {
		raw := get(k, "")
		if raw == "" {
			return def, nil
		}
		d, err := time.ParseDuration(raw)
		if err != nil {
			return 0, fmt.Errorf("env %s: %w", k, err)
		}
		return d, nil
	}
	getInt := func(k string, def int) (int, error) {
		raw := get(k, "")
		if raw == "" {
			return def, nil
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			return 0, fmt.Errorf("env %s: %w", k, err)
		}
		return n, nil
	}
	getBool := func(k string, def bool) bool {
		raw := get(k, "")
		if raw == "" {
			return def
		}
		switch raw {
		case "1", "true", "yes", "TRUE", "True":
			return true
		default:
			return false
		}
	}

	cfg := Config{
		SpoolDir:       get("RECORDING_UPLOADER_SPOOL_DIR", "/var/spool/sociopulse"),
		StateDir:       get("RECORDING_UPLOADER_STATE_DIR", "/var/lib/recording-uploader"),
		S3Endpoint:     get("RECORDING_UPLOADER_S3_ENDPOINT", "https://storage.yandexcloud.net"),
		S3Bucket:       get("RECORDING_UPLOADER_S3_BUCKET", ""),
		S3Prefix:       get("RECORDING_UPLOADER_S3_PREFIX", "recordings"),
		S3Region:       get("RECORDING_UPLOADER_S3_REGION", "ru-central1"),
		KMSKeyID:       get("RECORDING_UPLOADER_KMS_KEY_ID", ""),
		RecorderAddr:   get("RECORDING_UPLOADER_RECORDER_GRPC_ADDR", ""),
		TLSCAPath:      get("RECORDING_UPLOADER_RECORDER_GRPC_TLS_CA", ""),
		TLSCertPath:    get("RECORDING_UPLOADER_RECORDER_GRPC_TLS_CERT", ""),
		TLSKeyPath:     get("RECORDING_UPLOADER_RECORDER_GRPC_TLS_KEY", ""),
		FFmpegPath:     get("RECORDING_UPLOADER_FFMPEG_PATH", "/usr/bin/ffmpeg"),
		OpusBitrate:    get("RECORDING_UPLOADER_OPUS_BITRATE", "32k"),
		MetricsAddr:    get("RECORDING_UPLOADER_METRICS_ADDR", ":9091"),
		LogLevel:       get("RECORDING_UPLOADER_LOG_LEVEL", "info"),
		LogFormat:      get("RECORDING_UPLOADER_LOG_FORMAT", "json"),
		DeleteAfterAck: getBool("RECORDING_UPLOADER_DELETE_AFTER_ACK", true),
		InstanceID:     get("RECORDING_UPLOADER_INSTANCE_ID", "unknown"),
	}

	var err error
	if cfg.RetryInitial, err = getDur("RECORDING_UPLOADER_RETRY_INITIAL", 5*time.Second); err != nil {
		return cfg, err
	}
	if cfg.RetryMax, err = getDur("RECORDING_UPLOADER_RETRY_MAX", 5*time.Minute); err != nil {
		return cfg, err
	}
	if cfg.RetryAttempts, err = getInt("RECORDING_UPLOADER_RETRY_ATTEMPTS", 100); err != nil {
		return cfg, err
	}
	if cfg.OpusSampleRate, err = getInt("RECORDING_UPLOADER_OPUS_SAMPLE_RATE", 16000); err != nil {
		return cfg, err
	}

	return cfg, cfg.Validate()
}

// Validate ensures required fields are populated and well-formed.
func (c Config) Validate() error {
	if c.SpoolDir == "" {
		return errors.New("SpoolDir is required")
	}
	if _, err := url.Parse(c.S3Endpoint); err != nil {
		return fmt.Errorf("S3Endpoint: %w", err)
	}
	if c.S3Bucket == "" {
		return errors.New("S3Bucket is required (RECORDING_UPLOADER_S3_BUCKET)")
	}
	if c.KMSKeyID == "" {
		return errors.New("KMSKeyID is required (RECORDING_UPLOADER_KMS_KEY_ID)")
	}
	if c.RecorderAddr == "" {
		return errors.New("RecorderAddr is required (RECORDING_UPLOADER_RECORDER_GRPC_ADDR)")
	}
	if c.RetryAttempts < 1 {
		return errors.New("RetryAttempts must be >= 1")
	}
	if c.RetryInitial >= c.RetryMax {
		return errors.New("RetryInitial must be < RetryMax")
	}
	if c.OpusSampleRate != 8000 && c.OpusSampleRate != 16000 && c.OpusSampleRate != 24000 && c.OpusSampleRate != 48000 {
		return fmt.Errorf("OpusSampleRate must be 8000/16000/24000/48000, got %d", c.OpusSampleRate)
	}
	return nil
}
```

#### 5.4. `internal/uploader/config_test.go`

```go
package uploader

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfigFromEnv_Defaults(t *testing.T) {
	t.Setenv("RECORDING_UPLOADER_S3_BUCKET", "test-bucket")
	t.Setenv("RECORDING_UPLOADER_KMS_KEY_ID", "kms-1")
	t.Setenv("RECORDING_UPLOADER_RECORDER_GRPC_ADDR", "rec.local:443")

	cfg, err := LoadConfigFromEnv()
	require.NoError(t, err)
	assert.Equal(t, "/var/spool/sociopulse", cfg.SpoolDir)
	assert.Equal(t, "32k", cfg.OpusBitrate)
	assert.Equal(t, 16000, cfg.OpusSampleRate)
	assert.Equal(t, 5*time.Second, cfg.RetryInitial)
	assert.Equal(t, 5*time.Minute, cfg.RetryMax)
	assert.Equal(t, 100, cfg.RetryAttempts)
	assert.True(t, cfg.DeleteAfterAck)
}

func TestLoadConfigFromEnv_MissingRequired(t *testing.T) {
	cases := []struct {
		name       string
		envs       map[string]string
		errSubstr  string
	}{
		{"missing bucket", map[string]string{
			"RECORDING_UPLOADER_KMS_KEY_ID":         "k",
			"RECORDING_UPLOADER_RECORDER_GRPC_ADDR": "a:1",
		}, "S3Bucket"},
		{"missing kms", map[string]string{
			"RECORDING_UPLOADER_S3_BUCKET":          "b",
			"RECORDING_UPLOADER_RECORDER_GRPC_ADDR": "a:1",
		}, "KMSKeyID"},
		{"missing recorder", map[string]string{
			"RECORDING_UPLOADER_S3_BUCKET": "b",
			"RECORDING_UPLOADER_KMS_KEY_ID": "k",
		}, "RecorderAddr"},
		{"bad sample rate", map[string]string{
			"RECORDING_UPLOADER_S3_BUCKET":          "b",
			"RECORDING_UPLOADER_KMS_KEY_ID":         "k",
			"RECORDING_UPLOADER_RECORDER_GRPC_ADDR": "a:1",
			"RECORDING_UPLOADER_OPUS_SAMPLE_RATE":   "44100",
		}, "OpusSampleRate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.envs {
				t.Setenv(k, v)
			}
			_, err := LoadConfigFromEnv()
			require.Error(t, err)
			assert.True(t, strings.Contains(err.Error(), tc.errSubstr),
				"err %q should contain %q", err, tc.errSubstr)
		})
	}
}
```

#### 5.5. `internal/uploader/watcher.go`

```go
package uploader

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// FileEvent reports a fully written WAV recording ready for processing.
type FileEvent struct {
	Path      string
	Size      int64
	DetectedAt time.Time
}

// Watcher abstracts fsnotify so we can mock it in tests.
type Watcher interface {
	Events() <-chan FileEvent
	Errors() <-chan error
	Close() error
}

// FSNotifyWatcher implements Watcher using fsnotify.
type FSNotifyWatcher struct {
	dir       string
	stable    time.Duration
	notifier  *fsnotify.Watcher
	events    chan FileEvent
	errors    chan error
	wg        sync.WaitGroup
	done      chan struct{}
	logger    *slog.Logger
	openFiles sync.Map // path -> bool, tracks files seen but not yet stable
	statFn    func(string) (FileInfo, error)
}

// FileInfo is a minimal stat-like info for testability.
type FileInfo struct {
	Size    int64
	ModTime time.Time
}

// NewFSNotifyWatcher constructs a watcher rooted at dir. CLOSE_WRITE events
// trigger a stability check before emitting FileEvent (FreeSWITCH may keep
// the file open during the call; it issues final close at hangup, after which
// the file size is stable).
func NewFSNotifyWatcher(dir string, stable time.Duration, logger *slog.Logger) (*FSNotifyWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("fsnotify new: %w", err)
	}
	if err := w.Add(dir); err != nil {
		w.Close()
		return nil, fmt.Errorf("fsnotify add %s: %w", dir, err)
	}
	return &FSNotifyWatcher{
		dir:      dir,
		stable:   stable,
		notifier: w,
		events:   make(chan FileEvent, 64),
		errors:   make(chan error, 16),
		done:     make(chan struct{}),
		logger:   logger,
		statFn:   defaultStat,
	}, nil
}

// Events returns the channel of file-ready events.
func (w *FSNotifyWatcher) Events() <-chan FileEvent { return w.events }

// Errors returns the channel of watcher errors.
func (w *FSNotifyWatcher) Errors() <-chan error { return w.errors }

// Run starts the event loop. Blocking; returns when ctx is done or watcher is closed.
func (w *FSNotifyWatcher) Run(ctx context.Context) error {
	defer close(w.events)
	defer close(w.errors)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.done:
			return nil
		case ev, ok := <-w.notifier.Events:
			if !ok {
				return nil
			}
			w.handle(ctx, ev)
		case err, ok := <-w.notifier.Errors:
			if !ok {
				return nil
			}
			select {
			case w.errors <- err:
			default:
			}
		}
	}
}

func (w *FSNotifyWatcher) handle(ctx context.Context, ev fsnotify.Event) {
	if !strings.HasSuffix(ev.Name, ".wav") {
		return
	}
	switch {
	case ev.Has(fsnotify.Create):
		w.openFiles.Store(ev.Name, true)
		w.logger.Debug("watcher: file created", slog.String("path", ev.Name))
	case ev.Has(fsnotify.Write):
		// активная запись; ничего не делаем — ждём CLOSE
	case ev.Has(fsnotify.Remove), ev.Has(fsnotify.Rename):
		w.openFiles.Delete(ev.Name)
		w.logger.Debug("watcher: file gone", slog.String("path", ev.Name))
	default:
		// fsnotify on Linux exposes IN_CLOSE_WRITE через Op==Write+финальный stat;
		// мы используем стабилизационную задержку.
	}

	w.wg.Add(1)
	go func(path string) {
		defer w.wg.Done()
		if w.waitStable(ctx, path) {
			info, err := w.statFn(path)
			if err != nil {
				select {
				case w.errors <- fmt.Errorf("stat %s: %w", path, err):
				default:
				}
				return
			}
			select {
			case <-ctx.Done():
			case w.events <- FileEvent{Path: path, Size: info.Size, DetectedAt: time.Now().UTC()}:
				w.openFiles.Delete(path)
				w.logger.Info("watcher: file stable",
					slog.String("path", filepath.Base(path)),
					slog.Int64("size", info.Size),
				)
			}
		}
	}(ev.Name)
}

func (w *FSNotifyWatcher) waitStable(ctx context.Context, path string) bool {
	prevSize := int64(-1)
	ticker := time.NewTicker(w.stable)
	defer ticker.Stop()
	for i := 0; i < 12; i++ { // max ~12 * stable секунд
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			info, err := w.statFn(path)
			if err != nil {
				return false
			}
			if info.Size == prevSize && info.Size > 0 {
				return true
			}
			prevSize = info.Size
		}
	}
	return false
}

// Close stops the watcher.
func (w *FSNotifyWatcher) Close() error {
	close(w.done)
	err := w.notifier.Close()
	w.wg.Wait()
	return err
}
```

`defaultStat` определяется в utility-файле:

`internal/uploader/stat.go`:

```go
package uploader

import (
	"os"
	"time"
)

func defaultStat(path string) (FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return FileInfo{}, err
	}
	return FileInfo{Size: info.Size(), ModTime: info.ModTime().UTC()}, nil
}

// nowUTC overridable for tests
var nowUTC = func() time.Time { return time.Now().UTC() }
```

#### 5.6. `internal/uploader/watcher_test.go`

```go
package uploader

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFSNotifyWatcher_DetectsStableWAV(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	w, err := NewFSNotifyWatcher(dir, 200*time.Millisecond, logger)
	require.NoError(t, err)
	defer w.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() { _ = w.Run(ctx) }()

	path := filepath.Join(dir, "test-call-1.wav")
	f, err := os.Create(path)
	require.NoError(t, err)
	_, err = f.Write(make([]byte, 1024))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	select {
	case ev := <-w.Events():
		assert.Equal(t, path, ev.Path)
		assert.Equal(t, int64(1024), ev.Size)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for stable file event")
	}
}

func TestFSNotifyWatcher_IgnoresNonWAV(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	w, err := NewFSNotifyWatcher(dir, 100*time.Millisecond, logger)
	require.NoError(t, err)
	defer w.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	require.NoError(t, os.WriteFile(filepath.Join(dir, "x.txt"), []byte("hi"), 0o644))

	select {
	case ev := <-w.Events():
		t.Fatalf("unexpected event for non-wav: %v", ev)
	case <-time.After(800 * time.Millisecond):
		// ok
	}
}
```

#### 5.7. `internal/uploader/transcoder.go`

```go
package uploader

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Transcoder converts source audio (WAV) to target codec (Opus).
type Transcoder interface {
	Transcode(ctx context.Context, src, dst string) error
}

// FFmpegTranscoder runs ffmpeg as subprocess.
type FFmpegTranscoder struct {
	binary     string
	bitrate    string
	sampleRate int
	logger     *slog.Logger
}

// NewFFmpegTranscoder constructs a transcoder with given parameters.
func NewFFmpegTranscoder(binary, bitrate string, sampleRate int, logger *slog.Logger) *FFmpegTranscoder {
	return &FFmpegTranscoder{binary: binary, bitrate: bitrate, sampleRate: sampleRate, logger: logger}
}

// Transcode wav→opus 32k 16kHz mono. Result file overwrites dst.
func (t *FFmpegTranscoder) Transcode(ctx context.Context, src, dst string) error {
	if !strings.HasSuffix(src, ".wav") {
		return fmt.Errorf("transcode: src must be .wav, got %s", src)
	}
	tmp := dst + ".part"
	defer os.Remove(tmp)

	args := []string{
		"-y", "-loglevel", "error",
		"-i", src,
		"-c:a", "libopus",
		"-b:a", t.bitrate,
		"-ar", fmt.Sprintf("%d", t.sampleRate),
		"-ac", "1",
		"-application", "voip",
		"-vbr", "on",
		"-frame_duration", "20",
		filepath.Clean(tmp),
	}
	cmd := exec.CommandContext(ctx, t.binary, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg %s: %w (%s)", strings.Join(args, " "), err, stderr.String())
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, dst, err)
	}
	if info, err := os.Stat(dst); err == nil {
		t.logger.Debug("transcoded",
			slog.String("src", filepath.Base(src)),
			slog.String("dst", filepath.Base(dst)),
			slog.Int64("size", info.Size()),
		)
	}
	return nil
}
```

#### 5.8. `internal/uploader/transcoder_test.go`

```go
package uploader

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFFmpegTranscoder_RealConversion(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available, skipping integration test")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.wav")
	dst := filepath.Join(dir, "dst.opus")

	// generate 1-sec sine wave WAV via ffmpeg itself
	gen := exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=1000:duration=1:sample_rate=8000",
		"-ac", "1", src)
	require.NoError(t, gen.Run())

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tc := NewFFmpegTranscoder("ffmpeg", "32k", 16000, logger)

	require.NoError(t, tc.Transcode(context.Background(), src, dst))

	info, err := os.Stat(dst)
	require.NoError(t, err)
	assert.Greater(t, info.Size(), int64(100), "opus output should be non-trivial")

	// validate file is opus by checking magic bytes "OggS"
	f, err := os.Open(dst)
	require.NoError(t, err)
	defer f.Close()
	hdr := make([]byte, 4)
	_, _ = f.Read(hdr)
	assert.Equal(t, "OggS", string(hdr), "should start with Ogg container magic")
}

func TestFFmpegTranscoder_RejectsNonWAV(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tc := NewFFmpegTranscoder("ffmpeg", "32k", 16000, logger)
	err := tc.Transcode(context.Background(), "foo.mp3", "bar.opus")
	require.Error(t, err)
}
```

#### 5.9. `internal/uploader/encryptor.go`

```go
package uploader

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/yandex-cloud/go-genproto/yandex/cloud/kms/v1"
	"github.com/yandex-cloud/go-sdk/sdkresolvers"
	ycsdk "github.com/yandex-cloud/go-sdk"
)

// Encryptor wraps client-side AES-256-GCM with a KMS-derived data key.
type Encryptor interface {
	Encrypt(ctx context.Context, src, dst string) (EncryptionMetadata, error)
}

// EncryptionMetadata describes the wrapped data key + GCM nonce required
// to decrypt the file later (stored alongside in S3 metadata + DB).
type EncryptionMetadata struct {
	KMSKeyID         string `json:"kms_key_id"`
	WrappedDataKey   []byte `json:"wrapped_data_key"`
	Nonce            []byte `json:"nonce"`
	Algorithm        string `json:"algorithm"` // AES-256-GCM
	PlaintextSHA256  []byte `json:"plaintext_sha256"`
	CiphertextSHA256 []byte `json:"ciphertext_sha256"`
}

// KMSClient narrowing what we use; allows test stubs.
type KMSClient interface {
	GenerateDataKey(ctx context.Context, in *kms.GenerateDataKeyRequest) (*kms.GenerateDataKeyResponse, error)
}

// YCKMSClient adapts yandex-cloud SDK to KMSClient interface.
type YCKMSClient struct {
	sdk *ycsdk.SDK
}

// NewYCKMSClient creates a kms client using IAM-token auth.
func NewYCKMSClient(ctx context.Context) (*YCKMSClient, error) {
	creds, err := ycsdk.InstanceServiceAccount() // VM metadata service
	if err != nil {
		return nil, fmt.Errorf("yc creds: %w", err)
	}
	sdk, err := ycsdk.Build(ctx, ycsdk.Config{Credentials: creds})
	if err != nil {
		return nil, fmt.Errorf("yc sdk build: %w", err)
	}
	return &YCKMSClient{sdk: sdk}, nil
}

// GenerateDataKey calls KMS via SDK.
func (c *YCKMSClient) GenerateDataKey(ctx context.Context, in *kms.GenerateDataKeyRequest) (*kms.GenerateDataKeyResponse, error) {
	return c.sdk.KMSCrypto().SymmetricCrypto().GenerateDataKey(ctx, in)
}

// AESGCMEncryptor is the default Encryptor.
type AESGCMEncryptor struct {
	kms      KMSClient
	keyID    string
	keySpec  kms.SymmetricAlgorithm
	logger   *slog.Logger
	chunkLen int
}

// NewAESGCMEncryptor constructs an encryptor.
func NewAESGCMEncryptor(c KMSClient, keyID string, logger *slog.Logger) *AESGCMEncryptor {
	return &AESGCMEncryptor{
		kms:      c,
		keyID:    keyID,
		keySpec:  kms.SymmetricAlgorithm_AES_256,
		logger:   logger,
		chunkLen: 64 * 1024,
	}
}

// Encrypt produces an AES-256-GCM ciphertext at dst path. Source is unchanged.
// The first 12 bytes of the output file are the GCM nonce.
func (e *AESGCMEncryptor) Encrypt(ctx context.Context, src, dst string) (EncryptionMetadata, error) {
	resp, err := e.kms.GenerateDataKey(ctx, &kms.GenerateDataKeyRequest{
		KeyId:           e.keyID,
		DataKeySpec:     e.keySpec,
		AadContext:      []byte(filepath.Base(src)),
	})
	if err != nil {
		return EncryptionMetadata{}, fmt.Errorf("kms generate: %w", err)
	}
	dataKey := resp.GetDataKeyPlaintext()
	defer zero(dataKey)
	if len(dataKey) != 32 {
		return EncryptionMetadata{}, fmt.Errorf("expected 32-byte data key, got %d", len(dataKey))
	}

	nonce := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return EncryptionMetadata{}, fmt.Errorf("rand nonce: %w", err)
	}

	block, err := aes.NewCipher(dataKey)
	if err != nil {
		return EncryptionMetadata{}, fmt.Errorf("aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return EncryptionMetadata{}, fmt.Errorf("gcm: %w", err)
	}

	plaintext, err := os.ReadFile(src)
	if err != nil {
		return EncryptionMetadata{}, fmt.Errorf("read src: %w", err)
	}

	plainSum := sha256Sum(plaintext)
	ciphertext := gcm.Seal(nil, nonce, plaintext, []byte(filepath.Base(src)))
	cipherSum := sha256Sum(ciphertext)

	tmp := dst + ".part"
	if err := os.WriteFile(tmp, append(nonce, ciphertext...), 0o600); err != nil {
		return EncryptionMetadata{}, fmt.Errorf("write enc: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return EncryptionMetadata{}, fmt.Errorf("rename: %w", err)
	}

	return EncryptionMetadata{
		KMSKeyID:         e.keyID,
		WrappedDataKey:   resp.GetDataKeyCiphertext(),
		Nonce:            nonce,
		Algorithm:        "AES-256-GCM",
		PlaintextSHA256:  plainSum,
		CiphertextSHA256: cipherSum,
	}, nil
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

var errNotImplemented = errors.New("not implemented")

// resolverHelper kept to silence unused import in production builds.
var _ = sdkresolvers.SubjectByNameResolver
```

`internal/uploader/sha256.go`:

```go
package uploader

import "crypto/sha256"

func sha256Sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}
```

#### 5.10. `internal/uploader/encryptor_test.go`

```go
package uploader

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/yandex-cloud/go-genproto/yandex/cloud/kms/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubKMS returns deterministic data keys for tests.
type stubKMS struct {
	calls int
	plain []byte
}

func newStubKMS() *stubKMS {
	plain := make([]byte, 32)
	_, _ = io.ReadFull(rand.Reader, plain)
	return &stubKMS{plain: plain}
}

func (s *stubKMS) GenerateDataKey(_ context.Context, _ *kms.GenerateDataKeyRequest) (*kms.GenerateDataKeyResponse, error) {
	s.calls++
	wrapped := append([]byte("wrapped:"), s.plain...) // toy wrap
	return &kms.GenerateDataKeyResponse{
		DataKeyPlaintext:  s.plain,
		DataKeyCiphertext: wrapped,
		KeyId:             "test-key",
		VersionId:         "v1",
	}, nil
}

func TestAESGCMEncryptor_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "audio.opus")
	body := []byte("hello sociopulse audio body bytes 0123456789")
	require.NoError(t, os.WriteFile(src, body, 0o644))

	stub := newStubKMS()
	enc := NewAESGCMEncryptor(stub, "test-key", slog.New(slog.NewTextHandler(io.Discard, nil)))
	dst := filepath.Join(dir, "audio.opus.enc")

	meta, err := enc.Encrypt(context.Background(), src, dst)
	require.NoError(t, err)
	assert.Equal(t, "AES-256-GCM", meta.Algorithm)
	assert.Equal(t, 12, len(meta.Nonce))
	assert.Equal(t, 32, len(meta.PlaintextSHA256))
	assert.Equal(t, 1, stub.calls)

	// Decrypt manually and compare
	encBytes, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.Greater(t, len(encBytes), 12)
	nonce := encBytes[:12]
	cipherBlob := encBytes[12:]

	block, err := aes.NewCipher(stub.plain)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	plain, err := gcm.Open(nil, nonce, cipherBlob, []byte(filepath.Base(src)))
	require.NoError(t, err)
	assert.Equal(t, body, plain)
}
```

#### 5.11. `internal/uploader/s3client.go`

```go
package uploader

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Uploader uploads encrypted ciphertext blobs to Yandex Object Storage.
type S3Uploader interface {
	Upload(ctx context.Context, bucket, key string, body []byte, meta EncryptionMetadata) error
}

// YCS3Uploader implements S3Uploader using AWS SDK against YC S3-compatible API.
type YCS3Uploader struct {
	client   *s3.Client
	endpoint string
	region   string
	logger   *slog.Logger
	httpClient *http.Client
}

// NewYCS3Uploader uses static credentials from env (AWS_ACCESS_KEY_ID/SECRET).
func NewYCS3Uploader(ctx context.Context, endpoint, region string, logger *slog.Logger) (*YCS3Uploader, error) {
	ak := os.Getenv("AWS_ACCESS_KEY_ID")
	sk := os.Getenv("AWS_SECRET_ACCESS_KEY")
	if ak == "" || sk == "" {
		return nil, fmt.Errorf("AWS_ACCESS_KEY_ID/SECRET_ACCESS_KEY required for S3")
	}
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(ak, sk, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("aws cfg: %w", err)
	}
	cl := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	return &YCS3Uploader{
		client:   cl,
		endpoint: endpoint,
		region:   region,
		logger:   logger,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// Upload performs PUT object with metadata fields for KMS+nonce.
func (u *YCS3Uploader) Upload(ctx context.Context, bucket, key string, body []byte, meta EncryptionMetadata) error {
	metaMap := map[string]string{
		"x-amz-meta-kms-key-id":   meta.KMSKeyID,
		"x-amz-meta-algorithm":    meta.Algorithm,
		"x-amz-meta-nonce":        base64.StdEncoding.EncodeToString(meta.Nonce),
		"x-amz-meta-wrapped-key":  base64.StdEncoding.EncodeToString(meta.WrappedDataKey),
		"x-amz-meta-pt-sha256":    base64.StdEncoding.EncodeToString(meta.PlaintextSHA256),
		"x-amz-meta-ct-sha256":    base64.StdEncoding.EncodeToString(meta.CiphertextSHA256),
		"x-amz-meta-bytes":        strconv.Itoa(len(body)),
		"x-amz-meta-uploaded-at":  time.Now().UTC().Format(time.RFC3339),
	}
	awsMeta := map[string]string{}
	for k, v := range metaMap {
		// AWS SDK v2 expects metadata without x-amz-meta- prefix
		key := k[len("x-amz-meta-"):]
		awsMeta[key] = v
	}
	_, err := u.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("application/octet-stream"),
		Metadata:    awsMeta,
	})
	if err != nil {
		return fmt.Errorf("s3 put: %w", err)
	}
	u.logger.Debug("s3 upload ok",
		slog.String("bucket", bucket),
		slog.String("key", key),
		slog.Int("bytes", len(body)),
	)
	return nil
}

// MarshalSidecar produces a JSON metadata document we ship as separate object
// for backups and easier auditing.
func MarshalSidecar(meta EncryptionMetadata, callID, tenantID string) ([]byte, error) {
	return json.Marshal(struct {
		CallID   string             `json:"call_id"`
		TenantID string             `json:"tenant_id"`
		Meta     EncryptionMetadata `json:"meta"`
	}{callID, tenantID, meta})
}
```

#### 5.12. `internal/uploader/s3client_test.go`

```go
package uploader

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarshalSidecar(t *testing.T) {
	meta := EncryptionMetadata{
		KMSKeyID:        "kms-1",
		WrappedDataKey:  []byte{1, 2, 3},
		Nonce:           []byte{4, 5, 6},
		Algorithm:       "AES-256-GCM",
		PlaintextSHA256: []byte{7},
	}
	b, err := MarshalSidecar(meta, "call-1", "tenant-1")
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, "call-1", got["call_id"])
	assert.Equal(t, "tenant-1", got["tenant_id"])
}
```

(полный e2e-тест S3-uploader'а с настоящим S3 — в отдельном integration-тесте, опционально через minio.)

#### 5.13. `internal/uploader/recorder_client.go`

```go
package uploader

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
)

// CommitRequest is what we send to the Recording.Commit gRPC.
type CommitRequest struct {
	CallID         string
	TenantID       string
	S3Bucket       string
	S3Key          string
	SidecarKey     string
	Bytes          int64
	DurationMS     int64
	StartedAt      time.Time
	FinishedAt     time.Time
	WrappedDataKey []byte
	Nonce          []byte
	KMSKeyID       string
	Algorithm      string
	PlaintextSHA256 []byte
	CiphertextSHA256 []byte
	Node           string
}

// RecorderClient calls Recording.Commit.
type RecorderClient interface {
	Commit(ctx context.Context, req CommitRequest) error
}

// GRPCRecorderClient is the default impl.
type GRPCRecorderClient struct {
	conn   *grpc.ClientConn
	logger *slog.Logger
}

// NewGRPCRecorderClient dials a TLS gRPC server with optional client cert (mTLS).
func NewGRPCRecorderClient(ctx context.Context, addr, caPath, certPath, keyPath string, logger *slog.Logger) (*GRPCRecorderClient, error) {
	tlsCfg, err := buildTLSConfig(caPath, certPath, keyPath)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.DialContext(ctx, addr,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:    20 * time.Second,
			Timeout: 5 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("grpc dial %s: %w", addr, err)
	}
	return &GRPCRecorderClient{conn: conn, logger: logger}, nil
}

func buildTLSConfig(caPath, certPath, keyPath string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caPath != "" {
		caPEM, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("read ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("invalid ca pem")
		}
		cfg.RootCAs = pool
	}
	if certPath != "" && keyPath != "" {
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("load mtls cert: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// Commit calls the proto-generated Recording.Commit RPC. The actual proto stub
// is generated in Plan #09; here we leave a placeholder calling pattern that
// will be wired once protobufs land. Until then we use a `recordingv1` package
// (defined in pkg/proto/recording/v1) — assumed shape:
//
//   service Recording {
//     rpc Commit(CommitRequest) returns (CommitResponse);
//   }
//
// The shape lives in pkg/proto/recording/v1/recording.proto; client stubs are
// generated via `make proto`. Until then we wrap the dial+call here.
func (c *GRPCRecorderClient) Commit(ctx context.Context, req CommitRequest) error {
	// Pseudocode pending Plan #09 — uses generated stub when proto lands.
	// stub := recordingv1.NewRecordingClient(c.conn)
	// _, err := stub.Commit(ctx, &recordingv1.CommitRequest{
	//     CallId: req.CallID, ...
	// })
	// return err
	c.logger.Info("recorder commit",
		slog.String("call_id", req.CallID),
		slog.String("s3_key", req.S3Key),
		slog.Int64("bytes", req.Bytes),
	)
	return errCommitNotWired
}

var errCommitNotWired = errors.New("recording.Commit gRPC stub pending Plan #09")

// Close releases the gRPC connection.
func (c *GRPCRecorderClient) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}
```

> Важно: до Plan #09 stub возвращает `errCommitNotWired` — pipeline в этом случае удерживает recording в state-dir с пометкой "pending recorder", retry до бесконечности, файл локально не удаляется. После Plan #09 заменяется на сгенерированный стаб с `recordingv1.NewRecordingClient(...).Commit(...)`.

#### 5.14. `internal/uploader/recorder_client_test.go`

```go
package uploader

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGRPCRecorderClient_NotWiredYet(t *testing.T) {
	c := &GRPCRecorderClient{}
	err := c.Commit(context.Background(), CommitRequest{CallID: "x"})
	assert.True(t, errors.Is(err, errCommitNotWired))
}
```

#### 5.15. `internal/uploader/retry.go`

```go
package uploader

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"time"
)

// Backoff yields exponentially increasing delays with full jitter.
type Backoff struct {
	Initial  time.Duration
	Max      time.Duration
	attempt  int
}

// NewBackoff resets to attempt=0.
func NewBackoff(initial, max time.Duration) *Backoff {
	return &Backoff{Initial: initial, Max: max}
}

// Next returns the next sleep duration.
func (b *Backoff) Next() time.Duration {
	exp := math.Pow(2, float64(b.attempt))
	d := time.Duration(float64(b.Initial) * exp)
	if d > b.Max {
		d = b.Max
	}
	jitter := time.Duration(rand.Int63n(int64(d) / 2 + 1))
	b.attempt++
	return d/2 + jitter
}

// Reset clears attempt counter.
func (b *Backoff) Reset() { b.attempt = 0 }

// RetryDo runs op with backoff, up to maxAttempts. retriable decides if err is retryable.
func RetryDo(ctx context.Context, b *Backoff, maxAttempts int, op func(ctx context.Context) error, retriable func(error) bool) error {
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := op(ctx)
		if err == nil {
			return nil
		}
		if !retriable(err) {
			return err
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b.Next()):
		}
	}
	return errors.Join(errors.New("max retries"), lastErr)
}
```

#### 5.16. `internal/uploader/retry_test.go`

```go
package uploader

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackoff_Increases(t *testing.T) {
	b := NewBackoff(10*time.Millisecond, 100*time.Millisecond)
	prev := time.Duration(0)
	for i := 0; i < 4; i++ {
		next := b.Next()
		assert.LessOrEqual(t, next, 100*time.Millisecond)
		assert.GreaterOrEqual(t, next, time.Duration(0))
		_ = prev
		prev = next
	}
}

func TestRetryDo_RetriesAndSucceeds(t *testing.T) {
	calls := 0
	err := RetryDo(context.Background(), NewBackoff(time.Millisecond, 5*time.Millisecond), 5,
		func(ctx context.Context) error {
			calls++
			if calls < 3 {
				return errors.New("transient")
			}
			return nil
		},
		func(err error) bool { return true },
	)
	require.NoError(t, err)
	assert.Equal(t, 3, calls)
}

func TestRetryDo_RespectsContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	err := RetryDo(ctx, NewBackoff(50*time.Millisecond, 100*time.Millisecond), 100,
		func(ctx context.Context) error { return errors.New("transient") },
		func(err error) bool { return true },
	)
	require.Error(t, err)
}
```

#### 5.17. `internal/uploader/pipeline.go`

```go
package uploader

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Pipeline orchestrates watch → transcode → encrypt → upload → commit.
type Pipeline struct {
	cfg        Config
	logger     *slog.Logger
	watcher    Watcher
	rawWatcher *FSNotifyWatcher
	transcoder Transcoder
	encryptor  Encryptor
	s3         S3Uploader
	recorder   RecorderClient
	metrics    *Metrics
	wg         sync.WaitGroup
}

// NewPipeline wires all dependencies, including Yandex SDK clients.
func NewPipeline(ctx context.Context, cfg Config, logger *slog.Logger) (*Pipeline, error) {
	w, err := NewFSNotifyWatcher(cfg.SpoolDir, 1*time.Second, logger)
	if err != nil {
		return nil, fmt.Errorf("watcher: %w", err)
	}
	tc := NewFFmpegTranscoder(cfg.FFmpegPath, cfg.OpusBitrate, cfg.OpusSampleRate, logger)
	kmsClient, err := NewYCKMSClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("kms: %w", err)
	}
	enc := NewAESGCMEncryptor(kmsClient, cfg.KMSKeyID, logger)
	s3, err := NewYCS3Uploader(ctx, cfg.S3Endpoint, cfg.S3Region, logger)
	if err != nil {
		return nil, fmt.Errorf("s3: %w", err)
	}
	rec, err := NewGRPCRecorderClient(ctx, cfg.RecorderAddr, cfg.TLSCAPath, cfg.TLSCertPath, cfg.TLSKeyPath, logger)
	if err != nil {
		return nil, fmt.Errorf("recorder: %w", err)
	}
	metrics := NewMetrics()
	return &Pipeline{
		cfg:        cfg,
		logger:     logger,
		watcher:    w,
		rawWatcher: w,
		transcoder: tc,
		encryptor:  enc,
		s3:         s3,
		recorder:   rec,
		metrics:    metrics,
	}, nil
}

// Run starts the watcher and processing loop.
func (p *Pipeline) Run(ctx context.Context) error {
	go p.serveMetrics(ctx)

	go func() {
		if err := p.rawWatcher.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			p.logger.Error("watcher exit", slog.String("err", err.Error()))
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-p.watcher.Events():
			if !ok {
				return nil
			}
			p.wg.Add(1)
			go func(ev FileEvent) {
				defer p.wg.Done()
				if err := p.processOne(ctx, ev); err != nil {
					p.logger.Error("process file failed",
						slog.String("path", ev.Path),
						slog.String("err", err.Error()),
					)
					p.metrics.UploadsTotal.WithLabelValues("error").Inc()
				} else {
					p.metrics.UploadsTotal.WithLabelValues("success").Inc()
				}
			}(ev)
		case err, ok := <-p.watcher.Errors():
			if !ok {
				return nil
			}
			p.logger.Warn("watcher error", slog.String("err", err.Error()))
		}
	}
}

func (p *Pipeline) processOne(ctx context.Context, ev FileEvent) error {
	startedAt := time.Now()
	timer := p.metrics.UploadDuration.WithLabelValues("ok")
	defer func() {
		timer.Observe(time.Since(startedAt).Seconds())
	}()

	base := strings.TrimSuffix(filepath.Base(ev.Path), ".wav")
	tmpDir := filepath.Join(p.cfg.StateDir, base)
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		return fmt.Errorf("mkdir state: %w", err)
	}

	opusPath := filepath.Join(tmpDir, base+".opus")
	encPath := filepath.Join(tmpDir, base+".opus.enc")

	// 1. transcode
	if err := p.transcoder.Transcode(ctx, ev.Path, opusPath); err != nil {
		return fmt.Errorf("transcode: %w", err)
	}

	// 2. encrypt
	meta, err := p.encryptor.Encrypt(ctx, opusPath, encPath)
	if err != nil {
		return fmt.Errorf("encrypt: %w", err)
	}

	body, err := os.ReadFile(encPath)
	if err != nil {
		return fmt.Errorf("read enc: %w", err)
	}

	now := time.Now().UTC()
	s3Key := fmt.Sprintf("%s/%04d/%02d/%02d/%s.opus.enc",
		p.cfg.S3Prefix, now.Year(), now.Month(), now.Day(), base)
	sidecarKey := s3Key + ".sidecar.json"

	// 3. upload main + sidecar
	bo := NewBackoff(p.cfg.RetryInitial, p.cfg.RetryMax)
	err = RetryDo(ctx, bo, p.cfg.RetryAttempts, func(ctx context.Context) error {
		return p.s3.Upload(ctx, p.cfg.S3Bucket, s3Key, body, meta)
	}, isRetriable)
	if err != nil {
		return fmt.Errorf("s3 upload: %w", err)
	}

	sidecar, err := MarshalSidecar(meta, base, "")
	if err != nil {
		return fmt.Errorf("sidecar marshal: %w", err)
	}
	bo.Reset()
	err = RetryDo(ctx, bo, p.cfg.RetryAttempts, func(ctx context.Context) error {
		return p.s3.Upload(ctx, p.cfg.S3Bucket, sidecarKey, sidecar, meta)
	}, isRetriable)
	if err != nil {
		return fmt.Errorf("sidecar upload: %w", err)
	}

	// 4. commit via gRPC
	commitReq := CommitRequest{
		CallID:           base,
		S3Bucket:         p.cfg.S3Bucket,
		S3Key:            s3Key,
		SidecarKey:       sidecarKey,
		Bytes:            int64(len(body)),
		StartedAt:        ev.DetectedAt,
		FinishedAt:       now,
		WrappedDataKey:   meta.WrappedDataKey,
		Nonce:            meta.Nonce,
		KMSKeyID:         meta.KMSKeyID,
		Algorithm:        meta.Algorithm,
		PlaintextSHA256:  meta.PlaintextSHA256,
		CiphertextSHA256: meta.CiphertextSHA256,
		Node:             p.cfg.InstanceID,
	}
	bo.Reset()
	err = RetryDo(ctx, bo, p.cfg.RetryAttempts, func(ctx context.Context) error {
		return p.recorder.Commit(ctx, commitReq)
	}, isRetriable)
	if err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	// 5. delete local copies (only after ack)
	if p.cfg.DeleteAfterAck {
		_ = os.RemoveAll(tmpDir)
		_ = os.Remove(ev.Path)
	}

	p.logger.Info("recording uploaded",
		slog.String("call_id", base),
		slog.String("s3_key", s3Key),
		slog.Int("bytes", len(body)),
	)
	return nil
}

// isRetriable classifies errors. Real impl differentiates between
// permanent (e.g. invalid kms key) and transient (network).
func isRetriable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errCommitNotWired) {
		return true
	}
	// крайне грубая фильтрация — улучшается в Plan #09
	return true
}

func (p *Pipeline) serveMetrics(ctx context.Context) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{Addr: p.cfg.MetricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		p.logger.Error("metrics server", slog.String("err", err.Error()))
	}
}

// Close drains in-flight goroutines.
func (p *Pipeline) Close() {
	p.wg.Wait()
	_ = p.rawWatcher.Close()
}
```

#### 5.18. `internal/uploader/pipeline_test.go`

```go
package uploader

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeWatcher struct {
	events chan FileEvent
	errs   chan error
}

func (f *fakeWatcher) Events() <-chan FileEvent { return f.events }
func (f *fakeWatcher) Errors() <-chan error     { return f.errs }
func (f *fakeWatcher) Close() error             { return nil }

type fakeTC struct{ calls int32 }

func (f *fakeTC) Transcode(_ context.Context, src, dst string) error {
	atomic.AddInt32(&f.calls, 1)
	return os.WriteFile(dst, []byte("opus-bytes"), 0o644)
}

type fakeEnc struct{ calls int32 }

func (f *fakeEnc) Encrypt(_ context.Context, src, dst string) (EncryptionMetadata, error) {
	atomic.AddInt32(&f.calls, 1)
	if err := os.WriteFile(dst, []byte("enc-blob"), 0o644); err != nil {
		return EncryptionMetadata{}, err
	}
	return EncryptionMetadata{
		KMSKeyID:        "k",
		WrappedDataKey:  []byte{1},
		Nonce:           []byte{2},
		Algorithm:       "AES-256-GCM",
		PlaintextSHA256: []byte{3},
		CiphertextSHA256: []byte{4},
	}, nil
}

type fakeS3 struct{ calls int32 }

func (f *fakeS3) Upload(_ context.Context, bucket, key string, body []byte, _ EncryptionMetadata) error {
	atomic.AddInt32(&f.calls, 1)
	return nil
}

type fakeRec struct {
	calls int32
	fail  int32
}

func (f *fakeRec) Commit(_ context.Context, _ CommitRequest) error {
	if atomic.AddInt32(&f.calls, 1) <= f.fail {
		return errors.New("transient")
	}
	return nil
}

func TestPipeline_ProcessOne_Happy(t *testing.T) {
	tmp := t.TempDir()
	srcPath := filepath.Join(tmp, "call-abc.wav")
	require.NoError(t, os.WriteFile(srcPath, []byte("RIFFsamples"), 0o644))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := Config{
		SpoolDir:       tmp,
		StateDir:       filepath.Join(tmp, "state"),
		S3Bucket:       "b",
		S3Prefix:       "rec",
		KMSKeyID:       "k",
		RecorderAddr:   "addr",
		FFmpegPath:     "true",
		OpusBitrate:    "32k",
		OpusSampleRate: 16000,
		RetryInitial:   1 * time.Millisecond,
		RetryMax:       2 * time.Millisecond,
		RetryAttempts:  5,
		DeleteAfterAck: true,
	}
	p := &Pipeline{
		cfg:        cfg,
		logger:     logger,
		transcoder: &fakeTC{},
		encryptor:  &fakeEnc{},
		s3:         &fakeS3{},
		recorder:   &fakeRec{fail: 0},
		metrics:    NewMetrics(),
	}

	ev := FileEvent{Path: srcPath, Size: 11, DetectedAt: time.Now()}
	require.NoError(t, p.processOne(context.Background(), ev))

	_, err := os.Stat(srcPath)
	assert.True(t, os.IsNotExist(err), "src wav should be deleted after ack")
}

func TestPipeline_RetriesCommit(t *testing.T) {
	tmp := t.TempDir()
	srcPath := filepath.Join(tmp, "call-x.wav")
	require.NoError(t, os.WriteFile(srcPath, []byte("RIFF"), 0o644))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := &fakeRec{fail: 2}
	p := &Pipeline{
		cfg: Config{
			SpoolDir: tmp, StateDir: filepath.Join(tmp, "state"),
			S3Bucket: "b", S3Prefix: "rec", KMSKeyID: "k", RecorderAddr: "addr",
			FFmpegPath: "true", OpusSampleRate: 16000,
			RetryInitial: time.Millisecond, RetryMax: 2 * time.Millisecond,
			RetryAttempts: 5, DeleteAfterAck: false,
		},
		logger:     logger,
		transcoder: &fakeTC{},
		encryptor:  &fakeEnc{},
		s3:         &fakeS3{},
		recorder:   rec,
		metrics:    NewMetrics(),
	}
	require.NoError(t, p.processOne(context.Background(), FileEvent{Path: srcPath}))
	assert.GreaterOrEqual(t, atomic.LoadInt32(&rec.calls), int32(3))
}
```

#### 5.19. `internal/uploader/metrics.go`

```go
package uploader

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics exposes Prometheus metrics for recording-uploader.
type Metrics struct {
	UploadsTotal   *prometheus.CounterVec
	UploadDuration *prometheus.HistogramVec
	BytesTotal     *prometheus.CounterVec
	RetriesTotal   *prometheus.CounterVec
	InflightFiles  prometheus.Gauge
}

// NewMetrics constructs and registers the metrics on the default registry.
func NewMetrics() *Metrics {
	m := &Metrics{
		UploadsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "recording_uploader_uploads_total",
			Help: "Total uploads by status",
		}, []string{"status"}),
		UploadDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "recording_uploader_upload_duration_seconds",
			Help:    "End-to-end pipeline duration",
			Buckets: prometheus.DefBuckets,
		}, []string{"status"}),
		BytesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "recording_uploader_bytes_total",
			Help: "Bytes uploaded to S3",
		}, []string{"stage"}),
		RetriesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "recording_uploader_retries_total",
			Help: "Retry counts by stage",
		}, []string{"stage"}),
		InflightFiles: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "recording_uploader_inflight_files",
			Help: "Files currently in pipeline",
		}),
	}
	prometheus.MustRegister(m.UploadsTotal, m.UploadDuration, m.BytesTotal, m.RetriesTotal, m.InflightFiles)
	return m
}
```

#### 5.20. `internal/uploader/metrics_test.go`

```go
package uploader

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewMetrics_RegistersOnce(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatal("unexpected panic on first registration")
		}
	}()
	m := NewMetrics()
	assert.NotNil(t, m.UploadsTotal)
}
```

#### 5.21. `internal/uploader/logger.go`

```go
package uploader

import (
	"io"
	"log/slog"
	"strings"
)

// NewLogger returns a JSON slog logger filtered by level.
func NewLogger(w io.Writer, levelStr string) *slog.Logger {
	lvl := slog.LevelInfo
	switch strings.ToLower(levelStr) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl}))
}
```

#### 5.22. `cmd/recording-uploader/main_test.go`

```go
package main

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBinary_HelpDoesNotPanic(t *testing.T) {
	// build & run --help-equivalent (we just exercise compilation here)
	out, err := exec.Command("go", "build", "-o", "/tmp/recording-uploader-test", ".").CombinedOutput()
	assert.NoError(t, err, "build error: %s", out)
}
```

#### 5.23. Тесты + сборка

```
make build-recording-uploader   # → bin/recording-uploader
go test ./internal/uploader/... -race -coverprofile=cover.out
go tool cover -func=cover.out | tail -1   # ≥ 80%
```

**Acceptance:**
- [ ] `go build ./cmd/recording-uploader` без ошибок.
- [ ] `go test ./internal/uploader/... -race` зелёный.
- [ ] Coverage ≥ 80 %.
- [ ] Lint `golangci-lint run ./internal/uploader/... ./cmd/recording-uploader/...` без ошибок.
- [ ] Бинарь стартует и сразу падает с понятной ошибкой при пустых ENV (validate fires).
- [ ] При наличии корректных ENV — бинарь поднимает HTTP-метрики на `:9091` (`curl localhost:9091/metrics` → 200).
- [ ] Создание WAV в spool-dir — pipeline вызывает transcode → encrypt → upload → commit (видно в логах JSON).

---

### Task 6 — Disk-spool watermark, eviction, и fail-closed `record_session`

**Цель:** защитить FS-узлы от тихого выхода spool-диска из строя. Если диск переполнен:
1. **Alert** срабатывает заранее (≥70%) до отказа.
2. **Emergency eviction** удаляет уже uploaded WAV-файлы по возрасту, чтобы освободить место без потери данных.
3. **`record_session` fail-closed** — если spool недоступен для записи, дозвон отказывается с понятным error event'ом, а не идёт без записи (152-ФЗ нарушение).

**Файлы:**
- Создать: `roles/freeswitch/files/spool-watchdog.sh` (Bash скрипт-watcher).
- Создать: `roles/freeswitch/templates/spool-watchdog.service.j2`, `spool-watchdog.timer.j2` (systemd unit + timer).
- Изменить: `roles/freeswitch/templates/dialplan_default.xml.j2` — pre-check spool-dir перед `record_session`.
- Изменить: `roles/freeswitch/templates/freeswitch_exporter.yml.j2` — экспортировать `node_filesystem_avail_bytes` уже включён по умолчанию (verify).

- [ ] **Step 1: Написать `spool-watchdog.sh`**

```bash
#!/usr/bin/env bash
# spool-watchdog: monitors /var/spool/sociopulse usage and evicts already-
# uploaded recordings if usage exceeds the high-water mark.
#
# Run from systemd timer every 60 seconds.
#
# Tiers:
#   <70%  : nothing to do.
#   70-85%: log warning. (Prometheus alert WARNING -- via node_exporter.)
#   ≥85%  : emergency eviction — delete WAV files whose .uploaded sentinel
#           exists AND mtime > 4h. Stop after freeing 5GB.
#   ≥95%  : critical eviction — same but mtime > 30 min. Page on-call.
#   ≥98%  : touch /var/run/sociopulse-spool-full sentinel; dialplan
#           pre-check refuses new `record_session` calls.
set -euo pipefail

SPOOL=/var/spool/sociopulse
HWM_WARN=70
HWM_EVICT=85
HWM_CRIT=95
HWM_DRAIN=98
SENTINEL=/var/run/sociopulse-spool-full

usage_pct() {
    df -P "$SPOOL" | awk 'NR==2 {gsub("%","",$5); print $5}'
}

evict_uploaded() {
    local age_min=$1
    # Find .wav files whose .uploaded sentinel exists and is older than age_min.
    find "$SPOOL" -maxdepth 1 -name '*.wav' -mmin "+$age_min" \
        -exec test -f "{}.uploaded" \; -print -delete
}

USE=$(usage_pct)
logger -t spool-watchdog "spool usage=${USE}%"

if (( USE >= HWM_DRAIN )); then
    touch "$SENTINEL"
    logger -t spool-watchdog "CRITICAL: spool ≥${HWM_DRAIN}%; refusing new recordings"
elif (( USE >= HWM_CRIT )); then
    rm -f "$SENTINEL"
    evict_uploaded 30
    logger -t spool-watchdog "EMERGENCY eviction (≥${HWM_CRIT}%): freed old uploaded files"
elif (( USE >= HWM_EVICT )); then
    rm -f "$SENTINEL"
    evict_uploaded 240
    logger -t spool-watchdog "high-water eviction (≥${HWM_EVICT}%): freed 4h-old uploaded files"
elif (( USE >= HWM_WARN )); then
    rm -f "$SENTINEL"
    logger -t spool-watchdog "WARN: spool ≥${HWM_WARN}%"
else
    rm -f "$SENTINEL"
fi
```

- [ ] **Step 2: Systemd unit + timer**

`spool-watchdog.service.j2`:

```ini
[Unit]
Description=SocioPulse spool watchdog
[Service]
Type=oneshot
ExecStart=/usr/local/bin/spool-watchdog.sh
```

`spool-watchdog.timer.j2`:

```ini
[Unit]
Description=Run spool-watchdog every 60s
[Timer]
OnBootSec=30s
OnUnitActiveSec=60s
[Install]
WantedBy=timers.target
```

Ansible role installs both, `systemctl enable --now spool-watchdog.timer`.

- [ ] **Step 3: Dialplan pre-check (fail-closed `record_session`)**

In `roles/freeswitch/templates/dialplan_default.xml.j2`, modify the `start_recording` extension:

```xml
<extension name="start_recording" continue="true">
  <!-- Refuse to start recording if the spool watchdog has marked the
       disk as full. We hang up the call rather than serving an
       unrecorded sociology call (152-ФЗ violation). -->
  <condition field="${file_exists(/var/run/sociopulse-spool-full)}" expression="^true$">
    <action application="log" data="ERR start_recording: spool full sentinel present; refusing call ${sociopulse_call_id}"/>
    <action application="set" data="hangup_cause=NETWORK_OUT_OF_ORDER"/>
    <action application="hangup"/>
  </condition>
  <condition field="${recording_required}" expression="^(true|1|yes)$">
    <!-- explicit-yes only: empty/unset = no recording. Backend always sets true on originate (see §B.2). -->
    <action application="set" data="RECORD_STEREO=true"/>
    <!-- ... existing actions ... -->
    <action application="record_session" data="${recording_path}"/>
  </condition>
</extension>
```

Note: I'm tightening `recording_required` to require explicit "yes" while leaving `consent_required` regex as-is — empty/unset → consent prompt plays (fail-closed for RU).

- [ ] **Step 4: Prometheus alert rules**

Add to `helm/freeswitch-monitoring/values.yaml` (or wherever the alert rules live):

```yaml
groups:
  - name: freeswitch-spool
    rules:
      - alert: FSSpoolHighWaterMark
        expr: 100 * (1 - node_filesystem_avail_bytes{mountpoint="/var/spool/sociopulse"} / node_filesystem_size_bytes{mountpoint="/var/spool/sociopulse"}) > 70
        for: 5m
        labels: { severity: warning }
        annotations:
          summary: "FS-{{ $labels.instance }} spool ≥ 70% full"
          runbook_url: "https://wiki/runbooks/fs-spool-full"
      - alert: FSSpoolCritical
        expr: 100 * (1 - node_filesystem_avail_bytes{mountpoint="/var/spool/sociopulse"} / node_filesystem_size_bytes{mountpoint="/var/spool/sociopulse"}) > 90
        for: 1m
        labels: { severity: critical }
        annotations:
          summary: "FS-{{ $labels.instance }} spool CRITICAL ≥ 90% — calls being rejected at 98%"
          runbook_url: "https://wiki/runbooks/fs-spool-full"
```

- [ ] **Step 5: Integration test (Ansible Molecule)**

In `roles/freeswitch/molecule/default/verify.yml`:

```yaml
- name: Spool watchdog timer is enabled
  systemd:
    name: spool-watchdog.timer
    state: started
    enabled: true

- name: Sentinel absent on healthy disk
  stat:
    path: /var/run/sociopulse-spool-full
  register: sentinel
  failed_when: sentinel.stat.exists
```

A second test (chaos) populates `/var/spool/sociopulse` to 99% with `dd`, runs the watchdog manually, asserts sentinel created, runs SIPp call, expects `record_session` to refuse with NETWORK_OUT_OF_ORDER.

- [ ] **Step 6: Commit**

```bash
git add roles/freeswitch/ helm/freeswitch-monitoring/values.yaml
git commit -m "fs: add spool watchdog + fail-closed record_session at 98% disk usage"
```

**Operational note:** при срабатывании sentinel ≥98% дозвоны отвергаются с `NETWORK_OUT_OF_ORDER`. Для оператора это видится как "звонок не удалось установить — попробовать позже". Это сознательная компромиссная позиция: лучше отказать в звонке, чем записывать без согласия / без места на диск. Page on-call → диагностируется по runbook'у `fs-spool-full.md` (Plan 20).

---

---

## Self-review

**Spec coverage** (against §7, §9, ADR-001/002/005/007, §16.1, §13.1-2, §FR-E):
- ADR-001/002 FreeSWITCH self-hosted, multi-trunk routing с health-check'ами. ✓
- §7 топология: 3 ноды на старте, public IP для SIP-trunk'ов, kernel-tuning (RT-priority, max-sessions=200). ✓
- §7.2 sofia-профили `trunks` (gateway-секции) + `operators` (verto-binding mTLS на :8082). ✓
- §7.3 dialplan: originate → playback consent prompt → record_session → bridge на оператора. Маршрутизация trunk через переменную `sip_trunk_id` из originate. ✓
- §9 запись: `mod_record_session` пишет .wav 8kHz PCM mono → fsnotify watcher uploader → ffmpeg → Opus 32k 16000Hz → KMS GenerateDataKey → AES-256-GCM client-side encrypt → S3 PUT → gRPC Recording.Commit → удаление локального файла после ack. ✓
- ADR-005 client-side AES-256-GCM с envelope KMS (per-recording DEK, encrypted by per-tenant KEK). ✓
- ADR-007 storage tiers: hot → cold → delete по lifecycle policy. ✓
- §13.1-2 promo консента перед записью (URL аудио из `tenant_settings.consent_prompt_url`). ✓
- R-1 mitigation (TURN-сервер): отдельная VM coturn 4.6+, listen :3478 UDP/TCP + :5349 TLS, long-term-creds в Lockbox. ✓
- ESL :8021 mTLS-only, доступен только из k8s VPC subnet. ✓
- Packer image (Ubuntu 22.04 + FreeSWITCH 1.10.10 + recording-uploader + node_exporter + blackbox_exporter) собирается через packer build. ✓
- Ansible playbooks с `molecule` + `ansible-lint` тестами. ✓
- Terraform-модуль `freeswitch-cluster` (count=3 VM, 4vCPU/8GB/200GB NVMe), security-group whitelist операторов. ✓
- DNS-записи для всех нод и TURN-сервера. ✓
- E2E smoke: Packer-build, VM-boot, SIPp-симуляция входящего вызова, запись пишется в spool. ✓

**Placeholder scan:** End-to-end test включает зависимость от Plan 09 (`telephony-bridge originate/bridge`) — это явно отмечено как cross-plan dependency.

**Type/name consistency:** `recording-uploader` бинарь, конфиг `/etc/sociopulse/recording-uploader.yaml`, метрики на :9091, gRPC `Recording.Commit` — все имена стабильны и используются Plan 12.

**Out of scope (correctly deferred):**
- ESL-клиент (telephony-bridge Go) — Plan 09.
- Recording-metadata API (gRPC server side) — Plan 12.
- Listen-in (admin прослушивает живой звонок) — Plan 11.
- Per-tenant trunks (тенанты приносят свои SIP-trunk'и) — v2.

Plan 08 verified.

---

**Plan complete and saved to `docs/superpowers/plans/2026-05-06-08-freeswitch-cluster.md`.**

