package mux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/b-nnett/codex-subscription-router/internal/protocol"
	"github.com/b-nnett/codex-subscription-router/internal/state"
)

type temporaryFailureKind uint8

const (
	temporaryFailureNone temporaryFailureKind = iota
	temporaryFailureQuota
	temporaryFailureAuthentication
)

func (m *Multiplexer) temporaryAccountRetiring(accountID string) bool {
	m.temporaryMu.RLock()
	_, retiring := m.temporaryRetiring[accountID]
	m.temporaryMu.RUnlock()
	return retiring
}

func (m *Multiplexer) beginTemporaryAccountRetirement(accountID string) bool {
	m.temporaryMu.Lock()
	defer m.temporaryMu.Unlock()
	if _, exists := m.temporaryRetiring[accountID]; exists {
		return false
	}
	m.temporaryRetiring[accountID] = struct{}{}
	return true
}

func (m *Multiplexer) endTemporaryAccountRetirement(accountID string) {
	m.temporaryMu.Lock()
	delete(m.temporaryRetiring, accountID)
	m.temporaryMu.Unlock()
}

func (m *Multiplexer) maybeRetireTemporaryProbe(
	account state.Account,
	method string,
	response protocol.Message,
) bool {
	if !account.Temporary || account.Controller {
		return false
	}
	kind := temporaryFailureFromRPC(method, response)
	if kind == temporaryFailureNone {
		return false
	}
	// account/read can be called while the add-account login is still pending.
	// Require an actual stored credential before treating its auth error as a
	// terminal failure of a previously connected temporary account.
	if kind == temporaryFailureAuthentication && !temporaryCredentialPresent(account) {
		return false
	}
	if !m.beginTemporaryAccountRetirement(account.ID) {
		return true
	}
	reason := temporaryFailureReason(kind, method)
	go m.finishTemporaryAccountRetirement(account, reason, "")
	return true
}

func temporaryCredentialPresent(account state.Account) bool {
	_, err := os.Stat(filepath.Join(account.CodexHome, "auth.json"))
	return err == nil
}

func (m *Multiplexer) maybeHandleTemporaryExternalFailure(
	accountID string,
	route externalRoute,
	response protocol.Message,
	raw []byte,
) bool {
	account, ok := m.store.Account(accountID)
	if !ok || !account.Temporary || account.Controller {
		return false
	}
	kind := temporaryFailureFromRPC(route.method, response)
	if kind == temporaryFailureNone {
		return false
	}
	if !m.beginTemporaryAccountRetirement(account.ID) {
		m.writeRaw(raw)
		return true
	}
	reason := temporaryFailureReason(kind, route.method)
	m.publish(Event{
		Type: "temporary-account-retiring", AccountID: account.ID,
		Message: reason,
	})
	switch route.method {
	case "turn/start":
		go m.retryTemporaryTurnAndRetire(account, route, raw, reason)
	case "thread/start":
		go m.retryTemporaryThreadStartAndRetire(account, route, raw, reason)
	default:
		m.writeRaw(raw)
		go m.finishTemporaryAccountRetirement(account, reason, threadIDFromParams(route.message.Params))
	}
	return true
}

func (m *Multiplexer) maybeRetireTemporaryNotification(accountID string, message protocol.Message) {
	if message.Method != "error" && message.Method != "turn/completed" {
		return
	}
	account, ok := m.store.Account(accountID)
	if !ok || !account.Temporary || account.Controller {
		return
	}
	kind := temporaryFailureFromPayload(message.Params, true)
	if kind == temporaryFailureNone || !m.beginTemporaryAccountRetirement(account.ID) {
		return
	}
	reason := temporaryFailureReason(kind, message.Method)
	threadID := threadIDFromParams(message.Params)
	m.publish(Event{
		Type: "temporary-account-retiring", AccountID: account.ID,
		Message: reason, Data: map[string]any{"threadId": threadID},
	})
	go m.finishTemporaryAccountRetirement(account, reason, threadID)
}

