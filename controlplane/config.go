package controlplane

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/ghantakiran/OTEL/contract"
)

// CollectorConfig is a compiled OpenTelemetry Collector configuration for one
// service's Agent.
//
// It is a typed value rather than a string of YAML on purpose: the Control Plane
// wants to ask questions of it before it ever reaches a file — does it collect
// the Signals we require, does every pipeline reference a component that exists —
// and a caller comparing two rollouts wants to diff structure, not formatting.
// YAML is one rendering of it, not the thing itself.
type CollectorConfig struct {
	Receivers  map[string]any   `yaml:"receivers"`
	Processors map[string]any   `yaml:"processors"`
	Exporters  map[string]any   `yaml:"exporters"`
	Service    CollectorService `yaml:"service"`
}

// CollectorService is the collector's `service` block: which pipelines run, and
// what each is wired from.
type CollectorService struct {
	Pipelines map[string]Pipeline `yaml:"pipelines"`
}

// Pipeline is one signal's path through the Agent.
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
)

// assemble builds the config from the three inputs. Kept separate from Compile so
// that Compile reads as the decisions and this reads as the shape.
func assemble(c contract.Contract, profile Profile, signals []contract.Signal) CollectorConfig {
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
	return config
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

	actions := make([]any, 0, len(keys))
	for _, key := range keys {
		actions = append(actions, map[string]any{
			"key":    key,
			"value":  c.ResourceAttributes[key],
			"action": "upsert",
		})
	}
	return actions
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

	for kind, defined := range map[string]map[string]any{
		"receiver": cc.Receivers, "processor": cc.Processors, "exporter": cc.Exporters,
	} {
		for name := range defined {
			if !used[kind+"/"+name] {
				return fmt.Errorf("the config defines %s %q but no pipeline uses it", kind, name)
			}
		}
	}
	return nil
}

// YAML renders the config as a collector configuration file.
func (cc CollectorConfig) YAML() ([]byte, error) {
	rendered, err := yaml.Marshal(cc)
	if err != nil {
		return nil, fmt.Errorf("render collector config: %w", err)
	}
	return rendered, nil
}
