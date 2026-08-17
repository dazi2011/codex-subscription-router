package mux

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/b-nnett/codex-subscription-router/internal/protocol"
)

func (m *Multiplexer) aggregateThreadList(request protocol.Message) {
	ctx, cancel := context.WithTimeout(context.Background(), controlRequestTimeout)
	defer cancel()
	entries := m.childEntries()
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
		if persistedOwner != "" {
			owners[threadID] = persistedOwner
		} else {
			owners[threadID] = selected.accountID
		}
	}
	threads = append(threads, anonymous...)
	if err := m.store.MergeThreadMetadata(owners, nil); err != nil {
		m.write(protocol.Failure(request.ID, -32603, fmt.Sprintf("persist merged thread list: %v", err)))
		return
	}
	sortThreads(threads)
	encoded, err := json.Marshal(map[string]any{"data": threads, "nextCursor": nil})
	if err != nil {
		m.write(protocol.Failure(request.ID, -32603, "failed to merge thread list"))
		return
	}
	m.write(protocol.Success(request.ID, encoded))
}

func (m *Multiplexer) listAllThreads(parent context.Context, entry childEntry, originalParams json.RawMessage) ([]map[string]any, error) {
	var params map[string]any
	if json.Unmarshal(originalParams, &params) != nil {
		params = make(map[string]any)
	}
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
