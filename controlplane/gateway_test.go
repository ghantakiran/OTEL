package controlplane_test

import (
	"fmt"
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
	config, err := controlplane.CompileGateway(gatewayDeclaration(t), profiles(t), orgStandards(t))
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
	config, err := controlplane.CompileGateway(declaration, profiles(t), orgStandards(t))
	if err != nil {
		t.Fatalf("compile the Gateway: %v", err)
	}

	// The two ends of the chain are what this test is about, not its length: the
	// Gateway also runs the Pipeline Guardrails the Standard catalog compiles into
	// it, and how many of those there are is the catalog's business.
	for signal, pipeline := range config.Service.Pipelines {
		if first := pipeline.Processors[0]; first != "memory_limiter" {
			t.Errorf("the %s pipeline runs %q before its memory ceiling: %v", signal, first, pipeline.Processors)
		}
		if last := pipeline.Processors[len(pipeline.Processors)-1]; last != "batch" {
			t.Errorf("the %s pipeline batches before %q rather than last: %v", signal, last, pipeline.Processors)
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

	_, err := controlplane.CompileGateway(declaration, sendingAgentsElsewhere, orgStandards(t))
	if err == nil {
		t.Fatal("the Gateway compiled while a Pipeline Profile points Agents at a port it does not answer on")
	}
	for _, want := range []string{"tier-1-critical", ":4319", declaration.Address} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q, so a reader cannot see which side to fix: %v", want, err)
		}
	}
}

func TestTheOrgsGatewayFansOutToSeveralIsolatedBackends(t *testing.T) {
	// The acceptance criterion of #13, asked of the declaration the org actually
	// ships rather than a fixture: more than one Backend, and no two of them
	// sharing the thing that would let one stall the others. A test over fixtures
	// only proves fan-out is *possible*.
	declaration := gatewayDeclaration(t)

	if len(declaration.Backends) < 2 {
		t.Fatalf("the org's Gateway declares %d Backend(s); fan-out means more than one", len(declaration.Backends))
	}

	config, err := controlplane.CompileGateway(declaration, profiles(t), orgStandards(t))
	if err != nil {
		t.Fatalf("compile the Gateway: %v", err)
	}

	queues, storages := map[string]string{}, map[string]string{}
	for _, backend := range declaration.Backends {
		exporter, defined := config.Exporters["otlp/"+backend.Name].(map[string]any)
		if !defined {
			t.Fatalf("Backend %q has no exporter of its own: %v", backend.Name, config.Exporters)
		}

		queue, queued := exporter["sending_queue"].(map[string]any)
		if !queued {
			t.Errorf("Backend %q has no queue of its own, so a stall there applies back-pressure to the Gateway itself", backend.Name)
			continue
		}
		if owner, taken := queues[fmt.Sprint(queue)]; taken {
			t.Errorf("Backends %q and %q are the same queue, so one filling fills the other's", owner, backend.Name)
		}
		queues[fmt.Sprint(queue)] = backend.Name

		if storage, spills := queue["storage"].(string); spills {
			if owner, taken := storages[storage]; taken {
				t.Errorf("Backends %q and %q spill through the same storage %q, so they share a file lock and a disk budget", owner, backend.Name, storage)
			}
			storages[storage] = backend.Name
		}
	}

	if len(storages) < 2 {
		t.Errorf("only %d of the org's Backends spill; a Backend whose queue does not survive a Gateway restart is the durability C7 (#15) will read", len(storages))
	}
}

