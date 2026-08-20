package matrix

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	_ "go.mau.fi/util/dbutil/litestream" // registers the sqlite3-fk-wal driver
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/crypto/verificationhelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the pgx driver

	"github.com/thomas-maurice/tocsin/internal/config"
	"github.com/thomas-maurice/tocsin/internal/logging"
	"github.com/thomas-maurice/tocsin/internal/metrics"
	"github.com/thomas-maurice/tocsin/internal/notify"
)

const (
	firstSyncTimeout = 30 * time.Second
	// syncStaleThreshold is how old the last sync may be before the bot is
	// considered unhealthy. Matrix long-polls every ~30s, so a couple of
	// missed cycles is the signal.
	syncStaleThreshold = 90 * time.Second
	// metricsInterval is how often the sync-age gauge is refreshed.
	metricsInterval = 15 * time.Second
)

// Bot is a Matrix client that delivers notifications to a single encrypted
// room. It refuses to send to unencrypted rooms.
type Bot struct {
	cfg    *config.Config
	client *mautrix.Client
	helper *cryptohelper.CryptoHelper

	firstSync     chan struct{}
	firstSyncOnce sync.Once
	syncErr       chan error

	verifHelper *verificationhelper.VerificationHelper

	startTime    time.Time
	delivered    atomic.Int64
	lastSyncUnix atomic.Int64

	// retryBase is the linear backoff step for sends; overridable in tests.
	retryBase time.Duration
}

func New(ctx context.Context, cfg *config.Config) (*Bot, error) {
	client, err := mautrix.NewClient(cfg.Matrix.Homeserver, "", "")
	if err != nil {
		return nil, fmt.Errorf("creating matrix client: %w", err)
	}
	client.Log = mautrixLogger(cfg.LogLevel)

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}
	pickleKey, err := loadOrCreatePickleKey(filepath.Join(cfg.DataDir, "pickle.key"))
	if err != nil {
		return nil, err
	}

	var store any
	switch cfg.Database.Type {
	case "sqlite":
		store = cfg.Database.URI
	case "postgres":
		db, err := dbutil.NewWithDialect(cfg.Database.URI, "pgx")
		if err != nil {
			return nil, fmt.Errorf("opening postgres crypto store: %w", err)
		}
		store = db
	default:
		return nil, fmt.Errorf("unsupported database type %q", cfg.Database.Type)
	}

	helper, err := cryptohelper.NewCryptoHelper(client, pickleKey, store)
	if err != nil {
		return nil, fmt.Errorf("creating crypto helper: %w", err)
	}
	helper.LoginAs = &mautrix.ReqLogin{
		Type:                     mautrix.AuthTypePassword,
		Identifier:               mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: cfg.Matrix.UserID},
		Password:                 cfg.Matrix.Password,
		InitialDeviceDisplayName: "tocsin",
	}

	b := &Bot{
		cfg:       cfg,
		client:    client,
		helper:    helper,
		firstSync: make(chan struct{}),
		syncErr:   make(chan error, 1),
		startTime: time.Now(),
	}

	syncer := client.Syncer.(*mautrix.DefaultSyncer)
	syncer.OnEventType(event.StateMember, b.handleMember)
	syncer.OnEventType(event.EventMessage, b.handleMessage)
	syncer.OnSync(func(ctx context.Context, resp *mautrix.RespSync, since string) bool {
		b.lastSyncUnix.Store(time.Now().Unix())
		b.firstSyncOnce.Do(func() { close(b.firstSync) })
		return true
	})
	return b, nil
}

