// Package cli is the command surface of otel-guardrail. It owns argument
// parsing, result formatting and exit codes; the Guardrail itself lives in the
// guardrail package.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/ghantakiran/OTEL/contract"
	"github.com/ghantakiran/OTEL/guardrail"
)

// Exit codes. CI distinguishes "the Contract violates a Standard" from "the
// Guardrail could not run" — only the first is the service team's problem.
const (
	exitOK        = 0
	exitViolation = 1
	exitError     = 2
)

// Run executes otel-guardrail and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return exitError
	}

	switch args[0] {
	case "check":
		return runCheck(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s\n", args[0], usage)
		return exitError
	}
}

const usage = `usage: otel-guardrail check <telemetry-contract.yaml>

Runs the Preflight Guardrail over a declared Telemetry Contract.
Every violated Standard is reported with its Severity; only a block Severity
fails the build.
Exit codes: 0 no blocking Standard violated, 1 a blocking Standard was violated,
2 the Guardrail could not run.`

func runCheck(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return exitError
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, usage)
		return exitError
	}

	declared, err := contract.Load(flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "otel-guardrail: %v\n", err)
		return exitError
	}

	preflight, err := guardrail.NewPreflight(guardrail.StandardPolicies())
	if err != nil {
		fmt.Fprintf(stderr, "otel-guardrail: %v\n", err)
		return exitError
	}

	result, err := preflight.Check(context.Background(), declared)
	if err != nil {
		fmt.Fprintf(stderr, "otel-guardrail: %v\n", err)
		return exitError
	}

	// Violations arrive most severe first, so the reason a build failed leads
	// the report and the severities read as groups.
	switch {
	case len(result.Violations) == 0:
		fmt.Fprintf(stdout, "%s: Telemetry Contract meets all Standards\n", declared.ServiceName)
	case result.FailsTheBuild():
		fmt.Fprintf(stdout, "%s: %d blocking Standard violation(s), %d non-blocking\n",
			declared.ServiceName, len(result.Blocking()), len(result.NonBlocking()))
	default:
		fmt.Fprintf(stdout, "%s: Telemetry Contract meets every blocking Standard; %d non-blocking finding(s) to address\n",
			declared.ServiceName, len(result.NonBlocking()))
	}
	for _, v := range result.Violations {
		fmt.Fprintf(stdout, "  %s\n", v)
	}

	if result.FailsTheBuild() {
		return exitViolation
	}
	return exitOK
}
