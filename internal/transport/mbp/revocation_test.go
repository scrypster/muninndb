package mbp_test

import (
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// keyNoLongerValidMsg is what a frame is answered with once the key its
// connection was opened with has been revoked or has expired.
const keyNoLongerValidMsg = "api key is no longer valid"

// expectSessionEnded checks the answer to a frame sent on a connection whose
// session is over: the protocol's auth failure with the given message (any
// message when want is ""), and then the connection closed by the server —
// once a session has ended there is nothing more for it to be open for.
func expectSessionEnded(t *testing.T, c *mbpClient, r reply, want string) {
	t.Helper()
	if !r.isError() || r.code != mbp.ErrAuthFailed || (want != "" && r.message != want) {
		t.Errorf("answered frame 0x%02X %d %q; want error %d %q", r.frameType, r.code, r.message, mbp.ErrAuthFailed, want)
		return // the session was not ended, so the connection will not be closed either
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if f, err := mbp.ReadFrame(c.conn); err == nil {
		t.Errorf("the connection is still open and sent frame 0x%02X; want it closed by the server", f.Type)
	} else if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
		t.Errorf("the connection was still open 10s after its session ended")
	}
}

// TestRevokedKeyStopsWorkingOnAnOpenConnection pins what revoking a key does to
// a connection that is ALREADY open with it: the next frame — of any type, for
// a key of any mode — is refused with the protocol's auth failure, the engine
// is not reached, nothing is changed, and the server closes the connection.
//
// Before this, a key was looked at once, at HELLO. A connection opened with it
// kept its access for as long as it stayed open: after RevokeAPIKey it still
// read, wrote and hard-deleted.
//
// It is the mode matrix again, with every key revoked between the handshake
// and the frame.
func TestRevokedKeyStopsWorkingOnAnOpenConnection(t *testing.T) {
	env := newMatrixEnv(t)
	const vault = "pilot-station"
	if err := env.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: false}); err != nil {
		t.Fatalf("configure %s: %v", vault, err)
	}

	for _, mode := range []string{auth.ModeFull, auth.ModeWrite, auth.ModeAppend, auth.ModeObserve} {
		t.Run(mode, func(t *testing.T) {
			for _, op := range matrixOps {
				t.Run(op.name, func(t *testing.T) {
					token, key, err := env.store.GenerateAPIKey(vault, "revoked-"+mode, mode, nil)
					if err != nil {
						t.Fatalf("GenerateAPIKey(%s): %v", mode, err)
					}
					client := dialMBP(t, env.addr, token, vault)
					// The connection is open and works.
					if r := client.request(mbp.TypePing, &mbp.PingRequest{Data: "watch"}); r.frameType != mbp.TypePong {
						t.Fatalf("before revocation, PING answered with frame 0x%02X %q", r.frameType, r.message)
					}
					c := newCell(t, env, vault)
					req := op.request(c)

					if err := env.store.RevokeAPIKey(vault, key.ID); err != nil {
						t.Fatalf("RevokeAPIKey: %v", err)
					}

					env.rec.take()
					r := client.request(op.frame, req)
					if reached := env.rec.take(); len(reached) != 0 {
						t.Errorf("the frame reached the engine (%v) on a connection whose key was revoked", reached)
					}
					op.check(c, r, false) // nothing was read out or changed
					expectSessionEnded(t, client, r, keyNoLongerValidMsg)
				})
			}
		})
	}
}

