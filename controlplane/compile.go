package controlplane

import (
	"fmt"
	"net"
	"path"
	"regexp"
	"slices"
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

	// The platform's namespace is what the platform says about ITSELF, and
	// `otel.platform.config_version` is the single field an operator reads to decide
	// whether a Rollout has landed. A Contract declaring anything under it would
	// compile to a resource processor upserting it onto every record the service
	// emits — a service answering, for itself, the one question there is no status
	// channel to ask (ADR 0010). Refused here, where it was written, rather than
	// stripped later where the declaration would still read as honoured.
	for _, attribute := range sorted(attributeNamesOf(c)) {
		if inThePlatformNamespace(attribute) {
			return CollectorConfig{}, fmt.Errorf(
				"cannot compile %s: its Telemetry Contract declares the resource attribute %q, and the %s namespace is what the platform says about its own collectors — a service stamping it would be answering the question a Rollout is confirmed by (drop it from `resource_attributes:`)",
				c.ServiceName, attribute, platformNamespace)
		}
	}

	// The compiled config must route every Signal the tier mandates. A pipeline
	// missing one would mean the fleet quietly not collecting telemetry the org
	// requires — and the Contract would still read as compliant.
	if missing := missingFrom(declared, mandatory); len(missing) > 0 {
		return CollectorConfig{}, fmt.Errorf(
			"cannot compile %s: Service Tier %s mandates the %s Signal(s), which the Telemetry Contract does not declare",
			c.ServiceName, c.Tier, strings.Join(missing, ", "))
	}

	return assemble(c, profile, declared)
}

