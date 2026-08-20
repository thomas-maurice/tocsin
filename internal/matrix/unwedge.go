package matrix

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/thomas-maurice/tocsin/internal/config"
)

// Unwedged reports what Unwedge removed.
type Unwedged struct {
	OutboundSessions int64
	OlmSessions      int64
}

// Unwedge drops crypto state that leaves a peer permanently unable to
// decrypt, so the bot rebuilds it from scratch on the next send.
//
// Two failure modes need this, and neither self-heals:
//
//   - A stored outbound megolm session that a recipient never managed to
//     import. The session is reused until it expires, so every message in it
//     is undecryptable for that peer. Discarding it forces a fresh session
//     and a fresh key share.
//   - A wedged olm session: the bot believes it has a working channel to a
//     device, but that device can no longer decrypt what the bot encrypts,
//     so the room key never arrives. Discarding the olm sessions makes the
//     bot claim new one-time keys and start over.
//
// Discarding olm sessions alone would change nothing while a valid megolm
// session is still cached, so the outbound sessions always go too.
func Unwedge(ctx context.Context, cfg *config.Config, roomID, userID string) (Unwedged, error) {
	var res Unwedged

	db, driver, err := openCryptoDB(cfg)
	if err != nil {
		return res, err
	}
	defer func() { _ = db.Close() }()
	placeholder := func(n int) string {
		if driver == "pgx" {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}

	if userID != "" {
		query := "delete from crypto_olm_session where sender_key in " +
			"(select identity_key from crypto_device where user_id = " + placeholder(1) + ")"
		out, err := db.ExecContext(ctx, query, userID)
		if err != nil {
			return res, fmt.Errorf("discarding olm sessions for %s: %w", userID, err)
		}
		res.OlmSessions, _ = out.RowsAffected()
	}

	query := "delete from crypto_megolm_outbound_session"
	var args []any
	if roomID != "" {
		query += " where room_id = " + placeholder(1)
		args = append(args, roomID)
	}
	out, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return res, fmt.Errorf("discarding outbound megolm sessions: %w", err)
	}
	res.OutboundSessions, _ = out.RowsAffected()
	return res, nil
}

// openCryptoDB opens the same database the crypto store lives in. The
// drivers are registered by the blank imports in bot.go.
func openCryptoDB(cfg *config.Config) (*sql.DB, string, error) {
	var driver string
	switch cfg.Database.Type {
	case "postgres":
		driver = "pgx"
	case "sqlite":
		driver = "sqlite3-fk-wal"
	default:
		return nil, "", fmt.Errorf("unsupported database type %q", cfg.Database.Type)
	}
	db, err := sql.Open(driver, cfg.Database.URI)
	if err != nil {
		return nil, "", fmt.Errorf("opening crypto store: %w", err)
	}
	return db, driver, nil
}
