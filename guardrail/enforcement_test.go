package guardrail_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ghantakiran/OTEL/contract"
	"github.com/ghantakiran/OTEL/guardrail"
)

func TestTheEnforcementScheduleLoadsThePublishedEpochAndGraduationDeadlines(t *testing.T) {
	schedule := scheduleFrom(t, `apiVersion: guardrail.otel/v1
kind: EnforcementSchedule
epoch: 2026-01-01
graduations:
  - standard: S1
    graduates: 2027-01-01
`)

	if got := schedule.Epoch().String(); got != "2026-01-01" {
		t.Errorf("Enforcement Epoch is %q, want 2026-01-01", got)
	}
	graduates, published := schedule.Graduation("S1")
	if !published {
		t.Fatal("S1 has no published graduation deadline")
	}
	if got := graduates.String(); got != "2027-01-01" {
		t.Errorf("S1 graduates on %q, want 2027-01-01", got)
	}
}

func TestAScheduleDeclaringAnApiVersionThisBinaryDoesNotUnderstandIsRefused(t *testing.T) {
	// The schedule decides which services block today. Reading one written for a
	// later schema under this binary's rules is the worst kind of confident wrong.
	_, err := guardrail.LoadEnforcementSchedule(writeSchedule(t, `apiVersion: guardrail.otel/v99
kind: EnforcementSchedule
epoch: 2026-01-01
graduations:
  - standard: S1
    graduates: 2027-01-01
`))

	if err == nil {
		t.Fatal("an enforcement schedule declaring an unsupported apiVersion loaded")
	}
	if !strings.Contains(err.Error(), "guardrail.otel/v99") {
		t.Errorf("the error does not name the version found: %v", err)
	}
}

func TestAScheduleThatPublishesNoEpochIsRefused(t *testing.T) {
	// The worst silent failure in the system. An absent or mistyped `epoch:` leaves
	// a zero Date, every service then dates as on-or-after it, and every blocking
	// Standard blocks the entire fleet at once — the political death ADR 0003
	// exists to prevent, from one typo, with no error. The Waiver register refuses
	// a Waiver with no expiry for exactly this reason; the schedule must match it.
	_, err := guardrail.LoadEnforcementSchedule(writeSchedule(t, `apiVersion: guardrail.otel/v1
kind: EnforcementSchedule
epock: 2026-01-01
graduations:
  - standard: S1
    graduates: 2027-01-01
`))

	if err == nil {
		t.Fatal("a schedule with no Enforcement Epoch loaded: every service would classify as new")
	}
	if !strings.Contains(err.Error(), "epoch") {
		t.Errorf("the error does not name the missing field: %v", err)
	}
}

func TestAGraduationDeadlineThatIsNotADateIsRefused(t *testing.T) {
	// A mistyped `graduates:` key leaves a zero Date, which reads as a deadline
	// that has already arrived — so the Standard is published *and* blocks legacy
	// services immediately. Its neighbour, a mistyped `standard:` key, fails loudly.
	// One typo cannot fail loud while the one beside it fails silent.
	_, err := guardrail.LoadEnforcementSchedule(writeSchedule(t, `apiVersion: guardrail.otel/v1
kind: EnforcementSchedule
epoch: 2026-01-01
graduations:
  - standard: S1
    gradutes: 2027-01-01
`))

	if err == nil {
		t.Fatal("a graduation with no deadline loaded: the Standard would block legacy services at once")
	}
	if !strings.Contains(err.Error(), "S1") {
		t.Errorf("the error does not name the Standard at fault: %v", err)
	}
}

func TestTheShippedEnforcementScheduleIsUsable(t *testing.T) {
	// The schedule this binary actually ships. Nothing else in the suite loads it,
	// so a typo committed to guardrail/enforcement.yaml would reach the fleet.
	schedule, err := guardrail.CentralEnforcementSchedule()
	if err != nil {
		t.Fatalf("the shipped enforcement schedule does not load: %v", err)
	}

	if schedule.Epoch().IsZero() {
		t.Error("the shipped schedule publishes no Enforcement Epoch, so every service classifies as new")
	}
	// Every Standard that can block needs a published deadline, or a legacy
	// service violating it can be neither blocked nor deferred (ADR 0003).
	for _, standard := range []string{"S1", "S2"} {
		graduates, published := schedule.Graduation(standard)
		if !published {
			t.Errorf("blocking Standard %s has no published graduation deadline", standard)
			continue
		}
		if graduates.IsZero() {
			t.Errorf("blocking Standard %s graduates on the zero date, so it blocks legacy services at once", standard)
		}
	}
}

