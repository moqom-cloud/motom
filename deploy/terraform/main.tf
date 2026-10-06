# Copyright 2026 Elliott Wave LLC. All rights reserved.
#
# DRAFT. Not applied. MOTOM on Cloud Run with its own Cloud SQL Postgres:
#
#   motom-api        Cloud Run service: the API and the Stripe webhook endpoint
#   motom-migrate    Cloud Run job: `motom migrate`, run before each release
#   motom-payouts    Cloud Run job: `motom payouts run`, fired by Cloud Scheduler
#
# Secrets are Secret Manager containers only. Terraform never sees a value:
# an operator adds versions out of band (gcloud secrets versions add ... with
# the value on stdin), so no Stripe key or database password lands in state.

locals {
  name = "motom-${var.environment}"
  prod = var.environment == "prod"

  # Environment variable -> Secret Manager secret.
  secrets = {
    MOTOM_DATABASE_URL            = "${local.name}-database-url"
    STRIPE_SECRET_KEY             = "${local.name}-stripe-secret-key"
    STRIPE_WEBHOOK_SECRET         = "${local.name}-stripe-webhook-secret"
    STRIPE_CONNECT_WEBHOOK_SECRET = "${local.name}-stripe-connect-webhook-secret"
  }

  env = {
    MOTOM_MIN_HOLD_DAYS = tostring(var.min_hold_days)
  }
}

resource "google_service_account" "motom" {
  account_id   = local.name
  project      = var.project_id
  display_name = "MOTOM ${var.environment}"
  description  = "Runtime identity for the MOTOM API and its jobs."
}

resource "google_project_iam_member" "runtime" {
  for_each = toset([
    "roles/logging.logWriter",
    "roles/monitoring.metricWriter",
    "roles/cloudsql.client",
  ])

  project = var.project_id
  role    = each.value
  member  = google_service_account.motom.member
}

resource "google_secret_manager_secret" "motom" {
  for_each = local.secrets

  project   = var.project_id
  secret_id = each.value
  labels    = var.labels

  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_iam_member" "motom" {
  for_each = local.secrets

  project   = var.project_id
  secret_id = google_secret_manager_secret.motom[each.key].secret_id
  role      = "roles/secretmanager.secretAccessor"
  member    = google_service_account.motom.member
}

# A dedicated instance: the ledger is a financial record kept for seven
# years, so it gets point-in-time recovery and deletion protection and does
# not share a blast radius with the control plane's database.
resource "google_sql_database_instance" "motom" {
  name                = local.name
  project             = var.project_id
  region              = var.region
  database_version    = "POSTGRES_16"
  deletion_protection = true

  settings {
    tier              = var.sql_tier
    availability_type = local.prod ? "REGIONAL" : "ZONAL"
    edition           = "ENTERPRISE"
    user_labels       = var.labels

    backup_configuration {
      enabled                        = true
      point_in_time_recovery_enabled = true
      start_time                     = "07:00"
      transaction_log_retention_days = 7

      backup_retention_settings {
        retained_backups = local.prod ? 30 : 7
      }
    }

    # Reached only through the Cloud SQL connector Cloud Run mounts; no
    # authorised networks, TLS required.
    ip_configuration {
      ipv4_enabled = true
      ssl_mode     = "ENCRYPTED_ONLY"
    }

    database_flags {
      name  = "cloudsql.enable_pgaudit"
      value = "on"
    }

    insights_config {
      query_insights_enabled = true
    }
  }
}

resource "google_sql_database" "motom" {
  name     = "motom"
  project  = var.project_id
  instance = google_sql_database_instance.motom.name

  deletion_policy = "ABANDON"
}

# The database user and its password are created out of band and the DSN
# (postgres://motom:...@/motom?host=/cloudsql/<connection name>) stored as
# the database-url secret, so the password never enters Terraform state.

locals {
  container_env = [for k, v in local.env : { name = k, value = v }]
}

