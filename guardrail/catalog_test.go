package guardrail_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ghantakiran/OTEL/contract"
	"github.com/ghantakiran/OTEL/guardrail"
)

// The Standard catalog is authored once and read twice: by the Rego that runs a
// Preflight Guardrail, and by the Control Plane that compiles a Pipeline
// Guardrail into the Gateway. These tests are about the document, not about
// either consumer.

func TestEveryStandardDeclaresWhereItIsEnforced(t *testing.T) {
	catalog, err := guardrail.CentralStandards()
	if err != nil {
		t.Fatalf("the Standard catalog shipped in the binary does not load: %v", err)
	}
	if len(catalog.Standards()) == 0 {
		t.Fatal("the shipped Standard catalog declares no Standards")
	}

	for _, standard := range catalog.Standards() {
		if len(standard.EnforcedAt) == 0 {
			t.Errorf("Standard %s declares no enforcement point", standard.ID)
		}
	}
}

func TestACatalogDeclaringNoStandardsIsRefused(t *testing.T) {
	// The worst state this document can reach, and the one every other check here
	// assumes away: a catalog with no entries validates perfectly, because there is
	// nothing to validate. Both enforcement points then disarm silently — the
	// Gateway compiles no Guardrail processor at all, and every policy reads an
	// absent data document and reports nothing, so `check` returns zero violations
	// and a nil error on a Contract that violates everything.
	//
	// Rego's absence is silence, so this cannot be caught downstream. It is the
	// same refusal guardrail/tiers.yaml already makes for a taxonomy with no tiers.
	for name, body := range map[string]string{
		"no standards key": `apiVersion: guardrail.otel/v1
kind: StandardCatalog
`,
		"an empty list": `apiVersion: guardrail.otel/v1
kind: StandardCatalog
standards: []
`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := guardrail.LoadStandards(writeCatalog(t, body)); err == nil {
				t.Error("a catalog declaring no Standards was accepted; nothing would be enforced at either point")
			}
		})
	}
}

func TestACatalogWithAMisspeltKeyIsRefusedRatherThanReadAsEmpty(t *testing.T) {
	// `standard:` instead of `standards:` decodes cleanly into a document with no
	// entries — the disarmed catalog above, reached by a typo rather than by
	// somebody deleting everything. A misspelt key anywhere in this document is the
	// same shape: `severity` becomes an absent Severity, `enforced_at` an absent
	// enforcement point. Those two are caught by name; the top-level one has no
	// entry to be caught on, so the decoder has to refuse what it does not know.
	for name, body := range map[string]string{
		"the standards list": `apiVersion: guardrail.otel/v1
kind: StandardCatalog
standard:
  - standard: S9
    severity: block
    enforced_at: [preflight]
    requires:
      resource_attributes: [service.name]
`,
		"a field of an entry": `apiVersion: guardrail.otel/v1
kind: StandardCatalog
standards:
  - standard: S9
    severity: block
    enforced_at: [preflight]
    require:
      resource_attributes: [service.name]
`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := guardrail.LoadStandards(writeCatalog(t, body)); err == nil {
				t.Error("a misspelt key was read as an absent value rather than refused")
			}
		})
	}
}

func TestAStandardDeclaringNoEnforcementPointIsRefused(t *testing.T) {
	// The same treatment an absent Severity gets, and for the same reason: a
	// Standard that forgets to say where it is enforced must not quietly stop
	// being enforced anywhere. A broken catalog is the platform team's problem.
	_, err := guardrail.LoadStandards(writeCatalog(t, `apiVersion: guardrail.otel/v1
kind: StandardCatalog
standards:
  - standard: S9
    severity: warn
    requires:
      resource_attributes: [service.namespace]
`))
	if err == nil {
		t.Fatal("a Standard declaring no enforcement point was accepted")
	}
	if !strings.Contains(err.Error(), "S9") {
		t.Errorf("the error does not name the Standard at fault: %v", err)
	}
}

