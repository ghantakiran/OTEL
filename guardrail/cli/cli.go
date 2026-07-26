// Package cli is the command surface of otel-guardrail. It owns argument
// parsing, result formatting and exit codes; the Guardrail itself lives in the
// guardrail package.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
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
	case "waivers":
		return runWaivers(args[1:], stdout, stderr)
	case "compile":
		return runCompile(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s\n", args[0], usage)
		return exitError
	}
}

const usage = `usage: otel-guardrail <command> [flags]

Commands:
  check     Run the Preflight Guardrail over a service's Telemetry Contract.
  waivers   Report Waivers that have expired or are about to. Run
            'otel-guardrail waivers --help' for its flags.
  compile   Compile a Telemetry Contract with its tier's Pipeline Profile into
            collector configuration. Run 'otel-guardrail compile --help'.

otel-guardrail check [--waivers <waivers.yaml>] [--enforcement <enforcement.yaml>]
                     [--as-of <YYYY-MM-DD>] <telemetry-contract.yaml>

Runs the Preflight Guardrail over a declared Telemetry Contract.
Every violated Standard is reported with its Severity; only a block Severity
fails the build. A violation held back by an unexpired Waiver is reported with
its approver and expiry date, and does not fail the build.

A service whose Telemetry Contract first appeared before the org's Enforcement
Epoch is legacy: a blocking Standard is reported but held back until that
Standard's graduation deadline, then blocks by itself. First appearance comes
from the first git commit that added the Contract, so full history is required.

  --waivers      Waiver register to honour. Defaults to the org register built
                 into this binary (guardrail/waivers.yaml).
  --enforcement  Enforcement schedule to apply. Defaults to the org schedule
                 built into this binary (guardrail/enforcement.yaml).
  --as-of        Day to judge Waiver expiry and graduation deadlines on,
                 YYYY-MM-DD. Defaults to today; set it to a future day to see
                 what will have lapsed or graduated by then.

Exit codes: 0 no blocking Standard violated, 1 a blocking Standard was violated,
2 the Guardrail could not run.`

// dateLayout is the one date format the CLI speaks, matching a Waiver's expiry.
const dateLayout = "2006-01-02"

func runCheck(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	waiverRegister := flags.String("waivers", "", "Waiver register to honour (default: the register built into this binary)")
	schedulePath := flags.String("enforcement", "", "enforcement schedule to apply (default: the schedule built into this binary)")
	asOf := flags.String("as-of", "", "day to judge Waiver expiry and graduation deadlines on, YYYY-MM-DD (default: today)")
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

	schedule, err := enforcementScheduleFrom(*schedulePath)
	if err != nil {
		fmt.Fprintf(stderr, "otel-guardrail: %v\n", err)
		return exitError
	}

	preflight, err := guardrail.NewPreflight(guardrail.StandardPolicies(),
		guardrail.WithWaivers(waivers),
		guardrail.WithEnforcementSchedule(schedule),
		// The service is dated by its Contract's own git history, so the date is
		// not the service team's to declare and not theirs to backdate.
		guardrail.WithFirstAppearance(guardrail.FirstAppearanceInGit(flags.Arg(0))),
		guardrail.WithClock(clock))
	if err != nil {
		fmt.Fprintf(stderr, "otel-guardrail: %v\n", err)
		return exitError
	}

	result, err := preflight.Check(context.Background(), declared)
	if err != nil {
		fmt.Fprintf(stderr, "otel-guardrail: %v\n", err)
		return exitError
	}

	// Violations arrive most severe first, so the reason a build failed leads the
	// report and the severities read as groups. Neither a Waiver nor a legacy
	// deferral ever removes a violation from the report — each says why one
	// stopped failing the build, and until when.
	fmt.Fprintf(stdout, "%s: %s\n", declared.ServiceName, summarize(result))
	for _, v := range result.Violations {
		fmt.Fprintf(stdout, "  %s\n", v)
	}

	if result.FailsTheBuild() {
		return exitViolation
	}
	return exitOK
}

// summarize is the one-line headline above the violations. Every count and every
// name in it comes from the Result's effective enforcement, so the summary cannot
// disagree with the per-violation lines below it or with the exit code.
func summarize(result guardrail.Result) string {
	if len(result.Violations) == 0 {
		return "Telemetry Contract meets all Standards"
	}

	failing := len(result.With(guardrail.EnforcementFailsBuild))
	heldBack := len(result.With(guardrail.EnforcementHeldBack))
	advisory := len(result.With(guardrail.EnforcementAdvisory))

	var parts []string
	if failing > 0 {
		parts = append(parts, fmt.Sprintf("%s failing the build", count(failing, "blocking Standard violation")))
	}
	if heldBack > 0 {
		// Named, not buried in a total, and said with what is holding them: each of
		// these breaks the build on a known day with nobody deciding anything
		// further, and the reader needs to know which clock to watch. Describe reads
		// the held-back violations only, so it cannot name a Hold on a finding that
		// was never going to fail the build.
		parts = append(parts, fmt.Sprintf("%s held back by %s",
			count(heldBack, "blocking Standard violation"), result.Describe()))
	}
	if advisory > 0 {
		parts = append(parts, count(advisory, "non-blocking finding")+" to address")
	}

	headline := strings.Join(parts, ", ")
	if failing > 0 {
		return headline
	}
	return "nothing fails the build — " + headline
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// waiverRegisterFrom picks the register to honour: the one the caller pointed
// at, else the org register compiled into this binary.
func waiverRegisterFrom(path string) (*guardrail.WaiverRegister, error) {
	if path == "" {
		return guardrail.CentralWaiverRegister()
	}
	return guardrail.LoadWaiverRegister(path)
}

// enforcementScheduleFrom picks the schedule to apply: the one the caller
// pointed at, else the org schedule compiled into this binary.
func enforcementScheduleFrom(path string) (*guardrail.EnforcementSchedule, error) {
	if path == "" {
		return guardrail.CentralEnforcementSchedule()
	}
	return guardrail.LoadEnforcementSchedule(path)
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
