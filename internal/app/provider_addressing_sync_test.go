package app

import (
	"context"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// AITOOLS-927 end-to-end through the REAL sync path: a message arrives, we derive MentionsMe /
// RepliesToMe, and they land in the store. Nothing is mocked between parse and persist.

const (
	e2eOwnPN  = "1234567890@s.whatsapp.net" // what fakeWA reports as LinkedJID
	e2eOwnLID = "99887766554433@lid"
	e2eGroup  = "120363000000000000@g.us"
	e2eOther  = "15559990000@s.whatsapp.net"
)

func newAddressingApp(t *testing.T) *App {
	t.Helper()
	a := newTestApp(t)
	f := newFakeWA()
	f.linkedLID = e2eOwnLID // the account knows BOTH of its identities
	a.wa = f
	if err := a.db.UpsertChat(e2eGroup, "group", "Test Group", time.Now()); err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	return a
}

func storeMsg(t *testing.T, a *App, id string) store.Message {
	t.Helper()
	msgs, err := a.db.ListMessages(store.ListMessagesParams{Limit: 50})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	for _, m := range msgs {
		if m.MsgID == id {
			return m
		}
	}
	t.Fatalf("message %q not found in store", id)
	return store.Message{}
}

func triS(v *bool) string {
	if v == nil {
		return "null"
	}
	if *v {
		return "true"
	}
	return "false"
}

func inbound(id string, ctxInfo *waProto.ContextInfo) wa.ParsedMessage {
	chat, _ := types.ParseJID(e2eGroup)
	return wa.ParsedMessage{
		Chat:      chat,
		ID:        id,
		SenderJID: e2eOther,
		Timestamp: time.Now().UTC().Truncate(time.Second),
		Text:      "hey",
		Context:   ctxInfo,
	}
}

// THE BUG, END TO END: a native group mention carries our LID, the text is unmatchable, and before
// this change nothing recorded that Wave was addressed.
func TestSyncPersistsNativeMention(t *testing.T) {
	a := newAddressingApp(t)
	pm := inbound("M-MENTION", &waProto.ContextInfo{MentionedJID: []string{e2eOwnLID}})
	pm.Text = "@99887766554433 what's my day look like" // the wire text: a LID, not "@wave"

	if err := a.storeParsedMessage(context.Background(), pm); err != nil {
		t.Fatalf("storeParsedMessage: %v", err)
	}
	got := storeMsg(t, a, "M-MENTION")
	if got.MentionsMe == nil || !*got.MentionsMe {
		t.Errorf("MentionsMe = %s, want true (the mention carries our LID)", triS(got.MentionsMe))
	}
}

// A proven reply: we actually sent the quoted message, and our own store says so.
func TestSyncPersistsProvenReply(t *testing.T) {
	a := newAddressingApp(t)

	// Wave's own outbound message, recorded locally (from_me=1). THIS is the proof.
	if err := a.db.UpsertMessage(store.UpsertMessageParams{
		ChatJID: e2eGroup, MsgID: "WAVE-MSG", SenderJID: e2eOwnPN,
		Timestamp: time.Now().UTC().Add(-time.Minute), FromMe: true, Text: "here are 3 slots",
	}); err != nil {
		t.Fatalf("seed outbound: %v", err)
	}

	pm := inbound("M-REPLY", &waProto.ContextInfo{
		StanzaID:    proto.String("WAVE-MSG"),
		Participant: proto.String(e2eOwnPN),
	})
	pm.ReplyToID = "WAVE-MSG"
	pm.Text = "yes, book the second one" // no tag at all

	if err := a.storeParsedMessage(context.Background(), pm); err != nil {
		t.Fatalf("storeParsedMessage: %v", err)
	}
	got := storeMsg(t, a, "M-REPLY")
	if got.RepliesToMe == nil || !*got.RepliesToMe {
		t.Errorf("RepliesToMe = %s, want true (we hold local proof we sent the quoted message)", triS(got.RepliesToMe))
	}
}

// 🔴 THE SPOOF. A message FORGES reply metadata claiming to quote Wave, but we never sent that
// message — there is no local record. This must NOT become true, or anyone could make Wave reply into
// a group on demand by fabricating a stanza id.
func TestSyncRefusesForgedReply(t *testing.T) {
	a := newAddressingApp(t)

	pm := inbound("M-FORGED", &waProto.ContextInfo{
		StanzaID:    proto.String("NEVER-SENT-BY-US"),
		Participant: proto.String(e2eOwnLID), // claims Wave authored it
	})
	pm.ReplyToID = "NEVER-SENT-BY-US"

	if err := a.storeParsedMessage(context.Background(), pm); err != nil {
		t.Fatalf("storeParsedMessage: %v", err)
	}
	got := storeMsg(t, a, "M-FORGED")
	if got.RepliesToMe != nil {
		t.Errorf(
			"RepliesToMe = %s on a FORGED reply with no local proof; want null (quarantine). "+
				"A true here is a remote-triggered wake: anyone could forge a stanza id and make Wave "+
				"reply into a group.",
			triS(got.RepliesToMe),
		)
	}
}

// The proof is keyed on (chat_jid, msg_id), never msg_id alone: the SAME id in a DIFFERENT chat must
// not authorize a reply here.
func TestSyncReplyProofIsChatScoped(t *testing.T) {
	a := newAddressingApp(t)

	otherChat := "120363999999999999@g.us"
	if err := a.db.UpsertChat(otherChat, "group", "Other Group", time.Now()); err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	// Wave DID send "SHARED-ID" — but in a DIFFERENT chat.
	if err := a.db.UpsertMessage(store.UpsertMessageParams{
		ChatJID: otherChat, MsgID: "SHARED-ID", SenderJID: e2eOwnPN,
		Timestamp: time.Now().UTC().Add(-time.Minute), FromMe: true, Text: "elsewhere",
	}); err != nil {
		t.Fatalf("seed outbound: %v", err)
	}

	pm := inbound("M-CROSSCHAT", &waProto.ContextInfo{
		StanzaID:    proto.String("SHARED-ID"),
		Participant: proto.String(e2eOwnPN),
	})
	pm.ReplyToID = "SHARED-ID"

	if err := a.storeParsedMessage(context.Background(), pm); err != nil {
		t.Fatalf("storeParsedMessage: %v", err)
	}
	got := storeMsg(t, a, "M-CROSSCHAT")
	if got.RepliesToMe != nil {
		t.Errorf(
			"RepliesToMe = %s; proof from ANOTHER chat must not authorize a reply in THIS one (the "+
				"index is keyed on (chat_jid, msg_id), not msg_id alone)",
			triS(got.RepliesToMe),
		)
	}
}

// An ordinary message mentioning someone else records a clean false — not null, not true.
func TestSyncOrdinaryMessageRecordsFalse(t *testing.T) {
	a := newAddressingApp(t)
	pm := inbound("M-PLAIN", &waProto.ContextInfo{MentionedJID: []string{e2eOther}})

	if err := a.storeParsedMessage(context.Background(), pm); err != nil {
		t.Fatalf("storeParsedMessage: %v", err)
	}
	got := storeMsg(t, a, "M-PLAIN")
	if got.MentionsMe == nil || *got.MentionsMe {
		t.Errorf("MentionsMe = %s, want false (someone else was tagged, and we know both our identities)", triS(got.MentionsMe))
	}
}

// RELINK: our LID rotated, so the mention names an identity we no longer recognise. That is UNKNOWN,
// not false — a false would silently drop a message that may well have addressed us.
func TestSyncRelinkedIdentityQuarantinesRatherThanDrops(t *testing.T) {
	a := newAddressingApp(t)
	f := newFakeWA()
	f.linkedLID = "" // mid-relink: the LID is not resolvable yet
	a.wa = f

	pm := inbound("M-RELINK", &waProto.ContextInfo{MentionedJID: []string{"55554444333322@lid"}})

	if err := a.storeParsedMessage(context.Background(), pm); err != nil {
		t.Fatalf("storeParsedMessage: %v", err)
	}
	got := storeMsg(t, a, "M-RELINK")
	if got.MentionsMe != nil {
		t.Errorf(
			"MentionsMe = %s while our own LID is unknown; want null. An authoritative false here "+
				"silently drops a message we could not actually evaluate.",
			triS(got.MentionsMe),
		)
	}
}
