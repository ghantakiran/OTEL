package guardrail_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ghantakiran/OTEL/contract"
	"github.com/ghantakiran/OTEL/guardrail"
)

func TestPreflightPassesCompliantContract(t *testing.T) {
	violations := check(t, "examples/compliant-contract.yaml")

	if len(violations) != 0 {
		t.Fatalf("compliant Contract produced violations: %+v", violations)
	}
}

func TestPreflightReportsUndeclaredRequiredResourceAttribute(t *testing.T) {
	violations := check(t, "examples/missing-attributes-contract.yaml")

	if !reports(violations, "S1", "deployment.environment") {
		t.Fatalf("want an S1 violation naming deployment.environment, got %+v", violations)
	}
}

// reports says whether the Standard flagged something, and named it.
func reports(violations []guardrail.Violation, standard, subject string) bool {
	for _, v := range violations {
		if v.Standard == standard && strings.Contains(v.Message, subject) {
			return true
		}
	}
	return false
}

func check(t *testing.T, contractPath string) []guardrail.Violation {
	t.Helper()

	c, err := contract.Load(contractPath)
	if err != nil {
		t.Fatalf("load Contract: %v", err)
	}
	preflight, err := guardrail.NewPreflight(guardrail.StandardPolicies())
	if err != nil {
		t.Fatalf("new Preflight Guardrail: %v", err)
	}
	violations, err := preflight.Check(context.Background(), c)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	return violations
}
