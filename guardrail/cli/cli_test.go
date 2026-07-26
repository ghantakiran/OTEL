package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ghantakiran/OTEL/guardrail/cli"
)

func TestCheckLetsACompliantContractThrough(t *testing.T) {
	code, out, _ := run(t, "check", "../examples/compliant-contract.yaml")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (output: %s)", code, out)
	}
}

func TestCheckBlocksAContractThatViolatesAStandard(t *testing.T) {
	code, out, _ := run(t, "check", "../examples/missing-attributes-contract.yaml")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero for a violating Contract")
	}
	if !strings.Contains(out, "S1") || !strings.Contains(out, "deployment.environment") {
		t.Errorf("output does not name the Standard and the problem:\n%s", out)
	}
}

func TestCheckLetsAContractThroughThatOnlyViolatesANonBlockingStandard(t *testing.T) {
	code, out, _ := run(t, "check", "../examples/missing-recommended-attributes-contract.yaml")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (only warn Severity violations)\n%s", code, out)
	}
	if !strings.Contains(out, "S3") || !strings.Contains(out, "warn") {
		t.Errorf("output does not report the warn Severity violation:\n%s", out)
	}
}

func TestCheckDoesNotSummariseANonBlockingResultAsAFailure(t *testing.T) {
	_, out, _ := run(t, "check", "../examples/missing-recommended-attributes-contract.yaml")

	summary, _, _ := strings.Cut(out, "\n")
	if strings.Contains(summary, "violation") {
		t.Errorf("summary line reports violations though nothing blocks the build:\n%s", summary)
	}
	if !strings.Contains(summary, "pricing-api") {
		t.Errorf("summary line does not name the service:\n%s", summary)
	}
}

func TestCheckReportsTheSeverityAlongsideEachViolation(t *testing.T) {
	code, out, _ := run(t, "check", "../examples/missing-attributes-contract.yaml")

	if code != 1 {
		t.Fatalf("exit code = %d, want 1 for a blocking Standard violation\n%s", code, out)
	}
	if got := severityReportedFor(out, "S1"); got != "block" {
		t.Errorf("S1 reported with Severity %q, want block:\n%s", got, out)
	}
	if got := severityReportedFor(out, "S3"); got != "warn" {
		t.Errorf("S3 reported with Severity %q, want warn:\n%s", got, out)
	}
}

func TestCheckLetsThroughAContractWhoseOnlyBlockingStandardIsWaived(t *testing.T) {
	code, out, errOut := run(t, "check", "--as-of", "2026-08-01", "../examples/waived-contract.yaml")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 for a service whose blocking Standard is waived\n%s%s", code, out, errOut)
	}
}

func TestCheckReportsTheWaivedStandardAndTheDateItsWaiverExpires(t *testing.T) {
	_, out, _ := run(t, "check", "--as-of", "2026-08-01", "../examples/waived-contract.yaml")

	if !strings.Contains(out, "S1") || !strings.Contains(out, "deployment.environment") {
		t.Errorf("the waived violation vanished from the report:\n%s", out)
	}
	if !strings.Contains(out, "waived") || !strings.Contains(out, "2027-04-01") {
		t.Errorf("the report hides that a Waiver is holding S1 back, and until when:\n%s", out)
	}
	summary, _, _ := strings.Cut(out, "\n")
	if !strings.Contains(summary, "Waiver") {
		t.Errorf("the summary line does not say a Waiver is why nothing blocked:\n%s", summary)
	}
}

func TestCheckBlocksAgainOnceTheWaiverHasExpired(t *testing.T) {
	code, out, _ := run(t, "check", "--as-of", "2026-08-01", "../examples/expired-waiver-contract.yaml")

	if code != 1 {
		t.Fatalf("exit code = %d, want 1: an expired Waiver holds nothing back\n%s", code, out)
	}
	if strings.Contains(out, "waived") {
		t.Errorf("an expired Waiver is still reported as holding S1 back:\n%s", out)
	}
}

func TestCheckJudgesWaiverExpiryOnTheDayItIsAskedAbout(t *testing.T) {
	before, out, _ := run(t, "check", "--as-of", "2027-03-31", "../examples/waived-contract.yaml")
	if before != 0 {
		t.Fatalf("exit code = %d the day before the Waiver expires, want 0\n%s", before, out)
	}

	// Nobody edits the register and nobody revokes anything; the day moves on.
	after, out, _ := run(t, "check", "--as-of", "2027-04-02", "../examples/waived-contract.yaml")

	if after != 1 {
		t.Fatalf("exit code = %d the day after the Waiver expires, want 1\n%s", after, out)
	}
}

func TestCheckCouldNotRunWhenTheWaiverRegisterIsIncomplete(t *testing.T) {
	register := filepath.Join(t.TempDir(), "waivers.yaml")
	if err := os.WriteFile(register, []byte(`apiVersion: guardrail.otel/v1
kind: WaiverRegister
waivers:
  - service_name: legacy-inventory
    standard: S1
    approved_by: obs-team
`), 0o600); err != nil {
		t.Fatalf("write Waiver register: %v", err)
	}

	code, out, errOut := run(t, "check", "--waivers", register, "../examples/waived-contract.yaml")

	if code != 2 {
		t.Fatalf("exit code = %d, want 2: a broken register is the platform team's bug, not the service's\n%s%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "legacy-inventory") {
		t.Errorf("stderr does not name the Waiver at fault:\n%s", errOut)
	}
}

func TestCheckFailsWhenTheContractIsMissing(t *testing.T) {
	code, _, errOut := run(t, "check", "../examples/no-such-contract.yaml")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero for an unreadable Contract")
	}
	if !strings.Contains(errOut, "no-such-contract.yaml") {
		t.Errorf("stderr does not name the missing file:\n%s", errOut)
	}
}

// severityReportedFor is the Severity the output attached to a Standard's line.
func severityReportedFor(out, standard string) string {
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, standard+":") {
			continue
		}
		for _, severity := range []string{"block", "warn", "info"} {
			if strings.Contains(line, severity) {
				return severity
			}
		}
	}
	return ""
}

func TestCheckRefusesToJudgeAContractItCannotDate(t *testing.T) {
	// Committed nowhere, so the Enforcement Epoch cannot tell whether this
	// service is new or legacy. Guessing either way is worse than stopping — one
	// guess blocks every legacy service the moment someone shallow-clones, the
	// other hands every service a way out.
	uncommitted := filepath.Join(t.TempDir(), "telemetry-contract.yaml")
	if err := os.WriteFile(uncommitted, []byte("service_name: undated\ntier: tier-3\nsignals: [traces]\n"), 0o600); err != nil {
		t.Fatalf("write Contract: %v", err)
	}

	code, _, errOut := run(t, "check", uncommitted)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2 — this is a Guardrail that could not run, not a non-compliant Contract", code)
	}
	if !strings.Contains(errOut, "fetch-depth") {
		t.Errorf("stderr does not say how to fix it:\n%s", errOut)
	}
}

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()

	var out, errOut bytes.Buffer
	code = cli.Run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}