// CompileGateway turns the org's Gateway Declaration into collector configuration
// for the shared Gateway (ADR 0013).
//
// It takes the Standard catalog because the Gateway is where Pipeline Guardrails
// run. Every Standard the catalog marks `enforced_at: [pipeline]` compiles into
// collector processors here — from the same catalog entry the Rego reads at
// Preflight, so one Standard has one definition and the two enforcement points
// cannot drift apart (ADR 0015). A Standard that declares `pipeline` and cannot
// be expressed as processors is refused when the catalog is read, not weakened
// here.
//
// It takes the Pipeline Profiles too, and not for anything it copies out of them:
// the Profiles are the other half of the same topology. They tell every Agent in
// the fleet where to forward, this tells the Gateway where to listen, and the two
// living in separate files is exactly how they come to disagree. So compiling
// cross-checks them, and refuses rather than emitting a Gateway that answers
// somewhere no Agent is sending.
func CompileGateway(declaration *GatewayDeclaration, profiles *ProfileSet, standards *guardrail.StandardCatalog) (CollectorConfig, error) {
	// A catalog that is absent has no pipeline-enforced Standards, which is exactly
	// what a catalog that enforces none looks like — so omitting it would compile a
	// Gateway that runs no Guardrail at all, silently. "The org enforces nothing at
	// the pipeline" has to be something written in a document, never something a
	// caller forgot.
	if standards == nil {
		return CollectorConfig{}, fmt.Errorf(
			"cannot compile the Gateway: no Standard catalog was given, so it is not decided which Standards it enforces at the pipeline — an empty catalog says none, an absent one says nothing")
	}

	// Every pipeline-enforced Standard must compile to a condition with something
	// in it. It is unreachable today — the catalog refuses an empty
	// `resource_attributes:`, and only that requirement kind may declare `pipeline`
	// — but it is unreachable because of two guards in another package, which is
	// not the same as being checked. Adding a requirement kind to the expressible
	// set without teaching the compiler to build its condition would otherwise emit
	// `set(...) where ` with a dangling clause: a Gateway that fails at load, on a
	// rollout, for the whole fleet. Same reason Validate re-checks the spill
	// storage that assembly already keeps true.
	for _, standard := range standards.PipelineEnforced() {
		if len(standard.Requires.ResourceAttributes) == 0 {
			return CollectorConfig{}, fmt.Errorf(
				"cannot compile the Gateway: Standard %s is enforced at the pipeline but compiles to no condition, so it would emit a processor statement the collector cannot parse",
				standard.ID)
		}
	}

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

	// A Backend is a name and an endpoint. Missing either compiles an exporter that
	// is unnameable or unreachable, and both are found out only once the Gateway is
	// already holding the fleet's telemetry.
	declaredOnce := map[string]bool{}
	for _, backend := range declaration.Backends {
		if backend.Name == "" {
			return CollectorConfig{}, fmt.Errorf(
				"cannot compile the Gateway: a Backend has no name, so its exporter could not be named (set `backend:` on each)")
		}
		if backend.Endpoint == "" {
			return CollectorConfig{}, fmt.Errorf(
				"cannot compile the Gateway: Backend %q names no endpoint, so the Gateway would have nowhere to export to", backend.Name)
		}
		// The name is not a label. It becomes a collector component ID and a path
		// segment under spill_root, and both of those normalise — a component ID is
		// trimmed, a path is cleaned. So two names that differ to the duplicate check
		// below can still collapse into one exporter or one spill directory, which
		// would make ADR 0014's "unrepresentable" merely "unlikely": `./a` and `a`
		// clean to the same directory, `../x` escapes spill_root altogether, and
		// " primary-apm" is a distinct string here that the collector trims into an
		// existing exporter there.
		if err := validBackendName(backend.Name); err != nil {
			return CollectorConfig{}, fmt.Errorf("cannot compile the Gateway: %w", err)
		}
		// A Backend's name is its identity in the compiled config — its exporter is
		// otlp/<name>, its spill lives under <name> — so two of them sharing a name
		// collapse into one component, and the map that swallowed the first shows no
		// trace of it.
		if declaredOnce[backend.Name] {
			return CollectorConfig{}, fmt.Errorf(
				"cannot compile the Gateway: Backend %q is declared twice, and the second would silently replace the first", backend.Name)
		}
		declaredOnce[backend.Name] = true
		// Spill needs somewhere to spill to, and that somewhere is an absolute path.
		// A root that is empty OR relative both resolve against wherever the Gateway
		// process happened to be started — inside the container image, on no mounted
		// volume, gone on the next restart — so refusing only the empty string would
		// leave the same failure reachable by a shorter route. A spill volume is
		// mounted at an absolute path or it is not mounted.
		if backend.Delivery.Spill && !path.IsAbs(declaration.SpillRoot) {
			return CollectorConfig{}, fmt.Errorf(
				"cannot compile the Gateway: Backend %q asks to spill but the Gateway's `spill_root:` is %q, which is not an absolute path — its queue would be written relative to the Gateway's working directory, on no mounted volume, and lost on restart",
				backend.Name, declaration.SpillRoot)
		}
		// Spill is where the sending queue is kept, not a mechanism beside it. With no
		// queue there is nothing to keep, and the compiled config would carry a storage
		// extension, a directory and a mounted volume that nothing ever writes to.
		if backend.Delivery.Spill && backend.Delivery.QueueSize == 0 {
			return CollectorConfig{}, fmt.Errorf(
				"cannot compile the Gateway: Backend %q asks to spill but declares no `queue_size:`, and spill is where the sending queue is kept — with no queue there is nothing to spill", backend.Name)
		}
		// Omitting `signals:` means every Signal. Writing `signals: []` is somebody
		// saying something, and the only thing it can mean is "none" — a Backend that
		// does nothing. Letting both fall through one length check would read the
		// second as its exact opposite, which is the widest gap there is between what
		// was written and what runs.
		if backend.Signals != nil && len(backend.Signals) == 0 {
			return CollectorConfig{}, fmt.Errorf(
				"cannot compile the Gateway: Backend %q declares an empty `signals: []`, so it would receive nothing — remove the line to give it every Signal, or list the ones it takes", backend.Name)
		}
		// A Signal typo matches no pipeline, so the Backend simply receives nothing
		// while every export elsewhere still succeeds. The same reason Compile rejects
		// a Contract's Signal typo rather than compiling a pipeline for it.
		for _, declared := range backend.Signals {
			if _, err := contract.ParseSignal(declared); err != nil {
				return CollectorConfig{}, fmt.Errorf(
					"cannot compile the Gateway: Backend %q declares %w, so it would receive nothing", backend.Name, err)
			}
		}
	}

	if err := validSelfTelemetry(declaration, standards); err != nil {
		return CollectorConfig{}, fmt.Errorf("cannot compile the Gateway: %w", err)
	}

	// Every Agent forwards every Signal it collects here, so a Signal no Backend
	// takes is a pipeline that receives the fleet's telemetry and exports it
	// nowhere. It is the no-Backend failure one Signal at a time, and just as
	// invisible from a service: the Agent's export still succeeds.
	if orphaned := signalsNoBackendReceives(declaration.Backends); len(orphaned) > 0 {
		return CollectorConfig{}, fmt.Errorf(
			"cannot compile the Gateway: no Backend receives the %s Signal(s), so they would arrive and stop there (add one, or widen a Backend's `signals:`)",
			strings.Join(orphaned, ", "))
	}

	return assembleGateway(*declaration, standards)
}

