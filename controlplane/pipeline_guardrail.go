package controlplane

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/ghantakiran/OTEL/guardrail"
)

// Compiling a Pipeline Guardrail: the Standards the catalog marks
// `enforced_at: [pipeline]`, turned into collector processors that run in the
// Gateway (ADR 0015).
//
// A collector cannot evaluate Rego, so what runs here is not the Standard's Rego
// body — it is the same catalog entry the Rego reads, compiled a second way. The
// entry is the single definition; the .rego and this file are two consumers of
// it, and neither declares what a Standard requires. That is the whole reason
// guardrail/standards.yaml exists.
//
// WHAT THE ACTION IS, AND WHY IT IS NEVER A DROP
//
// CONTEXT.md says a Pipeline Guardrail acts by drop, tag or alert. Severity
// decides how loudly the violation is marked and never whether the telemetry
// survives: every Severity tags, none drops. Dropping would destroy the evidence
// of the violation at exactly the moment it is most wanted, and would make a
// non-compliant service indistinguishable from a service that is down — the
// failure mode this platform refuses everywhere else. The full argument, and the
// reading of `block` it rejects, is ADR 0015.
//
//	info | warn | block   set otel.guardrail.violation.<id> = the Severity
//	block                 additionally set otel.guardrail.blocking = true
//
// The roll-up exists so an operator has one low-cardinality field to count and
// alert on. It is written by every blocking Standard with the same value, so no
// statement order can change what an operator sees — and no violation can mask
// another, because each Standard writes its own key.

// pipelineGuardrailProcessor is the collector component ID for the Pipeline
// Guardrails. One processor, not one per Standard: it is a single pass over each
// record, and a Gateway pipeline listing eight processors named after Standards
// would make the order look meaningful when it is not.
const pipelineGuardrailProcessor = "transform/guardrail"

// The attribute names a Pipeline Guardrail writes. They are the platform's
// interface to an operator and to C7's self-observation (#15, ADR 0010), so they
// are named here once rather than formatted at each use.
const (
	// guardrailNamespace is the attribute namespace the Gateway's verdict occupies.
	// Everything under it is cleared before the verdict is written, so it belongs to
	// the Gateway and to nothing upstream of it.
	guardrailNamespace = "otel.guardrail."
	// violationAttributePrefix is completed by the Standard's id; the value is the
	// Severity that Standard declares. One key per Standard, so a record violating
	// two Standards names both and neither can overwrite the other.
	violationAttributePrefix = guardrailNamespace + "violation."
	// blockingAttribute is the roll-up: present and true when the record violated
	// at least one Standard whose Severity is `block`.
	blockingAttribute = guardrailNamespace + "blocking"
)

// clearPlatformNamespace deletes the platform's own namespace from a record.
//
// It is not a Guardrail verdict and it is not derived from any Standard — it is
// the same hygiene the verdict namespace gets, applied to the other namespace the
// Gateway must be able to trust. It rides in this processor rather than in one of
// its own because it is the same single pass over the same records, and a second
// transform would be a second component whose ordering relative to this one would
// then matter.
var clearPlatformNamespace = fmt.Sprintf(
	"delete_matching_keys(attributes, %q)", "^"+regexp.QuoteMeta(platformNamespace))