func TestTheGatewayExportsToEveryBackendItDeclares(t *testing.T) {
	// The point of ADR 0007: one Gateway, several Backends, and no service naming
	// any of them. A Backend declared but left out of the pipelines receives
	// nothing — which is how an org discovers mid-migration that half its telemetry
	// never left the Gateway — so every declared Backend must appear in the
	// pipelines it takes, not just the first one.
	declaration := gatewayFrom(t, `apiVersion: guardrail.otel/v1
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
  self_telemetry:
    backend: primary-apm
    resource_attributes:
      service.name: otel-gateway
      service.version: 0.127.0
      deployment.environment: production
`)

	config, err := controlplane.CompileGateway(declaration, profiles(t), orgStandards(t))
	if err != nil {
		t.Fatalf("compile the Gateway: %v", err)
	}

	for _, backend := range declaration.Backends {
		exporter := "otlp/" + backend.Name
		if _, defined := config.Exporters[exporter]; !defined {
			t.Errorf("the Gateway defines no exporter for Backend %q: %v", backend.Name, config.Exporters)
			continue
		}
		for signal, pipeline := range config.Service.Pipelines {
			if !slices.Contains(pipeline.Exporters, exporter) {
				t.Errorf("the %s pipeline does not export to Backend %q, which would therefore receive no %s: %v",
					signal, backend.Name, signal, pipeline.Exporters)
			}
		}
	}
}

func TestABackendReceivesOnlyTheSignalsItDeclares(t *testing.T) {
	// A metrics store is not a trace store. A Backend that takes one Signal must be
	// absent from the other pipelines — otherwise the Gateway ships it telemetry it
	// will reject, and the resulting export failures are indistinguishable from the
	// Backend being down.
	declaration := gatewayFrom(t, `apiVersion: guardrail.otel/v1
kind: GatewayDeclaration
gateway:
  address: otel-gateway.observability.svc.cluster.local:4317
  batch:
    timeout: 5s
    send_batch_size: 8192
  backends:
    - backend: primary-apm
      endpoint: apm-otlp.observability.svc.cluster.local:4317
    - backend: metrics-store
      endpoint: metrics-otlp.observability.svc.cluster.local:4317
      signals: [metrics]
  self_telemetry:
    backend: primary-apm
    resource_attributes:
      service.name: otel-gateway
      service.version: 0.127.0
      deployment.environment: production
`)

	config, err := controlplane.CompileGateway(declaration, profiles(t), orgStandards(t))
	if err != nil {
		t.Fatalf("compile the Gateway: %v", err)
	}

	for signal, pipeline := range config.Service.Pipelines {
		// Omitting `signals:` means every Signal — the one-Backend reading, unchanged.
		if !slices.Contains(pipeline.Exporters, "otlp/primary-apm") {
			t.Errorf("the %s pipeline does not export to primary-apm, which declares no Signal subset: %v", signal, pipeline.Exporters)
		}

		takesMetrics := slices.Contains(pipeline.Exporters, "otlp/metrics-store")
		if signal == "metrics" && !takesMetrics {
			t.Errorf("the metrics pipeline does not export to metrics-store, which is the one Signal it takes: %v", pipeline.Exporters)
		}
		if signal != "metrics" && takesMetrics {
			t.Errorf("the %s pipeline exports to metrics-store, which declares only metrics: %v", signal, pipeline.Exporters)
		}
	}
}

func TestABackendThatNamesSomethingThatIsNotASignalDoesNotCompile(t *testing.T) {
	// `signals: [metricks]` is a Backend that silently receives nothing: it matches
	// no pipeline, every export succeeds everywhere else, and the Backend is simply
	// empty. The same reason Compile rejects a Contract's Signal typo.
	_, err := controlplane.CompileGateway(gatewayFrom(t, `apiVersion: guardrail.otel/v1
kind: GatewayDeclaration
gateway:
  address: otel-gateway.observability.svc.cluster.local:4317
  batch:
    timeout: 5s
    send_batch_size: 8192
  backends:
    - backend: metrics-store
      endpoint: metrics-otlp.observability.svc.cluster.local:4317
      signals: [metricks]
`), profiles(t), orgStandards(t))

	if err == nil {
		t.Fatal("a Backend declaring a Signal that does not exist compiled, so it receives nothing")
	}
	for _, want := range []string{"metrics-store", "metricks"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q, so a reader cannot see what to fix: %v", want, err)
		}
	}
}

