package mux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/b-nnett/codex-subscription-router/internal/protocol"
)

func (m *Multiplexer) routeThreadCapabilityOverride(message protocol.Message, threadID, ownerID string) {
	lock := m.threadLock(threadID)
	lock.Lock()
	defer lock.Unlock()
	if currentOwner, ok := m.store.ThreadOwner(threadID); ok {
		ownerID = currentOwner
	}

	requested := modelRequirementFromParams(message.Params)
	requirement := storedModelRequirement(m.store.ThreadCapability(threadID)).overlay(requested)
	if requirement.Model == "" {
		// Without a concrete model, capability observation is UNKNOWN. Let the
		// current owner resolve its own defaults instead of causing a migration.
		m.forwardThreadOverrideToOwner(message, ownerID)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*controlRequestTimeout)
	defer cancel()
	source, err := m.accountSnapshotWithProfile(ctx, ownerID, false)
	if err != nil || !accountEligibleForRouting(source) {
		// Account/profile observation failed. UNKNOWN is not UNSUPPORTED.
		m.forwardThreadOverrideToOwner(message, ownerID)
		return
	}
	supported, capabilityErr := m.accountSupportsRequirement(ctx, source, requirement)
	if capabilityErr != nil || supported {
		m.forwardThreadOverrideToOwner(message, ownerID)
		return
	}
	if m.store.ControllerAffinedThread(threadID) {
		m.write(protocol.Failure(message.ID, -32035,
			"this chat references Controller-local state, but the Controller does not support the requested model capability"))
		return
	}
	m.migrateThreadCapabilityOverride(ctx, message, threadID, ownerID, source, requested)
}

func (m *Multiplexer) forwardThreadOverrideToOwner(message protocol.Message, ownerID string) {
	if err := m.forward(ownerID, message); err != nil {
		m.write(protocol.Failure(message.ID, -32023, err.Error()))
	}
}

