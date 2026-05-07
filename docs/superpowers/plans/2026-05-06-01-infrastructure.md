# Infrastructure Implementation Plan

> **🛑 DEFERRED — Phase 2 (pre-production cutover).**
>
> Per project decision (2026-05-06), this plan is **not executed during product development**. Phase 1 (Plans 00, 02-19) runs entirely on a developer's laptop via `docker-compose.dev.yml` (Plan 02 Task 5) plus an in-Docker FreeSWITCH container for telephony work (Plan 08 Task 0). Zero cloud spend during product implementation.
>
> Execute this plan only when Phase 1 is complete and you're ready to deploy to staging/production. At that point also re-evaluate which data-layer hosting model to use (managed Yandex services / self-hosted on Compute VMs / hybrid) — see `docs/superpowers/reviews/2026-05-06-architecture-and-plans-review.md` "Phase 2 deployment" section. The plan as written below assumes **fully managed** Yandex Cloud services; if you choose self-hosted, replace the Managed-* Terraform modules with `compute_instance` + Ansible playbooks (Patroni for PG, ClickHouse Keeper, Valkey Sentinel) at that point.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use `- [ ]` checkbox syntax.

**Goal:** Provision Yandex Cloud infrastructure (network, MKS, MPG, MCH, MRD, KMS, Lockbox, Object Storage) via Terraform; install Kubernetes baseline (ingress-nginx, cert-manager, ArgoCD, NATS, kube-prometheus-stack + Loki + Tempo); deploy the `cmd/api` Helm chart end-to-end so the running pod's `/healthz` is reachable through HTTPS.

**Architecture:** Terraform code under `deployments/terraform/` with environments dir (`environments/dev`, `staging`, `production`) and reusable modules (`modules/network`, `modules/mks`, `modules/managed-pg`, ...). Remote state in Yandex Object Storage. Helm charts under `deployments/helm/` (chart per binary). ArgoCD pulls from this repo and reconciles. Secrets in Yandex Lockbox, mounted via External Secrets Operator (ESO) since the official Yandex Lockbox CSI driver coverage is partial.

**Tech Stack:** Terraform 1.7+, Yandex Cloud Provider, Helm 3, ArgoCD 2.10+, kube-prometheus-stack, ingress-nginx, cert-manager 1.14+, External Secrets Operator 0.9+, NATS 2.10+ via Helm.

**Spec sections covered:** §16.1 (infra topology), §16.2 (network), §16.3 (IaC), §16.6 (DR), §15 (observability stack baseline).

**Prerequisites:**
- Plan 00 completed (Go module, cmd/api with /healthz, Dockerfile).
- Yandex Cloud organization with billing account + access for `yc` CLI.
- Cloud-folder ID and zone access available as `YC_TOKEN`, `YC_CLOUD_ID`, `YC_FOLDER_ID`.

---

## File Structure

```
deployments/
├── terraform/
│   ├── README.md
│   ├── environments/
│   │   ├── dev/
│   │   │   ├── main.tf
│   │   │   ├── variables.tf
│   │   │   ├── terraform.tfvars.example
│   │   │   └── backend.tf
│   │   ├── staging/    # populated later, structure mirrors dev
│   │   └── production/
│   ├── modules/
│   │   ├── network/
│   │   │   ├── main.tf
│   │   │   ├── variables.tf
│   │   │   └── outputs.tf
│   │   ├── mks/
│   │   ├── managed-pg/
│   │   ├── managed-ch/
│   │   ├── managed-redis/
│   │   ├── object-storage/
│   │   ├── kms/
│   │   └── lockbox/
│   └── versions.tf
│
├── helm/
│   ├── api/                         # cmd/api chart
│   │   ├── Chart.yaml
│   │   ├── values.yaml
│   │   ├── values-dev.yaml
│   │   └── templates/
│   │       ├── _helpers.tpl
│   │       ├── deployment.yaml
│   │       ├── service.yaml
│   │       ├── ingress.yaml
│   │       ├── serviceaccount.yaml
│   │       ├── configmap.yaml
│   │       └── externalsecret.yaml
│   ├── worker/                      # placeholder, scaffolded later
│   ├── nats/                        # values overrides for upstream NATS chart
│   └── README.md
│
├── argocd/
│   ├── README.md
│   ├── applications/
│   │   ├── api-dev.yaml
│   │   ├── nats-dev.yaml
│   │   ├── monitoring-dev.yaml
│   │   ├── ingress-dev.yaml
│   │   └── external-secrets-dev.yaml
│   ├── app-of-apps/
│   │   └── dev.yaml
│   └── projects/
│       └── sociopulse.yaml
│
└── k8s-bootstrap/                  # one-off install scripts
    ├── README.md
    ├── 01-cert-manager.sh
    ├── 02-ingress-nginx.sh
    ├── 03-external-secrets.sh
    ├── 04-argocd.sh
    └── 05-bootstrap-app-of-apps.sh
```

---

## Task 1: Bootstrap Terraform repo structure

**Files:** all paths under `deployments/terraform/`.

- [ ] **Step 1: Create directories**

```bash
cd "$(git rev-parse --show-toplevel)"
mkdir -p deployments/terraform/{environments/dev,environments/staging,environments/production}
mkdir -p deployments/terraform/modules/{network,mks,managed-pg,managed-ch,managed-redis,object-storage,kms,lockbox}
```

- [ ] **Step 2: Add Terraform version pinning**

Create `deployments/terraform/versions.tf`:

```hcl
terraform {
  required_version = ">= 1.7.0, < 2.0.0"

  required_providers {
    yandex = {
      source  = "yandex-cloud/yandex"
      version = "~> 0.114.0"
    }
    helm = {
      source  = "hashicorp/helm"
      version = "~> 2.13.0"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.27.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6.0"
    }
    null = {
      source  = "hashicorp/null"
      version = "~> 3.2.0"
    }
  }
}
```

- [ ] **Step 3: Add `deployments/terraform/README.md`**

```markdown
# Terraform — СоциоПульс

## Layout

- `environments/<env>/` — per-environment root: `main.tf`, `variables.tf`, `terraform.tfvars`, `backend.tf`.
- `modules/<name>/` — reusable modules.
- `versions.tf` — provider pinning (shared, copied or symlinked into each env).

## Bootstrap

Before first `terraform init` for an environment:

1. Create the bucket for remote state (one-time, manually):
   ```
   yc storage bucket create --name sociopulse-tfstate-<env> --acl=private
   ```
2. Create a service account for Terraform with `editor` role on the folder.
3. Export `YC_TOKEN` from `yc iam create-token`.

## Per-environment workflow

```bash
cd deployments/terraform/environments/dev
cp terraform.tfvars.example terraform.tfvars   # fill in your values
terraform init
terraform plan -out=tfplan
terraform apply tfplan
```

## Modules

| Module | Purpose |
|---|---|
| `network` | VPC + subnets per AZ + security groups |
| `mks` | Managed Kubernetes cluster + node groups |
| `managed-pg` | Managed PostgreSQL (master + replica) |
| `managed-ch` | Managed ClickHouse (single host on dev) |
| `managed-redis` | Managed Redis (master + replica) |
| `object-storage` | S3 buckets (recordings/backups/reports/consent-prompts) |
| `kms` | Platform-level KMS keys (per-tenant keys created at runtime by tenancy module) |
| `lockbox` | Lockbox secrets bundle |
```

- [ ] **Step 4: Commit**

```bash
git add deployments/terraform/versions.tf deployments/terraform/README.md
git commit -m "infra: scaffold terraform repo structure"
```

---

## Task 2: Network module

**Files:** `deployments/terraform/modules/network/{main.tf,variables.tf,outputs.tf}`

- [ ] **Step 1: `variables.tf`**

```hcl
variable "folder_id" {
  type        = string
  description = "Yandex Cloud folder ID"
}

variable "name_prefix" {
  type        = string
  description = "Resource name prefix (e.g., 'sp-dev')"
}

variable "zones" {
  type        = list(string)
  description = "List of AZs to deploy into (e.g., ru-central1-a, ru-central1-b)"
  default     = ["ru-central1-a", "ru-central1-b", "ru-central1-d"]
}

variable "subnet_cidrs" {
  type        = list(string)
  description = "Subnet CIDR per zone"
  default     = ["10.10.0.0/20", "10.10.16.0/20", "10.10.32.0/20"]

  validation {
    condition     = length(var.subnet_cidrs) >= 1
    error_message = "At least one subnet CIDR required."
  }
}

variable "labels" {
  type    = map(string)
  default = {}
}
```