func TestABackendThatWritesAnEmptySignalListDoesNotCompile(t *testing.T) {
	// Omitting `signals:` means every Signal. Writing `signals: []` is somebody
	// saying something, and the only thing it can mean is "none" — which is a
	// Backend that does nothing. Reading it as "everything" is the widest possible
	// distance between what was written and what happens, so the two cases are kept
	// apart rather than both falling through a length check.
	_, err := controlplane.CompileGateway(gatewayFrom(t, `apiVersion: guardrail.otel/v1
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
      signals: []
`), profiles(t), orgStandards(t))

	if err == nil {
		t.Fatal("a Backend with an empty signals list compiled — written as `none` and read as `every Signal`")
	}
	if !strings.Contains(err.Error(), "cold-archive") {
		t.Errorf("the error does not name the Backend: %v", err)
	}
}

func TestASignalNoBackendReceivesDoesNotCompile(t *testing.T) {
	// Every Agent forwards every Signal it collects to the Gateway. A Signal with
	// no Backend behind it is a pipeline that receives the fleet's telemetry and
	// exports it nowhere — the no-Backend failure again, one Signal at a time, and
	// invisible from every service because the Agent's export still succeeds.
	_, err := controlplane.CompileGateway(gatewayFrom(t, `apiVersion: guardrail.otel/v1
kind: GatewayDeclaration
gateway:
  address: otel-gateway.observability.svc.cluster.local:4317
  batch:
    timeout: 5s
    send_batch_size: 8192
  backends:
    - backend: metrics-store
      endpoint: metrics-otlp.observability.svc.cluster.local:4317
      signals: [metrics]
  self_telemetry:
    backend: metrics-store
    resource_attributes:
      service.name: otel-gateway
      service.version: 0.127.0
      deployment.environment: production
`), profiles(t), orgStandards(t))

	if err == nil {
		t.Fatal("a Gateway compiled with no Backend for traces or logs, so both arrive and stop there")
	}
	for _, want := range []string{"traces", "logs"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name the %s Signal that nothing receives: %v", want, err)
		}
	}
}

func TestTwoBackendsCannotShareAName(t *testing.T) {
	// A Backend's name is its identity in the compiled config: its exporter is
	// otlp/<name> and its spill lives under <name>. Two Backends sharing one would
	// collapse into a single exporter pointed at whichever endpoint was written
	// last — one of them then receives nothing, and the map that swallowed it shows
	// no sign of the other ever having been declared.
	_, err := controlplane.CompileGateway(gatewayFrom(t, `apiVersion: guardrail.otel/v1
kind: GatewayDeclaration
gateway:
  address: otel-gateway.observability.svc.cluster.local:4317
  batch:
    timeout: 5s
    send_batch_size: 8192
  backends:
    - backend: primary-apm
      endpoint: apm-otlp.observability.svc.cluster.local:4317
    - backend: primary-apm
      endpoint: apm-standby.observability.svc.cluster.local:4317
`), profiles(t), orgStandards(t))

	if err == nil {
		t.Fatal("two Backends named the same compiled, so one of them silently replaced the other")
	}
	if !strings.Contains(err.Error(), "primary-apm") {
		t.Errorf("the error does not name the Backend declared twice: %v", err)
	}
}

func TestABackendNameThatIsNotASimpleIdentifierDoesNotCompile(t *testing.T) {
	// The Backend's name is not just a label: it becomes a collector component ID
	// and a path segment under spill_root. Both of those normalise, and the
	// duplicate-name check compares raw strings — so without a charset, two names
	// that differ here collapse there, and the claim that a shared storage instance
	// is unrepresentable is simply false.
	//
	//   "./a" and "a"       two names, one directory: one bbolt file, one lock.
	//   "../x"              escapes spill_root entirely, onto whatever is mounted
	//                       above it.
	//   " primary-apm"      distinct here, but the collector trims a component ID,
	//                       so it collapses into another exporter there — the
	//                       silent replacement the duplicate check exists to stop.
	for name, backend := range map[string]string{
		"a path segment":     "./a",
		"a parent traversal": "../escapes",
		"leading space":      " primary-apm",
		"a slash":            "apm/primary",
		"upper case":         "Primary-APM",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := controlplane.CompileGateway(gatewayFrom(t, `apiVersion: guardrail.otel/v1
kind: GatewayDeclaration
gateway:
  address: otel-gateway.observability.svc.cluster.local:4317
  spill_root: /var/lib/otelcol/spill
  batch:
    timeout: 5s
    send_batch_size: 8192
  backends:
    - backend: "`+backend+`"
      endpoint: apm-otlp.observability.svc.cluster.local:4317
      delivery:
        queue_size: 20000
        spill: true
`), profiles(t), orgStandards(t))

			if err == nil {
				t.Fatalf("a Backend named %q compiled, so its exporter ID and its spill directory are whatever that normalises to", backend)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "name") {
				t.Errorf("the error does not say the name is the problem: %v", err)
			}
		})
	}
}

