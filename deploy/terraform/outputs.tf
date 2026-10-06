# Copyright 2026 Elliott Wave LLC. All rights reserved.

output "api_url" {
  description = "Base URL of the API. Stripe's webhook endpoints are this plus /v1/motom/webhooks/stripe."
  value       = google_cloud_run_v2_service.api.uri
}

output "sql_connection_name" {
  description = "Cloud SQL connection name for the database-url secret's host=/cloudsql/<name>."
  value       = google_sql_database_instance.motom.connection_name
}

output "secret_ids" {
  description = "Secret Manager secrets an operator must add a version to before first deploy."
  value       = { for k, s in google_secret_manager_secret.motom : k => s.secret_id }
}

output "service_account" {
  value = google_service_account.motom.email
}
