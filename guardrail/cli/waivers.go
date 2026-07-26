package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/ghantakiran/OTEL/guardrail"
)

// waiversUsage documents the expiry report. ADR 0003 makes the Waiver the
// escape hatch that keeps phased enforcement survivable, and warns that without
// a report of soon-to-expire Waivers the hatch becomes a permanent hole. This
// command is that report; the scheduled job in .github/workflows/waiver-expiry.yml
// runs it daily.
const waiversUsage = `usage: otel-guardrail waivers [--within <days>] [--as-of <YYYY-MM-DD>] [--register <waivers.yaml>] [--format text|json]

Reports the Waivers that need somebody's attention: those already expired, and
those expiring within the next --within days. An expiring Waiver is a deadline —
someone must fix the Telemetry Contract or file a fresh Waiver before the build
breaks. An expired one is a problem now: the Standard blocks that service again
already. The two are reported separately for that reason.

  --within    How many days ahead to look. Defaults to 30.
  --as-of     Day to judge Waiver expiry on, YYYY-MM-DD. Defaults to today; set it
              to a future day to see what next month's report will say.
  --register  Waiver register to scan. Defaults to the org register built into
              this binary (guardrail/waivers.yaml).
  --format    text (default) for a person, json for a program.

Exit codes: 0 no Waiver needs attention, 1 at least one Waiver needs attention,
2 the report could not run.`

func runWaivers(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("waivers", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprintln(stderr, waiversUsage) }
	register := flags.String("register", "", "Waiver register to scan (default: the register built into this binary)")
	asOf := flags.String("as-of", "", "day to judge Waiver expiry on, YYYY-MM-DD (default: today)")
	within := flags.Int("within", defaultWithinDays, "how many days ahead to look for expiring Waivers")
	format := flags.String("format", formatText, "output format: text or json")
	if err := flags.Parse(args); err != nil {
		return exitError
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, waiversUsage)
		return exitError
	}

	// A negative window silently matches nothing, and "nothing to report" is
	// exactly what an all-clear looks like — the failure this job exists to
	// prevent. Refuse rather than reassure.
	if *within < 0 {
		fmt.Fprintf(stderr, "otel-guardrail: --within %d is not a number of days ahead to look\n", *within)
		return exitError
	}
	// Likewise a format nobody can produce: falling back to text would hand a
	// caller expecting JSON some prose its parser reads as an empty report.
	if *format != formatText && *format != formatJSON {
		fmt.Fprintf(stderr, "otel-guardrail: --format %q is not one of text, json\n", *format)
		return exitError
	}

	// A register that cannot be read is the platform team's bug and lands on
	// exit 2, alongside a broken Standard catalog — never on exit 1, which would
	// tell the scheduled job to file an issue about Waivers it never scanned.
	waivers, err := waiverRegisterFrom(*register)
	if err != nil {
		fmt.Fprintf(stderr, "otel-guardrail: %v\n", err)
		return exitError
	}
	clock, err := clockAt(*asOf)
	if err != nil {
		fmt.Fprintf(stderr, "otel-guardrail: %v\n", err)
		return exitError
	}

	report := expiryReportOf(waivers, clock(), *within)

	if *format == formatJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			fmt.Fprintf(stderr, "otel-guardrail: %v\n", err)
			return exitError
		}
	} else {
		writeExpiryReport(stdout, report)
	}

	// Exit 1 means "somebody must act", the same as it does for a violated
	// Standard: a finding, not a tooling failure. The scheduled job branches on
	// this alone to decide whether to file its tracking issue or close it.
	if report.needsAttention() {
		return exitViolation
	}
	return exitOK
}

const (
	// defaultWithinDays is the notice period a Waiver's approver gets. A month is
	// long enough to fix a Telemetry Contract or get a fresh Waiver reviewed and
	// merged, and short enough that the report is not permanently full.
	defaultWithinDays = 30

	formatText = "text"
	formatJSON = "json"
)

// expiryReport is what the daily scan found. Its JSON is the contract the
// scheduled job reads: the job builds an issue body and the list of approvers to
// notify from this structure, so that changing the prose below never changes
// what the job files.
type expiryReport struct {
	AsOf       string           `json:"as_of"`
	WithinDays int              `json:"within_days"`
	Expired    []reportedWaiver `json:"expired"`
	Expiring   []reportedWaiver `json:"expiring"`
}

// reportedWaiver is one Waiver as the report describes it — every field a reader
// needs in order to act without opening the register.
type reportedWaiver struct {
	ServiceName     string `json:"service_name"`
	Standard        string `json:"standard"`
	Reason          string `json:"reason"`
	ApprovedBy      string `json:"approved_by"`
	Expires         string `json:"expires"`
	DaysUntilExpiry int    `json:"days_until_expiry"`
}

