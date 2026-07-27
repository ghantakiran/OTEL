package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGatewayWritesCollectorConfigurationForTheSharedGateway(t *testing.T) {
	code, out, errOut := run(t, "gateway")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s%s", code, out, errOut)
	}

	// Same bar as `compile`: the point of the command is a file a collector can
	// load, so what it writes has to parse as collector configuration.
	var config struct {
		Receivers map[string]any `yaml:"receivers"`
		Exporters map[string]any `yaml:"exporters"`
		Service   struct {
			Pipelines map[string]struct {
				Receivers  []string `yaml:"receivers"`
				Processors []string `yaml:"processors"`
				Exporters  []string `yaml:"exporters"`
			} `yaml:"pipelines"`
		} `yaml:"service"`
	}
	if err := yaml.Unmarshal([]byte(out), &config); err != nil {
		t.Fatalf("the output is not loadable YAML: %v\n%s", err, out)
	}
	if len(config.Service.Pipelines) == 0 {
		t.Fatalf("the compiled Gateway runs no pipelines:\n%s", out)
	}
	for signal, pipeline := range config.Service.Pipelines {
		if len(pipeline.Receivers) == 0 || len(pipeline.Exporters) == 0 {
			t.Errorf("the %s pipeline is not wired end to end: %+v", signal, pipeline)
		}
	}
}

func TestGatewayBlamesTheDeclarationWhenItCannotBeCompiled(t *testing.T) {
	// The exit-code split `check` established, applied here: 1 is a finding about
	// this input, 2 is the tool failing. A Backend fanned out to on a Signal that
	// does not exist parses perfectly and still cannot be built — it would match no
	// pipeline and receive nothing — so it is a 1.
	notASignal := gatewayDeclarationFile(t, `apiVersion: guardrail.otel/v1
kind: GatewayDeclaration
gateway:
  address: otel-gateway.observability.svc.cluster.local:4317
  batch:
    timeout: 5s
    send_batch_size: 8192
  backends:
    - backend: primary-apm
      endpoint: apm-otlp.observability.svc.cluster.local:4317
    - backend: cold-archive
      endpoint: archive-otlp.observability.svc.cluster.local:4317
      signals: [metricks]
`)

	code, out, errOut := run(t, "gateway", "--declaration", notASignal)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1 for a Gateway that cannot be compiled\n%s%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "cold-archive") {
		t.Errorf("stderr does not say which Backend it could not handle:\n%s", errOut)
	}
}

func TestGatewayFailsAsAToolWhenTheDeclarationCannotBeRead(t *testing.T) {
	code, _, errOut := run(t, "gateway", "--declaration", "../examples/no-such-gateway.yaml")

	if code != 2 {
		t.Fatalf("exit code = %d, want 2 — an unreadable file is not a finding about the Gateway", code)
	}
	if !strings.Contains(errOut, "no-such-gateway.yaml") {
		t.Errorf("stderr does not name the missing file:\n%s", errOut)
	}
}

func TestGatewayIsDiscoverableFromTheTopLevelUsage(t *testing.T) {
	// A command nobody can find is a command nobody runs.
	_, _, errOut := run(t)

	if !strings.Contains(errOut, "gateway") {
		t.Errorf("the usage does not list the gateway command:\n%s", errOut)
	}
}

func TestGatewayMarksItsOutputAsGenerated(t *testing.T) {
	// It lands in a repo via GitOps (ADR 0006), so a reader who finds it needs to
	// know not to edit it and what to edit instead. For the Gateway that is the
	// Gateway Declaration, not a Contract — a different answer from `compile`'s.
	_, out, _ := run(t, "gateway")

	if !strings.Contains(out, "do not edit") {
		t.Errorf("the compiled Gateway config does not say it is generated:\n%s", out)
	}
	if !strings.Contains(out, "Gateway Declaration") {
		t.Errorf("the compiled Gateway config does not say what to edit instead:\n%s", out)
	}
}

func TestGatewayCompilesThePipelineEnforcedStandardsIntoTheConfig(t *testing.T) {
	// The Gateway is where Pipeline Guardrails run (#14). The catalog the org
	// enforces before deploy is the catalog compiled in here, so a Standard has
	// one definition and the same id at both points.
	_, out, _ := run(t, "gateway")

	if !strings.Contains(out, "transform/guardrail") {
		t.Errorf("the compiled Gateway runs no Pipeline Guardrail:\n%s", out)
	}
	if !strings.Contains(out, "otel.guardrail.violation.S1") {
		t.Errorf("the compiled Gateway does not enforce S1, which the catalog marks enforced_at: [preflight, pipeline]:\n%s", out)
	}
}

func TestGatewayFailsAsAToolWhenAStandardCannotBeEnforcedAtThePipeline(t *testing.T) {
	// A Standard claiming an enforcement point it cannot deliver is a broken
	// catalog — the platform team's problem, not a finding about the Gateway
	// Declaration in front of it. Exit 2, the same split an absent Severity makes.
	notExpressible := filepath.Join(t.TempDir(), "standards.yaml")
	if err := os.WriteFile(notExpressible, []byte(`apiVersion: guardrail.otel/v1
kind: StandardCatalog
standards:
  - standard: S2
    severity: block
    enforced_at: [pipeline]
    requires:
      tier_mandatory_signals: true
`), 0o600); err != nil {
		t.Fatalf("write the Standard catalog: %v", err)
	}

	code, _, errOut := run(t, "gateway", "--standards", notExpressible)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2 — a Standard that cannot be compiled is a broken catalog, not a finding about the Gateway", code)
	}
	if !strings.Contains(errOut, "S2") {
		t.Errorf("stderr does not name the Standard at fault:\n%s", errOut)
	}
}

func gatewayDeclarationFile(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the Gateway Declaration: %v", err)
	}
	return path
}
