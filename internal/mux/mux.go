package mux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/b-nnett/codex-subscription-router/internal/backend"
	"github.com/b-nnett/codex-subscription-router/internal/protocol"
	"github.com/b-nnett/codex-subscription-router/internal/state"
)

// controlRequestTimeout applies only to router-owned diagnostic and metadata
// RPCs. Proxied Desktop requests and app-server approval requests live until
// their protocol response arrives or the owning connection exits.
const controlRequestTimeout = 30 * time.Second

type Options struct {
	RealExecutable string
	RealArgs       []string
	Environment    []string
	Store          *state.Store
	Output         io.Writer
}

type externalRoute struct {
	sequence             uint64
	accountID            string
	method               string
	message              protocol.Message
	unsubscribeAccountID string
	unsubscribeThreadID  string
}

type serverRequestRoute struct {
	accountID string
	method    string
	original  json.RawMessage
	forwarded json.RawMessage
	responded bool
}

type childInitializeResult struct {
	accountID  string
	controller bool
	child      *backend.Child
	response   protocol.Message
	err        error
}

type internalResumeSuppression struct {
	key         string
	started     chan struct{}
	startedSeen bool
	capture     bool
	captured    [][]byte
}

type Event struct {
	Type      string `json:"type"`
	AccountID string `json:"accountId,omitempty"`
	Message   string `json:"message,omitempty"`
	Data      any    `json:"data,omitempty"`
}

// Multiplexer presents one app-server connection to ChatGPT.app while owning
// one real app-server process per ChatGPT subscription.
type Multiplexer struct {
	realExecutable string
	realArgs       []string
	environment    []string
	store          *state.Store
	output         io.Writer

	childrenMu sync.RWMutex
	children   map[string]*backend.Child
	startMu    sync.Mutex
	inbound    chan backend.Inbound
	lifecycle  context.Context

	initializationMu sync.RWMutex
	initializeParams json.RawMessage
	initialized      bool
	globalApplyMu    sync.Mutex
	globalStateMu    sync.RWMutex
	globalMutations  map[string]globalMutation
	globalOrder      []string

	externalMu       sync.Mutex
	externalRoutes   map[string]externalRoute
	routeSequence    atomic.Uint64
	serverMu         sync.Mutex
	serverRoutes     map[string]serverRequestRoute
	serverSequence   atomic.Uint64
	internalResumeMu sync.Mutex
	internalResumes  map[string][]*internalResumeSuppression

	outputMu sync.Mutex
	eventsMu sync.RWMutex
	events   map[chan Event]struct{}

	profileMu     sync.Mutex
	profileClient *http.Client
	profileCache  map[string]profileCacheEntry
	now           func() time.Time

	resetCreditsMu       sync.Mutex
	resetCreditsCache    map[string]resetCreditsCacheEntry
	resetCreditsEndpoint string

	previewMu        sync.RWMutex
	rateLimitPreview *RateLimitPreview

	resetPreviewMu sync.RWMutex
	resetPreviews  map[string]ResetCreditsPreview

	temporaryMu       sync.RWMutex
	temporaryRetiring map[string]struct{}

	threadLocks        [64]sync.Mutex
	threadListCursorMu sync.Mutex
	threadListCursors  map[string]threadListCursorState
	threadListSequence atomic.Uint64
}

func New(options Options) (*Multiplexer, error) {
	if options.RealExecutable == "" || options.Store == nil || options.Output == nil {
		return nil, errors.New("real executable, store, and output are required")
	}
	return &Multiplexer{
		realExecutable:       options.RealExecutable,
		realArgs:             append([]string(nil), options.RealArgs...),
		environment:          append([]string(nil), options.Environment...),
		store:                options.Store,
		output:               options.Output,
		children:             make(map[string]*backend.Child),
		inbound:              make(chan backend.Inbound, 1024),
		externalRoutes:       make(map[string]externalRoute),
		serverRoutes:         make(map[string]serverRequestRoute),
		internalResumes:      make(map[string][]*internalResumeSuppression),
		globalMutations:      make(map[string]globalMutation),
		events:               make(map[chan Event]struct{}),
		profileClient:        &http.Client{Timeout: 10 * time.Second},
		profileCache:         make(map[string]profileCacheEntry),
		now:                  time.Now,
		resetCreditsCache:    make(map[string]resetCreditsCacheEntry),
		resetCreditsEndpoint: rateLimitResetCreditsURL,
		resetPreviews:        make(map[string]ResetCreditsPreview),
		temporaryRetiring:    make(map[string]struct{}),
		threadListCursors:    make(map[string]threadListCursorState),
	}, nil
}

func (m *Multiplexer) Start(ctx context.Context) error {
	m.lifecycle = ctx
	for _, account := range m.store.Accounts() {
		if !account.Enabled {
			continue
		}
		if _, err := m.startChild(ctx, account); err != nil {
			fmt.Fprintf(os.Stderr, "codex-mux: start account %s: %v\n", account.ID, err)
		}
	}
	if len(m.childEntries()) == 0 {
		return errors.New("no Codex app-server process could be started")
	}
	go m.inboundLoop(ctx)
	go m.syncManagedConfigLoop(ctx)
	return nil
}

func (m *Multiplexer) syncManagedConfigLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.store.SyncManagedConfig(); err != nil {
				fmt.Fprintf(os.Stderr, "codex-mux: sync shared plugin config: %v\n", err)
			}
		}
	}
}

func (m *Multiplexer) Close() {
	for _, entry := range m.childEntries() {
		_ = entry.child.Close()
	}
}

func (m *Multiplexer) HandleClient(message protocol.Message) {
	if message.Method == "" && len(message.ID) > 0 {
		m.handleServerRequestResponse(message)
		return
	}
	if message.Method == "initialize" && len(message.ID) > 0 {
		go m.initialize(message)
		return
	}
	if len(message.ID) == 0 {
		m.handleClientNotification(message)
		return
	}
	if controllerGlobalStateMethod(message.Method) {
		m.routeControllerRequest(message)
		return
	}

	switch message.Method {
	case "thread/list":
		go m.aggregateThreadList(message)
	case "thread/loaded/list":
		go m.aggregateLoadedThreadList(message)
	case "model/list":
		go m.aggregateModelList(message)
	case "thread/start":
		go m.routeNewThread(message)
	case "account/rateLimits/read":
		go m.routeAggregatedRateLimits(message)
	case "environment/add", "skills/extraRoots/set", "experimentalFeature/enablement/set":
		go m.broadcastGlobalMutation(message)
	default:
		m.routeExistingRequest(message)
	}
}

