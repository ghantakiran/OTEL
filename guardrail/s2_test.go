// Standard S2 — a Service Tier determines which Signals are mandatory.
// The taxonomy under test is documented in docs/service-tiers.md.
// The check and reports helpers live in guardrail_test.go.
package guardrail_test

import "testing"

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

// The taxonomy is graded: a lower Service Tier mandates strictly fewer Signals.

func TestPreflightDoesNotDemandLogsOfATierTwoContract(t *testing.T) {
	violations := check(t, "examples/tier-2-missing-signal-contract.yaml")

	if reports(violations, "S2", "logs") {
		t.Fatalf("tier-2 does not mandate the logs Signal, got %+v", violations)
	}
}

func TestPreflightPassesATierThreeContractDeclaringTracesAlone(t *testing.T) {
	violations := check(t, "examples/tier-3-least-signals-contract.yaml")

	if len(violations) != 0 {
		t.Fatalf("tier-3 mandates traces alone, got %+v", violations)
	}
}
