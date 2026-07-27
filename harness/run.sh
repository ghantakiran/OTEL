#!/usr/bin/env bash
#
# Stands up the two-tier topology and asserts OTLP flows
# service -> Agent -> Gateway -> Backend.
#
# Everything the Agent and the Gateway run is COMPILED here, first, by the binary
# under test. A harness that proved a hand-written config works would prove nothing
# about the compiler.
#
# What it asserts, in order:
#
#   1. NEGATIVE CONTROL. With the Gateway not running, a span emitted to the Agent
#      does not reach the Backend. This is what makes the positive case mean
#      something: it rules out the Agent having a path of its own.
#   2. The span emitted after the Gateway starts arrives at the Backend, by trace
#      ID, having crossed two collectors it was never told about.
#   3. What arrives carries the TELEMETRY CONTRACT's service.name, not the one the
#      sample service sent. The compiled Agent config is the only thing that could
#      have made that substitution, so it ran.
#
# Needs: docker, docker compose, go. Takes about a minute.
# Read docs/agent-gateway-topology.md for what this does and does not prove.
#
# Usage: bash harness/run.sh [--keep]
#   --keep  leave the containers running afterwards, for poking at.

set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$here/.." && pwd)"
generated="$here/generated"

keep=false
[ "${1:-}" = "--keep" ] && keep=true

compose() { docker compose --project-directory "$here" -f "$here/docker-compose.yaml" "$@"; }

say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok() { printf '  \033[32mok\033[0m   %s\n' "$*"; }
bad() {
	printf '  \033[31mFAIL\033[0m %s\n' "$*"
	failed=$((failed + 1))
}

failed=0
cleanup() {
	if [ "$keep" = true ]; then
		printf '\nContainers left running. Tear them down with:\n  docker compose --project-directory %s -f %s down -v\n' "$here" "$here/docker-compose.yaml"
		return
	fi
	say "Tearing down"
	compose down -v --remove-orphans >/dev/null 2>&1
}
trap cleanup EXIT

