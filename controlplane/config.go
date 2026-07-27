package controlplane

import (
	"fmt"
	"net"
	"path"
	"regexp"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/ghantakiran/OTEL/contract"
	"github.com/ghantakiran/OTEL/guardrail"
)

// CollectorConfig is a compiled OpenTelemetry Collector configuration — for one
// service's Agent, or for the shared Gateway. One intermediate model for both:
// they are the same kind of artifact, and a second would mean two Validate
// implementations disagreeing about what "coherent" means.
//
// It is a typed value rather than a string of YAML on purpose: the Control Plane
// wants to ask questions of it before it ever reaches a file — does it collect
// the Signals we require, does every pipeline reference a component that exists —
// and a caller comparing two rollouts wants to diff structure, not formatting.
// YAML is one rendering of it, not the thing itself.
type CollectorConfig struct {
	Receivers  map[string]any `yaml:"receivers"`
	Processors map[string]any `yaml:"processors"`
	Exporters  map[string]any `yaml:"exporters"`
	// Extensions are components that serve the collector rather than a pipeline —
	// today, the per-Backend spill storage the Gateway's sending queues write to.
	// Omitted entirely when there are none, so an Agent's config is unchanged by
	// the field existing.
	Extensions map[string]any   `yaml:"extensions,omitempty"`
	Service    CollectorService `yaml:"service"`
}

// CollectorService is the collector's `service` block: which pipelines run, and
// what each is wired from.
type CollectorService struct {
	// Extensions are the extensions the collector actually starts. Defining one is
	// not enough — an extension absent from here is inert, which for spill storage
	// means a queue that silently is not persistent.
	Extensions []string            `yaml:"extensions,omitempty"`
	Pipelines  map[string]Pipeline `yaml:"pipelines"`
	// Telemetry is what this collector says about ITSELF — the configuration it is
	// running, and its own queue, export-failure and drop counters — and the path
	// it says it on. That path is deliberately not one of the pipelines above: the
	// signals exist to report on those, so they must not travel through them
	// (ADR 0010). Omitted entirely when absent, so a config assembled by a caller
	// renders exactly as it did before this field existed.
	Telemetry *CollectorTelemetry `yaml:"telemetry,omitempty"`
}

// Pipeline is one Signal's path through a collector.
type Pipeline struct {
	Receivers  []string `yaml:"receivers"`
	Processors []string `yaml:"processors"`
	Exporters  []string `yaml:"exporters"`
}

// Component names. An Agent receives OTLP, batches, stamps the resource
// attributes the Contract declares, and forwards to the Gateway — it names no
// Backend and does no enforcement (ADR 0007).
const (
	otlpReceiver           = "otlp"
	batchProcessor         = "batch"
	resourceProcessor      = "resource"
	memoryLimiterProcessor = "memory_limiter"
	gatewayExporter        = "otlp/gateway"
	// fileStorageExtension backs a persistent sending queue. It is the one component
	// on this platform that is not in the collector's core distribution (ADR 0014).
	fileStorageExtension = "file_storage"
)

