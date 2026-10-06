# Copyright 2026 Elliott Wave LLC. All rights reserved.

variable "project_id" {
  description = "GCP project MOTOM runs in."
  type        = string
  default     = "moqom-503700"
}

variable "region" {
  description = "Region for Cloud Run, Cloud SQL and Cloud Scheduler."
  type        = string
  default     = "us-central1"
}

variable "image" {
  description = "Container image for cmd/motom, by digest."
  type        = string
}

variable "environment" {
  description = "dev or prod; names resources and sets protections."
  type        = string
  default     = "dev"

  validation {
    condition     = contains(["dev", "prod"], var.environment)
    error_message = "environment is dev or prod."
  }
}

variable "sql_tier" {
  description = "Cloud SQL machine tier."
  type        = string
  default     = "db-custom-1-3840"
}

variable "min_hold_days" {
  description = "Shortest payout hold a tenant may configure."
  type        = number
  default     = 2
}

variable "payout_schedule" {
  description = "Cron (UTC) for the payout run. A run named for the UTC date pays each creator at most once that day."
  type        = string
  default     = "0 14 * * *"
}

variable "public_ingress" {
  description = "Serve the API from the Cloud Run URL. False puts it behind an internal load balancer."
  type        = bool
  default     = true
}

variable "labels" {
  type    = map(string)
  default = { service = "motom", owner = "elliott-wave-llc" }
}
