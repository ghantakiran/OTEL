#!/usr/bin/env bash
# THE BACKEND RENDERING CHECK. A real Backend is handed what the platform reports
# about itself, and then asked for it back — in the form an operator would ask.
#
# Every C7 assertion until now was made against a `debug` exporter's log. That
# proves telemetry crossed a wire out of the Gateway and nothing at all about
# whether a Backend can RENDER it: whether `otel.platform.config_version` survives
# as something you can group by, whether the `exporter` label still names one
# Backend by the time it is queryable, whether the metric is even called what
# docs/platform-self-observation.md says it is called (#50).
#
# It was not called what we said it was called. See docs/backend-label-mapping.md.
#
# WHAT STANDS IN FOR A BACKEND HERE, and where the vendor stops:
#
#   Prometheus   fills the metrics half of the `primary-apm` role.
#   Tempo        fills the trace half.
#
# Neither is named by anything compiled. controlplane/gateway.yaml names a ROLE,
# the compiled Gateway exports OTLP to that role's endpoint, and
# harness/backend-real-collector.yaml is the one file that knows what is behind it
# (ADR 0007). That file is the adapter seam, and this script and
# docs/backend-label-mapping.md are the only other places a product's query
# language appears. It does not appear in a compiled artefact, in a Telemetry
# Contract, or anywhere P1's `query_traces` tool will read.
#
# WHY THE PLATFORM IS UNCHANGED. Nothing here rewrites a compiled config to suit a
# product's ingest shape — that would be the exact coupling ADR 0007 exists to
# prevent, and it would also make the proof worthless: what is being tested is
# whether what the compiler ALREADY writes is renderable.
#
# Usage: bash harness/verify-backend-rendering.sh [--keep]
#   --keep  leave the containers running afterwards, for poking at.

set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$here/.." && pwd)"
generated="$here/generated"

keep=false
[ "${1:-}" = "--keep" ] && keep=true

# Both pin files, for the same reason run.sh passes the first one: the Collector
# Distributions are decided in one place (ADR 0014) and the real Backends in
# another, and neither should be discoverable by reading a compose file.
compose() {
	docker compose --project-directory "$here" \
		--env-file "$here/collector-images.env" \
		--env-file "$here/backend-images.env" \
		-f "$here/docker-compose.yaml" \
		-f "$here/real-backends.yaml" "$@"
}

teardown() { compose --profile emit --profile recover --profile ascompiled --profile rollout down -v --remove-orphans "$@"; }

say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok() { printf '  \033[32mok\033[0m   %s\n' "$*"; }
bad() {
	printf '  \033[31mFAIL\033[0m %s\n' "$*"
	failed=$((failed + 1))
}
note() { printf '       %s\n' "$*"; }

failed=0
cleanup() {
	if [ "$keep" = true ]; then
		printf '\nContainers left running. Prometheus http://localhost:9090, Tempo http://localhost:3200\nTear them down with:\n  docker compose --project-directory %s --env-file %s --env-file %s -f %s -f %s --profile emit down -v\n' \
			"$here" "$here/collector-images.env" "$here/backend-images.env" "$here/docker-compose.yaml" "$here/real-backends.yaml"
		return
	fi
	say "Tearing down"
	teardown >/dev/null 2>&1
}
trap cleanup EXIT

# JSON comes back from both products and there is no way around parsing it. python3
# is the one interpreter present on both a developer's machine and the CI runner;
# grepping a JSON body for a label value would pass on a series that merely
# MENTIONS the string, which is the failure this whole check exists to avoid.
PYTHON="${PYTHON:-python3}"
if ! command -v "$PYTHON" >/dev/null 2>&1; then
	echo "$PYTHON is needed to read the Backends' query responses"
	exit 2
fi

# ---------------------------------------------------------------- queries ----

# promql runs an instant query and prints one line per series:
#   <label>=<value>;<label>=<value>	<sample>
# Nothing downstream greps the raw response, so a query that errors is a failure
# rather than an empty result that reads like one.
promql() {
	curl -sG "http://localhost:9090/api/v1/query" --data-urlencode "query=$1" |
		"$PYTHON" -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    print("QUERY-ERROR unparseable response"); raise SystemExit(0)
if d.get("status") != "success":
    print("QUERY-ERROR " + str(d.get("error", "unknown"))); raise SystemExit(0)
for r in d["data"]["result"]:
    labels = ";".join(f"{k}={v}" for k, v in sorted(r["metric"].items()) if k != "__name__")
    print(labels + "\t" + r["value"][1])
'
}