func (m *Multiplexer) initialize(message protocol.Message) {
	m.initializationMu.Lock()
	m.initializeParams = append(json.RawMessage(nil), message.Params...)
	m.initializationMu.Unlock()

	entries := m.childEntries()
	controllerID := ""
	if controller, ok := m.store.Controller(); ok {
		controllerID = controller.ID
	}
	results := make(chan childInitializeResult, len(entries))
	ctx, cancel := context.WithTimeout(context.Background(), controlRequestTimeout)
	defer cancel()
	for _, entry := range entries {
		go func(entry childEntry) {
			response, err := entry.child.Request(ctx, "initialize", message.Params)
			results <- childInitializeResult{
				accountID: entry.account.ID, controller: entry.account.ID == controllerID,
				child:    entry.child,
				response: response, err: err,
			}
		}(entry)
	}
	completed := make([]childInitializeResult, 0, len(entries))
	for range entries {
		completed = append(completed, <-results)
	}
	controllerResult, failedSecondaries, err := authoritativeInitializeResult(completed)
	if err != nil {
		m.write(protocol.Failure(message.ID, -32000, err.Error()))
		return
	}
	if len(failedSecondaries) > 0 {
		m.publish(Event{
			Type:    "initialize-partial",
			Message: "Some secondary app-servers failed to initialize",
			Data:    map[string]any{"failedAccountIds": failedSecondaries},
		})
		failed := make(map[string]struct{}, len(failedSecondaries))
		for _, accountID := range failedSecondaries {
			failed[accountID] = struct{}{}
		}
		var cleanup sync.WaitGroup
		for _, result := range completed {
			if _, shouldDiscard := failed[result.accountID]; !shouldDiscard || result.child == nil {
				continue
			}
			cleanup.Add(1)
			go func(result childInitializeResult) {
				defer cleanup.Done()
				m.discardUnreadyChild(result.accountID, result.child)
			}(result)
		}
		cleanup.Wait()
	}
	m.write(protocol.Success(message.ID, controllerResult))
}

func authoritativeInitializeResult(results []childInitializeResult) (json.RawMessage, []string, error) {
	failedSecondaries := make([]string, 0)
	var controller *childInitializeResult
	for index := range results {
		result := results[index]
		if result.controller {
			copy := result
			controller = &copy
		} else if result.err != nil {
			failedSecondaries = append(failedSecondaries, result.accountID)
		}
	}
	if controller == nil {
		return nil, failedSecondaries, errors.New("failed to initialize account pool: controller app-server is unavailable")
	}
	if controller.err != nil {
		return nil, failedSecondaries, fmt.Errorf("failed to initialize controller app-server: %v", controller.err)
	}
	return controller.response.Result, failedSecondaries, nil
}

func (m *Multiplexer) handleClientNotification(message protocol.Message) {
	if message.Method == "initialized" {
		m.initializationMu.Lock()
		m.initialized = true
		m.initializationMu.Unlock()
		for _, entry := range m.childEntries() {
			_ = entry.child.Send(message)
		}
		return
	}
	if controller, ok := m.controllerChild(); ok {
		_ = controller.Send(message)
	}
}

func (m *Multiplexer) routeNewThread(message protocol.Message) {
	ctx, cancel := context.WithTimeout(context.Background(), controlRequestTimeout)
	defer cancel()
	if threadStartUsesControllerState(message.Params) {
		m.routeControllerAffinedThread(ctx, message)
		return
	}
	account, reason, err := m.chooseAccountForRequirement(ctx, modelRequirementFromParams(message.Params))
	if err != nil {
		if errors.Is(err, errNoSubscriptionCapacity) {
			m.write(m.allSubscriptionsDepleted(ctx, message.ID))
			return
		}
		m.write(protocol.Failure(message.ID, -32020, err.Error()))
		return
	}
	if err := m.forward(account.ID, message); err != nil {
		m.write(protocol.Failure(message.ID, -32021, err.Error()))
		return
	}
	m.publish(Event{
		Type:      "thread-routed",
		AccountID: account.ID,
		Message:   fmt.Sprintf("New chat pinned to %s", account.Label),
		Data:      reason,
	})
}

func (m *Multiplexer) routeExistingRequest(message protocol.Message) {
	accountID := ""
	if scopedAccountID, cleanedParams, ok := scopedPluginRequest(message.Method, message.Params); ok {
		if account, exists := m.store.Account(scopedAccountID); exists && account.Enabled {
			message.Params = cleanedParams
			if err := m.forward(scopedAccountID, message); err != nil {
				m.write(protocol.Failure(message.ID, -32023, err.Error()))
			}
			return
		}
	}
	threadID := threadIDFromParams(message.Params)
	if threadID != "" {
		accountID, _ = m.store.ThreadOwner(threadID)
	}
	if accountID == "" {
		if controller, ok := m.store.Controller(); ok {
			accountID = controller.ID
		}
	}
	if accountID == "" {
		m.write(protocol.Failure(message.ID, -32022, "no controller account is configured"))
		return
	}
	if message.Method == "thread/section/move" {
		if err := m.validateThreadSectionMove(message.Params, accountID); err != nil {
			m.write(protocol.Failure(message.ID, -32036, err.Error()))
			return
		}
	}
	if message.Method == "turn/start" && threadID != "" {
		go m.routeTurnStart(message, threadID, accountID)
		return
	}
	if capabilityAwareThreadOverrideMethod(message.Method) &&
		threadID != "" && !modelRequirementFromParams(message.Params).empty() {
		go m.routeThreadCapabilityOverride(message, threadID, accountID)
		return
	}
	if err := m.forward(accountID, message); err != nil {
		m.write(protocol.Failure(message.ID, -32023, err.Error()))
	}
}

func capabilityAwareThreadOverrideMethod(method string) bool {
	return method == "thread/resume" || method == "thread/fork" || method == "thread/settings/update"
}

func (m *Multiplexer) forward(accountID string, message protocol.Message) error {
	return m.forwardWithCleanup(accountID, message, "", "")
}

func (m *Multiplexer) forwardWithCleanup(
	accountID string,
	message protocol.Message,
	unsubscribeAccountID, unsubscribeThreadID string,
) error {
	child, err := m.ensureChild(context.Background(), accountID)
	if err != nil {
		return err
	}
	key := protocol.RequestIDKey(message.ID)
	sequence := m.routeSequence.Add(1)
	m.externalMu.Lock()
	m.externalRoutes[key] = externalRoute{
		sequence: sequence, accountID: accountID, method: message.Method, message: message,
		unsubscribeAccountID: unsubscribeAccountID, unsubscribeThreadID: unsubscribeThreadID,
	}
	m.externalMu.Unlock()
	if err := child.Send(message); err != nil {
		m.externalMu.Lock()
		if m.externalRoutes[key].sequence == sequence {
			delete(m.externalRoutes, key)
		}
		m.externalMu.Unlock()
		return err
	}
	return nil
}

func (m *Multiplexer) routeAggregatedRateLimits(message protocol.Message) {
	ctx, cancel := context.WithTimeout(context.Background(), controlRequestTimeout)
	defer cancel()
	rateLimits, err := m.AggregatedRateLimits(ctx)
	if err != nil {
		m.write(protocol.Failure(message.ID, -32024, err.Error()))
		return
	}
	result, err := json.Marshal(map[string]any{"rateLimits": rateLimits})
	if err != nil {
		m.write(protocol.Failure(message.ID, -32025, err.Error()))
		return
	}
	m.write(protocol.Success(message.ID, result))
}

