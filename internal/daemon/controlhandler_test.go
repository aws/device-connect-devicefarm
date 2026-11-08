// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/aws/device-connect-devicefarm/internal/bridge"
	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/control"
	"github.com/aws/device-connect-devicefarm/internal/session"
	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// addReq builds an OpAdd request for a session ARN. The label is no longer carried
// on the request: the daemon derives it from the resolved session.
func addReq(arn string) control.Request {
	return control.Request{Op: control.OpAdd, ARN: arn}
}

// Commands and fixes that a bridge on the default port sends for testSessionArnOne.
const (
	fixAddCommand    = "devicefarm-connect device add --session-arn " + testSessionArnOne
	fixDoctorCommand = "devicefarm-connect doctor --session-arn " + testSessionArnOne + " --port 5037"
	// The restart, then the add, quoted.
	fixRestartThenAdd = "'devicefarm-connect bridge start --port 5037', then run '" + fixAddCommand + "'"
	fixNewSession     = "Start a new remote access session, then run 'devicefarm-connect device add --session-arn <new ARN>'."
	fixOtherDevice    = "Start a remote access session on another device, then run 'devicefarm-connect device add " +
		"--session-arn <new ARN>'. If the error remains, contact AWS Support."
	fixPassRunningARN = "Pass the ARN of a running remote access session. To list your sessions, run " +
		"'aws devicefarm list-remote-access-sessions --arn <project ARN> --region us-west-2'."
	fixCredentials = "The bridge uses the AWS credentials of the shell that started it. From a shell with working AWS " +
		"credentials that allow devicefarm:GetRemoteAccessSession, run " + fixRestartThenAdd + "."
	fixDoctorThenSupport = "Run '" + fixDoctorCommand + "'. If the doctor does not find the cause, contact AWS Support."
	fixRetryThenDoctor   = "Run '" + fixAddCommand + "' again. If the error remains, run '" + fixDoctorCommand +
		"'. If the doctor does not find the cause, contact AWS Support."
	// Ends the fix for a host the bridge cannot reach, after "... on TCP port <port>".
	fixReachTail = ". If a firewall or a proxy on your network blocks the connection, ask your network administrator to allow " +
		"direct outbound WebSocket connections (wss) to that host and port. Then run '" + fixAddCommand + "' again. '" +
		fixDoctorCommand + "' tests the connection."
	fixOtherVersion = "The running bridge is another version of devicefarm-connect than this command. Restart it: run " +
		"'devicefarm-connect bridge start' with the options that you started it with. If the error remains, install the " +
		"latest devicefarm-connect, then run 'devicefarm-connect doctor'. If the doctor does not find the cause, contact AWS Support."
)

// controlHandlerSuite exercises the daemon's control handler. SetupTest builds the shared
// fixture: a mock endpoint that answers the add's serial fetch, a fake remote-access client
// resolving to it, a DaemonManager on that client, an empty device, a dialer, and a handler
// over them with a no-op shutdown. A test that needs a special client (a failing resolve, or
// one that binds a device mid-resolve) rebuilds the handler from it.
type controlHandlerSuite struct {
	suite.Suite
	mock    *testsupport.SessionMockWSS
	client  *fakeRemoteAccessClient
	mgr     *DaemonManager
	device  *bridge.ActiveDevice
	dialer  *bridge.WSDialer
	handler control.Handler
}

func (s *controlHandlerSuite) SetupTest() {
	s.mock = testsupport.NewSessionMockWSS(s.T(), testsupport.SessionMockConfig{OnMessage: answerAttach})
	s.client = newFakeClient(s.mock.URL, "Pixel 10 (OS 17)")
	s.mgr = newTestManagerWithClient(s.T(), config.Default(), s.client)
	s.device = bridge.NewActiveDevice()
	s.dialer = s.mgr.newDialer()
	s.handler = s.mgr.controlHandler(context.Background(), s.device, s.dialer, func() {})
}

// attachedArnOne is the Devices entry the handler builds for testSessionArnOne once an add bound it.
func (s *controlHandlerSuite) attachedArnOne() []control.AttachedDevice {
	bound, _ := s.device.Current()
	return []control.AttachedDevice{{SessionARN: testSessionArnOne, Label: "Pixel 10 (OS 17)", Serial: attachSerial,
		AttachedAt: bound.AttachedAt, SessionStartedAt: bound.SessionStartedAt,
		SessionExpiresAt: bound.SessionExpiresAt}}
}

func TestControlHandlerSuite(t *testing.T) {
	suite.Run(t, new(controlHandlerSuite))
}

