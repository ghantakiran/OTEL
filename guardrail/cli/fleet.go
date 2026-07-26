package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/ghantakiran/OTEL/controlplane"
	"github.com/ghantakiran/OTEL/guardrail"
)

const compileFleetUsage = `usage: otel-guardrail compile-fleet [--profiles <profiles.yaml>] <fleet-directory>

Compiles every Telemetry Contract in a Fleet into collector configuration, in
place, so that the result can be committed and rolled out by existing GitOps
tooling (ADR 0006). This is the fleet-wide counterpart to 'compile', which writes
one service's config to stdout.

The Fleet directory holds one Contract per service, named for that service:

  <fleet>/contracts/checkout-api.yaml   the Telemetry Contract, hand-written
  <fleet>/compiled/checkout-api.yaml    its collector configuration, generated
  <fleet>/rollout-manifest.yaml         the Rollout Manifest, generated

Compiling is deterministic: the same Contracts produce byte-identical files, and a
file already correct is left untouched. A rollout is therefore reviewed as a git
diff that shows only what actually changed.

A Contract that does not compile is reported and skipped — one team's mistake does
not freeze every other team's rollout. The skip is recorded in the Rollout Manifest
and this command exits 1, so the partial state is in the diff a human reviews. That
service keeps whatever collector configuration it last compiled to; a config with
no Contract behind it at all is pruned.

  --profiles  Pipeline Profiles to compile against. Defaults to the org Profiles
              built into this binary (controlplane/profiles.yaml).
  --format    text (default) for a person, json for a program. The scheduled
              rollout job reads the json.

Exit codes: 0 every Contract compiled, 1 at least one did not, 2 the compiler
could not run.`

func runCompileFleet(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("compile-fleet", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprintln(stderr, compileFleetUsage) }
	profilePath := flags.String("profiles", "", "Pipeline Profiles to compile against (default: the Profiles built into this binary)")
	format := flags.String("format", formatText, "output format: text or json")
	if err := flags.Parse(args); err != nil {
		return exitError
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, compileFleetUsage)
		return exitError
	}

	// Falling back to text would hand a caller expecting JSON some prose its parser
	// reads as an empty rollout — a silent all-clear, the one outcome this must
	// never fake.
	if *format != formatText && *format != formatJSON {
		fmt.Fprintf(stderr, "otel-guardrail: --format %q is not one of text, json\n", *format)
		return exitError
	}

	// An unreadable Fleet, a broken Profile set or an incoherent compiled config are
	// all the platform team's problem rather than a statement about any one service:
	// exit 2, the same split `check` and `compile` make.
	fleet, err := controlplane.LoadFleet(flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "otel-guardrail: %v\n", err)
		return exitError
	}
	taxonomy, err := guardrail.CentralTaxonomy()
	if err != nil {
		fmt.Fprintf(stderr, "otel-guardrail: %v\n", err)
		return exitError
	}
	profiles, err := pipelineProfilesFrom(*profilePath)
	if err != nil {
		fmt.Fprintf(stderr, "otel-guardrail: %v\n", err)
		return exitError
	}

	rollout, err := controlplane.CompileFleet(fleet, taxonomy, profiles)
	if err != nil {
		fmt.Fprintf(stderr, "otel-guardrail: %v\n", err)
		return exitError
	}
	back, err := rollout.Write()
	if err != nil {
		fmt.Fprintf(stderr, "otel-guardrail: %v\n", err)
		return exitError
	}

	if *format == formatJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(rolloutReportOf(rollout, back)); err != nil {
			fmt.Fprintf(stderr, "otel-guardrail: %v\n", err)
			return exitError
		}
	} else {
		writeRolloutReport(stdout, rollout, back)
	}

	// Exit 1 means "somebody must act", as it does for a violated Standard and for
	// a Contract that will not compile on its own. Files that did compile have
	// still been written: the point is that the failure is never silent, not that
	// the whole fleet waits for one team.
	if len(rollout.Failed) > 0 {
		return exitViolation
	}
	return exitOK
}

// rolloutReport is what one fleet-wide Compile did, as the scheduled rollout job
// reads it. That job builds its pull-request body from this structure and not from
// the prose below, so rewording the terminal output never rewrites an open PR.
type rolloutReport struct {
	Compiled    []reportedService `json:"compiled"`
	NotCompiled []reportedFailure `json:"not_compiled"`
	Written     []string          `json:"written"`
	Unchanged   []string          `json:"unchanged"`
	Pruned      []string          `json:"pruned"`
}

type reportedService struct {
	ServiceName string   `json:"service_name"`
	Tier        string   `json:"tier"`
	Profile     string   `json:"pipeline_profile"`
	Signals     []string `json:"signals"`
	Config      string   `json:"collector_config"`
	Digest      string   `json:"digest"`
}

type reportedFailure struct {
	ServiceName string `json:"service_name"`
	Contract    string `json:"telemetry_contract"`
	Tier        string `json:"tier"`
	Reason      string `json:"reason"`
}

func rolloutReportOf(rollout controlplane.Rollout, back controlplane.Writeback) rolloutReport {
	// Every list is present and empty rather than absent: the job counts them with
	// jq, where `null | length` is not 0, and a missing key reads as a report that
	// never looked rather than one that found nothing.
	report := rolloutReport{
		Compiled:    []reportedService{},
		NotCompiled: []reportedFailure{},
		Written:     orEmpty(back.Wrote),
		Unchanged:   orEmpty(back.Unchanged),
		Pruned:      orEmpty(back.Pruned),
	}
	for _, service := range rollout.Compiled {
		report.Compiled = append(report.Compiled, reportedService{
			ServiceName: service.ServiceName,
			Tier:        service.Tier,
			Profile:     service.Profile,
			Signals:     orEmpty(service.Signals),
			Config:      service.Path,
			Digest:      service.Digest,
		})
	}
	for _, failed := range rollout.Failed {
		report.NotCompiled = append(report.NotCompiled, reportedFailure{
			ServiceName: failed.ServiceName,
			Contract:    failed.ContractPath,
			Tier:        failed.Tier,
			Reason:      failed.Reason,
		})
	}
	return report
}

func orEmpty(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// writeRolloutReport says what the Fleet compiled to and what it did to the
// directory. Every file touched is named, because the next step is committing
// them and a reviewer needs to know what to expect in the diff.
func writeRolloutReport(stdout io.Writer, rollout controlplane.Rollout, back controlplane.Writeback) {
	total := len(rollout.Compiled) + len(rollout.Failed)
	fmt.Fprintf(stdout, "%s in the Fleet: %d compiled, %d did not\n",
		plural(total, "Telemetry Contract"), len(rollout.Compiled), len(rollout.Failed))

	for _, file := range back.Wrote {
		fmt.Fprintf(stdout, "  %s written\n", file)
	}
	for _, file := range back.Unchanged {
		fmt.Fprintf(stdout, "  %s unchanged\n", file)
	}
	for _, file := range back.Pruned {
		fmt.Fprintf(stdout, "  %s pruned — no Telemetry Contract in the Fleet accounts for it\n", file)
	}
	if !back.Changed() {
		fmt.Fprintln(stdout, "  nothing changed — the compiled configuration already matches the Contracts")
	}

	if len(rollout.Failed) == 0 {
		return
	}
	// The count is in the headline already, so this line says what happens next
	// rather than repeating it — and reads the same for one Contract or forty.
	fmt.Fprintln(stdout, "\nDid not compile, so the last collector configuration that did is left in place:")
	for _, failed := range rollout.Failed {
		fmt.Fprintf(stdout, "  %s: %s\n", failed.ContractPath, failed.Reason)
	}
}
