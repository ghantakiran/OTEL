package guardrail_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ghantakiran/OTEL/contract"
	"github.com/ghantakiran/OTEL/guardrail"
)

// The Service Tier taxonomy used to live only inside the S2 policy, as a Rego
// map literal. The Control Plane keys Pipeline Profiles off the same three tier
// identifiers (ADR 0005), so it would have had to re-encode them — two copies
// that fail independently and silently. The taxonomy is now a document Go owns
// and hands to Rego as data: policy is a consumer of it, never its definition.

func TestTheTaxonomySaysWhichSignalsATierMandates(t *testing.T) {
	taxonomy, err := guardrail.CentralTaxonomy()
	if err != nil {
		t.Fatalf("the taxonomy shipped in the binary does not load: %v", err)
	}

	mandatory, known := taxonomy.MandatorySignals("tier-1")
	if !known {
		t.Fatal("tier-1 is not in the shipped taxonomy")
	}
	if len(mandatory) != 3 {
		t.Errorf("tier-1 mandates %v, want all three Signals", mandatory)
	}
	if _, known := taxonomy.MandatorySignals("tier-4"); known {
		t.Error("tier-4 is not a Service Tier, but the taxonomy claims to know it")
	}
}

func TestS2HonoursTheTaxonomyItIsGivenRatherThanOneOfItsOwn(t *testing.T) {
	// The load-bearing test for #28. A tier that exists in no Rego file is
	// accepted, and its mandatory Signals enforced, purely because the taxonomy
	// handed to the Guardrail says so. If S2 ever goes back to defining tiers
	// itself, this fails.
	custom := writeTaxonomy(t, `apiVersion: guardrail.otel/v1
kind: ServiceTierTaxonomy
tiers:
  - tier: tier-experimental
    criticality: A tier that appears in no policy file.
    mandatory_signals: [logs]
`)

	declaresLogs := contract.Contract{
		APIVersion: contract.APIVersion, Kind: contract.Kind,
		ServiceName: "experiment", Owner: "team-x", Tier: "tier-experimental",
		Signals: []string{"logs"},
		ResourceAttributes: map[string]string{
			"service.name": "experiment", "service.version": "1.0.0", "deployment.environment": "production",
			"service.namespace": "x", "service.instance.id": "experiment-0",
		},
	}
	declaresTracesOnly := declaresLogs
	declaresTracesOnly.Signals = []string{"traces"}

	if violations := checkAgainst(t, custom, declaresLogs); reports(violations, "S2", "tier-experimental") {
		t.Errorf("S2 rejected a tier the taxonomy defines: %+v", violations)
	}
	if violations := checkAgainst(t, custom, declaresTracesOnly); !reports(violations, "S2", "logs") {
		t.Errorf("S2 did not enforce the Signals the taxonomy makes mandatory: %+v", violations)
	}
}

func TestTheS2PolicyDoesNotDefineAnyServiceTierItself(t *testing.T) {
	// The guard against the taxonomy drifting back into policy. If someone adds a
	// tier to the Rego — or hardcodes one to "fix" something — Go and Rego would
	// disagree, and nothing else would notice.
	catalog, err := os.ReadDir("policies")
	if err != nil {
		t.Fatalf("read the Standard catalog: %v", err)
	}

	tierLiteral := regexp.MustCompile(`"tier-[^"]*"`)
	for _, entry := range catalog {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".rego") {
			continue
		}
		source, err := os.ReadFile(filepath.Join("policies", entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		if found := tierLiteral.FindAllString(string(source), -1); len(found) > 0 {
			t.Errorf("%s names Service Tiers directly (%s); the taxonomy is data the policy reads, not something it declares",
				entry.Name(), strings.Join(found, ", "))
		}
	}
}

func TestEveryTierInTheTaxonomyIsOneS2Accepts(t *testing.T) {
	// Consistency in the other direction: whatever the taxonomy publishes, S2 must
	// treat as a real tier and must enforce exactly its mandatory Signals.
	taxonomy, err := guardrail.CentralTaxonomy()
	if err != nil {
		t.Fatalf("load the taxonomy: %v", err)
	}

	for _, tier := range taxonomy.Tiers() {
		mandatory, _ := taxonomy.MandatorySignals(tier)
		declared := contract.Contract{
			APIVersion: contract.APIVersion, Kind: contract.Kind,
			ServiceName: "conformant", Owner: "team-x", Tier: tier, Signals: mandatory,
			ResourceAttributes: map[string]string{
				"service.name": "conformant", "service.version": "1.0.0", "deployment.environment": "production",
				"service.namespace": "x", "service.instance.id": "conformant-0",
			},
		}

		violations := checkAgainst(t, "", declared)
		for _, v := range violations {
			if v.Standard == "S2" {
				t.Errorf("%s declares exactly the Signals the taxonomy mandates, but S2 flagged it: %s", tier, v)
			}
		}
	}
}

// checkAgainst runs the Preflight Guardrail with a given taxonomy — the shipped
// one when path is empty.
func checkAgainst(t *testing.T, taxonomyPath string, c contract.Contract) []guardrail.Violation {
	t.Helper()

	var options []guardrail.Option
	if taxonomyPath != "" {
		taxonomy, err := guardrail.LoadTaxonomy(taxonomyPath)
		if err != nil {
			t.Fatalf("load taxonomy: %v", err)
		}
		options = append(options, guardrail.WithTaxonomy(taxonomy))
	}

	preflight, err := guardrail.NewPreflight(guardrail.StandardPolicies(), options...)
	if err != nil {
		t.Fatalf("new Preflight Guardrail: %v", err)
	}
	result, err := preflight.Check(context.Background(), c)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	return result.Violations
}

func writeTaxonomy(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "tiers.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write taxonomy: %v", err)
	}
	return path
}
