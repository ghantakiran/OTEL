// Package controlplane turns Telemetry Contracts into running collector
// configuration. It Compiles a Contract (what a service emits) with its Service
// Tier's Pipeline Profile (how that telemetry ships) into a collector config,
// and reads the Service Tier Taxonomy the Guardrails already enforce rather than
// re-encoding it (ADR 0005, ADR 0007).
//
// It compiles the other half of the topology too: the shared Gateway that every
// Agent forwards to, from the org's Gateway Declaration (ADR 0013).
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
	MemoryLimitMiB  int      `yaml:"memory_limit_mib"`
	Batch           Batch    `yaml:"batch"`
	Delivery        Delivery `yaml:"delivery"`
	Sampling        Sampling `yaml:"sampling"`
}

// Delivery is how hard one collector tries to get telemetry to the next hop when
// the next hop is not answering. Retrying protects telemetry the org cannot lose;
// not retrying protects the sender from applying back-pressure over telemetry
// nobody will read.
//
// A Profile sets it for the Agent-to-Gateway hop, where which matters is a
// criticality decision and so belongs per Service Tier. A Backend sets it for the
// Gateway-to-Backend hop, where it belongs per Backend so that one slow
// destination cannot block the others (ADR 0010).
type Delivery struct {
	// QueueSize is how many batches the sender holds before dropping.
	QueueSize int `yaml:"queue_size"`
	// Retry is whether a failed export is retried.
	Retry bool `yaml:"retry"`
	// Spill makes the queue persistent: it is written to disk, so it survives the
	// collector restarting while the next hop is still down. Only a Backend sets
	// it — a persistent queue needs a storage extension, which is the collector's
	// contrib distribution rather than core (ADR 0014), and an Agent runs beside a
	// service on whatever disk that service has. Its queue stays in memory.
	Spill bool `yaml:"spill"`
}

// Batch is how telemetry is grouped before it leaves the Agent.
type Batch struct {
	Timeout       string `yaml:"timeout"`
	SendBatchSize int    `yaml:"send_batch_size"`
}

// Sampling is the GATEWAY's tail-sampling budget for this tier — a per-tier cost
// decision the Profile owns. It stays here rather than in the Gateway Declaration
// precisely because it varies by tier, which is the line ADR 0013 draws between
// the two documents.
//
// It is deliberately not head sampling at the Agent: the Gateway tail-samples
// with the whole trace in hand (ADR 0007), and an Agent dropping spans first
// would hand it broken traces. Nothing in an Agent config is derived from this.
//
// NOTHING READS IT YET, and #40 is where that is tracked. C5 (#13) built the
// Gateway's fan-out and deferred tail sampling deliberately: the blocker is not
// the processor but that the Gateway cannot tell Service Tiers apart at run time
// — no Contract stamps a tier attribute, so a per-tier policy has nothing to key
// on (the consequence ADR 0013 flagged). #40 either closes that or deletes this
// field; a setting nobody reads is worse than no setting, because it reads as
// working.
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
		// `delivery` is shared by a Profile and a Backend, but `spill` belongs to the
		// Backend alone: it needs a storage extension from the collector's contrib
		// distribution, and the Agent stays core-only (ADR 0014). Nothing compiles it
		// into an Agent config — which is precisely why writing it here has to fail
		// rather than be ignored. Silently dropping it would leave a Profile that
		// reads as durable in every review while the fleet runs an in-memory queue.
		if profile.Delivery.Spill {
			return nil, fmt.Errorf("Pipeline Profiles %s: Profile %q asks its Agents to spill, but spill is a Backend's setting and nothing compiles it into an Agent — the Agent stays on the collector's core distribution (ADR 0014), so remove `spill:` here and set it on a Backend in the Gateway Declaration", origin, profile.Name)
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
