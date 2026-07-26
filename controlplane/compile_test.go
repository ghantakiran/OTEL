package controlplane_test

import (
	"strings"
	"testing"

	"github.com/ghantakiran/OTEL/contract"
	"github.com/ghantakiran/OTEL/controlplane"
	"github.com/ghantakiran/OTEL/guardrail"
)

func TestCompileCollectsEverySignalTheContractDeclares(t *testing.T) {
	config, err := controlplane.Compile(tierOneService(), taxonomy(t), profiles(t))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	for _, signal := range []string{"traces", "metrics", "logs"} {
		if !config.Collects(signal) {
			t.Errorf("the compiled config has no pipeline for the %s Signal", signal)
		}
	}
}

func TestCompileStampsTheResourceAttributesTheContractDeclares(t *testing.T) {
	// This is what makes "declared equals deployed" true by construction: the same
	// Contract Preflight checked produces the running config, so the two cannot
	// drift into disagreement (ADR 0005).
	rendered := compiledYAML(t, tierOneService())

	for _, declared := range []string{"service.name", "deployment.environment", "2.4.1"} {
		if !strings.Contains(rendered, declared) {
			t.Errorf("the compiled config does not carry %q from the Telemetry Contract:\n%s", declared, rendered)
		}
	}
}

func TestCompileSendsTelemetryToTheGatewayTheProfileNamesAndNoBackend(t *testing.T) {
	// A service is Backend-agnostic (ADR 0007): its Agent forwards to the Gateway
	// and names no Backend. Fan-out is the Gateway's job and lands with C5.
	rendered := compiledYAML(t, tierOneService())

	profile, published := profiles(t).For("tier-1")
	if !published {
		t.Fatal("tier-1 has no Pipeline Profile")
	}
	if !strings.Contains(rendered, profile.GatewayEndpoint) {
		t.Errorf("the compiled config does not forward to the Gateway the Profile names:\n%s", rendered)
	}
}

func TestCompileRefusesAContractMissingASignalItsTierMandates(t *testing.T) {
	// A pipeline missing a mandated Signal means the fleet quietly not collecting
	// telemetry the org requires, while the Contract still reads as compliant.
	tierOneWithoutMetrics := tierOneService()
	tierOneWithoutMetrics.Signals = []string{"traces", "logs"}

	err := mustNotCompile(t, tierOneWithoutMetrics, "tier-1 mandates metrics")

	if !strings.Contains(err.Error(), "metrics") {
		t.Errorf("the error does not name the missing Signal: %v", err)
	}
}

func TestCompileRefusesAContractDeclaringSomethingThatIsNotASignal(t *testing.T) {
	typo := tierOneService()
	typo.Signals = []string{"traces", "metricks", "logs"}

	err := mustNotCompile(t, typo, "metricks is not a Signal")

	if !strings.Contains(err.Error(), "metricks") {
		t.Errorf("the error does not name what it could not understand: %v", err)
	}
}

func TestCompileRefusesAServiceTierOutsideTheTaxonomy(t *testing.T) {
	invented := tierOneService()
	invented.Tier = "tier-platinum"

	err := mustNotCompile(t, invented, "tier-platinum is not a Service Tier")

	if !strings.Contains(err.Error(), "tier-platinum") {
		t.Errorf("the error does not name the tier: %v", err)
	}
}

func TestCompileRefusesATierWithNoPublishedPipelineProfile(t *testing.T) {
	// Every tier the org ships now has a Profile (C2), so this needs a tier that
	// genuinely has none: one the taxonomy defines and the Profiles do not cover.
	// That is the state a tier is in between being added to the taxonomy and having
	// its pipeline shape decided, and compiling then would put a pipeline on the
	// fleet that nobody chose.
	taxonomyKnowingAnExtraTier := taxonomyFrom(t, `apiVersion: guardrail.otel/v1
kind: ServiceTierTaxonomy
tiers:
  - tier: tier-unprofiled
    criticality: Added to the taxonomy before its pipeline shape was decided.
    mandatory_signals: [traces]
`)
	unprofiled := tierOneService()
	unprofiled.Tier = "tier-unprofiled"
	unprofiled.Signals = []string{"traces"}

	_, err := controlplane.Compile(unprofiled, taxonomyKnowingAnExtraTier, profiles(t))
	if err == nil {
		t.Fatal("a Contract on a tier with no Pipeline Profile compiled anyway")
	}
	if !strings.Contains(err.Error(), "tier-unprofiled") || !strings.Contains(strings.ToLower(err.Error()), "profile") {
		t.Errorf("the error does not say which tier lacks a Profile: %v", err)
	}
}

func TestACompiledConfigIsInternallyCoherent(t *testing.T) {
	config, err := controlplane.Compile(tierOneService(), taxonomy(t), profiles(t))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	if err := config.Validate(); err != nil {
		t.Errorf("the compiled config does not validate: %v", err)
	}
}

func tierOneService() contract.Contract {
	return contract.Contract{
		APIVersion:  contract.APIVersion,
		Kind:        contract.Kind,
		ServiceName: "checkout-api",
		Owner:       "team-payments",
		Tier:        "tier-1",
		Signals:     []string{"traces", "metrics", "logs"},
		ResourceAttributes: map[string]string{
			"service.name":           "checkout-api",
			"service.version":        "2.4.1",
			"deployment.environment": "production",
		},
	}
}

func taxonomy(t *testing.T) *guardrail.Taxonomy {
	t.Helper()

	// The same taxonomy the Guardrails enforce — not a second copy. That is the
	// point of #28: one definition, two consumers.
	loaded, err := guardrail.CentralTaxonomy()
	if err != nil {
		t.Fatalf("load the Service Tier taxonomy: %v", err)
	}
	return loaded
}

func profiles(t *testing.T) *controlplane.ProfileSet {
	t.Helper()

	loaded, err := controlplane.CentralProfiles()
	if err != nil {
		t.Fatalf("load the Pipeline Profiles: %v", err)
	}
	return loaded
}

func compiledYAML(t *testing.T, c contract.Contract) string {
	t.Helper()

	config, err := controlplane.Compile(c, taxonomy(t), profiles(t))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	rendered, err := config.YAML()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return string(rendered)
}

func mustNotCompile(t *testing.T, c contract.Contract, because string) error {
	t.Helper()

	_, err := controlplane.Compile(c, taxonomy(t), profiles(t))
	if err == nil {
		t.Fatalf("a Contract compiled that should not have: %s", because)
	}
	if strings.TrimSpace(err.Error()) == "" {
		t.Fatal("compile failed with an empty message")
	}
	return err
}
