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

func TestCheckFailsWhenTheContractIsMissing(t *testing.T) {
	code, _, errOut := run(t, "check", "../examples/no-such-contract.yaml")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero for an unreadable Contract")
	}
	if !strings.Contains(errOut, "no-such-contract.yaml") {
		t.Errorf("stderr does not name the missing file:\n%s", errOut)
	}
}

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()

	var out, errOut bytes.Buffer
	code = cli.Run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}
