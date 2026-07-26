package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ghantakiran/OTEL/guardrail"
)

func TestTheReportNamesAWaiverExpiringWithinTheWindow(t *testing.T) {
	register := waiverRegister(t, `
  - service_name: legacy-inventory
    standard: S1
    reason: the sidecar rollout has not landed
    approved_by: obs-team
    expires: 2026-08-10
`)

	_, out, errOut := run(t, "waivers", "--register", register, "--as-of", "2026-07-25", "--within", "30")

	if !strings.Contains(out, "legacy-inventory") {
		t.Errorf("the report does not name the service whose Waiver is about to lapse:\n%s%s", out, errOut)
	}
}

func TestTheReportSeparatesAlreadyExpiredWaiversFromExpiringOnes(t *testing.T) {
	register := waiverRegister(t, `
  - service_name: soon-to-lapse
    standard: S1
    reason: the sidecar rollout has not landed
    approved_by: obs-team
    expires: 2026-08-10
  - service_name: already-lapsed
    standard: S2
    reason: the batch runner is still on the old config service
    approved_by: obs-team
    expires: 2026-01-15
`)

	_, out, errOut := run(t, "waivers", "--register", register, "--as-of", "2026-07-25", "--within", "30")

	// An expiring Waiver needs attention before a build breaks; an expired one
	// means a service is already blocked. They are different problems, so a
	// reader must not have to work out which is which from the dates.
	expired := sectionNaming(out, "already-lapsed")
	expiring := sectionNaming(out, "soon-to-lapse")
	if expired == "" || expiring == "" {
		t.Fatalf("the report does not name both Waivers:\n%s%s", out, errOut)
	}
	if expired == expiring {
		t.Errorf("an expired Waiver is reported under the same heading as an expiring one:\n%s", out)
	}
}

func TestTheReportLeavesOutAWaiverExpiringBeyondTheWindow(t *testing.T) {
	register := waiverRegister(t, `
  - service_name: comfortably-covered
    standard: S1
    reason: the sidecar rollout has not landed
    approved_by: obs-team
    expires: 2027-04-01
`)

	_, out, errOut := run(t, "waivers", "--register", register, "--as-of", "2026-07-25", "--within", "30")

	if strings.Contains(out, "comfortably-covered") {
		t.Errorf("a Waiver eight months out is reported as needing attention now:\n%s%s", out, errOut)
	}
}

func TestTheReportExitsOneWhenAWaiverNeedsAttention(t *testing.T) {
	register := waiverRegister(t, `
  - service_name: soon-to-lapse
    standard: S1
    reason: the sidecar rollout has not landed
    approved_by: obs-team
    expires: 2026-08-10
`)

	// The scheduled job branches on this code alone: 1 means "somebody must act",
	// which is what tells the workflow to open or update the tracking issue.
	code, out, errOut := run(t, "waivers", "--register", register, "--as-of", "2026-07-25", "--within", "30")

	if code != 1 {
		t.Fatalf("exit code = %d, want 1 when a Waiver needs attention\n%s%s", code, out, errOut)
	}
}

func TestTheReportSaysSoWhenNoWaiverNeedsAttention(t *testing.T) {
	register := waiverRegister(t, `
  - service_name: comfortably-covered
    standard: S1
    reason: the sidecar rollout has not landed
    approved_by: obs-team
    expires: 2027-04-01
`)

	code, out, errOut := run(t, "waivers", "--register", register, "--as-of", "2026-07-25", "--within", "30")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 when nothing needs attention\n%s%s", code, out, errOut)
	}
	// The scheduled job closes its tracking issue on this path, and a human
	// running the report by hand should not be left staring at silence.
	if strings.TrimSpace(out) == "" {
		t.Errorf("the report says nothing at all when nothing needs attention")
	}
}

func TestEachReportedWaiverNamesItsStandardApproverExpiryAndDaysAway(t *testing.T) {
	register := waiverRegister(t, `
  - service_name: soon-to-lapse
    standard: S1
    reason: the sidecar rollout has not landed
    approved_by: obs-team
    expires: 2026-08-10
`)

	_, out, errOut := run(t, "waivers", "--register", register, "--as-of", "2026-07-25", "--within", "30")

	// Whoever reads this — in a terminal or in the issue the cron files — must be
	// able to act without opening the register.
	line := reportLineNaming(out, "soon-to-lapse")
	if line == "" {
		t.Fatalf("the report does not name the service:\n%s%s", out, errOut)
	}
	for _, want := range []string{"S1", "obs-team", "2026-08-10", "16"} {
		if !strings.Contains(line, want) {
			t.Errorf("the reported line does not carry %q:\n%s", want, line)
		}
	}
}