func TestAStandardDeclaringAnUnrecognisedEnforcementPointIsRefused(t *testing.T) {
	_, err := guardrail.LoadStandards(writeCatalog(t, `apiVersion: guardrail.otel/v1
kind: StandardCatalog
standards:
  - standard: S9
    severity: warn
    enforced_at: [runtime]
    requires:
      resource_attributes: [service.namespace]
`))
	if err == nil {
		t.Fatal("a Standard declaring the enforcement point \"runtime\" was accepted")
	}
	if !strings.Contains(err.Error(), "S9") || !strings.Contains(err.Error(), "runtime") {
		t.Errorf("the error names neither the Standard nor the unrecognised point: %v", err)
	}
}

func TestAStandardEnforcedAtThePipelineWhoseRequirementIsNotExpressibleThereIsRefused(t *testing.T) {
	// The load-bearing refusal for C6. A collector cannot evaluate Rego, so a
	// Standard enforced at the pipeline has to become collector processors. When
	// its requirement cannot become processors, the honest outcome is refusing to
	// compile — not writing a second, weaker definition of the Standard beside the
	// Rego one, which is the drift this catalog exists to prevent.
	_, err := guardrail.LoadStandards(writeCatalog(t, `apiVersion: guardrail.otel/v1
kind: StandardCatalog
standards:
  - standard: S9
    severity: block
    enforced_at: [pipeline]
    requires:
      tier_mandatory_signals: true
`))
	if err == nil {
		t.Fatal("a Standard was allowed to declare pipeline enforcement it cannot deliver")
	}
	if !strings.Contains(err.Error(), "S9") {
		t.Errorf("the error does not name the Standard at fault: %v", err)
	}
}

func TestAStandardRequiringNothingIsRefused(t *testing.T) {
	// A Standard with no requirement is a Standard nothing can enforce. At
	// Preflight it silently never fires; at the pipeline it compiles to no
	// processor at all, so the Gateway would report it as enforced while checking
	// nothing.
	_, err := guardrail.LoadStandards(writeCatalog(t, `apiVersion: guardrail.otel/v1
kind: StandardCatalog
standards:
  - standard: S9
    severity: block
    enforced_at: [preflight, pipeline]
`))
	if err == nil {
		t.Fatal("a Standard requiring nothing was accepted")
	}
	if !strings.Contains(err.Error(), "S9") {
		t.Errorf("the error does not name the Standard at fault: %v", err)
	}
}

func TestAStandardDeclaringAnEmptyRequiredAttributeListIsRefused(t *testing.T) {
	// `resource_attributes: []` is somebody writing something, and the only thing
	// it can mean is "require nothing" — a Standard that reads as enforced and
	// checks nothing, at either point. Reading it as its opposite (or as absent)
	// is the widest gap there is between what was written and what runs.
	_, err := guardrail.LoadStandards(writeCatalog(t, `apiVersion: guardrail.otel/v1
kind: StandardCatalog
standards:
  - standard: S9
    severity: block
    enforced_at: [preflight, pipeline]
    requires:
      resource_attributes: []
`))
	if err == nil {
		t.Fatal("a Standard requiring an empty list of resource attributes was accepted")
	}
	if !strings.Contains(err.Error(), "S9") {
		t.Errorf("the error does not name the Standard at fault: %v", err)
	}
}

func TestAStandardDeclaredTwiceIsRefused(t *testing.T) {
	// A Standard's id is its identity at both enforcement points: it is what a
	// Preflight violation is reported under, and it is the attribute key the
	// Gateway tags live telemetry with. Two entries sharing an id would write the
	// same attribute with two Severities, and which one an operator saw would
	// depend on statement order.
	_, err := guardrail.LoadStandards(writeCatalog(t, `apiVersion: guardrail.otel/v1
kind: StandardCatalog
standards:
  - standard: S9
    severity: block
    enforced_at: [pipeline]
    requires:
      resource_attributes: [service.name]
  - standard: S9
    severity: warn
    enforced_at: [pipeline]
    requires:
      resource_attributes: [service.namespace]
`))
	if err == nil {
		t.Fatal("a Standard declared twice was accepted")
	}
	if !strings.Contains(err.Error(), "S9") {
		t.Errorf("the error does not name the Standard at fault: %v", err)
	}
}

