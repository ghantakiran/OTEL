package guardrail

import (
	_ "embed"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ghantakiran/OTEL/contract"
)

// Waiver is a time-boxed, owner-approved exemption letting one service skip one
// Standard until an expiry date, downgrading that Standard's effective
// enforcement from block to non-failing. It is scoped to exactly one service and
// exactly one Standard, and it reverts on its own when the expiry passes — no
// one has to take an action to re-enable enforcement (ADR 0003).
type Waiver struct {
	ServiceName string `yaml:"service_name"`
	Standard    string `yaml:"standard"`
	Reason      string `yaml:"reason"`
	ApprovedBy  string `yaml:"approved_by"`
	Expires     Date   `yaml:"expires"`
}

// DaysUntilExpiry is the whole days left before the Waiver expires, judged on
// the given day: positive while it still holds, 0 on the expiry day itself
// (a Waiver holds for all of the day it names), negative once it has expired.
// This is the one place expiry arithmetic lives — the expiry report that lists
// Waivers lapsing within N days asks the same question.
func (w Waiver) DaysUntilExpiry(asOf time.Time) int {
	return w.Expires.daysUntil(asOf)
}

// daysUntil is the whole days from a given day to this one: positive while the
// date is still ahead, 0 on the day itself, negative once it has passed. Both
// a Waiver's expiry and a Standard's graduation deadline are answered here, so
// "the day it names counts" is decided once.
func (d Date) daysUntil(asOf time.Time) int {
	return int(d.day.Sub(startOfDay(asOf)).Hours() / 24)
}

// startOfDay drops the time of day, so "now" is a calendar day like an expiry.
func startOfDay(t time.Time) time.Time {
	year, month, day := t.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// Date is a calendar day: a Waiver expires on a day, not at an instant.
type Date struct {
	day time.Time
}

func (d Date) String() string {
	return d.day.Format(dateLayout)
}

// IsZero reports whether no date was given at all.
func (d Date) IsZero() bool {
	return d.day.IsZero()
}

const dateLayout = "2006-01-02"

// UnmarshalYAML reads a plain YYYY-MM-DD scalar. The message stays neutral about
// which field it came from: a Date is a Waiver's expiry, the Enforcement Epoch
// and a graduation deadline, and saying "expiry date" for a bad `epoch:` sent
// operators grepping the schedule for a word that is not in it. The yaml decoder
// supplies the line number.
func (d *Date) UnmarshalYAML(node *yaml.Node) error {
	day, err := time.Parse(dateLayout, node.Value)
	if err != nil {
		return fmt.Errorf("%q is not a YYYY-MM-DD date", node.Value)
	}
	d.day = day
	return nil
}

// WaiverRegister is the org's central register of Waivers. Waivers live here,
// in the platform team's repository, and not in service repositories: a service
// must not be able to waive itself, and the register is the single place that
// can be scanned for expiring Waivers.
type WaiverRegister struct {
	waivers []Waiver
}

// registerKind is what a Waiver register file must declare itself to be.
const registerKind = "WaiverRegister"

// registerDocument is the on-disk shape of the register.
type registerDocument struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Waivers    []Waiver `yaml:"waivers"`
}

//go:embed waivers.yaml
var builtinWaivers []byte

// CentralWaiverRegister is the org's register, shipped inside the CLI from
// guardrail/waivers.yaml — the same way the Standard catalog ships. Git is the
// source of truth and there is no service to fetch from at run time (ADR 0004),
// so the register travels with the binary a service repo pins.
func CentralWaiverRegister() (*WaiverRegister, error) {
	return parseWaiverRegister(builtinWaivers, "guardrail/waivers.yaml")
}

// LoadWaiverRegister reads a Waiver register from a YAML file.
func LoadWaiverRegister(path string) (*WaiverRegister, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Waiver register %s: %w", path, err)
	}
	return parseWaiverRegister(data, path)
}

func parseWaiverRegister(data []byte, origin string) (*WaiverRegister, error) {
	var document registerDocument
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("parse Waiver register %s: %w", origin, err)
	}
	// A file that is not a register would otherwise load as a register with no
	// Waivers in it, and every Waiver the platform team wrote would silently
	// stop being honoured.
	if document.Kind != registerKind {
		return nil, fmt.Errorf("Waiver register %s: kind is %q, want %q", origin, document.Kind, registerKind)
	}
	// Decoded and ignored was the old behaviour, and it is the one thing this
	// field must never be: an old binary reading a newer register under this
	// binary's rules would honour Waivers by rules that no longer apply.
	if err := contract.RequireAPIVersion(document.APIVersion, origin, "Waiver register"); err != nil {
		return nil, err
	}
	// A Waiver covers exactly one service and one Standard, so two of them for the
	// same pair means one is inert: present in the register, reading as approved,
	// doing nothing. Retiring or extending that one changes nothing and the
	// confusion is unbounded, so the register is refused instead.
	covered := make(map[string]struct{}, len(document.Waivers))
	for _, w := range document.Waivers {
		if err := w.validate(); err != nil {
			return nil, fmt.Errorf("Waiver register %s: %w", origin, err)
		}
		scope := w.ServiceName + "/" + w.Standard
		if _, twice := covered[scope]; twice {
			return nil, fmt.Errorf(
				"Waiver register %s covers %s twice: only one Waiver can hold a Standard back for a service, "+
					"so the other would sit in the register doing nothing",
				origin, scope)
		}
		covered[scope] = struct{}{}
	}
	return &WaiverRegister{waivers: document.Waivers}, nil
}

// validate rejects a Waiver that is not what a Waiver is required to be: an
// unbounded, unexplained or unapproved exemption is exactly the permanent hole
// ADR 0003 warns about, so an incomplete Waiver is an error against the
// register — the platform team's bug — and never a violation charged to the
// service team.
func (w Waiver) validate() error {
	if w.ServiceName == "" {
		return w.incomplete("service_name", "a Waiver is scoped to exactly one service")
	}
	if w.Standard == "" {
		return w.incomplete("standard", "a Waiver is scoped to exactly one Standard")
	}
	if w.Reason == "" {
		return w.incomplete("reason", "a Waiver must say why the Standard cannot be met yet, or nobody can review it")
	}
	if w.ApprovedBy == "" {
		return w.incomplete("approved_by", "a Waiver must name its approver; a service does not waive itself")
	}
	if w.Expires.IsZero() {
		return w.incomplete("expires", "a Waiver must be time-boxed, so that enforcement reverts on its own")
	}
	return nil
}

func (w Waiver) incomplete(field, why string) error {
	return fmt.Errorf("Waiver for service %q and Standard %q declares no %q: %s",
		w.ServiceName, w.Standard, field, why)
}

// Waivers is every Waiver in the register, in the order the platform team wrote
// them.
func (r *WaiverRegister) Waivers() []Waiver {
	if r == nil {
		return nil
	}
	return r.waivers
}

// InForce is the Waiver holding back one Standard for one service on a given
// day, if there is one. asOf is the seam that makes expiry testable and lets a
// caller ask what the register will look like on a future day; nothing here
// reads the wall clock.
func (r *WaiverRegister) InForce(serviceName, standard string, asOf time.Time) (Waiver, bool) {
	if r == nil {
		return Waiver{}, false
	}
	for _, w := range r.waivers {
		if w.ServiceName == serviceName && w.Standard == standard && w.DaysUntilExpiry(asOf) >= 0 {
			return w, true
		}
	}
	return Waiver{}, false
}
