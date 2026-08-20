package matrix

import (
	"context"
	"fmt"
	"slices"

	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/thomas-maurice/tocsin/internal/logging"
	"github.com/thomas-maurice/tocsin/internal/metrics"
)

// keyBootstrapCrypto wraps the crypto helper so every room encryption first
// makes sure the recipients' device keys are known. It is installed as
// Client.Crypto, the single choke point every room send passes through, so
// no call site can forget the check.
type keyBootstrapCrypto struct {
	*cryptohelper.CryptoHelper
	bot *Bot
}

func (c *keyBootstrapCrypto) Encrypt(ctx context.Context, roomID id.RoomID, evtType event.Type, content any) (*event.EncryptedEventContent, error) {
	if err := c.bot.ensureRecipientKeys(ctx, roomID); err != nil {
		return nil, err
	}
	return c.CryptoHelper.Encrypt(ctx, roomID, evtType, content)
}

// ensureRecipientKeys makes sure every member of roomID has known device
// keys before a megolm session is shared with them, and refuses the send
// outright when the session would reach nobody.
//
// mautrix-go learns other users' devices only from device_lists.changed in
// /sync, and that handler queries keys for already-tracked users only. On
// the share path, OlmMachine.ShareGroupSession refetches keys only when the
// crypto store has never heard of a user at all (GetDevices returns nil for
// an untracked user); a user that is tracked but whose device list is empty
// is logged as "user has no devices, skipping" and silently dropped. The
// session is then marked shared, persisted, and reused for every later
// message — so one missed key share turns into weeks of "Unable to decrypt
// message" while every send still reports success.
func (b *Bot) ensureRecipientKeys(ctx context.Context, roomID id.RoomID) error {
	log := logging.From(ctx)
	members, err := b.client.StateStore.GetRoomJoinedOrInvitedMembers(ctx, roomID)
	if err != nil {
		return fmt.Errorf("listing members of %s: %w", roomID, err)
	}
	if len(members) == 0 {
		// The share path would encrypt for nobody and still succeed. An
		// encrypted room the bot can send to always has at least the bot in
		// it, so an empty list means the state store never got the member
		// list, not that the room is empty.
		metrics.KeyShareEmpty.Inc()
		return fmt.Errorf("refusing to send to %s: the state store knows no members of the room", roomID)
	}
	known, err := b.knownDevices(ctx, members)
	if err != nil {
		return err
	}
	if missing := usersNeedingKeys(known); len(missing) > 0 {
		log.Debug("querying device keys for room members with none known", "room_id", roomID, "users", missing)
		// includeUntracked so users the crypto store has forgotten are
		// queried too — that is the state the share path cannot recover
		// from on its own.
		if _, err := b.helper.Machine().FetchKeys(ctx, missing, true); err != nil {
			return fmt.Errorf("querying device keys for %v: %w", missing, err)
		}
		if known, err = b.knownDevices(ctx, members); err != nil {
			return err
		}
	}

	reach, empty := checkRecipients(known, b.client.UserID, b.client.DeviceID)
	if reach == 0 {
		metrics.KeyShareEmpty.Inc()
		return fmt.Errorf("refusing to send to %s: no recipient devices known (members without devices: %v), the message would be undecryptable", roomID, empty)
	}
	if len(empty) > 0 {
		log.Warn("room members have no devices and will not be able to decrypt",
			"room_id", roomID, "users", empty, "recipient_devices", reach)
	}
	log.Debug("recipient devices resolved", "room_id", roomID, "recipient_devices", reach)
	return b.dropUnsharedSession(ctx, roomID, known)
}

// dropUnsharedSession discards a persisted outbound megolm session that never
// reached a single device, so the next encrypt creates and shares a fresh one.
//
// Checking the recipients' keys only prevents *new* poisoned sessions.
// ShareGroupSession sets Shared = true and stores the session even when the
// to-device payload was empty, and the store outlives restarts, so a session
// poisoned before this check existed would keep being reused — every message
// in it undecryptable — until it hit its expiry. mautrix records which
// devices actually received a session in crypto_megolm_outbound_session_shared;
// a stored session with no row there reached nobody.
func (b *Bot) dropUnsharedSession(ctx context.Context, roomID id.RoomID, known map[id.UserID]map[id.DeviceID]*id.Device) error {
	mach := b.helper.Machine()
	if mach.DisableSharedGroupSessionTracking {
		// Nothing populates the shared-with table, so its emptiness would
		// say nothing and we would re-share on every single send.
		return nil
	}
	store := mach.CryptoStore
	session, err := store.GetOutboundGroupSession(ctx, roomID)
	if err != nil {
		return fmt.Errorf("reading outbound group session for %s: %w", roomID, err)
	}
	if session == nil || !session.Shared || session.Expired() {
		// No session, or one that is going to be re-shared anyway.
		return nil
	}
	for user, devices := range known {
		for deviceID, device := range devices {
			if user == b.client.UserID && deviceID == b.client.DeviceID {
				continue
			}
			shared, err := store.IsOutboundGroupSessionShared(ctx, user, device.IdentityKey, session.ID())
			if err != nil {
				return fmt.Errorf("checking whether session %s reached %s: %w", session.ID(), deviceID, err)
			}
			if shared {
				return nil
			}
		}
	}

	logging.From(ctx).Warn("stored megolm session reached no known device, discarding it so a fresh one is shared",
		"room_id", roomID, "session_id", session.ID())
	metrics.SessionsDiscarded.Inc()
	if err := store.RemoveOutboundGroupSession(ctx, roomID); err != nil {
		return fmt.Errorf("discarding unshared outbound session for %s: %w", roomID, err)
	}
	return nil
}

// knownDevices reads the crypto store's device list for each member. A nil
// entry means the user was never tracked, an empty one means the user is
// tracked but has no usable devices; both are invisible to the share path.
func (b *Bot) knownDevices(ctx context.Context, members []id.UserID) (map[id.UserID]map[id.DeviceID]*id.Device, error) {
	store := b.helper.Machine().CryptoStore
	known := make(map[id.UserID]map[id.DeviceID]*id.Device, len(members))
	for _, user := range members {
		devices, err := store.GetDevices(ctx, user)
		if err != nil {
			return nil, fmt.Errorf("reading devices of %s: %w", user, err)
		}
		known[user] = devices
	}
	return known, nil
}

// usersNeedingKeys returns the members whose device list is empty in the
// crypto store, i.e. the ones a /keys/query has to cover before sharing.
// Sorted so logs and errors are stable.
func usersNeedingKeys(known map[id.UserID]map[id.DeviceID]*id.Device) []id.UserID {
	var users []id.UserID
	for user, devices := range known {
		if len(devices) == 0 {
			users = append(users, user)
		}
	}
	slices.Sort(users)
	return users
}

// checkRecipients reports how many devices a megolm session shared now would
// reach, and which members it would not reach at all.
//
// The bot's own sending device is excluded from the tally: it already holds
// the outbound session, so counting it would let a send that no other device
// can read look healthy — precisely how a share to zero recipients stayed
// invisible. The bot's own user is never listed as unreachable for the same
// reason.
func checkRecipients(known map[id.UserID]map[id.DeviceID]*id.Device, ownUser id.UserID, ownDevice id.DeviceID) (reach int, empty []id.UserID) {
	for user, devices := range known {
		count := len(devices)
		if user == ownUser {
			if _, ok := devices[ownDevice]; ok {
				count--
			}
		}
		if count == 0 {
			if user != ownUser {
				empty = append(empty, user)
			}
			continue
		}
		reach += count
	}
	slices.Sort(empty)
	return reach, empty
}
