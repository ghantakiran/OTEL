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
#   6. PIPELINE GUARDRAIL. A span that writes the verdict attributes ITSELF, sent
#      through the compliant Agent, arrives with none of them — so the namespace is
#      the Gateway's, and the assertion that compliant telemetry is untagged is not
#      vacuous. A SECOND Agent, compiled from a Contract declaring one of the three
#      resource attributes S1 requires, then emits a span, and it arrives TAGGED
#      and NOT DROPPED: violation.S1=block, violation.S3=warn, blocking=true —
#      values, not just keys, because the Severity mapping is the decision. Neither
#      Agent's compiled config mentions any of it, so only the Gateway wrote it.
#   7. The unreachable Backend had no container at all while 3-6 ran, so those
#      assertions really were made against a Gateway holding a down Backend.
#   8. A span emitted while the archive had no container reaches it once it is
#      started — which it cannot have had before. The Gateway held that span in the
#      ARCHIVE's OWN queue while serving the other two normally, which is
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

# emit_forged posts a span that writes the Gateway's own verdict attributes itself,
# at both the resource and the span level, through the COMPLIANT Agent. Its
# telemetry violates nothing, so if any otel.guardrail attribute reaches the
# Backend it came from the service and the Gateway failed to scrub it.
emit_forged() {
	local trace_id="$1"
	local span_name="$2"
	local start
	start="$(now_nanos)"

	sed -e "s/__TRACE_ID__/$trace_id/" \
		-e "s/__SPAN_ID__/$(hexrand 8)/" \
		-e "s/__SPAN_NAME__/$span_name/" \
		-e "s/__START_NANOS__/$start/" \
		-e "s/__END_NANOS__/$((start + 1000000))/" \
		"$here/forged-span.json.template" >"$generated/span.json"

	if ! compose run --rm sample-service >"$generated/emit.log" 2>&1; then
		echo "the sample service could not POST the forged OTLP to its Agent:"
		sed 's/^/    /' "$generated/emit.log"
		exit 2
	fi
}

