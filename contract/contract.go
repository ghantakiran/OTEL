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

// APIVersion is the schema version this binary understands. It versions the
// whole guardrail.otel configuration family — Telemetry Contracts, the Waiver
// register, the enforcement schedule — so all three are declared and checked
// against one value rather than drifting apart. It lives here because this is
// the package every layer already depends on.
const APIVersion = "guardrail.otel/v1"

// RequireAPIVersion refuses a document this binary cannot claim to understand.
//
// The point of the field is to make changing these schemas survivable: a later
// version may move a field or reinterpret one. A binary that accepts a version
// it predates reads the new file under the old rules and reports a confident
// wrong answer — so the one thing apiVersion must never be is decoded and then
// ignored. A pinned old binary meeting a new file now says so.
func RequireAPIVersion(declared, path, kind string) error {
	if declared == APIVersion {
		return nil
	}
	if declared == "" {
		return fmt.Errorf("%s declares no apiVersion: set `apiVersion: %s`, so a %s written for a later schema is not read under this one's rules",
			path, APIVersion, kind)
	}
	return fmt.Errorf("%s declares apiVersion %q, but this otel-guardrail understands %q: upgrade otel-guardrail, or write the %s against the schema this binary supports",
		path, declared, APIVersion, kind)
}

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
	if err := RequireAPIVersion(c.APIVersion, path, "Telemetry Contract"); err != nil {
		return Contract{}, err
	}
	return c, nil
}