func (m *Multiplexer) migrateThreadCapabilityOverride(
	ctx context.Context,
	message protocol.Message,
	threadID string,
	sourceAccountID string,
	source AccountSnapshot,
	requested modelRequirement,
) {
	info, err := m.readThreadResumeInfo(ctx, threadID, sourceAccountID, message.Method == "thread/fork")
	if err != nil {
		m.write(protocol.Failure(message.ID, -32027, fmt.Sprintf("recover chat settings before capability routing: %v", err)))
		return
	}
	cleanupInternalSource := message.Method == "thread/fork" && info.LoadedKnown && !info.WasLoaded
	defer func() {
		if cleanupInternalSource {
			go m.unsubscribeThreadOnAccount(sourceAccountID, threadID)
		}
	}()
	if !historyModeSupportsCrossProcessFailover(info.HistoryMode) {
		m.write(protocol.Failure(message.ID, -32027,
			"the chat history mode cannot be opened safely by a different app-server process"))
		return
	}
	requirement := info.Capability.overlay(requested)
	if requirement.Model == "" {
		m.write(protocol.Failure(message.ID, -32031, errUnknownThreadModelCapability.Error()))
		return
	}
	if sourceStillSupports, supportErr := m.accountSupportsRequirement(ctx, source, requirement); supportErr != nil || sourceStillSupports {
		// The source resume refreshed stale effective settings. UNKNOWN remains
		// non-destructive, and a now-compatible owner should handle the request.
		if cleanupInternalSource {
			if err := m.forwardWithCleanup(sourceAccountID, message, sourceAccountID, threadID); err != nil {
				m.write(protocol.Failure(message.ID, -32023, err.Error()))
			} else {
				cleanupInternalSource = false
			}
		} else {
			m.forwardThreadOverrideToOwner(message, sourceAccountID)
		}
		return
	}
	excluded := map[string]struct{}{sourceAccountID: {}}
	target, _, err := m.chooseAccountForRequirementExcluding(ctx, excluded, requirement, &source)
	if err != nil {
		switch {
		case errors.Is(err, errNoModelCapableSubscription),
			errors.Is(err, errModelCapabilityUnavailable),
			errors.Is(err, errNoDataBoundarySubscription),
			errors.Is(err, errUnknownThreadModelCapability):
			m.write(protocol.Failure(message.ID, -32031, err.Error()))
		default:
			m.write(m.allSubscriptionsDepleted(ctx, message.ID))
		}
		return
	}

	if message.Method == "thread/settings/update" {
		m.migrateThreadSettingsUpdate(ctx, message, threadID, sourceAccountID, info, requirement, target.ID, target.Label)
		cleanupInternalSource = false
		return
	}

	result, targetInfo, targetSuppression, err := m.requestCrossAccountThread(ctx, message.Method, message.Params, info, requirement, target.ID)
	if err != nil {
		m.write(protocol.Failure(message.ID, -32027, fmt.Sprintf("route %s to %s: %v", message.Method, target.Label, err)))
		return
	}
	if message.Method == "thread/resume" {
		if targetInfo.ID != threadID {
			_ = m.finishInternalResume(targetSuppression)
			go m.unsubscribeThreadOnAccount(target.ID, threadID)
			m.write(protocol.Failure(message.ID, -32028, fmt.Sprintf(
				"target resume returned thread %q instead of %q", targetInfo.ID, threadID,
			)))
			return
		}
		if err := m.store.CompareAndSwapThreadOwner(threadID, sourceAccountID, target.ID); err != nil {
			_ = m.finishInternalResume(targetSuppression)
			go m.unsubscribeThreadOnAccount(target.ID, threadID)
			m.write(protocol.Failure(message.ID, -32028, err.Error()))
			return
		}
		if err := m.store.UpdateThreadCapability(threadID, stateCapabilityUpdate(targetInfo.Capability)); err != nil {
			_ = m.finishInternalResume(targetSuppression)
			go m.unsubscribeThreadOnAccount(target.ID, threadID)
			if rollbackErr := m.store.CompareAndSwapThreadOwner(threadID, target.ID, sourceAccountID); rollbackErr != nil {
				m.write(protocol.Failure(message.ID, -32028, fmt.Sprintf(
					"persist target chat settings: %v; owner rollback failed: %v", err, rollbackErr,
				)))
			} else {
				m.write(protocol.Failure(message.ID, -32028, fmt.Sprintf("persist target chat settings: %v", err)))
			}
			return
		}
		go m.unsubscribeThreadOnAccount(sourceAccountID, threadID)
	} else {
		if targetInfo.ID == "" || targetInfo.ID == threadID {
			m.write(protocol.Failure(message.ID, -32028, "target fork did not return a new thread identity"))
			return
		}
		if err := m.store.SetThreadOwner(targetInfo.ID, target.ID); err != nil {
			cleanupErr := m.cleanupFailedFork(target.ID, targetInfo.ID)
			m.write(protocol.Failure(message.ID, -32028, compensationError("persist fork owner", err, cleanupErr)))
			return
		}
		if err := m.store.UpdateThreadCapability(targetInfo.ID, stateCapabilityUpdate(targetInfo.Capability)); err != nil {
			cleanupErr := m.cleanupFailedFork(target.ID, targetInfo.ID)
			m.write(protocol.Failure(message.ID, -32028, compensationError("persist fork settings", err, cleanupErr)))
			return
		}
		if err := m.store.CopyControllerAffinity(threadID, targetInfo.ID); err != nil {
			cleanupErr := m.cleanupFailedFork(target.ID, targetInfo.ID)
			m.write(protocol.Failure(message.ID, -32028, compensationError("persist fork Controller affinity", err, cleanupErr)))
			return
		}
		if info.LoadedKnown && !info.WasLoaded {
			go m.unsubscribeThreadOnAccount(sourceAccountID, threadID)
			cleanupInternalSource = false
		}
	}

	m.write(protocol.Success(message.ID, result))
	if message.Method == "thread/resume" {
		targetNotifications := m.finishInternalResume(targetSuppression)
		m.replayCapturedNotifications(target.ID, targetNotifications)
	}
	m.publish(Event{
		Type:      "thread-capability-routed",
		AccountID: target.ID,
		Message:   fmt.Sprintf("%s routed to %s for the requested model capability", message.Method, target.Label),
		Data: map[string]any{
			"threadId": targetInfo.ID, "sourceThreadId": threadID, "previousAccountId": sourceAccountID,
		},
	})
}

