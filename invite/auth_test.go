package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRolesFromGroups(t *testing.T) {
	t.Parallel()
	const admin, inviter = "mc-admin", "mc-inviter"
	tests := []struct {
		name   string
		groups []string
		want   []string
	}{
		{"admin implies inviter", []string{"mc-admin"}, []string{"admin", "inviter"}},
		{"inviter only", []string{"mc-inviter"}, []string{"inviter"}},
		{"both groups", []string{"mc-admin", "mc-inviter"}, []string{"admin", "inviter"}},
		{"unrelated groups", []string{"homelab-admins", "everyone"}, nil},
		{"no groups", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, rolesFromGroups(tt.groups, admin, inviter))
		})
	}
}

func TestUserRoleHelpers(t *testing.T) {
	t.Parallel()
	u := User{roles: map[string]bool{"admin": true, "inviter": true}}
	assert.True(t, u.IsAdmin())
	assert.True(t, u.IsInviter())
	assert.True(t, u.Has("admin"))

	plain := User{roles: map[string]bool{"inviter": true}}
	assert.False(t, plain.IsAdmin())
	assert.True(t, plain.IsInviter())
}