func (m *Multiplexer) retryTemporaryTurnAndRetire(
	account state.Account,
	route externalRoute,
	originalFailure []byte,
	reason string,
) {
	threadID := threadIDFromParams(route.message.Params)
	ctx, cancel := context.WithTimeout(context.Background(), 4*controlRequestTimeout)
	defer cancel()
	source, err := m.accountIdentitySnapshot(ctx, account.ID, false)
	if err == nil && threadID != "" {
		var targetID string
		targetID, err = m.evacuateTemporaryThread(
			ctx, account.ID, source, threadID, modelRequirementFromParams(route.message.Params),
		)
		if err == nil {
			effective := storedModelRequirement(m.store.ThreadCapability(threadID)).overlay(
				modelRequirementFromParams(route.message.Params),
			)
			route.message.Params = paramsWithModelRequirement(route.message.Params, effective)
			if forwardErr := m.forward(targetID, route.message); forwardErr != nil {
				err = forwardErr
			}
		}
	}
	if err != nil || threadID == "" {
		m.writeRaw(originalFailure)
	}
	m.finishTemporaryAccountRetirement(account, reason, threadID)
}

func (m *Multiplexer) retryTemporaryThreadStartAndRetire(
	account state.Account,
	route externalRoute,
	originalFailure []byte,
	reason string,
) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*controlRequestTimeout)
	excluded := map[string]struct{}{account.ID: {}}
	target, _, err := m.chooseAccountForRequirementExcluding(
		ctx, excluded, modelRequirementFromParams(route.message.Params), nil,
	)
	if err == nil {
		err = m.forward(target.ID, route.message)
	}
	cancel()
	if err != nil {
		m.writeRaw(originalFailure)
	}
	m.finishTemporaryAccountRetirement(account, reason, "")
}

func (m *Multiplexer) finishTemporaryAccountRetirement(
	account state.Account,
	reason, preferredThreadID string,
) {
	retiredFromPool := false
	defer func() {
		if retiredFromPool {
			m.endTemporaryAccountRetirement(account.ID)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 4*controlRequestTimeout)
	defer cancel()

	threadIDs := m.store.ThreadIDsForAccount(account.ID)
	if preferredThreadID != "" {
		for index, threadID := range threadIDs {
			if threadID == preferredThreadID {
				threadIDs[0], threadIDs[index] = threadIDs[index], threadIDs[0]
				break
			}
		}
	}
	migrationErrors := make(map[string]string)
	var source AccountSnapshot
	var sourceErr error
	if len(threadIDs) > 0 {
		source, sourceErr = m.accountIdentitySnapshot(ctx, account.ID, false)
	}
	if sourceErr != nil && len(threadIDs) > 0 {
		migrationErrors["source"] = sourceErr.Error()
	} else {
		for _, threadID := range threadIDs {
			if _, err := m.evacuateTemporaryThread(ctx, account.ID, source, threadID, modelRequirement{}); err != nil {
				migrationErrors[threadID] = err.Error()
			}
		}
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), controlRequestTimeout)
	stopErr := m.stopChild(stopCtx, account.ID)
	stopCancel()
	_, removeErr := m.store.RetireTemporaryAccount(account.ID)
	retiredFromPool = removeErr == nil || !accountStillExists(m.store, account.ID)
	if retiredFromPool {
		m.forgetAccountCaches(account.ID)
		m.publish(Event{
			Type: "account-removed", AccountID: account.ID,
			Message: reason,
			Data: map[string]any{
				"temporary": true, "migrationErrors": migrationErrors,
			},
		})
	}
	if stopErr != nil || removeErr != nil || len(migrationErrors) > 0 {
		details := map[string]any{
			"stopError": errorText(stopErr), "removeError": errorText(removeErr),
			"migrationErrors": migrationErrors,
		}
		m.publish(Event{
			Type: "temporary-account-retirement-warning", AccountID: account.ID,
			Message: "Temporary subscription was retired with cleanup warnings", Data: details,
		})
		fmt.Fprintf(os.Stderr, "codex-mux: retire temporary account %s: %#v\n", account.ID, details)
	}
}

