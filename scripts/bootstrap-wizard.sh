#!/usr/bin/env bash
#
# Bootstrap wizard for the Platform admin. Walks a human through the
# one-time human steps of setting up the iidp Platform (Hetzner, Cloudflare,
# Grafana Cloud, a GitHub App, two Entra app registrations), then runs
# OpenTofu and writes the Platform repository's bootstrap files.
#
# Run from a clone of this repository, with the Platform repository cloned
# somewhere else (default ../iidp-platform, override with --platform-repo).
# See infra/README.md ("Bootstrap wizard") for the human-facing walkthrough
# and docs/implementation-notes/05-bootstrap-wizard.md for why this script
# is shaped the way it is.
#
# Every step is idempotent: it detects a result that already exists (a
# tfvars value, a platform.yaml field, an encrypted secret), shows it
# (masked when secret) and offers to keep it instead of asking again.
#
# --dry-run prints every step and every command this script would run,
# touching no network, no disk outside itself and no terminal device, and
# is this script's smoke test.
#
set -euo pipefail

# ---------------------------------------------------------------------------
# Bash version. macOS ships bash 3.2, which has neither namerefs (used by
# the Entra helper to hand three values back to its caller) nor reliable
# associative-array support. Rather than write the whole wizard against
# bash 3.2, this requires bash >= 4.3 and says so plainly: install one with
# `brew install bash` on macOS (it will not replace /bin/bash) and invoke
# this script with it explicitly if it is not first on PATH.
# ---------------------------------------------------------------------------
_bash_too_old=0
if [ -z "${BASH_VERSINFO:-}" ] || [ "${BASH_VERSINFO[0]}" -lt 4 ]; then
  _bash_too_old=1
elif [ "${BASH_VERSINFO[0]}" -eq 4 ] && [ "${BASH_VERSINFO[1]}" -lt 3 ]; then
  _bash_too_old=1
fi
if [ "$_bash_too_old" -eq 1 ]; then
  echo "bootstrap-wizard requires bash >= 4.3 (this shell reports ${BASH_VERSION:-an unknown version})." >&2
  echo "On macOS: brew install bash, then run: \$(brew --prefix)/bin/bash $0 \"\$@\"" >&2
  exit 1
fi
unset _bash_too_old

# ──────────────────────────────────────────────────────────────────────────
# Output helpers
# ──────────────────────────────────────────────────────────────────────────

if [[ -t 1 ]] && command -v tput >/dev/null 2>&1 && [[ "$(tput colors 2>/dev/null || echo 0)" -ge 8 ]]; then
  BOLD=$(tput bold); DIM=$(tput dim); RESET=$(tput sgr0)
  BLUE=$(tput setaf 4); GREEN=$(tput setaf 2); YELLOW=$(tput setaf 3); RED=$(tput setaf 1)
else
  BOLD=""; DIM=""; RESET=""; BLUE=""; GREEN=""; YELLOW=""; RED=""
fi

TOTAL_STAGES=9
_STAGE_INDEX=0

_clear() { [[ -t 1 ]] || return 0; command -v tput >/dev/null 2>&1 && tput clear || printf '\033[2J\033[3J\033[H'; }

banner() {
  _clear
  {
    printf '\n%s%s  %s%s\n' "$BOLD" "$BLUE" "iidp bootstrap wizard" "$RESET"
    printf '%s  %s stages · platform repository: %s%s\n\n' "$DIM" "$TOTAL_STAGES" "$PLATFORM_REPO" "$RESET"
    if [[ "$DRY_RUN" == "1" ]]; then
      printf '%s  --dry-run: nothing is called, written or pushed. Every command that would\n' "$YELLOW"
      printf '  run is printed instead.%s\n\n' "$RESET"
    fi
  } >&2
}

stage() {
  _clear
  _STAGE_INDEX=$((_STAGE_INDEX + 1))
  printf '\n%s%s▸ Stage %s/%s · %s%s\n' "$BOLD" "$BLUE" "$_STAGE_INDEX" "$TOTAL_STAGES" "$1" "$RESET" >&2
}

# All human-facing output below goes to stderr, on purpose: several of these
# functions (latest_release_tag, wait_for_age_key, tfvar_get, ...) are called
# inside command substitutions for their actual return value, and anything
# they print to stdout would otherwise be captured into that value along
# with it. Only genuine return values are ever written to stdout.
say()  { printf '  %s\n' "$1" >&2; }
step() { printf '  %s•%s %s\n' "$BLUE" "$RESET" "$1" >&2; }
note() { printf '  %s%s%s\n' "$DIM" "$1" "$RESET" >&2; }
warn() { printf '  %s⚠ %s%s\n' "$YELLOW" "$1" "$RESET" >&2; }
ok()   { printf '  %s✓%s %s\n' "$GREEN" "$RESET" "$1" >&2; }
die()  { printf '%s✗ %s%s\n' "$RED" "$1" "$RESET" >&2; exit 1; }
log_choice() { printf '  %s»%s %s\n' "$DIM" "$RESET" "$1" >&2; }

dry() { # dry "would-do description" -- printed only in --dry-run
  printf '  %s↷ dry-run%s %s\n' "$DIM" "$RESET" "$1" >&2
}

MANUAL_STEPS=()   # things left for the admin to finish by hand, printed at the end

# ──────────────────────────────────────────────────────────────────────────
# Args, flags and mode switches
# ──────────────────────────────────────────────────────────────────────────

PLATFORM_REPO="../iidp-platform"
DRY_RUN=0
NO_PUSH=0

usage() {
  cat <<'EOF'
Usage: scripts/bootstrap-wizard.sh [--platform-repo DIR] [--dry-run] [--no-push]

  --platform-repo DIR  Clone of the Platform repository (default: ../iidp-platform)
  --dry-run            Print every step and command without calling anything
  --no-push            Write and commit the Platform repository but do not push
  -h, --help           Show this help
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --platform-repo)
      [[ $# -ge 2 ]] || die "--platform-repo requires a directory (see --help)"
      PLATFORM_REPO="$2"; shift 2 ;;
    --platform-repo=*) PLATFORM_REPO="${1#*=}"; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --no-push) NO_PUSH=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $1 (see --help)" ;;
  esac
done

IIDP_WIZARD_FAKE="${IIDP_WIZARD_FAKE:-0}"

# Repository root of this iidp clone, from the script's own location, so it
# works regardless of the caller's working directory.
IIDP_REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "[iidp-wizard] running under bash ${BASH_VERSION}" >&2

# ──────────────────────────────────────────────────────────────────────────
# Prompts. Read from /dev/tty, never from stdin, so the script works when
# something is piped into it. --dry-run never touches the tty at all: it
# keeps any existing value (for tracing) or fills in a placeholder.
# ──────────────────────────────────────────────────────────────────────────

# ask VAR "prompt" ["default"] [--secret]
ask() {
  local __var="$1" __prompt="$2" __default="${3:-}" __secret=0
  [[ "${4:-}" == "--secret" ]] && __secret=1

  if [[ "$DRY_RUN" == "1" ]]; then
    dry "would ask: $__prompt"
    printf -v "$__var" '%s' "${__default:-<dry-run:$__var>}"
    return 0
  fi

  if [[ "${IIDP_WIZARD_FAKE:-0}" == "1" ]]; then
    # Test mode: an answer file (IIDP_WIZARD_ANSWERS) provides VAR=value
    # lines instead of a real terminal, so idempotency and wrapper-function
    # tests run with no tty at all.
    local __answer=""
    if [[ -n "${IIDP_WIZARD_ANSWERS:-}" && -f "${IIDP_WIZARD_ANSWERS}" ]]; then
      __answer=$(grep -E "^${__var}=" "${IIDP_WIZARD_ANSWERS}" | tail -n1 | cut -d= -f2- || true)
    fi
    [[ -z "$__answer" ]] && __answer="$__default"
    printf -v "$__var" '%s' "$__answer"
    return 0
  fi

  [[ -r /dev/tty ]] || die "no controlling terminal to prompt on: run this from an interactive shell (see --dry-run to trace without one)"

  local __input=""
  if [[ -n "$__default" ]]; then
    if [[ "$__secret" == "1" ]]; then
      printf '  %s%s%s %s[Enter keeps current: %s]%s ' "$BOLD" "$__prompt" "$RESET" "$DIM" "$(mask "$__default")" "$RESET" >/dev/tty
    else
      printf '  %s%s%s %s[Enter keeps: %s]%s ' "$BOLD" "$__prompt" "$RESET" "$DIM" "$__default" "$RESET" >/dev/tty
    fi
  else
    printf '  %s%s%s ' "$BOLD" "$__prompt" "$RESET" >/dev/tty
  fi

  if [[ "$__secret" == "1" ]]; then
    IFS= read -rs __input </dev/tty; printf '\n' >/dev/tty
  else
    IFS= read -r __input </dev/tty
  fi
  [[ -z "$__input" && -n "$__default" ]] && __input="$__default"
  printf -v "$__var" '%s' "$__input"
}

