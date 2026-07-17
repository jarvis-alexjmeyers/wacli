package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func TestMessageChangesPageSaturatesBySequenceNotTimestamp(t *testing.T) {
	db := openTestDB(t)
	chat := "saturation@s.whatsapp.net"
	ts := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	if err := db.UpsertChat(chat, "dm", "Saturation", ts); err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	for i := 0; i < 250; i++ {
		id := fmt.Sprintf("m-%03d", i)
		if err := db.UpsertMessage(UpsertMessageParams{
			ChatJID:   chat,
			MsgID:     id,
			SenderJID: chat,
			Timestamp: ts,
			Text:      id,
		}); err != nil {
			t.Fatalf("UpsertMessage %s: %v", id, err)
		}
	}

	first, err := db.ListMessageChanges(0, 200)
	if err != nil {
		t.Fatalf("ListMessageChanges first: %v", err)
	}
	if len(first.Changes) != 200 {
		t.Fatalf("first page length = %d, want 200", len(first.Changes))
	}
	second, err := db.ListMessageChanges(first.Changes[len(first.Changes)-1].Seq, 200)
	if err != nil {
		t.Fatalf("ListMessageChanges second: %v", err)
	}
	if len(second.Changes) != 50 {
		t.Fatalf("second page length = %d, want 50", len(second.Changes))
	}
	seen := make(map[string]bool, 250)
	for _, change := range append(first.Changes, second.Changes...) {
		if change.Message == nil {
			t.Fatalf("change %d unexpectedly purged", change.Seq)
		}
		seen[change.MsgID] = true
	}
	if len(seen) != 250 {
		t.Fatalf("unique messages = %d, want 250", len(seen))
	}

	after := ts.Add(-time.Second)
	legacyHead, err := db.ListMessages(ListMessagesParams{After: &after, Limit: 200, Asc: true})
	if err != nil {
		t.Fatalf("ListMessages first: %v", err)
	}
	legacyHeadAgain, err := db.ListMessages(ListMessagesParams{After: &after, Limit: 200, Asc: true})
	if err != nil {
		t.Fatalf("ListMessages second: %v", err)
	}
	if len(legacyHead) != 200 || !reflect.DeepEqual(messageIDsForChangesTest(legacyHead), messageIDsForChangesTest(legacyHeadAgain)) {
		t.Fatalf("timestamp page was not the same stable 200-row head")
	}
}

