package matrix

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thomas-maurice/tocsin/internal/config"
)

// newCryptoStoreStub builds the two crypto-store tables Unwedge touches,
// with the same column names the mautrix schema uses.
func newCryptoStoreStub(t *testing.T) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "crypto.db")
	db, err := sql.Open("sqlite3-fk-wal", path)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	for _, stmt := range []string{
		`CREATE TABLE crypto_megolm_outbound_session (room_id TEXT, session_id TEXT)`,
		`CREATE TABLE crypto_device (user_id TEXT, identity_key TEXT)`,
		`CREATE TABLE crypto_olm_session (session_id TEXT, sender_key TEXT)`,
		`INSERT INTO crypto_megolm_outbound_session VALUES ('!wedged:example.org', 'S1')`,
		`INSERT INTO crypto_megolm_outbound_session VALUES ('!other:example.org', 'S2')`,
		`INSERT INTO crypto_device VALUES ('@peer:example.org', 'IK-PEER')`,
		`INSERT INTO crypto_device VALUES ('@bystander:example.org', 'IK-BYSTANDER')`,
		`INSERT INTO crypto_olm_session VALUES ('O1', 'IK-PEER')`,
		`INSERT INTO crypto_olm_session VALUES ('O2', 'IK-BYSTANDER')`,
	} {
		_, err := db.Exec(stmt)
		require.NoError(t, err, stmt)
	}
	return &config.Config{Database: config.Database{Type: "sqlite", URI: path}}
}

func countRows(t *testing.T, cfg *config.Config, table string) int {
	t.Helper()
	db, err := sql.Open("sqlite3-fk-wal", cfg.Database.URI)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	var n int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM "+table).Scan(&n))
	return n
}

func TestUnwedgeRoomScopedLeavesOtherRoomsAlone(t *testing.T) {
	// Recovering one room must not force every other room to re-share: that
	// would make a targeted fix a server-wide key storm.
	cfg := newCryptoStoreStub(t)
	res, err := Unwedge(context.Background(), cfg, "!wedged:example.org", "")
	require.NoError(t, err)

	assert.EqualValues(t, 1, res.OutboundSessions)
	assert.EqualValues(t, 0, res.OlmSessions, "olm sessions are only touched when a user is named")
	assert.Equal(t, 1, countRows(t, cfg, "crypto_megolm_outbound_session"), "the untargeted room keeps its session")
	assert.Equal(t, 2, countRows(t, cfg, "crypto_olm_session"))
}

func TestUnwedgeUserDropsOnlyThatUsersOlmSessions(t *testing.T) {
	// Forcing fresh one-time keys for the stuck peer must not tear down
	// working channels to everyone else.
	cfg := newCryptoStoreStub(t)
	res, err := Unwedge(context.Background(), cfg, "", "@peer:example.org")
	require.NoError(t, err)

	assert.EqualValues(t, 1, res.OlmSessions)
	var remaining string
	db, err := sql.Open("sqlite3-fk-wal", cfg.Database.URI)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	require.NoError(t, db.QueryRow("SELECT sender_key FROM crypto_olm_session").Scan(&remaining))
	assert.Equal(t, "IK-BYSTANDER", remaining)

	// Dropping olm sessions is pointless while a cached megolm session is
	// still valid — nothing would re-share — so every room rotates too.
	assert.EqualValues(t, 2, res.OutboundSessions)
	assert.Equal(t, 0, countRows(t, cfg, "crypto_megolm_outbound_session"))
}

func TestUnwedgeWithoutScopeRotatesEveryRoom(t *testing.T) {
	cfg := newCryptoStoreStub(t)
	res, err := Unwedge(context.Background(), cfg, "", "")
	require.NoError(t, err)
	assert.EqualValues(t, 2, res.OutboundSessions)
	assert.EqualValues(t, 0, res.OlmSessions)
}

func TestUnwedgeRejectsUnknownDatabaseType(t *testing.T) {
	// A typo'd type must fail loudly rather than silently no-op and leave
	// the operator believing they recovered the room.
	_, err := Unwedge(context.Background(), &config.Config{
		Database: config.Database{Type: "mysql", URI: "x"},
	}, "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mysql")
}
