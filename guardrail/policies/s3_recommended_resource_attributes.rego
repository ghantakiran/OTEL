# S3 — every service should declare the org's recommended resource attributes.
#
# These are not required to ship: a service is still operable without them, so
# S3 warns rather than blocks. They pay off during triage — `service.namespace`
# tells two same-named services in different systems apart, and
# `service.instance.id` tells one replica from another when only some are sick.
#
# Which attributes, and at what Severity, come from `data.otel.standards.S3` —
# guardrail/standards.yaml, the one catalog both enforcement points read. See S1
# for why this policy names neither.
package otel.guardrail.standards.s3

standard := data.otel.standards.S3

violation contains v if {
	some attribute in standard.requires.resource_attributes
	not input.resource_attributes[attribute]
	v := {
		"standard": "S3",
		"severity": standard.severity,
		"message": sprintf("recommended resource attribute %q is not declared", [attribute]),
	}
}
