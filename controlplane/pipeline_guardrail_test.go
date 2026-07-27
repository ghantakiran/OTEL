package controlplane_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ghantakiran/OTEL/controlplane"
	"github.com/ghantakiran/OTEL/guardrail"
)

// A Pipeline Guardrail is the same Standard as a Preflight Guardrail, enforced
// against live telemetry in the Gateway instead of against a declared Telemetry
// Contract before deploy. A collector cannot evaluate Rego, so what runs is
// collector processors — compiled from the same catalog entry the Rego reads, so
// that the Standard has one definition rather than two (ADR 0015).

func TestAStandardEnforcedAtThePipelineCompilesIntoTheGateway(t *testing.T) {
	config, err := controlplane.CompileGateway(gatewayDeclaration(t), profiles(t), standardsFrom(t, `apiVersion: guardrail.otel/v1
kind: StandardCatalog
standards:
  - standard: S9
    severity: block
    enforced_at: [pipeline]
    requires:
      resource_attributes: [deployment.environment]
`))
	if err != nil {
		t.Fatalf("compile the Gateway: %v", err)
	}

	if _, defined := config.Processors["transform/guardrail"]; !defined {
		t.Fatalf("the Gateway defines no Pipeline Guardrail processor; it has %v", processorNames(config))
	}
	for signal, pipeline := range config.Service.Pipelines {
		if !contains(pipeline.Processors, "transform/guardrail") {
			t.Errorf("the %s pipeline does not run the Pipeline Guardrail: %v", signal, pipeline.Processors)
		}
	}

	rendered := renderedYAML(t, config)
	if !strings.Contains(rendered, `deployment.environment`) {
		t.Errorf("the compiled Pipeline Guardrail does not check the attribute the catalog requires:\n%s", rendered)
	}
	if !strings.Contains(rendered, `otel.guardrail.violation.S9`) {
		t.Errorf("the compiled Pipeline Guardrail does not tag telemetry with the Standard it violated:\n%s", rendered)
	}
}

func TestSeverityDecidesHowLoudlyAViolationIsMarkedAndNeverWhetherTelemetrySurvives(t *testing.T) {
	// The Severity mapping, and the decision under it (ADR 0015). A `block`
	// Standard buys the record the one field an operator alerts on; it does not
	// buy the record's deletion. Dropping would destroy the evidence of the
	// violation at the moment it is most wanted, and make a non-compliant service
	// indistinguishable from one that is down.
	config, err := controlplane.CompileGateway(gatewayDeclaration(t), profiles(t), standardsFrom(t, `apiVersion: guardrail.otel/v1
kind: StandardCatalog
standards:
  - standard: SBLOCK
    severity: block
    enforced_at: [pipeline]
    requires:
      resource_attributes: [service.version]
  - standard: SWARN
    severity: warn
    enforced_at: [pipeline]
    requires:
      resource_attributes: [service.namespace]
  - standard: SINFO
    severity: info
    enforced_at: [pipeline]
    requires:
      resource_attributes: [cloud.region]
`))
	if err != nil {
		t.Fatalf("compile the Gateway: %v", err)
	}
	rendered := renderedYAML(t, config)

	for standard, severity := range map[string]string{"SBLOCK": "block", "SWARN": "warn", "SINFO": "info"} {
		tag := `set(attributes["otel.guardrail.violation.` + standard + `"], "` + severity + `")`
		if !strings.Contains(rendered, tag) {
			t.Errorf("a %s Standard does not tag the record with its Severity; wanted %s in:\n%s", severity, tag, rendered)
		}
	}

	// The roll-up: one low-cardinality field to count and alert on, written only
	// by a Standard the org blocks builds for.
	blocking := strings.Count(rendered, `set(attributes["otel.guardrail.blocking"], true)`)
	if blocking == 0 {
		t.Errorf("a block Standard does not raise the field an operator alerts on:\n%s", rendered)
	}
	// Once per Signal, for the one blocking Standard — not for the warn or info ones.
	if want := 3; blocking != want {
		t.Errorf("the blocking roll-up is written %d times, want %d — once per Signal for the one block Standard", blocking, want)
	}

	// Nothing anywhere discards or diverts a record. This is the whole decision.
	// (The Guardrail does delete attributes — its own, and only its own, so a
	// service cannot forge a verdict. That is asserted separately, on what the
	// clearing pattern reaches.)
	for _, forbidden := range []string{"filter", "drop", "route"} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("the compiled Pipeline Guardrail mentions %q; Severity must never decide whether telemetry survives:\n%s", forbidden, rendered)
		}
	}
}

