package controlplane_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/ghantakiran/OTEL/contract"
	"github.com/ghantakiran/OTEL/controlplane"
	"github.com/ghantakiran/OTEL/guardrail"
)

func TestAFleetCompilesEveryTelemetryContractInIt(t *testing.T) {
	root := fleetWith(t, map[string]string{
		"checkout-api":  contractYAML("checkout-api", "tier-1", "traces", "metrics", "logs"),
		"payments-edge": contractYAML("payments-edge", "tier-1", "traces", "metrics", "logs"),
	})

	rollout := compileFleet(t, root)

	if len(rollout.Compiled) != 2 {
		t.Fatalf("compiled %v, want checkout-api and payments-edge", compiledNames(rollout))
	}
	if _, err := rollout.Write(); err != nil {
		t.Fatalf("write the rollout: %v", err)
	}
	for _, service := range []string{"checkout-api", "payments-edge"} {
		path := filepath.Join(root, "compiled", service+".yaml")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("no collector configuration at compiled/%s.yaml: %v", service, err)
		}
	}
}

func TestCompilingTheFleetTwiceProducesIdenticalFiles(t *testing.T) {
	// The whole review model rests on this. A rollout is reviewed as a git diff,
	// and a diff carrying reshuffled map keys, a timestamp or a build path buries
	// the one line that actually changed — at which point nobody reads it.
	contracts := map[string]string{
		"checkout-api":  contractYAML("checkout-api", "tier-1", "traces", "metrics", "logs"),
		"payments-edge": contractYAML("payments-edge", "tier-1", "logs", "traces", "metrics"),
	}

	first := writtenFleet(t, fleetWith(t, contracts))

	// Ten more compiles, each in a fresh directory and in this same process: Go
	// randomises map iteration order per range, so a single second run could agree
	// with the first by luck.
	for attempt := 0; attempt < 10; attempt++ {
		again := writtenFleet(t, fleetWith(t, contracts))

		if len(again) != len(first) {
			t.Fatalf("compile %d produced %d files, the first produced %d", attempt+2, len(again), len(first))
		}
		for file, content := range first {
			if again[file] != content {
				t.Fatalf("%s differs between two compiles of the same Fleet:\n--- first\n%s\n--- again\n%s",
					file, content, again[file])
			}
		}
	}
}

func TestACompiledCollectorConfigSaysItIsGeneratedAndWhatToEditInstead(t *testing.T) {
	// These files sit in a repo where somebody will eventually find one and want to
	// change a batch timeout in it. Editing it would work until the next rollout
	// silently reverted them, so the file has to name its two real inputs.
	files := writtenFleet(t, fleetWith(t, map[string]string{
		"checkout-api": contractYAML("checkout-api", "tier-1", "traces", "metrics", "logs"),
	}))

	config := files["compiled/checkout-api.yaml"]
	for _, phrase := range []string{
		"do not edit",
		"Telemetry Contract",
		"Pipeline Profile",
		"contracts/checkout-api.yaml",
		"tier-1-critical",
	} {
		if !strings.Contains(config, phrase) {
			t.Errorf("the compiled config does not say %q:\n%s", phrase, config)
		}
	}
}

func TestACompiledCollectorConfigNamesItsContractByThePathInsideTheFleet(t *testing.T) {
	// An absolute path would be whatever directory the job happened to run in, so
	// the same Fleet compiled on a laptop and on a runner would differ — and every
	// rollout diff would be noise. It is also useless to a reader: the path that
	// means anything is the one inside the repo they are looking at.
	root := fleetWith(t, map[string]string{
		"checkout-api": contractYAML("checkout-api", "tier-1", "traces", "metrics", "logs"),
	})

	files := writtenFleet(t, root)

	for file, content := range files {
		if strings.Contains(content, root) {
			t.Errorf("%s names the directory it was compiled in (%s):\n%s", file, root, content)
		}
	}
}

