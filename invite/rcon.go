package main

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/gorcon/rcon"
)

// RCONClient issues commands to the Minecraft server's RCON port. It reconnects
// per command (RCON connections are cheap and the server may have restarted
// between calls) and retries politely, because the invite app can outlive a
// server bounce and RCON is unavailable during the ~20 minute first boot.
type RCONClient struct {
	Addr     string
	Password string

	// Retries is the number of extra attempts after the first for the grant
	// path; DialTimeout and Deadline bound each attempt. Zero values fall back
	// to sane defaults. The player-list path uses no retries so a poll fails
	// fast rather than blocking the page.
	Retries     int
	DialTimeout time.Duration
	Deadline    time.Duration
}

// OnlinePlayers is the parsed result of the `list` command.
type OnlinePlayers struct {
	Online int
	Max    int
	Names  []string
}

// WhitelistAdd runs `whitelist add <name>` and returns the server's reply. name
// must already be validated and canonicalized (Mojang name); this method does
// no escaping beyond trusting that contract, so never pass unvalidated input.
func (c RCONClient) WhitelistAdd(ctx context.Context, name string) (string, error) {
	retries := c.Retries
	if retries <= 0 {
		retries = 4
	}
	return c.execute(ctx, "whitelist add "+name, retries)
}

// ListPlayers runs `list` and parses who is online. It does not retry: this is
// polled for a status widget, so a momentary RCON hiccup should surface quickly
// as "unavailable" rather than block.
func (c RCONClient) ListPlayers(ctx context.Context) (OnlinePlayers, error) {
	resp, err := c.execute(ctx, "list", 0)
	if err != nil {
		return OnlinePlayers{}, err
	}
	return parsePlayerList(resp)
}

func (c RCONClient) execute(ctx context.Context, cmd string, retries int) (string, error) {
	dialTimeout := cmp.Or(c.DialTimeout, 5*time.Second)
	deadline := cmp.Or(c.Deadline, 5*time.Second)

	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			// Exponential backoff capped at 8s, interruptible by the context.
			wait := min(time.Duration(1<<uint(attempt-1))*time.Second, 8*time.Second)
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return "", fmt.Errorf("rcon %q: %w", cmd, ctx.Err())
			case <-t.C:
			}
		}

		conn, err := rcon.Dial(c.Addr, c.Password,
			rcon.SetDialTimeout(dialTimeout),
			rcon.SetDeadline(deadline),
		)
		if err != nil {
			lastErr = err
			continue
		}
		resp, err := conn.Execute(cmd)
		conn.Close()
		if err != nil {
			lastErr = err
			continue
		}
		return resp, nil
	}
	return "", fmt.Errorf("rcon %q unavailable after %d attempts: %w", cmd, retries+1, lastErr)
}

// colorCodes matches Minecraft section-sign formatting and ANSI escapes, which
// some server builds include in the `list` reply.
var colorCodes = regexp.MustCompile("§.|\x1b\\[[0-9;]*m")

// listRE matches the vanilla `list` response, e.g.
// "There are 1 of a max of 10 players online: msmborders".
var listRE = regexp.MustCompile(`(?i)there are (\d+) of a max of (\d+) players online:?\s*(.*)`)

func parsePlayerList(resp string) (OnlinePlayers, error) {
	clean := strings.TrimSpace(colorCodes.ReplaceAllString(resp, ""))
	m := listRE.FindStringSubmatch(clean)
	if m == nil {
		return OnlinePlayers{}, fmt.Errorf("unexpected list response: %q", clean)
	}
	var op OnlinePlayers
	fmt.Sscan(m[1], &op.Online)
	fmt.Sscan(m[2], &op.Max)
	for _, name := range strings.Split(m[3], ",") {
		if n := strings.TrimSpace(name); n != "" {
			op.Names = append(op.Names, n)
		}
	}
	return op, nil
}
