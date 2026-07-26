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
	"time"

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

// Severity is the enforcement weight of a Standard when it is violated.
// Only SeverityBlock fails the build (ADR 0003).
type Severity string

const (
	SeverityInfo  Severity = "info"
	SeverityWarn  Severity = "warn"
	SeverityBlock Severity = "block"
)

// rank orders severities from most to least severe, for reproducible output.
func (s Severity) rank() int {
	switch s {
	case SeverityBlock:
		return 0
	case SeverityWarn:
		return 1
	default:
		return 2
	}
}

// validate rejects a Severity no Standard is allowed to emit. A Standard that
// forgets to declare its Severity must not quietly stop blocking, so an absent
// or unrecognised Severity is an error against the catalog — not a violation
// charged to the service team.
func (s Severity) validate(standard string) error {
	switch s {
	case SeverityInfo, SeverityWarn, SeverityBlock:
		return nil
	case "":
		return fmt.Errorf("Standard %q declared no Severity: every Standard must declare one of %q, %q or %q",
			standard, SeverityInfo, SeverityWarn, SeverityBlock)
	default:
		return fmt.Errorf("Standard %q declared unrecognised Severity %q: must be one of %q, %q or %q",
			standard, s, SeverityInfo, SeverityWarn, SeverityBlock)
	}
}

