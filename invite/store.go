package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/schema.sql
var schemaSQL string

// Redemption outcomes that map to a specific user-facing message. Anything else
// is an operational error (DB down, RCON down) and is not one of these.
var (
	ErrInviteNotFound = errors.New("invite not found")
	ErrInviteUsed     = errors.New("invite already used")
	ErrInviteExpired  = errors.New("invite expired")
)

// Store is the Postgres-backed persistence for invites and the audit log.
type Store struct {
	pool *pgxpool.Pool
}

// Invite is a stored invite plus its derived status.
type Invite struct {
	ID            int64
	CreatedBy     string
	CreatedAt     time.Time
	ExpiresAt     time.Time
	UsedAt        *time.Time
	MinecraftName string
}

// Status is "used", "expired", or "active", evaluated at now.
func (i Invite) Status(now time.Time) string {
	switch {
	case i.UsedAt != nil:
		return "used"
	case now.After(i.ExpiresAt):
		return "expired"
	default:
		return "active"
	}
}

// AuditEntry is one row of the audit log.
type AuditEntry struct {
	At     time.Time
	Actor  string
	Action string
	Detail json.RawMessage
}

// NewStore opens a pooled connection and verifies it is reachable.
func NewStore(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("opening postgres pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping reports whether Postgres is reachable, for the readiness probe.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Migrate applies the idempotent schema.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("applying schema: %w", err)
	}
	return nil
}

// CreateInvite records a new invite and an invite_created audit row in one
// transaction, returning the stored invite.
func (s *Store) CreateInvite(ctx context.Context, tokenHash []byte, createdBy string, ttl time.Duration) (Invite, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Invite{}, fmt.Errorf("begin create invite: %w", err)
	}
	defer tx.Rollback(ctx)

	inv := Invite{CreatedBy: createdBy}
	err = tx.QueryRow(ctx,
		`insert into invites (token_hash, created_by, expires_at)
		 values ($1, $2, now() + $3::interval)
		 returning id, created_at, expires_at`,
		tokenHash, createdBy, ttl.String(),
	).Scan(&inv.ID, &inv.CreatedAt, &inv.ExpiresAt)
	if err != nil {
		return Invite{}, fmt.Errorf("inserting invite: %w", err)
	}

	if err := auditTx(ctx, tx, createdBy, "invite_created", map[string]any{
		"invite_id":  inv.ID,
		"expires_at": inv.ExpiresAt,
	}); err != nil {
		return Invite{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Invite{}, fmt.Errorf("commit create invite: %w", err)
	}
	return inv, nil
}

// RedeemInvite consumes an invite for the resolved profile. Single use is
// enforced in the same transaction as the whitelist grant: the invite row is
// locked FOR UPDATE, checked, granted (via the grant callback, which issues the
// RCON whitelist add), then marked used and audited, all atomically. If grant
// fails the transaction rolls back and the invite stays usable, so a transient
// RCON outage does not burn a link. Concurrent redemptions of the same token
// serialize on the row lock, so the second sees used_at set and is rejected.
//
// It returns the grant's response text on success, or one of ErrInviteNotFound
// / ErrInviteUsed / ErrInviteExpired for the expected rejections.
func (s *Store) RedeemInvite(ctx context.Context, tokenHash []byte, p Profile, grant func(context.Context) (string, error)) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin redeem: %w", err)
	}
	defer tx.Rollback(ctx)

	var (
		id        int64
		expiresAt time.Time
		usedAt    *time.Time
	)
	err = tx.QueryRow(ctx,
		`select id, expires_at, used_at from invites where token_hash = $1 for update`,
		tokenHash,
	).Scan(&id, &expiresAt, &usedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", ErrInviteNotFound
	case err != nil:
		return "", fmt.Errorf("locking invite: %w", err)
	case usedAt != nil:
		return "", ErrInviteUsed
	case time.Now().After(expiresAt):
		return "", ErrInviteExpired
	}

	resp, err := grant(ctx)
	if err != nil {
		return "", err // rollback leaves the invite unused for a retry
	}

	if _, err := tx.Exec(ctx,
		`update invites set used_at = now(), minecraft_name = $1, minecraft_uuid = $2 where id = $3`,
		p.Name, p.UUID, id,
	); err != nil {
		return "", fmt.Errorf("marking invite used: %w", err)
	}

	if err := auditTx(ctx, tx, "invitee", "invite_redeemed", map[string]any{
		"invite_id":      id,
		"minecraft_name": p.Name,
		"minecraft_uuid": p.UUID,
		"rcon_response":  resp,
	}); err != nil {
		return "", err
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit redeem: %w", err)
	}
	return resp, nil
}

// ListInvites returns invites newest first. If createdBy is empty it returns
// everyone's (admin view); otherwise just that inviter's.
func (s *Store) ListInvites(ctx context.Context, createdBy string, limit int) ([]Invite, error) {
	q := `select id, created_by, created_at, expires_at, used_at, coalesce(minecraft_name, '')
	      from invites`
	args := []any{}
	if createdBy != "" {
		q += ` where created_by = $1`
		args = append(args, createdBy)
	}
	q += fmt.Sprintf(` order by created_at desc limit %d`, clampLimit(limit))

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("querying invites: %w", err)
	}
	defer rows.Close()

	var out []Invite
	for rows.Next() {
		var inv Invite
		if err := rows.Scan(&inv.ID, &inv.CreatedBy, &inv.CreatedAt, &inv.ExpiresAt, &inv.UsedAt, &inv.MinecraftName); err != nil {
			return nil, fmt.Errorf("scanning invite: %w", err)
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// FindInvite reads an invite by token hash without locking, for rendering the
// redemption page. It returns ErrInviteNotFound if there is no such invite.
func (s *Store) FindInvite(ctx context.Context, tokenHash []byte) (Invite, error) {
	var inv Invite
	err := s.pool.QueryRow(ctx,
		`select id, created_by, created_at, expires_at, used_at, coalesce(minecraft_name, '')
		 from invites where token_hash = $1`,
		tokenHash,
	).Scan(&inv.ID, &inv.CreatedBy, &inv.CreatedAt, &inv.ExpiresAt, &inv.UsedAt, &inv.MinecraftName)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invite{}, ErrInviteNotFound
	}
	if err != nil {
		return Invite{}, fmt.Errorf("finding invite: %w", err)
	}
	return inv, nil
}

// RecentAudit returns the newest audit entries, for the admin view.
func (s *Store) RecentAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.pool.Query(ctx,
		fmt.Sprintf(`select at, actor, action, detail from audit_log order by at desc limit %d`, clampLimit(limit)))
	if err != nil {
		return nil, fmt.Errorf("querying audit log: %w", err)
	}
	defer rows.Close()

	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.At, &e.Actor, &e.Action, &e.Detail); err != nil {
			return nil, fmt.Errorf("scanning audit entry: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// auditTx writes one audit row inside an existing transaction.
func auditTx(ctx context.Context, tx pgx.Tx, actor, action string, detail map[string]any) error {
	raw, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("marshaling audit detail: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`insert into audit_log (actor, action, detail) values ($1, $2, $3)`,
		actor, action, json.RawMessage(raw),
	); err != nil {
		return fmt.Errorf("writing audit row: %w", err)
	}
	return nil
}

// clampLimit keeps list queries bounded even if a caller passes something odd.
func clampLimit(limit int) int {
	if limit <= 0 || limit > 500 {
		return 100
	}
	return limit
}
