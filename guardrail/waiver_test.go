package guardrail_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ghantakiran/OTEL/contract"
	"github.com/ghantakiran/OTEL/guardrail"
)

func TestTheWaiverRegisterLoadsATimeBoxedApprovedWaiver(t *testing.T) {
	register := registerFrom(t, `apiVersion: guardrail.otel/v1
kind: WaiverRegister
waivers:
  - service_name: legacy-inventory
    standard: S1
    reason: "Attribute is set by the platform sidecar, not the service."
    approved_by: obs-team
    expires: 2026-10-01
`)

	waivers := register.Waivers()

	if len(waivers) != 1 {
		t.Fatalf("register holds %d Waiver(s), want 1: %+v", len(waivers), waivers)
	}
	w := waivers[0]
	if w.ServiceName != "legacy-inventory" || w.Standard != "S1" {
		t.Errorf("Waiver is scoped to %q/%q, want legacy-inventory/S1", w.ServiceName, w.Standard)
	}
	if w.ApprovedBy != "obs-team" {
		t.Errorf("Waiver approved by %q, want obs-team", w.ApprovedBy)
	}
	if w.Reason == "" {
		t.Error("Waiver carries no reason")
	}
	if got := w.Expires.String(); got != "2026-10-01" {
		t.Errorf("Waiver expires %q, want 2026-10-01", got)
	}
}

func TestAWaiverIsInForceBeforeItsExpiryDate(t *testing.T) {
	register := registerFrom(t, oneWaiverExpiring("2026-10-01"))

	waiver, waived := register.InForce("legacy-inventory", "S1", on(t, "2026-08-01"))

	if !waived {
		t.Fatal("a Waiver two months from expiry is not in force")
	}
	if waiver.ApprovedBy != "obs-team" {
		t.Errorf("Waiver in force approved by %q, want obs-team", waiver.ApprovedBy)
	}
}

func TestAWaiverIsNoLongerInForceOnceItsExpiryHasPassed(t *testing.T) {
	register := registerFrom(t, oneWaiverExpiring("2026-10-01"))

	_, waived := register.InForce("legacy-inventory", "S1", on(t, "2026-10-02"))

	if waived {
		t.Fatal("a Waiver whose expiry has passed is still in force; enforcement must revert on its own")
	}
}

func TestAWaiverStillHoldsOnTheDayItExpires(t *testing.T) {
	register := registerFrom(t, oneWaiverExpiring("2026-10-01"))

	waiver, waived := register.InForce("legacy-inventory", "S1", on(t, "2026-10-01"))

	if !waived {
		t.Fatal("a Waiver lapsed on its own expiry date; it holds for the whole day it names")
	}
	if got := waiver.DaysUntilExpiry(on(t, "2026-10-01")); got != 0 {
		t.Errorf("DaysUntilExpiry on the expiry date = %d, want 0", got)
	}
}

func TestAWaiverReportsHowManyDaysRemainBeforeItExpires(t *testing.T) {
	register := registerFrom(t, oneWaiverExpiring("2026-10-01"))

	if got := register.Waivers()[0].DaysUntilExpiry(on(t, "2026-09-24")); got != 7 {
		t.Errorf("DaysUntilExpiry a week out = %d, want 7", got)
	}
}

func TestAWaiverDoesNotReachAnotherService(t *testing.T) {
	register := registerFrom(t, oneWaiverExpiring("2026-10-01"))

	if _, waived := register.InForce("checkout-api", "S1", on(t, "2026-08-01")); waived {
		t.Fatal("legacy-inventory's Waiver waived S1 for checkout-api")
	}
}

func TestAWaiverDoesNotReachAnotherStandardOfTheSameService(t *testing.T) {
	register := registerFrom(t, oneWaiverExpiring("2026-10-01"))

	if _, waived := register.InForce("legacy-inventory", "S2", on(t, "2026-08-01")); waived {
		t.Fatal("a Waiver for S1 also waived S2 for the same service")
	}
}

func TestTheWaiverRegisterRefusesAWaiverWithNoExpiry(t *testing.T) {
	_, err := guardrail.LoadWaiverRegister(writeRegister(t, `apiVersion: guardrail.otel/v1
kind: WaiverRegister
waivers:
  - service_name: legacy-inventory
    standard: S1
    reason: "Attribute is set by the platform sidecar, not the service."
    approved_by: obs-team
`))

	if err == nil {
		t.Fatal("an unbounded Waiver was accepted; a Waiver with no expiry is a permanent hole")
	}
	if !strings.Contains(err.Error(), "legacy-inventory") || !strings.Contains(err.Error(), "expires") {
		t.Errorf("error does not name the Waiver and the missing field: %v", err)
	}
}

