#!/usr/bin/env bash
#
# Exercises waiver-expiry-issue.sh against real `otel-guardrail waivers` output,
# with `gh` stubbed, to prove the open-vs-update-vs-close branching. GitHub
# Actions cannot run on this account, so this is the only thing standing between
# the cron and a repository full of duplicate issues.
#
# No network, no `gh` authentication and no issue is ever created: the stub
# records what would have been called.
#
#   .github/scripts/waiver-expiry-issue_test.sh

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="$repo_root/.github/scripts/waiver-expiry-issue.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

failures=0

# --- the stub -----------------------------------------------------------------
# Records every `gh` invocation, answers reads from STUB_* environment, and
# writes the body it was handed to $GH_BODY so a test can inspect it.
mkdir -p "$work/bin"
cat >"$work/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
echo "gh $*" >>"$GH_LOG"

# Capture --body-file's contents, whatever subcommand carried it.
previous=""
for arg in "$@"; do
	if [ "$previous" = "--body-file" ]; then cp "$arg" "$GH_BODY"; fi
	previous="$arg"
done

case "${1:-} ${2:-}" in
"issue list")
	# The real command is filtered through --jq '.[].number', so emit numbers.
	if [ -n "${STUB_ISSUE_NUMBERS:-}" ]; then printf '%s\n' $STUB_ISSUE_NUMBERS; fi
	;;
"issue view")
	printf '%s\n' "${STUB_ISSUE_BODY:-}"
	;;
esac
exit 0
STUB
chmod +x "$work/bin/gh"
export PATH="$work/bin:$PATH"

# --- fixtures: real reports from the real register ----------------------------
go build -o "$work/otel-guardrail" "$repo_root/guardrail/cmd/otel-guardrail" || {
	echo "FAIL: could not build otel-guardrail"
	exit 1
}

# 2026-07-25: legacy-payments-batch lapsed on 2026-01-15, legacy-inventory goes
# on 2027-04-01 — one on each path.
"$work/otel-guardrail" waivers --as-of 2026-07-25 --within 365 --format json >"$work/attention.json"
# 2025-01-01: both Waivers are comfortably in the future.
"$work/otel-guardrail" waivers --as-of 2025-01-01 --within 30 --format json >"$work/all-clear.json"

if [ "$(jq '.expired | length' "$work/attention.json")" != "1" ] ||
	[ "$(jq '.expiring | length' "$work/attention.json")" != "1" ]; then
	echo "FAIL: the fixture report is not the one/one split these tests assume"
	exit 1
fi
if [ "$(jq '.expired + .expiring | length' "$work/all-clear.json")" != "0" ]; then
	echo "FAIL: the all-clear fixture is not all-clear"
	exit 1
fi

# --- harness ------------------------------------------------------------------

# run_job REPORT [STUB_ISSUE_NUMBERS] [STUB_ISSUE_BODY]
# Runs the script with a fresh log; sets $code, $log, $body, $output.
run_job() {
	GH_LOG="$work/gh.log"
	GH_BODY="$work/gh-body.md"
	: >"$GH_LOG"
	: >"$GH_BODY"
	export GH_LOG GH_BODY
	export STUB_ISSUE_NUMBERS="${2:-}"
	export STUB_ISSUE_BODY="${3:-}"

	output="$(REPORT="$1" LABEL=waiver-expiry GITHUB_STEP_SUMMARY="$work/summary.md" bash "$script" 2>&1)"
	code=$?
	log="$(cat "$GH_LOG")"
	body="$(cat "$GH_BODY")"
}

expect() {
	local description="$1" condition="$2"
	if eval "$condition"; then
		echo "ok   - $description"
	else
		echo "FAIL - $description"
		echo "       condition: $condition"
		echo "       gh calls:"
		sed 's/^/         /' <<<"$log"
		echo "       output: $output"
		failures=$((failures + 1))
	fi
}

calls() { grep -c "^gh $1" <<<"$log" || true; }

# --- the cases ----------------------------------------------------------------

