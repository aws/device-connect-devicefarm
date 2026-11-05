package cli

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/control"
)

const detachReason = "remote access session arn:aws:devicefarm:us-west-2:1:session:a/b/0 no longer exists; " +
	"start a new session and run 'devicefarm-connect device add --session-arn <new ARN>'"

// detachedReply is a no-device reply carrying a detach three minutes old.
func detachedReply() control.Response {
	return control.Response{OK: true, LastDetachReason: detachReason, LastDetachAt: time.Now().Add(-3 * time.Minute)}
}

func TestGivenADaemonThatDetachedADeviceWhenStatusInvokedThenTheLineEndsWithTheReason(t *testing.T) {
	fakeDaemon(t, func(control.Request) control.Response { return detachedReply() })

	out, err := execRoot(t, "bridge", "status")

	require.NoError(t, err)
	assert.Contains(t, out.String(), "no device attached; the last device detached 3 minutes ago: "+detachReason+"\n")
}

func TestGivenADaemonThatDetachedADeviceWhenListDevicesInvokedThenTheReasonFollowsTheNoDeviceLine(t *testing.T) {
	fakeDaemon(t, func(control.Request) control.Response { return detachedReply() })

	out, err := execRoot(t, "device", "list")

	require.NoError(t, err)
	assert.Equal(t, "no device attached; use 'device add' to connect one\n"+
		"the last device detached 3 minutes ago: "+detachReason+"\n", out.String())
}

func TestGivenAgesWhenRenderedThenTheLargestWholeUnitUpToHoursIsUsed(t *testing.T) {
	tests := []struct {
		age  time.Duration
		want string
	}{
		{age: -time.Second, want: "0 seconds"},
		{age: time.Second, want: "1 second"},
		{age: 42 * time.Second, want: "42 seconds"},
		{age: time.Minute, want: "1 minute"},
		{age: 59*time.Minute + 59*time.Second, want: "59 minutes"},
		{age: 2*time.Hour + 30*time.Minute, want: "2 hours"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			assert.Equal(t, tc.want, humanAge(tc.age))
		})
	}
}
