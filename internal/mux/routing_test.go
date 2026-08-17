package mux

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/b-nnett/codex-subscription-router/internal/backend"
	"github.com/b-nnett/codex-subscription-router/internal/protocol"
	"github.com/b-nnett/codex-subscription-router/internal/state"
)

func TestMuxDelayedInitializeHelperProcess(t *testing.T) {
	if os.Getenv("CODEX_MUX_TEST_HELPER") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		message, err := protocol.Parse(scanner.Bytes())
		if err != nil {
			continue
		}
		if eventLog := os.Getenv("CODEX_MUX_HELPER_EVENT_LOG"); eventLog != "" {
			if file, openErr := os.OpenFile(eventLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); openErr == nil {
				_, _ = fmt.Fprintf(file, "%s\t%s\n", os.Getenv("CODEX_HOME"), message.Method)
				_ = file.Close()
			}
		}
		if message.Method == "initialize" && os.Getenv("CODEX_MUX_DELAY_INITIALIZE") == "1" {
			time.Sleep(250 * time.Millisecond)
		}
		if len(message.ID) == 0 {
			continue
		}
		response := protocol.Success(message.ID, json.RawMessage(`{}`))
		if message.Method == "skills/extraRoots/set" && os.Getenv("CODEX_HOME") == os.Getenv("CODEX_MUX_FAIL_ONCE_HOME") {
			marker := os.Getenv("CODEX_MUX_FAIL_ONCE_MARKER")
			if file, createErr := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); createErr == nil {
				_ = file.Close()
				response = protocol.Failure(message.ID, -32000, "injected global mutation failure")
			}
		}
		encoded, _ := protocol.Encode(response)
		_, _ = os.Stdout.Write(append(encoded, '\n'))
	}
	os.Exit(0)
}

