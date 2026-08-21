package state

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const stateVersion = 1

type Account struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	CodexHome  string `json:"codexHome"`
	Enabled    bool   `json:"enabled"`
	Controller bool   `json:"controller"`
	Temporary  bool   `json:"temporary,omitempty"`
	CreatedAt  int64  `json:"createdAt"`
}

type persistedState struct {
	Version                 int               `json:"version"`
	Accounts                []Account         `json:"accounts"`
	ThreadOwner             map[string]string `json:"threadOwner"`
	ThreadModel             map[string]string `json:"threadModel,omitempty"`
	ThreadEffort            map[string]string `json:"threadEffort,omitempty"`
	ThreadServiceTier       map[string]string `json:"threadServiceTier,omitempty"`
	ControllerThreads       map[string]bool   `json:"controllerThreads,omitempty"`
	ControllerThreadReasons map[string]uint8  `json:"controllerThreadReasons,omitempty"`
}

const (
	controllerAffinityLegacy uint8 = 1 << iota
	controllerAffinityState
	controllerAffinitySection
)

type ThreadCapability struct {
	Model            string
	ModelKnown       bool
	Effort           string
	EffortKnown      bool
	ServiceTier      string
	ServiceTierKnown bool
}

// ThreadCapabilityUpdate uses nil for an omitted field and a pointer to the
// empty string for an explicit clear/default value.
type ThreadCapabilityUpdate struct {
	Model       *string
	Effort      *string
	ServiceTier *string
}

// Store persists only routing metadata. OAuth credentials and conversation
// databases remain inside each account's isolated Codex home.
type Store struct {
	mu                sync.RWMutex
	root              string
	path              string
	primaryCodexHome  string
	accounts          []Account
	owners            map[string]string
	models            map[string]string
	efforts           map[string]string
	serviceTiers      map[string]string
	controllerReasons map[string]uint8
	configMu          sync.Mutex
	managedConfigSum  [sha256.Size]byte
	hasManagedSum     bool
}