func (s *controlHandlerSuite) TestNoDeviceAttachedAddResolvesAttachesAndReportsLabel() {
	resp := s.handler(addReq(testSessionArnOne))

	s.Require().True(resp.OK)
	s.Equal("Pixel 10 (OS 17)", resp.DeviceLabel)
	s.Equal(s.mgr.config.ListenPort, resp.Port)
	s.Equal(testSessionArnOne, resp.ARN, "the reply names the stored ARN")
	current, present := s.device.Current()
	s.Require().True(present)
	s.Equal(testSessionArnOne, current.ARN)
	s.Equal(s.mock.URL, current.Endpoint, "the resolved endpoint must be stored verbatim")
	s.Equal(attachSerial, current.Serial, "the add must bind the device with the serial it fetched")
	s.WithinDuration(time.Now().Add(statusTimeLeft), current.SessionExpiresAt, 5*time.Second,
		"the expiry is the status read's time plus the mock's time left")
	s.Equal(current.SessionExpiresAt.Add(-session.MaxSessionLength), current.SessionStartedAt,
		"the start is the expiry minus the session length")
	s.WithinDuration(time.Now(), current.AttachedAt, time.Minute)
	s.Equal([]string{hostDevicesRequest, statusRequestFrame}, sortedInbounds(s.mock),
		"the fetch sends host:devices alone; the status read follows on its own session")
}

func (s *controlHandlerSuite) TestSerialFetchFailsTheReplyNamesWhatFailedAndTheLogKeepsTheSteps() {
	// The session resolves; the fetch at its endpoint fails. The reply names what failed. The
	// daemon log keeps the fetch's steps and the ARN. Neither carries the endpoint's query.
	closing := testsupport.NewSessionMockWSS(s.T(), testsupport.SessionMockConfig{})
	closing.CloseNewConnections(1008, "Device Connect is not supported on this device")
	refusing := testsupport.NewSessionMockWSS(s.T(), testsupport.SessionMockConfig{RefuseNewSessions: -1})
	cases := []struct {
		name     string
		endpoint string
		cause    string
		fix      string
		logged   string
	}{
		{name: "nothing listens at the endpoint", endpoint: testsupport.PresignedUnreachableEndpoint,
			cause:  "cannot connect to Device Farm at 127.0.0.1:1: dial tcp 127.0.0.1:1: connect: connection refused",
			fix:    "Check that this machine can reach 127.0.0.1 on TCP port 1" + fixReachTail,
			logged: "fetch the device serial: " + testsupport.ScrubbedDialText + "adb: dial tcp 127.0.0.1:1"},
		{name: "the service closes with a reason", endpoint: closing.URL, fix: fixOtherDevice,
			cause:  "Device Farm refused the connection to the device: Device Connect is not supported on this device (close code 1008)",
			logged: "fetch the device serial: read the devices reply: websocket closed by the peer: code 1008"},
		{name: "the server refuses the session", endpoint: refusing.URL, fix: fixRetryThenDoctor,
			cause:  "Device Farm refused the connection to the device: " + testsupport.NewSessionRefusalReason,
			logged: "fetch the device serial: the remote access service refused the session: " + testsupport.NewSessionRefusalReason},
	}
	for _, testCase := range cases {
		s.Run(testCase.name, func() {
			s.client.set(testCase.endpoint, nil)
			logs := &lockedBuffer{}
			s.mgr.logger = slog.New(slog.NewTextHandler(logs, nil))

			resp := s.handler(addReq(testSessionArnOne))

			s.False(resp.OK, "a failed serial fetch must fail the add")
			s.Equal(testCase.cause, resp.Error)
			s.Equal([]string{testCase.fix}, resp.Fixes)
			s.Contains(logs.String(), testCase.logged)
			s.Contains(logs.String(), "arn="+testSessionArnOne)
			for _, sink := range []string{resp.Error, logs.String()} {
				s.NotContains(sink, testsupport.SignatureMarker)
				s.NotContains(sink, "X-Amz-")
			}
			_, present := s.device.Current()
			s.False(present, "nothing must be attached when the serial fetch fails")
		})
	}
}

func (s *controlHandlerSuite) TestSerialFetchMeetsTheSessionStoppingCloseAddFailsWithTheStoppingCauseAndItsFixApart() {
	// The relay closes the fetch's websocket with the stopping code; its cause replaces the fetch's error.
	s.mock.CloseNewConnections(4001, "remote access session is stopping")
	logs := &lockedBuffer{}
	s.mgr.logger = slog.New(slog.NewTextHandler(logs, nil))

	resp := s.handler(addReq(testSessionArnOne))

	s.False(resp.OK)
	s.Equal("remote access session "+testSessionArnOne+" is stopping", resp.Error, "the cause carries no fix text")
	s.Equal([]string{fixNewSession}, resp.Fixes, "the fix of an ended session")
	s.Contains(logs.String(), "fetch the device serial: read the devices reply: websocket closed by the peer: code 4001",
		"the log keeps the close as the fetch read it")
	_, present := s.device.Current()
	s.False(present, "nothing must be attached when the session is stopping")
}

