package mux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/b-nnett/codex-subscription-router/internal/backend"
	"github.com/b-nnett/codex-subscription-router/internal/protocol"
)

type globalMutation struct {
	method string
	params json.RawMessage
}

type globalStateTarget struct {
	accountID  string
	controller bool
	child      *backend.Child
	err        error
}

// Server-generated global identities stay in the Controller's state database.
// Methods with thread-scoped state are deliberately excluded from this list.
func controllerGlobalStateMethod(method string) bool {
	return strings.HasPrefix(method, "project/") ||
		strings.HasPrefix(method, "threadSection/") ||
		method == "environment/info" ||
		method == "environment/status" ||
		method == "experimentalFeature/list"
}

func (m *Multiplexer) routeControllerRequest(message protocol.Message) {
	controller, ok := m.store.Controller()
	if !ok {
		m.write(protocol.Failure(message.ID, -32022, "no controller account is configured"))
		return
	}
	if err := m.forward(controller.ID, message); err != nil {
		m.write(protocol.Failure(message.ID, -32023, err.Error()))
	}
}

// These mutations are safe to replicate because their identity and desired
// value come from the client. Commit the Controller first, remember that
// authoritative value, then update every Secondary. A Secondary with an
// uncertain outcome is removed from routing and rebuilt from the journal.
func (m *Multiplexer) broadcastGlobalMutation(message protocol.Message) {
	accounts := m.store.Accounts()
	controllerAccount, controllerConfigured := m.store.Controller()
	if !controllerConfigured {
		m.write(protocol.Failure(message.ID, -32022, "no controller account is configured"))
		return
	}
	enabled := make([]globalStateTarget, 0, len(accounts))
	for _, account := range accounts {
		if !account.Enabled {
			continue
		}
		enabled = append(enabled, globalStateTarget{accountID: account.ID, controller: account.ID == controllerAccount.ID})
	}
	if len(enabled) == 0 {
		m.write(protocol.Failure(message.ID, -32022, "no controller account is configured"))
		return
	}

	ensureCtx, ensureCancel := context.WithTimeout(context.Background(), controlRequestTimeout)
	defer ensureCancel()
	ensured := make(chan globalStateTarget, len(enabled))
	for _, candidate := range enabled {
		go func(candidate globalStateTarget) {
			candidate.child, candidate.err = m.ensureChild(ensureCtx, candidate.accountID)
			ensured <- candidate
		}(candidate)
	}
	secondaries := make([]globalStateTarget, 0, len(enabled)-1)
	var controller globalStateTarget
	hasController := false
	failed := make([]string, 0)
	var firstErr error
	for range enabled {
		candidate := <-ensured
		if candidate.err != nil {
			failed = append(failed, candidate.accountID)
			if firstErr == nil {
				firstErr = candidate.err
			}
			continue
		}
		if candidate.controller {
			controller = candidate
			hasController = true
		} else {
			secondaries = append(secondaries, candidate)
		}
	}
	if !hasController {
		if firstErr == nil {
			firstErr = errors.New("controller app-server is unavailable")
		}
		m.write(protocol.Failure(message.ID, -32037, fmt.Sprintf("cannot synchronize %s: %v", message.Method, firstErr)))
		return
	}
	if len(failed) > 0 {
		m.write(protocol.Failure(message.ID, -32037, fmt.Sprintf(
			"%s was not applied because enabled app-servers are unavailable (%s): %v",
			message.Method, strings.Join(failed, ", "), firstErr,
		)))
		return
	}
	ensureCancel()

	m.globalApplyMu.Lock()
	controllerCtx, controllerCancel := context.WithTimeout(context.Background(), controlRequestTimeout)
	response, err := controller.child.Request(controllerCtx, message.Method, message.Params)
	controllerCancel()
	if err != nil {
		m.globalApplyMu.Unlock()
		var restoreErr error
		if response.Error == nil {
			// A timeout or transport failure has an uncertain commit outcome. A
			// structured RPC rejection is definitive and leaves the child usable.
			restoreErr = m.restoreGlobalStateChild(controller.accountID, controller.child)
		}
		m.publish(Event{
			Type:    "global-state-sync-failed",
			Message: "Controller rejected a process-wide app-server setting",
			Data:    map[string]any{"method": message.Method},
		})
		if restoreErr != nil {
			m.write(protocol.Failure(message.ID, -32037, fmt.Sprintf(
				"Controller %s failed: %v; Controller baseline restore failed: %v", message.Method, err, restoreErr,
			)))
		} else {
			m.write(protocol.Failure(message.ID, -32037, fmt.Sprintf("Controller %s failed: %v", message.Method, err)))
		}
		return
	}
	m.rememberGlobalMutation(message.Method, message.Params)

	type result struct {
		target globalStateTarget
		err    error
	}
	results := make(chan result, len(secondaries))
	for _, candidate := range secondaries {
		go func(candidate globalStateTarget) {
			ctx, cancel := context.WithTimeout(context.Background(), controlRequestTimeout)
			_, err := candidate.child.Request(ctx, message.Method, message.Params)
			cancel()
			results <- result{target: candidate, err: err}
		}(candidate)
	}
	failed = failed[:0]
	firstErr = nil
	failedTargets := make([]globalStateTarget, 0)
	for range secondaries {
		result := <-results
		if result.err != nil {
			failed = append(failed, result.target.accountID)
			failedTargets = append(failedTargets, result.target)
			if firstErr == nil {
				firstErr = result.err
			}
		}
	}
	m.globalApplyMu.Unlock()
	if len(failed) > 0 {
		restoreErrors := m.restoreGlobalStateChildren(failedTargets)
		eventType := "global-state-sync-recovered"
		eventMessage := "Secondary app-servers were rebuilt from the authoritative process-wide state"
		if len(restoreErrors) > 0 {
			eventType = "global-state-sync-failed"
			eventMessage = "Some Secondary app-servers could not be rebuilt from the authoritative process-wide state"
		}
		m.publish(Event{
			Type:    eventType,
			Message: eventMessage,
			Data: map[string]any{
				"method": message.Method, "failedAccountIds": failed, "restoreErrors": restoreErrors,
			},
		})
		if len(restoreErrors) > 0 {
			m.write(protocol.Failure(message.ID, -32037, fmt.Sprintf(
				"Controller applied %s, but some Secondary app-servers could not be restored (%s): %v",
				message.Method, strings.Join(failed, ", "), firstErr,
			)))
			return
		}
	}
	m.write(protocol.Success(message.ID, response.Result))
}

