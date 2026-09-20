# State lives in the bucket created by infra/state-bucket. Backend blocks
# cannot read variables, so the bucket and endpoint are spelled out here; if
# you change state_bucket_name or location there, change them here too, or
# override at init time with -backend-config="bucket=..." and
# -backend-config="endpoints={s3=\"https://<location>.your-objectstorage.com\"}".
#
# Credentials are read from AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY.
# The skip_* flags turn off checks that only make sense against AWS itself;
# Hetzner Object Storage speaks S3 but has no STS, IAM or metadata service.
terraform {
  backend "s3" {
    bucket = "itema-iidp-tofu-state"
    key    = "platform/terraform.tfstate"
    region = "hel1"

    endpoints = {
      s3 = "https://hel1.your-objectstorage.com"
    }

    use_path_style              = true
    skip_credentials_validation = true
    skip_region_validation      = true
    skip_requesting_account_id  = true
    skip_metadata_api_check     = true
    skip_s3_checksum            = true
  }
}
