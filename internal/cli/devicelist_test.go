// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/control"
	"github.com/aws/device-connect-devicefarm/internal/report"
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

func TestGivenAttachedDevicesWhenTabledThenTheSessionCellHoldsTheStartAndTheExpiryAndMissingValuesSaySo(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	devices := []control.AttachedDevice{
		{SessionARN: "arn:1", Label: "Pixel 9 (OS 16)", Serial: "R3CN30ABCDE", AttachedAt: now.Add(-90 * time.Second),
			SessionStartedAt: now.Add(-time.Hour - 10*time.Minute), SessionExpiresAt: now},
		{Label: "Galaxy S9 (OS 9)"},
	}

	assert.Equal(t, [][]report.Cell{
		{{"Pixel 9 (OS 16)"}, {"R3CN30ABCDE"}, {"1 minute ago"}, {"started 1 hour 10 minutes ago", "expired"}},
		{{"Galaxy S9 (OS 9)"}, {"unknown"}, {"unknown"}, {"started unknown", "expiry unknown"}},
	}, deviceTable(devices, now).Rows)
}

func TestGivenDaemonRepliesWhenRenderedAsJSONThenDevicesIsAlwaysAnArrayAndUnknownTimesAndLastDetachAppearOnlyWhenKnown(t *testing.T) {
	// README contract: `device list --json` fields. Do not break.
	attachedAt := time.Date(2026, 6, 26, 11, 48, 0, 0, time.UTC)
	tests := []struct {
		name     string
		response control.Response
		want     string
	}{
		{name: "one device", response: control.Response{OK: true, Devices: []control.AttachedDevice{
			{SessionARN: "arn:aws:devicefarm:us-west-2:1:session:a/b/0", Label: "Google Pixel 9 (OS 16)", Serial: "R3CN30ABCDE",
				AttachedAt: attachedAt, SessionStartedAt: attachedAt.Add(-13 * time.Minute), SessionExpiresAt: attachedAt.Add(137 * time.Minute)}}},
			want: `{"devices":[{"sessionArn":"arn:aws:devicefarm:us-west-2:1:session:a/b/0","label":"Google Pixel 9 (OS 16)",` +
				`"serial":"R3CN30ABCDE","attachedAt":"2026-06-26T11:48:00Z","sessionStartedAt":"2026-06-26T11:35:00Z",` +
				`"sessionExpiresAt":"2026-06-26T14:05:00Z"}]}`},
		{name: "a device whose session times the daemon does not know", response: control.Response{OK: true, Devices: []control.AttachedDevice{
			{SessionARN: "arn:aws:devicefarm:us-west-2:1:session:a/b/0", Label: "Google Pixel 9 (OS 16)", Serial: "R3CN30ABCDE", AttachedAt: attachedAt}}},
			want: `{"devices":[{"sessionArn":"arn:aws:devicefarm:us-west-2:1:session:a/b/0","label":"Google Pixel 9 (OS 16)",` +
				`"serial":"R3CN30ABCDE","attachedAt":"2026-06-26T11:48:00Z"}]}`},
		{name: "no device", response: control.Response{OK: true}, want: `{"devices":[]}`},
		{name: "no device after a detach", response: control.Response{OK: true, LastDetachReason: detachReason, LastDetachAt: attachedAt},
			want: `{"devices":[],"lastDetach":{"reason":"` + detachReason + `","at":"2026-06-26T11:48:00Z"}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeDaemon(t, func(control.Request) control.Response { return tc.response })

			out, err := execRoot(t, "device", "list", "--json")

			require.NoError(t, err)
			assert.Equal(t, tc.want+"\n", out.String())
		})
	}
}