func TestAServiceWhoseContractFirstAppearedBeforeTheEpochIsLegacy(t *testing.T) {
	schedule := scheduleFrom(t, epochOf("2026-01-01"))

	if got := schedule.Classify(on(t, "2025-12-31")); got != guardrail.ServiceLegacy {
		t.Errorf("a Contract first committed the day before the Epoch classifies as %q, want %q",
			got, guardrail.ServiceLegacy)
	}
}

func TestAServiceWhoseContractFirstAppearedOnTheEpochIsNew(t *testing.T) {
	schedule := scheduleFrom(t, epochOf("2026-01-01"))

	// The Epoch is inclusive: a service arriving on the published day is new, so
	// there is no day on which a service is neither.
	if got := schedule.Classify(on(t, "2026-01-01")); got != guardrail.ServiceNew {
		t.Errorf("a Contract first committed on the Epoch itself classifies as %q, want %q",
			got, guardrail.ServiceNew)
	}
}

func TestALegacyServiceIsNotFailedByABlockingStandardBeforeItsGraduationDeadline(t *testing.T) {
	result := checkEpoch(t, epochOf("2026-01-01"), firstAppeared(t, "2025-06-11"), on(t, "2026-08-01"))

	if result.FailsTheBuild() {
		t.Fatalf("a legacy service was blocked before the Standard's graduation deadline: %+v", result.Violations)
	}
}

func TestALegacyServiceBlocksOnceTheStandardsGraduationDeadlinePasses(t *testing.T) {
	// Same schedule, same service, same Contract as the test above. Nothing was
	// edited and nobody revoked anything — the graduation deadline simply arrived.
	result := checkEpoch(t, epochOf("2026-01-01"), firstAppeared(t, "2025-06-11"), on(t, "2027-01-01"))

	if !result.FailsTheBuild() {
		t.Fatalf("a legacy service was still deferred on the graduation deadline: %+v", result.Violations)
	}
}

func TestANewServiceIsBlockedByAStandardThatLegacyServicesAreStillDeferredFrom(t *testing.T) {
	result := checkEpoch(t, epochOf("2026-01-01"), firstAppeared(t, "2026-06-11"), on(t, "2026-08-01"))

	if !result.FailsTheBuild() {
		t.Fatalf("a new service was let through by a blocking Standard: %+v", result.Violations)
	}
}

func TestADeferredViolationIsStillReportedWithTheDayItStartsBlocking(t *testing.T) {
	result := checkEpoch(t, epochOf("2026-01-01"), firstAppeared(t, "2025-06-11"), on(t, "2026-08-01"))

	deferred := result.Deferred()
	if len(deferred) != 1 {
		t.Fatalf("want the violation reported while deferred, got %+v", result.Violations)
	}
	// A finding that quietly stops mattering is how a phased rollout becomes a
	// permanent exemption. The service must see the day it starts blocking.
	if got := deferred[0].String(); !strings.Contains(got, "2027-01-01") {
		t.Errorf("a deferred violation does not say when it starts blocking: %s", got)
	}
}

func TestTheGuardrailRefusesToJudgeALegacyServiceAgainstAStandardWithNoGraduationDeadline(t *testing.T) {
	// S2 blocks this Contract and the schedule publishes a deadline only for S1.
	// Deferring by default would make the rollout permanent for S2; blocking by
	// default would spring it on every legacy service at once. Neither is ours.
	schedule := `apiVersion: guardrail.otel/v1
kind: EnforcementSchedule
epoch: 2026-01-01
graduations:
  - standard: S1
    graduates: 2027-01-01
`
	_, err := checkEpochErr(t, schedule, firstAppeared(t, "2025-06-11"), on(t, "2026-08-01"), tierOneMissingSignals())
	if err == nil {
		t.Fatal("want an error naming the Standard with no published graduation deadline, got none")
	}
	if !strings.Contains(err.Error(), "S2") {
		t.Errorf("the error does not name the Standard at fault: %v", err)
	}
}

func TestAWaiverAndALegacyDeferralBothNameTheirOwnExpiry(t *testing.T) {
	// Both hold the same violation back and they lapse on different days. A
	// service told about only one of them is told the wrong date.
	preflight, err := guardrail.NewPreflight(guardrail.StandardPolicies(),
		guardrail.WithWaivers(registerFrom(t, oneWaiverExpiring("2026-12-01"))),
		guardrail.WithEnforcementSchedule(scheduleFrom(t, epochOf("2026-01-01"))),
		guardrail.WithFirstAppearance(firstAppeared(t, "2025-06-11")),
		guardrail.WithClock(func() time.Time { return on(t, "2026-08-01") }))
	if err != nil {
		t.Fatalf("new Preflight Guardrail: %v", err)
	}
	result, err := preflight.Check(context.Background(), waivedService())
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	if result.FailsTheBuild() {
		t.Fatalf("want the build to pass, got %+v", result.Blocking())
	}
	reported := result.Violations[0].String()
	for _, day := range []string{"2026-12-01", "2027-01-01"} {
		if !strings.Contains(reported, day) {
			t.Errorf("the report does not name %s, so it tells the service the wrong date: %s", day, reported)
		}
	}
}