func TestTwoStandardsWhoseIdsDifferOnlyByCaseAreRefused(t *testing.T) {
	// `S9` and `s9` are two entries and two attribute keys, but they are one Rego
	// package (`otel.guardrail.standards.s9` — package names cannot carry case) and
	// one thing to a reader. Two Standards nobody can tell apart, one of which has
	// no policy and therefore silently enforces nothing at preflight.
	_, err := guardrail.LoadStandards(writeCatalog(t, `apiVersion: guardrail.otel/v1
kind: StandardCatalog
standards:
  - standard: S9
    severity: block
    enforced_at: [pipeline]
    requires:
      resource_attributes: [service.name]
  - standard: s9
    severity: warn
    enforced_at: [pipeline]
    requires:
      resource_attributes: [service.namespace]
`))
	if err == nil {
		t.Fatal("two Standards whose ids differ only by case were accepted")
	}
}

func TestAStandardRequiringTheSameAttributeTwiceIsRefused(t *testing.T) {
	// It compiles to the same OTTL condition twice, joined by `or`, and it means
	// somebody edited a list and did not read it. Cheap to refuse where it was
	// written; invisible everywhere else.
	_, err := guardrail.LoadStandards(writeCatalog(t, `apiVersion: guardrail.otel/v1
kind: StandardCatalog
standards:
  - standard: S9
    severity: warn
    enforced_at: [pipeline]
    requires:
      resource_attributes: [service.name, service.name]
`))
	if err == nil {
		t.Fatal("a Standard requiring the same resource attribute twice was accepted")
	}
	if !strings.Contains(err.Error(), "service.name") {
		t.Errorf("the error does not name the repeated attribute: %v", err)
	}
}

func TestARequiredAttributeNameThatCannotBeWrittenIntoAnOTTLStatementIsRefused(t *testing.T) {
	// A required attribute name is not only data: the Gateway writes it inside a
	// quoted OTTL string in the compiled config. Quoting it escapes it, so nothing
	// can break out — but an escape sequence the collector's OTTL parser does not
	// accept gives a config that fails at load, on a rollout, for the whole fleet.
	// Narrowing the name where it is written keeps the generated statement
	// parseable by construction rather than by the escaping happening to line up.
	for _, attribute := range []string{`service."name`, `service\name`, "service name", "service.naïve", "service.name\n"} {
		_, err := guardrail.LoadStandards(writeCatalog(t, `apiVersion: guardrail.otel/v1
kind: StandardCatalog
standards:
  - standard: S9
    severity: warn
    enforced_at: [pipeline]
    requires:
      resource_attributes: [`+strconv.Quote(attribute)+`]
`))
		if err == nil {
			t.Errorf("required attribute %q was accepted; it is written into a quoted OTTL string", attribute)
		}
	}

	// And the ordinary shapes a real semantic convention takes must all still load.
	for _, attribute := range []string{"service.name", "http.request.method", "k8s.pod.name", "url.full", "host_id", "aws.s3/key"} {
		if _, err := guardrail.LoadStandards(writeCatalog(t, `apiVersion: guardrail.otel/v1
kind: StandardCatalog
standards:
  - standard: S9
    severity: warn
    enforced_at: [pipeline]
    requires:
      resource_attributes: [`+strconv.Quote(attribute)+`]
`)); err != nil {
			t.Errorf("required attribute %q is an ordinary attribute name but was refused: %v", attribute, err)
		}
	}
}

