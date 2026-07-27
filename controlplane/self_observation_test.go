package controlplane_test

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/ghantakiran/OTEL/controlplane"
)

func TestACompiledAgentReportsItsOwnConfigVersion(t *testing.T) {
	// A Rollout is confirmed by the expected config_version appearing fleet-wide in
	// telemetry, and by nothing else — there is no status back-channel and none is
	// to be built (ADR 0010). That only works if a collector says which
	// configuration it is running, in its own telemetry, unprompted.
	config, err := controlplane.Compile(tierOneService(), taxonomy(t), profiles(t))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	version := config.ConfigVersion()
	if version == "" {
		t.Fatal("the compiled Agent reports no config_version, so a Rollout could never be confirmed")
	}
}

func TestConfigVersionChangesWhenTheProfileChanges(t *testing.T) {
	// Half of what makes the version worth trusting. A Pipeline Profile is how a
	// service's telemetry ships, so changing one changes what the Agent runs — and
	// a version that did not move would confirm a Rollout that had not happened.
	before := agentConfigVersion(t, profilesBatchingAfter(t, "5s"))
	after := agentConfigVersion(t, profilesBatchingAfter(t, "30s"))

	if before == after {
		t.Errorf("the Profile's batch timeout changed and config_version did not (%s): a Rollout would confirm itself against the version it was already reporting", before)
	}
}

func TestConfigVersionDoesNotChangeWhenTheInputsDoNot(t *testing.T) {
	// The other half, and the one a Rollout depends on to be a no-op. If a recompile
	// produced a new version from unchanged inputs, every rollout would rewrite every
	// compiled file, every Agent would restart, and a version an operator was waiting
	// on would be stale before it arrived.
	//
	// Compiled from a FRESH Profile set each time — a new file, parsed again — so
	// what is asserted is that the derivation is a function of the inputs, not that
	// one in-memory value was reused.
	first := agentConfigVersion(t, profilesBatchingAfter(t, "5s"))

	for range 20 {
		if again := agentConfigVersion(t, profilesBatchingAfter(t, "5s")); again != first {
			t.Fatalf("recompiling the same Contract and Profile produced a different config_version:\n first %s\n again %s", first, again)
		}
	}
}

func TestConfigVersionChangesWhenTheTelemetryContractChanges(t *testing.T) {
	// A Profile is one of the two inputs; the Contract is the other. A service that
	// changes what it declares gets a new compiled config, so it must get a new
	// version — otherwise the one Rollout an operator most wants to confirm, the one
	// they asked a team to make, is the one that confirms silently against the old.
	before, err := controlplane.Compile(tierOneService(), taxonomy(t), profiles(t))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	renamed := tierOneService()
	renamed.ResourceAttributes["service.version"] = "2.4.2"
	after, err := controlplane.Compile(renamed, taxonomy(t), profiles(t))
	if err != nil {
		t.Fatalf("compile the changed Contract: %v", err)
	}

	if before.ConfigVersion() == after.ConfigVersion() {
		t.Errorf("the Contract's service.version changed and config_version did not (%s)", before.ConfigVersion())
	}
}

func TestTheRolloutManifestRecordsTheConfigVersionEachServiceWillReport(t *testing.T) {
	// "Confirmed fleet-wide" needs something to confirm AGAINST, and it is the
	// committed Manifest: for every service, the version its Agent should be
	// reporting once the Rollout has landed. Without it an operator can see the
	// versions their fleet reports and has nothing to compare them to but a
	// recompile.
	root := fleetWith(t, map[string]string{
		"checkout-api": contractYAML("checkout-api", "tier-1", "traces", "metrics", "logs"),
	})
	rollout := compileFleet(t, root)

	compiled := rollout.Compiled[0]
	if compiled.ConfigVersion == "" {
		t.Fatal("the Rollout records no config_version for checkout-api, so nothing says what its Agent should report")
	}

	manifest, err := rollout.ManifestYAML()
	if err != nil {
		t.Fatalf("render the Manifest: %v", err)
	}
	if !strings.Contains(string(manifest), compiled.ConfigVersion) {
		t.Errorf("the Rollout Manifest does not carry checkout-api's config_version:\n%s", manifest)
	}
}