// assemble builds the config from the three inputs. Kept separate from Compile so
// that Compile reads as the decisions and this reads as the shape.
func assemble(c contract.Contract, profile Profile, signals []contract.Signal) (CollectorConfig, error) {
	config := CollectorConfig{
		Receivers: map[string]any{
			otlpReceiver: map[string]any{
				"protocols": map[string]any{
					"grpc": map[string]any{"endpoint": "0.0.0.0:4317"},
					"http": map[string]any{"endpoint": "0.0.0.0:4318"},
				},
			},
		},
		Processors: map[string]any{
			batchProcessor: map[string]any{
				"timeout":         profile.Batch.Timeout,
				"send_batch_size": profile.Batch.SendBatchSize,
			},
			// The Contract's declared resource attributes are stamped here, which is
			// what makes "declared equals deployed" true by construction (ADR 0005):
			// the same Contract that Preflight checked produces the running config.
			resourceProcessor: map[string]any{"attributes": resourceAttributes(c)},
		},
		Exporters: map[string]any{
			gatewayExporter: gatewayExport(profile),
		},
		Service: CollectorService{Pipelines: map[string]Pipeline{}},
	}

	// An Agent runs beside the service it collects from. A memory ceiling is what
	// keeps it from becoming the reason that service degrades, so the limit is a
	// per-tier decision rather than a global default.
	processors := []string{resourceProcessor, batchProcessor}
	if profile.MemoryLimitMiB > 0 {
		config.Processors[memoryLimiterProcessor] = map[string]any{
			"limit_mib":      profile.MemoryLimitMiB,
			"check_interval": "1s",
		}
		// First in the chain: a limiter that runs after batching has already let the
		// memory it was meant to cap be allocated.
		processors = append([]string{memoryLimiterProcessor}, processors...)
	}

	for _, signal := range signals {
		config.Service.Pipelines[string(signal)] = Pipeline{
			Receivers:  []string{otlpReceiver},
			Processors: processors,
			Exporters:  []string{gatewayExporter},
		}
	}

	// The Agent's own telemetry, on its own path to the Gateway. Its identity is
	// the service's — the same Telemetry Contract the resource processor stamps —
	// because an Agent is that service's Agent: an operator asking why this
	// service's telemetry is thin wants the Agent's queue depth filed under the
	// service, not under a thousandth anonymous collector.
	//
	// It is the SAME address the pipeline forwards to and a DIFFERENT client: the
	// Gateway is the only hop an Agent knows (ADR 0007), so independence here means
	// independence from the exporter, not from the Gateway.
	config.Service.Telemetry = selfTelemetry(c.ResourceAttributes, profile.GatewayEndpoint)
	if err := config.stampConfigVersion(); err != nil {
		return CollectorConfig{}, err
	}
	return config, nil
}

