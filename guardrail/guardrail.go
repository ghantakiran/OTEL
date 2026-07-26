// Package guardrail runs Guardrails: automated checks that enforce the org's
// observability Standards. It currently implements the Preflight Guardrail,
// which checks a declared Telemetry Contract before deploy.
//
// Standards are Rego policies (ADR 0002); this package owns packaging, result
// formatting and CI integration, not policy evaluation itself.
package guardrail

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"

	"github.com/open-policy-agent/opa/v1/rego"

	"github.com/ghantakiran/OTEL/contract"
)

//go:embed policies
var builtinPolicies embed.FS

// StandardPolicies is the Standard catalog shipped with the CLI.
func StandardPolicies() fs.FS {
	catalog, err := fs.Sub(builtinPolicies, "policies")
	if err != nil {
		panic(fmt.Sprintf("embedded Standard catalog is unreadable: %v", err))
	}
	return catalog
}

// Violation is one Standard a Telemetry Contract fails to meet.
type Violation struct {
	Standard string `json:"standard"`
	Message  string `json:"message"`
}

func (v Violation) String() string {
	return fmt.Sprintf("%s: %s", v.Standard, v.Message)
}

// Preflight is the static Guardrail that runs before deploy.
type Preflight struct {
	standards rego.PreparedEvalQuery
}

// NewPreflight compiles a Standard catalog into a Preflight Guardrail. The
// catalog is a filesystem of .rego files; use StandardPolicies for the built-in one.
func NewPreflight(catalog fs.FS) (*Preflight, error) {
	options := []func(*rego.Rego){rego.Query("data.otel.guardrail.violations")}

	err := fs.WalkDir(catalog, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || path.Ext(name) != ".rego" {
			return err
		}
		source, err := fs.ReadFile(catalog, name)
		if err != nil {
			return err
		}
		options = append(options, rego.Module(name, string(source)))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read Standard catalog: %w", err)
	}

	standards, err := rego.New(options...).PrepareForEval(context.Background())
	if err != nil {
		return nil, fmt.Errorf("compile Standard catalog: %w", err)
	}
	return &Preflight{standards: standards}, nil
}

// Check evaluates every Standard against a declared Telemetry Contract.
// Violations come back in a stable order; an empty result means the Contract complies.
func (p *Preflight) Check(ctx context.Context, c contract.Contract) ([]Violation, error) {
	input, err := asDocument(c)
	if err != nil {
		return nil, err
	}

	results, err := p.standards.Eval(ctx, rego.EvalInput(input))
	if err != nil {
		return nil, fmt.Errorf("evaluate Standards: %w", err)
	}
	if len(results) == 0 || len(results[0].Expressions) == 0 {
		return nil, nil
	}

	raw, err := json.Marshal(results[0].Expressions[0].Value)
	if err != nil {
		return nil, fmt.Errorf("read Standard results: %w", err)
	}
	var violations []Violation
	if err := json.Unmarshal(raw, &violations); err != nil {
		return nil, fmt.Errorf("decode Standard results: %w", err)
	}

	// Rego sets are unordered; CI output and exit codes must be reproducible.
	sort.Slice(violations, func(i, j int) bool {
		if violations[i].Standard != violations[j].Standard {
			return violations[i].Standard < violations[j].Standard
		}
		return violations[i].Message < violations[j].Message
	})
	return violations, nil
}

// asDocument converts a Contract into the generic document Rego evaluates against.
func asDocument(c contract.Contract) (map[string]any, error) {
	encoded, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("encode Contract: %w", err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		return nil, fmt.Errorf("decode Contract: %w", err)
	}
	return document, nil
}
