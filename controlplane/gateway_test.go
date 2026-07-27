package controlplane_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ghantakiran/OTEL/contract"
	"github.com/ghantakiran/OTEL/controlplane"
)

func TestTheGatewayReceivesOTLPFromAgentsAndExportsToABackend(t *testing.T) {
	// The two-tier topology in one assertion: Agents forward OTLP to the address
	// the Gateway answers on, and the Gateway — not the Agent — is what names a
	// Backend (ADR 0007).
	declaration := gatewayDeclaration(t)
	rendered := compiledGatewayYAML(t, declaration)

	if !strings.Contains(rendered, "0.0.0.0:4317") {
		t.Errorf("the Gateway does not receive OTLP on the port its address publishes:\n%s", rendered)
	}
	if !strings.Contains(rendered, declaration.Backends[0].Endpoint) {
		t.Errorf("the Gateway does not export to the Backend %q that the declaration names:\n%s",
			declaration.Backends[0].Name, rendered)
	}
}

func TestTheGatewayCarriesEverySignalAnAgentCanForward(t *testing.T) {
	// An Agent's pipelines are the Signals one Contract declares. The Gateway is
	// shared by the whole fleet, so it must relay anything any Contract could
	// declare — every Signal there is, not a per-tier subset.
	config, err := controlplane.CompileGateway(gatewayDeclaration(t), profiles(t))
	if err != nil {
		t.Fatalf("compile the Gateway: %v", err)
	}

	for _, signal := range contract.Signals() {
		if !config.Collects(string(signal)) {
			t.Errorf("the Gateway has no pipeline for the %s Signal, so an Agent forwarding it is dropped", signal)
		}
	}
	if got := len(config.Signals()); got != len(contract.Signals()) {
		t.Errorf("the Gateway runs %d pipelines, want one per Signal (%v)", got, config.Signals())
	}
}

func TestTheGatewayRebatchesTheWholeFleetUnderAMemoryCeiling(t *testing.T) {
	// Agents batch per service; the Gateway rebatches across all of them, which is
	// most of the reason to have a Gateway at all. The memory ceiling comes first in
	// the chain for the same reason an Agent's does — a limiter placed after
	// batching caps memory that batching has already been allowed to allocate.
	declaration := gatewayDeclaration(t)
	config, err := controlplane.CompileGateway(declaration, profiles(t))
	if err != nil {
		t.Fatalf("compile the Gateway: %v", err)
	}

	want := []string{"memory_limiter", "batch"}
	for signal, pipeline := range config.Service.Pipelines {
		if !slices.Equal(pipeline.Processors, want) {
			t.Errorf("the %s pipeline's processors are %v, want %v", signal, pipeline.Processors, want)
		}
	}

	batch, declared := config.Processors["batch"].(map[string]any)
	if !declared {
		t.Fatalf("the Gateway defines no batch processor: %v", config.Processors)
	}
	if batch["timeout"] != declaration.Batch.Timeout {
		t.Errorf("the Gateway batches on a %v timeout, but the declaration says %q", batch["timeout"], declaration.Batch.Timeout)
	}

	limiter, declared := config.Processors["memory_limiter"].(map[string]any)
	if !declared {
		t.Fatalf("the Gateway defines no memory ceiling: %v", config.Processors)
	}
	if limiter["limit_mib"] != declaration.MemoryLimitMiB {
		t.Errorf("the Gateway's memory ceiling is %v MiB, but the declaration says %d", limiter["limit_mib"], declaration.MemoryLimitMiB)
	}
}

func TestTheGatewayMustListenWhereEveryPipelineProfileSendsAgents(t *testing.T) {
	// The topology is declared in two files — the Profiles say where an Agent
	// forwards, the Gateway Declaration says where the Gateway answers — and an
	// Agent forwarding somewhere nothing is listening drops every span, silently and
	// for as long as nobody notices. It is catchable at compile time, so it is.
	sendingAgentsElsewhere := profilesFrom(t, `apiVersion: guardrail.otel/v1
kind: PipelineProfileSet
profiles:
  - profile: tier-1-critical
    tiers: [tier-1]
    gateway_endpoint: otel-gateway.observability.svc.cluster.local:4319
    batch:
      timeout: 5s
      send_batch_size: 8192
`)
	declaration := gatewayDeclaration(t)

	_, err := controlplane.CompileGateway(declaration, sendingAgentsElsewhere)
	if err == nil {
		t.Fatal("the Gateway compiled while a Pipeline Profile points Agents at a port it does not answer on")
	}
	for _, want := range []string{"tier-1-critical", ":4319", declaration.Address} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q, so a reader cannot see which side to fix: %v", want, err)
		}
	}
}