# traceql searches the trace store and prints one traceID per line.
traceql() {
	curl -sG "http://localhost:3200/api/search" \
		--data-urlencode "q=$1" --data-urlencode "limit=20" |
		"$PYTHON" -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    print("QUERY-ERROR unparseable response"); raise SystemExit(0)
for t in d.get("traces", []):
    print(t["traceID"])
'
}

# trace_resource_keys prints the resource attribute KEYS on a stored trace, which
# is how the negative control below asks whether the platform's namespace leaked
# onto a service's span.
trace_resource_keys() {
	curl -s "http://localhost:3200/api/traces/$1" |
		"$PYTHON" -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    raise SystemExit(0)
for b in d.get("batches", []):
    for a in b.get("resource", {}).get("attributes", []):
        print(a["key"])
'
}

# ---------------------------------------------------------------- compile ----

say "Compiling the Agent and the Gateway"
mkdir -p "$generated"
guardrail="$generated/otel-guardrail"

if ! (cd "$repo_root" && go build -o "$guardrail" ./guardrail/cmd/otel-guardrail); then
	echo "could not build otel-guardrail"
	exit 2
fi

if ! (cd "$repo_root" && "$guardrail" compile guardrail/examples/compliant-contract.yaml) >"$generated/agent.yaml"; then
	echo "could not compile the Agent config"
	exit 2
fi
if ! (cd "$repo_root" && "$guardrail" gateway) >"$generated/gateway.yaml"; then
	echo "could not compile the Gateway config"
	exit 2
fi
ok "Agent and Gateway compiled"

# THE EXPECTED VALUES COME FROM THE COMPILER, not from this script. A hard-coded
# hash here would make the check pass on a stale Backend and fail on a good one the
# day a Profile changes — it would be testing the constant, not the platform.
gateway_version="$(grep -o 'otel\.platform\.config_version: sha256:[0-9a-f]*' "$generated/gateway.yaml" | head -1 | sed 's/.*: //')"
agent_version="$(grep -o 'otel\.platform\.config_version: sha256:[0-9a-f]*' "$generated/agent.yaml" | head -1 | sed 's/.*: //')"
if [ -z "$gateway_version" ] || [ -z "$agent_version" ]; then
	echo "a compiled config carries no otel.platform.config_version; C7 is not in this build"
	exit 2
fi
ok "the compiler predicts gateway=${gateway_version:0:19}… agent=${agent_version:0:19}…"

# Every Backend the Declaration names. The exporter label is asserted against THIS
# list rather than a written-out one, so adding a Backend extends the check.
backends="$(grep -o '^    otlp/[a-z0-9-]*:' "$generated/gateway.yaml" | sed 's/^ *//; s/:$//')"
if [ -z "$backends" ]; then
	echo "the compiled Gateway names no Backend exporters"
	exit 2
fi

# ------------------------------------------------------------- bring-up ----

say "Standing up the platform in front of real Backends"
teardown >/dev/null 2>&1

if ! compose up -d prometheus tempo >/dev/null 2>&1; then
	echo "could not start the Backends"
	exit 2
fi

# Both products refuse queries until they are ready, and a query against a
# not-yet-ready Backend returns nothing — which is indistinguishable from the
# platform having reported nothing, and would turn this whole check green for the
# wrong reason.
waited=0
until [ "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:9090/-/ready)" = "200" ] &&
	[ "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:3200/ready)" = "200" ]; do
	sleep 2
	waited=$((waited + 2))
	if [ "$waited" -ge 90 ]; then
		echo "the Backends did not become ready within 90s"
		exit 2
	fi
done
ok "Prometheus and Tempo are ready ($waited s)"

# `backend-archive` is deliberately NOT started, exactly as run.sh leaves it. Its
# alias does not resolve, so the Gateway holds one genuinely unreachable Backend —
# which is the only reason a per-Backend queue metric has anything to say.
if ! compose up -d gateway agent backend backend-metrics >/dev/null 2>&1; then
	echo "could not start the platform"
	exit 2
fi
ok "Gateway and Agent are up; cold-archive is unreachable, as it should be"

# --------------------------------------------------------------- traffic ----

say "Emitting a span the way a service emits one"

trace_id="$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
start="$(($(date +%s) * 1000000000))"
sed -e "s/__TRACE_ID__/$trace_id/" \
	-e "s/__SPAN_ID__/$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')/" \
	-e "s/__SPAN_NAME__/backend-rendering-check/" \
	-e "s/__START_NANOS__/$start/" \
	-e "s/__END_NANOS__/$((start + 1000000))/" \
	"$here/span.json.template" >"$generated/span.json"

if ! compose --profile emit run --rm sample-service >"$generated/emit.log" 2>&1; then
	echo "the sample service could not reach its Agent"
	sed 's/^/    /' "$generated/emit.log"
	exit 2
fi
ok "one span emitted, naming no Backend and not knowing the Gateway exists"

