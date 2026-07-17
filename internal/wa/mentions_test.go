package wa

import (
	"testing"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"google.golang.org/protobuf/proto"
)

// AITOOLS-927. A WhatsApp group @-mention puts a numeric LID on the wire, never the display name, so
// no text predicate can ever detect it. These tables are the contract for reading the provider's own
// mention/reply metadata instead.
//
// The tri-state is load-bearing: nil means "a mention entity was present but we could not resolve it".
// It must NEVER collapse to false (a silent drop) nor to true (an unsolicited reply into a group).
//
// NOTE: "null" here means "we cannot tell", NOT "safely held for replay". No durable quarantine exists
// yet; an unresolved message currently behaves exactly as it does today (no wake). These tests prove
// the DERIVATION, not a hold.

const (
	ownPN    = "15551230000@s.whatsapp.net"
	ownLID   = "99887766554433@lid"
	otherPN  = "15559990000@s.whatsapp.net"
	otherLID = "11122233344455@lid"
)

func self() SelfIdentity { return SelfIdentity{PN: ownPN, LID: ownLID} }

func ctxWith(mentioned ...string) *waProto.ContextInfo {
	return &waProto.ContextInfo{MentionedJID: mentioned}
}

func tri(b *bool) string {
	if b == nil {
		return "null"
	}
	if *b {
		return "true"
	}
	return "false"
}

func want(t *testing.T, row string, got *bool, expect *bool) {
	t.Helper()
	if (got == nil) != (expect == nil) || (got != nil && expect != nil && *got != *expect) {
		t.Errorf("T1 row %s: MentionsMe = %s, want %s", row, tri(got), tri(expect))
	}
}

var (
	yes = proto.Bool(true)
	no  = proto.Bool(false)
)

// TestDeriveMentionsMe is TRUTH_TABLES.md T1 executed.
func TestDeriveMentionsMe(t *testing.T) {
	cases := []struct {
		row      string
		ctx      *waProto.ContextInfo
		self     SelfIdentity
		forwards bool
		expect   *bool
	}{
		// 1: plain conversation, no ContextInfo at all.
		{"1", nil, self(), false, no},

		// 2: THE BUG. The group mention carries our own LID — proven by the GATE 0 live capture
		//    (own_pn_match=FALSE, own_lid_match=TRUE). A PN-only comparison returns false here and
		//    silently drops every real mention.
		{"2", ctxWith(ownLID), self(), false, yes},

		// 3: same, but WhatsApp used our PN.
		{"3", ctxWith(ownPN), self(), false, yes},

		// 4: someone else was tagged; we were not.
		{"4", ctxWith(otherPN, otherLID), self(), false, no},

		// 12: FORWARDED copy of a message that mentioned us. The mention metadata rides along, so a
		//     naive read is a FALSE POSITIVE wake. Suppress.
		{"12", ctxWith(ownLID), self(), true, no},

		// 14: groupMentions only — a GroupMention references a GROUP, never a user. It can never
		//     identify Wave, so on its own it is not a mention of us.
		{"14", &waProto.ContextInfo{
			GroupMentions: []*waProto.GroupMention{{GroupJID: proto.String("g@g.us")}},
		}, self(), false, no},

		// 15: nonJIDMentions is a scalar COUNT of mentions with no JID. We cannot rule out that one
		//     of them was us => unresolved => null. Never false (that would be a lie).
		{"15", &waProto.ContextInfo{NonJIDMentions: proto.Uint32(1)}, self(), false, nil},

		// 19: our own identity is not fully known (store not ready / mid-relink). A mention is
		//     present and we cannot compare => null, never an authoritative false.
		{"19", ctxWith(otherLID), SelfIdentity{PN: ownPN}, false, nil},

		// 19b: identity unknown BUT the mentioned namespace matches the identity we DO know, and it
		//      is us — decide on what we know rather than withholding a certainty.
		{"19b", ctxWith(ownPN), SelfIdentity{PN: ownPN}, false, yes},

		// 20: multiple entities reduce through T0: any self-match wins.
		{"20", ctxWith(otherPN, ownLID, otherLID), self(), false, yes},

		// 20b: multiple entities, none of them us, all resolvable => a clean false.
		{"20b", ctxWith(otherPN, otherLID), self(), false, no},

		// 20c: multiple entities, none of them us, but an unresolvable one is present => null.
		//      An unparseable entity could have been us.
		{"20c", ctxWith(otherPN, "not-a-jid"), self(), false, nil},

		// Device-suffixed JIDs must canonicalize: 5:x@s.whatsapp.net is the same principal as x@...
		{"canon-device", ctxWith("15551230000:5@s.whatsapp.net"), self(), false, yes},

		// A PN and a LID with the SAME numeric user part are DIFFERENT principals. Comparing bare
		// user parts (as the throwaway probe did) would false-positive here.
		{"canon-namespace", ctxWith("99887766554433@s.whatsapp.net"), self(), false, no},

		// LEGACY "@c.us" is the same phone-number namespace as "@s.whatsapp.net". If it does not fold,
		// a legacy-shaped mention of US compares against the modern PN, misses, and is recorded as an
		// authoritative `false` -- silently dropping a message that really did tag us.
		{"canon-legacy-c.us", ctxWith("15551230000@c.us"), self(), false, yes},
		{"canon-legacy-other", ctxWith("15559990000@c.us"), self(), false, no},
	}

	for _, tc := range cases {
		got := DeriveMentionsMe(tc.ctx, tc.self, tc.forwards)
		want(t, tc.row, got, tc.expect)
	}
}