// Start logs in, starts syncing, and bootstraps cross-signing so the device
// is verified. It returns once the bot is ready to send; syncing continues
// in the background until ctx is cancelled.
func (b *Bot) Start(ctx context.Context) error {
	log := logging.From(ctx)
	if err := b.helper.Init(ctx); err != nil {
		return fmt.Errorf("initializing e2ee: %w", err)
	}
	// Wrapped, not the bare helper: every room send goes through
	// Client.Crypto.Encrypt, which is where recipient device keys get
	// bootstrapped and a share that would reach nobody is rejected.
	b.client.Crypto = &keyBootstrapCrypto{CryptoHelper: b.helper, bot: b}
	log.Info("logged in", "user_id", b.client.UserID, "device_id", b.client.DeviceID)

	// Interactive (SAS) verification support: registered before syncing so no
	// verification events are missed. SAS only — no QR on a headless bot.
	// The in-room fix must be registered before the helper's own handlers.
	RegisterInRoomVerificationFix(b.client, b.client.Syncer.(mautrix.ExtensibleSyncer))
	b.verifHelper = verificationhelper.NewVerificationHelper(
		b.client, b.helper.Machine(), verificationhelper.NewInMemoryVerificationStore(),
		&verificationCallbacks{b: b}, false, false, true,
	)
	if err := b.verifHelper.Init(ctx); err != nil {
		return fmt.Errorf("initializing verification helper: %w", err)
	}

	go func() {
		if err := b.client.SyncWithContext(ctx); err != nil && !errors.Is(err, context.Canceled) {
			b.syncErr <- err
		}
	}()

	select {
	case <-b.firstSync:
	case err := <-b.syncErr:
		return fmt.Errorf("initial sync: %w", err)
	case <-time.After(firstSyncTimeout):
		return fmt.Errorf("timed out waiting for initial sync")
	case <-ctx.Done():
		return ctx.Err()
	}

	if err := b.ensureVerified(ctx); err != nil {
		return fmt.Errorf("bootstrapping device verification: %w", err)
	}

	// A megolm session's key is delivered exactly once, when the session is
	// first shared. If that delivery silently fails for a recipient, every
	// message in the session is unreadable for them for its whole lifetime
	// (a week by default) and nothing detects it. Starting each run with
	// fresh sessions bounds that damage to a single bot lifetime; the cost
	// is one extra key share per active room per restart.
	if res, err := Unwedge(ctx, b.cfg, "", ""); err != nil {
		log.Warn("could not rotate outbound megolm sessions at startup", "error", err)
	} else if res.OutboundSessions > 0 {
		log.Info("rotated outbound megolm sessions at startup", "sessions", res.OutboundSessions)
	}

	go b.metricsLoop(ctx)

	log.Info("matrix bot ready", "user_id", b.client.UserID, "device_id", b.client.DeviceID)
	return nil
}

// Profile is the bot account's public Matrix profile.
type Profile struct {
	DisplayName string
	Avatar      []byte
	AvatarMIME  string
}

// Profile returns the account's display name and avatar. The avatar is
// downloaded and inlined so callers (the admin UI) can show it without an
// authenticated media endpoint; a missing or undownloadable avatar is not
// an error — the name is still useful on its own.
func (b *Bot) Profile(ctx context.Context) (Profile, error) {
	name, err := b.client.GetOwnDisplayName(ctx)
	if err != nil {
		return Profile{}, fmt.Errorf("fetching display name: %w", err)
	}
	p := Profile{DisplayName: name.DisplayName}
	uri, err := b.client.GetAvatarURL(ctx, b.client.UserID)
	if err != nil || uri.IsEmpty() {
		// Synapse answers 404 when no avatar was ever set.
		return p, nil
	}
	data, err := b.client.DownloadBytes(ctx, uri)
	if err != nil {
		logging.From(ctx).Warn("downloading own avatar", "error", err)
		return p, nil
	}
	p.Avatar = data
	p.AvatarMIME = http.DetectContentType(data)
	return p, nil
}

