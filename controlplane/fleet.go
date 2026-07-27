package controlplane

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/ghantakiran/OTEL/contract"
	"github.com/ghantakiran/OTEL/guardrail"
)

// The Fleet directory layout. One file per service under contracts/, one
// compiled collector configuration per service under compiled/.
const (
	fleetContractsDir = "contracts"
	fleetCompiledDir  = "compiled"
	fleetManifestFile = "rollout-manifest.yaml"

	// contractExtension is the one extension a Telemetry Contract in the Fleet uses.
	contractExtension = ".yaml"

	// manifestKind is what a Rollout Manifest declares itself to be, in the same
	// apiVersion/kind family as a Telemetry Contract and a Pipeline Profile set.
	manifestKind = "RolloutManifest"
)

// Fleet is the set of services whose Telemetry Contracts the Control Plane
// compiles together — the contracts/ directory of a config repo.
type Fleet struct {
	root      string
	contracts []fleetEntry
}

// fleetEntry is one file in the Fleet's contracts/, with whatever went wrong
// reading it. A file that will not load is kept as an entry rather than dropped:
// contracts/ aggregates files from many service repos, so one arriving malformed
// must cost that service its rollout and nobody else's — and an entry is also
// what keeps its last collector configuration from being pruned.
type fleetEntry struct {
	name     string // the file name without its extension
	path     string // relative to the Fleet root, e.g. contracts/checkout-api.yaml
	declared contract.Contract
	loadErr  error
}

// LoadFleet reads every Telemetry Contract in a Fleet directory's contracts/.
//
// A Contract is filed under its own service name: contracts/checkout-api.yaml
// declares service_name checkout-api and compiles to compiled/checkout-api.yaml.
// The file name is the key the layout turns on, so a file whose name disagrees
// with the Contract inside it is a reported failure, not a guess.
func LoadFleet(root string) (*Fleet, error) {
	// Both extensions are collected, but only .yaml is a Contract. A file named
	// .yml would otherwise be ignored in silence — never compiled, absent from the
	// Rollout Manifest, and the service simply left with no collector configuration
	// and nobody told why. It is the near miss that will actually happen, so it is
	// reported rather than skipped.
	files, err := filepath.Glob(filepath.Join(root, fleetContractsDir, "*.y*ml"))
	if err != nil {
		return nil, fmt.Errorf("read the Fleet at %s: %w", root, err)
	}
	sort.Strings(files)

	// A mistyped path, or a checkout that never fetched the Contracts, would read
	// as "the Fleet is empty" — and the next step would prune every collector
	// configuration in the repo and commit it. That is a fleet-wide outage arriving
	// as a green run, so an empty Fleet is refused here, before anything is written.
	if len(files) == 0 {
		return nil, fmt.Errorf(
			"the Fleet at %s has no Telemetry Contracts: expected one or more %s/*.yaml, each named for the service it declares",
			root, fleetContractsDir)
	}

	fleet := &Fleet{root: root}
	for _, file := range files {
		entry := fleetEntry{
			name: stem(file),
			path: path.Join(fleetContractsDir, filepath.Base(file)),
		}
		if filepath.Ext(file) != contractExtension {
			entry.loadErr = fmt.Errorf(
				"%s is not named %s%s: a Contract in the Fleet is filed as <service-name>%s, so this one is not being compiled at all",
				entry.path, entry.name, contractExtension, contractExtension)
			fleet.contracts = append(fleet.contracts, entry)
			continue
		}

		entry.declared, entry.loadErr = contract.Load(file)
		if entry.loadErr == nil && entry.declared.ServiceName != entry.name {
			entry.loadErr = fmt.Errorf(
				"%s declares service_name %q: a Contract in the Fleet is filed under its own service name, so this belongs at %s/%s.yaml",
				entry.path, entry.declared.ServiceName, fleetContractsDir, entry.declared.ServiceName)
		}
		fleet.contracts = append(fleet.contracts, entry)
	}
	return fleet, nil
}