- [ ] **Step 2: `main.tf`**

```hcl
resource "yandex_vpc_network" "this" {
  folder_id = var.folder_id
  name      = "${var.name_prefix}-vpc"
  labels    = var.labels
}

resource "yandex_vpc_subnet" "this" {
  for_each = { for idx, z in var.zones : z => var.subnet_cidrs[idx] if idx < length(var.subnet_cidrs) }

  folder_id      = var.folder_id
  name           = "${var.name_prefix}-subnet-${each.key}"
  network_id     = yandex_vpc_network.this.id
  zone           = each.key
  v4_cidr_blocks = [each.value]
  labels         = var.labels
}

# A reusable security group for "internal-only" workloads (DB, Redis, ClickHouse)
resource "yandex_vpc_security_group" "internal" {
  folder_id   = var.folder_id
  name        = "${var.name_prefix}-internal"
  description = "Internal-only traffic between workloads in this VPC"
  network_id  = yandex_vpc_network.this.id
  labels      = var.labels

  ingress {
    description    = "All TCP within VPC"
    protocol       = "TCP"
    v4_cidr_blocks = var.subnet_cidrs
    from_port      = 0
    to_port        = 65535
  }

  ingress {
    description    = "All UDP within VPC"
    protocol       = "UDP"
    v4_cidr_blocks = var.subnet_cidrs
    from_port      = 0
    to_port        = 65535
  }

  egress {
    description    = "Egress all"
    protocol       = "ANY"
    v4_cidr_blocks = ["0.0.0.0/0"]
    from_port      = 0
    to_port        = 65535
  }
}
```

- [ ] **Step 3: `outputs.tf`**

```hcl
output "network_id" {
  value = yandex_vpc_network.this.id
}

output "subnet_ids" {
  value = { for z, s in yandex_vpc_subnet.this : z => s.id }
}

output "internal_security_group_id" {
  value = yandex_vpc_security_group.internal.id
}
```

- [ ] **Step 4: Commit**

```bash
git add deployments/terraform/modules/network/
git commit -m "infra(terraform): add network module"
```

---

## Task 3: KMS module

**Files:** `deployments/terraform/modules/kms/{main.tf,variables.tf,outputs.tf}`

- [ ] **Step 1: `variables.tf`**

```hcl
variable "folder_id" {
  type = string
}

variable "name_prefix" {
  type = string
}

variable "labels" {
  type    = map(string)
  default = {}
}
```

- [ ] **Step 2: `main.tf`**

```hcl
# Platform-level keys. Per-tenant keys are created at runtime by the
# `tenancy` module (Plan 04) via the Yandex KMS API — they are NOT defined here.

resource "yandex_kms_symmetric_key" "platform" {
  folder_id         = var.folder_id
  name              = "${var.name_prefix}-platform-key"
  description       = "Platform-level key (system secrets, postgres backup encryption)"
  default_algorithm = "AES_256"
  rotation_period   = "8760h" # 1 year
  labels            = var.labels
}

resource "yandex_kms_symmetric_key" "audit" {
  folder_id         = var.folder_id
  name              = "${var.name_prefix}-audit-key"
  description       = "Encrypts audit log archive in S3 cold tier"
  default_algorithm = "AES_256"
  rotation_period   = "8760h"
  labels            = var.labels
}
```

- [ ] **Step 3: `outputs.tf`**

```hcl
output "platform_key_id" {
  value = yandex_kms_symmetric_key.platform.id
}

output "audit_key_id" {
  value = yandex_kms_symmetric_key.audit.id
}
```

- [ ] **Step 4: Commit**

```bash
git add deployments/terraform/modules/kms/
git commit -m "infra(terraform): add kms module"
```

---

## Task 4: Object Storage module

**Files:** `deployments/terraform/modules/object-storage/{main.tf,variables.tf,outputs.tf}`

- [ ] **Step 1: `variables.tf`**

```hcl
variable "folder_id" {
  type = string
}

variable "name_prefix" {
  type = string
}

variable "kms_key_id" {
  type        = string
  description = "Platform KMS key for SSE-KMS on backups bucket"
}

variable "labels" {
  type    = map(string)
  default = {}
}

variable "recordings_retention_days" {
  type    = number
  default = 365
}

variable "recordings_cold_after_days" {
  type    = number
  default = 365
}

variable "recordings_delete_after_days" {
  type    = number
  default = 1095 # 3 years total
}
```

- [ ] **Step 2: `main.tf`**

```hcl
# A service account that owns the buckets — used by Terraform and by app
# pods (via Lockbox-injected IAM keys; see Plan 04 for the access path).
resource "yandex_iam_service_account" "storage" {
  folder_id = var.folder_id
  name      = "${var.name_prefix}-storage-sa"
}

resource "yandex_resourcemanager_folder_iam_member" "storage_admin" {
  folder_id = var.folder_id
  role      = "storage.admin"
  member    = "serviceAccount:${yandex_iam_service_account.storage.id}"
}

# Static keys for S3-compatible API access
resource "yandex_iam_service_account_static_access_key" "storage" {
  service_account_id = yandex_iam_service_account.storage.id
  description        = "Static keys for S3 API; rotated on Plan 04 onwards via Lockbox"
}

# ----- Buckets -----
# Recordings bucket — bucket-per-tenant gets created at tenant-onboarding time
# by the `tenancy` module. This shared "platform" bucket is ONLY for system files.

resource "yandex_storage_bucket" "backups" {
  bucket    = "${var.name_prefix}-backups"
  folder_id = var.folder_id

  access_key = yandex_iam_service_account_static_access_key.storage.access_key
  secret_key = yandex_iam_service_account_static_access_key.storage.secret_key

  acl = "private"

  versioning {
    enabled = true
  }

  server_side_encryption_configuration {
    rule {
      apply_server_side_encryption_by_default {
        kms_master_key_id = var.kms_key_id
        sse_algorithm     = "aws:kms"
      }
    }
  }

  lifecycle_rule {
    id      = "rotate-old-backups"
    enabled = true
    expiration {
      days = 90 # WAL archives + daily snapshots older than this
    }
  }
}

resource "yandex_storage_bucket" "reports" {
  bucket    = "${var.name_prefix}-reports"
  folder_id = var.folder_id

  access_key = yandex_iam_service_account_static_access_key.storage.access_key
  secret_key = yandex_iam_service_account_static_access_key.storage.secret_key

  acl = "private"

  lifecycle_rule {
    id      = "auto-cleanup-reports"
    enabled = true
    expiration {
      days = 30 # generated reports — short-lived presigned URLs anyway
    }
  }
}

resource "yandex_storage_bucket" "consent_prompts" {
  bucket    = "${var.name_prefix}-consent-prompts"
  folder_id = var.folder_id

  access_key = yandex_iam_service_account_static_access_key.storage.access_key
  secret_key = yandex_iam_service_account_static_access_key.storage.secret_key

  acl = "private"
}

resource "yandex_storage_bucket" "tfstate" {
  bucket    = "${var.name_prefix}-tfstate"
  folder_id = var.folder_id

  access_key = yandex_iam_service_account_static_access_key.storage.access_key
  secret_key = yandex_iam_service_account_static_access_key.storage.secret_key

  acl = "private"

  versioning {
    enabled = true
  }

  server_side_encryption_configuration {
    rule {
      apply_server_side_encryption_by_default {
        kms_master_key_id = var.kms_key_id
        sse_algorithm     = "aws:kms"
      }
    }
  }
}
```

- [ ] **Step 3: `outputs.tf`**

```hcl
output "service_account_id" {
  value = yandex_iam_service_account.storage.id
}

output "static_access_key" {
  value     = yandex_iam_service_account_static_access_key.storage.access_key
  sensitive = true
}

output "static_secret_key" {
  value     = yandex_iam_service_account_static_access_key.storage.secret_key
  sensitive = true
}

output "buckets" {
  value = {
    backups         = yandex_storage_bucket.backups.bucket
    reports         = yandex_storage_bucket.reports.bucket
    consent_prompts = yandex_storage_bucket.consent_prompts.bucket
    tfstate         = yandex_storage_bucket.tfstate.bucket
  }
}
```

