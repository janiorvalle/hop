//go:build !windows

package cli

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func loginTerminationSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}
}

func tryAcquireFileLock(path string) (func(), bool, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}, true, nil
}

func processIsRunning(processID int) bool {
	err := syscall.Kill(processID, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
