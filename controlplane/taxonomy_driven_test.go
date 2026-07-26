package controlplane_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ghantakiran/OTEL/contract"
	"github.com/ghantakiran/OTEL/controlplane"
	"github.com/ghantakiran/OTEL/guardrail"
)

// Compile reads the same Service Tier Taxonomy the Guardrails enforce (#28). These
// tests prove that by changing the taxonomy and watching the compiled config
// change — if the Control Plane ever grows its own copy of the tier list, they fail.

func TestRaisingATiersMandatorySignalsChangesWhatCompiles(t *testing.T) {
	// A tier-2 service emitting traces and metrics. Under the shipped taxonomy
	// that is exactly its floor, so it compiles. Add logs to the tier's mandatory
	// Signals and the same Contract must stop compiling — nothing else changed.
	service := contract.Contract{
		APIVersion: contract.APIVersion, Kind: contract.Kind,
		ServiceName: "inventory-api", Owner: "team-supply", Tier: "tier-2",
		Signals:            []string{"traces", "metrics"},
		ResourceAttributes: map[string]string{"service.name": "inventory-api"},
	}
	tierTwoProfiles := profilesFor(t, "tier-2")

	asShipped := taxonomyFrom(t, `apiVersion: guardrail.otel/v1
kind: ServiceTierTaxonomy
tiers:
  - tier: tier-2
    criticality: As the org ships it.
    mandatory_signals: [traces, metrics]
`)
	if _, err := controlplane.Compile(service, asShipped, tierTwoProfiles); err != nil {
		t.Fatalf("a Contract meeting its tier's floor did not compile: %v", err)
	}

	withLogsMandated := taxonomyFrom(t, `apiVersion: guardrail.otel/v1
kind: ServiceTierTaxonomy
tiers:
  - tier: tier-2
    criticality: With logs raised to mandatory.
    mandatory_signals: [traces, metrics, logs]
`)
	_, err := controlplane.Compile(service, withLogsMandated, tierTwoProfiles)
	if err == nil {
		t.Fatal("raising the tier's mandatory Signals did not change what compiles; the Control Plane is not reading the taxonomy")
	}
	if !strings.Contains(err.Error(), "logs") {
		t.Errorf("the error does not name the newly mandated Signal: %v", err)
	}
}

func TestATierKnownOnlyToTheTaxonomyCompilesOnceItHasAProfile(t *testing.T) {
	// A tier that appears in no Go source and no policy file. It compiles purely
	// because the taxonomy defines it and a Profile selects it — which is the whole
	// claim of #28, checked from the Control Plane's side.
	custom := taxonomyFrom(t, `apiVersion: guardrail.otel/v1
kind: ServiceTierTaxonomy
tiers:
  - tier: tier-experimental
    criticality: Defined nowhere but the taxonomy.
    mandatory_signals: [logs]
`)
	profileSet := profilesFor(t, "tier-experimental")

	service := contract.Contract{
		APIVersion: contract.APIVersion, Kind: contract.Kind,
		ServiceName: "experiment", Owner: "team-x", Tier: "tier-experimental",
		Signals:            []string{"logs"},
		ResourceAttributes: map[string]string{"service.name": "experiment"},
	}

	config, err := controlplane.Compile(service, custom, profileSet)
	if err != nil {
		t.Fatalf("a tier the taxonomy defines did not compile: %v", err)
	}
	if !config.Collects("logs") {
		t.Errorf("the compiled config does not collect the Signal the tier mandates: %v", config.Signals())
	}
	if config.Collects("traces") {
		t.Errorf("the compiled config collects a Signal the Contract never declared: %v", config.Signals())
	}
}

func TestEveryTierInTheTaxonomyEitherCompilesOrSaysWhyNot(t *testing.T) {
	// Walks the real taxonomy. A tier with a Profile must compile a coherent config;
	// a tier without one must fail saying so. Neither may produce a config quietly.
	realTaxonomy := taxonomy(t)
	realProfiles := profiles(t)

	for _, tier := range realTaxonomy.Tiers() {
		mandatory, _ := realTaxonomy.MandatorySignals(tier)
		service := contract.Contract{
			APIVersion: contract.APIVersion, Kind: contract.Kind,
			ServiceName: "conformant-" + tier, Owner: "team-x", Tier: tier,
			Signals:            mandatory,
			ResourceAttributes: map[string]string{"service.name": "conformant-" + tier},
		}

		config, err := controlplane.Compile(service, realTaxonomy, realProfiles)
		_, profiled := realProfiles.For(tier)

		switch {
		case profiled && err != nil:
			t.Errorf("%s has a Pipeline Profile but a conformant Contract did not compile: %v", tier, err)
		case profiled:
			if validateErr := config.Validate(); validateErr != nil {
				t.Errorf("%s compiled an incoherent config: %v", tier, validateErr)
			}
			for _, signal := range mandatory {
				if !config.Collects(signal) {
					t.Errorf("%s mandates %s but the compiled config does not collect it", tier, signal)
				}
			}
		case err == nil:
			t.Errorf("%s has no Pipeline Profile, yet a Contract compiled anyway", tier)
		case !strings.Contains(err.Error(), tier):
			t.Errorf("%s has no Profile and the error does not name it: %v", tier, err)
		}
	}
}

func taxonomyFrom(t *testing.T, body string) *guardrail.Taxonomy {
	t.Helper()

	path := filepath.Join(t.TempDir(), "tiers.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write taxonomy: %v", err)
	}
	loaded, err := guardrail.LoadTaxonomy(path)
	if err != nil {
		t.Fatalf("load taxonomy: %v", err)
	}
	return loaded
}

// profilesFor is a Profile set claiming the given tiers, so a test can compile a
// tier the org has not published a Profile for yet.
func profilesFor(t *testing.T, tiers ...string) *controlplane.ProfileSet {
	t.Helper()

	body := `apiVersion: guardrail.otel/v1
kind: PipelineProfileSet
profiles:
  - profile: under-test
    tiers: [` + strings.Join(tiers, ", ") + `]
    gateway_endpoint: gateway.test:4317
    batch:
      timeout: 5s
      send_batch_size: 8192
`
	path := filepath.Join(t.TempDir(), "profiles.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write Profiles: %v", err)
	}
	loaded, err := controlplane.LoadProfiles(path)
	if err != nil {
		t.Fatalf("load Profiles: %v", err)
	}
	return loaded
}
