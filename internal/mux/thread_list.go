package mux

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/b-nnett/codex-subscription-router/internal/protocol"
)

const (
	defaultThreadListPageSize = 50
	maxThreadListPageSize     = 500
	threadListCursorTTL       = 5 * time.Minute
	maxThreadListCursors      = 16
)

type threadListCursorState struct {
	threads   []map[string]any
	pageSize  int
	expiresAt time.Time
}

func (m *Multiplexer) aggregateThreadList(request protocol.Message) {
	if cursor := threadListCursor(request.Params); cursor != "" {
		state, ok := m.takeThreadListCursor(cursor)
		if !ok {
			m.write(protocol.Failure(request.ID, -32602, "thread/list cursor is invalid or expired"))
			return
		}
		m.writeThreadListPage(request, state)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), controlRequestTimeout)
	defer cancel()
	entries := m.threadListEntries(request.Params)
	if len(entries) == 0 {
		m.write(protocol.Failure(request.ID, -32034, "controller history is unavailable for the requested section"))
		return
	}
	type result struct {
		index     int
		accountID string
		threads   []map[string]any
		err       error
	}
	results := make(chan result, len(entries))
	var wait sync.WaitGroup
	for index, entry := range entries {
		wait.Add(1)
		go func(index int, entry childEntry) {
			defer wait.Done()
			threads, err := m.listAllThreads(ctx, entry, request.Params)
			results <- result{index: index, accountID: entry.account.ID, threads: threads, err: err}
		}(index, entry)
	}
	wait.Wait()
	close(results)

	ordered := make([]result, len(entries))
	for accountResult := range results {
		if accountResult.err != nil {
			m.write(protocol.Failure(request.ID, -32034, fmt.Sprintf("subscription %s history is incomplete: %v", accountResult.accountID, accountResult.err)))
			return
		}
		ordered[accountResult.index] = accountResult
	}

	type ownedThread struct {
		accountID string
		thread    map[string]any
	}
	byID := make(map[string][]ownedThread)
	order := make([]string, 0)
	anonymous := make([]map[string]any, 0)
	for _, accountResult := range ordered {
		for _, thread := range accountResult.threads {
			threadID, _ := thread["id"].(string)
			if threadID == "" {
				anonymous = append(anonymous, thread)
				continue
			}
			if _, exists := byID[threadID]; !exists {
				order = append(order, threadID)
			}
			byID[threadID] = append(byID[threadID], ownedThread{accountID: accountResult.accountID, thread: thread})
		}
	}

	threads := make([]map[string]any, 0, len(order)+len(anonymous))
	owners := make(map[string]string, len(order))
	sectionThreadIDs := make([]string, 0)
	for _, threadID := range order {
		candidates := byID[threadID]
		selected := candidates[0]
		persistedOwner := ""
		if currentOwner, ok := m.store.ThreadOwner(threadID); ok {
			persistedOwner = currentOwner
			for _, candidate := range candidates {
				if candidate.accountID == currentOwner {
					selected = candidate
					break
				}
			}
		}
		threads = append(threads, selected.thread)
		if section, present := selected.thread["section"]; present && section != nil {
			sectionThreadIDs = append(sectionThreadIDs, threadID)
		}
		if persistedOwner != "" {
			owners[threadID] = persistedOwner
		} else {
			owners[threadID] = selected.accountID
		}
	}
	threads = append(threads, anonymous...)
	if err := m.store.MergeThreadMetadata(owners, nil, sectionThreadIDs); err != nil {
		m.write(protocol.Failure(request.ID, -32603, fmt.Sprintf("persist merged thread list: %v", err)))
		return
	}
	sortThreads(threads, request.Params)
	m.writeThreadListPage(request, threadListCursorState{
		threads: threads, pageSize: threadListPageSize(request.Params, defaultThreadListPageSize),
	})
}

func threadListCursor(params json.RawMessage) string {
	var decoded map[string]any
	if json.Unmarshal(params, &decoded) != nil || decoded == nil {
		return ""
	}
	return anyString(decoded["cursor"])
}

