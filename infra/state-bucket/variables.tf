variable "location" {
  description = "Hetzner location that hosts the buckets. Object Storage exists in fsn1, nbg1 and hel1; keep it in the same location as the node."
  type        = string
  default     = "hel1"

  validation {
    condition     = contains(["fsn1", "nbg1", "hel1"], var.location)
    error_message = "Hetzner Object Storage is available in fsn1, nbg1 and hel1 only."
  }
}

variable "object_storage_access_key" {
  description = "Access key of the Hetzner Object Storage credentials (Cloud Console: Security > S3 credentials)."
  type        = string
  sensitive   = true
}

variable "object_storage_secret_key" {
  description = "Secret key belonging to object_storage_access_key."
  type        = string
  sensitive   = true
}

variable "state_bucket_name" {
  description = "Bucket that holds the OpenTofu state of infra/platform. Must match the bucket in infra/platform/backend.tf."
  type        = string
  default     = "itema-iidp-tofu-state"
}

variable "backup_bucket_name" {
  description = "Bucket that receives the continuous CloudNativePG backups of every Application database."
  type        = string
  default     = "itema-iidp-db-backups"
}
