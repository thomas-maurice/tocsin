package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/thomas-maurice/tocsin/internal/config"
	"github.com/thomas-maurice/tocsin/internal/matrix"
)

// newUnwedgeCmd builds the recovery command for "the bot reports healthy
// sends but I still see Unable to decrypt": it throws away the crypto state
// that cannot self-heal so the next send rebuilds it.
func newUnwedgeCmd() *cobra.Command {
	var configPath, roomID, userID string
	cmd := &cobra.Command{
		Use:   "unwedge",
		Short: "Force fresh megolm (and optionally olm) sessions so recipients get new keys",
		Long: `Discards stored outbound megolm sessions so the next message creates and
shares a brand new one. With --user it also discards the olm sessions with
that user's devices, forcing the bot to claim fresh one-time keys.

Use it when the bot reports healthy sends but a recipient still sees
"Unable to decrypt message": a megolm session the peer failed to import is
reused until it expires, and a wedged olm session means the room key never
arrives in the first place. Neither recovers on its own.

Safe to run against a live bot; it only removes regenerable state. Messages
already sent with the discarded session stay unreadable — their keys were
never successfully delivered.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			res, err := matrix.Unwedge(cmd.Context(), cfg, roomID, userID)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"discarded %d outbound megolm session(s) and %d olm session(s); the next send re-shares keys\n",
				res.OutboundSessions, res.OlmSessions)
			return nil
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", "", "path to config file (default: ./config.yaml)")
	cmd.Flags().StringVar(&roomID, "room", "", "only this room (default: every room)")
	cmd.Flags().StringVar(&userID, "user", "", "also discard olm sessions with this user's devices")
	return cmd
}