func (m *Multiplexer) routeTurnStart(message protocol.Message, threadID, ownerID string) {
	lock := m.threadLock(threadID)
	lock.Lock()
	defer lock.Unlock()
	if currentOwner, ok := m.store.ThreadOwner(threadID); ok {
		ownerID = currentOwner
	} else if err := m.store.SetThreadOwner(threadID, ownerID); err != nil {
		m.write(protocol.Failure(message.ID, -32028, err.Error()))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*controlRequestTimeout)
	defer cancel()
	snapshot, err := m.accountSnapshotWithProfile(ctx, ownerID, false)
	requested := modelRequirementFromParams(message.Params)
	effective := storedModelRequirement(m.store.ThreadCapability(threadID)).overlay(requested)
	if err == nil && accountEligibleForRouting(snapshot) &&
		accountQuotaState(snapshot) != quotaCapacityExhausted &&
		!m.temporaryAccountRetiring(ownerID) {
		if effective.Model == "" {
			if err := m.forward(ownerID, message); err != nil {
				m.write(protocol.Failure(message.ID, -32023, err.Error()))
			}
			return
		}
		ownerSupportsRequest, capabilityErr := m.accountSupportsRequirement(ctx, snapshot, effective)
		if capabilityErr != nil || ownerSupportsRequest {
			// A transient catalog failure is UNKNOWN, not UNSUPPORTED. Keep
			// the healthy current owner and let its app-server decide.
			if err := m.forward(ownerID, message); err != nil {
				m.write(protocol.Failure(message.ID, -32023, err.Error()))
			}
			return
		}
	}
	if m.store.ControllerAffinedThread(threadID) {
		m.write(protocol.Failure(message.ID, -32035,
			"this chat references Controller-local state and cannot fail over to a different app-server process"))
		return
	}
	excluded := map[string]struct{}{ownerID: {}}
	var source *AccountSnapshot
	if err == nil {
		source = &snapshot
	}
	m.failoverTurn(ctx, message, threadID, ownerID, excluded, source)
}

func (m *Multiplexer) failoverTurn(
	ctx context.Context,
	message protocol.Message,
	threadID string,
	sourceAccountID string,
	excluded map[string]struct{},
	source *AccountSnapshot,
) {
	if source == nil {
		m.write(protocol.Failure(message.ID, -32031, "cannot establish the source account data boundary for safe failover"))
		return
	}
	resume, err := m.readThreadResumeInfo(ctx, threadID, sourceAccountID, false)
	if err != nil {
		m.write(protocol.Failure(message.ID, -32027, fmt.Sprintf("recover chat settings before failover: %v", err)))
		return
	}
	if !historyModeSupportsCrossProcessFailover(resume.HistoryMode) {
		m.write(protocol.Failure(
			message.ID,
			-32027,
			"paginated chat cannot be failed over safely: the app-server protocol exposes no verified cross-process write-owner release operation",
		))
		return
	}
	sourceCapability := resume.Capability
	requested := modelRequirementFromParams(message.Params)
	requirement := resume.Capability.overlay(requested)
	if requirement.Model == "" {
		m.write(protocol.Failure(message.ID, -32031, errUnknownThreadModelCapability.Error()))
		return
	}
	resume.Capability = requirement
	fallback, _, err := m.chooseAccountForRequirementExcluding(ctx, excluded, requirement, source)
	if err != nil {
		if errors.Is(err, errNoModelCapableSubscription) ||
			errors.Is(err, errModelCapabilityUnavailable) ||
			errors.Is(err, errNoDataBoundarySubscription) ||
			errors.Is(err, errUnknownThreadModelCapability) {
			m.write(protocol.Failure(message.ID, -32031, err.Error()))
		} else {
			m.write(m.allSubscriptionsDepleted(ctx, message.ID))
		}
		return
	}
	targetInfo, targetSuppression, err := m.resumeThreadOnAccount(ctx, resume, fallback.ID)
	if err != nil {
		m.write(protocol.Failure(message.ID, -32027, fmt.Sprintf("move chat to %s: %v", fallback.Label, err)))
		return
	}
	targetCapability := targetInfo.Capability
	forwardCapability := requirement.overlay(targetCapability)
	if requirement.EffortKnown {
		// Preserve the source/current-turn effort on the pending turn even when
		// the target resume response normalizes its next-turn setting.
		forwardCapability.Effort = requirement.Effort
		forwardCapability.EffortKnown = true
	}
	if err := m.store.CompareAndSwapThreadOwner(threadID, sourceAccountID, fallback.ID); err != nil {
		_ = m.finishInternalResume(targetSuppression)
		go m.unsubscribeThreadOnAccount(fallback.ID, threadID)
		m.write(protocol.Failure(message.ID, -32028, err.Error()))
		return
	}
	if err := m.store.UpdateThreadCapability(threadID, stateCapabilityUpdate(targetCapability)); err != nil {
		_ = m.finishInternalResume(targetSuppression)
		go m.unsubscribeThreadOnAccount(fallback.ID, threadID)
		if rollbackErr := m.store.CompareAndSwapThreadOwner(threadID, fallback.ID, sourceAccountID); rollbackErr != nil {
			m.write(protocol.Failure(message.ID, -32028, fmt.Sprintf("persist target chat settings: %v; owner rollback failed: %v", err, rollbackErr)))
		} else {
			m.write(protocol.Failure(message.ID, -32028, fmt.Sprintf("persist target chat settings: %v", err)))
		}
		return
	}
	message.Params = paramsWithModelRequirement(message.Params, forwardCapability)
	if err := m.forward(fallback.ID, message); err != nil {
		_ = m.finishInternalResume(targetSuppression)
		go m.unsubscribeThreadOnAccount(fallback.ID, threadID)
		if rollbackErr := m.store.CompareAndSwapThreadOwner(threadID, fallback.ID, sourceAccountID); rollbackErr != nil {
			m.write(protocol.Failure(message.ID, -32023, fmt.Sprintf("%v; owner rollback failed: %v", err, rollbackErr)))
		} else {
			if capabilityRollbackErr := m.store.UpdateThreadCapability(threadID, stateCapabilityUpdate(sourceCapability)); capabilityRollbackErr != nil {
				m.write(protocol.Failure(message.ID, -32023, fmt.Sprintf("%v; capability rollback failed: %v", err, capabilityRollbackErr)))
			} else {
				m.write(protocol.Failure(message.ID, -32023, err.Error()))
			}
		}
		return
	}
	targetNotifications := m.finishInternalResume(targetSuppression)
	m.replayCapturedNotifications(fallback.ID, targetNotifications)
	go m.unsubscribeThreadOnAccount(sourceAccountID, threadID)
	m.publish(Event{
		Type:      "thread-failed-over",
		AccountID: fallback.ID,
		Message:   fmt.Sprintf("Chat continued with %s", fallback.Label),
		Data:      map[string]any{"threadId": threadID, "previousAccountId": sourceAccountID},
	})
}

func historyModeSupportsCrossProcessFailover(historyMode string) bool {
	return historyMode == "" || historyMode == "legacy"
}

type threadResumeInfo struct {
	ID            string
	Path          string
	CWD           string
	ModelProvider string
	HistoryMode   string
	Capability    modelRequirement
	WasLoaded     bool
	LoadedKnown   bool
}

