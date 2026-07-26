package contract_test

import (
	"path/filepath"
	"testing"

	"github.com/ghantakiran/OTEL/contract"
)

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