// Rollout is the outcome of compiling a Fleet: every service in it, in one of
// exactly two states.
//
// A Contract that does not compile is reported and skipped rather than failing
// the whole Fleet. One team naming a Signal `metricks` must not freeze every
// other team's rollout — the blast radius of a broken Contract is the service
// that broke it. What makes that safe rather than silent is that the skip is
// recorded in the committed Rollout Manifest and the command exits non-zero, so
// the partial state is in the diff a human reviews before anything merges.
type Rollout struct {
	Compiled []CompiledService
	Failed   []FailedService

	root string
	// accounted is every compiled/ file some Contract in the Fleet answers for,
	// whether or not it compiled. Anything else under compiled/ belongs to a
	// service that has left the Fleet, and is pruned.
	accounted map[string]bool
}

// FailedService is one Telemetry Contract in the Fleet that did not compile, and
// why. Its previous collector configuration, if it had one, is left exactly as it
// was: a service whose Contract was just broken should keep collecting telemetry
// under the last configuration that did compile.
type FailedService struct {
	ContractPath string // relative to the Fleet root
	ServiceName  string
	Tier         string
	Reason       string
}

// CompiledService is one service's compiled collector configuration and where it
// lands in the Fleet.
type CompiledService struct {
	ServiceName string
	Tier        string
	Profile     string   // the Pipeline Profile its Service Tier selected
	Signals     []string // the Signals its collector configuration routes
	Path        string   // relative to the Fleet root
	Digest      string   // sha256 of the file at Path, as sha256:<hex>
	// ConfigVersion is what this service's Agent will report about itself once this
	// Rollout has reached it — the value an operator waits to see fleet-wide, since
	// there is no status back-channel to ask (ADR 0010).
	//
	// It is NOT the Digest and the two are never equal. The Digest hashes this file,
	// header and stamp included, and answers "is the file in the repo still the one
	// this Rollout wrote?". The ConfigVersion hashes the collector configuration
	// with its own stamp excluded, and answers "is the pipeline running out there
	// the one this Rollout compiled?". Only the second can be compared against
	// telemetry; comparing the first to a version seen in a Backend is a category
	// error. See ADR 0016.
	ConfigVersion string

	contents []byte
}

// Writeback is what writing a Rollout did to the Fleet directory. A file already
// byte-identical is reported Unchanged and not touched at all, so a Rollout that
// changes nothing is visibly a no-op rather than a run that claims to have
// rewritten the whole fleet.
type Writeback struct {
	Wrote     []string
	Unchanged []string
	Pruned    []string
}

// Changed reports whether writing this Rollout altered the Fleet directory.
func (w Writeback) Changed() bool { return len(w.Wrote) > 0 || len(w.Pruned) > 0 }

// CompileFleet Compiles every Telemetry Contract in the Fleet.
func CompileFleet(fleet *Fleet, taxonomy *guardrail.Taxonomy, profiles *ProfileSet) (Rollout, error) {
	rollout := Rollout{root: fleet.root, accounted: map[string]bool{}}

	for _, entry := range fleet.contracts {
		// Accounted for before it is compiled, so that a Contract which fails keeps
		// its last collector configuration instead of having it pruned.
		rollout.accounted[entry.compiledPath()] = true

		if entry.loadErr != nil {
			rollout.Failed = append(rollout.Failed, entry.failure(entry.loadErr))
			continue
		}

		config, err := Compile(entry.declared, taxonomy, profiles)
		if err != nil {
			rollout.Failed = append(rollout.Failed, entry.failure(err))
			continue
		}
		// An incoherent compiled config is a bug in the compiler or in a Pipeline
		// Profile, never in one service's Contract — and a Profile is fleet-wide, so
		// it would be wrong for every service that selects it. That refuses the whole
		// Rollout rather than landing in not_compiled blaming the service.
		if err := config.Validate(); err != nil {
			return Rollout{}, fmt.Errorf(
				"the config compiled for %s is not coherent, which is a bug in the compiler or the Pipeline Profiles rather than in the Telemetry Contract: %w",
				entry.declared.ServiceName, err)
		}

		rendered, err := config.YAML()
		if err != nil {
			return Rollout{}, err
		}

		// Compile has already refused a tier with no Profile, so this is published.
		profile, _ := profiles.For(entry.declared.Tier)
		contents := append(generatedHeader(entry, profile.Name), rendered...)
		rollout.Compiled = append(rollout.Compiled, CompiledService{
			ServiceName:   entry.declared.ServiceName,
			Tier:          entry.declared.Tier,
			Profile:       profile.Name,
			Signals:       config.Signals(),
			Path:          entry.compiledPath(),
			Digest:        digestOf(contents),
			ConfigVersion: config.ConfigVersion(),
			contents:      contents,
		})
	}
	return rollout, nil
}

