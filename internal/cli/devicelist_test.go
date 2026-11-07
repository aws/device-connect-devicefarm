package cli

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/aws/device-connect-devicefarm/internal/control"
)

func TestGivenSpansWhenRenderedThenTheMinutesFollowTheHours(t *testing.T) {
	tests := []struct {
		span time.Duration
		want string
	}{
		{span: -time.Second, want: "0 seconds"},
		{span: 59*time.Minute + 59*time.Second, want: "59 minutes"},
		{span: time.Hour, want: "1 hour"},
		{span: time.Hour + time.Minute, want: "1 hour 1 minute"},
		{span: 2*time.Hour - time.Second, want: "1 hour 59 minutes"},
		{span: 2*time.Hour + 5*time.Minute + 59*time.Second, want: "2 hours 5 minutes"},
		{span: 3 * time.Hour, want: "3 hours"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			assert.Equal(t, tc.want, humanSpan(tc.span))
		})
	}
}

func TestGivenAnAttachedDeviceWhenTheLineIsRenderedThenEveryColumnIsWorded(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	attached := control.AttachedDevice{SessionARN: "arn:1", Label: "Pixel 9 (OS 16)", Serial: "R3CN30ABCDE",
		AttachedAt: now.Add(-90 * time.Second), SessionStartedAt: now.Add(-time.Hour - 10*time.Minute),
		SessionExpiresAt: now.Add(80 * time.Minute)}

	assert.Equal(t, "arn:1\tPixel 9 (OS 16)\tserial R3CN30ABCDE\tattached 1 minute ago\t"+
		"session started 1 hour 10 minutes ago\tsession expires in 1 hour 20 minutes", deviceListLine(attached, now))
}

func TestGivenMissingOrPassedValuesWhenTheLineIsRenderedThenTheColumnsSaySo(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		attached control.AttachedDevice
		want     string
	}{
		{name: "no session times", attached: control.AttachedDevice{AttachedAt: now},
			want: "attached 0 seconds ago\tsession started unknown\tsession expiry unknown"},
		{name: "expiry passed", attached: control.AttachedDevice{AttachedAt: now,
			SessionStartedAt: now.Add(-151 * time.Minute), SessionExpiresAt: now.Add(-time.Minute)},
			want: "attached 0 seconds ago\tsession started 2 hours 31 minutes ago\tsession expired"},
		{name: "expiry now", attached: control.AttachedDevice{AttachedAt: now, SessionExpiresAt: now},
			want: "attached 0 seconds ago\tsession started unknown\tsession expired"},
		{name: "no times at all", attached: control.AttachedDevice{},
			want: "attached unknown\tsession started unknown\tsession expiry unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, "\t\tserial unknown\t"+tc.want, deviceListLine(tc.attached, now))
		})
	}
}