func TestTheReportTreatsTheExpiryDayItselfAsExpiringNotExpired(t *testing.T) {
	register := waiverRegister(t, `
  - service_name: lapses-tonight
    standard: S1
    reason: the sidecar rollout has not landed
    approved_by: obs-team
    expires: 2026-07-25
`)

	// A Waiver holds for all of the day it names, so on that day the service is
	// not blocked yet — it is the last day to act, not a fire already burning.
	_, out, errOut := run(t, "waivers", "--register", register, "--as-of", "2026-07-25", "--within", "30")

	heading := sectionNaming(out, "lapses-tonight")
	if heading == "" {
		t.Fatalf("the report does not name the Waiver expiring today:\n%s%s", out, errOut)
	}
	if strings.Contains(strings.ToLower(heading), "expired") {
		t.Errorf("a Waiver still in force for the rest of today is reported as expired: %q", heading)
	}
}

func TestTheReportLeadsWithTheMostUrgentWaiver(t *testing.T) {
	// Written into the register in the least urgent order, to prove the report
	// does not simply echo the file.
	register := waiverRegister(t, `
  - service_name: lapses-last
    standard: S1
    reason: the sidecar rollout has not landed
    approved_by: obs-team
    expires: 2026-08-20
  - service_name: lapses-first
    standard: S2
    reason: the sidecar rollout has not landed
    approved_by: obs-team
    expires: 2026-07-30
`)

	_, out, errOut := run(t, "waivers", "--register", register, "--as-of", "2026-07-25", "--within", "30")

	if strings.Index(out, "lapses-first") > strings.Index(out, "lapses-last") {
		t.Errorf("the report buries the Waiver that lapses soonest below one with weeks to run:\n%s%s", out, errOut)
	}
}

