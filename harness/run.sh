#!/usr/bin/env bash
#
# Stands up the two-tier topology and asserts OTLP flows
# service -> Agent -> Gateway -> several Backends.
#
# Everything the Agent and the Gateway run is COMPILED here, first, by the binary
# under test. A harness that proved a hand-written config works would prove nothing
# about the compiler.
#
# The Gateway Declaration names three Backends. This starts TWO of them: the cold
# archive is never brought up, so its DNS name does not resolve and the Gateway
# holds a genuinely unreachable Backend for the whole run. Everything below happens
# while that is true, which is the point — one Backend being down must not be
# everyone's outage (ADR 0010, #13).
#
# What it asserts, in order:
#
#   1. NEGATIVE CONTROL. With the Gateway not running, a span emitted to the Agent
#      does not reach the Backend. This is what makes the positive case mean
#      something: it rules out the Agent having a path of its own.
#   2. The Gateway STARTS while one of its three Backends is unreachable. A Gateway
#      that refused to start, or started and exported nothing, would be a total
#      failure of per-Backend isolation.
#   3. The span emitted after the Gateway starts arrives at the healthy Backend, by
#      trace ID, having crossed two collectors it was never told about.
#   4. What arrives carries the TELEMETRY CONTRACT's service.name, not the one the
#      sample service sent. The compiled Agent config is the only thing that could
#      have made that substitution, so it ran.
#   5. FAN-OUT PER SIGNAL. The span does NOT reach the metrics-only Backend, while
#      a metric emitted next does. Both directions, on a running Backend.
#   6. The unreachable Backend had no container at all while 3-5 ran, so those
#      assertions really were made against a Gateway holding a down Backend.
#   7. Once the archive is started it receives the span emitted minutes earlier —
#      which it cannot have had before. The Gateway held that span in the ARCHIVE's
#      OWN queue the whole time it was serving the other two normally, which is
#      per-Backend isolation observed rather than asserted.
#
# Needs: docker, docker compose, go. Takes about two minutes.
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

# `docker compose down` leaves containers belonging to a profile alone, so a bare
# down would leave the archive Backend from a previous run standing — and the run
# after that would assert against a Backend that was never down. Every teardown
# names both profiles.
teardown() { compose --profile emit --profile recover down -v --remove-orphans "$@"; }

say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok() { printf '  \033[32mok\033[0m   %s\n' "$*"; }
bad() {
	printf '  \033[31mFAIL\033[0m %s\n' "$*"
	failed=$((failed + 1))
}

failed=0
cleanup() {
	if [ "$keep" = true ]; then
		printf '\nContainers left running. Tear them down with:\n  docker compose --project-directory %s -f %s --profile emit --profile recover down -v\n' "$here" "$here/docker-compose.yaml"
		return
	fi
	say "Tearing down"
	teardown >/dev/null 2>&1
}
trap cleanup EXIT

