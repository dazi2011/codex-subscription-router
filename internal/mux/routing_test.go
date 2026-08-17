package mux

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/b-nnett/codex-subscription-router/internal/backend"
	"github.com/b-nnett/codex-subscription-router/internal/protocol"
	"github.com/b-nnett/codex-subscription-router/internal/state"
)

func TestIsUsageLimitResponseRecognizesStructuredError(t *testing.T) {
	message := protocol.Message{Error: &protocol.RPCError{
		Code:    -32000,
		Message: "turn failed",
		Data:    json.RawMessage(`{"codexErrorInfo":"usage_limit_exceeded"}`),
	}}
	if !isUsageLimitResponse(message) {
		t.Fatal("expected usage-limit error to be recognized")
	}
}

func TestIsUsageLimitResponseIgnoresUnrelatedError(t *testing.T) {
	message := protocol.Message{Error: &protocol.RPCError{
		Code:    -32000,
		Message: "workspace folder is unavailable",
	}}
	if isUsageLimitResponse(message) {
		t.Fatal("unrelated error was misclassified as a usage limit")
	}
}

func TestIsUsageLimitResponseIgnoresToolRateLimitsAndQuotaText(t *testing.T) {
	for _, message := range []string{
		"MCP server rate limit exceeded",
		"GitHub API quota exceeded",
		"provider rate_limit response",
	} {
		response := protocol.Message{Error: &protocol.RPCError{Code: -32000, Message: message}}
		if isUsageLimitResponse(response) {
			t.Fatalf("tool error %q was misclassified as subscription depletion", message)
		}
	}
	structuredToolError := protocol.Message{Error: &protocol.RPCError{
		Code: -32000, Message: "tool failed", Data: json.RawMessage(`{"code":"usage_limit_exceeded","source":"mcp"}`),
	}}
	if isUsageLimitResponse(structuredToolError) {
		t.Fatal("structured MCP usage limit was misclassified as subscription depletion")
	}
}

func TestModelHelpersUseConcreteModelIdentifier(t *testing.T) {
	params := json.RawMessage(`{"model":"daybreak-blue"}`)
	if got := modelFromParams(params); got != "daybreak-blue" {
		t.Fatalf("modelFromParams() = %q", got)
	}
	models := []map[string]any{{"id": "display-id", "model": "daybreak-blue"}}
	if !modelsContain(models, "daybreak-blue") || modelsContain(models, "other") {
		t.Fatalf("unexpected model capability match: %#v", models)
	}
}

func TestModelRequirementUsesCurrentTurnAndCollaborationOverrides(t *testing.T) {
	stored := modelRequirement{
		Model: "old-model", ModelKnown: true,
		Effort: "high", EffortKnown: true,
		ServiceTier: "default", ServiceTierKnown: true,
	}
	requested := modelRequirementFromParams(json.RawMessage(`{
		"model":"new-model",
		"effort":"medium",
		"serviceTier":"priority",
		"collaborationMode":{"mode":"default","settings":{"model":"collab-model","reasoning_effort":"xhigh"}}
	}`))
	effective := stored.overlay(requested)
	if effective.Model != "collab-model" || effective.Effort != "xhigh" || effective.ServiceTier != "priority" {
		t.Fatalf("current-turn overrides did not win: %#v", effective)
	}
}

func TestFailoverParamsCarryStickyModelSubCapabilities(t *testing.T) {
	params := paramsWithModelRequirement(
		json.RawMessage(`{"threadId":"thread-1","input":[]}`),
		modelRequirement{
			Model: "daybreak-blue", ModelKnown: true,
			Effort: "xhigh", EffortKnown: true,
			ServiceTier: "priority", ServiceTierKnown: true,
		},
	)
	requirement := modelRequirementFromParams(params)
	if requirement.Model != "daybreak-blue" || requirement.Effort != "xhigh" || requirement.ServiceTier != "priority" {
		t.Fatalf("failover turn lost sticky capability settings: %s", params)
	}
	if threadIDFromParams(params) != "thread-1" {
		t.Fatalf("failover parameter rewrite lost thread identity: %s", params)
	}
}

func TestCapabilityFieldsPreserveExplicitNull(t *testing.T) {
	stored := modelRequirement{
		Model: "daybreak-blue", ModelKnown: true,
		ServiceTier: "priority", ServiceTierKnown: true,
	}
	requested := modelRequirementFromParams(json.RawMessage(`{"serviceTier":null}`))
	if !requested.ServiceTierKnown || requested.ServiceTier != "" {
		t.Fatalf("explicit null was not preserved: %#v", requested)
	}
	effective := stored.overlay(requested)
	params := paramsWithModelRequirement(json.RawMessage(`{"threadId":"thread-1"}`), effective)
	var decoded map[string]any
	if err := json.Unmarshal(params, &decoded); err != nil {
		t.Fatal(err)
	}
	value, present := decoded["serviceTier"]
	if !present || value != nil {
		t.Fatalf("service-tier clear was not re-encoded as null: %s", params)
	}
}

