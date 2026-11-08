// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/aws/device-connect-devicefarm/internal/bridge"
	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/session"
)

// refreshSuite exercises the daemon's single-tick endpoint refresh. SetupTest
// builds a fake remote-access client and a DaemonManager wired to it; each test
// arranges the attached device and the client's next return, then calls
// refreshEndpointOnce directly (no ticker) and asserts the device state.
type refreshSuite struct {
	suite.Suite
	client *fakeRemoteAccessClient
	mgr    *DaemonManager
	device *bridge.ActiveDevice
}

func (s *refreshSuite) SetupTest() {
	s.client = newFakeClient(testEndpoint, "Pixel 10 (OS 17)")
	s.mgr = newTestManagerWithClient(s.T(), config.Default(), s.client)
	s.device = bridge.NewActiveDevice()
}

func TestRefreshSuite(t *testing.T) {
	suite.Run(t, new(refreshSuite))
}

func (s *refreshSuite) TestAttachedDeviceRefreshedUpdatesEndpointInPlace() {
	s.device.Set(bridge.DeviceInfo{ARN: "arn:1", Endpoint: testEndpoint, Label: "Pixel 10 (OS 17)"})
	s.client.set(testEndpointRenewed, nil)

	refreshed := s.mgr.refreshEndpointOnce(context.Background(), s.device)

	s.Require().True(refreshed, "an attached device must be refreshed")
	current, present := s.device.Current()
	s.Require().True(present, "the device must stay attached across a successful refresh")
	s.Equal(testEndpointRenewed, current.Endpoint, "the renewed endpoint must replace the old one")
	s.Equal("arn:1", current.ARN)
}

func (s *refreshSuite) TestNoDeviceAttachedRefreshIsNoOpAndDoesNotResolve() {
	refreshed := s.mgr.refreshEndpointOnce(context.Background(), s.device)

	s.False(refreshed, "with no device attached there is nothing to refresh")
	s.Empty(s.client.resolvedARNs(), "no device means no AWS call")
}

func (s *refreshSuite) TestRefreshFailsDeviceDetached() {
	s.device.Set(bridge.DeviceInfo{ARN: "arn:1", Endpoint: testEndpoint, Label: "Pixel 10 (OS 17)"})
	s.client.set("", errors.New("session ended"))

	refreshed := s.mgr.refreshEndpointOnce(context.Background(), s.device)

	s.False(refreshed)
	_, present := s.device.Current()
	s.False(present, "a session that can no longer be resolved must be detached")
}

func (s *refreshSuite) TestRefreshFailsTheDetachRecordsTheVerdictsReason() {
	tests := []struct {
		name       string
		err        error
		wantReason string
	}{
		{name: "a dead session", err: &session.SessionEndedError{SessionARN: "arn:1", Status: "COMPLETED"},
			wantReason: "remote access session arn:1 has ended (COMPLETED); start a new remote access session, then run " +
				"'devicefarm-connect device add --session-arn <new ARN>'"},
		{name: "a transient failure still detaches on the tick", err: errors.New("connection reset"),
			wantReason: "the bridge could not look up remote access session arn:1: connection reset; " +
				"run 'devicefarm-connect device add --session-arn arn:1' to attach it again"},
	}
	for _, tc := range tests {
		s.Run(tc.name, func() {
			device := bridge.NewActiveDevice()
			device.Set(bridge.DeviceInfo{ARN: "arn:1", Endpoint: testEndpoint, Label: "Pixel 10 (OS 17)"})
			s.client.set("", tc.err)

			s.False(s.mgr.refreshEndpointOnce(context.Background(), device))

			last, detached := device.LastDetach()
			s.Require().True(detached, "the tick records why it detached")
			s.Equal(tc.wantReason, last.Reason)
		})
	}
}

func (s *refreshSuite) TestLookupSlowerThanTheAttachBoundButInsideTheRefreshBoundRefreshesTheEndpoint() {
	// The tick uses the renew margin, not the attach bound, so throttled retries still renew
	// the endpoint. The refresh bound stays far off: the fake's answer must win every time.
	shrinkLookupBounds(s.T(), 50*time.Millisecond, time.Minute)
	s.device.Set(bridge.DeviceInfo{ARN: "arn:1", Endpoint: testEndpoint, Label: "Pixel 10 (OS 17)"})
	s.client.set(testEndpointRenewed, nil)
	s.client.delayAnswersBy(200 * time.Millisecond)

	refreshed := s.mgr.refreshEndpointOnce(context.Background(), s.device)

	s.Require().True(refreshed, "a lookup past the attach bound but inside the refresh bound must refresh")
	current, present := s.device.Current()
	s.Require().True(present)
	s.Equal(testEndpointRenewed, current.Endpoint)
}

func (s *refreshSuite) TestLookupPastTheRefreshBoundFailsTheTickAndDetaches() {
	// The attach bound is long and the refresh bound short, so only the refresh bound can end
	// this lookup; a tick on the attach bound would wait out the fake.
	shrinkLookupBounds(s.T(), time.Minute, 200*time.Millisecond)
	s.device.Set(bridge.DeviceInfo{ARN: "arn:1", Endpoint: testEndpoint, Label: "Pixel 10 (OS 17)"})
	s.client.set(testEndpointRenewed, nil)
	s.client.delayAnswersBy(time.Minute)
	started := time.Now()

	refreshed := s.mgr.refreshEndpointOnce(context.Background(), s.device)

	s.False(refreshed)
	s.Less(time.Since(started), 10*time.Second, "the refresh bound ends the lookup, not the fake's delay")
	_, present := s.device.Current()
	s.False(present, "a lookup past the refresh bound fails the tick, which detaches")
	last, detached := s.device.LastDetach()
	s.Require().True(detached)
	s.Contains(last.Reason, "deadline exceeded")
}

func (s *refreshSuite) TestContextCancelledRefreshFailsDeviceKept() {
	s.device.Set(bridge.DeviceInfo{ARN: "arn:1", Endpoint: testEndpoint, Label: "Pixel 10 (OS 17)"})
	s.client.set("", errors.New("context canceled"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	refreshed := s.mgr.refreshEndpointOnce(ctx, s.device)

	s.False(refreshed)
	_, present := s.device.Current()
	s.True(present, "a cancelled context is daemon shutdown, not a dead session; the device must be kept")
}