func TestEachBackendGetsItsOwnQueueAndItsOwnRetry(t *testing.T) {
	// The whole point of fanning out from one Gateway (ADR 0010): a Backend that
	// stops answering must fill its own queue and nobody else's. Exporters sharing
	// one sending queue is precisely what turns one slow Backend into everyone's
	// outage, so each Backend's durability is read off its own `delivery` block.
	declaration := gatewayFrom(t, `apiVersion: guardrail.otel/v1
kind: GatewayDeclaration
gateway:
  address: otel-gateway.observability.svc.cluster.local:4317
  batch:
    timeout: 5s
    send_batch_size: 8192
  backends:
    - backend: primary-apm
      endpoint: apm-otlp.observability.svc.cluster.local:4317
      delivery:
        queue_size: 20000
        retry: true
    - backend: cold-archive
      endpoint: archive-otlp.observability.svc.cluster.local:4317
      delivery:
        queue_size: 500
        retry: false
  self_telemetry:
    backend: primary-apm
    resource_attributes:
      service.name: otel-gateway
      service.version: 0.127.0
      deployment.environment: production
`)

	config, err := controlplane.CompileGateway(declaration, profiles(t), orgStandards(t))
	if err != nil {
		t.Fatalf("compile the Gateway: %v", err)
	}

	for _, backend := range declaration.Backends {
		exporter, defined := config.Exporters["otlp/"+backend.Name].(map[string]any)
		if !defined {
			t.Fatalf("the Gateway defines no exporter for Backend %q: %v", backend.Name, config.Exporters)
		}

		queue, queued := exporter["sending_queue"].(map[string]any)
		if !queued {
			t.Errorf("Backend %q has no sending queue of its own, so it shares whatever the Gateway holds: %v", backend.Name, exporter)
			continue
		}
		if queue["queue_size"] != backend.Delivery.QueueSize {
			t.Errorf("Backend %q queues %v, but its declaration asks for %d — its queue is not its own",
				backend.Name, queue["queue_size"], backend.Delivery.QueueSize)
		}

		_, retries := exporter["retry_on_failure"]
		if retries != backend.Delivery.Retry {
			t.Errorf("Backend %q retries=%v, but its declaration says %v", backend.Name, retries, backend.Delivery.Retry)
		}
	}
}

