package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/aws/device-connect-devicefarm/internal/wireschema"
)

// JSON control frame: controlFrameMagic, then one compact JSON object with one key, the
// arm. One whole websocket message, no length field. The decoder applies its laws in
// order: magic, strict parse, schema, mapping onto controlFrame.
const (
	controlFrameMagic = "\x00DFADB\x01"

	// Whole-message bound, checked after the magic and before any parse. The largest
	// schema-valid frame is far below it.
	maxControlFrameBytes = 32768
)

// controlArm is the one key of a JSON control frame. Zero names no arm.
type controlArm uint8

const (
	armNewSession controlArm = iota + 1
	armReattach
	armError
	armNotifierHello
	armNotifierReady
	armReverseOpen
	armPing
	armPong
	armConnectorHello
)

// controlArmKeys maps each arm to its JSON key.
var controlArmKeys = [...]string{
	armNewSession:     "newSession",
	armReattach:       "reattach",
	armError:          "error",
	armNotifierHello:  "notifierHello",
	armNotifierReady:  "notifierReady",
	armReverseOpen:    "reverseOpen",
	armPing:           "ping",
	armPong:           "pong",
	armConnectorHello: "connectorHello",
}

// String returns the arm's JSON key.
func (arm controlArm) String() string {
	if int(arm) < len(controlArmKeys) && controlArmKeys[arm] != "" {
		return controlArmKeys[arm]
	}
	return fmt.Sprintf("controlArm(%d)", uint8(arm))
}

func controlArmForKey(key string) (controlArm, bool) {
	for arm, armKey := range controlArmKeys {
		if armKey == key && armKey != "" {
			return controlArm(arm), true
		}
	}
	return 0, false
}

// marshalControlFrame writes the magic, then the one-key object. Struct field order is
// the wire order. It validates nothing; the peer's decoder does.
func marshalControlFrame(frame controlFrame) ([]byte, error) {
	var body any
	switch frame.arm {
	case armNewSession, armReattach:
		body = struct {
			SessionUUID string `json:"sessionUuid"`
		}{frame.payload}
	case armError:
		body = struct {
			Reason string `json:"reason"`
		}{frame.payload}
	case armReverseOpen:
		body = struct {
			Token        string `json:"token"`
			CustomerPort int    `json:"customerPort"`
		}{frame.reverseToken, frame.customerPort}
	case armConnectorHello:
		body = struct {
			Token string `json:"token"`
		}{frame.payload}
	case armNotifierHello, armNotifierReady, armPing, armPong:
		body = struct{}{}
	default:
		return nil, fmt.Errorf("no control frame arm is named %s", frame.arm)
	}

	var buffer bytes.Buffer
	buffer.WriteString(controlFrameMagic)
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(map[string]any{frame.arm.String(): body}); err != nil {
		return nil, fmt.Errorf("encode the %s control frame: %w", frame.arm, err)
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}

// unmarshalControlFrame decodes one whole websocket message. Only a missing magic
// returns errNotControlFrame; every later failure is another error.
func unmarshalControlFrame(message []byte) (controlFrame, error) {
	if err := requireControlFrameMagic(message); err != nil {
		return controlFrame{}, err
	}
	if len(message) > maxControlFrameBytes {
		return controlFrame{}, fmt.Errorf("malformed control frame: %d bytes is above the %d-byte bound",
			len(message), maxControlFrameBytes)
	}
	body := message[len(controlFrameMagic):]
	if !utf8.Valid(body) {
		return controlFrame{}, errors.New("malformed control frame: body is not valid UTF-8")
	}
	if err := requireStrictObject(body); err != nil {
		return controlFrame{}, fmt.Errorf("malformed control frame: %w", err)
	}
	if err := wireschema.Validate(body); err != nil {
		return controlFrame{}, err
	}
	return mapControlFrame(body)
}

// requireControlFrameMagic is law 1. When only the version byte differs, the error
// names it, so a peer on another wire version is visible.
func requireControlFrameMagic(message []byte) error {
	if len(message) < len(controlFrameMagic) {
		return errNotControlFrame
	}
	if string(message[:len(controlFrameMagic)]) == controlFrameMagic {
		return nil
	}
	versionAt := len(controlFrameMagic) - 1
	if string(message[:versionAt]) == controlFrameMagic[:versionAt] {
		return fmt.Errorf("%w: wire version 0x%02x, this client speaks 0x01", errNotControlFrame, message[versionAt])
	}
	return errNotControlFrame
}

// requireStrictObject is law 2, the strict parse: one JSON object, only whitespace
// around it, no repeated key in any object, every number an integer token. Depth is not
// counted; the byte bound caps it. Errors carry no key or value text.
func requireStrictObject(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return errors.New("body is not JSON")
	}
	if token != json.Delim('{') {
		return errors.New("body is not a JSON object")
	}
	if err := requireStrictMembers(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("body has data after the object")
	}
	return nil
}