// expiryReportOf splits the register into the two problems it holds on a given
// day. `asOf` is the only clock in play: Waiver.DaysUntilExpiry owns the
// arithmetic, here as everywhere (ADR 0003).
func expiryReportOf(register *guardrail.WaiverRegister, asOf time.Time, within int) expiryReport {
	// Never nil: the job's `jq` counts these lists, and `null | length` is not 0.
	report := expiryReport{
		AsOf:       asOf.Format(dateLayout),
		WithinDays: within,
		Expired:    []reportedWaiver{},
		Expiring:   []reportedWaiver{},
	}

	var expired, expiring []guardrail.Waiver
	for _, w := range register.Waivers() {
		switch days := w.DaysUntilExpiry(asOf); {
		case days < 0:
			expired = append(expired, w)
		case days <= within:
			expiring = append(expiring, w)
		}
	}

	// Most urgent first within each group: a reader acts top-down, and the
	// longest-lapsed Waiver has been blocking a service longest.
	sortByDaysUntilExpiry(expired, asOf)
	sortByDaysUntilExpiry(expiring, asOf)

	for _, w := range expired {
		report.Expired = append(report.Expired, describe(w, asOf))
	}
	for _, w := range expiring {
		report.Expiring = append(report.Expiring, describe(w, asOf))
	}
	return report
}

func describe(w guardrail.Waiver, asOf time.Time) reportedWaiver {
	return reportedWaiver{
		ServiceName:     w.ServiceName,
		Standard:        w.Standard,
		Reason:          w.Reason,
		ApprovedBy:      w.ApprovedBy,
		Expires:         w.Expires.String(),
		DaysUntilExpiry: w.DaysUntilExpiry(asOf),
	}
}

func (r expiryReport) needsAttention() bool {
	return len(r.Expired)+len(r.Expiring) > 0
}

// sortByDaysUntilExpiry puts the Waiver closest to (or furthest past) its expiry
// first, breaking ties by service and Standard so two runs on the same day over
// the same register produce byte-identical output — the scheduled job compares
// its report against the issue it already filed, and a reshuffled list would
// read as a change.
func sortByDaysUntilExpiry(waivers []guardrail.Waiver, asOf time.Time) {
	sort.SliceStable(waivers, func(i, j int) bool {
		left, right := waivers[i].DaysUntilExpiry(asOf), waivers[j].DaysUntilExpiry(asOf)
		if left != right {
			return left < right
		}
		if waivers[i].ServiceName != waivers[j].ServiceName {
			return waivers[i].ServiceName < waivers[j].ServiceName
		}
		return waivers[i].Standard < waivers[j].Standard
	})
}

func writeExpiryReport(stdout io.Writer, r expiryReport) {
	if !r.needsAttention() {
		fmt.Fprintf(stdout, "No Waiver has expired and none expires within %s, as of %s\n",
			plural(r.WithinDays, "day"), r.AsOf)
		return
	}

	fmt.Fprintf(stdout, "%s to review as of %s: %d already expired, %d expiring within %s\n",
		plural(len(r.Expired)+len(r.Expiring), "Waiver"), r.AsOf,
		len(r.Expired), len(r.Expiring), plural(r.WithinDays, "day"))

	// Expired first: those services are blocked right now.
	if len(r.Expired) > 0 {
		fmt.Fprintln(stdout, "\nAlready expired — the Standard is blocking these services again:")
		for _, w := range r.Expired {
			fmt.Fprintf(stdout, "  %s\n", w)
		}
	}
	if len(r.Expiring) > 0 {
		fmt.Fprintf(stdout, "\nExpiring within %s — fix the Telemetry Contract or file a fresh Waiver:\n",
			plural(r.WithinDays, "day"))
		for _, w := range r.Expiring {
			fmt.Fprintf(stdout, "  %s\n", w)
		}
	}
}

// String is one Waiver on one line: the service, the Standard it holds back,
// when it goes (or went), and who approved it.
func (w reportedWaiver) String() string {
	return fmt.Sprintf("%s: Standard %s %s, approved by %s",
		w.ServiceName, w.Standard, w.expiryPhrase(), w.ApprovedBy)
}

// expiryPhrase says how far off the expiry is, in the tense the day deserves: an
// expired Waiver is a problem now, an expiring one is a deadline.
func (w reportedWaiver) expiryPhrase() string {
	switch {
	case w.DaysUntilExpiry < 0:
		return fmt.Sprintf("expired %s, %s ago", w.Expires, plural(-w.DaysUntilExpiry, "day"))
	case w.DaysUntilExpiry == 0:
		return fmt.Sprintf("expires %s, today", w.Expires)
	default:
		return fmt.Sprintf("expires %s, in %s", w.Expires, plural(w.DaysUntilExpiry, "day"))
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