func TestEachSpillingBackendGetsItsOwnStorageAndItsOwnDirectory(t *testing.T) {
	// Spill is what makes a queue survive the Gateway being restarted while a
	// Backend is down. It is also the place per-Backend isolation is easiest to
	// lose: one storage extension shared by two exporters is one file lock, one
	// disk budget and one corruption blast radius — the Backends would be coupled
	// again through the very mechanism meant to decouple them. So the storage
	// instance and its directory are derived from the Backend's name, and no two
	// Backends can share a name.
	declaration := gatewayFrom(t, `apiVersion: guardrail.otel/v1
kind: GatewayDeclaration
gateway:
  address: otel-gateway.observability.svc.cluster.local:4317
  spill_root: /var/lib/otelcol/spill
  batch:
    timeout: 5s
    send_batch_size: 8192
  backends:
    - backend: primary-apm
      endpoint: apm-otlp.observability.svc.cluster.local:4317
      delivery:
        queue_size: 20000
        retry: true
        spill: true
    - backend: cold-archive
      endpoint: archive-otlp.observability.svc.cluster.local:4317
      delivery:
        queue_size: 5000
        retry: true
        spill: true
  self_telemetry:
    backend: primary-apm
    resource_attributes:
      service.name: otel-gateway
      service.version: 0.127.0
      deployment.environment: production
`)

	config, err := controlplane.CompileGateway(declaration, profiles(t), orgStandards(t))
	if err != nil {
		t.Fatalf("compile the Gateway: %v", err)
	}

	directories := map[string]string{}
	for _, backend := range declaration.Backends {
		storage := "file_storage/" + backend.Name

		extension, defined := config.Extensions[storage].(map[string]any)
		if !defined {
			t.Fatalf("Backend %q asked to spill but has no storage extension of its own: %v", backend.Name, config.Extensions)
		}
		directory, _ := extension["directory"].(string)
		if directory == "" {
			t.Errorf("the storage for Backend %q names no directory: %v", backend.Name, extension)
		}
		if owner, taken := directories[directory]; taken {
			t.Errorf("Backends %q and %q spill into the same directory %s, so they are coupled through it", owner, backend.Name, directory)
		}
		directories[directory] = backend.Name

		if !slices.Contains(config.Service.Extensions, storage) {
			t.Errorf("storage %q is defined but the Gateway does not run it, so the queue it backs is not persistent: %v", storage, config.Service.Extensions)
		}

		exporter := config.Exporters["otlp/"+backend.Name].(map[string]any)
		queue, queued := exporter["sending_queue"].(map[string]any)
		if !queued {
			t.Fatalf("Backend %q has no sending queue to spill from: %v", backend.Name, exporter)
		}
		if queue["storage"] != storage {
			t.Errorf("Backend %q spills to %v, want its own %q", backend.Name, queue["storage"], storage)
		}
	}
}

func TestABackendCannotSpillWithNowhereToSpillTo(t *testing.T) {
	// `spill: true` with no `spill_root` would compile a storage directory relative
	// to wherever the Gateway process happens to have been started — inside the
	// container image, on no mounted volume, gone on the next restart. That is a
	// queue that reads as persistent in every config review and is not.
	_, err := controlplane.CompileGateway(gatewayFrom(t, `apiVersion: guardrail.otel/v1
kind: GatewayDeclaration
gateway:
  address: otel-gateway.observability.svc.cluster.local:4317
  batch:
    timeout: 5s
    send_batch_size: 8192
  backends:
    - backend: primary-apm
      endpoint: apm-otlp.observability.svc.cluster.local:4317
      delivery:
        queue_size: 20000
        spill: true
`), profiles(t), orgStandards(t))

	if err == nil {
		t.Fatal("a Backend spilling with no spill_root compiled, so its queue is persistent nowhere")
	}
	for _, want := range []string{"primary-apm", "spill_root"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q, so a reader cannot see what to add: %v", want, err)
		}
	}
}

func TestASpillRootThatIsNotAnAbsolutePathDoesNotCompile(t *testing.T) {
	// A relative root produces exactly the outcome the missing-root error describes
	// — a directory resolved against wherever the Gateway process happened to be
	// started, on no mounted volume — so refusing only the empty string would leave
	// the failure reachable by a shorter route. A spill volume is mounted at an
	// absolute path or it is not mounted.
	for _, root := range []string{"spill", "./spill", "var/lib/otelcol/spill"} {
		t.Run(root, func(t *testing.T) {
			_, err := controlplane.CompileGateway(gatewayFrom(t, `apiVersion: guardrail.otel/v1
kind: GatewayDeclaration
gateway:
  address: otel-gateway.observability.svc.cluster.local:4317
  spill_root: `+root+`
  batch:
    timeout: 5s
    send_batch_size: 8192
  backends:
    - backend: primary-apm
      endpoint: apm-otlp.observability.svc.cluster.local:4317
      delivery:
        queue_size: 20000
        spill: true
`), profiles(t), orgStandards(t))

			if err == nil {
				t.Fatalf("a spill_root of %q compiled, so the queue is written relative to the process's working directory", root)
			}
			if !strings.Contains(err.Error(), "spill_root") {
				t.Errorf("the error does not say the spill_root is the problem: %v", err)
			}
		})
	}
}