func (m *Multiplexer) restoreGlobalStateChild(accountID string, child *backend.Child) error {
	m.discardChild(accountID, child, "process-wide app-server state became uncertain")
	account, ok := m.store.Account(accountID)
	if !ok || !account.Enabled {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*controlRequestTimeout)
	defer cancel()
	if _, err := m.startChild(ctx, account); err != nil {
		return fmt.Errorf("restart %s from the authoritative global-state journal: %w", accountID, err)
	}
	return nil
}

func (m *Multiplexer) restoreGlobalStateChildren(targets []globalStateTarget) map[string]string {
	errorsByAccount := make(map[string]string)
	for _, target := range targets {
		if err := m.restoreGlobalStateChild(target.accountID, target.child); err != nil {
			errorsByAccount[target.accountID] = err.Error()
		}
	}
	return errorsByAccount
}

func (m *Multiplexer) rememberGlobalMutation(method string, params json.RawMessage) {
	key := globalMutationKey(method, params)
	m.globalStateMu.Lock()
	defer m.globalStateMu.Unlock()
	if m.globalMutations == nil {
		m.globalMutations = make(map[string]globalMutation)
	}
	if method == "experimentalFeature/enablement/set" {
		if previous, exists := m.globalMutations[key]; exists {
			params = mergeFeatureEnablement(previous.params, params)
		}
	}
	if _, exists := m.globalMutations[key]; !exists {
		m.globalOrder = append(m.globalOrder, key)
	}
	m.globalMutations[key] = globalMutation{
		method: method, params: append(json.RawMessage(nil), params...),
	}
}

