package wa

import (
	"strings"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
)

// SelfIdentity holds the account's own identities.
//
// BOTH must be compared. The AITOOLS-927 GATE 0 live capture proved a real group mention matched the
// LID and NOT the phone number (own_pn_match=FALSE, own_lid_match=TRUE), so a PN-only comparison looks
// correct and silently fails on every message.
//
// These come from the logged-in session (LinkedJID/LinkedLID), so each account — staging, production,
// a private node — resolves its own. Nothing here is hardcoded to one number.
type SelfIdentity struct {
	PN  string // e.g. 15551230000@s.whatsapp.net
	LID string // e.g. 99887766554433@lid
}

// ReplyAuthorship is LOCAL proof about the quoted message: did WE actually send it?
//
// It is deliberately not a bool. "We have no record" and "our record says someone else wrote it" are
// different facts, and neither may be read as "we wrote it".
type ReplyAuthorship int

const (
	// ReplyProofAbsent: no local record of the quoted message. Cannot confirm authorship.
	ReplyProofAbsent ReplyAuthorship = iota
	// ReplyProofAuthoredByUs: our own outbound record proves we sent it.
	ReplyProofAuthoredByUs
	// ReplyProofAuthoredByOther: our record says we did NOT send it — conflicts with the claim.
	ReplyProofAuthoredByOther
)

func boolPtr(b bool) *bool { return &b }

// canonicalJID normalizes a mention entity to a comparable full JID, reporting whether it resolved.
//
// Compare FULL JIDs, never bare user parts: "123@s.whatsapp.net" and "123@lid" are DIFFERENT
// principals that can share a numeric user part. (The throwaway GATE 0 probe stripped namespaces —
// fine for a one-shot observation, unsafe in production.)
func canonicalJID(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	// types.ParseJID happily reads a bare token as a *server* with an empty user, so an explicit "@"
	// and a non-empty user are required before trusting it.
	if raw == "" || !strings.Contains(raw, "@") {
		return "", false
	}
	jid, err := types.ParseJID(raw)
	if err != nil || jid.User == "" {
		return "", false
	}
	switch jid.Server {
	case types.LegacyUserServer:
		// "@c.us" is the LEGACY spelling of the phone-number namespace. It must FOLD into the modern
		// one, or a legacy-shaped mention of us compares against "@s.whatsapp.net", misses, and is
		// recorded as an authoritative `false` — a silent drop of a message that really did tag us.
		jid.Server = types.DefaultUserServer
		return jid.ToNonAD().String(), true
	case types.DefaultUserServer, types.HiddenUserServer:
		// ToNonAD strips the device/agent suffix, so 15551230000:5@... == 15551230000@...
		return jid.ToNonAD().String(), true
	default:
		// A group/broadcast/newsletter JID is not a user identity and can never be us.
		return "", false
	}
}

// identityFor returns the one of our identities that lives in the same namespace as the entity, so a
// LID mention is only ever compared against our LID (and a PN against our PN).
func (s SelfIdentity) identityFor(canonical string) string {
	if strings.HasSuffix(canonical, "@"+types.HiddenUserServer) {
		return s.LID
	}
	return s.PN
}

func (s SelfIdentity) matches(canonical string) bool {
	own, ok := canonicalJID(s.identityFor(canonical))
	return ok && own == canonical
}

