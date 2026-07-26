# Aggregates every Standard in the catalog into one violation set.
# A Standard lives in its own package under data.otel.guardrail.standards and
# exposes a `violation` set; it is picked up here without registration.
package otel.guardrail

violations contains v if {
	some standard
	v := data.otel.guardrail.standards[standard].violation[_]
}
