# S1 — every service must declare the org's required resource attributes.
package otel.guardrail.standards.s1

required_attributes := {
	"service.name",
	"service.version",
	"deployment.environment",
}

violation contains v if {
	some attribute in required_attributes
	not input.resource_attributes[attribute]
	v := {
		"standard": "S1",
		"severity": "block",
		"message": sprintf("required resource attribute %q is not declared", [attribute]),
	}
}
