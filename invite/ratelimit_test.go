package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestIPLimiter(t *testing.T) {
	t.Parallel()
	l := newIPLimiter(3, time.Second) // burst 3, refill 1/sec
	now := time.Unix(1_700_000_000, 0)

	for i := range 3 {
		assert.True(t, l.allow("1.2.3.4", now), "burst attempt %d allowed", i)
	}
	assert.False(t, l.allow("1.2.3.4", now), "4th attempt blocked")

	assert.True(t, l.allow("5.6.7.8", now), "a different IP has its own bucket")

	later := now.Add(2 * time.Second)
	assert.True(t, l.allow("1.2.3.4", later), "token refilled after 1s")
	assert.True(t, l.allow("1.2.3.4", later), "and a second after 2s")
	assert.False(t, l.allow("1.2.3.4", later), "but not a third")
}

func TestClientIP(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		xff        string
		remoteAddr string
		want       string
	}{
		{"xff single", "203.0.113.7", "10.42.0.1:5000", "203.0.113.7"},
		{"xff chain uses leftmost", "203.0.113.7, 10.42.0.1", "10.42.0.1:5000", "203.0.113.7"},
		{"no xff falls back to remote", "", "198.51.100.9:41000", "198.51.100.9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r, _ := http.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			assert.Equal(t, tt.want, clientIP(r))
		})
	}
}
