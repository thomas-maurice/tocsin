package matrix

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"maunium.net/go/mautrix/id"
)

const (
	testOwnUser   = id.UserID("@notifier:example.org")
	testOwnDevice = id.DeviceID("WHPXDOFMWC")
)

func TestCheckRecipientsIgnoresOwnSendingDevice(t *testing.T) {
	// The bot's own device already holds the outbound session, so counting
	// it would make a share that reached nobody else look healthy — the
	// exact failure that hid undecryptable notifications for weeks.
	known := map[id.UserID]map[id.DeviceID]*id.Device{
		testOwnUser:         {testOwnDevice: {}},
		"@peer:example.org": {},
	}
	reach, empty := checkRecipients(known, testOwnUser, testOwnDevice)
	assert.Zero(t, reach, "a session only the sender can read reaches nobody")
	assert.Equal(t, []id.UserID{"@peer:example.org"}, empty)
}

func TestCheckRecipientsCountsRealDevices(t *testing.T) {
	known := map[id.UserID]map[id.DeviceID]*id.Device{
		testOwnUser:           {testOwnDevice: {}, "SECOND": {}},
		"@peer:example.org":   {"AAA": {}, "BBB": {}},
		"@lurker:example.org": nil, // never tracked by the crypto store
	}
	reach, empty := checkRecipients(known, testOwnUser, testOwnDevice)
	// Own second device counts (it is a real recipient), own sender does not.
	assert.Equal(t, 3, reach)
	// A member with no devices is reported even though the send can proceed:
	// they silently miss every notification otherwise.
	assert.Equal(t, []id.UserID{"@lurker:example.org"}, empty)
}

func TestCheckRecipientsNeverFlagsOwnUser(t *testing.T) {
	// The bot not being able to decrypt its own message is not a fault to
	// warn about, so it must not show up as an unreachable member.
	known := map[id.UserID]map[id.DeviceID]*id.Device{
		testOwnUser:         {testOwnDevice: {}},
		"@peer:example.org": {"AAA": {}},
	}
	reach, empty := checkRecipients(known, testOwnUser, testOwnDevice)
	assert.Equal(t, 1, reach)
	assert.Empty(t, empty)
}

func TestUsersNeedingKeys(t *testing.T) {
	// Untracked (nil) and tracked-but-empty must be treated the same: the
	// share path skips both silently, so both need an explicit /keys/query.
	known := map[id.UserID]map[id.DeviceID]*id.Device{
		"@zed:example.org":   nil,
		"@abe:example.org":   {},
		"@known:example.org": {"AAA": {}},
	}
	assert.Equal(t,
		[]id.UserID{"@abe:example.org", "@zed:example.org"},
		usersNeedingKeys(known),
		"sorted so log lines and errors are stable")

	assert.Empty(t, usersNeedingKeys(map[id.UserID]map[id.DeviceID]*id.Device{
		"@known:example.org": {"AAA": {}},
	}))
}
