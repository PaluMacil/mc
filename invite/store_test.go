package main

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testProfile is a fixed, Mojang-verified account used across redemption tests.
var testProfile = Profile{Name: "Palu_Macil", UUID: "892ab825-9c2d-46cb-94d5-f6ce6babfd00"}

// newTestStore connects to the Postgres named by INVITE_TEST_DATABASE_URL,
// applies the schema (twice, to prove idempotency), and truncates the tables so
// each test starts clean. These tests do not run in parallel: they share tables.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("INVITE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set INVITE_TEST_DATABASE_URL to run store integration tests")
	}
	ctx := context.Background()
	s, err := NewStore(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(s.Close)

	require.NoError(t, s.Migrate(ctx))
	require.NoError(t, s.Migrate(ctx), "schema is idempotent")
	_, err = s.pool.Exec(ctx, "truncate invites, audit_log restart identity")
	require.NoError(t, err)
	return s
}

func okGrant(string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return "Added Palu_Macil to the whitelist", nil }
}

func TestRedeemHappyPath(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, hash := newToken()
	inv, err := s.CreateInvite(ctx, hash, "oidc-sub-alice", "Alice", time.Hour)
	require.NoError(t, err)
	require.NotZero(t, inv.ID)

	var granted atomic.Int32
	resp, err := s.RedeemInvite(ctx, hash, testProfile, func(context.Context) (string, error) {
		granted.Add(1)
		return "ok", nil
	})
	require.NoError(t, err)
	assert.Equal(t, "ok", resp)
	assert.Equal(t, int32(1), granted.Load())

	got, err := s.FindInvite(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, "used", got.Status(time.Now()))
	assert.Equal(t, testProfile.Name, got.MinecraftName)

	// A second redemption of the same token is rejected without granting again.
	_, err = s.RedeemInvite(ctx, hash, testProfile, func(context.Context) (string, error) {
		t.Fatal("grant must not run for an already-used invite")
		return "", nil
	})
	assert.ErrorIs(t, err, ErrInviteUsed)

	audit, err := s.RecentAudit(ctx, 10)
	require.NoError(t, err)
	actions := map[string]int{}
	for _, a := range audit {
		actions[a.Action]++
	}
	assert.Equal(t, 1, actions["invite_created"])
	assert.Equal(t, 1, actions["invite_redeemed"])
}

func TestRedeemGrantFailureRollsBack(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, hash := newToken()
	_, err := s.CreateInvite(ctx, hash, "oidc-sub-alice", "Alice", time.Hour)
	require.NoError(t, err)

	_, err = s.RedeemInvite(ctx, hash, testProfile, func(context.Context) (string, error) {
		return "", errors.New("rcon down")
	})
	require.Error(t, err)

	// The invite must remain usable: the whitelist grant never happened.
	got, err := s.FindInvite(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, "active", got.Status(time.Now()), "failed grant leaves the invite unused")

	// And a subsequent successful redemption works.
	resp, err := s.RedeemInvite(ctx, hash, testProfile, okGrant(testProfile.Name))
	require.NoError(t, err)
	assert.NotEmpty(t, resp)
}

func TestRedeemExpired(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, hash := newToken()
	// Negative TTL puts expires_at in the past.
	_, err := s.CreateInvite(ctx, hash, "oidc-sub-alice", "Alice", -time.Hour)
	require.NoError(t, err)

	_, err = s.RedeemInvite(ctx, hash, testProfile, func(context.Context) (string, error) {
		t.Fatal("grant must not run for an expired invite")
		return "", nil
	})
	assert.ErrorIs(t, err, ErrInviteExpired)
}

func TestRedeemNotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, hash := newToken() // never stored
	_, err := s.RedeemInvite(ctx, hash, testProfile, func(context.Context) (string, error) {
		t.Fatal("grant must not run for an unknown token")
		return "", nil
	})
	assert.ErrorIs(t, err, ErrInviteNotFound)
}

// TestRedeemConcurrentSingleUse is the core safety property: two simultaneous
// redemptions of the same link must grant exactly once.
func TestRedeemConcurrentSingleUse(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, hash := newToken()
	_, err := s.CreateInvite(ctx, hash, "oidc-sub-alice", "Alice", time.Hour)
	require.NoError(t, err)

	var grants atomic.Int32
	grant := func(context.Context) (string, error) {
		grants.Add(1)
		time.Sleep(50 * time.Millisecond) // widen the race window
		return "ok", nil
	}

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = s.RedeemInvite(ctx, hash, testProfile, grant)
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(1), grants.Load(), "the whitelist grant runs exactly once")

	var success, used int
	for _, err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrInviteUsed):
			used++
		default:
			t.Errorf("unexpected redemption error: %v", err)
		}
	}
	assert.Equal(t, 1, success, "exactly one redemption succeeds")
	assert.Equal(t, 1, used, "the other is rejected as already used")
}