func TestAServicesConfigVersionIsNotTheDigestOfTheFileThatCarriesIt(t *testing.T) {
	// Two sha256 values sit side by side in the Rollout Manifest, and a reader who
	// took one for the other would confirm a Rollout against a hash no collector
	// ever reports. They cannot coincide: the digest covers the generated header and
	// the config_version attribute itself, which the version by construction cannot
	// cover. This asserts the difference rather than a comment claiming it.
	root := fleetWith(t, map[string]string{
		"checkout-api":  contractYAML("checkout-api", "tier-1", "traces", "metrics", "logs"),
		"payments-edge": contractYAML("payments-edge", "tier-1", "traces", "metrics", "logs"),
	})

	for _, service := range compileFleet(t, root).Compiled {
		if service.Digest == service.ConfigVersion {
			t.Errorf("%s's file digest and its config_version are the same value (%s): one of the two is not hashing what it claims to",
				service.ServiceName, service.Digest)
		}
	}
}

func TestAnAgentsOwnTelemetryCarriesTheSameIdentityItStampsOnTheServices(t *testing.T) {
	// The Agent's own signals reach the Gateway through the same pipeline as
	// everything else, so a Pipeline Guardrail judges them (C6). It must judge them
	// the same way it judges the service — no exemption, because an exemption would
	// be a resource attribute, and a resource attribute is exactly the thing a
	// service can forge. Instead the Agent carries the identity its Telemetry
	// Contract declares, so a compliant service's Agent is compliant for the same
	// reason the service is, and a drifted one's is drifted for the same reason.
	//
	// It is also what makes the signals findable: an operator asking why THIS
	// service's telemetry is thin gets the Agent's queue depth filed under it.
	config, err := controlplane.Compile(tierOneService(), taxonomy(t), profiles(t))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	for attribute, value := range tierOneService().ResourceAttributes {
		if got := config.Service.Telemetry.Resource[attribute]; got != value {
			t.Errorf("the Agent's own telemetry declares %s=%q, the Telemetry Contract says %q", attribute, got, value)
		}
	}
}

func TestAnAgentReportsOnItsExporterWithoutGoingThroughIt(t *testing.T) {
	// The bootstrap dependency ADR 0010 names, answered. The whole value of a
	// queue-depth metric is that it arrives WHILE the queue is full, so it must not
	// be behind that queue: the reader is its own OTLP client with no sending queue,
	// no retry and no processor in front of it, and it names none of the pipeline's
	// exporters. Same address as the pipeline — the Gateway is the only hop an Agent
	// knows (ADR 0007) — and a different client.
	config, err := controlplane.Compile(tierOneService(), taxonomy(t), profiles(t))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	readers, err := yaml.Marshal(config.Service.Telemetry.Metrics.Readers)
	if err != nil {
		t.Fatalf("render the readers: %v", err)
	}
	rendered := string(readers)

	profile, _ := profiles(t).For("tier-1")
	if !strings.Contains(rendered, profile.GatewayEndpoint) {
		t.Errorf("the Agent's own metrics do not go to the Gateway the Profile names:\n%s", rendered)
	}
	for name := range config.Exporters {
		if strings.Contains(rendered, name) {
			t.Errorf("the Agent's own metrics travel through %q, the very exporter whose queue they report on:\n%s", name, rendered)
		}
	}
	for _, backpressure := range []string{"sending_queue", "retry_on_failure", "memory_limiter", "batch"} {
		if strings.Contains(rendered, backpressure) {
			t.Errorf("the Agent's own metrics sit behind %s, so the outage they exist to report would hold them up:\n%s", backpressure, rendered)
		}
	}
}

