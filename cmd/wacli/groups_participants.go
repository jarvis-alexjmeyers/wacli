package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow/types"
)

func newGroupsParticipantsCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "participants",
		Short: "Manage group participants",
	}
	cmd.AddCommand(newGroupsParticipantsListCmd(flags))
	cmd.AddCommand(newGroupsParticipantsActionCmd(flags, "add"))
	cmd.AddCommand(newGroupsParticipantsActionCmd(flags, "remove"))
	cmd.AddCommand(newGroupsParticipantsActionCmd(flags, "promote"))
	cmd.AddCommand(newGroupsParticipantsActionCmd(flags, "demote"))
	return cmd
}

// localGroupParticipant mirrors the fields of whatsmeow's types.GroupParticipant
// that a local row can honestly fill. The JSON names match `groups info` exactly
// so a consumer can parse either without branching.
type localGroupParticipant struct {
	// JID is the PHONE jid, the only form that carries a phone number and so
	// the only form a contact directory can match on. Omitted when the stored
	// row is a privacy LID that cannot be resolved to one.
	JID string `json:"JID,omitempty"`
	// LID is the privacy identifier, present when that is what the store held.
	// whatsmeow's own GroupParticipant carries both side by side; emitting the
	// same shape means a consumer that prefers JID and falls back to LID needs
	// no special case for the local read.
	LID       string `json:"LID,omitempty"`
	IsAdmin   bool   `json:"IsAdmin"`
	Role      string `json:"Role"`
	UpdatedAt string `json:"UpdatedAt"`
}

// localGroupInfo is the `groups participants list` result. It is deliberately
// shaped like `groups info`'s types.GroupInfo — same key names, same nesting —
// so the two are interchangeable to a caller that only needs membership. The
// extra fields are the ones a live fetch cannot report and a local read can:
// FromLocalStore says this is a cache, and ParticipantsUpdatedAt says how stale.
type localGroupInfo struct {
	JID                    string                  `json:"JID"`
	Name                   string                  `json:"Name"`
	OwnerJID               string                  `json:"OwnerJID"`
	IsParent               bool                    `json:"IsParent"`
	LinkedParentJID        string                  `json:"LinkedParentJID"`
	Participants           []localGroupParticipant `json:"Participants"`
	FromLocalStore         bool                    `json:"FromLocalStore"`
	ParticipantsUpdatedAt  string                  `json:"ParticipantsUpdatedAt"`
	ParticipantsCachedOnly bool                    `json:"ParticipantsCachedOnly"`
	// LeftAt is set when this account is no longer in the group. Leaving does
	// NOT clear group_participants, so without this a caller would read a
	// resurrected membership for a group you are not in and have no way to
	// tell. Empty means still a member.
	LeftAt string `json:"LeftAt"`
}

// participantResolver returns a LID resolver only when one is needed and
// available. Mirrors `resolveStoredChats`: skip entirely when no participant is
// a LID, and degrade to nil (no resolution) rather than failing the read — a
// roster with unresolved identities is worth far more than no roster.
// lidResolver is the NARROW slice of the app resolver this file needs — the
// same shape `chats.go` uses for its own display resolution. Taking the wide
// interface would make every test stub implement methods it never calls.
type lidResolver interface {
	ResolveLIDToPN(ctx context.Context, jid types.JID) types.JID
}

func participantResolver(ctx context.Context, a *app.App, ps []store.GroupParticipant) lidResolver {
	needed := false
	for _, p := range ps {
		if strings.HasSuffix(strings.TrimSpace(p.UserJID), "@"+types.HiddenUserServer) {
			needed = true
			break
		}
	}
	if !needed {
		return nil
	}
	if _, err := os.Stat(filepath.Join(a.StoreDir(), "session.db")); err != nil {
		return nil
	}
	resolver, err := a.LocalResolver()
	if err != nil {
		return nil
	}
	return resolver
}

// resolveParticipantJID splits a stored identity into (phone JID, LID).
//
// A stored phone number returns as the JID with no LID. A stored LID returns
// its phone number as the JID and itself as the LID when the map knows it, and
// LID-only when it does not — never the LID masquerading as a JID, because a
// consumer preferring "JID" would then match nothing while believing it had a
// phone number.
func resolveParticipantJID(ctx context.Context, resolver lidResolver, stored string) (string, string) {
	trimmed := strings.TrimSpace(stored)
	jid, err := types.ParseJID(trimmed)
	if err != nil || jid.Server != types.HiddenUserServer {
		return trimmed, ""
	}
	if resolver == nil {
		return "", trimmed
	}
	resolved := resolver.ResolveLIDToPN(ctx, jid)
	if resolved.IsEmpty() || resolved.Server == types.HiddenUserServer {
		return "", trimmed
	}
	return resolved.String(), trimmed
}