// validSelfTelemetry checks the Gateway can be seen at all, and that what it says
// about itself is something it would accept from anybody else.
//
// Every refusal here is a Gateway that would run perfectly and be invisible, or
// visible and wrong. There is no status endpoint to fall back on and none is
// going to be built (ADR 0010), so this is the only place any of it is caught.
func validSelfTelemetry(declaration *GatewayDeclaration, standards *guardrail.StandardCatalog) error {
	self := declaration.SelfTelemetry

	// A Gateway that reports nothing about itself looks exactly like a Gateway that
	// is healthy: no config_version to confirm a Rollout against, and no queue depth
	// when a Backend stops answering. The whole fleet's telemetry goes through this
	// one process.
	if self.Backend == "" {
		return fmt.Errorf(
			"it declares no `self_telemetry:` backend, so the Gateway would emit no telemetry about itself — no config_version to confirm a Rollout by and no queue depth when a Backend stops answering, and there is no status channel to ask instead (ADR 0010)")
	}

	// Its own signals go straight to this Backend, so a name that is not one is a
	// Gateway exporting its own telemetry into nothing — and nothing about that is
	// visible from anywhere, because the thing that would have reported it is the
	// thing that is lost.
	receiver, declared := backendNamed(declaration.Backends, self.Backend)
	if !declared {
		return fmt.Errorf(
			"its `self_telemetry:` sends the Gateway's own signals to Backend %q, which it does not declare — they would be exported into nothing, and the telemetry that would have said so is the telemetry being lost (declared: %v)",
			self.Backend, backendNames(declaration.Backends))
	}
	// The platform's own signals are metrics. Sending them to a Backend that
	// declares it does not receive metrics produces export failures that look
	// exactly like that Backend being down — the failure mode a per-Signal fan-out
	// exists to remove, reintroduced on the one stream that reports on the rest.
	if !receiver.Receives(contract.SignalMetrics) {
		return fmt.Errorf(
			"its `self_telemetry:` sends the Gateway's own signals to Backend %q, which declares `signals: %v` and so does not receive metrics — the platform's own signals are metrics, and exporting them there would fail in a way indistinguishable from that Backend being down",
			self.Backend, receiver.Signals)
	}

	// A Gateway that says nothing about who it is emits telemetry under whatever the
	// collector defaults to — `service.name: otelcol` — so every Gateway on every
	// platform looks the same in a Backend and none of them can be found.
	//
	// Refused on its own rather than left to the Standards check below, because that
	// check borrows its strictness from the catalog: a catalog whose pipeline
	// Standards all happen to be `warn` would have nothing to say, and the identity
	// would go missing exactly when nothing was watching. It is the empty-catalog
	// disarm ADR 0015 refuses, one layer further out.
	if len(self.ResourceAttributes) == 0 {
		return fmt.Errorf(
			"its `self_telemetry:` declares no `resource_attributes:`, so the Gateway's own telemetry would arrive under whatever the collector defaults to and could not be told from any other collector's — the Gateway has no Telemetry Contract, so this is where it says who it is")
	}

	// The Gateway has no Telemetry Contract, so nothing else checks who it says it
	// is. Every `block` Standard it tags the fleet with, it has to satisfy itself: a
	// Gateway marking a service's telemetry blocking for a missing attribute while
	// its own telemetry omits the same attribute is the platform exempting itself
	// from the rule it enforces, and it would never show up anywhere.
	//
	// Only `block`, and for the same reason only `block` fails a build at Preflight
	// (ADR 0003): a `warn` Standard is advice, and refusing to compile over advice
	// would make Severity mean something different here than everywhere else.
	for _, standard := range standards.PipelineEnforced() {
		if standard.Severity != guardrail.SeverityBlock {
			continue
		}
		for _, attribute := range standard.Requires.ResourceAttributes {
			if self.ResourceAttributes[attribute] == "" {
				return fmt.Errorf(
					"its `self_telemetry.resource_attributes:` omits %q, which Standard %s requires at the pipeline and tags as %s — the Gateway would mark every service's telemetry for an attribute its own telemetry does not carry",
					attribute, standard.ID, standard.Severity)
			}
		}
	}

	// The same namespace a Telemetry Contract may not claim, refused for the same
	// reason: `otel.platform.config_version` is derived from the compiled config,
	// and a declaration setting it by hand would have the Gateway announce a
	// configuration it is not running.
	for _, attribute := range sorted(keysOf(self.ResourceAttributes)) {
		if inThePlatformNamespace(attribute) {
			return fmt.Errorf(
				"its `self_telemetry.resource_attributes:` declares %q, and the %s namespace is derived from the compiled configuration rather than written — a declared one would have the Gateway announce a configuration it is not running",
				attribute, platformNamespace)
		}
	}
	return nil
}

