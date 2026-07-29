package store

import (
	"testing"
	"time"
)

// The local participants read exists because `groups info` — the only command
// that reports membership — needs the store write lock, which a long-running
// `sync --follow` holds for its whole lifetime. These cover the read itself;
// that it takes no lock is a property of the command that calls it.
// openTestDB comes from store_test.go.

func seedGroup(t *testing.T, db *DB, jid, name string, participants ...GroupParticipant) {
	t.Helper()
	if err := db.UpsertGroupWithHierarchy(jid, name, "", time.Now(), false, ""); err != nil {
		t.Fatalf("UpsertGroupWithHierarchy: %v", err)
	}
	if len(participants) > 0 {
		if err := db.ReplaceGroupParticipants(jid, participants); err != nil {
			t.Fatalf("ReplaceGroupParticipants: %v", err)
		}
	}
}

func TestListGroupParticipantsReturnsStoredMembers(t *testing.T) {
	db := openTestDB(t)
	const jid = "120363000000000002@g.us"
	seedGroup(t, db, jid, "Weekend Plans",
		GroupParticipant{GroupJID: jid, UserJID: "15550000001@s.whatsapp.net", Role: "admin"},
		GroupParticipant{GroupJID: jid, UserJID: "15550000002@s.whatsapp.net", Role: "member"},
	)

	got, err := db.ListGroupParticipants(jid)
	if err != nil {
		t.Fatalf("ListGroupParticipants: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 participants, got %d (%+v)", len(got), got)
	}
	for _, p := range got {
		if p.GroupJID != jid {
			t.Fatalf("participant carries the wrong group: %+v", p)
		}
		if p.UpdatedAt.IsZero() {
			t.Fatalf("participant lost its freshness stamp: %+v", p)
		}
	}
}

// The caller uses this to decide whether the cache is fresh enough to act on, so
// losing it would silently turn "stale" into "unknown".
func TestListGroupParticipantsPreservesTheFreshnessStamp(t *testing.T) {
	db := openTestDB(t)
	const jid = "120363000000000002@g.us"
	before := time.Now().Add(-time.Second)
	seedGroup(t, db, jid, "Weekend Plans",
		GroupParticipant{GroupJID: jid, UserJID: "15550000001@s.whatsapp.net", Role: "member"},
	)

	got, err := db.ListGroupParticipants(jid)
	if err != nil {
		t.Fatalf("ListGroupParticipants: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 participant, got %d", len(got))
	}
	if got[0].UpdatedAt.Before(before) {
		t.Fatalf("stamp is older than the write: %v", got[0].UpdatedAt)
	}
}

// An unknown group must be empty rather than an error, and must never bleed
// another group's members — the roster is authorization input downstream.
func TestListGroupParticipantsIsScopedToOneGroup(t *testing.T) {
	db := openTestDB(t)
	const mine = "120363000000000002@g.us"
	const other = "120363000000000009@g.us"
	seedGroup(t, db, mine, "Weekend Plans",
		GroupParticipant{GroupJID: mine, UserJID: "15550000001@s.whatsapp.net", Role: "member"},
	)
	seedGroup(t, db, other, "Other",
		GroupParticipant{GroupJID: other, UserJID: "15550000009@s.whatsapp.net", Role: "member"},
	)

	got, err := db.ListGroupParticipants(mine)
	if err != nil {
		t.Fatalf("ListGroupParticipants: %v", err)
	}
	if len(got) != 1 || got[0].UserJID != "15550000001@s.whatsapp.net" {
		t.Fatalf("scope leak: %+v", got)
	}

	empty, err := db.ListGroupParticipants("120363000000000404@g.us")
	if err != nil {
		t.Fatalf("unknown group should not error: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("unknown group returned members: %+v", empty)
	}
}

func TestListGroupParticipantsRejectsABlankJID(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.ListGroupParticipants("   "); err == nil {
		t.Fatal("a blank JID must be refused, not treated as a wildcard")
	}
}

// GetGroup is an EXACT lookup on purpose. ListGroups matches with LIKE, so a JID
// is a substring pattern there and a numeric stem could name the wrong group.
func TestGetGroupMatchesExactlyAndNotByStem(t *testing.T) {
	db := openTestDB(t)
	const full = "120363000000000002@g.us"
	const longer = "1203630000000000021@g.us"
	seedGroup(t, db, full, "Weekend Plans")
	seedGroup(t, db, longer, "Different Group")

	got, err := db.GetGroup(full)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if got == nil || got.JID != full || got.Name != "Weekend Plans" {
		t.Fatalf("exact lookup returned the wrong row: %+v", got)
	}

	missing, err := db.GetGroup("120363000000000404@g.us")
	if err != nil {
		t.Fatalf("unknown group should not error: %v", err)
	}
	if missing != nil {
		t.Fatalf("unknown group returned a row: %+v", missing)
	}
}

// A group you have left is still a group you may need to name, so the exact
// lookup must not inherit ListGroups' left_at filter.
func TestGetGroupStillFindsALeftGroup(t *testing.T) {
	db := openTestDB(t)
	const jid = "120363000000000002@g.us"
	seedGroup(t, db, jid, "Weekend Plans")
	if err := db.MarkGroupLeft(jid, time.Now()); err != nil {
		t.Fatalf("MarkGroupLeft: %v", err)
	}

	got, err := db.GetGroup(jid)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if got == nil {
		t.Fatal("a left group must still be findable by exact JID")
	}

	listed, err := db.ListGroups(jid, 10)
	if err != nil {
		t.Fatalf("ListGroups: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("ListGroups is expected to hide left groups; got %+v", listed)
	}
}