func TestAContractThatDoesNotCompileIsReportedAndTheRestOfTheFleetStillCompiles(t *testing.T) {
	// One service's bad Contract must not freeze every other service's rollout: the
	// blast radius of a broken Contract is the service that broke it. reporting-api
	// is tier-2, which has no Pipeline Profile here, so how its telemetry ships is
	// undecided and it cannot compile.
	root := fleetWith(t, map[string]string{
		"checkout-api":  contractYAML("checkout-api", "tier-1", "traces", "metrics", "logs"),
		"reporting-api": contractYAML("reporting-api", "tier-2", "traces", "metrics"),
	})

	rollout := compileFleet(t, root)

	if len(rollout.Compiled) != 1 || rollout.Compiled[0].ServiceName != "checkout-api" {
		t.Fatalf("compiled %v, want checkout-api alone", compiledNames(rollout))
	}
	if len(rollout.Failed) != 1 {
		t.Fatalf("reported %d failures, want 1: %+v", len(rollout.Failed), rollout.Failed)
	}

	failure := rollout.Failed[0]
	if failure.ServiceName != "reporting-api" {
		t.Errorf("the failure names service %q, want reporting-api", failure.ServiceName)
	}
	if !strings.Contains(failure.Reason, "Pipeline Profile") {
		t.Errorf("the failure does not say why reporting-api could not compile: %q", failure.Reason)
	}
	if failure.ContractPath != "contracts/reporting-api.yaml" {
		t.Errorf("the failure names Contract %q, want contracts/reporting-api.yaml", failure.ContractPath)
	}
}

func TestAContractThatStopsCompilingKeepsItsLastCollectorConfiguration(t *testing.T) {
	// Deleting the config would take a running service's telemetry away over an
	// editing mistake, which is a bigger outage than the mistake. The last
	// configuration that did compile stays deployed until somebody fixes the
	// Contract, and the Rollout Manifest says that is what happened.
	root := fleetWith(t, map[string]string{
		"checkout-api": contractYAML("checkout-api", "tier-1", "traces", "metrics", "logs"),
	})
	before := writtenFleet(t, root)["compiled/checkout-api.yaml"]
	if before == "" {
		t.Fatal("checkout-api did not compile in the first place")
	}

	// Now somebody edits the Contract into something that cannot compile.
	broken := contractYAML("checkout-api", "tier-1", "traces", "metricks", "logs")
	if err := os.WriteFile(filepath.Join(root, "contracts", "checkout-api.yaml"), []byte(broken), 0o644); err != nil {
		t.Fatalf("break the Contract: %v", err)
	}

	rollout := compileFleet(t, root)
	if len(rollout.Failed) != 1 {
		t.Fatalf("a Contract naming a non-Signal compiled anyway, as %v", compiledNames(rollout))
	}
	if _, err := rollout.Write(); err != nil {
		t.Fatalf("write the rollout: %v", err)
	}

	after, err := os.ReadFile(filepath.Join(root, "compiled", "checkout-api.yaml"))
	if err != nil {
		t.Fatalf("checkout-api's collector configuration was removed when its Contract broke: %v", err)
	}
	if string(after) != before {
		t.Errorf("checkout-api's collector configuration changed when its Contract stopped compiling:\n--- was\n%s\n--- now\n%s", before, after)
	}
}