// backendNamed is the declared Backend with a given name.
func backendNamed(backends []Backend, name string) (Backend, bool) {
	for _, backend := range backends {
		if backend.Name == name {
			return backend, true
		}
	}
	return Backend{}, false
}

// backendNames is every declared Backend's name, for an error that has to say
// what the caller could have written instead.
func backendNames(backends []Backend) []string {
	names := make([]string, 0, len(backends))
	for _, backend := range backends {
		names = append(names, backend.Name)
	}
	return names
}

// keysOf is a map's keys, unordered.
func keysOf(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

// signalsNoBackendReceives is every Signal that no declared Backend takes.
func signalsNoBackendReceives(backends []Backend) []string {
	var orphaned []string
	for _, signal := range contract.Signals() {
		if !slices.ContainsFunc(backends, func(b Backend) bool { return b.Receives(signal) }) {
			orphaned = append(orphaned, string(signal))
		}
	}
	return orphaned
}

// backendName is what a Backend may be called: lower-case alphanumerics and
// hyphens, starting and ending with an alphanumeric. Deliberately narrower than
// either a collector component ID or a directory name, because the name has to be
// safely both — and because it is a role the org names once (`primary-apm`,
// `cold-archive`), not a free-text field.
var backendName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func validBackendName(name string) error {
	if !backendName.MatchString(name) {
		return fmt.Errorf(
			"Backend name %q is not a role name — it becomes both a collector component ID and a directory under `spill_root:`, so it must be lower-case letters, digits and hyphens, starting and ending with a letter or digit (like `primary-apm`)",
			name)
	}
	return nil
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

// attributeNamesOf is every resource attribute key a Contract declares.
func attributeNamesOf(c contract.Contract) []string {
	names := make([]string, 0, len(c.ResourceAttributes))
	for name := range c.ResourceAttributes {
		names = append(names, name)
	}
	return names
}

func sorted(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}