// Write puts the Rollout into the Fleet directory: one collector configuration
// per service that compiled, plus the Rollout Manifest.
func (r Rollout) Write() (Writeback, error) {
	var back Writeback

	if err := os.MkdirAll(filepath.Join(r.root, fleetCompiledDir), 0o755); err != nil {
		return Writeback{}, fmt.Errorf("make the compiled directory: %w", err)
	}

	manifest, err := r.ManifestYAML()
	if err != nil {
		return Writeback{}, err
	}

	files := map[string][]byte{fleetManifestFile: manifest}
	for _, service := range r.Compiled {
		files[service.Path] = service.contents
	}
	for _, file := range sortedPaths(files) {
		absolute := filepath.Join(r.root, file)
		if existing, err := os.ReadFile(absolute); err == nil && bytes.Equal(existing, files[file]) {
			back.Unchanged = append(back.Unchanged, file)
			continue
		}
		if err := os.WriteFile(absolute, files[file], 0o644); err != nil {
			return Writeback{}, fmt.Errorf("write %s: %w", file, err)
		}
		back.Wrote = append(back.Wrote, file)
	}

	pruned, err := r.prune()
	if err != nil {
		return Writeback{}, err
	}
	back.Pruned = pruned
	return back, nil
}

// prune removes collector configuration for services that have left the Fleet.
//
// The compiled tree is a statement of what the Fleet runs, so a decommissioned
// service's config cannot linger: GitOps tooling would keep applying it, with no
// idea the Telemetry Contract is gone.
func (r Rollout) prune() ([]string, error) {
	existing, err := filepath.Glob(filepath.Join(r.root, fleetCompiledDir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read the compiled directory: %w", err)
	}
	sort.Strings(existing)

	var pruned []string
	for _, file := range existing {
		relative := path.Join(fleetCompiledDir, filepath.Base(file))
		if r.accounted[relative] {
			continue
		}
		if err := os.Remove(file); err != nil {
			return nil, fmt.Errorf("prune %s: %w", relative, err)
		}
		pruned = append(pruned, relative)
	}
	return pruned, nil
}

// ManifestYAML renders the Rollout Manifest: the committed index of this Compile.
func (r Rollout) ManifestYAML() ([]byte, error) {
	document := manifestDocument{
		APIVersion: contract.APIVersion,
		Kind:       manifestKind,
		// Never nil: an empty list reads as "nothing failed", where a missing key
		// reads as "this Manifest predates the idea of failure".
		Compiled:    []manifestCompiled{},
		NotCompiled: []manifestNotCompiled{},
	}
	for _, service := range r.Compiled {
		document.Compiled = append(document.Compiled, manifestCompiled{
			ServiceName:   service.ServiceName,
			Tier:          service.Tier,
			Profile:       service.Profile,
			Signals:       service.Signals,
			Config:        service.Path,
			Digest:        service.Digest,
			ConfigVersion: service.ConfigVersion,
		})
	}
	for _, service := range r.Failed {
		document.NotCompiled = append(document.NotCompiled, manifestNotCompiled{
			ServiceName: service.ServiceName,
			Contract:    service.ContractPath,
			Tier:        service.Tier,
			Reason:      service.Reason,
		})
	}

	body, err := yaml.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("render the Rollout Manifest: %w", err)
	}
	return append([]byte(manifestHeader), body...), nil
}

// manifestDocument is the Rollout Manifest on disk. Struct fields marshal in
// declaration order, so the file's shape is fixed rather than a map's whim.
type manifestDocument struct {
	APIVersion  string                `yaml:"apiVersion"`
	Kind        string                `yaml:"kind"`
	Compiled    []manifestCompiled    `yaml:"compiled"`
	NotCompiled []manifestNotCompiled `yaml:"not_compiled"`
}

type manifestCompiled struct {
	ServiceName   string   `yaml:"service_name"`
	Tier          string   `yaml:"tier"`
	Profile       string   `yaml:"pipeline_profile"`
	Signals       []string `yaml:"signals"`
	Config        string   `yaml:"collector_config"`
	Digest        string   `yaml:"digest"`
	ConfigVersion string   `yaml:"config_version"`
}

