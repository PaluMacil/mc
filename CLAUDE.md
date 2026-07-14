# CLAUDE.md

Guidance for Claude Code working in this repo.

## What this is

Kustomize base and operator docs for an All the Mods 10 (NeoForge,
MC 1.21.1) server in the `mc` namespace of the homelab K3s cluster.
This repo is **public**. The README's "How traffic flows" section is
the authoritative picture; read it before changing anything.

Sibling repos, usually cloned alongside this one:

- `homelab/`: GitOps source of truth. Its `workloads/mc/` overlay pins
  `deploy/base?ref=<tag>` from this repo and adds the jade nodeSelector.
  ArgoCD reconciles; changes go through git.
- `tin/`: provisioning for the VPS that fronts the game port. Strict
  plan-first workflow; read its CLAUDE.md before touching it.

## Hard rules

- **No secret material in this repo, ever.** Not in examples, not in
  comments, not as realistic-looking placeholders. The two Secrets
  (`mc-secrets`, `mc-r2`) are materialized by External Secrets Operator
  from OpenBao (`kv/mc/*`); the `ExternalSecret` manifests live in the
  homelab repo, not here. `deploy/README.md` documents the field names
  mc expects and the break-glass imperative fallback, with obvious
  placeholders.
- **No em-dashes in prose** (READMEs, comments, commit messages). Use
  commas, semicolons, or parentheses. Em-dashes are fine inside code.
- **Verify itzg semantics against the docs**
  (<https://docker-minecraft-server.readthedocs.io>), not from memory.
  Env var interactions (AUTO_CURSEFORGE + Modrinth especially) have
  sharp edges, and a wrong guess costs a 20 minute boot cycle.
- Go first for any code (Phase 3), bash for provisioning. Concise
  commits.

## Invariants in the manifests (do not "fix" these)

- `PREVENT_PROXY_CONNECTIONS=FALSE`: all players arrive from tin's IP;
  the vanilla default locks everyone out.
- No CPU limit on the server container: CFS throttling tanks the
  single-threaded tick loop.
- `MAX_TICK_TIME=-1`: the vanilla watchdog kills ATM10 during heavy
  chunk generation otherwise.
- `EXISTING_WHITELIST_FILE=SKIP` / `EXISTING_OPS_FILE=SKIP`: seed once,
  then runtime RCON changes own the files.
- The pack is the official ATM10 **server pack** zip, staged from the
  private R2 bucket `mc-mods` by the `fetch-pack` initContainer and
  applied via `GENERIC_PACK`. The pin is the object name; `PACK_OBJECT`,
  `GENERIC_PACK`, and `NEOFORGE_VERSION` must always move together.
  There is deliberately no AUTO_CURSEFORGE machinery (README: "Why the
  server pack instead of AUTO_CURSEFORGE"); do not reintroduce it.
- `/data/.generic_pack.sum` and `/data/manifest.txt` are load-bearing
  itzg state (skip-reapply and stale-file cleanup). Never delete them,
  and never set `SKIP_GENERIC_PACK_UPDATE_CHECK` (silently blocks
  future pack upgrades) or `REMOVE_OLD_MODS` (forces a full 1.1GB
  reapply every boot).
- `MODRINTH_PROJECTS` entries are pinned by version ID;
  `MODRINTH_DOWNLOAD_DEPENDENCIES=none` (dependency resolution fights
  the pack's own pinned mods, itzg issue #3849).
- World data on `local-path` (jade NVMe), backups on `longhorn`. Never
  swap those; replicated sync writes hurt the tick loop, and
  un-replicated backups defeat their purpose.
- `externalTrafficPolicy: Local` on `mc-game`, NodePort 30565: tin's
  nginx points at jade's tailnet IP specifically.
- BlueMap (Phase 2): `SYNC_SKIP_NEWER_IN_DESTINATION=false` is
  load-bearing; it is what re-applies `deploy/base/bluemap/core.conf` from
  the `/config` overlay every boot. Do not set BlueMap's webserver `ip`
  to localhost (breaks the `mc-map` Service). `/data/bluemap` (render
  output, re-derivable) is excluded from backups; `/data/config/bluemap`
  (config) is not, so keep them distinct in `EXCLUDES`.
- `mc-rcon` (Phase 3) exposes RCON cluster-internal for `mc-invite` only;
  it is still never reachable from outside the cluster. The password stays
  the shared `mc-secrets/rcon-password`.

## Releasing a change

Tag this repo, then bump the `?ref=` pin in homelab
`workloads/mc/kustomization.yaml`. ArgoCD does the rest. Pack upgrades
follow the README's "Upgrading the modpack" runbook, snapshot first.

CI builds `ghcr.io/palumacil/mc-web` and `mc-invite` at the git tag, and
`deploy/base` pins those image tags (the homelab overlay has no `images:`
block). So when a change touches `web/` or `invite/`, bump the image tags
in `deploy/base` to the new version in the same commit you tag; otherwise
ArgoCD keeps deploying the old image. Tag first so the image exists before
the `?ref=` bump lands.

## Phase status

- Phase 1 (game server): shipped 2026-07.
- Phase 2 (landing page + BlueMap on `mc.danwolf.net`): built. `mc-web`
  (`web/`) + BlueMap wiring + `mc-map` Service + Ingress with a `/map`
  StripPrefix middleware, all in `deploy/base`. Not yet cut over (see
  `DEPLOY-PHASES-2-3.md`): DNS check, image build, pin bump.
- Phase 3 (invite app): built. `mc-invite` (`invite/`, Go + templ +
  HTMX, OIDC + Postgres + RCON) + `mc-rcon` Service + Deployment, in
  `deploy/base`. Authentik and OpenBao are in place; cutover (Authentik
  app + groups, Postgres database, secrets) is in `DEPLOY-PHASES-2-3.md`.

## The two web apps

- `web/` (`mc-web`) is stdlib-only by design; keep it dependency-free.
  The pack version is the `-pack-version` flag and the screenshot is
  `web/assets/atm10-7-1.png`; both move with the server (upgrade runbook).
- `invite/` (`mc-invite`) is Go + templ + HTMX. Commit the generated
  `*_templ.go` files (CI checks they are current). Run
  `templ generate` (v0.3.x) after editing `.templ`. Redemption is
  security-sensitive: single-use is enforced by `SELECT ... FOR UPDATE`
  in the same transaction as the RCON grant; do not loosen that. It is a
  single replica (in-memory sessions); do not scale without a shared
  session store. Integration tests need Postgres
  (`INVITE_TEST_DATABASE_URL`); CI provides one.
