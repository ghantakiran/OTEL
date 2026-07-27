package guardrail

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ghantakiran/OTEL/contract"
)

// The Standard catalog: every Standard, its Severity, where it is enforced, and
// the requirement it checks.
//
// The requirement used to live inside each policy as a Rego literal, which was
// correct while a Preflight Guardrail was the only consumer. It stopped being
// correct at C6: a collector cannot evaluate Rego, so a Pipeline Guardrail is
// collector processors, and the requirement would have existed twice — once in
// Rego, once in whatever the Gateway compiler was told — in two languages,
// failing independently and silently. A service could then pass Preflight against
// one list while the Gateway tagged its telemetry against another.
//
// So the catalog is a document this package owns, and the policies receive it as
// the Rego data document `data.otel.standards`. Policy is a consumer of the
// catalog, never its definition — enforced by tests asserting no .rego file names
// a required attribute or a Severity. It is the same move guardrail/tiers.yaml
// made for the Service Tier taxonomy (#28).

//go:embed standards.yaml
var builtinStandards []byte

// catalogKind is what a Standard catalog file must declare itself to be.
const catalogKind = "StandardCatalog"

// EnforcementPoint is where a Standard is enforced. A Standard declares one or
// both: not every requirement is checkable at both, and the ones that are are
// checked against different evidence at different moments.
type EnforcementPoint string

const (
	// EnforcedAtPreflight is before deploy, against the declared Telemetry
	// Contract — does the declaration comply?
	EnforcedAtPreflight EnforcementPoint = "preflight"
	// EnforcedAtPipeline is at run time in the Gateway, against live telemetry —
	// does reality match the declaration?
	EnforcedAtPipeline EnforcementPoint = "pipeline"
)

// Standard is one entry in the catalog.
type Standard struct {
	ID string `yaml:"standard"`
	// Title is what the Standard requires, in words. Nothing computes on it — it is
	// there for whoever opens this file, which is what an operator does when a
	// Guardrail Tag names a Standard they do not recognise. Optional, like a
	// Backend's `description:` and a Service Tier's `criticality:`.
	Title      string             `yaml:"title"`
	Severity   Severity           `yaml:"severity"`
	EnforcedAt []EnforcementPoint `yaml:"enforced_at"`
	Requires   Requirement        `yaml:"requires"`
}

// Requirement is what a Standard demands, by kind. Exactly one kind is set.
//
// The kind is what decides whether the Standard can be enforced at the pipeline
// at all: `resource_attributes` is a property of a single record and compiles to
// collector processors, whereas `tier_mandatory_signals` is a property of a
// service's stream over time and does not.
type Requirement struct {
	// ResourceAttributes is a list of resource attribute keys that must be present.
	ResourceAttributes []string `yaml:"resource_attributes"`
	// TierMandatorySignals says the Standard checks the Signals the service's
	// Service Tier makes mandatory, read from the Service Tier Taxonomy.
	TierMandatorySignals bool `yaml:"tier_mandatory_signals"`
}

// StandardCatalog is the org's catalog of Standards.
type StandardCatalog struct {
	standards []Standard
}

type catalogDocument struct {
	APIVersion string     `yaml:"apiVersion"`
	Kind       string     `yaml:"kind"`
	Standards  []Standard `yaml:"standards"`
}

// CentralStandards is the org's Standard catalog, shipped in the binary from
// guardrail/standards.yaml the same way the taxonomy and the Rego policies are.
func CentralStandards() (*StandardCatalog, error) {
	return parseCatalog(builtinStandards, "guardrail/standards.yaml")
}

// LoadStandards reads a Standard catalog from a YAML file.
func LoadStandards(path string) (*StandardCatalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Standard catalog %s: %w", path, err)
	}
	return parseCatalog(data, path)
}

