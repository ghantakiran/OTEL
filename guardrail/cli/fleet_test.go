package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompileFleetCompilesTheFleetAndExitsZero(t *testing.T) {
	root := sampleFleet(t, map[string]string{
		"checkout-api":  fleetContract("checkout-api", "tier-1"),
		"payments-edge": fleetContract("payments-edge", "tier-1"),
	})

	code, out, errOut := run(t, "compile-fleet", root)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 for a Fleet that fully compiles\n%s%s", code, out, errOut)
	}
	for _, file := range []string{"compiled/checkout-api.yaml", "compiled/payments-edge.yaml", "rollout-manifest.yaml"} {
		if _, err := os.Stat(filepath.Join(root, file)); err != nil {
			t.Errorf("%s was not written: %v", file, err)
		}
		if !strings.Contains(out, file) {
			t.Errorf("the report does not mention %s:\n%s", file, out)
		}
	}
}

func TestCompileFleetExitsOneWhenAContractDoesNotCompileAndStillRollsOutTheRest(t *testing.T) {
	root := sampleFleet(t, map[string]string{
		"checkout-api":  fleetContract("checkout-api", "tier-1"),
		"reporting-api": fleetContract("reporting-api", "not-a-tier"),
	})

	code, out, errOut := run(t, "compile-fleet", root)

	// Exit 1, not 2: a Contract that will not compile is a finding about that
	// Contract, the same split `check` and `compile` make.
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 when a Contract does not compile\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, "contracts/reporting-api.yaml") || !strings.Contains(out, "not-a-tier") {
		t.Errorf("the report does not say which Contract did not compile, or why:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(root, "compiled", "checkout-api.yaml")); err != nil {
		t.Errorf("checkout-api was not rolled out alongside a failing Contract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "compiled", "reporting-api.yaml")); !os.IsNotExist(err) {
		t.Errorf("a Contract that did not compile produced a collector configuration anyway (%v)", err)
	}
}

func TestCompileFleetFailsAsAToolWhenThereIsNoFleetToCompile(t *testing.T) {
	// Exit 2, and nothing written. A mistyped path that read as an empty Fleet would
	// otherwise prune every collector configuration in the repo and report success.
	code, out, errOut := run(t, "compile-fleet", filepath.Join(t.TempDir(), "nowhere"))

	if code != 2 {
		t.Fatalf("exit code = %d, want 2 — a missing Fleet is not a finding about a service\n%s%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "no Telemetry Contracts") {
		t.Errorf("stderr does not say the Fleet is empty:\n%s", errOut)
	}
}

func TestCompileFleetReportsTheRolloutAsJSONForTheScheduledJob(t *testing.T) {
	// The scheduled rollout job builds its pull-request body from this, not from the
	// prose above, so rewording the terminal output never rewrites an open PR.
	root := sampleFleet(t, map[string]string{
		"checkout-api":  fleetContract("checkout-api", "tier-1"),
		"reporting-api": fleetContract("reporting-api", "not-a-tier"),
	})

	code, out, errOut := run(t, "compile-fleet", "--format", "json", root)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1\n%s%s", code, out, errOut)
	}

	report := decodeJSON(t, out)

	compiled, ok := report["compiled"].([]any)
	if !ok || len(compiled) != 1 {
		t.Fatalf("the JSON report does not list the one compiled service:\n%s", out)
	}
	first, _ := compiled[0].(map[string]any)
	for key, want := range map[string]string{
		"service_name":     "checkout-api",
		"tier":             "tier-1",
		"collector_config": "compiled/checkout-api.yaml",
	} {
		if first[key] != want {
			t.Errorf("compiled[0].%s = %v, want %q", key, first[key], want)
		}
	}

	failed, ok := report["not_compiled"].([]any)
	if !ok || len(failed) != 1 {
		t.Fatalf("the JSON report does not list the service that did not compile:\n%s", out)
	}
	entry, _ := failed[0].(map[string]any)
	if entry["telemetry_contract"] != "contracts/reporting-api.yaml" {
		t.Errorf("not_compiled[0].telemetry_contract = %v", entry["telemetry_contract"])
	}
	if reason, _ := entry["reason"].(string); !strings.Contains(reason, "not-a-tier") {
		t.Errorf("not_compiled[0].reason does not say why: %v", entry["reason"])
	}

	// The lists the job counts must exist even when empty: `null | length` is not 0
	// to jq, and a report that omits a key reads as a report that never looked.
	for _, key := range []string{"compiled", "not_compiled", "written", "unchanged", "pruned"} {
		if _, present := report[key]; !present {
			t.Errorf("the JSON report has no %q key:\n%s", key, out)
		}
	}
}

func TestCompileFleetRefusesAFormatNobodyCanRead(t *testing.T) {
	root := sampleFleet(t, map[string]string{"checkout-api": fleetContract("checkout-api", "tier-1")})

	// Falling back to text would hand the scheduled job prose its parser reads as an
	// empty rollout — a silent all-clear, which is the one outcome this must not fake.
	code, _, errOut := run(t, "compile-fleet", "--format", "yaml", root)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2 for an unknown --format", code)
	}
	if !strings.Contains(errOut, "yaml") {
		t.Errorf("stderr does not name the format it refused:\n%s", errOut)
	}
}

// --- helpers -----------------------------------------------------------------

func sampleFleet(t *testing.T, contracts map[string]string) string {
	t.Helper()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "contracts"), 0o755); err != nil {
		t.Fatalf("make the fleet: %v", err)
	}
	for name, body := range contracts {
		if err := os.WriteFile(filepath.Join(root, "contracts", name+".yaml"), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return root
}

// fleetContract is a Telemetry Contract declaring every Signal, so the only thing
// that decides whether it compiles is whether its Service Tier has a Profile.
func fleetContract(service, tier string) string {
	return "apiVersion: guardrail.otel/v1\nkind: TelemetryContract\nservice_name: " + service +
		"\nowner: team-" + service + "\ntier: " + tier +
		"\nsignals:\n  - traces\n  - metrics\n  - logs\nresource_attributes:\n  service.name: " + service +
		"\n  service.version: \"1.0.0\"\n  deployment.environment: production\n"
}

func decodeJSON(t *testing.T, out string) map[string]any {
	t.Helper()

	var decoded map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("the --format json output is not JSON: %v\n%s", err, out)
	}
	return decoded
}