func TestStartChildPublishesOnlyAfterInitialization(t *testing.T) {
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "mux"), filepath.Join(root, "primary"))
	if err != nil {
		t.Fatal(err)
	}
	multiplexer, err := New(Options{
		RealExecutable: os.Args[0],
		RealArgs:       []string{"-test.run=TestMuxDelayedInitializeHelperProcess"},
		Environment: append(os.Environ(),
			"CODEX_MUX_TEST_HELPER=1",
			"CODEX_MUX_DELAY_INITIALIZE=1",
		),
		Store: store, Output: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatal(err)
	}
	multiplexer.initializeParams = json.RawMessage(`{"clientInfo":{"name":"test","version":"1"}}`)
	multiplexer.initialized = true
	account, _ := store.Account("primary")
	type startResult struct {
		child *backend.Child
		err   error
	}
	result := make(chan startResult, 1)
	go func() {
		child, err := multiplexer.startChild(context.Background(), account)
		result <- startResult{child: child, err: err}
	}()
	time.Sleep(50 * time.Millisecond)
	if _, published := multiplexer.child(account.ID); published {
		t.Fatal("initializing child was published as routable")
	}
	var started startResult
	select {
	case started = <-result:
	case <-time.After(3 * time.Second):
		t.Fatal("child did not finish initialization")
	}
	if started.err != nil || started.child == nil {
		t.Fatalf("start child failed: %v", started.err)
	}
	if current, published := multiplexer.child(account.ID); !published || current != started.child {
		t.Fatal("ready child was not published")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := multiplexer.stopChild(stopCtx, account.ID); err != nil {
		t.Fatal(err)
	}
}

func TestGlobalMutationRecoversFailedSecondaryFromControllerJournal(t *testing.T) {
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "mux"), filepath.Join(root, "primary"))
	if err != nil {
		t.Fatal(err)
	}
	secondary, err := store.AddAccount("Secondary")
	if err != nil {
		t.Fatal(err)
	}
	eventLog := filepath.Join(root, "events.log")
	failMarker := filepath.Join(root, "failed-once")
	output := &bytes.Buffer{}
	multiplexer, err := New(Options{
		RealExecutable: os.Args[0],
		RealArgs:       []string{"-test.run=TestMuxDelayedInitializeHelperProcess"},
		Environment: append(os.Environ(),
			"CODEX_MUX_TEST_HELPER=1",
			"CODEX_MUX_HELPER_EVENT_LOG="+eventLog,
			"CODEX_MUX_FAIL_ONCE_HOME="+secondary.CodexHome,
			"CODEX_MUX_FAIL_ONCE_MARKER="+failMarker,
		),
		Store: store, Output: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, cancelLifecycle := context.WithCancel(context.Background())
	if err := multiplexer.Start(lifecycle); err != nil {
		cancelLifecycle()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancelLifecycle()
		for _, account := range store.Accounts() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = multiplexer.stopChild(ctx, account.ID)
			cancel()
		}
	})
	multiplexer.initialize(protocol.Message{
		ID: json.RawMessage(`1`), Params: json.RawMessage(`{"clientInfo":{"name":"test","version":"1"}}`),
	})
	multiplexer.handleClientNotification(protocol.Message{Method: "initialized"})
	originalSecondary, ok := multiplexer.child(secondary.ID)
	if !ok {
		t.Fatal("secondary was not initialized")
	}
	output.Reset()
	multiplexer.broadcastGlobalMutation(protocol.Message{
		ID:     json.RawMessage(`2`),
		Method: "skills/extraRoots/set",
		Params: json.RawMessage(`{"extraRoots":["/shared"]}`),
	})
	response, err := protocol.Parse(bytes.TrimSpace(output.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if response.Error != nil {
		t.Fatalf("recovered global mutation returned failure: %#v", response.Error)
	}
	replacement, ok := multiplexer.child(secondary.ID)
	if !ok || replacement == originalSecondary {
		t.Fatal("failed Secondary was not replaced from the journal")
	}
	contents, err := os.ReadFile(eventLog)
	if err != nil {
		t.Fatal(err)
	}
	primary, _ := store.Account("primary")
	mutationHomes := make([]string, 0, 3)
	for _, line := range strings.Split(strings.TrimSpace(string(contents)), "\n") {
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) == 2 && fields[1] == "skills/extraRoots/set" {
			mutationHomes = append(mutationHomes, fields[0])
		}
	}
	if len(mutationHomes) != 3 || mutationHomes[0] != primary.CodexHome ||
		mutationHomes[1] != secondary.CodexHome || mutationHomes[2] != secondary.CodexHome {
		t.Fatalf("unexpected Controller/Secondary mutation order: %#v\n%s", mutationHomes, contents)
	}
}

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

func TestModelRequirementReadsConfigOverridesWithExplicitFieldsWinning(t *testing.T) {
	configOnly := modelRequirementFromParams(json.RawMessage(`{
		"config":{"model":"daybreak-blue","model_reasoning_effort":"xhigh","service_tier":"priority"}
	}`))
	if configOnly.Model != "daybreak-blue" || configOnly.Effort != "xhigh" ||
		configOnly.ServiceTier != "priority" || !configOnly.ModelKnown ||
		!configOnly.EffortKnown || !configOnly.ServiceTierKnown {
		t.Fatalf("config capability override was ignored: %#v", configOnly)
	}
	explicit := modelRequirementFromParams(json.RawMessage(`{
		"config":{"model":"config-model","model_reasoning_effort":"high"},
		"model":"explicit-model","effort":"medium"
	}`))
	if explicit.Model != "explicit-model" || explicit.Effort != "medium" {
		t.Fatalf("explicit app-server fields did not override config defaults: %#v", explicit)
	}
}

func TestThreadSettingsUpdateUsesCapabilityAwareRouting(t *testing.T) {
	for _, method := range []string{"thread/resume", "thread/fork", "thread/settings/update"} {
		if !capabilityAwareThreadOverrideMethod(method) {
			t.Fatalf("%s bypassed capability-aware routing", method)
		}
	}
	if capabilityAwareThreadOverrideMethod("thread/metadata/update") {
		t.Fatal("metadata-only update entered capability migration")
	}
}

