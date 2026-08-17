package mux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/b-nnett/codex-subscription-router/internal/backend"
	"github.com/b-nnett/codex-subscription-router/internal/protocol"
)

var (
	errNoModelCapableSubscription = errors.New("no enabled subscription with known capacity supports the required model capability")
	errModelCapabilityUnavailable = errors.New("model capability catalog is unavailable")
)

type modelRequirement struct {
	Model       string
	Effort      string
	ServiceTier string
}

func (requirement modelRequirement) empty() bool {
	return requirement.Model == "" && requirement.Effort == "" && requirement.ServiceTier == ""
}

func (requirement modelRequirement) overlay(override modelRequirement) modelRequirement {
	if override.Model != "" {
		requirement.Model = override.Model
	}
	if override.Effort != "" {
		requirement.Effort = override.Effort
	}
	if override.ServiceTier != "" {
		requirement.ServiceTier = override.ServiceTier
	}
	return requirement
}

func (m *Multiplexer) aggregateModelList(request protocol.Message) {
	entries := m.childEntries()
	if len(entries) == 0 {
		m.write(protocol.Failure(request.ID, -32033, "no connected subscription can list models"))
		return
	}
	type result struct {
		index     int
		accountID string
		models    []map[string]any
		err       error
	}
	results := make(chan result, len(entries))
	for index, entry := range entries {
		go func(index int, entry childEntry) {
			models, err := m.listAllModels(context.Background(), entry.child, request.Params)
			results <- result{index: index, accountID: entry.account.ID, models: models, err: err}
		}(index, entry)
	}

	ordered := make([]result, len(entries))
	succeeded := 0
	failedAccounts := make([]string, 0)
	var firstErr error
	for range entries {
		result := <-results
		if result.err != nil {
			failedAccounts = append(failedAccounts, result.accountID)
			if firstErr == nil {
				firstErr = result.err
			}
			continue
		}
		ordered[result.index] = result
		succeeded++
	}
	if succeeded == 0 {
		m.write(protocol.Failure(request.ID, -32033, fmt.Sprintf("failed to list models from every subscription: %v", firstErr)))
		return
	}
	if len(failedAccounts) > 0 {
		m.publish(Event{
			Type:    "model-list-partial",
			Message: "Some subscriptions could not provide their model catalog",
			Data:    map[string]any{"failedAccountIds": failedAccounts},
		})
	}
	merged := make([]map[string]any, 0)
	mergedIndex := make(map[string]int)
	hasDefault := false
	for _, result := range ordered {
		if result.err != nil || result.accountID == "" {
			continue
		}
		for _, model := range result.models {
			key := modelKey(model)
			if key == "" {
				encoded, _ := json.Marshal(model)
				key = string(encoded)
			}
			if index, exists := mergedIndex[key]; exists {
				mergeModelCapabilities(merged[index], model)
				continue
			}
			if isDefault, _ := model["isDefault"].(bool); isDefault {
				if hasDefault {
					copy := make(map[string]any, len(model))
					for field, value := range model {
						copy[field] = value
					}
					copy["isDefault"] = false
					model = copy
				} else {
					hasDefault = true
				}
			}
			mergedIndex[key] = len(merged)
			merged = append(merged, model)
		}
	}
	encoded, err := json.Marshal(map[string]any{"data": merged, "nextCursor": nil})
	if err != nil {
		m.write(protocol.Failure(request.ID, -32603, "failed to merge model list"))
		return
	}
	m.write(protocol.Success(request.ID, encoded))
}

