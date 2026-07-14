package main

import (
	"cmp"
	"context"
	"fmt"
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

	// Retries is the number of extra attempts after the first; DialTimeout and
	// Deadline bound each attempt. Zero values fall back to sane defaults.
	Retries     int
	DialTimeout time.Duration
	Deadline    time.Duration
}

// WhitelistAdd runs `whitelist add <name>` and returns the server's reply. name
// must already be validated and canonicalized (Mojang name); this method does
// no escaping beyond trusting that contract, so never pass unvalidated input.
func (c RCONClient) WhitelistAdd(ctx context.Context, name string) (string, error) {
	return c.execute(ctx, "whitelist add "+name)
}

func (c RCONClient) execute(ctx context.Context, cmd string) (string, error) {
	retries := c.Retries
	if retries <= 0 {
		retries = 4
	}
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