func Open(root, primaryCodexHome string) (*Store, error) {
	if root == "" {
		return nil, errors.New("state root is required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create state root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("secure state root: %w", err)
	}

	store := &Store{
		root:              root,
		path:              filepath.Join(root, "state.json"),
		primaryCodexHome:  primaryCodexHome,
		owners:            make(map[string]string),
		models:            make(map[string]string),
		efforts:           make(map[string]string),
		serviceTiers:      make(map[string]string),
		controllerReasons: make(map[string]uint8),
	}
	data, err := os.ReadFile(store.path)
	switch {
	case err == nil:
		var persisted persistedState
		if err := json.Unmarshal(data, &persisted); err != nil {
			return nil, fmt.Errorf("read state: %w", err)
		}
		if persisted.Version != stateVersion {
			return nil, fmt.Errorf("unsupported state version %d", persisted.Version)
		}
		store.accounts = persisted.Accounts
		if persisted.ThreadOwner != nil {
			store.owners = persisted.ThreadOwner
		}
		if persisted.ThreadModel != nil {
			store.models = persisted.ThreadModel
		}
		if persisted.ThreadEffort != nil {
			store.efforts = persisted.ThreadEffort
		}
		if persisted.ThreadServiceTier != nil {
			store.serviceTiers = persisted.ThreadServiceTier
		}
		if persisted.ControllerThreadReasons != nil {
			store.controllerReasons = persisted.ControllerThreadReasons
		} else {
			for threadID, affined := range persisted.ControllerThreads {
				if affined {
					store.controllerReasons[threadID] = controllerAffinityLegacy
				}
			}
		}
	case errors.Is(err, os.ErrNotExist):
		store.accounts = []Account{{
			ID:         "primary",
			Label:      "Primary",
			CodexHome:  primaryCodexHome,
			Enabled:    true,
			Controller: true,
			CreatedAt:  time.Now().Unix(),
		}}
		if err := store.saveLocked(); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("read state: %w", err)
	}
	if err := store.SyncManagedConfig(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Root() string {
	return s.root
}

// SyncManagedConfig propagates desktop-managed configuration (including
// plugins, marketplaces, skills, and MCP server definitions) to every
// isolated subscription. Credential stores and project trust remain local to
// each account; syncIsolatedConfig deliberately excludes both.
func (s *Store) SyncManagedConfig() error {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	primaryConfig, err := readConfig(filepath.Join(s.primaryCodexHome, "config.toml"))
	if err != nil {
		return fmt.Errorf("read primary config: %w", err)
	}
	configSum := sha256.Sum256(primaryConfig)
	if s.hasManagedSum && configSum == s.managedConfigSum {
		return nil
	}

	s.mu.RLock()
	accounts := slices.Clone(s.accounts)
	primaryCodexHome := s.primaryCodexHome
	s.mu.RUnlock()

	for _, account := range accounts {
		if samePath(account.CodexHome, primaryCodexHome) {
			continue
		}
		if err := syncIsolatedConfig(primaryCodexHome, account.CodexHome); err != nil {
			return fmt.Errorf("sync account %q config: %w", account.ID, err)
		}
	}
	s.managedConfigSum = configSum
	s.hasManagedSum = true
	return nil
}

func (s *Store) Accounts() []Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.accounts)
}

func (s *Store) Account(id string) (Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, account := range s.accounts {
		if account.ID == id {
			return account, true
		}
	}
	return Account{}, false
}

func (s *Store) Controller() (Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, account := range s.accounts {
		if account.Controller {
			return account, true
		}
	}
	return Account{}, false
}

func (s *Store) AddAccount(label string) (Account, error) {
	return s.addAccount(label, false)
}

func (s *Store) AddTemporaryAccount(label string) (Account, error) {
	return s.addAccount(label, true)
}

func (s *Store) addAccount(label string, temporary bool) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	label = strings.TrimSpace(label)
	if label == "" {
		label = fmt.Sprintf("Subscription %d", len(s.accounts)+1)
	}
	id, err := randomID()
	if err != nil {
		return Account{}, err
	}
	codexHome := filepath.Join(s.root, "accounts", id, "codex-home")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		return Account{}, fmt.Errorf("create account home: %w", err)
	}
	if err := os.Chmod(codexHome, 0o700); err != nil {
		_ = os.RemoveAll(filepath.Join(s.root, "accounts", id))
		return Account{}, fmt.Errorf("secure account home: %w", err)
	}
	if err := syncIsolatedConfig(s.primaryCodexHome, codexHome); err != nil {
		_ = os.RemoveAll(filepath.Join(s.root, "accounts", id))
		return Account{}, fmt.Errorf("write account config: %w", err)
	}

	account := Account{
		ID:        id,
		Label:     label,
		CodexHome: codexHome,
		Enabled:   true,
		Temporary: temporary,
		CreatedAt: time.Now().Unix(),
	}
	s.accounts = append(s.accounts, account)
	if err := s.saveLocked(); err != nil {
		s.accounts = s.accounts[:len(s.accounts)-1]
		_ = os.RemoveAll(filepath.Join(s.root, "accounts", id))
		return Account{}, err
	}
	return account, nil
}

func (s *Store) RemoveAccount(id string) (Account, error) {
	return s.removeAccount(id, false)
}

// RetireTemporaryAccount removes a disposable subscription and its credential
// while retaining the credential-free Codex home. Cross-account resume uses
// rollout paths from that home, so deleting it here would break chats that were
// moved away immediately before retirement.
func (s *Store) RetireTemporaryAccount(id string) (Account, error) {
	return s.removeAccount(id, true)
}

