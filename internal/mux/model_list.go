package mux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/b-nnett/codex-subscription-router/internal/backend"
	"github.com/b-nnett/codex-subscription-router/internal/protocol"
)

var errNoModelCapableSubscription = errors.New("no enabled subscription with known capacity supports model")

func (m *Multiplexer) aggregateModelList(request protocol.Message) {
	entries := m.childEntries()
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
	for range entries {
		result := <-results
		if result.err != nil {
			m.write(protocol.Failure(request.ID, -32033, fmt.Sprintf("failed to list models for subscription %s: %v", result.accountID, result.err)))
			return
		}
		ordered[result.index] = result
	}
	merged := make([]map[string]any, 0)
	seen := make(map[string]struct{})
	hasDefault := false
	for _, result := range ordered {
		for _, model := range result.models {
			key := modelKey(model)
			if key == "" {
				encoded, _ := json.Marshal(model)
				key = string(encoded)
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
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

func (m *Multiplexer) modelCapableAccounts(ctx context.Context, snapshots []AccountSnapshot, requiredModel string) map[string]bool {
	capable := make(map[string]bool)
	var mu sync.Mutex
	var wait sync.WaitGroup
	for _, snapshot := range snapshots {
		if !snapshot.Enabled || !snapshot.Connected || snapshot.AuthType != "chatgpt" {
			continue
		}
		child, ok := m.child(snapshot.ID)
		if !ok {
			continue
		}
		wait.Add(1)
		go func(accountID string, child *backend.Child) {
			defer wait.Done()
			models, err := m.listAllModels(ctx, child, nil)
			if err != nil || !modelsContain(models, requiredModel) {
				return
			}
			mu.Lock()
			capable[accountID] = true
			mu.Unlock()
		}(snapshot.ID, child)
	}
	wait.Wait()
	return capable
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
	var decoded map[string]any
	if json.Unmarshal(params, &decoded) != nil {
		return ""
	}
	model, _ := decoded["model"].(string)
	return model
}

func modelsContain(models []map[string]any, required string) bool {
	for _, model := range models {
		for _, key := range []string{"model", "id"} {
			if value, ok := model[key].(string); ok && value == required {
				return true
			}
		}
	}
	return false
}

func modelKey(model map[string]any) string {
	for _, key := range []string{"model", "id"} {
		if value, ok := model[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}
