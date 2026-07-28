#!/usr/bin/env bash
#
# THE DISTRIBUTION CHECK: every collector configuration this repository ships is
# loaded and started by the Collector Distribution it is compiled for.
#
# WHY THIS EXISTS (#34)
#
# `controlplane.CollectorConfig.Validate()` checks referential integrity — every
# component a pipeline names is defined, every defined component is used. That
# catches what this class of generator actually gets wrong, and it is the strongest
# check available inside `go test`, because a collector is not a Go library this
# repository can import.
#
# It cannot know what a given distribution accepts. An unknown processor, a setting
# a built-in component does not have, a type mismatch inside a component's own
# config block — all of them are internally coherent and all of them are refused by
# the binary that has to run the file. Until this ran, every compiled configuration
# in fleet/ was YAML no collector had ever parsed.
#
# TWO STEPS, AND THE SECOND IS NOT REDUNDANT
#
#   validate   `otelcol validate` resolves the config against the distribution's
#              component set. Cheap, and it catches the whole unknown-component
#              class.
#   start      The collector is then actually started on the file and has to
#              announce itself ready.
#
# The second exists because the first is not sufficient, and that is not a
# precaution — it is a defect this repository already hit. `protocol: grpc/protobuf`
# in a self-telemetry reader passes `otelcol validate` and then refuses to start,
# because the telemetry SDK is built after config resolution and before the first
# pipeline byte moves. A check that stopped at `validate` would have shipped it.
#
# WHICH DISTRIBUTION, AND WHY THAT IS THE POINT
#
# Agents are checked on CORE and the Gateway on CONTRIB, because that is what they
# run on (ADR 0014): the Gateway needs `file_storage` to Spill and `transform` for
# the Pipeline Guardrail, and neither is in core. Checking every file on contrib
# would pass a compiled Agent that had quietly acquired a contrib-only component,
# and the first anyone would know is a crash loop on a node. The split here is
# ADR 0014's line, mechanised.
#
# WHAT IT DOES NOT PROVE
#
# A started collector is not a working one. Nothing here sends telemetry through
# these configurations — harness/run.sh does that, on the topology, and this is
# deliberately the cheap check that can run on every pull request. The endpoints in
# a compiled config are cluster DNS names that do not resolve here, so exporters
# and the self-telemetry reader dial and fail; that is expected and is not what is
# being asserted. See docs/collector-distribution-check.md.
#
# Usage:
#   GUARDRAIL=<path to otel-guardrail> bash .github/scripts/validate-compiled-configs.sh
#
# Environment:
#   GUARDRAIL       the built CLI. Required — the Gateway's configuration is not
#                   committed, so it has to be compiled to be checked.
#   FLEET           the Fleet directory. Default `fleet`.
#   IMAGES          the pinned Collector Distributions. Default
#                   `harness/collector-images.env` — the same file the harness
#                   reads, so the pin is decided once (ADR 0014).
#   READY_TIMEOUT   seconds to wait for a collector to announce itself. Default 60.

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fleet="${FLEET:-$repo_root/fleet}"
images="${IMAGES:-$repo_root/harness/collector-images.env}"
ready_timeout="${READY_TIMEOUT:-60}"

if [ -z "${GUARDRAIL:-}" ]; then
	echo "GUARDRAIL is not set: the Gateway's collector configuration is compiled rather than committed, so the CLI is needed to check it" >&2
	exit 2
fi
if [ ! -r "$images" ]; then
	echo "cannot read the pinned Collector Distributions at $images" >&2
	exit 2
fi
# shellcheck source=/dev/null
. "$images"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

failed=0
ok() { printf '  \033[32mok\033[0m   %s\n' "$*"; }
bad() {
	printf '  \033[31mFAIL\033[0m %s\n' "$*"
	failed=$((failed + 1))
}

# spillMounts is one --tmpfs argument per storage directory the configuration
# declares, so a Spilling Backend's `create_directory: true` has somewhere writable
# to create into. Read out of the artefact rather than hard-coded: the spill root
# is the Gateway Declaration's to choose, and a check that restated it would pass
# the day the two disagreed.
#
# The PARENT of each declared directory, deduplicated — that is what the extension
# creates into, and two Backends under one root must not become two mounts.
spillMounts() {
	sed -n 's/^ *directory: *//p' "$1" | while read -r directory; do
		printf '%s\n' "$(dirname "$directory")"
	done | sort -u | while read -r root; do
		[ -n "$root" ] && [ "$root" != "." ] && printf -- '--tmpfs\n%s\n' "$root"
	done
}