type manifestNotCompiled struct {
	ServiceName string `yaml:"service_name"`
	Contract    string `yaml:"telemetry_contract"`
	Tier        string `yaml:"tier"`
	Reason      string `yaml:"reason"`
}

const manifestHeader = `# The Rollout Manifest: what the last fleet-wide Compile produced.
#
# Generated by 'otel-guardrail compile-fleet'; do not edit this file. It exists so
# that a partial rollout is reviewable. A git diff shows what changed and never
# what is absent, so a service missing from compiled/ would otherwise be invisible
# — here it appears under not_compiled with the reason, and the command that wrote
# this exited non-zero.
#
# A service under not_compiled keeps whatever collector configuration it last
# compiled to, if any: a broken Contract must not take a running service's
# telemetry away.
#
# THE TWO HASHES BELOW ARE NOT INTERCHANGEABLE, and only one of them is a thing
# to look for in telemetry:
#
#   digest          sha256 of the file named on the line above it, header and
#                   config_version included. It answers "is that file in this
#                   repo still the one this Rollout wrote?" — so a hand-edited
#                   config_version is caught by it, and nothing else is.
#   config_version  what that service's Agent reports about itself once this
#                   Rollout reaches it. It is the sha256 of the collector
#                   configuration with its own stamp excluded, so it identifies
#                   the running pipeline rather than the file. THIS is the value
#                   an operator waits to see fleet-wide; a Rollout is confirmed
#                   when every service here reports the version written here, and
#                   there is no status channel to ask instead (ADR 0010, 0016).
#
# They are never equal — the digest hashes a strict superset of what the version
# hashes — so a value that matches both is a bug, not a coincidence.
`

// compiledPath is where this entry's collector configuration lands, derived from
// the file name rather than from the service_name inside the file. That is what
// makes a Contract which no longer compiles keep its last configuration, while a
// Contract that has been deleted loses it.
func (e fleetEntry) compiledPath() string {
	return path.Join(fleetCompiledDir, e.name+contractExtension)
}

// failure is this entry recorded as a service that did not compile.
func (e fleetEntry) failure(reason error) FailedService {
	return FailedService{
		ContractPath: e.path,
		ServiceName:  e.declared.ServiceName,
		Tier:         e.declared.Tier,
		Reason:       reason.Error(),
	}
}

// generatedHeader is the preamble on every compiled collector configuration.
//
// These files land in a repo, and somebody will eventually find one and want to
// change a batch timeout in it. That edit would work until the next rollout
// silently reverted it, so the file names both of its real inputs — the Telemetry
// Contract for what the service emits, the Pipeline Profile for how it ships.
//
// The Contract is named by its path inside the Fleet, never on disk: an absolute
// path would differ between a laptop and a CI runner, making every rollout diff
// noise, and the only path a reader can act on is the one in the repo.
func generatedHeader(entry fleetEntry, profile string) []byte {
	// Each input on its own line rather than wrapped into a sentence: a long service
	// name would otherwise reflow the paragraph, and a reflowed paragraph is a diff.
	return []byte(fmt.Sprintf(`# Compiled collector configuration for the %s Agent — Service Tier %s.
#
# Generated by 'otel-guardrail compile-fleet'; do not edit this file, the next
# rollout overwrites it. Change one of its two inputs instead:
#
#   Telemetry Contract: %s — what this service emits
#   Pipeline Profile: %s — how that telemetry ships
`, entry.declared.ServiceName, entry.declared.Tier, entry.path, profile))
}

// digestOf is a file's content hash, as recorded in the Rollout Manifest, so a
// reader can tell whether the file on disk is still the one this Manifest indexed.
func digestOf(contents []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(contents))
}

// sortedPaths is the files to write, in a fixed order. Ranging a map directly
// would write them in a different order each run, which is invisible in the result
// but would make the Writeback lists — and so the CLI's report — reshuffle.
func sortedPaths(files map[string][]byte) []string {
	paths := make([]string, 0, len(files))
	for file := range files {
		paths = append(paths, file)
	}
	sort.Strings(paths)
	return paths
}

// stem is a file's name without its directory or extension.
func stem(file string) string {
	base := filepath.Base(file)
	return base[:len(base)-len(filepath.Ext(base))]
}