func newGroupsParticipantsListCmd(flags *rootFlags) *cobra.Command {
	var jidStr string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List a group's members (from local DB; no store lock)",
		Long: "List a group's members from the local store.\n\n" +
			"Unlike `groups info` this takes no store lock and makes no network\n" +
			"call, so it works while a long-running `sync --follow` holds the\n" +
			"lock. The rows are as fresh as the last group message that sync\n" +
			"processed; ParticipantsUpdatedAt reports when that was.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jidStr) == "" {
				return fmt.Errorf("--jid is required")
			}
			// newReadOnlyApp, NOT newApp(needLock=false). Skipping the flock
			// is not enough: a read-WRITE open runs ensureSchema() DDL and a
			// store_meta seed on every invocation, which contends for the WAL
			// writer the sync --follow daemon holds — so under a concurrent
			// write this command fails "database is locked" despite taking no
			// lock of its own. It would also CREATE a store from nothing if the
			// resolved path were wrong. newReadOnlyApp's own doc comment names
			// this exact case: a command polled by an edge consumer beside the
			// follower.
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newReadOnlyApp(ctx, flags, false)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			gjid, err := types.ParseJID(jidStr)
			if err != nil {
				return err
			}
			canonical := gjid.String()

			group, err := a.DB().GetGroup(canonical)
			if err != nil {
				return err
			}
			if group == nil {
				return fmt.Errorf("group not found in local store: %s", canonical)
			}
			ps, err := a.DB().ListGroupParticipants(canonical)
			if err != nil {
				return err
			}

			info := localGroupInfo{
				JID:                    group.JID,
				Name:                   group.Name,
				OwnerJID:               group.OwnerJID,
				IsParent:               group.IsParent,
				LinkedParentJID:        group.LinkedParentJID,
				Participants:           make([]localGroupParticipant, 0, len(ps)),
				FromLocalStore:         true,
				ParticipantsCachedOnly: true,
				LeftAt:                 formatLocalTS(group.LeftAt),
			}
			// The sync path stores whatever JID the provider used, and for a
			// modern WhatsApp group that is very often a privacy LID rather
			// than a phone number. A LID matches nothing in a contact
			// directory, so emitting it raw makes every member unresolvable —
			// which is exactly what happened in production. Resolve through the
			// existing READ-ONLY session map (same helper `chats` already uses)
			// and report the phone JID when one exists.
			resolver := participantResolver(ctx, a, ps)
			var freshest time.Time
			for _, p := range ps {
				if p.UpdatedAt.After(freshest) {
					freshest = p.UpdatedAt
				}
				entry := localGroupParticipant{
					IsAdmin:   p.Role == "admin" || p.Role == "superadmin",
					Role:      p.Role,
					UpdatedAt: formatLocalTS(p.UpdatedAt),
				}
				entry.JID, entry.LID = resolveParticipantJID(ctx, resolver, p.UserJID)
				info.Participants = append(info.Participants, entry)
			}
			info.ParticipantsUpdatedAt = formatLocalTS(freshest)

			if flags.asJSON {
				return out.WriteJSON(os.Stdout, info)
			}

			name := info.Name
			if name == "" {
				name = info.JID
			}
			fmt.Fprintf(os.Stdout, "Group: %s\nJID: %s\nParticipants: %d\nCached as of: %s\n",
				sanitize(name), info.JID, len(info.Participants), info.ParticipantsUpdatedAt,
			)
			// Say the departure out loud. Leaving does not clear the member
			// rows, so without this the table renders a clean, current-looking
			// roster for a group this account is no longer in — and only a
			// --json caller could tell.
			if info.LeftAt != "" {
				fmt.Fprintf(os.Stdout, "LEFT THIS GROUP: %s (membership below is stale)\n", info.LeftAt)
			}
			w := newTableWriter(os.Stdout)
			fmt.Fprintln(w, "JID\tROLE")
			for _, p := range info.Participants {
				role := p.Role
				if role == "" {
					role = "member"
				}
				fmt.Fprintf(w, "%s\t%s\n", p.JID, role)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&jidStr, "jid", "", "group JID (…@g.us)")
	return cmd
}

// formatLocalTS renders a stored timestamp, or "" when the row carried none.
// An empty string is honest about "unknown"; a zero time formatted as year 1
// would read as an extremely stale cache and could be acted on as such.
func formatLocalTS(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.UTC().Format(time.RFC3339)
}

func newGroupsParticipantsActionCmd(flags *rootFlags, action string) *cobra.Command {
	var group string
	var users []string
	cmd := &cobra.Command{
		Use:   action,
		Short: action + " participants",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(group) == "" || len(users) == 0 {
				return fmt.Errorf("--jid and at least one --user are required")
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, true, false)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			if err := a.EnsureAuthed(); err != nil {
				return err
			}
			if err := a.Connect(ctx, false, nil); err != nil {
				return err
			}

			gjid, err := types.ParseJID(group)
			if err != nil {
				return err
			}
			var jids []types.JID
			for _, u := range users {
				j, err := wa.ParseUserOrJID(u)
				if err != nil {
					return err
				}
				jids = append(jids, j)
			}

			updated, err := a.WA().UpdateGroupParticipants(ctx, gjid, jids, wa.GroupParticipantAction(action))
			if err != nil {
				return err
			}
			if info, err := a.WA().GetGroupInfo(ctx, gjid); err == nil && info != nil {
				_ = persistGroupInfo(a.DB(), info)
			}

			if flags.asJSON {
				return out.WriteJSON(os.Stdout, updated)
			}
			fmt.Fprintln(os.Stdout, "OK")
			return nil
		},
	}
	cmd.Flags().StringVar(&group, "jid", "", "group JID (…@g.us)")
	cmd.Flags().StringSliceVar(&users, "user", nil, "user phone number (+E164 and formatting ok) or JID (repeatable)")
	return cmd
}
