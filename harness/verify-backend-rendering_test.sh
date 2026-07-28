#!/usr/bin/env bash
#
# Tests the Backend Rendering Check's branching, with `docker`, `curl` and `go`
# stubbed.
#
# The check itself is a handful of queries and a lot of judgement about what came
# back, and the judgement is the part that can be silently wrong. A Backend that
# returns an empty result set for every query would make a naive check green while
# proving the exact opposite of what it claims — so "returned nothing" has to fail,
# "returned an error" has to fail differently, and a version that came back but is
# not the compiled one has to fail loudest of all. None of that is reachable
# without standing up Prometheus, Tempo, a Gateway and an Agent, which takes
# minutes and cannot run on a machine without Docker.
#
# Same shape as validate-compiled-configs_test.sh: stubs on PATH, one temporary
# repository per case, assertions on exit code and output rather than internals.
#
# Creates no containers, pulls no images, and makes no network call.

set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source_script="$here/verify-backend-rendering.sh"

failed=0
ok() { printf '  \033[32mok\033[0m   %s\n' "$*"; }
bad() {
	printf '  \033[31mFAIL\033[0m %s\n' "$*"
	failed=$((failed + 1))
}
say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

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

# The compiled versions the fake compiler emits, and therefore the ones every
# assertion is made against. Deliberately not the real repository's — a test that
# happened to share a hash with the artefact under test would pass on a script that
# compared nothing.
GW_VERSION="sha256:aaaa000000000000000000000000000000000000000000000000000000000001"
AG_VERSION="sha256:bbbb000000000000000000000000000000000000000000000000000000000002"

# ---------------------------------------------------------------------------
# The fixture: a fake repository, a fake compiler, stubbed docker and curl.
# ---------------------------------------------------------------------------

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

mkdir -p "$work/harness" "$work/controlplane" "$work/bin"
cp "$source_script" "$work/harness/verify-backend-rendering.sh"

# Only the fields the check reads: the per-Backend queue_size it round-trips
# against what a Backend reports back.
cat >"$work/controlplane/gateway.yaml" <<'EOF'
gateway:
  backends:
    - backend: primary-apm
      delivery:
        queue_size: 20000
    - backend: cold-archive
      delivery:
        queue_size: 2000
EOF

# The compose files are never parsed by the check — `docker` is stubbed — but they
# have to exist for the paths to resolve.
: >"$work/harness/docker-compose.yaml"
: >"$work/harness/real-backends.yaml"
: >"$work/harness/collector-images.env"
: >"$work/harness/backend-images.env"
cat >"$work/harness/span.json.template" <<'EOF'
{"traceId":"__TRACE_ID__","spanId":"__SPAN_ID__","name":"__SPAN_NAME__","start":"__START_NANOS__","end":"__END_NANOS__"}
EOF

# `go build -o PATH ...` writes a fake compiler at PATH. The compiler emits the two
# compiled configs the check greps for a config_version and for exporter names.
cat >"$work/bin/go" <<'STUB'
#!/usr/bin/env bash
set -u
out=""
prev=""
for a in "$@"; do
	[ "$prev" = "-o" ] && out="$a"
	prev="$a"
done
[ -z "$out" ] && exit 0
cat >"$out" <<'COMPILER'
#!/usr/bin/env bash
if [ "${1:-}" = "gateway" ]; then
	cat <<'GW'
exporters:
    otlp/primary-apm:
        endpoint: apm-otlp:4317
    otlp/cold-archive:
        endpoint: archive-otlp:4317
service:
  telemetry:
    resource:
            otel.platform.config_version: __GW_VERSION__
GW
	exit 0
fi
cat <<'AG'
service:
  telemetry:
    resource:
            otel.platform.config_version: __AG_VERSION__
AG
COMPILER
sed -i.bak "s|__GW_VERSION__|$STUB_GW_VERSION|; s|__AG_VERSION__|$STUB_AG_VERSION|" "$out" && rm -f "$out.bak"
chmod +x "$out"
STUB
chmod +x "$work/bin/go"

# docker does nothing and succeeds. Every bring-up in the check is a compose call
# whose result it does not inspect beyond the exit code.
cat >"$work/bin/docker" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$work/bin/docker"

