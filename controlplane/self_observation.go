package controlplane

import (
	"fmt"
	"maps"
	"strings"
)

// The platform observing itself (ADR 0010, ADR 0016).
//
// Every collector this Control Plane compiles emits its own OTEL: the
// configuration it is running, and the queue, export-failure and drop counters
// that say whether it is keeping up. There is no status API, no heartbeat and no
// back-channel of any kind, deliberately — ADR 0006 refused a control protocol
// and ADR 0010 refused to smuggle one back in as a health endpoint. What an
// operator watches is telemetry, arriving in a Backend, alongside the fleet's.
//
// WHAT config_version IS, AND WHY IT IS DERIVED THIS WAY
//
// A Rollout is confirmed when the expected config_version appears fleet-wide.
// That makes the derivation load-bearing: a version that does not identify what
// is running is worse than no version at all, because an operator would trust it.
//
// It is the sha256 of the rendered collector configuration WITH THE VERSION
// ATTRIBUTE ITSELF EXCLUDED. Hashing the config including the stamp is circular
// — writing the digest in changes the digest — so the one field that cannot be
// covered is the field carrying the answer, and nothing else is left out.
//
// The alternative was to hash the compile INPUTS (Contract + Profile + taxonomy
// + Standard catalog). Rejected: a comment or a description edited in
// controlplane/profiles.yaml would then change every Agent's config_version
// fleet-wide while not one byte of what runs had changed — a Rollout that has to
// be confirmed and never can be, because no collector's file changed and so no
// collector restarts. Hashing the artefact makes the version mean exactly "this
// is the pipeline I am running", which is the only claim an operator can act on.
//
// The cost is the mirror image and it is the honest one: an input that does not
// reach the compiled config does not move the version. `sampling.traces_percent`
// is the standing example — nothing reads it yet (#40), so changing it changes no
// Agent's version. That is correct rather than a gap: nothing about what runs
// changed.
//
// HOW IT RELATES TO THE ROLLOUT MANIFEST'S DIGEST
//
// They are two hashes of two different things, answering two different questions,
// and they are never equal:
//
//	digest          sha256 of the compiled FILE, header and stamp included. "Is
//	                the file in this repo still the one this Rollout wrote?" It
//	                covers the config_version attribute, so a hand-edited stamp
//	                is caught by it.
//	config_version  sha256 of the collector configuration with the stamp
//	                excluded, and no header. "Is the pipeline running out on the
//	                fleet the one this Rollout compiled?" It is what a collector
//	                reports about itself, so it is the only one of the two that
//	                can be compared against telemetry.
//
// Comparing a digest to a version seen in telemetry is a category error. They
// cannot coincide by construction — the digest hashes a strict superset of the
// version's input — and a test asserts it rather than a comment claiming it.
//
// THE INDEPENDENT EXPORT PATH
//
// ADR 0010 names a bootstrap dependency: if a collector's export path is broken,
// the telemetry saying so may not escape along it. So the platform's own signals
// do not travel the pipeline they report on. `service.telemetry.metrics.readers`
// is a separate OTLP client with no sending queue, no retry, no batch processor
// and no memory limiter in front of it — none of the machinery whose failure it
// exists to report. A full queue on `otlp/gateway` cannot hold up the metric that
// says the queue is full, and a Backend that stops answering cannot hold up the
// Gateway's report that it stopped answering.
//
// It is not independent of everything, and the residual is written down rather
// than glossed: an Agent's only next hop is the Gateway (ADR 0007 — an Agent
// names no Backend), so an Agent cannot report through a Gateway that is down.
// What reports that is the Gateway's own path, which goes straight to a Backend.
// A collector that is not running reports nothing at all, and absence is what
// says so.

const (
	// platformNamespace is the resource-attribute namespace the platform uses to
	// say things about ITSELF. It is the Gateway's `otel.guardrail.` namespace
	// applied to the other half of ADR 0010: one prefix, so an operator can ask for
	// the platform's own signals in one query, and so a service cannot answer that
	// query by accident.
	platformNamespace = "otel.platform."
	// configVersionAttribute is the collector configuration a process is running,
	// as it reports it. `config_version` is the name ADR 0010 gives the concept;
	// the namespace is what makes it unambiguous on a record that a hundred teams
	// also write resource attributes onto.
	configVersionAttribute = platformNamespace + "config_version"

	// selfTelemetryLevel is how much of its own instrumentation a collector emits.
	// `normal` is the collector's current default, and it is written out anyway:
	// the queue-depth, export-failure and drop counters C7 exists to route are
	// exactly what the level gates, so leaving it implicit would let a default that
	// moves in a later collector release take the platform's back-pressure signals
	// away without changing a line of this repository.
	selfTelemetryLevel = "normal"
	// selfTelemetryIntervalMillis is how often a collector exports its own metrics,
	// and therefore how quickly a Rollout can be confirmed. Fleet-wide and not per
	// Service Tier: what varies per tier is how a SERVICE's telemetry ships, and
	// this is the platform reporting on itself at a cadence the platform owns.
	selfTelemetryIntervalMillis = 30000
)