func TestAContractsFirstAppearanceIsTheCommitThatAddedIt(t *testing.T) {
	repository := t.TempDir()
	contractPath := filepath.Join(repository, "telemetry-contract.yaml")

	git(t, repository, "init")
	git(t, repository, "config", "user.email", "guardrail@example.test")
	git(t, repository, "config", "user.name", "Guardrail")

	write(t, contractPath, "service_name: legacy-inventory\n")
	git(t, repository, "add", "telemetry-contract.yaml")
	git(t, repository, "commit", "-m", "declare a Telemetry Contract", "--date", "2025-06-11T09:00:00+00:00")

	// A later edit must not reset the clock: the Epoch asks when the service
	// first declared a Contract, not when it last touched one.
	write(t, contractPath, "service_name: legacy-inventory\nowner: team-supply\n")
	git(t, repository, "commit", "-am", "name an owner", "--date", "2026-05-02T09:00:00+00:00")

	appeared, err := guardrail.FirstAppearanceInGit(contractPath)()
	if err != nil {
		t.Fatalf("first appearance: %v", err)
	}
	if got := appeared.Format("2006-01-02"); got != "2025-06-11" {
		t.Errorf("the Contract first appeared on %q, want 2025-06-11", got)
	}
}

func TestDeletingAndRedeclaringAContractDoesNotResetTheClock(t *testing.T) {
	// Otherwise a legacy service buys itself new-service treatment — or escapes
	// a graduation deadline — with two commits and no conversation.
	repository := t.TempDir()
	contractPath := filepath.Join(repository, "telemetry-contract.yaml")

	git(t, repository, "init")
	git(t, repository, "config", "user.email", "guardrail@example.test")
	git(t, repository, "config", "user.name", "Guardrail")

	write(t, contractPath, "service_name: legacy-inventory\n")
	git(t, repository, "add", "telemetry-contract.yaml")
	git(t, repository, "commit", "-m", "declare a Telemetry Contract", "--date", "2025-06-11T09:00:00+00:00")

	git(t, repository, "rm", "-q", "telemetry-contract.yaml")
	git(t, repository, "commit", "-m", "remove the Contract", "--date", "2026-05-01T09:00:00+00:00")

	write(t, contractPath, "service_name: legacy-inventory\n")
	git(t, repository, "add", "telemetry-contract.yaml")
	git(t, repository, "commit", "-m", "declare it again", "--date", "2026-05-02T09:00:00+00:00")

	appeared, err := guardrail.FirstAppearanceInGit(contractPath)()
	if err != nil {
		t.Fatalf("first appearance: %v", err)
	}
	if got := appeared.Format("2006-01-02"); got != "2025-06-11" {
		t.Errorf("re-declaring the Contract moved its first appearance to %q, want 2025-06-11", got)
	}
}

func TestAShallowCloneCannotDateAContractAndSaysSo(t *testing.T) {
	// The scenario the Epoch's safety net was written for, and the one it missed:
	// a shallow clone grafts its tip as parentless, so git reports every file as
	// added in that tip commit. The lookup then *succeeds* and returns today —
	// classifying every legacy service as new and blocking the whole fleet, which
	// is the political death ADR 0003 exists to prevent. Absence of history has
	// to be detected, not inferred from a failed lookup.
	origin := t.TempDir()
	git(t, origin, "init")
	git(t, origin, "config", "user.email", "guardrail@example.test")
	git(t, origin, "config", "user.name", "Guardrail")

	write(t, filepath.Join(origin, "telemetry-contract.yaml"), "service_name: legacy-inventory\n")
	git(t, origin, "add", "telemetry-contract.yaml")
	git(t, origin, "commit", "-m", "declare a Telemetry Contract", "--date", "2025-06-11T09:00:00+00:00")

	write(t, filepath.Join(origin, "README.md"), "later\n")
	git(t, origin, "add", "README.md")
	git(t, origin, "commit", "-m", "something later", "--date", "2026-05-02T09:00:00+00:00")

	shallow := filepath.Join(t.TempDir(), "shallow")
	git(t, t.TempDir(), "clone", "--depth", "1", "file://"+origin, shallow)

	_, err := guardrail.FirstAppearanceInGit(filepath.Join(shallow, "telemetry-contract.yaml"))()
	if err == nil {
		t.Fatal("a shallow clone dated the Contract instead of refusing: every legacy service would be classified new")
	}
	if !strings.Contains(err.Error(), "fetch-depth") {
		t.Errorf("the error does not say how to fix it: %v", err)
	}
}

