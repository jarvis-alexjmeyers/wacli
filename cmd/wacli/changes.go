package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/spf13/cobra"
)

func newChangesCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "changes",
		Short: "Read the local message change stream",
	}
	cmd.AddCommand(newChangesListCmd(flags))
	cmd.AddCommand(newChangesStatusCmd(flags))
	return cmd
}

func newChangesListCmd(flags *rootFlags) *cobra.Command {
	var afterSeq int64
	var limit int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List message changes after a sequence cursor",
		RunE: func(cmd *cobra.Command, args []string) error {
			if afterSeq < 0 {
				return fmt.Errorf("--after-seq must not be negative")
			}
			if limit <= 0 {
				return fmt.Errorf("--limit must be positive")
			}
			if limit > 500 {
				limit = 500
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()
			a, lk, err := newReadOnlyApp(ctx, flags, false)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			page, err := a.DB().ListMessageChanges(afterSeq, limit)
			if err != nil {
				return err
			}
			if flags.asJSON {
				return out.WriteJSON(os.Stdout, page)
			}
			return writeChangesList(os.Stdout, page.Changes, fullTableOutput(flags.fullOutput))
		},
	}
	cmd.Flags().Int64Var(&afterSeq, "after-seq", 0, "only changes after this sequence")
	cmd.Flags().IntVar(&limit, "limit", 200, "max number of changes to return (max 500)")
	return cmd
}

func newChangesStatusCmd(flags *rootFlags) *cobra.Command {
	var lookbackSeconds int64
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show message change cursor status",
		RunE: func(cmd *cobra.Command, args []string) error {
			if lookbackSeconds < 0 {
				return fmt.Errorf("--lookback-s must not be negative")
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()
			a, lk, err := newReadOnlyApp(ctx, flags, false)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			status, err := a.DB().MessageChangesStatus(lookbackSeconds)
			if err != nil {
				return err
			}
			if flags.asJSON {
				return out.WriteJSON(os.Stdout, status)
			}
			fmt.Fprintf(os.Stdout, "Store instance: %s\n", status.StoreInstanceID)
			fmt.Fprintf(os.Stdout, "Available sequence range: %d-%d\n", status.MinSeq, status.LatestSeq)
			fmt.Fprintf(os.Stdout, "Maximum allocated sequence: %d\n", status.MaxAllocated)
			fmt.Fprintf(os.Stdout, "Bootstrap sequence: %d\n", status.BootstrapSeq)
			return nil
		},
	}
	cmd.Flags().Int64Var(&lookbackSeconds, "lookback-s", 3600, "bootstrap lookback in seconds")
	return cmd
}

func writeChangesList(dst io.Writer, changes []store.MessageChange, fullOutput bool) error {
	w := newTableWriter(dst)
	fmt.Fprintln(w, "SEQ\tTIME\tKIND\tORIGIN\tFROM ME\tCHAT\tMESSAGE ID\tPURGED")
	for _, change := range changes {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%t\t%s\t%s\t%t\n",
			change.Seq,
			time.Unix(change.TS, 0).Local().Format("2006-01-02 15:04:05"),
			change.Kind,
			change.Origin,
			change.FromMe,
			tableCell(change.ChatJID, 24, fullOutput),
			tableCell(change.MsgID, 24, fullOutput),
			change.Message == nil,
		)
	}
	return w.Flush()
}
