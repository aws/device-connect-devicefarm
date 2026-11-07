// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package testsupport

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// controlFrameMagic opens every JSON control frame. Copied on purpose: the mock never
// shares the client's codec.
const controlFrameMagic = "\x00DFADB\x01"

// Arm keys, duplicated from the wire contract like the magic.
const (
	controlStatusNew            = "newSession"
	controlStatusReattach       = "reattach"
	controlStatusError          = "error"
	controlStatusNotifierHello  = "notifierHello"
	controlStatusNotifierReady  = "notifierReady"
	controlStatusReverseOpen    = "reverseOpen"
	controlStatusPing           = "ping"
	controlStatusPong           = "pong"
	controlStatusConnectorHello = "connectorHello"
)

// FrameNewSession is the server's answer that creates a session.
func FrameNewSession(sessionUUID string) []byte {
	return frameWithArm(controlStatusNew, struct {
		SessionUUID string `json:"sessionUuid"`
	}{sessionUUID})
}

// FrameReattach is the server's answer that resumes a session.
func FrameReattach(sessionUUID string) []byte {
	return frameWithArm(controlStatusReattach, struct {
		SessionUUID string `json:"sessionUuid"`
	}{sessionUUID})
}

// FrameError is the server's refusal with a reason.
func FrameError(reason string) []byte {
	return frameWithArm(controlStatusError, struct {
		Reason string `json:"reason"`
	}{reason})
}

// FrameReverseOpen tells the notifier a device connection waits for a connector.
func FrameReverseOpen(token string, customerPort int) []byte {
	return frameWithArm(controlStatusReverseOpen, struct {
		Token        string `json:"token"`
		CustomerPort int    `json:"customerPort"`
	}{token, customerPort})
}

// FrameConnectorHello is the client's first frame on a connector.
func FrameConnectorHello(token string) []byte {
	return frameWithArm(controlStatusConnectorHello, struct {
		Token string `json:"token"`
	}{token})
}

// FrameEmptyArm frames an arm with an empty body: the contract's empty arms, or an
// unknown key for a negative test.
func FrameEmptyArm(arm string) []byte {
	return frameWithArm(arm, struct{}{})
}

// FrameWithBody frames body unchanged, for malformed bodies.
func FrameWithBody(body string) []byte {
	return append([]byte(controlFrameMagic), body...)
}

// frameWithArm writes the magic, then the one-key object. The encoder leaves < > & raw,
// like every conforming encoder.
func frameWithArm(arm string, body any) []byte {
	var buffer bytes.Buffer
	buffer.WriteString(controlFrameMagic)
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(map[string]any{arm: body}); err != nil {
		panic(fmt.Sprintf("testsupport: encode %s frame: %v", arm, err))
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'})
}

// connectorTokenOf returns the token when message is byte-equal to
// FrameConnectorHello(token), else "". An empty token reads as no hello.
func connectorTokenOf(message []byte) string {
	var frame struct {
		ConnectorHello struct {
			Token string `json:"token"`
		} `json:"connectorHello"`
	}
	if !bytes.HasPrefix(message, []byte(controlFrameMagic)) ||
		json.Unmarshal(message[len(controlFrameMagic):], &frame) != nil ||
		!bytes.Equal(message, FrameConnectorHello(frame.ConnectorHello.Token)) {
		return ""
	}
	return frame.ConnectorHello.Token
}