- [ ] **Step 4: Commit**

```bash
git add deployments/terraform/modules/object-storage/
git commit -m "infra(terraform): add object-storage module (buckets + lifecycle)"
```

---

## Task 5: Managed PostgreSQL module

**Files:** `deployments/terraform/modules/managed-pg/{main.tf,variables.tf,outputs.tf}`

- [ ] **Step 1: `variables.tf`**

```hcl
variable "folder_id"          { type = string }
variable "name_prefix"        { type = string }
variable "network_id"         { type = string }
variable "subnet_ids"         { type = map(string) }
variable "security_group_ids" { type = list(string) }
variable "labels"             { type = map(string)  default = {} }

variable "version" {
  type    = string
  default = "16"
}

variable "resource_preset_id" {
  type    = string
  default = "s2.micro" # 2 vCPU / 8 GB; bump for staging/prod
}

variable "disk_size_gb" {
  type    = number
  default = 50
}

variable "disk_type" {
  type    = string
  default = "network-ssd"
}

variable "high_availability" {
  type    = bool
  default = true
}

variable "db_name" {
  type    = string
  default = "sociopulse"
}

variable "app_user" {
  type    = string
  default = "app"
}

variable "app_password" {
  type      = string
  sensitive = true
}
```

- [ ] **Step 2: `main.tf`**

```hcl
locals {
  zones = keys(var.subnet_ids)
  hosts = local.zones[*]
}

resource "yandex_mdb_postgresql_cluster" "this" {
  folder_id = var.folder_id
  name      = "${var.name_prefix}-pg"
  network_id = var.network_id
  environment = "PRODUCTION"   # Yandex API only accepts PRODUCTION/PRESTABLE; we use PRODUCTION for all envs
  labels      = var.labels

  config {
    version = var.version

    resources {
      resource_preset_id = var.resource_preset_id
      disk_size          = var.disk_size_gb
      disk_type_id       = var.disk_type
    }

    backup_window_start {
      hours   = 3
      minutes = 0
    }

    postgresql_config = {
      max_connections          = 200
      shared_buffers           = "{1/4 RAM}"
      log_min_duration_statement = 100  # ms
      timezone                 = "UTC"
    }

    performance_diagnostics {
      enabled                      = true
      sessions_sampling_interval   = 60
      statements_sampling_interval = 600
    }
  }

  database {
    name  = var.db_name
    owner = var.app_user
    extension {
      name = "pgcrypto"
    }
    extension {
      name = "uuid-ossp"
    }
  }

  user {
    name     = var.app_user
    password = var.app_password
    permission {
      database_name = var.db_name
    }
    grants = ["CREATE"]
  }

  dynamic "host" {
    for_each = var.high_availability ? local.hosts : [local.hosts[0]]
    content {
      zone             = host.value
      subnet_id        = var.subnet_ids[host.value]
      assign_public_ip = false
    }
  }

  security_group_ids = var.security_group_ids
}
```

- [ ] **Step 3: `outputs.tf`**

```hcl
output "cluster_id" {
  value = yandex_mdb_postgresql_cluster.this.id
}

output "fqdn" {
  value = [for h in yandex_mdb_postgresql_cluster.this.host : h.fqdn]
}

output "connection_string" {
  value     = "postgres://${var.app_user}:${var.app_password}@${yandex_mdb_postgresql_cluster.this.host[0].fqdn}:6432/${var.db_name}?sslmode=require"
  sensitive = true
}
```

- [ ] **Step 4: Commit**

```bash
git add deployments/terraform/modules/managed-pg/
git commit -m "infra(terraform): add managed-pg module"
```

---

## Task 6: Managed Redis (Valkey) & Managed ClickHouse modules

> **Note on Redis → Valkey renaming (2025).** Yandex Cloud has renamed «Managed Service for Redis» to «Managed Service for Valkey» in the UI and catalog after Redis Inc. changed the upstream license; Valkey is the Linux Foundation fork of Redis 7.2 and is wire-protocol compatible. **Application code is unaffected** — the `go-redis` client and our config.redis section work as-is. **Terraform side**: as of writing, the `yandex_mdb_redis_cluster` resource is still functional and provisions the same engine; if the provider has added a `yandex_mdb_valkey_cluster` alias by the time this plan executes, prefer it (semantics identical, just a renamed resource). When the executor opens this plan, run `terraform-provider-yandex` docs check first; if both are available, use `valkey`; if only `redis` exists, keep `redis`.

**Files:**
- `deployments/terraform/modules/managed-redis/{main.tf,variables.tf,outputs.tf}` (rename to `managed-valkey/` if provider supports it)
- `deployments/terraform/modules/managed-ch/{main.tf,variables.tf,outputs.tf}`

- [ ] **Step 1: managed-redis `variables.tf`**

```hcl
variable "folder_id"   { type = string }
variable "name_prefix" { type = string }
variable "network_id"  { type = string }
variable "subnet_ids"  { type = map(string) }
variable "security_group_ids" { type = list(string) }
variable "labels"      { type = map(string) default = {} }

variable "resource_preset_id" {
  type    = string
  default = "hm1.nano"
}

variable "disk_size_gb" {
  type    = number
  default = 16
}

variable "redis_password" {
  type      = string
  sensitive = true
}
```

- [ ] **Step 2: managed-redis `main.tf`**

```hcl
resource "yandex_mdb_redis_cluster" "this" {
  folder_id   = var.folder_id
  name        = "${var.name_prefix}-redis"
  network_id  = var.network_id
  environment = "PRODUCTION"
  labels      = var.labels

  config {
    version  = "7.2"
    password = var.redis_password
    maxmemory_policy = "allkeys-lru"
    notify_keyspace_events = "Ex"  # expire events for asynq + presence
  }

  resources {
    resource_preset_id = var.resource_preset_id
    disk_size          = var.disk_size_gb
    disk_type_id       = "network-ssd"
  }

  dynamic "host" {
    for_each = keys(var.subnet_ids)
    content {
      zone      = host.value
      subnet_id = var.subnet_ids[host.value]
    }
  }

  sharded            = false
  tls_enabled        = true
  persistence_mode   = "ON"
  security_group_ids = var.security_group_ids
}
```

- [ ] **Step 3: managed-redis `outputs.tf`**

```hcl
output "cluster_id" { value = yandex_mdb_redis_cluster.this.id }
output "hosts"      { value = [for h in yandex_mdb_redis_cluster.this.host : h.fqdn] }
output "connection_string" {
  value     = "rediss://:${var.redis_password}@${yandex_mdb_redis_cluster.this.host[0].fqdn}:6380"
  sensitive = true
}
```

- [ ] **Step 4: managed-ch `variables.tf`**

```hcl
variable "folder_id"   { type = string }
variable "name_prefix" { type = string }
variable "network_id"  { type = string }
variable "subnet_ids"  { type = map(string) }
variable "security_group_ids" { type = list(string) }
variable "labels"      { type = map(string) default = {} }

variable "resource_preset_id" {
  type    = string
  default = "s2.micro"
}

variable "disk_size_gb" {
  type    = number
  default = 100
}

variable "ch_user" {
  type    = string
  default = "app"
}

variable "ch_password" {
  type      = string
  sensitive = true
}
```

- [ ] **Step 5: managed-ch `main.tf`**

```hcl
resource "yandex_mdb_clickhouse_cluster" "this" {
  folder_id   = var.folder_id
  name        = "${var.name_prefix}-ch"
  network_id  = var.network_id
  environment = "PRODUCTION"
  labels      = var.labels

  clickhouse {
    resources {
      resource_preset_id = var.resource_preset_id
      disk_size          = var.disk_size_gb
      disk_type_id       = "network-ssd"
    }
  }

  database {
    name = "sociopulse"
  }

  user {
    name     = var.ch_user
    password = var.ch_password
    permission {
      database_name = "sociopulse"
    }
  }

  dynamic "host" {
    for_each = slice(keys(var.subnet_ids), 0, 1)
    content {
      type      = "CLICKHOUSE"
      zone      = host.value
      subnet_id = var.subnet_ids[host.value]
    }
  }

  security_group_ids = var.security_group_ids
}
```

