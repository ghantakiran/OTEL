package controlplane

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ghantakiran/OTEL/contract"
	"github.com/ghantakiran/OTEL/guardrail"
)

// Compile turns a Telemetry Contract into collector configuration for that
// service's Agent, by combining it with its Service Tier's Pipeline Profile
// (ADR 0005).
//
// It is the single entry point for both callers: the CLI's `compile` command and
// the Control Plane's fleet-wide rollout. Three inputs, each owning one fact —
// the Contract says what the service emits, the Taxonomy says what its tier must
// emit, the Profile says how it ships — and none of the three restates another.
//
// Compile refuses rather than guesses. A Contract that omits a Signal its tier
// mandates, names something that is not a Signal, declares a tier outside the
// taxonomy, or belongs to a tier with no published Profile does not compile: a
// config built on any of those would be quietly wrong on the fleet, which is
// worse than a build that stops.
func Compile(c contract.Contract, taxonomy *guardrail.Taxonomy, profiles *ProfileSet) (CollectorConfig, error) {
	mandatory, known := taxonomy.MandatorySignals(c.Tier)
	if !known {
		return CollectorConfig{}, fmt.Errorf(
			"cannot compile %s: Service Tier %q is not in the Service Tier Taxonomy, so there is no telemetry floor to compile against (declare one of %v)",
			c.ServiceName, c.Tier, taxonomy.Tiers())
	}

	profile, published := profiles.For(c.Tier)
	if !published {
		return CollectorConfig{}, fmt.Errorf(
			"cannot compile %s: no Pipeline Profile is published for Service Tier %q, so how its telemetry ships is undecided (profiled tiers: %v)",
			c.ServiceName, c.Tier, sorted(profiles.ProfiledTiers()))
	}

	declared, err := signalsOf(c)
	if err != nil {
		return CollectorConfig{}, fmt.Errorf("cannot compile %s: %w", c.ServiceName, err)
	}

	// The compiled config must route every Signal the tier mandates. A pipeline
	// missing one would mean the fleet quietly not collecting telemetry the org
	// requires — and the Contract would still read as compliant.
	if missing := missingFrom(declared, mandatory); len(missing) > 0 {
		return CollectorConfig{}, fmt.Errorf(
			"cannot compile %s: Service Tier %s mandates the %s Signal(s), which the Telemetry Contract does not declare",
			c.ServiceName, c.Tier, strings.Join(missing, ", "))
	}

	return assemble(c, profile, declared), nil
}

// signalsOf is the Contract's declared Signals, rejecting anything that is not
// one. A typo like `metricks` would otherwise compile to a pipeline for a Signal
// no collector has ever heard of.
func signalsOf(c contract.Contract) ([]contract.Signal, error) {
	seen := map[contract.Signal]bool{}
	signals := make([]contract.Signal, 0, len(c.Signals))

	for _, declared := range c.Signals {
		signal, err := contract.ParseSignal(declared)
		if err != nil {
			return nil, err
		}
		if seen[signal] {
			continue
		}
		seen[signal] = true
		signals = append(signals, signal)
	}
	return signals, nil
}

// missingFrom is the mandatory Signals the Contract does not declare.
func missingFrom(declared []contract.Signal, mandatory []string) []string {
	has := map[string]bool{}
	for _, signal := range declared {
		has[string(signal)] = true
	}

	var missing []string
	for _, signal := range mandatory {
		if !has[signal] {
			missing = append(missing, signal)
		}
	}
	sort.Strings(missing)
	return missing
}

func sorted(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}
