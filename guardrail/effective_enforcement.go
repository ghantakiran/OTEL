package guardrail

import (
	"fmt"
	"strings"
)

// Effective Enforcement is what actually happens to a violated Standard on the
// day it is checked: the Severity the Standard declared, adjusted by anything
// holding it back. It is not the same question as "how severe is this Standard" —
// a blocking Standard held back by a Waiver is still a blocking Standard.
//
// This file exists because that question used to be answered in five places in
// four idioms: a display helper, a boolean predicate built on top of the display
// helper, two nil-checks, and a set-membership count in the command surface. The
// last of those got it wrong — it named a Waiver that was holding nothing back,
// pointing the reader at the wrong expiry date. One answer, one place.

// Enforcement is the effective enforcement of one violation.
type Enforcement string

const (
	// EnforcementFailsBuild is a blocking Standard with nothing holding it back.
	EnforcementFailsBuild Enforcement = "fails the build"
	// EnforcementHeldBack is a blocking Standard that would fail the build but for
	// a Hold. It breaks the build on a known future day with nobody deciding
	// anything further, which is why it is reported apart from an advisory finding.
	EnforcementHeldBack Enforcement = "held back"
	// EnforcementAdvisory is a Standard that never fails a build on its own
	// Severity — info or warn. Nothing can hold it back, because there is nothing
	// to hold: this is the distinction the summary line used to lose.
	EnforcementAdvisory Enforcement = "advisory"
)

// HoldKind is what kind of thing is holding a blocking Standard back. The two
// are independent and can apply at once, and they lapse on different days.
type HoldKind string

const (
	HoldByWaiver HoldKind = "a Waiver"
	HoldByEpoch  HoldKind = "the Enforcement Epoch"
)

// Hold is one thing holding a blocking Standard back, and the day it stops.
//
// Modelling both a Waiver and the Enforcement Epoch's legacy deferral as the
// same shape is the point: every caller that used to ask "is this waived?" and
// "is this deferred?" separately — and forget one — now asks one question and
// gets everything, each carrying its own lapse date.
type Hold struct {
	Kind HoldKind `json:"kind"`
	// Until is the last day this Hold applies. After it, enforcement reverts with
	// nobody taking an action.
	Until Date `json:"until"`
	// ApprovedBy is who approved a Waiver. Empty for the Enforcement Epoch, which
	// nobody files: a legacy service gets it for free.
	ApprovedBy string `json:"approved_by,omitempty"`
}

// Lapses is the day this Hold stops applying, for a caller that wants to sort or
// compare rather than print.
func (h Hold) Lapses() Date { return h.Until }

func (h Hold) String() string {
	if h.Kind == HoldByWaiver {
		return fmt.Sprintf("waived by %s until %s", h.ApprovedBy, h.Until)
	}
	return fmt.Sprintf("legacy service, blocks from %s", h.Until)
}

// waiverHold is the Hold a Waiver puts on a violation.
func waiverHold(w Waiver) Hold {
	return Hold{Kind: HoldByWaiver, Until: w.Expires, ApprovedBy: w.ApprovedBy}
}

// graceHold is the Hold the Enforcement Epoch puts on a legacy service's
// violation until the Standard graduates.
func graceHold(g LegacyGrace) Hold {
	return Hold{Kind: HoldByEpoch, Until: g.Graduates}
}

// Enforcement is the single computation of effective enforcement. Every caller —
// the CLI's exit code, its summary line, the CI action, the Control Plane later —
// goes through here rather than re-deriving it from severities and nil checks.
func (v Violation) Enforcement() Enforcement {
	if v.Severity != SeverityBlock {
		return EnforcementAdvisory
	}
	if len(v.Holds) > 0 {
		return EnforcementHeldBack
	}
	return EnforcementFailsBuild
}

// FailsTheBuild reports whether this one violation stops the pipeline.
func (v Violation) FailsTheBuild() bool {
	return v.Enforcement() == EnforcementFailsBuild
}

// HeldBackBy is every Hold keeping this violation from failing the build, in the
// order they were applied. Empty unless Enforcement is EnforcementHeldBack.
func (v Violation) HeldBackBy() []Hold {
	if v.Enforcement() != EnforcementHeldBack {
		return nil
	}
	return v.Holds
}

// hold records a Hold against this violation, if there is enforcement to hold.
// A Standard that never fails the build cannot be held back — recording a Hold
// against one is what made the report name a Waiver that was doing nothing.
func (v *Violation) hold(h Hold) {
	if v.Severity != SeverityBlock {
		return
	}
	v.Holds = append(v.Holds, h)
}

// With is every violation whose effective enforcement is the given one. It
// replaces the five near-identical filters that used to hang off Result.
func (r Result) With(enforcement Enforcement) []Violation {
	var selected []Violation
	for _, v := range r.Violations {
		if v.Enforcement() == enforcement {
			selected = append(selected, v)
		}
	}
	return selected
}

// FailsTheBuild reports whether any violated Standard stops the pipeline. It is
// the only question CI has to ask.
func (r Result) FailsTheBuild() bool {
	return len(r.With(EnforcementFailsBuild)) > 0
}

// HoldKinds is the distinct kinds of Hold actually holding a blocking Standard
// back, in a stable order.
//
// Derived from the held-back violations alone, which is the structural fix for
// the mis-attribution in #31: a Hold on a violation that was never going to fail
// the build cannot reach the summary, because such a Hold is never recorded and
// would not be read from here even if it were.
func (r Result) HoldKinds() []HoldKind {
	seen := map[HoldKind]bool{}
	for _, v := range r.With(EnforcementHeldBack) {
		for _, h := range v.HeldBackBy() {
			seen[h.Kind] = true
		}
	}

	var kinds []HoldKind
	for _, kind := range []HoldKind{HoldByWaiver, HoldByEpoch} {
		if seen[kind] {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// Describe names what is holding blocking Standards back, for a reader who needs
// to know which clock to watch: "a Waiver and the Enforcement Epoch".
func (r Result) Describe() string {
	kinds := r.HoldKinds()
	names := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		names = append(names, string(kind))
	}
	return strings.Join(names, " and ")
}
