#!/usr/bin/env bash
#
# Shell test suite for scripts/bootstrap-wizard.sh. Run from the repository
# root (or anywhere: paths are resolved relative to this file):
#
#   bash test/wizard/run.sh
#
# No network, no real git remotes, no real tofu/gh/sops/az/ssh calls: the
# wizard's wrapper functions are exercised through IIDP_WIZARD_FAKE=1 and
# IIDP_WIZARD_SOURCE_ONLY=1 (which stops the script after defining its
# functions, so this file can call them directly and inspect the result).
# This is the "fake run" the wizard CI job runs, alongside shellcheck.
set -uo pipefail

# The wizard itself requires bash >= 4.3 (namerefs); fail clearly here too,
# rather than have every test below fail confusingly because sourcing it
# tripped its own version guard inside a subshell.
if [ -z "${BASH_VERSINFO:-}" ] || [ "${BASH_VERSINFO[0]}" -lt 4 ] || { [ "${BASH_VERSINFO[0]}" -eq 4 ] && [ "${BASH_VERSINFO[1]}" -lt 3 ]; }; then
  echo "test/wizard/run.sh requires bash >= 4.3, like the wizard itself (see scripts/bootstrap-wizard.sh)." >&2
  echo "On macOS: brew install bash, then run: \$(brew --prefix)/bin/bash test/wizard/run.sh" >&2
  exit 1
fi

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$TEST_DIR/../.." && pwd)"
WIZARD="$REPO_ROOT/scripts/bootstrap-wizard.sh"

PASS=0
FAIL=0

t_start() { CURRENT_TEST="$1"; }
t_pass() { PASS=$((PASS + 1)); printf 'ok   - %s\n' "$CURRENT_TEST"; }
t_fail() { FAIL=$((FAIL + 1)); printf 'FAIL - %s: %s\n' "$CURRENT_TEST" "$1"; }

assert_eq() { # assert_eq actual expected
  if [[ "$1" == "$2" ]]; then t_pass; else t_fail "expected '$2', got '$1'"; fi
}

assert_contains() { # assert_contains haystack needle
  if [[ "$1" == *"$2"* ]]; then t_pass; else t_fail "expected output to contain '$2'"; fi
}

assert_not_contains() { # assert_not_contains haystack needle
  if [[ "$1" != *"$2"* ]]; then t_pass; else t_fail "expected output NOT to contain '$2'"; fi
}

assert_success() { # assert_success "$?"
  if [[ "$1" == "0" ]]; then t_pass; else t_fail "expected exit 0, got $1"; fi
}

# Every test sources the wizard fresh (IIDP_WIZARD_SOURCE_ONLY=1: functions
# only, main() does not run) into a subshell, so tests never share state.
# Positional parameters are cleared before sourcing: otherwise the wizard's
# own argument parser would see this function's argument ($1, the code to
# eval) as its first CLI flag and reject it.
in_wizard() { # in_wizard 'shell code using the wizard's functions'
  local code="$1"
  ( set --
    # A prefix like `VAR=1 source file` only sets VAR for the duration of
    # that one command, reverting it the moment sourcing finishes and
    # leaving it unbound (under the wizard's own set -u) for every line
    # eval'd below. Plain assignments in this subshell persist instead.
    # shellcheck disable=SC2034
    IIDP_WIZARD_SOURCE_ONLY=1
    # shellcheck disable=SC2034
    IIDP_WIZARD_FAKE=1
    # shellcheck disable=SC1090
    source "$WIZARD" >/dev/null 2>&1
    eval "$code" )
}

scratch_dir() { mktemp -d "${TMPDIR:-/tmp}/iidp-wizard-test.XXXXXX"; }

# ── bash -n and shellcheck (also run as separate CI steps; cheap here too) ──

t_start "bash -n"
bash -n "$WIZARD" 2>/dev/null
assert_success "$?"

if command -v shellcheck >/dev/null 2>&1; then
  t_start "shellcheck"
  shellcheck "$WIZARD" >/dev/null
  assert_success "$?"
else
  echo "skip - shellcheck not installed"
fi

# ── tfvar_get / tfvar_set ────────────────────────────────────────────────