func parseCatalog(data []byte, origin string) (*StandardCatalog, error) {
	var document catalogDocument

	// Strictly: a key this document does not know is refused rather than ignored.
	// Every other kind of mistake in here is caught by name — an absent Severity, an
	// absent enforcement point — but a misspelt `standards:` has no entry to be
	// caught on. It decodes into a catalog with nothing in it, which is the one
	// state that disarms both enforcement points at once (see below).
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse Standard catalog %s: %w", origin, err)
	}
	if document.Kind != catalogKind {
		return nil, fmt.Errorf("Standard catalog %s: kind is %q, want %q", origin, document.Kind, catalogKind)
	}
	if err := contract.RequireAPIVersion(document.APIVersion, origin, "Standard catalog"); err != nil {
		return nil, err
	}

	// A catalog with no entries validates perfectly, because there is nothing to
	// validate — and then NOTHING IS ENFORCED ANYWHERE. The Gateway compiles no
	// Guardrail processor, and every policy reads an absent data document and
	// reports nothing, so `check` returns no violations and no error on a Contract
	// that violates everything. Rego's absence is silence, so no consumer
	// downstream can tell this from a compliant fleet; it has to be refused here.
	// The Service Tier taxonomy refuses an empty document for the same reason.
	if len(document.Standards) == 0 {
		return nil, fmt.Errorf(
			"Standard catalog %s declares no Standards: nothing would be enforced at either point, and neither a Preflight nor a Pipeline Guardrail can tell that apart from a fleet that complies",
			origin)
	}

	declaredOnce := map[string]bool{}
	for _, standard := range document.Standards {
		if err := standard.validate(origin); err != nil {
			return nil, err
		}
		// A Standard's id is its identity at BOTH enforcement points — what a
		// Preflight violation is reported under, and the attribute key the Gateway
		// tags live telemetry with. Two entries sharing one would write that
		// attribute twice with two Severities, and which one an operator saw would
		// depend on statement order.
		//
		// Case-insensitively, because the id is also half of a Rego package name
		// (`otel.guardrail.standards.s9`) and a package name cannot carry case. `S9`
		// and `s9` are two catalog entries and two attribute keys but one policy, so
		// one of them would silently have no implementation at preflight.
		folded := strings.ToLower(standard.ID)
		if declaredOnce[folded] {
			return nil, fmt.Errorf(
				"Standard catalog %s: Standard %s is declared twice, and the two would enforce under one id",
				origin, standard.ID)
		}
		declaredOnce[folded] = true
	}
	return &StandardCatalog{standards: document.Standards}, nil
}