func TestASecondBackendDoesNotCompileUntilFanOutLands(t *testing.T) {
	// Fan-out to several Backends is C5 (#13). Until then a second Backend is
	// refused by name rather than quietly ignored: silently exporting to the first
	// of two declared Backends is how an org discovers, during a Splunk migration,
	// that half its telemetry never left the Gateway.
	twoBackends := gatewayFrom(t, `apiVersion: guardrail.otel/v1
kind: GatewayDeclaration
gateway:
  address: otel-gateway.observability.svc.cluster.local:4317
  batch:
    timeout: 5s
    send_batch_size: 8192
  backends:
    - backend: primary-apm
      endpoint: apm-otlp.observability.svc.cluster.local:4317
    - backend: cold-archive
      endpoint: archive-otlp.observability.svc.cluster.local:4317
`)

	_, err := controlplane.CompileGateway(twoBackends, profiles(t))
	if err == nil {
		t.Fatal("two Backends compiled, so one of them is silently receiving nothing")
	}
	if !strings.Contains(err.Error(), "cold-archive") {
		t.Errorf("the error does not name the Backend that would not have received anything: %v", err)
	}
}

func TestAGatewayThatNamesNoBackendDoesNotCompile(t *testing.T) {
	// A Gateway with no Backend receives the whole fleet's telemetry and drops it.
	// Nothing about that is visible from a service — every Agent's export succeeds —
	// so it has to be refused where it is written.
	noBackend := gatewayFrom(t, `apiVersion: guardrail.otel/v1
kind: GatewayDeclaration
gateway:
  address: otel-gateway.observability.svc.cluster.local:4317
  batch:
    timeout: 5s
    send_batch_size: 8192
`)

	_, err := controlplane.CompileGateway(noBackend, profiles(t))
	if err == nil {
		t.Fatal("a Gateway with no Backend compiled, so the fleet's telemetry would arrive and stop")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "backend") {
		t.Errorf("the error does not say what is missing: %v", err)
	}
}

func TestAGatewayAddressThatIsNotHostAndPortDoesNotCompile(t *testing.T) {
	// The Gateway's OTLP receiver is derived from this address's port. An address
	// with no port would compile a receiver bound to nothing, and a Gateway that
	// never starts is discovered by the fleet, not by whoever edited the file.
	for name, address := range map[string]string{
		"no address at all":     "",
		"no port":               "otel-gateway.observability.svc.cluster.local",
		"a URL, not an address": "http://otel-gateway.observability.svc.cluster.local:4317",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := controlplane.CompileGateway(gatewayAt(t, address), profiles(t))
			if err == nil {
				t.Fatalf("the Gateway compiled with %q as its address", address)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "address") {
				t.Errorf("the error does not say the address is the problem: %v", err)
			}
		})
	}
}