// Violation is one Standard a Telemetry Contract fails to meet, carrying the
// Severity the Standard declared for it.
type Violation struct {
	Standard string   `json:"standard"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	// Waived is the Waiver holding this violation back, when one is in force for
	// this service and this Standard. A waived violation is still reported — a
	// Waiver that hides its violation is how a Waiver becomes invisible and
	// permanent — it simply stops failing the build until the Waiver expires.
	Waived *Waiver `json:"waived,omitempty"`
}

func (v Violation) String() string {
	if v.Waived != nil {
		return fmt.Sprintf("[%s, waived by %s until %s] %s: %s",
			v.Severity, v.Waived.ApprovedBy, v.Waived.Expires, v.Standard, v.Message)
	}
	return fmt.Sprintf("[%s] %s: %s", v.Severity, v.Standard, v.Message)
}

// failsTheBuild is the effective enforcement of one violation: a block Severity
// stops the pipeline unless an unexpired Waiver holds it back.
func (v Violation) failsTheBuild() bool {
	return v.Severity == SeverityBlock && v.Waived == nil
}

// Result is the outcome of running the Preflight Guardrail over one Telemetry
// Contract. It owns the enforcement rule — only a block Severity fails a build
// (ADR 0003) — so a caller never re-derives that rule from severities.
type Result struct {
	// Violations is every Standard the Contract failed, most severe first.
	Violations []Violation
}

// FailsTheBuild reports whether any violated Standard is severe enough to stop
// the pipeline. It is the only question CI has to ask.
func (r Result) FailsTheBuild() bool {
	return len(r.Blocking()) > 0
}

// Blocking is the violations that fail the build.
func (r Result) Blocking() []Violation {
	return r.violationsThatBlock(true)
}

// NonBlocking is the violations that are reported but let the build through.
func (r Result) NonBlocking() []Violation {
	return r.violationsThatBlock(false)
}

// Waived is the violations an unexpired Waiver is holding back. They are
// reported like any other; they just do not fail the build until the Waiver
// expires, at which point enforcement reverts on its own.
func (r Result) Waived() []Violation {
	var selected []Violation
	for _, v := range r.Violations {
		if v.Waived != nil {
			selected = append(selected, v)
		}
	}
	return selected
}

func (r Result) violationsThatBlock(blocking bool) []Violation {
	var selected []Violation
	for _, v := range r.Violations {
		if v.failsTheBuild() == blocking {
			selected = append(selected, v)
		}
	}
	return selected
}

// Preflight is the static Guardrail that runs before deploy.
type Preflight struct {
	standards rego.PreparedEvalQuery
	waivers   *WaiverRegister
	now       Clock
}

// Clock reports the day a Waiver's expiry is judged against. It is the seam
// that keeps expiry testable: production passes time.Now, a test passes a
// function returning a fixed day, and neither touches a global clock.
type Clock func() time.Time

// Option adjusts a Preflight Guardrail at construction.
type Option func(*Preflight)

// WithWaivers gives the Guardrail the org's Waiver register, so an unexpired
// Waiver can downgrade a service's blocking violation to non-failing. Without
// it every Standard enforces at its declared Severity.
func WithWaivers(register *WaiverRegister) Option {
	return func(p *Preflight) { p.waivers = register }
}

// WithClock fixes the day Waiver expiry is judged on. The default is time.Now.
func WithClock(now Clock) Option {
	return func(p *Preflight) { p.now = now }
}

// NewPreflight compiles a Standard catalog into a Preflight Guardrail. The
// catalog is a filesystem of .rego files; use StandardPolicies for the built-in one.
func NewPreflight(catalog fs.FS, options ...Option) (*Preflight, error) {
	regoOptions := []func(*rego.Rego){rego.Query("data.otel.guardrail.violations")}

	err := fs.WalkDir(catalog, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || path.Ext(name) != ".rego" {
			return err
		}
		source, err := fs.ReadFile(catalog, name)
		if err != nil {
			return err
		}
		regoOptions = append(regoOptions, rego.Module(name, string(source)))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read Standard catalog: %w", err)
	}

	standards, err := rego.New(regoOptions...).PrepareForEval(context.Background())
	if err != nil {
		return nil, fmt.Errorf("compile Standard catalog: %w", err)
	}

	preflight := &Preflight{standards: standards}
	for _, option := range options {
		option(preflight)
	}
	if preflight.now == nil {
		preflight.now = time.Now
	}
	return preflight, nil
}

// Check evaluates every Standard against a declared Telemetry Contract.
// Violations come back in a stable order; a Result with no violations means the
// Contract meets every Standard in the catalog.
func (p *Preflight) Check(ctx context.Context, c contract.Contract) (Result, error) {
	input, err := asDocument(c)
	if err != nil {
		return Result{}, err
	}

	results, err := p.standards.Eval(ctx, rego.EvalInput(input))
	if err != nil {
		return Result{}, fmt.Errorf("evaluate Standards: %w", err)
	}
	if len(results) == 0 || len(results[0].Expressions) == 0 {
		return Result{}, nil
	}

	raw, err := json.Marshal(results[0].Expressions[0].Value)
	if err != nil {
		return Result{}, fmt.Errorf("read Standard results: %w", err)
	}
	var violations []Violation
	if err := json.Unmarshal(raw, &violations); err != nil {
		return Result{}, fmt.Errorf("decode Standard results: %w", err)
	}
	// The catalog is trusted to say how severely it enforces; it is not trusted
	// to remember to say it at all.
	for _, v := range violations {
		if err := v.Severity.validate(v.Standard); err != nil {
			return Result{}, err
		}
	}

	// A Waiver is not a Standard, so it does not get a say in what the catalog
	// found; it only downgrades how hard a finding lands, once the finding exists.
	asOf := p.now()
	for i, v := range violations {
		if w, waived := p.waivers.InForce(c.ServiceName, v.Standard, asOf); waived {
			violations[i].Waived = &w
		}
	}

	// Rego sets are unordered; CI output and exit codes must be reproducible.
	// Most severe first, so the reason a build failed leads the output.
	sort.Slice(violations, func(i, j int) bool {
		a, b := violations[i], violations[j]
		if a.Severity != b.Severity {
			return a.Severity.rank() < b.Severity.rank()
		}
		if a.Standard != b.Standard {
			return a.Standard < b.Standard
		}
		return a.Message < b.Message
	})
	return Result{Violations: violations}, nil
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
