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

// Redemption/cancellation outcomes that map to a specific user-facing message.
// Anything else is an operational error (DB down, RCON down).
var (
	ErrInviteNotFound = errors.New("invite not found")
	ErrInviteUsed     = errors.New("invite already used")
	ErrInviteExpired  = errors.New("invite expired")
	ErrInviteCanceled = errors.New("invite canceled")
	ErrForbidden      = errors.New("not permitted")
)

// Store is the Postgres-backed persistence for invites and the audit log.
type Store struct {
	pool *pgxpool.Pool
}

// Invite is a stored invite plus its derived status. The *Name fields are for
// display; CreatedBy / canceled_by hold the stable OIDC subject.
type Invite struct {
	ID             int64
	CreatedBy      string
	CreatedByName  string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	UsedAt         *time.Time
	CanceledAt     *time.Time
	CanceledByName string
	MinecraftName  string
}

// Status is "used", "canceled", "expired", or "active", evaluated at now.
func (i Invite) Status(now time.Time) string {
	switch {
	case i.UsedAt != nil:
		return "used"
	case i.CanceledAt != nil:
		return "canceled"
	case now.After(i.ExpiresAt):
		return "expired"
	default:
		return "active"
	}
}

// Cancelable reports whether the invite can still be canceled (not used, not
// already canceled).
func (i Invite) Cancelable() bool {
	return i.UsedAt == nil && i.CanceledAt == nil
}

// AuditEntry is one row of the audit log.
type AuditEntry struct {
	At        time.Time
	Actor     string
	ActorName string
	Action    string
	Detail    json.RawMessage
}

// inviteCols is the shared select list so Find/List/Get scan identically.
const inviteCols = `id, created_by, coalesce(created_by_name, ''), created_at,
	expires_at, used_at, canceled_at, coalesce(canceled_by_name, ''),
	coalesce(minecraft_name, '')`

