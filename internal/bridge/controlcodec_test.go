// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/wireschema"
)

// fixtureUUID is the uuid in the fixture's valid vectors.
const fixtureUUID = "f47ac10b-58cc-4372-a567-0e02b2c3d479"

// armByKey is the test's own key-to-arm table, so a swapped entry in the codec's table
// cannot pass the fixture tests.
var armByKey = map[string]controlArm{
	"newSession":     armNewSession,
	"reattach":       armReattach,
	"error":          armError,
	"notifierHello":  armNotifierHello,
	"notifierReady":  armNotifierReady,
	"reverseOpen":    armReverseOpen,
	"ping":           armPing,
	"pong":           armPong,
	"connectorHello": armConnectorHello,
}

func loadFrameVectors(t *testing.T) []wireschema.FrameVector {
	t.Helper()
	fixture, err := wireschema.LoadFixture()
	require.NoError(t, err)
	require.NotEmpty(t, fixture.Frames)
	return fixture.Frames
}

func frameVector(t *testing.T, id string) wireschema.FrameVector {
	t.Helper()
	for _, vector := range loadFrameVectors(t) {
		if vector.ID == id {
			return vector
		}
	}
	t.Fatalf("the fixture has no frame vector %q", id)
	return wireschema.FrameVector{}
}

func decodeHex(t *testing.T, text string) []byte {
	t.Helper()
	message, err := hex.DecodeString(text)
	require.NoError(t, err)
	return message
}

// frameFromVector builds the controlFrame a vector describes.
func frameFromVector(t *testing.T, vector wireschema.FrameVector) controlFrame {
	t.Helper()
	arm, ok := armByKey[vector.Arm]
	require.True(t, ok, "fixture arm %q", vector.Arm)
	frame := controlFrame{arm: arm}
	switch arm {
	case armNewSession, armReattach:
		frame.payload = vector.Fields.SessionUUID
	case armError:
		frame.payload = vector.Fields.Reason
	case armConnectorHello:
		frame.payload = vector.Fields.Token
	case armReverseOpen:
		frame.reverseToken, frame.customerPort = vector.Fields.Token, vector.Fields.CustomerPort
	}
	return frame
}

func TestGivenEveryFixtureFrameWhenUnmarshalledThenTheVerdictArmAndFieldsMatch(t *testing.T) {
	for _, vector := range loadFrameVectors(t) {
		t.Run(vector.ID, func(t *testing.T) {
			frame, err := unmarshalControlFrame(decodeHex(t, vector.Hex))

			switch {
			case vector.Valid:
				require.NoError(t, err, vector.Why)
				assert.Equal(t, frameFromVector(t, vector), frame)
			case strings.HasPrefix(vector.Why, "law 1"):
				assert.ErrorIs(t, err, errNotControlFrame, vector.Why)
			default:
				require.Error(t, err, vector.Why)
				assert.NotErrorIs(t, err, errNotControlFrame, vector.Why)
			}
		})
	}
}

func TestGivenEveryCanonicalFixtureFrameWhenMarshalledThenTheBytesMatchItsHex(t *testing.T) {
	canonical := 0
	for _, vector := range loadFrameVectors(t) {
		if !vector.Canonical {
			continue
		}
		canonical++
		t.Run(vector.ID, func(t *testing.T) {
			message, err := marshalControlFrame(frameFromVector(t, vector))

			require.NoError(t, err)
			assert.Equal(t, decodeHex(t, vector.Hex), message)
		})
	}
	assert.Positive(t, canonical, "the fixture must carry canonical vectors")
}

func TestGivenAV1FrameWhenUnmarshalledThenErrNotControlFrame(t *testing.T) {
	tests := []struct {
		name    string
		message []byte
	}{
		{name: "a byte-protocol NEW frame", message: decodeHex(t, frameVector(t, "v1-magic-new-frame").Hex)},
		{name: "a byte-protocol PING", message: []byte("\x00DFADB\x00\x13")},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := unmarshalControlFrame(testCase.message)

			assert.ErrorIs(t, err, errNotControlFrame)
			assert.Contains(t, err.Error(), "wire version 0x00")
		})
	}
}

func TestGivenADuplicateKeyAtAnyDepthWhenUnmarshalledThenItFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		message []byte
	}{
		{name: "two arms with one name", message: decodeHex(t, frameVector(t, "duplicate-key").Hex)},
		{name: "token twice inside reverseOpen", message: []byte(controlFrameMagic +
			`{"reverseOpen":{"token":"` + fixtureUUID + `","token":"` + fixtureUUID + `","customerPort":9090}}`)},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := unmarshalControlFrame(testCase.message)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "repeats a key")
			assert.NotErrorIs(t, err, errNotControlFrame)
		})
	}
}

func TestGivenAnArrayInsideTheBodyWhenUnmarshalledThenTheStrictParseReachesIntoIt(t *testing.T) {
	tests := []struct {
		name string
		body string
		text string
	}{
		{name: "a fraction inside an array", body: `{"ping":[1.5]}`, text: "not an integer token"},
		{name: "a repeated key inside an array", body: `{"ping":[{"a":1,"a":2}]}`, text: "repeats a key"},
		{name: "a whole array reaches the schema", body: `{"ping":[1]}`, text: "at /ping (type)"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := unmarshalControlFrame([]byte(controlFrameMagic + testCase.body))

			require.Error(t, err)
			assert.Contains(t, err.Error(), testCase.text)
			assert.NotErrorIs(t, err, errNotControlFrame)
		})
	}
}