func TestAStandardIdThatIsNotAnAttributeNameSegmentIsRefused(t *testing.T) {
	// The id is not a label. The Gateway tags telemetry with
	// `otel.guardrail.violation.<id>`, so an id containing a quote, a bracket or a
	// dot is either unquotable in an OTTL statement or silently reshapes the
	// attribute namespace. Narrow it where it is written rather than escaping it
	// at every use.
	// A hyphen is in the list because it is legal in an attribute name and illegal
	// in a Rego package name: `package otel.guardrail.standards.s-9` does not
	// compile, so Standard `S-9` could be tagged at the Gateway and have no policy
	// able to exist at preflight — the two enforcement points disagreeing with
	// nothing to say so.
	for _, id := range []string{`S"9`, "S 9", "S.9", "s9!", "S-9"} {
		_, err := guardrail.LoadStandards(writeCatalog(t, `apiVersion: guardrail.otel/v1
kind: StandardCatalog
standards:
  - standard: `+strconv.Quote(id)+`
    severity: warn
    enforced_at: [pipeline]
    requires:
      resource_attributes: [service.namespace]
`))
		if err == nil {
			t.Errorf("Standard id %q was accepted; it becomes an attribute name segment", id)
		}
	}
}

func TestAStandardHonoursTheCatalogItIsGivenRatherThanOneOfItsOwn(t *testing.T) {
	// The load-bearing test for one catalog, two consumers. S1 requires an
	// attribute that appears in no .rego file, at a Severity that appears in no
	// .rego file, purely because the catalog handed to the Guardrail says so. If a
	// policy ever goes back to declaring its own requirement or its own Severity,
	// this fails.
	custom := writeCatalog(t, `apiVersion: guardrail.otel/v1
kind: StandardCatalog
standards:
  - standard: S1
    severity: info
    enforced_at: [preflight]
    requires:
      resource_attributes: [cloud.region]
  - standard: S2
    severity: block
    enforced_at: [preflight]
    requires:
      tier_mandatory_signals: true
  - standard: S3
    severity: warn
    enforced_at: [preflight]
    requires:
      resource_attributes: [service.namespace]
`)

	// Declares every attribute the shipped catalog requires, and not the one this
	// catalog does.
	c := compliantContract()
	violations := checkWithCatalog(t, custom, c)

	var s1 *guardrail.Violation
	for i, v := range violations {
		if v.Standard == "S1" {
			s1 = &violations[i]
		}
	}
	if s1 == nil {
		t.Fatalf("S1 did not enforce the requirement the catalog gave it: %+v", violations)
	}
	if !strings.Contains(s1.Message, "cloud.region") {
		t.Errorf("S1 named %q, not the attribute the catalog requires", s1.Message)
	}
	if s1.Severity != guardrail.SeverityInfo {
		t.Errorf("S1 enforced at Severity %q; the catalog declares info", s1.Severity)
	}
}

func TestNoPolicyDeclaresARequiredAttributeOrASeverityItself(t *testing.T) {
	// The guard against the catalog drifting back into policy — the same guard
	// #28 put on the Service Tier taxonomy. A Rego file that names a required
	// attribute or a Severity is a second definition of a Standard, and the
	// Gateway compiler would keep reading the first one.
	forbidden := regexp.MustCompile(`"(info|warn|block|service\.[a-z.]+|deployment\.[a-z.]+)"`)

	for name, source := range policySources(t) {
		if found := forbidden.FindAllString(source, -1); len(found) > 0 {
			t.Errorf("%s names %s directly; the catalog is data the policy reads, not something it declares",
				name, strings.Join(found, ", "))
		}
	}
}