func TestTheWaiverRegisterRefusesAWaiverWithNoReason(t *testing.T) {
	_, err := guardrail.LoadWaiverRegister(writeRegister(t, `apiVersion: guardrail.otel/v1
kind: WaiverRegister
waivers:
  - service_name: legacy-inventory
    standard: S1
    approved_by: obs-team
    expires: 2026-10-01
`))

	if err == nil {
		t.Fatal("an unexplained Waiver was accepted; nobody can review a Waiver with no reason")
	}
	if !strings.Contains(err.Error(), "legacy-inventory") || !strings.Contains(err.Error(), "reason") {
		t.Errorf("error does not name the Waiver and the missing field: %v", err)
	}
}

func TestTheWaiverRegisterRefusesAWaiverNobodyApproved(t *testing.T) {
	_, err := guardrail.LoadWaiverRegister(writeRegister(t, `apiVersion: guardrail.otel/v1
kind: WaiverRegister
waivers:
  - service_name: legacy-inventory
    standard: S1
    reason: "Attribute is set by the platform sidecar, not the service."
    expires: 2026-10-01
`))

	if err == nil {
		t.Fatal("an unapproved Waiver was accepted; a service must not be able to waive itself")
	}
	if !strings.Contains(err.Error(), "legacy-inventory") || !strings.Contains(err.Error(), "approved_by") {
		t.Errorf("error does not name the Waiver and the missing field: %v", err)
	}
}

func TestTheWaiverRegisterRefusesAWaiverThatNamesNoStandard(t *testing.T) {
	_, err := guardrail.LoadWaiverRegister(writeRegister(t, `apiVersion: guardrail.otel/v1
kind: WaiverRegister
waivers:
  - service_name: legacy-inventory
    reason: "Attribute is set by the platform sidecar, not the service."
    approved_by: obs-team
    expires: 2026-10-01
`))

	if err == nil {
		t.Fatal("a Waiver naming no Standard was accepted; a Waiver is scoped to exactly one Standard")
	}
	if !strings.Contains(err.Error(), "standard") {
		t.Errorf("error does not name the missing field: %v", err)
	}
}

func TestTheWaiverRegisterRefusesAFileThatIsNotAWaiverRegister(t *testing.T) {
	_, err := guardrail.LoadWaiverRegister(writeRegister(t, `apiVersion: guardrail.otel/v1
kind: TelemetryContract
service_name: legacy-inventory
tier: tier-3
`))

	if err == nil {
		t.Fatal("a Telemetry Contract loaded as a Waiver register; the wrong file must not silently waive nothing")
	}
	if !strings.Contains(err.Error(), "WaiverRegister") {
		t.Errorf("error does not say what kind of file was expected: %v", err)
	}
}

func TestAWaiverInForceStopsABlockingStandardFromFailingTheBuild(t *testing.T) {
	result := checkWaived(t, oneWaiverExpiring("2026-10-01"), on(t, "2026-08-01"))

	if result.FailsTheBuild() {
		t.Fatalf("a Waiver in force did not hold back the blocking Standard: %+v", result.Violations)
	}
}

func TestAWaivedViolationIsStillReportedWithItsExpiry(t *testing.T) {
	result := checkWaived(t, oneWaiverExpiring("2026-10-01"), on(t, "2026-08-01"))

	if !reports(result.Violations, "S1", "deployment.environment") {
		t.Fatalf("the waived S1 violation vanished from the report: %+v", result.Violations)
	}
	waived := result.Waived()
	if len(waived) != 1 {
		t.Fatalf("%d waived violation(s), want 1: %+v", len(waived), result.Violations)
	}
	if got := waived[0].Waived.Expires.String(); got != "2026-10-01" {
		t.Errorf("waived violation carries expiry %q, want 2026-10-01", got)
	}
	if !strings.Contains(waived[0].String(), "2026-10-01") {
		t.Errorf("the reported line hides the expiry date: %s", waived[0])
	}
}

func TestAnExpiredWaiverNoLongerHoldsBackTheStandard(t *testing.T) {
	register := oneWaiverExpiring("2026-10-01")

	held := checkWaived(t, register, on(t, "2026-09-30"))
	if held.FailsTheBuild() {
		t.Fatalf("the Waiver was not honoured the day before it expired: %+v", held.Violations)
	}

	// Nobody edits the register, nobody revokes anything: the day simply moves on.
	reverted := checkWaived(t, register, on(t, "2026-10-02"))

	if !reverted.FailsTheBuild() {
		t.Fatalf("an expired Waiver still held back a blocking Standard: %+v", reverted.Violations)
	}
	if len(reverted.Waived()) != 0 {
		t.Errorf("an expired Waiver is still attached to a violation: %+v", reverted.Waived())
	}
}