func accountStillExists(store *state.Store, accountID string) bool {
	_, ok := store.Account(accountID)
	return ok
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (m *Multiplexer) evacuateTemporaryThread(
	ctx context.Context,
	sourceAccountID string,
	source AccountSnapshot,
	threadID string,
	requested modelRequirement,
) (string, error) {
	lock := m.threadLock(threadID)
	lock.Lock()
	defer lock.Unlock()
	ownerID, ok := m.store.ThreadOwner(threadID)
	if !ok {
		return "", errors.New("thread has no recorded owner")
	}
	if ownerID != sourceAccountID {
		return ownerID, nil
	}
	if m.store.ControllerAffinedThread(threadID) {
		return "", errors.New("thread references Controller-local state")
	}
	info, err := m.readThreadResumeInfo(ctx, threadID, sourceAccountID, false)
	if err != nil {
		return "", err
	}
	if !historyModeSupportsCrossProcessFailover(info.HistoryMode) {
		return "", errors.New("thread history mode cannot be moved to another app-server")
	}
	requirement := info.Capability.overlay(requested)
	if requirement.Model == "" {
		return "", errUnknownThreadModelCapability
	}
	excluded := map[string]struct{}{sourceAccountID: {}}
	target, _, err := m.chooseAccountForRequirementExcluding(ctx, excluded, requirement, &source)
	if err != nil {
		return "", err
	}
	info.Capability = requirement
	targetInfo, suppression, err := m.resumeThreadOnAccount(ctx, info, target.ID)
	if err != nil {
		return "", err
	}
	if targetInfo.ID != threadID {
		_ = m.finishInternalResume(suppression)
		go m.unsubscribeThreadOnAccount(target.ID, threadID)
		return "", fmt.Errorf("target resume returned thread %q instead of %q", targetInfo.ID, threadID)
	}
	if err := m.store.CompareAndSwapThreadOwner(threadID, sourceAccountID, target.ID); err != nil {
		_ = m.finishInternalResume(suppression)
		go m.unsubscribeThreadOnAccount(target.ID, threadID)
		return "", err
	}
	if err := m.store.UpdateThreadCapability(threadID, stateCapabilityUpdate(targetInfo.Capability)); err != nil {
		_ = m.finishInternalResume(suppression)
		go m.unsubscribeThreadOnAccount(target.ID, threadID)
		rollbackErr := m.store.CompareAndSwapThreadOwner(threadID, target.ID, sourceAccountID)
		if rollbackErr != nil {
			return "", fmt.Errorf("persist target settings: %v; owner rollback failed: %v", err, rollbackErr)
		}
		return "", fmt.Errorf("persist target settings: %w", err)
	}
	notifications := m.finishInternalResume(suppression)
	m.replayCapturedNotifications(target.ID, notifications)
	go m.unsubscribeThreadOnAccount(sourceAccountID, threadID)
	m.publish(Event{
		Type: "temporary-thread-routed", AccountID: target.ID,
		Message: fmt.Sprintf("Chat moved away from temporary subscription to %s", target.Label),
		Data:    map[string]any{"threadId": threadID, "previousAccountId": sourceAccountID},
	})
	return target.ID, nil
}

func temporaryFailureFromRPC(method string, message protocol.Message) temporaryFailureKind {
	if message.Error == nil {
		return temporaryFailureNone
	}
	allowQuota := temporaryQuotaMethod(method)
	if len(message.Error.Data) > 0 {
		if kind := temporaryFailureFromPayload(message.Error.Data, allowQuota); kind != temporaryFailureNone {
			if kind != temporaryFailureAuthentication || temporaryAuthenticationMethod(method) {
				return kind
			}
		}
	}
	kind := temporaryFailureFromText(message.Error.Message, allowQuota)
	if kind == temporaryFailureAuthentication && !temporaryAuthenticationMethod(method) {
		return temporaryFailureNone
	}
	return kind
}

func temporaryAuthenticationMethod(method string) bool {
	return method == "account/read" || method == "account/rateLimits/read" ||
		method == "model/list" || temporaryQuotaMethod(method)
}

func temporaryQuotaMethod(method string) bool {
	switch method {
	case "account/rateLimits/read", "thread/start", "thread/resume", "thread/fork",
		"thread/settings/update", "turn/start", "turn/steer":
		return true
	default:
		return false
	}
}

func temporaryFailureFromPayload(payload json.RawMessage, allowQuota bool) temporaryFailureKind {
	var decoded any
	if len(payload) == 0 || json.Unmarshal(payload, &decoded) != nil {
		return temporaryFailureNone
	}
	return temporaryFailureFromValue(decoded, allowQuota)
}

func temporaryFailureFromValue(value any, allowQuota bool) temporaryFailureKind {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalizedKey := compactErrorText(key)
			if normalizedKey == "codexerrorinfo" {
				if text, ok := child.(string); ok {
					if kind := temporaryFailureFromStructuredText(text, allowQuota); kind != temporaryFailureNone {
						return kind
					}
				}
			}
			if normalizedKey == "httpstatuscode" || normalizedKey == "statuscode" || normalizedKey == "status" {
				if status, ok := child.(float64); ok {
					if allowQuota && int(status) == 429 {
						return temporaryFailureQuota
					}
					if int(status) == 401 {
						return temporaryFailureAuthentication
					}
				}
			}
			if kind := temporaryFailureFromValue(child, allowQuota); kind != temporaryFailureNone {
				return kind
			}
		}
	case []any:
		for _, child := range typed {
			if kind := temporaryFailureFromValue(child, allowQuota); kind != temporaryFailureNone {
				return kind
			}
		}
	case string:
		return temporaryFailureFromText(typed, false)
	}
	return temporaryFailureNone
}