func (m *Multiplexer) migrateThreadSettingsUpdate(
	ctx context.Context,
	message protocol.Message,
	threadID, sourceAccountID string,
	info threadResumeInfo,
	requirement modelRequirement,
	targetAccountID, targetLabel string,
) {
	sourceCapability := info.Capability
	info.Capability = requirement
	targetInfo, targetSuppression, err := m.resumeThreadOnAccount(ctx, info, targetAccountID)
	if err != nil {
		m.write(protocol.Failure(message.ID, -32027, fmt.Sprintf("move chat to %s for settings update: %v", targetLabel, err)))
		return
	}
	if targetInfo.ID != threadID {
		_ = m.finishInternalResume(targetSuppression)
		go m.unsubscribeThreadOnAccount(targetAccountID, threadID)
		m.write(protocol.Failure(message.ID, -32028, fmt.Sprintf(
			"target resume returned thread %q instead of %q", targetInfo.ID, threadID,
		)))
		return
	}
	if err := m.store.CompareAndSwapThreadOwner(threadID, sourceAccountID, targetAccountID); err != nil {
		_ = m.finishInternalResume(targetSuppression)
		go m.unsubscribeThreadOnAccount(targetAccountID, threadID)
		m.write(protocol.Failure(message.ID, -32028, err.Error()))
		return
	}
	if err := m.store.UpdateThreadCapability(threadID, stateCapabilityUpdate(targetInfo.Capability)); err != nil {
		_ = m.finishInternalResume(targetSuppression)
		go m.unsubscribeThreadOnAccount(targetAccountID, threadID)
		rollbackErr := m.store.CompareAndSwapThreadOwner(threadID, targetAccountID, sourceAccountID)
		if rollbackErr != nil {
			m.write(protocol.Failure(message.ID, -32028, fmt.Sprintf(
				"persist target chat settings: %v; owner rollback failed: %v", err, rollbackErr,
			)))
		} else {
			m.write(protocol.Failure(message.ID, -32028, fmt.Sprintf("persist target chat settings: %v", err)))
		}
		return
	}
	if err := m.forwardWithCleanup(targetAccountID, message, sourceAccountID, threadID); err != nil {
		_ = m.finishInternalResume(targetSuppression)
		go m.unsubscribeThreadOnAccount(targetAccountID, threadID)
		ownerRollbackErr := m.store.CompareAndSwapThreadOwner(threadID, targetAccountID, sourceAccountID)
		capabilityRollbackErr := m.store.UpdateThreadCapability(threadID, stateCapabilityUpdate(sourceCapability))
		switch {
		case ownerRollbackErr != nil:
			m.write(protocol.Failure(message.ID, -32023, fmt.Sprintf("%v; owner rollback failed: %v", err, ownerRollbackErr)))
		case capabilityRollbackErr != nil:
			m.write(protocol.Failure(message.ID, -32023, fmt.Sprintf("%v; capability rollback failed: %v", err, capabilityRollbackErr)))
		default:
			m.write(protocol.Failure(message.ID, -32023, err.Error()))
		}
		return
	}
	targetNotifications := m.finishInternalResume(targetSuppression)
	m.replayCapturedNotifications(targetAccountID, targetNotifications)
	m.publish(Event{
		Type:      "thread-capability-routed",
		AccountID: targetAccountID,
		Message:   fmt.Sprintf("thread/settings/update routed to %s for the requested model capability", targetLabel),
		Data: map[string]any{
			"threadId": threadID, "previousAccountId": sourceAccountID,
		},
	})
}

func compensationError(operation string, operationErr, cleanupErr error) string {
	if cleanupErr != nil {
		return fmt.Sprintf("%s: %v; target compensation failed: %v", operation, operationErr, cleanupErr)
	}
	return fmt.Sprintf("%s: %v; target fork was deleted", operation, operationErr)
}

func (m *Multiplexer) cleanupFailedFork(accountID, threadID string) error {
	remoteErr := m.deleteThreadOnAccount(accountID, threadID)
	metadataErr := m.store.DeleteThreadMetadata(threadID)
	if remoteErr != nil && metadataErr != nil {
		return fmt.Errorf("delete target thread: %v; delete Router metadata: %v", remoteErr, metadataErr)
	}
	if remoteErr != nil {
		return fmt.Errorf("delete target thread: %w", remoteErr)
	}
	if metadataErr != nil {
		return fmt.Errorf("delete Router metadata: %w", metadataErr)
	}
	return nil
}