func threadResumeInfoFromResponse(result json.RawMessage) (threadResumeInfo, error) {
	var decoded map[string]any
	if err := json.Unmarshal(result, &decoded); err != nil {
		return threadResumeInfo{}, err
	}
	thread, ok := decoded["thread"].(map[string]any)
	if !ok {
		return threadResumeInfo{}, errors.New("response has no thread object")
	}
	cwd := anyString(decoded["cwd"])
	if cwd == "" {
		cwd = anyString(thread["cwd"])
	}
	modelProvider := anyString(decoded["modelProvider"])
	if modelProvider == "" {
		modelProvider = anyString(thread["modelProvider"])
	}
	return threadResumeInfo{
		ID:            anyString(thread["id"]),
		Path:          anyString(thread["path"]),
		CWD:           cwd,
		ModelProvider: modelProvider,
		HistoryMode:   anyString(thread["historyMode"]),
		Capability:    modelRequirementFromEffectiveSettings(decoded, "reasoningEffort"),
	}, nil
}

func modelRequirementFromEffectiveSettings(settings map[string]any, effortKey string) modelRequirement {
	model, modelKnown := capabilityField(settings, "model")
	effort, effortKnown := capabilityField(settings, effortKey)
	serviceTier, serviceTierKnown := capabilityField(settings, "serviceTier")
	// Effective settings objects are complete snapshots. Optional fields may
	// be omitted when their effective value is the default/None state.
	if !effortKnown {
		effortKnown = true
	}
	if !serviceTierKnown {
		serviceTierKnown = true
	}
	return modelRequirement{
		Model: model, ModelKnown: modelKnown,
		Effort: effort, EffortKnown: effortKnown,
		ServiceTier: serviceTier, ServiceTierKnown: serviceTierKnown,
	}
}

func storedModelRequirement(capability state.ThreadCapability) modelRequirement {
	return modelRequirement{
		Model: capability.Model, ModelKnown: capability.ModelKnown,
		Effort: capability.Effort, EffortKnown: capability.EffortKnown,
		ServiceTier: capability.ServiceTier, ServiceTierKnown: capability.ServiceTierKnown,
	}
}

func stateCapabilityUpdate(requirement modelRequirement) state.ThreadCapabilityUpdate {
	update := state.ThreadCapabilityUpdate{}
	if requirement.ModelKnown {
		value := requirement.Model
		update.Model = &value
	}
	if requirement.EffortKnown {
		value := requirement.Effort
		update.Effort = &value
	}
	if requirement.ServiceTierKnown {
		value := requirement.ServiceTier
		update.ServiceTier = &value
	}
	return update
}

func (m *Multiplexer) readThreadResumeInfo(
	ctx context.Context,
	threadID, sourceAccountID string,
	trackPriorLoadedState bool,
) (threadResumeInfo, error) {
	source, err := m.ensureChild(ctx, sourceAccountID)
	if err != nil {
		return threadResumeInfo{}, fmt.Errorf("source subscription is unavailable: %w", err)
	}
	wasLoaded, loadedKnown := false, false
	if trackPriorLoadedState {
		wasLoaded, loadedKnown = m.threadLoadedOnAccount(ctx, sourceAccountID, source, threadID)
	}
	resumeParams, _ := json.Marshal(map[string]any{"threadId": threadID})
	suppression := m.registerInternalResume(sourceAccountID, threadID, false)
	resumeResponse, err := source.Request(ctx, "thread/resume", resumeParams)
	_ = m.finishInternalResume(suppression)
	if err != nil {
		return threadResumeInfo{}, fmt.Errorf("resume existing chat on source subscription: %w", err)
	}
	info, err := threadResumeInfoFromResponse(resumeResponse.Result)
	if err != nil {
		return threadResumeInfo{}, fmt.Errorf("decode source chat settings: %w", err)
	}
	info.WasLoaded = wasLoaded
	info.LoadedKnown = loadedKnown
	if info.ID == "" {
		return threadResumeInfo{}, errors.New("existing chat has no thread ID")
	}
	if !info.Capability.ModelKnown || info.Capability.Model == "" {
		return threadResumeInfo{}, errors.New("source chat response has no effective model")
	}
	if info.HistoryMode != "paginated" && info.Path == "" {
		return threadResumeInfo{}, errors.New("existing chat has no resumable history path")
	}
	if err := m.store.UpdateThreadCapability(threadID, stateCapabilityUpdate(info.Capability)); err != nil {
		return threadResumeInfo{}, fmt.Errorf("persist recovered chat settings: %w", err)
	}
	return info, nil
}

func (m *Multiplexer) threadLoadedOnAccount(
	ctx context.Context,
	accountID string,
	child *backend.Child,
	threadID string,
) (bool, bool) {
	account, _ := m.store.Account(accountID)
	threadIDs, err := listAllLoadedThreads(ctx, childEntry{account: account, child: child}, json.RawMessage(`{}`))
	if err != nil {
		return false, false
	}
	for _, candidate := range threadIDs {
		if candidate == threadID {
			return true, true
		}
	}
	return false, true
}

func (m *Multiplexer) unsubscribeThreadOnAccount(accountID, threadID string) {
	if err := m.unsubscribeThreadOnAccountResult(accountID, threadID); err != nil {
		fmt.Fprintf(os.Stderr, "codex-mux: unsubscribe compensated thread %s on %s: %v\n", threadID, accountID, err)
		m.publish(Event{
			Type: "thread-cleanup-error", AccountID: accountID,
			Message: "A compensated target thread could not be unsubscribed",
			Data:    map[string]any{"threadId": threadID, "error": err.Error()},
		})
	}
}