func TestAnAgentStaysOnTheCollectorsCoreDistribution(t *testing.T) {
	// ADR 0014 put the GATEWAY on contrib, for spill, and drew the line there: an
	// Agent runs beside every service on the platform, so it is where a bigger image
	// and a wider attack surface cost the most. Self-observation is the first thing
	// since to be added to an Agent, and it does not move that line — a periodic
	// OTLP reader under `service.telemetry` is in core, which is why the platform's
	// own signals can be pushed rather than scraped.
	config, err := controlplane.Compile(tierOneService(), taxonomy(t), profiles(t))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	core := map[string]bool{"otlp": true, "otlp/gateway": true, "batch": true, "resource": true, "memory_limiter": true}
	for kind, components := range map[string]map[string]any{
		"receiver": config.Receivers, "processor": config.Processors, "exporter": config.Exporters,
	} {
		for name := range components {
			if !core[name] {
				t.Errorf("the compiled Agent names %s %q, which is not one of the core components an Agent is allowed", kind, name)
			}
		}
	}
	// The one extension this platform compiles is `file_storage`, and it is the one
	// component that is not in core (ADR 0014). An Agent has none.
	if len(config.Extensions) != 0 || len(config.Service.Extensions) != 0 {
		t.Errorf("the compiled Agent runs extensions (%v); the only one this platform compiles is the contrib spill storage", config.Service.Extensions)
	}
}

func TestAServiceCannotDeclareAnAttributeInThePlatformsOwnNamespace(t *testing.T) {
	// `otel.platform.config_version` is what an operator reads to decide whether a
	// Rollout has landed. A Contract declaring it would have the Agent stamp a
	// version of the service's choosing onto every record that service emits — and
	// a fleet where one service always reports the expected version is a fleet that
	// confirms rollouts it never received.
	//
	// Refused where it is written, rather than filtered later: the compiled config
	// would otherwise contain a resource processor upserting it, which is the
	// forgery made durable and reviewed as normal.
	forging := tierOneService()
	forging.ResourceAttributes["otel.platform.config_version"] = "sha256:whatever-you-are-waiting-for"

	err := mustNotCompile(t, forging, "a Contract claimed the platform's own namespace")

	if !strings.Contains(err.Error(), "otel.platform.config_version") {
		t.Errorf("the error does not name the attribute it refused: %v", err)
	}
}

func TestAnAgentStripsThePlatformNamespaceFromWhatTheServiceSends(t *testing.T) {
	// Refusing the Contract closes the declaration route; this closes the runtime
	// one. Nothing stops a service's SDK setting otel.platform.config_version on its
	// own resource, and an Agent forwards what it is given — so the query an
	// operator confirms a Rollout with would answer from the service rather than
	// from the collector.
	//
	// The Agent is the right place, and it is the only place: an Agent's OWN signals
	// do not pass through its pipelines (that is the independent export path), so it
	// can delete the namespace from everything that does, without deleting its own
	// answer. The Gateway cannot make that distinction — by the time telemetry
	// reaches it, a collector's legitimate stamp and a service's forgery are the
	// same resource attribute.
	//
	// LAST, after the upserts. Deleting first would leave a Contract's own
	// declaration free to re-add it — and the last action is the one that decides.
	config, err := controlplane.Compile(tierOneService(), taxonomy(t), profiles(t))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	actions, ok := config.Processors["resource"].(map[string]any)["attributes"].([]any)
	if !ok || len(actions) == 0 {
		t.Fatalf("the compiled Agent's resource processor has no actions: %v", config.Processors["resource"])
	}
	last, ok := actions[len(actions)-1].(map[string]any)
	if !ok {
		t.Fatalf("the resource processor's last action is not an action: %v", actions[len(actions)-1])
	}
	if last["action"] != "delete" {
		t.Fatalf("the resource processor's last action is %v, not the delete that strips the platform's namespace", last["action"])
	}
	pattern, _ := last["pattern"].(string)
	if !regexp.MustCompile(pattern).MatchString("otel.platform.config_version") {
		t.Errorf("the resource processor's last action deletes %q, which does not match a forged otel.platform.config_version", pattern)
	}
	if regexp.MustCompile(pattern).MatchString("service.name") {
		t.Errorf("the resource processor's last action deletes %q, which also matches the service's own attributes", pattern)
	}
}

