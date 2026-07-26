package controlplane_test

import (
	"strings"
	"testing"

	"github.com/ghantakiran/OTEL/contract"
	"github.com/ghantakiran/OTEL/controlplane"
)

// Every Service Tier in the taxonomy now has a default Pipeline Profile, so a
// conformant Contract on any tier compiles. C1 shipped tier-1 only; the other two
// failed loudly rather than defaulting, which is what this closes.

func TestEveryServiceTierInTheTaxonomyHasADefaultProfile(t *testing.T) {
	taxonomy := taxonomy(t)
	profileSet := profiles(t)

	for _, tier := range taxonomy.Tiers() {
		if _, published := profileSet.For(tier); !published {
			t.Errorf("Service Tier %s selects no default Pipeline Profile, so its Contracts cannot compile", tier)
		}
	}
}

func TestATierTwoContractCompilesAndRoutesItsMandatorySignals(t *testing.T) {
	config, err := controlplane.Compile(serviceOn("tier-2", "traces", "metrics"), taxonomy(t), profiles(t))
	if err != nil {
		t.Fatalf("a conformant tier-2 Contract did not compile: %v", err)
	}

	if got := config.Signals(); len(got) != 2 {
		t.Errorf("the compiled config routes %v, want traces and metrics", got)
	}
	if config.Collects("logs") {
		t.Error("the compiled config routes logs, which this Contract never declared")
	}
	if err := config.Validate(); err != nil {
		t.Errorf("the compiled tier-2 config is not coherent: %v", err)
	}
}

func TestATierThreeContractCompilesOnTracesAlone(t *testing.T) {
	config, err := controlplane.Compile(serviceOn("tier-3", "traces"), taxonomy(t), profiles(t))
	if err != nil {
		t.Fatalf("a conformant tier-3 Contract did not compile: %v", err)
	}

	if got := config.Signals(); len(got) != 1 || !config.Collects("traces") {
		t.Errorf("the compiled config routes %v, want traces alone", got)
	}
	if err := config.Validate(); err != nil {
		t.Errorf("the compiled tier-3 config is not coherent: %v", err)
	}
}

func TestASignalATierDoesNotMandateIsStillRoutedWhenDeclared(t *testing.T) {
	// S2 is a floor, not a ceiling: tier-3 mandates traces alone, but a tier-3
	// service that chooses to emit logs must have them collected. A compiled config
	// that dropped them would silently discard telemetry the service is producing.
	config, err := controlplane.Compile(serviceOn("tier-3", "traces", "logs"), taxonomy(t), profiles(t))
	if err != nil {
		t.Fatalf("a tier-3 Contract declaring an optional Signal did not compile: %v", err)
	}

	if !config.Collects("logs") {
		t.Errorf("the optional logs Signal is declared but not routed: %v", config.Signals())
	}
	if !config.Collects("traces") {
		t.Errorf("the mandatory traces Signal is not routed: %v", config.Signals())
	}
}

func TestEachTiersProfileShapesItsCompiledConfigDifferently(t *testing.T) {
	// The Profiles are not three copies of one pipeline. Criticality decides how
	// long telemetry waits, how much memory the Agent may take, and how hard it
	// tries to deliver — so a tier-1 and a tier-3 config must not be identical.
	tierOne := rendered(t, serviceOn("tier-1", "traces", "metrics", "logs"))
	tierTwo := rendered(t, serviceOn("tier-2", "traces", "metrics"))
	tierThree := rendered(t, serviceOn("tier-3", "traces"))

	mustContain(t, tierOne, "timeout: 5s", "tier-1 ships fast so telemetry is visible during an incident")
	mustContain(t, tierTwo, "timeout: 15s", "tier-2 trades seconds of latency for cheaper batches")
	mustContain(t, tierThree, "timeout: 30s", "tier-3 favours efficiency over latency")

	mustContain(t, tierOne, "limit_mib: 512", "a tier-1 Agent sits beside a latency-sensitive service")
	mustContain(t, tierThree, "limit_mib: 128", "a tier-3 Agent gets the smallest ceiling")

	// Delivery durability is the sharpest difference: tier-1 telemetry is retried,
	// tier-3 telemetry is dropped rather than back-pressuring a batch job.
	mustContain(t, tierOne, "retry_on_failure", "tier-1 telemetry must not be lost")
	if strings.Contains(tierThree, "retry_on_failure") {
		t.Errorf("a tier-3 Agent retries exports, so it can back-pressure a batch job over telemetry nobody is waiting on:\n%s", tierThree)
	}
}