# 32 and 16 hex characters, the widths OTLP wants for a trace and a span ID.
hexrand() { head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'; }

# emit writes a span payload with the given trace ID and name, and posts it to the
# Agent from a container that knows only where its Agent is.
emit() {
	local trace_id="$1"
	local span_name="$2"
	local now_nanos
	now_nanos="$(($(date +%s) * 1000000000))"

	sed -e "s/__TRACE_ID__/$trace_id/" \
		-e "s/__SPAN_ID__/$(hexrand 8)/" \
		-e "s/__SPAN_NAME__/$span_name/" \
		-e "s/__START_NANOS__/$now_nanos/" \
		-e "s/__END_NANOS__/$((now_nanos + 1000000))/" \
		"$here/span.json.template" >"$generated/span.json"

	if ! compose run --rm sample-service >"$generated/emit.log" 2>&1; then
		# Worth distinguishing loudly: a span the Agent never accepted would otherwise
		# read below as a span the Gateway failed to relay.
		echo "the sample service could not POST OTLP to its Agent:"
		sed 's/^/    /' "$generated/emit.log"
		exit 2
	fi
}

# arrived polls the Backend's log for a string, up to a deadline.
arrived() {
	local needle="$1"
	local seconds="$2"
	local deadline=$((SECONDS + seconds))
	while [ "$SECONDS" -lt "$deadline" ]; do
		if compose logs backend 2>&1 | grep -q -- "$needle"; then return 0; fi
		sleep 2
	done
	return 1
}

ready() {
	local service="$1"
	local deadline=$((SECONDS + 60))
	while [ "$SECONDS" -lt "$deadline" ]; do
		if compose logs "$service" 2>&1 | grep -q "Everything is ready"; then return 0; fi
		sleep 1
	done
	return 1
}

# ---------------------------------------------------------------- compile ----

say "Compiling the Agent and the Gateway"
mkdir -p "$generated"
guardrail="$generated/otel-guardrail"

if ! (cd "$repo_root" && go build -o "$guardrail" ./guardrail/cmd/otel-guardrail); then
	echo "could not build otel-guardrail"
	exit 2
fi

# The Agent's config: one service's Telemetry Contract, compiled with its Service
# Tier's Pipeline Profile.
if ! (cd "$repo_root" && "$guardrail" compile guardrail/examples/compliant-contract.yaml) >"$generated/agent.yaml"; then
	echo "could not compile the Agent config"
	exit 2
fi
ok "Agent config compiled from guardrail/examples/compliant-contract.yaml"

# The Gateway's config: the org's one Gateway Declaration. No Contract involved —
# there is one Gateway and it is nobody's service.
if ! (cd "$repo_root" && "$guardrail" gateway) >"$generated/gateway.yaml"; then
	echo "could not compile the Gateway config"
	exit 2
fi
ok "Gateway config compiled from controlplane/gateway.yaml"

contract_service_name="$(grep -A1 'key: service.name' "$generated/agent.yaml" | grep 'value:' | head -1 | sed 's/.*value: //')"
if [ -z "$contract_service_name" ]; then
	echo "the compiled Agent config stamps no service.name; the harness has nothing to assert on"
	exit 2
fi
ok "the Contract's service.name is $contract_service_name"

# ------------------------------------------------------- negative control ----

say "Negative control: the Agent alone cannot reach the Backend"
compose down -v --remove-orphans >/dev/null 2>&1
if ! compose up -d backend agent >/dev/null 2>&1; then
	echo "could not start the Agent and the Backend"
	exit 2
fi
ready backend || {
	echo "the Backend never became ready"
	exit 2
}

orphan_trace="$(hexrand 16)"
emit "$orphan_trace" "harness-with-no-gateway"

if arrived "$orphan_trace" 20; then
	bad "a span reached the Backend with no Gateway running — the Agent has a path of its own, so nothing below proves the Gateway relayed anything"
else
	ok "nothing reached the Backend while the Gateway was down"
fi

# ---------------------------------------------------------- end to end ----

# The negative control leaves the Agent mid-retry with the orphan span still queued
# — tier-1's Profile says retry, and it is doing exactly that. Replacing the Agent
# before the Gateway exists discards both, so what the Backend prints below can only
# have come from the span emitted after this point, and the Agent starts the real
# test with a clean export backoff rather than a growing one.
say "Replacing the Agent, so nothing from the control run can arrive"
if ! compose up -d --force-recreate agent >/dev/null 2>&1; then
	echo "could not replace the Agent"
	exit 2
fi

say "Starting the Gateway"
if ! compose up -d gateway >/dev/null 2>&1; then
	echo "could not start the Gateway"
	exit 2
fi
ready gateway || {
	echo "the Gateway never became ready — its compiled config may not be one this collector accepts"
	compose logs gateway 2>&1 | tail -30
	exit 2
}
ok "the Gateway started from its compiled config"

say "service -> Agent -> Gateway -> Backend"
trace="$(hexrand 16)"
emit "$trace" "harness-end-to-end"
ok "the sample service emitted one span to its Agent, naming no Backend"

if arrived "$trace" 120; then
	ok "the span arrived at the Backend, by trace ID $trace"
else
	bad "the span never reached the Backend (trace ID $trace)"
	compose logs 2>&1 | tail -40
fi

if arrived "$contract_service_name" 5; then
	ok "it carries service.name=$contract_service_name — from the Telemetry Contract, stamped by the compiled Agent config"
else
	bad "the Contract's service.name never reached the Backend, so the compiled resource processor did not run"
fi

if arrived "harness-sample-service" 5; then
	bad "the sample service's own service.name reached the Backend; the Contract's value did not replace it"
else
	ok "the sample service's own service.name did not survive — declared equals deployed, by construction"
fi

# ------------------------------------------------------------- verdict ----

say "Verdict"
if [ "$failed" -ne 0 ]; then
	printf '  %d assertion(s) failed\n' "$failed"
	exit 1
fi
printf '  OTLP flows service -> Agent -> Gateway -> Backend, on compiled configs.\n'
printf '  What this does NOT prove is in docs/agent-gateway-topology.md.\n'
exit 0