func TestACompiledGatewayReportsItsOwnConfigVersion(t *testing.T) {
	// A Rollout reaches the Gateway too, and the Gateway is the one component the
	// whole fleet's telemetry passes through — so "did the new configuration land?"
	// matters more there than anywhere. It is answered the same way and by the same
	// derivation: no status endpoint, no second mechanism.
	config, err := controlplane.CompileGateway(gatewayDeclaration(t), profiles(t), orgStandards(t))
	if err != nil {
		t.Fatalf("compile the Gateway: %v", err)
	}

	if config.ConfigVersion() == "" {
		t.Fatal("the compiled Gateway reports no config_version, so a Rollout reaching it could never be confirmed")
	}
}

func TestAGatewayThatSaysNothingAboutItselfDoesNotCompile(t *testing.T) {
	// Absence is silence, and this is the one place on the platform where silence
	// is indistinguishable from health: a Gateway that emits no telemetry of its own
	// looks exactly like a Gateway that is fine. There is no status endpoint to ask
	// instead — ADR 0010 refused one and refuses one still — so the declaration is
	// where "nobody can see this Gateway" has to be caught.
	silent := gatewayFrom(t, `apiVersion: guardrail.otel/v1
kind: GatewayDeclaration
gateway:
  address: otel-gateway.observability.svc.cluster.local:4317
  batch:
    timeout: 5s
    send_batch_size: 8192
  backends:
    - backend: primary-apm
      endpoint: apm-otlp.observability.svc.cluster.local:4317
`)

	err := mustNotCompileGateway(t, silent, "the Gateway declares no self_telemetry")

	if !strings.Contains(err.Error(), "self_telemetry") {
		t.Errorf("the error does not name what is missing: %v", err)
	}
}

func TestTheGatewayReportsOnItsBackendsWithoutGoingThroughAnyOfThem(t *testing.T) {
	// The bootstrap dependency at the point where it bites hardest. A Backend that
	// has stopped answering is discovered from the Gateway's queue-depth metric for
	// that Backend — so if that metric queued behind the Gateway's own exporters,
	// the Backend going down would take with it the news that it had. Straight to
	// one declared Backend, on a client with no queue, no retry, no batch and no
	// memory limiter in front of it.
	declaration := gatewayDeclaration(t)
	config, err := controlplane.CompileGateway(declaration, profiles(t), orgStandards(t))
	if err != nil {
		t.Fatalf("compile the Gateway: %v", err)
	}

	readers, err := yaml.Marshal(config.Service.Telemetry.Metrics.Readers)
	if err != nil {
		t.Fatalf("render the readers: %v", err)
	}
	rendered := string(readers)

	var destination string
	for _, backend := range declaration.Backends {
		if backend.Name == declaration.SelfTelemetry.Backend {
			destination = backend.Endpoint
		}
	}
	if destination == "" {
		t.Fatalf("the declaration's self_telemetry backend %q is not one of its Backends", declaration.SelfTelemetry.Backend)
	}
	if !strings.Contains(rendered, destination) {
		t.Errorf("the Gateway's own metrics do not go to the Backend it named (%s):\n%s", destination, rendered)
	}
	for name := range config.Exporters {
		if strings.Contains(rendered, name) {
			t.Errorf("the Gateway's own metrics travel through %q, one of the exporters whose queue depth they report:\n%s", name, rendered)
		}
	}
	for _, backpressure := range []string{"sending_queue", "retry_on_failure", "memory_limiter", "batch", "transform"} {
		if strings.Contains(rendered, backpressure) {
			t.Errorf("the Gateway's own metrics sit behind %s, so the outage they exist to report would hold them up:\n%s", backpressure, rendered)
		}
	}
}

