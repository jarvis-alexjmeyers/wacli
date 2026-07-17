package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/store"
)

func TestChangesListCommandWritesStandardJSONEnvelopeReadOnly(t *testing.T) {
	storeDir := t.TempDir()
	db, err := store.Open(filepath.Join(storeDir, "wacli.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	chat := "changes-command@s.whatsapp.net"
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	if err := db.UpsertChat(chat, "dm", "Changes", now); err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: chat, MsgID: "m1", SenderJID: chat, Timestamp: now, Text: "body"}); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	flags := &rootFlags{storeDir: storeDir, asJSON: true, readOnly: true, timeout: time.Minute}
	raw := captureRootStdout(t, func() {
		cmd := newChangesListCmd(flags)
		cmd.SetArgs([]string{"--after-seq", "0", "--limit", "10"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("changes list: %v", err)
		}
	})
	var envelope struct {
		Success bool `json:"success"`
		Data    struct {
			StoreInstanceID string                `json:"store_instance_id"`
			Changes         []store.MessageChange `json:"changes"`
			Purged          int                   `json:"purged"`
		} `json:"data"`
		Error *string `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, raw)
	}
	if !envelope.Success || envelope.Error != nil || envelope.Data.StoreInstanceID == "" {
		t.Fatalf("envelope = %+v", envelope)
	}
	if len(envelope.Data.Changes) != 1 || envelope.Data.Changes[0].Message == nil || envelope.Data.Purged != 0 {
		t.Fatalf("data = %+v", envelope.Data)
	}
}

func TestChangesListCommandPreservesProviderAddressingTriStateJSON(t *testing.T) {
	storeDir := t.TempDir()
	db, err := store.Open(filepath.Join(storeDir, "wacli.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	chat := "changes-addressing@s.whatsapp.net"
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	if err := db.UpsertChat(chat, "dm", "Addressing", now); err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	mentionsMe := true
	repliesToMe := false
	if err := db.UpsertMessage(store.UpsertMessageParams{
		ChatJID:     chat,
		MsgID:       "derived",
		Timestamp:   now,
		MentionsMe:  &mentionsMe,
		RepliesToMe: &repliesToMe,
	}); err != nil {
		t.Fatalf("derived insert: %v", err)
	}
	if err := db.UpsertMessage(store.UpsertMessageParams{
		ChatJID:   chat,
		MsgID:     "underived",
		Timestamp: now,
	}); err != nil {
		t.Fatalf("underived insert: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	flags := &rootFlags{storeDir: storeDir, asJSON: true, readOnly: true, timeout: time.Minute}
	raw := captureRootStdout(t, func() {
		cmd := newChangesListCmd(flags)
		cmd.SetArgs([]string{"--after-seq", "0", "--limit", "10"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("changes list: %v", err)
		}
	})
	var envelope struct {
		Success bool `json:"success"`
		Data    struct {
			Changes []struct {
				Message map[string]json.RawMessage `json:"message"`
			} `json:"changes"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !envelope.Success || len(envelope.Data.Changes) != 2 {
		t.Fatalf("changes response success=%v count=%d, want true/2", envelope.Success, len(envelope.Data.Changes))
	}
	derived := envelope.Data.Changes[0].Message
	if got := string(derived["MentionsMe"]); got != "true" {
		t.Fatalf("derived MentionsMe JSON = %q, want true", got)
	}
	if got := string(derived["RepliesToMe"]); got != "false" {
		t.Fatalf("derived RepliesToMe JSON = %q, want false", got)
	}
	underived := envelope.Data.Changes[1].Message
	if _, ok := underived["MentionsMe"]; ok {
		t.Fatal("underived message fabricated MentionsMe instead of omitting unknown tri-state")
	}
	if _, ok := underived["RepliesToMe"]; ok {
		t.Fatal("underived message fabricated RepliesToMe instead of omitting unknown tri-state")
	}
}

func TestChangesStatusCommandEmptyStoreBootstrapIsZero(t *testing.T) {
	storeDir := t.TempDir()
	db, err := store.Open(filepath.Join(storeDir, "wacli.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	flags := &rootFlags{storeDir: storeDir, asJSON: true, readOnly: true, timeout: time.Minute}
	raw := captureRootStdout(t, func() {
		cmd := newChangesStatusCmd(flags)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("changes status: %v", err)
		}
	})
	var envelope struct {
		Success bool `json:"success"`
		Data    struct {
			BootstrapSeq int64 `json:"bootstrap_seq"`
			MaxAllocated int64 `json:"max_allocated"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, raw)
	}
	if !envelope.Success || envelope.Data.BootstrapSeq != 0 || envelope.Data.MaxAllocated != 0 {
		t.Fatalf("status envelope = %+v", envelope)
	}
}

func TestChangesListCommandRejectsFutureCursor(t *testing.T) {
	storeDir := t.TempDir()
	db, err := store.Open(filepath.Join(storeDir, "wacli.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	cmd := newChangesListCmd(&rootFlags{storeDir: storeDir, readOnly: true, timeout: time.Minute})
	cmd.SetArgs([]string{"--after-seq", "1"})
	if err := cmd.Execute(); !errors.Is(err, store.ErrChangeCursorFuture) {
		t.Fatalf("error = %v, want cursor_future", err)
	}
}

func TestChangesCursorErrorUsesStandardJSONEnvelope(t *testing.T) {
	storeDir := t.TempDir()
	db, err := store.Open(filepath.Join(storeDir, "wacli.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var executeErr error
	raw := captureRootStderr(t, func() {
		executeErr = execute([]string{"--store", storeDir, "--read-only", "--json", "changes", "list", "--after-seq", "1"})
	})
	if !errors.Is(executeErr, store.ErrChangeCursorFuture) {
		t.Fatalf("execute error = %v, want cursor_future", executeErr)
	}
	var envelope struct {
		Success bool    `json:"success"`
		Data    any     `json:"data"`
		Error   *string `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, raw)
	}
	if envelope.Success || envelope.Data != nil || envelope.Error == nil || *envelope.Error != "cursor_future" {
		t.Fatalf("error envelope = %+v", envelope)
	}
}