- [ ] **Step 6: managed-ch `outputs.tf`**

```hcl
output "cluster_id" { value = yandex_mdb_clickhouse_cluster.this.id }
output "hosts"      { value = [for h in yandex_mdb_clickhouse_cluster.this.host : h.fqdn] }
output "connection_string" {
  value     = "clickhouse://${var.ch_user}:${var.ch_password}@${yandex_mdb_clickhouse_cluster.this.host[0].fqdn}:9440/sociopulse?secure=true"
  sensitive = true
}
```

- [ ] **Step 7: Commit**

```bash
git add deployments/terraform/modules/managed-redis/ deployments/terraform/modules/managed-ch/
git commit -m "infra(terraform): add managed-redis and managed-ch modules"
```

---

## Task 7: Managed Kubernetes (MKS) module

**Files:** `deployments/terraform/modules/mks/{main.tf,variables.tf,outputs.tf}`

- [ ] **Step 1: `variables.tf`**

```hcl
variable "folder_id"   { type = string }
variable "name_prefix" { type = string }
variable "network_id"  { type = string }
variable "subnet_ids"  { type = map(string) }
variable "labels"      { type = map(string) default = {} }

variable "k8s_version" {
  type    = string
  default = "1.29"
}

variable "node_pool_size" {
  type        = number
  default     = 3
}

variable "node_resource_preset" {
  type    = string
  default = "standard-v3" # 4 vCPU / 16 GB
}

variable "node_disk_gb" {
  type    = number
  default = 64
}
```

- [ ] **Step 2: `main.tf`**

```hcl
resource "yandex_iam_service_account" "k8s" {
  folder_id = var.folder_id
  name      = "${var.name_prefix}-k8s-sa"
}

resource "yandex_resourcemanager_folder_iam_member" "k8s_clusters_agent" {
  folder_id = var.folder_id
  role      = "k8s.clusters.agent"
  member    = "serviceAccount:${yandex_iam_service_account.k8s.id}"
}

resource "yandex_resourcemanager_folder_iam_member" "vpc_public_admin" {
  folder_id = var.folder_id
  role      = "vpc.publicAdmin"
  member    = "serviceAccount:${yandex_iam_service_account.k8s.id}"
}

resource "yandex_resourcemanager_folder_iam_member" "container_registry_puller" {
  folder_id = var.folder_id
  role      = "container-registry.images.puller"
  member    = "serviceAccount:${yandex_iam_service_account.k8s.id}"
}

resource "yandex_kubernetes_cluster" "this" {
  folder_id  = var.folder_id
  name       = "${var.name_prefix}-mks"
  network_id = var.network_id
  labels     = var.labels

  master {
    version = var.k8s_version

    zonal {
      zone      = keys(var.subnet_ids)[0]
      subnet_id = var.subnet_ids[keys(var.subnet_ids)[0]]
    }

    public_ip = true # MKS API endpoint reachable from CI/agents

    maintenance_policy {
      auto_upgrade = true
      maintenance_window {
        start_time = "03:00"
        duration   = "3h"
      }
    }
  }

  service_account_id      = yandex_iam_service_account.k8s.id
  node_service_account_id = yandex_iam_service_account.k8s.id
  release_channel         = "STABLE"
}

resource "yandex_kubernetes_node_group" "default" {
  cluster_id  = yandex_kubernetes_cluster.this.id
  name        = "${var.name_prefix}-default-pool"
  description = "Default node pool"
  version     = var.k8s_version
  labels      = var.labels

  instance_template {
    platform_id = "standard-v3"

    resources {
      cores  = 4
      memory = 16
    }

    boot_disk {
      type = "network-ssd"
      size = var.node_disk_gb
    }

    network_interface {
      subnet_ids = values(var.subnet_ids)
      nat        = true
    }

    container_runtime {
      type = "containerd"
    }
  }

  scale_policy {
    fixed_scale {
      size = var.node_pool_size
    }
  }

  allocation_policy {
    location {
      zone = keys(var.subnet_ids)[0]
    }
  }
}
```

- [ ] **Step 3: `outputs.tf`**

```hcl
output "cluster_id" {
  value = yandex_kubernetes_cluster.this.id
}

output "external_endpoint" {
  value = yandex_kubernetes_cluster.this.master[0].external_v4_endpoint
}

output "ca_certificate" {
  value = yandex_kubernetes_cluster.this.master[0].cluster_ca_certificate
}

output "service_account_id" {
  value = yandex_iam_service_account.k8s.id
}
```

- [ ] **Step 4: Commit**

```bash
git add deployments/terraform/modules/mks/
git commit -m "infra(terraform): add managed-kubernetes module"
```

---

## Task 8: Lockbox module (secrets)

**Files:** `deployments/terraform/modules/lockbox/{main.tf,variables.tf,outputs.tf}`

- [ ] **Step 1: `variables.tf`**

```hcl
variable "folder_id"   { type = string }
variable "name_prefix" { type = string }
variable "labels"      { type = map(string) default = {} }

variable "secrets" {
  type = map(string)
  description = "Map of secret-key → plaintext value (passed in from environment via tfvars)"
  sensitive   = true
}
```

- [ ] **Step 2: `main.tf`**

```hcl
resource "yandex_lockbox_secret" "app" {
  folder_id = var.folder_id
  name      = "${var.name_prefix}-app"
  labels    = var.labels
}

resource "yandex_lockbox_secret_version" "v1" {
  secret_id = yandex_lockbox_secret.app.id

  dynamic "entries" {
    for_each = var.secrets
    content {
      key        = entries.key
      text_value = entries.value
    }
  }
}

# Service account for ESO (External Secrets Operator) to read this secret.
resource "yandex_iam_service_account" "secrets_reader" {
  folder_id = var.folder_id
  name      = "${var.name_prefix}-secrets-reader"
}

resource "yandex_resourcemanager_folder_iam_member" "secrets_viewer" {
  folder_id = var.folder_id
  role      = "lockbox.payloadViewer"
  member    = "serviceAccount:${yandex_iam_service_account.secrets_reader.id}"
}

resource "yandex_iam_service_account_key" "secrets_reader" {
  service_account_id = yandex_iam_service_account.secrets_reader.id
  description        = "Authorized key for ESO to call Lockbox"
  key_algorithm      = "RSA_4096"
}
```

- [ ] **Step 3: `outputs.tf`**

```hcl
output "secret_id" {
  value = yandex_lockbox_secret.app.id
}

output "reader_sa_id" {
  value = yandex_iam_service_account.secrets_reader.id
}

output "reader_authorized_key" {
  value     = yandex_iam_service_account_key.secrets_reader.private_key
  sensitive = true
}
```

- [ ] **Step 4: Commit**

```bash
git add deployments/terraform/modules/lockbox/
git commit -m "infra(terraform): add lockbox module"
```

---

## Task 9: Dev environment composition

**Files:** `deployments/terraform/environments/dev/{backend.tf,main.tf,variables.tf,terraform.tfvars.example}`

- [ ] **Step 1: `backend.tf`**

```hcl
terraform {
  backend "s3" {
    endpoint   = "storage.yandexcloud.net"
    bucket     = "sp-dev-tfstate"
    key        = "dev/terraform.tfstate"
    region     = "ru-central1"
    skip_region_validation      = true
    skip_credentials_validation = true
    skip_metadata_api_check     = true
    skip_requesting_account_id  = true
  }
}
```

Bucket `sp-dev-tfstate` must be created **before** `terraform init` (one-time, see README §Bootstrap).

- [ ] **Step 2: `variables.tf`**

```hcl
variable "yc_token"         { type = string sensitive = true }
variable "yc_cloud_id"      { type = string }
variable "yc_folder_id"     { type = string }

variable "name_prefix"      { type = string default = "sp-dev" }

variable "pg_password"      { type = string sensitive = true }
variable "redis_password"   { type = string sensitive = true }
variable "ch_password"      { type = string sensitive = true }
variable "jwt_secret"       { type = string sensitive = true }

variable "labels" {
  type = map(string)
  default = {
    project    = "sociopulse"
    env        = "dev"
    managed_by = "terraform"
  }
}
```