# curl answers the readiness probes and every query, steered by files in $STUB_DIR
# rather than by env vars — the check runs curl inside command substitution where
# exported state is easy to lose.
#
#   $STUB_DIR/<name>.json  the body returned for that query shape
#
# A missing file means "empty result", which is itself one of the cases under test.
cat >"$work/bin/curl" <<'STUB'
#!/usr/bin/env bash
set -u
dir="$STUB_DIR"

# Readiness probes: `curl -s -o /dev/null -w '%{http_code}' URL`
for a in "$@"; do
	case "$a" in
	*'%{http_code}'*)
		printf '200'
		exit 0
		;;
	esac
done

# Trace-by-ID: `curl -s http://localhost:3200/api/traces/<id>`
for a in "$@"; do
	case "$a" in
	*/api/traces/*)
		cat "$dir/trace.json" 2>/dev/null || printf '{}'
		exit 0
		;;
	esac
done

# Everything else carries the query in a --data-urlencode argument.
query=""
prev=""
for a in "$@"; do
	case "$prev" in
	--data-urlencode)
		case "$a" in
		query=* | q=*) query="${a#*=}" ;;
		esac
		;;
	esac
	prev="$a"
done

body() { cat "$dir/$1.json" 2>/dev/null || printf '{"status":"success","data":{"resultType":"vector","result":[]}}'; }

case "$query" in
*'{resource.service.name'*) cat "$dir/search.json" 2>/dev/null || printf '{"traces":[]}' ;;
*count' by (service_name, otel_platform_config_version)'*) body rollout ;;
*count' by (service_name)'*) body straggler ;;
*queue_capacity*) body capacity ;;
*'queue_size{service_name="otel-gateway"}) > 0'*) body holding ;;
*queue_size*) body queue ;;
# The bare uptime query is the wait-for-self-telemetry probe, and it is answered
# SEPARATELY from the grouped rollout query on purpose. "The series arrived" and
# "the attribute survived onto it" are two different facts, and the whole finding
# behind #50 is that a Backend can be true on the first and false on the second.
*otelcol_process_uptime_seconds_total) body uptime ;;
*) printf '{"status":"success","data":{"resultType":"vector","result":[]}}' ;;
esac
STUB
chmod +x "$work/bin/curl"

# Writes a Prometheus instant-query body from `label=value,...  sample` lines.
prom_body() {
	local out="$1"
	shift
	"${PYTHON:-python3}" - "$out" "$@" <<'PY'
import json, sys
out = sys.argv[1]
result = []
for spec in sys.argv[2:]:
    labels, _, value = spec.partition(' ')
    metric = dict(kv.split('=', 1) for kv in labels.split(',') if kv)
    result.append({"metric": metric, "value": [0, value or "1"]})
json.dump({"status": "success", "data": {"resultType": "vector", "result": result}}, open(out, "w"))
PY
}

# A run of the check with a fresh stub directory. Everything after the first
# argument is left to the caller to have written into $STUB_DIR beforehand.
run_check() {
	output="$(
		cd "$work" &&
			PATH="$work/bin:$PATH" \
				STUB_DIR="$stub" \
				STUB_GW_VERSION="$GW_VERSION" \
				STUB_AG_VERSION="$AG_VERSION" \
				bash "$work/harness/verify-backend-rendering.sh" 2>&1
	)"
	status=$?
}

# A stub directory in which every query answers the way a healthy platform would.
# Individual cases overwrite one file to make one thing wrong, so a failure is
# always attributable to the thing the case changed.
healthy_stub() {
	stub="$(mktemp -d "$work/stub.XXXXXX")"
	# The self-telemetry probe. Non-empty in every case, including the ones where
	# the grouped query is empty: a Backend that received the platform's metrics
	# and filed the resource attribute somewhere ungroupable is precisely the
	# default-Prometheus behaviour this check exists to catch.
	prom_body "$stub/uptime.json" "service_name=otel-gateway 30"
	prom_body "$stub/rollout.json" \
		"service_name=otel-gateway,otel_platform_config_version=$GW_VERSION 1" \
		"service_name=checkout-api,otel_platform_config_version=$AG_VERSION 1"
	prom_body "$stub/straggler.json" "service_name=checkout-api 1"
	prom_body "$stub/queue.json" \
		"exporter=otlp/primary-apm,service_name=otel-gateway 0" \
		"exporter=otlp/cold-archive,service_name=otel-gateway 1" \
		"exporter=otlp/gateway,service_name=checkout-api 0"
	prom_body "$stub/capacity.json" \
		"exporter=otlp/primary-apm 20000" \
		"exporter=otlp/cold-archive 2000"
	prom_body "$stub/holding.json" "exporter=otlp/cold-archive 1"
	printf '{"traces":[{"traceID":"abc123"}]}' >"$stub/search.json"
	printf '{"batches":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"checkout-api"}}]}}]}' >"$stub/trace.json"
}

# ---------------------------------------------------------------------------

say "A healthy platform in front of a real Backend passes"

healthy_stub
run_check
if [ "$status" -eq 0 ]; then
	ok "exits 0 when every query answers the way the platform claims it will"
else
	bad "exited $status on a healthy run — output was:"
	printf '%s\n' "$output" | sed 's/^/       /'
fi
contains "queryable as the label otel_platform_config_version" "names the label the attribute actually became"
contains "the Gateway reports the version the compiler wrote" "compares the reported version against the compiled one"
contains "otlp/cold-archive is attributable by its own exporter label" "checks every Backend the Declaration names"
contains "queue capacity reads back as the declared 2000" "round-trips the declared queue_size through the Backend"

say "A Backend that renders nothing is not a pass"

healthy_stub
prom_body "$stub/rollout.json"
run_check
if [ "$status" -ne 0 ]; then
	ok "exits non-zero when the rollout-confirmation query returns no series"
else
	bad "an empty result set passed — the check would be green on a Backend that dropped everything"
fi
contains "returned nothing" "says the attribute did not survive ingest, rather than reporting a green run"

say "A query that errors is distinguished from one that finds nothing"

healthy_stub
printf '{"status":"error","error":"unknown function"}' >"$stub/rollout.json"
run_check
contains "did not run" "reports a failed query as a failed query"
missing "returned nothing — the attribute did not survive" "does not report a broken query as a missing attribute"

say "A version that is not the compiled one fails"

healthy_stub
prom_body "$stub/rollout.json" \
	"service_name=otel-gateway,otel_platform_config_version=sha256:deadbeef 1" \
	"service_name=checkout-api,otel_platform_config_version=$AG_VERSION 1"
run_check
if [ "$status" -ne 0 ]; then
	ok "exits non-zero when a Backend reports a version the compiler never wrote"
else
	bad "a wrong config_version passed; a Rollout could be confirmed by the wrong configuration"
fi
contains "is not the compiled one" "names the mismatch rather than the absence"

say "A straggler query that cannot tell rolled-out from not fails"

healthy_stub
prom_body "$stub/straggler.json" "service_name=otel-gateway 1"
run_check
contains "lists the Gateway" "fails when the exclusion filter did not exclude"

say "Per-Backend attribution that did not survive fails"

healthy_stub
prom_body "$stub/queue.json" \
	"exporter=otlp/primary-apm,service_name=otel-gateway 0" \
	"exporter=otlp/gateway,service_name=checkout-api 0"
run_check
if [ "$status" -ne 0 ]; then
	ok "exits non-zero when a declared Backend has no queue metric of its own"
else
	bad "a missing exporter label passed; back-pressure would be unattributable"
fi
contains "no queue metric labelled exporter=otlp/cold-archive" "names the Backend that went missing"

say "Isolation that is not visible in the data fails"

healthy_stub
prom_body "$stub/holding.json" \
	"exporter=otlp/cold-archive 1" \
	"exporter=otlp/primary-apm 4"
run_check
contains "isolation is not attributable" "fails when a second Backend's queue is holding too"

say "The platform namespace reaching a Backend on a service's span fails"

healthy_stub
printf '{"batches":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"checkout-api"}},{"key":"otel.platform.config_version","value":{"stringValue":"sha256:forged"}}]}}]}' >"$stub/trace.json"
run_check
if [ "$status" -ne 0 ]; then
	ok "exits non-zero when a service's span carries otel.platform."
else
	bad "a forged platform attribute passed; any service could confirm a Rollout it never received"
fi
contains "carries the platform namespace" "names what leaked"

# ---------------------------------------------------------------------------

printf '\n'
if [ "$failed" -eq 0 ]; then
	printf '\033[32mThe Backend Rendering Check behaves as specified.\033[0m\n'
	exit 0
fi
printf '\033[31m%d assertion(s) failed.\033[0m\n' "$failed"
exit 1
