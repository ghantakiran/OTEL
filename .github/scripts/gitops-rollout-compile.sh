#!/usr/bin/env bash
#
# Recompiles the Fleet and captures the JSON report the rollout job reads.
#
# This is the first half of .github/workflows/gitops-rollout.yml, in a script so it
# can be tested — see gitops-rollout-compile_test.sh. The decision it makes is small
# and entirely about `compile-fleet`'s exit code, which is the whole contract:
#
#   0  every Telemetry Contract compiled            -> report it, carry on
#   1  some did not, the rest were rolled out       -> report it, warn, carry on
#   2  the compiler could not run                   -> fail, propose nothing
#
# Exit 2 is not a partial rollout, it is no rollout: the Fleet directory was
# unreadable, or a Pipeline Profile is broken, or a compiled config came out
# incoherent. Committing whatever got written, or reading an empty report as "the
# fleet compiles to nothing", would put a half-compiled fleet in front of a reviewer
# as though it were the fleet.
#
# Environment:
#   GUARDRAIL  Path to the otel-guardrail binary. Default: otel-guardrail on PATH.
#   FLEET      The Fleet directory. Default: fleet.
#   REPORT     Where to write the JSON report. Required.
#   ERRORS     Where to keep the compiler's stderr. Default: alongside REPORT.

set -uo pipefail

GUARDRAIL="${GUARDRAIL:-otel-guardrail}"
FLEET="${FLEET:-fleet}"
REPORT="${REPORT:?REPORT must be the path to write the JSON rollout report to}"
ERRORS="${ERRORS:-${REPORT}.err}"

code=0
"$GUARDRAIL" compile-fleet --format json "$FLEET" >"$REPORT" 2>"$ERRORS" || code=$?

if [ "$code" -ne 0 ] && [ "$code" -ne 1 ]; then
	echo "::error title=The Fleet could not be compiled::$(tr '\n' ' ' <"$ERRORS")"
	cat "$ERRORS" >&2
	exit 1
fi

# A partial rollout is a normal, reportable outcome rather than a broken run: the
# Contracts that did compile still roll out, and the pull request names the ones that
# did not, with the reason. The warning is so the run itself says so too.
if [ "$code" -eq 1 ]; then
	echo "::warning title=Some Telemetry Contracts did not compile::$(jq -r '.not_compiled[] | "\(.service_name): \(.reason)"' "$REPORT" | tr '\n' ' ')"
fi

echo "compiled the Fleet: $(jq '.compiled | length' "$REPORT") service(s) compiled, $(jq '.not_compiled | length' "$REPORT") did not"

if [ -n "${GITHUB_ENV:-}" ]; then
	echo "REPORT=$REPORT" >>"$GITHUB_ENV"
fi
