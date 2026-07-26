package controlplane

import (
	_ "embed"
	"fmt"
	"os"

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
	// Backends are where telemetry lands. Declared here and nowhere else, which is
	// what makes a service Backend-agnostic (ADR 0007).
	Backends []Backend `yaml:"backends"`
}

// Backend is one destination the Gateway exports to, reached over OTLP.
//
// OTLP rather than a vendor-specific exporter keeps the compiled config to the
// collector's core distribution: a Splunk or Datadog exporter would pull in the
// contrib distribution, which C1 and C2 deliberately avoided. A Backend that
// speaks only a proprietary protocol is therefore an explicit decision to take,
// not something that happens by accident.
type Backend struct {
	Name        string `yaml:"backend"`
	Description string `yaml:"description"`
	Endpoint    string `yaml:"endpoint"`
	// Delivery is per Backend, not per Gateway: one slow or unreachable Backend
	// must not block exports to the others (ADR 0010).
	Delivery Delivery `yaml:"delivery"`
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
