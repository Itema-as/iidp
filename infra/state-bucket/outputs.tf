output "object_storage_endpoint" {
  description = "S3 endpoint of the location the buckets live in."
  value       = "https://${local.object_storage_host}"
}

output "state_bucket_name" {
  description = "Bucket holding the OpenTofu state of infra/platform."
  value       = minio_s3_bucket.state.bucket
}

output "backup_bucket_name" {
  description = "Bucket receiving the database backups. Referenced from platform.yaml in the Platform repository."
  value       = minio_s3_bucket.backups.bucket
}