func (s *controlHandlerSuite) TestSerialFetchMeetsTheSessionStoppingCloseUnderAReturnedArnTheCauseNamesTheReturnedArn() {
	s.client.returnArnFor(testSessionArnOne, testSessionArnTwo)
	s.client.returnArnFor(testSessionArnTwo, testSessionArnTwo)
	s.mock.CloseNewConnections(4001, "remote access session is stopping")

	resp := s.handler(addReq(testSessionArnOne))

	s.False(resp.OK)
	s.Equal("remote access session "+testSessionArnTwo+" is stopping", resp.Error,
		"the cause names the ARN the service returned")
}

func (s *controlHandlerSuite) TestSerialFetchHangsAddFailsAtTheResolveTimeoutAndNothingAttached() {
	// A server that never answers host:devices must not hold the inline control loop past
	// resolveTimeout, which sits under the control channel's deadline.
	silent := testsupport.NewSessionMockWSS(s.T(), testsupport.SessionMockConfig{
		OnMessage: func(_ *testsupport.MockSession, _ []byte) []byte { return nil },
	})
	s.client.set(silent.URL, nil)
	shrinkLookupBounds(s.T(), 200*time.Millisecond, refreshLookupTimeout)
	started := time.Now()

	resp := s.handler(addReq(testSessionArnOne))

	s.False(resp.OK, "a hung fetch must fail the add")
	s.Equal("the attach ran out of time before the device answered", resp.Error)
	s.Equal([]string{fixRetryThenDoctor}, resp.Fixes)
	s.Less(time.Since(started), 2*time.Second, "the add must end at resolveTimeout, not at the reply timeout")
	_, present := s.device.Current()
	s.False(present)
}

func (s *controlHandlerSuite) TestSessionLookupOutlivesTheResolveTimeoutAddFailsWithTheTimeoutCauseAndTheRetryFix() {
	// The SDK's retries under a throttle can outlive the attach bound, and its canceled backoff
	// drops the throttle text, so the add words the timeout for the user, inside the bound.
	shrinkLookupBounds(s.T(), 200*time.Millisecond, refreshLookupTimeout)
	s.client.delayAnswersBy(time.Minute)
	started := time.Now()

	resp := s.handler(addReq(testSessionArnOne))

	s.False(resp.OK, "a lookup past the bound must fail the add")
	s.Equal("Device Farm did not answer the session lookup within 200ms", resp.Error)
	s.Equal([]string{fixRetryThenDoctor}, resp.Fixes)
	s.Less(time.Since(started), 10*time.Second, "the add ends at resolveTimeout, not at the fake's delay")
	_, present := s.device.Current()
	s.False(present)
}