t_start "tfvar_set writes a KEY = \"value\" line, tfvar_get reads it back"
d=$(scratch_dir)
out=$(in_wizard "
  f='$d/terraform.tfvars'
  tfvar_set \"\$f\" hcloud_token 'abc123' >/dev/null
  tfvar_get \"\$f\" hcloud_token
")
assert_eq "$out" "abc123"

t_start "tfvar_set is an upsert: re-running keeps one line with the new value"
d=$(scratch_dir)
out=$(in_wizard "
  f='$d/terraform.tfvars'
  tfvar_set \"\$f\" hcloud_token 'first' >/dev/null
  tfvar_set \"\$f\" hcloud_token 'second' >/dev/null
  grep -c '^hcloud_token' \"\$f\"
")
assert_eq "$out" "1"
out=$(in_wizard "
  f='$d/terraform.tfvars'
  tfvar_get \"\$f\" hcloud_token
")
assert_eq "$out" "second"

t_start "tfvar_get on a missing key fails"
d=$(scratch_dir)
( in_wizard "
  f='$d/terraform.tfvars'
  touch \"\$f\"
  tfvar_get \"\$f\" nope
" >/dev/null 2>&1 )
assert_eq "$?" "1"

# ── tfvar_get_multiline / tfvar_set_multiline (the PEM, #41) ────────────

t_start "tfvar_set_multiline writes a heredoc, tfvar_get_multiline reads it back verbatim"
d=$(scratch_dir)
out=$(in_wizard "
  f='$d/terraform.tfvars'
  key=\$'-----BEGIN RSA PRIVATE KEY-----\nline one\nline two\n-----END RSA PRIVATE KEY-----'
  tfvar_set_multiline \"\$f\" platform_repo_github_app_private_key \"\$key\" >/dev/null
  tfvar_get_multiline \"\$f\" platform_repo_github_app_private_key
")
assert_contains "$out" "-----BEGIN RSA PRIVATE KEY-----"
assert_contains "$out" "line one"
assert_contains "$out" "line two"
assert_contains "$out" "-----END RSA PRIVATE KEY-----"

t_start "tfvar_set_multiline is an upsert: re-running keeps one block with the new value"
d=$(scratch_dir)
out=$(in_wizard "
  f='$d/terraform.tfvars'
  tfvar_set_multiline \"\$f\" platform_repo_github_app_private_key \$'first\nkey' >/dev/null
  tfvar_set_multiline \"\$f\" platform_repo_github_app_private_key \$'second\nkey' >/dev/null
  grep -c 'platform_repo_github_app_private_key =' \"\$f\"
")
assert_eq "$out" "1"
out=$(in_wizard "
  f='$d/terraform.tfvars'
  tfvar_get_multiline \"\$f\" platform_repo_github_app_private_key
")
assert_contains "$out" "second"
assert_not_contains "$out" "first"

t_start "tfvar_set_multiline replaces a single-line tfvar_set value for the same key"
d=$(scratch_dir)
out=$(in_wizard "
  f='$d/terraform.tfvars'
  tfvar_set \"\$f\" platform_repo_github_app_private_key 'placeholder' >/dev/null
  tfvar_set_multiline \"\$f\" platform_repo_github_app_private_key \$'-----BEGIN RSA PRIVATE KEY-----\nreal\n-----END RSA PRIVATE KEY-----' >/dev/null
  grep -c 'platform_repo_github_app_private_key' \"\$f\"
")
assert_eq "$out" "1"
out=$(in_wizard "
  f='$d/terraform.tfvars'
  tfvar_get_multiline \"\$f\" platform_repo_github_app_private_key
")
assert_contains "$out" "real"
assert_not_contains "$out" "placeholder"

t_start "tfvar_get_multiline on a missing key fails"
d=$(scratch_dir)
( in_wizard "
  f='$d/terraform.tfvars'
  touch \"\$f\"
  tfvar_get_multiline \"\$f\" nope
" >/dev/null 2>&1 )
assert_eq "$?" "1"

t_start "tfvar_get_multiline does not mistake a plain tfvar_set line for a heredoc"
d=$(scratch_dir)
( in_wizard "
  f='$d/terraform.tfvars'
  tfvar_set \"\$f\" hcloud_token 'abc123' >/dev/null
  tfvar_get_multiline \"\$f\" hcloud_token
" >/dev/null 2>&1 )
assert_eq "$?" "1"

t_start "tfvar_set_multiline chmods the file 600, like tfvar_set"
d=$(scratch_dir)
out=$(in_wizard "
  f='$d/terraform.tfvars'
  tfvar_set_multiline \"\$f\" platform_repo_github_app_private_key 'x' >/dev/null
  stat -c '%a' \"\$f\" 2>/dev/null || stat -f '%OLp' \"\$f\"
")
assert_eq "$out" "600"

# ── platform_yaml_get ────────────────────────────────────────────────────

t_start "platform_yaml_get reads a top-level scalar"
d=$(scratch_dir)
cat > "$d/platform.yaml" <<'EOF'
baseDomain: app.itma.no
cloudflareZone: itma.no
githubApp:
  id: 42
  installationId: 99
acme:
  email: platform@itma.no
  server: https://acme-v02.api.letsencrypt.org/directory
EOF
out=$(in_wizard "platform_yaml_get '$d/platform.yaml' baseDomain")
assert_eq "$out" "app.itma.no"

t_start "platform_yaml_get reads a nested field"
out=$(in_wizard "platform_yaml_get '$d/platform.yaml' githubApp.id")
assert_eq "$out" "42"
out=$(in_wizard "platform_yaml_get '$d/platform.yaml' githubApp.installationId")
assert_eq "$out" "99"
out=$(in_wizard "platform_yaml_get '$d/platform.yaml' acme.email")
assert_eq "$out" "platform@itma.no"

t_start "ask_platform_settings offers what platform.yaml already holds, so a re-run keeps it"
d=$(scratch_dir)
cat > "$d/platform.yaml" <<'EOF'
acme:
  email: admin@example.test
  server: https://acme-staging-v02.api.letsencrypt.org/directory
clusterName: custom-cluster
backupsBucket: custom-bucket
EOF
answers=$(scratch_dir)/answers.env
: > "$answers"
out=$(in_wizard "
  PLATFORM_REPO='$d'
  BASE_DOMAIN=app.itma.no
  STATE_TFVARS='$d/state.tfvars'
  IIDP_WIZARD_ANSWERS='$answers'
  ask_platform_settings >/dev/null
  echo \"[\$ACME_EMAIL] [\$ACME_SERVER] [\$CLUSTER_NAME] [\$BACKUPS_BUCKET]\"
" 2>&1)
assert_eq "$out" "[admin@example.test] [https://acme-staging-v02.api.letsencrypt.org/directory] [custom-cluster] [custom-bucket]"

t_start "ask_platform_settings falls back to the built-in defaults on a first run"
d=$(scratch_dir)
out=$(in_wizard "
  PLATFORM_REPO='$d'
  BASE_DOMAIN=app.itma.no
  STATE_TFVARS='$d/state.tfvars'
  IIDP_WIZARD_ANSWERS='$answers'
  ask_platform_settings >/dev/null
  echo \"[\$ACME_EMAIL] [\$ACME_SERVER] [\$CLUSTER_NAME] [\$BACKUPS_BUCKET]\"
" 2>&1)
assert_eq "$out" "[platform@itma.no] [https://acme-v02.api.letsencrypt.org/directory] [iidp] [itema-iidp-db-backups]"

t_start "platform_yaml_get on a missing file fails"
( in_wizard "platform_yaml_get '$d/does-not-exist.yaml' baseDomain" >/dev/null 2>&1 )
assert_eq "$?" "1"

# ── mask ─────────────────────────────────────────────────────────────────

t_start "mask keeps the first and last two characters"
out=$(in_wizard "mask 'abcdefgh'")
assert_eq "$out" "ab****gh"

t_start "mask fully hides short values"
out=$(in_wizard "mask 'ab'")
assert_eq "$out" "**"

# ── git URL helpers ──────────────────────────────────────────────────────

t_start "normalize_git_url turns an ssh remote into https"
out=$(in_wizard "normalize_git_url 'git@github.com:Itema-as/iidp-platform.git'")
assert_eq "$out" "https://github.com/Itema-as/iidp-platform.git"

t_start "normalize_git_url leaves an https remote as it is"
out=$(in_wizard "normalize_git_url 'https://github.com/Itema-as/iidp-platform.git'")
assert_eq "$out" "https://github.com/Itema-as/iidp-platform.git"

t_start "github_owner_of extracts owner/repo"
out=$(in_wizard "github_owner_of 'https://github.com/Itema-as/iidp-platform.git'")
assert_eq "$out" "Itema-as/iidp-platform"

t_start "html_escape escapes everything that would break a single-quoted HTML attribute"
export IIDP_TEST_HTML_INPUT="a & b 'quoted' \"double\" <tag>"
# shellcheck disable=SC2016  # deliberately unexpanded here: eval'd inside in_wizard's subshell
out=$(in_wizard 'html_escape "$IIDP_TEST_HTML_INPUT"')
unset IIDP_TEST_HTML_INPUT
assert_eq "$out" "a &amp; b &#39;quoted&#39; &quot;double&quot; &lt;tag&gt;"

t_start "yaml_str single-quotes a value and doubles any single quote in it"
export IIDP_TEST_YAML_INPUT="it's: a #value"
# shellcheck disable=SC2016  # deliberately unexpanded here: eval'd inside in_wizard's subshell
out=$(in_wizard 'yaml_str "$IIDP_TEST_YAML_INPUT"')
unset IIDP_TEST_YAML_INPUT
assert_eq "$out" "'it''s: a #value'"

t_start "yaml_str keeps a value that looks like a number a string"
# shellcheck disable=SC2016  # deliberately unexpanded here: eval'd inside in_wizard's subshell
out=$(in_wizard 'yaml_str 3607024')
assert_eq "$out" "'3607024'"

# ── network wrapper functions under IIDP_WIZARD_FAKE=1 ──────────────────

t_start "validate_hetzner_token succeeds for the fake token"
( in_wizard "validate_hetzner_token 'fake-token'" )
assert_success "$?"

t_start "validate_hetzner_token fails for any other token"
if in_wizard "validate_hetzner_token 'wrong-token'"; then
  t_fail "expected a non-zero exit"
else
  t_pass
fi

t_start "validate_cloudflare_token succeeds for the fake token"
( in_wizard "validate_cloudflare_token 'fake-token'" )
assert_success "$?"

t_start "validate_cloudflare_token fails for any other token"
if in_wizard "validate_cloudflare_token 'wrong-token'"; then
  t_fail "expected a non-zero exit"
else
  t_pass
fi

t_start "check_cloudflare_zone_readable succeeds against the fake response"
( in_wizard "check_cloudflare_zone_readable 'fake-token' 'itma.no'" )
assert_success "$?"

# ── idempotency: re-running a stage keeps an already-encrypted secret ────

t_start "stage_github_app keeps an existing githubApp.id, never calls gh again, and writes the tfvars credential (#41)"
d=$(scratch_dir)
mkdir -p "$d/platform-repo" "$d/infra-platform"
cat > "$d/platform-repo/platform.yaml" <<'EOF'
githubApp:
  id: 4242
  installationId: 9191
EOF
out=$(in_wizard "
  PLATFORM_REPO='$d/platform-repo'
  INFRA_PLATFORM_DIR='$d/infra-platform'
  IIDP_WIZARD_FAKE_APP_PEM=\$'-----BEGIN RSA PRIVATE KEY-----\nfresh-key\n-----END RSA PRIVATE KEY-----'
  stage_github_app
  echo \"id=\$GITHUB_APP_ID installation=\$GITHUB_APP_INSTALLATION_ID\"
" 2>&1)
rc=$?
t_start "stage_github_app (keep path) exits 0"
assert_success "$rc"
t_start "stage_github_app keeps an existing githubApp.id and never calls gh again"
assert_contains "$out" "keeping existing GitHub App id 4242"
assert_contains "$out" "id=4242 installation=9191"
assert_not_contains "$out" "set org secret"
assert_not_contains "$out" "set org variable"

t_start "stage_github_app (keep path, no tfvars yet) writes the three OpenTofu variables"
tfvars_content=$(cat "$d/infra-platform/terraform.tfvars" 2>/dev/null || echo "MISSING")
assert_contains "$tfvars_content" 'platform_repo_github_app_id = "4242"'
assert_contains "$tfvars_content" 'platform_repo_github_app_installation_id = "9191"'
assert_contains "$tfvars_content" "platform_repo_github_app_private_key = <<"
assert_contains "$tfvars_content" "-----BEGIN RSA PRIVATE KEY-----"
assert_contains "$tfvars_content" "fresh-key"

t_start "stage_github_app (keep path, PEM already in tfvars) keeps it without asking again"
out=$(in_wizard "
  PLATFORM_REPO='$d/platform-repo'
  INFRA_PLATFORM_DIR='$d/infra-platform'
  IIDP_WIZARD_FAKE_APP_PEM=\$'-----BEGIN RSA PRIVATE KEY-----\nshould-not-be-written\n-----END RSA PRIVATE KEY-----'
  stage_github_app
" 2>&1)
rc=$?
t_start "stage_github_app (keep path, PEM already in tfvars) exits 0"
assert_success "$rc"
t_start "stage_github_app (keep path, PEM already in tfvars) keeps it without asking again"
assert_contains "$out" "kept the existing platform_repo_github_app_private_key"
tfvars_content=$(cat "$d/infra-platform/terraform.tfvars" 2>/dev/null || echo "MISSING")
assert_contains "$tfvars_content" "fresh-key"
assert_not_contains "$tfvars_content" "should-not-be-written"

t_start "stage_github_app (fresh App) creates the App, sets org secret/variable, and writes tfvars"
d=$(scratch_dir)
mkdir -p "$d/platform-repo" "$d/infra-platform"
cat > "$d/platform-repo/platform.yaml" <<'EOF'
baseDomain: app.itma.no
EOF
answers=$(scratch_dir)/answers.env
cat > "$answers" <<'EOF'
GITHUB_ORG=itema-as
GITHUB_APP_ID=5555
PEM_PATH=/dev/null
GITHUB_APP_INSTALLATION_ID=7777
EOF
out=$(in_wizard "
  PLATFORM_REPO='$d/platform-repo'
  INFRA_PLATFORM_DIR='$d/infra-platform'
  IIDP_WIZARD_ANSWERS='$answers'
  IIDP_WIZARD_FAKE_APP_PEM=\$'-----BEGIN RSA PRIVATE KEY-----\nbrand-new-key\n-----END RSA PRIVATE KEY-----'
  stage_github_app
" 2>&1)
rc=$?
t_start "stage_github_app (fresh App) exits 0"
assert_success "$rc"
t_start "stage_github_app (fresh App) sets the org secret and variable"
assert_contains "$out" "(fake) set org secret IIDP_DEPLOY_APP_PRIVATE_KEY"
assert_contains "$out" "(fake) set org variable IIDP_DEPLOY_APP_ID=5555"
t_start "stage_github_app (fresh App) writes the tfvars credential for ArgoCD"
tfvars_content=$(cat "$d/infra-platform/terraform.tfvars" 2>/dev/null || echo "MISSING")
assert_contains "$tfvars_content" 'platform_repo_github_app_id = "5555"'
assert_contains "$tfvars_content" 'platform_repo_github_app_installation_id = "7777"'
assert_contains "$tfvars_content" "brand-new-key"

t_start "stage_cloudflare keeps existing encrypted secrets without asking for a new token"
d=$(scratch_dir)
mkdir -p "$d/platform-repo/bootstrap/sops"
cat > "$d/platform-repo/platform.yaml" <<'EOF'
baseDomain: app.itma.no
cloudflareZone: itma.no
EOF
touch "$d/platform-repo/bootstrap/sops/cloudflare-api-token-cert-manager.enc.yaml"
touch "$d/platform-repo/bootstrap/sops/cloudflare-api-token-external-dns.enc.yaml"
out=$(in_wizard "
  PLATFORM_REPO='$d/platform-repo'
  stage_cloudflare
  echo \"token=[\${CLOUDFLARE_TOKEN}]\"
" 2>&1)
rc=$?
t_start "stage_cloudflare (keep path) exits 0"
assert_success "$rc"
t_start "stage_cloudflare keeps existing encrypted secrets without asking for a new token"
assert_contains "$out" "keeping existing encrypted Cloudflare secrets"
assert_contains "$out" "token=[]"

t_start "stage_cloudflare asks for and validates a token when none is encrypted yet"
d=$(scratch_dir)
mkdir -p "$d/platform-repo/bootstrap/sops"
cat > "$d/platform-repo/platform.yaml" <<'EOF'
baseDomain: app.itma.no
cloudflareZone: itma.no
EOF
answers=$(scratch_dir)/answers.env
cat > "$answers" <<'EOF'
CLOUDFLARE_TOKEN=fake-token
EOF
out=$(in_wizard "
  PLATFORM_REPO='$d/platform-repo'
  IIDP_WIZARD_ANSWERS='$answers'
  stage_cloudflare
  echo \"token=[\${CLOUDFLARE_TOKEN}]\"
" 2>&1)
rc=$?
t_start "stage_cloudflare (fresh path) exits 0"
assert_success "$rc"
t_start "stage_cloudflare asks for and validates a token when none is encrypted yet"
assert_contains "$out" "token validated against /user/tokens/verify"
assert_contains "$out" "zone itma.no is readable"
assert_contains "$out" "token=[fake-token]"

# ── Object Storage location / objectStorageEndpoint derivation ──────────

t_start "stage_hetzner derives objectStorageEndpoint from the default location (hel1)"
d=$(scratch_dir)
answers=$(scratch_dir)/answers.env
cat > "$answers" <<'EOF'
HCLOUD_TOKEN=fake-token
OBJECT_STORAGE_ACCESS_KEY=access123
OBJECT_STORAGE_SECRET_KEY=secret123
EOF
out=$(in_wizard "
  STATE_TFVARS='$d/state.tfvars'
  PLATFORM_TFVARS='$d/platform.tfvars'
  IIDP_WIZARD_ANSWERS='$answers'
  stage_hetzner >/dev/null
  echo \"location=[\${OBJECT_STORAGE_LOCATION}] endpoint=[\${OBJECT_STORAGE_ENDPOINT}]\"
" 2>&1)
rc=$?
t_start "stage_hetzner (derive endpoint) exits 0"
assert_success "$rc"
t_start "stage_hetzner derives objectStorageEndpoint from the default location (hel1)"
assert_contains "$out" "location=[hel1]"
assert_contains "$out" "endpoint=[https://hel1.your-objectstorage.com]"

t_start "stage_hetzner derives objectStorageEndpoint from a non-default location"
d=$(scratch_dir)
answers=$(scratch_dir)/answers.env
cat > "$answers" <<'EOF'
HCLOUD_TOKEN=fake-token
OBJECT_STORAGE_ACCESS_KEY=access123
OBJECT_STORAGE_SECRET_KEY=secret123
OBJECT_STORAGE_LOCATION=fsn1
EOF
out=$(in_wizard "
  STATE_TFVARS='$d/state.tfvars'
  PLATFORM_TFVARS='$d/platform.tfvars'
  IIDP_WIZARD_ANSWERS='$answers'
  stage_hetzner >/dev/null
  echo \"location=[\${OBJECT_STORAGE_LOCATION}] endpoint=[\${OBJECT_STORAGE_ENDPOINT}]\"
" 2>&1)
rc=$?
t_start "stage_hetzner (non-default location) exits 0"
assert_success "$rc"
t_start "stage_hetzner derives objectStorageEndpoint from a non-default location"
assert_contains "$out" "location=[fsn1]"
assert_contains "$out" "endpoint=[https://fsn1.your-objectstorage.com]"
out=$(in_wizard "tfvar_get '$d/state.tfvars' location")
assert_eq "$out" "fsn1"

t_start "stage_hetzner honours an explicit objectStorageEndpoint override"
d=$(scratch_dir)
answers=$(scratch_dir)/answers.env
cat > "$answers" <<'EOF'
HCLOUD_TOKEN=fake-token
OBJECT_STORAGE_ACCESS_KEY=access123
OBJECT_STORAGE_SECRET_KEY=secret123
OBJECT_STORAGE_ENDPOINT=https://custom.example.test
EOF
out=$(in_wizard "
  STATE_TFVARS='$d/state.tfvars'
  PLATFORM_TFVARS='$d/platform.tfvars'
  IIDP_WIZARD_ANSWERS='$answers'
  stage_hetzner >/dev/null
  echo \"endpoint=[\${OBJECT_STORAGE_ENDPOINT}]\"
" 2>&1)
rc=$?
t_start "stage_hetzner (override) exits 0"
assert_success "$rc"
t_start "stage_hetzner honours an explicit objectStorageEndpoint override"
assert_contains "$out" "endpoint=[https://custom.example.test]"

t_start "write_platform_yaml writes objectStorageEndpoint"
d=$(scratch_dir)
mkdir -p "$d/platform-repo"
out=$(in_wizard "
  PLATFORM_REPO='$d/platform-repo'
  BASE_DOMAIN=app.itma.no
  CLOUDFLARE_ZONE=itma.no
  ARGOCD_URL=https://argocd.app.itma.no
  GRAFANA_URL=https://itema.grafana.net
  GITHUB_APP_ID=1
  GITHUB_APP_INSTALLATION_ID=1
  AGE_PUBLIC_KEY=age1test
  ARGOCD_ADMIN_GROUP=00000000-0000-0000-0000-000000000001
  write_platform_yaml 0.1.0 platform@itma.no https://acme-v02.api.letsencrypt.org/directory iidp itema-iidp-db-backups https://hel1.your-objectstorage.com
")
content=$(cat "$d/platform-repo/platform.yaml" 2>/dev/null || echo "MISSING")
assert_contains "$content" "objectStorageEndpoint: https://hel1.your-objectstorage.com"

# ── sops round-trip: what write_and_encrypt_secrets actually produces ───

if command -v age-keygen >/dev/null 2>&1 && command -v sops >/dev/null 2>&1; then
  t_start "write_and_encrypt_secrets produces a document sops can decrypt back"
  d=$(scratch_dir)
  keydir=$(scratch_dir)
  age-keygen -o "$keydir/key.txt" >/dev/null 2>&1
  pub=$(grep '^# public key:' "$keydir/key.txt" | sed 's/^# public key: *//')
  mkdir -p "$d/bootstrap/sops"
  out=$(in_wizard "
    PLATFORM_REPO='$d'
    AGE_PUBLIC_KEY='$pub'
    GRAFANA_ACCESS_TOKEN='shh-token'
    GRAFANA_PROM_URL='https://prom.example/api/prom/push'
    GRAFANA_PROM_USER='12345'
    GRAFANA_LOKI_URL='https://loki.example/loki/api/v1/push'
    GRAFANA_LOKI_USER='67890'
    ENTRA_OAUTH2_PROXY_CLIENT_ID='oauth2-client-id'
    ENTRA_OAUTH2_PROXY_CLIENT_SECRET='oauth2-client-secret'
    ENTRA_OAUTH2_PROXY_TENANT='11111111-1111-1111-1111-111111111111'
    ENTRA_OAUTH2_PROXY_COOKIE_SECRET='cookie-secret-value'
    write_sops_yaml
    write_and_encrypt_secrets
  " 2>&1)
  rc=$?
  assert_success "$rc"
  t_start "the written grafana secret file is sops ciphertext, not plaintext"
  content=$(cat "$d/bootstrap/sops/grafana-cloud.enc.yaml" 2>/dev/null || echo "MISSING")
  assert_contains "$content" "ENC["
  t_start "sops --decrypt reproduces the original grafana values"
  decrypted=$(SOPS_AGE_KEY_FILE="$keydir/key.txt" sops --decrypt "$d/bootstrap/sops/grafana-cloud.enc.yaml" 2>&1)
  assert_contains "$decrypted" "access-token: shh-token"
  assert_contains "$decrypted" "prometheus-username:"
  assert_contains "$decrypted" "12345"
  # Grafana Cloud instance ids are plain numbers. Unquoted, YAML reads them
  # as integers and the API server refuses the Secret ("stringData...
  # expected string"), which is what broke platform-secrets on the first
  # real bootstrap; every stringData value must stay a string.
  t_start "numeric grafana usernames stay strings in the decrypted Secret"
  types=$(SOPS_AGE_KEY_FILE="$keydir/key.txt" sops --decrypt --output-type json "$d/bootstrap/sops/grafana-cloud.enc.yaml" 2>&1 \
    | jq -r '[.stringData[] | type] | unique | join(",")')
  assert_eq "$types" "string"

  t_start "the written oauth2-proxy Entra secret file is sops ciphertext, not plaintext"
  oauth2content=$(cat "$d/bootstrap/sops/oauth2-proxy-entra.enc.yaml" 2>/dev/null || echo "MISSING")
  assert_contains "$oauth2content" "ENC["
  t_start "sops --decrypt reproduces the original oauth2-proxy Entra values"
  oauth2decrypted=$(SOPS_AGE_KEY_FILE="$keydir/key.txt" sops --decrypt "$d/bootstrap/sops/oauth2-proxy-entra.enc.yaml" 2>&1)
  assert_contains "$oauth2decrypted" "clientID: oauth2-client-id"
  assert_contains "$oauth2decrypted" "clientSecret: oauth2-client-secret"
  assert_contains "$oauth2decrypted" "tenant: 11111111-1111-1111-1111-111111111111"
  assert_contains "$oauth2decrypted" "cookieSecret: cookie-secret-value"
  assert_contains "$oauth2decrypted" "namespace: oauth2-proxy"

  t_start "write_backups_credentials produces a document sops can decrypt back, with no namespace"
  d2=$(scratch_dir)
  mkdir -p "$d2/bootstrap"
  out=$(in_wizard "
    PLATFORM_REPO='$d2'
    AGE_PUBLIC_KEY='$pub'
    OBJECT_STORAGE_ACCESS_KEY='iidpe2e'
    OBJECT_STORAGE_SECRET_KEY='iidpe2epassword'
    OBJECT_STORAGE_KEYS_CHANGED=1
    write_sops_yaml
    write_backups_credentials
  " 2>&1)
  rc=$?
  assert_success "$rc"
  t_start "the written backups-credentials file is sops ciphertext, not plaintext"
  backupscontent=$(cat "$d2/bootstrap/templates/backups-credentials.enc.yaml" 2>/dev/null || echo "MISSING")
  assert_contains "$backupscontent" "ENC["
  t_start "the written backups-credentials file carries no namespace"
  assert_not_contains "$backupscontent" "namespace:"
  t_start "sops --decrypt reproduces the original Object Storage keys"
  backupsdecrypted=$(SOPS_AGE_KEY_FILE="$keydir/key.txt" sops --decrypt "$d2/bootstrap/templates/backups-credentials.enc.yaml" 2>&1)
  assert_contains "$backupsdecrypted" "ACCESS_KEY_ID: iidpe2e"
  assert_contains "$backupsdecrypted" "ACCESS_SECRET_KEY: iidpe2epassword"
  assert_contains "$backupsdecrypted" 'kustomize.config.k8s.io/needs-hash: "false"'
  # A wave before the Cluster: the ScheduledBackup is immediate, so a
  # Secret in the Cluster's own wave loses the race and the Environment's
  # first backup fails for good.
  t_start "the written backups-credentials file syncs a wave before the database"
  assert_contains "$backupsdecrypted" 'argocd.argoproj.io/sync-wave: "-2"'

  t_start "write_backups_credentials decrypts unchanged after being copied to a different path"
  mkdir -p "$d2/applications/shop/prod/sops"
  cp "$d2/bootstrap/templates/backups-credentials.enc.yaml" "$d2/applications/shop/prod/sops/backups-credentials.enc.yaml"
  copieddecrypted=$(SOPS_AGE_KEY_FILE="$keydir/key.txt" sops --decrypt "$d2/applications/shop/prod/sops/backups-credentials.enc.yaml" 2>&1)
  assert_eq "$copieddecrypted" "$backupsdecrypted"

  t_start "write_backups_credentials keeps an existing file when the Object Storage keys are unchanged"
  before=$(cat "$d2/bootstrap/templates/backups-credentials.enc.yaml")
  out=$(in_wizard "
    PLATFORM_REPO='$d2'
    AGE_PUBLIC_KEY='$pub'
    OBJECT_STORAGE_ACCESS_KEY='iidpe2e'
    OBJECT_STORAGE_SECRET_KEY='iidpe2epassword'
    OBJECT_STORAGE_KEYS_CHANGED=0
    write_backups_credentials
  " 2>&1)
  rc=$?
  t_start "write_backups_credentials (kept path) exits 0"
  assert_success "$rc"
  t_start "write_backups_credentials keeps an existing file when the Object Storage keys are unchanged"
  assert_contains "$out" "keeping existing bootstrap/templates/backups-credentials.enc.yaml"
  after=$(cat "$d2/bootstrap/templates/backups-credentials.enc.yaml")
  assert_eq "$after" "$before"
else
  echo "skip - age-keygen or sops not installed, skipping the sops round-trip test"
fi

# ── full script, --dry-run: the smoke test the ticket asks for ──────────

t_start "--dry-run runs every stage without touching the network or disk, and exits 0"
d=$(scratch_dir)
out=$("$WIZARD" --dry-run --platform-repo "$d/does-not-exist" 2>&1)
rc=$?
assert_success "$rc"
assert_contains "$out" "Stage 1/9 · Preflight"
assert_contains "$out" "Stage 9/9 · Done"
for marker in "Hetzner" "Cloudflare" "Grafana Cloud" "GitHub App" "Entra ID" "OpenTofu" "Platform repository"; do
  t_start "--dry-run mentions the $marker stage"
  assert_contains "$out" "$marker"
done

t_start "--dry-run writes nothing under the (non-existent) platform repository"
if [[ -e "$d/does-not-exist" ]]; then
  t_fail "dry-run created $d/does-not-exist"
else
  t_pass
fi

t_start "--help exits 0 and does not require bash >= 4.3 checks to prompt for anything"
"$WIZARD" --help >/dev/null 2>&1
assert_success "$?"

# ── summary ──────────────────────────────────────────────────────────────

printf '\n%s passed, %s failed\n' "$PASS" "$FAIL"
[[ "$FAIL" -eq 0 ]]