func (m *Multiplexer) modelCapableAccounts(
	ctx context.Context,
	snapshots []AccountSnapshot,
	requirement modelRequirement,
) (map[string]bool, error) {
	capable := make(map[string]bool)
	type result struct {
		accountID string
		supports  bool
		err       error
	}
	results := make(chan result, len(snapshots))
	attempted := 0
	for _, snapshot := range snapshots {
		if !snapshot.Enabled || !snapshot.Connected || snapshot.AuthType != "chatgpt" {
			continue
		}
		child, ok := m.child(snapshot.ID)
		if !ok {
			continue
		}
		attempted++
		go func(accountID string, child *backend.Child) {
			models, err := m.listAllModels(ctx, child, json.RawMessage(`{"includeHidden":true}`))
			results <- result{accountID: accountID, supports: modelsSupportRequirement(models, requirement), err: err}
		}(snapshot.ID, child)
	}
	succeeded := 0
	var firstErr error
	for range attempted {
		result := <-results
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
			}
			continue
		}
		succeeded++
		if result.supports {
			capable[result.accountID] = true
		}
	}
	if attempted > 0 && succeeded == 0 {
		return capable, fmt.Errorf("%w: %v", errModelCapabilityUnavailable, firstErr)
	}
	return capable, nil
}

func (m *Multiplexer) accountSupportsRequirement(
	ctx context.Context,
	snapshot AccountSnapshot,
	requirement modelRequirement,
) (bool, error) {
	if requirement.Model == "" {
		return false, errors.New("thread model metadata is unavailable")
	}
	child, ok := m.child(snapshot.ID)
	if !ok {
		return false, fmt.Errorf("subscription %s is unavailable", snapshot.ID)
	}
	models, err := m.listAllModels(ctx, child, json.RawMessage(`{"includeHidden":true}`))
	if err != nil {
		return false, fmt.Errorf("%w for subscription %s: %v", errModelCapabilityUnavailable, snapshot.ID, err)
	}
	return modelsSupportRequirement(models, requirement), nil
}

func (m *Multiplexer) listAllModels(parent context.Context, child *backend.Child, originalParams json.RawMessage) ([]map[string]any, error) {
	var params map[string]any
	if len(originalParams) == 0 || json.Unmarshal(originalParams, &params) != nil {
		params = make(map[string]any)
	}
	params["limit"] = 500
	models := make([]map[string]any, 0)
	seenCursors := make(map[string]struct{})
	var cursor string
	for {
		if cursor == "" {
			params["cursor"] = nil
		} else {
			params["cursor"] = cursor
		}
		encodedParams, _ := json.Marshal(params)
		ctx, cancel := context.WithTimeout(parent, requestTimeout)
		response, err := child.Request(ctx, "model/list", encodedParams)
		cancel()
		if err != nil {
			return nil, err
		}
		var decoded struct {
			Data       []map[string]any `json:"data"`
			NextCursor *string          `json:"nextCursor"`
		}
		if err := json.Unmarshal(response.Result, &decoded); err != nil {
			return nil, fmt.Errorf("decode model list: %w", err)
		}
		models = append(models, decoded.Data...)
		if decoded.NextCursor == nil || *decoded.NextCursor == "" {
			return models, nil
		}
		cursor = *decoded.NextCursor
		if _, repeated := seenCursors[cursor]; repeated {
			return nil, errors.New("model list repeated a cursor")
		}
		seenCursors[cursor] = struct{}{}
	}
}

func modelFromParams(params json.RawMessage) string {
	return modelRequirementFromParams(params).Model
}

func modelsContain(models []map[string]any, required string) bool {
	return modelsSupportRequirement(models, modelRequirement{Model: required})
}

func modelRequirementFromParams(params json.RawMessage) modelRequirement {
	var decoded map[string]any
	if json.Unmarshal(params, &decoded) != nil {
		return modelRequirement{}
	}
	requirement := modelRequirement{
		Model:       anyString(decoded["model"]),
		Effort:      anyString(decoded["effort"]),
		ServiceTier: anyString(decoded["serviceTier"]),
	}
	if collaboration, ok := decoded["collaborationMode"].(map[string]any); ok {
		if settings, ok := collaboration["settings"].(map[string]any); ok {
			if model := anyString(settings["model"]); model != "" {
				requirement.Model = model
			}
			if effort := anyString(settings["reasoning_effort"]); effort != "" {
				requirement.Effort = effort
			}
		}
	}
	return requirement
}

