package guardrail

import (
	_ "embed"
	"fmt"
	"os"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/ghantakiran/OTEL/contract"
)

// The Service Tier taxonomy: which tiers exist, and which Signals each mandates.
//
// It used to live inside the S2 policy as a Rego map literal, which was correct
// while policy was the only consumer. It stopped being correct at the Control
// Plane: ADR 0005 keys Pipeline Profiles off these same identifiers, so the
// taxonomy would have existed twice, in two languages, failing independently —
// and silently, since a Contract declaring a tier the Guardrail knows but the
// Control Plane does not would pass Preflight and compile to the wrong pipeline.
//
// So the taxonomy is a document this package owns, and the S2 policy receives it
// as the Rego data document `data.otel.taxonomy`. Policy is a consumer of the
// taxonomy, never its definition — enforced by a test asserting no .rego file
// names a tier at all.

//go:embed tiers.yaml
var builtinTaxonomy []byte

// taxonomyKind is what a taxonomy file must declare itself to be.
const taxonomyKind = "ServiceTierTaxonomy"

// Taxonomy is the org's Service Tier taxonomy.
type Taxonomy struct {
	order            []string
	mandatorySignals map[string][]string
	criticality      map[string]string
}

type taxonomyDocument struct {
	APIVersion string          `yaml:"apiVersion"`
	Kind       string          `yaml:"kind"`
	Tiers      []taxonomyEntry `yaml:"tiers"`
}

type taxonomyEntry struct {
	Tier             string   `yaml:"tier"`
	Criticality      string   `yaml:"criticality"`
	MandatorySignals []string `yaml:"mandatory_signals"`
}

// CentralTaxonomy is the org's taxonomy, shipped in the binary from
// guardrail/tiers.yaml the same way the Standard catalog is.
func CentralTaxonomy() (*Taxonomy, error) {
	return parseTaxonomy(builtinTaxonomy, "guardrail/tiers.yaml")
}

// LoadTaxonomy reads a Service Tier taxonomy from a YAML file.
func LoadTaxonomy(path string) (*Taxonomy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Service Tier taxonomy %s: %w", path, err)
	}
	return parseTaxonomy(data, path)
}

func parseTaxonomy(data []byte, origin string) (*Taxonomy, error) {
	var document taxonomyDocument
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("parse Service Tier taxonomy %s: %w", origin, err)
	}
	if document.Kind != taxonomyKind {
		return nil, fmt.Errorf("Service Tier taxonomy %s: kind is %q, want %q", origin, document.Kind, taxonomyKind)
	}
	if err := contract.RequireAPIVersion(document.APIVersion, origin, "Service Tier taxonomy"); err != nil {
		return nil, err
	}
	// An empty taxonomy would make S2 reject every Contract as declaring an
	// unknown tier — the whole fleet blocked, from a file that parsed.
	if len(document.Tiers) == 0 {
		return nil, fmt.Errorf("Service Tier taxonomy %s defines no tiers: every Telemetry Contract would declare a tier outside the taxonomy", origin)
	}

	taxonomy := &Taxonomy{
		mandatorySignals: map[string][]string{},
		criticality:      map[string]string{},
	}
	for _, entry := range document.Tiers {
		if entry.Tier == "" {
			return nil, fmt.Errorf("Service Tier taxonomy %s has an entry naming no tier: set `tier:` on each", origin)
		}
		if _, twice := taxonomy.mandatorySignals[entry.Tier]; twice {
			return nil, fmt.Errorf("Service Tier taxonomy %s defines %s twice; one of the two would be ignored", origin, entry.Tier)
		}
		taxonomy.order = append(taxonomy.order, entry.Tier)
		taxonomy.mandatorySignals[entry.Tier] = entry.MandatorySignals
		taxonomy.criticality[entry.Tier] = entry.Criticality
	}
	return taxonomy, nil
}

// Tiers is every Service Tier in the taxonomy, in the order it declares them
// (most critical first).
func (t *Taxonomy) Tiers() []string {
	if t == nil {
		return nil
	}
	return t.order
}

// MandatorySignals is the Signals a Service Tier makes mandatory, and whether the
// tier is in the taxonomy at all. The Control Plane asks the same question when
// it picks a Pipeline Profile.
func (t *Taxonomy) MandatorySignals(tier string) ([]string, bool) {
	if t == nil {
		return nil, false
	}
	signals, known := t.mandatorySignals[tier]
	return signals, known
}

// Criticality is the prose description of what a Service Tier means.
func (t *Taxonomy) Criticality(tier string) string {
	if t == nil {
		return ""
	}
	return t.criticality[tier]
}

// asRegoData is the taxonomy in the shape S2 reads: tier -> mandatory Signals,
// plus a sorted tier list for the message naming the legal tiers.
func (t *Taxonomy) asRegoData() map[string]any {
	mandatory := make(map[string]any, len(t.mandatorySignals))
	for tier, signals := range t.mandatorySignals {
		asAny := make([]any, 0, len(signals))
		for _, signal := range signals {
			asAny = append(asAny, signal)
		}
		mandatory[tier] = asAny
	}

	tiers := append([]string(nil), t.order...)
	sort.Strings(tiers)
	known := make([]any, 0, len(tiers))
	for _, tier := range tiers {
		known = append(known, tier)
	}

	return map[string]any{"mandatory_signals": mandatory, "known_tiers": known}
}
