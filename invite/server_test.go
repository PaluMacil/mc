package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubWhitelist struct {
	resp       string
	err        error
	calls      atomic.Int32
	players    OnlinePlayers
	playersErr error
}

func (s *stubWhitelist) WhitelistAdd(_ context.Context, _ string) (string, error) {
	s.calls.Add(1)
	return s.resp, s.err
}

func (s *stubWhitelist) ListPlayers(_ context.Context) (OnlinePlayers, error) {
	return s.players, s.playersErr
}

// newTestServer wires a Server against the test Postgres with a stubbed Mojang
// (only knownName resolves) and a stubbed RCON.
func newTestServer(t *testing.T, wl minecraftRCON, limiter *ipLimiter) (*Server, *Store) {
	t.Helper()
	store := newTestStore(t)

	mojang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users/profiles/minecraft/Palu_Macil" {
			w.Write([]byte(`{"name":"Palu_Macil","id":"892ab8259c2d46cb94d5f6ce6babfd00"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(mojang.Close)

	base, _ := url.Parse("https://mc.danwolf.net/invite")
	sessions := scs.New()
	cfg := Config{
		BaseURL:       base,
		BasePath:      "/invite",
		ServerAddress: "mc.danwolf.net",
		InviteTTL:     time.Hour,
	}
	if limiter == nil {
		limiter = newIPLimiter(100, time.Second)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &Server{
		cfg:      cfg,
		store:    store,
		auth:     &Auth{cfg: cfg, sessions: sessions, log: log},
		sessions: sessions,
		mojang:   MojangResolver{BaseURL: mojang.URL, Client: mojang.Client()},
		rcon:     wl,
		players:  &playersCache{ttl: 10 * time.Second},
		limiter:  limiter,
		loc:      time.UTC,
		log:      log,
	}, store
}

func do(t *testing.T, h http.Handler, method, target, body string) (int, string) {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	r.Header.Set("Sec-Fetch-Site", "same-origin") // satisfy CrossOriginProtection on POST
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code, rec.Body.String()
}

func TestHealthAndReady(t *testing.T) {
	srv, _ := newTestServer(t, &stubWhitelist{resp: "ok"}, nil)
	h := srv.Handler()

	code, body := do(t, h, http.MethodGet, "/healthz", "")
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "ok", body)

	code, _ = do(t, h, http.MethodGet, "/readyz", "")
	assert.Equal(t, http.StatusOK, code, "readyz reflects a reachable DB")
}

func TestRedeemFlowEndToEnd(t *testing.T) {
	wl := &stubWhitelist{resp: "Added Palu_Macil to the whitelist"}
	srv, store := newTestServer(t, wl, nil)
	h := srv.Handler()
	ctx := context.Background()

	raw, hash := newToken()
	_, err := store.CreateInvite(ctx, hash, "oidc-sub-alice", "Alice", time.Hour)
	require.NoError(t, err)

	// The form renders for a valid, unused link.
	code, body := do(t, h, http.MethodGet, "/invite/i/"+raw, "")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, `name="username"`)

	// Redeeming with a valid name whitelists the player and shows success.
	code, body = do(t, h, http.MethodPost, "/invite/i/"+raw, "username=Palu_Macil")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "added to the whitelist")
	assert.Contains(t, body, "Palu_Macil")
	assert.Contains(t, body, "mc.danwolf.net")
	assert.Equal(t, int32(1), wl.calls.Load(), "whitelist granted exactly once")

	got, err := store.FindInvite(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, "used", got.Status(time.Now()))

	// The same link cannot be redeemed again.
	code, body = do(t, h, http.MethodPost, "/invite/i/"+raw, "username=Palu_Macil")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "already been used")
	assert.Equal(t, int32(1), wl.calls.Load(), "no second grant")

	// And the redemption GET page also reports it used.
	_, body = do(t, h, http.MethodGet, "/invite/i/"+raw, "")
	assert.Contains(t, body, "already been used")
}

func TestRedeemInvalidLink(t *testing.T) {
	srv, _ := newTestServer(t, &stubWhitelist{resp: "ok"}, nil)
	h := srv.Handler()

	raw, _ := newToken() // never stored
	_, body := do(t, h, http.MethodGet, "/invite/i/"+raw, "")
	assert.Contains(t, body, "not valid")
}

func TestRedeemUnknownName(t *testing.T) {
	wl := &stubWhitelist{resp: "ok"}
	srv, store := newTestServer(t, wl, nil)
	h := srv.Handler()

	raw, hash := newToken()
	_, err := store.CreateInvite(context.Background(), hash, "oidc-sub-alice", "Alice", time.Hour)
	require.NoError(t, err)

	code, body := do(t, h, http.MethodPost, "/invite/i/"+raw, "username=NoSuchPlayer")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "No Minecraft Java account")
	assert.Equal(t, int32(0), wl.calls.Load(), "no grant for an unknown name")
}

func TestRedeemRateLimited(t *testing.T) {
	wl := &stubWhitelist{resp: "ok"}
	srv, store := newTestServer(t, wl, newIPLimiter(1, time.Hour)) // 1 attempt, then blocked
	h := srv.Handler()

	raw, hash := newToken()
	_, err := store.CreateInvite(context.Background(), hash, "oidc-sub-alice", "Alice", time.Hour)
	require.NoError(t, err)

	// First POST consumes the only token (unknown name, so the invite survives).
	do(t, h, http.MethodPost, "/invite/i/"+raw, "username=NoSuchPlayer")
	// Second POST is rate limited before any Mojang/DB work.
	_, body := do(t, h, http.MethodPost, "/invite/i/"+raw, "username=Palu_Macil")
	assert.Contains(t, body, "Too many attempts")
	assert.Equal(t, int32(0), wl.calls.Load())
}

func TestDashboardRequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, &stubWhitelist{resp: "ok"}, nil)
	h := srv.Handler()

	code, _ := do(t, h, http.MethodGet, "/invite/", "")
	require.Equal(t, http.StatusFound, code, "unauthenticated dashboard redirects")
}

func TestPlayersPublic(t *testing.T) {
	wl := &stubWhitelist{players: OnlinePlayers{Online: 1, Max: 10, Names: []string{"msmborders"}}}
	srv, _ := newTestServer(t, wl, nil)
	h := srv.Handler()

	// No auth: the online list is public so the landing page can embed it.
	code, body := do(t, h, http.MethodGet, "/invite/players", "")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "1 / 10")
	assert.Contains(t, body, "msmborders")
}

func TestPlayersUnavailable(t *testing.T) {
	wl := &stubWhitelist{playersErr: errors.New("rcon down")}
	srv, _ := newTestServer(t, wl, nil)
	h := srv.Handler()

	code, body := do(t, h, http.MethodGet, "/invite/players", "")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "unavailable")
}