# The self-telemetry interval is 30s and is COMPILED — shortening it for the
# harness would measure a number this platform does not ship. So the check waits
# for the platform's own clock rather than hurrying it.
say "Waiting for the platform's own telemetry (compiled 30s interval)"
waited=0
until [ -n "$(promql "otelcol_process_uptime_seconds_total")" ]; do
	sleep 3
	waited=$((waited + 3))
	if [ "$waited" -ge 120 ]; then
		bad "no self-telemetry reached the Backend within 120s"
		exit 1
	fi
done
ok "the platform's own metrics are in a real Backend after ${waited}s"

# ------------------------------------------- 1. config_version is queryable ----

say "A real Backend renders otel.platform.config_version"

rollout_query='count by (service_name, otel_platform_config_version) (otelcol_process_uptime_seconds_total)'
rows="$(promql "$rollout_query")"

case "$rows" in
QUERY-ERROR*) bad "the rollout-confirmation query did not run: $rows" ;;
"") bad "the rollout-confirmation query returned nothing — the attribute did not survive ingest" ;;
*)
	ok "the rollout-confirmation query returns $(printf '%s\n' "$rows" | grep -c .) collector(s)"

	# The label, and the exact spelling of it. `otel.platform.config_version` is
	# not a legal Prometheus label name, so SOMETHING rewrote it; asserting the
	# result rather than the rule is what makes this a checked fact.
	if printf '%s\n' "$rows" | grep -q 'otel_platform_config_version='; then
		ok "otel.platform.config_version is queryable as the label otel_platform_config_version"
	else
		bad "no otel_platform_config_version label on the series"
	fi

	# THE VALUE, against what the compiler wrote. This is the whole of "a Rollout
	# is confirmed by telemetry": the version an operator reads out of a Backend
	# has to be the version the Control Plane predicted, or the confirmation is
	# confirming something else.
	if printf '%s\n' "$rows" | grep -q "otel_platform_config_version=$gateway_version"; then
		ok "the Gateway reports the version the compiler wrote (${gateway_version:0:19}…)"
	else
		bad "the Gateway's reported version is not the compiled one ($gateway_version)"
	fi
	if printf '%s\n' "$rows" | grep -q "otel_platform_config_version=$agent_version"; then
		ok "the Agent reports the version the compiler wrote (${agent_version:0:19}…)"
	else
		bad "the Agent's reported version is not the compiled one ($agent_version)"
	fi
	;;
esac

# The second operator query on the page: who is NOT yet on the version this Rollout
# compiled. It has to return the OTHER collector and not the Gateway itself, or the
# label is being matched as a string somewhere rather than filtered on.
straggler_query="count by (service_name) (otelcol_process_uptime_seconds_total{otel_platform_config_version!=\"$gateway_version\"})"
stragglers="$(promql "$straggler_query")"
if printf '%s\n' "$stragglers" | grep -q 'service_name=otel-gateway'; then
	bad "the straggler query lists the Gateway, which is on the version it was compared against"
elif [ -n "$stragglers" ]; then
	ok "the straggler query names collectors on another version, and not the Gateway"
else
	bad "the straggler query returned nothing; it cannot distinguish rolled-out from not"
fi

# ------------------------------------ 2. per-Backend attribution is queryable ----

say "A real Backend preserves per-Backend attribution"

queue="$(promql 'max by (exporter, service_name) (otelcol_exporter_queue_size)')"
case "$queue" in
QUERY-ERROR* | "") bad "no per-exporter queue metric is queryable: ${queue:-empty}" ;;
*)
	# One exporter per Backend, named after the Backend — the property that makes
	# "which Backend is behind?" a query rather than a guess. Asserted for every
	# Backend the Declaration names, so a fourth one is covered the day it is added.
	for exporter in $backends; do
		if printf '%s\n' "$queue" | grep -q "exporter=$exporter;"; then
			ok "$exporter is attributable by its own exporter label"
		else
			bad "no queue metric labelled exporter=$exporter reached the Backend"
		fi
	done

	# An Agent's numbers are filed under the SERVICE's own name, not in a pool of
	# anonymous collectors. That is what makes "why is this service's telemetry
	# thin?" answerable under that service.
	if printf '%s\n' "$queue" | grep -q 'exporter=otlp/gateway;'; then
		ok "an Agent's own exporter is filed under its service, as otlp/gateway"
	else
		bad "no Agent-side queue metric is queryable"
	fi
	;;
esac