func TestAGatewayWhoseOwnSignalsWouldGoNowhereUsefulDoesNotCompile(t *testing.T) {
	// Both of these compile a Gateway whose own telemetry is exported into a hole,
	// and both are invisible afterwards for the same circular reason: the telemetry
	// that would have reported the failure is the telemetry being lost.
	for name, declared := range map[string]struct{ backend, names string }{
		"a Backend nobody declared": {
			backend: "observability-store",
			names: `    - backend: primary-apm
      endpoint: apm-otlp.observability.svc.cluster.local:4317
`,
		},
		"a Backend that does not receive metrics": {
			backend: "cold-archive",
			names: `    - backend: primary-apm
      endpoint: apm-otlp.observability.svc.cluster.local:4317
    - backend: cold-archive
      endpoint: archive-otlp.observability.svc.cluster.local:4317
      signals: [traces, logs]
`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := mustNotCompileGateway(t, gatewayFrom(t, `apiVersion: guardrail.otel/v1
kind: GatewayDeclaration
gateway:
  address: otel-gateway.observability.svc.cluster.local:4317
  batch:
    timeout: 5s
    send_batch_size: 8192
  backends:
`+declared.names+`  self_telemetry:
    backend: `+declared.backend+`
    resource_attributes:
      service.name: otel-gateway
      service.version: 0.127.0
      deployment.environment: production
`), "the Gateway's own signals go to "+name)

			if !strings.Contains(err.Error(), declared.backend) {
				t.Errorf("the error does not name the Backend it refused: %v", err)
			}
		})
	}
}

func TestAGatewayCannotEnforceAStandardItsOwnTelemetryViolates(t *testing.T) {
	// The platform is its own first telemetry citizen (ADR 0010), and a citizen is
	// held to the law it writes. A Gateway that tags every service's records
	// `blocking` for a missing attribute while its own telemetry omits that
	// attribute is the platform exempting itself — and nothing downstream would
	// ever say so, because the Gateway does not judge its own signals: they never
	// enter a pipeline.
	//
	// `block` only, for the same reason only `block` fails a build at Preflight
	// (ADR 0003). A `warn` Standard is advice, and refusing to compile over advice
	// would make Severity mean something different here than everywhere else.
	exempt := gatewayFrom(t, `apiVersion: guardrail.otel/v1
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
`)

	err := mustNotCompileGateway(t, exempt, "the Gateway omits an attribute S1 tags the whole fleet for")

	for _, named := range []string{"deployment.environment", "S1"} {
		if !strings.Contains(err.Error(), named) {
			t.Errorf("the error does not name %s: %v", named, err)
		}
	}
}

func TestTheGatewayCannotDeclareItsOwnConfigVersionByHand(t *testing.T) {
	// The same refusal a Telemetry Contract gets, for the same reason and one layer
	// up. config_version is DERIVED from the compiled configuration — that is what
	// makes it identify what is running — so a declared one is a Gateway announcing
	// a configuration it is not running, to the only query an operator has.
	forging := gatewayFrom(t, `apiVersion: guardrail.otel/v1
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
      otel.platform.config_version: sha256:the-one-you-are-waiting-for
`)

	err := mustNotCompileGateway(t, forging, "the declaration wrote the Gateway's own config_version")

	if !strings.Contains(err.Error(), "otel.platform.config_version") {
		t.Errorf("the error does not name the attribute it refused: %v", err)
	}
}

func TestEveryBackendsBackPressureIsAttributableToThatBackendAndNoOther(t *testing.T) {
	// "Which Backend is behind?" is answerable rather than inferred, and the whole
	// mechanism is the naming. The collector labels its own queue-depth, export-
	// failure and enqueue-failure metrics with the exporter's component ID and
	// nothing else — so an exporter shared by two Backends, or named otlp/1, would
	// leave an operator with a number and no way to know whose it is.
	//
	// One exporter per Backend, named for it, and no exporter that is not one:
	// that is what makes the label a Backend's name.
	declaration := gatewayDeclaration(t)
	config, err := controlplane.CompileGateway(declaration, profiles(t), orgStandards(t))
	if err != nil {
		t.Fatalf("compile the Gateway: %v", err)
	}

	attributedTo := map[string]string{}
	for _, backend := range declaration.Backends {
		exporter := "otlp/" + backend.Name
		if _, defined := config.Exporters[exporter]; !defined {
			t.Errorf("Backend %q has no exporter of its own, so its queue depth is somebody else's number", backend.Name)
			continue
		}
		if already, taken := attributedTo[exporter]; taken {
			t.Errorf("Backends %q and %q compile to the same exporter %q, so one metric would speak for both", already, backend.Name, exporter)
		}
		attributedTo[exporter] = backend.Name
	}

	if len(config.Exporters) != len(declaration.Backends) {
		t.Errorf("the Gateway defines %d exporters for %d Backends: an exporter belonging to no Backend produces back-pressure metrics attributed to nothing (%v)",
			len(config.Exporters), len(declaration.Backends), config.Exporters)
	}
}