// SetProfile applies the non-empty parts: a display name rename and/or a
// new avatar image, which is uploaded to the media repo first.
func (b *Bot) SetProfile(ctx context.Context, displayName string, avatar []byte) error {
	if displayName != "" {
		if err := b.client.SetDisplayName(ctx, displayName); err != nil {
			return fmt.Errorf("setting display name: %w", err)
		}
		logging.From(ctx).Info("display name updated", "display_name", displayName)
	}
	if len(avatar) > 0 {
		mime := http.DetectContentType(avatar)
		if !strings.HasPrefix(mime, "image/") {
			return fmt.Errorf("avatar looks like %s, not an image", mime)
		}
		resp, err := b.client.UploadBytes(ctx, avatar, mime)
		if err != nil {
			return fmt.Errorf("uploading avatar: %w", err)
		}
		if err := b.client.SetAvatarURL(ctx, resp.ContentURI); err != nil {
			return fmt.Errorf("setting avatar url: %w", err)
		}
		logging.From(ctx).Info("avatar updated", "size", len(avatar), "mime", mime)
	}
	return nil
}

// metricsLoop keeps the sync-age and verification gauges fresh until ctx is
// cancelled.
func (b *Bot) metricsLoop(ctx context.Context) {
	ticker := time.NewTicker(metricsInterval)
	defer ticker.Stop()
	for {
		if unix := b.lastSyncUnix.Load(); unix > 0 {
			metrics.SyncAge.Set(time.Since(time.Unix(unix, 0)).Seconds())
		}
		verified := 0.0
		if _, ok, err := b.helper.Machine().GetOwnVerificationStatus(ctx); err == nil && ok {
			verified = 1
		}
		metrics.Verified.Set(verified)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// SyncErr delivers a fatal sync-loop error (e.g. a revoked access token).
// The process should exit and let the supervisor restart it: a fresh start
// logs in again and self-heals.
func (b *Bot) SyncErr() <-chan error {
	return b.syncErr
}

// Status is a point-in-time snapshot of the bot's health for the admin API.
type Status struct {
	UserID    string
	DeviceID  string
	Verified  bool
	LastSync  time.Time
	Delivered int64
	Uptime    time.Duration
}

func (b *Bot) Status(ctx context.Context) Status {
	st := Status{
		UserID:    b.client.UserID.String(),
		DeviceID:  b.client.DeviceID.String(),
		Delivered: b.delivered.Load(),
		Uptime:    time.Since(b.startTime),
	}
	if unix := b.lastSyncUnix.Load(); unix > 0 {
		st.LastSync = time.Unix(unix, 0)
	}
	if _, verified, err := b.helper.Machine().GetOwnVerificationStatus(ctx); err == nil {
		st.Verified = verified
	}
	return st
}

// RoomInfo describes a room the bot is joined to.
type RoomInfo struct {
	ID   string
	Name string
	// DMWith is the other member's MXID when the room looks like a direct
	// message — nameless with exactly two joined members. Element creates
	// such rooms for user verification; they are not notification targets.
	DMWith string
}

// JoinedRooms lists the rooms the bot is currently in, with display names
// where set.
func (b *Bot) JoinedRooms(ctx context.Context) ([]RoomInfo, error) {
	resp, err := b.client.JoinedRooms(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing joined rooms: %w", err)
	}
	rooms := make([]RoomInfo, 0, len(resp.JoinedRooms))
	for _, rid := range resp.JoinedRooms {
		var name event.RoomNameEventContent
		// Rooms without an m.room.name simply show their ID.
		_ = b.client.StateEvent(ctx, rid, event.StateRoomName, "", &name)
		info := RoomInfo{ID: rid.String(), Name: name.Name}
		if name.Name == "" {
			// Best effort: an unreachable members endpoint just means the
			// room is presented as a plain unnamed room.
			if members, err := b.client.JoinedMembers(ctx, rid); err == nil && len(members.Joined) == 2 {
				for uid := range members.Joined {
					if uid != b.client.UserID {
						info.DMWith = uid.String()
					}
				}
			}
		}
		rooms = append(rooms, info)
	}
	return rooms, nil
}

// LeaveRoom leaves and forgets a room, e.g. to clean up after an unwanted
// invite.
func (b *Bot) LeaveRoom(ctx context.Context, roomID string) error {
	rid := id.RoomID(roomID)
	if _, err := b.client.LeaveRoom(ctx, rid); err != nil {
		return fmt.Errorf("leaving room %s: %w", rid, err)
	}
	if _, err := b.client.ForgetRoom(ctx, rid); err != nil {
		// Leaving succeeded; forgetting is cosmetic cleanup.
		logging.From(ctx).Warn("failed to forget room after leaving", "room_id", rid, "error", err)
	}
	return nil
}

// ResolveRoom turns a room alias (#foo:server) into a room ID; room IDs
// pass through after a shape check.
func (b *Bot) ResolveRoom(ctx context.Context, room string) (string, error) {
	if !strings.HasPrefix(room, "#") {
		if err := ValidateRoomID(room); err != nil {
			return "", err
		}
		return room, nil
	}
	resp, err := b.client.ResolveAlias(ctx, id.RoomAlias(room))
	if err != nil {
		return "", fmt.Errorf("resolving alias %s: %w", room, err)
	}
	return resp.RoomID.String(), nil
}

// ValidateRoomID checks that a string is shaped like a Matrix room ID.
// Aliases are resolved server-side and thus validated for real; raw IDs used
// to pass through completely unchecked, letting any garbage be stored as a
// channel's room. Existence is deliberately NOT checked: mapping a room the
// bot has not been invited to yet is a supported flow (the UI shows it as
// not joined). Both !opaque:server (classic) and !opaque (room v12+,
// MSC4291) forms are accepted.
func ValidateRoomID(room string) error {
	malformed := fmt.Errorf("%q is not a room ID (!opaque:server) or alias (#alias:server)", room)
	if !strings.HasPrefix(room, "!") || len(room) < 2 || len(room) > 255 {
		return malformed
	}
	local, server, hasServer := strings.Cut(room[1:], ":")
	if local == "" || (hasServer && server == "") {
		return malformed
	}
	for _, r := range room {
		if r <= ' ' || r > '~' { // control chars, whitespace, non-ASCII
			return malformed
		}
	}
	return nil
}

// RoomAlias returns the room's canonical alias (#foo:server), or "" when
// none is set. Best-effort: lookup failures just mean no alias to display.
func (b *Bot) RoomAlias(ctx context.Context, roomID string) string {
	var content event.CanonicalAliasEventContent
	if err := b.client.StateEvent(ctx, id.RoomID(roomID), event.StateCanonicalAlias, "", &content); err != nil {
		return ""
	}
	return content.Alias.String()
}

// RoomStatus reports whether the bot has joined a room and whether the room
// is encrypted, for per-channel health in the admin API.
func (b *Bot) RoomStatus(ctx context.Context, roomID string) (joined, encrypted bool) {
	rid := id.RoomID(roomID)
	joined = b.client.StateStore.IsInRoom(ctx, rid, b.client.UserID)
	encrypted, _ = b.client.StateStore.IsEncrypted(ctx, rid)
	return joined, encrypted
}

// roomEncrypted reports whether a room is encrypted. If the state store says
// no, it refetches the room state from the server before answering: the
// store can be missing state entirely, e.g. when a previous run died between
// saving the sync token and processing the sync response (mautrix persists
// the token first), which would otherwise poison the store forever.
// Client.State also repopulates the cached member list E2EE needs.
func (b *Bot) roomEncrypted(ctx context.Context, roomID id.RoomID) (bool, error) {
	encrypted, err := b.client.StateStore.IsEncrypted(ctx, roomID)
	if err != nil || encrypted {
		return encrypted, err
	}
	if _, err := b.client.State(ctx, roomID); err != nil {
		// Typically M_FORBIDDEN when the bot has not joined yet; the state
		// store's answer stands.
		logging.From(ctx).Warn("could not refetch room state", "room_id", roomID, "error", err)
		return false, nil
	}
	return b.client.StateStore.IsEncrypted(ctx, roomID)
}

// ensureVerified makes sure the account has cross-signing keys and that this
// device is signed by them. The recovery key is persisted in the data dir on
// first generation and reused on later devices/databases.
func (b *Bot) ensureVerified(ctx context.Context) error {
	log := logging.From(ctx)
	mach := b.helper.Machine()
	recoveryKeyPath := filepath.Join(b.cfg.DataDir, "recovery.key")

	if b.cfg.ResetIdentity {
		log.Warn("resetting identity: logging out all other devices, replacing cross-signing keys and recovery key")
		if err := b.nukeOtherDevices(ctx); err != nil {
			return fmt.Errorf("deleting other devices: %w", err)
		}
		return b.bootstrapCrossSigning(ctx, recoveryKeyPath)
	}

	hasKeys, isVerified, err := mach.GetOwnVerificationStatus(ctx)
	if err != nil {
		return fmt.Errorf("querying verification status: %w", err)
	}
	switch {
	case isVerified:
		log.Info("device already cross-signed and verified")
		// The private cross-signing keys live only in memory + SSSS; without
		// them the bot cannot user-sign peers during interactive
		// verification. Restore them from SSSS on every start.
		if mach.CrossSigningKeys == nil {
			if err := b.loadCrossSigningPrivateKeys(ctx, recoveryKeyPath); err != nil {
				log.Warn("could not restore cross-signing private keys; verifying users will not work", "error", err)
			}
		}
		return nil
	case hasKeys:
		raw, err := os.ReadFile(recoveryKeyPath)
		if err != nil {
			return fmt.Errorf("cross-signing keys exist on the server but the recovery key is unavailable (%w); restore %s or reset cross-signing for this account", err, recoveryKeyPath)
		}
		if err := mach.VerifyWithRecoveryKey(ctx, strings.TrimSpace(string(raw))); err != nil {
			return fmt.Errorf("verifying with recovery key: %w", err)
		}
		log.Info("device verified with existing recovery key")
	default:
		return b.bootstrapCrossSigning(ctx, recoveryKeyPath)
	}
	return nil
}

// bootstrapCrossSigning generates fresh cross-signing keys (replacing any
// existing ones on the server), signs this device with them, and persists the
// new recovery key.
func (b *Bot) bootstrapCrossSigning(ctx context.Context, recoveryKeyPath string) error {
	// The recovery key is the identity anchor; never overwrite one that is
	// already on disk unless the operator explicitly asked for a reset.
	if !b.cfg.ResetIdentity {
		if _, err := os.Stat(recoveryKeyPath); err == nil {
			return fmt.Errorf("%s exists but the server has no matching cross-signing keys; move the file away or run with --reset-identity", recoveryKeyPath)
		}
	}
	mach := b.helper.Machine()
	recoveryKey, _, err := mach.GenerateAndUploadCrossSigningKeysWithPassword(ctx, b.cfg.Matrix.Password, "")
	if err != nil {
		return fmt.Errorf("generating cross-signing keys: %w", err)
	}
	if err := mach.SignOwnDevice(ctx, mach.OwnIdentity()); err != nil {
		return fmt.Errorf("signing own device: %w", err)
	}
	if err := mach.SignOwnMasterKey(ctx); err != nil {
		return fmt.Errorf("signing own master key: %w", err)
	}
	if err := os.WriteFile(recoveryKeyPath, []byte(recoveryKey+"\n"), 0o600); err != nil {
		return fmt.Errorf("persisting recovery key: %w", err)
	}
	logging.From(ctx).Info("generated cross-signing keys and verified device", "recovery_key_path", recoveryKeyPath)
	return nil
}

// loadCrossSigningPrivateKeys unlocks SSSS with the persisted recovery key
// and imports the private cross-signing keys into the olm machine's cache.
func (b *Bot) loadCrossSigningPrivateKeys(ctx context.Context, recoveryKeyPath string) error {
	raw, err := os.ReadFile(recoveryKeyPath)
	if err != nil {
		return fmt.Errorf("reading recovery key: %w", err)
	}
	mach := b.helper.Machine()
	keyID, keyData, err := mach.SSSS.GetDefaultKeyData(ctx)
	if err != nil {
		return fmt.Errorf("getting default SSSS key data: %w", err)
	}
	key, err := keyData.VerifyRecoveryKey(keyID, strings.TrimSpace(string(raw)))
	if err != nil {
		return fmt.Errorf("verifying recovery key against SSSS: %w", err)
	}
	if err := mach.FetchCrossSigningKeysFromSSSS(ctx, key); err != nil {
		return fmt.Errorf("fetching cross-signing keys from SSSS: %w", err)
	}
	return nil
}

// nukeOtherDevices logs out every device of the account except the current
// one: their access tokens are revoked and their device keys removed, so
// they receive no future megolm sessions. Deletion requires user-interactive
// auth; the account password satisfies it.
func (b *Bot) nukeOtherDevices(ctx context.Context) error {
	log := logging.From(ctx)
	devices, err := b.client.GetDevicesInfo(ctx)
	if err != nil {
		return fmt.Errorf("listing devices: %w", err)
	}
	var doomed []id.DeviceID
	for _, dev := range devices.Devices {
		if dev.DeviceID != b.client.DeviceID {
			doomed = append(doomed, dev.DeviceID)
		}
	}
	if len(doomed) == 0 {
		log.Info("no other devices to delete")
		return nil
	}

	// DeleteDevices discards the 401 UIA challenge body, so make the request
	// directly: MakeFullRequest returns the body even on error responses.
	req := &mautrix.ReqDeleteDevices[any]{Devices: doomed}
	deleteReq := mautrix.FullRequest{
		Method:      http.MethodPost,
		URL:         b.client.BuildClientURL("v3", "delete_devices"),
		RequestJSON: req,
	}
	content, err := b.client.MakeFullRequest(ctx, deleteReq)
	var httpErr mautrix.HTTPError
	if errors.As(err, &httpErr) && httpErr.IsStatus(401) {
		var uia mautrix.RespUserInteractive
		if jsonErr := json.Unmarshal(content, &uia); jsonErr != nil || uia.Session == "" {
			return fmt.Errorf("decoding UIA challenge: %w", err)
		}
		req.Auth = &mautrix.ReqUIAuthLogin{
			BaseAuthData: mautrix.BaseAuthData{Type: mautrix.AuthTypePassword, Session: uia.Session},
			User:         b.client.UserID.String(),
			Password:     b.cfg.Matrix.Password,
		}
		deleteReq.SensitiveContent = true
		_, err = b.client.MakeFullRequest(ctx, deleteReq)
	}
	if err != nil {
		return err
	}
	log.Info("deleted other devices", "count", len(doomed))
	return nil
}

func (b *Bot) handleMember(ctx context.Context, evt *event.Event) {
	if evt.GetStateKey() != b.client.UserID.String() {
		return
	}
	if evt.Content.AsMember().Membership != event.MembershipInvite {
		return
	}
	log := logging.From(ctx)
	if !InviterAllowed(evt.Sender, b.client.UserID, b.cfg.Matrix.AllowedServers) {
		log.Warn("declining invite from untrusted homeserver", "room_id", evt.RoomID, "inviter", evt.Sender)
		if _, err := b.client.LeaveRoom(ctx, evt.RoomID); err != nil {
			log.Error("failed to decline invite", "room_id", evt.RoomID, "error", err)
		}
		return
	}
	if _, err := b.client.JoinRoomByID(ctx, evt.RoomID); err != nil {
		log.Error("failed to join room after invite", "room_id", evt.RoomID, "error", err)
		return
	}
	log.Info("joined room after invite", "room_id", evt.RoomID, "inviter", evt.Sender)
}

// InviterAllowed reports whether an invite from the given user may be
// accepted: their homeserver must be in the allowlist, which defaults to the
// bot's own homeserver when empty. This is the only gate on auto-join — a
// federated stranger must not be able to pull the bot into rooms.
func InviterAllowed(inviter, self id.UserID, allowedServers []string) bool {
	server := inviter.Homeserver()
	if len(allowedServers) == 0 {
		return server == self.Homeserver()
	}
	for _, allowed := range allowedServers {
		if server == allowed {
			return true
		}
	}
	return false
}

const (
	sendMaxAttempts = 3
	sendRetryBase   = 2 * time.Second
)

// Send renders the notification as markdown and sends it to the given room,
// returning the event ID of the delivered message. Sending to an
// unencrypted room is a hard error.
func (b *Bot) Send(ctx context.Context, roomID string, n notify.Notification) (string, error) {
	start := time.Now()
	eventID, err := b.sendMarkdown(ctx, id.RoomID(roomID), BuildMarkdown(n))
	if err != nil {
		return "", err
	}
	metrics.SendDuration.Observe(time.Since(start).Seconds())
	b.delivered.Add(1)
	return eventID, nil
}

// SendEdit replaces an earlier message with the rendered notification via an
// m.replace relation — used to flip a firing alert message into its resolved
// form in place. The target must be a plain text message the bot sent.
func (b *Bot) SendEdit(ctx context.Context, roomID, targetEventID string, n notify.Notification) error {
	rid := id.RoomID(roomID)
	encrypted, err := b.roomEncrypted(ctx, rid)
	if err != nil {
		return fmt.Errorf("checking room encryption: %w", err)
	}
	if !encrypted {
		return fmt.Errorf("room %s is not encrypted (or the bot has not joined it): refusing to send", rid)
	}
	start := time.Now()
	content := format.RenderMarkdown(BuildMarkdown(n), true, false)
	content.SetEdit(id.EventID(targetEventID))
	if err := b.retrySend(ctx, func() error {
		_, err := b.client.SendMessageEvent(ctx, rid, event.EventMessage, &content)
		return err
	}); err != nil {
		return err
	}
	metrics.SendDuration.Observe(time.Since(start).Seconds())
	b.delivered.Add(1)
	return nil
}

// sendMarkdown delivers a markdown message to a room, returning the sent
// event ID and refusing if the room is not encrypted. The network send is
// retried to ride out transient failures (e.g. a homeserver restart).
func (b *Bot) sendMarkdown(ctx context.Context, roomID id.RoomID, md string) (string, error) {
	if md == "" {
		return "", nil
	}
	encrypted, err := b.roomEncrypted(ctx, roomID)
	if err != nil {
		return "", fmt.Errorf("checking room encryption: %w", err)
	}
	if !encrypted {
		return "", fmt.Errorf("room %s is not encrypted (or the bot has not joined it): refusing to send", roomID)
	}
	content := format.RenderMarkdown(md, true, false)
	var eventID string
	err = b.retrySend(ctx, func() error {
		resp, err := b.client.SendMessageEvent(ctx, roomID, event.EventMessage, &content)
		if err == nil {
			eventID = resp.EventID.String()
		}
		return err
	})
	return eventID, err
}

// retrySend runs fn up to sendMaxAttempts times with linear backoff. It is
// for transient Matrix/network errors only; the caller must have already
// rejected non-retryable conditions (e.g. an unencrypted room). Context
// cancellation aborts immediately.
func (b *Bot) retrySend(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 1; attempt <= sendMaxAttempts; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("sending message: %w", err)
		}
		if attempt < sendMaxAttempts {
			metrics.SendRetries.Inc()
			logging.From(ctx).Warn("send failed, retrying", "attempt", attempt, "error", err)
			base := b.retryBase
			if base == 0 {
				base = sendRetryBase
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("sending message: %w", err)
			case <-time.After(time.Duration(attempt) * base):
			}
		}
	}
	return fmt.Errorf("sending message after %d attempts: %w", sendMaxAttempts, err)
}

