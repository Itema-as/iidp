#!/usr/bin/env bash
#
# Tests for infra/tofu-env.sh, the file infra/README.md has the admin source
# before any `tofu` command in infra/platform. Run from anywhere:
#
#   bash test/tofu-env/run.sh
#
# Each case sources the script in a fresh shell process, the way an admin's
# new terminal would, under bash and (when installed) zsh, the macOS login
# shell. The fixtures are never named terraform.tfvars: the script is
# pointed at them with IIDP_STATE_TFVARS, the same override it documents.
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$TEST_DIR/../.." && pwd)"
SCRIPT="$REPO_ROOT/infra/tofu-env.sh"

PASS=0
FAIL=0

t_start() { CURRENT_TEST="$1"; }
t_pass() { PASS=$((PASS + 1)); printf 'ok   - %s\n' "$CURRENT_TEST"; }
t_fail() { FAIL=$((FAIL + 1)); printf 'FAIL - %s: %s\n' "$CURRENT_TEST" "$1"; }

assert_eq() { # assert_eq actual expected
  if [[ "$1" == "$2" ]]; then t_pass; else t_fail "expected '$2', got '$1'"; fi
}

assert_contains() { # assert_contains haystack needle
  if [[ "$1" == *"$2"* ]]; then t_pass; else t_fail "expected output to contain '$2', got: $1"; fi
}

assert_not_contains() { # assert_not_contains haystack needle
  if [[ "$1" != *"$2"* ]]; then t_pass; else t_fail "expected output NOT to contain '$2'"; fi
}

scratch_dir() { mktemp -d "${TMPDIR:-/tmp}/iidp-tofu-env-test.XXXXXX"; }

# in_shell SHELL SCRIPT [TFVARS]: sources SCRIPT (from directory $FROM_DIR,
# default /, so a relative SCRIPT is relative to that) in a new SHELL process with
# no AWS keys inherited (or with the stale ones in $STALE_KEYS, when set),
# then prints the source's status and what the environment holds afterwards,
# one per line: status, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, and "leak"
# when any helper function or variable survived. stderr goes to $ERR_FILE.
in_shell() {
  local shell="$1" script="$2" tfvars="${3:-}"
  local -a env_args=(env -u AWS_ACCESS_KEY_ID -u AWS_SECRET_ACCESS_KEY -u IIDP_STATE_TFVARS)
  if [[ -n "${STALE_KEYS:-}" ]]; then
    env_args+=(AWS_ACCESS_KEY_ID=stale-access AWS_SECRET_ACCESS_KEY=stale-secret)
  fi
  if [[ -n "$tfvars" ]]; then
    env_args+=("IIDP_STATE_TFVARS=$tfvars")
  fi
  # shellcheck disable=SC2016  # expanded by the inner shell, not this one
  "${env_args[@]}" "$shell" -c '
    cd "$2" || exit 99
    source "$1"
    rc=$?
    printf "%s\n" "$rc" "${AWS_ACCESS_KEY_ID-unset}" "${AWS_SECRET_ACCESS_KEY-unset}"
    if typeset -f _iidp_tofu_env_load >/dev/null 2>&1 || typeset -f _iidp_tofu_env_value >/dev/null 2>&1 || [ -n "${_iidp_tofu_env_self+x}" ]; then
      echo leak
    fi
  ' _ "$script" "${FROM_DIR:-/}" 2>"$ERR_FILE"
}

SHELLS=(bash)
if command -v zsh >/dev/null 2>&1; then
  SHELLS+=(zsh)
else
  echo "# zsh not installed; testing bash only"
fi

WORK="$(scratch_dir)"
ERR_FILE="$WORK/stderr"

# The format the bootstrap wizard's tfvar_set writes (one key = "value" per
# line), with the other keys it writes into the same file around them.
cat > "$WORK/wizard.tfvars" <<'EOF'
object_storage_access_key = "OLDACCESSKEY"
object_storage_access_key = "AKIAFIXTURE0123"
object_storage_secret_key = "fixture/secret+Key=0123456789abcdef"
location = "hel1"
EOF

cat > "$WORK/no-secret.tfvars" <<'EOF'
object_storage_access_key = "AKIAFIXTURE0123"
location = "hel1"
EOF

cp "$REPO_ROOT/infra/state-bucket/terraform.tfvars.example" "$WORK/example.tfvars"

