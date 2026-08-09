package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

const recoveryPattern = ".credentials-recovery-*.json"

type credentialEnvelope struct {
	OAuth Credentials `json:"claudeAiOauth"`
}

// FileStore persists Claude credentials in a hop-owned slot file.
type FileStore struct {
	Path string
}

// RecoveryJournal is private storage reserved before a rotating token request.
type RecoveryJournal struct {
	file *os.File
	path string
}

// LiveFile reads and writes a live credential file owned by the Claude CLI.
// Unlike FileStore it never creates or re-permissions the parent directory —
// live locations such as ~/.claude belong to the provider, not to hop.
type LiveFile struct {
	Path string
}

// Read loads live credentials without modifying the file.
func (live LiveFile) Read() (Credentials, error) {
	contents, err := os.ReadFile(live.Path)
	if err != nil {
		return Credentials{}, fmt.Errorf("read live Claude credentials from %s; run 'claude auth login' or fix the override path: %w", live.Path, err)
	}
	return parseCredentials(contents)
}

// Write atomically replaces the live credential file with private permissions,
// leaving the parent directory untouched.
func (live LiveFile) Write(credentials Credentials) error {
	contents, err := encodeCredentials(credentials)
	if err != nil {
		return err
	}
	return replaceFileInExistingDirectory(live.Path, contents, "live Claude credentials")
}

// Clear removes the live credential file so the provider CLI sees no login,
// leaving the parent directory untouched. A file that is already absent counts
// as cleared.
func (live LiveFile) Clear() error {
	if err := os.Remove(live.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear live Claude credentials at %s; fix its permissions and retry: %w", live.Path, err)
	}
	return nil
}

func replaceFileInExistingDirectory(path string, contents []byte, description string) error {
	directory := filepath.Dir(path)
	if _, err := os.Stat(directory); err != nil {
		return fmt.Errorf("write %s: directory %s must already exist; create it or fix the configured path: %w", description, directory, err)
	}
	temporary, err := os.CreateTemp(directory, ".live-*.json")
	if err != nil {
		return fmt.Errorf("create temporary %s in %s: %w", description, directory, err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure temporary %s: %w", description, err)
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary %s: %w", description, err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary %s: %w", description, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary %s: %w", description, err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("install %s at %s: %w", description, path, err)
	}
	return nil
}

// Read loads credentials without modifying the source file.
func (store FileStore) Read() (Credentials, error) {
	candidates, err := credentialCandidates(store.Path)
	if err != nil {
		return Credentials{}, err
	}
	var parseErr error
	for _, candidate := range candidates {
		contents, err := os.ReadFile(candidate.path)
		if err != nil {
			parseErr = err
			continue
		}
		credentials, err := parseCredentials(contents)
		if err == nil {
			return credentials, nil
		}
		parseErr = err
	}
	return Credentials{}, fmt.Errorf("read Claude credentials from %s; check the slot path and permissions: %w", store.Path, parseErr)
}

// Write atomically replaces credentials with private permissions.
func (store FileStore) Write(credentials Credentials) error {
	directory := filepath.Dir(store.Path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create Claude slot directory %s: %w", directory, err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("secure Claude slot directory %s: %w", directory, err)
	}

	contents, err := encodeCredentials(credentials)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".credentials-*.json")
	if err != nil {
		return fmt.Errorf("create temporary Claude credentials: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure temporary Claude credentials: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary Claude credentials: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary Claude credentials: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary Claude credentials: %w", err)
	}
	if err := os.Rename(temporaryPath, store.Path); err != nil {
		return fmt.Errorf("install Claude credentials at %s: %w", store.Path, err)
	}
	if err := removeSupersededRecoveryJournals(store.Path); err != nil {
		return err
	}
	return nil
}