func threadListPageSize(params json.RawMessage, fallback int) int {
	var decoded map[string]any
	if json.Unmarshal(params, &decoded) == nil && decoded != nil {
		if value, ok := decoded["limit"].(float64); ok && value > 0 {
			limit := int(value)
			if limit > maxThreadListPageSize {
				return maxThreadListPageSize
			}
			return limit
		}
	}
	if fallback <= 0 {
		return defaultThreadListPageSize
	}
	return fallback
}

func (m *Multiplexer) writeThreadListPage(request protocol.Message, state threadListCursorState) {
	limit := threadListPageSize(request.Params, state.pageSize)
	if limit > len(state.threads) {
		limit = len(state.threads)
	}
	page := state.threads[:limit]
	var nextCursor any
	if limit < len(state.threads) {
		state.threads = state.threads[limit:]
		state.pageSize = limit
		nextCursor = m.storeThreadListCursor(state)
	}
	encoded, err := json.Marshal(map[string]any{"data": page, "nextCursor": nextCursor})
	if err != nil {
		m.write(protocol.Failure(request.ID, -32603, "failed to merge thread list"))
		return
	}
	m.write(protocol.Success(request.ID, encoded))
}

func (m *Multiplexer) storeThreadListCursor(state threadListCursorState) string {
	now := m.now()
	state.expiresAt = now.Add(threadListCursorTTL)
	cursor := fmt.Sprintf("codex-mux-thread:%d", m.threadListSequence.Add(1))
	m.threadListCursorMu.Lock()
	for key, candidate := range m.threadListCursors {
		if !candidate.expiresAt.After(now) {
			delete(m.threadListCursors, key)
		}
	}
	if len(m.threadListCursors) >= maxThreadListCursors {
		oldestKey := ""
		var oldest time.Time
		for key, candidate := range m.threadListCursors {
			if oldestKey == "" || candidate.expiresAt.Before(oldest) {
				oldestKey = key
				oldest = candidate.expiresAt
			}
		}
		delete(m.threadListCursors, oldestKey)
	}
	m.threadListCursors[cursor] = state
	m.threadListCursorMu.Unlock()
	return cursor
}

func (m *Multiplexer) takeThreadListCursor(cursor string) (threadListCursorState, bool) {
	m.threadListCursorMu.Lock()
	state, ok := m.threadListCursors[cursor]
	m.threadListCursorMu.Unlock()
	if !ok || !state.expiresAt.After(m.now()) {
		return threadListCursorState{}, false
	}
	return state, true
}

func (m *Multiplexer) threadListEntries(params json.RawMessage) []childEntry {
	entries := m.childEntries()
	var decoded map[string]any
	if json.Unmarshal(params, &decoded) != nil {
		return entries
	}
	sectionID, present := decoded["sectionId"]
	if !present || sectionID == nil || anyString(sectionID) == "" {
		return entries
	}
	controller, ok := m.store.Controller()
	if !ok {
		return nil
	}
	for _, entry := range entries {
		if entry.account.ID == controller.ID {
			return []childEntry{entry}
		}
	}
	return nil
}

func (m *Multiplexer) listAllThreads(parent context.Context, entry childEntry, originalParams json.RawMessage) ([]map[string]any, error) {
	params := mutableObjectParams(originalParams)
	params["limit"] = 500
	threads := make([]map[string]any, 0)
	seenCursors := make(map[string]struct{})
	var cursor string
	for {
		if cursor == "" {
			params["cursor"] = nil
		} else {
			params["cursor"] = cursor
		}
		encodedParams, _ := json.Marshal(params)
		ctx, cancel := context.WithTimeout(parent, controlRequestTimeout)
		response, err := entry.child.Request(ctx, "thread/list", encodedParams)
		cancel()
		if err != nil {
			return nil, err
		}
		var decoded struct {
			Data       []map[string]any `json:"data"`
			NextCursor *string          `json:"nextCursor"`
		}
		if err := json.Unmarshal(response.Result, &decoded); err != nil {
			return nil, fmt.Errorf("decode thread list: %w", err)
		}
		threads = append(threads, decoded.Data...)
		if decoded.NextCursor == nil || *decoded.NextCursor == "" {
			return threads, nil
		}
		cursor = *decoded.NextCursor
		if _, repeated := seenCursors[cursor]; repeated {
			return nil, fmt.Errorf("thread list repeated cursor %q", cursor)
		}
		seenCursors[cursor] = struct{}{}
	}
}