func (m *Multiplexer) unsubscribeThreadOnAccountResult(accountID, threadID string) error {
	child, ok := m.child(accountID)
	if !ok {
		return errors.New("app-server is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), controlRequestTimeout)
	defer cancel()
	params, _ := json.Marshal(map[string]any{"threadId": threadID})
	_, err := child.Request(ctx, "thread/unsubscribe", params)
	return err
}

func (m *Multiplexer) resumeThreadOnAccount(
	ctx context.Context,
	info threadResumeInfo,
	targetAccountID string,
) (threadResumeInfo, *internalResumeSuppression, error) {
	target, err := m.ensureChild(ctx, targetAccountID)
	if err != nil {
		return threadResumeInfo{}, nil, fmt.Errorf("target subscription is unavailable: %w", err)
	}
	params := map[string]any{"threadId": info.ID, "history": nil, "path": info.Path}
	for key, value := range map[string]string{
		"cwd": info.CWD, "model": info.Capability.Model, "modelProvider": info.ModelProvider,
	} {
		if value != "" {
			params[key] = value
		}
	}
	if info.Capability.ServiceTierKnown {
		params["serviceTier"] = nullableCapabilityValue(info.Capability.ServiceTier)
	}
	if info.Capability.EffortKnown {
		params["config"] = map[string]any{
			"model_reasoning_effort": nullableCapabilityValue(info.Capability.Effort),
		}
	}
	resumeParams, _ := json.Marshal(params)
	suppression := m.registerInternalResume(targetAccountID, info.ID, true)
	response, err := target.Request(ctx, "thread/resume", resumeParams)
	if err != nil {
		_ = m.finishInternalResume(suppression)
		go m.unsubscribeThreadOnAccount(targetAccountID, info.ID)
		return threadResumeInfo{}, nil, fmt.Errorf("resume existing chat: %w", err)
	}
	targetInfo, err := threadResumeInfoFromResponse(response.Result)
	if err != nil {
		_ = m.finishInternalResume(suppression)
		go m.unsubscribeThreadOnAccount(targetAccountID, info.ID)
		return threadResumeInfo{}, nil, fmt.Errorf("decode target chat settings: %w", err)
	}
	if !targetInfo.Capability.ModelKnown || targetInfo.Capability.Model == "" {
		_ = m.finishInternalResume(suppression)
		go m.unsubscribeThreadOnAccount(targetAccountID, info.ID)
		return threadResumeInfo{}, nil, errors.New("target chat response has no effective model")
	}
	return targetInfo, suppression, nil
}

func (m *Multiplexer) handleServerRequestResponse(message protocol.Message) {
	key := protocol.RequestIDKey(message.ID)
	m.serverMu.Lock()
	route, ok := m.serverRoutes[key]
	if ok && !route.responded {
		route.responded = true
		m.serverRoutes[key] = route
	} else if ok {
		ok = false
	}
	m.serverMu.Unlock()
	if !ok {
		return
	}
	message.ID = route.original
	if child, exists := m.child(route.accountID); exists {
		if child.Send(message) == nil {
			if serverRequestCompletesOnResponse(route.method) {
				m.serverMu.Lock()
				delete(m.serverRoutes, key)
				m.serverMu.Unlock()
			}
			return
		}
	}
	m.serverMu.Lock()
	delete(m.serverRoutes, key)
	m.serverMu.Unlock()
}

func serverRequestCompletesOnResponse(method string) bool {
	return method == "attestation/generate"
}

func (m *Multiplexer) inboundLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case inbound := <-m.inbound:
			m.handleInbound(inbound)
		}
	}
}

func (m *Multiplexer) handleInbound(inbound backend.Inbound) {
	message := inbound.Message
	if message.Method == "" && len(message.ID) > 0 {
		key := protocol.RequestIDKey(message.ID)
		m.externalMu.Lock()
		route, ok := m.externalRoutes[key]
		if ok {
			delete(m.externalRoutes, key)
		}
		m.externalMu.Unlock()
		if ok {
			if route.unsubscribeAccountID != "" && route.unsubscribeThreadID != "" {
				go m.unsubscribeThreadOnAccount(route.unsubscribeAccountID, route.unsubscribeThreadID)
			}
			if m.maybeHandleTemporaryExternalFailure(
				inbound.AccountID, route, message, inbound.Raw,
			) {
				return
			}
			if route.method == "turn/start" && isUsageLimitResponse(message) {
				go m.publishAccountRefresh(inbound.AccountID)
				m.publish(Event{
					Type:      "turn-depleted",
					AccountID: inbound.AccountID,
					Message:   "Subscription depleted after the turn was submitted; the turn was not replayed",
				})
			}
			if message.Error == nil {
				if err := m.learnThreadOwner(route, inbound.AccountID, message.Result); err != nil {
					threadID := threadIDFromParams(route.message.Params)
					if threadID == "" {
						threadID = threadIDFromResult(message.Result)
					}
					m.reportRoutingMetadataError(inbound.AccountID, threadID, route.method, err)
					messageText := fmt.Sprintf(
						"app-server operation succeeded, but routing metadata could not be persisted: %v", err,
					)
					if route.method == "thread/fork" && threadID != "" {
						messageText = compensationError("persist fork routing metadata", err, m.cleanupFailedFork(inbound.AccountID, threadID))
					}
					m.write(protocol.Failure(route.message.ID, -32028, messageText))
					return
				}
			}
			m.writeRaw(inbound.Raw)
		}
		return
	}
	if message.Method != "" && len(message.ID) > 0 {
		m.forwardServerRequest(inbound)
		return
	}
	notificationThreadID := threadIDFromParams(message.Params)
	if notificationThreadID == "" {
		notificationThreadID = threadIDFromNotification(message.Params)
	}
	if notificationThreadID != "" && m.suppressInternalResumeNotification(
		inbound.AccountID, notificationThreadID, message.Method, inbound.Raw,
	) {
		return
	}
	m.maybeRetireTemporaryNotification(inbound.AccountID, message)
	if message.Method == "account/rateLimits/updated" {
		go m.forwardAggregatedRateLimitNotification(inbound.Raw)
		return
	}
	if message.Method == "thread/started" {
		if threadID := threadIDFromNotification(message.Params); threadID != "" {
			if err := m.store.SetThreadOwnerIfAbsent(threadID, inbound.AccountID); err != nil {
				m.reportRoutingMetadataError(inbound.AccountID, threadID, message.Method, err)
			}
		}
	}
	if message.Method == "thread/settings/updated" {
		if err := m.learnEffectiveThreadSettings(inbound.AccountID, message.Params); err != nil {
			m.reportRoutingMetadataError(inbound.AccountID, threadIDFromParams(message.Params), message.Method, err)
		}
	}
	if message.Method == "model/rerouted" {
		if err := m.learnModelReroute(inbound.AccountID, message.Params); err != nil {
			m.reportRoutingMetadataError(inbound.AccountID, threadIDFromParams(message.Params), message.Method, err)
		}
	}
	if message.Method == "serverRequest/resolved" {
		if rewritten, ok := m.rewriteServerRequestResolved(inbound.AccountID, message); ok {
			m.writeRaw(rewritten)
			return
		}
	}
	if message.Method == "turn/completed" ||
		message.Method == "account/login/completed" ||
		message.Method == "account/updated" {
		go m.publishAccountRefresh(inbound.AccountID)
	}
	if m.shouldForwardNotification(inbound.AccountID, message) {
		m.writeRaw(inbound.Raw)
	}
}

func (m *Multiplexer) forwardAggregatedRateLimitNotification(fallback []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), controlRequestTimeout)
	defer cancel()
	rateLimits, err := m.AggregatedRateLimits(ctx)
	if err != nil {
		m.writeRaw(fallback)
		return
	}
	params, err := json.Marshal(map[string]any{"rateLimits": rateLimits})
	if err != nil {
		m.writeRaw(fallback)
		return
	}
	m.write(protocol.Message{Method: "account/rateLimits/updated", Params: params})
}

func (m *Multiplexer) forwardServerRequest(inbound backend.Inbound) {
	sequence := m.serverSequence.Add(1)
	newID := protocol.StringID(fmt.Sprintf("codex-mux:%s:%d", inbound.AccountID, sequence))
	key := protocol.RequestIDKey(newID)
	m.serverMu.Lock()
	m.serverRoutes[key] = serverRequestRoute{
		accountID: inbound.AccountID,
		method:    inbound.Message.Method,
		original:  append(json.RawMessage(nil), inbound.Message.ID...),
		forwarded: append(json.RawMessage(nil), newID...),
	}
	m.serverMu.Unlock()
	inbound.Message.ID = newID
	m.write(inbound.Message)
}