func TestTheGatewayClearsThePlatformNamespaceFromRecordsButNotFromResources(t *testing.T) {
	// Where the two halves of this slice meet, and the asymmetry is the decision.
	//
	// A collector's own config_version lives on the RESOURCE. The Gateway must not
	// clear it there — that stamp is the Agent's answer arriving, and deleting it
	// would leave the platform unable to see the very fleet it is confirming.
	//
	// Nothing legitimate ever puts the namespace on a span, a datapoint, a log
	// record or a scope. A service can, though, and most Backends flatten record
	// and resource attributes into one queryable field — so a forged record-level
	// key would answer an operator's rollout query exactly as well as a real one.
	// It is swept everywhere it can only be a forgery, which is everywhere except
	// the resource.
	config, err := controlplane.CompileGateway(gatewayDeclaration(t), profiles(t), orgStandards(t))
	if err != nil {
		t.Fatalf("compile the Gateway: %v", err)
	}

	guardrails, running := config.Processors["transform/guardrail"].(map[string]any)
	if !running {
		t.Fatalf("the Gateway runs no Pipeline Guardrail, so there is nothing here to sweep with: %v", config.Processors)
	}

	for _, key := range []string{"trace_statements", "metric_statements", "log_statements"} {
		for _, entry := range guardrails[key].([]any) {
			group := entry.(map[string]any)
			context := group["context"].(string)

			swept := false
			for _, statement := range group["statements"].([]any) {
				if clears(t, statement.(string), "otel.platform.config_version") {
					swept = true
				}
			}
			if context == "resource" && swept {
				t.Errorf("%s clears the platform namespace on the resource, which is where a collector's own config_version arrives — the Gateway would delete the answer it is waiting for", key)
			}
			if context != "resource" && !swept {
				t.Errorf("%s does not clear the platform namespace in the %q context, so a service can put otel.platform.config_version there and answer a rollout query with it", key, context)
			}
		}
	}
}

func TestTheGuardrailsVerdictAndThePlatformsOwnSignalsDoNotReachIntoEachOther(t *testing.T) {
	// Two namespaces now live on the same records, written by two mechanisms with
	// different rules: the Gateway's verdict about a service (C6), and a collector's
	// statement about itself (C7). Each is cleared before it is written, so a
	// pattern that reached one attribute too far would silently delete the other —
	// a Guardrail sweeping `^otel\.` would take every config_version on the fleet
	// with it, and a rollout would stop confirming with nothing to say why.
	config, err := controlplane.CompileGateway(gatewayDeclaration(t), profiles(t), orgStandards(t))
	if err != nil {
		t.Fatalf("compile the Gateway: %v", err)
	}

	// Both are present at once. A Gateway that dropped either would pass every
	// disjointness check below by having nothing to be disjoint from.
	if _, running := config.Processors["transform/guardrail"]; !running {
		t.Fatal("the Gateway runs no Pipeline Guardrail, so this asserts nothing about the two co-existing")
	}
	if config.ConfigVersion() == "" {
		t.Fatal("the Gateway reports no config_version, so this asserts nothing about the two co-existing")
	}

	// Asserted on what each statement REACHES, never on how it is spelled: a sweep
	// widened to `^otel\.` would still read as the Guardrail's own line and would
	// delete every config_version on the fleet. No one statement may reach both
	// namespaces, whatever it is called.
	guardrails := config.Processors["transform/guardrail"].(map[string]any)
	for _, key := range []string{"trace_statements", "metric_statements", "log_statements"} {
		for _, entry := range guardrails[key].([]any) {
			group := entry.(map[string]any)
			for _, statement := range group["statements"].([]any) {
				text := statement.(string)
				if clears(t, text, "otel.guardrail.blocking") && clears(t, text, "otel.platform.config_version") {
					t.Errorf("%s/%v: one clearing statement reaches both the Guardrail's verdict and the platform's own signals, so widening either namespace silently deletes the other: %s",
						key, group["context"], text)
				}
			}
		}
	}
}

