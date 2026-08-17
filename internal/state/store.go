package state

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
	CreatedAt  int64  `json:"createdAt"`
}

type persistedState struct {
	Version     int               `json:"version"`
	Accounts    []Account         `json:"accounts"`
	ThreadOwner map[string]string `json:"threadOwner"`
	ThreadModel map[string]string `json:"threadModel,omitempty"`
}

// Store persists only routing metadata. OAuth credentials and conversation
// databases remain inside each account's isolated Codex home.
type Store struct {
	mu               sync.RWMutex
	root             string
	path             string
	primaryCodexHome string
	accounts         []Account
	owners           map[string]string
	models           map[string]string
	configMu         sync.Mutex
	managedConfigSum [sha256.Size]byte
	hasManagedSum    bool
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
		root:             root,
		path:             filepath.Join(root, "state.json"),
		primaryCodexHome: primaryCodexHome,
		owners:           make(map[string]string),
		models:           make(map[string]string),
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
		if account.Controller && account.Enabled {
			return account, true
		}
	}
	for _, account := range s.accounts {
		if account.Enabled {
			return account, true
		}
	}
	return Account{}, false
}

func (s *Store) AddAccount(label string) (Account, error) {
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
	accountRoot := filepath.Join(s.root, "accounts", id)
	if !samePath(filepath.Dir(removed.CodexHome), accountRoot) {
		return Account{}, errors.New("account home is outside the managed account root")
	}

	previousAccounts := s.accounts
	previousOwners := s.owners
	previousModels := s.models
	s.accounts = append(slices.Clone(s.accounts[:index]), s.accounts[index+1:]...)
	s.owners = make(map[string]string, len(previousOwners))
	s.models = make(map[string]string, len(previousModels))
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
	if err := s.saveLocked(); err != nil {
		s.accounts = previousAccounts
		s.owners = previousOwners
		s.models = previousModels
		return Account{}, err
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

func (s *Store) CompareAndSwapThreadOwner(threadID, oldAccountID, newAccountID string) error {
	if threadID == "" || oldAccountID == "" || newAccountID == "" {
		return errors.New("thread and account IDs are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.owners[threadID]
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
	if threadID == "" || model == "" {
		return errors.New("thread ID and model are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.models[threadID] == model {
		return nil
	}
	previous, existed := s.models[threadID]
	s.models[threadID] = model
	if err := s.saveLocked(); err != nil {
		if existed {
			s.models[threadID] = previous
		} else {
			delete(s.models, threadID)
		}
		return err
	}
	return nil
}

func (s *Store) ReplaceThreadMetadata(owners, models map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previousOwners := s.owners
	previousModels := s.models
	s.owners = make(map[string]string, len(owners))
	for threadID, accountID := range owners {
		if threadID != "" && accountID != "" {
			if persistedOwner := previousOwners[threadID]; persistedOwner != "" {
				s.owners[threadID] = persistedOwner
			} else {
				s.owners[threadID] = accountID
			}
		}
	}
	s.models = make(map[string]string, len(previousModels)+len(models))
	for threadID, model := range previousModels {
		if _, exists := s.owners[threadID]; exists {
			s.models[threadID] = model
		}
	}
	for threadID, model := range models {
		if _, exists := s.owners[threadID]; exists && model != "" {
			s.models[threadID] = model
		}
	}
	if maps.Equal(previousOwners, s.owners) && maps.Equal(previousModels, s.models) {
		s.owners = previousOwners
		s.models = previousModels
		return nil
	}
	if err := s.saveLocked(); err != nil {
		s.owners = previousOwners
		s.models = previousModels
		return err
	}
	return nil
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

func (s *Store) saveLocked() error {
	persisted := persistedState{
		Version:     stateVersion,
		Accounts:    s.accounts,
		ThreadOwner: s.owners,
		ThreadModel: s.models,
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
