// Standard S2 — a Service Tier determines which Signals are mandatory.
// The taxonomy under test is documented in docs/service-tiers.md.
// The check and reports helpers live in guardrail_test.go.
package guardrail_test

import (
	"testing"

	"github.com/ghantakiran/OTEL/guardrail"
)

func TestPreflightReportsATierOneContractMissingMetrics(t *testing.T) {
	violations := check(t, "examples/tier-1-missing-signal-contract.yaml")

	if !reports(violations, "S2", "metrics") {
		t.Fatalf("want an S2 violation naming the metrics Signal, got %+v", violations)
	}
}

func TestPreflightReportsATierTwoContractMissingMetrics(t *testing.T) {
	violations := check(t, "examples/tier-2-missing-signal-contract.yaml")

	if !reports(violations, "S2", "metrics") {
		t.Fatalf("want an S2 violation naming the metrics Signal, got %+v", violations)
	}
}

func TestPreflightReportsATierThreeContractMissingTraces(t *testing.T) {
	violations := check(t, "examples/tier-3-missing-signal-contract.yaml")

	if !reports(violations, "S2", "traces") {
		t.Fatalf("want an S2 violation naming the traces Signal, got %+v", violations)
	}
}

func TestPreflightReportsAContractDeclaringAServiceTierOutsideTheTaxonomy(t *testing.T) {
	violations := check(t, "examples/unknown-tier-contract.yaml")

	if !reports(violations, "S2", "tier-0") {
		t.Fatalf("want an S2 violation naming the unknown Service Tier, got %+v", violations)
	}
}

func TestS2BlocksTheBuild(t *testing.T) {
	violations := check(t, "examples/tier-1-missing-signal-contract.yaml")

	for _, v := range violations {
		if v.Standard == "S2" && v.Severity != guardrail.SeverityBlock {
			t.Fatalf("S2 declared Severity %q, want %q", v.Severity, guardrail.SeverityBlock)
		}
	}
}

// The taxonomy is graded: a lower Service Tier mandates strictly fewer Signals.

func TestPreflightDoesNotDemandLogsOfATierTwoContract(t *testing.T) {
	violations := check(t, "examples/tier-2-missing-signal-contract.yaml")

	if reports(violations, "S2", "logs") {
		t.Fatalf("tier-2 does not mandate the logs Signal, got %+v", violations)
	}
}

func TestPreflightPassesATierThreeContractDeclaringTracesAlone(t *testing.T) {
	violations := check(t, "examples/tier-3-least-signals-contract.yaml")

	// Scoped to S2 on purpose: this fixture pins the floor of the taxonomy, not
	// compliance with the whole catalog. Other Standards are free to have their
	// own opinion about it without making this test lie.
	for _, v := range violations {
		if v.Standard == "S2" {
			t.Fatalf("tier-3 mandates traces alone, got %+v", violations)
		}
	}
}
