# S2 — a Service Tier determines which Signals are mandatory.
#
# A Telemetry Contract declares a Service Tier; that tier fixes the Signals the
# service must emit.
#
# This policy does NOT define the taxonomy. It reads it from the data document
# data.otel.taxonomy, which the Guardrail builds from guardrail/tiers.yaml — the
# single source of truth shared with the Control Plane, which keys Pipeline
# Profiles off the same identifiers (ADR 0005). Naming a tier here would create a
# second definition that drifts silently, so a test asserts no .rego file
# mentions one. Documented in docs/service-tiers.md.
package otel.guardrail.standards.s2

mandatory_signals := data.otel.taxonomy.mandatory_signals

known_tiers := data.otel.taxonomy.known_tiers

declared_signals := {signal | some signal in input.signals}

violation contains v if {
	some signal in mandatory_signals[input.tier]
	not signal in declared_signals
	v := block(sprintf(
		"Service Tier %q mandates the %s Signal, which is not declared",
		[input.tier, signal],
	))
}

# A tier outside the taxonomy has no mandatory Signal set, so the rule above
# cannot fire. Blocking here keeps an unrecognised tier from passing silently.
violation contains v if {
	not mandatory_signals[input.tier]
	v := block(sprintf(
		"Service Tier %q is not in the taxonomy; declare one of %v",
		[input.tier, known_tiers],
	))
}

# S2 is a build-blocking Standard: a service emitting less than its criticality
# demands is not observable enough to operate. The Severity is read from the
# catalog, not written here, for the same reason the tiers are — see S1.
block(message) := {
	"standard": "S2",
	"severity": data.otel.standards.S2.severity,
	"message": message,
}