func TestAConfigThatSaysItObservesItselfAndDoesNotIsNotCoherent(t *testing.T) {
	// Referential integrity, extended to the block ADR 0010 added. A
	// `service.telemetry` with no reader emits nothing while every reviewer, every
	// diff and the config itself say the collector reports on itself — and one
	// carrying no config_version is a collector an operator can see and cannot
	// identify. Both are the same failure as a spill extension the service block
	// never starts: configured, inert, and indistinguishable from working.
	coherent := func() controlplane.CollectorConfig {
		config, err := controlplane.Compile(tierOneService(), taxonomy(t), profiles(t))
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		return config
	}

	if err := coherent().Validate(); err != nil {
		t.Fatalf("a compiled Agent does not validate: %v", err)
	}

	t.Run("not reporting on itself at all", func(t *testing.T) {
		// The strongest of the three, and the one the other two are special cases of.
		// A collector that runs pipelines and emits nothing about itself is invisible
		// on the fleet — no version to confirm a Rollout by, no queue depth when it
		// starts dropping — and it looks exactly like one that is healthy, because
		// there is no status channel that would have said otherwise (ADR 0010). The
		// existing refusal of a config that runs no pipelines is the same shape: a
		// completeness claim, not referential integrity.
		config := coherent()
		config.Service.Telemetry = nil

		if err := config.Validate(); err == nil {
			t.Error("a config validated while saying nothing about itself, so nobody could see the collector running it")
		}
	})

	t.Run("reporting on itself to nowhere", func(t *testing.T) {
		config := coherent()
		config.Service.Telemetry.Metrics.Readers = nil

		if err := config.Validate(); err == nil {
			t.Error("a config validated while its own telemetry has nowhere to go, so it emits nothing and reads as though it does")
		}
	})

	t.Run("reporting without saying what it is running", func(t *testing.T) {
		config := coherent()
		delete(config.Service.Telemetry.Resource, "otel.platform.config_version")

		if err := config.Validate(); err == nil {
			t.Error("a config validated while reporting no config_version, so no Rollout reaching it could ever be confirmed")
		}
	})
}

func TestAContractDeclaringNoResourceAttributesStillCompiles(t *testing.T) {
	// A Contract with no `resource_attributes:` at all violates S1 and every other
	// Standard about them — and `compile` is not `check`, so that is a finding for a
	// Preflight Guardrail rather than a reason the compiler cannot produce a config.
	// It compiled before self-observation existed, and it has to keep compiling: a
	// crash here would take out `compile-fleet` for the whole Fleet on one team's
	// omission, which is the exact opposite of the per-service blast radius the
	// Rollout Manifest exists to keep.
	bare := tierOneService()
	bare.ResourceAttributes = nil

	config, err := controlplane.Compile(bare, taxonomy(t), profiles(t))
	if err != nil {
		t.Fatalf("a Contract with no resource attributes did not compile: %v", err)
	}
	if config.ConfigVersion() == "" {
		t.Error("it compiled without a config_version, so nothing it reports could be matched to a Rollout")
	}
}

