package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMojangResolveValid(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/users/profiles/minecraft/Palu_Macil", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"name":"Palu_Macil","id":"892ab8259c2d46cb94d5f6ce6babfd00"}`))
	}))
	t.Cleanup(srv.Close)

	m := MojangResolver{BaseURL: srv.URL, Client: srv.Client()}
	p, err := m.Resolve(context.Background(), "Palu_Macil")
	require.NoError(t, err)
	assert.Equal(t, "Palu_Macil", p.Name, "canonical name from Mojang is used")
	assert.Equal(t, "892ab825-9c2d-46cb-94d5-f6ce6babfd00", p.UUID, "undashed id is dashed")
}

func TestMojangResolveRejections(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr error
	}{
		{name: "unknown 404", status: http.StatusNotFound, wantErr: ErrUnknownPlayer},
		{name: "unknown 204", status: http.StatusNoContent, wantErr: ErrUnknownPlayer},
		{name: "empty 200 body", status: http.StatusOK, body: `{}`, wantErr: ErrUnknownPlayer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				if tt.body != "" {
					w.Write([]byte(tt.body))
				}
			}))
			t.Cleanup(srv.Close)

			m := MojangResolver{BaseURL: srv.URL, Client: srv.Client()}
			_, err := m.Resolve(context.Background(), "SomeName")
			assert.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func TestMojangResolveTransient(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	m := MojangResolver{BaseURL: srv.URL, Client: srv.Client()}
	_, err := m.Resolve(context.Background(), "SomeName")
	require.Error(t, err)
	// A server-side failure is not a verdict on the name.
	assert.NotErrorIs(t, err, ErrUnknownPlayer)
	assert.NotErrorIs(t, err, ErrInvalidUsername)
}

func TestMojangInvalidUsernames(t *testing.T) {
	t.Parallel()
	// No HTTP server: invalid names must be rejected before any request.
	m := MojangResolver{BaseURL: "http://127.0.0.1:0", Client: &http.Client{}}
	for _, name := range []string{"", "ab", "waytoolongusername", "bad name", "inject/../x", "emoji😀"} {
		_, err := m.Resolve(context.Background(), name)
		assert.ErrorIs(t, err, ErrInvalidUsername, "name %q", name)
	}
}

func TestDashUUID(t *testing.T) {
	t.Parallel()
	got, err := dashUUID("892ab8259c2d46cb94d5f6ce6babfd00")
	require.NoError(t, err)
	assert.Equal(t, "892ab825-9c2d-46cb-94d5-f6ce6babfd00", got)

	for _, bad := range []string{"", "tooshort", "892ab8259c2d46cb94d5f6ce6babfd0", "892ab8259c2d46cb94d5f6ce6babfd00x", "zzzab8259c2d46cb94d5f6ce6babfd00"} {
		_, err := dashUUID(bad)
		assert.Error(t, err, "input %q", bad)
	}
}

func FuzzUsernameRE(f *testing.F) {
	for _, s := range []string{"Palu_Macil", "abc", "ABC123_", "bad name", "", "😀"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if usernameRE.MatchString(s) {
			// Anything the regex accepts must be a safe URL path segment.
			assert.Len(t, s, len(s)) // trivially true; keeps s referenced
			for _, r := range s {
				ok := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_'
				assert.True(t, ok, "accepted char %q", r)
			}
			assert.GreaterOrEqual(t, len(s), 3)
			assert.LessOrEqual(t, len(s), 16)
		}
	})
}