ask_secret() { ask "$1" "$2" "${3:-}" --secret; }

confirm() { # confirm "question" [default: N]
  local reply=""
  if [[ "$DRY_RUN" == "1" ]]; then
    dry "would confirm: $1"
    return 0
  fi
  if [[ "${IIDP_WIZARD_FAKE:-0}" == "1" ]]; then
    return 0
  fi
  [[ -r /dev/tty ]] || die "no controlling terminal to confirm on"
  printf '  %s? %s [y/N] ' "$YELLOW" "$1" >/dev/tty
  IFS= read -r reply </dev/tty || true
  [[ "$reply" =~ ^[Yy] ]]
}

pause() {
  [[ "$DRY_RUN" == "1" || "${IIDP_WIZARD_FAKE:-0}" == "1" ]] && return 0
  [[ -r /dev/tty ]] || return 0
  printf '  %s%s%s ' "$DIM" "${1:-Press Enter to continue}" "$RESET" >/dev/tty
  IFS= read -r _ </dev/tty || true
}

# mask VALUE -> first two and last two characters, the rest asterisked.
mask() {
  local v="$1" n
  n=${#v}
  if (( n <= 4 )); then
    printf '%s' "$(printf '%*s' "$n" '' | tr ' ' '*')"
  else
    printf '%s%s%s' "${v:0:2}" "$(printf '%*s' $((n-4)) '' | tr ' ' '*')" "${v: -2}"
  fi
}

open_url() {
  local url="$1"
  printf '  %s↗ opening%s %s\n' "$GREEN" "$RESET" "$url" >&2
  if [[ "$DRY_RUN" == "1" ]]; then
    dry "would open a browser at the URL above"
    return 0
  fi
  { if   command -v wslview     >/dev/null 2>&1; then wslview "$url"
    elif command -v explorer.exe >/dev/null 2>&1; then explorer.exe "$url"
    elif command -v xdg-open    >/dev/null 2>&1; then xdg-open "$url"
    elif command -v open        >/dev/null 2>&1; then open "$url"
    else warn "couldn't open a browser; visit it manually"; fi
  } >/dev/null 2>&1 || warn "couldn't open a browser; visit it manually: $url"
}

# ──────────────────────────────────────────────────────────────────────────
# tfvars files: git-ignored KEY = "value" files OpenTofu reads directly.
# ──────────────────────────────────────────────────────────────────────────

tfvar_get() { # tfvar_get FILE KEY
  local file="$1" key="$2"
  [[ -f "$file" ]] || return 1
  local line
  line=$(grep -E "^${key}[[:space:]]*=" "$file" | tail -n1) || return 1
  [[ -z "$line" ]] && return 1
  line="${line#*=}"
  line="$(echo "$line" | sed -E 's/^[[:space:]]*"?//; s/"?[[:space:]]*$//')"
  printf '%s' "$line"
}

tfvar_set() { # tfvar_set FILE KEY VALUE
  local file="$1" key="$2" value="$3" tmp
  if [[ "$DRY_RUN" == "1" ]]; then
    dry "would write $key to $file"
    return 0
  fi
  mkdir -p "$(dirname "$file")"
  touch "$file"
  tmp=$(mktemp "${TMPDIR:-/tmp}/iidp-wizard.XXXXXX")
  grep -vE "^${key}[[:space:]]*=" "$file" > "$tmp" || true
  printf '%s = "%s"\n' "$key" "$value" >> "$tmp"
  mv "$tmp" "$file"
  chmod 600 "$file"
  ok "wrote $key to $file"
}

# ──────────────────────────────────────────────────────────────────────────
# platform.yaml readers. This is not a general YAML parser: it understands
# exactly the shape this wizard itself writes (two-space indent, scalars,
# one level of nesting), which is enough since platform.yaml is always
# rewritten whole by write_platform_yaml, never patched in place.
# ──────────────────────────────────────────────────────────────────────────

platform_yaml_get() { # platform_yaml_get FILE PATH (e.g. baseDomain or githubApp.id)
  local file="$1" path="$2"
  [[ -f "$file" ]] || return 1
  if [[ "$path" == *.* ]]; then
    local parent="${path%%.*}" child="${path#*.}"
    awk -v parent="$parent" -v child="$child" '
      $0 ~ "^"parent":[[:space:]]*$" { infield=1; next }
      infield && $0 ~ "^[^[:space:]#]" { infield=0 }
      infield && $0 ~ "^[[:space:]]+"child":" {
        sub("^[[:space:]]+"child":[[:space:]]*", "");
        print;
        exit
      }
    ' "$file"
  else
    awk -v key="$path" '
      $0 ~ "^"key":[[:space:]]*" {
        sub("^"key":[[:space:]]*", "");
        print;
        exit
      }
    ' "$file"
  fi
}

# ──────────────────────────────────────────────────────────────────────────
# Network wrapper functions. --dry-run never calls out at all. IIDP_WIZARD_FAKE=1
# replaces the actual curl with a fixed response, so validation logic (and
# the tests in test/wizard/) run with no network.
# ──────────────────────────────────────────────────────────────────────────

HTTP_STATUS=""
HTTP_BODY=""

http_get() { _http GET "$1" "${2:-}"; }
http_post() { _http POST "$1" "${2:-}" "${3:-}"; }

_http() {
  local method="$1" url="$2" auth_header="${3:-}" body="${4:-}"
  if [[ "$DRY_RUN" == "1" ]]; then
    dry "would $method $url"
    HTTP_STATUS=200; HTTP_BODY="{}"
    return 0
  fi
  if [[ "${IIDP_WIZARD_FAKE:-0}" == "1" ]]; then
    _fake_http "$method" "$url"
    return 0
  fi
  local tmp status
  tmp=$(mktemp "${TMPDIR:-/tmp}/iidp-wizard.XXXXXX")
  local -a curl_args=(-sS -o "$tmp" -w '%{http_code}')
  [[ -n "$auth_header" ]] && curl_args+=(-H "$auth_header")
  if [[ "$method" == "POST" ]]; then
    curl_args+=(-X POST)
    [[ -n "$body" ]] && curl_args+=(-d "$body")
  fi
  curl_args+=("$url")
  status=$(curl "${curl_args[@]}") || { rm -f "$tmp"; die "network call failed: $method $url"; }
  HTTP_STATUS="$status"
  HTTP_BODY=$(cat "$tmp")
  rm -f "$tmp"
}

# Canned responses for the shell test suite. Only the shapes the validators
# below actually read are filled in.
_fake_http() {
  local method="$1" url="$2"
  case "$url" in
    *api.hetzner.cloud/v1/servers*)
      if [[ "${IIDP_WIZARD_FAKE_HETZNER_TOKEN:-fake-token}" == "${HETZNER_TOKEN:-}" ]]; then
        HTTP_STATUS=200; HTTP_BODY='{"servers":[]}'
      else
        HTTP_STATUS=401; HTTP_BODY='{"error":{"message":"unable to authenticate"}}'
      fi
      ;;
    *api.cloudflare.com/client/v4/user/tokens/verify*)
      if [[ "${IIDP_WIZARD_FAKE_CLOUDFLARE_TOKEN:-fake-token}" == "${CLOUDFLARE_TOKEN:-}" ]]; then
        HTTP_STATUS=200; HTTP_BODY='{"success":true,"result":{"status":"active"}}'
      else
        HTTP_STATUS=403; HTTP_BODY='{"success":false,"errors":[{"message":"Invalid API Token"}]}'
      fi
      ;;
    *api.cloudflare.com/client/v4/zones*)
      HTTP_STATUS=200; HTTP_BODY='{"success":true,"result":[{"id":"fakezoneid","name":"itma.no"}]}'
      ;;
    *)
      HTTP_STATUS=200; HTTP_BODY='{}'
      ;;
  esac
}

