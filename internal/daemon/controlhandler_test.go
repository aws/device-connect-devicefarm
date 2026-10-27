package daemon

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/aws/device-connect-devicefarm/internal/bridge"
	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/control"
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

func TestControlHandlerSuite(t *testing.T) {
	suite.Run(t, new(controlHandlerSuite))
}

func (s *controlHandlerSuite) TestNoDeviceAttachedAddResolvesAttachesAndReportsLabel() {
	resp := s.handler(addReq("arn:1"))

	s.Require().True(resp.OK)
	s.Equal("Pixel 10 (OS 17)", resp.DeviceLabel)
	s.Equal(s.mgr.config.ListenPort, resp.Port)
	current, present := s.device.Current()
	s.Require().True(present)
	s.Equal("arn:1", current.ARN)
	s.Equal(s.mock.URL, current.Endpoint, "the resolved endpoint must be stored verbatim")
	s.Equal(attachSerial, current.Serial, "the add must bind the device with the serial it fetched")
	s.Equal([]string{hostDevicesRequest}, sessionInbounds(s.mock), "the fetch sends host:devices alone")
}

func (s *controlHandlerSuite) TestSerialFetchDialFailsAddRejectedAndNothingAttached() {
	// The session resolves, but nothing listens at its endpoint.
	s.client.set(testEndpoint, nil)

	resp := s.handler(addReq("arn:1"))

	s.False(resp.OK, "a failed serial fetch must fail the add")
	s.Contains(resp.Error, "fetch the device serial")
	_, present := s.device.Current()
	s.False(present, "nothing must be attached when the serial fetch fails")
}

func (s *controlHandlerSuite) TestSerialFetchFailsAtAPresignedEndpointAddErrorAndLogCarryNoCredential() {
	// The fetch's error reaches the CLI and the daemon log; neither may carry the URL's query.
	s.client.set(testsupport.PresignedUnreachableEndpoint, nil)
	logs := &lockedBuffer{}
	s.mgr.logger = slog.New(slog.NewTextHandler(logs, nil))

	resp := s.handler(addReq("arn:1"))

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

	resp := s.handler(addReq("arn:1"))

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

	resp := s.handler(addReq("arn:1"))

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

	resp := s.handler(addReq("arn:1"))

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
			s.Require().True(s.device.Set(bridge.DeviceInfo{ARN: "arn:other", Label: "Other"}))
		},
	}
	mgr := newTestManagerWithClient(s.T(), config.Default(), racing)
	handler := mgr.controlHandler(context.Background(), s.device, s.dialer, func() {})

	resp := handler(addReq("arn:1"))

	s.False(resp.OK, "a device bound mid-resolve must fail the add")
	s.Contains(resp.Error, "already attached (Other)")
	current, present := s.device.Current()
	s.Require().True(present)
	s.Equal("arn:other", current.ARN, "the device that won the race must stay attached")
}

func (s *controlHandlerSuite) TestDifferentDeviceAlreadyAttachedAddRejectedWithoutResolving() {
	s.Require().True(s.handler(addReq("arn:1")).OK)

	resp := s.handler(addReq("arn:2"))

	s.False(resp.OK, "the one-device limit must reject adding a different device")
	s.NotEmpty(resp.Error)
	current, _ := s.device.Current()
	s.Equal("arn:1", current.ARN, "the first device must stay attached")
	s.Equal([]string{"arn:1"}, s.client.resolvedARNs(), "a rejected different device must not trigger a resolve")
}

func (s *controlHandlerSuite) TestSameDeviceAlreadyAttachedAddAgainIsIdempotentWithoutResolving() {
	s.Require().True(s.handler(addReq("arn:1")).OK)

	// Re-adding the same session ARN must succeed as a no-op rather than erroring,
	// and must report the attached device so bridge start/device add can reprint the
	// listening line, without making a second AWS call.
	resp := s.handler(addReq("arn:1"))

	s.Require().True(resp.OK, "re-adding the same device must be idempotent")
	s.Equal("Pixel 10 (OS 17)", resp.DeviceLabel)
	current, present := s.device.Current()
	s.Require().True(present)
	s.Equal("arn:1", current.ARN, "the device must remain attached")
	s.Equal([]string{"arn:1"}, s.client.resolvedARNs(), "a re-add of the current device must not resolve again")
}

func (s *controlHandlerSuite) TestRemoveWithMatchingARNDetaches() {
	s.Require().True(s.handler(addReq("arn:1")).OK)

	resp := s.handler(control.Request{Op: control.OpRemove, ARN: "arn:1"})

	s.Require().True(resp.OK)
	_, present := s.device.Current()
	s.False(present)
}

func (s *controlHandlerSuite) TestRemoveWithoutARNDetachesTheAttachedDevice() {
	s.Require().True(s.handler(addReq("arn:1")).OK)

	resp := s.handler(control.Request{Op: control.OpRemove})

	s.Require().True(resp.OK)
	s.Equal("Pixel 10 (OS 17)", resp.DeviceLabel)
	_, present := s.device.Current()
	s.False(present)
}

func (s *controlHandlerSuite) TestRemoveWithMismatchedARNRejectedAndKept() {
	s.Require().True(s.handler(addReq("arn:1")).OK)

	resp := s.handler(control.Request{Op: control.OpRemove, ARN: "arn:other"})

	s.False(resp.OK, "a mismatched ARN must not remove the attached device")
	_, present := s.device.Current()
	s.True(present)
}

func (s *controlHandlerSuite) TestRemoveWithNoDeviceRejected() {
	resp := s.handler(control.Request{Op: control.OpRemove, ARN: "arn:1"})

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

func (s *controlHandlerSuite) TestListReportsEachAttachedDeviceWithItsSessionARN() {
	s.Require().True(s.handler(addReq("arn:1")).OK)

	resp := s.handler(control.Request{Op: control.OpList})

	s.Require().True(resp.OK)
	s.Equal([]control.AttachedDevice{{SessionARN: "arn:1", Label: "Pixel 10 (OS 17)"}}, resp.Devices)
}

func (s *controlHandlerSuite) TestListWithNoDeviceReportsAnEmptyList() {
	resp := s.handler(control.Request{Op: control.OpList})

	s.Require().True(resp.OK)
	s.Empty(resp.Devices)
}

func (s *controlHandlerSuite) TestStatusReportsAttachedLabelAndARNWithReverseOff() {
	s.Require().True(s.handler(addReq("arn:1")).OK)

	resp := s.handler(control.Request{Op: control.OpStatus})

	s.Require().True(resp.OK)
	s.Equal("Pixel 10 (OS 17)", resp.DeviceLabel)
	s.Equal("arn:1", resp.ARN)
	s.False(resp.ReverseEnabled, "the default config starts with reverse off")
}

func (s *controlHandlerSuite) TestStatusReportsReverseEnabledFromTheConfig() {
	cfg := config.Default()
	cfg.ReverseEnabled = true
	handler := newTestManagerWithClient(s.T(), cfg, s.client).controlHandler(context.Background(), s.device, s.dialer, func() {})

	resp := handler(control.Request{Op: control.OpStatus})

	s.Require().True(resp.OK)
	s.True(resp.ReverseEnabled)
	s.Empty(resp.ARN, "no device is attached")
}
