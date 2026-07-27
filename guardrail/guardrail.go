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
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/open-policy-agent/opa/v1/rego"
	"github.com/open-policy-agent/opa/v1/storage/inmem"

	"github.com/ghantakiran/OTEL/contract"
)

//go:embed policies
var builtinPolicies embed.FS

// StandardPolicies is the Rego shipped with the CLI: how each Standard is
// detected. What each Standard requires is CentralStandards, a separate document
// the policies read as data — see catalog.go.
func StandardPolicies() fs.FS {
	policies, err := fs.Sub(builtinPolicies, "policies")
	if err != nil {
		panic(fmt.Sprintf("the embedded Standard policies are unreadable: %v", err))
	}
	return policies
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
// Severity the Standard declared for it and anything holding that Severity back.
type Violation struct {
	Standard string   `json:"standard"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	// Holds is everything keeping this violation from failing the build — a
	// Waiver, the Enforcement Epoch's legacy grace, or both at once. A held-back
	// violation is still reported: one that vanishes from the output is one
	// nobody retires. See effective_enforcement.go for what this means.
	Holds []Hold `json:"holds,omitempty"`
}

func (v Violation) String() string {
	holds := v.HeldBackBy()
	if len(holds) == 0 {
		return fmt.Sprintf("[%s] %s: %s", v.Severity, v.Standard, v.Message)
	}
	// Every Hold is named, not just the first: they lapse on different days, and a
	// service told about only one of them is told the wrong date.
	described := make([]string, 0, len(holds))
	for _, h := range holds {
		described = append(described, h.String())
	}
	return fmt.Sprintf("[%s, %s] %s: %s", v.Severity, strings.Join(described, "; "), v.Standard, v.Message)
}

// Result is the outcome of running the Preflight Guardrail over one Telemetry
// Contract. Grouping violations by their effective enforcement — and the one
// question CI asks, FailsTheBuild — live in effective_enforcement.go, so the rule
// ADR 0003 is about has exactly one implementation.
type Result struct {
	// Violations is every Standard the Contract failed, most severe first.
	Violations []Violation
}

// Preflight is the static Guardrail that runs before deploy.
type Preflight struct {
	standards rego.PreparedEvalQuery
	taxonomy  *Taxonomy
	catalog   *StandardCatalog
	waivers   *WaiverRegister
	schedule  *EnforcementSchedule
	appeared  FirstAppearance
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

// WithClock fixes the day a Waiver expiry or a graduation deadline is judged
// on. The default is time.Now.
func WithClock(now Clock) Option {
	return func(p *Preflight) { p.now = now }
}

// WithTaxonomy gives the Guardrail a Service Tier taxonomy for the Standards that
// need one. It defaults to the org taxonomy compiled into this binary — unlike
// Waivers and the enforcement schedule, this is not optional: S2 cannot decide
// anything without it, and an absent taxonomy would make every declared tier look
// unknown and block the whole fleet.
func WithTaxonomy(taxonomy *Taxonomy) Option {
	return func(p *Preflight) { p.taxonomy = taxonomy }
}

// WithStandardCatalog gives the Guardrail the org's Standard catalog — what each
// Standard requires, at what Severity, and where it is enforced. Like the
// taxonomy and unlike Waivers, it is not optional: it defaults to the catalog
// compiled into this binary, because a policy with no catalog entry reads an
// absent data document and quietly enforces nothing.
func WithStandardCatalog(catalog *StandardCatalog) Option {
	return func(p *Preflight) { p.catalog = catalog }
}

// WithEnforcementSchedule gives the Guardrail the org's published Enforcement
// Epoch and graduation deadlines, so a legacy service is held back from a
// blocking Standard until that Standard graduates. It requires
// WithFirstAppearance: a schedule with no way to date the service decides
// nothing. Without either, every Standard enforces at its declared Severity.
func WithEnforcementSchedule(schedule *EnforcementSchedule) Option {
	return func(p *Preflight) { p.schedule = schedule }
}

// WithFirstAppearance tells the Guardrail how to date a service's Telemetry
// Contract. Production passes FirstAppearanceInGit; a test passes the day directly.
func WithFirstAppearance(appeared FirstAppearance) Option {
	return func(p *Preflight) { p.appeared = appeared }
}

// NewPreflight compiles the Standard policies into a Preflight Guardrail.
//
// `policies` is a filesystem of .rego files — how each Standard is detected in a
// declared Contract; use StandardPolicies for the built-in one. WHAT each
// Standard requires and how severely is the Standard catalog, a separate
// document the policies read as data (WithStandardCatalog). The two are not the
// same thing and this constructor takes both.
func NewPreflight(policies fs.FS, options ...Option) (*Preflight, error) {
	regoOptions := []func(*rego.Rego){rego.Query("data.otel.guardrail.violations")}

	sources := map[string]string{}
	err := fs.WalkDir(policies, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || path.Ext(name) != ".rego" {
			return err
		}
		source, err := fs.ReadFile(policies, name)
		if err != nil {
			return err
		}
		sources[name] = string(source)
		regoOptions = append(regoOptions, rego.Module(name, string(source)))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the Standard policies: %w", err)
	}

	preflight := &Preflight{}
	for _, option := range options {
		option(preflight)
	}
	if preflight.taxonomy == nil {
		taxonomy, err := CentralTaxonomy()
		if err != nil {
			return nil, fmt.Errorf("load the Service Tier taxonomy: %w", err)
		}
		preflight.taxonomy = taxonomy
	}
	if preflight.catalog == nil {
		declared, err := CentralStandards()
		if err != nil {
			return nil, fmt.Errorf("load the Standard catalog: %w", err)
		}
		preflight.catalog = declared
	}

	// The policies and the catalog are two halves of one Standard, and a caller can
	// assemble any pair of them. A half that does not meet its other half does not
	// fail — it reports nothing, because an absent Rego data document is silence
	// rather than an error. So the pair is checked here, the one place that sees
	// the pair somebody actually assembled.
	if err := preflight.catalog.matches(sources); err != nil {
		return nil, fmt.Errorf("the Standard policies and the Standard catalog do not match: %w", err)
	}

	// The taxonomy and the Standard catalog reach the Standards as Rego *data*
	// documents, not as part of the input: input is the Contract under test, data
	// is what the org has decided. A Standard reads data.otel.taxonomy and
	// data.otel.standards; none of them defines a tier, a required attribute or a
	// Severity. That is what lets the Gateway compile the same catalog into
	// Pipeline Guardrails without a second definition of anything (ADR 0015).
	store := inmem.NewFromObject(map[string]any{"otel": map[string]any{
		"taxonomy":  preflight.taxonomy.asRegoData(),
		"standards": preflight.catalog.asRegoData(),
	}})
	regoOptions = append(regoOptions, rego.Store(store))

	standards, err := rego.New(regoOptions...).PrepareForEval(context.Background())
	if err != nil {
		return nil, fmt.Errorf("compile Standard catalog: %w", err)
	}
	preflight.standards = standards
	if preflight.now == nil {
		preflight.now = time.Now
	}
	if preflight.schedule != nil && preflight.appeared == nil {
		return nil, fmt.Errorf("an enforcement schedule needs a way to date the service: pass WithFirstAppearance alongside it")
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
		// `enforced_at` has to be honoured here as well as at the Gateway, or it is
		// a setting one layer obeys and the other ignores. The Rego aggregator picks
		// up every policy in the catalog directory without registration, so a
		// Standard moved to `pipeline` while its .rego stayed behind would go on
		// failing builds at a point the catalog says it is not enforced at. Stopping
		// the run is the same treatment an absent Severity gets, and for the same
		// reason: quietly discarding the violation instead would leave a Standard
		// nobody notices has died.
		if err := p.enforcedHere(v.Standard); err != nil {
			return Result{}, err
		}
	}

	// A Waiver is not a Standard, so it does not get a say in what the catalog
	// found; it only downgrades how hard a finding lands, once the finding exists.
	// Both a Waiver and the Enforcement Epoch answer the same question — how hard
	// does this finding land — so both attach only where there is enforcement to
	// modify. A warn Standard is already non-failing; recording a modifier against
	// one would make the report name a Waiver that is holding nothing back, and
	// point the reader at its expiry date instead of the date that matters.
	// Both a Waiver and the Enforcement Epoch are recorded as Holds, and
	// Violation.hold declines to record one against a Standard that never fails
	// the build — so a modifier can only ever attach where there is enforcement to
	// modify. That is enforced in one place rather than remembered in two.
	asOf := p.now()
	for i, v := range violations {
		if w, waived := p.waivers.InForce(c.ServiceName, v.Standard, asOf); waived {
			violations[i].hold(waiverHold(w))
		}
	}

	// The Enforcement Epoch is likewise not a Standard: it does not change what
	// the catalog found, only how hard it lands on a service that predates the
	// org's standards programme.
	if err := p.deferForLegacyService(violations, asOf); err != nil {
		return Result{}, err
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

// enforcedHere rejects a violation from a Standard the catalog does not enforce
// at preflight — including one the catalog does not declare at all, which is a
// policy reading an absent data document and reporting against nothing.
func (p *Preflight) enforcedHere(standard string) error {
	for _, declared := range p.catalog.Standards() {
		if declared.ID != standard {
			continue
		}
		if slices.Contains(declared.EnforcedAt, EnforcedAtPreflight) {
			return nil
		}
		return fmt.Errorf(
			"Standard %s reported a violation, but the Standard catalog declares it enforced_at %v — either enforce it at %q or remove its policy from the catalog directory",
			standard, declared.EnforcedAt, EnforcedAtPreflight)
	}
	return fmt.Errorf(
		"Standard %s reported a violation, but the Standard catalog does not declare it: its policy is reading an absent catalog entry, so what it required and how severely is undefined",
		standard)
}

// deferForLegacyService holds a legacy service back from each blocking Standard
// that has not reached its graduation deadline. A new service, or a Guardrail
// running without a published schedule, is untouched.
func (p *Preflight) deferForLegacyService(violations []Violation, asOf time.Time) error {
	if p.schedule == nil {
		return nil
	}

	appeared, err := p.appeared()
	if err != nil {
		return err
	}
	if p.schedule.Classify(appeared) == ServiceNew {
		return nil
	}

	for i, v := range violations {
		if v.Severity != SeverityBlock {
			continue
		}
		grace, deferred, err := p.schedule.grace(v.Standard, asOf)
		if err != nil {
			return err
		}
		if deferred {
			violations[i].hold(graceHold(grace))
		}
	}
	return nil
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
