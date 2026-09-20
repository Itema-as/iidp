# Hetzner Object Storage has no Hetzner Cloud API and is not covered by the
# hcloud provider. Hetzner's own documentation manages buckets through the
# S3 API with the MinIO provider, so that is what this root uses.
provider "minio" {
  minio_server   = local.object_storage_host
  minio_region   = var.location
  minio_user     = var.object_storage_access_key
  minio_password = var.object_storage_secret_key
  minio_ssl      = true
}

locals {
  object_storage_host = "${var.location}.your-objectstorage.com"
}
