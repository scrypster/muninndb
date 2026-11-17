package mbp_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// selfSignedTLS returns a server TLS configuration with a certificate made for
// this test run.
func selfSignedTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "pilot-station.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

// helloOver completes a HELLO for a session with no key on an existing
// connection and reports whether the session was established.
func helloOver(t *testing.T, conn net.Conn, vault string) bool {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	defer conn.SetDeadline(time.Time{})
	if _, err := conn.Write(helloBytes(t, vault)); err != nil {
		return false
	}
	f, err := mbp.ReadFrame(conn)
	return err == nil && f.Type == mbp.TypeHelloOK
}

// TestRefusingAtTheCapNeverHoldsUpAccept: a connection the listener has no
// place for must cost the accept loop nothing — whatever the peer does, and in
// particular when it does nothing at all.
//
// The case that matters is a TLS listener. There the first write to a
// connection runs the TLS handshake, which READS from the peer, and a write
// deadline does not bound that read. When the refusal was written from the
// accept loop itself, one peer that connected at the cap and stayed silent
// stopped the listener accepting anyone: the next client could not connect
// even after a place had come free.
//
// It is run on a plain listener too, where it also has to hold.
func TestRefusingAtTheCapNeverHoldsUpAccept(t *testing.T) {
	const vault = "harbour-notices"
	for _, tc := range []struct {
		name string
		tls  bool
	}{{"plain TCP", false}, {"TLS", true}} {
		t.Run(tc.name, func(t *testing.T) {
			var serverTLS *tls.Config
			if tc.tls {
				serverTLS = selfSignedTLS(t)
			}
			const limit = 2
			env := newMatrixEnvTLS(t, serverTLS, func(s *mbp.Server) {
				s.SetMaxConnectionsForTest(limit)
				s.SetHandshakeTimeoutForTest(30 * time.Second) // long: nothing here is rescued by it
			})
			if err := env.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: true}); err != nil {
				t.Fatalf("configure %s: %v", vault, err)
			}
			dial := func() (net.Conn, error) {
				if tc.tls {
					return tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", env.addr, &tls.Config{InsecureSkipVerify: true})
				}
				return net.DialTimeout("tcp", env.addr, 5*time.Second)
			}

			// Fill the listener.
			held := make([]net.Conn, limit)
			for i := range held {
				conn, err := dial()
				if err != nil {
					t.Fatalf("dial %d: %v", i+1, err)
				}
				t.Cleanup(func() { conn.Close() })
				if !helloOver(t, conn, vault) {
					t.Fatalf("connection %d of %d was not established", i+1, limit)
				}
				held[i] = conn
			}

			// Peers that connect at the cap and then do nothing: no TLS
			// handshake, no bytes, no reading. They keep their sockets open.
			const silent = 8
			for i := 0; i < silent; i++ {
				conn, err := net.DialTimeout("tcp", env.addr, 5*time.Second)
				if err != nil {
					t.Fatalf("silent peer %d: %v", i+1, err)
				}
				t.Cleanup(func() { conn.Close() })
			}
			time.Sleep(100 * time.Millisecond) // let the accept loop meet them

			// A place comes free. A real client must get it promptly, though
			// the silent peers are all still there.
			held[0].Close()
			start := time.Now()
			const bound = 5 * time.Second
			for {
				conn, err := dial()
				if err == nil {
					t.Cleanup(func() { conn.Close() })
					if helloOver(t, conn, vault) {
						break
					}
				}
				if time.Since(start) > bound {
					t.Fatalf("no connection was admitted within %s of a place coming free, with %d silent peers waiting at the cap: the accept loop is waiting on one of them", bound, silent)
				}
				time.Sleep(20 * time.Millisecond)
			}
			t.Logf("admitted %s after the place came free, with %d silent peers at the cap", time.Since(start).Round(time.Millisecond), silent)

			// The connections that were there throughout are still served.
			if !pingOK(held[1]) {
				t.Errorf("an established connection stopped being served")
			}
		})
	}
}