func TestChangingATiersProfileChangesEveryServiceOnThatTier(t *testing.T) {
	// The acceptance criterion for #11, and the reason pipeline shape is owned
	// centrally (ADR 0005): the platform team retunes a tier by editing one
	// Profile, not by touching the fleet.
	retuned := profilesFrom(t, `apiVersion: guardrail.otel/v1
kind: PipelineProfileSet
profiles:
  - profile: tier-2-retuned
    tiers: [tier-2]
    gateway_endpoint: gateway.test:4317
    memory_limit_mib: 999
    batch:
      timeout: 90s
      send_batch_size: 128
    delivery:
      queue_size: 7
      retry: false
`)

	service := serviceOn("tier-2", "traces", "metrics")
	config, err := controlplane.Compile(service, taxonomy(t), retuned)
	if err != nil {
		t.Fatalf("compile against the retuned Profile: %v", err)
	}
	out, err := config.YAML()
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	for _, want := range []string{"timeout: 90s", "limit_mib: 999", "queue_size: 7"} {
		mustContain(t, string(out), want, "editing the tier's Profile did not reach its services")
	}
	if strings.Contains(string(out), "retry_on_failure") {
		t.Error("the retuned Profile turns retry off, but the compiled config still retries")
	}
}

func TestAProfileSettingThatDoesNothingIsNotEmitted(t *testing.T) {
	// A compiled config carrying a disabled block invites the next reader to think
	// it is doing something. If the Profile asks for no retry and no queue, neither
	// appears at all.
	minimal := profilesFrom(t, `apiVersion: guardrail.otel/v1
kind: PipelineProfileSet
profiles:
  - profile: bare
    tiers: [tier-3]
    gateway_endpoint: gateway.test:4317
    batch:
      timeout: 30s
      send_batch_size: 8192
`)

	config, err := controlplane.Compile(serviceOn("tier-3", "traces"), taxonomy(t), minimal)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	out, err := config.YAML()
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	for _, unwanted := range []string{"sending_queue", "retry_on_failure", "memory_limiter"} {
		if strings.Contains(string(out), unwanted) {
			t.Errorf("the Profile asks for no %s, but the compiled config carries one:\n%s", unwanted, out)
		}
	}
	if err := config.Validate(); err != nil {
		t.Errorf("a minimal Profile compiled an incoherent config: %v", err)
	}
}

func serviceOn(tier string, signals ...string) contract.Contract {
	return contract.Contract{
		APIVersion:  contract.APIVersion,
		Kind:        contract.Kind,
		ServiceName: "service-on-" + tier,
		Owner:       "team-x",
		Tier:        tier,
		Signals:     signals,
		ResourceAttributes: map[string]string{
			"service.name":           "service-on-" + tier,
			"service.version":        "1.0.0",
			"deployment.environment": "production",
		},
	}
}

func rendered(t *testing.T, c contract.Contract) string {
	t.Helper()

	config, err := controlplane.Compile(c, taxonomy(t), profiles(t))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	out, err := config.YAML()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return string(out)
}

func mustContain(t *testing.T, body, want, why string) {
	t.Helper()

	if !strings.Contains(body, want) {
		t.Errorf("%s — the compiled config does not carry %q:\n%s", why, want, body)
	}
}
