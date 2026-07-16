-- mc-invite schema. Applied idempotently at startup (CREATE ... IF NOT EXISTS),
-- so this doubles as the migration: adding a column means adding an ALTER guard
-- here, never editing existing statements in place.

create table if not exists invites (
    id             bigint generated always as identity primary key,
    token_hash     bytea       not null unique,   -- sha256 of the raw token; the raw token is never stored
    created_by     text        not null,          -- OIDC subject of the inviter who minted it
    created_at     timestamptz not null default now(),
    expires_at     timestamptz not null,
    used_at        timestamptz,                    -- null until redeemed; single-use is enforced on this
    minecraft_name text,                           -- canonical Mojang name, set at redemption
    minecraft_uuid uuid                            -- resolved Mojang UUID, set at redemption
);

create index if not exists invites_created_by_idx on invites (created_by);
create index if not exists invites_created_at_idx on invites (created_at desc);

create table if not exists audit_log (
    id     bigint      generated always as identity primary key,
    at     timestamptz not null default now(),
    actor  text        not null,   -- OIDC subject, or 'invitee' for a redemption
    action text        not null,   -- invite_created, invite_redeemed, ...
    detail jsonb       not null
);

create index if not exists audit_log_at_idx on audit_log (at desc);

-- v0.4 additions (idempotent): human-readable names alongside the OIDC subject,
-- and invite cancellation (soft revoke, kept for the audit trail).
alter table invites    add column if not exists created_by_name  text;
alter table invites    add column if not exists canceled_at      timestamptz;
alter table invites    add column if not exists canceled_by      text;
alter table invites    add column if not exists canceled_by_name text;
alter table audit_log  add column if not exists actor_name       text;