func TestMessageChangeKindsAndIdempotentUpsert(t *testing.T) {
	db := openTestDB(t)
	chat := "kinds@s.whatsapp.net"
	ts := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	if err := db.UpsertChat(chat, "dm", "Kinds", ts); err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	p := UpsertMessageParams{
		ChatJID:     chat,
		MsgID:       "m1",
		SenderJID:   chat,
		Timestamp:   ts,
		Text:        "one",
		DisplayText: "one",
	}
	if err := db.UpsertMessage(p); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := db.UpsertMessage(p); err != nil {
		t.Fatalf("identical upsert: %v", err)
	}
	p.Text = "two"
	p.DisplayText = "two"
	if err := db.UpsertMessage(p); err != nil {
		t.Fatalf("edit upsert: %v", err)
	}
	if err := db.MarkMessageRevoked(chat, p.MsgID); err != nil {
		t.Fatalf("MarkMessageRevoked: %v", err)
	}
	if err := db.MarkMessageDeletedForMe(chat, p.MsgID, chat, false, ts); err != nil {
		t.Fatalf("MarkMessageDeletedForMe: %v", err)
	}

	page, err := db.ListMessageChanges(0, 20)
	if err != nil {
		t.Fatalf("ListMessageChanges: %v", err)
	}
	got := make([]string, 0, len(page.Changes))
	for _, change := range page.Changes {
		got = append(got, change.Kind)
	}
	want := []string{"insert", "edit", "revoke", "delete"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
}

func TestEveryExportedMessageMutationEmitsItsChangeKind(t *testing.T) {
	db := openTestDB(t)
	chat := "mutations@s.whatsapp.net"
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	if err := db.UpsertChat(chat, "dm", "Mutations", now); err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	for _, id := range []string{"edit", "revoke", "delete", "preserve"} {
		if err := db.UpsertMessage(UpsertMessageParams{ChatJID: chat, MsgID: id, Timestamp: now, Text: id}); err != nil {
			t.Fatalf("UpsertMessage %s: %v", id, err)
		}
	}
	status, err := db.MessageChangesStatus(3600)
	if err != nil {
		t.Fatalf("MessageChangesStatus: %v", err)
	}
	if err := db.UpdateMessageText(chat, "edit", "edited"); err != nil {
		t.Fatalf("UpdateMessageText: %v", err)
	}
	if err := db.MarkMessageRevoked(chat, "revoke"); err != nil {
		t.Fatalf("MarkMessageRevoked: %v", err)
	}
	if err := db.MarkMessageDeletedForMe(chat, "delete", chat, false, now); err != nil {
		t.Fatalf("MarkMessageDeletedForMe: %v", err)
	}
	if err := db.MarkMessageDeletedForMePreserveMedia(chat, "preserve"); err != nil {
		t.Fatalf("MarkMessageDeletedForMePreserveMedia: %v", err)
	}
	page, err := db.ListMessageChanges(status.MaxAllocated, 10)
	if err != nil {
		t.Fatalf("ListMessageChanges: %v", err)
	}
	got := make([]string, 0, len(page.Changes))
	for _, change := range page.Changes {
		got = append(got, change.MsgID+":"+change.Kind)
	}
	want := []string{"edit:edit", "revoke:revoke", "delete:delete", "preserve:delete"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mutation changes = %v, want %v", got, want)
	}
}

func TestMessageChangeOriginsAndDurableUpgrade(t *testing.T) {
	db := openTestDB(t)
	chat := "origin@s.whatsapp.net"
	ts := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	if err := db.UpsertChat(chat, "dm", "Origin", ts); err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	history := UpsertMessageParams{ChatJID: chat, MsgID: "history", SenderJID: chat, Timestamp: ts, Text: "history", Origin: "history"}
	live := UpsertMessageParams{ChatJID: chat, MsgID: "live", SenderJID: chat, Timestamp: ts, Text: "live"}
	if err := db.UpsertMessage(history); err != nil {
		t.Fatalf("history insert: %v", err)
	}
	if err := db.UpsertMessage(live); err != nil {
		t.Fatalf("live insert: %v", err)
	}
	if got := messageIngestOrigin(t, db, chat, history.MsgID); got != "history" {
		t.Fatalf("history ingest origin = %q", got)
	}
	if got := messageIngestOrigin(t, db, chat, live.MsgID); got != "live" {
		t.Fatalf("live ingest origin = %q", got)
	}

	history.Origin = "live"
	if err := db.UpsertMessage(history); err != nil {
		t.Fatalf("live over history: %v", err)
	}
	if got := messageIngestOrigin(t, db, chat, history.MsgID); got != "live" {
		t.Fatalf("upgraded ingest origin = %q", got)
	}
	live.Origin = "history"
	live.Text = "different history payload"
	if err := db.UpsertMessage(live); err != nil {
		t.Fatalf("history over live: %v", err)
	}
	if got := messageIngestOrigin(t, db, chat, live.MsgID); got != "live" {
		t.Fatalf("downgraded ingest origin = %q, want live", got)
	}

	page, err := db.ListMessageChanges(0, 20)
	if err != nil {
		t.Fatalf("ListMessageChanges: %v", err)
	}
	if len(page.Changes) != 3 {
		t.Fatalf("changes = %d, want 3", len(page.Changes))
	}
	got := []string{
		page.Changes[0].Kind + ":" + page.Changes[0].Origin,
		page.Changes[1].Kind + ":" + page.Changes[1].Origin,
		page.Changes[2].Kind + ":" + page.Changes[2].Origin,
	}
	want := []string{"insert:history", "insert:live", "insert:live"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("origin changes = %v, want %v", got, want)
	}
}

func TestOriginUpgradeSurvivesChangePruning(t *testing.T) {
	db := openTestDB(t)
	chat := "prune-origin@s.whatsapp.net"
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	withStoreNow(t, now)
	if err := db.UpsertChat(chat, "dm", "Prune Origin", now); err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	p := UpsertMessageParams{ChatJID: chat, MsgID: "m1", SenderJID: chat, Timestamp: now, Text: "same", Origin: "history"}
	if err := db.UpsertMessage(p); err != nil {
		t.Fatalf("history insert: %v", err)
	}
	if _, err := db.sql.Exec(`UPDATE message_changes SET created_at = ?`, now.AddDate(0, 0, -31).Unix()); err != nil {
		t.Fatalf("age change: %v", err)
	}
	pruned, err := db.PruneMessageChangesOlderThan(30)
	if err != nil || pruned != 1 {
		t.Fatalf("prune = %d, %v", pruned, err)
	}
	p.Origin = "live"
	if err := db.UpsertMessage(p); err != nil {
		t.Fatalf("live upgrade: %v", err)
	}
	if got := messageIngestOrigin(t, db, chat, p.MsgID); got != "live" {
		t.Fatalf("ingest origin = %q, want live", got)
	}
	page, err := db.ListMessageChanges(1, 10)
	if err != nil {
		t.Fatalf("ListMessageChanges: %v", err)
	}
	if len(page.Changes) != 1 || page.Changes[0].Kind != "insert" || page.Changes[0].Origin != "live" {
		t.Fatalf("upgrade changes = %+v", page.Changes)
	}
}

func TestMessageChangeCursorChecksAndBurnTolerance(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.ListMessageChanges(1, 10); !errors.Is(err, ErrChangeCursorFuture) {
		t.Fatalf("empty stale cursor error = %v, want cursor_future", err)
	}
	status, err := db.MessageChangesStatus(3600)
	if err != nil {
		t.Fatalf("empty status: %v", err)
	}
	if status.BootstrapSeq != 0 || status.MaxAllocated != 0 {
		t.Fatalf("empty status = %+v", status)
	}

	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	withStoreNow(t, now)
	chat := "burn@s.whatsapp.net"
	if err := db.UpsertChat(chat, "dm", "Burn", now); err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	if err := db.UpsertMessage(UpsertMessageParams{ChatJID: chat, MsgID: "old", Timestamp: now, Text: "old"}); err != nil {
		t.Fatalf("old insert: %v", err)
	}
	if _, err := db.sql.Exec(`UPDATE message_changes SET created_at = ? WHERE seq = 1`, now.AddDate(0, 0, -31).Unix()); err != nil {
		t.Fatalf("age old change: %v", err)
	}
	if _, err := db.sql.Exec(`UPDATE sqlite_sequence SET seq = 2 WHERE name = 'message_changes'`); err != nil {
		t.Fatalf("burn sequence: %v", err)
	}
	if err := db.UpsertMessage(UpsertMessageParams{ChatJID: chat, MsgID: "recent", Timestamp: now, Text: "recent"}); err != nil {
		t.Fatalf("recent insert: %v", err)
	}
	pruned, err := db.PruneMessageChangesOlderThan(30)
	if err != nil || pruned != 1 {
		t.Fatalf("prune = %d, %v", pruned, err)
	}
	if got := storeMetaInt(t, db, "changes_last_pruned_seq"); got != 1 {
		t.Fatalf("last pruned = %d, want 1", got)
	}
	page, err := db.ListMessageChanges(2, 10)
	if err != nil {
		t.Fatalf("burn cursor should serve: %v", err)
	}
	if len(page.Changes) != 1 || page.Changes[0].Seq != 3 {
		t.Fatalf("burn page = %+v", page.Changes)
	}
	if _, err := db.ListMessageChanges(0, 10); !errors.Is(err, ErrChangeCursorGap) {
		t.Fatalf("gap error = %v, want cursor_gap", err)
	}
	if _, err := db.ListMessageChanges(4, 10); !errors.Is(err, ErrChangeCursorFuture) {
		t.Fatalf("future error = %v, want cursor_future", err)
	}
}

func TestPruneMessageChangesIsAtomicAndZeroPrunePreservesCursor(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.sql.Exec(`UPDATE store_meta SET value = '7' WHERE key = 'changes_last_pruned_seq'`); err != nil {
		t.Fatalf("seed prune cursor: %v", err)
	}
	pruned, err := db.PruneMessageChangesOlderThan(30)
	if err != nil || pruned != 0 {
		t.Fatalf("zero prune = %d, %v", pruned, err)
	}
	if got := storeMetaInt(t, db, "changes_last_pruned_seq"); got != 7 {
		t.Fatalf("zero prune cursor = %d, want 7", got)
	}

	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	withStoreNow(t, now)
	chat := "atomic-prune@s.whatsapp.net"
	if err := db.UpsertChat(chat, "dm", "Atomic", now); err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	if err := db.UpsertMessage(UpsertMessageParams{ChatJID: chat, MsgID: "m1", Timestamp: now, Text: "old"}); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
	if _, err := db.sql.Exec(`UPDATE message_changes SET created_at = ?`, now.AddDate(0, 0, -31).Unix()); err != nil {
		t.Fatalf("age change: %v", err)
	}
	if _, err := db.sql.Exec(`
		CREATE TRIGGER fail_prune_cursor BEFORE UPDATE ON store_meta
		WHEN NEW.key = 'changes_last_pruned_seq'
		BEGIN
			SELECT RAISE(ABORT, 'induced prune failure');
		END
	`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if _, err := db.PruneMessageChangesOlderThan(30); err == nil {
		t.Fatal("expected induced prune failure")
	}
	if got := countRows(t, db.sql, `SELECT COUNT(*) FROM message_changes`); got != 1 {
		t.Fatalf("change rows after failed prune = %d, want 1", got)
	}
	if got := storeMetaInt(t, db, "changes_last_pruned_seq"); got != 7 {
		t.Fatalf("cursor after failed prune = %d, want 7", got)
	}
}

func TestMessageChangesPurgedJoinAndBootstrapMapping(t *testing.T) {
	db := openTestDB(t)
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	withStoreNow(t, now)
	chat := "purged@s.whatsapp.net"
	if err := db.UpsertChat(chat, "dm", "Purged", now); err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	for _, id := range []string{"old", "recent"} {
		if err := db.UpsertMessage(UpsertMessageParams{ChatJID: chat, MsgID: id, Timestamp: now, Text: id}); err != nil {
			t.Fatalf("UpsertMessage %s: %v", id, err)
		}
	}
	if _, err := db.sql.Exec(`UPDATE message_changes SET created_at = ? WHERE msg_id = 'old'`, now.Add(-2*time.Hour).Unix()); err != nil {
		t.Fatalf("age old: %v", err)
	}
	status, err := db.MessageChangesStatus(3600)
	if err != nil {
		t.Fatalf("mixed status: %v", err)
	}
	if status.BootstrapSeq != 1 {
		t.Fatalf("mixed bootstrap = %d, want 1", status.BootstrapSeq)
	}
	if _, err := db.sql.Exec(`UPDATE message_changes SET created_at = ?`, now.Add(-2*time.Hour).Unix()); err != nil {
		t.Fatalf("age all: %v", err)
	}
	status, err = db.MessageChangesStatus(3600)
	if err != nil || status.BootstrapSeq != 2 {
		t.Fatalf("all-old status = %+v, %v", status, err)
	}
	if _, err := db.sql.Exec(`UPDATE message_changes SET created_at = ?`, now.Unix()); err != nil {
		t.Fatalf("make recent: %v", err)
	}
	status, err = db.MessageChangesStatus(3600)
	if err != nil || status.BootstrapSeq != 0 {
		t.Fatalf("all-recent status = %+v, %v", status, err)
	}
	if _, err := db.sql.Exec(`DELETE FROM messages WHERE msg_id = 'old'`); err != nil {
		t.Fatalf("purge message: %v", err)
	}
	page, err := db.ListMessageChanges(0, 10)
	if err != nil {
		t.Fatalf("ListMessageChanges: %v", err)
	}
	if page.Purged != 1 || page.Changes[0].Message != nil || page.Changes[1].Message == nil {
		t.Fatalf("purged page = %+v", page)
	}
}

func TestMessageMutationRollsBackWhenChangeAppendFails(t *testing.T) {
	db := openTestDB(t)
	chat := "atomic@s.whatsapp.net"
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	if err := db.UpsertChat(chat, "dm", "Atomic", now); err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	if err := db.UpsertMessage(UpsertMessageParams{ChatJID: chat, MsgID: "existing", Timestamp: now, Text: "before"}); err != nil {
		t.Fatalf("initial upsert: %v", err)
	}
	if _, err := db.sql.Exec(`
		CREATE TRIGGER fail_change_append BEFORE INSERT ON message_changes
		BEGIN
			SELECT RAISE(ABORT, 'induced change failure');
		END
	`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if err := db.UpdateMessageText(chat, "existing", "after"); err == nil {
		t.Fatal("expected induced update failure")
	}
	msg, err := db.GetMessage(chat, "existing")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if msg.Text != "before" {
		t.Fatalf("text after rollback = %q, want before", msg.Text)
	}
	if got := countRows(t, db.sql, `SELECT COUNT(*) FROM message_changes`); got != 1 {
		t.Fatalf("change rows after rollback = %d, want 1", got)
	}
	if err := db.UpsertMessage(UpsertMessageParams{ChatJID: chat, MsgID: "new", Timestamp: now, Text: "new"}); err == nil {
		t.Fatal("expected induced insert failure")
	}
	if _, err := db.GetMessage(chat, "new"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("new message error = %v, want sql.ErrNoRows", err)
	}
}

func TestMigration21DefaultsOriginAndPersistsInstanceIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wacli.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	legacySchema := strings.Replace(coreSchemaSQL, "    ingest_origin TEXT NOT NULL DEFAULT 'live',\n", "", 1)
	metaStart := strings.Index(legacySchema, "CREATE TABLE IF NOT EXISTS store_meta")
	statusStart := strings.Index(legacySchema, "CREATE TABLE IF NOT EXISTS status_messages")
	if metaStart < 0 || statusStart < 0 || statusStart <= metaStart {
		_ = raw.Close()
		t.Fatal("failed to derive migration-20 schema")
	}
	legacySchema = legacySchema[:metaStart] + legacySchema[statusStart:]
	if _, err := raw.Exec(legacySchema + `
		CREATE TABLE schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at INTEGER NOT NULL
		);
		INSERT INTO chats(jid, kind) VALUES('legacy@s.whatsapp.net', 'dm');
		INSERT INTO messages(chat_jid, msg_id, ts, from_me, text)
		VALUES('legacy@s.whatsapp.net', 'legacy', 1, 0, 'legacy');
	`); err != nil {
		_ = raw.Close()
		t.Fatalf("create legacy schema: %v", err)
	}
	for _, migration := range schemaMigrations {
		if migration.version >= 21 {
			continue
		}
		if _, err := raw.Exec(`INSERT INTO schema_migrations(version, name, applied_at) VALUES(?, ?, 1)`, migration.version, migration.name); err != nil {
			_ = raw.Close()
			t.Fatalf("record migration %d: %v", migration.version, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open migrated: %v", err)
	}
	if got := messageIngestOrigin(t, db, "legacy@s.whatsapp.net", "legacy"); got != "live" {
		t.Fatalf("legacy ingest origin = %q, want live", got)
	}
	firstID := storeMetaString(t, db, "store_instance_id")
	if firstID == "" {
		t.Fatal("empty store instance ID")
	}
	if len(firstID) != 36 || firstID[14] != '4' || !strings.ContainsRune("89ab", rune(firstID[19])) {
		t.Fatalf("store instance ID is not UUIDv4: %q", firstID)
	}
	if got := storeMetaInt(t, db, "changes_last_pruned_seq"); got != 0 {
		t.Fatalf("initial prune cursor = %d, want 0", got)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close migrated: %v", err)
	}

	db, err = Open(path)
	if err != nil {
		t.Fatalf("reopen migrated: %v", err)
	}
	if got := storeMetaString(t, db, "store_instance_id"); got != firstID {
		t.Fatalf("reopened instance ID = %q, want %q", got, firstID)
	}
	_ = db.Close()

	freshPath := filepath.Join(t.TempDir(), "wacli.db")
	fresh, err := Open(freshPath)
	if err != nil {
		t.Fatalf("Open fresh: %v", err)
	}
	defer fresh.Close()
	if got := storeMetaString(t, fresh, "store_instance_id"); got == firstID {
		t.Fatalf("fresh store reused instance ID %q", got)
	}
}

func messageIDsForChangesTest(messages []Message) []string {
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.MsgID)
	}
	return ids
}

func messageIngestOrigin(t *testing.T, db *DB, chatJID, msgID string) string {
	t.Helper()
	var origin string
	if err := db.sql.QueryRow(`SELECT ingest_origin FROM messages WHERE chat_jid = ? AND msg_id = ?`, chatJID, msgID).Scan(&origin); err != nil {
		t.Fatalf("read ingest origin: %v", err)
	}
	return origin
}

func storeMetaString(t *testing.T, db *DB, key string) string {
	t.Helper()
	var value string
	if err := db.sql.QueryRow(`SELECT value FROM store_meta WHERE key = ?`, key).Scan(&value); err != nil {
		t.Fatalf("read store meta: %v", err)
	}
	return value
}

func storeMetaInt(t *testing.T, db *DB, key string) int64 {
	t.Helper()
	var value int64
	if err := db.sql.QueryRow(`SELECT CAST(value AS INTEGER) FROM store_meta WHERE key = ?`, key).Scan(&value); err != nil {
		t.Fatalf("read integer store meta: %v", err)
	}
	return value
}

func withStoreNow(t *testing.T, now time.Time) {
	t.Helper()
	previous := nowUTC
	nowUTC = func() time.Time { return now }
	t.Cleanup(func() { nowUTC = previous })
}

func TestLiveRevokeOverHistoryRowEmitsRevokeNotForwardableInsert(t *testing.T) {
	db := openTestDB(t)
	chat := "revoke-upgrade@s.whatsapp.net"
	ts := time.Date(2026, 7, 17, 13, 0, 0, 0, time.UTC)
	if err := db.UpsertChat(chat, "dm", "RevokeUpgrade", ts); err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	msg := UpsertMessageParams{ChatJID: chat, MsgID: "m1", SenderJID: chat, Timestamp: ts, Text: "hello", Origin: "history"}
	if err := db.UpsertMessage(msg); err != nil {
		t.Fatalf("history insert: %v", err)
	}
	// The live revoke arrives through the same upsert path sync uses
	// (Origin zero-value = live, Revoked = true).
	revoke := UpsertMessageParams{ChatJID: chat, MsgID: "m1", SenderJID: chat, Timestamp: ts, Revoked: true}
	if err := db.UpsertMessage(revoke); err != nil {
		t.Fatalf("live revoke: %v", err)
	}
	if got := messageIngestOrigin(t, db, chat, "m1"); got != "live" {
		t.Fatalf("ingest origin after live revoke = %q, want live", got)
	}
	page, err := db.ListMessageChanges(0, 10)
	if err != nil {
		t.Fatalf("ListMessageChanges: %v", err)
	}
	if len(page.Changes) != 2 {
		t.Fatalf("changes = %d, want 2", len(page.Changes))
	}
	last := page.Changes[1]
	if last.Kind != "revoke" {
		t.Fatalf("live revoke over history row emitted kind=%q origin=%q, want revoke (a forwardable insert here would push a blank tombstone to the consumer)", last.Kind, last.Origin)
	}
}

func TestDeletedForMeTombstoneFallbackEmitsNoChange(t *testing.T) {
	db := openTestDB(t)
	chat := "tombstone@s.whatsapp.net"
	ts := time.Date(2026, 7, 17, 13, 30, 0, 0, time.UTC)
	if err := db.UpsertChat(chat, "dm", "Tombstone", ts); err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	// Delete-for-me for a message the store never held: the fallback inserts a
	// contentless tombstone row and must deliberately emit NO change row.
	if err := db.MarkMessageDeletedForMe(chat, "never-seen", chat, false, ts); err != nil {
		t.Fatalf("MarkMessageDeletedForMe fallback: %v", err)
	}
	if _, err := db.GetMessage(chat, "never-seen"); err != nil {
		t.Fatalf("tombstone row missing: %v", err)
	}
	page, err := db.ListMessageChanges(0, 10)
	if err != nil {
		t.Fatalf("ListMessageChanges: %v", err)
	}
	if len(page.Changes) != 0 {
		t.Fatalf("tombstone fallback emitted %d change(s), want 0", len(page.Changes))
	}
}