func (s *Store) removeAccount(id string, preserveHistory bool) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := -1
	var removed Account
	for candidate, account := range s.accounts {
		if account.ID == id {
			index = candidate
			removed = account
			break
		}
	}
	if index < 0 {
		return Account{}, fmt.Errorf("account %q not found", id)
	}
	if removed.Controller || samePath(removed.CodexHome, s.primaryCodexHome) {
		return Account{}, errors.New("the primary subscription cannot be removed")
	}
	if preserveHistory && !removed.Temporary {
		return Account{}, errors.New("only a temporary subscription can be retired")
	}
	accountRoot := filepath.Join(s.root, "accounts", id)
	if !samePath(filepath.Dir(removed.CodexHome), accountRoot) {
		return Account{}, errors.New("account home is outside the managed account root")
	}

	previousAccounts := s.accounts
	previousOwners := s.owners
	previousModels := s.models
	previousEfforts := s.efforts
	previousServiceTiers := s.serviceTiers
	previousControllerReasons := s.controllerReasons
	s.accounts = append(slices.Clone(s.accounts[:index]), s.accounts[index+1:]...)
	s.owners = make(map[string]string, len(previousOwners))
	s.models = make(map[string]string, len(previousModels))
	s.efforts = make(map[string]string, len(previousEfforts))
	s.serviceTiers = make(map[string]string, len(previousServiceTiers))
	s.controllerReasons = make(map[string]uint8, len(previousControllerReasons))
	for threadID, accountID := range previousOwners {
		if accountID != id {
			s.owners[threadID] = accountID
		}
	}
	for threadID, model := range previousModels {
		if _, exists := s.owners[threadID]; exists {
			s.models[threadID] = model
		}
	}
	for threadID, effort := range previousEfforts {
		if _, exists := s.owners[threadID]; exists {
			s.efforts[threadID] = effort
		}
	}
	for threadID, serviceTier := range previousServiceTiers {
		if _, exists := s.owners[threadID]; exists {
			s.serviceTiers[threadID] = serviceTier
		}
	}
	for threadID, reasons := range previousControllerReasons {
		if _, exists := s.owners[threadID]; exists && reasons != 0 {
			s.controllerReasons[threadID] = reasons
		}
	}
	if err := s.saveLocked(); err != nil {
		s.accounts = previousAccounts
		s.owners = previousOwners
		s.models = previousModels
		s.efforts = previousEfforts
		s.serviceTiers = previousServiceTiers
		s.controllerReasons = previousControllerReasons
		return Account{}, err
	}
	if preserveHistory {
		for _, name := range []string{"auth.json", "auth.json.tmp"} {
			if err := os.Remove(filepath.Join(removed.CodexHome, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return removed, fmt.Errorf("remove temporary account credential: %w", err)
			}
		}
		return removed, nil
	}
	if err := os.RemoveAll(accountRoot); err != nil {
		return removed, fmt.Errorf("remove account home: %w", err)
	}
	return removed, nil
}

func (s *Store) UpdateAccount(id string, label *string, enabled *bool) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.accounts {
		if s.accounts[index].ID != id {
			continue
		}
		if enabled != nil && !*enabled && s.accounts[index].Controller {
			return Account{}, errors.New("the Controller subscription cannot be disabled")
		}
		previous := s.accounts[index]
		if label != nil {
			trimmed := strings.TrimSpace(*label)
			if trimmed == "" {
				return Account{}, errors.New("account label cannot be empty")
			}
			s.accounts[index].Label = trimmed
		}
		if enabled != nil {
			s.accounts[index].Enabled = *enabled
		}
		if err := s.saveLocked(); err != nil {
			s.accounts[index] = previous
			return Account{}, err
		}
		return s.accounts[index], nil
	}
	return Account{}, fmt.Errorf("account %q not found", id)
}

func (s *Store) ThreadOwner(threadID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	owner, ok := s.owners[threadID]
	return owner, ok
}

