//go:build !windows

package lock

import (
	"syscall"
	"testing"
)

func TestIsLockContention(t *testing.T) {
	if !isLockContention(syscall.EWOULDBLOCK) {
		t.Fatalf("EWOULDBLOCK should be lock contention")
	}
	if !isLockContention(syscall.EAGAIN) {
		t.Fatalf("EAGAIN should be lock contention")
	}
	if isLockContention(syscall.EINTR) {
		t.Fatalf("EINTR should not be lock contention")
	}
}
