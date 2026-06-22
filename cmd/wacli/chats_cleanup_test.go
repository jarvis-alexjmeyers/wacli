package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChatsCleanupRejectsNonPositiveDaysBeforeOpeningStore(t *testing.T) {
	for _, days := range []string{"0", "-1"} {
		t.Run(days, func(t *testing.T) {
			storeDir := t.TempDir()
			cmd := newChatsCleanupCmd(&rootFlags{storeDir: storeDir, timeout: time.Minute})
			cmd.SetArgs([]string{"--days", days, "--confirm"})

			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), "--days must be greater than 0") {
				t.Fatalf("error = %v, want days validation", err)
			}
			if _, statErr := os.Stat(filepath.Join(storeDir, "wacli.db")); !os.IsNotExist(statErr) {
				t.Fatalf("wacli.db stat err = %v, want not created", statErr)
			}
		})
	}
}