func TestTheReportCouldNotRunWhenTheRegisterIsIncomplete(t *testing.T) {
	register := waiverRegister(t, `
  - service_name: legacy-inventory
    standard: S1
    approved_by: obs-team
`)

	// Exit 1 would tell the scheduled job to file an issue saying which Waivers
	// lapse — but nothing was scanned, so that issue would be a lie. A register
	// that cannot be read is a tooling failure and has to land on exit 2.
	code, out, errOut := run(t, "waivers", "--register", register, "--as-of", "2026-07-25")

	if code != 2 {
		t.Fatalf("exit code = %d, want 2: an unreadable register is not a report of zero expiring Waivers\n%s%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "legacy-inventory") {
		t.Errorf("stderr does not name the Waiver at fault:\n%s", errOut)
	}
}

func TestTheReportCouldNotRunWhenTheWindowIsNegative(t *testing.T) {
	register := waiverRegister(t, `
  - service_name: soon-to-lapse
    standard: S1
    reason: the sidecar rollout has not landed
    approved_by: obs-team
    expires: 2026-08-10
`)

	// A negative window quietly reports nothing as expiring, which reads exactly
	// like an all-clear — the failure mode this whole job exists to prevent.
	code, out, errOut := run(t, "waivers", "--register", register, "--as-of", "2026-07-25", "--within", "-1")

	if code != 2 {
		t.Fatalf("exit code = %d, want 2: a negative window is a mistake, not an all-clear\n%s%s", code, out, errOut)
	}
}

func TestTheReportScansTheOrgRegisterWhenNoneIsNamed(t *testing.T) {
	// The scheduled job runs the command with no --register, so the register
	// embedded in the binary is the one that actually gets scanned. What is
	// asserted is that wiring, not the register's contents: ADR 0004 makes that
	// file the source of truth, so filing or retiring a real Waiver must not
	// break this test. A window wide enough to catch everything means the report
	// covers exactly what the register holds — including nothing, when it is empty.
	org, err := guardrail.CentralWaiverRegister()
	if err != nil {
		t.Fatalf("the register shipped in the binary does not load: %v", err)
	}

	_, out, errOut := run(t, "waivers", "--as-of", "2026-07-25", "--within", "100000", "--format", "json")

	var report struct {
		Expired []struct {
			ServiceName string `json:"service_name"`
		} `json:"expired"`
		Expiring []struct {
			ServiceName string `json:"service_name"`
		} `json:"expiring"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("the report is not the JSON the scheduled job parses: %v\n%s%s", err, out, errOut)
	}
	if got, want := len(report.Expired)+len(report.Expiring), len(org.Waivers()); got != want {
		t.Errorf("the report covers %d Waiver(s), but the org register holds %d", got, want)
	}
}

func TestTheReportSeparatesExpiredFromExpiringInTheDemonstrationRegister(t *testing.T) {
	// The honoured/lapsed split, asserted against the register that exists to
	// demonstrate it rather than against the org's live one.
	code, out, errOut := run(t, "waivers", "--register", demoWaivers, "--as-of", "2026-07-25", "--within", "365")

	if code != 1 {
		t.Fatalf("exit code = %d, want 1: the demonstration register holds a lapsed Waiver\n%s%s", code, out, errOut)
	}
	if sectionNaming(out, "legacy-payments-batch") == sectionNaming(out, "legacy-inventory") {
		t.Errorf("lapsed and still-in-force Waivers land under one heading:\n%s", out)
	}
}

func TestTheJSONReportKeepsExpiredAndExpiringInSeparateLists(t *testing.T) {
	register := waiverRegister(t, `
  - service_name: soon-to-lapse
    standard: S1
    reason: the sidecar rollout has not landed
    approved_by: obs-team
    expires: 2026-08-10
  - service_name: already-lapsed
    standard: S2
    reason: the batch runner is still on the old config service
    approved_by: payments-leads
    expires: 2026-01-15
`)

	// The scheduled job builds an issue body and a list of approvers to notify
	// out of this. Grepping prose for service names would break the moment the
	// wording changed, so the structure is the contract.
	_, out, errOut := run(t, "waivers", "--register", register, "--as-of", "2026-07-25", "--within", "30", "--format", "json")

	var report struct {
		AsOf       string `json:"as_of"`
		WithinDays int    `json:"within_days"`
		Expired    []struct {
			ServiceName     string `json:"service_name"`
			Standard        string `json:"standard"`
			Reason          string `json:"reason"`
			ApprovedBy      string `json:"approved_by"`
			Expires         string `json:"expires"`
			DaysUntilExpiry int    `json:"days_until_expiry"`
		} `json:"expired"`
		Expiring []struct {
			ServiceName     string `json:"service_name"`
			ApprovedBy      string `json:"approved_by"`
			DaysUntilExpiry int    `json:"days_until_expiry"`
		} `json:"expiring"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("the report is not JSON: %v\n%s%s", err, out, errOut)
	}

	if report.AsOf != "2026-07-25" || report.WithinDays != 30 {
		t.Errorf("the report does not carry the day and window it was run for: %+v", report)
	}
	if len(report.Expired) != 1 || len(report.Expiring) != 1 {
		t.Fatalf("want one expired and one expiring Waiver, got %d and %d", len(report.Expired), len(report.Expiring))
	}
	lapsed := report.Expired[0]
	if lapsed.ServiceName != "already-lapsed" || lapsed.Standard != "S2" || lapsed.ApprovedBy != "payments-leads" {
		t.Errorf("the expired Waiver is not described well enough to act on: %+v", lapsed)
	}
	if lapsed.Expires != "2026-01-15" || lapsed.DaysUntilExpiry >= 0 {
		t.Errorf("the expired Waiver's expiry reads as still in force: %+v", lapsed)
	}
	if lapsed.Reason == "" {
		t.Errorf("the expired Waiver carries no reason, so the issue cannot say why it was filed: %+v", lapsed)
	}
	if soon := report.Expiring[0]; soon.ServiceName != "soon-to-lapse" || soon.DaysUntilExpiry != 16 {
		t.Errorf("the expiring Waiver is misreported: %+v", soon)
	}
}

func TestTheJSONReportIsStillJSONWhenNothingNeedsAttention(t *testing.T) {
	register := waiverRegister(t, `
  - service_name: comfortably-covered
    standard: S1
    reason: the sidecar rollout has not landed
    approved_by: obs-team
    expires: 2027-04-01
`)

	// The all-clear path is the one that closes the tracking issue, and the job
	// parses the same document there as on every other path.
	code, out, errOut := run(t, "waivers", "--register", register, "--as-of", "2026-07-25", "--within", "30", "--format", "json")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 when nothing needs attention\n%s%s", code, out, errOut)
	}
	var report struct {
		Expired  []map[string]any `json:"expired"`
		Expiring []map[string]any `json:"expiring"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("the all-clear report is not JSON: %v\n%s%s", err, out, errOut)
	}
	// Absent lists would make the workflow's jq emit "null" instead of an empty
	// list, and `null | length` is not 0.
	if report.Expired == nil || report.Expiring == nil {
		t.Errorf("the all-clear report omits the empty lists rather than sending them empty:\n%s", out)
	}
}

func TestTheReportCouldNotRunInAnUnknownFormat(t *testing.T) {
	register := waiverRegister(t, `
  - service_name: soon-to-lapse
    standard: S1
    reason: the sidecar rollout has not landed
    approved_by: obs-team
    expires: 2026-08-10
`)

	// Falling back to text would hand the workflow prose where it expects JSON,
	// and its jq would report zero Waivers needing attention.
	code, out, errOut := run(t, "waivers", "--register", register, "--as-of", "2026-07-25", "--format", "yaml")

	if code != 2 {
		t.Fatalf("exit code = %d, want 2 for a format the report cannot produce\n%s%s", code, out, errOut)
	}
}

// reportLineNaming is the indented report line for a service.
func reportLineNaming(out, serviceName string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "  ") && strings.Contains(line, serviceName) {
			return line
		}
	}
	return ""
}

// sectionNaming is the heading line above the report line naming the service.
func sectionNaming(out, serviceName string) string {
	heading := ""
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "  "):
			if strings.Contains(line, serviceName) {
				return heading
			}
		case strings.TrimSpace(line) != "":
			heading = line
		}
	}
	return ""
}

// waiverRegister writes a Waiver register whose entries are the given YAML list
// items, and returns its path.
func waiverRegister(t *testing.T, waivers string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "waivers.yaml")
	document := "apiVersion: guardrail.otel/v1\nkind: WaiverRegister\nwaivers:" + waivers
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatalf("write Waiver register: %v", err)
	}
	return path
}
