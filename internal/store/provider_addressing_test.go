package store

import (
	"path/filepath"
	"testing"
	"time"
)

const testChatJID = "120363000000000000@g.us"

func newTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// messages.chat_jid is a FOREIGN KEY into chats.
	if err := db.UpsertChat(testChatJID, "group", "Test Group", time.Now()); err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	return db
}

// AITOOLS-927 persistence. Two invariants, both of which protect a KNOWN truth from being erased:
//
//  1. NULL means "we never derived this" and must never be read as false. A false default would
//     assert "this message did not mention you" about every row in the deployed store.
//  2. An UNKNOWN must never clobber a KNOWN. A re-sync by an older producer (or a history backfill
//     that cannot derive these) would otherwise silently erase a true and drop the wake.

func ptr(b bool) *bool { return &b }

func tri(v *bool) string {
	if v == nil {
		return "null"
	}
	if *v {
		return "true"
	}
	return "false"
}

func baseMsg(id string, ts time.Time) UpsertMessageParams {
	return UpsertMessageParams{
		ChatJID:   testChatJID,
		MsgID:     id,
		SenderJID: "15559990000@s.whatsapp.net",
		Timestamp: ts,
		Text:      "hello",
	}
}

func onlyMsg(t *testing.T, d *DB) Message {
	t.Helper()
	msgs, err := d.ListMessages(ListMessagesParams{Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("want exactly 1 message, got %d", len(msgs))
	}
	return msgs[0]
}

func TestProviderAddressingRoundTripsAsTriState(t *testing.T) {
	d := newTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	p := baseMsg("M1", now)
	p.MentionsMe = ptr(true)
	p.RepliesToMe = ptr(false)
	if err := d.UpsertMessage(p); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got := onlyMsg(t, d)
	if got.MentionsMe == nil || !*got.MentionsMe {
		t.Errorf("MentionsMe = %s, want true", tri(got.MentionsMe))
	}
	// The important half: a real `false` must survive as false, NOT decay to null...
	if got.RepliesToMe == nil || *got.RepliesToMe {
		t.Errorf("RepliesToMe = %s, want false", tri(got.RepliesToMe))
	}
}

func TestUnderivedStaysNullNotFalse(t *testing.T) {
	d := newTestDB(t)
	// ...and the other half: never derived must stay NULL, not become a false we never established.
	if err := d.UpsertMessage(baseMsg("M1", time.Now().UTC())); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got := onlyMsg(t, d)
	if got.MentionsMe != nil {
		t.Errorf("MentionsMe = %s on a message we never examined; want null", tri(got.MentionsMe))
	}
	if got.RepliesToMe != nil {
		t.Errorf("RepliesToMe = %s; want null", tri(got.RepliesToMe))
	}
}

// THE PRECEDENCE BUG THIS PREVENTS: wacli re-syncs the same message from history (or an older
// producer re-delivers it) with no derivation. Without the COALESCE guard the NULL overwrites the
// stored `true`, MentionsMe silently becomes unknown, and the wake is lost with no error anywhere.
func TestUnknownNeverClobbersKnown(t *testing.T) {
	d := newTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	first := baseMsg("M1", now)
	first.MentionsMe = ptr(true)
	first.RepliesToMe = ptr(true)
	if err := d.UpsertMessage(first); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// Same message, re-delivered by something that cannot derive provider addressing.
	redeliver := baseMsg("M1", now)
	redeliver.MentionsMe = nil
	redeliver.RepliesToMe = nil
	if err := d.UpsertMessage(redeliver); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}

	got := onlyMsg(t, d)
	if got.MentionsMe == nil || !*got.MentionsMe {
		t.Errorf("MentionsMe = %s after an underived re-sync; a NULL must NEVER erase a known true", tri(got.MentionsMe))
	}
	if got.RepliesToMe == nil || !*got.RepliesToMe {
		t.Errorf("RepliesToMe = %s after an underived re-sync; want the stored true to survive", tri(got.RepliesToMe))
	}
}

// A KNOWN value may correct another KNOWN value — the guard must not freeze the column.
func TestKnownOverwritesKnown(t *testing.T) {
	d := newTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	first := baseMsg("M1", now)
	first.MentionsMe = ptr(false)
	if err := d.UpsertMessage(first); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	corrected := baseMsg("M1", now)
	corrected.MentionsMe = ptr(true)
	if err := d.UpsertMessage(corrected); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	got := onlyMsg(t, d)
	if got.MentionsMe == nil || !*got.MentionsMe {
		t.Errorf("MentionsMe = %s; a KNOWN value must be able to correct another known value", tri(got.MentionsMe))
	}
}

// A STALE delivery (an older copy of the same message arriving late) must not overwrite a fresher
// derivation — reusing wacli's own staleness predicate, not a new one.
func TestStaleDeliveryDoesNotOverwrite(t *testing.T) {
	d := newTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	fresh := baseMsg("M1", now)
	fresh.MentionsMe = ptr(true)
	if err := d.UpsertMessage(fresh); err != nil {
		t.Fatalf("fresh upsert: %v", err)
	}

	stale := baseMsg("M1", now.Add(-1*time.Hour)) // an OLDER copy, arriving late
	stale.MentionsMe = ptr(false)
	if err := d.UpsertMessage(stale); err != nil {
		t.Fatalf("stale upsert: %v", err)
	}

	got := onlyMsg(t, d)
	if got.MentionsMe == nil || !*got.MentionsMe {
		t.Errorf("MentionsMe = %s; a STALE delivery must not overwrite a fresher derivation", tri(got.MentionsMe))
	}
}

// The deployed store (v0.11.2) has neither column. Upgrading must add them as NULL — every existing
// row becomes "we never derived this", which is the truth. NOT NULL DEFAULT 0 would instead assert a
// false about millions of rows nobody ever examined.
func TestUpgradedStoreBackfillsNullNotFalse(t *testing.T) {
	d := newTestDB(t)

	for _, col := range []string{"mentions_me", "replies_to_me"} {
		has, err := d.tableHasColumn("messages", col)
		if err != nil {
			t.Fatalf("tableHasColumn(%s): %v", col, err)
		}
		if !has {
			t.Fatalf("migration did not add messages.%s", col)
		}
	}

	// Simulate a row written by the OLD binary: inserted without ever touching the new columns.
	if _, err := d.sql.Exec(
		`INSERT INTO messages(chat_jid, msg_id, ts, from_me, text) VALUES (?, ?, ?, 0, ?)`,
		testChatJID, "OLD-ROW", time.Now().Unix(), "written by v0.11.2",
	); err != nil {
		t.Fatalf("legacy insert: %v", err)
	}

	got := onlyMsg(t, d)
	if got.MentionsMe != nil || got.RepliesToMe != nil {
		t.Errorf(
			"a pre-existing row came back MentionsMe=%s RepliesToMe=%s; both must be NULL — "+
				"a false here is an authoritative 'you were not addressed' about a row we never examined",
			tri(got.MentionsMe), tri(got.RepliesToMe),
		)
	}
}

// The migration must be safe to re-run (an interrupted upgrade re-runs it).
func TestMigrationIsIdempotent(t *testing.T) {
	d := newTestDB(t)
	for i := 0; i < 3; i++ {
		if err := migrateMessagesProviderAddressingColumns(d); err != nil {
			t.Fatalf("re-running the migration failed on pass %d: %v", i, err)
		}
	}
}