func (s *controlHandlerSuite) TestDaemonShutdownDuringTheLookupAddFailsWithoutTheTimeoutWording() {
	// A lookup the daemon context ends is a shutdown, not a timeout: the raw error stays.
	s.client.delayAnswersBy(time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	handler := s.mgr.controlHandler(ctx, s.device, s.dialer, func() {})

	resp := handler(addReq(testSessionArnOne))

	s.False(resp.OK)
	s.Equal("context canceled", resp.Error)
	_, present := s.device.Current()
	s.False(present)
}

func (s *controlHandlerSuite) TestDeviceListNamesNoDeviceAddRejectedAndNothingAttached() {
	emptyList := testsupport.NewSessionMockWSS(s.T(), testsupport.SessionMockConfig{
		OnMessage: func(_ *testsupport.MockSession, _ []byte) []byte { return []byte("OKAY0000") },
	})
	s.client.set(emptyList.URL, nil)

	resp := s.handler(addReq(testSessionArnOne))

	s.False(resp.OK, "a device list naming no device must fail the add")
	s.Equal("the remote adb server lists no device", resp.Error)
	s.Equal([]string{fixDoctorThenSupport}, resp.Fixes)
	_, present := s.device.Current()
	s.False(present, "nothing must be attached without a serial")
}

func (s *controlHandlerSuite) TestResolutionFailsAddRejectedAndNothingAttached() {
	// A malformed ARN and an ended session both reach the daemon the same way:
	// RemoteAccessClient.GetSession returns an error. The error must surface as a
	// failed add, never a nil-endpoint attach.
	s.client.set("", errors.New("session not RUNNING"))
	logs := &lockedBuffer{}
	s.mgr.logger = slog.New(slog.NewTextHandler(logs, nil))

	resp := s.handler(addReq(testSessionArnOne))

	s.False(resp.OK, "a failed resolution must fail the add")
	s.Equal("session not RUNNING", resp.Error, "the reply is the session error alone")
	s.Contains(logs.String(), "resolve session: session not RUNNING", "the log names the step")
	_, present := s.device.Current()
	s.False(present, "nothing must be attached when resolution fails")
}

func (s *controlHandlerSuite) TestDeviceAttachedMidResolveAddRejectedAndNewerDeviceKept() {
	// The defense-in-depth branch: control commands are serialized today, so
	// nothing should attach between the limit check and Set, but a future
	// concurrent caller could. Model it by binding a device from inside the resolve
	// itself, after the limit check has already passed.
	racing := &attachDuringResolve{
		inner: newFakeClient(s.mock.URL, "Pixel 10 (OS 17)"),
		attach: func() {
			s.Require().True(s.device.Set(bridge.DeviceInfo{ARN: testSessionArnOther, Label: "Other"}))
		},
	}
	mgr := newTestManagerWithClient(s.T(), config.Default(), racing)
	handler := mgr.controlHandler(context.Background(), s.device, s.dialer, func() {})

	resp := handler(addReq(testSessionArnOne))

	s.False(resp.OK, "a device bound mid-resolve must fail the add")
	s.Contains(resp.Error, "already attached (Other)")
	current, present := s.device.Current()
	s.Require().True(present)
	s.Equal(testSessionArnOther, current.ARN, "the device that won the race must stay attached")
}

func (s *controlHandlerSuite) TestDifferentDeviceAlreadyAttachedAddRejectedWithoutResolving() {
	s.Require().True(s.handler(addReq(testSessionArnOne)).OK)

	resp := s.handler(addReq(testSessionArnTwo))

	s.False(resp.OK, "the one-device limit must reject adding a different device")
	s.Equal("a different device is already attached (Pixel 10 (OS 17))", resp.Error)
	s.Equal([]string{"Remove that device with 'devicefarm-connect device remove', then run 'devicefarm-connect device add " +
		"--session-arn " + testSessionArnTwo + "' again."}, resp.Fixes)
	current, _ := s.device.Current()
	s.Equal(testSessionArnOne, current.ARN, "the first device must stay attached")
	s.Equal([]string{testSessionArnOne}, s.client.resolvedARNs(), "a rejected different device must not trigger a resolve")
}

func (s *controlHandlerSuite) TestSameDeviceAlreadyAttachedAddAgainIsIdempotentWithoutResolving() {
	s.Require().True(s.handler(addReq(testSessionArnOne)).OK)

	// Re-adding the same session ARN must succeed as a no-op rather than erroring,
	// and must report the attached device so bridge start/device add can reprint the
	// listening line, without making a second AWS call.
	resp := s.handler(addReq(testSessionArnOne))

	s.Require().True(resp.OK, "re-adding the same device must be idempotent")
	s.Equal("Pixel 10 (OS 17)", resp.DeviceLabel)
	current, present := s.device.Current()
	s.Require().True(present)
	s.Equal(testSessionArnOne, current.ARN, "the device must remain attached")
	s.Equal([]string{testSessionArnOne}, s.client.resolvedARNs(), "a re-add of the current device must not resolve again")
}

func (s *controlHandlerSuite) TestMalformedArnAddRefusedWithNoLookup() {
	resp := s.handler(addReq("not-an-arn-at-all"))

	s.False(resp.OK)
	s.Contains(resp.Error, "invalid session ARN")
	s.Equal([]string{fixPassRunningARN}, resp.Fixes)
	s.Empty(s.client.resolvedARNs(), "a refused ARN must make no service call")
	_, present := s.device.Current()
	s.False(present)
}

func (s *controlHandlerSuite) TestProjectArnAddRefusedWithNoLookup() {
	resp := s.handler(addReq("arn:aws:devicefarm:us-west-2:111122223333:project:eeeeeeee-5555-4555-a555-555555555555"))

	s.False(resp.OK)
	s.Contains(resp.Error, "is not a remote access session ARN")
	s.Empty(s.client.resolvedARNs())
}

func (s *controlHandlerSuite) TestDeviceLimitReachedMalformedArnGetsTheArnErrorFirst() {
	s.Require().True(s.handler(addReq(testSessionArnOne)).OK)

	resp := s.handler(addReq("not-an-arn-at-all"))

	s.False(resp.OK)
	s.Contains(resp.Error, "invalid session ARN", "the ARN error wins over the attached refusal")
	s.NotContains(resp.Error, "already attached")
}

func (s *controlHandlerSuite) TestTheServiceReturnsAnotherArnAddResolvesOnceMoreAndAttachesUnderIt() {
	s.client.returnArnFor(testSessionArnOne, testSessionArnTwo)
	s.client.returnArnFor(testSessionArnTwo, testSessionArnTwo)

	resp := s.handler(addReq(testSessionArnOne))

	s.Require().True(resp.OK, resp.Error)
	s.Equal([]string{testSessionArnOne, testSessionArnTwo}, s.client.resolvedARNs(),
		"the typed lookup, then one more with the returned ARN")
	s.Equal(testSessionArnTwo, resp.ARN)
	current, present := s.device.Current()
	s.Require().True(present)
	s.Equal(testSessionArnTwo, current.ARN, "the attachment keys on the ARN the service named")
}

func (s *controlHandlerSuite) TestTheSecondAnswerNamesAThirdArnAndNoThirdLookupIsMade() {
	s.client.returnArnFor(testSessionArnOne, testSessionArnTwo)
	s.client.returnArnFor(testSessionArnTwo, testSessionArnThree)

	resp := s.handler(addReq(testSessionArnOne))

	s.Require().True(resp.OK, resp.Error)
	s.Equal([]string{testSessionArnOne, testSessionArnTwo}, s.client.resolvedARNs(),
		"the retry is capped at one call in code")
	s.Equal(testSessionArnThree, resp.ARN, "the second answer is used as is")
}

func (s *controlHandlerSuite) TestTheSecondAnswerCarriesNoArnAndTheAskedOneIsStored() {
	s.client.returnArnFor(testSessionArnOne, testSessionArnTwo)

	resp := s.handler(addReq(testSessionArnOne))

	s.Require().True(resp.OK, resp.Error)
	s.Equal([]string{testSessionArnOne, testSessionArnTwo}, s.client.resolvedARNs())
	s.Equal(testSessionArnTwo, resp.ARN, "an answer with no ARN stores the asked one")
	current, present := s.device.Current()
	s.Require().True(present)
	s.Equal(testSessionArnTwo, current.ARN)
}

func (s *controlHandlerSuite) TestTheServiceReturnsTheTypedArnAndOneLookupIsMade() {
	s.client.returnArnFor(testSessionArnOne, testSessionArnOne)

	resp := s.handler(addReq(testSessionArnOne))

	s.Require().True(resp.OK, resp.Error)
	s.Equal([]string{testSessionArnOne}, s.client.resolvedARNs())
	s.Equal(testSessionArnOne, resp.ARN)
}

func (s *controlHandlerSuite) TestATypedArnThatResolvesToTheAttachedSessionIsANoOpWithNoSerialFetch() {
	// Nothing is attached at the pre-check; the attach lands mid-resolve under the
	// ARN the answer names. The repeated re-add check answers the no-op before any
	// serial fetch.
	racing := newFakeClient(s.mock.URL, "Pixel 10 (OS 17)")
	racing.returnArnFor(testSessionArnOne, testSessionArnTwo)
	racing.returnArnFor(testSessionArnTwo, testSessionArnTwo)
	client := &attachDuringResolve{inner: racing, attach: func() {
		s.device.Set(bridge.DeviceInfo{ARN: testSessionArnTwo, Label: "Pixel 10 (OS 17)"})
	}}
	handler := newTestManagerWithClient(s.T(), config.Default(), client).controlHandler(
		context.Background(), s.device, s.dialer, func() {})

	resp := handler(addReq(testSessionArnOne))

	s.Require().True(resp.OK, resp.Error)
	s.Equal(testSessionArnTwo, resp.ARN)
	s.Empty(sessionInbounds(s.mock), "a no-op re-add fetches no serial")
	current, present := s.device.Current()
	s.Require().True(present)
	s.Equal(testSessionArnTwo, current.ARN, "one attachment, unchanged")
}

func (s *controlHandlerSuite) TestTheSecondLookupFailsAddFailsAndNothingIsAttached() {
	failing := &failSecondLookup{inner: s.client}
	s.client.returnArnFor(testSessionArnOne, testSessionArnTwo)
	handler := newTestManagerWithClient(s.T(), config.Default(), failing).controlHandler(
		context.Background(), s.device, s.dialer, func() {})

	resp := handler(addReq(testSessionArnOne))

	s.False(resp.OK)
	s.Contains(resp.Error, "second lookup failed for the test")
	_, present := s.device.Current()
	s.False(present)
}

func (s *controlHandlerSuite) TestAnAttachUnderAReturnedArnRefreshesByTheReturnedArn() {
	s.client.returnArnFor(testSessionArnOne, testSessionArnTwo)
	s.client.returnArnFor(testSessionArnTwo, testSessionArnTwo)
	s.Require().True(s.handler(addReq(testSessionArnOne)).OK)

	s.True(s.mgr.refreshEndpointOnce(context.Background(), s.device), "the refresh finds the session")

	resolved := s.client.resolvedARNs()
	s.Equal(testSessionArnTwo, resolved[len(resolved)-1], "the refresh looks the stored ARN up")
}

func (s *controlHandlerSuite) TestEveryFailedLookupIsRefusedWithItsCauseAloneAndExactlyTheOneFixOfItsClass() {
	cases := []struct {
		name    string
		failure error
		fix     string
	}{
		{"no credentials", fmt.Errorf("get identity: %w", session.ErrNoCredentials), fixCredentials},
		{"expired credentials", &session.RequestRejectedError{Code: "ExpiredTokenException", Cause: errors.New("expired")}, fixCredentials},
		{"access denied", fmt.Errorf("%w: not allowed", session.ErrAccessDenied), fixCredentials},
		{"another rejected request", &session.RequestRejectedError{Code: "InvalidSignatureException", Cause: errors.New("signature expired")},
			fixCredentials},
		{"not found", fmt.Errorf("session %w", session.ErrSessionNotFound), fixPassRunningARN},
		{"an ARN the service rejects", fmt.Errorf("%w: api error", session.ErrInvalidARN), fixPassRunningARN},
		{"ended", &session.SessionEndedError{SessionARN: testSessionArnOne, Status: "COMPLETED"}, fixNewSession},
		{"unsupported device", session.UnsupportedDeviceError{SessionARN: testSessionArnOne, Platform: "iOS"},
			"Start a remote access session on an Android 9 or later device, then run 'devicefarm-connect device add --session-arn <new ARN>'."},
		{"not running yet", &session.NoEndpointError{SessionARN: testSessionArnOne, Status: "PREPARING"},
			"Wait for the session to begin running, then run '" + fixAddCommand + "' again."},
		{"running with no endpoint", &session.NoEndpointError{SessionARN: testSessionArnOne, Status: "RUNNING"}, fixDoctorThenSupport},
		{"a failure with no class", errors.New("connection reset"),
			"Run '" + fixDoctorCommand + "'. If its AWS and Network rows pass, restart the bridge from this shell with " + fixRestartThenAdd + "."},
	}
	for _, testCase := range cases {
		s.Run(testCase.name, func() {
			s.client.set("", testCase.failure)

			resp := s.handler(addReq(testSessionArnOne))

			s.False(resp.OK)
			s.Equal(testCase.failure.Error(), resp.Error, "the cause carries no step and no fix text")
			s.Equal([]string{testCase.fix}, resp.Fixes)
		})
	}
}

func (s *controlHandlerSuite) TestBridgeOffTheDefaultsWithReverseOnItsFixQuotesItsPortAndAddressAndNamesNoReverseFlag() {
	cfg := config.Default()
	cfg.ListenAddr, cfg.ListenPort, cfg.ReverseEnabled = "0.0.0.0", 6000, true
	s.client.set("", errors.New("connection reset"))
	handler := newTestManagerWithClient(s.T(), cfg, s.client).controlHandler(context.Background(), s.device, s.dialer, func() {})

	resp := handler(addReq(testSessionArnOne))

	s.Equal([]string{"Run 'devicefarm-connect doctor --session-arn " + testSessionArnOne + " --port 6000 --listen-addr 0.0.0.0'. " +
		"If its AWS and Network rows pass, restart the bridge from this shell with 'devicefarm-connect bridge start --port 6000 " +
		"--listen-addr 0.0.0.0' with any other option the bridge started with, then run '" + fixAddCommand + "'."}, resp.Fixes)
}

func TestGivenEachKindOfFailedConnectionWhenItsFixIsPickedThenItIsTheFixOfItsClass(t *testing.T) {
	manager := newTestManagerWithClient(t, config.Default(), newFakeClient(testEndpoint, "Pixel 10 (OS 17)"))
	tests := []struct {
		name   string
		failed bridge.ConnectionFailure
		fix    string
	}{
		{"a close with a reason", bridge.ConnectionFailure{Kind: bridge.FailureServiceRefused, CloseCode: 1013}, fixRetryThenDoctor},
		{"the close for a device the service does not serve", bridge.ConnectionFailure{Kind: bridge.FailureServiceRefused, CloseCode: 1008},
			fixOtherDevice},
		{"that close with no reason", bridge.ConnectionFailure{Kind: bridge.FailureServiceClosed, CloseCode: 1008}, fixOtherDevice},
		{"the catch-all close", bridge.ConnectionFailure{Kind: bridge.FailureServiceClosed, CloseCode: 1011}, fixRetryThenDoctor},
		{"a rejected upgrade", bridge.ConnectionFailure{Kind: bridge.FailureUpgradeRefused, Host: "device.example"}, fixRetryThenDoctor},
		{"a dial that ran out of time", bridge.ConnectionFailure{Kind: bridge.FailureAttachDeadline, Host: "device.example"}, fixRetryThenDoctor},
		{"no device listed", bridge.ConnectionFailure{Kind: bridge.FailureNoDevice}, fixDoctorThenSupport},
		{"an address that is not wss", bridge.ConnectionFailure{Kind: bridge.FailureNotWSS}, fixDoctorThenSupport},
		{"an unreachable host", bridge.ConnectionFailure{Kind: bridge.FailureUnreachable, Host: "device.example"},
			"Check that this machine can reach device.example on TCP port 443" + fixReachTail},
		{"an unreachable host that names its port", bridge.ConnectionFailure{Kind: bridge.FailureUnreachable, Host: "device.example:8443"},
			"Check that this machine can reach device.example on TCP port 8443" + fixReachTail},
		{"another wire protocol", bridge.ConnectionFailure{Kind: bridge.FailureOtherProtocol},
			"Install the latest devicefarm-connect, then run " + fixRestartThenAdd + "."},
		{"the session-stopping close", bridge.ConnectionFailure{Kind: bridge.FailureSessionStopping}, fixNewSession},
	}
	// The next try may work: the refused session, the drop, the timers and a failure no kind names.
	for _, kind := range []bridge.FailureKind{bridge.FailureSessionRefused, bridge.FailureDropped, bridge.FailureReplyTimeout,
		bridge.FailureAttachDeadline, bridge.FailureOther} {
		tests = append(tests, struct {
			name   string
			failed bridge.ConnectionFailure
			fix    string
		}{fmt.Sprintf("kind %d", kind), bridge.ConnectionFailure{Kind: kind}, fixRetryThenDoctor})
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.fix, manager.connectionFix(tc.failed, testSessionArnOne))
		})
	}
}

