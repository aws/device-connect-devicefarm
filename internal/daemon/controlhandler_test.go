package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/suite"

	"github.com/aws/device-connect-devicefarm/internal/bridge"
	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/control"
)

// addReq builds an OpAdd request for a session ARN. The label is no longer carried
// on the request: the daemon derives it from the resolved session.
func addReq(arn string) control.Request {
	return control.Request{Op: control.OpAdd, ARN: arn}
}

// controlHandlerSuite exercises the daemon's control handler. SetupTest builds the
// shared fixture every test needs - a fake remote-access client, a DaemonManager
// wired to it, an empty device, and a handler over that device with a no-op
// shutdown - so each test asserts behavior instead of repeating construction. A
// test that needs a special client (a resolve that fails, or one that binds a
// device mid-resolve) rebuilds the handler from a bespoke client.
type controlHandlerSuite struct {
	suite.Suite
	client  *fakeRemoteAccessClient
	mgr     *DaemonManager
	device  *bridge.ActiveDevice
	handler control.Handler
}

func (s *controlHandlerSuite) SetupTest() {
	s.client = newFakeClient(testEndpoint, "Pixel 10 (OS 17)")
	s.mgr = newTestManagerWithClient(s.T(), config.Default(), s.client)
	s.device = bridge.NewActiveDevice()
	s.handler = s.mgr.controlHandler(context.Background(), s.device, func() {})
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
	s.Equal(testEndpoint, current.Endpoint, "the resolved endpoint must be stored verbatim")
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
		inner: newFakeClient(testEndpoint, "Pixel 10 (OS 17)"),
		attach: func() {
			s.Require().True(s.device.Set(bridge.DeviceInfo{ARN: "arn:other", Label: "Other"}))
		},
	}
	mgr := newTestManagerWithClient(s.T(), config.Default(), racing)
	handler := mgr.controlHandler(context.Background(), s.device, func() {})

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
	// and must report the attached device so proxy start/device add can reprint the
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
	handler := s.mgr.controlHandler(context.Background(), s.device, func() { called = true })

	resp := handler(control.Request{Op: control.OpShutdown})

	s.True(resp.OK)
	s.True(called, "shutdown op must invoke the cancel func")
}

func (s *controlHandlerSuite) TestUnknownOpRejected() {
	resp := s.handler(control.Request{Op: "bogus"})

	s.False(resp.OK)
}

func (s *controlHandlerSuite) TestStatusReportsAttachedLabel() {
	s.Require().True(s.handler(addReq("arn:1")).OK)

	resp := s.handler(control.Request{Op: control.OpStatus})

	s.Require().True(resp.OK)
	s.Equal("Pixel 10 (OS 17)", resp.DeviceLabel)
}