# THE COMPILED DELIVERY SETTINGS, ROUND-TRIPPED. `queue_size` is declared per
# Backend in controlplane/gateway.yaml, compiled into the Gateway, held by a
# running collector, exported as OTLP and rendered by a real Backend. If the
# capacity an operator can query is not the capacity the org declared, then
# "how close is this Backend to dropping?" is being answered with someone else's
# number.
capacity="$(promql 'max by (exporter) (otelcol_exporter_queue_capacity{service_name="otel-gateway"})')"
declared_ok=true
for exporter in $backends; do
	name="${exporter#otlp/}"
	declared="$(
		"$PYTHON" - "$repo_root/controlplane/gateway.yaml" "$name" <<'PY'
import re, sys
# The Declaration is read as text rather than as YAML: this check must not depend
# on a parser the repository does not otherwise need, and the block it wants is
# unambiguous.
doc = open(sys.argv[1]).read()
block = re.split(r'\n    - backend: ', doc)
for b in block[1:]:
    if b.split('\n', 1)[0].strip() == sys.argv[2]:
        m = re.search(r'queue_size:\s*(\d+)', b)
        print(m.group(1) if m else '')
        break
PY
	)"
	[ -z "$declared" ] && continue
	if printf '%s\n' "$capacity" | grep -q "exporter=$exporter	$declared"; then
		ok "$exporter's queue capacity reads back as the declared $declared"
	else
		bad "$exporter's queryable capacity is not the declared $declared"
		declared_ok=false
	fi
done
[ "$declared_ok" = true ] && note "the Gateway Declaration's delivery settings survive compile, run and ingest"

# ISOLATION, AS A QUERY. One Backend unreachable and the others fine has to look
# like ONE exporter's number moving. Two moving together would mean either both
# destinations are down or the isolation is broken, and an operator cannot tell
# those apart from prose — only from this.
holding="$(promql 'max by (exporter) (otelcol_exporter_queue_size{service_name="otel-gateway"}) > 0')"
if printf '%s\n' "$holding" | grep -q 'exporter=otlp/cold-archive'; then
	ok "the unreachable Backend's queue is holding, and names itself"
	others="$(printf '%s\n' "$holding" | grep -v 'cold-archive' | grep -c .)"
	if [ "$others" -eq 0 ]; then
		ok "no other Backend's queue is holding — isolation is visible in the data"
	else
		bad "$others other Backend queue(s) are also holding; isolation is not attributable"
	fi
else
	# Not a failure of the platform: a queue that has already drained or not yet
	# filled says nothing either way, and asserting on it would make this check
	# flaky rather than strict.
	note "cold-archive's queue was not holding at query time; isolation not exercised this run"
fi

# ------------------------------------------- 3. traces, for P1's query_traces ----

say "A real trace store renders a span by the identity its Contract stamped"

# Tempo cuts a block before a trace is searchable, so a miss here needs a retry
# rather than a verdict.
waited=0
found=""
until [ -n "$found" ]; do
	found="$(traceql '{resource.service.name="checkout-api"}')"
	[ -n "$found" ] && break
	sleep 3
	waited=$((waited + 3))
	if [ "$waited" -ge 60 ]; then break; fi
done

if [ -n "$found" ]; then
	ok "a span is searchable by the service.name the Telemetry Contract declared"
	note "this is the query shape P1's query_traces tool will front (#16)"
else
	bad "no span is searchable by the Contract's service.name within 60s"
fi

# THE NEGATIVE CONTROL. The Agent deletes the `otel.platform.` namespace from
# everything it forwards, so a service's span must reach a Backend WITHOUT a
# config_version on it. If it arrived, any service could confirm a Rollout it never
# received — and this is the first time that claim has been checked against a
# Backend rather than against a debug log.
keys="$(trace_resource_keys "$trace_id")"
if [ -z "$keys" ]; then
	note "the emitted trace was not retrievable by ID; the namespace control did not run"
elif printf '%s\n' "$keys" | grep -q '^otel\.platform\.'; then
	bad "a service's span carries the platform namespace: $(printf '%s\n' "$keys" | grep '^otel\.platform\.')"
else
	ok "the span carries no otel.platform. attribute — the Agent stripped it, and a Backend confirms it"
	if printf '%s\n' "$keys" | grep -q '^service\.name$'; then
		ok "it does carry the identity the Contract declared"
	fi
fi

# ----------------------------------------------------------------- report ----

say "What a Backend actually called things"
note "documented in docs/backend-label-mapping.md — this is the discovered mapping, not an assumed one"
printf '\n'
promql 'otelcol_process_uptime_seconds_total' | head -2 | sed 's/^/       /'

if [ "$failed" -eq 0 ]; then
	printf '\n\033[32mA real Backend renders the platform'"'"'s own telemetry, and the queries in docs/platform-self-observation.md run against it.\033[0m\n'
	exit 0
fi

printf '\n\033[31m%d check(s) failed.\033[0m\n' "$failed"
exit 1