func TestEveryPolicyHasACatalogEntryAndEveryPreflightStandardHasAPolicy(t *testing.T) {
	// A policy with no catalog entry reads `data.otel.standards.S9`, finds nothing,
	// and emits no violation — a Standard that silently stopped enforcing. A
	// preflight catalog entry with no policy is a Standard the catalog advertises
	// and nothing runs. Neither is visible from any output.
	catalog, err := guardrail.CentralStandards()
	if err != nil {
		t.Fatalf("load the Standard catalog: %v", err)
	}

	declared := map[string]guardrail.Standard{}
	for _, standard := range catalog.Standards() {
		declared[strings.ToLower(standard.ID)] = standard
	}

	packageLine := regexp.MustCompile(`(?m)^package otel\.guardrail\.standards\.([a-z0-9_-]+)`)
	implemented := map[string]bool{}
	for name, source := range policySources(t) {
		match := packageLine.FindStringSubmatch(source)
		if match == nil {
			continue // the aggregator, which is not a Standard
		}
		implemented[match[1]] = true
		standard, known := declared[match[1]]
		if !known {
			t.Errorf("%s implements Standard %s, which the catalog does not declare — it would read an absent data document and enforce nothing",
				name, match[1])
			continue
		}
		// A policy for a Standard the catalog does not enforce at preflight would go
		// on failing builds at a point the catalog says it is not enforced at. The
		// Guardrail refuses that at run time; this catches it before anything has to
		// violate the Standard for it to show.
		if !slices.Contains(standard.EnforcedAt, guardrail.EnforcedAtPreflight) {
			t.Errorf("%s implements Standard %s, which the catalog enforces only at %v",
				name, standard.ID, standard.EnforcedAt)
		}
	}

	for id, standard := range declared {
		if slices.Contains(standard.EnforcedAt, guardrail.EnforcedAtPreflight) && !implemented[id] {
			t.Errorf("the catalog declares Standard %s enforced at preflight, but no policy implements it", standard.ID)
		}
	}
}

func TestAViolationReportedUnderAStandardThePolicyIsNotIsRefused(t *testing.T) {
	// The one mismatch construction cannot catch, and why the check at Check time
	// stays. Matching the policies to the catalog pairs each POLICY with an entry,
	// via its package name — but the Standard id inside the violation object is a
	// separate literal in the Rego body, and nothing ties the two together. A
	// policy for one Standard can therefore report under another's id, or under an
	// id nothing declares, and it would inherit that Standard's Waivers, its
	// graduation deadline, and its Severity treatment.
	preflight := preflightOver(t, `package otel.guardrail.standards.sx

violation contains v if {
	some attribute in data.otel.standards.SX.requires.resource_attributes
	v := {"standard": "SOMETHING_ELSE", "severity": "block", "message": sprintf("SX requires %q", [attribute])}
}
`, catalogDeclaring("SX"))

	_, err := preflight.Check(context.Background(), contract.Contract{ServiceName: "any-service"})
	if err == nil {
		t.Fatal("a violation was reported under a Standard the catalog does not declare")
	}
	if !strings.Contains(err.Error(), "SOMETHING_ELSE") {
		t.Errorf("the error does not name the Standard at fault: %v", err)
	}
}

