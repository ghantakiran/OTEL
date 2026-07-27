package guardrail

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ghantakiran/OTEL/contract"
)

//go:embed enforcement.yaml
var builtinSchedule []byte

// CentralEnforcementSchedule is the org's published schedule, shipped in the
// binary alongside the Standard catalog and the Waiver register.
func CentralEnforcementSchedule() (*EnforcementSchedule, error) {
	return parseEnforcementSchedule(builtinSchedule, "guardrail/enforcement.yaml")
}

// EnforcementSchedule is the org's published phasing of enforcement: one
// Enforcement Epoch that classifies a service as new or legacy, plus the day
// each Standard graduates to blocking for legacy services (ADR 0003).
type EnforcementSchedule struct {
	epoch       Date
	graduations map[string]Date
}

// scheduleKind is what an enforcement schedule file must declare itself to be.
const scheduleKind = "EnforcementSchedule"

// scheduleDocument is the on-disk shape of the schedule.
type scheduleDocument struct {
	APIVersion  string       `yaml:"apiVersion"`
	Kind        string       `yaml:"kind"`
	Epoch       Date         `yaml:"epoch"`
	Graduations []graduation `yaml:"graduations"`
}

type graduation struct {
	Standard  string `yaml:"standard"`
	Graduates Date   `yaml:"graduates"`
}

// LoadEnforcementSchedule reads an enforcement schedule from a YAML file.
func LoadEnforcementSchedule(path string) (*EnforcementSchedule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read enforcement schedule %s: %w", path, err)
	}
	return parseEnforcementSchedule(data, path)
}

func parseEnforcementSchedule(data []byte, origin string) (*EnforcementSchedule, error) {
	var document scheduleDocument
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("parse enforcement schedule %s: %w", origin, err)
	}
	if document.Kind != scheduleKind {
		return nil, fmt.Errorf("enforcement schedule %s: kind is %q, want %q", origin, document.Kind, scheduleKind)
	}
	// This file decides which services block today, so reading one written for a
	// later schema under this binary's rules is the worst kind of confident wrong.
	if err := contract.RequireAPIVersion(document.APIVersion, origin, "enforcement schedule"); err != nil {
		return nil, err
	}

	// A zero Date is not a missing value the caller can notice later — it reads as
	// the year 1, which every real date is after. An absent or misspelt `epoch:`
	// would therefore classify every service as new and block the entire fleet at
	// once, silently. The Waiver register refuses a Waiver with no expiry for
	// exactly this reason; this refuses the same shape of mistake.
	if document.Epoch.IsZero() {
		return nil, fmt.Errorf(
			"enforcement schedule %s publishes no Enforcement Epoch: set `epoch: YYYY-MM-DD`, "+
				"or every service is classified new and every blocking Standard blocks the whole fleet at once",
			origin)
	}

	schedule := &EnforcementSchedule{epoch: document.Epoch, graduations: map[string]Date{}}
	for _, g := range document.Graduations {
		if g.Standard == "" {
			return nil, fmt.Errorf("enforcement schedule %s has a graduation naming no Standard: set `standard:` on each entry", origin)
		}
		// Same trap, opposite direction: a zero deadline reads as one that has
		// already arrived, so the Standard would be published *and* block legacy
		// services immediately. A misspelt `standard:` key already fails loudly;
		// its neighbour must not fail silently.
		if g.Graduates.IsZero() {
			return nil, fmt.Errorf(
				"enforcement schedule %s gives Standard %q no graduation deadline: set `graduates: YYYY-MM-DD`, "+
					"or it blocks every legacy service the day it is published",
				origin, g.Standard)
		}
		schedule.graduations[g.Standard] = g.Graduates
	}
	return schedule, nil
}

// Epoch is the published Enforcement Epoch: a service whose Telemetry Contract
// first appeared on or after it is new, one appearing before it is legacy.
func (s *EnforcementSchedule) Epoch() Date {
	return s.epoch
}

// GraduatingStandards is every Standard the schedule publishes a deadline for,
// in a stable order. It exists so the schedule can be checked against the
// Standard catalog: a deadline for a Standard nobody declares defers nothing,
// and reads as filed.
func (s *EnforcementSchedule) GraduatingStandards() []string {
	if s == nil {
		return nil
	}
	standards := make([]string, 0, len(s.graduations))
	for standard := range s.graduations {
		standards = append(standards, standard)
	}
	sort.Strings(standards)
	return standards
}

// ServiceAge is which side of the Enforcement Epoch a service's Telemetry
// Contract first appeared on. It is the whole of what the Epoch decides.
type ServiceAge string

const (
	// ServiceNew is a service whose Contract first appeared on or after the
	// Epoch: every Standard enforces at its declared Severity immediately.
	ServiceNew ServiceAge = "new"
	// ServiceLegacy is a service whose Contract first appeared before the Epoch:
	// a blocking Standard is held back until that Standard graduates.
	ServiceLegacy ServiceAge = "legacy"
)

// Classify decides whether a service is new or legacy from the day its
// Telemetry Contract first appeared. The Epoch is inclusive — a Contract
// appearing on the published day is new — so no service falls between the two.
func (s *EnforcementSchedule) Classify(firstAppeared time.Time) ServiceAge {
	if s.epoch.daysUntil(firstAppeared) > 0 {
		return ServiceLegacy
	}
	return ServiceNew
}