func (m *Multiplexer) rewriteServerRequestResolved(accountID string, message protocol.Message) ([]byte, bool) {
	var params map[string]json.RawMessage
	if json.Unmarshal(message.Params, &params) != nil || params == nil {
		return nil, false
	}
	originalID := params["requestId"]
	if len(originalID) == 0 {
		return nil, false
	}
	originalKey := protocol.RequestIDKey(originalID)
	var forwarded json.RawMessage
	m.serverMu.Lock()
	for key, route := range m.serverRoutes {
		if route.accountID == accountID && protocol.RequestIDKey(route.original) == originalKey {
			forwarded = append(json.RawMessage(nil), route.forwarded...)
			delete(m.serverRoutes, key)
			break
		}
	}
	m.serverMu.Unlock()
	if len(forwarded) == 0 {
		return nil, false
	}
	params["requestId"] = forwarded
	encodedParams, err := json.Marshal(params)
	if err != nil {
		return nil, false
	}
	message.Params = encodedParams
	encoded, err := protocol.Encode(message)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

func (m *Multiplexer) shouldForwardNotification(accountID string, message protocol.Message) bool {
	controller, ok := m.store.Controller()
	if ok && controller.ID == accountID {
		return true
	}
	method := message.Method
	if threadID := threadIDFromParams(message.Params); threadID != "" {
		return m.notificationOwnsThread(threadID, accountID)
	}
	return strings.HasPrefix(method, "thread/") ||
		strings.HasPrefix(method, "turn/") ||
		strings.HasPrefix(method, "item/") ||
		strings.HasPrefix(method, "model/") ||
		strings.HasPrefix(method, "hook/") ||
		strings.HasPrefix(method, "rawResponse")
}

func (m *Multiplexer) learnThreadOwner(route externalRoute, accountID string, result json.RawMessage) error {
	switch route.method {
	case "thread/start", "thread/fork", "thread/resume":
		if threadID := threadIDFromResult(result); threadID != "" {
			if err := m.store.SetThreadOwner(threadID, accountID); err != nil {
				return err
			}
			if info, err := threadResumeInfoFromResponse(result); err == nil {
				if err := m.store.UpdateThreadCapability(threadID, stateCapabilityUpdate(info.Capability)); err != nil {
					return err
				}
			}
			if route.method == "thread/start" {
				usesProject, usesSection := threadStartControllerAffinity(route.message.Params)
				if usesProject {
					if err := m.store.SetControllerAffinedThread(threadID); err != nil {
						return err
					}
				}
				if usesSection {
					if err := m.store.SetThreadSectionAffinity(threadID, true); err != nil {
						return err
					}
				}
			}
			if route.method == "thread/fork" {
				if err := m.store.CopyControllerAffinity(threadIDFromParams(route.message.Params), threadID); err != nil {
					return err
				}
			}
		}
	case "thread/unarchive":
		if threadID := threadIDFromResult(result); threadID != "" {
			if err := m.store.SetThreadOwner(threadID, accountID); err != nil {
				return err
			}
		}
	case "turn/start", "thread/settings/update":
		if threadID := threadIDFromParams(route.message.Params); threadID != "" {
			if err := m.store.SetThreadOwner(threadID, accountID); err != nil {
				return err
			}
		}
	case "thread/section/move":
		if threadID := threadIDFromParams(route.message.Params); threadID != "" {
			if err := m.store.SetThreadOwner(threadID, accountID); err != nil {
				return err
			}
			var params map[string]any
			if json.Unmarshal(route.message.Params, &params) == nil {
				for _, key := range []string{"sectionId", "section_id"} {
					if sectionID, present := params[key]; present {
						if err := m.store.SetThreadSectionAffinity(threadID, sectionID != nil && anyString(sectionID) != ""); err != nil {
							return err
						}
						break
					}
				}
			}
		}
	}
	return nil
}

func (m *Multiplexer) learnEffectiveThreadSettings(accountID string, params json.RawMessage) error {
	var decoded map[string]any
	if json.Unmarshal(params, &decoded) != nil {
		return nil
	}
	threadID := anyString(decoded["threadId"])
	settings, ok := decoded["threadSettings"].(map[string]any)
	if threadID == "" || !ok || !m.notificationOwnsThread(threadID, accountID) {
		return nil
	}
	requirement := modelRequirementFromEffectiveSettings(settings, "effort")
	return m.store.UpdateThreadCapability(threadID, stateCapabilityUpdate(requirement))
}

func (m *Multiplexer) learnModelReroute(accountID string, params json.RawMessage) error {
	var decoded map[string]any
	if json.Unmarshal(params, &decoded) != nil {
		return nil
	}
	threadID := anyString(decoded["threadId"])
	toModel := anyString(decoded["toModel"])
	if threadID == "" || toModel == "" || !m.notificationOwnsThread(threadID, accountID) {
		return nil
	}
	return m.store.UpdateThreadCapability(threadID, state.ThreadCapabilityUpdate{Model: &toModel})
}

func (m *Multiplexer) reportRoutingMetadataError(accountID, threadID, operation string, err error) {
	fmt.Fprintf(os.Stderr, "codex-mux: persist routing metadata for %s on %s: %v\n", operation, accountID, err)
	m.publish(Event{
		Type: "routing-metadata-error", AccountID: accountID,
		Message: "An app-server operation succeeded, but Router metadata persistence failed",
		Data:    map[string]any{"operation": operation, "error": err.Error()},
	})
	params := map[string]any{
		"message": fmt.Sprintf("Router could not persist routing metadata after %s: %v", operation, err),
	}
	if threadID != "" {
		params["threadId"] = threadID
	}
	if encoded, marshalErr := json.Marshal(params); marshalErr == nil {
		m.write(protocol.Message{Method: "warning", Params: encoded})
	}
}

func (m *Multiplexer) notificationOwnsThread(threadID, accountID string) bool {
	owner, exists := m.store.ThreadOwner(threadID)
	if !exists {
		if err := m.store.SetThreadOwnerIfAbsent(threadID, accountID); err != nil {
			m.reportRoutingMetadataError(accountID, threadID, "notification owner learning", err)
			return false
		}
		owner, exists = m.store.ThreadOwner(threadID)
	}
	return exists && owner == accountID
}

func internalResumeKey(accountID, threadID string) string {
	return accountID + "\x00" + threadID
}

func (m *Multiplexer) registerInternalResume(accountID, threadID string, capture bool) *internalResumeSuppression {
	suppression := &internalResumeSuppression{
		key:     internalResumeKey(accountID, threadID),
		started: make(chan struct{}),
		capture: capture,
	}
	m.internalResumeMu.Lock()
	m.internalResumes[suppression.key] = append(m.internalResumes[suppression.key], suppression)
	m.internalResumeMu.Unlock()
	return suppression
}

func (m *Multiplexer) suppressInternalResumeNotification(accountID, threadID, method string, raw []byte) bool {
	switch method {
	case "thread/started", "thread/tokenUsage/updated", "thread/status/changed", "thread/settings/updated",
		"model/rerouted", "warning", "error":
	default:
		return false
	}
	key := internalResumeKey(accountID, threadID)
	m.internalResumeMu.Lock()
	queue := m.internalResumes[key]
	if len(queue) == 0 {
		m.internalResumeMu.Unlock()
		return false
	}
	suppression := queue[0]
	if suppression.capture {
		suppression.captured = append(suppression.captured, append([]byte(nil), raw...))
	}
	if method == "thread/started" && !suppression.startedSeen {
		suppression.startedSeen = true
		close(suppression.started)
	}
	m.internalResumeMu.Unlock()
	return true
}

func (m *Multiplexer) finishInternalResume(suppression *internalResumeSuppression) [][]byte {
	if suppression == nil {
		return nil
	}
	select {
	case <-suppression.started:
		// The response and its bootstrap notifications are adjacent on the
		// child stream. Keep a short grace period for token/status updates that
		// can follow thread/started.
		time.Sleep(50 * time.Millisecond)
	case <-time.After(500 * time.Millisecond):
	}
	m.internalResumeMu.Lock()
	queue := m.internalResumes[suppression.key]
	for index, candidate := range queue {
		if candidate != suppression {
			continue
		}
		queue = append(queue[:index], queue[index+1:]...)
		if len(queue) == 0 {
			delete(m.internalResumes, suppression.key)
		} else {
			m.internalResumes[suppression.key] = queue
		}
		break
	}
	captured := append([][]byte(nil), suppression.captured...)
	m.internalResumeMu.Unlock()
	return captured
}

func (m *Multiplexer) replayCapturedNotifications(accountID string, captured [][]byte) {
	for _, raw := range captured {
		message, err := protocol.Parse(raw)
		if err != nil {
			continue
		}
		m.handleInbound(backend.Inbound{AccountID: accountID, Message: message, Raw: raw})
	}
}

func (m *Multiplexer) write(message protocol.Message) {
	encoded, err := protocol.Encode(message)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codex-mux: encode response: %v\n", err)
		return
	}
	m.writeRaw(encoded)
}

