#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="$root/scripts/require-released-kandev-sdk-contract.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fake_git="$tmp/git"
cat >"$fake_git" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$GIT_LOG"
case "$*" in
  *"ls-remote"*) test "${GIT_SCENARIO:-valid}" != "missing-tag" ;;
  *"merge-base --is-ancestor"*) test "${GIT_SCENARIO:-valid}" != "missing-contract" ;;
esac
EOF
chmod +x "$fake_git"

# These stand in for release metadata and refs. The preflight has no mutation
# path; the invalid cases below prove their bytes remain untouched.
release_state="$tmp/release-state"
printf 'manifest=0.3.0\nrefs=v0.3.0\n' >"$release_state"
assert_state_unchanged() {
  test "$(cat "$release_state")" = $'manifest=0.3.0\nrefs=v0.3.0' || {
    echo "preflight changed release state" >&2
    exit 1
  }
}

run_case() {
  local name="$1" expected="$2"
  shift 2
  : >"$tmp/git.log"
  if GIT_BIN="$fake_git" GIT_LOG="$tmp/git.log" "$@" "$script"; then
    actual=0
  else
    actual=$?
  fi
  test "$actual" = "$expected" || { echo "$name: expected exit $expected, got $actual" >&2; exit 1; }
}

env_base=(env SDK_REPOSITORY=kdlbs/kandev SDK_REF=v0.94.0 REQUIRED_CONTRACT_COMMIT=5f5b1f0f319e688c7aa4f161c30e0dd024d3f1eb)

run_case invalid-repository 1 env SDK_REPOSITORY=yattdev/kandev SDK_REF=v0.94.0 REQUIRED_CONTRACT_COMMIT=5f5b1f0f319e688c7aa4f161c30e0dd024d3f1eb
test ! -s "$tmp/git.log" || { echo "invalid repository contacted git" >&2; exit 1; }
assert_state_unchanged

run_case raw-sha 1 env SDK_REPOSITORY=kdlbs/kandev SDK_REF=5f5b1f0f319e688c7aa4f161c30e0dd024d3f1eb REQUIRED_CONTRACT_COMMIT=5f5b1f0f319e688c7aa4f161c30e0dd024d3f1eb
test ! -s "$tmp/git.log" || { echo "raw SHA contacted git" >&2; exit 1; }
assert_state_unchanged

run_case missing-tag 1 env GIT_SCENARIO=missing-tag SDK_REPOSITORY=kdlbs/kandev SDK_REF=v0.94.0 REQUIRED_CONTRACT_COMMIT=5f5b1f0f319e688c7aa4f161c30e0dd024d3f1eb
grep -q 'ls-remote' "$tmp/git.log"
test "$(wc -l <"$tmp/git.log")" -eq 1 || { echo "missing tag continued after lookup" >&2; exit 1; }
assert_state_unchanged

run_case missing-contract 1 env GIT_SCENARIO=missing-contract SDK_REPOSITORY=kdlbs/kandev SDK_REF=v0.94.0 REQUIRED_CONTRACT_COMMIT=5f5b1f0f319e688c7aa4f161c30e0dd024d3f1eb
grep -q 'merge-base --is-ancestor' "$tmp/git.log"
assert_state_unchanged

run_case valid-tag 0 "${env_base[@]}"
grep -q 'merge-base --is-ancestor' "$tmp/git.log"

preflight_line="$(grep -n 'Require a released upstream SDK contract' "$root/.github/workflows/release.yml" | head -1 | cut -d: -f1)"
version_line="$(grep -n 'Compute next release version' "$root/.github/workflows/release.yml" | head -1 | cut -d: -f1)"
test "$preflight_line" -lt "$version_line" || {
  echo "preflight must run before release version calculation" >&2
  exit 1
}

echo "release preflight tests passed"