validate_hetzner_token() { # validate_hetzner_token TOKEN -> 0/1
  local token="$1"
  [[ "$DRY_RUN" == "1" ]] && { dry "would GET https://api.hetzner.cloud/v1/servers"; return 0; }
  HETZNER_TOKEN="$token"
  http_get "https://api.hetzner.cloud/v1/servers" "Authorization: Bearer $token"
  [[ "$HTTP_STATUS" == "200" ]]
}

validate_cloudflare_token() { # validate_cloudflare_token TOKEN -> 0/1
  local token="$1"
  [[ "$DRY_RUN" == "1" ]] && { dry "would GET https://api.cloudflare.com/client/v4/user/tokens/verify"; return 0; }
  CLOUDFLARE_TOKEN="$token"
  http_get "https://api.cloudflare.com/client/v4/user/tokens/verify" "Authorization: Bearer $token"
  [[ "$HTTP_STATUS" == "200" ]] && echo "$HTTP_BODY" | jq -e '.success == true' >/dev/null 2>&1
}

check_cloudflare_zone_readable() { # check_cloudflare_zone_readable TOKEN ZONE -> 0/1
  local token="$1" zone="$2"
  [[ "$DRY_RUN" == "1" ]] && { dry "would GET https://api.cloudflare.com/client/v4/zones?name=${zone}"; return 0; }
  http_get "https://api.cloudflare.com/client/v4/zones?name=${zone}" "Authorization: Bearer $token"
  [[ "$HTTP_STATUS" == "200" ]] && echo "$HTTP_BODY" | jq -e '.success == true and ((.result | length) > 0)' >/dev/null 2>&1
}

# ──────────────────────────────────────────────────────────────────────────
# git / gh helpers
# ──────────────────────────────────────────────────────────────────────────

strip_git_suffix() { local u="$1"; printf '%s' "${u%.git}"; }

# normalize_git_url URL -> the https://github.com/<owner>/<repo>.git form,
# whatever form (ssh, git@, https) the local remote is configured with.
normalize_git_url() {
  local u="$1"
  case "$u" in
    git@github.com:*) u="https://github.com/${u#git@github.com:}" ;;
    ssh://git@github.com/*) u="https://github.com/${u#ssh://git@github.com/}" ;;
  esac
  [[ "$u" == *.git ]] || u="${u}.git"
  printf '%s' "$u"
}

github_owner_of() { # github_owner_of URL -> "owner/repo" without scheme or .git
  local u
  u=$(strip_git_suffix "$1")
  u="${u#*github.com[:/]}"
  printf '%s' "$u"
}

gh_secret_set_org() { # gh_secret_set_org NAME ORG VALUE_OR_FILE [--from-file]
  local name="$1" org="$2" value="$3" from_file="${4:-}"
  if [[ "$DRY_RUN" == "1" ]]; then
    dry "would run: gh secret set $name --org $org --visibility all"
    return 0
  fi
  if [[ "${IIDP_WIZARD_FAKE:-0}" == "1" ]]; then
    ok "(fake) set org secret $name"
    return 0
  fi
  if [[ "$from_file" == "--from-file" ]]; then
    gh secret set "$name" --org "$org" --visibility all < "$value" \
      || die "gh secret set $name --org $org failed"
  else
    printf '%s' "$value" | gh secret set "$name" --org "$org" --visibility all \
      || die "gh secret set $name --org $org failed"
  fi
  ok "set org secret $name (visibility: all)"
}

gh_variable_set_org() { # gh_variable_set_org NAME ORG VALUE
  local name="$1" org="$2" value="$3"
  if [[ "$DRY_RUN" == "1" ]]; then
    dry "would run: gh variable set $name --org $org --visibility all --body $value"
    return 0
  fi
  if [[ "${IIDP_WIZARD_FAKE:-0}" == "1" ]]; then
    ok "(fake) set org variable $name=$value"
    return 0
  fi
  gh variable set "$name" --org "$org" --visibility all --body "$value" \
    || die "gh variable set $name --org $org failed"
  ok "set org variable $name (visibility: all)"
}

latest_release_tag() { # latest_release_tag OWNER/REPO -> prints tag, or nothing
  local repo="$1"
  if [[ "$DRY_RUN" == "1" ]]; then
    dry "would run: gh release list --repo $repo --limit 1"
    return 1
  fi
  if [[ "${IIDP_WIZARD_FAKE:-0}" == "1" ]]; then
    printf '%s' "${IIDP_WIZARD_FAKE_RELEASE_TAG:-}"
    [[ -n "${IIDP_WIZARD_FAKE_RELEASE_TAG:-}" ]]
    return $?
  fi
  gh release list --repo "$repo" --limit 1 --json tagName --jq '.[0].tagName // empty' 2>/dev/null
}

# ──────────────────────────────────────────────────────────────────────────
# OpenTofu / SSH wrapper functions
# ──────────────────────────────────────────────────────────────────────────

run_tofu() { # run_tofu DIR ARG...
  local dir="$1"; shift
  if [[ "$DRY_RUN" == "1" ]]; then
    dry "would run: (cd $dir && tofu $*)"
    return 0
  fi
  ( cd "$dir" && tofu "$@" ) || die "tofu $* failed in $dir"
}

# wait_for_age_key IP -> prints the age public key on stdout
wait_for_age_key() {
  local ip="$1" timeout="${2:-900}" interval=15
  if [[ "$DRY_RUN" == "1" ]]; then
    dry "would run: ssh root@$ip cat /root/age.pub (retrying up to ${timeout}s)"
    printf 'age1dryrunplaceholder0000000000000000000000000000000000000000000000\n'
    return 0
  fi
  if [[ "${IIDP_WIZARD_FAKE:-0}" == "1" ]]; then
    printf '%s\n' "${IIDP_WIZARD_FAKE_AGE_KEY:-age1faketestkeyplaceholder00000000000000000000000000000000000000000}"
    return 0
  fi
  local deadline=$(( $(date +%s) + timeout ))
  local key=""
  while true; do
    if key=$(ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=5 -o BatchMode=yes \
                  "root@${ip}" cat /root/age.pub 2>/dev/null); then
      [[ -n "$key" ]] && { printf '%s\n' "$key"; return 0; }
    fi
    if [[ "$(date +%s)" -ge "$deadline" ]]; then
      warn "timed out waiting for /root/age.pub on ${ip}"
      note "check the bootstrap log: ssh root@${ip} tail -f /var/log/iidp-bootstrap.log"
      return 1
    fi
    sleep "$interval"
  done
}

# ──────────────────────────────────────────────────────────────────────────
# Azure CLI (Entra) helpers. az is optional; every function checks it first.
# ──────────────────────────────────────────────────────────────────────────

az_available() {
  command -v az >/dev/null 2>&1 || return 1
  [[ "$DRY_RUN" == "1" ]] && return 0
  [[ "${IIDP_WIZARD_FAKE:-0}" == "1" ]] && return 1  # tests exercise the manual path
  az account show >/dev/null 2>&1
}