# emit_drifted posts the span payload to the SECOND Agent — the one compiled from
# a Telemetry Contract that declares one resource attribute where S1 requires
# three. Same payload, same hop, different compiled config.
emit_drifted() {
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

	if ! compose run --rm sample-service-drifted >"$generated/emit.log" 2>&1; then
		echo "the sample service could not POST OTLP to the drifted Agent:"
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
#
# -F here and below: every needle is a literal — a trace ID, an attribute name, a
# component ID — and an unanchored `.` in `otel.guardrail` would happily match the
# hyphen in `otel-guardrail`, which is a different thing entirely.
arrived() {
	local backend="$1"
	local needle="$2"
	local seconds="$3"
	local deadline=$((SECONDS + seconds))
	while [ "$SECONDS" -lt "$deadline" ]; do
		if compose logs "$backend" 2>&1 | grep -qF -- "$needle"; then return 0; fi
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
		if compose logs "$service" 2>&1 | grep -qF -- "$needle"; then return 0; fi
		sleep 2
	done
	return 1
}

ready() {
	local service="$1"
	local deadline=$((SECONDS + 60))
	while [ "$SECONDS" -lt "$deadline" ]; do
		if compose logs "$service" 2>&1 | grep -qF "Everything is ready"; then return 0; fi
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

# The second Agent's config: a Telemetry Contract that declares one resource
# attribute where S1 requires three. Compiled by the same binary, from a Contract
# `otel-guardrail check` would have blocked — which is exactly the stream a
# Pipeline Guardrail exists to catch when Preflight did not run.
if ! (cd "$repo_root" && "$guardrail" compile harness/drifted-contract.yaml) >"$generated/agent-drifted.yaml"; then
	echo "could not compile the drifted Agent config"
	exit 2
fi
ok "a second Agent config compiled from harness/drifted-contract.yaml"

# Read out of the compiled Gateway rather than listed here: a catalog that stops
# enforcing anything at the pipeline must not leave these assertions passing
# against a Gateway that checks nothing.
if ! grep -q '^    transform/guardrail:' "$generated/gateway.yaml"; then
	echo "the compiled Gateway runs no Pipeline Guardrail; there is nothing here to test"
	exit 2
fi
ok "the Gateway runs the Pipeline Guardrails compiled from the Standard catalog"

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
#
# Every one of them is checked, read out of the compiled config rather than listed
# here: a Gateway that started one extension and not the other must not report ok,
# and this way adding a spilling Backend cannot leave the assertion behind.
spilling="$(sed -n 's/^ *\(file_storage\/[a-z0-9-]*\):$/\1/p' "$generated/gateway.yaml")"
if [ -z "$spilling" ]; then
	echo "the compiled Gateway defines no file_storage extension; spill is what needs the contrib distribution, so there is nothing here to test"
	exit 2
fi
for storage in $spilling; do
	if logged gateway "$storage" 20; then
		ok "$storage started — its Backend's own spill storage"
	else
		bad "the Gateway never started $storage, so that Backend's queue is not persistent"
	fi
done

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

# --------------------------------------------------- Pipeline Guardrail ----

say "Pipeline Guardrail: compliant telemetry is not touched, and cannot forge a verdict"

# This whole section has to come BEFORE any non-compliant telemetry exists:
# afterwards the Backend's log carries real Guardrail attributes and the assertion
# below would pass or fail for the wrong reason.
#
# A span that writes the verdict attributes ITSELF, at both the resource and the
# span level, through the compliant Agent. Without it the assertion below would be
# vacuous — nothing would ever have tried, so "no otel.guardrail in the log" would
# hold even if the Gateway's clearing statements were deleted.
forged_trace="$(hexrand 16)"
emit_forged "$forged_trace" "harness-forged-verdict"
ok "a service claiming otel.guardrail.blocking=false emitted a span through the compliant Agent"

if arrived backend "$forged_trace" 120; then
	ok "the forged span arrived at the Backend (trace ID $forged_trace) — so what it carries can be judged"
else
	bad "the forged span never reached the Backend, so nothing below is being asserted about it"
	compose logs gateway 2>&1 | tail -20
fi

# Everything before this went through the Agent compiled from a Contract that
# declares every attribute S1 requires and S3 recommends — so it violates nothing
# and must carry no verdict; and the forged span's own claims must have been
# scrubbed. One assertion covers both, and it is the load-bearing one for the
# namespace being the Gateway's (ADR 0015).
if arrived backend "otel.guardrail" 5; then
	bad "an otel.guardrail attribute reached the Backend from compliant telemetry — either the Guardrail tags everything, or a service's forged verdict survived"
	compose logs backend 2>&1 | grep -F "otel.guardrail" | head -5
else
	ok "no otel.guardrail attribute survived: compliant telemetry is untagged and the forged one was scrubbed"
fi

say "Pipeline Guardrail: a non-compliant stream is tagged, and still arrives"

if ! compose up -d agent-drifted >/dev/null 2>&1; then
	echo "could not start the drifted Agent"
	exit 2
fi
ready agent-drifted || {
	echo "the drifted Agent never became ready"
	exit 2
}

drift_trace="$(hexrand 16)"
emit_drifted "$drift_trace" "harness-drifted-stream"
ok "a service whose Contract declares one of S1's three required attributes emitted a span"

# THE LOAD-BEARING ASSERTION OF ADR 0015. A Standard whose Severity is `block`
# does not delete telemetry at run time — `block` fails a BUILD, and the runtime
# analogue of stopping a deploy is not destroying the observation. If this ever
# starts failing because the span is gone, the Severity mapping has been changed
# into the one that ADR rejects.
if arrived backend "$drift_trace" 120; then
	ok "the non-compliant span arrived at the Backend (trace ID $drift_trace) — a block Standard tags, it does not drop"
else
	bad "the non-compliant span never reached the Backend; a Pipeline Guardrail must not delete telemetry"
	compose logs gateway 2>&1 | tail -20
fi

# What an operator reads. One key per violated Standard, VALUED AT THAT STANDARD'S
# SEVERITY, plus the one low-cardinality roll-up to alert on.
#
# The values are asserted, not just the keys: the Severity mapping is the decision
# ADR 0015 turns on, and a Guardrail that tagged every violation `block` would look
# identical here if only the key were checked. The forms are the debug exporter's —
# `Str(...)` for a string attribute, `Bool(...)` for a boolean.
for tag in \
	"otel.guardrail.violation.S1: Str(block)" \
	"otel.guardrail.violation.S3: Str(warn)" \
	"otel.guardrail.blocking: Bool(true)"; do
	if arrived backend "$tag" 60; then
		ok "the Gateway tagged it $tag — the Standard is named on the record, at its own Severity"
	else
		bad "the Gateway did not tag the non-compliant span with $tag"
		compose logs backend 2>&1 | grep -F "otel.guardrail" | head -5
		compose logs gateway 2>&1 | tail -20
	fi
done

# The Agent is a core collector and stamps only what its Contract declares; the
# transform that wrote those attributes exists in the GATEWAY's compiled config
# and in no Agent's. Enforcement is centralised (ADR 0007, #14).
# -F, because every compiled config carries the string `otel-guardrail` in its
# generated-by header and an unanchored `.` would match the hyphen.
if grep -qF "otel.guardrail" "$generated/agent-drifted.yaml" || grep -qF "otel.guardrail" "$generated/agent.yaml"; then
	bad "an Agent's compiled config mentions the Guardrail attributes; enforcement must be centralised in the Gateway"
else
	ok "no Agent config mentions a Guardrail — the tags can only have come from the Gateway"
fi

# ---------------------------------------------------- the down Backend ----

say "The unreachable Backend really was unreachable"

# Without this, everything above would be consistent with cold-archive having been
# healthy all along and the isolation never having been tested. It is asserted
# structurally rather than from the Gateway's log: a collector retrying a failed
# export says nothing at default verbosity, so the log is silent either way.
# `--profile recover` is named because compose subcommands do not agree about
# profile-gated services — `down` skips them, which is why teardown names the
# profiles too. Without it here, a `ps` that filtered the way `down` does would
# report empty whether or not an archive were running, and the one check that gives
# assertions 3-5 their meaning would pass vacuously.
if [ -z "$(compose --profile recover ps -aq backend-archive 2>/dev/null)" ]; then
	ok "cold-archive had no container at all while everything above ran — its endpoint did not even resolve"
else
	bad "a cold-archive container exists, so the Backend the fan-out was tested against was not actually down"
fi

# The strongest evidence that the isolation is real, and it is this rather than
# anything in a log: the archive receives a span emitted while it did not exist,
# which it cannot have had before. The Gateway held that span in COLD-ARCHIVE's OWN
# queue while serving the other two Backends normally.
#
# A FRESH span, not the end-to-end one, and the reason is a deadline rather than a
# preference. cold-archive compiles `retry_on_failure` with no max_elapsed_time, so
# the collector's 300s default applies and a span queued for it is dropped for good
# after five minutes. The end-to-end span is already several polling windows old by
# now, and on a slow machine it could cross that line — which would fail here for a
# reason that has nothing to do with per-Backend isolation. This one is emitted on
# a known clock.
say "Bringing the archive up: its own queue drains, on its own"

recovery_trace="$(hexrand 16)"
emit "$recovery_trace" "harness-archive-recovery"
ok "emitted a span while the archive still had no container at all"

# Long enough that the Gateway has certainly tried this Backend and failed: the
# exporter dials on the next flush, DNS returns nothing, and the batch goes to
# cold-archive's queue. Without the pause the archive could come up first, and
# arrival below would prove ordinary delivery rather than a queue that held.
sleep 15

if ! compose --profile recover up -d backend-archive >/dev/null 2>&1; then
	echo "could not start the archive Backend"
	exit 2
fi
ready backend-archive || {
	echo "the archive Backend never became ready"
	exit 2
}

if arrived backend-archive "$recovery_trace" 120; then
	ok "the span the archive missed arrived once it came up (trace ID $recovery_trace) — its own queue held it, and its own retry drained it"
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
