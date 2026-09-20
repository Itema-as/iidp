# The two buckets the Platform needs. This root keeps its own state locally:
# the state bucket cannot hold the record of its own creation.

resource "minio_s3_bucket" "state" {
  bucket         = var.state_bucket_name
  acl            = "private"
  object_locking = false
}

# Versioning keeps every previous state file, which is the only undo button
# OpenTofu state has.
resource "minio_s3_bucket_versioning" "state" {
  bucket = minio_s3_bucket.state.bucket

  versioning_configuration {
    status = "Enabled"
  }
}

resource "minio_s3_bucket" "backups" {
  bucket         = var.backup_bucket_name
  acl            = "private"
  object_locking = false
}