// requireStrictMembers walks an object from after '{' through '}'. Inside an object
// Token returns a key, then its value.
func requireStrictMembers(decoder *json.Decoder) error {
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return errors.New("body is not JSON")
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("body has a key that is not a string")
		}
		if seen[key] {
			return errors.New("body repeats a key")
		}
		seen[key] = true
		if err := requireStrictValue(decoder); err != nil {
			return err
		}
	}
	return requireClosingDelim(decoder)
}

// requireStrictValue walks one value, recursing into objects and arrays.
func requireStrictValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return errors.New("body is not JSON")
	}
	switch value := token.(type) {
	case json.Delim:
		// A Delim at a value start is '{' or '['.
		if value == '{' {
			return requireStrictMembers(decoder)
		}
		return requireStrictElements(decoder)
	case json.Number:
		if strings.ContainsAny(value.String(), ".eE") {
			return errors.New("body has a number that is not an integer token")
		}
	}
	return nil
}

func requireStrictElements(decoder *json.Decoder) error {
	for decoder.More() {
		if err := requireStrictValue(decoder); err != nil {
			return err
		}
	}
	return requireClosingDelim(decoder)
}

// requireClosingDelim reads the closer after More is false. Token matches it to its
// opener, so only a truncated body errors here.
func requireClosingDelim(decoder *json.Decoder) error {
	if _, err := decoder.Token(); err != nil {
		return errors.New("body is not JSON")
	}
	return nil
}

// validatedArmBody holds every member any arm carries; the schema already fixed which
// are present. CustomerPort stays json.Number so the mapping reads an int64, not a float.
type validatedArmBody struct {
	SessionUUID  string      `json:"sessionUuid"`
	Reason       string      `json:"reason"`
	Token        string      `json:"token"`
	CustomerPort json.Number `json:"customerPort"`
}

// errUnmappableControlFrame is law 4's one error. The body passed the schema, so no
// conforming peer reaches it; its text carries no key or value.
var errUnmappableControlFrame = errors.New("map the control frame: body does not fit an arm")

// mapControlFrame is law 4: the one arm onto controlFrame.
func mapControlFrame(body []byte) (controlFrame, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var object map[string]validatedArmBody
	if err := decoder.Decode(&object); err != nil {
		return controlFrame{}, errUnmappableControlFrame
	}
	for key, fields := range object {
		arm, ok := controlArmForKey(key)
		if !ok {
			break
		}
		frame := controlFrame{arm: arm}
		switch arm {
		case armNewSession, armReattach:
			frame.payload = fields.SessionUUID
		case armError:
			frame.payload = fields.Reason
		case armConnectorHello:
			frame.payload = fields.Token
		case armReverseOpen:
			customerPort, err := fields.CustomerPort.Int64()
			if err != nil {
				return controlFrame{}, errUnmappableControlFrame
			}
			frame.reverseToken, frame.customerPort = fields.Token, int(customerPort)
		}
		return frame, nil
	}
	return controlFrame{}, errUnmappableControlFrame
}