func TestEffectiveThreadResponseOverridesRequestedModel(t *testing.T) {
	info, err := threadResumeInfoFromResponse(json.RawMessage(`{
		"model":"effective-model",
		"modelProvider":"openai",
		"reasoningEffort":"xhigh",
		"serviceTier":null,
		"cwd":"/tmp/project",
		"thread":{"id":"thread-1","path":"/tmp/thread.jsonl","historyMode":"legacy"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if info.Capability.Model != "effective-model" || !info.Capability.ModelKnown ||
		info.Capability.Effort != "xhigh" || !info.Capability.EffortKnown ||
		info.Capability.ServiceTier != "" || !info.Capability.ServiceTierKnown {
		t.Fatalf("effective response settings were not preserved: %#v", info.Capability)
	}
}

func TestModelCapabilityChecksReasoningAndServiceTier(t *testing.T) {
	models := []map[string]any{{
		"model":  "daybreak-blue",
		"hidden": true,
		"supportedReasoningEfforts": []any{
			map[string]any{"reasoningEffort": "high"},
			map[string]any{"reasoningEffort": "xhigh"},
		},
		"serviceTiers": []any{map[string]any{"id": "priority"}},
	}}
	if !modelsSupportRequirement(models, modelRequirement{
		Model: "daybreak-blue", ModelKnown: true,
		Effort: "xhigh", EffortKnown: true,
		ServiceTier: "priority", ServiceTierKnown: true,
	}) {
		t.Fatal("hidden model with the requested sub-capabilities was rejected")
	}
	if modelsSupportRequirement(models, modelRequirement{
		Model: "daybreak-blue", ModelKnown: true,
		Effort: "ultra", EffortKnown: true,
		ServiceTier: "priority", ServiceTierKnown: true,
	}) {
		t.Fatal("unsupported reasoning effort was accepted")
	}
	if modelsSupportRequirement(models, modelRequirement{
		Model: "daybreak-blue", ModelKnown: true,
		Effort: "xhigh", EffortKnown: true,
		ServiceTier: "flex", ServiceTierKnown: true,
	}) {
		t.Fatal("unsupported service tier was accepted")
	}
}

func TestMergedModelCatalogKeepsOneRealCapabilityTuple(t *testing.T) {
	first := map[string]any{
		"model":                     "model-x",
		"supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "high"}},
		"serviceTiers":              []any{map[string]any{"id": "default"}},
	}
	second := map[string]any{
		"model":                     "model-x",
		"supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "xhigh"}},
		"serviceTiers":              []any{map[string]any{"id": "priority"}},
	}
	merged := mergeModelCatalogs([][]map[string]any{{first}, {second}})
	if len(merged) != 1 || len(anySlice(merged[0]["supportedReasoningEfforts"])) != 1 ||
		len(anySlice(merged[0]["serviceTiers"])) != 1 {
		t.Fatalf("duplicate model capabilities were cross-unioned: %#v", merged)
	}
	if !modelsSupportRequirement(merged, modelRequirement{
		Model: "model-x", ModelKnown: true,
		Effort: "high", EffortKnown: true,
		ServiceTier: "default", ServiceTierKnown: true,
	}) {
		t.Fatalf("the retained real capability tuple was damaged: %#v", merged)
	}
}

func TestPaginatedHistoryFailsClosedForCrossProcessFailover(t *testing.T) {
	if historyModeSupportsCrossProcessFailover("paginated") {
		t.Fatal("paginated history was allowed to race two app-server writers")
	}
	if historyModeSupportsCrossProcessFailover("future-mode") {
		t.Fatal("unknown history mode should fail closed")
	}
	if !historyModeSupportsCrossProcessFailover("legacy") {
		t.Fatal("legacy rollout history should remain eligible for failover")
	}
}

func TestEffectiveSettingsAndModelRerouteNotificationsUpdateOwnerState(t *testing.T) {
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "mux"), filepath.Join(root, "primary"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetThreadOwner("thread-1", "secondary"); err != nil {
		t.Fatal(err)
	}
	multiplexer := &Multiplexer{store: store}
	multiplexer.learnEffectiveThreadSettings("secondary", json.RawMessage(`{
		"threadId":"thread-1",
		"threadSettings":{"model":"daybreak-blue","effort":"xhigh","serviceTier":null}
	}`))
	capability := store.ThreadCapability("thread-1")
	if capability.Model != "daybreak-blue" || capability.Effort != "xhigh" ||
		!capability.ServiceTierKnown || capability.ServiceTier != "" {
		t.Fatalf("effective settings notification was not learned: %#v", capability)
	}
	multiplexer.learnModelReroute("secondary", json.RawMessage(`{
		"threadId":"thread-1","fromModel":"daybreak-blue","toModel":"safety-model"
	}`))
	if capability = store.ThreadCapability("thread-1"); capability.Model != "safety-model" {
		t.Fatalf("model reroute was not learned: %#v", capability)
	}
	multiplexer.learnModelReroute("stale-source", json.RawMessage(`{
		"threadId":"thread-1","fromModel":"safety-model","toModel":"stale-model"
	}`))
	if capability = store.ThreadCapability("thread-1"); capability.Model != "safety-model" {
		t.Fatalf("non-owner notification corrupted capability state: %#v", capability)
	}
	if !multiplexer.shouldForwardNotification("secondary", "model/rerouted") ||
		!multiplexer.shouldForwardNotification("secondary", "model/verification") ||
		!multiplexer.shouldForwardNotification("secondary", "model/safetyBuffering/updated") {
		t.Fatal("secondary model notifications were not forwarded")
	}
}

func TestThreadStartLearnsEffectiveResponseInsteadOfRequestedModel(t *testing.T) {
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "mux"), filepath.Join(root, "primary"))
	if err != nil {
		t.Fatal(err)
	}
	multiplexer := &Multiplexer{store: store}
	multiplexer.learnThreadOwner(externalRoute{
		method:  "thread/start",
		message: protocol.Message{Params: json.RawMessage(`{"model":"requested-model"}`)},
	}, "primary", json.RawMessage(`{
		"model":"effective-model",
		"modelProvider":"openai",
		"reasoningEffort":"high",
		"serviceTier":null,
		"cwd":"/tmp/project",
		"thread":{"id":"thread-1","path":"/tmp/thread.jsonl","historyMode":"legacy"}
	}`))
	capability := store.ThreadCapability("thread-1")
	if capability.Model != "effective-model" || capability.Effort != "high" ||
		!capability.ServiceTierKnown || capability.ServiceTier != "" {
		t.Fatalf("thread start persisted request settings instead of effective response: %#v", capability)
	}
	multiplexer.learnThreadOwner(externalRoute{
		method: "turn/start",
		message: protocol.Message{Params: json.RawMessage(
			`{"threadId":"thread-1","model":"unconfirmed-turn-model"}`,
		)},
	}, "primary", json.RawMessage(`{"turn":{"id":"turn-1"}}`))
	if capability = store.ThreadCapability("thread-1"); capability.Model != "effective-model" {
		t.Fatalf("turn request was incorrectly treated as effective settings: %#v", capability)
	}
}

func TestServerRequestRouteLivesUntilResponseOrChildExit(t *testing.T) {
	output := &bytes.Buffer{}
	multiplexer := &Multiplexer{
		output:       output,
		serverRoutes: make(map[string]serverRequestRoute),
	}
	multiplexer.forwardServerRequest(backend.Inbound{
		AccountID: "secondary",
		Message: protocol.Request(
			"item/commandExecution/requestApproval",
			json.RawMessage(`17`),
			json.RawMessage(`{"threadId":"thread-1"}`),
		),
	})
	if len(multiplexer.serverRoutes) != 1 {
		t.Fatalf("approval route was not retained: %#v", multiplexer.serverRoutes)
	}
	for key := range multiplexer.serverRoutes {
		multiplexer.handleServerRequestResponse(protocol.Success(json.RawMessage(key), json.RawMessage(`{}`)))
	}
	if len(multiplexer.serverRoutes) != 0 {
		t.Fatalf("approval route survived its response: %#v", multiplexer.serverRoutes)
	}
	multiplexer.serverRoutes[`"orphan"`] = serverRequestRoute{accountID: "secondary"}
	multiplexer.dropServerRoutes("secondary")
	if len(multiplexer.serverRoutes) != 0 {
		t.Fatalf("child-exit cleanup retained server routes: %#v", multiplexer.serverRoutes)
	}
}

func TestAllSubscriptionsDepletedUsesActionableMessage(t *testing.T) {
	message := allSubscriptionsDepleted(json.RawMessage(`7`), nil)
	if message.Error == nil || message.Error.Code != -32026 {
		t.Fatalf("unexpected error response: %#v", message)
	}
	if message.Error.Message != "All connected subscriptions are depleted. Add another subscription or wait for usage to reset." {
		t.Fatalf("unexpected depletion message: %q", message.Error.Message)
	}
}

func TestAllSubscriptionsDepletedShowsKnownResetTime(t *testing.T) {
	reset := time.Date(2026, time.August, 16, 10, 30, 0, 0, time.Local).Unix()
	message := allSubscriptionsDepleted(json.RawMessage(`7`), &reset)
	if message.Error == nil {
		t.Fatal("expected an error response")
	}
	want := "All connected subscriptions are depleted. Usage resets on Sunday, 16 August at 10:30 AM."
	if message.Error.Message != want {
		t.Fatalf("unexpected reset message: %q", message.Error.Message)
	}
}