func TestTheRolloutManifestRecordsEveryServiceInTheFleetAndWhyEachOneDidNotCompile(t *testing.T) {
	// Without this, a partial rollout is a fleet in two states with no record of
	// which — and a diff shows what changed, never what is absent. The Manifest is
	// how the absence gets into the diff and in front of a reviewer.
	root := fleetWith(t, map[string]string{
		"checkout-api":  contractYAML("checkout-api", "tier-1", "traces", "metrics", "logs"),
		"reporting-api": contractYAML("reporting-api", "tier-2", "traces", "metrics"),
	})

	manifest := writtenFleet(t, root)["rollout-manifest.yaml"]
	if manifest == "" {
		t.Fatal("no rollout-manifest.yaml was written")
	}

	var recorded struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Compiled   []struct {
			ServiceName string   `yaml:"service_name"`
			Tier        string   `yaml:"tier"`
			Profile     string   `yaml:"pipeline_profile"`
			Signals     []string `yaml:"signals"`
			Config      string   `yaml:"collector_config"`
			Digest      string   `yaml:"digest"`
		} `yaml:"compiled"`
		NotCompiled []struct {
			ServiceName string `yaml:"service_name"`
			Contract    string `yaml:"telemetry_contract"`
			Tier        string `yaml:"tier"`
			Reason      string `yaml:"reason"`
		} `yaml:"not_compiled"`
	}
	if err := yaml.Unmarshal([]byte(manifest), &recorded); err != nil {
		t.Fatalf("the Rollout Manifest is not loadable YAML: %v\n%s", err, manifest)
	}

	if recorded.Kind != "RolloutManifest" {
		t.Errorf("the Manifest declares kind %q, want RolloutManifest", recorded.Kind)
	}
	if recorded.APIVersion != contract.APIVersion {
		t.Errorf("the Manifest declares apiVersion %q, want %q", recorded.APIVersion, contract.APIVersion)
	}

	if len(recorded.Compiled) != 1 {
		t.Fatalf("the Manifest records %d compiled services, want 1:\n%s", len(recorded.Compiled), manifest)
	}
	compiled := recorded.Compiled[0]
	if compiled.ServiceName != "checkout-api" || compiled.Tier != "tier-1" ||
		compiled.Profile != "tier-1-critical" || compiled.Config != "compiled/checkout-api.yaml" {
		t.Errorf("the Manifest's compiled entry is wrong: %+v", compiled)
	}
	if compiled.Digest == "" {
		t.Error("the compiled entry has no digest, so nobody can tell whether the file still matches the Manifest")
	}

	if len(recorded.NotCompiled) != 1 {
		t.Fatalf("the Manifest records %d services that did not compile, want 1:\n%s", len(recorded.NotCompiled), manifest)
	}
	failed := recorded.NotCompiled[0]
	if failed.ServiceName != "reporting-api" || failed.Contract != "contracts/reporting-api.yaml" || failed.Tier != "tier-2" {
		t.Errorf("the Manifest's not-compiled entry is wrong: %+v", failed)
	}
	if !strings.Contains(failed.Reason, "Pipeline Profile") {
		t.Errorf("the Manifest does not say why reporting-api did not compile: %q", failed.Reason)
	}
}

