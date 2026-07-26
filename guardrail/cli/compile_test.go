package cli_test

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCompileWritesCollectorConfigForACompilableContract(t *testing.T) {
	code, out, errOut := run(t, "compile", "../examples/compliant-contract.yaml")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s%s", code, out, errOut)
	}

	// The point of the command is a file a collector can load, so what it writes
	// has to parse as collector configuration — not merely look plausible.
	var config struct {
		Receivers  map[string]any `yaml:"receivers"`
		Processors map[string]any `yaml:"processors"`
		Exporters  map[string]any `yaml:"exporters"`
		Service    struct {
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
		t.Fatalf("the compiled config runs no pipelines:\n%s", out)
	}
	for signal, pipeline := range config.Service.Pipelines {
		if len(pipeline.Receivers) == 0 || len(pipeline.Exporters) == 0 {
			t.Errorf("the %s pipeline is not wired end to end: %+v", signal, pipeline)
		}
	}
}

func TestCompileMarksItsOutputAsGenerated(t *testing.T) {
	// It lands in a repo via GitOps (ADR 0006), so a reader who finds it needs to
	// know not to edit it and what to edit instead.
	_, out, _ := run(t, "compile", "../examples/compliant-contract.yaml")

	if !strings.Contains(out, "do not edit") {
		t.Errorf("the compiled config does not say it is generated:\n%s", out)
	}
}

func TestCompileWorksForEveryServiceTier(t *testing.T) {
	// Every tier the org ships has a default Pipeline Profile (C2), so `compile`
	// behaves uniformly across tiers rather than working only for tier-1.
	for _, sample := range []string{
		"../examples/compliant-contract.yaml",            // tier-1
		"../examples/tier-3-least-signals-contract.yaml", // tier-3
	} {
		code, out, errOut := run(t, "compile", sample)

		if code != 0 {
			t.Errorf("%s did not compile: exit %d\n%s%s", sample, code, out, errOut)
		}
		if !strings.Contains(out, "service:") || !strings.Contains(out, "pipelines:") {
			t.Errorf("%s produced no collector pipelines:\n%s", sample, out)
		}
	}
}

func TestCompileBlamesTheContractWhenItCannotBeCompiled(t *testing.T) {
	// A tier outside the Service Tier Taxonomy: there is no telemetry floor to
	// compile against. Exit 1, not 2 — the split `check` established: 1 is a
	// finding about this input, 2 is the tool failing.
	code, out, errOut := run(t, "compile", "../examples/unknown-tier-contract.yaml")

	if code != 1 {
		t.Fatalf("exit code = %d, want 1 for a Contract that cannot be compiled\n%s%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "Taxonomy") {
		t.Errorf("stderr does not say why it could not be compiled:\n%s", errOut)
	}
}

func TestCompileFailsAsAToolWhenTheContractCannotBeRead(t *testing.T) {
	code, _, errOut := run(t, "compile", "../examples/no-such-contract.yaml")

	if code != 2 {
		t.Fatalf("exit code = %d, want 2 — an unreadable file is not a finding about a service", code)
	}
	if !strings.Contains(errOut, "no-such-contract.yaml") {
		t.Errorf("stderr does not name the missing file:\n%s", errOut)
	}
}

func TestCompileDoesNotRunGuardrails(t *testing.T) {
	// Compiling and checking answer different questions: what would ship, versus
	// whether it is allowed to. A Contract that violates a warn Standard still
	// compiles, and compiling must not report Standards at all.
	code, out, _ := run(t, "compile", "../examples/missing-recommended-attributes-contract.yaml")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0: a warn-severity violation does not stop a compile\n%s", code, out)
	}
	for _, standard := range []string{"S1", "S2", "S3"} {
		if strings.Contains(out, standard) {
			t.Errorf("the compiled config reports Standard %s; compiling is not checking:\n%s", standard, out)
		}
	}
}
