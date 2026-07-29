package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/types"
)

// The whole point of `groups participants list` is that it answers while a
// `sync --follow` daemon owns the store. TWO properties have to hold for that,
// and only one of them is about the flock:
//
//  1. it must not acquire the store lock, and
//  2. it must open the store READ-ONLY — a read-write open runs ensureSchema()
//     DDL plus a store_meta seed on every invocation, which contends for the
//     WAL writer and fails "database is locked" under a concurrent writer.
//
// Neither is visible from the store-level tests; both are properties of this
// command. Note the flags below deliberately leave readOnly FALSE: the command
// must be read-only regardless of what the caller passed, because the edge
// invokes a bare `wacli`.

func seedStoreForListCmd(t *testing.T, storeDir, groupJID string) {
	t.Helper()
	db, err := store.Open(filepath.Join(storeDir, "wacli.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.UpsertGroupWithHierarchy(
		groupJID, "Weekend Plans", "", time.Now(), false, "",
	); err != nil {
		t.Fatalf("UpsertGroupWithHierarchy: %v", err)
	}
	if err := db.ReplaceGroupParticipants(groupJID, []store.GroupParticipant{
		{GroupJID: groupJID, UserJID: "15550000001@s.whatsapp.net", Role: "member"},
		{GroupJID: groupJID, UserJID: "15550000002@s.whatsapp.net", Role: "admin"},
	}); err != nil {
		t.Fatalf("ReplaceGroupParticipants: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

type listEnvelope struct {
	Success bool `json:"success"`
	Data    struct {
		JID            string `json:"JID"`
		Name           string `json:"Name"`
		LeftAt         string `json:"LeftAt"`
		FromLocalStore bool   `json:"FromLocalStore"`
		Participants   []struct {
			JID  string `json:"JID"`
			Role string `json:"Role"`
		} `json:"Participants"`
	} `json:"data"`
}

func runListCmd(t *testing.T, storeDir, groupJID string) (listEnvelope, error) {
	t.Helper()
	flags := &rootFlags{storeDir: storeDir, asJSON: true, timeout: time.Minute}
	var runErr error
	raw := captureRootStdout(t, func() {
		cmd := newGroupsParticipantsListCmd(flags)
		cmd.SetArgs([]string{"--jid", groupJID})
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		runErr = cmd.Execute()
	})
	var envelope listEnvelope
	if runErr == nil {
		if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
			t.Fatalf("unmarshal %q: %v", raw, err)
		}
	}
	return envelope, runErr
}

// Fails if the command is switched back to a read-write open: a concurrent
// write transaction is exactly the follow daemon's posture.
func TestGroupsParticipantsListAnswersUnderAConcurrentWriter(t *testing.T) {
	storeDir := t.TempDir()
	const groupJID = "120363000000000002@g.us"
	seedStoreForListCmd(t, storeDir, groupJID)

	raw, err := sql.Open("sqlite3", filepath.Join(storeDir, "wacli.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()
	tx, err := raw.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// Claim the writer for the duration of the command.
	if _, err := tx.Exec("CREATE TABLE IF NOT EXISTS _writer_probe (x INTEGER)"); err != nil {
		t.Fatalf("claim the writer: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	envelope, err := runListCmd(t, storeDir, groupJID)
	if err != nil {
		t.Fatalf("command failed while a writer held the store: %v", err)
	}
	if !envelope.Success {
		t.Fatal("envelope reported failure")
	}
	if len(envelope.Data.Participants) != 2 {
		t.Fatalf("want 2 participants, got %d", len(envelope.Data.Participants))
	}
	if !envelope.Data.FromLocalStore {
		t.Fatal("FromLocalStore must mark this as a cache read")
	}
	if envelope.Data.LeftAt != "" {
		t.Fatalf("a group still joined must report no LeftAt, got %q", envelope.Data.LeftAt)
	}
}

// Leaving a group does not clear group_participants, so without LeftAt a caller
// reads a resurrected membership for a group it is not in, with no way to tell.
func TestGroupsParticipantsListReportsALeftGroup(t *testing.T) {
	storeDir := t.TempDir()
	const groupJID = "120363000000000002@g.us"
	seedStoreForListCmd(t, storeDir, groupJID)

	db, err := store.Open(filepath.Join(storeDir, "wacli.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.MarkGroupLeft(groupJID, time.Now()); err != nil {
		t.Fatalf("MarkGroupLeft: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	envelope, err := runListCmd(t, storeDir, groupJID)
	if err != nil {
		t.Fatalf("command failed: %v", err)
	}
	if envelope.Data.LeftAt == "" {
		t.Fatal("a left group must report LeftAt so the caller can refuse its roster")
	}
	// The members are still stored, which is exactly why LeftAt has to be said
	// out loud rather than inferred from an empty list.
	if len(envelope.Data.Participants) == 0 {
		t.Fatal("expected the stale membership to still be present")
	}
}

// A read-only open must never bring a store into existence.
func TestGroupsParticipantsListDoesNotCreateAStore(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "absent")

	if _, err := runListCmd(t, storeDir, "120363000000000002@g.us"); err == nil {
		t.Fatal("a missing store must be an error, not a fresh empty database")
	}
	if _, err := os.Stat(filepath.Join(storeDir, "wacli.db")); err == nil {
		t.Fatal("the command created a store database")
	}
}

// --- LID resolution -------------------------------------------------------
//
// The sync path stores whatever JID the provider used, and for a modern
// WhatsApp group that is a privacy LID. A LID matches nothing in a contact
// directory, so emitting it raw makes every member unresolvable — which is what
// production did: 9 of 9 members stored as LIDs, every one showing "no contact
// linked". These pin the split.

type stubResolver struct{ m map[string]string }

func (s stubResolver) ResolveLIDToPN(_ context.Context, jid types.JID) types.JID {
	if pn, ok := s.m[jid.User]; ok {
		return types.JID{User: pn, Server: types.DefaultUserServer}
	}
	return jid
}

func TestResolveParticipantJIDSplitsLidFromPhone(t *testing.T) {
	r := stubResolver{m: map[string]string{"999123456789": "15550000001"}}

	// A stored phone number passes through as the JID, with no LID.
	jid, lid := resolveParticipantJID(context.Background(), r, "15550000002@s.whatsapp.net")
	if jid != "15550000002@s.whatsapp.net" || lid != "" {
		t.Fatalf("phone passthrough: jid=%q lid=%q", jid, lid)
	}

	// A resolvable LID yields the PHONE as the JID and keeps the LID beside it.
	jid, lid = resolveParticipantJID(context.Background(), r, "999123456789@lid")
	if jid != "15550000001@s.whatsapp.net" {
		t.Fatalf("resolvable lid should yield the phone jid, got %q", jid)
	}
	if lid != "999123456789@lid" {
		t.Fatalf("the original lid must be preserved, got %q", lid)
	}
}

// The bug this whole change exists to prevent: an UNRESOLVABLE lid must never
// be emitted as the JID. A consumer that prefers JID would then hold something
// it believes is a phone number, match nothing, and report a member it cannot
// name — silently.
func TestAnUnresolvableLidIsNeverEmittedAsAPhoneJID(t *testing.T) {
	r := stubResolver{m: map[string]string{}}

	jid, lid := resolveParticipantJID(context.Background(), r, "999999999999@lid")
	if jid != "" {
		t.Fatalf("an unresolvable lid must not become the JID, got %q", jid)
	}
	if lid != "999999999999@lid" {
		t.Fatalf("the lid must still be reported, got %q", lid)
	}

	// Same rule with no resolver at all (no session.db / open failure):
	// degrade to LID-only, never fail the read and never fake a phone jid.
	jid, lid = resolveParticipantJID(context.Background(), nil, "999999999999@lid")
	if jid != "" || lid != "999999999999@lid" {
		t.Fatalf("nil resolver: jid=%q lid=%q", jid, lid)
	}
}

// A resolver that hands back another LID (whatsmeow returns the input unchanged
// when it has no mapping) must be treated as unresolved, not as a phone number.
func TestALidResolvingToALidIsStillUnresolved(t *testing.T) {
	r := stubResolver{m: map[string]string{}}
	jid, lid := resolveParticipantJID(context.Background(), r, "888888888888@lid")
	if jid != "" || lid != "888888888888@lid" {
		t.Fatalf("lid->lid must stay unresolved: jid=%q lid=%q", jid, lid)
	}
}
