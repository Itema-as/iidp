terraform {
  required_version = ">= 1.10"

  required_providers {
    minio = {
      source  = "aminueza/minio"
      version = "~> 3.43"
    }
  }
}