// SendWithImage delivers a notification as a single m.image event carrying
// the rendered notification text as its caption (MSC2530: filename holds the
// file name, body/formatted_body become the caption). The PNG is AES-CTR
// encrypted client-side before upload; the decryption key travels inside the
// megolm-encrypted event. Refuses unencrypted rooms like every send path.
func (b *Bot) SendWithImage(ctx context.Context, roomID string, n notify.Notification, filename string, png []byte) (string, error) {
	rid := id.RoomID(roomID)
	encrypted, err := b.roomEncrypted(ctx, rid)
	if err != nil {
		return "", fmt.Errorf("checking room encryption: %w", err)
	}
	if !encrypted {
		return "", fmt.Errorf("room %s is not encrypted (or the bot has not joined it): refusing to send", rid)
	}

	start := time.Now()
	size := len(png)
	file := attachment.NewEncryptedFile()
	file.EncryptInPlace(png)
	upload, err := b.client.UploadBytes(ctx, png, "application/octet-stream")
	if err != nil {
		return "", fmt.Errorf("uploading encrypted attachment: %w", err)
	}

	caption := format.RenderMarkdown(BuildMarkdown(n), true, false)
	content := &event.MessageEventContent{
		MsgType:       event.MsgImage,
		FileName:      filename,
		Body:          caption.Body,
		Format:        caption.Format,
		FormattedBody: caption.FormattedBody,
		Info: &event.FileInfo{
			MimeType: "image/png",
			Size:     size,
		},
		File: &event.EncryptedFileInfo{
			EncryptedFile: *file,
			URL:           upload.ContentURI.CUString(),
		},
	}
	var eventID string
	if err := b.retrySend(ctx, func() error {
		resp, err := b.client.SendMessageEvent(ctx, rid, event.EventMessage, content)
		if err == nil {
			eventID = resp.EventID.String()
		}
		return err
	}); err != nil {
		return "", err
	}
	metrics.SendDuration.Observe(time.Since(start).Seconds())
	b.delivered.Add(1)
	return eventID, nil
}