func TestAGuardrailRefusesToRunPoliciesAndACatalogThatDoNotMatch(t *testing.T) {
	// These four are what the shipped-catalog tests below check statically, moved
	// to where they hold for ANY caller: LoadStandards and WithStandardCatalog are
	// exported, and NewPreflight is the only place that sees the actual pair of
	// policies and catalog somebody assembled.
	//
	// Every case here ends the same way if it is not refused — no violation, no
	// error, on a Contract that violates everything. Rego's absence is silence, so
	// none of them can be caught after the fact.
	for name, catalog := range map[string]string{
		// The policy reads data.otel.standards.S1, finds nothing, and reports nothing.
		"a policy the catalog does not declare": `apiVersion: guardrail.otel/v1
kind: StandardCatalog
standards:
  - standard: S2
    severity: block
    enforced_at: [preflight]
    requires:
      tier_mandatory_signals: true
`,
		// Rego document keys carry case; package names cannot. `data.otel.standards.S1`
		// does not reach an entry called `s1`.
		"an entry whose id differs from the one its policy reads": `apiVersion: guardrail.otel/v1
kind: StandardCatalog
standards:
  - standard: s1
    severity: block
    enforced_at: [preflight]
    requires:
      resource_attributes: [service.name]
  - standard: S2
    severity: block
    enforced_at: [preflight]
    requires:
      tier_mandatory_signals: true
  - standard: S3
    severity: warn
    enforced_at: [preflight]
    requires:
      resource_attributes: [service.namespace]
`,
		// S1's policy iterates requires.resource_attributes; an entry of the other kind
		// gives it an empty list to iterate, so a blocking Standard silently dies.
		"an entry whose requirement kind its policy does not implement": `apiVersion: guardrail.otel/v1
kind: StandardCatalog
standards:
  - standard: S1
    severity: block
    enforced_at: [preflight]
    requires:
      tier_mandatory_signals: true
  - standard: S2
    severity: block
    enforced_at: [preflight]
    requires:
      tier_mandatory_signals: true
  - standard: S3
    severity: warn
    enforced_at: [preflight]
    requires:
      resource_attributes: [service.namespace]
`,
		// The aggregator picks the policy up regardless, so it would fail builds at a
		// point the catalog says it is not enforced at.
		"an entry its policy implements but which is not enforced at preflight": `apiVersion: guardrail.otel/v1
kind: StandardCatalog
standards:
  - standard: S1
    severity: block
    enforced_at: [pipeline]
    requires:
      resource_attributes: [service.name]
  - standard: S2
    severity: block
    enforced_at: [preflight]
    requires:
      tier_mandatory_signals: true
  - standard: S3
    severity: warn
    enforced_at: [preflight]
    requires:
      resource_attributes: [service.namespace]
`,
	} {
		t.Run(name, func(t *testing.T) {
			standards, err := guardrail.LoadStandards(writeCatalog(t, catalog))
			if err != nil {
				t.Fatalf("load Standard catalog: %v", err)
			}
			if _, err := guardrail.NewPreflight(guardrail.StandardPolicies(), guardrail.WithStandardCatalog(standards)); err == nil {
				t.Error("the Guardrail was built from policies and a catalog that do not match; a Standard would enforce nothing and say nothing")
			}
		})
	}
}

func TestAGuardrailRefusesACatalogPromisingAPreflightStandardNoPolicyImplements(t *testing.T) {
	// The other direction: a Standard the catalog advertises as enforced before
	// deploy, with nothing to run. It is not a violation of anything — it is a
	// requirement the org believes it is checking and is not.
	standards, err := guardrail.LoadStandards(writeCatalog(t, `apiVersion: guardrail.otel/v1
kind: StandardCatalog
standards:
  - standard: S1
    severity: block
    enforced_at: [preflight]
    requires:
      resource_attributes: [service.name]
  - standard: S2
    severity: block
    enforced_at: [preflight]
    requires:
      tier_mandatory_signals: true
  - standard: S3
    severity: warn
    enforced_at: [preflight]
    requires:
      resource_attributes: [service.namespace]
  - standard: S9
    severity: block
    enforced_at: [preflight]
    requires:
      resource_attributes: [cloud.region]
`))
	if err != nil {
		t.Fatalf("load Standard catalog: %v", err)
	}

	_, err = guardrail.NewPreflight(guardrail.StandardPolicies(), guardrail.WithStandardCatalog(standards))
	if err == nil {
		t.Fatal("the Guardrail was built with a preflight Standard no policy implements")
	}
	if !strings.Contains(err.Error(), "S9") {
		t.Errorf("the error does not name the Standard at fault: %v", err)
	}
}

func TestTheShippedPoliciesAndCatalogMatch(t *testing.T) {
	// The pair that actually ships, through the same check.
	if _, err := guardrail.NewPreflight(guardrail.StandardPolicies()); err != nil {
		t.Fatalf("the shipped policies and Standard catalog do not match: %v", err)
	}
}