func (s *controlHandlerSuite) TestRemoveWithMatchingARNDetaches() {
	s.Require().True(s.handler(addReq(testSessionArnOne)).OK)

	resp := s.handler(control.Request{Op: control.OpRemove, ARN: testSessionArnOne})

	s.Require().True(resp.OK)
	_, present := s.device.Current()
	s.False(present)
}

func (s *controlHandlerSuite) TestRemoveWithoutARNDetachesTheAttachedDevice() {
	s.Require().True(s.handler(addReq(testSessionArnOne)).OK)

	resp := s.handler(control.Request{Op: control.OpRemove})

	s.Require().True(resp.OK)
	s.Equal("Pixel 10 (OS 17)", resp.DeviceLabel)
	_, present := s.device.Current()
	s.False(present)
}

func (s *controlHandlerSuite) TestRemoveWithMismatchedARNRejectedAndKept() {
	s.Require().True(s.handler(addReq(testSessionArnOne)).OK)

	resp := s.handler(control.Request{Op: control.OpRemove, ARN: testSessionArnOther})

	s.Equal(control.Response{Error: "the attached device does not match the requested session", Fixes: []string{
		"Run 'devicefarm-connect device list --json' to see the ARN of the attached session. To detach that device, run " +
			"'devicefarm-connect device remove'."}}, resp, "a mismatched ARN must not remove the attached device")
	_, present := s.device.Current()
	s.True(present)
}

