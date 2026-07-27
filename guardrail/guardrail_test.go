package guardrail_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
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
	// The Severity belongs in the catalog and the policy reads it from there — but
	// a policy is Rego and can emit whatever it likes, so what it emits is still
	// validated where policy data becomes domain data. This is the shape that bug
	// takes in practice: an author who read the requirement from the catalog and
	// then wrote the violation object by hand.
	preflight := preflightOver(t, `package otel.guardrail.standards.sx

violation contains v if {
	some attribute in data.otel.standards.SX.requires.resource_attributes
	v := {"standard": "SX", "message": sprintf("SX requires %q", [attribute])}
}
`, catalogDeclaring("SX"))

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
	some attribute in data.otel.standards.SY.requires.resource_attributes
	v := {"standard": "SY", "severity": "critical", "message": sprintf("SY requires %q", [attribute])}
}
`, catalogDeclaring("SY"))

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

// preflightOver builds a Guardrail over one hand-written policy and a Standard
// catalog that matches it. Both are needed: a Standard is a policy AND a catalog
// entry, and NewPreflight refuses a pair that does not meet.
func preflightOver(t *testing.T, policy, catalog string) *guardrail.Preflight {
	t.Helper()

	aggregator, err := fs.ReadFile(guardrail.StandardPolicies(), "guardrail.rego")
	if err != nil {
		t.Fatalf("read aggregator: %v", err)
	}

	path := filepath.Join(t.TempDir(), "standards.yaml")
	if err := os.WriteFile(path, []byte(catalog), 0o600); err != nil {
		t.Fatalf("write Standard catalog: %v", err)
	}
	standards, err := guardrail.LoadStandards(path)
	if err != nil {
		t.Fatalf("load Standard catalog: %v", err)
	}

	preflight, err := guardrail.NewPreflight(fstest.MapFS{
		"guardrail.rego": {Data: aggregator},
		"standard.rego":  {Data: []byte(policy)},
	}, guardrail.WithStandardCatalog(standards))
	if err != nil {
		t.Fatalf("new Preflight Guardrail: %v", err)
	}
	return preflight
}

// catalogDeclaring is a one-entry Standard catalog for a hand-written policy.
func catalogDeclaring(id string) string {
	return `apiVersion: guardrail.otel/v1
kind: StandardCatalog
standards:
  - standard: ` + id + `
    title: A Standard written for one test.
    severity: block
    enforced_at: [preflight]
    requires:
      resource_attributes: [service.name]
`
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
