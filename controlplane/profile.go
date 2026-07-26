// Package controlplane turns Telemetry Contracts into running collector
// configuration. It Compiles a Contract (what a service emits) with its Service
// Tier's Pipeline Profile (how that telemetry ships) into a collector config,
// and reads the Service Tier Taxonomy the Guardrails already enforce rather than
// re-encoding it (ADR 0005, ADR 0007).
package controlplane

import (
	_ "embed"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/ghantakiran/OTEL/contract"
)

//go:embed profiles.yaml
var builtinProfiles []byte

// profileSetKind is what a Pipeline Profile file must declare itself to be.
const profileSetKind = "PipelineProfileSet"

// Profile is a named, org-owned template describing how telemetry ships for the
// Service Tiers that select it — batching, sampling, and where the Agent
// forwards to. It deliberately says nothing about which Signals a service must
// emit: that is the Service Tier Taxonomy's job, and duplicating it here would
// give one fact two homes that drift.
type Profile struct {
	Name            string   `yaml:"profile"`
	Tiers           []string `yaml:"tiers"`
	Description     string   `yaml:"description"`
	GatewayEndpoint string   `yaml:"gateway_endpoint"`
	Batch           Batch    `yaml:"batch"`
	Sampling        Sampling `yaml:"sampling"`
}

// Batch is how telemetry is grouped before it leaves the Agent.
type Batch struct {
	Timeout       string `yaml:"timeout"`
	SendBatchSize int    `yaml:"send_batch_size"`
}

// Sampling is what the Agent drops before forwarding. The Gateway tail-samples
// with the whole trace in hand, so an Agent sampling below 100% is a deliberate
// cost decision recorded in the Profile, never a per-service choice.
type Sampling struct {
	TracesPercent int `yaml:"traces_percent"`
}

// ProfileSet is the org's Pipeline Profiles, indexed by the Service Tier that
// selects each one.
type ProfileSet struct {
	order      []string
	byName     map[string]Profile
	byTier     map[string]Profile
	profiledBy map[string]string
}

type profileSetDocument struct {
	APIVersion string    `yaml:"apiVersion"`
	Kind       string    `yaml:"kind"`
	Profiles   []Profile `yaml:"profiles"`
}

// CentralProfiles is the org's Profile set, shipped in the binary the same way
// the Standard catalog and the taxonomy are (ADR 0004: git is the source of
// truth, nothing is fetched at run time).
func CentralProfiles() (*ProfileSet, error) {
	return parseProfileSet(builtinProfiles, "controlplane/profiles.yaml")
}

// LoadProfiles reads a Pipeline Profile set from a YAML file.
func LoadProfiles(path string) (*ProfileSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Pipeline Profiles %s: %w", path, err)
	}
	return parseProfileSet(data, path)
}

func parseProfileSet(data []byte, origin string) (*ProfileSet, error) {
	var document profileSetDocument
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("parse Pipeline Profiles %s: %w", origin, err)
	}
	if document.Kind != profileSetKind {
		return nil, fmt.Errorf("Pipeline Profiles %s: kind is %q, want %q", origin, document.Kind, profileSetKind)
	}
	if err := contract.RequireAPIVersion(document.APIVersion, origin, "Pipeline Profile set"); err != nil {
		return nil, err
	}

	set := &ProfileSet{
		byName:     map[string]Profile{},
		byTier:     map[string]Profile{},
		profiledBy: map[string]string{},
	}
	for _, profile := range document.Profiles {
		if profile.Name == "" {
			return nil, fmt.Errorf("Pipeline Profiles %s has a Profile with no name: set `profile:` on each", origin)
		}
		if _, twice := set.byName[profile.Name]; twice {
			return nil, fmt.Errorf("Pipeline Profiles %s defines Profile %q twice; one would be ignored", origin, profile.Name)
		}
		if profile.GatewayEndpoint == "" {
			return nil, fmt.Errorf("Pipeline Profiles %s: Profile %q names no gateway_endpoint, so a compiled Agent would have nowhere to forward to", origin, profile.Name)
		}
		if len(profile.Tiers) == 0 {
			return nil, fmt.Errorf("Pipeline Profiles %s: Profile %q is selected by no Service Tier, so nothing would ever compile with it", origin, profile.Name)
		}

		set.order = append(set.order, profile.Name)
		set.byName[profile.Name] = profile
		for _, tier := range profile.Tiers {
			// Two Profiles claiming one tier means the tier's pipeline shape depends
			// on file order, which is exactly the silent drift ADR 0005 removes.
			if existing, taken := set.profiledBy[tier]; taken {
				return nil, fmt.Errorf("Pipeline Profiles %s: Service Tier %s is claimed by both %q and %q; a tier selects exactly one Profile",
					origin, tier, existing, profile.Name)
			}
			set.profiledBy[tier] = profile.Name
			set.byTier[tier] = profile
		}
	}
	return set, nil
}

// For is the Profile a Service Tier selects, and whether the org has published
// one for that tier at all.
func (s *ProfileSet) For(tier string) (Profile, bool) {
	if s == nil {
		return Profile{}, false
	}
	profile, published := s.byTier[tier]
	return profile, published
}

// Names is every Profile in the set, in the order the platform team wrote them.
func (s *ProfileSet) Names() []string {
	if s == nil {
		return nil
	}
	return s.order
}

// ProfiledTiers is every Service Tier that has a Profile, so a caller can report
// which tiers can be compiled today.
func (s *ProfileSet) ProfiledTiers() []string {
	if s == nil {
		return nil
	}
	tiers := make([]string, 0, len(s.profiledBy))
	for tier := range s.profiledBy {
		tiers = append(tiers, tier)
	}
	return tiers
}
