# mc

All the Mods 10 (NeoForge, Minecraft 1.21.1) server for a small invited
group, running on the [homelab K3s cluster](https://github.com/PaluMacil/homelab).
This repo holds the Kustomize base and the operator documentation. The
homelab repo references `deploy/base` at a pinned tag and ArgoCD
reconciles it; nothing is applied from here directly.

## For players

Run the All the Mods 10 client pack at **exactly the version the server
runs** (currently 7.1), then connect to **`mc.danwolf.net`**. The
launcher resolves the rest through DNS. A client on any other pack
version fails the connection handshake with an unhelpful mod-list
error; the fix is always "match the server's version", never a setting.

If a launcher misbehaves and refuses to connect, use the explicit
fallback address: **`game.danwolf.net:25999`**.

The server is whitelist-only. Ask Dan to be added (or open an invite link
if someone sent you one).

Open **`https://mc.danwolf.net`** in a browser for the full setup guide
(written for parents too), and **`https://mc.danwolf.net/map`** for the
live world map.

## How traffic flows

Minecraft Java is raw TCP. The cluster's Cloudflare Tunnel is HTTP-only
and the cluster sits behind CGNAT, so game traffic detours through
`tin`, a small VPS with a public IP:

```
player (ATM10 client)
  -> DNS: SRV _minecraft._tcp.mc.danwolf.net -> game.danwolf.net:25999
  -> A: game.danwolf.net -> 108.165.213.64 (tin, DNS-only / grey cloud)
  -> tin: nginx stream, public :25999
  -> tailnet -> jade (100.71.141.66) NodePort 30565
  -> Service mc-game (externalTrafficPolicy: Local)
  -> StatefulSet mc, pinned to jade
```

Web traffic for the same hostname (Phase 2: landing page and BlueMap) is
an ordinary cluster workload behind the Cloudflare Tunnel and Traefik,
completely separate from the game path.

### DNS records (Cloudflare, zone danwolf.net)

| Record | Type | Value | Proxy status |
| --- | --- | --- | --- |
| `_minecraft._tcp.mc` | SRV | priority 0, weight 5, port 25999, target `game.danwolf.net` | n/a |
| `game` | A | `108.165.213.64` | **DNS only (grey). Must stay grey; a proxied record cannot carry raw TCP.** |
| `mc` | CNAME | `<tunnel-UUID>.cfargotunnel.com` | Proxied (HTTP only, Phase 2 web surface) |

### Two consequences to internalize

1. **`prevent-proxy-connections` must stay `false`.** Every player
   arrives from tin's IP, which does not match what Mojang's session
   server saw during login. With the vanilla default (`true`), nobody
   can log in.
2. **Source IPs collapse.** Every player looks like tin. NeoForge has no
   PROXY-protocol support and a modded pack cannot sit behind
   Velocity/Paper, so this is not fixable. IP bans are useless; rate
   limiting happens on tin. Do not attempt a workaround.

## Repo layout

```
deploy/
  README.md      how mc-secrets/mc-r2/mc-invite secrets come from OpenBao
                 via ESO, plus the field contract and first-sync notes
  base/          Kustomize base: StatefulSet (server + backup sidecar),
                 headless + mc-game + mc-map + mc-rcon Services, the
                 mc-web and mc-invite Deployments, Ingress + /map
                 StripPrefix middleware, and the BlueMap core.conf
web/             mc-web: the static landing page (Go + embedded assets),
                 published as ghcr.io/palumacil/mc-web
invite/          mc-invite: the invite app (Go + templ + HTMX, OIDC,
                 Postgres, RCON), published as ghcr.io/palumacil/mc-invite
.github/         CI that tests both apps and, on a version tag, builds
                 and pushes their images
DEPLOY-PHASES-2-3.md   one-time cutover runbook for the web surface and
                 invite app (homelab manifests + imperative steps)
```

Releases are git tags here. The homelab repo pins
`deploy/base?ref=<tag>` in `workloads/mc/kustomization.yaml`; bumping
that pin is what makes any change (including pack upgrades) a deliberate
commit there.

## Operations

All commands assume a kubeconfig for the cluster.

### Logs and console

```sh
kubectl -n mc logs mc-0 -c mc -f            # server log
kubectl -n mc logs mc-0 -c backup -f        # backup sidecar log
kubectl -n mc exec -it mc-0 -c mc -- rcon-cli   # interactive console
```

### Whitelist and ops

```sh
kubectl -n mc exec mc-0 -c mc -- rcon-cli whitelist add <name>
kubectl -n mc exec mc-0 -c mc -- rcon-cli whitelist remove <name>
kubectl -n mc exec mc-0 -c mc -- rcon-cli whitelist list
```

Runtime additions persist across restarts: the manifests seed
`whitelist.json` and `ops.json` on first boot only
(`EXISTING_WHITELIST_FILE=SKIP`) and never touch them again.

### Upgrading the modpack

The pack is the official ATM10 **server pack** (`ServerFiles-<ver>.zip`),
stored in the private R2 bucket `mc-mods` and staged onto the data
volume by the `fetch-pack` initContainer. The staged zip is the version
pin: the initContainer skips the fetch when the object is already
present (keyed on object name), and the itzg image records a sha1 of
the applied pack in `/data/.generic_pack.sum`, so a normal restart
neither downloads nor re-extracts anything. Changing the object name is
the only thing that triggers a fetch and a reapply. An unpinned pack
that silently upgrades across a restart can corrupt or regenerate
chunks; this design makes that impossible.

**A pack bump is a coordinated event, not a unilateral one.** Every
player's client must move to the same version at the same time, because
a version mismatch fails the connection handshake. Announce the new
version and a switchover time before touching the pin.

1. Download the new `ServerFiles-<ver>.zip` from the
   [ATM10 files page](https://www.curseforge.com/minecraft/modpacks/all-the-mods-10/files)
   with a browser and upload it to the `mc-mods` R2 bucket (Cloudflare
   dashboard or `wrangler r2 object put`). Verify the sha1 after
   upload.
2. Find the new pack's NeoForge version (the `modlist.json` under
   `config/crash_assistant/` in
   [AllTheMods/ATM-10](https://github.com/AllTheMods/ATM-10) names it,
   as does the pack changelog).
3. Take a snapshot first:
   `kubectl -n mc exec mc-0 -c backup -- backup now`
4. In `deploy/base/statefulset.yaml`, update the trio that must move
   together: `PACK_OBJECT` (initContainer), `GENERIC_PACK` (mc
   container), and `NEOFORGE_VERSION`. Then update the landing page guide
   so players install the matching version: bump the `-pack-version`
   default in `web/main.go` (or the `args` in
   `deploy/base/web-deployment.yaml`) and replace
   `web/assets/atm10-7-1.png` with a screenshot of the new version on the
   CurseForge Versions tab. If the bump also crosses a Minecraft version,
   confirm the pinned BlueMap build in `MODRINTH_PROJECTS` still supports
   it (5.7 is the last build that supports 1.21.1). Commit, tag a new
   version, and bump the `mc-web`/`mc-invite` image tags in `deploy/base`
   to match (CI builds those images at the git tag).
5. Bump the `?ref=` pin in homelab `workloads/mc/kustomization.yaml`,
   commit, push. ArgoCD restarts the pod: the initContainer fetches the
   new object, and the image removes every file the old pack installed
   (tracked in `/data/manifest.txt`) before overlaying the new one.
   `world/` and the Modrinth-managed mods are never touched.
6. Watch `kubectl -n mc logs mc-0 -c mc -f` through startup, then have a
   player (on the new client version) verify they can connect.
7. Rollback: restore the pre-upgrade snapshot (below) **and** revert the
   pin. A world touched by newer mod versions is not safe to load under
   older ones, which is exactly why step 3 is not optional.
8. After a verified upgrade, delete the old zip from `/data/packs/`.

Hand edits to pack-shipped files (anything under `config/` that came
from the zip) are deleted and replaced on every pack upgrade. Server
behavior that must survive upgrades belongs in env vars in the
StatefulSet, which the image reapplies over `server.properties` every
boot.

### Why the server pack instead of AUTO_CURSEFORGE

The usual itzg flow (`TYPE=AUTO_CURSEFORGE` + `CF_SLUG` + `CF_FILE_ID`)
was abandoned on day one after hitting three independent problems: the
freshly issued CurseForge API key was rejected by `/v1/mods/search`
alone (a known, recurring CurseForge-side key defect,
itzg/docker-minecraft-server#3591), one pack mod (`cc-tweaked`)
disallows automated CurseForge downloads entirely, and the client pack
carries client-only mods (colorwheel, sodium, iris and friends) that
crash a dedicated server unless slug-based excludes work, which the
broken key prevented. The official server pack sidesteps all three: it
is built by the pack authors with distribution permission, contains
exactly the server-side mod list, and needs no CurseForge API at
runtime. The CurseForge API key was removed from `mc-secrets` once the
server-pack flow shipped.

### Backups

The `backup` sidecar (itzg/mc-backup) snapshots `/data` hourly into a
restic repository on the Longhorn `backups` PVC (replicated across all
three nodes, so backups survive losing jade). Around every snapshot it
runs `save-off`, `save-all flush`, `sync`, then `save-on`, so snapshots
are world-consistent. When nobody is online, backups pause after one
final snapshot. Retention: 24 hourly, 14 daily, 8 weekly.

```sh
# list snapshots
kubectl -n mc exec mc-0 -c backup -- restic snapshots

# force a snapshot now (used before upgrades)
kubectl -n mc exec mc-0 -c backup -- backup now
```

### Restore runbook

Two flavors: inspecting a snapshot without touching the live world, and
rolling the world back (griefing, corruption, botched upgrade).

**Inspect into a scratch path** (safe anytime; the sidecar mounts
`/data` read-only, so restores from inside it cannot hurt the world):

```sh
kubectl -n mc exec mc-0 -c backup -- restic snapshots
kubectl -n mc exec mc-0 -c backup -- \
  restic restore <snapshot-id> --target /backups/restore-scratch
kubectl -n mc exec mc-0 -c backup -- ls /backups/restore-scratch/data
# when done:
kubectl -n mc exec mc-0 -c backup -- rm -rf /backups/restore-scratch
```

**Full world rollback:**

1. Pause ArgoCD automation so it does not fight the scale-down:
   ```sh
   kubectl -n argocd patch application mc --type merge \
     -p '{"spec":{"syncPolicy":{"automated":null}}}'
   ```
2. Warn players, then stop the server:
   ```sh
   kubectl -n mc exec mc-0 -c mc -- rcon-cli say Rolling back the world in 60 seconds
   sleep 60
   kubectl -n mc scale statefulset mc --replicas=0
   kubectl -n mc wait pod mc-0 --for=delete --timeout=360s
   ```
3. Run a restore pod that mounts both PVCs (the PV affinity pins it to
   jade automatically):
   ```sh
   kubectl -n mc apply -f - <<'EOF'
   apiVersion: v1
   kind: Pod
   metadata:
     name: mc-restore
   spec:
     restartPolicy: Never
     containers:
       - name: restore
         image: itzg/mc-backup:2026.7.0
         command: [sleep, infinity]
         env:
           - name: RESTIC_REPOSITORY
             value: /backups
           - name: RESTIC_PASSWORD_FILE
             value: /secrets/restic-password
         volumeMounts:
           - { name: data, mountPath: /data }
           - { name: backups, mountPath: /backups }
           - { name: secrets, mountPath: /secrets, readOnly: true }
     volumes:
       - { name: data, persistentVolumeClaim: { claimName: data-mc-0 } }
       - { name: backups, persistentVolumeClaim: { claimName: backups-mc-0 } }
       - { name: secrets, secret: { secretName: mc-secrets } }
   EOF
   ```
4. Restore. Snapshot paths are absolute (`/data/...`), so target `/`:
   ```sh
   kubectl -n mc exec mc-restore -- restic snapshots
   kubectl -n mc exec mc-restore -- sh -c 'rm -rf /data/world'
   kubectl -n mc exec mc-restore -- restic restore <snapshot-id> --target /
   ```
5. Tear down and bring the server back:
   ```sh
   kubectl -n mc delete pod mc-restore
   kubectl -n mc scale statefulset mc --replicas=1
   kubectl -n argocd patch application mc --type merge \
     -p '{"spec":{"syncPolicy":{"automated":{"prune":true,"selfHeal":true}}}}'
   ```
6. Watch the log for a clean load (no "chunk was not saved" or missing
   registry complaints), then let players back on.

### Weekly maintenance window

`jade` reboots Thursdays at 3:00 AM America/Chicago when a kernel or
libc update requires it. Kubelet's Graceful Node Shutdown stops the pod
with its full termination grace, so the server announces the shutdown
in-game, waits 60 seconds, saves, and comes back a few minutes later.
Details live in the homelab README.

## Phase 2: web surface on mc.danwolf.net

A static landing page and BlueMap (live 3D world map, server-side-only
mod, no client install), served as an ordinary tunnel-and-Traefik
workload. The game path is untouched. Built; the manifests live in
`deploy/base`, the landing page in `web/`. The one-time cutover (DNS
check, image build, pin bump) is in `DEPLOY-PHASES-2-3.md`.

- **BlueMap** is pinned in `MODRINTH_PROJECTS` as `bluemap:8iJcPOHJ`
  (5.7-neoforge, the last BlueMap release that supports 1.21.1; later
  releases dropped it). Server-side only, its integrated webserver binds
  `0.0.0.0:8100`, reached cluster-internal by the `mc-map` ClusterIP
  Service. `deploy/base/bluemap/core.conf` is pre-seeded onto
  `/data/config/bluemap` through itzg's `/config` overlay
  (`SYNC_SKIP_NEWER_IN_DESTINATION=false`) so `accept-download: true` and
  `render-thread-count: 1` are set on first boot, no hand-edit and second
  restart. The pod needs outbound HTTPS to Mojang the first time it
  renders (it downloads and caches the client jar). BlueMap's render
  output lives at `/data/bluemap` and is excluded from backups (it is
  re-derived from the world).
- **Landing page** (`mc-web`, `ghcr.io/palumacil/mc-web`): a tiny
  stdlib-only Go binary with the template and the version-select
  screenshot embedded (`web/`). It serves the connect info, a link to the
  live map, the rules, and the parent-facing client setup guide (Java
  Edition with a Microsoft account and the Java profile name that gets
  whitelisted; install CurseForge; install ATM10 at the server's exact
  version from the **Versions** tab, never the default Install button or
  an update prompt; raise allocated memory; then Multiplayer, Add Server,
  `mc.danwolf.net`, fallback `game.danwolf.net:25999`). The version string
  is the `-pack-version` flag (default matches the server) and the
  screenshot is `web/assets/atm10-7-1.png`; both move with the pack, per
  the upgrade runbook above.
- **Map subpath**: BlueMap has no base-path setting and its webapp uses
  relative asset paths, so it is served at `mc.danwolf.net/map` via a
  Traefik `StripPrefix` middleware (plus a redirect of bare `/map` to
  `/map/` so the relative assets resolve). These are the fleet's first
  Traefik `Middleware` resources. `map.danwolf.net` as a separate proxied
  subdomain remains the documented fallback if the subpath ever misbehaves.
- **Ingress** host `mc.danwolf.net`, no `tls:` block (TLS terminates at
  Cloudflare's edge): `/` to `mc-web`, `/map` to `mc-map` (prefix
  stripped), `/portal` to the Phase 3 app. `mc.danwolf.net` is already a
  proxied CNAME to the cluster tunnel.
- **Planned**: a live online-player list on the landing page (from the
  server's status ping or RCON), so you can see who is on without joining.

## Phase 3: member portal (invites and sign-up)

A small web service so trusted friends can whitelist their own kids
without asking Dan, and so people can self-register and be granted access.
Built (`invite/`, image `ghcr.io/palumacil/mc-invite`); Authentik and
OpenBao are in place. The one-time cutover (Authentik app + groups +
enrollment, Postgres database, secrets) is in `DEPLOY-PHASES-2-3.md`.

- **Stack**: Go + templ + HTMX, single replica. Postgres via the shared
  CNPG pooler (`postgres-pooler.postgres.svc.cluster.local:5432`); the
  schema (`invite/migrations/schema.sql`) is applied idempotently on
  startup. OIDC (Authorization Code + PKCE) against Authentik at
  `authlayer.cloud` (application slug and client id `minecraft`, since it
  does more than invites). Served under `mc.danwolf.net/portal`; the app
  is path-prefix aware, so the Ingress does not strip the prefix.
- **Roles** from the OIDC `groups` claim: `admin` (manage inviters, see
  everything and the audit log) and `inviter` (mint links). The group
  names are env-configurable (`INVITE_ADMIN_GROUP` default `mc-admin`,
  `INVITE_INVITER_GROUP` default `mc-inviter`); admin implies inviter. A
  signed-in user with neither role is a guest (self-registered into
  `mc-guest` via the Authentik enrollment flow) and sees a "pending" page
  until an admin promotes them, which is how new users get onboarded.
- **Flow**: an inviter signs in and mints a single-use link with a 7 day
  expiry (`INVITE_TTL`). The invitee (who signs in to nothing) opens the
  link and types their Minecraft Java username. The app resolves it to a
  UUID against Mojang (rejecting unknown names, and distinguishing a real
  "no such player" from a transient Mojang outage), then issues
  `whitelist add <name>` over RCON in the **same database transaction**
  that marks the invite used and writes the audit row.
- **RCON wiring**: the `mc-rcon` ClusterIP Service (25575) reaches the
  server in-cluster only; the password is the existing
  `mc-secrets/rcon-password`. The app reconnects per command and retries
  with backoff, since the server may be mid-restart.
- **Security posture**: redemption is unauthenticated by design, so the
  token is the credential: 128 bits of entropy, stored only as a sha256
  hash, single-use enforced by a `SELECT ... FOR UPDATE` in the same
  transaction as the grant (so concurrent redemptions of one link grant
  exactly once, and a failed grant rolls back and leaves the link
  usable). Redemption is rate-limited per client IP (accepting that IPs
  collapse behind CGNAT households), CSRF is covered by
  `Sec-Fetch-Site` (`http.CrossOriginProtection`), and every grant is
  auditable back to the inviter who minted the link.
- **Secrets**: OIDC client secret via OpenBao + ESO (`kv/mc/mc-invite`);
  DB credentials via the imperative `mc-invite-db-credentials` basic-auth
  Secret carrying a pooler `uri` (the homelab postgres convention); RCON
  password reused from `mc-secrets`. Field contract in `deploy/README.md`.
