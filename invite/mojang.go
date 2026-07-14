package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"
)

// ErrUnknownPlayer means Mojang has no account for the requested username. It is
// a user-facing rejection, not an operational failure.
var ErrUnknownPlayer = errors.New("no Minecraft Java account with that name")

// ErrInvalidUsername means the name cannot be a Minecraft Java username, so we
// reject it without troubling Mojang (and without letting arbitrary text into
// the request path).
var ErrInvalidUsername = errors.New("not a valid Minecraft Java username")

// Java usernames are 3 to 16 characters of ASCII letters, digits, and
// underscore. Anything else cannot exist, so reject before the network call.
var usernameRE = regexp.MustCompile(`^[A-Za-z0-9_]{3,16}$`)

// MojangResolver resolves a Java Edition username to its account. It is a thin
// client so tests can point it at a stub server.
type MojangResolver struct {
	Client  *http.Client
	BaseURL string // defaults to the public Mojang API; overridable in tests
}

// Profile is a resolved Mojang account: the canonical-case name and the dashed
// UUID (the whitelist wants the canonical name, not the user's casing).
type Profile struct {
	Name string
	UUID string
}

// Resolve looks up username against Mojang. It rejects malformed names locally
// and treats any non-200 response as "unknown player": Mojang has, over time,
// answered unknown names with both 404 and 204, so keying off the exact error
// code is unreliable. Only a 200 with a parseable body is a real account.
func (m MojangResolver) Resolve(ctx context.Context, username string) (Profile, error) {
	if !usernameRE.MatchString(username) {
		return Profile{}, ErrInvalidUsername
	}

	base := m.BaseURL
	if base == "" {
		base = "https://api.mojang.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/users/profiles/minecraft/"+username, nil)
	if err != nil {
		return Profile{}, fmt.Errorf("building mojang request: %w", err)
	}

	client := m.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Profile{}, fmt.Errorf("calling mojang: %w", err)
	}
	defer resp.Body.Close()

	// 200 is a real account. 404 and 204 both mean "no such player" (Mojang has
	// used each over time). Anything else (429, 5xx) is a transient failure, not
	// a verdict on the name, so surface it as an error the caller can retry
	// rather than telling the user their name is wrong.
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusNoContent:
		return Profile{}, ErrUnknownPlayer
	default:
		return Profile{}, fmt.Errorf("mojang returned status %d", resp.StatusCode)
	}

	var body struct {
		Name string `json:"name"`
		ID   string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Profile{}, fmt.Errorf("decoding mojang response: %w", err)
	}
	if body.Name == "" || body.ID == "" {
		return Profile{}, ErrUnknownPlayer
	}

	uuid, err := dashUUID(body.ID)
	if err != nil {
		return Profile{}, fmt.Errorf("mojang returned %w", err)
	}
	return Profile{Name: body.Name, UUID: uuid}, nil
}

// dashUUID turns Mojang's 32 hex character undashed id into the canonical
// 8-4-4-4-12 dashed form. We format the wire shape ourselves rather than pull
// in a UUID dependency to parse two fields.
func dashUUID(id string) (string, error) {
	if len(id) != 32 {
		return "", fmt.Errorf("uuid %q: want 32 hex chars, got %d", id, len(id))
	}
	for _, r := range id {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		if !isHex {
			return "", fmt.Errorf("uuid %q: non-hex character", id)
		}
	}
	return id[0:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:32], nil
}