func TestGivenATruncatedOrBrokenBodyWhenUnmarshalledThenLaw2RejectsIt(t *testing.T) {
	// A fragmented websocket frame arrives as truncated JSON.
	for _, body := range []string{`{"ping":{}`, `{"ping":[1`, `{"ping":}`} {
		t.Run(body, func(t *testing.T) {
			_, err := unmarshalControlFrame([]byte(controlFrameMagic + body))

			require.Error(t, err)
			assert.Equal(t, "malformed control frame: body is not JSON", err.Error())
		})
	}
}

func TestGivenAValidFramePaddedToTheBoundWhenUnmarshalledThenOnlyTheOverBoundCopyFails(t *testing.T) {
	ping := decodeHex(t, frameVector(t, "ping").Hex)
	atBound := append(ping, []byte(strings.Repeat(" ", maxControlFrameBytes-len(ping)))...)
	require.Len(t, atBound, maxControlFrameBytes)

	frame, err := unmarshalControlFrame(atBound)
	require.NoError(t, err)
	assert.Equal(t, armPing, frame.arm)

	_, err = unmarshalControlFrame(append(atBound, ' '))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "above the 32768-byte bound")
	assert.NotErrorIs(t, err, errNotControlFrame)
}

func TestGivenACustomerPortWithAFractionOrExponentWhenUnmarshalledThenLaw2RejectsIt(t *testing.T) {
	reverseOpenWithPort := func(port string) []byte {
		return []byte(controlFrameMagic + `{"reverseOpen":{"token":"` + fixtureUUID + `","customerPort":` + port + `}}`)
	}
	tests := []struct {
		name    string
		message []byte
	}{
		{name: "fraction", message: decodeHex(t, frameVector(t, "reverse-open-port-float").Hex)},
		{name: "exponent", message: decodeHex(t, frameVector(t, "reverse-open-port-exponent").Hex)},
		{name: "lowercase exponent", message: reverseOpenWithPort("1e2")},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := unmarshalControlFrame(testCase.message)

			require.Error(t, err)
			assert.True(t, strings.HasPrefix(err.Error(), "malformed control frame:"), err.Error())
			assert.NotErrorIs(t, err, errNotControlFrame)
		})
	}

	t.Run("negative zero passes the parse and fails the schema", func(t *testing.T) {
		_, err := unmarshalControlFrame(reverseOpenWithPort("-0"))

		require.Error(t, err)
		assert.NotContains(t, err.Error(), "malformed control frame")
		assert.Contains(t, err.Error(), "/reverseOpen/customerPort")
	})
}

func TestGivenAReverseOpenWithAMalformedTokenWhenUnmarshalledThenTheErrorDoesNotEchoTheToken(t *testing.T) {
	message := []byte(controlFrameMagic + `{"reverseOpen":{"token":"` + fixtureUUID + `\n","customerPort":9090}}`)

	_, err := unmarshalControlFrame(message)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), fixtureUUID)
	assert.Equal(t, "control frame does not match the schema at /reverseOpen/token (maxLength, pattern)", err.Error())
}

func TestGivenAFrameWithNoArmWhenMarshalledThenItFails(t *testing.T) {
	message, err := marshalControlFrame(controlFrame{})

	require.Error(t, err)
	assert.Nil(t, message)
	assert.Contains(t, err.Error(), "no control frame arm is named controlArm(0)")
}

func FuzzUnmarshalControlFrame(fuzzer *testing.F) {
	fixture, err := wireschema.LoadFixture()
	if err != nil {
		fuzzer.Fatal(err)
	}
	for _, vector := range fixture.Frames {
		message, err := hex.DecodeString(vector.Hex)
		if err != nil {
			fuzzer.Fatal(err)
		}
		fuzzer.Add(message)
	}

	fuzzer.Fuzz(func(t *testing.T, message []byte) {
		frame, err := unmarshalControlFrame(message)

		magicMismatch := len(message) < len(controlFrameMagic) || string(message[:len(controlFrameMagic)]) != controlFrameMagic
		if errors.Is(err, errNotControlFrame) != magicMismatch {
			t.Fatalf("errNotControlFrame %v for a %d-byte message with magic mismatch %v", err, len(message), magicMismatch)
		}
		if err != nil {
			return
		}
		if len(message) > maxControlFrameBytes {
			t.Fatalf("%d bytes decoded above the bound", len(message))
		}
		if validateErr := wireschema.Validate(message[len(controlFrameMagic):]); validateErr != nil {
			t.Fatalf("a decoded frame fails the schema: %v", validateErr)
		}
		if frame.arm == 0 {
			t.Fatal("a decoded frame names no arm")
		}
		encoded, err := marshalControlFrame(frame)
		if err != nil {
			t.Fatalf("a decoded frame fails to encode: %v", err)
		}
		again, err := unmarshalControlFrame(encoded)
		if err != nil || again != frame {
			t.Fatalf("a decoded frame does not round-trip: %v", err)
		}
	})
}