func TestTheGuardrailSaysHowToFixAContractItCannotDate(t *testing.T) {
	// An uncommitted Contract, or a shallow clone, has no such history. Guessing
	// "new" would fail every legacy service the moment someone shallow-clones;
	// guessing "legacy" would hand every service a way out.
	repository := t.TempDir()
	contractPath := filepath.Join(repository, "telemetry-contract.yaml")
	git(t, repository, "init")
	write(t, contractPath, "service_name: undeclared\n")

	_, err := guardrail.FirstAppearanceInGit(contractPath)()
	if err == nil {
		t.Fatal("want an error for a Contract with no commit adding it, got none")
	}
	if !strings.Contains(err.Error(), "fetch-depth") {
		t.Errorf("the error does not say how to fix it: %v", err)
	}
}

func git(t *testing.T, repository string, args ...string) {
	t.Helper()

	command := exec.Command("git", args...)
	command.Dir = repository
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// checkEpoch runs the shipped Standard catalog over a Telemetry Contract that
// violates S1 and nothing else, classifying the service by a first-appearance
// date the test supplies — no test reads this repository's own git history.
func checkEpoch(t *testing.T, schedule string, appeared guardrail.FirstAppearance, asOf time.Time) guardrail.Result {
	t.Helper()

	// waivedService violates S1 and nothing else, so one Standard decides the
	// build; no Waiver register is given here, so only the Epoch is at work.
	result, err := checkEpochErr(t, schedule, appeared, asOf, waivedService())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	return result
}

// checkEpochErr is checkEpoch for the cases where the error is the behavior.
func checkEpochErr(t *testing.T, schedule string, appeared guardrail.FirstAppearance, asOf time.Time, c contract.Contract) (guardrail.Result, error) {
	t.Helper()

	preflight, err := guardrail.NewPreflight(guardrail.StandardPolicies(),
		guardrail.WithEnforcementSchedule(scheduleFrom(t, schedule)),
		guardrail.WithFirstAppearance(appeared),
		guardrail.WithClock(func() time.Time { return asOf }))
	if err != nil {
		t.Fatalf("new Preflight Guardrail: %v", err)
	}
	return preflight.Check(context.Background(), c)
}

// tierOneMissingSignals violates S2 — a tier-1 service declaring only traces —
// and is otherwise complete.
func tierOneMissingSignals() contract.Contract {
	return contract.Contract{
		APIVersion:  "guardrail.otel/v1",
		Kind:        "TelemetryContract",
		ServiceName: "legacy-inventory",
		Owner:       "team-supply-chain",
		Tier:        "tier-1",
		Signals:     []string{"traces"},
		ResourceAttributes: map[string]string{
			"service.name":           "legacy-inventory",
			"service.version":        "1.0.0",
			"deployment.environment": "production",
			"service.namespace":      "supply-chain",
			"service.instance.id":    "legacy-inventory-0",
		},
	}
}

// firstAppeared supplies the day a Contract first appeared directly, standing in
// for the git lookup so no test depends on this repository's real history.
func firstAppeared(t *testing.T, date string) guardrail.FirstAppearance {
	t.Helper()

	day := on(t, date)
	return func() (time.Time, error) { return day, nil }
}

// epochOf is a schedule publishing one Epoch and a graduation deadline for S1.
func epochOf(epoch string) string {
	return `apiVersion: guardrail.otel/v1
kind: EnforcementSchedule
epoch: ` + epoch + `
graduations:
  - standard: S1
    graduates: 2027-01-01
`
}

// scheduleFrom writes an enforcement schedule to disk and loads it, so every
// test exercises the same path the platform team's checked-in file takes.
func scheduleFrom(t *testing.T, yaml string) *guardrail.EnforcementSchedule {
	t.Helper()

	schedule, err := guardrail.LoadEnforcementSchedule(writeSchedule(t, yaml))
	if err != nil {
		t.Fatalf("load enforcement schedule: %v", err)
	}
	return schedule
}

func writeSchedule(t *testing.T, yaml string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "enforcement.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write enforcement schedule: %v", err)
	}
	return path
}