func scanInvite(row pgx.Row) (Invite, error) {
	var inv Invite
	err := row.Scan(&inv.ID, &inv.CreatedBy, &inv.CreatedByName, &inv.CreatedAt,
		&inv.ExpiresAt, &inv.UsedAt, &inv.CanceledAt, &inv.CanceledByName, &inv.MinecraftName)
	return inv, err
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
// transaction, returning the stored invite. createdByName is the inviter's
// display name; createdBy is their stable OIDC subject.
func (s *Store) CreateInvite(ctx context.Context, tokenHash []byte, createdBy, createdByName string, ttl time.Duration) (Invite, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Invite{}, fmt.Errorf("begin create invite: %w", err)
	}
	defer tx.Rollback(ctx)

	inv := Invite{CreatedBy: createdBy, CreatedByName: createdByName}
	err = tx.QueryRow(ctx,
		`insert into invites (token_hash, created_by, created_by_name, expires_at)
		 values ($1, $2, $3, now() + $4::interval)
		 returning id, created_at, expires_at`,
		tokenHash, createdBy, createdByName, ttl.String(),
	).Scan(&inv.ID, &inv.CreatedAt, &inv.ExpiresAt)
	if err != nil {
		return Invite{}, fmt.Errorf("inserting invite: %w", err)
	}

	if err := auditTx(ctx, tx, createdBy, createdByName, "invite_created", map[string]any{
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
// / ErrInviteUsed / ErrInviteExpired / ErrInviteCanceled for the expected
// rejections.
func (s *Store) RedeemInvite(ctx context.Context, tokenHash []byte, p Profile, grant func(context.Context) (string, error)) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin redeem: %w", err)
	}
	defer tx.Rollback(ctx)

	var (
		id         int64
		expiresAt  time.Time
		usedAt     *time.Time
		canceledAt *time.Time
	)
	err = tx.QueryRow(ctx,
		`select id, expires_at, used_at, canceled_at from invites where token_hash = $1 for update`,
		tokenHash,
	).Scan(&id, &expiresAt, &usedAt, &canceledAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", ErrInviteNotFound
	case err != nil:
		return "", fmt.Errorf("locking invite: %w", err)
	case usedAt != nil:
		return "", ErrInviteUsed
	case canceledAt != nil:
		return "", ErrInviteCanceled
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

	// actor is 'invitee' (the child does not log in); actor_name is the player
	// so the audit log reads naturally.
	if err := auditTx(ctx, tx, "invitee", p.Name, "invite_redeemed", map[string]any{
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

// CancelInvite soft-revokes an unused invite so its link can no longer be
// redeemed. An inviter may cancel only their own; an admin may cancel any.
// Returns ErrForbidden (not owner and not admin), ErrInviteNotFound, or
// ErrInviteUsed (already redeemed, nothing to cancel). The updated invite is
// returned on success.
func (s *Store) CancelInvite(ctx context.Context, id int64, canceledBy, canceledByName string, isAdmin bool) (Invite, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Invite{}, fmt.Errorf("begin cancel: %w", err)
	}
	defer tx.Rollback(ctx)

	var createdBy string
	var usedAt, canceledAt *time.Time
	err = tx.QueryRow(ctx,
		`select created_by, used_at, canceled_at from invites where id = $1 for update`, id,
	).Scan(&createdBy, &usedAt, &canceledAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Invite{}, ErrInviteNotFound
	case err != nil:
		return Invite{}, fmt.Errorf("locking invite: %w", err)
	case !isAdmin && createdBy != canceledBy:
		return Invite{}, ErrForbidden
	case usedAt != nil:
		return Invite{}, ErrInviteUsed
	}
	if canceledAt == nil { // idempotent: skip the write if already canceled
		if _, err := tx.Exec(ctx,
			`update invites set canceled_at = now(), canceled_by = $1, canceled_by_name = $2 where id = $3`,
			canceledBy, canceledByName, id,
		); err != nil {
			return Invite{}, fmt.Errorf("canceling invite: %w", err)
		}
		if err := auditTx(ctx, tx, canceledBy, canceledByName, "invite_canceled", map[string]any{
			"invite_id": id,
		}); err != nil {
			return Invite{}, err
		}
	}

	inv, err := scanInvite(tx.QueryRow(ctx, `select `+inviteCols+` from invites where id = $1`, id))
	if err != nil {
		return Invite{}, fmt.Errorf("reading canceled invite: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Invite{}, fmt.Errorf("commit cancel: %w", err)
	}
	return inv, nil
}

// GetInvite reads one invite by id.
func (s *Store) GetInvite(ctx context.Context, id int64) (Invite, error) {
	inv, err := scanInvite(s.pool.QueryRow(ctx, `select `+inviteCols+` from invites where id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Invite{}, ErrInviteNotFound
	}
	if err != nil {
		return Invite{}, fmt.Errorf("getting invite: %w", err)
	}
	return inv, nil
}

// ListInvites returns invites newest first. If createdBy is empty it returns
// everyone's (admin view); otherwise just that inviter's.
func (s *Store) ListInvites(ctx context.Context, createdBy string, limit int) ([]Invite, error) {
	q := `select ` + inviteCols + ` from invites`
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
		inv, err := scanInvite(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning invite: %w", err)
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// FindInvite reads an invite by token hash without locking, for rendering the
// redemption page. It returns ErrInviteNotFound if there is no such invite.
func (s *Store) FindInvite(ctx context.Context, tokenHash []byte) (Invite, error) {
	inv, err := scanInvite(s.pool.QueryRow(ctx, `select `+inviteCols+` from invites where token_hash = $1`, tokenHash))
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
		fmt.Sprintf(`select at, actor, coalesce(actor_name, ''), action, detail
		             from audit_log order by at desc limit %d`, clampLimit(limit)))
	if err != nil {
		return nil, fmt.Errorf("querying audit log: %w", err)
	}
	defer rows.Close()

	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.At, &e.Actor, &e.ActorName, &e.Action, &e.Detail); err != nil {
			return nil, fmt.Errorf("scanning audit entry: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// auditTx writes one audit row inside an existing transaction.
func auditTx(ctx context.Context, tx pgx.Tx, actor, actorName, action string, detail map[string]any) error {
	raw, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("marshaling audit detail: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`insert into audit_log (actor, actor_name, action, detail) values ($1, $2, $3, $4)`,
		actor, actorName, action, json.RawMessage(raw),
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
