package livefile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ClearIfMatches atomically moves the current live file out of the provider's
// path before inspecting it. A provider that writes a new file during the
// check keeps that new file; hop never removes a path it did not inspect.
func ClearIfMatches(path, description string, matches func(string) (bool, error)) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect %s at %s before clearing it: %w", description, path, err)
	}

	quarantine, err := reserveQuarantinePath(path)
	if err != nil {
		return fmt.Errorf("reserve a private recovery path beside %s before clearing it: %w", path, err)
	}
	if err := os.Rename(path, quarantine); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("move %s aside before clearing it; the live file was not changed: %w", description, err)
	}

	matched, matchErr := matches(quarantine)
	if matchErr != nil {
		return restoreQuarantine(path, quarantine, fmt.Errorf("verify the quarantined %s before clearing it: %w", description, matchErr))
	}
	if !matched {
		return restoreQuarantine(path, quarantine, fmt.Errorf("%s changed before hop could clear it; hop preserved the unexpected login", description))
	}
	if err := os.Remove(quarantine); err != nil {
		return restoreQuarantine(path, quarantine, fmt.Errorf("remove the verified %s from its private recovery path: %w", description, err))
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("a new %s appeared while hop restored the previous absence; hop left that new login untouched", description)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("confirm %s are absent after clearing them: %w", description, err)
	}
	return nil
}

func reserveQuarantinePath(path string) (string, error) {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".hop-conditional-clear-*")
	if err != nil {
		return "", err
	}
	quarantine := temporary.Name()
	if err := temporary.Close(); err != nil {
		_ = os.Remove(quarantine)
		return "", err
	}
	if err := os.Remove(quarantine); err != nil {
		return "", err
	}
	return quarantine, nil
}

func restoreQuarantine(path, quarantine string, reason error) error {
	if err := os.Link(quarantine, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w; another live file now exists, so both were preserved. Restore or remove the quarantined credential at %s, then retry", reason, quarantine)
		}
		return fmt.Errorf("%w; restore the preserved credential from %s to %s, then retry: %v", reason, quarantine, path, err)
	}
	if err := os.Remove(quarantine); err != nil {
		return fmt.Errorf("%w; the live file was restored, but remove its extra recovery link at %s before retrying: %v", reason, quarantine, err)
	}
	return reason
}
