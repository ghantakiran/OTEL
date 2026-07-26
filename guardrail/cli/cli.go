// Package cli is the command surface of otel-guardrail. It owns argument
// parsing, result formatting and exit codes; the Guardrail itself lives in the
// guardrail package.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

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

const usage = `usage: otel-guardrail check [--waivers <waivers.yaml>] [--as-of <YYYY-MM-DD>] <telemetry-contract.yaml>

Runs the Preflight Guardrail over a declared Telemetry Contract.
Every violated Standard is reported with its Severity; only a block Severity
fails the build. A violation held back by an unexpired Waiver is reported with
its approver and expiry date, and does not fail the build.

  --waivers  Waiver register to honour. Defaults to the org register built into
             this binary (guardrail/waivers.yaml).
  --as-of    Day to judge Waiver expiry on, YYYY-MM-DD. Defaults to today; set it
             to a future day to see which Waivers will have lapsed by then.

Exit codes: 0 no blocking Standard violated, 1 a blocking Standard was violated,
2 the Guardrail could not run.`

// dateLayout is the one date format the CLI speaks, matching a Waiver's expiry.
const dateLayout = "2006-01-02"

func runCheck(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	waiverRegister := flags.String("waivers", "", "Waiver register to honour (default: the register built into this binary)")
	asOf := flags.String("as-of", "", "day to judge Waiver expiry on, YYYY-MM-DD (default: today)")
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

	// A broken register is the platform team's bug, not a finding about this
	// Contract, so it lands on exit 2 alongside a broken Standard catalog.
	waivers, err := waiverRegisterFrom(*waiverRegister)
	if err != nil {
		fmt.Fprintf(stderr, "otel-guardrail: %v\n", err)
		return exitError
	}
	clock, err := clockAt(*asOf)
	if err != nil {
		fmt.Fprintf(stderr, "otel-guardrail: %v\n", err)
		return exitError
	}

	preflight, err := guardrail.NewPreflight(guardrail.StandardPolicies(),
		guardrail.WithWaivers(waivers), guardrail.WithClock(clock))
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
	// the report and the severities read as groups. A Waiver never removes a
	// violation from the report — it says why one stopped failing the build, and
	// until when.
	waived := len(result.Waived())
	otherNonBlocking := len(result.NonBlocking()) - waived
	switch {
	case len(result.Violations) == 0:
		fmt.Fprintf(stdout, "%s: Telemetry Contract meets all Standards\n", declared.ServiceName)
	case result.FailsTheBuild() && waived > 0:
		fmt.Fprintf(stdout, "%s: %d blocking Standard violation(s), %d non-blocking, %d held back by a Waiver\n",
			declared.ServiceName, len(result.Blocking()), otherNonBlocking, waived)
	case result.FailsTheBuild():
		fmt.Fprintf(stdout, "%s: %d blocking Standard violation(s), %d non-blocking\n",
			declared.ServiceName, len(result.Blocking()), len(result.NonBlocking()))
	case waived > 0:
		fmt.Fprintf(stdout, "%s: nothing fails the build, but %d blocking Standard violation(s) are only held back by a Waiver; %d other non-blocking finding(s) to address\n",
			declared.ServiceName, waived, otherNonBlocking)
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

// waiverRegisterFrom picks the register to honour: the one the caller pointed
// at, else the org register compiled into this binary.
func waiverRegisterFrom(path string) (*guardrail.WaiverRegister, error) {
	if path == "" {
		return guardrail.CentralWaiverRegister()
	}
	return guardrail.LoadWaiverRegister(path)
}

// clockAt fixes the day Waiver expiry is judged on. Left empty it is today, so
// an expired Waiver stops holding without anyone running anything.
func clockAt(day string) (guardrail.Clock, error) {
	if day == "" {
		return time.Now, nil
	}
	fixed, err := time.Parse(dateLayout, day)
	if err != nil {
		return nil, fmt.Errorf("--as-of %q is not a YYYY-MM-DD date", day)
	}
	return func() time.Time { return fixed }, nil
}
