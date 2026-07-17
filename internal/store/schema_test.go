package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenCreatesExpectedSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wacli.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	cols, err := tableColumns(db.sql, "messages")
	if err != nil {
		t.Fatalf("tableColumns: %v", err)
	}

	for _, want := range []string{
		"chat_name",
		"sender_name",
		"display_text",
		"quoted_msg_id",
		"quoted_sender_jid",
		"is_forwarded",
		"forwarding_score",
		"reaction_to_id",
		"reaction_emoji",
		"local_path",
		"downloaded_at",
		"media_unavailable_at",
		"revoked",
		"deleted_for_me",
		"edited",
		"edited_ts",
	} {
		if !cols[want] {
			t.Fatalf("expected messages column %q to exist", want)
		}
	}

	callCols, err := tableColumns(db.sql, "call_events")
	if err != nil {
		t.Fatalf("call_events tableColumns: %v", err)
	}
	for _, want := range []string{"chat_jid", "call_id", "event_type", "direction", "media", "outcome", "duration_secs", "participants"} {
		if !callCols[want] {
			t.Fatalf("expected call_events column %q to exist", want)
		}
	}
	if !indexExists(t, db.sql, "idx_call_events_chat_ts") {
		t.Fatalf("expected call_events chat index to exist")
	}

	starredCols, err := tableColumns(db.sql, "starred")
	if err != nil {
		t.Fatalf("starred tableColumns: %v", err)
	}
	for _, want := range []string{"chat_jid", "msg_id", "sender_jid", "from_me", "starred_at"} {
		if !starredCols[want] {
			t.Fatalf("expected starred column %q to exist", want)
		}
	}

	groupCols, err := tableColumns(db.sql, "groups")
	if err != nil {
		t.Fatalf("groups tableColumns: %v", err)
	}
	for _, want := range []string{"is_parent", "linked_parent_jid"} {
		if !groupCols[want] {
			t.Fatalf("expected groups column %q to exist", want)
		}
	}
	if !indexExists(t, db.sql, "idx_groups_linked_parent_jid") {
		t.Fatalf("expected linked-parent group index to exist")
	}

	contactCols, err := tableColumns(db.sql, "contacts")
	if err != nil {
		t.Fatalf("contacts tableColumns: %v", err)
	}
	if !contactCols["system_name"] {
		t.Fatalf("expected contacts system_name column to exist")
	}

	chatCols, err := tableColumns(db.sql, "chats")
	if err != nil {
		t.Fatalf("chats tableColumns: %v", err)
	}
	if !chatCols["unread_count"] {
		t.Fatalf("expected chats unread_count column to exist")
	}

	statusCols, err := tableColumns(db.sql, "status_messages")
	if err != nil {
		t.Fatalf("status_messages tableColumns: %v", err)
	}
	for _, want := range []string{"msg_id", "ts", "from_me", "sender_jid", "media_key", "background_color", "font"} {
		if !statusCols[want] {
			t.Fatalf("expected status_messages column %q to exist", want)
		}
	}
	if !indexExists(t, db.sql, "idx_status_messages_ts") {
		t.Fatalf("expected status_messages timestamp index to exist")
	}
}