func (m *Multiplexer) deleteThreadOnAccount(accountID, threadID string) error {
	child, ok := m.child(accountID)
	if !ok {
		return errors.New("target app-server is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), controlRequestTimeout)
	defer cancel()
	params, _ := json.Marshal(map[string]any{"threadId": threadID})
	if _, err := child.Request(ctx, "thread/delete", params); err != nil {
		go m.unsubscribeThreadOnAccount(accountID, threadID)
		return err
	}
	return nil
}

func (m *Multiplexer) requestCrossAccountThread(
	ctx context.Context,
	method string,
	originalParams json.RawMessage,
	info threadResumeInfo,
	requirement modelRequirement,
	targetAccountID string,
) (json.RawMessage, threadResumeInfo, *internalResumeSuppression, error) {
	target, err := m.ensureChild(ctx, targetAccountID)
	if err != nil {
		return nil, threadResumeInfo{}, nil, fmt.Errorf("target subscription is unavailable: %w", err)
	}
	params, err := crossAccountThreadParams(method, originalParams, info, requirement)
	if err != nil {
		return nil, threadResumeInfo{}, nil, err
	}
	var suppression *internalResumeSuppression
	if method == "thread/resume" {
		suppression = m.registerInternalResume(targetAccountID, info.ID, true)
	}
	response, err := target.Request(ctx, method, params)
	if err != nil {
		if method == "thread/resume" {
			_ = m.finishInternalResume(suppression)
			go m.unsubscribeThreadOnAccount(targetAccountID, info.ID)
		}
		return nil, threadResumeInfo{}, nil, err
	}
	targetInfo, err := threadResumeInfoFromResponse(response.Result)
	if err != nil {
		if method == "thread/resume" {
			_ = m.finishInternalResume(suppression)
			go m.unsubscribeThreadOnAccount(targetAccountID, info.ID)
		} else if forkID := threadIDFromResult(response.Result); forkID != "" && forkID != info.ID {
			_ = m.cleanupFailedFork(targetAccountID, forkID)
		}
		return nil, threadResumeInfo{}, nil, fmt.Errorf("decode target chat settings: %w", err)
	}
	if !targetInfo.Capability.ModelKnown || targetInfo.Capability.Model == "" {
		if method == "thread/resume" {
			_ = m.finishInternalResume(suppression)
			go m.unsubscribeThreadOnAccount(targetAccountID, info.ID)
		} else if targetInfo.ID != "" && targetInfo.ID != info.ID {
			_ = m.cleanupFailedFork(targetAccountID, targetInfo.ID)
		}
		return nil, threadResumeInfo{}, nil, errors.New("target chat response has no effective model")
	}
	return response.Result, targetInfo, suppression, nil
}

func crossAccountThreadParams(
	method string,
	original json.RawMessage,
	info threadResumeInfo,
	requirement modelRequirement,
) (json.RawMessage, error) {
	var params map[string]any
	if err := json.Unmarshal(original, &params); err != nil {
		return nil, fmt.Errorf("decode %s params: %w", method, err)
	}
	params["threadId"] = info.ID
	params["path"] = info.Path
	if method == "thread/resume" {
		params["history"] = nil
	}
	if _, explicitlySet := params["cwd"]; !explicitlySet && info.CWD != "" {
		params["cwd"] = info.CWD
	}
	if _, explicitlySet := params["modelProvider"]; !explicitlySet && info.ModelProvider != "" {
		params["modelProvider"] = info.ModelProvider
	}
	config, _ := params["config"].(map[string]any)
	if config == nil {
		config = make(map[string]any)
	}
	if requirement.ModelKnown {
		value := nullableCapabilityValue(requirement.Model)
		params["model"] = value
		config["model"] = value
	}
	if requirement.EffortKnown {
		config["model_reasoning_effort"] = nullableCapabilityValue(requirement.Effort)
	}
	if requirement.ServiceTierKnown {
		params["serviceTier"] = nullableCapabilityValue(requirement.ServiceTier)
	}
	if len(config) > 0 {
		params["config"] = config
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("encode %s params: %w", method, err)
	}
	return encoded, nil
}