// clears reports whether an OTTL statement is a delete_matching_keys whose
// pattern reaches a given attribute name. It unquotes the pattern rather than
// matching the statement text, so what is asserted is the reach rather than the
// spelling of the escaping.
func clears(t *testing.T, statement, attribute string) bool {
	t.Helper()

	const prefix = "delete_matching_keys(attributes, "
	if !strings.HasPrefix(statement, prefix) {
		return false
	}
	pattern, err := strconv.Unquote(strings.TrimSuffix(strings.TrimPrefix(statement, prefix), ")"))
	if err != nil {
		t.Fatalf("the clearing statement's pattern is not a quoted string: %s", statement)
	}
	cleared, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("the clearing pattern %q is not a valid regexp: %v", pattern, err)
	}
	// A pattern that also reached the service's own attributes would delete the
	// telemetry rather than the forgery.
	for _, keep := range []string{"service.name", "service.version", "deployment.environment"} {
		if cleared.MatchString(keep) {
			t.Errorf("the clearing pattern %q also deletes the service's own %s", pattern, keep)
		}
	}
	return cleared.MatchString(attribute)
}

func TestAGatewayThatSaysNothingAboutWhoItIsDoesNotCompile(t *testing.T) {
	// Checking the Gateway's identity against the Standards it enforces is not
	// enough on its own: what it checks depends on the CATALOG, so a catalog whose
	// pipeline Standards are all `warn` would leave a Gateway free to emit telemetry
	// with no identity at all. The collector would then supply its own defaults —
	// `service.name: otelcol` — and every Gateway on every platform would look
	// identical in a Backend.
	//
	// It is the empty-catalog disarm C6 found (#14, ADR 0015) reaching one layer
	// further: a check that borrows its strictness from a document can go quiet when
	// that document does. So the Gateway must say who it is regardless of what any
	// catalog requires.
	anonymous := gatewayFrom(t, `apiVersion: guardrail.otel/v1
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
`)

	// A catalog that enforces something at the pipeline, but nothing that blocks —
	// so the Standards check below has nothing to say and cannot be what refuses it.
	nothingBlocks := standardsFrom(t, `apiVersion: guardrail.otel/v1
kind: StandardCatalog
standards:
  - standard: S9
    severity: warn
    enforced_at: [pipeline]
    requires:
      resource_attributes: [service.namespace]
`)

	_, err := controlplane.CompileGateway(anonymous, profiles(t), nothingBlocks)
	if err == nil {
		t.Fatal("a Gateway compiled while saying nothing about who it is; its own telemetry would arrive as an anonymous collector")
	}
	if !strings.Contains(err.Error(), "resource_attributes") {
		t.Errorf("the error does not name what is missing: %v", err)
	}
}

// mustNotCompileGateway asserts a Gateway Declaration is refused, and hands back
// the reason.
func mustNotCompileGateway(t *testing.T, declaration *controlplane.GatewayDeclaration, because string) error {
	t.Helper()

	_, err := controlplane.CompileGateway(declaration, profiles(t), orgStandards(t))
	if err == nil {
		t.Fatalf("a Gateway compiled that should not have: %s", because)
	}
	if strings.TrimSpace(err.Error()) == "" {
		t.Fatal("compiling the Gateway failed with an empty message")
	}
	return err
}

// agentConfigVersion compiles the same service against a given Profile set and
// reports the config_version its Agent would announce.
func agentConfigVersion(t *testing.T, set *controlplane.ProfileSet) string {
	t.Helper()

	config, err := controlplane.Compile(tierOneService(), taxonomy(t), set)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return config.ConfigVersion()
}

// profilesBatchingAfter is a Profile set for tier-1 that differs from the next one
// only in its batch timeout — one field, reaching one line of the compiled config.
func profilesBatchingAfter(t *testing.T, timeout string) *controlplane.ProfileSet {
	t.Helper()

	return profilesFrom(t, `apiVersion: guardrail.otel/v1
kind: PipelineProfileSet
profiles:
  - profile: tier-1-critical
    tiers: [tier-1]
    description: Everything is kept.
    gateway_endpoint: otel-gateway.observability.svc.cluster.local:4317
    batch:
      timeout: `+timeout+`
      send_batch_size: 8192
`)
}