// TestEndedSessionIsToldNothingElse: once the session is over, that is the
// answer to everything — a frame type the server does not route, a frame the
// key's mode would have refused anyway, garbage flagged as compressed. The
// session check comes before the frame is looked at in any other way, so a
// dead session learns nothing about the server from what it sends.
func TestEndedSessionIsToldNothingElse(t *testing.T) {
	env := newMatrixEnv(t)
	const vault = "pilot-station"
	if err := env.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: false}); err != nil {
		t.Fatalf("configure %s: %v", vault, err)
	}
	garbage := []byte("flagged as compressed, and nothing of the kind")
	for name, send := range map[string]func(c *mbpClient) reply{
		"an unrouted frame type":            func(c *mbpClient) reply { return c.send(0x7E, nil) },
		"a cluster frame type":              func(c *mbpClient) reply { return c.send(mbp.TypeJoinRequest, nil) },
		"a second HELLO":                    func(c *mbpClient) reply { return c.send(mbp.TypeHello, nil) },
		"a frame the mode refuses anyway":   func(c *mbpClient) reply { return c.send(mbp.TypeWrite, nil) },
		"compressed garbage, routed type":   func(c *mbpClient) reply { return c.sendFlags(mbp.TypeStat, mbp.FlagCompressed, garbage) },
		"compressed garbage, unrouted type": func(c *mbpClient) reply { return c.sendFlags(0x7E, mbp.FlagCompressed, garbage) },
	} {
		t.Run(name, func(t *testing.T) {
			token, key, err := env.store.GenerateAPIKey(vault, "ended", auth.ModeObserve, nil)
			if err != nil {
				t.Fatalf("GenerateAPIKey: %v", err)
			}
			client := dialMBP(t, env.addr, token, vault)
			if r := client.request(mbp.TypePing, &mbp.PingRequest{Data: "watch"}); r.frameType != mbp.TypePong {
				t.Fatalf("before revocation, PING answered with frame 0x%02X %q", r.frameType, r.message)
			}
			if err := env.store.RevokeAPIKey(vault, key.ID); err != nil {
				t.Fatalf("RevokeAPIKey: %v", err)
			}
			env.rec.take()
			r := send(client)
			if reached := env.rec.take(); len(reached) != 0 {
				t.Errorf("reached the engine: %v", reached)
			}
			expectSessionEnded(t, client, r, keyNoLongerValidMsg)
		})
	}
}

// TestExpiredKeyStopsWorkingOnAnOpenConnection: a key's expiry applies to the
// connections it opened, not only to new ones. A connection opened a moment
// before its key expires is refused the first frame it sends after.
func TestExpiredKeyStopsWorkingOnAnOpenConnection(t *testing.T) {
	env := newMatrixEnv(t)
	const vault = "pilot-station"
	if err := env.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: false}); err != nil {
		t.Fatalf("configure %s: %v", vault, err)
	}
	expires := time.Now().Add(700 * time.Millisecond)
	token, _, err := env.store.GenerateAPIKey(vault, "short-lived", auth.ModeFull, &expires)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	client := dialMBP(t, env.addr, token, vault)
	c := newCell(t, env, vault)
	c.seedMemory()
	if r := client.request(mbp.TypeRead, &mbp.ReadRequest{Vault: vault, ID: c.memory}); r.frameType != mbp.TypeReadResp {
		t.Fatalf("before expiry, READ answered with frame 0x%02X %q", r.frameType, r.message)
	}

	time.Sleep(time.Until(expires) + 100*time.Millisecond)

	env.rec.take()
	r := client.request(mbp.TypeRead, &mbp.ReadRequest{Vault: vault, ID: c.memory})
	if reached := env.rec.take(); len(reached) != 0 {
		t.Errorf("the frame reached the engine (%v) on a connection whose key had expired", reached)
	}
	expectSessionEnded(t, client, r, keyNoLongerValidMsg)
}