func TestFreshStoreRunsMigrationsThrough22AndMatchesEmbeddedMessageSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wacli.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open fresh: %v", err)
	}
	defer db.Close()

	rows, err := db.sql.Query(`SELECT version, name FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("read schema migrations: %v", err)
	}
	defer rows.Close()
	for index, want := range schemaMigrations {
		if !rows.Next() {
			t.Fatalf("schema migrations stopped at %d, want migration %d", index, want.version)
		}
		var version int
		var name string
		if err := rows.Scan(&version, &name); err != nil {
			t.Fatalf("scan schema migration: %v", err)
		}
		if version != want.version || name != want.name {
			t.Fatalf("schema migration %d = (%d, %q), want (%d, %q)", index, version, name, want.version, want.name)
		}
	}
	if rows.Next() {
		t.Fatal("fresh store recorded an unexpected migration after 22")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate schema migrations: %v", err)
	}

	reference, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "schema-reference.db"))
	if err != nil {
		t.Fatalf("open schema reference: %v", err)
	}
	defer reference.Close()
	if _, err := reference.Exec(coreSchemaSQL); err != nil {
		t.Fatalf("apply embedded schema: %v", err)
	}
	schemaRows, err := reference.Query(`
		SELECT type, name, sql
		FROM sqlite_schema
		WHERE type IN ('table', 'index') AND sql IS NOT NULL
		ORDER BY type, name
	`)
	if err != nil {
		t.Fatalf("read embedded schema objects: %v", err)
	}
	for schemaRows.Next() {
		var objectType string
		var name string
		var wantSQL string
		if err := schemaRows.Scan(&objectType, &name, &wantSQL); err != nil {
			_ = schemaRows.Close()
			t.Fatalf("scan embedded schema object: %v", err)
		}
		var gotSQL string
		if err := db.sql.QueryRow(`SELECT sql FROM sqlite_schema WHERE type = ? AND name = ?`, objectType, name).Scan(&gotSQL); err != nil {
			_ = schemaRows.Close()
			t.Fatalf("read fresh schema object %s %s: %v", objectType, name, err)
		}
		if gotSQL != wantSQL {
			_ = schemaRows.Close()
			t.Fatalf("fresh schema object %s %s does not match schema.sql", objectType, name)
		}
	}
	if err := schemaRows.Err(); err != nil {
		_ = schemaRows.Close()
		t.Fatalf("iterate embedded schema objects: %v", err)
	}
	if err := schemaRows.Close(); err != nil {
		t.Fatalf("close embedded schema objects: %v", err)
	}
	if got := countRows(t, db.sql, `
		SELECT COUNT(*)
		FROM pragma_table_info('messages')
		WHERE name IN ('mentions_me', 'replies_to_me')
		  AND "notnull" = 0
		  AND dflt_value IS NULL
	`); got != 2 {
		t.Fatalf("nullable provider-addressing columns without defaults = %d, want 2", got)
	}
}

func TestMigration22UpgradesMigration21StoreWithoutFabricatingFalse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wacli.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	migration21Schema := strings.Replace(coreSchemaSQL, "    mentions_me INTEGER,\n", "", 1)
	migration21Schema = strings.Replace(migration21Schema, "    replies_to_me INTEGER,\n", "", 1)
	if _, err := raw.Exec(migration21Schema + `
		CREATE TABLE schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at INTEGER NOT NULL
		);
		INSERT INTO chats(jid, kind) VALUES('legacy@s.whatsapp.net', 'dm');
		INSERT INTO messages(chat_jid, msg_id, ts, from_me, text)
		VALUES('legacy@s.whatsapp.net', 'pre-provider-addressing', 1, 0, 'legacy');
	`); err != nil {
		_ = raw.Close()
		t.Fatalf("create migration-21 store: %v", err)
	}
	for _, migration := range schemaMigrations {
		if migration.version > 21 {
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
		t.Fatalf("Open migration-21 store: %v", err)
	}
	defer db.Close()
	for _, column := range []string{"mentions_me", "replies_to_me"} {
		has, err := db.tableHasColumn("messages", column)
		if err != nil {
			t.Fatalf("tableHasColumn(%s): %v", column, err)
		}
		if !has {
			t.Fatalf("migration 22 did not add messages.%s", column)
		}
	}
	if got := countRows(t, db.sql, `SELECT COUNT(*) FROM schema_migrations WHERE version = 22`); got != 1 {
		t.Fatalf("migration 22 records = %d, want 1", got)
	}
	var mentionsMe sql.NullInt64
	var repliesToMe sql.NullInt64
	if err := db.sql.QueryRow(`
		SELECT mentions_me, replies_to_me
		FROM messages
		WHERE msg_id = 'pre-provider-addressing'
	`).Scan(&mentionsMe, &repliesToMe); err != nil {
		t.Fatalf("read migrated provider addressing: %v", err)
	}
	if mentionsMe.Valid || repliesToMe.Valid {
		t.Fatalf("migration 22 fabricated provider addressing values: mentions valid=%v replies valid=%v", mentionsMe.Valid, repliesToMe.Valid)
	}
}

func TestTableHasColumnRejectsUnsafeIdentifier(t *testing.T) {
	db := openTestDB(t)

	if _, err := db.tableHasColumn(`messages); DROP TABLE messages; --`, "msg_id"); err == nil {
		t.Fatalf("expected unsafe table identifier to be rejected")
	}
	if _, err := db.tableHasColumn("messages", `msg_id); DROP TABLE messages; --`); err == nil {
		t.Fatalf("expected unsafe column identifier to be rejected")
	}

	if got := countRows(t, db.sql, "SELECT COUNT(*) FROM messages"); got != 0 {
		t.Fatalf("messages table was unexpectedly modified, row count = %d", got)
	}
}

func TestTableHasColumnAllowsSchemaIdentifiers(t *testing.T) {
	db := openTestDB(t)

	hasColumn, err := db.tableHasColumn("messages", "display_text")
	if err != nil {
		t.Fatalf("tableHasColumn: %v", err)
	}
	if !hasColumn {
		t.Fatalf("expected messages.display_text to exist")
	}
}

func TestOpenRepairsRecordedMediaUnavailableMigrationMissingColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wacli.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	legacySchema := strings.Replace(coreSchemaSQL, "    media_unavailable_at INTEGER,\n", "", 1)
	if _, err := raw.Exec(legacySchema + `
		CREATE TABLE schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at INTEGER NOT NULL
		);
	`); err != nil {
		_ = raw.Close()
		t.Fatalf("create legacy schema: %v", err)
	}
	for _, migration := range schemaMigrations {
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
		t.Fatalf("Open repaired DB: %v", err)
	}
	defer db.Close()
	hasColumn, err := db.tableHasColumn("messages", "media_unavailable_at")
	if err != nil {
		t.Fatalf("tableHasColumn: %v", err)
	}
	if !hasColumn {
		t.Fatalf("expected media_unavailable_at repair")
	}
}

func TestOpenMigratesLegacyUnreadCounts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wacli.db")

	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at INTEGER NOT NULL
		);
		CREATE TABLE chats (
			jid TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			name TEXT,
			last_message_ts INTEGER,
			archived INTEGER NOT NULL DEFAULT 0,
			pinned INTEGER NOT NULL DEFAULT 0,
			muted_until INTEGER NOT NULL DEFAULT 0,
			unread INTEGER NOT NULL DEFAULT 0
		);
		INSERT INTO chats(jid, kind, unread) VALUES
			('counted@s.whatsapp.net', 'dm', 4),
			('marker@s.whatsapp.net', 'dm', -1),
			('read@s.whatsapp.net', 'dm', 0);
	`); err != nil {
		_ = raw.Close()
		t.Fatalf("create old schema: %v", err)
	}
	for _, m := range schemaMigrations {
		if m.version >= 18 {
			continue
		}
		if _, err := raw.Exec(`INSERT INTO schema_migrations(version, name, applied_at) VALUES(?, ?, 1)`, m.version, m.name); err != nil {
			_ = raw.Close()
			t.Fatalf("mark migration %d: %v", m.version, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open migrated DB: %v", err)
	}
	defer db.Close()

	counted, err := db.GetChat("counted@s.whatsapp.net")
	if err != nil {
		t.Fatalf("GetChat counted: %v", err)
	}
	if !counted.Unread || counted.UnreadCount != 4 {
		t.Fatalf("counted unread = %+v, want unread count 4", counted)
	}
	marker, err := db.GetChat("marker@s.whatsapp.net")
	if err != nil {
		t.Fatalf("GetChat marker: %v", err)
	}
	if !marker.Unread || marker.UnreadCount != 0 {
		t.Fatalf("marker unread = %+v, want unread marker only", marker)
	}
	read, err := db.GetChat("read@s.whatsapp.net")
	if err != nil {
		t.Fatalf("GetChat read: %v", err)
	}
	if read.Unread || read.UnreadCount != 0 {
		t.Fatalf("read unread = %+v, want read count 0", read)
	}
}

func TestOpenRepairsRecordedCallEventsMigrationMissingTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wacli.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`
		DROP TABLE call_events;
		INSERT OR IGNORE INTO schema_migrations(version, name, applied_at) VALUES(14, 'call events', 1);
	`); err != nil {
		_ = raw.Close()
		t.Fatalf("create inconsistent schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	db, err = Open(path)
	if err != nil {
		t.Fatalf("Open repaired DB: %v", err)
	}
	defer db.Close()

	if ok, err := db.tableExists("call_events"); err != nil || !ok {
		t.Fatalf("call_events exists=%v err=%v", ok, err)
	}
	if !indexExists(t, db.sql, "idx_call_events_chat_ts") {
		t.Fatalf("expected call_events chat index to be recreated")
	}
	if _, err := db.ListCallEvents(ListCallEventsParams{Limit: 1}); err != nil {
		t.Fatalf("ListCallEvents after schema repair: %v", err)
	}
}

func TestOpenMigratesGroupHierarchyColumns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wacli.db")

	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at INTEGER NOT NULL
		);
		CREATE TABLE groups (
			jid TEXT PRIMARY KEY,
			name TEXT,
			owner_jid TEXT,
			created_ts INTEGER,
			left_at INTEGER,
			updated_at INTEGER NOT NULL
		);
		INSERT INTO groups(jid, name, updated_at) VALUES('g@g.us', 'Old', 1);
	`); err != nil {
		_ = raw.Close()
		t.Fatalf("create old schema: %v", err)
	}
	for _, m := range schemaMigrations {
		if m.version >= 11 {
			continue
		}
		if _, err := raw.Exec(`INSERT INTO schema_migrations(version, name, applied_at) VALUES(?, ?, 1)`, m.version, m.name); err != nil {
			_ = raw.Close()
			t.Fatalf("mark migration %d: %v", m.version, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open migrated DB: %v", err)
	}
	defer db.Close()

	groupCols, err := tableColumns(db.sql, "groups")
	if err != nil {
		t.Fatalf("groups tableColumns: %v", err)
	}
	for _, want := range []string{"is_parent", "linked_parent_jid"} {
		if !groupCols[want] {
			t.Fatalf("expected migrated groups column %q to exist", want)
		}
	}
	if !indexExists(t, db.sql, "idx_groups_linked_parent_jid") {
		t.Fatalf("expected migrated linked-parent group index to exist")
	}
}

func TestOpenMigratesLegacyGroupsWithoutMigrationTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wacli.db")

	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE groups (
			jid TEXT PRIMARY KEY,
			name TEXT,
			owner_jid TEXT,
			created_ts INTEGER,
			left_at INTEGER,
			updated_at INTEGER NOT NULL
		);
		INSERT INTO groups(jid, name, updated_at) VALUES('g@g.us', 'Old', 1);
	`); err != nil {
		_ = raw.Close()
		t.Fatalf("create legacy schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy DB: %v", err)
	}
	defer db.Close()

	groupCols, err := tableColumns(db.sql, "groups")
	if err != nil {
		t.Fatalf("groups tableColumns: %v", err)
	}
	for _, want := range []string{"is_parent", "linked_parent_jid"} {
		if !groupCols[want] {
			t.Fatalf("expected migrated groups column %q to exist", want)
		}
	}
	if !indexExists(t, db.sql, "idx_groups_linked_parent_jid") {
		t.Fatalf("expected migrated linked-parent group index to exist")
	}
}

