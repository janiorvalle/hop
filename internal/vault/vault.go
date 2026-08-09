// Package vault defines the on-disk account slot layout.
package vault

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const CredentialsFilename = "credentials.json"

var ErrInvalidSlot = errors.New("invalid account slot")

// Vault locates account credentials below a hop data directory.
type Vault struct {
	root string
}

// New creates a vault rooted at root.
func New(root string) (Vault, error) {
	if strings.TrimSpace(root) == "" {
		return Vault{}, fmt.Errorf("vault root cannot be empty: %w", ErrInvalidSlot)
	}
	return Vault{root: filepath.Clean(root)}, nil
}

// Default returns the vault at ~/.hop.
func Default() (Vault, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Vault{}, fmt.Errorf("find home directory for ~/.hop: %w", err)
	}
	return New(filepath.Join(home, ".hop"))
}

// Root returns the vault data directory.
func (v Vault) Root() string {
	return v.root
}

// SlotPath returns the directory for one provider account.
func (v Vault) SlotPath(provider, account string) (string, error) {
	if provider != "claude" && provider != "codex" {
		return "", fmt.Errorf("provider %q is not supported; use claude or codex: %w", provider, ErrInvalidSlot)
	}
	if !validAccountName(account) {
		return "", fmt.Errorf("account %q is invalid; use letters, numbers, dots, dashes, or underscores: %w", account, ErrInvalidSlot)
	}
	return filepath.Join(v.root, provider, account), nil
}

// CredentialsPath returns the only credential file owned by a slot.
func (v Vault) CredentialsPath(provider, account string) (string, error) {
	slot, err := v.SlotPath(provider, account)
	if err != nil {
		return "", err
	}
	return filepath.Join(slot, CredentialsFilename), nil
}

// EnsureSlot creates a private directory for one provider account.
func (v Vault) EnsureSlot(provider, account string) (string, error) {
	slot, err := v.SlotPath(provider, account)
	if err != nil {
		return "", err
	}
	for _, directory := range []string{v.root, filepath.Join(v.root, provider), slot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return "", fmt.Errorf("create account slot %s: %w", slot, err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return "", fmt.Errorf("secure account slot directory %s: %w", directory, err)
		}
	}
	return slot, nil
}

func validAccountName(account string) bool {
	if account == "" || account == "." || account == ".." {
		return false
	}
	for _, character := range account {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			continue
		}
		if character != '.' && character != '-' && character != '_' {
			return false
		}
	}
	return true
}