func (s *controlHandlerSuite) TestRemoveWithNoDeviceRejected() {
	resp := s.handler(control.Request{Op: control.OpRemove, ARN: testSessionArnOne})

	s.Equal(control.Response{Error: "no device is attached", Fixes: []string{
		"Run 'devicefarm-connect device list' to see what the bridge holds and why the last device detached."}}, resp)
}

func (s *controlHandlerSuite) TestShutdownInvokesCancelAndAcks() {
	called := false
	handler := s.mgr.controlHandler(context.Background(), s.device, s.dialer, func() { called = true })

	resp := handler(control.Request{Op: control.OpShutdown})

	s.True(resp.OK)
	s.True(called, "shutdown op must invoke the cancel func")
}

func (s *controlHandlerSuite) TestUnknownOpRejected() {
	resp := s.handler(control.Request{Op: "bogus"})

	s.Equal(control.Response{Error: `unknown control operation "bogus"`, Fixes: []string{fixOtherVersion}}, resp)
}

func (s *controlHandlerSuite) TestListReportsEachAttachedDeviceWithItsSessionARNSerialAndTimes() {
	s.Require().True(s.handler(addReq(testSessionArnOne)).OK)

	resp := s.handler(control.Request{Op: control.OpList})

	s.Require().True(resp.OK)
	s.Equal(s.attachedArnOne(), resp.Devices)
}

