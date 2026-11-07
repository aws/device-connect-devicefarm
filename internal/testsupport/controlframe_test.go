// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package testsupport

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/wireschema"
)

// fixtureUUID is the fixture's lowercase uuid.
const fixtureUUID = "f47ac10b-58cc-4372-a567-0e02b2c3d479"

func loadFrameVectors(t *testing.T) []wireschema.FrameVector {
	t.Helper()
	fixture, err := wireschema.LoadFixture()
	require.NoError(t, err)
	require.NotEmpty(t, fixture.Frames)
	return fixture.Frames
}

func frameVectorBytes(t *testing.T, id string) []byte {
	t.Helper()
	for _, vector := range loadFrameVectors(t) {
		if vector.ID == id {
			message, err := hex.DecodeString(vector.Hex)
			require.NoError(t, err)
			return message
		}
	}
	t.Fatalf("the fixture has no frame vector %q", id)
	return nil
}

// buildFromVector calls the builder for the vector's arm.
func buildFromVector(t *testing.T, vector wireschema.FrameVector) []byte {
	t.Helper()
	fields := vector.Fields
	switch vector.Arm {
	case "newSession":
		return FrameNewSession(fields.SessionUUID)
	case "reattach":
		return FrameReattach(fields.SessionUUID)
	case "error":
		return FrameError(fields.Reason)
	case "reverseOpen":
		return FrameReverseOpen(fields.Token, fields.CustomerPort)
	case "connectorHello":
		return FrameConnectorHello(fields.Token)
	case "notifierHello", "notifierReady", "ping", "pong":
		return FrameEmptyArm(vector.Arm)
	}
	t.Fatalf("no builder for arm %q", vector.Arm)
	return nil
}

func TestGivenEveryCanonicalFixtureFrameWhenBuiltByTheMockThenTheBytesMatchItsHex(t *testing.T) {
	canonical := 0
	for _, vector := range loadFrameVectors(t) {
		if !vector.Canonical {
			continue
		}
		canonical++
		t.Run(vector.ID, func(t *testing.T) {
			want, err := hex.DecodeString(vector.Hex)
			require.NoError(t, err)

			assert.Equal(t, want, buildFromVector(t, vector))
		})
	}
	assert.Positive(t, canonical, "the fixture must carry canonical vectors")
}

func TestGivenAReasonWithQuotesAndHTMLWhenTheMockFramesItThenTheBodyPassesTheSchemaAndKeepsTheReason(t *testing.T) {
	// The mock's route refusal, plus the characters a JSON encoder may escape.
	reason := fmt.Sprintf("dialed on route %q, not %q <a&b>", "adb", "adbreverse")

	frame := FrameError(reason)

	body := frame[len(controlFrameMagic):]
	require.NoError(t, wireschema.Validate(body))
	var decoded map[string]struct {
		Reason string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal(body, &decoded))
	assert.Equal(t, reason, decoded["error"].Reason)
}

func TestGivenAFirstFrameWhenTheMockReadsAConnectorTokenThenOnlyAnExactConnectorHelloYieldsOne(t *testing.T) {
	tests := []struct {
		name    string
		message []byte
		want    string
	}{
		{name: "the fixture's connector hello", message: frameVectorBytes(t, "connector-hello"), want: fixtureUUID},
		{name: "a byte-protocol connector hello", message: []byte("\x00DFADB\x00\x15" + fixtureUUID)},
		{name: "a hello with an extra key", message: FrameWithBody(`{"connectorHello":{"token":"` + fixtureUUID + `","extra":1}}`)},
		{name: "a hello with whitespace", message: FrameWithBody(`{"connectorHello": {"token": "` + fixtureUUID + `"}}`)},
		{name: "a token the encoder escapes", message: FrameWithBody(`{"connectorHello":{"token":"a\"b"}}`), want: `a"b`},
		{name: "a hello with an empty token", message: FrameWithBody(`{"connectorHello":{"token":""}}`)},
		{name: "a hello whose body is not JSON", message: FrameWithBody(`{"connectorHello":`)},
		{name: "a ping", message: FrameEmptyArm("ping")},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equal(t, testCase.want, connectorTokenOf(testCase.message))
		})
	}
}

func TestGivenAFixtureBodyWhenFramedRawThenTheBytesMatchTheVector(t *testing.T) {
	tests := []struct {
		vectorID string
		body     string
	}{
		{vectorID: "duplicate-key", body: `{"ping":{},"ping":{}}`},
		{vectorID: "two-arms-frame", body: `{"ping":{},"pong":{}}`},
	}

	for _, testCase := range tests {
		t.Run(testCase.vectorID, func(t *testing.T) {
			assert.Equal(t, frameVectorBytes(t, testCase.vectorID), FrameWithBody(testCase.body))
		})
	}
}
