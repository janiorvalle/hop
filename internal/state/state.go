// Package state stores which account is active for each provider.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const Filename = "state.json"

// State is the durable active-account selection.
type State struct {
	ActiveAccounts map[string]string `json:"active"`
}

// New returns an empty, writable state value.
func New() State {
	return State{ActiveAccounts: make(map[string]string)}
}

// Load reads state.json below root. A missing file is an empty state.
func Load(root string) (State, error) {
	contents, err := os.ReadFile(filepath.Join(root, Filename))
	if errors.Is(err, os.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return State{}, fmt.Errorf("read active-account state: %w", err)
	}

	loaded := New()
	if err := json.Unmarshal(contents, &loaded); err != nil {
		return State{}, fmt.Errorf("read active-account state: %w; fix or remove %s", err, filepath.Join(root, Filename))
	}
	if loaded.ActiveAccounts == nil {
		loaded.ActiveAccounts = make(map[string]string)
	}
	return loaded, nil
}

// SetActive records the active account for a provider in memory.
func (s *State) SetActive(provider, account string) {
	if s.ActiveAccounts == nil {
		s.ActiveAccounts = make(map[string]string)
	}
	s.ActiveAccounts[provider] = account
}

// Active returns the active account for a provider.
func (s State) Active(provider string) (string, bool) {
	account, found := s.ActiveAccounts[provider]
	return account, found
}

// Save atomically writes state.json below root with private permissions.
func (s State) Save(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create hop data directory %s: %w", root, err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return fmt.Errorf("secure hop data directory %s: %w", root, err)
	}

	activeAccounts := s.ActiveAccounts
	if activeAccounts == nil {
		activeAccounts = make(map[string]string)
	}
	contents, err := json.MarshalIndent(State{ActiveAccounts: activeAccounts}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode active-account state: %w", err)
	}
	contents = append(contents, '\n')

	temporary, err := os.CreateTemp(root, ".state-*.json")
	if err != nil {
		return fmt.Errorf("create temporary active-account state: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()

	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure temporary active-account state: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary active-account state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary active-account state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary active-account state: %w", err)
	}
	if err := os.Rename(temporaryPath, filepath.Join(root, Filename)); err != nil {
		return fmt.Errorf("install active-account state: %w", err)
	}
	return nil
}
