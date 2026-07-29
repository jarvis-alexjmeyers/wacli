package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/out"
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
	JID       string `json:"JID"`
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
			var freshest time.Time
			for _, p := range ps {
				if p.UpdatedAt.After(freshest) {
					freshest = p.UpdatedAt
				}
				info.Participants = append(info.Participants, localGroupParticipant{
					JID:       p.UserJID,
					IsAdmin:   p.Role == "admin" || p.Role == "superadmin",
					Role:      p.Role,
					UpdatedAt: formatLocalTS(p.UpdatedAt),
				})
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