func modelsSupportRequirement(models []map[string]any, requirement modelRequirement) bool {
	if requirement.Model == "" {
		return requirement.empty()
	}
	for _, model := range models {
		matchesModel := false
		for _, key := range []string{"model", "id"} {
			if value, ok := model[key].(string); ok && value == requirement.Model {
				matchesModel = true
				break
			}
		}
		if !matchesModel {
			continue
		}
		if requirement.Effort != "" && !modelSupportsEffort(model, requirement.Effort) {
			continue
		}
		if requirement.ServiceTier != "" && !modelSupportsServiceTier(model, requirement.ServiceTier) {
			continue
		}
		return true
	}
	return false
}

func modelSupportsEffort(model map[string]any, effort string) bool {
	if anyString(model["defaultReasoningEffort"]) == effort {
		return true
	}
	for _, option := range anySlice(model["supportedReasoningEfforts"]) {
		if fields, ok := option.(map[string]any); ok && anyString(fields["reasoningEffort"]) == effort {
			return true
		}
	}
	return false
}

func modelSupportsServiceTier(model map[string]any, serviceTier string) bool {
	if anyString(model["defaultServiceTier"]) == serviceTier {
		return true
	}
	for _, option := range anySlice(model["serviceTiers"]) {
		if fields, ok := option.(map[string]any); ok && anyString(fields["id"]) == serviceTier {
			return true
		}
	}
	for _, option := range anySlice(model["additionalSpeedTiers"]) {
		if anyString(option) == serviceTier {
			return true
		}
	}
	return false
}

func mergeModelCapabilities(target, source map[string]any) {
	target["supportedReasoningEfforts"] = mergeObjectArrayByKey(
		anySlice(target["supportedReasoningEfforts"]),
		anySlice(source["supportedReasoningEfforts"]),
		"reasoningEffort",
	)
	target["serviceTiers"] = mergeObjectArrayByKey(
		anySlice(target["serviceTiers"]),
		anySlice(source["serviceTiers"]),
		"id",
	)
	target["additionalSpeedTiers"] = mergeStringArray(
		anySlice(target["additionalSpeedTiers"]),
		anySlice(source["additionalSpeedTiers"]),
	)
}

func mergeObjectArrayByKey(target, source []any, key string) []any {
	merged := append([]any(nil), target...)
	seen := make(map[string]struct{}, len(target))
	for _, value := range target {
		if fields, ok := value.(map[string]any); ok {
			seen[anyString(fields[key])] = struct{}{}
		}
	}
	for _, value := range source {
		fields, ok := value.(map[string]any)
		if !ok {
			continue
		}
		identifier := anyString(fields[key])
		if identifier == "" {
			continue
		}
		if _, exists := seen[identifier]; exists {
			continue
		}
		seen[identifier] = struct{}{}
		merged = append(merged, value)
	}
	return merged
}

func mergeStringArray(target, source []any) []any {
	merged := append([]any(nil), target...)
	seen := make(map[string]struct{}, len(target))
	for _, value := range target {
		seen[anyString(value)] = struct{}{}
	}
	for _, value := range source {
		identifier := anyString(value)
		if identifier == "" {
			continue
		}
		if _, exists := seen[identifier]; exists {
			continue
		}
		seen[identifier] = struct{}{}
		merged = append(merged, identifier)
	}
	return merged
}

func anySlice(value any) []any {
	values, _ := value.([]any)
	return values
}

func anyString(value any) string {
	text, _ := value.(string)
	return text
}

func paramsWithModelRequirement(params json.RawMessage, requirement modelRequirement) json.RawMessage {
	var decoded map[string]any
	if json.Unmarshal(params, &decoded) != nil {
		return params
	}
	if requirement.Model != "" {
		decoded["model"] = requirement.Model
	}
	if requirement.Effort != "" {
		decoded["effort"] = requirement.Effort
	}
	if requirement.ServiceTier != "" {
		decoded["serviceTier"] = requirement.ServiceTier
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return params
	}
	return encoded
}

func modelKey(model map[string]any) string {
	for _, key := range []string{"model", "id"} {
		if value, ok := model[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}