// ReserveRecovery creates a private journal before a token rotation begins.
func (store FileStore) ReserveRecovery() (*RecoveryJournal, error) {
	directory := filepath.Dir(store.Path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create Claude slot directory %s before refresh: %w", directory, err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, fmt.Errorf("secure Claude slot directory %s before refresh: %w", directory, err)
	}
	file, err := os.CreateTemp(directory, recoveryPattern)
	if err != nil {
		return nil, fmt.Errorf("reserve Claude token recovery journal in %s before refresh: %w", directory, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, fmt.Errorf("secure Claude token recovery journal %s: %w", file.Name(), err)
	}
	return &RecoveryJournal{file: file, path: file.Name()}, nil
}

// Save retains rotated credentials when the primary slot replacement failed.
func (journal *RecoveryJournal) Save(credentials Credentials) error {
	contents, err := encodeCredentials(credentials)
	if err != nil {
		return err
	}
	if journal == nil || journal.file == nil {
		return fmt.Errorf("claude token recovery journal is not open; run 'hop login claude <account>' before retrying")
	}
	if _, err := journal.file.Write(contents); err != nil {
		return fmt.Errorf("write Claude token recovery journal %s: %w", journal.path, err)
	}
	if err := journal.file.Sync(); err != nil {
		return fmt.Errorf("sync Claude token recovery journal %s: %w", journal.path, err)
	}
	if err := journal.file.Close(); err != nil {
		return fmt.Errorf("close Claude token recovery journal %s: %w", journal.path, err)
	}
	journal.file = nil
	return nil
}

// Discard removes an unused recovery reservation after a safe outcome.
func (journal *RecoveryJournal) Discard() error {
	if journal == nil {
		return nil
	}
	if journal.file != nil {
		if err := journal.file.Close(); err != nil {
			return fmt.Errorf("close unused Claude token recovery journal %s: %w", journal.path, err)
		}
		journal.file = nil
	}
	if err := os.Remove(journal.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove unused Claude token recovery journal %s: %w", journal.path, err)
	}
	return nil
}

type credentialCandidate struct {
	path    string
	modTime int64
	primary bool
}

func credentialCandidates(primaryPath string) ([]credentialCandidate, error) {
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(primaryPath), recoveryPattern))
	if err != nil {
		return nil, fmt.Errorf("find Claude token recovery journals beside %s: %w", primaryPath, err)
	}
	paths = append(paths, primaryPath)
	candidates := make([]credentialCandidate, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect Claude credential candidate %s: %w", path, err)
		}
		if info.Size() == 0 {
			continue
		}
		candidates = append(candidates, credentialCandidate{path: path, modTime: info.ModTime().UnixNano(), primary: path == primaryPath})
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("read Claude credentials from %s; no primary credentials or saved recovery copy exists: %w", primaryPath, os.ErrNotExist)
	}
	sort.Slice(candidates, func(left, right int) bool {
		if candidates[left].modTime == candidates[right].modTime {
			return candidates[left].primary
		}
		return candidates[left].modTime > candidates[right].modTime
	})
	return candidates, nil
}

func encodeCredentials(credentials Credentials) ([]byte, error) {
	contents, err := json.MarshalIndent(credentialEnvelope{OAuth: credentials}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode Claude credentials: %w", err)
	}
	return append(contents, '\n'), nil
}

func removeSupersededRecoveryJournals(primaryPath string) error {
	primaryInfo, err := os.Stat(primaryPath)
	if err != nil {
		return fmt.Errorf("inspect installed Claude credentials at %s: %w", primaryPath, err)
	}
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(primaryPath), recoveryPattern))
	if err != nil {
		return fmt.Errorf("find superseded Claude token recovery journals beside %s: %w", primaryPath, err)
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect Claude token recovery journal %s: %w", path, err)
		}
		if info.Size() == 0 || info.ModTime().After(primaryInfo.ModTime()) {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove superseded Claude token recovery journal %s: %w", path, err)
		}
	}
	return nil
}

func parseCredentials(contents []byte) (Credentials, error) {
	var envelope credentialEnvelope
	if err := json.Unmarshal(contents, &envelope); err != nil {
		return Credentials{}, fmt.Errorf("decode Claude credentials; expected .claudeAiOauth with accessToken and refreshToken: %w: %w", err, ErrCredentials)
	}
	if envelope.OAuth.AccessToken == "" {
		return Credentials{}, fmt.Errorf("claude credentials omit .claudeAiOauth.accessToken; run 'hop login claude <account>': %w", ErrCredentials)
	}
	return envelope.OAuth, nil
}