func TestAServiceRemovedFromTheFleetHasItsCollectorConfigurationPruned(t *testing.T) {
	// The compiled tree is a statement of what the Fleet runs. A decommissioned
	// service whose config lingered would keep being rolled out by GitOps tooling
	// that has no idea the Contract is gone.
	root := fleetWith(t, map[string]string{
		"checkout-api":  contractYAML("checkout-api", "tier-1", "traces", "metrics", "logs"),
		"payments-edge": contractYAML("payments-edge", "tier-1", "traces", "metrics", "logs"),
	})
	writtenFleet(t, root)

	if err := os.Remove(filepath.Join(root, "contracts", "payments-edge.yaml")); err != nil {
		t.Fatalf("retire payments-edge: %v", err)
	}

	rollout := compileFleet(t, root)
	back, err := rollout.Write()
	if err != nil {
		t.Fatalf("write the rollout: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "compiled", "payments-edge.yaml")); !os.IsNotExist(err) {
		t.Errorf("payments-edge left the Fleet but its collector configuration is still there (%v)", err)
	}
	if len(back.Pruned) != 1 || back.Pruned[0] != "compiled/payments-edge.yaml" {
		t.Errorf("the writeback does not report the pruned file: %+v", back.Pruned)
	}
	if _, err := os.Stat(filepath.Join(root, "compiled", "checkout-api.yaml")); err != nil {
		t.Errorf("checkout-api's collector configuration was pruned too: %v", err)
	}
}

func TestRecompilingAnUnchangedFleetWritesNothingAtAll(t *testing.T) {
	// The scheduled rollout job runs whether or not anything changed, and it decides
	// what to do from what this reports. "I wrote 400 files" every morning is the
	// same lie as an issue tracker that files a duplicate every day: the run has to
	// be able to say there was no news.
	root := fleetWith(t, map[string]string{
		"checkout-api":  contractYAML("checkout-api", "tier-1", "traces", "metrics", "logs"),
		"reporting-api": contractYAML("reporting-api", "tier-2", "traces", "metrics"),
	})
	writtenFleet(t, root)

	rollout := compileFleet(t, root)
	back, err := rollout.Write()
	if err != nil {
		t.Fatalf("write the rollout: %v", err)
	}

	if len(back.Wrote) != 0 {
		t.Errorf("recompiling an unchanged Fleet rewrote %v", back.Wrote)
	}
	if len(back.Pruned) != 0 {
		t.Errorf("recompiling an unchanged Fleet pruned %v", back.Pruned)
	}
	// The manifest and the one compiled config, both already correct.
	if len(back.Unchanged) != 2 {
		t.Errorf("reported %d files already up to date, want 2: %+v", len(back.Unchanged), back.Unchanged)
	}
}

func TestAFleetWithNoContractsIsRefusedRatherThanPruningTheWholeTree(t *testing.T) {
	// A mistyped path or a checkout that did not fetch the Contracts would read as
	// "the fleet is empty", and the next step would prune every collector
	// configuration in the repo and commit it. That is a fleet-wide outage arriving
	// as a green run, so an empty Fleet is refused before anything is written.
	for _, fleet := range map[string]string{
		"a directory with no contracts/ in it": t.TempDir(),
		"an empty contracts/":                  fleetWith(t, nil),
		"a path that does not exist":           filepath.Join(t.TempDir(), "nowhere"),
	} {
		if _, err := controlplane.LoadFleet(fleet); err == nil {
			t.Errorf("LoadFleet(%s) succeeded; an empty Fleet must be refused", fleet)
		}
	}
}

func TestAContractFiledUnderANameThatIsNotItsServiceIsReportedRatherThanCompiled(t *testing.T) {
	// The file name is the key the whole layout turns on: it decides where the
	// compiled config lands and whether a config is pruned. If it could disagree
	// with the service_name inside, one service would silently own another's file.
	root := fleetWith(t, map[string]string{
		"checkout-api": contractYAML("checkout-api-v2", "tier-1", "traces", "metrics", "logs"),
	})

	rollout := compileFleet(t, root)

	if len(rollout.Compiled) != 0 {
		t.Errorf("a misfiled Contract compiled anyway, as %v", compiledNames(rollout))
	}
	if len(rollout.Failed) != 1 {
		t.Fatalf("reported %d failures, want 1: %+v", len(rollout.Failed), rollout.Failed)
	}
	if !strings.Contains(rollout.Failed[0].Reason, "checkout-api.yaml") {
		t.Errorf("the failure does not say which file is misnamed: %q", rollout.Failed[0].Reason)
	}
}

func TestAFileInTheFleetThatIsNotATelemetryContractIsReportedRatherThanFreezingTheFleet(t *testing.T) {
	// contracts/ aggregates files from many service repos, so one of them arriving
	// malformed is a normal Tuesday — and must cost that service its rollout, not
	// everyone's.
	root := fleetWith(t, map[string]string{
		"checkout-api": contractYAML("checkout-api", "tier-1", "traces", "metrics", "logs"),
		"notes":        "just some notes somebody left here\n",
	})

	rollout := compileFleet(t, root)

	if len(rollout.Compiled) != 1 {
		t.Errorf("compiled %v, want checkout-api alone; a stray file stopped the rest of the Fleet", compiledNames(rollout))
	}
	if len(rollout.Failed) != 1 || rollout.Failed[0].ContractPath != "contracts/notes.yaml" {
		t.Errorf("the stray file is not reported: %+v", rollout.Failed)
	}
}

func TestAContractFiledWithTheWrongExtensionIsReportedRatherThanIgnored(t *testing.T) {
	// The worst outcome for a Contract the Fleet does not recognise is silence: it
	// would never be compiled, never appear in the Rollout Manifest, and the service
	// would simply have no collector configuration with nobody told why. Being
	// off-by-one-extension is the near miss that will actually happen.
	root := fleetWith(t, map[string]string{
		"orders-api": contractYAML("orders-api", "tier-1", "traces", "metrics", "logs"),
	})
	if err := os.WriteFile(filepath.Join(root, "contracts", "billing-api.yml"),
		[]byte(contractYAML("billing-api", "tier-1", "traces", "metrics", "logs")), 0o644); err != nil {
		t.Fatalf("write the misnamed Contract: %v", err)
	}

	rollout := compileFleet(t, root)

	if len(rollout.Failed) != 1 || rollout.Failed[0].ContractPath != "contracts/billing-api.yml" {
		t.Fatalf("a Contract named .yml was ignored rather than reported: compiled %v, failed %+v",
			compiledNames(rollout), rollout.Failed)
	}
	if !strings.Contains(rollout.Failed[0].Reason, ".yaml") {
		t.Errorf("the failure does not say what to rename it to: %q", rollout.Failed[0].Reason)
	}
}

// --- helpers -----------------------------------------------------------------

// compiledNames is the services a Rollout compiled, for a failure message that
// does not dump every byte of every collector configuration.
func compiledNames(rollout controlplane.Rollout) []string {
	names := make([]string, 0, len(rollout.Compiled))
	for _, service := range rollout.Compiled {
		names = append(names, service.ServiceName)
	}
	return names
}

// writtenFleet compiles a Fleet, writes it, and reads back every file that
// landed, keyed by its path relative to the Fleet root.
func writtenFleet(t *testing.T, root string) map[string]string {
	t.Helper()

	rollout := compileFleet(t, root)
	if _, err := rollout.Write(); err != nil {
		t.Fatalf("write the rollout: %v", err)
	}

	files := map[string]string{}
	err := filepath.WalkDir(root, func(file string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		relative, err := filepath.Rel(root, file)
		if err != nil {
			return err
		}
		if strings.HasPrefix(relative, "contracts") {
			return nil
		}
		content, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		files[relative] = string(content)
		return nil
	})
	if err != nil {
		t.Fatalf("read back the fleet: %v", err)
	}
	return files
}

// fleetWith builds a Fleet directory whose contracts/ holds one file per entry,
// named by its service. Returns the Fleet root.
func fleetWith(t *testing.T, contracts map[string]string) string {
	t.Helper()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "contracts"), 0o755); err != nil {
		t.Fatalf("make the fleet: %v", err)
	}
	for name, body := range contracts {
		if err := os.WriteFile(filepath.Join(root, "contracts", name+".yaml"), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return root
}

func contractYAML(service, tier string, signals ...string) string {
	body := "apiVersion: guardrail.otel/v1\nkind: TelemetryContract\n" +
		"service_name: " + service + "\nowner: team-" + service + "\ntier: " + tier + "\nsignals:\n"
	for _, signal := range signals {
		body += "  - " + signal + "\n"
	}
	return body + "resource_attributes:\n  service.name: " + service +
		"\n  service.version: \"1.0.0\"\n  deployment.environment: production\n"
}

func compileFleet(t *testing.T, root string) controlplane.Rollout {
	t.Helper()

	fleet, err := controlplane.LoadFleet(root)
	if err != nil {
		t.Fatalf("load the fleet: %v", err)
	}
	rollout, err := controlplane.CompileFleet(fleet, fleetTaxonomy(t), fleetProfiles(t))
	if err != nil {
		t.Fatalf("compile the fleet: %v", err)
	}
	return rollout
}

func fleetTaxonomy(t *testing.T) *guardrail.Taxonomy {
	t.Helper()

	loaded, err := guardrail.CentralTaxonomy()
	if err != nil {
		t.Fatalf("load the Service Tier Taxonomy: %v", err)
	}
	return loaded
}

// fleetProfiles is a Pipeline Profile set these tests own, profiling tier-1 only.
// Deliberately not controlplane/profiles.yaml: which tiers the org has published a
// Profile for is a decision that moves, and a test about fleet-wide compiling must
// not change its meaning the day another tier gets one.
func fleetProfiles(t *testing.T) *controlplane.ProfileSet {
	t.Helper()

	path := filepath.Join(t.TempDir(), "profiles.yaml")
	if err := os.WriteFile(path, []byte(`apiVersion: guardrail.otel/v1
kind: PipelineProfileSet
profiles:
  - profile: tier-1-critical
    tiers: [tier-1]
    description: Everything is kept.
    gateway_endpoint: otel-gateway.observability.svc.cluster.local:4317
    batch:
      timeout: 5s
      send_batch_size: 8192
    sampling:
      traces_percent: 100
`), 0o644); err != nil {
		t.Fatalf("write the Pipeline Profiles: %v", err)
	}

	loaded, err := controlplane.LoadProfiles(path)
	if err != nil {
		t.Fatalf("load the Pipeline Profiles: %v", err)
	}
	return loaded
}