func TestInitializeResponseIsAlwaysControllerAuthoritative(t *testing.T) {
	result, failed, err := authoritativeInitializeResult([]childInitializeResult{
		{
			accountID: "secondary", response: protocol.Message{
				Result: json.RawMessage(`{"codexHome":"/secondary"}`),
			},
		},
		{
			accountID: "primary", controller: true, response: protocol.Message{
				Result: json.RawMessage(`{"codexHome":"/primary"}`),
			},
		},
	})
	if err != nil || len(failed) != 0 || string(result) != `{"codexHome":"/primary"}` {
		t.Fatalf("secondary won initialize race: result=%s failed=%v err=%v", result, failed, err)
	}
	_, _, err = authoritativeInitializeResult([]childInitializeResult{
		{accountID: "secondary", response: protocol.Message{Result: json.RawMessage(`{}`)}},
		{accountID: "primary", controller: true, err: errors.New("not initialized")},
	})
	if err == nil {
		t.Fatal("secondary success masked Controller initialize failure")
	}
}

func TestGlobalStateRoutingClassifiesGeneratedAndReplicatedState(t *testing.T) {
	for _, method := range []string{
		"project/create", "project/list", "threadSection/create", "threadSection/list",
		"environment/status", "experimentalFeature/list",
	} {
		if !controllerGlobalStateMethod(method) {
			t.Fatalf("%s was not Controller-affined", method)
		}
	}
	for _, method := range []string{
		"environment/add", "skills/extraRoots/set", "experimentalFeature/enablement/set",
		"thread/loaded/list", "thread/start",
	} {
		if controllerGlobalStateMethod(method) {
			t.Fatalf("%s was incorrectly forced through the generic Controller path", method)
		}
	}
	if !threadStartUsesControllerState(json.RawMessage(`{"projectId":"project-1"}`)) ||
		!threadStartUsesControllerState(json.RawMessage(`{"sectionId":"section-1"}`)) ||
		threadStartUsesControllerState(json.RawMessage(`{"projectId":null}`)) {
		t.Fatal("thread/start Controller-state affinity was not detected precisely")
	}
}