# az_create_entra_app NAME REDIRECT_URI OUT_TENANT OUT_CLIENT_ID OUT_CLIENT_SECRET
# Uses namerefs (bash >= 4.3) to hand three values back to the caller.
az_create_entra_app() {
  local name="$1" redirect_uri="$2"
  # shellcheck disable=SC2034  # namerefs: written here, read through the caller's own variable names
  local -n out_tenant="$3" out_client_id="$4" out_client_secret="$5"

  # Delegated Microsoft Graph permission ids (Microsoft Graph permissions
  # reference, checked 2026-09-21): User.Read and GroupMember.Read.All.
  local graph_api="00000003-0000-0000-c000-000000000000"
  local perm_user_read="e1fe6dd8-ba31-4d61-89e7-88639da4683d=Scope"
  local perm_group_member_read_all="bc024368-1153-4739-b217-4326f2e966d0=Scope"

  if [[ "$DRY_RUN" == "1" ]]; then
    dry "would run: az ad app create --display-name $name --sign-in-audience AzureADMyOrg --web-redirect-uris $redirect_uri"
    dry "would run: az ad app permission add --id <appId> --api $graph_api --api-permissions $perm_user_read $perm_group_member_read_all"
    dry "would run: az ad app permission admin-consent --id <appId>"
    dry "would run: az ad app credential reset --id <appId> --append"
    # Namerefs: each is read through the caller's own variable name.
    # shellcheck disable=SC2034
    out_tenant="<dry-run:tenant>"
    # shellcheck disable=SC2034
    out_client_id="<dry-run:client-id>"
    # shellcheck disable=SC2034
    out_client_secret="<dry-run:client-secret>"
    return 0
  fi

  local app_id
  app_id=$(az ad app create --display-name "$name" --sign-in-audience AzureADMyOrg \
             --web-redirect-uris "$redirect_uri" --query appId -o tsv) \
    || die "az ad app create failed for $name"

  if ! az ad app permission add --id "$app_id" --api "$graph_api" \
         --api-permissions "$perm_user_read" "$perm_group_member_read_all" >/dev/null 2>&1; then
    warn "az ad app permission add failed for $name; grant User.Read and GroupMember.Read.All by hand"
    MANUAL_STEPS+=("Grant $name (app id $app_id) the delegated Graph permissions User.Read and GroupMember.Read.All")
  elif ! az ad app permission admin-consent --id "$app_id" >/dev/null 2>&1; then
    warn "az ad app permission admin-consent failed for $name (needs Global/Privileged Role admin); grant admin consent by hand"
    MANUAL_STEPS+=("Grant admin consent for $name (app id $app_id) in Entra > App registrations > API permissions")
  fi

  local secret
  secret=$(az ad app credential reset --id "$app_id" --append --query password -o tsv) \
    || die "az ad app credential reset failed for $name"

  local tenant
  tenant=$(az account show --query tenantId -o tsv) || die "az account show failed"

  # Namerefs: each is read through the caller's own variable name.
  # shellcheck disable=SC2034
  out_tenant="$tenant"
  # shellcheck disable=SC2034
  out_client_id="$app_id"
  # shellcheck disable=SC2034
  out_client_secret="$secret"
}

# ──────────────────────────────────────────────────────────────────────────
# sops
# ──────────────────────────────────────────────────────────────────────────

sops_encrypt_in_place() { # sops_encrypt_in_place FILE (relative to the platform repo)
  local file="$1"
  if [[ "$DRY_RUN" == "1" ]]; then
    dry "would run: (cd $PLATFORM_REPO && sops --encrypt --in-place $file)"
    return 0
  fi
  ( cd "$PLATFORM_REPO" && sops --encrypt --in-place "$file" ) \
    || die "sops --encrypt --in-place $file failed"
}

# ──────────────────────────────────────────────────────────────────────────
# Preflight
# ──────────────────────────────────────────────────────────────────────────