func globalMutationKey(method string, params json.RawMessage) string {
	if method != "environment/add" {
		return method
	}
	var decoded map[string]any
	if json.Unmarshal(params, &decoded) == nil {
		if environmentID := anyString(decoded["environmentId"]); environmentID != "" {
			return method + ":" + environmentID
		}
	}
	return method
}

func mergeFeatureEnablement(previous, update json.RawMessage) json.RawMessage {
	var oldParams, newParams map[string]any
	if json.Unmarshal(previous, &oldParams) != nil || json.Unmarshal(update, &newParams) != nil {
		return update
	}
	oldEnablement, oldOK := oldParams["enablement"].(map[string]any)
	newEnablement, newOK := newParams["enablement"].(map[string]any)
	if !oldOK || !newOK {
		return update
	}
	for feature, enabled := range newEnablement {
		oldEnablement[feature] = enabled
	}
	encoded, err := json.Marshal(map[string]any{"enablement": oldEnablement})
	if err != nil {
		return update
	}
	return encoded
}

func (m *Multiplexer) replayGlobalMutations(parent context.Context, child *backend.Child) error {
	m.globalStateMu.RLock()
	mutations := make([]globalMutation, 0, len(m.globalOrder))
	for _, key := range m.globalOrder {
		mutation := m.globalMutations[key]
		mutation.params = append(json.RawMessage(nil), mutation.params...)
		mutations = append(mutations, mutation)
	}
	m.globalStateMu.RUnlock()
	for _, mutation := range mutations {
		ctx, cancel := context.WithTimeout(parent, controlRequestTimeout)
		_, err := child.Request(ctx, mutation.method, mutation.params)
		cancel()
		if err != nil {
			return fmt.Errorf("%s: %w", mutation.method, err)
		}
	}
	return nil
}

func threadStartUsesControllerState(params json.RawMessage) bool {
	usesProject, usesSection := threadStartControllerAffinity(params)
	return usesProject || usesSection
}

func threadStartControllerAffinity(params json.RawMessage) (usesProject, usesSection bool) {
	var decoded map[string]any
	if json.Unmarshal(params, &decoded) != nil {
		return false, false
	}
	for _, key := range []string{"projectId", "project_id"} {
		if value, present := decoded[key]; present && value != nil && anyString(value) != "" {
			usesProject = true
		}
	}
	for _, key := range []string{"sectionId", "section_id"} {
		if value, present := decoded[key]; present && value != nil && anyString(value) != "" {
			usesSection = true
		}
	}
	return usesProject, usesSection
}

func (m *Multiplexer) routeControllerAffinedThread(ctx context.Context, message protocol.Message) {
	controller, ok := m.store.Controller()
	if !ok {
		m.write(protocol.Failure(message.ID, -32022, "no controller account is configured"))
		return
	}
	requirement := modelRequirementFromParams(message.Params)
	if snapshot, err := m.accountSnapshotWithProfile(ctx, controller.ID, false); err == nil {
		if accountQuotaState(snapshot) == quotaCapacityExhausted {
			m.write(protocol.Failure(message.ID, -32035,
				"Controller-local project or section state cannot be routed to another subscription while the Controller is depleted"))
			return
		}
		if requirement.Model != "" {
			supported, capabilityErr := m.accountSupportsRequirement(ctx, snapshot, requirement)
			if capabilityErr == nil && !supported {
				m.write(protocol.Failure(message.ID, -32035, fmt.Sprintf(
					"Controller-local project or section state requires the Controller, but it does not support %s",
					requirementDescription(requirement),
				)))
				return
			}
		}
	}
	if err := m.forward(controller.ID, message); err != nil {
		m.write(protocol.Failure(message.ID, -32023, err.Error()))
		return
	}
	m.publish(Event{
		Type:      "thread-routed",
		AccountID: controller.ID,
		Message:   "New chat pinned to the Controller because it references Controller-local state",
	})
}

