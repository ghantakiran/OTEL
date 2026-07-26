# S3 — every service should declare the org's recommended resource attributes.
#
# These are not required to ship: a service is still operable without them, so
# S3 warns rather than blocks. They pay off during triage — `service.namespace`
# tells two same-named services in different systems apart, and
# `service.instance.id` tells one replica from another when only some are sick.
package otel.guardrail.standards.s3

recommended_attributes := {
	"service.namespace",
	"service.instance.id",
}

violation contains v if {
	some attribute in recommended_attributes
	not input.resource_attributes[attribute]
	v := {
		"standard": "S3",
		"severity": "warn",
		"message": sprintf("recommended resource attribute %q is not declared", [attribute]),
	}
}