# checkOn runs one configuration through one distribution: resolved, then started.
#
# Both halves report the collector's own output rather than a summary of it. The
# whole value of this check is that the collector says something `Validate()`
# cannot, so paraphrasing it would throw away the thing being bought.
checkOn() {
	local label="$1" config="$2" image="$3"
	local mounts=()
	while IFS= read -r line; do [ -n "$line" ] && mounts+=("$line"); done < <(spillMounts "$config")

	local out="$work/$label.validate.log"
	if ! docker run --rm \
		${mounts[@]+"${mounts[@]}"} \
		-v "$config:/etc/otelcol/config.yaml:ro" \
		"$image" validate --config=/etc/otelcol/config.yaml >"$out" 2>&1; then
		bad "$label was rejected by $image"
		sed 's/^/       /' "$out"
		return 1
	fi

	# Started in the background and watched through a captured file. Not
	# `docker logs | grep -q`: grep exits at the first match and SIGPIPEs the
	# writer, which pipefail then reports as a failed pipeline — so a line that IS
	# present reads as absent once the log is large enough. That bug was live in
	# harness/run.sh until C7.
	local name="distribution-check-$label-$$"
	local log="$work/$label.start.log"
	docker run --rm --name "$name" \
		${mounts[@]+"${mounts[@]}"} \
		-v "$config:/etc/otelcol/config.yaml:ro" \
		"$image" --config=/etc/otelcol/config.yaml >"$log" 2>&1 &
	local runner=$!

	local ready=false elapsed=0
	while [ "$elapsed" -lt "$ready_timeout" ]; do
		if grep -qF "Everything is ready" "$log" 2>/dev/null; then
			ready=true
			break
		fi
		# A collector that has already exited will never become ready, and waiting
		# out the timeout would turn a fast, clear failure into a slow one.
		kill -0 "$runner" 2>/dev/null || break
		sleep 1
		elapsed=$((elapsed + 1))
	done

	docker rm -f "$name" >/dev/null 2>&1
	kill "$runner" >/dev/null 2>&1
	wait "$runner" 2>/dev/null

	if [ "$ready" = true ]; then
		ok "$label starts on $image"
		return 0
	fi
	bad "$label did not start on $image, so what a Rollout would deploy does not run"
	sed 's/^/       /' "$log"
	return 1
}

# ---------------------------------------------------------------------------
# The Agents: every compiled configuration the Fleet ships, on core.
# ---------------------------------------------------------------------------

printf '\n\033[1m== The Fleet'"'"'s compiled Agent configuration, on the core distribution\033[0m\n'

agents=()
while IFS= read -r config; do agents+=("$config"); done < <(find "$fleet/compiled" -maxdepth 1 -name '*.yaml' 2>/dev/null | sort)

# A Distribution Check that checked nothing must not read as a pass. A moved
# directory or a Fleet that failed to compile would otherwise turn CI green on the
# strength of an empty loop — the same vacuous-success failure the Rollout Manifest
# exists to prevent.
if [ "${#agents[@]}" -eq 0 ]; then
	bad "there is no compiled collector configuration under $fleet/compiled, so this check verified nothing"
else
	for config in "${agents[@]}"; do
		checkOn "$(basename "$config")" "$config" "$COLLECTOR_CORE_IMAGE"
	done
fi

# ---------------------------------------------------------------------------
# The Gateway: compiled here, because it is not committed.
# ---------------------------------------------------------------------------

printf '\n\033[1m== The shared Gateway, on the contrib distribution\033[0m\n'

gateway="$work/gateway.yaml"
if ! "$GUARDRAIL" gateway >"$gateway" 2>"$work/gateway.err"; then
	bad "the Gateway Declaration does not compile, so there is nothing to check"
	sed 's/^/       /' "$work/gateway.err"
else
	checkOn "gateway.yaml" "$gateway" "$COLLECTOR_CONTRIB_IMAGE"
fi

# ---------------------------------------------------------------------------

if [ "$failed" -eq 0 ]; then
	printf '\n\033[32mEvery compiled collector configuration is accepted and started by the distribution it runs on.\033[0m\n'
	exit 0
fi
printf '\n\033[31m%d compiled collector configuration(s) a real collector will not run.\033[0m\n' "$failed"
exit 1