func TestOpenMigratesContactsSystemNameColumn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wacli.db")

	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at INTEGER NOT NULL
		);
		CREATE TABLE contacts (
			jid TEXT PRIMARY KEY,
			phone TEXT,
			push_name TEXT,
			full_name TEXT,
			first_name TEXT,
			business_name TEXT,
			updated_at INTEGER NOT NULL
		);
		INSERT INTO contacts(jid, phone, updated_at) VALUES('111@s.whatsapp.net', '111', 1);
	`); err != nil {
		_ = raw.Close()
		t.Fatalf("create old contacts schema: %v", err)
	}
	for _, m := range schemaMigrations {
		if m.version >= 12 {
			continue
		}
		if _, err := raw.Exec(`INSERT INTO schema_migrations(version, name, applied_at) VALUES(?, ?, 1)`, m.version, m.name); err != nil {
			_ = raw.Close()
			t.Fatalf("mark migration %d: %v", m.version, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open migrated DB: %v", err)
	}
	defer db.Close()

	contactCols, err := tableColumns(db.sql, "contacts")
	if err != nil {
		t.Fatalf("contacts tableColumns: %v", err)
	}
	if !contactCols["system_name"] {
		t.Fatalf("expected migrated contacts system_name column")
	}
}

func TestOpenMigratesStatusMessagesTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wacli.db")

	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at INTEGER NOT NULL
		);
	`); err != nil {
		_ = raw.Close()
		t.Fatalf("create old schema: %v", err)
	}
	for _, m := range schemaMigrations {
		if m.version >= 17 {
			continue
		}
		if _, err := raw.Exec(`INSERT INTO schema_migrations(version, name, applied_at) VALUES(?, ?, 1)`, m.version, m.name); err != nil {
			_ = raw.Close()
			t.Fatalf("mark migration %d: %v", m.version, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open migrated DB: %v", err)
	}
	defer db.Close()

	if ok, err := db.tableExists("status_messages"); err != nil || !ok {
		t.Fatalf("status_messages exists=%v err=%v", ok, err)
	}
	if !indexExists(t, db.sql, "idx_status_messages_ts") {
		t.Fatalf("expected status_messages timestamp index to exist")
	}
	if err := db.UpsertStatusMessage(UpsertStatusMessageParams{
		MsgID:     "status-after-upgrade",
		Timestamp: nowUTC(),
		FromMe:    true,
		Text:      "after upgrade",
	}); err != nil {
		t.Fatalf("UpsertStatusMessage after migration: %v", err)
	}
}

func tableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name string
		var colType string
		var notNull int
		var pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[strings.ToLower(name)] = true
	}
	return cols, rows.Err()
}

func indexExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var found string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		t.Fatalf("query index %q: %v", name, err)
	}
	return found == name
}

// TestOld927StoreUpgradeGainsMessageChangesMachinery pins the highest-risk
// merge seam: the DEPLOYED 0.11.2-wave.927 staging store recorded version 21
// with the OLD branch's semantics ("messages provider addressing columns"), so
// this merged binary's version loop SKIPS 21 ("message changes") on such a
// store. Correctness rides entirely on ensureCurrentSchema unconditionally
// re-running the idempotent migrateMessageChanges; if that safety net is ever
// removed, a 927-deployed store silently ships without the change stream —
// this test is what fails first.
func TestOld927StoreUpgradeGainsMessageChangesMachinery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wacli.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// Old-927 shape: mentions columns PRESENT; message-changes machinery
	// (messages.ingest_origin, store_meta, message_changes) ABSENT.
	old927Schema := strings.Replace(
		coreSchemaSQL, "    ingest_origin TEXT NOT NULL DEFAULT 'live',\n", "", 1,
	)
	old927Schema = dropSchemaStatement(t, old927Schema, "CREATE TABLE IF NOT EXISTS store_meta")
	old927Schema = dropSchemaStatement(t, old927Schema, "CREATE TABLE IF NOT EXISTS message_changes")
	old927Schema = dropSchemaStatement(t, old927Schema, "CREATE INDEX IF NOT EXISTS idx_message_changes_created_at")
	if _, err := raw.Exec(old927Schema + `
		CREATE TABLE schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at INTEGER NOT NULL
		);
		INSERT INTO chats(jid, kind) VALUES('legacy@s.whatsapp.net', 'dm');
		INSERT INTO messages(chat_jid, msg_id, ts, from_me, text, mentions_me)
		VALUES('legacy@s.whatsapp.net', 'pre-938', 1, 0, 'legacy', NULL);
	`); err != nil {
		_ = raw.Close()
		t.Fatalf("create old-927 store: %v", err)
	}
	// The old branch recorded 1..21 where 21 was "messages provider addressing
	// columns" — reproduce that record verbatim.
	for _, migration := range schemaMigrations {
		if migration.version > 20 {
			continue
		}
		if _, err := raw.Exec(`INSERT INTO schema_migrations(version, name, applied_at) VALUES(?, ?, 1)`, migration.version, migration.name); err != nil {
			_ = raw.Close()
			t.Fatalf("record migration %d: %v", migration.version, err)
		}
	}
	if _, err := raw.Exec(`INSERT INTO schema_migrations(version, name, applied_at) VALUES(21, 'messages provider addressing columns', 1)`); err != nil {
		_ = raw.Close()
		t.Fatalf("record old-927 migration 21: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open old-927 store: %v", err)
	}
	defer db.Close()
	for _, table := range []string{"store_meta", "message_changes"} {
		has, err := db.tableExists(table)
		if err != nil {
			t.Fatalf("tableExists(%s): %v", table, err)
		}
		if !has {
			t.Fatalf("old-927 upgrade left %s missing — the change stream is dead on staging-shaped stores", table)
		}
	}
	hasOrigin, err := db.tableHasColumn("messages", "ingest_origin")
	if err != nil {
		t.Fatalf("tableHasColumn(ingest_origin): %v", err)
	}
	if !hasOrigin {
		t.Fatal("old-927 upgrade did not add messages.ingest_origin")
	}
	var origin string
	if err := db.sql.QueryRow(`SELECT ingest_origin FROM messages WHERE msg_id = 'pre-938'`).Scan(&origin); err != nil {
		t.Fatalf("read legacy ingest_origin: %v", err)
	}
	if origin != "live" {
		t.Fatalf("legacy row ingest_origin = %q, want live", origin)
	}
	var mentions sql.NullInt64
	if err := db.sql.QueryRow(`SELECT mentions_me FROM messages WHERE msg_id = 'pre-938'`).Scan(&mentions); err != nil {
		t.Fatalf("read legacy mentions_me: %v", err)
	}
	if mentions.Valid {
		t.Fatalf("legacy mentions_me became %d, want NULL (never fabricate a verdict)", mentions.Int64)
	}
	var instance string
	if err := db.sql.QueryRow(`SELECT value FROM store_meta WHERE key = 'store_instance_id'`).Scan(&instance); err != nil {
		t.Fatalf("store_instance_id missing after old-927 upgrade: %v", err)
	}
	if instance == "" {
		t.Fatal("store_instance_id empty after old-927 upgrade")
	}
}

// dropSchemaStatement removes one statement (matched by its opening line) from
// the canonical schema, failing loudly if the marker is not found so schema
// drift cannot silently turn this reconstruction into a no-op.
func dropSchemaStatement(t *testing.T, schema, marker string) string {
	t.Helper()
	start := strings.Index(schema, marker)
	if start < 0 {
		t.Fatalf("schema marker %q not found", marker)
	}
	end := strings.Index(schema[start:], ";")
	if end < 0 {
		t.Fatalf("schema statement for %q not terminated", marker)
	}
	return schema[:start] + schema[start+end+1:]
}
