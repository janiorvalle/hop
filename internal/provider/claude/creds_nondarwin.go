//go:build !darwin

package claude

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// ReadLiveCredentials reads Claude Code's credential file without changing it.
func ReadLiveCredentials(_ context.Context) (Credentials, error) {
	path, err := liveCredentialsPath()
	if err != nil {
		return Credentials{}, err
	}
	return (FileStore{Path: path}).Read()
}

// WriteLiveCredentials replaces Claude Code's credential file without touching
// the surrounding provider-owned directory.
func WriteLiveCredentials(_ context.Context, credentials Credentials) error {
	path, err := liveCredentialsPath()
	if err != nil {
		return err
	}
	return (LiveFile{Path: path}).Write(credentials)
}

// ClearLiveCredentials removes Claude Code's credential file so the Claude CLI
// sees no login and opens its browser sign-in. A file that is already absent
// counts as cleared.
func ClearLiveCredentials(_ context.Context) error {
	path, err := liveCredentialsPath()
	if err != nil {
		return err
	}
	return (LiveFile{Path: path}).Clear()
}

// ClearLiveCredentialsIfMatches removes only the file hop installed.
func ClearLiveCredentialsIfMatches(_ context.Context, expected Credentials) error {
	path, err := liveCredentialsPath()
	if err != nil {
		return err
	}
	return (LiveFile{Path: path}).ClearIfMatches(expected)
}

// LiveCredentialsTarget names where live Claude credentials are stored, for
// switch-transaction fingerprints. It resolves CLAUDE_CONFIG_DIR so a
// transaction recorded under one config directory is never restored into
// another.
func LiveCredentialsTarget() (string, error) {
	path, err := liveCredentialsPath()
	if err != nil {
		return "", err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve the live Claude credential path %s: %w", path, err)
	}
	return absolute, nil
}

func liveCredentialsPath() (string, error) {
	if configDirectory := os.Getenv("CLAUDE_CONFIG_DIR"); configDirectory != "" {
		return filepath.Join(configDirectory, ".credentials.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find the home directory for Claude credentials: %w", err)
	}
	return filepath.Join(home, ".claude", ".credentials.json"), nil
}
