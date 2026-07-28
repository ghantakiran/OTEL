#!/usr/bin/env bash
#
# Tests the Distribution Check's branching, with `docker` stubbed.
#
# The check itself is one `docker run` per compiled collector configuration, so
# almost all of it is branching: which distribution a file goes to, whether a
# refusal came from `validate` or from the collector failing to start, and whether
# a run that found nothing to check reads as a pass. None of that is reachable from
# `go test`, and all of it is what the check is FOR — a Distribution Check that
# exits 0 because it checked nothing is worse than no check, because CI is green.
#
# Same shape as gitops-rollout-pr_test.sh: a stub on PATH, one temporary fleet per
# case, and assertions on exit code and output rather than on internals.
#
# Creates no containers and pulls no images.

set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
under_test="$here/validate-compiled-configs.sh"

failed=0
ok() { printf '  \033[32mok\033[0m   %s\n' "$*"; }
bad() {
	printf '  \033[31mFAIL\033[0m %s\n' "$*"
	failed=$((failed + 1))
}
say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

# asserts that $output contains a needle, naming the case when it does not.
contains() {
	if printf '%s' "$output" | grep -qF -- "$1"; then
		ok "$2"
	else
		bad "$2 — output was:"
		printf '%s\n' "$output" | sed 's/^/       /'
	fi
}

missing() {
	if printf '%s' "$output" | grep -qF -- "$1"; then
		bad "$2 — output was:"
		printf '%s\n' "$output" | sed 's/^/       /'
	else
		ok "$2"
	fi
}

# ---------------------------------------------------------------------------
# The fixture: a fake fleet, a fake compiler, and a stubbed docker.
# ---------------------------------------------------------------------------

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

cat >"$work/collector-images.env" <<'EOF'
COLLECTOR_CORE_IMAGE=stub/collector:test
COLLECTOR_CONTRIB_IMAGE=stub/collector-contrib:test
EOF

# The stub. It answers the two shapes the check uses and records every call, so a
# test can assert WHICH distribution a file was handed to — the ADR 0014 line is
# the thing most likely to be got wrong silently.
#
#   docker run --rm ... IMAGE validate --config=...   -> a validation
#   docker run --rm --name N ... IMAGE --config=...   -> a start
#   docker rm -f N                                    -> teardown, always fine
#
# Behaviour is steered by files in $work, not by env vars, because the check runs
# the stub in a subshell and background job where exported state is easy to lose.
mkdir -p "$work/bin"
cat >"$work/bin/docker" <<'STUB'
#!/usr/bin/env bash
set -u
log="$STUB_DIR/calls.log"
printf '%s\n' "$*" >>"$log"

if [ "${1:-}" = "rm" ]; then exit 0; fi
if [ "${1:-}" != "run" ]; then exit 0; fi

# The mounted host path is the config under test; its basename is how a case
# names the file it wants to fail.
config=""
for arg in "$@"; do
	case "$arg" in
	*:/etc/otelcol/config.yaml:ro) config="$(basename "${arg%%:/etc/otelcol/config.yaml:ro}")" ;;
	esac
done

validating=false
for arg in "$@"; do [ "$arg" = "validate" ] && validating=true; done

if [ "$validating" = true ]; then
	if [ -e "$STUB_DIR/fail-validate-$config" ]; then
		echo "error decoding 'processors': unknown type: \"nonesuch\"" >&2
		exit 1
	fi
	exit 0
fi

# A start. Either it never becomes ready and exits (the grpc/protobuf shape), or
# it announces itself and runs until it is killed.
if [ -e "$STUB_DIR/never-ready-$config" ]; then
	echo "error: cannot start pipeline: invalid reader"
	exit 1
fi
echo "Everything is ready. Begin running and processing data."
sleep 60
STUB
chmod +x "$work/bin/docker"

# A fake compiler that writes a Gateway config, or refuses to.
cat >"$work/guardrail" <<'FAKE'
#!/usr/bin/env bash
set -u
[ "${1:-}" = "gateway" ] || { echo "unexpected subcommand: ${1:-}" >&2; exit 2; }
if [ -e "$STUB_DIR/fail-gateway-compile" ]; then
	echo "cannot compile the Gateway: it declares no self_telemetry backend" >&2
	exit 1
fi
cat <<'YAML'
extensions:
    file_storage/primary-apm:
        directory: /var/lib/otelcol/spill/primary-apm