func (s *Store) SetThreadOwner(threadID, accountID string) error {
	if threadID == "" || accountID == "" {
		return errors.New("thread and account IDs are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owners[threadID] == accountID {
		return nil
	}
	previous, existed := s.owners[threadID]
	s.owners[threadID] = accountID
	if err := s.saveLocked(); err != nil {
		if existed {
			s.owners[threadID] = previous
		} else {
			delete(s.owners, threadID)
		}
		return err
	}
	return nil
}

// SetThreadOwnerIfAbsent learns ownership from asynchronous notifications
// without allowing a late notification to steal an existing assignment.
func (s *Store) SetThreadOwnerIfAbsent(threadID, accountID string) error {
	if threadID == "" || accountID == "" {
		return errors.New("thread and account IDs are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owners[threadID] != "" {
		return nil
	}
	s.owners[threadID] = accountID
	if err := s.saveLocked(); err != nil {
		delete(s.owners, threadID)
		return err
	}
	return nil
}

func (s *Store) CompareAndSwapThreadOwner(threadID, oldAccountID, newAccountID string) error {
	if threadID == "" || oldAccountID == "" || newAccountID == "" {
		return errors.New("thread and account IDs are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.owners[threadID]
	if current == newAccountID {
		return nil
	}
	if current != oldAccountID {
		return fmt.Errorf("thread %q owner changed from %q to %q", threadID, oldAccountID, current)
	}
	s.owners[threadID] = newAccountID
	if err := s.saveLocked(); err != nil {
		s.owners[threadID] = oldAccountID
		return err
	}
	return nil
}

func (s *Store) ThreadModel(threadID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	model, ok := s.models[threadID]
	return model, ok
}

func (s *Store) SetThreadModel(threadID, model string) error {
	if model == "" {
		return errors.New("thread ID and model are required")
	}
	return s.UpdateThreadCapability(threadID, ThreadCapabilityUpdate{Model: &model})
}

func (s *Store) ThreadCapability(threadID string) ThreadCapability {
	s.mu.RLock()
	defer s.mu.RUnlock()
	model, modelKnown := s.models[threadID]
	effort, effortKnown := s.efforts[threadID]
	serviceTier, serviceTierKnown := s.serviceTiers[threadID]
	return ThreadCapability{
		Model: model, ModelKnown: modelKnown,
		Effort: effort, EffortKnown: effortKnown,
		ServiceTier: serviceTier, ServiceTierKnown: serviceTierKnown,
	}
}

// ControllerAffinedThread records that a thread references a server-generated
// identity which only exists in the Controller's state database.
func (s *Store) ControllerAffinedThread(threadID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.controllerReasons[threadID] != 0
}

func (s *Store) SetControllerAffinedThread(threadID string) error {
	return s.setControllerAffinityReason(threadID, controllerAffinityState, true)
}

func (s *Store) SetThreadSectionAffinity(threadID string, affined bool) error {
	return s.setControllerAffinityReason(threadID, controllerAffinitySection, affined)
}

func (s *Store) setControllerAffinityReason(threadID string, reason uint8, enabled bool) error {
	if threadID == "" {
		return errors.New("thread ID is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.controllerReasons[threadID]
	next := previous
	if reason == controllerAffinitySection && previous == controllerAffinityLegacy {
		// The previous format used one boolean, and the current protocol exposes
		// section membership as the only recoverable historical affinity. The
		// first authoritative section operation specializes that legacy value.
		if enabled {
			next = controllerAffinitySection
		} else {
			next = 0
		}
	} else if enabled {
		next |= reason
	} else {
		next &^= reason
	}
	if next == previous {
		return nil
	}
	if next == 0 {
		delete(s.controllerReasons, threadID)
	} else {
		s.controllerReasons[threadID] = next
	}
	if err := s.saveLocked(); err != nil {
		if previous == 0 {
			delete(s.controllerReasons, threadID)
		} else {
			s.controllerReasons[threadID] = previous
		}
		return err
	}
	return nil
}

func (s *Store) CopyControllerAffinity(sourceThreadID, targetThreadID string) error {
	if sourceThreadID == "" || targetThreadID == "" {
		return errors.New("source and target thread IDs are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	reasons := s.controllerReasons[sourceThreadID]
	previous := s.controllerReasons[targetThreadID]
	if previous == reasons {
		return nil
	}
	if reasons == 0 {
		delete(s.controllerReasons, targetThreadID)
	} else {
		s.controllerReasons[targetThreadID] = reasons
	}
	if err := s.saveLocked(); err != nil {
		if previous == 0 {
			delete(s.controllerReasons, targetThreadID)
		} else {
			s.controllerReasons[targetThreadID] = previous
		}
		return err
	}
	return nil
}

func (s *Store) DeleteThreadMetadata(threadID string) error {
	if threadID == "" {
		return errors.New("thread ID is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, hasOwner := s.owners[threadID]
	_, hasModel := s.models[threadID]
	_, hasEffort := s.efforts[threadID]
	_, hasServiceTier := s.serviceTiers[threadID]
	_, hasAffinity := s.controllerReasons[threadID]
	if !hasOwner && !hasModel && !hasEffort && !hasServiceTier && !hasAffinity {
		return nil
	}
	previousOwners := mapsClone(s.owners)
	previousModels := mapsClone(s.models)
	previousEfforts := mapsClone(s.efforts)
	previousServiceTiers := mapsClone(s.serviceTiers)
	previousControllerReasons := uint8MapsClone(s.controllerReasons)
	delete(s.owners, threadID)
	delete(s.models, threadID)
	delete(s.efforts, threadID)
	delete(s.serviceTiers, threadID)
	delete(s.controllerReasons, threadID)
	if err := s.saveLocked(); err != nil {
		s.owners = previousOwners
		s.models = previousModels
		s.efforts = previousEfforts
		s.serviceTiers = previousServiceTiers
		s.controllerReasons = previousControllerReasons
		return err
	}
	return nil
}

// UpdateThreadCapability applies a three-state capability update. A nil field
// leaves the existing value unchanged; a non-nil empty value records an
// explicit clear/default.
func (s *Store) UpdateThreadCapability(threadID string, capability ThreadCapabilityUpdate) error {
	if threadID == "" {
		return errors.New("thread ID is required")
	}
	if capability.Model == nil && capability.Effort == nil && capability.ServiceTier == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previousModels := mapsClone(s.models)
	previousEfforts := mapsClone(s.efforts)
	previousServiceTiers := mapsClone(s.serviceTiers)
	changed := setCapabilityValue(s.models, threadID, capability.Model)
	changed = setCapabilityValue(s.efforts, threadID, capability.Effort) || changed
	changed = setCapabilityValue(s.serviceTiers, threadID, capability.ServiceTier) || changed
	if !changed {
		return nil
	}
	if err := s.saveLocked(); err != nil {
		s.models = previousModels
		s.efforts = previousEfforts
		s.serviceTiers = previousServiceTiers
		return err
	}
	return nil
}

// MergeThreadMetadata learns from a possibly filtered or stale thread/list
// result. It deliberately never removes unseen threads or overwrites an owner
// that may have moved concurrently.
func (s *Store) MergeThreadMetadata(owners, models map[string]string, sectionThreadIDs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previousOwners := mapsClone(s.owners)
	previousModels := mapsClone(s.models)
	previousControllerReasons := uint8MapsClone(s.controllerReasons)
	changed := false
	for threadID, accountID := range owners {
		if threadID != "" && accountID != "" && s.owners[threadID] == "" {
			s.owners[threadID] = accountID
			changed = true
		}
	}
	for threadID, model := range models {
		if _, exists := s.owners[threadID]; exists && model != "" && s.models[threadID] == "" {
			s.models[threadID] = model
			changed = true
		}
	}
	for _, threadID := range sectionThreadIDs {
		if threadID == "" {
			continue
		}
		previous := s.controllerReasons[threadID]
		next := previous | controllerAffinitySection
		if previous == controllerAffinityLegacy {
			// Version 1 stored a single boolean. A positive section observation
			// lets us safely specialize the common legacy section-only record.
			next = controllerAffinitySection
		}
		if next != previous {
			s.controllerReasons[threadID] = next
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := s.saveLocked(); err != nil {
		s.owners = previousOwners
		s.models = previousModels
		s.controllerReasons = previousControllerReasons
		return err
	}
	return nil
}

func uint8MapsClone(source map[string]uint8) map[string]uint8 {
	clone := make(map[string]uint8, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func mapsClone(source map[string]string) map[string]string {
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func setCapabilityValue(values map[string]string, key string, value *string) bool {
	if value == nil {
		return false
	}
	if current, exists := values[key]; exists && current == *value {
		return false
	}
	values[key] = *value
	return true
}

func (s *Store) ThreadCounts() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	counts := make(map[string]int)
	for _, accountID := range s.owners {
		counts[accountID]++
	}
	return counts
}

func (s *Store) ThreadIDsForAccount(accountID string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	threadIDs := make([]string, 0)
	for threadID, ownerID := range s.owners {
		if ownerID == accountID {
			threadIDs = append(threadIDs, threadID)
		}
	}
	slices.Sort(threadIDs)
	return threadIDs
}

func (s *Store) saveLocked() error {
	controllerThreads := make(map[string]bool, len(s.controllerReasons))
	for threadID, reasons := range s.controllerReasons {
		if reasons != 0 {
			controllerThreads[threadID] = true
		}
	}
	persisted := persistedState{
		Version:                 stateVersion,
		Accounts:                s.accounts,
		ThreadOwner:             s.owners,
		ThreadModel:             s.models,
		ThreadEffort:            s.efforts,
		ThreadServiceTier:       s.serviceTiers,
		ControllerThreads:       controllerThreads,
		ControllerThreadReasons: s.controllerReasons,
	}
	data, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	temporary := s.path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	if err := os.Chmod(temporary, 0o600); err != nil {
		return fmt.Errorf("secure state: %w", err)
	}
	if err := os.Rename(temporary, s.path); err != nil {
		return fmt.Errorf("commit state: %w", err)
	}
	return nil
}

func randomID() (string, error) {
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate account ID: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}