echo "# Waivers need attention and no tracking issue is open"
run_job "$work/attention.json" ""
expect "opens exactly one issue" '[ "$(calls "issue create")" = "1" ]'
expect "does not edit anything" '[ "$(calls "issue edit")" = "0" ]'
expect "does not close anything" '[ "$(calls "issue close")" = "0" ]'
expect "labels it so tomorrow's run can find it" 'grep -q -- "--label waiver-expiry" <<<"$log"'
expect "makes sure the label exists first" '[ "$(calls "label create")" = "1" ]'
expect "the body carries the marker" 'grep -qF -- "<!-- otel-guardrail:waiver-expiry -->" <<<"$body"'
expect "the body names the expired Waiver" 'grep -q "legacy-payments-batch" <<<"$body"'
expect "the body names the expiring Waiver" 'grep -q "legacy-inventory" <<<"$body"'
expect "the body separates expired from expiring" '[ "$(grep -c "^## " <<<"$body")" = "2" ]'
expect "the body mentions the approver" 'grep -q "@obs-team" <<<"$body"'
expect "the body names the Standard and the expiry date" 'grep -q "2026-01-15" <<<"$body" && grep -q "| S1 |" <<<"$body"'
expect "the run summary gets the report too" 'grep -q "legacy-payments-batch" "$work/summary.md"'
expect "succeeds" '[ "$code" = "0" ]'

# Keep the body this run would have filed: the next case replays it.
filed_body="$body"

echo
echo "# Waivers need attention and the tracking issue is open with a stale body"
run_job "$work/attention.json" "42" "<!-- otel-guardrail:waiver-expiry -->
yesterday's report, which said something else"
expect "edits the open issue" '[ "$(calls "issue edit 42")" = "1" ]'
expect "does not open a second issue" '[ "$(calls "issue create")" = "0" ]'
expect "does not close it" '[ "$(calls "issue close")" = "0" ]'
expect "succeeds" '[ "$code" = "0" ]'

echo
echo "# Waivers need attention and the tracking issue already says exactly this"
run_job "$work/attention.json" "42" "$filed_body"
expect "writes nothing at all" '[ "$(calls "issue edit")" = "0" ] && [ "$(calls "issue create")" = "0" ]'
expect "does not close it either" '[ "$(calls "issue close")" = "0" ]'
expect "says why it did nothing" 'grep -q "already says exactly this" <<<"$output"'
expect "succeeds" '[ "$code" = "0" ]'

echo
echo "# Nothing needs attention and a tracking issue is open"
run_job "$work/all-clear.json" "42" "<!-- otel-guardrail:waiver-expiry -->
1 Waiver needed attention yesterday"
expect "closes the issue" '[ "$(calls "issue close 42")" = "1" ]'
expect "says why, on the issue" 'grep -q -- "--comment" <<<"$log"'
expect "does not open anything" '[ "$(calls "issue create")" = "0" ]'
expect "succeeds" '[ "$code" = "0" ]'

echo
echo "# Nothing needs attention and no tracking issue is open"
run_job "$work/all-clear.json" ""
expect "opens nothing" '[ "$(calls "issue create")" = "0" ]'
expect "closes nothing" '[ "$(calls "issue close")" = "0" ]'
expect "edits nothing" '[ "$(calls "issue edit")" = "0" ]'
expect "succeeds" '[ "$code" = "0" ]'

echo
echo "# A human's issue happens to carry the label"
run_job "$work/attention.json" "77" "I labelled this myself while triaging."
expect "refuses to overwrite it" '[ "$(calls "issue edit")" = "0" ]'
expect "does not open a duplicate either" '[ "$(calls "issue create")" = "0" ]'
expect "fails loudly rather than silently" '[ "$code" != "0" ] && grep -q "not filed by this job" <<<"$output"'

echo
echo "# Two open issues carry the label"
run_job "$work/attention.json" "42 43"
expect "writes nothing" '[ "$(calls "issue create")" = "0" ] && [ "$(calls "issue edit")" = "0" ] && [ "$(calls "issue close")" = "0" ]'
expect "says which way to fix it" '[ "$code" != "0" ] && grep -q "cannot tell which one to update" <<<"$output"'

echo
if [ "$failures" -eq 0 ]; then
	echo "all cases pass"
else
	echo "$failures assertion(s) failed"
fi
exit "$failures"