// Healthy reports whether the bot is operational: logged in and syncing
// recently. Used by the /health endpoint so a stalled sync is visible to
// traefik and docker, not just the fatal-exit path.
func (b *Bot) Healthy() (bool, string) {
	if b.client.UserID == "" {
		return false, "not logged in"
	}
	unix := b.lastSyncUnix.Load()
	if unix == 0 {
		return false, "no sync yet"
	}
	age := time.Since(time.Unix(unix, 0))
	if age > syncStaleThreshold {
		return false, fmt.Sprintf("last sync %s ago", age.Round(time.Second))
	}
	return true, "ok"
}

// BuildMarkdown converts a notification into the markdown that gets rendered
// into the Matrix message.
func BuildMarkdown(n notify.Notification) string {
	var sb strings.Builder
	if n.Title != "" {
		if n.Priority >= 8 {
			sb.WriteString("‼️ ")
		}
		sb.WriteString("**")
		sb.WriteString(n.Title)
		sb.WriteString("**")
	}
	if n.Body != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(n.Body)
	}
	return sb.String()
}

// Close releases the crypto store.
func (b *Bot) Close() error {
	return b.helper.Close()
}

func loadOrCreatePickleKey(path string) ([]byte, error) {
	if raw, err := os.ReadFile(path); err == nil {
		key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil {
			return nil, fmt.Errorf("decoding pickle key %s: %w", path, err)
		}
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("reading pickle key: %w", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generating pickle key: %w", err)
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("persisting pickle key: %w", err)
	}
	return key, nil
}

func mautrixLogger(level string) zerolog.Logger {
	lvl := zerolog.WarnLevel
	if level == "debug" {
		lvl = zerolog.DebugLevel
	}
	return zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.Kitchen}).
		Level(lvl).With().Timestamp().Str("component", "mautrix").Logger()
}

var _ notify.Sender = (*Bot)(nil)