// standardID is what a Standard may be called: letters, digits and underscores,
// starting with a letter or digit. Deliberately narrow, because the id has to be
// three things at once and the intersection is small:
//
//   - a segment of the attribute name the Gateway tags telemetry with
//     (`otel.guardrail.violation.<id>`), written inside a quoted OTTL string — so
//     a dot would silently reshape that namespace and a quote would end the
//     string early;
//   - a Rego document key (`data.otel.standards.<id>`), which is case-sensitive;
//   - the last segment of that Standard's Rego package name, which is NOT — and
//     which cannot carry a hyphen at all. `package otel.guardrail.standards.s-9`
//     does not compile, so `S-9` would be a Standard the Gateway tags and no
//     policy can be written for: the two enforcement points disagreeing, with
//     nothing able to say so.
var standardID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_]*$`)

// attributeName is what a required resource attribute may be called. Every OTEL
// semantic-convention key fits — `service.name`, `http.request.method`,
// `k8s.pod.name` — while quotes, backslashes, whitespace and non-ASCII do not,
// because the name is written inside a quoted OTTL string in the compiled Gateway
// config and those are the characters whose escaping the collector's parser would
// have to agree with.
var attributeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// validate rejects a catalog entry no Standard is allowed to be. Every check
// here is one a Standard could otherwise fail silently: it is exactly the
// treatment Severity.validate gives an absent Severity, applied to the rest of
// the entry, and it happens where the document is read so that both consumers
// get it rather than whichever one remembered.
func (s Standard) validate(origin string) error {
	if s.ID == "" {
		return fmt.Errorf("Standard catalog %s has an entry naming no Standard: set `standard:` on each", origin)
	}
	if !standardID.MatchString(s.ID) {
		return fmt.Errorf(
			"Standard catalog %s: Standard id %q is not usable as one — it becomes a segment of the attribute name the Gateway tags telemetry with, so it must be letters, digits, hyphens and underscores, starting with a letter or digit (like `S1`)",
			origin, s.ID)
	}
	if err := s.Severity.validate(s.ID); err != nil {
		return err
	}
	if len(s.EnforcedAt) == 0 {
		return fmt.Errorf(
			"Standard catalog %s: Standard %s declares no enforcement point: every Standard must declare where it is enforced — %q, %q, or both",
			origin, s.ID, EnforcedAtPreflight, EnforcedAtPipeline)
	}
	declaredOnce := map[EnforcementPoint]bool{}
	for _, point := range s.EnforcedAt {
		switch point {
		case EnforcedAtPreflight, EnforcedAtPipeline:
		default:
			return fmt.Errorf(
				"Standard catalog %s: Standard %s declares unrecognised enforcement point %q: must be %q, %q, or both",
				origin, s.ID, point, EnforcedAtPreflight, EnforcedAtPipeline)
		}
		if declaredOnce[point] {
			return fmt.Errorf(
				"Standard catalog %s: Standard %s declares the %q enforcement point twice",
				origin, s.ID, point)
		}
		declaredOnce[point] = true
	}
	return s.Requires.validate(origin, s)
}

// EnforcedAtThePipeline reports whether this Standard is enforced against live
// telemetry in the Gateway.
func (s Standard) EnforcedAtThePipeline() bool {
	return slices.Contains(s.EnforcedAt, EnforcedAtPipeline)
}

// kinds is every requirement kind this entry sets. A Standard sets exactly one:
// zero is a Standard nothing enforces, and two would be two Standards wearing one
// id, reported under one Severity and tagged under one attribute.
func (r Requirement) kinds() []string {
	var set []string
	if r.ResourceAttributes != nil {
		set = append(set, "resource_attributes")
	}
	if r.TierMandatorySignals {
		set = append(set, "tier_mandatory_signals")
	}
	return set
}

// expressibleAsProcessors reports whether this requirement can be checked by a
// collector on one record at a time — which is the only thing a Pipeline
// Guardrail gets to do.
func (r Requirement) expressibleAsProcessors() bool {
	return r.ResourceAttributes != nil
}

func (r Requirement) validate(origin string, s Standard) error {
	kinds := r.kinds()
	switch {
	case len(kinds) == 0:
		return fmt.Errorf(
			"Standard catalog %s: Standard %s requires nothing, so nothing enforces it — give it a requirement under `requires:`",
			origin, s.ID)
	case len(kinds) > 1:
		return fmt.Errorf(
			"Standard catalog %s: Standard %s declares %d requirement kinds (%s); a Standard is one requirement, so split it into two Standards",
			origin, s.ID, len(kinds), strings.Join(kinds, ", "))
	}

	// An omitted `resource_attributes:` is a different requirement kind. An empty
	// list written out is somebody saying something, and the only thing it can mean
	// is "require nothing" — a Standard that reads as enforced at both points and
	// checks nothing at either.
	if r.ResourceAttributes != nil && len(r.ResourceAttributes) == 0 {
		return fmt.Errorf(
			"Standard catalog %s: Standard %s declares an empty `resource_attributes: []`, so it would require nothing while reading as enforced — list the attributes, or remove the Standard",
			origin, s.ID)
	}
	requiredOnce := map[string]bool{}
	for _, attribute := range r.ResourceAttributes {
		if strings.TrimSpace(attribute) == "" {
			return fmt.Errorf(
				"Standard catalog %s: Standard %s requires a resource attribute with no name",
				origin, s.ID)
		}
		// The name is not only data. The Gateway writes it inside a quoted OTTL
		// string in the compiled config, so quoting keeps anything from breaking out
		// — but an escape sequence the collector's OTTL parser does not accept gives
		// a config that fails at load, on a rollout, for the whole fleet. Narrowing
		// it here keeps the generated statement parseable by construction rather than
		// by the escaping happening to line up, and it is narrower than any real
		// semantic-convention key needs.
		if !attributeName.MatchString(attribute) {
			return fmt.Errorf(
				"Standard catalog %s: Standard %s requires resource attribute %q, which is not a usable attribute name — it is written into a quoted OTTL string in the Gateway's config, so it must be letters, digits, and `. _ - /`, starting with a letter or digit (like `service.name`)",
				origin, s.ID, attribute)
		}
		// It compiles to the same condition twice, joined by `or`, and it means
		// somebody edited a list without reading it. Invisible anywhere but here.
		if requiredOnce[attribute] {
			return fmt.Errorf(
				"Standard catalog %s: Standard %s requires resource attribute %q twice",
				origin, s.ID, attribute)
		}
		requiredOnce[attribute] = true
	}

	// The refusal C6 turns on. A collector cannot evaluate Rego, so a Standard
	// enforced at the pipeline has to become collector processors. When its
	// requirement cannot, the choice is to refuse or to author the pipeline check
	// separately — and the second gives one Standard two definitions that drift,
	// which is the failure this catalog exists to prevent. So: refuse, loudly, at
	// the place the claim was written (ADR 0015).
	if s.EnforcedAtThePipeline() && !r.expressibleAsProcessors() {
		return fmt.Errorf(
			"Standard catalog %s: Standard %s declares `enforced_at: [%s]`, but its %s requirement is not expressible as collector processors — a Gateway inspects one record at a time and cannot answer it, so there is nothing to compile (drop %q from `enforced_at:`)",
			origin, s.ID, EnforcedAtPipeline, kinds[0], EnforcedAtPipeline)
	}
	return nil
}

// Standards is every Standard in the catalog, in the order it declares them.
//
// A copy: the catalog is validated once, at load, and everything downstream —
// the Rego data document, the compiled Gateway processors — assumes what it holds
// is what was validated. Handing out the slice would let a caller edit a Severity
// or a required attribute after the fact, in a value two enforcement points share.
func (c *StandardCatalog) Standards() []Standard {
	if c == nil {
		return nil
	}
	copied := make([]Standard, len(c.standards))
	for i, standard := range c.standards {
		standard.EnforcedAt = slices.Clone(standard.EnforcedAt)
		standard.Requires.ResourceAttributes = slices.Clone(standard.Requires.ResourceAttributes)
		copied[i] = standard
	}
	return copied
}

// PipelineEnforced is every Standard enforced against live telemetry in the
// Gateway, in catalog order. It is what the Control Plane compiles into Pipeline
// Guardrails.
func (c *StandardCatalog) PipelineEnforced() []Standard {
	var selected []Standard
	for _, standard := range c.Standards() {
		if standard.EnforcedAtThePipeline() {
			selected = append(selected, standard)
		}
	}
	return selected
}

// policyPackage matches a Standard's Rego package declaration. The last segment
// is the Standard's id, lower-cased — a package name cannot carry case, which is
// why the id it stands for has to be recovered from the catalog rather than read
// off here.
var policyPackage = regexp.MustCompile(`(?m)^package otel\.guardrail\.standards\.([a-z0-9_]+)`)

// matches checks that a set of Standard policies and this catalog describe the
// same Standards, and refuses when they do not.
//
// Every mismatch it rejects ends the same way if it is allowed through: no
// violation, no error, on a Contract that violates the Standard. A policy reads
// its catalog entry as a Rego data document, and in Rego an absent document is
// not an error — the rule body simply fails and the Standard silently stops
// enforcing. Nothing downstream can tell that from a compliant fleet, so it has
// to be caught here, where both halves are in hand.
//
// It runs at construction rather than in a test because LoadStandards and
// WithStandardCatalog are exported: a caller can assemble any pair, and only this
// function sees the one they assembled.
func (c *StandardCatalog) matches(sources map[string]string) error {
	declared := map[string]Standard{}
	for _, standard := range c.Standards() {
		declared[strings.ToLower(standard.ID)] = standard
	}

	implemented := map[string]bool{}
	for name, source := range sources {
		match := policyPackage.FindStringSubmatch(source)
		if match == nil {
			continue // not a Standard — the aggregator
		}
		standard, known := declared[match[1]]
		if !known {
			return fmt.Errorf(
				"%s implements Standard %s, which the Standard catalog does not declare: it would read an absent catalog entry and report nothing",
				name, match[1])
		}
		implemented[match[1]] = true

		// The catalog entry is reached by an EXACT id, and Rego document keys carry
		// case where package names cannot — so a policy and an entry can agree on the
		// package and still not meet.
		if reference := "data.otel.standards." + standard.ID; !strings.Contains(source, reference) {
			return fmt.Errorf(
				"%s does not read %s, so Standard %s would evaluate against an absent catalog entry and report nothing",
				name, reference, standard.ID)
		}

		// The aggregator picks up every policy in the directory with no registration
		// step, so a Standard moved to `pipeline` while its policy stayed behind would
		// go on failing builds at a point the catalog says it is not enforced at.
		if !slices.Contains(standard.EnforcedAt, EnforcedAtPreflight) {
			return fmt.Errorf(
				"%s implements Standard %s, which the Standard catalog enforces only at %v: remove the policy, or enforce the Standard at %q",
				name, standard.ID, standard.EnforcedAt, EnforcedAtPreflight)
		}

		// And the entry must be the KIND of requirement the policy implements. This is
		// the quietest mismatch of the lot: a policy that iterates
		// requires.resource_attributes against an entry of the other kind iterates an
		// empty list, so a blocking Standard reports nothing and looks fine.
		if err := standard.implementedBy(name, source); err != nil {
			return err
		}
	}

	for _, standard := range c.Standards() {
		if !slices.Contains(standard.EnforcedAt, EnforcedAtPreflight) {
			continue
		}
		if !implemented[strings.ToLower(standard.ID)] {
			return fmt.Errorf(
				"the Standard catalog declares Standard %s enforced at %q, but no policy implements it: it is a requirement the org believes it is checking and is not",
				standard.ID, EnforcedAtPreflight)
		}
	}
	return nil
}

// implementedBy checks that a policy reads the requirement kind its catalog
// entry declares. The two are separate statements — the entry claims a kind, the
// Rego implements one — and only `resource_attributes` carries its content in
// the catalog, so only a matching pair is coherent.
func (s Standard) implementedBy(name, source string) error {
	readsAttributes := strings.Contains(source, ".requires.resource_attributes")
	readsTaxonomy := strings.Contains(source, "data.otel.taxonomy")

	if s.Requires.ResourceAttributes != nil && !readsAttributes {
		return fmt.Errorf(
			"Standard %s requires resource attributes, but %s does not read them: it is enforcing something the catalog does not describe",
			s.ID, name)
	}
	if readsAttributes && s.Requires.ResourceAttributes == nil {
		return fmt.Errorf(
			"%s reads the resource attributes Standard %s requires, but the catalog declares a different requirement kind for it, so the list it iterates is empty and the Standard reports nothing",
			name, s.ID)
	}
	if s.Requires.TierMandatorySignals && !readsTaxonomy {
		return fmt.Errorf(
			"Standard %s requires its Service Tier's mandatory Signals, but %s does not read the Service Tier Taxonomy",
			s.ID, name)
	}
	if readsTaxonomy && !s.Requires.TierMandatorySignals {
		return fmt.Errorf(
			"%s reads the Service Tier Taxonomy, but Standard %s does not declare `tier_mandatory_signals`: the catalog claims a requirement kind the policy does not implement",
			name, s.ID)
	}
	return nil
}

// asRegoData is the catalog in the shape the policies read: keyed by Standard
// id, each carrying its Severity and its requirement. The policies address
// `data.otel.standards.S1.severity` and `.requires.resource_attributes`, so a
// Standard's requirement and its Severity have exactly one definition and the
// Gateway compiler reads the same one.
func (c *StandardCatalog) asRegoData() map[string]any {
	entries := make(map[string]any, len(c.Standards()))
	for _, standard := range c.Standards() {
		required := make([]any, 0, len(standard.Requires.ResourceAttributes))
		for _, attribute := range standard.Requires.ResourceAttributes {
			required = append(required, attribute)
		}
		entries[standard.ID] = map[string]any{
			"severity": string(standard.Severity),
			"requires": map[string]any{"resource_attributes": required},
		}
	}
	return entries
}