// assembleGateway builds the shared Gateway's config: receive OTLP from the
// fleet's Agents, rebatch across all of them, export to a Backend.
func assembleGateway(declaration GatewayDeclaration, standards *guardrail.StandardCatalog) (CollectorConfig, error) {
	config := CollectorConfig{
		Receivers: map[string]any{
			otlpReceiver: map[string]any{
				"protocols": map[string]any{
					// Derived from the address Agents were told to forward to, so the two
					// halves of the topology cannot be configured to disagree.
					"grpc": map[string]any{"endpoint": listenOn(declaration.Address)},
				},
			},
		},
		Processors: map[string]any{
			batchProcessor: map[string]any{
				"timeout":         declaration.Batch.Timeout,
				"send_batch_size": declaration.Batch.SendBatchSize,
			},
		},
		Exporters: map[string]any{},
		Service:   CollectorService{Pipelines: map[string]Pipeline{}},
	}

	// The Gateway absorbs the whole fleet's telemetry, so its ceiling matters more
	// than any Agent's — and it goes first for the same reason: a limiter placed
	// after batching caps memory batching has already been allowed to allocate.
	var processors []string
	if declaration.MemoryLimitMiB > 0 {
		config.Processors[memoryLimiterProcessor] = map[string]any{
			"limit_mib":      declaration.MemoryLimitMiB,
			"check_interval": "1s",
		}
		processors = append(processors, memoryLimiterProcessor)
	}

	// The Pipeline Guardrails, compiled from the Standards the catalog enforces at
	// the pipeline. They run in the GATEWAY and nowhere else: an Agent does no
	// enforcement (ADR 0007), and one central place to inspect the whole fleet's
	// telemetry is the reason there is a Gateway tier at all.
	//
	// Upstream of the fan-out, so a record is judged once rather than once per
	// Backend, and before `batch`, so that what every exporter sends is already
	// tagged and no Backend receives a different verdict from another.
	if guardrails, enforced := pipelineGuardrails(standards); enforced {
		config.Processors[pipelineGuardrailProcessor] = guardrails
		processors = append(processors, pipelineGuardrailProcessor)
	}

	// Batching is last: it is how telemetry leaves, not something that decides
	// anything about it.
	processors = append(processors, batchProcessor)

	// One exporter per Backend, each owning its queue, its retry and — when it
	// spills — its own storage instance on its own directory. Isolation is exactly
	// this: nothing here is shared between two Backends, so a Backend that stops
	// answering fills its own queue and spills to its own disk (ADR 0010).
	for _, backend := range declaration.Backends {
		config.Exporters[backendExporter(backend)] = backendExport(backend)

		if backend.Delivery.Spill {
			if config.Extensions == nil {
				config.Extensions = map[string]any{}
			}
			config.Extensions[backendStorage(backend)] = map[string]any{
				"directory": spillDirectory(declaration.SpillRoot, backend),
				// The directory is a mount point's subdirectory, so on a fresh volume it
				// does not exist yet. Without this the Gateway refuses to start on the
				// first rollout after a Backend is given spill.
				"create_directory": true,
			}
			config.Service.Extensions = append(config.Service.Extensions, backendStorage(backend))
		}
	}

	// One pipeline per Signal, unconditionally. An Agent's pipelines are the Signals
	// one Contract declares; the Gateway is shared by the whole fleet, so it must
	// relay anything any Contract could declare.
	//
	// Which Backends each pipeline fans out to is per Signal, though: a metrics
	// store has no traces pipeline to receive into, and sending it traces would
	// produce export failures indistinguishable from it being down.
	for _, signal := range contract.Signals() {
		exporters := make([]string, 0, len(declaration.Backends))
		for _, backend := range declaration.Backends {
			if backend.Receives(signal) {
				exporters = append(exporters, backendExporter(backend))
			}
		}
		config.Service.Pipelines[string(signal)] = Pipeline{
			Receivers:  []string{otlpReceiver},
			Processors: processors,
			Exporters:  exporters,
		}
	}

	// The Gateway's own telemetry, straight to one Backend and past everything
	// above it. Not through a pipeline, so: not behind the memory limiter that is
	// refusing data, not tagged by the Guardrail (its own signals are not a service
	// making a claim), not batched, and above all not queued behind the exporter
	// whose queue depth it is reporting. This is the "direct Backend route not
	// gated on the same failing exporter" ADR 0010 asks for, and it is why one
	// Backend stalling is answerable rather than inferred.
	config.Service.Telemetry = selfTelemetry(
		declaration.SelfTelemetry.ResourceAttributes,
		endpointOf(declaration, declaration.SelfTelemetry.Backend),
	)
	if err := config.stampConfigVersion(); err != nil {
		return CollectorConfig{}, err
	}
	return config, nil
}

// endpointOf is a named Backend's endpoint, or "" if the declaration has no such
// Backend. CompileGateway has already refused the second case.
func endpointOf(declaration GatewayDeclaration, name string) string {
	for _, backend := range declaration.Backends {
		if backend.Name == name {
			return backend.Endpoint
		}
	}
	return ""
}

// backendExporter is the collector component ID for a Backend's exporter. Named
// after the Backend so that a Gateway fanning out to several reads as a list of
// destinations rather than otlp/1, otlp/2 — and so that the per-Backend metrics
// C7 (#15) will read are labelled by something a human recognises.
func backendExporter(backend Backend) string {
	return otlpReceiver + "/" + backend.Name
}

// backendStorage is the collector component ID for one Backend's spill storage.
// Named after the Backend for the same reason its exporter is: two Backends
// sharing a storage instance share a file lock, a disk budget and a corruption
// blast radius, which is the coupling per-Backend queues exist to remove.
func backendStorage(backend Backend) string {
	return fileStorageExtension + "/" + backend.Name
}

// spillDirectory is where one Backend's persistent queue lives: its own
// subdirectory of the Gateway's spill volume, named after it.
func spillDirectory(root string, backend Backend) string {
	return path.Join(root, backend.Name)
}

