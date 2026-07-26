#!/usr/bin/env bash
#
# Exercises gitops-rollout-compile.sh against the real otel-guardrail, to prove the
# one decision it makes: a Fleet that partly compiles is a reportable rollout, and a
# Fleet the compiler could not read at all is a failed run that proposes nothing.
#
# GitHub Actions cannot run on this account, so without this the difference between
# "one Contract is broken" and "the compiler is broken" is untested — and getting it
# wrong means committing a half-compiled fleet as though it were the fleet.
#
#   .github/scripts/gitops-rollout-compile_test.sh

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="$repo_root/.github/scripts/gitops-rollout-compile.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

failures=0

go build -o "$work/otel-guardrail" "$repo_root/guardrail/cmd/otel-guardrail" || {
	echo "FAIL: could not build otel-guardrail"
	exit 1
}

expect() {
	local description="$1" condition="$2"
	if eval "$condition"; then
		echo "ok   - $description"
	else
		echo "FAIL - $description"
		echo "       condition: $condition"
		echo "       output: $output"
		failures=$((failures + 1))
	fi
}

# run_step FLEET; sets $code, $output, $report, $env_file.
run_step() {
	report="$work/report.json"
	env_file="$work/github-env"
	: >"$env_file"
	rm -f "$report"

	output="$(GUARDRAIL="$work/otel-guardrail" FLEET="$1" REPORT="$report" \
		GITHUB_ENV="$env_file" bash "$script" 2>&1)"
	code=$?
}

# Always a copy of the sample fleet, never the repository's own: compile-fleet
# writes into the directory it is given, and a test must not edit the artefact it
# is testing against.
cp -R "$repo_root/fleet" "$work/fleet"

echo "# A Fleet in which every Telemetry Contract compiles"
# The sample fleet minus the one Contract with no Pipeline Profile yet.
cp -R "$work/fleet" "$work/whole-fleet"
rm -f "$work/whole-fleet/contracts/reporting-worker.yaml"
run_step "$work/whole-fleet"
expect "succeeds, so the rollout goes ahead" '[ "$code" = "0" ]'
expect "captures a report" '[ -s "$report" ]'
expect "reports nothing failed" '[ "$(jq ".not_compiled | length" "$report")" = "0" ]'
expect "warns about nothing" '! grep -q "::warning" <<<"$output"'
expect "hands the report path to the next step" 'grep -q "^REPORT=$report$" "$env_file"'

echo
echo "# A Fleet where one Telemetry Contract does not compile"
# The whole sample fleet: reporting-worker is tier-2, which has no Profile yet.
run_step "$work/fleet"
expect "still succeeds, so the rest of the fleet still rolls out" '[ "$code" = "0" ]'
expect "the report names the service that did not compile" 'grep -q "reporting-worker" "$report"'
expect "the report still names the ones that did" '[ "$(jq ".compiled | length" "$report")" -ge 1 ]'
expect "says so on the run itself, rather than passing silently" 'grep -q "::warning" <<<"$output"'
expect "the warning says which service and why" 'grep -q "reporting-worker" <<<"$output" && grep -q "Pipeline Profile" <<<"$output"'
expect "hands the report on" 'grep -q "^REPORT=$report$" "$env_file"'

echo
echo "# A Fleet the compiler cannot read at all"
run_step "$work/there-is-no-fleet-here"
expect "fails the run rather than proposing an empty rollout" '[ "$code" != "0" ]'
expect "says so as an error, not a warning" 'grep -q "::error" <<<"$output"'
expect "names what went wrong" 'grep -q "no Telemetry Contracts" <<<"$output"'
expect "hands no report to the next step" '! grep -q "^REPORT=" "$env_file"'

echo
if [ "$failures" -eq 0 ]; then
	echo "all cases pass"
else
	echo "$failures assertion(s) failed"
fi
exit "$failures"
