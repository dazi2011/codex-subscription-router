package mux

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/b-nnett/codex-subscription-router/internal/protocol"
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
	stored := modelRequirement{Model: "old-model", Effort: "high", ServiceTier: "default"}
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
		modelRequirement{Model: "daybreak-blue", Effort: "xhigh", ServiceTier: "priority"},
	)
	requirement := modelRequirementFromParams(params)
	if requirement.Model != "daybreak-blue" || requirement.Effort != "xhigh" || requirement.ServiceTier != "priority" {
		t.Fatalf("failover turn lost sticky capability settings: %s", params)
	}
	if threadIDFromParams(params) != "thread-1" {
		t.Fatalf("failover parameter rewrite lost thread identity: %s", params)
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
		Model: "daybreak-blue", Effort: "xhigh", ServiceTier: "priority",
	}) {
		t.Fatal("hidden model with the requested sub-capabilities was rejected")
	}
	if modelsSupportRequirement(models, modelRequirement{
		Model: "daybreak-blue", Effort: "ultra", ServiceTier: "priority",
	}) {
		t.Fatal("unsupported reasoning effort was accepted")
	}
	if modelsSupportRequirement(models, modelRequirement{
		Model: "daybreak-blue", Effort: "xhigh", ServiceTier: "flex",
	}) {
		t.Fatal("unsupported service tier was accepted")
	}
}

func TestMergeModelCapabilitiesUnionsAccountCatalogs(t *testing.T) {
	target := map[string]any{
		"supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "high"}},
		"serviceTiers":              []any{map[string]any{"id": "default"}},
		"additionalSpeedTiers":      []any{"fast"},
	}
	source := map[string]any{
		"supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "xhigh"}},
		"serviceTiers":              []any{map[string]any{"id": "priority"}},
		"additionalSpeedTiers":      []any{"fast", "faster"},
	}
	mergeModelCapabilities(target, source)
	if len(anySlice(target["supportedReasoningEfforts"])) != 2 ||
		len(anySlice(target["serviceTiers"])) != 2 ||
		len(anySlice(target["additionalSpeedTiers"])) != 2 {
		t.Fatalf("model sub-capabilities were not unioned: %#v", target)
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
