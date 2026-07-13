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

## Releasing a change

Tag this repo, then bump the `?ref=` pin in homelab
`workloads/mc/kustomization.yaml`. ArgoCD does the rest. Pack upgrades
follow the README's "Upgrading the modpack" runbook, snapshot first.

## Phase status

- Phase 1 (game server): shipped 2026-07.
- Phase 2 (landing page + BlueMap on `mc.danwolf.net`): designed in the
  README, not built. DNS already exists (proxied CNAME to the tunnel).
- Phase 3 (invite app): designed in the README, **gated on Authentik**,
  which does not exist in the cluster yet. Do not scaffold it early.