func TestProcessGlobalMutationStateSurvivesChildRestartReplay(t *testing.T) {
	multiplexer := &Multiplexer{globalMutations: make(map[string]globalMutation)}
	multiplexer.rememberGlobalMutation("experimentalFeature/enablement/set", json.RawMessage(`{
		"enablement":{"alpha":true}
	}`))
	multiplexer.rememberGlobalMutation("experimentalFeature/enablement/set", json.RawMessage(`{
		"enablement":{"beta":false}
	}`))
	multiplexer.rememberGlobalMutation("environment/add", json.RawMessage(`{
		"environmentId":"env-1","execServerUrl":"wss://one"
	}`))
	multiplexer.rememberGlobalMutation("environment/add", json.RawMessage(`{
		"environmentId":"env-2","execServerUrl":"wss://two"
	}`))
	multiplexer.rememberGlobalMutation("skills/extraRoots/set", json.RawMessage(`{"extraRoots":["/old"]}`))
	multiplexer.rememberGlobalMutation("skills/extraRoots/set", json.RawMessage(`{"extraRoots":["/current"]}`))
	if len(multiplexer.globalOrder) != 4 || len(multiplexer.globalMutations) != 4 {
		t.Fatalf("unexpected replay state: order=%v mutations=%#v", multiplexer.globalOrder, multiplexer.globalMutations)
	}
	feature := multiplexer.globalMutations["experimentalFeature/enablement/set"]
	var featureParams map[string]any
	if err := json.Unmarshal(feature.params, &featureParams); err != nil {
		t.Fatal(err)
	}
	enablement, _ := featureParams["enablement"].(map[string]any)
	if enablement["alpha"] != true || enablement["beta"] != false {
		t.Fatalf("incremental feature settings were not merged: %s", feature.params)
	}
	if got := string(multiplexer.globalMutations["skills/extraRoots/set"].params); got != `{"extraRoots":["/current"]}` {
		t.Fatalf("replace-style global state retained a stale value: %s", got)
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

func TestCrossAccountResumeAndForkCarryEffectiveCapabilityOverrides(t *testing.T) {
	info := threadResumeInfo{
		ID: "thread-1", Path: "/tmp/thread.jsonl", CWD: "/tmp/project", ModelProvider: "openai",
	}
	requirement := modelRequirement{
		Model: "daybreak-blue", ModelKnown: true,
		Effort: "xhigh", EffortKnown: true,
		ServiceTier: "priority", ServiceTierKnown: true,
	}
	for _, method := range []string{"thread/resume", "thread/fork"} {
		params, err := crossAccountThreadParams(method, json.RawMessage(`{
			"threadId":"thread-1","excludeTurns":true,"config":{"unrelated":"preserved"}
		}`), info, requirement)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(params, &decoded); err != nil {
			t.Fatal(err)
		}
		config, _ := decoded["config"].(map[string]any)
		if decoded["path"] != "/tmp/thread.jsonl" || decoded["cwd"] != "/tmp/project" ||
			decoded["model"] != "daybreak-blue" || decoded["serviceTier"] != "priority" ||
			config["model"] != "daybreak-blue" || config["model_reasoning_effort"] != "xhigh" ||
			config["unrelated"] != "preserved" || decoded["excludeTurns"] != true {
			t.Fatalf("%s lost cross-account overrides: %s", method, params)
		}
		_, historyPresent := decoded["history"]
		if historyPresent != (method == "thread/resume") {
			t.Fatalf("%s encoded an invalid history field: %s", method, params)
		}
	}
	explicit, err := crossAccountThreadParams("thread/resume", json.RawMessage(`{
		"threadId":"thread-1","cwd":null,"modelProvider":"custom"
	}`), info, requirement)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(explicit, &decoded); err != nil {
		t.Fatal(err)
	}
	if cwd, present := decoded["cwd"]; !present || cwd != nil || decoded["modelProvider"] != "custom" {
		t.Fatalf("explicit cwd/provider overrides were replaced: %s", explicit)
	}
}

func TestMutableObjectParamsHandlesExplicitNull(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`[]`)} {
		params := mutableObjectParams(raw)
		params["limit"] = 500
		if params["limit"] != 500 {
			t.Fatalf("mutable params rejected %s", raw)
		}
	}
}

