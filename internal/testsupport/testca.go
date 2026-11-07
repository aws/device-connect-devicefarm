// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package testsupport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"net"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// testAuthority is the private CA behind every TLS mock, minted once per process.
// Its loopback leaf names 127.0.0.1, ::1 and localhost.
type testAuthority struct {
	certificate  *x509.Certificate
	signingKey   *ecdsa.PrivateKey
	loopbackLeaf tls.Certificate
}

var loadAuthority = sync.OnceValue(newTestAuthority)

// must panics on a mint failure: unrecoverable for the test binary, as in httptest.
func must[T any](value T, err error) T {
	if err != nil {
		panic(fmt.Sprintf("testsupport: mint test certificate: %v", err))
	}
	return value
}

// newTestAuthority mints the CA and its loopback leaf. crypto/x509 picks random
// serials for the nil SerialNumber fields.
func newTestAuthority() *testAuthority {
	signingKey := must(ecdsa.GenerateKey(elliptic.P256(), rand.Reader))
	template := &x509.Certificate{
		Subject:               pkix.Name{CommonName: "devicefarm-connect test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	derBytes := must(x509.CreateCertificate(rand.Reader, template, template, &signingKey.PublicKey, signingKey))
	authority := &testAuthority{certificate: must(x509.ParseCertificate(derBytes)), signingKey: signingKey}
	authority.loopbackLeaf = authority.issue([]string{"localhost"}, []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback})
	return authority
}

// issue signs a server leaf for the given names with a fresh P-256 key.
func (authority *testAuthority) issue(dnsNames []string, ipAddresses []net.IP) tls.Certificate {
	leafKey := must(ecdsa.GenerateKey(elliptic.P256(), rand.Reader))
	template := &x509.Certificate{
		Subject:     pkix.Name{CommonName: "devicefarm-connect test server"},
		NotBefore:   time.Now().Add(-time.Hour),
		NotAfter:    time.Now().Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    dnsNames,
		IPAddresses: ipAddresses,
	}
	derBytes := must(x509.CreateCertificate(rand.Reader, template, authority.certificate, &leafKey.PublicKey, authority.signingKey))
	return tls.Certificate{Certificate: [][]byte{derBytes}, PrivateKey: leafKey}
}

// TestRoots returns a pool holding only the test CA, for bridge.WSDialer.TrustRoots.
func TestRoots() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(loadAuthority().certificate)
	return pool
}

// NewMockWSSForName serves a leaf for one DNS name instead of the loopback names, so
// a dial to 127.0.0.1 must fail hostname verification even with TestRoots.
func NewMockWSSForName(t *testing.T, handler func(*websocket.Conn), name string) string {
	t.Helper()
	return startTLSWSS(t, handler, loadAuthority().issue([]string{name}, nil))
}

// acceptedConnKey keys a websocket's accepted TLS connection in its request context, so a mock
// can drop it with no close frame (see dropWithoutCloseFrame).
type acceptedConnKey struct{}

func startTLSWSS(t *testing.T, handler func(*websocket.Conn), leaf tls.Certificate) string {
	t.Helper()
	server := httptest.NewUnstartedServer(websocket.Handler(handler))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	server.Config.ConnContext = func(ctx context.Context, conn net.Conn) context.Context {
		return context.WithValue(ctx, acceptedConnKey{}, conn)
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	return fmt.Sprintf("wss://%s/adb", server.Listener.Addr())
}