preflight() {
  stage "Preflight"
  local required=(tofu gh sops ssh jq curl) missing=()
  for tool in "${required[@]}"; do
    if command -v "$tool" >/dev/null 2>&1; then
      ok "$tool found"
    else
      missing+=("$tool")
    fi
  done
  if command -v az >/dev/null 2>&1; then
    ok "az found (optional; used for automatic Entra app registration)"
  else
    note "az not found (optional): Entra app registrations will be manual"
  fi

  if (( ${#missing[@]} > 0 )); then
    if [[ "$DRY_RUN" == "1" ]]; then
      dry "would fail here: missing required tools: ${missing[*]}"
    else
      die "missing required tools: ${missing[*]}"
    fi
  fi

  if [[ "$DRY_RUN" == "1" ]]; then
    dry "would run: gh auth status"
  elif [[ "${IIDP_WIZARD_FAKE:-0}" == "1" ]]; then
    ok "(fake) gh auth status"
  else
    gh auth status >/dev/null 2>&1 || die "not logged in to GitHub: run 'gh auth login' first"
    ok "gh auth status"
  fi

  if [[ -d "$PLATFORM_REPO/.git" ]]; then
    local branch
    branch=$(git -C "$PLATFORM_REPO" symbolic-ref --short HEAD 2>/dev/null || echo "")
    if [[ "$branch" == "main" ]]; then
      ok "Platform repository clone found at $PLATFORM_REPO, on main"
    else
      if [[ "$DRY_RUN" == "1" ]]; then
        dry "would fail here: $PLATFORM_REPO is on branch '$branch', not main"
      else
        die "$PLATFORM_REPO is on branch '${branch:-detached HEAD}', not main; check it out first"
      fi
    fi
  else
    if [[ "$DRY_RUN" == "1" ]]; then
      dry "would fail here: no git clone found at $PLATFORM_REPO (pass --platform-repo)"
    else
      die "no git clone found at $PLATFORM_REPO; clone the Platform repository there first (see --platform-repo)"
    fi
  fi

  pause "Preflight looks good. Ready to start?"
}

# ──────────────────────────────────────────────────────────────────────────
# Stage 2: Hetzner
# ──────────────────────────────────────────────────────────────────────────

STATE_TFVARS="$IIDP_REPO_ROOT/infra/state-bucket/terraform.tfvars"
PLATFORM_TFVARS="$IIDP_REPO_ROOT/infra/platform/terraform.tfvars"
HCLOUD_TOKEN="" OBJECT_STORAGE_ACCESS_KEY="" OBJECT_STORAGE_SECRET_KEY="" SSH_PUBLIC_KEY_PATH=""
HETZNER_TOKEN=""

stage_hetzner() {
  stage "Hetzner"
  say "The Platform's node, firewall and SSH key are created by OpenTofu; it needs"
  say "a Hetzner Cloud API token and Object Storage credentials for the same project."

  local existing_token
  existing_token=$(tfvar_get "$PLATFORM_TFVARS" hcloud_token || true)
  if [[ -n "$existing_token" ]]; then
    note "existing Hetzner API token found in $PLATFORM_TFVARS: $(mask "$existing_token")"
  fi
  open_url "https://console.hetzner.cloud/"
  step "Open the Platform's project > Security > API tokens > Generate API token (read & write)."
  ask_secret HCLOUD_TOKEN "Paste the Hetzner API token:" "$existing_token"

  if [[ -z "$existing_token" || "$HCLOUD_TOKEN" != "$existing_token" ]]; then
    if validate_hetzner_token "$HCLOUD_TOKEN"; then
      ok "token validated against GET /v1/servers"
    else
      die "Hetzner token did not validate (GET /v1/servers returned $HTTP_STATUS); check it and re-run"
    fi
  else
    note "keeping existing token; not re-validating"
  fi
  tfvar_set "$PLATFORM_TFVARS" hcloud_token "$HCLOUD_TOKEN"

  local existing_access
  existing_access=$(tfvar_get "$STATE_TFVARS" object_storage_access_key || true)
  if [[ -n "$existing_access" ]]; then
    note "existing Object Storage access key found: $(mask "$existing_access")"
  fi
  step "Same project > Security > S3 credentials > Generate credentials."
  ask_secret OBJECT_STORAGE_ACCESS_KEY "Paste the Object Storage access key:" "$existing_access"
  ask_secret OBJECT_STORAGE_SECRET_KEY "Paste the Object Storage secret key:" "$(tfvar_get "$STATE_TFVARS" object_storage_secret_key || true)"
  tfvar_set "$STATE_TFVARS" object_storage_access_key "$OBJECT_STORAGE_ACCESS_KEY"
  tfvar_set "$STATE_TFVARS" object_storage_secret_key "$OBJECT_STORAGE_SECRET_KEY"

  local existing_ssh
  existing_ssh=$(tfvar_get "$PLATFORM_TFVARS" ssh_public_key || true)
  if [[ -n "$existing_ssh" ]]; then
    note "existing SSH public key found in $PLATFORM_TFVARS"
  fi
  ask SSH_PUBLIC_KEY_PATH "Path to your SSH public key (goes on the node):" "${SSH_PUBLIC_KEY_PATH:-$HOME/.ssh/id_ed25519.pub}"
  local ssh_pub_content="$existing_ssh"
  if [[ "$DRY_RUN" != "1" && "${IIDP_WIZARD_FAKE:-0}" != "1" ]]; then
    [[ -f "$SSH_PUBLIC_KEY_PATH" ]] || die "no such file: $SSH_PUBLIC_KEY_PATH"
    ssh_pub_content=$(cat "$SSH_PUBLIC_KEY_PATH")
  elif [[ -z "$ssh_pub_content" ]]; then
    ssh_pub_content="ssh-ed25519 AAAAfakeplaceholder wizard-test"
  fi
  tfvar_set "$PLATFORM_TFVARS" ssh_public_key "$ssh_pub_content"

  # AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY are what infra/platform's S3
  # backend reads (backend blocks cannot read OpenTofu variables).
  export AWS_ACCESS_KEY_ID="$OBJECT_STORAGE_ACCESS_KEY"
  export AWS_SECRET_ACCESS_KEY="$OBJECT_STORAGE_SECRET_KEY"
  note "exported AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY for the S3 backend"
}

# ──────────────────────────────────────────────────────────────────────────
# Stage 3: Cloudflare
# ──────────────────────────────────────────────────────────────────────────

CLOUDFLARE_TOKEN=""
BASE_DOMAIN=""
CLOUDFLARE_ZONE=""

stage_cloudflare() {
  stage "Cloudflare"
  local platform_yaml="$PLATFORM_REPO/platform.yaml"

  ask BASE_DOMAIN "Platform base domain:" "$(platform_yaml_get "$platform_yaml" baseDomain || echo app.itma.no)"
  ask CLOUDFLARE_ZONE "Cloudflare zone containing it:" "$(platform_yaml_get "$platform_yaml" cloudflareZone || echo itma.no)"

  local sops_cm="$PLATFORM_REPO/bootstrap/sops/cloudflare-api-token-cert-manager.enc.yaml"
  local sops_dns="$PLATFORM_REPO/bootstrap/sops/cloudflare-api-token-external-dns.enc.yaml"
  if [[ -f "$sops_cm" && -f "$sops_dns" ]] && confirm "Cloudflare secrets already encrypted in the Platform repository. Keep them and skip asking for a new token?"; then
    note "keeping existing encrypted Cloudflare secrets"
    CLOUDFLARE_TOKEN=""
    return 0
  fi

  say "external-dns and cert-manager both need a Cloudflare API token scoped to"
  say "the zone (Zone:DNS:Edit is enough; Zone:Zone:Read to list it)."
  open_url "https://dash.cloudflare.com/profile/api-tokens"
  step "Create Token > Edit zone DNS template, scoped to $CLOUDFLARE_ZONE."
  ask_secret CLOUDFLARE_TOKEN "Paste the Cloudflare API token:"

  if validate_cloudflare_token "$CLOUDFLARE_TOKEN"; then
    ok "token validated against /user/tokens/verify"
  else
    die "Cloudflare token did not validate; check it and re-run"
  fi
  if check_cloudflare_zone_readable "$CLOUDFLARE_TOKEN" "$CLOUDFLARE_ZONE"; then
    ok "zone $CLOUDFLARE_ZONE is readable with this token"
  else
    die "zone $CLOUDFLARE_ZONE is not readable with this token; check the token's scope"
  fi
}

# ──────────────────────────────────────────────────────────────────────────
# Stage 4: Grafana Cloud
# ──────────────────────────────────────────────────────────────────────────

GRAFANA_URL="" GRAFANA_PROM_URL="" GRAFANA_PROM_USER="" GRAFANA_LOKI_URL="" GRAFANA_LOKI_USER="" GRAFANA_ACCESS_TOKEN=""

stage_grafana() {
  stage "Grafana Cloud"
  local platform_yaml="$PLATFORM_REPO/platform.yaml"
  ask GRAFANA_URL "Grafana Cloud stack URL:" "$(platform_yaml_get "$platform_yaml" grafanaURL || echo "")"

  local secret_file="$PLATFORM_REPO/bootstrap/sops/grafana-cloud.enc.yaml"
  if [[ -f "$secret_file" ]] && confirm "Grafana Cloud credentials already encrypted. Keep them and skip asking again?"; then
    note "keeping existing encrypted Grafana Cloud secret"
    return 0
  fi

  say "Alloy on the node ships logs and metrics to Grafana Cloud's free tier."
  open_url "https://grafana.com/orgs"
  step "Open your stack > Details, and the connection instructions for Prometheus and Loki."
  ask GRAFANA_PROM_URL "Prometheus remote_write URL:"
  ask GRAFANA_PROM_USER "Prometheus username (instance id):"
  ask GRAFANA_LOKI_URL "Loki push URL:"
  ask GRAFANA_LOKI_USER "Loki username (instance id):"
  step "My Account > Access Policies > Create access policy with metrics:write and logs:write, then create a token for it."
  ask_secret GRAFANA_ACCESS_TOKEN "Paste the access token:"
}

# ──────────────────────────────────────────────────────────────────────────
# Stage 5: GitHub App for CI write-back
# ──────────────────────────────────────────────────────────────────────────

GITHUB_APP_ID="" GITHUB_APP_INSTALLATION_ID="" GITHUB_ORG="" PEM_PATH=""

html_escape() {
  local s="$1"
  s="${s//&/&amp;}"; s="${s//\'/\&#39;}"; s="${s//\"/\&quot;}"; s="${s//</\&lt;}"; s="${s//>/\&gt;}"
  printf '%s' "$s"
}

stage_github_app() {
  stage "GitHub App for CI write-back"
  local platform_yaml="$PLATFORM_REPO/platform.yaml"
  local existing_id
  existing_id=$(platform_yaml_get "$platform_yaml" githubApp.id || true)
  if [[ -n "$existing_id" ]] && confirm "platform.yaml already has githubApp.id=$existing_id. Keep it and skip creating a new App?"; then
    GITHUB_APP_ID="$existing_id"
    GITHUB_APP_INSTALLATION_ID=$(platform_yaml_get "$platform_yaml" githubApp.installationId || true)
    note "keeping existing GitHub App id $GITHUB_APP_ID / installation $GITHUB_APP_INSTALLATION_ID"
    return 0
  fi

  local origin
  origin=$(git -C "$PLATFORM_REPO" remote get-url origin 2>/dev/null || echo "https://github.com/itema-as/iidp-platform.git")
  GITHUB_ORG=$(github_owner_of "$origin"); GITHUB_ORG="${GITHUB_ORG%%/*}"
  ask GITHUB_ORG "GitHub org to create the App under:" "$GITHUB_ORG"

  say "The REST API cannot create a GitHub App directly; only the manifest flow"
  say "can. This opens a page that submits a prepared manifest to GitHub."
  local manifest state tmp_dir tmp_html
  state=$(head -c16 /dev/urandom | od -An -tx1 | tr -d ' \n')
  manifest=$(jq -nc --arg name "iidp-deploy" --arg url "$(strip_git_suffix "$(normalize_git_url "$origin")")" \
    '{name: $name, url: $url, public: false, default_events: [], default_permissions: {contents: "write"}, hook_attributes: {active: false}}')

  if [[ "$DRY_RUN" == "1" ]]; then
    dry "would open a self-submitting form posting this manifest to https://github.com/organizations/${GITHUB_ORG}/settings/apps/new?state=${state}:"
    dry "  $manifest"
  else
    tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/iidp-wizard-app.XXXXXX")
    tmp_html="$tmp_dir/github-app-manifest.html"
    cat > "$tmp_html" <<EOF
<!doctype html><html><body onload="document.forms[0].submit()">
<form action="https://github.com/organizations/${GITHUB_ORG}/settings/apps/new?state=${state}" method="post">
<input type="hidden" name="manifest" value='$(html_escape "$manifest")'>
<noscript><button type="submit">Create the iidp-deploy GitHub App</button></noscript>
</form>
</body></html>
EOF
    open_url "file://$tmp_html"
  fi

  step "Confirm creating the App. GitHub shows its settings page with the App ID."
  ask GITHUB_APP_ID "App id shown on the settings page:"
  step "Click 'Generate a private key' and save the downloaded .pem file somewhere safe."
  ask PEM_PATH "Path to the downloaded private key .pem file:"
  step "In the left sidebar, 'Install App', install it on ${GITHUB_ORG}. The resulting URL ends in /installations/<id>."
  ask GITHUB_APP_INSTALLATION_ID "Installation id from that URL:"

  if [[ "$DRY_RUN" != "1" && "${IIDP_WIZARD_FAKE:-0}" != "1" ]]; then
    [[ -f "$PEM_PATH" ]] || die "no such file: $PEM_PATH"
    grep -q "BEGIN.*PRIVATE KEY" "$PEM_PATH" || die "$PEM_PATH does not look like a PEM private key"
  else
    PEM_PATH="${PEM_PATH:-/dev/null}"
  fi

  gh_secret_set_org IIDP_DEPLOY_APP_PRIVATE_KEY "$GITHUB_ORG" "$PEM_PATH" --from-file
  gh_variable_set_org IIDP_DEPLOY_APP_ID "$GITHUB_ORG" "$GITHUB_APP_ID"
  log_choice "org secret IIDP_DEPLOY_APP_PRIVATE_KEY and org variable IIDP_DEPLOY_APP_ID are what ticket #12's deploy workflow reads"
}

# ──────────────────────────────────────────────────────────────────────────
# Stage 6: Entra
# ──────────────────────────────────────────────────────────────────────────

ENTRA_ARGOCD_TENANT="" ENTRA_ARGOCD_CLIENT_ID="" ENTRA_ARGOCD_CLIENT_SECRET=""
ENTRA_OAUTH2_PROXY_TENANT="" ENTRA_OAUTH2_PROXY_CLIENT_ID="" ENTRA_OAUTH2_PROXY_CLIENT_SECRET=""
ARGOCD_URL="" ARGOCD_ADMIN_GROUP=""
OAUTH2_PROXY_HOST=""

# entra_register_app NAME REDIRECT_URI DEFAULT_TENANT OUT_TENANT OUT_CLIENT_ID OUT_CLIENT_SECRET OPEN_URL
# Creates the app registration via az_create_entra_app when az is available
# and logged in, otherwise prints what to create by hand and asks for the
# three resulting values. OUT_* are namerefs (bash >= 4.3), same convention
# as az_create_entra_app itself, which this wraps.
entra_register_app() {
  local name="$1" redirect_uri="$2" default_tenant="$3"
  # shellcheck disable=SC2034  # namerefs: written here, read through the caller's own variable names
  local -n reg_tenant="$4" reg_client_id="$5" reg_client_secret="$6"
  local do_open_url="$7"

  if az_available; then
    say "az is logged in: creating the $name app registration automatically."
    az_create_entra_app "$name" "$redirect_uri" reg_tenant reg_client_id reg_client_secret
    ok "created Entra app '$name' (client id $reg_client_id)"
  else
    say "az is not available or not logged in with rights to register apps."
    say "In Entra ID > App registrations > New registration, create:"
    note "  name: $name"
    note "  redirect URI (Web): ${redirect_uri}"
    note "  API permissions (delegated, admin consent): User.Read, GroupMember.Read.All"
    note "  Certificates & secrets: new client secret"
    [[ "$do_open_url" == "1" ]] && open_url "https://portal.azure.com/#view/Microsoft_AAD_RegisteredApps/ApplicationsListBlade"
    ask reg_tenant "Tenant id:" "$default_tenant"
    ask reg_client_id "Client id:"
    ask_secret reg_client_secret "Client secret:"
  fi
}

stage_entra() {
  stage "Entra ID"
  local platform_yaml="$PLATFORM_REPO/platform.yaml"
  ask ARGOCD_URL "ArgoCD URL:" "$(platform_yaml_get "$platform_yaml" argocdURL || echo "https://argocd.${BASE_DOMAIN}")"
  ask ARGOCD_ADMIN_GROUP "Entra group object id whose members are ArgoCD admins (argocdAdminGroup):" "$(platform_yaml_get "$platform_yaml" argocdAdminGroup || true)"

  local argocd_host="${ARGOCD_URL#*://}"
  OAUTH2_PROXY_HOST="auth.${BASE_DOMAIN}"
  local argocd_redirect="https://${argocd_host}/api/dex/callback"
  local oauth2_proxy_redirect="https://${OAUTH2_PROXY_HOST}/oauth2/callback"
  log_choice "oauth2-proxy host: ${OAUTH2_PROXY_HOST} (ticket #18 reads this from docs/implementation-notes/05-bootstrap-wizard.md)"

  local sops_entra="$PLATFORM_REPO/bootstrap/sops/argocd-entra.enc.yaml"
  local skip_argocd_app=0
  if [[ -f "$sops_entra" ]] && confirm "ArgoCD's Entra secret is already encrypted. Keep it and skip re-creating that registration?"; then
    skip_argocd_app=1
  fi

  if [[ "$skip_argocd_app" != "1" ]]; then
    entra_register_app "iidp-argocd" "$argocd_redirect" "" \
      ENTRA_ARGOCD_TENANT ENTRA_ARGOCD_CLIENT_ID ENTRA_ARGOCD_CLIENT_SECRET 1
  fi

  say "oauth2-proxy (ticket #18, Itema login) needs its own registration now, so it"
  say "is ready when that ticket lands."
  entra_register_app "iidp-oauth2-proxy" "$oauth2_proxy_redirect" "$ENTRA_ARGOCD_TENANT" \
    ENTRA_OAUTH2_PROXY_TENANT ENTRA_OAUTH2_PROXY_CLIENT_ID ENTRA_OAUTH2_PROXY_CLIENT_SECRET 0

  # No Platform component consumes this yet (ticket #18 adds it); keep it out
  # of git, in a local file next to the other tfvars, until then.
  local oauth2_env="$IIDP_REPO_ROOT/infra/platform/oauth2-proxy-entra.env"
  if [[ "$DRY_RUN" == "1" ]]; then
    dry "would write $oauth2_env (tenant/client id/secret for ticket #18)"
  else
    umask 077
    cat > "$oauth2_env" <<EOF
# Entra app registration for oauth2-proxy (ticket #18, Itema login).
# Not read by anything yet; #18's component reads it, or re-runs this wizard.
# Redirect URI: ${oauth2_proxy_redirect}
TENANT_ID=${ENTRA_OAUTH2_PROXY_TENANT}
CLIENT_ID=${ENTRA_OAUTH2_PROXY_CLIENT_ID}
CLIENT_SECRET=${ENTRA_OAUTH2_PROXY_CLIENT_SECRET}
EOF
    chmod 600 "$oauth2_env"
    ok "wrote oauth2-proxy Entra credentials to $oauth2_env (git-ignored)"
  fi
  MANUAL_STEPS+=("oauth2-proxy Entra credentials are saved at infra/platform/oauth2-proxy-entra.env for ticket #18")
}

# ──────────────────────────────────────────────────────────────────────────
# Stage 7: OpenTofu
# ──────────────────────────────────────────────────────────────────────────

NODE_IP="" AGE_PUBLIC_KEY=""
ACME_EMAIL="" ACME_SERVER="" CLUSTER_NAME="" BACKUPS_BUCKET=""

stage_opentofu() {
  stage "OpenTofu"
  local state_dir="$IIDP_REPO_ROOT/infra/state-bucket"
  local platform_dir="$IIDP_REPO_ROOT/infra/platform"

  say "First the two Object Storage buckets, then the node."
  run_tofu "$state_dir" init -input=false
  run_tofu "$state_dir" plan -input=false -out=tfplan
  if confirm "Apply the state-bucket plan shown above?"; then
    run_tofu "$state_dir" apply -input=false tfplan
  else
    die "aborted: the state bucket must exist before infra/platform can use it as a backend"
  fi

  run_tofu "$platform_dir" init -input=false
  run_tofu "$platform_dir" plan -input=false -out=tfplan
  if confirm "Apply the platform plan shown above? This creates the Hetzner node."; then
    run_tofu "$platform_dir" apply -input=false tfplan
  else
    die "aborted before creating the node"
  fi

  if [[ "$DRY_RUN" == "1" ]]; then
    dry "would run: tofu output -raw node_public_ipv4"
    NODE_IP="203.0.113.10"
  elif [[ "${IIDP_WIZARD_FAKE:-0}" == "1" ]]; then
    NODE_IP="${IIDP_WIZARD_FAKE_NODE_IP:-203.0.113.10}"
  else
    NODE_IP=$(cd "$platform_dir" && tofu output -raw node_public_ipv4) || die "tofu output node_public_ipv4 failed"
  fi
  ok "node public IPv4: $NODE_IP"

  say "Waiting for cloud-init to finish and publish the age public key."
  say "This can take a few minutes (k3s and ArgoCD installing)."
  AGE_PUBLIC_KEY=$(wait_for_age_key "$NODE_IP" 900) || die "could not retrieve the age public key"
  ok "age public key: $AGE_PUBLIC_KEY"
}

# ──────────────────────────────────────────────────────────────────────────
# Stage 8: Platform repository
# ──────────────────────────────────────────────────────────────────────────

write_platform_yaml() {
  local file="$PLATFORM_REPO/platform.yaml"
  local chart_version="$1" acme_email="$2" acme_server="$3" cluster_name="$4" backups_bucket="$5"

  if [[ "$DRY_RUN" == "1" ]]; then
    dry "would write $file"
    return 0
  fi
  cat > "$file" <<EOF
# Platform-wide settings. The bootstrap reads this as Helm values (see
# bootstrap/README.md in the iidp repository for every field); the CLI
# reads the fields marked so. Written by scripts/bootstrap-wizard.sh.

# Applications are served under <application>.<baseDomain>; the wildcard
# certificate covers it. Read by the CLI.
baseDomain: ${BASE_DOMAIN}

# The Cloudflare zone containing baseDomain, the only zone external-dns
# manages and where the CLI can automate custom domains. Read by the CLI.
cloudflareZone: ${CLOUDFLARE_ZONE}

# Where ArgoCD and Grafana Cloud are, for the CLI's closing summary.
argocdURL: ${ARGOCD_URL}
grafanaURL: ${GRAFANA_URL}

# The application chart version the CLI writes into new Environments.
chartVersion: ${chart_version}

# The org GitHub App the deploy workflow uses to write image tags back.
githubApp:
  id: ${GITHUB_APP_ID}
  installationId: ${GITHUB_APP_INSTALLATION_ID}

# The age public key the CLI encrypts secrets with. The private key exists
# only in the cluster (Secret argocd/sops-age).
agePublicKey: ${AGE_PUBLIC_KEY}

# The Object Storage bucket CloudNativePG backups go to.
backupsBucket: ${backups_bucket}

# Bootstrap-only settings.
acme:
  email: ${acme_email}
  server: ${acme_server}
argocdAdminGroup: ${ARGOCD_ADMIN_GROUP}
clusterName: ${cluster_name}
EOF
  ok "wrote $file"
}

write_sops_yaml() {
  local file="$PLATFORM_REPO/.sops.yaml"
  if [[ "$DRY_RUN" == "1" ]]; then dry "would write $file"; return 0; fi
  cat > "$file" <<EOF
# SOPS creation rules for this Platform repository. Every file under
# bootstrap/sops is encrypted for the Platform's age public key (the one
# in platform.yaml); only data and stringData are encrypted so names,
# namespaces and labels stay readable and diffable. Written by
# scripts/bootstrap-wizard.sh.
creation_rules:
  - path_regex: bootstrap/sops/.*\.enc\.yaml\$
    encrypted_regex: ^(data|stringData)\$
    age: ${AGE_PUBLIC_KEY}
EOF
  ok "wrote $file"
}

write_bootstrap_components() {
  local file="$PLATFORM_REPO/bootstrap/platform-components.yaml"
  local iidp_url="$1" bootstrap_rev="$2" platform_url="$3"
  if [[ "$DRY_RUN" == "1" ]]; then dry "would write $file (bootstrap pinned to $bootstrap_rev)"; return 0; fi
  mkdir -p "$(dirname "$file")"
  cat > "$file" <<EOF
# Pins the bootstrap from the iidp repository and renders it with this
# repository's platform.yaml as values. The root Application 'platform'
# (applied by cloud-init) syncs this file. Written by
# scripts/bootstrap-wizard.sh; upgrading the bootstrap is changing
# targetRevision below to a newer iidp release tag.
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: platform-components
  namespace: argocd
spec:
  project: default
  sources:
    - repoURL: ${iidp_url}
      targetRevision: ${bootstrap_rev}
      path: bootstrap
      helm:
        valueFiles:
          - \$platform/platform.yaml
        parameters:
          - name: bootstrap.repoURL
            value: \$ARGOCD_APP_SOURCE_REPO_URL
          - name: bootstrap.targetRevision
            value: \$ARGOCD_APP_SOURCE_TARGET_REVISION
    - repoURL: ${platform_url}
      targetRevision: HEAD
      ref: platform
  destination:
    server: https://kubernetes.default.svc
    namespace: argocd
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - ServerSideApply=true
    retry:
      limit: -1
      backoff:
        duration: 10s
        factor: 2
        maxDuration: 3m
EOF
  ok "wrote $file"
}

write_bootstrap_secrets() {
  local file="$PLATFORM_REPO/bootstrap/platform-secrets.yaml"
  local platform_url="$1"
  if [[ "$DRY_RUN" == "1" ]]; then dry "would write $file"; return 0; fi
  mkdir -p "$(dirname "$file")"
  cat > "$file" <<EOF
# The SOPS-encrypted Secrets the Platform components read, decrypted in the
# ArgoCD repo server by KSOPS with the age key in the Secret argocd/sops-age.
# Written by scripts/bootstrap-wizard.sh.
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: platform-secrets
  namespace: argocd
spec:
  project: default
  source:
    repoURL: ${platform_url}
    targetRevision: HEAD
    path: bootstrap/sops
  destination:
    server: https://kubernetes.default.svc
    namespace: argocd
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - ServerSideApply=true
    retry:
      limit: -1
      backoff:
        duration: 10s
        factor: 2
        maxDuration: 3m
EOF
  ok "wrote $file"
}

write_sops_kustomization() {
  local dir="$PLATFORM_REPO/bootstrap/sops"
  if [[ "$DRY_RUN" == "1" ]]; then dry "would write $dir/kustomization.yaml and $dir/ksops.yaml"; return 0; fi
  mkdir -p "$dir"
  cat > "$dir/kustomization.yaml" <<'EOF'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
generators:
  - ksops.yaml
EOF
  cat > "$dir/ksops.yaml" <<'EOF'
# KSOPS generator: every listed file is decrypted with sops and emitted as a
# resource. Each Secret carries kustomize.config.k8s.io/needs-hash: "false"
# so its name stays what the components reference.
apiVersion: viaduct.ai/v1
kind: ksops
metadata:
  name: platform-secrets
  annotations:
    config.kubernetes.io/function: |
      exec:
        path: ksops
files:
  - argocd-entra.enc.yaml
  - cloudflare-api-token-cert-manager.enc.yaml
  - cloudflare-api-token-external-dns.enc.yaml
  - grafana-cloud.enc.yaml
EOF
  ok "wrote $dir/kustomization.yaml and $dir/ksops.yaml"
}

# write_secret FILE NAME NAMESPACE EXTRA_LABEL_LINE STRINGDATA_LINES
write_secret_plaintext() {
  local file="$1" name="$2" namespace="$3" extra_labels="$4" stringdata="$5"
  mkdir -p "$(dirname "$file")"
  {
    echo "apiVersion: v1"
    echo "kind: Secret"
    echo "metadata:"
    echo "  name: ${name}"
    echo "  namespace: ${namespace}"
    if [[ -n "$extra_labels" ]]; then
      echo "  labels:"
      echo "$extra_labels"
    fi
    echo "  annotations:"
    echo '    kustomize.config.k8s.io/needs-hash: "false"'
    echo "type: Opaque"
    echo "stringData:"
    echo "$stringdata"
  } > "$file"
}

write_and_encrypt_secrets() {
  local sops_dir="$PLATFORM_REPO/bootstrap/sops"

  if [[ "$DRY_RUN" == "1" ]]; then
    dry "would write and sops-encrypt bootstrap/sops/argocd-entra.enc.yaml"
    dry "would write and sops-encrypt bootstrap/sops/cloudflare-api-token-cert-manager.enc.yaml"
    dry "would write and sops-encrypt bootstrap/sops/cloudflare-api-token-external-dns.enc.yaml"
    dry "would write and sops-encrypt bootstrap/sops/grafana-cloud.enc.yaml"
    return 0
  fi

  if [[ -n "$ENTRA_ARGOCD_CLIENT_ID" ]]; then
    write_secret_plaintext "$sops_dir/argocd-entra.enc.yaml" argocd-entra argocd \
      "    app.kubernetes.io/part-of: argocd" \
      "  clientID: ${ENTRA_ARGOCD_CLIENT_ID}
  clientSecret: ${ENTRA_ARGOCD_CLIENT_SECRET}
  tenant: ${ENTRA_ARGOCD_TENANT}"
    sops_encrypt_in_place "bootstrap/sops/argocd-entra.enc.yaml"
    ok "wrote and encrypted bootstrap/sops/argocd-entra.enc.yaml"
  else
    note "no new ArgoCD Entra values collected; leaving argocd-entra.enc.yaml as it is"
  fi

  if [[ -n "$CLOUDFLARE_TOKEN" ]]; then
    write_secret_plaintext "$sops_dir/cloudflare-api-token-cert-manager.enc.yaml" cloudflare-api-token cert-manager "" \
      "  apiToken: ${CLOUDFLARE_TOKEN}"
    sops_encrypt_in_place "bootstrap/sops/cloudflare-api-token-cert-manager.enc.yaml"

    write_secret_plaintext "$sops_dir/cloudflare-api-token-external-dns.enc.yaml" cloudflare-api-token external-dns "" \
      "  apiToken: ${CLOUDFLARE_TOKEN}"
    sops_encrypt_in_place "bootstrap/sops/cloudflare-api-token-external-dns.enc.yaml"
    ok "wrote and encrypted the two Cloudflare secrets"
  else
    note "no new Cloudflare token collected; leaving the Cloudflare secrets as they are"
  fi

  if [[ -n "$GRAFANA_ACCESS_TOKEN" ]]; then
    write_secret_plaintext "$sops_dir/grafana-cloud.enc.yaml" grafana-cloud monitoring "" \
      "  prometheus-url: ${GRAFANA_PROM_URL}
  prometheus-username: ${GRAFANA_PROM_USER}
  loki-url: ${GRAFANA_LOKI_URL}
  loki-username: ${GRAFANA_LOKI_USER}
  access-token: ${GRAFANA_ACCESS_TOKEN}"
    sops_encrypt_in_place "bootstrap/sops/grafana-cloud.enc.yaml"
    ok "wrote and encrypted bootstrap/sops/grafana-cloud.enc.yaml"
  else
    note "no new Grafana Cloud values collected; leaving grafana-cloud.enc.yaml as it is"
  fi
}

git_commit_if_changed() { # git_commit_if_changed "message" PATH...
  local message="$1"; shift
  if [[ "$DRY_RUN" == "1" ]]; then
    dry "would run: git -C $PLATFORM_REPO add $* && git commit -m '$message' (if there are changes)"
    return 0
  fi
  ( cd "$PLATFORM_REPO" && git add -- "$@" )
  if ( cd "$PLATFORM_REPO" && git diff --cached --quiet ); then
    note "no changes to commit for: $message"
    return 0
  fi
  ( cd "$PLATFORM_REPO" && git commit -q -m "$message" )
  ok "committed: $message"
}

stage_platform_repo() {
  stage "Platform repository"

  local iidp_origin platform_origin iidp_url platform_url
  iidp_origin=$(git -C "$IIDP_REPO_ROOT" remote get-url origin 2>/dev/null || echo "https://github.com/itema-as/iidp.git")
  platform_origin=$(git -C "$PLATFORM_REPO" remote get-url origin 2>/dev/null || echo "https://github.com/itema-as/iidp-platform.git")
  iidp_url=$(normalize_git_url "$iidp_origin")
  platform_url=$(normalize_git_url "$platform_origin")

  local owner_repo tag chart_version bootstrap_rev
  owner_repo=$(github_owner_of "$iidp_origin")
  tag=$(latest_release_tag "$owner_repo" || true)
  if [[ -n "$tag" ]]; then
    chart_version="${tag#v}"
    bootstrap_rev="$tag"
    log_choice "using the latest release tag ($tag) for chartVersion and the bootstrap pin"
  else
    chart_version="0.1.0"
    bootstrap_rev="main"
    log_choice "no release tag found; chartVersion defaults to 0.1.0 and the bootstrap pins to main"
  fi

  ask ACME_EMAIL "Contact email for Let's Encrypt expiry notices (acme.email):" "${ACME_EMAIL:-platform@${BASE_DOMAIN#*.}}"
  ask ACME_SERVER "ACME server (acme.server):" "${ACME_SERVER:-https://acme-v02.api.letsencrypt.org/directory}"
  ask CLUSTER_NAME "Cluster label for Grafana Cloud (clusterName):" "${CLUSTER_NAME:-iidp}"
  ask BACKUPS_BUCKET "Object Storage bucket for database backups (backupsBucket):" "${BACKUPS_BUCKET:-$(tfvar_get "$STATE_TFVARS" backup_bucket_name || echo itema-iidp-db-backups)}"

  write_platform_yaml "$chart_version" "$ACME_EMAIL" "$ACME_SERVER" "$CLUSTER_NAME" "$BACKUPS_BUCKET"
  write_sops_yaml
  write_bootstrap_components "$iidp_url" "$bootstrap_rev" "$platform_url"
  write_bootstrap_secrets "$platform_url"
  write_sops_kustomization
  write_and_encrypt_secrets

  git_commit_if_changed "Add platform.yaml" platform.yaml .sops.yaml bootstrap/platform-components.yaml bootstrap/platform-secrets.yaml bootstrap/sops/kustomization.yaml bootstrap/sops/ksops.yaml
  git_commit_if_changed "Add Platform secrets" bootstrap/sops

  if [[ "$NO_PUSH" == "1" ]]; then
    note "--no-push: leaving the commits unpushed"
  elif [[ "$DRY_RUN" == "1" ]]; then
    dry "would run: git -C $PLATFORM_REPO push"
  else
    ( cd "$PLATFORM_REPO" && git push ) || die "git push failed for the Platform repository"
    ok "pushed the Platform repository"
  fi
}

# ──────────────────────────────────────────────────────────────────────────
# Stage 9: closing summary
# ──────────────────────────────────────────────────────────────────────────

closing_summary() {
  stage "Done"
  printf '\n%s%s  ✓ Platform bootstrap complete%s\n\n' "$BOLD" "$GREEN" "$RESET" >&2
  note "ArgoCD:        ${ARGOCD_URL:-<not set>}"
  note "Node IPv4:     ${NODE_IP:-<not set>}"
  note "Kubeconfig:    ssh root@${NODE_IP:-<node-ip>} cat /etc/rancher/k3s/k3s.yaml | sed \"s/127.0.0.1/${NODE_IP:-<node-ip>}/\" > ~/.kube/iidp.yaml"

  if [[ -z "$CLOUDFLARE_ZONE" ]] || ! confirm "Does ${BASE_DOMAIN:-the base domain} already resolve through Cloudflare?"; then
    MANUAL_STEPS+=("Make sure ${BASE_DOMAIN:-the base domain} resolves once external-dns has created its first record (see bootstrap/README.md and bootstrap/components/tls)")
  fi

  if (( ${#MANUAL_STEPS[@]} > 0 )); then
    printf '\n' >&2; warn "still to do by hand:"
    for m in "${MANUAL_STEPS[@]}"; do note "  - $m"; done
  fi
  printf '\n' >&2
}

# ──────────────────────────────────────────────────────────────────────────
# main
# ──────────────────────────────────────────────────────────────────────────

main() {
  banner
  preflight
  stage_hetzner
  stage_cloudflare
  stage_grafana
  stage_github_app
  stage_entra
  stage_opentofu
  stage_platform_repo
  closing_summary
}

if [[ "${IIDP_WIZARD_SOURCE_ONLY:-0}" != "1" ]]; then
  main "$@"
fi