- [ ] **Step 3: `terraform.tfvars.example`**

```hcl
# Copy to terraform.tfvars and fill in your real values.
# DO NOT COMMIT terraform.tfvars (.gitignore covers it).

yc_token       = "y0_AgAAAA..."   # output of `yc iam create-token`
yc_cloud_id    = "b1g..."
yc_folder_id   = "b1g..."

# Generate these (e.g., openssl rand -hex 24)
pg_password    = "REPLACE_ME"
redis_password = "REPLACE_ME"
ch_password    = "REPLACE_ME"
jwt_secret     = "REPLACE_ME"
```

- [ ] **Step 4: `main.tf`**

```hcl
provider "yandex" {
  token     = var.yc_token
  cloud_id  = var.yc_cloud_id
  folder_id = var.yc_folder_id
  zone      = "ru-central1-a"
}

module "network" {
  source = "../../modules/network"

  folder_id    = var.yc_folder_id
  name_prefix  = var.name_prefix
  zones        = ["ru-central1-a", "ru-central1-b", "ru-central1-d"]
  subnet_cidrs = ["10.10.0.0/20", "10.10.16.0/20", "10.10.32.0/20"]
  labels       = var.labels
}

module "kms" {
  source = "../../modules/kms"

  folder_id   = var.yc_folder_id
  name_prefix = var.name_prefix
  labels      = var.labels
}

module "object_storage" {
  source = "../../modules/object-storage"

  folder_id   = var.yc_folder_id
  name_prefix = var.name_prefix
  kms_key_id  = module.kms.platform_key_id
  labels      = var.labels
}

module "managed_pg" {
  source = "../../modules/managed-pg"

  folder_id          = var.yc_folder_id
  name_prefix        = var.name_prefix
  network_id         = module.network.network_id
  subnet_ids         = module.network.subnet_ids
  security_group_ids = [module.network.internal_security_group_id]
  labels             = var.labels
  app_password       = var.pg_password
  resource_preset_id = "s2.micro"  # dev-tier
  disk_size_gb       = 50
  high_availability  = false       # dev: single host
}

module "managed_redis" {
  source = "../../modules/managed-redis"

  folder_id          = var.yc_folder_id
  name_prefix        = var.name_prefix
  network_id         = module.network.network_id
  subnet_ids         = { for k, v in module.network.subnet_ids : k => v if k == "ru-central1-a" } # single host on dev
  security_group_ids = [module.network.internal_security_group_id]
  labels             = var.labels
  redis_password     = var.redis_password
}

module "managed_ch" {
  source = "../../modules/managed-ch"

  folder_id          = var.yc_folder_id
  name_prefix        = var.name_prefix
  network_id         = module.network.network_id
  subnet_ids         = { for k, v in module.network.subnet_ids : k => v if k == "ru-central1-a" }
  security_group_ids = [module.network.internal_security_group_id]
  labels             = var.labels
  ch_password        = var.ch_password
}

module "mks" {
  source = "../../modules/mks"

  folder_id      = var.yc_folder_id
  name_prefix    = var.name_prefix
  network_id     = module.network.network_id
  subnet_ids     = module.network.subnet_ids
  labels         = var.labels
  node_pool_size = 3
}

module "lockbox" {
  source = "../../modules/lockbox"

  folder_id   = var.yc_folder_id
  name_prefix = var.name_prefix
  labels      = var.labels

  secrets = {
    pg_password    = var.pg_password
    redis_password = var.redis_password
    ch_password    = var.ch_password
    jwt_secret     = var.jwt_secret
    s3_access_key  = module.object_storage.static_access_key
    s3_secret_key  = module.object_storage.static_secret_key
  }
}

# Convenience outputs printed at apply
output "kubeconfig_command" {
  value = "yc managed-kubernetes cluster get-credentials --id ${module.mks.cluster_id} --external"
}

output "lockbox_secret_id" {
  value = module.lockbox.secret_id
}
```

- [ ] **Step 5: Manual one-time bootstrap (state bucket)**

Documented in README; only run once per environment:

```bash
yc storage bucket create --name sp-dev-tfstate --acl=private
```

- [ ] **Step 6: Apply (manual, requires real Yandex creds)**

```bash
cd deployments/terraform/environments/dev
cp terraform.tfvars.example terraform.tfvars
# fill in terraform.tfvars

terraform init
terraform plan -out=tfplan
terraform apply tfplan
```

Expected: ~30 minutes, creates ~30 resources. Last output is the `kubeconfig_command`.

