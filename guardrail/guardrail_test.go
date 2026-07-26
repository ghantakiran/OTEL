package guardrail_test

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

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

func TestAStandardDeclaresTheSeverityOfTheViolationItEmits(t *testing.T) {
	violations := check(t, "examples/missing-attributes-contract.yaml")

	for _, v := range violations {
		if v.Standard != "S1" {
			continue
		}
		if v.Severity != guardrail.SeverityBlock {
			t.Fatalf("S1 violation Severity = %q, want %q", v.Severity, guardrail.SeverityBlock)
		}
		return
	}
	t.Fatalf("no S1 violation to read a Severity from, got %+v", violations)
}

func TestCheckFailsTheBuildForABlockSeverityViolation(t *testing.T) {
	result := checkContract(t, "examples/missing-attributes-contract.yaml")

	if !result.FailsTheBuild() {
		t.Fatalf("a block Severity violation did not fail the build: %+v", result.Violations)
	}
}

func TestCheckDoesNotFailTheBuildForAWarnSeverityViolation(t *testing.T) {
	result := checkContract(t, "examples/missing-recommended-attributes-contract.yaml")

	if !reports(result.Violations, "S3", "service.namespace") {
		t.Fatalf("want an S3 violation naming service.namespace, got %+v", result.Violations)
	}
	if result.FailsTheBuild() {
		t.Fatalf("a warn Severity violation failed the build: %+v", result.Violations)
	}
}

func TestCheckRefusesAStandardThatDeclaresNoSeverity(t *testing.T) {
	preflight := preflightOver(t, `package otel.guardrail.standards.sx

violation contains v if {
	v := {"standard": "SX", "message": "SX is always violated"}
}
`)

	_, err := preflight.Check(context.Background(), contract.Contract{ServiceName: "any-service"})

	if err == nil {
		t.Fatal("a Standard that declares no Severity was accepted; it must be a loud error")
	}
	if !strings.Contains(err.Error(), "SX") || !strings.Contains(err.Error(), "Severity") {
		t.Errorf("error does not name the Standard and the missing Severity: %v", err)
	}
}

func TestCheckRefusesAStandardThatDeclaresAnUnrecognisedSeverity(t *testing.T) {
	preflight := preflightOver(t, `package otel.guardrail.standards.sy

violation contains v if {
	v := {"standard": "SY", "severity": "critical", "message": "SY is always violated"}
}
`)

	_, err := preflight.Check(context.Background(), contract.Contract{ServiceName: "any-service"})

	if err == nil {
		t.Fatal("a Standard declaring Severity \"critical\" was accepted; it must be a loud error")
	}
	if !strings.Contains(err.Error(), "SY") || !strings.Contains(err.Error(), "critical") {
		t.Errorf("error does not name the Standard and the bad Severity: %v", err)
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

// preflightOver builds a Preflight Guardrail whose catalog is the real
// aggregator plus one hand-written Standard, so a test can exercise a Standard
// the shipped catalog would never contain.
func preflightOver(t *testing.T, standard string) *guardrail.Preflight {
	t.Helper()

	aggregator, err := fs.ReadFile(guardrail.StandardPolicies(), "guardrail.rego")
	if err != nil {
		t.Fatalf("read aggregator: %v", err)
	}
	preflight, err := guardrail.NewPreflight(fstest.MapFS{
		"guardrail.rego": {Data: aggregator},
		"standard.rego":  {Data: []byte(standard)},
	})
	if err != nil {
		t.Fatalf("new Preflight Guardrail: %v", err)
	}
	return preflight
}

func check(t *testing.T, contractPath string) []guardrail.Violation {
	t.Helper()

	return checkContract(t, contractPath).Violations
}

// checkContract runs the shipped Standard catalog over a Telemetry Contract.
func checkContract(t *testing.T, contractPath string) guardrail.Result {
	t.Helper()

	c, err := contract.Load(contractPath)
	if err != nil {
		t.Fatalf("load Contract: %v", err)
	}
	preflight, err := guardrail.NewPreflight(guardrail.StandardPolicies())
	if err != nil {
		t.Fatalf("new Preflight Guardrail: %v", err)
	}
	result, err := preflight.Check(context.Background(), c)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	return result
}
