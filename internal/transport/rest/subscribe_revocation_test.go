package rest

import (
	"bufio"
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// pushEnv is a REST server over a real engine whose trigger system is running,
// so a write really is pushed to the streams subscribed to its vault. Fixture:
// a fictional harbour pilot station.
type pushEnv struct {
	eng      *engine.Engine
	store    *auth.Store
	pstore   *storage.PebbleStore
	triggers *trigger.TriggerSystem
	url      string
	session  string
}

// newPushEnv starts the server. keepalive is how often an open stream is sent
// a ping; zero keeps the server's own interval.
func newPushEnv(t *testing.T, keepalive time.Duration) *pushEnv {
	t.Helper()
	opts := storage.DefaultOptions()
	opts.FS = vfs.NewMem()
	db, err := storage.OpenPebble("rest-push", opts)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	ftsIdx := fts.New(db)
	embedder := activation.NewNoopEmbedder()
	authStore := auth.NewStore(db)
	triggers := trigger.New(store, trigger.NewFTSAdapter(ftsIdx), nil, embedder)
	ctx, cancel := context.WithCancel(context.Background())
	go triggers.Start(ctx)
	eng := engine.NewEngine(engine.EngineConfig{
		Store: store, AuthStore: authStore, FTSIndex: ftsIdx,
		ActivationEngine: activation.New(store, activation.NewFTSAdapter(ftsIdx), nil, embedder),
		TriggerSystem:    triggers,
		Embedder:         embedder,
	})
	secret := []byte("pilot-station-session-secret-32b")
	srv := NewServer("127.0.0.1:0", NewEngineWrapper(eng, nil), authStore, secret, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
	if keepalive > 0 {
		srv.sseKeepalive = keepalive
	}
	ts := httptest.NewServer(srv.Handler())
	session, err := auth.NewSessionToken("admin", secret)
	if err != nil {
		t.Fatalf("NewSessionToken: %v", err)
	}
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
		cancel()
		eng.Stop()
		store.Close()
	})
	return &pushEnv{eng: eng, store: authStore, pstore: store, triggers: triggers, url: ts.URL, session: session}
}

