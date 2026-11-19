package adbwire

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGivenWellFormedSpecsWhenParsedThenKindAndValueSplitAtFirstColon(t *testing.T) {
	tests := []struct {
		text string
		spec AdbSpec
	}{
		{text: "tcp:1234", spec: AdbSpec{Kind: "tcp", Value: "1234"}},
		{text: "tcp:0", spec: AdbSpec{Kind: "tcp", Value: "0"}},
		{text: "tcp:", spec: AdbSpec{Kind: "tcp", Value: ""}},
		{text: "localabstract:chrome_devtools_remote", spec: AdbSpec{Kind: "localabstract", Value: "chrome_devtools_remote"}},
		{text: "vsock:3:5555", spec: AdbSpec{Kind: "vsock", Value: "3:5555"}},
	}

	for _, testCase := range tests {
		t.Run(testCase.text, func(t *testing.T) {
			spec, err := ParseSpec(testCase.text)

			require.NoError(t, err)
			assert.Equal(t, testCase.spec, spec)
			assert.Equal(t, testCase.text, spec.String(), "String must render the wire form back")
		})
	}
}

func TestGivenMalformedSpecsWhenParsedThenAdbListenerPathFailureText(t *testing.T) {
	tests := []struct {
		text    string
		failure string
	}{
		{text: "1234", failure: "unknown socket specification:1234"},
		{text: ":1234", failure: "unknown socket specification::1234"},
		{text: "", failure: "unknown socket specification:"},
	}

	for _, testCase := range tests {
		t.Run(testCase.text, func(t *testing.T) {
			_, err := ParseSpec(testCase.text)

			require.Error(t, err)
			assert.Equal(t, testCase.failure, err.Error())
		})
	}
}

func TestGivenTcpSpecsWhenPortExtractedThenValidatedLikeAdb(t *testing.T) {
	tests := []struct {
		name    string
		spec    AdbSpec
		port    int
		failure string
	}{
		{name: "plain port", spec: AdbSpec{Kind: "tcp", Value: "1234"}, port: 1234},
		{name: "port zero asks for resolution", spec: AdbSpec{Kind: "tcp", Value: "0"}, port: 0},
		{name: "highest port", spec: AdbSpec{Kind: "tcp", Value: "65535"}, port: 65535},
		{name: "signed port", spec: AdbSpec{Kind: "tcp", Value: "+80"}, port: 80},
		{name: "leading zeros", spec: AdbSpec{Kind: "tcp", Value: "0080"}, port: 80},
		{name: "leading zeros are not octal", spec: AdbSpec{Kind: "tcp", Value: "017700"}, port: 17700},
		{name: "hex port", spec: AdbSpec{Kind: "tcp", Value: "0x50"}, port: 80},
		{name: "uppercase hex port", spec: AdbSpec{Kind: "tcp", Value: "0X50"}, port: 80},
		{name: "blank-padded port", spec: AdbSpec{Kind: "tcp", Value: " 80"}, port: 80},
		{name: "blank-padded hex port", spec: AdbSpec{Kind: "tcp", Value: " 0x50"}, port: 80},
		{name: "negative zero", spec: AdbSpec{Kind: "tcp", Value: "-0"}, port: 0},
		{name: "port too high", spec: AdbSpec{Kind: "tcp", Value: "65536"}, failure: "bad port number '65536'"},
		{name: "negative port", spec: AdbSpec{Kind: "tcp", Value: "-1"}, failure: "bad port number '-1'"},
		{name: "empty value has no host", spec: AdbSpec{Kind: "tcp", Value: ""}, failure: "no host in ''"},
		{name: "non-numeric value is a hostname", spec: AdbSpec{Kind: "tcp", Value: "abc"}, failure: "listening on specified hostname currently unsupported"},
		{name: "value past a C int is a hostname", spec: AdbSpec{Kind: "tcp", Value: "2147483648"}, failure: "listening on specified hostname currently unsupported"},
		{name: "signed hex value is a hostname", spec: AdbSpec{Kind: "tcp", Value: "0x-5"}, failure: "listening on specified hostname currently unsupported"},
		{name: "hex prefix with no digits is a hostname", spec: AdbSpec{Kind: "tcp", Value: "0x"}, failure: "listening on specified hostname currently unsupported"},
		{name: "trailing blank is a hostname", spec: AdbSpec{Kind: "tcp", Value: "80 "}, failure: "listening on specified hostname currently unsupported"},
		{name: "hex port past a C int is a hostname", spec: AdbSpec{Kind: "tcp", Value: "0xFFFFFFFF"}, failure: "listening on specified hostname currently unsupported"},
		{name: "loopback host form is not mirrored", spec: AdbSpec{Kind: "tcp", Value: "localhost:8080"}, failure: "listening on specified hostname currently unsupported"},
		{name: "not tcp", spec: AdbSpec{Kind: "localabstract", Value: "x"}, failure: "specification is not tcp: localabstract:x"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			port, err := testCase.spec.TCPPort()

			if testCase.failure != "" {
				require.Error(t, err)
				assert.Equal(t, testCase.failure, err.Error())
				return
			}
			require.NoError(t, err)
			assert.Equal(t, testCase.port, port)
		})
	}
}