func TestABackendTheGatewayCannotDialDoesNotCompile(t *testing.T) {
	// A Backend is a name and an endpoint. Missing either compiles an exporter that
	// is either unreachable or unnameable, and both are only discovered once the
	// Gateway is holding the fleet's telemetry.
	for name, backend := range map[string]string{
		"no endpoint": "    - backend: primary-apm\n",
		"no name":     "    - endpoint: apm-otlp.observability.svc.cluster.local:4317\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := controlplane.CompileGateway(gatewayFrom(t, `apiVersion: guardrail.otel/v1
kind: GatewayDeclaration
gateway:
  address: otel-gateway.observability.svc.cluster.local:4317
  batch:
    timeout: 5s
    send_batch_size: 8192
  backends:
`+backend), profiles(t))

			if err == nil {
				t.Fatalf("a Backend with %s compiled anyway", name)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "backend") {
				t.Errorf("the error does not say a Backend is the problem: %v", err)
			}
		})
	}
}

func TestADocumentThatIsNotAGatewayDeclarationIsNotRead(t *testing.T) {
	// Every field of a Gateway Declaration is simply absent from another document
	// kind, so the wrong file decodes cleanly into an empty one — and the reader
	// then gets a confident error about a Gateway nobody described. The same
	// reasoning that makes contract.Load check its kind.
	for name, body := range map[string]string{
		"another kind entirely": `apiVersion: guardrail.otel/v1
kind: PipelineProfileSet
profiles: []
`,
		"a schema this binary predates": `apiVersion: guardrail.otel/v99
kind: GatewayDeclaration
gateway:
  address: otel-gateway.observability.svc.cluster.local:4317
`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gateway.yaml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			if _, err := controlplane.LoadGateway(path); err == nil {
				t.Fatalf("%s was read as a Gateway Declaration", name)
			}
		})
	}
}

func TestACompiledGatewayConfigIsInternallyCoherent(t *testing.T) {
	// The same referential-integrity question the Agent's config answers, asked of
	// the Gateway's — one intermediate model, so one Validate.
	config, err := controlplane.CompileGateway(gatewayDeclaration(t), profiles(t))
	if err != nil {
		t.Fatalf("compile the Gateway: %v", err)
	}

	if err := config.Validate(); err != nil {
		t.Errorf("the compiled Gateway config does not validate: %v", err)
	}
}

func TestABackendThatAsksForNoDurabilityGetsNoDeadSettings(t *testing.T) {
	// The Agent's rule, applied to the Gateway: a setting that would do nothing is
	// not emitted, rather than emitted disabled where the next reader takes it for
	// something that is working.
	rendered := compiledGatewayYAML(t, gatewayFrom(t, `apiVersion: guardrail.otel/v1
kind: GatewayDeclaration
gateway:
  address: otel-gateway.observability.svc.cluster.local:4317
  batch:
    timeout: 5s
    send_batch_size: 8192
  backends:
    - backend: primary-apm
      endpoint: apm-otlp.observability.svc.cluster.local:4317
`))

	for _, dead := range []string{"sending_queue", "retry_on_failure", "memory_limiter"} {
		if strings.Contains(rendered, dead) {
			t.Errorf("the compiled Gateway carries a %s the declaration never asked for:\n%s", dead, rendered)
		}
	}
}

func gatewayDeclaration(t *testing.T) *controlplane.GatewayDeclaration {
	t.Helper()

	loaded, err := controlplane.CentralGateway()
	if err != nil {
		t.Fatalf("load the Gateway Declaration: %v", err)
	}
	return loaded
}

// gatewayAt is a Gateway Declaration that is otherwise complete, answering on the
// given address.
func gatewayAt(t *testing.T, address string) *controlplane.GatewayDeclaration {
	t.Helper()

	return gatewayFrom(t, `apiVersion: guardrail.otel/v1
kind: GatewayDeclaration
gateway:
  address: `+address+`
  batch:
    timeout: 5s
    send_batch_size: 8192
  backends:
    - backend: primary-apm
      endpoint: apm-otlp.observability.svc.cluster.local:4317
`)
}

// gatewayFrom loads a Gateway Declaration written inline, so a test can reshape
// the Gateway and watch the compiled config follow.
func gatewayFrom(t *testing.T, body string) *controlplane.GatewayDeclaration {
	t.Helper()

	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the Gateway Declaration: %v", err)
	}
	loaded, err := controlplane.LoadGateway(path)
	if err != nil {
		t.Fatalf("load the Gateway Declaration: %v", err)
	}
	return loaded
}

func compiledGatewayYAML(t *testing.T, declaration *controlplane.GatewayDeclaration) string {
	t.Helper()

	config, err := controlplane.CompileGateway(declaration, profiles(t))
	if err != nil {
		t.Fatalf("compile the Gateway: %v", err)
	}
	rendered, err := config.YAML()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return string(rendered)
}
