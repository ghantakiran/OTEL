package controlplane

import (
	_ "embed"
	"fmt"
	"os"
	"slices"

	"gopkg.in/yaml.v3"

	"github.com/ghantakiran/OTEL/contract"
)

//go:embed gateway.yaml
var builtinGateway []byte

// gatewayKind is what a Gateway Declaration file must declare itself to be.
const gatewayKind = "GatewayDeclaration"

// GatewayDeclaration is the org's single declaration of the shared Gateway: where
// Agents reach it, how it regroups telemetry, and which Backend(s) it exports to.
//
// There is exactly one, because there is exactly one Gateway tier. That is what
// separates it from a Pipeline Profile: a Profile is per Service Tier and selected
// by a tier, whereas nothing selects the Gateway — every Agent in the fleet
// forwards to it (ADR 0013).
type GatewayDeclaration struct {
	// Address is where Agents reach the Gateway. The Gateway's own OTLP receiver
	// is derived from its port, so the two halves of the topology cannot disagree.
	Address string `yaml:"address"`
	// MemoryLimitMiB is the Gateway's memory ceiling.
	MemoryLimitMiB int `yaml:"memory_limit_mib"`
	// Batch is how telemetry is regrouped on the way out to a Backend.
	Batch Batch `yaml:"batch"`
	// SpillRoot is the directory the Gateway's persistent sending queues spill to.
	// It is a Gateway fact — one volume, mounted once — while WHETHER a given
	// Backend spills is that Backend's own durability decision, so the two sit in
	// different places (the split ADR 0013 draws).
	//
	// Each spilling Backend gets its own subdirectory under this, named after it.
	// Deriving rather than declaring the directory is what makes two Backends
	// sharing one storage instance unrepresentable: sharing would give them one
	// file lock, one disk budget and one corruption blast radius, re-coupling the
	// Backends through the mechanism meant to decouple them.
	SpillRoot string `yaml:"spill_root"`
	// Backends are where telemetry lands. Declared here and nowhere else, which is
	// what makes a service Backend-agnostic (ADR 0007).
	Backends []Backend `yaml:"backends"`
}

// Backend is one destination the Gateway exports to, reached over OTLP.
//
// OTLP rather than a vendor-specific exporter, and the Backend is named for the
// ROLE it fills rather than the product filling it. Both keep the vendor out of
// the compiled config, out of git history, and out of the exporter IDs the
// platform's own metrics are labelled by — so swapping a Backend stays one edit
// to one file (ADR 0007) rather than a rename across all of it. A Backend that
// speaks only a proprietary protocol is an explicit decision to take, not
// something that happens by accident.
//
// Each Backend compiles to its OWN exporter with its own queue, its own retry and
// its own spill storage. Nothing is shared between two of them: a shared queue is
// what makes one slow Backend everyone's outage, and a shared storage instance
// would re-couple them a layer down (ADR 0010, ADR 0014).
type Backend struct {
	Name        string `yaml:"backend"`
	Description string `yaml:"description"`
	Endpoint    string `yaml:"endpoint"`
	// Signals are the Signals this Backend receives. Empty means every Signal — a
	// Backend takes the whole stream unless it says otherwise, which is what an APM
	// does and what the one-Backend Gateway did before fan-out.
	//
	// A metrics store is not a trace store, so this is not cosmetic: exporting a
	// Signal a Backend rejects produces export failures indistinguishable from that
	// Backend being down.
	Signals []string `yaml:"signals"`
	// Delivery is per Backend, not per Gateway: one slow or unreachable Backend
	// must not block exports to the others (ADR 0010).
	Delivery Delivery `yaml:"delivery"`
}

// Receives reports whether this Backend takes a Signal. An omitted `signals:` —
// a nil slice — is every Signal. An empty list written out is refused at compile
// rather than reaching here, since "none" read as "everything" would be the widest
// gap possible between what was written and what runs.
func (b Backend) Receives(signal contract.Signal) bool {
	if b.Signals == nil {
		return true
	}
	return slices.Contains(b.Signals, string(signal))
}

type gatewayDocument struct {
	APIVersion string             `yaml:"apiVersion"`
	Kind       string             `yaml:"kind"`
	Gateway    GatewayDeclaration `yaml:"gateway"`
}

// CentralGateway is the org's Gateway Declaration, shipped in the binary the same
// way the Profiles, the Standard catalog and the taxonomy are (ADR 0004: git is
// the source of truth, nothing is fetched at run time).
func CentralGateway() (*GatewayDeclaration, error) {
	return parseGateway(builtinGateway, "controlplane/gateway.yaml")
}

// LoadGateway reads a Gateway Declaration from a YAML file.
func LoadGateway(path string) (*GatewayDeclaration, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Gateway Declaration %s: %w", path, err)
	}
	return parseGateway(data, path)
}

func parseGateway(data []byte, origin string) (*GatewayDeclaration, error) {
	var document gatewayDocument
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("parse Gateway Declaration %s: %w", origin, err)
	}

	// Every field of a Gateway Declaration is simply absent from another document
	// kind, so the wrong file decodes cleanly into an empty one — and the caller then
	// reports a confident error about a Gateway nobody described. This is the only
	// thing the loader judges: whether the file is this kind of document at all.
	// Whether the Gateway it describes can be built is CompileGateway's question, so
	// that "your declaration is wrong" and "the tool had no input" stay distinct exit
	// codes.
	if document.Kind != gatewayKind {
		return nil, fmt.Errorf("%s is not a Gateway Declaration: kind is %q, want %q", origin, document.Kind, gatewayKind)
	}
	if err := contract.RequireAPIVersion(document.APIVersion, origin, "Gateway Declaration"); err != nil {
		return nil, err
	}
	return &document.Gateway, nil
}