(In CI we'll add `terraform plan` validation in Plan 02.)

- [ ] **Step 7: Get kubeconfig**

```bash
yc managed-kubernetes cluster get-credentials --id <cluster_id> --external
kubectl get nodes
```

Expected: 3 ready nodes.

- [ ] **Step 8: Commit (config files only, NOT tfvars)**

```bash
git add deployments/terraform/environments/dev/{backend.tf,main.tf,variables.tf,terraform.tfvars.example}
git commit -m "infra(terraform): wire dev environment"
```

---

## Task 10: Bootstrap k8s baseline — cert-manager, ingress-nginx, ESO

Done via shell scripts (one-time installs); ArgoCD takes over after.

**Files:** `deployments/k8s-bootstrap/{README.md,01-cert-manager.sh,02-ingress-nginx.sh,03-external-secrets.sh,04-argocd.sh,05-bootstrap-app-of-apps.sh}`

- [ ] **Step 1: `README.md`**

```markdown
# k8s-bootstrap

One-time scripts to bring a freshly-provisioned MKS cluster to a state where
ArgoCD can take over. Run scripts in numeric order.

Prerequisites:
- `kubectl` configured (see Plan 01 Task 9 Step 7).
- `helm` 3.x installed.
- Cluster reachable: `kubectl get nodes` succeeds.

## Scripts

| Script | Purpose |
|---|---|
| `01-cert-manager.sh` | Installs cert-manager for TLS via Let's Encrypt |
| `02-ingress-nginx.sh` | Installs ingress-nginx as default ingress controller |
| `03-external-secrets.sh` | Installs External Secrets Operator + Yandex provider config |
| `04-argocd.sh` | Installs ArgoCD |
| `05-bootstrap-app-of-apps.sh` | Applies the root ArgoCD Application that pulls the rest |

After all five run successfully, ArgoCD reconciles everything else from this repo.
```

- [ ] **Step 2: `01-cert-manager.sh`**

```bash
#!/usr/bin/env bash
set -euo pipefail

VERSION="${CERT_MANAGER_VERSION:-v1.14.5}"

echo "Installing cert-manager $VERSION..."

helm repo add jetstack https://charts.jetstack.io
helm repo update

helm upgrade --install cert-manager jetstack/cert-manager \
  --namespace cert-manager \
  --create-namespace \
  --version "$VERSION" \
  --set installCRDs=true \
  --set replicaCount=2 \
  --wait \
  --timeout 5m

# ClusterIssuer for Let's Encrypt staging (use this first to verify setup)
cat <<EOF | kubectl apply -f -
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: letsencrypt-staging
spec:
  acme:
    server: https://acme-staging-v02.api.letsencrypt.org/directory
    email: ops@sociopulse.ru
    privateKeySecretRef:
      name: letsencrypt-staging
    solvers:
      - http01:
          ingress:
            class: nginx
EOF

cat <<EOF | kubectl apply -f -
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: letsencrypt-prod
spec:
  acme:
    server: https://acme-v02.api.letsencrypt.org/directory
    email: ops@sociopulse.ru
    privateKeySecretRef:
      name: letsencrypt-prod
    solvers:
      - http01:
          ingress:
            class: nginx
EOF

echo "✓ cert-manager installed"
```

- [ ] **Step 3: `02-ingress-nginx.sh`**

```bash
#!/usr/bin/env bash
set -euo pipefail

VERSION="${INGRESS_NGINX_VERSION:-4.10.0}"

echo "Installing ingress-nginx $VERSION..."

helm repo add ingress-nginx https://kubernetes.github.io/ingress-nginx
helm repo update

helm upgrade --install ingress-nginx ingress-nginx/ingress-nginx \
  --namespace ingress-nginx \
  --create-namespace \
  --version "$VERSION" \
  --set controller.replicaCount=2 \
  --set controller.service.type=LoadBalancer \
  --set controller.metrics.enabled=true \
  --set controller.metrics.serviceMonitor.enabled=false \
  --set controller.config.proxy-body-size=10m \
  --set controller.config.use-forwarded-headers=true \
  --wait --timeout 5m

echo "✓ ingress-nginx installed"
echo
echo "External IP (used for DNS A-record):"
kubectl get svc -n ingress-nginx ingress-nginx-controller -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
echo
```

- [ ] **Step 4: `03-external-secrets.sh`**

```bash
#!/usr/bin/env bash
set -euo pipefail

VERSION="${ESO_VERSION:-0.9.18}"
LOCKBOX_SA_KEY_FILE="${LOCKBOX_SA_KEY_FILE:?Set path to service-account JSON key from terraform output}"

echo "Installing External Secrets Operator $VERSION..."

helm repo add external-secrets https://charts.external-secrets.io
helm repo update

helm upgrade --install external-secrets external-secrets/external-secrets \
  --namespace external-secrets \
  --create-namespace \
  --version "$VERSION" \
  --wait --timeout 5m

# Wait for ESO CRDs to be ready
sleep 10

# Create k8s secret with the SA key
kubectl create namespace sociopulse --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret generic yc-lockbox-sa-key \
  --namespace sociopulse \
  --from-file=key.json="$LOCKBOX_SA_KEY_FILE" \
  --dry-run=client -o yaml | kubectl apply -f -

# ClusterSecretStore for Yandex Lockbox
cat <<EOF | kubectl apply -f -
apiVersion: external-secrets.io/v1beta1
kind: ClusterSecretStore
metadata:
  name: yc-lockbox
spec:
  provider:
    yandexlockbox:
      auth:
        authorizedKeySecretRef:
          name: yc-lockbox-sa-key
          key: key.json
          namespace: sociopulse
EOF

echo "✓ External Secrets Operator installed"
```

- [ ] **Step 5: `04-argocd.sh`**

```bash
#!/usr/bin/env bash
set -euo pipefail

VERSION="${ARGOCD_VERSION:-7.3.0}"
ARGOCD_HOSTNAME="${ARGOCD_HOSTNAME:-argocd.dev.sociopulse.ru}"

echo "Installing ArgoCD $VERSION..."

helm repo add argo https://argoproj.github.io/argo-helm
helm repo update

helm upgrade --install argocd argo/argo-cd \
  --namespace argocd \
  --create-namespace \
  --version "$VERSION" \
  --set server.ingress.enabled=true \
  --set server.ingress.ingressClassName=nginx \
  --set server.ingress.hosts[0]="$ARGOCD_HOSTNAME" \
  --set server.ingress.tls[0].secretName=argocd-tls \
  --set server.ingress.tls[0].hosts[0]="$ARGOCD_HOSTNAME" \
  --set "server.ingress.annotations.cert-manager\.io/cluster-issuer=letsencrypt-prod" \
  --set "server.ingress.annotations.nginx\.ingress\.kubernetes\.io/backend-protocol=GRPC" \
  --set configs.params."server\.insecure"=false \
  --wait --timeout 10m

echo
echo "✓ ArgoCD installed"
echo
echo "Initial admin password:"
kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d
echo
echo
echo "ArgoCD URL: https://$ARGOCD_HOSTNAME"
echo "Login as 'admin' with the password above. Change it via 'argocd account update-password' immediately."
```

- [ ] **Step 6: `05-bootstrap-app-of-apps.sh`**

```bash
#!/usr/bin/env bash
set -euo pipefail

REPO_URL="${REPO_URL:-https://github.com/sociopulse/platform.git}"
REPO_BRANCH="${REPO_BRANCH:-main}"

echo "Bootstrapping app-of-apps for dev..."

cat <<EOF | kubectl apply -f -
apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata:
  name: sociopulse
  namespace: argocd
spec:
  description: SocioPulse platform
  sourceRepos:
    - "$REPO_URL"
  destinations:
    - namespace: "*"
      server: https://kubernetes.default.svc
  clusterResourceWhitelist:
    - group: "*"
      kind: "*"
  namespaceResourceWhitelist:
    - group: "*"
      kind: "*"
EOF

cat <<EOF | kubectl apply -f -
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: app-of-apps-dev
  namespace: argocd
spec:
  project: sociopulse
  source:
    repoURL: "$REPO_URL"
    targetRevision: "$REPO_BRANCH"
    path: deployments/argocd/applications
    directory:
      include: "*-dev.yaml"
  destination:
    server: https://kubernetes.default.svc
    namespace: argocd
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
EOF

echo "✓ app-of-apps applied; ArgoCD will sync remaining components"
```

- [ ] **Step 7: Make all scripts executable + commit**

```bash
chmod +x deployments/k8s-bootstrap/*.sh
git add deployments/k8s-bootstrap/
git commit -m "infra(k8s): add bootstrap scripts for cert-manager, ingress-nginx, ESO, ArgoCD"
```

---

## Task 11: Helm chart for `cmd/api`

**Files:** `deployments/helm/api/{Chart.yaml,values.yaml,values-dev.yaml,templates/*}`

- [ ] **Step 1: `Chart.yaml`**

```yaml
apiVersion: v2
name: sociopulse-api
description: СоциоПульс monolith HTTP/WS API (cmd/api binary)
type: application
version: 0.1.0
appVersion: "0.0.1"
maintainers:
  - name: SocioPulse Core Team
    email: dev@sociopulse.ru
```

- [ ] **Step 2: `values.yaml`**

```yaml
image:
  repository: cr.yandex/<your-registry-id>/sociopulse-api
  tag: 0.0.1
  pullPolicy: IfNotPresent

replicaCount: 2

resources:
  requests:
    cpu: 200m
    memory: 256Mi
  limits:
    cpu: 1000m
    memory: 1Gi

env:
  - name: HTTP_ADDR
    value: ":8080"
  - name: SERVICE_ENV
    value: development
  - name: LOG_LEVEL
    value: info

service:
  type: ClusterIP
  port: 8080

ingress:
  enabled: true
  className: nginx
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"      # WS keepalive
    nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
  hosts:
    - host: app.dev.sociopulse.ru
      paths:
        - path: /
          pathType: Prefix
  tls:
    - secretName: api-tls
      hosts:
        - app.dev.sociopulse.ru

healthcheck:
  liveness:
    path: /healthz
    initialDelaySeconds: 5
    periodSeconds: 10
  readiness:
    path: /healthz
    initialDelaySeconds: 5
    periodSeconds: 5

externalSecrets:
  enabled: true
  storeName: yc-lockbox
  lockboxSecretId: <fill-in-from-terraform-output>
  remoteToLocal:
    pg_password:    PG_PASSWORD
    redis_password: REDIS_PASSWORD
    jwt_secret:     JWT_SECRET
    s3_access_key:  S3_ACCESS_KEY
    s3_secret_key:  S3_SECRET_KEY

podAnnotations: {}
podLabels: {}
nodeSelector: {}
tolerations: []
affinity: {}
```

- [ ] **Step 3: `values-dev.yaml`**

```yaml
image:
  tag: dev
replicaCount: 2
resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    cpu: 500m
    memory: 512Mi

env:
  - name: HTTP_ADDR
    value: ":8080"
  - name: SERVICE_ENV
    value: development
  - name: LOG_LEVEL
    value: debug

ingress:
  hosts:
    - host: app.dev.sociopulse.ru
      paths:
        - path: /
          pathType: Prefix
  tls:
    - secretName: api-tls
      hosts:
        - app.dev.sociopulse.ru
```

- [ ] **Step 4: `templates/_helpers.tpl`**

```yaml
{{/*
Common name helpers
*/}}
{{- define "sociopulse-api.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "sociopulse-api.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "sociopulse-api.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "sociopulse-api.labels" -}}
app.kubernetes.io/name: {{ include "sociopulse-api.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "sociopulse-api.selectorLabels" -}}
app.kubernetes.io/name: {{ include "sociopulse-api.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
```

- [ ] **Step 5: `templates/serviceaccount.yaml`**

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ include "sociopulse-api.fullname" . }}
  labels:
    {{- include "sociopulse-api.labels" . | nindent 4 }}