func TestThreadSortingHonorsRequestedKeyDirectionAndSectionOrder(t *testing.T) {
	original := []map[string]any{
		{"id": "C", "createdAt": float64(3), "updatedAt": float64(1), "recencyAt": float64(2)},
		{"id": "A", "createdAt": float64(1), "updatedAt": float64(3), "recencyAt": nil},
		{"id": "B", "createdAt": float64(2), "updatedAt": float64(2), "recencyAt": float64(1)},
	}
	tests := []struct {
		params string
		want   string
	}{
		{`{"sortKey":"created_at","sortDirection":"asc"}`, "ABC"},
		{`{"sortKey":"updated_at","sortDirection":"desc"}`, "ABC"},
		{`{"sortKey":"recency_at","sortDirection":"asc"}`, "ABC"},
		{`{}`, "CBA"},
		{`{"sortKey":"section_position","sortDirection":"asc"}`, "CAB"},
	}
	for _, test := range tests {
		threads := make([]map[string]any, len(original))
		for index, thread := range original {
			copy := make(map[string]any, len(thread))
			for key, value := range thread {
				copy[key] = value
			}
			threads[index] = copy
		}
		sortThreads(threads, json.RawMessage(test.params))
		got := ""
		for _, thread := range threads {
			got += anyString(thread["id"])
		}
		if got != test.want {
			t.Fatalf("sort %s = %s, want %s", test.params, got, test.want)
		}
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

func TestUnavailableCatalogRemainsUnknown(t *testing.T) {
	multiplexer := &Multiplexer{children: make(map[string]*backend.Child)}
	states := multiplexer.modelCapabilityStates(context.Background(), []AccountSnapshot{{
		ID: "secondary", Enabled: true, Connected: true, AuthType: "chatgpt",
	}}, modelRequirement{Model: "daybreak-blue", ModelKnown: true})
	if states["secondary"] != modelCapabilityUnknown {
		t.Fatalf("unavailable catalog was treated as a definitive result: %#v", states)
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
	if !multiplexer.shouldForwardNotification("secondary", protocol.Message{Method: "model/rerouted"}) ||
		!multiplexer.shouldForwardNotification("secondary", protocol.Message{Method: "model/verification"}) ||
		!multiplexer.shouldForwardNotification("secondary", protocol.Message{Method: "model/safetyBuffering/updated"}) {
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

func TestThreadStartReturnsRoutingMetadataPersistenceError(t *testing.T) {
	root := t.TempDir()
	storeRoot := filepath.Join(root, "mux")
	store, err := state.Open(storeRoot, filepath.Join(root, "primary"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(storeRoot); err != nil {
		t.Fatal(err)
	}
	multiplexer := &Multiplexer{store: store}
	err = multiplexer.learnThreadOwner(externalRoute{
		method: "thread/start",
		message: protocol.Message{Params: json.RawMessage(
			`{"model":"daybreak-blue"}`,
		)},
	}, "primary", json.RawMessage(`{"thread":{"id":"thread-1"}}`))
	if err == nil {
		t.Fatal("thread/start hid routing metadata persistence failure")
	}
	if _, exists := store.ThreadOwner("thread-1"); exists {
		t.Fatal("failed routing metadata write was not rolled back")
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

func TestServerRequestResolvedUsesForwardedID(t *testing.T) {
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
	var forwarded json.RawMessage
	for _, route := range multiplexer.serverRoutes {
		forwarded = route.forwarded
	}
	encoded, ok := multiplexer.rewriteServerRequestResolved("secondary", protocol.Message{
		Method: "serverRequest/resolved",
		Params: json.RawMessage(`{"threadId":"thread-1","requestId":17}`),
	})
	if !ok {
		t.Fatal("serverRequest/resolved was not rewritten")
	}
	message, err := protocol.Parse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(message.Params, &params); err != nil {
		t.Fatal(err)
	}
	if protocol.RequestIDKey(params["requestId"]) != protocol.RequestIDKey(forwarded) {
		t.Fatalf("resolved request ID was not translated: got=%s want=%s", params["requestId"], forwarded)
	}
	if len(multiplexer.serverRoutes) != 0 {
		t.Fatalf("resolved route was retained: %#v", multiplexer.serverRoutes)
	}
}

func TestAttestationServerRequestCompletesOnResponse(t *testing.T) {
	if !serverRequestCompletesOnResponse("attestation/generate") {
		t.Fatal("attestation route would wait forever for serverRequest/resolved")
	}
	if serverRequestCompletesOnResponse("item/commandExecution/requestApproval") {
		t.Fatal("approval route was classified as response-complete")
	}
}

func TestSecondaryThreadScopedNotificationsUseOwner(t *testing.T) {
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "mux"), filepath.Join(root, "primary"))
	if err != nil {
		t.Fatal(err)
	}
	secondary, err := store.AddAccount("Secondary")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetThreadOwner("thread-1", secondary.ID); err != nil {
		t.Fatal(err)
	}
	multiplexer := &Multiplexer{store: store}
	message := protocol.Message{Method: "warning", Params: json.RawMessage(`{"threadId":"thread-1"}`)}
	if !multiplexer.shouldForwardNotification(secondary.ID, message) {
		t.Fatal("owner warning notification was dropped")
	}
	if multiplexer.shouldForwardNotification("stale-account", message) {
		t.Fatal("non-owner warning notification was forwarded")
	}
}

func TestInternalThreadStartedSuppressionIsSingleUse(t *testing.T) {
	multiplexer := &Multiplexer{internalResumes: make(map[string][]*internalResumeSuppression)}
	suppression := multiplexer.registerInternalResume("secondary", "thread-1", true)
	if !multiplexer.suppressInternalResumeNotification("secondary", "thread-1", "thread/tokenUsage/updated", []byte(`{"method":"thread/tokenUsage/updated"}`)) ||
		!multiplexer.suppressInternalResumeNotification("secondary", "thread-1", "thread/started", []byte(`{"method":"thread/started"}`)) {
		t.Fatal("internal thread/started notification was not consumed")
	}
	if captured := multiplexer.finishInternalResume(suppression); len(captured) != 2 {
		t.Fatalf("captured bootstrap notifications = %d, want 2", len(captured))
	}
	if multiplexer.suppressInternalResumeNotification("secondary", "thread-1", "thread/started", nil) {
		t.Fatal("one suppression consumed more than one notification")
	}
}

func TestThreadListRouterCursorPreservesPageSize(t *testing.T) {
	output := &bytes.Buffer{}
	now := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	multiplexer := &Multiplexer{
		output:            output,
		now:               func() time.Time { return now },
		threadListCursors: make(map[string]threadListCursorState),
	}
	threads := []map[string]any{{"id": "A"}, {"id": "B"}, {"id": "C"}}
	multiplexer.writeThreadListPage(protocol.Message{
		ID: json.RawMessage(`1`), Params: json.RawMessage(`{"limit":2}`),
	}, threadListCursorState{threads: threads, pageSize: 2})
	first, err := protocol.Parse(bytes.TrimSpace(output.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var firstResult struct {
		Data            []map[string]any `json:"data"`
		NextCursor      *string          `json:"nextCursor"`
		BackwardsCursor *string          `json:"backwardsCursor"`
	}
	if err := json.Unmarshal(first.Result, &firstResult); err != nil {
		t.Fatal(err)
	}
	if len(firstResult.Data) != 2 || firstResult.NextCursor == nil || firstResult.BackwardsCursor == nil {
		t.Fatalf("first page ignored limit/cursor: %s", first.Result)
	}
	output.Reset()
	cursorParams, _ := json.Marshal(map[string]any{"cursor": *firstResult.NextCursor})
	multiplexer.aggregateThreadList(protocol.Message{ID: json.RawMessage(`2`), Params: cursorParams})
	second, err := protocol.Parse(bytes.TrimSpace(output.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var secondResult struct {
		Data            []map[string]any `json:"data"`
		NextCursor      *string          `json:"nextCursor"`
		BackwardsCursor *string          `json:"backwardsCursor"`
	}
	if err := json.Unmarshal(second.Result, &secondResult); err != nil {
		t.Fatal(err)
	}
	if len(secondResult.Data) != 1 || anyString(secondResult.Data[0]["id"]) != "C" ||
		secondResult.NextCursor != nil || secondResult.BackwardsCursor == nil {
		t.Fatalf("second page was not resumed from Router cursor: %s", second.Result)
	}
	output.Reset()
	backwardsParams, _ := json.Marshal(map[string]any{
		"cursor": *secondResult.BackwardsCursor, "sortDirection": "asc",
	})
	multiplexer.aggregateThreadList(protocol.Message{ID: json.RawMessage(`3`), Params: backwardsParams})
	backwards, err := protocol.Parse(bytes.TrimSpace(output.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var backwardsResult struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(backwards.Result, &backwardsResult); err != nil {
		t.Fatal(err)
	}
	if len(backwardsResult.Data) != 2 || anyString(backwardsResult.Data[0]["id"]) != "B" ||
		anyString(backwardsResult.Data[1]["id"]) != "A" {
		t.Fatalf("backwards cursor did not reverse from the page anchor: %s", backwards.Result)
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
