# S1 — every service must declare the org's required resource attributes.
#
# This policy does NOT define which attributes those are, nor how severely S1
# enforces. It reads both from the data document `data.otel.standards.S1`, which
# the Guardrail builds from guardrail/standards.yaml — the single catalog shared
# with the Control Plane, which compiles the same entry into a Pipeline Guardrail
# at the Gateway (ADR 0015). Naming an attribute or a Severity here would create a
# second definition that drifts silently, so a test asserts no .rego file mentions
# one. Documented in docs/standards.md.
package otel.guardrail.standards.s1

standard := data.otel.standards.S1

violation contains v if {
	some attribute in standard.requires.resource_attributes
	not input.resource_attributes[attribute]
	v := {
		"standard": "S1",
		"severity": standard.severity,
		"message": sprintf("required resource attribute %q is not declared", [attribute]),
	}
}