func (s *controlHandlerSuite) TestListWithNoDeviceReportsAnEmptyList() {
	resp := s.handler(control.Request{Op: control.OpList})

	s.Require().True(resp.OK)
	s.Empty(resp.Devices)
}

func (s *controlHandlerSuite) TestStatusReportsAttachedLabelARNAndDevicesWithReverseOff() {
	s.Require().True(s.handler(addReq(testSessionArnOne)).OK)

	resp := s.handler(control.Request{Op: control.OpStatus})

	s.Require().True(resp.OK)
	s.Equal("Pixel 10 (OS 17)", resp.DeviceLabel)
	s.Equal(testSessionArnOne, resp.ARN)
	s.Equal(s.attachedArnOne(), resp.Devices)
	s.False(resp.ReverseEnabled, "the default config starts with reverse off")
	s.Empty(resp.LastDetachReason, "an attached device reports no detach")
	s.True(resp.LastDetachAt.IsZero())
}

func (s *controlHandlerSuite) TestStatusReportsReverseEnabledFromTheConfig() {
	cfg := config.Default()
	cfg.ReverseEnabled = true
	handler := newTestManagerWithClient(s.T(), cfg, s.client).controlHandler(context.Background(), s.device, s.dialer, func() {})

	resp := handler(control.Request{Op: control.OpStatus})

	s.Require().True(resp.OK)
	s.True(resp.ReverseEnabled)
	s.Empty(resp.ARN, "no device is attached")
	s.Empty(resp.Devices)
}