func TestAServiceCannotForgeItsOwnGuardrailVerdict(t *testing.T) {
	// Nothing stops a service emitting otel.guardrail.blocking=false on its own
	// resource, and the Agent forwards whatever it is given. A Guardrail that only
	// ever `set`s on violation would leave that forgery in place, so compliant-
	// looking telemetry would be something a service could simply assert — and the
	// one field an operator alerts on would be the easiest to suppress.
	//
	// So the verdict namespace is cleared first, unconditionally, and then written.
	// The Gateway's verdict is the only thing in it by construction, not by
	// everyone remembering not to write there.
	config, err := controlplane.CompileGateway(gatewayDeclaration(t), profiles(t), orgStandards(t))
	if err != nil {
		t.Fatalf("compile the Gateway: %v", err)
	}

	guardrails, defined := config.Processors["transform/guardrail"].(map[string]any)
	if !defined {
		t.Fatalf("the Gateway defines no Pipeline Guardrail processor: %v", config.Processors)
	}

	// Clearing the resource is not enough. A service can set the same key on a
	// span, a datapoint or a log record, and most Backends flatten record and
	// resource attributes into one queryable field — so a forged record-level
	// `otel.guardrail.blocking` would answer the operator's query just as well as
	// a resource-level one. Every context that carries attributes is swept.
	wantContexts := map[string][]string{
		"trace_statements":  {"resource", "scope", "span", "spanevent"},
		"metric_statements": {"resource", "scope", "datapoint"},
		"log_statements":    {"resource", "scope", "log"},
	}

	for key, contexts := range wantContexts {
		byContext, ok := guardrails[key].([]any)
		if !ok || len(byContext) == 0 {
			t.Fatalf("%s is not a list of statement groups: %v", key, guardrails[key])
		}

		swept := map[string]bool{}
		for _, entry := range byContext {
			group := entry.(map[string]any)
			statements := group["statements"].([]any)
			first, isString := statements[0].(string)
			if !isString || !strings.HasPrefix(first, "delete_matching_keys(attributes, ") {
				t.Errorf("%s/%v does not clear the verdict namespace first; its first statement is %v", key, group["context"], statements[0])
				continue
			}
			swept[group["context"].(string)] = true

			// What the pattern MEANS, not how it is spelled: the dots are regex-escaped,
			// so a substring check would be asserting the escaping rather than the reach.
			pattern, err := strconv.Unquote(strings.TrimSuffix(strings.TrimPrefix(first, "delete_matching_keys(attributes, "), ")"))
			if err != nil {
				t.Fatalf("%s: the clearing statement's pattern is not a quoted string: %s", key, first)
			}
			cleared, err := regexp.Compile(pattern)
			if err != nil {
				t.Fatalf("%s: the clearing pattern %q is not a valid regexp: %v", key, pattern, err)
			}
			if !cleared.MatchString("otel.guardrail.blocking") || !cleared.MatchString("otel.guardrail.violation.S1") {
				t.Errorf("%s clears %q, which does not reach the Guardrail's own attributes", key, pattern)
			}
			// And nothing else. A pattern that swept the whole resource would delete the
			// very attributes the Standards are about, and every record would then
			// violate everything.
			for _, keep := range []string{"service.name", "service.version", "deployment.environment", "host.name"} {
				if cleared.MatchString(keep) {
					t.Errorf("%s clears %q, which also deletes the service's own %s", key, pattern, keep)
				}
			}
		}

		for _, context := range contexts {
			if !swept[context] {
				t.Errorf("%s does not clear the Guardrail namespace in the %q context, so a service can forge a verdict there", key, context)
			}
		}
	}

	// The verdict itself is written on the resource and nowhere else: one record,
	// one verdict, in the place an operator groups by service.
	for _, key := range []string{"trace_statements", "metric_statements", "log_statements"} {
		for _, entry := range guardrails[key].([]any) {
			group := entry.(map[string]any)
			for _, statement := range group["statements"].([]any) {
				if !strings.HasPrefix(statement.(string), "set(") {
					continue
				}
				if group["context"] != "resource" {
					t.Errorf("%s writes the verdict in the %q context: %s", key, group["context"], statement)
				}
			}
		}
	}
}

