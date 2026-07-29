package copilot

// EVIDENCE IS SHAPED BY KIND, AND IDENTIFIED WITHOUT REGARD TO KIND.
//
// Those are two different jobs and this file is where they are separated.
//
// A ToolResult carries one TYPED FIELD PER KIND — Traces today; Metrics, Logs,
// Contract and Standards as their tools arrive (#17). It is not one collection
// of a union type, and the reason is the invariant #67 was written to defend:
// evidence must stay a typed value at every layer, storage included. A union in
// Go is either a struct of N pointers with a tag nothing enforces, or a kind
// plus a json.RawMessage — and a raw payload is one string() away from being
// indistinguishable from text the platform authored. A concrete slice of a
// concrete type cannot become that by accident.
//
// The cost of one-field-per-kind is that "all the evidence" is no longer one
// range. That cost is paid HERE, by Citation: everything that only needs to know
// WHICH evidence exists — provenance, the grounding index — reads Citations()
// and never learns there are kinds at all. Everything that needs the evidence
// ITSELF — the renderer, the transcript, the judge — reads the typed field and
// gets a concrete type.
//
// So the split is: kind-shaped where the content matters, kind-blind where only
// identity matters. Neither half needs a type switch.

// EvidenceKind names what a citation points at.
//
// It exists so that an ID is never ambiguous. A trace ID and a Standard's name
// are both strings, and a provenance check that compared bare strings would
// match a claim citing standard "S1" against a trace that happened to be called
// the same thing. Rare, and wrong in the direction that says "verified".
type EvidenceKind string

const (
	// KindTrace is a Trace Reference. The only kind P1 produces.
	KindTrace EvidenceKind = "trace"

	// The remaining kinds arrive with their tools (#17): query_metrics,
	// query_logs, get_contract and get_standards. They are deliberately NOT
	// declared here in advance — a constant with nothing producing it is a
	// promise the code does not keep, and the tripwire test in this package is
	// what makes adding one safe rather than a declaration made early.
)

// Citation identifies one piece of evidence, independent of what kind it is.
//
// Comparable on purpose: it is used as a map key by the grounding index, where
// the alternative is a composite string like "trace:abc…" that every reader has
// to parse back apart.
type Citation struct {
	// Kind is what this points at.
	Kind EvidenceKind
	// ID is the handle an operator can follow. For a trace it is the trace ID.
	ID string
}

// Citation is the handle a claim cites this trace by.
//
// A method on TraceRef rather than a field, because it is derived — a TraceRef
// that stored its own Citation could disagree with its own TraceID, and then
// there would be two answers to "which trace is this?".
func (r TraceRef) Citation() Citation {
	return Citation{Kind: KindTrace, ID: r.TraceID}
}
