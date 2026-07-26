package contract_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ghantakiran/OTEL/contract"
)

func TestLoadRefusesADocumentThatIsNotATelemetryContract(t *testing.T) {
	// Every field of a WaiverRegister is absent from a Contract, so it decodes
	// cleanly into an empty one — and the Guardrail then reports a confident
	// verdict against a service whose name is the empty string. A wrong path or
	// a wrong `contract:` input in CI must be a Guardrail that could not run, not
	// a finding charged to a service team.
	_, err := contract.Load(filepath.Join("testdata", "not-a-contract.yaml"))

	if err == nil {
		t.Fatal("a WaiverRegister loaded as a Telemetry Contract")
	}
	if !strings.Contains(err.Error(), "WaiverRegister") || !strings.Contains(err.Error(), "TelemetryContract") {
		t.Errorf("the error does not say what was found and what was wanted: %v", err)
	}
}

func TestLoadRefusesAnApiVersionItDoesNotUnderstand(t *testing.T) {
	// apiVersion is what makes changing this schema survivable: a future version
	// may move or reinterpret a field. A binary that accepts a version it predates
	// reads the new file with the old rules and reports confident nonsense, so the
	// one thing the field must never be is decoded and ignored.
	unsupported := filepath.Join(t.TempDir(), "telemetry-contract.yaml")
	body := "apiVersion: guardrail.otel/v99\nkind: TelemetryContract\nservice_name: from-the-future\n"
	if err := os.WriteFile(unsupported, []byte(body), 0o600); err != nil {
		t.Fatalf("write Contract: %v", err)
	}

	_, err := contract.Load(unsupported)

	if err == nil {
		t.Fatal("a Telemetry Contract declaring an unsupported apiVersion loaded")
	}
	if !strings.Contains(err.Error(), "guardrail.otel/v99") || !strings.Contains(err.Error(), contract.APIVersion) {
		t.Errorf("the error does not say what was found and what this binary supports: %v", err)
	}
}

func TestLoadRefusesADocumentDeclaringNoApiVersion(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "telemetry-contract.yaml")
	body := "kind: TelemetryContract\nservice_name: unversioned\n"
	if err := os.WriteFile(missing, []byte(body), 0o600); err != nil {
		t.Fatalf("write Contract: %v", err)
	}

	_, err := contract.Load(missing)

	if err == nil {
		t.Fatal("a Telemetry Contract declaring no apiVersion loaded")
	}
}

func TestLoadReadsDeclaredTelemetry(t *testing.T) {
	c, err := contract.Load(filepath.Join("testdata", "checkout-api.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got, want := c.ServiceName, "checkout-api"; got != want {
		t.Errorf("ServiceName = %q, want %q", got, want)
	}
	if got, want := c.Owner, "team-payments"; got != want {
		t.Errorf("Owner = %q, want %q", got, want)
	}
	if got, want := c.Tier, "tier-1"; got != want {
		t.Errorf("Tier = %q, want %q", got, want)
	}
	if got, want := len(c.Signals), 3; got != want {
		t.Errorf("len(Signals) = %d, want %d", got, want)
	}
	if got, want := c.ResourceAttributes["deployment.environment"], "production"; got != want {
		t.Errorf("ResourceAttributes[deployment.environment] = %q, want %q", got, want)
	}
}
