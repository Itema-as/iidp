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

t_start "stage_github_app keeps an existing githubApp.id and never calls gh again"
d=$(scratch_dir)
mkdir -p "$d/platform-repo"
cat > "$d/platform-repo/platform.yaml" <<'EOF'
githubApp:
  id: 4242
  installationId: 9191
EOF
out=$(in_wizard "
  PLATFORM_REPO='$d/platform-repo'
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