// Graduation is the day a Standard starts blocking legacy services, if the
// schedule publishes one for it.
func (s *EnforcementSchedule) Graduation(standard string) (Date, bool) {
	graduates, published := s.graduations[standard]
	return graduates, published
}

// LegacyGrace is the deferral a legacy service still has on a blocking Standard
// that has not reached its graduation deadline. Like a Waiver, it holds the
// finding back without hiding it, and lapses without anyone taking an action.
type LegacyGrace struct {
	// Graduates is the day the Standard starts failing this service's build.
	Graduates Date `json:"graduates"`
}

// grace decides whether a legacy service is still held back from a blocking
// Standard on a given day.
//
// A Standard with no published graduation deadline is an error, not a decision.
// Defaulting to "keep deferring" would make the phased rollout permanent for
// that Standard — the hole ADR 0003 exists to prevent — and defaulting to
// "block now" would spring it on every legacy service the moment the Standard
// is authored, which is the political death the same ADR exists to avoid.
// Neither is ours to pick silently: the platform team publishes the day.
func (s *EnforcementSchedule) grace(standard string, asOf time.Time) (LegacyGrace, bool, error) {
	graduates, published := s.Graduation(standard)
	if !published {
		return LegacyGrace{}, false, fmt.Errorf(
			"the enforcement schedule does not say when Standard %q graduates to blocking: "+
				"publish a graduation deadline for it, or this legacy service can be neither blocked nor deferred",
			standard)
	}
	if graduates.daysUntil(asOf) <= 0 {
		return LegacyGrace{}, false, nil // the deadline has arrived; it blocks
	}
	return LegacyGrace{Graduates: graduates}, true, nil
}

// FirstAppearance reports the day a service's Telemetry Contract first appeared.
// It is the seam that keeps the Enforcement Epoch testable: production derives
// the day from git history, a test supplies it directly.
type FirstAppearance func() (time.Time, error)

// FirstAppearanceInGit derives a Contract's first-appearance day from the first
// commit that added it. Git is the source of truth (ADR 0004), so the date is
// not the service team's to declare — and not theirs to backdate.
func FirstAppearanceInGit(contractPath string) FirstAppearance {
	return func() (time.Time, error) {
		// Absence of history must be detected, never inferred from a failed
		// lookup. A shallow clone grafts its tip commit as parentless, so git
		// happily reports every file in it as *added* in that tip — the lookup
		// below would succeed and return today, classifying every legacy service
		// as new and blocking the whole fleet at once. That failure is silent,
		// and it is the one ADR 0003 exists to prevent, so it is checked first.
		if err := requireFullHistory(contractPath); err != nil {
			return time.Time{}, err
		}

		// --reverse puts the commit that added the file first; --diff-filter=A
		// keeps it to additions, so a later rename or rewrite cannot reset the clock.
		command := exec.Command("git", "log", "--diff-filter=A", "--format=%aI", "--reverse", "--", filepath.Base(contractPath))
		command.Dir = filepath.Dir(contractPath)

		out, err := command.Output()
		if err != nil {
			return time.Time{}, fmt.Errorf("%w: %s", errNoContractHistory(contractPath), gitComplaint(err))
		}

		first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		if first == "" {
			return time.Time{}, errNoContractHistory(contractPath)
		}

		appeared, err := time.Parse(time.RFC3339, first)
		if err != nil {
			return time.Time{}, fmt.Errorf("read the first commit of %s: %w", contractPath, err)
		}
		return appeared, nil
	}
}

// requireFullHistory refuses a checkout whose history has been truncated.
func requireFullHistory(contractPath string) error {
	shallow := exec.Command("git", "rev-parse", "--is-shallow-repository")
	shallow.Dir = filepath.Dir(contractPath)

	out, err := shallow.Output()
	if err != nil {
		// Not a git repository at all, or git is unavailable. Either way the
		// Contract cannot be dated; the lookup below reports it in full.
		return nil
	}
	if strings.TrimSpace(string(out)) == "true" {
		return errShallowCheckout(contractPath)
	}
	return nil
}

func errShallowCheckout(contractPath string) error {
	return fmt.Errorf(
		"cannot tell when %s first appeared: this is a shallow clone, so its history does not reach the commit "+
			"that added the Contract, and the Enforcement Epoch cannot classify this service as new or legacy. "+
			"Check out full history (actions/checkout with fetch-depth: 0)",
		contractPath)
}

// errNoContractHistory says what to do about it. Without the first-appearance
// day the Enforcement Epoch cannot classify the service, and guessing either way
// is worse than stopping: guessing "new" fails every legacy service the moment
// someone shallow-clones, and guessing "legacy" hands every service a way out.
func errNoContractHistory(contractPath string) error {
	return fmt.Errorf(
		"cannot tell when %s first appeared: no commit in this checkout adds it, so the Enforcement Epoch "+
			"cannot classify this service as new or legacy. A shallow clone has no such history — check out "+
			"full history (actions/checkout with fetch-depth: 0), or commit the Telemetry Contract first",
		contractPath)
}

func gitComplaint(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) && len(exit.Stderr) > 0 {
		return strings.TrimSpace(string(exit.Stderr))
	}
	return err.Error()
}