func (m *Multiplexer) validateThreadSectionMove(params json.RawMessage, ownerID string) error {
	var decoded map[string]any
	if json.Unmarshal(params, &decoded) != nil {
		return nil
	}
	controller, ok := m.store.Controller()
	if !ok {
		return errors.New("no controller account is configured")
	}
	if sectionID, present := decoded["sectionId"]; present && sectionID != nil && anyString(sectionID) != "" && ownerID != controller.ID {
		return errors.New("thread sections are stored by the Controller; a Secondary-owned thread cannot reference a Controller section")
	}
	if beforeThreadID := anyString(decoded["beforeThreadId"]); beforeThreadID != "" {
		if beforeOwner, known := m.store.ThreadOwner(beforeThreadID); known && beforeOwner != ownerID {
			return errors.New("threads owned by different app-server processes cannot share one section ordering")
		}
	}
	return nil
}

func (m *Multiplexer) aggregateLoadedThreadList(message protocol.Message) {
	entries := m.childEntries()
	if len(entries) == 0 {
		m.write(protocol.Failure(message.ID, -32038, "no connected app-server can list loaded threads"))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), controlRequestTimeout)
	defer cancel()
	type result struct {
		index     int
		accountID string
		threadIDs []string
		err       error
	}
	results := make(chan result, len(entries))
	for index, entry := range entries {
		go func(index int, entry childEntry) {
			threadIDs, err := listAllLoadedThreads(ctx, entry, message.Params)
			results <- result{index: index, accountID: entry.account.ID, threadIDs: threadIDs, err: err}
		}(index, entry)
	}
	ordered := make([]result, len(entries))
	for range entries {
		result := <-results
		if result.err != nil {
			m.write(protocol.Failure(message.ID, -32038, fmt.Sprintf(
				"subscription %s loaded-thread state is unavailable: %v", result.accountID, result.err,
			)))
			return
		}
		ordered[result.index] = result
	}
	seen := make(map[string]struct{})
	threadIDs := make([]string, 0)
	for _, result := range ordered {
		for _, threadID := range result.threadIDs {
			if _, exists := seen[threadID]; exists {
				continue
			}
			seen[threadID] = struct{}{}
			threadIDs = append(threadIDs, threadID)
		}
	}
	encoded, err := json.Marshal(map[string]any{"data": threadIDs, "nextCursor": nil})
	if err != nil {
		m.write(protocol.Failure(message.ID, -32603, "failed to merge loaded thread list"))
		return
	}
	m.write(protocol.Success(message.ID, encoded))
}

func listAllLoadedThreads(parent context.Context, entry childEntry, originalParams json.RawMessage) ([]string, error) {
	params := mutableObjectParams(originalParams)
	params["limit"] = 500
	threadIDs := make([]string, 0)
	seenCursors := make(map[string]struct{})
	var cursor string
	for {
		if cursor == "" {
			params["cursor"] = nil
		} else {
			params["cursor"] = cursor
		}
		encodedParams, _ := json.Marshal(params)
		response, err := entry.child.Request(parent, "thread/loaded/list", encodedParams)
		if err != nil {
			return nil, err
		}
		var decoded struct {
			Data       []string `json:"data"`
			NextCursor *string  `json:"nextCursor"`
		}
		if err := json.Unmarshal(response.Result, &decoded); err != nil {
			return nil, fmt.Errorf("decode loaded thread list: %w", err)
		}
		threadIDs = append(threadIDs, decoded.Data...)
		if decoded.NextCursor == nil || *decoded.NextCursor == "" {
			return threadIDs, nil
		}
		cursor = *decoded.NextCursor
		if _, repeated := seenCursors[cursor]; repeated {
			return nil, fmt.Errorf("loaded thread list repeated cursor %q", cursor)
		}
		seenCursors[cursor] = struct{}{}
	}
}