func TestNothingShippedNamesAStandardTheCatalogDoesNotDeclare(t *testing.T) {
	// Three documents name a Standard by id, and until there was a catalog there
	// was nothing to check them against. A Waiver for a Standard that does not
	// exist holds nothing back; a graduation deadline for one defers nothing. Both
	// are references nobody walks — they parse, they look filed, and they do
	// nothing, which is precisely the state a Waiver must never reach (ADR 0003).
	catalog, err := guardrail.CentralStandards()
	if err != nil {
		t.Fatalf("load the Standard catalog: %v", err)
	}
	declared := map[string]bool{}
	for _, standard := range catalog.Standards() {
		declared[standard.ID] = true
	}

	register, err := guardrail.CentralWaiverRegister()
	if err != nil {
		t.Fatalf("load the Waiver register: %v", err)
	}
	for _, waiver := range register.Waivers() {
		if !declared[waiver.Standard] {
			t.Errorf("guardrail/waivers.yaml waives Standard %s for %s, which the catalog does not declare — the Waiver holds nothing back",
				waiver.Standard, waiver.ServiceName)
		}
	}

	schedule, err := guardrail.CentralEnforcementSchedule()
	if err != nil {
		t.Fatalf("load the enforcement schedule: %v", err)
	}
	for _, standard := range schedule.GraduatingStandards() {
		if !declared[standard] {
			t.Errorf("guardrail/enforcement.yaml publishes a graduation deadline for Standard %s, which the catalog does not declare — it defers nothing",
				standard)
		}
	}

	// And the other direction, which is the one that bites: a Standard that can
	// block a build needs a deadline, or a legacy service violating it can be
	// neither blocked nor deferred (ADR 0003). A Standard enforced only at the
	// pipeline never fails a build, so it needs none.
	for _, standard := range catalog.Standards() {
		if standard.Severity != guardrail.SeverityBlock {
			continue
		}
		if !slices.Contains(standard.EnforcedAt, guardrail.EnforcedAtPreflight) {
			continue
		}
		if _, published := schedule.Graduation(standard.ID); !published {
			t.Errorf("Standard %s can block a build but guardrail/enforcement.yaml publishes no graduation deadline for it", standard.ID)
		}
	}
}

func TestEveryPolicyAddressesItsCatalogEntryByItsExactId(t *testing.T) {
	// The link between the two halves of a Standard is a Rego path — S1's policy
	// reads `data.otel.standards.S1` — and Rego document keys are case-sensitive
	// while a package name cannot be. So a catalog entry renamed to `s1` would
	// leave every policy reading an absent document and emitting no violation at
	// all: not a wrong answer, no answer, from a catalog and a policy that both
	// look present. Matching the package name case-insensitively is not enough to
	// catch that; the exact path is.
	catalog, err := guardrail.CentralStandards()
	if err != nil {
		t.Fatalf("load the Standard catalog: %v", err)
	}

	sources := policySources(t)
	packageLine := regexp.MustCompile(`(?m)^package otel\.guardrail\.standards\.([a-z0-9_-]+)`)

	for _, standard := range catalog.Standards() {
		if !slices.Contains(standard.EnforcedAt, guardrail.EnforcedAtPreflight) {
			continue
		}
		var found bool
		for name, source := range sources {
			match := packageLine.FindStringSubmatch(source)
			if match == nil || !strings.EqualFold(match[1], standard.ID) {
				continue
			}
			found = true
			if reference := "data.otel.standards." + standard.ID; !strings.Contains(source, reference) {
				t.Errorf("%s does not read %s; it would evaluate against an absent catalog entry and report nothing",
					name, reference)
			}
		}
		if !found {
			t.Errorf("no policy implements Standard %s", standard.ID)
		}
	}
}