for sh in "${SHELLS[@]}"; do
  t_start "$sh: sourcing exports both keys from the tfvars file"
  out="$(in_shell "$sh" "$SCRIPT" "$WORK/wizard.tfvars")"
  assert_eq "$out" "$(printf '%s\n' 0 AKIAFIXTURE0123 'fixture/secret+Key=0123456789abcdef')"

  t_start "$sh: says which file the keys came from"
  err="$(cat "$ERR_FILE")"
  assert_contains "$err" "exported AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY from $WORK/wizard.tfvars"
  # (stdout above is this harness printing the environment; the script's
  # own output all goes to stderr.)
  t_start "$sh: the access key is not printed"
  assert_not_contains "$err" "AKIAFIXTURE0123"
  t_start "$sh: the secret key is not printed"
  assert_not_contains "$err" "fixture/secret"

  t_start "$sh: a missing file fails, names the file, and exports nothing"
  out="$(in_shell "$sh" "$SCRIPT" "$WORK/does-not-exist.tfvars")"
  assert_eq "$out" "$(printf '%s\n' 1 unset unset)"
  t_start "$sh: the missing-file message names the file"
  assert_contains "$(cat "$ERR_FILE")" "no such file: $WORK/does-not-exist.tfvars"

  t_start "$sh: a file without the secret key fails and names that key"
  out="$(in_shell "$sh" "$SCRIPT" "$WORK/no-secret.tfvars")"
  assert_eq "$out" "$(printf '%s\n' 1 unset unset)"
  t_start "$sh: the missing-key message names only the missing key"
  err="$(cat "$ERR_FILE")"
  assert_contains "$err" "has no value for object_storage_secret_key"
  t_start "$sh: the missing-key message does not blame the key that is there"
  assert_not_contains "$err" "object_storage_access_key and"

  t_start "$sh: the example file's \"...\" placeholders count as missing"
  out="$(in_shell "$sh" "$SCRIPT" "$WORK/example.tfvars")"
  assert_eq "$out" "$(printf '%s\n' 1 unset unset)"
  t_start "$sh: the placeholder message names both keys"
  assert_contains "$(cat "$ERR_FILE")" "has no value for object_storage_access_key and object_storage_secret_key"

  t_start "$sh: a failure leaves keys already in the shell alone"
  out="$(STALE_KEYS=1 in_shell "$sh" "$SCRIPT" "$WORK/no-secret.tfvars")"
  assert_eq "$out" "$(printf '%s\n' 1 stale-access stale-secret)"

  # Without IIDP_STATE_TFVARS the file is found next to the script, however
  # it was sourced: the script is copied into a scratch infra/ directory and
  # sourced from / by absolute path. The file is left absent, so the path
  # the script resolved shows in its error without any tfvars file existing.
  t_start "$sh: without IIDP_STATE_TFVARS it reads state-bucket/terraform.tfvars next to itself"
  layout="$(scratch_dir)"
  mkdir -p "$layout/infra/state-bucket"
  cp "$SCRIPT" "$layout/infra/tofu-env.sh"
  out="$(in_shell "$sh" "$layout/infra/tofu-env.sh")"
  assert_contains "$(cat "$ERR_FILE")" "no such file: $(cd "$layout/infra" && pwd)/state-bucket/terraform.tfvars"
  t_start "$sh: and still fails cleanly when that file is absent"
  assert_eq "$out" "$(printf '%s\n' 1 unset unset)"

  # The README's own recipe: cd infra/platform, then source ../tofu-env.sh.
  t_start "$sh: sourced by relative path from infra/platform, as infra/README.md does, it finds the same file"
  mkdir -p "$layout/infra/platform"
  FROM_DIR="$layout/infra/platform" in_shell "$sh" ../tofu-env.sh >/dev/null
  assert_contains "$(cat "$ERR_FILE")" "no such file: $(cd "$layout/infra" && pwd)/state-bucket/terraform.tfvars"
done

t_start "running the script instead of sourcing it refuses and says to source it"
err="$("$SCRIPT" 2>&1)"
rc=$?
assert_eq "$rc" "2"
t_start "the refusal says to source it"
assert_contains "$err" "source this file instead of running it"

# ── summary ──────────────────────────────────────────────────────────────

printf '\n%s passed, %s failed\n' "$PASS" "$FAIL"
[[ "$FAIL" -eq 0 ]]
