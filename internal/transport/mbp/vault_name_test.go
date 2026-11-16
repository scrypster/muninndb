package mbp_test

import (
	"fmt"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// helloNaming sends one HELLO with no key naming vault, and returns the
// refusal's message and how many bytes the refusal's payload was. The
// connection is closed before it returns.
func helloNaming(t *testing.T, addr, vault string) (message string, replyBytes int) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = conn.Write(helloBytes(t, vault))
	f, err := mbp.ReadFrame(conn)
	if err != nil {
		t.Fatalf("no answer to a HELLO naming a %d-byte vault: %v", len(vault), err)
	}
	if f.Type != mbp.TypeError {
		t.Fatalf("HELLO naming a %d-byte vault was answered with frame 0x%02x; want an error", len(vault), f.Type)
	}
	var ep mbp.ErrorPayload
	_ = mbp.DecodeMsgpack(f.Payload, &ep)
	return ep.Message, len(f.Payload)
}

func heapInUse() int64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return int64(ms.HeapInuse)
}

// TestHelloNamingUnknownVaultsRetainsNothing pins that a client with no key
// cannot make the server keep anything by naming vaults in its handshake
// (SEC-27). Before this, each HELLO that named a vault with no configuration
// was refused, and the name was kept for the life of the process and written to
// the log: 2,000 handshakes naming distinct 32 KiB vaults left the heap 80 MiB
// larger with no connection open.
func TestHelloNamingUnknownVaultsRetainsNothing(t *testing.T) {
	env := newMatrixEnv(t)
	if err := env.store.SetVaultConfig(auth.VaultConfig{Name: "harbour-notices", Public: true}); err != nil {
		t.Fatal(err)
	}

	t.Run("a name no vault can have is refused as that, and is not repeated back", func(t *testing.T) {
		for _, name := range []string{
			strings.Repeat("v", 65),
			strings.Repeat("v", 32<<10),
			"Harbour-Notices",
			"harbour notices",
			"harbour/notices",
		} {
			message, n := helloNaming(t, env.addr, name)
			if message != "invalid vault name" {
				t.Errorf("%d-byte name %.20q: refused with %.80q; want \"invalid vault name\"", len(name), name, message)
			}
			if n > 256 {
				t.Errorf("%d-byte name: the refusal was %d bytes; it must not carry the name back", len(name), n)
			}
		}
	})

	t.Run("a well-formed name with no configuration is still told it needs a key", func(t *testing.T) {
		message, _ := helloNaming(t, env.addr, "tug-roster")
		if message != `vault "tug-roster" requires an API key` {
			t.Errorf("refused with %q", message)
		}
	})

	t.Run("2,000 handshakes naming distinct 32 KiB vaults leave the heap where it was", func(t *testing.T) {
		base := heapInUse()
		const handshakes = 2000
		suffix := strings.Repeat("v", 32<<10)
		for i := 0; i < handshakes; i++ {
			helloNaming(t, env.addr, fmt.Sprintf("%06d", i)+suffix)
		}
		grown := heapInUse() - base
		t.Logf("heap in use after %d handshakes and a collection: %+.1f MiB", handshakes, float64(grown)/(1<<20))
		// Each name is 32 KiB, so keeping them is 64 MiB or more. Eight is room
		// for whatever else the test binary did in the meantime.
		if grown > 8<<20 {
			t.Errorf("the heap grew by %.1f MiB over %d refused handshakes; the names are being kept", float64(grown)/(1<<20), handshakes)
		}
	})

	t.Run("a session with no key cannot name such a vault in a later frame either", func(t *testing.T) {
		client := dialMBP(t, env.addr, "", "harbour-notices")
		r := client.request(mbp.TypeStat, &mbp.StatRequest{Vault: strings.Repeat("v", 4096)})
		if r.frameType != mbp.TypeError || r.message != "invalid vault name" {
			t.Errorf("STAT naming a 4096-byte vault: frame 0x%02x %.80q; want an error saying \"invalid vault name\"", r.frameType, r.message)
		}
	})

	t.Run("a key is not told a 4096-byte name back when it names a vault that is not its own", func(t *testing.T) {
		if err := env.store.SetVaultConfig(auth.VaultConfig{Name: "pilot-station", Public: false}); err != nil {
			t.Fatal(err)
		}
		token, _, err := env.store.GenerateAPIKey("pilot-station", "names", auth.ModeFull, nil)
		if err != nil {
			t.Fatal(err)
		}
		client := dialMBP(t, env.addr, token, "pilot-station")
		r := client.request(mbp.TypeStat, &mbp.StatRequest{Vault: strings.Repeat("v", 4096)})
		if r.frameType != mbp.TypeError || r.message != "invalid vault name" {
			t.Errorf("frame 0x%02x %.80q; want an error saying \"invalid vault name\"", r.frameType, r.message)
		}
		// Its own vault, and a well-formed other vault, are answered as before.
		if r := client.request(mbp.TypeStat, &mbp.StatRequest{Vault: "pilot-station"}); r.frameType != mbp.TypeStatResp {
			t.Errorf("its own vault: frame 0x%02x %q", r.frameType, r.message)
		}
		if r := client.request(mbp.TypeStat, &mbp.StatRequest{Vault: "harbour-notices"}); r.message != `api key is not authorized for vault "harbour-notices"` {
			t.Errorf("another vault: %q", r.message)
		}
	})
}