func TestEveryPolicyDoesTheKindOfThingItsCatalogEntryClaims(t *testing.T) {
	// The one place the catalog could still drift from the policy. For a
	// `resource_attributes` Standard the catalog IS the requirement, so there is
	// nothing to disagree with — the policy reads the list. But
	// `tier_mandatory_signals` is only a marker: the requirement lives in the
	// policy and the Service Tier Taxonomy, and the entry merely claims that is the
	// shape of it.
	//
	// A mislabelled kind matters because the kind is what decides pipeline
	// eligibility. Relabelling S2 as `resource_attributes` would let it declare
	// `enforced_at: [pipeline]` and compile a Gateway check that S2's Rego does not
	// perform — one Standard, two meanings, which is the whole thing this catalog
	// exists to prevent. So the claim is checked against what the policy reads.
	catalog, err := guardrail.CentralStandards()
	if err != nil {
		t.Fatalf("load the Standard catalog: %v", err)
	}
	declared := map[string]guardrail.Standard{}
	for _, standard := range catalog.Standards() {
		declared[strings.ToLower(standard.ID)] = standard
	}

	packageLine := regexp.MustCompile(`(?m)^package otel\.guardrail\.standards\.([a-z0-9_-]+)`)
	for name, source := range policySources(t) {
		match := packageLine.FindStringSubmatch(source)
		if match == nil {
			continue // the aggregator
		}
		standard, known := declared[match[1]]
		if !known {
			continue // already reported by TestEveryPolicyHasACatalogEntry…
		}

		readsTaxonomy := strings.Contains(source, "data.otel.taxonomy")
		readsAttributes := strings.Contains(source, ".requires.resource_attributes")

		if readsTaxonomy && !standard.Requires.TierMandatorySignals {
			t.Errorf("%s reads the Service Tier Taxonomy, but Standard %s does not declare `tier_mandatory_signals` — the catalog claims a kind the policy does not implement",
				name, standard.ID)
		}
		if readsAttributes && standard.Requires.ResourceAttributes == nil {
			t.Errorf("%s reads .requires.resource_attributes, but Standard %s does not declare that requirement kind",
				name, standard.ID)
		}
		if standard.Requires.TierMandatorySignals && !readsTaxonomy {
			t.Errorf("Standard %s declares `tier_mandatory_signals`, but %s does not read the Service Tier Taxonomy",
				standard.ID, name)
		}
		if standard.Requires.ResourceAttributes != nil && !readsAttributes {
			t.Errorf("Standard %s declares `resource_attributes`, but %s does not read them — it is enforcing something the catalog does not describe",
				standard.ID, name)
		}
	}
}

func policySources(t *testing.T) map[string]string {
	t.Helper()

	catalog, err := os.ReadDir("policies")
	if err != nil {
		t.Fatalf("read the Standard catalog: %v", err)
	}

	sources := map[string]string{}
	for _, entry := range catalog {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".rego") {
			continue
		}
		source, err := os.ReadFile(filepath.Join("policies", entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		sources[entry.Name()] = string(source)
	}
	return sources
}

// compliantContract is a Telemetry Contract that meets every Standard the
// shipped catalog declares.
func compliantContract() contract.Contract {
	return contract.Contract{
		APIVersion: contract.APIVersion, Kind: contract.Kind,
		ServiceName: "checkout-api", Owner: "team-payments", Tier: "tier-1",
		Signals: []string{"traces", "metrics", "logs"},
		ResourceAttributes: map[string]string{
			"service.name": "checkout-api", "service.version": "2.4.1",
			"deployment.environment": "production",
			"service.namespace":      "payments", "service.instance.id": "checkout-api-0",
		},
	}
}

// checkWithCatalog runs the Preflight Guardrail against a given Standard catalog.
func checkWithCatalog(t *testing.T, catalogPath string, c contract.Contract) []guardrail.Violation {
	t.Helper()

	standards, err := guardrail.LoadStandards(catalogPath)
	if err != nil {
		t.Fatalf("load Standard catalog: %v", err)
	}
	preflight, err := guardrail.NewPreflight(guardrail.StandardPolicies(), guardrail.WithStandardCatalog(standards))
	if err != nil {
		t.Fatalf("new Preflight Guardrail: %v", err)
	}
	result, err := preflight.Check(context.Background(), c)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	return result.Violations
}

func writeCatalog(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "standards.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write Standard catalog: %v", err)
	}
	return path
}
