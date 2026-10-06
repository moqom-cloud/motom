# Copyright 2026 Elliott Wave LLC. All rights reserved.
#
# DRAFT. Not applied. Review with the MOQOM deploy envs before wiring it in.

terraform {
  required_version = ">= 1.9"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 7.41"
    }
  }
}