func temporaryFailureFromStructuredText(message string, allowQuota bool) temporaryFailureKind {
	if kind := temporaryFailureFromText(message, false); kind == temporaryFailureAuthentication {
		return kind
	}
	compact := compactErrorText(message)
	if allowQuota && (compact == "usagelimitexceeded" || compact == "usagelimitreached" ||
		compact == "ratelimitexceeded" || compact == "ratelimitreached") {
		return temporaryFailureQuota
	}
	return temporaryFailureNone
}

func temporaryFailureFromText(message string, allowQuota bool) temporaryFailureKind {
	lower := strings.ToLower(strings.TrimSpace(message))
	compact := compactErrorText(lower)
	authenticationFailure := compact == "unauthorized" || compact == "authenticationrequired" ||
		strings.Contains(compact, "tokenexpired") || strings.Contains(compact, "tokenrevoked") ||
		strings.Contains(compact, "invalidrefreshtoken") || strings.Contains(compact, "refreshtokenalreadyused") ||
		strings.Contains(compact, "accesstokencouldnotberefreshed") || strings.Contains(compact, "pleasesigninagain") ||
		strings.Contains(compact, "pleasereauthenticate") || strings.Contains(lower, "re-authenticate") ||
		strings.Contains(message, "令牌过期") || strings.Contains(message, "请重新认证") || strings.Contains(message, "登录已过期") ||
		(strings.Contains(lower, "unauthorized") && containsNumericToken(lower, "401"))
	if authenticationFailure && !temporaryToolScopedErrorText(lower) {
		return temporaryFailureAuthentication
	}
	if allowQuota && (compact == "usagelimitexceeded" || compact == "usagelimitreached" ||
		compact == "ratelimitexceeded" || compact == "ratelimitreached" ||
		(containsNumericToken(lower, "429") && !temporaryToolScopedErrorText(lower))) {
		return temporaryFailureQuota
	}
	return temporaryFailureNone
}

func temporaryToolScopedErrorText(value string) bool {
	for _, marker := range []string{"mcp", "github", "plugin", "tool", "hook"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func compactErrorText(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, value)
}

func containsNumericToken(value, token string) bool {
	for _, field := range strings.FieldsFunc(value, func(r rune) bool { return !unicode.IsDigit(r) }) {
		if field == token {
			return true
		}
	}
	return false
}

func temporaryFailureReason(kind temporaryFailureKind, source string) string {
	if kind == temporaryFailureAuthentication {
		return fmt.Sprintf("Temporary subscription removed after terminal authentication failure from %s", source)
	}
	return fmt.Sprintf("Temporary subscription removed after explicit 429/usage-limit failure from %s", source)
}