resource "google_cloud_run_v2_service" "api" {
  name                = "${local.name}-api"
  project             = var.project_id
  location            = var.region
  ingress             = var.public_ingress ? "INGRESS_TRAFFIC_ALL" : "INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER"
  deletion_protection = local.prod
  labels              = var.labels

  template {
    service_account                  = google_service_account.motom.email
    max_instance_request_concurrency = 80
    timeout                          = "60s"

    scaling {
      # Always one warm instance: Stripe retries webhooks, but a cold start on
      # every delivery is avoidable latency on money paths.
      min_instance_count = 1
      max_instance_count = local.prod ? 10 : 2
    }

    volumes {
      name = "cloudsql"
      cloud_sql_instance {
        instances = [google_sql_database_instance.motom.connection_name]
      }
    }

    containers {
      image = var.image
      args  = ["serve"]

      ports {
        container_port = 8080
      }

      resources {
        limits = {
          cpu    = "1"
          memory = "512Mi"
        }
        cpu_idle = true
      }

      dynamic "env" {
        for_each = local.container_env
        content {
          name  = env.value.name
          value = env.value.value
        }
      }

      dynamic "env" {
        for_each = local.secrets
        content {
          name = env.key
          value_source {
            secret_key_ref {
              secret  = google_secret_manager_secret.motom[env.key].secret_id
              version = "latest"
            }
          }
        }
      }

      volume_mounts {
        name       = "cloudsql"
        mount_path = "/cloudsql"
      }

      startup_probe {
        period_seconds    = 3
        failure_threshold = 10
        http_get {
          path = "/healthz"
        }
      }
    }
  }

  depends_on = [google_secret_manager_secret_iam_member.motom]
}

# The API authenticates every caller itself (tenant API keys, Stripe
# signatures), so Cloud Run lets unauthenticated requests reach it.
resource "google_cloud_run_v2_service_iam_member" "public" {
  count = var.public_ingress ? 1 : 0

  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.api.name
  role     = "roles/run.invoker"
  member   = "allUsers"
}

resource "google_cloud_run_v2_job" "task" {
  for_each = {
    migrate = ["migrate"]
    payouts = ["payouts", "run"]
  }

  name                = "${local.name}-${each.key}"
  project             = var.project_id
  location            = var.region
  deletion_protection = local.prod
  labels              = var.labels

  template {
    task_count = 1

    template {
      service_account = google_service_account.motom.email
      timeout         = "1800s"
      # A failed payout run is retried by the next schedule, not in a loop:
      # every payout is idempotent, but a person should see the failure.
      max_retries = 0

      volumes {
        name = "cloudsql"
        cloud_sql_instance {
          instances = [google_sql_database_instance.motom.connection_name]
        }
      }

      containers {
        image = var.image
        args  = each.value

        dynamic "env" {
          for_each = local.container_env
          content {
            name  = env.value.name
            value = env.value.value
          }
        }

        dynamic "env" {
          for_each = local.secrets
          content {
            name = env.key
            value_source {
              secret_key_ref {
                secret  = google_secret_manager_secret.motom[env.key].secret_id
                version = "latest"
              }
            }
          }
        }

        volume_mounts {
          name       = "cloudsql"
          mount_path = "/cloudsql"
        }
      }
    }
  }

  depends_on = [google_secret_manager_secret_iam_member.motom]
}

resource "google_service_account" "scheduler" {
  account_id   = "${local.name}-scheduler"
  project      = var.project_id
  display_name = "MOTOM ${var.environment} payout scheduler"
}

resource "google_cloud_run_v2_job_iam_member" "scheduler" {
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_job.task["payouts"].name
  role     = "roles/run.invoker"
  member   = google_service_account.scheduler.member
}

resource "google_cloud_scheduler_job" "payouts" {
  name      = "${local.name}-payouts"
  project   = var.project_id
  region    = var.region
  schedule  = var.payout_schedule
  time_zone = "Etc/UTC"

  http_target {
    http_method = "POST"
    uri         = "https://run.googleapis.com/v2/projects/${var.project_id}/locations/${var.region}/jobs/${google_cloud_run_v2_job.task["payouts"].name}:run"

    oauth_token {
      service_account_email = google_service_account.scheduler.email
    }
  }
}

# Alert when the payout job fails; a person decides whether to rerun it.
resource "google_logging_metric" "payout_failures" {
  name    = "${local.name}-payout-run-failed"
  project = var.project_id
  filter  = "resource.type=\"cloud_run_job\" AND resource.labels.job_name=\"${google_cloud_run_v2_job.task["payouts"].name}\" AND severity>=ERROR"
}