func TestAPipelineGuardrailRunsInTheGatewayAndNeverInAnAgent(t *testing.T) {
	// An explicit acceptance criterion of #14, and ADR 0007: Agents do no
	// enforcement. A per-service check would also be the wrong shape — a Standard
	// is org-wide, and enforcing it in a thousand Agents means a thousand places
	// for it to be out of date.
	agent, err := controlplane.Compile(tierOneService(), taxonomy(t), profiles(t))
	if err != nil {
		t.Fatalf("compile the Agent: %v", err)
	}

	if _, defined := agent.Processors["transform/guardrail"]; defined {
		t.Error("an Agent's compiled config runs a Pipeline Guardrail; enforcement is centralised in the Gateway")
	}
	rendered := renderedYAML(t, agent)
	if strings.Contains(rendered, "otel.guardrail") {
		t.Errorf("an Agent's compiled config mentions the Guardrail attributes:\n%s", rendered)
	}
}

func TestACatalogEnforcingNothingAtThePipelineCompilesNoProcessor(t *testing.T) {
	// The same rule the rest of this compiler follows: a setting that would do
	// nothing is not emitted. An empty transform processor would be a component
	// the config defines and no pipeline meaningfully uses, and it would read as a
	// Gateway that enforces something.
	config, err := controlplane.CompileGateway(gatewayDeclaration(t), profiles(t), standardsFrom(t, `apiVersion: guardrail.otel/v1
kind: StandardCatalog
standards:
  - standard: S2
    severity: block
    enforced_at: [preflight]
    requires:
      tier_mandatory_signals: true
`))
	if err != nil {
		t.Fatalf("compile the Gateway: %v", err)
	}

	if _, defined := config.Processors["transform/guardrail"]; defined {
		t.Error("the Gateway defines a Pipeline Guardrail processor although no Standard is enforced at the pipeline")
	}
	if err := config.Validate(); err != nil {
		t.Errorf("the compiled config is not coherent: %v", err)
	}
}

func TestAGatewayCannotBeCompiledWithoutAStandardCatalog(t *testing.T) {
	// A nil catalog has no pipeline-enforced Standards, which is indistinguishable
	// from a catalog that enforces none — so it would compile a Gateway running no
	// Guardrail at all, silently, from a caller that simply forgot an argument.
	// "No Standards" has to be something written down, not something omitted.
	_, err := controlplane.CompileGateway(gatewayDeclaration(t), profiles(t), nil)
	if err == nil {
		t.Fatal("the Gateway compiled with no Standard catalog; it would enforce nothing and say nothing")
	}
	if !strings.Contains(err.Error(), "Standard catalog") {
		t.Errorf("the error does not say what is missing: %v", err)
	}
}

func processorNames(config controlplane.CollectorConfig) []string {
	names := make([]string, 0, len(config.Processors))
	for name := range config.Processors {
		names = append(names, name)
	}
	return names
}

func contains(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

func renderedYAML(t *testing.T, config controlplane.CollectorConfig) string {
	t.Helper()

	rendered, err := config.YAML()
	if err != nil {
		t.Fatalf("render the compiled config: %v", err)
	}
	return string(rendered)
}

// standardsFrom is a Standard catalog written for one test.
func standardsFrom(t *testing.T, body string) *guardrail.StandardCatalog {
	t.Helper()

	path := filepath.Join(t.TempDir(), "standards.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write Standard catalog: %v", err)
	}
	catalog, err := guardrail.LoadStandards(path)
	if err != nil {
		t.Fatalf("load Standard catalog: %v", err)
	}
	return catalog
}

// orgStandards is the Standard catalog shipped in the binary.
func orgStandards(t *testing.T) *guardrail.StandardCatalog {
	t.Helper()

	catalog, err := guardrail.CentralStandards()
	if err != nil {
		t.Fatalf("load the org Standard catalog: %v", err)
	}
	return catalog
}