service:
    pipelines:
        traces:
            receivers: [otlp]
            exporters: [otlp/primary-apm]
YAML
FAKE
chmod +x "$work/guardrail"

# A fleet with two compiled Agent configs in it.
fleet() {
	rm -rf "$work/fleet"
	mkdir -p "$work/fleet/compiled"
	printf 'service:\n    pipelines: {}\n' >"$work/fleet/compiled/orders-api.yaml"
	printf 'service:\n    pipelines: {}\n' >"$work/fleet/compiled/payments-api.yaml"
}

# run invokes the check with a clean stub state.
run() {
	rm -rf "$work/stub"
	mkdir -p "$work/stub"
	for marker in "$@"; do : >"$work/stub/$marker"; done
	output="$(
		PATH="$work/bin:$PATH" \
			STUB_DIR="$work/stub" \
			GUARDRAIL="$work/guardrail" \
			FLEET="$work/fleet" \
			IMAGES="$work/collector-images.env" \
			READY_TIMEOUT=5 \
			bash "$under_test" 2>&1
	)"
	status=$?
	calls="$(cat "$work/stub/calls.log" 2>/dev/null)"
}

# ---------------------------------------------------------------------------

say "Every compiled configuration is accepted and starts"
fleet
run
[ "$status" -eq 0 ] && ok "exits 0 when every configuration validates and starts" ||
	bad "exited $status when everything passed — output was: $output"
contains "orders-api.yaml" "names each Agent configuration it checked"
contains "gateway" "checks the Gateway too, which is compiled rather than committed"

say "The distribution a configuration is checked on is the one it runs on"
fleet
run
if printf '%s' "$calls" | grep -F "orders-api.yaml" | grep -qF "stub/collector:test"; then
	ok "an Agent configuration is checked on the CORE distribution (ADR 0014)"
else
	bad "an Agent configuration was not handed to the core image — calls were: $calls"
fi
if printf '%s' "$calls" | grep -F "gateway" | grep -qF "stub/collector-contrib:test"; then
	ok "the Gateway configuration is checked on the CONTRIB distribution (ADR 0014)"
else
	bad "the Gateway configuration was not handed to the contrib image — calls were: $calls"
fi
if printf '%s' "$calls" | grep -F "orders-api.yaml" | grep -qF "stub/collector-contrib:test"; then
	bad "an Agent configuration was also checked on contrib, so a contrib-only component in an Agent would pass"
else
	ok "no Agent configuration is checked on contrib, so ADR 0014's line is what is being run"
fi

say "A configuration the distribution rejects fails the check"
fleet
run "fail-validate-payments-api.yaml"
[ "$status" -ne 0 ] && ok "exits non-zero when a configuration does not validate" ||
	bad "a configuration that failed validation exited 0"
contains "payments-api.yaml" "names the configuration that was rejected"
contains "unknown type" "prints the collector's own reason rather than a summary of it"
contains "gateway.yaml" "keeps going after a failure, so one run reports every configuration rather than the first"

say "A configuration that validates and then will not start fails the check"
fleet
run "never-ready-orders-api.yaml"
[ "$status" -ne 0 ] && ok "exits non-zero when a configuration validates and does not start" ||
	bad "a configuration that never started exited 0"
contains "did not start" "says the failure was at start-up, not at validation"
contains "invalid reader" "prints what the collector said on the way down"

say "A run that checks nothing is not a pass"
rm -rf "$work/fleet"
mkdir -p "$work/fleet/compiled"
run
[ "$status" -ne 0 ] && ok "exits non-zero when the Fleet has no compiled configuration at all" ||
	bad "a Fleet with nothing in it passed the Distribution Check"
contains "no compiled" "says why, rather than reporting a green run over an empty set"

say "A Gateway that does not compile fails the check"
fleet
run "fail-gateway-compile"
[ "$status" -ne 0 ] && ok "exits non-zero when the Gateway cannot be compiled to check" ||
	bad "a Gateway that would not compile passed the Distribution Check"
contains "self_telemetry" "prints the compiler's reason"

if [ "$failed" -eq 0 ]; then
	printf '\n\033[32mThe Distribution Check behaves as specified.\033[0m\n'
	exit 0
fi
printf '\n\033[31m%d assertion(s) failed.\033[0m\n' "$failed"
exit 1
