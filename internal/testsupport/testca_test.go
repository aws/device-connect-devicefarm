package testsupport

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"
)

// handshakeAs opens a TLS connection to the mock behind wssURL, trusting the test CA
// and verifying the server as serverName. Returns the handshake error.
func handshakeAs(t *testing.T, wssURL, serverName string) error {
	t.Helper()
	parsed, err := url.Parse(wssURL)
	require.NoError(t, err)
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: testTimeout}, "tcp", parsed.Host,
		&tls.Config{RootCAs: TestRoots(), ServerName: serverName})
	if err == nil {
		conn.Close()
	}
	return err
}

func TestGivenTheLoopbackLeafWhenVerifiedAsEachSupportedNameThenTheHandshakeSucceeds(t *testing.T) {
	wssURL := NewMockWSS(t, func(serverConn *websocket.Conn) { serverConn.Close() })

	for _, name := range []string{"127.0.0.1", "::1", "localhost"} {
		t.Run(name, func(t *testing.T) {
			assert.NoError(t, handshakeAs(t, wssURL, name))
		})
	}
}

func TestGivenALeafForAnotherNameWhenVerifiedAsLoopbackThenOnlyTheNameIsRefused(t *testing.T) {
	// Both handshakes trust the chain; only the name differs, so the refusal is
	// hostname verification alone.
	wssURL := NewMockWSSForName(t, func(serverConn *websocket.Conn) { serverConn.Close() }, "other.invalid")

	var hostnameErr x509.HostnameError
	require.ErrorAs(t, handshakeAs(t, wssURL, "127.0.0.1"), &hostnameErr)
	assert.NoError(t, handshakeAs(t, wssURL, "other.invalid"))
}