// CollectorTelemetry is the `service.telemetry` block: what a collector says
// about itself, and where it says it.
//
// It is not a pipeline. Nothing in `receivers`, `processors` or `exporters`
// touches it, which is the entire point — see the independent export path above.
type CollectorTelemetry struct {
	// Resource is the identity every one of this collector's own signals carries.
	// A map rather than the pipeline's list-of-upsert-actions, because that is the
	// shape the collector's own telemetry takes, and because a map marshals in key
	// order — so two compiles of one input render identically.
	Resource map[string]string `yaml:"resource"`
	Metrics  TelemetryMetrics  `yaml:"metrics"`
}

// TelemetryMetrics is how a collector exports its own metrics.
type TelemetryMetrics struct {
	Level string `yaml:"level"`
	// Readers is where the metrics go. Naming any reader replaces the collector's
	// default — which is a Prometheus endpoint to be scraped, and a scrape endpoint
	// is the status back-channel ADR 0010 refuses. The platform pushes its own
	// telemetry down the same protocol as everything else it emits.
	Readers []any `yaml:"readers"`
}

// selfTelemetry is a collector's own-telemetry block: an identity, and one
// periodic OTLP reader aimed at the next hop.
//
// The resource is COPIED into a fresh map, never aliased and never cloned. Two
// separate reasons, and both have teeth:
//
//   - The Agent's identity is its service's Telemetry Contract. Handing the
//     compiled config a live reference to the Contract's map would let a later
//     caller edit what a running collector claims to be.
//   - NO attributes at all is legal input here and reaches it: a Telemetry
//     Contract may omit `resource_attributes:` entirely, which violates several
//     Standards and is a Preflight Guardrail's finding rather than a reason the
//     compiler cannot produce a config. `maps.Clone` of nothing is nil, and the
//     stamp has to go somewhere — so the map is allocated first and copied into.
func selfTelemetry(resource map[string]string, endpoint string) *CollectorTelemetry {
	identity := make(map[string]string, len(resource)+1)
	maps.Copy(identity, resource)

	return &CollectorTelemetry{
		Resource: identity,
		Metrics: TelemetryMetrics{
			Level:   selfTelemetryLevel,
			Readers: []any{periodicOTLPReader(endpoint)},
		},
	}
}

// periodicOTLPReader exports a collector's own metrics over OTLP, on a timer.
//
// Deliberately bare. There is no sending queue and no retry, and that is the
// property rather than an omission: a queue is state that fills, and the thing
// most likely to fill it is exactly the outage this reader exists to report. An
// interval it misses is one interval of self-telemetry lost; a queue it blocked
// on would be the platform going quiet at the only moment anybody was looking.
//
// The endpoint is a URL rather than the host:port the pipeline exporters use,
// because that is the shape this reader takes. `https` is not decoration: the
// scheme is what decides whether the hop is encrypted, so a compiled config that
// dropped it would downgrade the platform's own telemetry to plaintext silently.
func periodicOTLPReader(endpoint string) any {
	return map[string]any{
		"periodic": map[string]any{
			"interval": selfTelemetryIntervalMillis,
			"exporter": map[string]any{
				"otlp": map[string]any{
					"protocol": "grpc",
					"endpoint": "https://" + endpoint,
				},
			},
		},
	}
}

// ConfigVersion is the collector configuration this config reports itself as
// running, or "" if it reports none.
func (cc CollectorConfig) ConfigVersion() string {
	if cc.Service.Telemetry == nil {
		return ""
	}
	return cc.Service.Telemetry.Resource[configVersionAttribute]
}

// stampConfigVersion writes the config's own identity into it.
//
// The exclusion is the whole trick. The attribute is removed before rendering and
// written after, so what is hashed is every byte of the configuration except the
// answer — which cannot be included without the digest changing whatever it is
// set to. Everything else that decides what the collector does is inside.
func (cc *CollectorConfig) stampConfigVersion() error {
	if cc.Service.Telemetry == nil {
		return fmt.Errorf("cannot stamp a config_version on a config that emits no telemetry of its own")
	}

	delete(cc.Service.Telemetry.Resource, configVersionAttribute)
	rendered, err := cc.YAML()
	if err != nil {
		return err
	}
	cc.Service.Telemetry.Resource[configVersionAttribute] = digestOf(rendered)
	return nil
}

// inThePlatformNamespace reports whether an attribute name is one the platform
// reserves for what it says about itself.
func inThePlatformNamespace(attribute string) bool {
	return strings.HasPrefix(attribute, platformNamespace)
}
