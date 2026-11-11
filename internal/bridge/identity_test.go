// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
)

// identityAnswer is what this process's bridge answers the identity request with.
func identityAnswer() string {
	return "OKAY" + string(framed(fmt.Sprintf("devicefarm-connect bridge pid=%d", os.Getpid())))
}

func TestGivenNoDeviceWhenTheIdentityIsAskedThenTheBridgeAnswersItsPidAndCloses(t *testing.T) {
	proxyAddress, _ := runProxy(t, NewActiveDevice(), testPolicy())

	assert.Equal(t, identityAnswer(), askHost(t, proxyAddress, adbwire.BridgeIdentityRequest))
}

func TestGivenAnAttachedDeviceWhenTheIdentityIsAskedThenItIsAnsweredWithNoDial(t *testing.T) {
	mock := switchReplyMock(t, "OKAY")
	proxyAddress, _ := startServer(t, mock.URL, testPolicy())

	assert.Equal(t, identityAnswer(), askHost(t, proxyAddress, adbwire.BridgeIdentityRequest))
	assertNoWebsocketDialed(t, mock)
}

func TestGivenAnIdentityRequestBehindASelectWhenItArrivesThenItIsRelayedToTheDevice(t *testing.T) {
	// Only a lone request is the bridge's: behind a select it is the device's service.
	client, mock := startSelectAwareProxy(t)

	selectAnyTransport(t, client)
	writeAll(t, client, []byte(adbwire.BridgeIdentityRequest))

	assertSessionReceived(t, mock, canonicalSelect+adbwire.BridgeIdentityRequest)
}