```

- [ ] **Step 6: `templates/deployment.yaml`**

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "sociopulse-api.fullname" . }}
  labels:
    {{- include "sociopulse-api.labels" . | nindent 4 }}
spec:
  replicas: {{ .Values.replicaCount }}
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
  selector:
    matchLabels:
      {{- include "sociopulse-api.selectorLabels" . | nindent 6 }}
  template:
    metadata:
      labels:
        {{- include "sociopulse-api.selectorLabels" . | nindent 8 }}
        {{- with .Values.podLabels }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      annotations:
        {{- with .Values.podAnnotations }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
    spec:
      serviceAccountName: {{ include "sociopulse-api.fullname" . }}
      terminationGracePeriodSeconds: 30
      containers:
        - name: api
          image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
          imagePullPolicy: {{ .Values.image.pullPolicy }}
          ports:
            - name: http
              containerPort: 8080
              protocol: TCP
          env:
            {{- toYaml .Values.env | nindent 12 }}
            {{- if .Values.externalSecrets.enabled }}
            {{- range $remote, $local := .Values.externalSecrets.remoteToLocal }}
            - name: {{ $local }}
              valueFrom:
                secretKeyRef:
                  name: {{ include "sociopulse-api.fullname" $ }}-secrets
                  key: {{ $remote }}
            {{- end }}
            {{- end }}
          livenessProbe:
            httpGet:
              path: {{ .Values.healthcheck.liveness.path }}
              port: http
            initialDelaySeconds: {{ .Values.healthcheck.liveness.initialDelaySeconds }}
            periodSeconds: {{ .Values.healthcheck.liveness.periodSeconds }}
          readinessProbe:
            httpGet:
              path: {{ .Values.healthcheck.readiness.path }}
              port: http
            initialDelaySeconds: {{ .Values.healthcheck.readiness.initialDelaySeconds }}
            periodSeconds: {{ .Values.healthcheck.readiness.periodSeconds }}
          resources:
            {{- toYaml .Values.resources | nindent 12 }}
      {{- with .Values.nodeSelector }}
      nodeSelector:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.tolerations }}
      tolerations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.affinity }}
      affinity:
        {{- toYaml . | nindent 8 }}
      {{- end }}
```

- [ ] **Step 7: `templates/service.yaml`**

```yaml
apiVersion: v1
kind: Service
metadata:
  name: {{ include "sociopulse-api.fullname" . }}
  labels:
    {{- include "sociopulse-api.labels" . | nindent 4 }}
spec:
  type: {{ .Values.service.type }}
  ports:
    - port: {{ .Values.service.port }}
      targetPort: http
      protocol: TCP
      name: http
  selector:
    {{- include "sociopulse-api.selectorLabels" . | nindent 4 }}
```

- [ ] **Step 8: `templates/ingress.yaml`**

```yaml
{{- if .Values.ingress.enabled -}}
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: {{ include "sociopulse-api.fullname" . }}
  labels:
    {{- include "sociopulse-api.labels" . | nindent 4 }}
  annotations:
    {{- with .Values.ingress.annotations }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
spec:
  ingressClassName: {{ .Values.ingress.className }}
  tls:
    {{- toYaml .Values.ingress.tls | nindent 4 }}
  rules:
    {{- range .Values.ingress.hosts }}
    - host: {{ .host | quote }}
      http:
        paths:
          {{- range .paths }}
          - path: {{ .path }}
            pathType: {{ .pathType }}
            backend:
              service:
                name: {{ include "sociopulse-api.fullname" $ }}
                port:
                  number: {{ $.Values.service.port }}
          {{- end }}
    {{- end }}
{{- end }}
```

- [ ] **Step 9: `templates/externalsecret.yaml`**

```yaml
{{- if .Values.externalSecrets.enabled }}
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: {{ include "sociopulse-api.fullname" . }}-secrets
  labels:
    {{- include "sociopulse-api.labels" . | nindent 4 }}
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: {{ .Values.externalSecrets.storeName }}
    kind: ClusterSecretStore
  target:
    name: {{ include "sociopulse-api.fullname" . }}-secrets
    creationPolicy: Owner
  data:
    {{- range $remote, $local := .Values.externalSecrets.remoteToLocal }}
    - secretKey: {{ $remote }}
      remoteRef:
        key: {{ $.Values.externalSecrets.lockboxSecretId }}
        property: {{ $remote }}
    {{- end }}
{{- end }}
```

- [ ] **Step 10: Helm lint and template**

```bash
helm lint deployments/helm/api
helm template deployments/helm/api -f deployments/helm/api/values-dev.yaml | head -80
```

Expected: lint passes; template output is valid YAML.

- [ ] **Step 11: Commit**

```bash
git add deployments/helm/api/ deployments/helm/README.md
git commit -m "infra(helm): add cmd/api Helm chart"
```

If `deployments/helm/README.md` doesn't exist yet, add a one-liner: "Helm charts for SocioPulse binaries. See `api/` for cmd/api chart. More charts added in subsequent plans."

---

## Task 12: ArgoCD Applications

**Files:** `deployments/argocd/{projects/sociopulse.yaml,applications/api-dev.yaml,applications/nats-dev.yaml,applications/monitoring-dev.yaml}`

- [ ] **Step 1: AppProject**

`deployments/argocd/projects/sociopulse.yaml`:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata:
  name: sociopulse
  namespace: argocd
spec:
  description: SocioPulse platform
  sourceRepos:
    - "https://github.com/sociopulse/platform.git"
    - "https://nats-io.github.io/k8s/helm/charts/"
    - "https://prometheus-community.github.io/helm-charts"
    - "https://grafana.github.io/helm-charts"
  destinations:
    - namespace: "*"
      server: https://kubernetes.default.svc
  clusterResourceWhitelist:
    - group: "*"
      kind: "*"
  namespaceResourceWhitelist:
    - group: "*"
      kind: "*"
```

- [ ] **Step 2: API application**

`deployments/argocd/applications/api-dev.yaml`:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: sociopulse-api-dev
  namespace: argocd
spec:
  project: sociopulse
  source:
    repoURL: "https://github.com/sociopulse/platform.git"
    targetRevision: main
    path: deployments/helm/api
    helm:
      releaseName: api
      valueFiles:
        - values.yaml
        - values-dev.yaml
  destination:
    server: https://kubernetes.default.svc
    namespace: sociopulse
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
      - ServerSideApply=true
```

- [ ] **Step 3: NATS application**

`deployments/argocd/applications/nats-dev.yaml`:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: nats-dev
  namespace: argocd
spec:
  project: sociopulse
  source:
    repoURL: "https://nats-io.github.io/k8s/helm/charts/"
    chart: nats
    targetRevision: 1.2.2
    helm:
      releaseName: nats
      values: |
        config:
          cluster:
            enabled: true
            replicas: 3
          jetstream:
            enabled: true
            fileStore:
              pvc:
                size: 10Gi
                storageClassName: yc-network-ssd
        natsBox:
          enabled: false
        promExporter:
          enabled: true
  destination:
    server: https://kubernetes.default.svc
    namespace: sociopulse
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
```

- [ ] **Step 4: Monitoring application (kube-prometheus-stack)**

`deployments/argocd/applications/monitoring-dev.yaml`:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: monitoring-dev
  namespace: argocd
spec:
  project: sociopulse
  source:
    repoURL: "https://prometheus-community.github.io/helm-charts"
    chart: kube-prometheus-stack
    targetRevision: 60.5.0
    helm:
      releaseName: kps
      values: |
        prometheus:
          prometheusSpec:
            retention: 15d
            storageSpec:
              volumeClaimTemplate:
                spec:
                  accessModes: ["ReadWriteOnce"]
                  resources:
                    requests:
                      storage: 30Gi
                  storageClassName: yc-network-ssd
            serviceMonitorSelectorNilUsesHelmValues: false
            podMonitorSelectorNilUsesHelmValues: false
        grafana:
          ingress:
            enabled: true
            ingressClassName: nginx
            hosts:
              - grafana.dev.sociopulse.ru
            tls:
              - secretName: grafana-tls
                hosts:
                  - grafana.dev.sociopulse.ru
            annotations:
              cert-manager.io/cluster-issuer: letsencrypt-prod
          adminPassword: REPLACE_VIA_LOCKBOX
        alertmanager:
          alertmanagerSpec:
            storage:
              volumeClaimTemplate:
                spec:
                  accessModes: ["ReadWriteOnce"]
                  resources:
                    requests:
                      storage: 10Gi
                  storageClassName: yc-network-ssd
  destination:
    server: https://kubernetes.default.svc
    namespace: monitoring
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
      - ServerSideApply=true
```