func TestABackendCannotSpillWithNoQueueToSpillFrom(t *testing.T) {
	// Spill is a property of the sending queue, not a separate mechanism: it is
	// where the queue is kept. Asking for it with `queue_size: 0` compiles a storage
	// extension, a directory and a mounted volume that nothing ever writes to — the
	// same class of dead setting the Agent refuses to emit, except this one looks
	// like durability.
	_, err := controlplane.CompileGateway(gatewayFrom(t, `apiVersion: guardrail.otel/v1
kind: GatewayDeclaration
gateway:
  address: otel-gateway.observability.svc.cluster.local:4317
  spill_root: /var/lib/otelcol/spill
  batch:
    timeout: 5s
    send_batch_size: 8192
  backends:
    - backend: primary-apm
      endpoint: apm-otlp.observability.svc.cluster.local:4317
      delivery:
        retry: true
        spill: true
`), profiles(t), orgStandards(t))

	if err == nil {
		t.Fatal("a Backend spilling with no queue compiled, so it has storage nothing writes to")
	}
	for _, want := range []string{"primary-apm", "queue_size"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q, so a reader cannot see what to add: %v", want, err)
		}
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

	_, err := controlplane.CompileGateway(noBackend, profiles(t), orgStandards(t))
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
			_, err := controlplane.CompileGateway(gatewayAt(t, address), profiles(t), orgStandards(t))
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
`+backend), profiles(t), orgStandards(t))

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
	config, err := controlplane.CompileGateway(gatewayDeclaration(t), profiles(t), orgStandards(t))
	if err != nil {
		t.Fatalf("compile the Gateway: %v", err)
	}

	if err := config.Validate(); err != nil {
		t.Errorf("the compiled Gateway config does not validate: %v", err)
	}
}

func TestACompiledConfigRunsExactlyTheExtensionsItDefines(t *testing.T) {
	// Referential integrity, extended to the components that are not in a pipeline.
	// Both directions are silent failures a compiled Gateway would otherwise ship:
	// a queue naming storage that does not exist stops the collector at load, and
	// storage the service block never starts is inert — the queue stays in memory
	// while the config, the mounted volume and every reviewer say it does not.
	coherent := func() controlplane.CollectorConfig {
		return controlplane.CollectorConfig{
			Receivers:  map[string]any{"otlp": map[string]any{}},
			Processors: map[string]any{"batch": map[string]any{}},
			Exporters:  map[string]any{"otlp/primary-apm": map[string]any{}},
			Extensions: map[string]any{"file_storage/primary-apm": map[string]any{}},
			Service: controlplane.CollectorService{
				Extensions: []string{"file_storage/primary-apm"},
				Pipelines: map[string]controlplane.Pipeline{
					"traces": {
						Receivers:  []string{"otlp"},
						Processors: []string{"batch"},
						Exporters:  []string{"otlp/primary-apm"},
					},
				},
				// A collector that runs pipelines and reports nothing about itself is
				// not coherent (ADR 0016), so a config assembled here to ask a
				// question about extensions has to be coherent in that respect too.
				Telemetry: selfObserving(),
			},
		}
	}

	if err := coherent().Validate(); err != nil {
		t.Fatalf("a config whose extensions line up does not validate: %v", err)
	}

	t.Run("running an extension nobody defined", func(t *testing.T) {
		config := coherent()
		config.Service.Extensions = append(config.Service.Extensions, "file_storage/cold-archive")

		if err := config.Validate(); err == nil {
			t.Error("a config validated while running an extension it does not define")
		} else if !strings.Contains(err.Error(), "cold-archive") {
			t.Errorf("the error does not name the dangling extension: %v", err)
		}
	})

	t.Run("defining an extension nobody runs", func(t *testing.T) {
		config := coherent()
		config.Service.Extensions = nil

		if err := config.Validate(); err == nil {
			t.Error("a config validated while defining storage the collector never starts, so the queue it backs is not persistent")
		} else if !strings.Contains(err.Error(), "primary-apm") {
			t.Errorf("the error does not name the inert extension: %v", err)
		}
	})
}

func TestAQueueMaySpillOnlyToStorageTheCollectorRuns(t *testing.T) {
	// The third reference in a spilling config, and the one Validate could not see:
	// an exporter's `sending_queue.storage` names an extension. Two independent
	// `if backend.Delivery.Spill` branches happen to keep it consistent today —
	// nothing in the types ties them together, and a config where they disagree
	// stops the collector at load while Validate calls it coherent.
	spilling := func() controlplane.CollectorConfig {
		return controlplane.CollectorConfig{
			Receivers:  map[string]any{"otlp": map[string]any{}},
			Processors: map[string]any{"batch": map[string]any{}},
			Exporters: map[string]any{"otlp/primary-apm": map[string]any{
				"sending_queue": map[string]any{"storage": "file_storage/primary-apm"},
			}},
			Extensions: map[string]any{"file_storage/primary-apm": map[string]any{}},
			Service: controlplane.CollectorService{
				Extensions: []string{"file_storage/primary-apm"},
				Pipelines: map[string]controlplane.Pipeline{
					"traces": {
						Receivers:  []string{"otlp"},
						Processors: []string{"batch"},
						Exporters:  []string{"otlp/primary-apm"},
					},
				},
				// A collector that runs pipelines and reports nothing about itself is
				// not coherent (ADR 0016), so a config assembled here to ask a
				// question about extensions has to be coherent in that respect too.
				Telemetry: selfObserving(),
			},
		}
	}

	if err := spilling().Validate(); err != nil {
		t.Fatalf("a coherent spilling config does not validate: %v", err)
	}

	t.Run("spilling to storage that does not exist", func(t *testing.T) {
		config := spilling()
		config.Exporters["otlp/primary-apm"].(map[string]any)["sending_queue"] = map[string]any{
			"storage": "file_storage/cold-archive",
		}

		if err := config.Validate(); err == nil {
			t.Error("a config validated while a queue spills to storage the config does not define; the collector would refuse it at load")
		} else if !strings.Contains(err.Error(), "cold-archive") {
			t.Errorf("the error does not name the storage that is missing: %v", err)
		}
	})

	t.Run("spilling to storage the service block does not start", func(t *testing.T) {
		config := spilling()
		config.Extensions["file_storage/cold-archive"] = map[string]any{}
		config.Service.Extensions = []string{"file_storage/cold-archive"}
		config.Exporters["otlp/primary-apm"].(map[string]any)["sending_queue"] = map[string]any{
			"storage": "file_storage/primary-apm",
		}

		if err := config.Validate(); err == nil {
			t.Error("a config validated while a queue spills to an extension that is defined but never started")
		}
	})
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
  self_telemetry:
    backend: primary-apm
    resource_attributes:
      service.name: otel-gateway
      service.version: 0.127.0
      deployment.environment: production
`))

	for _, dead := range []string{"sending_queue", "retry_on_failure", "memory_limiter"} {
		if strings.Contains(rendered, dead) {
			t.Errorf("the compiled Gateway carries a %s the declaration never asked for:\n%s", dead, rendered)
		}
	}
}

// selfObserving is the smallest `service.telemetry` block a coherent compiled
// config can carry: an identity that names the configuration it is running, and
// somewhere to say it. Tests that assemble a config by hand to ask about
// something else still need one, because a collector nobody can see is refused
// (ADR 0016) — the same reason a config that runs no pipelines is.
func selfObserving() *controlplane.CollectorTelemetry {
	return &controlplane.CollectorTelemetry{
		Resource: map[string]string{"otel.platform.config_version": "sha256:assembled-by-hand"},
		Metrics: controlplane.TelemetryMetrics{
			Level:   "normal",
			Readers: []any{map[string]any{"periodic": map[string]any{}}},
		},
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

	config, err := controlplane.CompileGateway(declaration, profiles(t), orgStandards(t))
	if err != nil {
		t.Fatalf("compile the Gateway: %v", err)
	}
	rendered, err := config.YAML()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return string(rendered)
}
