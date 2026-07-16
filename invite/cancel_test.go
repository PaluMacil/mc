package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCancelInvite(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, hash := newToken()
	inv, err := s.CreateInvite(ctx, hash, "sub-alice", "Alice", time.Hour)
	require.NoError(t, err)

	got, err := s.CancelInvite(ctx, inv.ID, "sub-alice", "Alice", false)
	require.NoError(t, err)
	assert.Equal(t, "canceled", got.Status(time.Now()))
	assert.False(t, got.Cancelable())

	// A canceled invite cannot be redeemed, and nothing is granted.
	_, err = s.RedeemInvite(ctx, hash, testProfile, func(context.Context) (string, error) {
		t.Fatal("grant must not run for a canceled invite")
		return "", nil
	})
	assert.ErrorIs(t, err, ErrInviteCanceled)

	audit, err := s.RecentAudit(ctx, 10)
	require.NoError(t, err)
	var canceled int
	for _, a := range audit {
		if a.Action == "invite_canceled" {
			canceled++
		}
	}
	assert.Equal(t, 1, canceled, "cancellation is audited")
}

func TestCancelInviteAuthorization(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, hash := newToken()
	inv, err := s.CreateInvite(ctx, hash, "sub-alice", "Alice", time.Hour)
	require.NoError(t, err)

	// A different, non-admin inviter may not cancel someone else's invite.
	_, err = s.CancelInvite(ctx, inv.ID, "sub-bob", "Bob", false)
	assert.ErrorIs(t, err, ErrForbidden)

	// An admin may cancel anyone's.
	got, err := s.CancelInvite(ctx, inv.ID, "sub-bob", "Bob", true)
	require.NoError(t, err)
	assert.Equal(t, "canceled", got.Status(time.Now()))
	assert.Equal(t, "Bob", got.CanceledByName)
}

func TestCancelUsedInvite(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, hash := newToken()
	inv, err := s.CreateInvite(ctx, hash, "sub-alice", "Alice", time.Hour)
	require.NoError(t, err)
	_, err = s.RedeemInvite(ctx, hash, testProfile, okGrant(""))
	require.NoError(t, err)

	// A redeemed invite has nothing to cancel.
	_, err = s.CancelInvite(ctx, inv.ID, "sub-alice", "Alice", false)
	assert.ErrorIs(t, err, ErrInviteUsed)
}

func TestCreatedByNameStored(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, hash := newToken()
	_, err := s.CreateInvite(ctx, hash, "sub-alice", "Alice Example", time.Hour)
	require.NoError(t, err)

	list, err := s.ListInvites(ctx, "sub-alice", 10)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "Alice Example", list[0].CreatedByName)

	audit, err := s.RecentAudit(ctx, 10)
	require.NoError(t, err)
	require.NotEmpty(t, audit)
	assert.Equal(t, "Alice Example", audit[0].ActorName, "audit captures the actor name")
}
