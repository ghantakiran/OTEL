// Package contract defines the Telemetry Contract: a per-service declaration of
// what telemetry a service intends to emit. It is declared intent, not observed
// reality — Preflight Guardrails check the declaration, Pipeline Guardrails later
// check reality against it.
package contract

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Contract is a service's declared telemetry intent.
// The json tags matter: a Contract is handed to Rego as the input document, and
// Standards address its fields by these names.
type Contract struct {
	APIVersion         string            `yaml:"apiVersion" json:"apiVersion"`
	Kind               string            `yaml:"kind" json:"kind"`
	ServiceName        string            `yaml:"service_name" json:"service_name"`
	Owner              string            `yaml:"owner" json:"owner"`
	Tier               string            `yaml:"tier" json:"tier"`
	Signals            []string          `yaml:"signals" json:"signals"`
	ResourceAttributes map[string]string `yaml:"resource_attributes" json:"resource_attributes"`
}

// Kind is what a Telemetry Contract file must declare itself to be.
const Kind = "TelemetryContract"

// Load reads a Telemetry Contract from a YAML file.
func Load(path string) (Contract, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Contract{}, fmt.Errorf("read Telemetry Contract %s: %w", path, err)
	}

	var c Contract
	if err := yaml.Unmarshal(data, &c); err != nil {
		return Contract{}, fmt.Errorf("parse Telemetry Contract %s: %w", path, err)
	}

	// Every field of another document kind is simply absent from a Contract, so
	// the wrong file decodes cleanly into an empty one — and the Guardrail then
	// reports a confident verdict about a service whose name is the empty string.
	// A wrong path is a Guardrail that could not run, not a service's fault.
	if c.Kind != Kind {
		return Contract{}, fmt.Errorf("%s is not a Telemetry Contract: kind is %q, want %q", path, c.Kind, Kind)
	}
	return c, nil
}
