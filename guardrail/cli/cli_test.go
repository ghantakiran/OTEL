package cli_test

import (
	"bytes"
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

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()

	var out, errOut bytes.Buffer
	code = cli.Run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}