// DeriveMentionsMe reports whether this message addresses us, as a tri-state (TRUTH_TABLES T0/T1).
//
// nil means "a mention entity was present and we could not resolve it" — we make NO claim either way.
// Collapsing nil to false is a silent drop; collapsing it to true is an unsolicited reply into a group.
//
// ⚠️ WHAT nil ACTUALLY DOES TODAY: it is persisted as NULL, stripped from the wire, and the backend
// treats the message as an ordinary non-wake — i.e. exactly today's behaviour, no regression, but ALSO
// NOT HELD. There is no durable quarantine/replay yet: an unresolved message is not parked for later
// re-derivation, so if it really did address us, that wake is lost. The tri-state is the PRECONDITION
// for a hold (we can now tell "not addressed" from "cannot tell"), not the hold itself.
// Durable edge hold + replay is tracked separately — do not read "null" as "safely quarantined".
func DeriveMentionsMe(ctx *waProto.ContextInfo, self SelfIdentity, isForwarded bool) *bool {
	if ctx == nil {
		return boolPtr(false)
	}

	// A forwarded copy carries the ORIGINAL's mention metadata. Reading it as an address to us is a
	// false-positive wake, so a forward never mentions us. (You cannot add a mention while forwarding;
	// tagging Wave about a forward is a separate message, which is read normally.)
	if isForwarded {
		return boolPtr(false)
	}

	mentioned := ctx.GetMentionedJID()
	// NonJIDMentions is a scalar COUNT of mention entities that carry no JID — not a list.
	nonJID := ctx.GetNonJIDMentions()

	// groupMentions (a GroupMention is {groupJID, groupSubject}) references a GROUP, never a user, so
	// it can never identify us and contributes nothing. If it is all that is present: not a mention.
	if len(mentioned) == 0 && nonJID == 0 {
		return boolPtr(false)
	}

	unresolved := false
	for _, entity := range mentioned {
		canonical, ok := canonicalJID(entity)
		if !ok {
			unresolved = true // an entity we cannot parse might have been us
			continue
		}
		// A self-match is decisive even when the other entities are unresolvable, and even when only
		// one of our own identities is known — we are certain, so there is nothing to withhold.
		if self.matches(canonical) {
			return boolPtr(true)
		}
	}

	// No self-match. A `false` here is an AUTHORITATIVE claim ("you were not addressed"), so we may
	// only make it when we could actually have seen a match.
	if unresolved || nonJID > 0 {
		return nil // an unresolved entity could have been us
	}
	if self.PN == "" || self.LID == "" {
		return nil // we do not fully know who we are (store not ready / mid-relink)
	}
	return boolPtr(false)
}

// DeriveRepliesToMe reports whether this message replies to something WE authored (TRUTH_TABLES T4).
//
// THE ASYMMETRY THAT MAKES THIS SAFE: a mention is an INVOCATION ("address Wave now") and needs no
// corroboration. A reply's `participant` is a HISTORICAL CLAIM ("Wave authored this") — it is attacker-
// supplied and therefore forgeable. So it requires LOCAL proof that we actually sent the quoted
// message. Absence of proof is never true.
func DeriveRepliesToMe(
	ctx *waProto.ContextInfo,
	self SelfIdentity,
	isForwarded bool,
	proof ReplyAuthorship,
) *bool {
	if ctx == nil {
		return boolPtr(false)
	}
	if strings.TrimSpace(ctx.GetStanzaID()) == "" || strings.TrimSpace(ctx.GetParticipant()) == "" {
		return boolPtr(false) // not a reply
	}
	// A forwarded copy of a reply-to-Wave carries the original's reply metadata. Stale: never a wake.
	if isForwarded {
		return boolPtr(false)
	}

	canonical, ok := canonicalJID(ctx.GetParticipant())
	if !ok {
		return nil // unresolvable author claim: make no claim, in either direction
	}
	if own := self.identityFor(canonical); own == "" {
		return nil // the claim is in a namespace whose identity we do not know yet
	}
	if !self.matches(canonical) {
		return boolPtr(false) // a reply to someone else
	}

	// The message CLAIMS to quote us. Believe it only with our own record.
	switch proof {
	case ReplyProofAuthoredByUs:
		return boolPtr(true)
	case ReplyProofAuthoredByOther:
		return nil // provider says us, our store says not us: a conflict, never a guess
	default:
		// No local record. This is the forgery vector: a `true` here would let anyone forge reply
		// metadata and make Wave reply into a group on demand.
		return nil
	}
}
