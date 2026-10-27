package adbwire

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGivenTheHostVersionRequestWhenFramedByTheEncoderThenTheConstantMatches(t *testing.T) {
	framed, err := EncodeRequest("host:version")
	require.NoError(t, err)
	assert.Equal(t, HostVersionRequest, string(framed))
}

func TestGivenHostVersionRepliesWhenParsedThenOnlyAFourHexDigitVersionIsRead(t *testing.T) {
	tests := []struct {
		name      string
		reply     string
		want      int
		wantError string
	}{
		{name: "adb 1.0.41", reply: "00040029", want: 41},
		{name: "the largest version", reply: "0004ffff", want: 65535},
		{name: "extra bytes are ignored", reply: "00040027tail", want: 39},
		{name: "a short reply", reply: "0004", wantError: "has 4 bytes after OKAY, not 8"},
		{name: "a length that is not 0004", reply: "00080029abcd", wantError: `has length "0008", not 0004`},
		{name: "digits that are not hex", reply: "0004zz29", wantError: `carries "zz29", not four hex digits`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			version, err := ParseHostVersionReply([]byte(tc.reply))
			if tc.wantError != "" {
				assert.ErrorContains(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, version)
		})
	}
}