func (m *Multiplexer) writeRaw(encoded []byte) {
	m.outputMu.Lock()
	defer m.outputMu.Unlock()
	_, _ = m.output.Write(append(encoded, '\n'))
}

type childEntry struct {
	account state.Account
	child   *backend.Child
}

func (m *Multiplexer) childEntries() []childEntry {
	accounts := m.store.Accounts()
	m.childrenMu.RLock()
	defer m.childrenMu.RUnlock()
	entries := make([]childEntry, 0, len(accounts))
	for _, account := range accounts {
		if !account.Enabled {
			continue
		}
		if child := m.children[account.ID]; child != nil && !child.IsClosed() {
			entries = append(entries, childEntry{account: account, child: child})
		}
	}
	return entries
}

func (m *Multiplexer) child(accountID string) (*backend.Child, bool) {
	m.childrenMu.RLock()
	child, ok := m.children[accountID]
	m.childrenMu.RUnlock()
	if !ok || child == nil || child.IsClosed() {
		return nil, false
	}
	return child, true
}

func (m *Multiplexer) controllerChild() (*backend.Child, bool) {
	controller, ok := m.store.Controller()
	if !ok {
		return nil, false
	}
	return m.child(controller.ID)
}

func (m *Multiplexer) startChild(ctx context.Context, account state.Account) (*backend.Child, error) {
	if !account.Enabled {
		return nil, fmt.Errorf("account %s is disabled", account.ID)
	}
	if m.temporaryAccountRetiring(account.ID) {
		return nil, fmt.Errorf("temporary account %s is being retired", account.ID)
	}
	m.startMu.Lock()
	defer m.startMu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if child, ok := m.child(account.ID); ok {
		return child, nil
	}
	child, err := backend.Start(
		account.ID,
		account.CodexHome,
		m.realExecutable,
		m.realArgs,
		m.environment,
		m.inbound,
	)
	if err != nil {
		return nil, err
	}

	m.initializationMu.RLock()
	params := append(json.RawMessage(nil), m.initializeParams...)
	initialized := m.initialized
	m.initializationMu.RUnlock()
	if len(params) > 0 {
		requestCtx, cancel := context.WithTimeout(ctx, controlRequestTimeout)
		_, err := child.Request(requestCtx, "initialize", params)
		cancel()
		if err != nil {
			m.discardUnreadyChild(account.ID, child)
			return nil, err
		}
		if initialized {
			_ = child.Send(protocol.Message{Method: "initialized"})
			m.globalApplyMu.Lock()
			replayErr := m.replayGlobalMutations(ctx, child)
			m.globalApplyMu.Unlock()
			if replayErr != nil {
				m.discardUnreadyChild(account.ID, child)
				return nil, fmt.Errorf("restore process-wide state: %w", replayErr)
			}
		}
	}
	if child.IsClosed() {
		m.discardUnreadyChild(account.ID, child)
		return nil, errors.New("Codex app-server exited before becoming ready")
	}
	m.childrenMu.Lock()
	m.children[account.ID] = child
	m.childrenMu.Unlock()
	go m.watchChild(account.ID, child)
	return child, nil
}

func (m *Multiplexer) discardUnreadyChild(accountID string, child *backend.Child) {
	m.discardChild(accountID, child, "Codex app-server failed to initialize")
}

