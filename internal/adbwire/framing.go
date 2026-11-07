// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package adbwire

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
)

// maxRequestPayload is the largest payload a smart-socket request can declare.
// Four hex digits cannot express more; adb additionally rejects zero.
const maxRequestPayload = 0xFFFF

// requestLengthDigits is the fixed size of the hex length prefix.
const requestLengthDigits = 4

// errZeroLengthRequest mirrors adb's rejection of a zero length prefix
// (sockets.cpp: "bad size"). Fails closed like the rest of the parser.
var errZeroLengthRequest = errors.New("smart-socket request payload is empty")

// RequestReader reassembles smart-socket requests (a 4-hex-digit payload
// length, then that many payload bytes) from a byte stream that arrives in
// arbitrary fragments. Feed appends raw bytes; Next yields complete request
// payloads in order.
//
// Consumed bytes are dropped; a caller that must re-send the raw prologue
// verbatim keeps its own copy.
type RequestReader struct {
	buf []byte
}

// Feed appends raw bytes read from the adb client's connection.
func (r *RequestReader) Feed(data []byte) {
	r.buf = append(r.buf, data...)
}

// Next returns the next complete request payload, or nil when more bytes are
// needed. A malformed length prefix (non-hex, or zero) errors and the reader
// must not be used again: the stream offset is no longer trustworthy, so the
// bytes stay unconsumed and every later Next repeats the error.
//
// A nil payload with a nil error always means "feed more"; a zero-length
// payload cannot occur because adb rejects a zero length prefix.
func (r *RequestReader) Next() ([]byte, error) {
	if len(r.buf) < requestLengthDigits {
		return nil, nil
	}

	length, err := parseHexLength(r.buf[:requestLengthDigits])
	if err != nil {
		return nil, err
	}
	if length == 0 {
		return nil, errZeroLengthRequest
	}

	if len(r.buf) < requestLengthDigits+length {
		return nil, nil
	}

	payload := bytes.Clone(r.buf[requestLengthDigits : requestLengthDigits+length])
	r.buf = r.buf[requestLengthDigits+length:]
	return payload, nil
}

// Buffered reports how many unconsumed bytes the reader holds.
func (r *RequestReader) Buffered() int {
	return len(r.buf)
}

// EncodeRequest frames a service string as a smart-socket request. The length
// prefix is lowercase hex, matching what adb's own clients emit.
func EncodeRequest(service string) ([]byte, error) {
	if len(service) == 0 {
		return nil, errZeroLengthRequest
	}
	if len(service) > maxRequestPayload {
		return nil, fmt.Errorf("service of %d bytes exceeds the %d-byte smart-socket maximum", len(service), maxRequestPayload)
	}
	return fmt.Appendf(nil, "%04x%s", len(service), service), nil
}

// parseHexLength decodes the 4-digit hex prefix, accepting both cases like
// adb's unhex. An explicit base rejects signs, underscores and a 0x prefix.
func parseHexLength(digits []byte) (int, error) {
	value, err := strconv.ParseUint(string(digits), 16, 16)
	if err != nil {
		return 0, fmt.Errorf("smart-socket length prefix %q is not hex: %w", digits, err)
	}
	return int(value), nil
}