- [ ] **Step 5: Commit**

```bash
git add deployments/argocd/
git commit -m "infra(argocd): wire dev applications (api, nats, monitoring)"
```

---

## Task 13: Container registry and image push

**Files:** `Makefile` extension, `deployments/argocd/README.md`

- [ ] **Step 1: Add Make targets for image push**

Append to `Makefile`:

```makefile
# Container registry — set REGISTRY and TAG
REGISTRY ?= cr.yandex/<your-cr-id>
TAG      ?= 0.0.1

.PHONY: docker-tag
docker-tag: docker-build ## Tag built image
	docker tag sociopulse-api:dev $(REGISTRY)/sociopulse-api:$(TAG)
	docker tag sociopulse-api:dev $(REGISTRY)/sociopulse-api:dev

.PHONY: docker-push
docker-push: docker-tag ## Push image to registry (requires `yc` CLI auth)
	docker push $(REGISTRY)/sociopulse-api:$(TAG)
	docker push $(REGISTRY)/sociopulse-api:dev
```

- [ ] **Step 2: First image push (manual one-time)**

```bash
yc container registry configure-docker
make docker-push REGISTRY=cr.yandex/<your-cr-id> TAG=0.0.1
```

- [ ] **Step 3: Update Helm values to point to your registry**

Edit `deployments/helm/api/values.yaml`:
```yaml
image:
  repository: cr.yandex/<your-cr-id>/sociopulse-api
  tag: 0.0.1
```

- [ ] **Step 4: Commit**

```bash
git add Makefile deployments/helm/api/values.yaml
git commit -m "build: add docker-tag/docker-push targets for Yandex CR"
```

---

## Task 14: End-to-end verification

**Files:** none (verification only)

- [ ] **Step 1: Apply Terraform (if not yet)**

```bash
cd deployments/terraform/environments/dev
terraform apply
cd -
```

- [ ] **Step 2: Run k8s bootstrap scripts**

```bash
cd deployments/k8s-bootstrap

# Get the service-account JSON key from terraform output
terraform -chdir=../terraform/environments/dev output -raw lockbox_secret_id  # note this for Helm
# Get authorized key for ESO:
terraform -chdir=../terraform/environments/dev output -raw "module.lockbox.reader_authorized_key" > /tmp/yc-eso-sa.json

LOCKBOX_SA_KEY_FILE=/tmp/yc-eso-sa.json ./03-external-secrets.sh

./01-cert-manager.sh
./02-ingress-nginx.sh
./04-argocd.sh
REPO_URL="https://github.com/sociopulse/platform.git" ./05-bootstrap-app-of-apps.sh

rm /tmp/yc-eso-sa.json
```

- [ ] **Step 3: Configure DNS A-records**

After `02-ingress-nginx.sh` prints the LB external IP, create:
- `app.dev.sociopulse.ru → <LB_IP>`
- `argocd.dev.sociopulse.ru → <LB_IP>`
- `grafana.dev.sociopulse.ru → <LB_IP>`

In Yandex Cloud DNS or whichever provider you use.

- [ ] **Step 4: Wait for ArgoCD sync**

```bash
kubectl get applications -n argocd
# Wait until all show "Synced" and "Healthy"
```

Should take ~5 minutes for everything.

- [ ] **Step 5: Smoke-test the deployed `cmd/api`**

```bash
curl -s https://app.dev.sociopulse.ru/healthz
```

Expected: `ok`.

- [ ] **Step 6: Verify ArgoCD UI**

Open `https://argocd.dev.sociopulse.ru/`, login as admin (password from script output, change immediately).
Verify all 3 apps (`sociopulse-api-dev`, `nats-dev`, `monitoring-dev`) are green.

- [ ] **Step 7: Verify Grafana**

Open `https://grafana.dev.sociopulse.ru/`, login (admin password from kube-secret).
Verify default Prometheus dashboards appear.

- [ ] **Step 8: No commit. Verification only.**

---

## Task 15: Document the path forward

**Files:** `deployments/README.md`

- [ ] **Step 1: Replace placeholder with real overview**

```markdown
# Deployments

This directory holds infrastructure-as-code, Helm charts, and ArgoCD application
definitions for the СоциоПульс platform.

## Layout

- `terraform/` — IaC (Yandex Cloud resources)
- `helm/` — Helm charts per binary
- `argocd/` — ArgoCD Applications and AppProject (GitOps source of truth)
- `k8s-bootstrap/` — one-time scripts to install ArgoCD prerequisites

## Workflow

```
[git push to main] → [ArgoCD detects] → [helm template] → [k8s applies] → [pods rolled]
```

## Per-environment status

| Environment | Status | Apps deployed |
|---|---|---|
| dev | Provisioned by this plan | api (cmd/api), nats, monitoring |
| staging | Pending (later plan) | — |
| production | Pending | — |

## Adding a new Helm chart

1. Create `deployments/helm/<chart-name>/` (Chart.yaml + values.yaml + templates).
2. `helm lint deployments/helm/<chart-name>` — must pass.
3. Add `deployments/argocd/applications/<chart-name>-dev.yaml` referencing the chart path.
4. Commit and push; ArgoCD picks it up.

## Disaster recovery

See [docs/runbooks/](../docs/runbooks/) (populated as we deploy more components).

The full DR plan is in the system design spec §16.6.
```

- [ ] **Step 2: Commit**

```bash
git add deployments/README.md
git commit -m "docs(deployments): document IaC + GitOps layout"
```

---

## Self-review

**Spec coverage check (against §16.1, §16.2, §16.3, §16.6, §15):**

- §16.1 infra components → all of MKS, MPG, MCH, MRD, KMS, Lockbox, Object Storage, NATS in chart, monitoring stack, ALB(=ingress-nginx LB), TURN deferred to Plan 08 (FreeSWITCH cluster). ✅
- §16.2 network topology → `network` module + ingress-nginx + WebSocket-friendly proxy timeouts. WebRTC media path to FreeSWITCH not in this plan (FreeSWITCH is Plan 08, which adds its own VMs and DNS). ✅
- §16.3 IaC → Terraform + Helm + ArgoCD all wired. ✅
- §16.6 DR → backup buckets configured with versioning + KMS-SSE; pgbackrest config deferred to Plan 03 (DB plan owns Postgres backup setup). Acceptable scope split. ✅
- §15 observability → kube-prometheus-stack + Grafana installed via ArgoCD; Loki and Tempo deferred to Plan 02 (cmd/api skeleton — that's where OTel-collector and Loki shipper need to be wired with the application). ✅

**Placeholder scan:** `<your-cr-id>`, `<your-registry-id>`, `<fill-in-from-terraform-output>` are explicit user-substitutions, documented in surrounding text. No bare TODOs.

**Type/name consistency:** secret keys flow `pg_password / redis_password / jwt_secret / s3_access_key / s3_secret_key` from Terraform → Lockbox → ExternalSecret → env-var. Naming is consistent throughout.

**Out of scope (correctly deferred):**
- Postgres schema + migrations: Plan 03.
- App-level config + observability wiring: Plan 02.
- FreeSWITCH cluster + TURN: Plan 08.
- Loki + Tempo collectors: Plan 02 (where the app-level OTel exporter is added).
- Production environment: future plan (after dev proves out).

Plan 01 verified.

---

**Plan complete and saved to `docs/superpowers/plans/2026-05-06-01-infrastructure.md`. Together with Plan 00, this delivers a fully-working empty SaaS infra: signed cert, ArgoCD, monitoring, and a "hello world" API reachable at `https://app.dev.sociopulse.ru/healthz`. The next plan (02) makes `cmd/api` real: config-loader, observability stack wiring, gateway middleware, structured logging.**
