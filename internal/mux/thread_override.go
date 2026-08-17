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
	info, err := m.readThreadResumeInfo(ctx, threadID, sourceAccountID)
	if err != nil {
		m.write(protocol.Failure(message.ID, -32027, fmt.Sprintf("recover chat settings before capability routing: %v", err)))
		return
	}
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

	result, targetInfo, err := m.requestCrossAccountThread(ctx, message.Method, message.Params, info, requirement, target.ID)
	if err != nil {
		m.write(protocol.Failure(message.ID, -32027, fmt.Sprintf("route %s to %s: %v", message.Method, target.Label, err)))
		return
	}
	if message.Method == "thread/resume" {
		if targetInfo.ID != threadID {
			m.write(protocol.Failure(message.ID, -32028, fmt.Sprintf(
				"target resume returned thread %q instead of %q", targetInfo.ID, threadID,
			)))
			return
		}
		if err := m.store.CompareAndSwapThreadOwner(threadID, sourceAccountID, target.ID); err != nil {
			m.write(protocol.Failure(message.ID, -32028, err.Error()))
			return
		}
		if err := m.store.UpdateThreadCapability(threadID, stateCapabilityUpdate(targetInfo.Capability)); err != nil {
			if rollbackErr := m.store.CompareAndSwapThreadOwner(threadID, target.ID, sourceAccountID); rollbackErr != nil {
				m.write(protocol.Failure(message.ID, -32028, fmt.Sprintf(
					"persist target chat settings: %v; owner rollback failed: %v", err, rollbackErr,
				)))
			} else {
				m.write(protocol.Failure(message.ID, -32028, fmt.Sprintf("persist target chat settings: %v", err)))
			}
			return
		}
	} else {
		if targetInfo.ID == "" || targetInfo.ID == threadID {
			m.write(protocol.Failure(message.ID, -32028, "target fork did not return a new thread identity"))
			return
		}
		if err := m.store.SetThreadOwner(targetInfo.ID, target.ID); err != nil {
			m.write(protocol.Failure(message.ID, -32028, fmt.Sprintf("persist fork owner: %v", err)))
			return
		}
		if err := m.store.UpdateThreadCapability(targetInfo.ID, stateCapabilityUpdate(targetInfo.Capability)); err != nil {
			m.write(protocol.Failure(message.ID, -32028, fmt.Sprintf("persist fork settings: %v", err)))
			return
		}
	}

	m.write(protocol.Success(message.ID, result))
	m.publish(Event{
		Type:      "thread-capability-routed",
		AccountID: target.ID,
		Message:   fmt.Sprintf("%s routed to %s for the requested model capability", message.Method, target.Label),
		Data: map[string]any{
			"threadId": targetInfo.ID, "sourceThreadId": threadID, "previousAccountId": sourceAccountID,
		},
	})
}

func (m *Multiplexer) requestCrossAccountThread(
	ctx context.Context,
	method string,
	originalParams json.RawMessage,
	info threadResumeInfo,
	requirement modelRequirement,
	targetAccountID string,
) (json.RawMessage, threadResumeInfo, error) {
	target, err := m.ensureChild(ctx, targetAccountID)
	if err != nil {
		return nil, threadResumeInfo{}, fmt.Errorf("target subscription is unavailable: %w", err)
	}
	params, err := crossAccountThreadParams(method, originalParams, info, requirement)
	if err != nil {
		return nil, threadResumeInfo{}, err
	}
	response, err := target.Request(ctx, method, params)
	if err != nil {
		return nil, threadResumeInfo{}, err
	}
	targetInfo, err := threadResumeInfoFromResponse(response.Result)
	if err != nil {
		return nil, threadResumeInfo{}, fmt.Errorf("decode target chat settings: %w", err)
	}
	if !targetInfo.Capability.ModelKnown || targetInfo.Capability.Model == "" {
		return nil, threadResumeInfo{}, errors.New("target chat response has no effective model")
	}
	return response.Result, targetInfo, nil
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
	if info.CWD != "" {
		params["cwd"] = info.CWD
	}
	if info.ModelProvider != "" {
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
