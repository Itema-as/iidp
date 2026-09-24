#!/usr/bin/env bash
# shellcheck shell=bash
#
# Loads the state backend credentials of infra/platform into the current
# shell. Source it, don't run it:
#
#   cd infra/platform
#   source ../tofu-env.sh
#   tofu output -raw node_public_ipv4
#
# The S3 backend in platform/backend.tf reads AWS_ACCESS_KEY_ID and
# AWS_SECRET_ACCESS_KEY from the environment (a backend block cannot read
# OpenTofu variables). The keys are the Object Storage keys the bootstrap
# wizard, or the admin by hand, already wrote into
# state-bucket/terraform.tfvars as
#
#   object_storage_access_key = "..."
#   object_storage_secret_key = "..."
#
# so this reads them from there instead of having them typed or pasted
# into shell history. It prints no secret. IIDP_STATE_TFVARS overrides
# the file to read.
#
# Works when sourced from bash (3.2 or newer) or zsh. Everything it defines
# besides the two exports is removed again before it returns.

# Run instead of sourced: the exports would die with this process, so say
# so rather than appear to succeed. Only bash can get here (the shebang).
if [ -n "${BASH_VERSION:-}" ] && [ "${BASH_SOURCE[0]}" = "$0" ]; then
  echo "tofu-env.sh: source this file instead of running it, so the exports reach your shell:" >&2
  echo "  source ${0}" >&2
  exit 2
fi

# Where this file lives: BASH_SOURCE in bash, $0 in zsh (which sets it to
# the sourced file's path). Read at the top level, because inside a zsh
# function $0 is the function's name.
_iidp_tofu_env_self="${BASH_SOURCE[0]:-$0}"

_iidp_tofu_env_value() { # _iidp_tofu_env_value FILE KEY: the last KEY = "value" line's value
  sed -n -E "s/^[[:space:]]*$2[[:space:]]*=[[:space:]]*\"?([^\"]*)\"?[[:space:]]*\$/\\1/p" "$1" | tail -n 1
}

_iidp_tofu_env_load() {
  local file access secret
  if [ -n "${IIDP_STATE_TFVARS:-}" ]; then
    file="$IIDP_STATE_TFVARS"
  else
    file="$(cd "$(dirname "$_iidp_tofu_env_self")" 2>/dev/null && pwd)/state-bucket/terraform.tfvars"
  fi

  if [ ! -f "$file" ]; then
    echo "tofu-env.sh: no such file: $file" >&2
    echo "  It holds the Object Storage keys the state backend needs. Run the bootstrap wizard," >&2
    echo "  or copy infra/state-bucket/terraform.tfvars.example to it and fill in the two keys," >&2
    echo "  or point IIDP_STATE_TFVARS at the file that has them." >&2
    return 1
  fi

  access="$(_iidp_tofu_env_value "$file" object_storage_access_key)"
  secret="$(_iidp_tofu_env_value "$file" object_storage_secret_key)"

  local missing=""
  if [ -z "$access" ] || [ "$access" = "..." ]; then
    missing="object_storage_access_key"
  fi
  if [ -z "$secret" ] || [ "$secret" = "..." ]; then
    missing="${missing:+$missing and }object_storage_secret_key"
  fi
  if [ -n "$missing" ]; then
    echo "tofu-env.sh: $file has no value for $missing" >&2
    echo "  (expected a line like: object_storage_access_key = \"<key>\"; \"...\" is the example's placeholder)" >&2
    return 1
  fi

  export AWS_ACCESS_KEY_ID="$access"
  export AWS_SECRET_ACCESS_KEY="$secret"
  echo "tofu-env.sh: exported AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY from $file" >&2
}

# unset -f/unset leave nothing behind but the two exports; the status of
# the load is what the caller's `source` returns.
if _iidp_tofu_env_load; then
  unset -f _iidp_tofu_env_value _iidp_tofu_env_load
  unset _iidp_tofu_env_self
  return 0
fi
unset -f _iidp_tofu_env_value _iidp_tofu_env_load
unset _iidp_tofu_env_self
return 1
