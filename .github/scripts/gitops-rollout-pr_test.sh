#!/usr/bin/env bash
#
# Exercises gitops-rollout-pr.sh against a real `otel-guardrail compile-fleet
# --format json` report, in a throwaway git repository with a local bare remote and
# `gh` stubbed, to prove the open-vs-update-vs-close-vs-do-nothing branching.
#
# GitHub Actions cannot run on this account, so this is the only thing standing
# between the rollout cron and a repository full of duplicate pull requests.
#
# No network, no `gh` authentication, and no pull request is ever created: the stub
# records what would have been called. The git work is real, against a bare
# repository in a temporary directory.
#
#   .github/scripts/gitops-rollout-pr_test.sh

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="$repo_root/.github/scripts/gitops-rollout-pr.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

failures=0

# --- the stub -----------------------------------------------------------------
# Records every `gh` invocation, answers reads from STUB_* environment, and writes
# the body it was handed to $GH_BODY so a test can inspect it.
mkdir -p "$work/bin"
cat >"$work/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
echo "gh $*" >>"$GH_LOG"

previous=""
for arg in "$@"; do
	if [ "$previous" = "--body-file" ]; then cp "$arg" "$GH_BODY"; fi
	previous="$arg"
done

case "${1:-} ${2:-}" in
"pr list")
	# The real command is filtered through --jq '.[].number', so emit numbers.
	if [ -n "${STUB_PR_NUMBERS:-}" ]; then printf '%s\n' $STUB_PR_NUMBERS; fi
	;;
"pr view")
	printf '%s\n' "${STUB_PR_BODY:-}"
	;;
esac
exit 0
STUB
chmod +x "$work/bin/gh"
export PATH="$work/bin:$PATH"

# --- a repository with a fleet in it ------------------------------------------

go build -o "$work/otel-guardrail" "$repo_root/guardrail/cmd/otel-guardrail" || {
	echo "FAIL: could not build otel-guardrail"
	exit 1
}

git init --quiet --bare "$work/origin.git"
mkdir -p "$work/repo"
cd "$work/repo" || exit 1
git init --quiet -b master .
git config user.name "test"
git config user.email "test@example.com"
git remote add origin "$work/origin.git"

# The real sample fleet, so this test reads the same Contracts, the same layout and
# the same report schema the workflow will.
cp -R "$repo_root/fleet" "$work/repo/fleet"

# The report the workflow captures. Exit 1 is expected: the sample fleet has one
# tier-2 Contract with no Pipeline Profile yet, which is exactly the partial rollout
# this job has to present honestly.
report="$work/rollout.json"
"$work/otel-guardrail" compile-fleet --format json fleet >"$report"
code=$?
if [ "$code" -gt 1 ]; then
	echo "FAIL: compile-fleet could not run (exit $code)"
	exit 1
fi
if [ "$(jq '.compiled | length' "$report")" -lt 1 ]; then
	echo "FAIL: the sample fleet compiled nothing, so this test proves nothing"
	exit 1
fi

git add -A
git commit --quiet -m "the fleet, already compiled"
git push --quiet -u origin master

# --- harness ------------------------------------------------------------------

# reset_repo puts the working tree back to a pushed, fully-compiled master.
reset_repo() {
	git switch --quiet --force master
	git reset --quiet --hard origin/master
	git clean --quiet -fd
}

# change_the_fleet edits a Contract and recompiles, which is what a real Contract
# change looks like by the time this script runs.
change_the_fleet() {
	sed -i.bak 's/service.version: "5.1.0"/service.version: "5.2.0"/' fleet/contracts/orders-api.yaml
	rm -f fleet/contracts/orders-api.yaml.bak
	"$work/otel-guardrail" compile-fleet --format json fleet >"$report"
}

