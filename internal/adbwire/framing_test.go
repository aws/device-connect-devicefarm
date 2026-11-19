package adbwire

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGivenWholeRequestWhenFedAtOnceThenNextReturnsPayload(t *testing.T) {
	var reader RequestReader
	reader.Feed([]byte("000chost:version"))

	payload, err := reader.Next()

	require.NoError(t, err)
	assert.Equal(t, []byte("host:version"), payload)
	assert.Zero(t, reader.Buffered())
}

func TestGivenRequestArrivingInFragmentsWhenFedThenNextReassembles(t *testing.T) {
	// Split the same request at every byte boundary: partial length prefix,
	// prefix/payload seam, and mid-payload must all reassemble identically.
	request := []byte("0011host:list-forward")
	for splitAt := 1; splitAt < len(request); splitAt++ {
		var reader RequestReader
		reader.Feed(request[:splitAt])

		payload, err := reader.Next()
		require.NoError(t, err)
		require.Nil(t, payload, "split at %d yielded a payload from an incomplete request", splitAt)
		assert.Equal(t, splitAt, reader.Buffered(), "split at %d", splitAt)

		reader.Feed(request[splitAt:])
		payload, err = reader.Next()
		require.NoError(t, err)
		assert.Equal(t, []byte("host:list-forward"), payload, "split at %d", splitAt)
	}
}

func TestGivenMultipleRequestsInOneFeedWhenNextCalledRepeatedlyThenEachReturnedInOrder(t *testing.T) {
	var reader RequestReader
	reader.Feed([]byte("000chost:devices" + "0011host:list-forward"))

	first, err := reader.Next()
	require.NoError(t, err)
	assert.Equal(t, []byte("host:devices"), first)

	second, err := reader.Next()
	require.NoError(t, err)
	assert.Equal(t, []byte("host:list-forward"), second)

	third, err := reader.Next()
	require.NoError(t, err)
	assert.Nil(t, third, "no third request was fed")
}

func TestGivenUppercaseHexLengthWhenNextThenAccepted(t *testing.T) {
	// adb's unhex accepts both cases; clients are free to emit either.
	var reader RequestReader
	reader.Feed([]byte("000Chost:devices"))

	payload, err := reader.Next()

	require.NoError(t, err)
	assert.Equal(t, []byte("host:devices"), payload)
}

func TestGivenMalformedLengthPrefixWhenNextThenFailsClosed(t *testing.T) {
	tests := []struct {
		name  string
		bytes string
		// wantErr pins the sentinel where one exists; nil means any error.
		wantErr error
	}{
		{name: "non-hex prefix", bytes: "zzzzhost:devices"},
		{name: "signed prefix", bytes: "-00chost:devices"},
		{name: "zero length", bytes: "0000", wantErr: errZeroLengthRequest},
		{name: "raw adb status word", bytes: "OKAYOKAY"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var reader RequestReader
			reader.Feed([]byte(tc.bytes))

			payload, err := reader.Next()
			require.Error(t, err)
			assert.Nil(t, payload)
			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr)
			}

			// Poisoned: the bytes stay put, and a well-formed request fed
			// afterwards cannot resync the reader.
			reader.Feed([]byte("000chost:devices"))
			payload, retryErr := reader.Next()
			assert.EqualError(t, retryErr, err.Error())
			assert.Nil(t, payload)
			assert.Equal(t, len(tc.bytes)+len("000chost:devices"), reader.Buffered())
		})
	}
}

func TestGivenServiceWhenEncodedThenFramedWithLowercaseHexAndRoundTrips(t *testing.T) {
	framed, err := EncodeRequest("host:devices")

	require.NoError(t, err)
	assert.Equal(t, []byte("000chost:devices"), framed)

	var reader RequestReader
	reader.Feed(framed)
	payload, err := reader.Next()
	require.NoError(t, err)
	assert.Equal(t, []byte("host:devices"), payload)
}

func TestGivenLargestServiceWhenEncodedThenPrefixIsFFFFAndRoundTrips(t *testing.T) {
	largest := strings.Repeat("a", maxRequestPayload)

	framed, err := EncodeRequest(largest)

	require.NoError(t, err)
	assert.Equal(t, "ffff", string(framed[:requestLengthDigits]))

	var reader RequestReader
	reader.Feed(framed)
	payload, err := reader.Next()
	require.NoError(t, err)
	assert.Equal(t, largest, string(payload))
	assert.Zero(t, reader.Buffered())
}

func TestGivenUnframeableServiceWhenEncodedThenRejected(t *testing.T) {
	_, err := EncodeRequest("")
	assert.ErrorIs(t, err, errZeroLengthRequest, "adb rejects a zero length prefix, so it must not be produced")

	_, err = EncodeRequest(strings.Repeat("a", maxRequestPayload+1))
	assert.Error(t, err, "four hex digits cannot express the length")
}
