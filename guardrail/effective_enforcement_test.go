package guardrail_test

import (
	"strings"
	"testing"

	"github.com/ghantakiran/OTEL/guardrail"
)

// Effective Enforcement is what actually happens to a violation on the day it is
// checked. It used to be computed in five places in four idioms, and the summary
// line got it wrong (see the Waiver mis-attribution fixed in #31) because it
// re-derived the answer from Result-wide sets rather than from each violation.
// These tests exercise the concept directly, so it is no longer only reachable
// by reading CLI output.

func waiverHold(t *testing.T, approver, until string) guardrail.Hold {
	t.Helper()

	day, err := guardrail.ParseDate(until)
	if err != nil {
		t.Fatalf("parse date: %v", err)
	}
	return guardrail.Hold{Kind: guardrail.HoldByWaiver, Until: day, ApprovedBy: approver}
}

func epochHold(t *testing.T, until string) guardrail.Hold {
	t.Helper()

	day, err := guardrail.ParseDate(until)
	if err != nil {
		t.Fatalf("parse date: %v", err)
	}
	return guardrail.Hold{Kind: guardrail.HoldByEpoch, Until: day}
}

func TestABlockingStandardWithNothingHoldingItBackFailsTheBuild(t *testing.T) {
	v := guardrail.Violation{Standard: "S1", Severity: guardrail.SeverityBlock}

	if got := v.Enforcement(); got != guardrail.EnforcementFailsBuild {
		t.Errorf("Enforcement() = %q, want %q", got, guardrail.EnforcementFailsBuild)
	}
}

func TestAWarnStandardIsAdvisoryNoMatterWhat(t *testing.T) {
	v := guardrail.Violation{Standard: "S3", Severity: guardrail.SeverityWarn}

	// A warn Standard was never going to fail the build, so nothing can "hold it
	// back" — that is the distinction the summary line previously lost.
	if got := v.Enforcement(); got != guardrail.EnforcementAdvisory {
		t.Errorf("Enforcement() = %q, want %q", got, guardrail.EnforcementAdvisory)
	}
}

func TestOverlappingHoldsAreBothNamedWithTheirOwnLapseDates(t *testing.T) {
	// A Waiver and the Enforcement Epoch can hold the same violation back, and
	// they lapse on different days. Reporting whichever was found first tells the
	// service the wrong date — which is the whole reason a violation carries a
	// list of Holds rather than two nullable fields somebody can forget to read.
	v := guardrail.Violation{
		Standard: "S1",
		Severity: guardrail.SeverityBlock,
		Holds: []guardrail.Hold{
			waiverHold(t, "obs-team", "2026-12-01"),
			epochHold(t, "2027-01-01"),
		},
	}

	if got := v.Enforcement(); got != guardrail.EnforcementHeldBack {
		t.Fatalf("Enforcement() = %q, want %q", got, guardrail.EnforcementHeldBack)
	}
	if got := len(v.HeldBackBy()); got != 2 {
		t.Fatalf("HeldBackBy() reports %d hold(s), want 2", got)
	}
	for _, day := range []string{"2026-12-01", "2027-01-01"} {
		if !strings.Contains(v.String(), day) {
			t.Errorf("the reported line does not name %s, so it gives the wrong date: %s", day, v)
		}
	}
}

func TestTheSummaryNamesOnlyHoldsThatAreActuallyHoldingSomethingBack(t *testing.T) {
	// The regression guard for the mis-attribution fixed in #31, now asserted
	// where the concept lives instead of by grepping CLI output. A Hold recorded
	// against an advisory finding must not reach the summary even if one somehow
	// exists — the summary reads held-back violations, not the whole Result.
	result := guardrail.Result{Violations: []guardrail.Violation{
		{Standard: "S1", Severity: guardrail.SeverityBlock, Holds: []guardrail.Hold{epochHold(t, "2027-01-01")}},
		{Standard: "S3", Severity: guardrail.SeverityWarn, Holds: []guardrail.Hold{waiverHold(t, "obs-team", "2027-06-01")}},
	}}

	if got := result.Describe(); got != string(guardrail.HoldByEpoch) {
		t.Errorf("the summary says %q; the Waiver is on an advisory finding and holds nothing back", got)
	}
}

func TestEveryViolationFallsInExactlyOneEnforcementGroup(t *testing.T) {
	// Result.With replaced five near-identical filters. If the groups ever stop
	// partitioning the violations, a caller counting them under-reports and the
	// summary line stops adding up.
	result := guardrail.Result{Violations: []guardrail.Violation{
		{Standard: "S1", Severity: guardrail.SeverityBlock},
		{Standard: "S2", Severity: guardrail.SeverityBlock, Holds: []guardrail.Hold{epochHold(t, "2027-04-01")}},
		{Standard: "S3", Severity: guardrail.SeverityWarn},
		{Standard: "S4", Severity: guardrail.SeverityInfo},
	}}

	grouped := 0
	for _, enforcement := range []guardrail.Enforcement{
		guardrail.EnforcementFailsBuild, guardrail.EnforcementHeldBack, guardrail.EnforcementAdvisory,
	} {
		grouped += len(result.With(enforcement))
	}

	if grouped != len(result.Violations) {
		t.Errorf("the enforcement groups cover %d of %d violations", grouped, len(result.Violations))
	}
	if !result.FailsTheBuild() {
		t.Error("a blocking Standard with nothing holding it back did not fail the build")
	}
}

func TestABlockingStandardHeldBackByOneHoldDoesNotFailTheBuild(t *testing.T) {
	v := guardrail.Violation{
		Standard: "S1",
		Severity: guardrail.SeverityBlock,
		Holds:    []guardrail.Hold{waiverHold(t, "obs-team", "2027-04-01")},
	}

	if got := v.Enforcement(); got != guardrail.EnforcementHeldBack {
		t.Errorf("Enforcement() = %q, want %q", got, guardrail.EnforcementHeldBack)
	}
}
