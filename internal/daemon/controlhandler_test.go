// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

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
		AttachedAt: bound.AttachedAt, SessionStartedAt: testSessionStartedAt, SessionExpiresAt: testSessionExpiresAt}}
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
	s.Equal(testSessionStartedAt, current.SessionStartedAt, "the add binds the session times it resolved")
	s.Equal(testSessionExpiresAt, current.SessionExpiresAt)
	s.WithinDuration(time.Now(), current.AttachedAt, time.Minute)
	s.Equal([]string{hostDevicesRequest}, sessionInbounds(s.mock), "the fetch sends host:devices alone")
}

func (s *controlHandlerSuite) TestSerialFetchDialFailsAddRejectedAndNothingAttached() {
	// The session resolves, but nothing listens at its endpoint.
	s.client.set(testEndpoint, nil)

	resp := s.handler(addReq(testSessionArnOne))

	s.False(resp.OK, "a failed serial fetch must fail the add")
	s.Contains(resp.Error, "fetch the device serial")
	_, present := s.device.Current()
	s.False(present, "nothing must be attached when the serial fetch fails")
}

func (s *controlHandlerSuite) TestSerialFetchMeetsTheSessionStoppingCloseAddFailsWithTheStoppingCauseAndItsFixApart() {
	// The relay closes the fetch's websocket with the stopping code; its cause replaces the fetch's error.
	s.mock.CloseNewConnections(4001, "remote access session is stopping")

	resp := s.handler(addReq(testSessionArnOne))

	s.False(resp.OK)
	s.Equal("remote access session "+testSessionArnOne+" is stopping", resp.Error, "the cause carries no fix text")
	s.Equal([]string{"start a new session and run '" + config.BinaryName + " device add --session-arn <new ARN>'"}, resp.Fixes)
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

func (s *controlHandlerSuite) TestSerialFetchFailsAtAPresignedEndpointAddErrorAndLogCarryNoCredential() {
	// The fetch's error reaches the CLI and the daemon log; neither may carry the URL's query.
	s.client.set(testsupport.PresignedUnreachableEndpoint, nil)
	logs := &lockedBuffer{}
	s.mgr.logger = slog.New(slog.NewTextHandler(logs, nil))

	resp := s.handler(addReq(testSessionArnOne))

	s.False(resp.OK)
	for _, sink := range []string{resp.Error, logs.String()} {
		s.Contains(sink, testsupport.ScrubbedDialText, "the dial must be named")
		s.NotContains(sink, testsupport.SignatureMarker)
		s.NotContains(sink, testsupport.SecurityTokenMarker)
		s.NotContains(sink, "X-Amz-")
	}
}

func (s *controlHandlerSuite) TestSerialFetchHangsAddFailsAtTheResolveTimeoutAndNothingAttached() {
	// A server that never answers host:devices must not hold the inline control loop past
	// resolveTimeout, which sits under the control channel's deadline.
	silent := testsupport.NewSessionMockWSS(s.T(), testsupport.SessionMockConfig{
		OnMessage: func(_ *testsupport.MockSession, _ []byte) []byte { return nil },
	})
	s.client.set(silent.URL, nil)
	previous := resolveTimeout
	resolveTimeout = 200 * time.Millisecond
	s.T().Cleanup(func() { resolveTimeout = previous })
	started := time.Now()

	resp := s.handler(addReq(testSessionArnOne))

	s.False(resp.OK, "a hung fetch must fail the add")
	s.Contains(resp.Error, "deadline exceeded")
	s.Less(time.Since(started), 2*time.Second, "the add must end at resolveTimeout, not at the reply timeout")
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
	s.Contains(resp.Error, "lists no device")
	_, present := s.device.Current()
	s.False(present, "nothing must be attached without a serial")
}

func (s *controlHandlerSuite) TestResolutionFailsAddRejectedAndNothingAttached() {
	// A malformed ARN and an ended session both reach the daemon the same way:
	// RemoteAccessClient.GetSession returns an error. The error must surface as a
	// failed add, never a nil-endpoint attach.
	s.client.set("", errors.New("session not RUNNING"))

	resp := s.handler(addReq(testSessionArnOne))

	s.False(resp.OK, "a failed resolution must fail the add")
	s.Contains(resp.Error, "session not RUNNING")
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
	s.Equal([]string{"run '" + config.BinaryName + " device remove' first"}, resp.Fixes)
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

func (s *controlHandlerSuite) TestFailedResolveWithAKnownRemedyCarriesItsFixApartFromTheCause() {
	restart := "then run '" + config.BinaryName + " bridge start --port 5037 --session-arn " + testSessionArnOne +
		"' with any other option the bridge started with; the bridge reads AWS settings only from the shell that starts it"
	cases := []struct {
		name    string
		failure error
		fix     string
	}{
		{"no credentials", fmt.Errorf("get identity: %w", session.ErrNoCredentials),
			"get working AWS credentials in this shell, " + restart},
		{"expired credentials", &session.RequestRejectedError{Code: "ExpiredTokenException", Cause: errors.New("expired")},
			"get working AWS credentials in this shell, " + restart},
		{"access denied", fmt.Errorf("%w: not allowed", session.ErrAccessDenied),
			"get AWS credentials allowed 'devicefarm:GetRemoteAccessSession' on this session in this shell, " + restart},
		{"ended", fmt.Errorf("session %w", session.ErrSessionEnded),
			"start a new remote access session, then run '" + config.BinaryName + " device add --session-arn <new ARN>'"},
		{"unsupported device", fmt.Errorf("session on Android 8: %w", session.ErrUnsupportedDevice),
			"start a remote access session on a supported Android device, then run '" + config.BinaryName +
				" device add --session-arn <its ARN>'"},
		{"not RUNNING", fmt.Errorf("session %w", session.ErrNoEndpointYet), "wait until the session is RUNNING with an endpoint, then try again"},
	}
	for _, testCase := range cases {
		s.Run(testCase.name, func() {
			s.client.set("", testCase.failure)

			resp := s.handler(addReq(testSessionArnOne))

			s.False(resp.OK)
			s.Equal("resolve session: "+testCase.failure.Error(), resp.Error, "the cause carries no fix text")
			s.Equal([]string{testCase.fix}, resp.Fixes)
		})
	}
}

func (s *controlHandlerSuite) TestFailedResolveWithNoKnownRemedyCarriesNoFix() {
	for _, failure := range []error{
		fmt.Errorf("session %w", session.ErrSessionNotFound),
		&session.RequestRejectedError{Code: "InvalidSignatureException", Cause: errors.New("signature expired")},
		errors.New("connection reset"),
	} {
		s.client.set("", failure)

		resp := s.handler(addReq(testSessionArnOne))

		s.False(resp.OK)
		s.Empty(resp.Fixes, failure.Error())
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

	s.False(resp.OK, "a mismatched ARN must not remove the attached device")
	_, present := s.device.Current()
	s.True(present)
}

func (s *controlHandlerSuite) TestRemoveWithNoDeviceRejected() {
	resp := s.handler(control.Request{Op: control.OpRemove, ARN: testSessionArnOne})

	s.False(resp.OK)
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

	s.False(resp.OK)
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