# 32 and 16 hex characters, the widths OTLP wants for a trace and a span ID.
hexrand() { head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'; }

now_nanos() { echo "$(($(date +%s) * 1000000000))"; }

# emit writes a span payload with the given trace ID and name, and posts it to the
# Agent from a container that knows only where its Agent is.
emit() {
	local trace_id="$1"
	local span_name="$2"
	local start
	start="$(now_nanos)"

	sed -e "s/__TRACE_ID__/$trace_id/" \
		-e "s/__SPAN_ID__/$(hexrand 8)/" \
		-e "s/__SPAN_NAME__/$span_name/" \
		-e "s/__START_NANOS__/$start/" \
		-e "s/__END_NANOS__/$((start + 1000000))/" \
		"$here/span.json.template" >"$generated/span.json"

	if ! compose run --rm sample-service >"$generated/emit.log" 2>&1; then
		# Worth distinguishing loudly: a span the Agent never accepted would otherwise
		# read below as a span the Gateway failed to relay.
		echo "the sample service could not POST OTLP to its Agent:"
		sed 's/^/    /' "$generated/emit.log"
		exit 2
	fi
}

# emit_metric posts one metric to the Agent, from the same service, over the same
# hop. The metrics pipeline fans out to a different set of Backends from the traces
# pipeline, which is the thing it exists to show.
emit_metric() {
	local metric_name="$1"
	local start
	start="$(now_nanos)"

	sed -e "s/__METRIC_NAME__/$metric_name/" \
		-e "s/__START_NANOS__/$start/" \
		-e "s/__END_NANOS__/$((start + 1000000))/" \
		"$here/metric.json.template" >"$generated/metric.json"

	if ! compose run --rm sample-service-metrics >"$generated/emit.log" 2>&1; then
		echo "the sample service could not POST an OTLP metric to its Agent:"
		sed 's/^/    /' "$generated/emit.log"
		exit 2
	fi
}

# arrived polls one Backend's log for a string, up to a deadline.
arrived() {
	local backend="$1"
	local needle="$2"
	local seconds="$3"
	local deadline=$((SECONDS + seconds))
	while [ "$SECONDS" -lt "$deadline" ]; do
		if compose logs "$backend" 2>&1 | grep -q -- "$needle"; then return 0; fi
		sleep 2
	done
	return 1
}

# logged polls any container's log for a string.
logged() {
	local service="$1"
	local needle="$2"
	local seconds="$3"
	local deadline=$((SECONDS + seconds))
	while [ "$SECONDS" -lt "$deadline" ]; do
		if compose logs "$service" 2>&1 | grep -q -- "$needle"; then return 0; fi
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

backend_count="$(grep -c '^    otlp/' "$generated/gateway.yaml")"
if [ "$backend_count" -lt 2 ]; then
	echo "the compiled Gateway exports to $backend_count Backend(s); this harness is about fanning out to several"
	exit 2
fi
ok "the Gateway fans out to $backend_count Backends, each with its own exporter"

contract_service_name="$(grep -A1 'key: service.name' "$generated/agent.yaml" | grep 'value:' | head -1 | sed 's/.*value: //')"
if [ -z "$contract_service_name" ]; then
	echo "the compiled Agent config stamps no service.name; the harness has nothing to assert on"
	exit 2
fi
ok "the Contract's service.name is $contract_service_name"

# ------------------------------------------------------- negative control ----

say "Negative control: the Agent alone cannot reach any Backend"
teardown >/dev/null 2>&1
if ! compose up -d backend backend-metrics agent >/dev/null 2>&1; then
	echo "could not start the Agent and the Backends"
	exit 2
fi
ready backend || {
	echo "the Backend never became ready"
	exit 2
}

orphan_trace="$(hexrand 16)"
emit "$orphan_trace" "harness-with-no-gateway"

if arrived backend "$orphan_trace" 20; then
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

say "Starting the Gateway, with one of its three Backends unreachable"
if ! compose up -d gateway >/dev/null 2>&1; then
	echo "could not start the Gateway"
	exit 2
fi
ready gateway || {
	echo "the Gateway never became ready — its compiled config may not be one this collector accepts"
	compose logs gateway 2>&1 | tail -30
	exit 2
}
ok "the Gateway started on its compiled config, with cold-archive unresolvable"

# The spill extensions are the one thing in the compiled Gateway config that the
# core distribution does not have (ADR 0014). If they did not start, everything
# below would still pass while the durability the declaration asks for was absent.
if logged gateway "file_storage/primary-apm" 20; then
	ok "each spilling Backend's own file_storage extension started"
else
	bad "the Gateway logged no file_storage extension, so the spill the declaration asks for is not running"
fi

say "service -> Agent -> Gateway -> Backends"
trace="$(hexrand 16)"
emit "$trace" "harness-end-to-end"
ok "the sample service emitted one span to its Agent, naming no Backend"

if arrived backend "$trace" 120; then
	ok "the span arrived at the healthy Backend, by trace ID $trace — while cold-archive was unreachable"
else
	bad "the span never reached the healthy Backend (trace ID $trace)"
	compose logs 2>&1 | tail -40
fi

if arrived backend "$contract_service_name" 30; then
	ok "it carries service.name=$contract_service_name — from the Telemetry Contract, stamped by the compiled Agent config"
else
	bad "the Contract's service.name never reached the Backend, so the compiled resource processor did not run"
fi

if arrived backend "harness-sample-service" 5; then
	bad "the sample service's own service.name reached the Backend; the Contract's value did not replace it"
else
	ok "the sample service's own service.name did not survive — declared equals deployed, by construction"
fi

# ------------------------------------------------------ fan-out per Signal ----

say "Fan-out per Signal: a metrics-only Backend gets metrics and nothing else"

# The metrics store declares `signals: [metrics]`, so it must not appear in the
# traces pipeline. It is running and would print anything that reached it.
if arrived backend-metrics "$trace" 10; then
	bad "the span reached the metrics-only Backend, which has nowhere to put it — the traces pipeline fans out to it anyway"
else
	ok "the span did not reach the metrics-only Backend"
fi

metric="harness.fanout.$(hexrand 4)"
emit_metric "$metric"
ok "the sample service emitted one metric to its Agent, over the same hop"

if arrived backend-metrics "$metric" 120; then
	ok "the metric arrived at the metrics-only Backend ($metric) — the metrics pipeline does fan out to it"
else
	bad "the metric never reached the metrics-only Backend ($metric)"
	compose logs gateway 2>&1 | tail -20
fi

if arrived backend "$metric" 30; then
	ok "the same metric also arrived at the primary APM, which takes every Signal — one Signal, two Backends"
else
	bad "the metric did not reach the primary APM, which declares no Signal subset"
fi

# ---------------------------------------------------- the down Backend ----

say "The unreachable Backend really was unreachable"

# Without this, everything above would be consistent with cold-archive having been
# healthy all along and the isolation never having been tested. It is asserted
# structurally rather than from the Gateway's log: a collector retrying a failed
# export says nothing at default verbosity, so the log is silent either way.
if [ -z "$(compose ps -aq backend-archive 2>/dev/null)" ]; then
	ok "cold-archive had no container at all while everything above ran — its endpoint did not even resolve"
else
	bad "a cold-archive container exists, so the Backend the fan-out was tested against was not actually down"
fi

# The strongest evidence that the isolation is real, and it is this rather than
# anything in a log: the archive receives a span emitted minutes ago, which it
# cannot have had before. The Gateway held that span in COLD-ARCHIVE's OWN queue
# for the whole time it was serving the other two Backends normally.
say "Bringing the archive up: its own queue drains, on its own"
if ! compose --profile recover up -d backend-archive >/dev/null 2>&1; then
	echo "could not start the archive Backend"
	exit 2
fi
ready backend-archive || {
	echo "the archive Backend never became ready"
	exit 2
}

if arrived backend-archive "$trace" 120; then
	ok "the span the archive missed arrived once it came up (trace ID $trace) — its queue held it, and its retry drained it"
else
	bad "the archive received nothing after coming up; its queue dropped what it was holding, or retry gave up"
	compose logs gateway 2>&1 | tail -20
fi

# ------------------------------------------------------------- verdict ----

say "Verdict"
if [ "$failed" -ne 0 ]; then
	printf '  %d assertion(s) failed\n' "$failed"
	exit 1
fi
printf '  OTLP flows service -> Agent -> Gateway -> several Backends, on compiled configs,\n'
printf '  fanned out per Signal, with one Backend unreachable throughout.\n'
printf '  What this does NOT prove is in docs/agent-gateway-topology.md.\n'
exit 0