func TestTheCentralWaiverRegisterShippedWithTheCLIIsComplete(t *testing.T) {
	register, err := guardrail.CentralWaiverRegister()

	if err != nil {
		t.Fatalf("the register shipped in the binary does not load: %v", err)
	}
	if len(register.Waivers()) == 0 {
		t.Fatal("the shipped register holds no Waiver; the honoured and expired paths have no example")
	}
}

// checkWaived runs the shipped Standard catalog over a Telemetry Contract that
// violates S1 and nothing else, judging Waiver expiry on a fixed day.
func checkWaived(t *testing.T, register string, asOf time.Time) guardrail.Result {
	t.Helper()

	preflight, err := guardrail.NewPreflight(guardrail.StandardPolicies(),
		guardrail.WithWaivers(registerFrom(t, register)),
		guardrail.WithClock(func() time.Time { return asOf }))
	if err != nil {
		t.Fatalf("new Preflight Guardrail: %v", err)
	}
	result, err := preflight.Check(context.Background(), waivedService())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	return result
}

func TestAWaiverDoesNotAttachToAViolationThatWasNeverGoingToFailTheBuild(t *testing.T) {
	// A Waiver downgrades a block Standard to non-failing. A warn Standard is
	// already non-failing, so there is nothing for a Waiver to do — and pretending
	// otherwise makes the report name a Waiver that is holding nothing back,
	// pointing the reader at the wrong expiry date.
	register := `apiVersion: guardrail.otel/v1
kind: WaiverRegister
waivers:
  - service_name: warn-only-service
    standard: S3
    reason: "Filed against a Standard that only warns."
    approved_by: obs-team
    expires: 2027-06-01
`
	preflight, err := guardrail.NewPreflight(guardrail.StandardPolicies(),
		guardrail.WithWaivers(registerFrom(t, register)),
		guardrail.WithClock(func() time.Time { return on(t, "2026-08-01") }))
	if err != nil {
		t.Fatalf("new Preflight Guardrail: %v", err)
	}
	result, err := preflight.Check(context.Background(), warnOnlyService())
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	if len(result.Advisory()) == 0 {
		t.Fatal("fixture no longer violates a warn Standard, so this test proves nothing")
	}
	if waived := result.Waived(); len(waived) != 0 {
		t.Errorf("a Waiver attached to a violation that never fails the build: %+v", waived)
	}
}

// warnOnlyService violates S3 — and only S3 — so nothing here fails the build.
func warnOnlyService() contract.Contract {
	return contract.Contract{
		APIVersion:  "guardrail.otel/v1",
		Kind:        "TelemetryContract",
		ServiceName: "warn-only-service",
		Owner:       "team-supply-chain",
		Tier:        "tier-3",
		Signals:     []string{"traces"},
		ResourceAttributes: map[string]string{
			"service.name":           "warn-only-service",
			"service.version":        "1.4.2",
			"deployment.environment": "production",
		},
	}
}

// waivedService is a Telemetry Contract violating S1 — and only S1 — so a test
// can watch one Waiver decide whether the build fails.
func waivedService() contract.Contract {
	return contract.Contract{
		APIVersion:  "guardrail.otel/v1",
		Kind:        "TelemetryContract",
		ServiceName: "legacy-inventory",
		Owner:       "team-supply-chain",
		Tier:        "tier-3",
		Signals:     []string{"traces"},
		ResourceAttributes: map[string]string{
			"service.name":        "legacy-inventory",
			"service.version":     "1.4.2",
			"service.namespace":   "supply-chain",
			"service.instance.id": "legacy-inventory-6b2c1a",
		},
	}
}

// oneWaiverExpiring is a register holding a single legacy-inventory/S1 Waiver.
func oneWaiverExpiring(expiry string) string {
	return `apiVersion: guardrail.otel/v1
kind: WaiverRegister
waivers:
  - service_name: legacy-inventory
    standard: S1
    reason: "Attribute is set by the platform sidecar, not the service."
    approved_by: obs-team
    expires: ` + expiry + "\n"
}

// on is a fixed date to judge expiry against, so no test depends on the day it runs.
func on(t *testing.T, date string) time.Time {
	t.Helper()

	day, err := time.Parse("2006-01-02", date)
	if err != nil {
		t.Fatalf("bad test date %q: %v", date, err)
	}
	return day
}

// registerFrom writes a Waiver register to disk and loads it, so every test
// exercises the same path the platform team's checked-in register takes.
func registerFrom(t *testing.T, yaml string) *guardrail.WaiverRegister {
	t.Helper()

	register, err := guardrail.LoadWaiverRegister(writeRegister(t, yaml))
	if err != nil {
		t.Fatalf("load Waiver register: %v", err)
	}
	return register
}

func writeRegister(t *testing.T, yaml string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "waivers.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write Waiver register: %v", err)
	}
	return path
}