func (s *controlHandlerSuite) TestStatusAndListCarryTheLastDetachWhileNoDeviceIsAttached() {
	s.Require().True(s.handler(addReq(testSessionArnOne)).OK)
	s.client.set("", errors.New("connection reset"))
	s.False(s.mgr.refreshEndpointOnce(context.Background(), s.device), "the tick detaches")

	for _, op := range []control.Op{control.OpStatus, control.OpList} {
		resp := s.handler(control.Request{Op: op})

		s.Require().True(resp.OK)
		s.Contains(resp.LastDetachReason, "could not look up remote access session "+testSessionArnOne, string(op))
		s.WithinDuration(time.Now(), resp.LastDetachAt, time.Minute, string(op))
	}
}

func (s *controlHandlerSuite) TestStatusAndListCarryNoDetachAfterARemoveByTheUser() {
	s.Require().True(s.handler(addReq(testSessionArnOne)).OK)
	s.Require().True(s.handler(control.Request{Op: control.OpRemove}).OK)

	for _, op := range []control.Op{control.OpStatus, control.OpList} {
		resp := s.handler(control.Request{Op: op})

		s.Require().True(resp.OK)
		s.Empty(resp.LastDetachReason, string(op))
		s.True(resp.LastDetachAt.IsZero(), string(op))
	}
}

// statusHandlerFixture builds an add handler whose endpoint serves the status route too, with
// the status mock a test tunes before the add.
func statusHandlerFixture(t *testing.T, loggerHandler slog.Handler) (*testsupport.StatusMockWSS,
	control.Handler, *bridge.ActiveDevice) {
	t.Helper()
	sessions, _, status := testsupport.NewServerMockWSS(t, testsupport.SessionMockConfig{OnMessage: answerAttach})
	mgr := newTestManagerLogging(t, config.Default(), newFakeClient(sessions.URL, "Pixel 10 (OS 17)"), loggerHandler)
	device := bridge.NewActiveDevice()
	return status, mgr.controlHandler(context.Background(), device, mgr.newDialer(), func() {}), device
}

func TestGivenAnAddWhenTheStatusAnswersThenTheDeviceCarriesTheExpiryAndTheDerivedStart(t *testing.T) {
	status, handler, device := statusHandlerFixture(t, slog.DiscardHandler)
	status.SetTimeLeft(140 * time.Minute)
	before := time.Now()

	resp := handler(addReq(testSessionArnOne))

	require.True(t, resp.OK, resp.Error)
	current, present := device.Current()
	require.True(t, present)
	assert.WithinDuration(t, before.Add(140*time.Minute), current.SessionExpiresAt, 5*time.Second,
		"the expiry is the read time plus the reported time left")
	assert.Equal(t, current.SessionExpiresAt.Add(-session.MaxSessionLength), current.SessionStartedAt,
		"the start is the expiry minus the session length")
}

func TestGivenAnAddWhenTheStatusIsSilentThenTheAddSucceedsWithUnknownTimesInsideTheResolveBound(t *testing.T) {
	previous := sessionStatusTimeout
	sessionStatusTimeout = 200 * time.Millisecond
	t.Cleanup(func() { sessionStatusTimeout = previous })
	status, handler, device := statusHandlerFixture(t, slog.DiscardHandler)
	status.StaySilent()
	started := time.Now()

	resp := handler(addReq(testSessionArnOne))

	require.True(t, resp.OK, resp.Error)
	assert.Less(t, time.Since(started), 10*sessionStatusTimeout,
		"a silent status route holds the add for its own bound only")
	current, present := device.Current()
	require.True(t, present)
	assert.True(t, current.SessionStartedAt.IsZero())
	assert.True(t, current.SessionExpiresAt.IsZero())
	assert.NotEmpty(t, status.Requests(), "the add asked before giving up")
}

func TestGivenAnAddWhenTheStatusRouteIsRefusedThenTheAddSucceedsAndOneWarningIsLogged(t *testing.T) {
	logs := &lockedBuffer{}
	status, handler, device := statusHandlerFixture(t, slog.NewTextHandler(logs, nil))
	status.RefuseDials()

	resp := handler(addReq(testSessionArnOne))

	require.True(t, resp.OK, resp.Error)
	current, present := device.Current()
	require.True(t, present)
	assert.True(t, current.SessionExpiresAt.IsZero())
	assert.Equal(t, 1, strings.Count(logs.String(), "could not read the session's time left"),
		"one warning per failed read")
}

func TestGivenAnAttachedSessionWhenAddedAgainThenNoStatusIsRead(t *testing.T) {
	status, handler, _ := statusHandlerFixture(t, slog.DiscardHandler)
	require.True(t, handler(addReq(testSessionArnOne)).OK)

	resp := handler(addReq(testSessionArnOne))

	require.True(t, resp.OK, "a re-add of the attached session is a no-op")
	assert.Len(t, status.Requests(), 1, "one status read per new attachment")
}