// TestDeriveRepliesToMe is TRUTH_TABLES.md T4 executed.
//
// THE LOAD-BEARING ASYMMETRY (Jarvis): a mention is an INVOCATION ("address Wave now") and needs no
// corroboration. A reply's `participant` is a HISTORICAL CLAIM ("Wave authored this") and is therefore
// forgeable — it REQUIRES local proof that we actually sent the quoted message. Absence of proof is
// never true.
func TestDeriveRepliesToMe(t *testing.T) {
	reply := func(participant string) *waProto.ContextInfo {
		return &waProto.ContextInfo{
			StanzaID:    proto.String("QUOTED-MSG-ID"),
			Participant: proto.String(participant),
		}
	}

	cases := []struct {
		row      string
		ctx      *waProto.ContextInfo
		proof    ReplyAuthorship
		forwards bool
		expect   *bool
	}{
		// R1: not a reply at all.
		{"R1", &waProto.ContextInfo{}, ReplyProofAbsent, false, no},

		// R2: THE NEW PRODUCT AC. A native reply to a Wave-authored message, proven by our OWN
		//     outbound record => Wave responds, whether or not it was tagged again.
		{"R2", reply(ownLID), ReplyProofAuthoredByUs, false, yes},
		{"R2-pn", reply(ownPN), ReplyProofAuthoredByUs, false, yes},

		// R3: 🔴 THE FORGERY VECTOR. The message CLAIMS to quote us, but we have no local record of
		//     ever sending it. Absence of proof => null. NEVER true: a `true` here lets
		//     anyone forge reply metadata and make Wave reply into a group on demand.
		{"R3", reply(ownLID), ReplyProofAbsent, false, nil},

		// R4: the provider says we wrote it; our own store says we did NOT. A direct conflict =>
		//     null + alert. Never guess.
		{"R4", reply(ownLID), ReplyProofAuthoredByOther, false, nil},

		// R5: a reply to ANOTHER participant. Not our business.
		{"R5", reply(otherPN), ReplyProofAbsent, false, no},

		// R7: a FORWARDED copy of a reply-to-Wave carries stale reply metadata => must not wake.
		{"R7", reply(ownLID), ReplyProofAuthoredByUs, true, no},

		// R8: the participant is unresolvable => null, no wake, no lie.
		{"R8", reply("not-a-jid"), ReplyProofAbsent, false, nil},
	}

	for _, tc := range cases {
		got := DeriveRepliesToMe(tc.ctx, self(), tc.forwards, tc.proof)
		if (got == nil) != (tc.expect == nil) || (got != nil && tc.expect != nil && *got != *tc.expect) {
			t.Errorf("T4 row %s: RepliesToMe = %s, want %s", tc.row, tri(got), tri(tc.expect))
		}
	}
}

// An unresolved (null) value must be distinguishable from a false one all the way out to the wire.
// Collapsing null -> false is the silent-drop bug; collapsing null -> true is the forgery.
func TestTriStateIsNotCollapsed(t *testing.T) {
	if got := DeriveMentionsMe(&waProto.ContextInfo{NonJIDMentions: proto.Uint32(2)}, self(), false); got != nil {
		t.Fatalf("an unresolved mention collapsed to %s; it must stay null (we cannot tell)", tri(got))
	}
	if got := DeriveRepliesToMe(
		&waProto.ContextInfo{StanzaID: proto.String("x"), Participant: proto.String(ownLID)},
		self(), false, ReplyProofAbsent,
	); got != nil {
		t.Fatalf("an unproven reply claim collapsed to %s; absence of proof must be null, never true", tri(got))
	}
}