func (e *pushEnv) write(t *testing.T, vault, berth string) {
	t.Helper()
	full := context.WithValue(context.Background(), auth.ContextMode, auth.ModeFull)
	_, err := e.eng.Write(full, &mbp.WriteRequest{
		Vault: vault, Concept: "berth notice " + berth, Content: "berth " + berth + " closed for dredging",
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
}

func (e *pushEnv) subscriptions(vault string) int {
	ws := e.pstore.ResolveVaultPrefix(vault)
	return len(e.triggers.ForVault(binary.BigEndian.Uint32(ws[:4])))
}

func (e *pushEnv) awaitSubscriptions(t *testing.T, vault string, want int, when string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for e.subscriptions(vault) != want {
		if time.Now().After(deadline) {
			t.Fatalf("%s: vault %s has %d subscriptions; want %d", when, vault, e.subscriptions(vault), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// sseStream is one open GET /api/subscribe, read event by event.
type sseStream struct {
	events chan sseEvent // closed when the server ends the stream
	close  func()
}

type sseEvent struct{ name, data string }

func (e *pushEnv) subscribe(t *testing.T, vault, bearer string, admin bool) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		e.url+"/api/subscribe?vault="+vault+"&push_on_write=true&threshold=0.01&context=berth", nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if admin {
		req.AddCookie(&http.Cookie{Name: "muninn_session", Value: e.session})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("subscribe: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("subscribe: HTTP %d", resp.StatusCode)
	}
	s := &sseStream{events: make(chan sseEvent, 64), close: func() { cancel(); resp.Body.Close() }}
	t.Cleanup(s.close)
	go func() {
		defer close(s.events)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				ev.name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				ev.data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			case line == "":
				if ev.name != "" || ev.data != "" {
					s.events <- ev
				}
				ev = sseEvent{}
			}
		}
	}()
	if ev := s.next(t, 5*time.Second); ev.name != "subscribed" {
		t.Fatalf("first event = %+v; want subscribed", ev)
	}
	return s
}

// next returns the next event that is not a keepalive ping. A stream the server
// has ended yields the event named "closed"; one that stays silent for the
// whole wait yields "silent".
func (s *sseStream) next(t *testing.T, wait time.Duration) sseEvent {
	t.Helper()
	deadline := time.After(wait)
	for {
		select {
		case ev, ok := <-s.events:
			if !ok {
				return sseEvent{name: "closed"}
			}
			if ev.name == "ping" {
				continue
			}
			return ev
		case <-deadline:
			return sseEvent{name: "silent"}
		}
	}
}

// isPush reports whether ev is a push carrying a memory.
func isPush(ev sseEvent) bool {
	return ev.name == "push" && strings.Contains(ev.data, `"content"`)
}

// endsWithout reads the stream until the server closes it, and fails if a push
// carrying a memory arrives first. It returns the error event the server sent
// before closing, if any.
func (s *sseStream) endsWithout(t *testing.T, what string) sseEvent {
	t.Helper()
	var last sseEvent
	deadline := time.Now().Add(10 * time.Second)
	for {
		ev := s.next(t, time.Until(deadline))
		switch {
		case isPush(ev):
			t.Fatalf("%s: a push carrying a memory was delivered: %.120s", what, ev.data)
		case ev.name == "closed":
			return last
		case ev.name == "silent":
			t.Fatalf("%s: the stream was still open after 10s", what)
		}
		last = ev
	}
}

// TestPushStreamEndsWhenItsAuthorisationDoes pins that an open SSE stream is
// authorised for as long as what authorised it holds, not once at open
// (SEC-23). Before this, a stream opened with a key went on delivering pushes,
// memory content included, after the key was revoked or had expired, while the
// same key was answered 401 on every other route.
func TestPushStreamEndsWhenItsAuthorisationDoes(t *testing.T) {
	const locked, public = "pilot-station", "harbour-notices"

	setup := func(t *testing.T, keepalive time.Duration) *pushEnv {
		env := newPushEnv(t, keepalive)
		for vault, open := range map[string]bool{locked: false, public: true} {
			if err := env.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: open}); err != nil {
				t.Fatalf("configure %s: %v", vault, err)
			}
		}
		return env
	}
	mustPush := func(t *testing.T, s *sseStream, when string) {
		t.Helper()
		if ev := s.next(t, 5*time.Second); !isPush(ev) {
			t.Fatalf("%s: got %+v; want a push carrying the memory", when, ev)
		}
	}

	t.Run("revoked key gets nothing after the revocation", func(t *testing.T) {
		env := setup(t, 0)
		token, key, err := env.store.GenerateAPIKey(locked, "revokee", auth.ModeObserve, nil)
		if err != nil {
			t.Fatal(err)
		}
		s := env.subscribe(t, locked, token, false)
		env.write(t, locked, "four")
		mustPush(t, s, "before revocation")

		if err := env.store.RevokeAPIKey(locked, key.ID); err != nil {
			t.Fatal(err)
		}
		env.write(t, locked, "nine")
		last := s.endsWithout(t, "after revocation")
		if last.name != "error" || !strings.Contains(last.data, "api key is no longer valid") {
			t.Errorf("the stream ended with %+v; want an error event saying the key is no longer valid", last)
		}
		env.awaitSubscriptions(t, locked, 0, "after the revoked stream ended")
	})

	t.Run("expired key gets nothing after it expires", func(t *testing.T) {
		env := setup(t, 0)
		expires := time.Now().Add(1500 * time.Millisecond)
		token, _, err := env.store.GenerateAPIKey(locked, "short-lived", auth.ModeFull, &expires)
		if err != nil {
			t.Fatal(err)
		}
		s := env.subscribe(t, locked, token, false)
		env.write(t, locked, "four")
		mustPush(t, s, "before expiry")

		time.Sleep(time.Until(expires) + 50*time.Millisecond)
		env.write(t, locked, "nine")
		s.endsWithout(t, "after expiry")
		env.awaitSubscriptions(t, locked, 0, "after the expired stream ended")
	})

	t.Run("a stream with no key ends when its vault stops being public", func(t *testing.T) {
		env := setup(t, 0)
		s := env.subscribe(t, public, "", false)
		env.write(t, public, "four")
		mustPush(t, s, "while public")

		if err := env.store.SetVaultConfig(auth.VaultConfig{Name: public, Public: false}); err != nil {
			t.Fatal(err)
		}
		env.write(t, public, "nine")
		last := s.endsWithout(t, "after the vault was locked")
		if last.name != "error" || !strings.Contains(last.data, "requires an API key") {
			t.Errorf("the stream ended with %+v; want an error event saying the vault requires an API key", last)
		}
		env.awaitSubscriptions(t, public, 0, "after the keyless stream ended")
	})

	t.Run("a revoked key's idle stream ends at the keepalive, with no push to prompt it", func(t *testing.T) {
		env := setup(t, 100*time.Millisecond)
		token, key, err := env.store.GenerateAPIKey(locked, "idle", auth.ModeObserve, nil)
		if err != nil {
			t.Fatal(err)
		}
		s := env.subscribe(t, locked, token, false)
		env.awaitSubscriptions(t, locked, 1, "after subscribing")
		if err := env.store.RevokeAPIKey(locked, key.ID); err != nil {
			t.Fatal(err)
		}
		// Nothing is written: only the keepalive can notice.
		s.endsWithout(t, "idle after revocation")
		env.awaitSubscriptions(t, locked, 0, "after the idle revoked stream ended")
	})

	t.Run("revoking one key leaves another key's stream and an admin stream alone", func(t *testing.T) {
		env := setup(t, 100*time.Millisecond)
		goneToken, goneKey, err := env.store.GenerateAPIKey(locked, "gone", auth.ModeObserve, nil)
		if err != nil {
			t.Fatal(err)
		}
		keptToken, _, err := env.store.GenerateAPIKey(locked, "kept", auth.ModeObserve, nil)
		if err != nil {
			t.Fatal(err)
		}
		gone := env.subscribe(t, locked, goneToken, false)
		kept := env.subscribe(t, locked, keptToken, false)
		admin := env.subscribe(t, locked, "", true)
		if err := env.store.RevokeAPIKey(locked, goneKey.ID); err != nil {
			t.Fatal(err)
		}
		gone.endsWithout(t, "the revoked key's stream")

		// Several keepalives have passed for the other two by now.
		env.write(t, locked, "nine")
		mustPush(t, kept, "the other key's stream")
		mustPush(t, admin, "the admin session's stream")
		env.awaitSubscriptions(t, locked, 2, "after one of three streams ended")
	})
}