// backendExport is the OTLP exporter aimed at one Backend, with that Backend's own
// delivery durability. Same omission rule as the Agent's: a setting that would do
// nothing is not emitted.
func backendExport(backend Backend) map[string]any {
	exporter := map[string]any{"endpoint": backend.Endpoint}

	if backend.Delivery.QueueSize > 0 {
		queue := map[string]any{
			"enabled":    true,
			"queue_size": backend.Delivery.QueueSize,
		}
		// A queue named a storage extension is written to disk instead of held in
		// memory, so it survives the Gateway restarting while this Backend is down.
		// The extension is this Backend's own; nothing else writes to it.
		if backend.Delivery.Spill {
			queue["storage"] = backendStorage(backend)
		}
		exporter["sending_queue"] = queue
	}
	if backend.Delivery.Retry {
		exporter["retry_on_failure"] = map[string]any{"enabled": true}
	}
	return exporter
}

// listenOn is the receiver endpoint for an address Agents forward to: the same
// port, on every interface.
func listenOn(address string) string {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return ""
	}
	return "0.0.0.0:" + port
}

// gatewayExport is the OTLP exporter aimed at the Gateway, with the durability
// the tier's Profile asks for. Retry is off by default in the collector, so the
// Profile saying retry: false means the block is simply absent — the compiled
// config never carries a setting that does nothing.
func gatewayExport(profile Profile) map[string]any {
	exporter := map[string]any{"endpoint": profile.GatewayEndpoint}

	if profile.Delivery.QueueSize > 0 {
		exporter["sending_queue"] = map[string]any{
			"enabled":    true,
			"queue_size": profile.Delivery.QueueSize,
		}
	}
	if profile.Delivery.Retry {
		exporter["retry_on_failure"] = map[string]any{"enabled": true}
	}
	return exporter
}

// resourceAttributes is the Contract's resource attributes as collector upsert
// actions, in a stable order so two compiles of one Contract are identical.
func resourceAttributes(c contract.Contract) []any {
	keys := make([]string, 0, len(c.ResourceAttributes))
	for key := range c.ResourceAttributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	actions := make([]any, 0, len(keys)+1)
	for _, key := range keys {
		actions = append(actions, map[string]any{
			"key":    key,
			"value":  c.ResourceAttributes[key],
			"action": "upsert",
		})
	}

	// LAST, unconditionally: strip the namespace the platform uses to describe its
	// own collectors. Nothing stops a service's SDK setting
	// otel.platform.config_version on its resource, and an Agent forwards what it is
	// given — so the one field a Rollout is confirmed by would be answerable by the
	// thing being rolled out. The Gateway cannot do this instead: by the time
	// telemetry reaches it, an Agent's legitimate stamp and a service's forgery are
	// the same attribute on the same kind of record. The Agent can, because its own
	// signals do not travel its own pipelines.
	//
	// After the upserts rather than before, so that nothing — including a Contract
	// that somehow reached here declaring one — can put the namespace back.
	return append(actions, map[string]any{
		"action":  "delete",
		"pattern": "^" + regexp.QuoteMeta(platformNamespace),
	})
}

// Collects reports whether the config has a pipeline for a Signal.
func (cc CollectorConfig) Collects(signal string) bool {
	_, running := cc.Service.Pipelines[signal]
	return running
}

// Signals is every Signal this config collects, in a stable order.
func (cc CollectorConfig) Signals() []string {
	signals := make([]string, 0, len(cc.Service.Pipelines))
	for signal := range cc.Service.Pipelines {
		signals = append(signals, signal)
	}
	sort.Strings(signals)
	return signals
}