// pipelineGuardrails compiles the catalog's pipeline-enforced Standards into one
// transform processor, and reports whether there is anything to run. A catalog
// with no pipeline-enforced Standard compiles to no processor at all rather than
// an empty one: an empty transform would be a component the config defines and
// no pipeline could meaningfully use, and Validate would refuse it.
func pipelineGuardrails(catalog *guardrail.StandardCatalog) (map[string]any, bool) {
	enforced := catalog.PipelineEnforced()
	if len(enforced) == 0 {
		return nil, false
	}

	// FIRST, unconditionally: clear the whole verdict namespace. Nothing stops a
	// service emitting otel.guardrail.blocking=false itself, and the Agent forwards
	// whatever it is given — so a Guardrail that only ever `set`s on violation would
	// leave that forgery in place, and the one field an operator alerts on would be
	// the easiest thing in the platform to suppress. Clearing first makes "these
	// attributes are the Gateway's verdict" true by construction rather than by
	// everyone remembering not to write there.
	clear := fmt.Sprintf("delete_matching_keys(attributes, %q)", "^"+regexp.QuoteMeta(guardrailNamespace))

	statements := make([]any, 0, 2*len(enforced)+1)
	statements = append(statements, clear)

	for _, standard := range enforced {
		violated := violatedWhen(standard)
		statements = append(statements, fmt.Sprintf(
			"set(attributes[%q], %q) where %s",
			violationAttributePrefix+standard.ID, string(standard.Severity), violated))

		// `block` is the Severity that fails a build at Preflight. At the pipeline it
		// buys the record the one field an operator alerts on — not its deletion.
		if standard.Severity == guardrail.SeverityBlock {
			statements = append(statements, fmt.Sprintf(
				"set(attributes[%q], true) where %s", blockingAttribute, violated))
		}
	}

	// The verdict is written on the RESOURCE, and the same way for all three
	// Signals: the requirement is about the record's resource, and a resource is a
	// resource whether it carries spans, metrics or logs. A Standard enforced on
	// traces but not on logs would mean something different depending on what a
	// service happens to emit. One record, one verdict, in the place an operator
	// groups by service.
	//
	// The CLEARING has to go further. A service can put `otel.guardrail.blocking`
	// on a span, a datapoint or a log record just as easily as on its resource, and
	// most Backends flatten record and resource attributes into one queryable
	// field — so a forged record-level key would answer the operator's query just
	// as well. Every context that carries attributes is swept, or the namespace is
	// the Gateway's only where somebody happened to look.
	//
	// Each context gets its own statement slice: sharing one would make three keys
	// of the compiled config alias a single slice, and a caller appending to the
	// traces statements would silently edit metrics and logs too.
	return map[string]any{
		// A malformed record must not stop the Gateway relaying the fleet's
		// telemetry: a Guardrail that takes the pipeline down is worse than the
		// violation it was watching for. `ignore` logs the failure and carries on.
		"error_mode":        "ignore",
		"trace_statements":  sweptThenJudged(clear, statements, "span", "spanevent"),
		"metric_statements": sweptThenJudged(clear, statements, "datapoint"),
		"log_statements":    sweptThenJudged(clear, statements, "log"),
	}, true
}

// sweptThenJudged is one Signal's statement groups: the verdict on the resource,
// and the namespaces cleared in every other context that carries attributes.
func sweptThenJudged(clear string, verdict []any, records ...string) []any {
	groups := []any{map[string]any{
		"context":    "resource",
		"statements": append([]any(nil), verdict...),
	}}
	// `scope` carries attributes for every Signal; the rest are the Signal's own
	// record levels.
	for _, context := range append([]string{"scope"}, records...) {
		groups = append(groups, map[string]any{
			"context": context,
			// The verdict namespace, and — off the resource only — the namespace the
			// PLATFORM uses to describe its own collectors. The asymmetry is the point.
			// `otel.platform.config_version` on a resource is a collector's own stamp
			// arriving, and clearing it here would delete the answer a Rollout is
			// confirmed by; on a span, a datapoint, a log record or a scope it can only
			// have been put there by a service, and most Backends flatten record and
			// resource attributes into one queryable field, so it would answer an
			// operator's rollout query just as well as the real thing.
			"statements": []any{clear, clearPlatformNamespace},
		})
	}
	return groups
}

// violatedWhen is the OTTL condition under which a Standard is violated by the
// record in hand: any one of the resource attributes it requires is absent.
//
// The whole Standard is one condition rather than one statement per attribute so
// that the compiled config reads as "S1 was violated" rather than as three
// unrelated statements that happen to write the same key.
// The attribute name is written with %q, and that is load-bearing rather than
// cosmetic: the collector's OTTL parser unquotes string literals with
// strconv.Unquote, which is exactly the inverse of %q. The catalog additionally
// narrows an attribute name to characters that need no escaping at all, so the
// two never have to agree about an edge case — but if that charset is ever
// widened, this coupling is what it has to stay inside.
//
// CompileGateway refuses a pipeline-enforced Standard with no attributes, so the
// join here always has at least one term and the `where` is never left dangling.
func violatedWhen(standard guardrail.Standard) string {
	missing := make([]string, 0, len(standard.Requires.ResourceAttributes))
	for _, attribute := range standard.Requires.ResourceAttributes {
		missing = append(missing, fmt.Sprintf("attributes[%q] == nil", attribute))
	}
	return strings.Join(missing, " or ")
}