# run_job [STUB_PR_NUMBERS] [STUB_PR_BODY]; sets $code, $log, $body, $output.
run_job() {
	GH_LOG="$work/gh.log"
	GH_BODY="$work/gh-body.md"
	: >"$GH_LOG"
	: >"$GH_BODY"
	export GH_LOG GH_BODY
	export STUB_PR_NUMBERS="${1:-}"
	export STUB_PR_BODY="${2:-}"

	output="$(REPORT="$report" FLEET=fleet BRANCH=gitops/fleet-rollout LABEL=fleet-rollout \
		GITHUB_STEP_SUMMARY="$work/summary.md" bash "$script" 2>&1)"
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
pushed() { [ -n "$(git ls-remote --heads origin gitops/fleet-rollout)" ]; }

# remote_tip is the commit the rollout branch points at on the remote, or nothing.
# A force-push is observable here even when it carries identical files: the rebuilt
# commit has a later committer date and therefore a different hash.
remote_tip() { git ls-remote --heads origin gitops/fleet-rollout | cut -f1; }

# --- the cases ----------------------------------------------------------------

echo "# The compiled tree already matches the Contracts and no rollout PR is open"
reset_repo
run_job ""
expect "opens nothing" '[ "$(calls "pr create")" = "0" ]'
expect "edits nothing" '[ "$(calls "pr edit")" = "0" ]'
expect "closes nothing" '[ "$(calls "pr close")" = "0" ]'
expect "pushes nothing" '! pushed'
expect "says why it did nothing" 'grep -q "already matches" <<<"$output"'
expect "succeeds" '[ "$code" = "0" ]'

echo
echo "# The compiled tree already matches the Contracts and a rollout PR is open"
reset_repo
run_job "42" "<!-- otel-guardrail:fleet-rollout -->
yesterday there was something to roll out"
expect "closes the pull request" '[ "$(calls "pr close 42")" = "1" ]'
expect "says why, on the pull request" 'grep -q -- "--comment" <<<"$log"'
expect "opens nothing" '[ "$(calls "pr create")" = "0" ]'
expect "succeeds" '[ "$code" = "0" ]'

echo
echo "# A Contract changed and no rollout PR is open"
reset_repo
change_the_fleet
run_job ""
expect "opens exactly one pull request" '[ "$(calls "pr create")" = "1" ]'
expect "pushes the compiled tree" 'pushed'
expect "edits nothing" '[ "$(calls "pr edit")" = "0" ]'
expect "closes nothing" '[ "$(calls "pr close")" = "0" ]'
expect "labels it so tomorrow's run can find it" 'grep -q -- "--label fleet-rollout" <<<"$log"'
expect "makes sure the label exists first" '[ "$(calls "label create")" = "1" ]'
expect "targets the branch it pushed" 'grep -q -- "--head gitops/fleet-rollout" <<<"$log"'
expect "the body carries the marker" 'grep -qF -- "<!-- otel-guardrail:fleet-rollout -->" <<<"$body"'
expect "the body names the changed compiled file" 'grep -q "compiled/orders-api.yaml" <<<"$body"'
expect "the body says merging it is the rollout" 'grep -qi "merging this" <<<"$body"'
expect "the body names the service that did not compile" 'grep -q "reporting-worker" <<<"$body"'
expect "the body says why it did not compile" 'grep -q "Pipeline Profile" <<<"$body"'
expect "the body says how to reproduce it" 'grep -q "compile-fleet fleet" <<<"$body"'
expect "the run summary gets the rollout too" 'grep -q "compiled/orders-api.yaml" "$work/summary.md"'
expect "the commit is attributed to the job, not to a person" '[ "$(git log -1 --format=%an)" = "otel-guardrail" ]'
expect "succeeds" '[ "$code" = "0" ]'

# Keep the body this run would have filed: the next cases replay it.
proposed_body="$body"

echo
echo "# The same Contract change, and the open PR already proposes exactly this"
reset_repo
change_the_fleet
tip_before="$(remote_tip)"
run_job "42" "$proposed_body"
expect "writes nothing at all" '[ "$(calls "pr create")" = "0" ] && [ "$(calls "pr edit")" = "0" ]'
expect "does not close it either" '[ "$(calls "pr close")" = "0" ]'
expect "leaves the pushed branch exactly as it was" '[ -n "$tip_before" ] && [ "$(remote_tip)" = "$tip_before" ]'
expect "says it had no need to push" 'grep -q "not pushing" <<<"$output"'
expect "says why it wrote no pull request" 'grep -q "left untouched" <<<"$output"'
expect "succeeds" '[ "$code" = "0" ]'

echo
echo "# A Contract change, and the open PR proposes something else"
reset_repo
change_the_fleet
run_job "42" "<!-- otel-guardrail:fleet-rollout -->
an older rollout, which said something else"
expect "updates the open pull request" '[ "$(calls "pr edit 42")" = "1" ]'
expect "does not open a second one" '[ "$(calls "pr create")" = "0" ]'
expect "does not close it" '[ "$(calls "pr close")" = "0" ]'
expect "succeeds" '[ "$code" = "0" ]'

echo
echo "# A human's pull request happens to carry the label"
reset_repo
change_the_fleet
run_job "77" "I labelled this myself while triaging."
expect "refuses to overwrite it" '[ "$(calls "pr edit")" = "0" ]'
expect "does not open a duplicate either" '[ "$(calls "pr create")" = "0" ]'
expect "fails loudly rather than silently" '[ "$code" != "0" ] && grep -q "not filed by this job" <<<"$output"'

echo
echo "# Two open pull requests carry the label"
reset_repo
change_the_fleet
run_job "42 43"
expect "writes nothing" '[ "$(calls "pr create")" = "0" ] && [ "$(calls "pr edit")" = "0" ] && [ "$(calls "pr close")" = "0" ]'
expect "says which way to fix it" '[ "$code" != "0" ] && grep -q "cannot tell which one to update" <<<"$output"'

echo
echo "# A service leaves the Fleet"
reset_repo
git rm --quiet fleet/contracts/payments-api.yaml
"$work/otel-guardrail" compile-fleet --format json fleet >"$report"
run_job ""
expect "prunes its collector configuration" '[ ! -f fleet/compiled/payments-api.yaml ]'
expect "opens a pull request for the removal" '[ "$(calls "pr create")" = "1" ]'
expect "the body says the file was removed and why" 'grep -q "removed — no Telemetry Contract accounts for it" <<<"$body"'
expect "the body names the pruned file" 'grep -q "compiled/payments-api.yaml" <<<"$body"'
expect "succeeds" '[ "$code" = "0" ]'

echo
if [ "$failures" -eq 0 ]; then
	echo "all cases pass"
else
	echo "$failures assertion(s) failed"
fi
exit "$failures"