// TestRevokingOneKeyLeavesOtherConnectionsAlone: the check is of the
// connection's own key. Revoking a key ends the connections opened with it and
// no others — not another key's on the same vault, and not the same key's
// frames that were already being handled.
func TestRevokingOneKeyLeavesOtherConnectionsAlone(t *testing.T) {
	env := newMatrixEnv(t)
	const vault = "pilot-station"
	if err := env.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: false}); err != nil {
		t.Fatalf("configure %s: %v", vault, err)
	}
	revokedToken, revoked, err := env.store.GenerateAPIKey(vault, "to-revoke", auth.ModeFull, nil)
	if err != nil {
		t.Fatal(err)
	}
	keptToken, _, err := env.store.GenerateAPIKey(vault, "to-keep", auth.ModeFull, nil)
	if err != nil {
		t.Fatal(err)
	}
	gone := dialMBP(t, env.addr, revokedToken, vault)
	kept := dialMBP(t, env.addr, keptToken, vault)

	// A WRITE on the soon-to-be-revoked connection is already in the engine
	// when the key is revoked.
	env.rec.writeEntered = make(chan struct{}, 1)
	env.rec.writeHold = make(chan struct{})
	payload, err := mbp.EncodeMsgpack(&mbp.WriteRequest{Vault: vault, Concept: "Pilot ladder", Content: "The pilot ladder on the east quay was renewed."})
	if err != nil {
		t.Fatal(err)
	}
	gone.post(mbp.TypeWrite, 0, payload)
	select {
	case <-env.rec.writeEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("the WRITE never reached the engine")
	}
	if err := env.store.RevokeAPIKey(vault, revoked.ID); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
	env.rec.writeHold <- struct{}{}
	env.rec.writeHold = nil

	// It was admitted while the key was valid, so it is carried out and
	// answered; the frame after it is not.
	_ = gone.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if f, err := mbp.ReadFrame(gone.conn); err != nil || f.Type != mbp.TypeWriteOK {
		t.Fatalf("the WRITE admitted before the revocation: frame %v, err %v; want WRITE_OK", f, err)
	}
	r := gone.request(mbp.TypePing, &mbp.PingRequest{Data: "watch"})
	expectSessionEnded(t, gone, r, keyNoLongerValidMsg)

	// The other key's connection never noticed.
	if r := kept.request(mbp.TypePing, &mbp.PingRequest{Data: "watch"}); r.frameType != mbp.TypePong {
		t.Errorf("the connection of a key that was not revoked: PING answered frame 0x%02X %q", r.frameType, r.message)
	}
	c := newCell(t, env, vault)
	c.seedMemory()
	if r := kept.request(mbp.TypeRead, &mbp.ReadRequest{Vault: vault, ID: c.memory}); r.frameType != mbp.TypeReadResp {
		t.Errorf("the connection of a key that was not revoked: READ answered frame 0x%02X %q", r.frameType, r.message)
	}
}

// TestLockedVaultEndsAKeylessSession: a session with no key exists because the
// vault it was opened on is public. When the operator locks that vault the
// session is over — for every frame type, including the ones that name no
// vault and so were never checked against one.
func TestLockedVaultEndsAKeylessSession(t *testing.T) {
	env := newMatrixEnv(t)
	const vault = "harbour-notices"
	if err := env.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: true}); err != nil {
		t.Fatalf("configure %s: %v", vault, err)
	}

	for _, op := range matrixOps {
		t.Run(op.name, func(t *testing.T) {
			if err := env.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: true}); err != nil {
				t.Fatalf("open %s: %v", vault, err)
			}
			client := dialMBP(t, env.addr, "", vault)
			if r := client.request(mbp.TypePing, &mbp.PingRequest{Data: "watch"}); r.frameType != mbp.TypePong {
				t.Fatalf("while the vault was public, PING answered with frame 0x%02X %q", r.frameType, r.message)
			}
			c := newCell(t, env, vault)
			req := op.request(c)

			if err := env.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: false}); err != nil {
				t.Fatalf("lock %s: %v", vault, err)
			}

			env.rec.take()
			r := client.request(op.frame, req)
			if reached := env.rec.take(); len(reached) != 0 {
				t.Errorf("the frame reached the engine (%v) after the vault was locked", reached)
			}
			op.check(c, r, false)
			expectSessionEnded(t, client, r, "vault \""+vault+"\" requires an API key")
		})
	}
}
