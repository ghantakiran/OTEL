# S2 — a Service Tier determines which Signals are mandatory.
#
# A Telemetry Contract declares a Service Tier; that tier fixes the Signals the
# service must emit. Documented in docs/service-tiers.md.
package otel.guardrail.standards.s2

# The Service Tier taxonomy: tier identifier -> the Signals that tier mandates.
# The higher the criticality, the more Signals are mandatory.
mandatory_signals := {
	"tier-1": {"traces", "metrics", "logs"},
	"tier-2": {"traces", "metrics"},
	"tier-3": {"traces"},
}

known_tiers := sort(object.keys(mandatory_signals))

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
# demands is not observable enough to operate.
block(message) := {
	"standard": "S2",
	"severity": "block",
	"message": message,
}