func (m *Multiplexer) discardChild(accountID string, child *backend.Child, reason string) {
	removed := false
	m.childrenMu.Lock()
	if m.children[accountID] == child {
		delete(m.children, accountID)
		removed = true
	}
	m.childrenMu.Unlock()
	if removed {
		m.failExternalRoutes(accountID, reason)
		m.dropServerRoutes(accountID)
	}
	_ = child.Close()
	select {
	case <-child.Done():
	case <-time.After(2 * time.Second):
		_ = child.Kill()
		select {
		case <-child.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

func (m *Multiplexer) ensureChild(ctx context.Context, accountID string) (*backend.Child, error) {
	if child, ok := m.child(accountID); ok {
		return child, nil
	}
	if m.temporaryAccountRetiring(accountID) {
		return nil, fmt.Errorf("temporary account %s is being retired", accountID)
	}
	account, ok := m.store.Account(accountID)
	if !ok {
		return nil, fmt.Errorf("account %s is unavailable", accountID)
	}
	if !account.Enabled {
		return nil, fmt.Errorf("account %s is disabled", accountID)
	}
	return m.startChild(ctx, account)
}

func (m *Multiplexer) stopChild(ctx context.Context, accountID string) error {
	m.childrenMu.Lock()
	child := m.children[accountID]
	delete(m.children, accountID)
	m.childrenMu.Unlock()
	if child == nil {
		return nil
	}
	m.failExternalRoutes(accountID, "subscription was stopped")
	m.dropServerRoutes(accountID)
	_ = child.Close()
	grace := time.NewTimer(2 * time.Second)
	defer grace.Stop()
	select {
	case <-child.Done():
		return nil
	case <-grace.C:
		if err := child.Kill(); err != nil {
			return fmt.Errorf("force-stop account %s: %w", accountID, err)
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-child.Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Multiplexer) watchChild(accountID string, child *backend.Child) {
	<-child.Done()
	removed := false
	m.childrenMu.Lock()
	if m.children[accountID] == child {
		delete(m.children, accountID)
		removed = true
	}
	m.childrenMu.Unlock()
	if !removed {
		return
	}
	m.failExternalRoutes(accountID, "Codex app-server exited before responding")
	m.dropServerRoutes(accountID)

	if m.lifecycle == nil {
		return
	}
	select {
	case <-m.lifecycle.Done():
		return
	case <-time.After(time.Second):
	}
	account, ok := m.store.Account(accountID)
	if !ok || !account.Enabled {
		return
	}
	if _, err := m.startChild(m.lifecycle, account); err != nil {
		fmt.Fprintf(os.Stderr, "codex-mux: restart account %s: %v\n", accountID, err)
	}
}

func (m *Multiplexer) failExternalRoutes(accountID, reason string) {
	type failedRoute struct {
		id                   json.RawMessage
		method               string
		unsubscribeAccountID string
		unsubscribeThreadID  string
	}
	failed := make([]failedRoute, 0)
	m.externalMu.Lock()
	for key, route := range m.externalRoutes {
		if route.accountID == accountID {
			failed = append(failed, failedRoute{
				id: route.message.ID, method: route.method,
				unsubscribeAccountID: route.unsubscribeAccountID,
				unsubscribeThreadID:  route.unsubscribeThreadID,
			})
			delete(m.externalRoutes, key)
		}
	}
	m.externalMu.Unlock()
	for _, route := range failed {
		if route.unsubscribeAccountID != "" && route.unsubscribeThreadID != "" {
			go m.unsubscribeThreadOnAccount(route.unsubscribeAccountID, route.unsubscribeThreadID)
		}
		m.write(protocol.Failure(route.id, -32032, fmt.Sprintf("%s: %s", route.method, reason)))
	}
}

func (m *Multiplexer) dropServerRoutes(accountID string) {
	m.serverMu.Lock()
	defer m.serverMu.Unlock()
	for key, route := range m.serverRoutes {
		if route.accountID == accountID {
			delete(m.serverRoutes, key)
		}
	}
}

func (m *Multiplexer) SubscribeEvents() (<-chan Event, func()) {
	channel := make(chan Event, 32)
	m.eventsMu.Lock()
	m.events[channel] = struct{}{}
	m.eventsMu.Unlock()
	return channel, func() {
		m.eventsMu.Lock()
		if _, ok := m.events[channel]; ok {
			delete(m.events, channel)
			close(channel)
		}
		m.eventsMu.Unlock()
	}
}

func (m *Multiplexer) publish(event Event) {
	m.eventsMu.RLock()
	defer m.eventsMu.RUnlock()
	for channel := range m.events {
		select {
		case channel <- event:
		default:
		}
	}
}

func (m *Multiplexer) publishAccountRefresh(accountID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snapshot, err := m.accountSnapshot(ctx, accountID)
	if err == nil {
		m.publish(Event{Type: "account-updated", AccountID: accountID, Data: snapshot})
	}
}

func threadIDFromParams(params json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	var decoded map[string]any
	if json.Unmarshal(params, &decoded) != nil {
		return ""
	}
	for _, key := range []string{"threadId", "thread_id"} {
		if value, ok := decoded[key].(string); ok {
			return value
		}
	}
	return ""
}

func mutableObjectParams(params json.RawMessage) map[string]any {
	var decoded map[string]any
	if len(params) == 0 || json.Unmarshal(params, &decoded) != nil || decoded == nil {
		return make(map[string]any)
	}
	return decoded
}

func threadIDFromResult(result json.RawMessage) string {
	var decoded struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if json.Unmarshal(result, &decoded) != nil {
		return ""
	}
	return decoded.Thread.ID
}

func threadIDFromNotification(params json.RawMessage) string {
	return threadIDFromResult(params)
}

func isUsageLimitResponse(message protocol.Message) bool {
	if message.Error == nil {
		return false
	}
	var structured any
	if len(message.Error.Data) > 0 && json.Unmarshal(message.Error.Data, &structured) == nil {
		if containsUsageLimitCode(structured) {
			return true
		}
	}
	normalized := strings.ToLower(strings.TrimSpace(message.Error.Message))
	return normalized == "usage_limit_exceeded" || normalized == "usage limit exceeded"
}

func containsUsageLimitCode(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalizedKey := strings.ToLower(key)
			if normalizedKey == "codexerrorinfo" {
				if text, ok := child.(string); ok && isUsageLimitCode(text) {
					return true
				}
			}
			if containsUsageLimitCode(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsUsageLimitCode(child) {
				return true
			}
		}
	}
	return false
}

func isUsageLimitCode(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	return normalized == "usage_limit_exceeded" || normalized == "usage_limit_reached"
}

func (m *Multiplexer) threadLock(threadID string) *sync.Mutex {
	var hash uint64 = 1469598103934665603
	for index := 0; index < len(threadID); index++ {
		hash ^= uint64(threadID[index])
		hash *= 1099511628211
	}
	return &m.threadLocks[hash%uint64(len(m.threadLocks))]
}

func (m *Multiplexer) allSubscriptionsDepleted(ctx context.Context, id json.RawMessage) protocol.Message {
	var resetsAt *int64
	if preview := m.currentRateLimitPreview(); preview != nil && preview.Mode.isAllDepleted() {
		resetsAt = preview.ResetsAt
	} else if limits, err := m.AggregatedRateLimits(ctx); err == nil {
		weekly, _ := longestAndShortestWindow(limits)
		if weekly != nil {
			resetsAt = weekly.ResetsAt
		}
	}
	return allSubscriptionsDepleted(id, resetsAt)
}

func allSubscriptionsDepleted(id json.RawMessage, resetsAt *int64) protocol.Message {
	message := "All connected subscriptions are depleted. Add another subscription or wait for usage to reset."
	if resetsAt != nil {
		reset := time.Unix(*resetsAt, 0).In(time.Local)
		message = fmt.Sprintf(
			"All connected subscriptions are depleted. Usage resets on %s.",
			reset.Format("Monday, 2 January at 3:04 PM"),
		)
	}
	return protocol.Failure(
		id,
		-32026,
		message,
	)
}

func sortThreads(threads []map[string]any, params json.RawMessage) {
	sortKey := "created_at"
	sortDirection := "desc"
	var decoded map[string]any
	if json.Unmarshal(params, &decoded) == nil {
		if value := anyString(decoded["sortKey"]); value != "" {
			sortKey = value
		}
		if value := anyString(decoded["sortDirection"]); value != "" {
			sortDirection = value
		}
	}
	field := ""
	switch sortKey {
	case "created_at":
		field = "createdAt"
	case "updated_at":
		field = "updatedAt"
	case "recency_at":
		field = "recencyAt"
	case "section_position":
		// Position is intentionally not exposed on Thread. Each child already
		// returned the authoritative order, so a timestamp re-sort would corrupt
		// it. Concrete section queries are Controller-only.
		return
	default:
		return
	}
	sort.SliceStable(threads, func(i, j int) bool {
		left, leftKnown := numericField(threads[i], field)
		right, rightKnown := numericField(threads[j], field)
		if leftKnown != rightKnown {
			if sortDirection == "asc" {
				return !leftKnown
			}
			return leftKnown
		}
		if !leftKnown || left == right {
			return false
		}
		if sortDirection == "asc" {
			return left < right
		}
		return left > right
	})
}

func numericField(value map[string]any, key string) (float64, bool) {
	if number, ok := value[key].(float64); ok {
		return number, true
	}
	return 0, false
}