// Validate checks the config is internally coherent: every component a pipeline
// names is defined, and every defined component is used.
//
// This is referential integrity, not full collector validation — proving a config
// is one a given otelcol build accepts needs that binary, which is a follow-up.
// Dangling references are the failure this class of generator actually produces,
// and an unused component means a Profile setting silently did nothing.
func (cc CollectorConfig) Validate() error {
	if len(cc.Service.Pipelines) == 0 {
		return fmt.Errorf("the compiled config runs no pipelines, so it collects nothing")
	}

	used := map[string]bool{}
	for signal, pipeline := range cc.Service.Pipelines {
		for _, group := range []struct {
			kind    string
			names   []string
			defined map[string]any
		}{
			{"receiver", pipeline.Receivers, cc.Receivers},
			{"processor", pipeline.Processors, cc.Processors},
			{"exporter", pipeline.Exporters, cc.Exporters},
		} {
			if len(group.names) == 0 {
				return fmt.Errorf("the %s pipeline names no %s", signal, group.kind)
			}
			for _, name := range group.names {
				if _, defined := group.defined[name]; !defined {
					return fmt.Errorf("the %s pipeline names %s %q, which the config does not define", signal, group.kind, name)
				}
				used[group.kind+"/"+name] = true
			}
		}
	}

	// Extensions serve the collector rather than a pipeline, so they are checked
	// against the service block instead. Both directions are silent failures: a
	// queue naming storage that does not exist stops the collector at load, and
	// storage the service block never starts is inert — the queue stays in memory
	// while the config, the mounted volume and every reviewer say otherwise.
	for _, name := range cc.Service.Extensions {
		if _, defined := cc.Extensions[name]; !defined {
			return fmt.Errorf("the config runs extension %q, which it does not define", name)
		}
		used["extension/"+name] = true
	}

	// The third reference a spilling config makes, and the only one not visible from
	// the service block: an exporter's queue names the storage it spills to. Whether
	// that lines up is currently kept true by two separate branches in assembly
	// agreeing, which is not the same as being checked.
	for exporter, definition := range cc.Exporters {
		storage, spills := spillStorageOf(definition)
		if !spills {
			continue
		}
		if !used["extension/"+storage] {
			return fmt.Errorf(
				"the %s exporter's sending queue spills to storage %q, which the config does not run — the collector would refuse this at load",
				exporter, storage)
		}
	}

	// A collector that runs pipelines has to report on itself, and the report has to
	// be one somebody could act on. All three of these read as self-observation and
	// deliver none: no block at all, readers with nowhere to go, or a report with no
	// config_version — a collector an operator can see and cannot match to a
	// Rollout. It is the spill failure again — configured, inert, indistinguishable
	// from working — and there is no status channel to notice it from (ADR 0010),
	// which is exactly why the refusal has to be here.
	//
	// A completeness check rather than referential integrity, like the refusal of a
	// config that runs no pipelines above it.
	switch {
	case cc.Service.Telemetry == nil:
		return fmt.Errorf("the config emits no telemetry about itself, so the collector running it would be invisible on the fleet and indistinguishable from one that is healthy")
	case len(cc.Service.Telemetry.Metrics.Readers) == 0:
		return fmt.Errorf("the config declares its own telemetry but names no reader for it, so it emits nothing about itself while reading as though it does")
	case cc.ConfigVersion() == "":
		return fmt.Errorf("the config emits telemetry about itself but no %s, so nothing it reports could be matched to a Rollout", configVersionAttribute)
	}

	for kind, defined := range map[string]map[string]any{
		"receiver": cc.Receivers, "processor": cc.Processors, "exporter": cc.Exporters,
	} {
		for name := range defined {
			if !used[kind+"/"+name] {
				return fmt.Errorf("the config defines %s %q but no pipeline uses it", kind, name)
			}
		}
	}
	for name := range cc.Extensions {
		if !used["extension/"+name] {
			return fmt.Errorf("the config defines extension %q but the service block does not run it, so it does nothing", name)
		}
	}
	return nil
}

// spillStorageOf is the storage extension an exporter's sending queue spills to,
// and whether it spills at all. Reading it back out of the emitted map rather than
// off the Backend keeps Validate a property of the config, checkable on one a
// caller assembled or loaded.
func spillStorageOf(exporter any) (string, bool) {
	definition, isMap := exporter.(map[string]any)
	if !isMap {
		return "", false
	}
	queue, queued := definition["sending_queue"].(map[string]any)
	if !queued {
		return "", false
	}
	storage, spills := queue["storage"].(string)
	return storage, spills && storage != ""
}

// YAML renders the config as a collector configuration file.
func (cc CollectorConfig) YAML() ([]byte, error) {
	rendered, err := yaml.Marshal(cc)
	if err != nil {
		return nil, fmt.Errorf("render collector config: %w", err)
	}
	return rendered, nil
}
