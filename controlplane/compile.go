package controlplane

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/ghantakiran/OTEL/contract"
	"github.com/ghantakiran/OTEL/guardrail"
)

// Compile turns a Telemetry Contract into collector configuration for that
// service's Agent, by combining it with its Service Tier's Pipeline Profile
// (ADR 0005).
//
// It is the single entry point for both callers: the CLI's `compile` command and
// the Control Plane's fleet-wide rollout. Three inputs, each owning one fact —
// the Contract says what the service emits, the Taxonomy says what its tier must
// emit, the Profile says how it ships — and none of the three restates another.
//
// Compile refuses rather than guesses. A Contract that omits a Signal its tier
// mandates, names something that is not a Signal, declares a tier outside the
// taxonomy, or belongs to a tier with no published Profile does not compile: a
// config built on any of those would be quietly wrong on the fleet, which is
// worse than a build that stops.
func Compile(c contract.Contract, taxonomy *guardrail.Taxonomy, profiles *ProfileSet) (CollectorConfig, error) {
	mandatory, known := taxonomy.MandatorySignals(c.Tier)
	if !known {
		return CollectorConfig{}, fmt.Errorf(
			"cannot compile %s: Service Tier %q is not in the Service Tier Taxonomy, so there is no telemetry floor to compile against (declare one of %v)",
			c.ServiceName, c.Tier, taxonomy.Tiers())
	}

	profile, published := profiles.For(c.Tier)
	if !published {
		return CollectorConfig{}, fmt.Errorf(
			"cannot compile %s: no Pipeline Profile is published for Service Tier %q, so how its telemetry ships is undecided (profiled tiers: %v)",
			c.ServiceName, c.Tier, sorted(profiles.ProfiledTiers()))
	}

	declared, err := signalsOf(c)
	if err != nil {
		return CollectorConfig{}, fmt.Errorf("cannot compile %s: %w", c.ServiceName, err)
	}

	// The compiled config must route every Signal the tier mandates. A pipeline
	// missing one would mean the fleet quietly not collecting telemetry the org
	// requires — and the Contract would still read as compliant.
	if missing := missingFrom(declared, mandatory); len(missing) > 0 {
		return CollectorConfig{}, fmt.Errorf(
			"cannot compile %s: Service Tier %s mandates the %s Signal(s), which the Telemetry Contract does not declare",
			c.ServiceName, c.Tier, strings.Join(missing, ", "))
	}

	return assemble(c, profile, declared), nil
}

// CompileGateway turns the org's Gateway Declaration into collector configuration
// for the shared Gateway (ADR 0013).
//
// It takes the Pipeline Profiles too, and not for anything it copies out of them:
// the Profiles are the other half of the same topology. They tell every Agent in
// the fleet where to forward, this tells the Gateway where to listen, and the two
// living in separate files is exactly how they come to disagree. So compiling
// cross-checks them, and refuses rather than emitting a Gateway that answers
// somewhere no Agent is sending.
func CompileGateway(declaration *GatewayDeclaration, profiles *ProfileSet) (CollectorConfig, error) {
	// The Gateway's OTLP receiver is derived from this address's port, so an address
	// that is not one compiles a Gateway that never starts — and the fleet, not the
	// person who edited the file, is who finds out.
	if err := validAddress(declaration.Address); err != nil {
		return CollectorConfig{}, fmt.Errorf("cannot compile the Gateway: %w", err)
	}

	for _, tier := range sorted(profiles.ProfiledTiers()) {
		profile, _ := profiles.For(tier)
		if profile.GatewayEndpoint != declaration.Address {
			return CollectorConfig{}, fmt.Errorf(
				"cannot compile the Gateway: Pipeline Profile %q forwards Service Tier %s to %s, but the Gateway answers on %s — telemetry sent there would be dropped, so fix one side or the other",
				profile.Name, tier, profile.GatewayEndpoint, declaration.Address)
		}
	}

	// A Gateway with no Backend receives the whole fleet's telemetry and drops it,
	// and nothing about that is visible from a service — every Agent's export
	// succeeds. So it is refused here, where it was written.
	if len(declaration.Backends) == 0 {
		return CollectorConfig{}, fmt.Errorf(
			"cannot compile the Gateway: it names no Backend, so the fleet's telemetry would arrive and stop there (declare one under `backends:`)")
	}

	// Fan-out to several Backends is C5 (#13). Until then the extra ones are named
	// and refused rather than quietly ignored: exporting to the first of two
	// declared Backends is how an org discovers mid-migration that half its
	// telemetry never left the Gateway.
	if len(declaration.Backends) > 1 {
		return CollectorConfig{}, fmt.Errorf(
			"cannot compile the Gateway: it declares %d Backends (%s), and fanning out to more than one is not built yet (C5, #13) — one of them would receive nothing",
			len(declaration.Backends), strings.Join(backendNames(declaration.Backends[1:]), ", "))
	}

	// A Backend is a name and an endpoint. Missing either compiles an exporter that
	// is unnameable or unreachable, and both are found out only once the Gateway is
	// already holding the fleet's telemetry.
	for _, backend := range declaration.Backends {
		if backend.Name == "" {
			return CollectorConfig{}, fmt.Errorf(
				"cannot compile the Gateway: a Backend has no name, so its exporter could not be named (set `backend:` on each)")
		}
		if backend.Endpoint == "" {
			return CollectorConfig{}, fmt.Errorf(
				"cannot compile the Gateway: Backend %q names no endpoint, so the Gateway would have nowhere to export to", backend.Name)
		}
	}

	return assembleGateway(*declaration), nil
}

// validAddress checks the Gateway's address is a host and port an Agent could
// dial and a receiver could bind the port of — not a URL, and not a bare hostname.
func validAddress(address string) error {
	if address == "" {
		return fmt.Errorf("it declares no address, so neither the port it listens on nor where Agents forward is decided (set `address: host:port`)")
	}

	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("its address %q is not a host and port: %w", address, err)
	}
	if _, err := strconv.Atoi(port); err != nil {
		return fmt.Errorf("its address %q names port %q, which is not a number", address, port)
	}
	// An OTLP endpoint is host:port, not a URL. A scheme or path here compiles an
	// exporter every Agent fails to dial.
	if host == "" || strings.ContainsAny(host, "/") {
		return fmt.Errorf("its address %q is a URL, not a host and port — write `host:port`", address)
	}
	return nil
}

// backendNames is the Backends by name, for an error that says which ones.
func backendNames(backends []Backend) []string {
	names := make([]string, 0, len(backends))
	for _, backend := range backends {
		names = append(names, backend.Name)
	}
	return names
}

// signalsOf is the Contract's declared Signals, rejecting anything that is not
// one. A typo like `metricks` would otherwise compile to a pipeline for a Signal
// no collector has ever heard of.
func signalsOf(c contract.Contract) ([]contract.Signal, error) {
	seen := map[contract.Signal]bool{}
	signals := make([]contract.Signal, 0, len(c.Signals))

	for _, declared := range c.Signals {
		signal, err := contract.ParseSignal(declared)
		if err != nil {
			return nil, err
		}
		if seen[signal] {
			continue
		}
		seen[signal] = true
		signals = append(signals, signal)
	}
	return signals, nil
}

// missingFrom is the mandatory Signals the Contract does not declare.
func missingFrom(declared []contract.Signal, mandatory []string) []string {
	has := map[string]bool{}
	for _, signal := range declared {
		has[string(signal)] = true
	}

	var missing []string
	for _, signal := range mandatory {
		if !has[signal] {
			missing = append(missing, signal)
		}
	}
	sort.Strings(missing)
	return missing
}

func sorted(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}
