package lock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLockBlocksOtherProcess(t *testing.T) {
	if os.Getenv("WACLI_LOCK_HELPER") == "1" {
		dir := os.Getenv("WACLI_LOCK_DIR")
		lk, err := Acquire(dir)
		if err == nil {
			_ = lk.Release()
			_, _ = os.Stdout.WriteString("UNEXPECTED_OK\n")
			os.Exit(2)
		}
		if !strings.Contains(err.Error(), "store is locked") {
			_, _ = fmt.Fprintf(os.Stdout, "UNEXPECTED_ERR:%v\n", err)
			os.Exit(3)
		}
		if !IsLocked(err) {
			_, _ = fmt.Fprintf(os.Stdout, "UNEXPECTED_NOT_LOCKED:%v\n", err)
			os.Exit(4)
		}
		_, _ = os.Stdout.WriteString("EXPECTED_LOCKED\n")
		return
	}

	dir := t.TempDir()

	lk, err := Acquire(dir)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lk.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestLockBlocksOtherProcess")
	cmd.Env = append(os.Environ(),
		"WACLI_LOCK_HELPER=1",
		"WACLI_LOCK_DIR="+dir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper failed: %v output=%s", err, strings.TrimSpace(string(out)))
	}
	got := string(out)
	if strings.Contains(got, "UNEXPECTED_OK") || strings.Contains(got, "UNEXPECTED_ERR:") {
		t.Fatalf("unexpected helper output: %q", strings.TrimSpace(got))
	}
	if strings.Contains(got, "UNEXPECTED_NOT_LOCKED:") {
		t.Fatalf("helper error did not wrap ErrLocked: %q", strings.TrimSpace(got))
	}
	if !strings.Contains(got, "EXPECTED_LOCKED") {
		t.Fatalf("expected helper to report locked; output=%q", strings.TrimSpace(got))
	}
}

func TestAcquireWithTimeout(t *testing.T) {
	dir := t.TempDir()

	lk, err := Acquire(dir)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lk.Release()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = AcquireWithTimeout(ctx, dir, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for store lock") {
		t.Fatalf("AcquireWithTimeout error = %v", err)
	}
	if !errors.Is(err, ErrLocked) || !IsLocked(err) {
		t.Fatalf("AcquireWithTimeout did not wrap ErrLocked: %v", err)
	}
}

func TestAcquireWithTimeoutHonorsCanceledContext(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	lk, err := AcquireWithTimeout(ctx, dir, time.Second)
	if err == nil {
		_ = lk.Release()
		t.Fatalf("AcquireWithTimeout acquired lock with canceled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("AcquireWithTimeout error = %v, want context.Canceled", err)
	}
}

func TestAcquireWithTimeoutDoesNotRetryNonLockErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := AcquireWithTimeout(ctx, path, time.Hour)
	if err == nil {
		t.Fatalf("AcquireWithTimeout succeeded for file store path")
	}
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "timed out waiting for store lock") {
		t.Fatalf("AcquireWithTimeout retried non-lock error: %v", err)
	}
	if IsLocked(err) {
		t.Fatalf("AcquireWithTimeout classified non-lock error as locked: %v", err)
	}
}
