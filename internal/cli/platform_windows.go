//go:build windows

package cli

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func loginTerminationSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

func processIsRunning(processID int) bool {
	process, err := os.FindProcess(processID)
	if err != nil {
		return false
	}
	_ = process.Release()
	return true
}

func tryAcquireFileLock(path string) (func(), bool, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	overlapped := &windows.Overlapped{}
	err = windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		overlapped,
	)
	if err != nil {
		_ = file.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() {
		_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
		_ = file.Close()
	}, true, nil
}
