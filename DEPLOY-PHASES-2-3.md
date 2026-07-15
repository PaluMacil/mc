# Cutover runbook: Phases 2 and 3

Everything buildable lives in this repo (BlueMap wiring, `mc-web`,
`mc-invite`, the new Services, the Ingress, CI). This runbook is the
one-time work that lives **outside** this repo: the homelab manifests,
the Authentik application, the Postgres database, and the secrets. Once
these are done, future changes are just tag-and-bump-the-pin as usual.

Do it in order. The theme throughout: build images and seed secrets
first, bump the homelab `?ref=` pin last.

Repo paths below are in the sibling `homelab` repo unless noted.

## 0. Prerequisites to confirm

- `mc.danwolf.net` is a **proxied** (orange-cloud) CNAME to the cluster
  tunnel in Cloudflare. It should already exist (the game path relies on
  the same name), but the homelab git tree does not contain DNS records,
  so verify in the Cloudflare dashboard. If missing, add
  `mc` -> `9d70f230-1de9-41da-9b79-525ea898bdfa.cfargotunnel.com`, proxy
  ON. cloudflared already forwards everything to Traefik, so no tunnel
  config change is needed.
- Authentik is up at `authlayer.cloud`; OpenBao and ESO are healthy; the
  CNPG `postgres` cluster is healthy.

## 1. Build and publish the images

1. Cut a tag in **this** repo (the image tags in `deploy/base` must match
   the tag you push; they are currently `v0.3.1`):
   ```sh
   git tag v0.3.1 && git push origin v0.3.1
   ```
   CI (`.github/workflows/ci.yml`) tests both apps and pushes
   `ghcr.io/palumacil/mc-web:v0.3.1` and
   `ghcr.io/palumacil/mc-invite:v0.3.1`.
2. **Make both ghcr packages public** the first time (GitHub, Packages,
   each package, Package settings, Change visibility, Public). The base
   references them without a pull secret; public is the simplest path and
   matches the design. If you would rather keep them private, add a
   per-namespace `ghcr-pull-secret` (OpenBao + ESO, `dockerconfigjson`
   type) and an `imagePullSecrets` patch in the overlay instead.

## 2. Phase 2 goes live with the pin bump

Phase 2 (landing page + map) has no secret or database dependencies. When
the `?ref=` pin is bumped (done in the homelab repo), `mc-web` and `mc-map`
come up immediately and the game StatefulSet rolls to add BlueMap. The
single Ingress also routes `/portal` to the Phase 3 app; that app sits in
`CreateContainerConfigError` until its `minecraft` / `minecraft-db-credentials`
Secrets exist. That is harmless (it recovers on its own once the Phase 3
steps below are done) but not tidy, so finish Phase 3's Authentik + secret
steps promptly after cutover.

## 3. Phase 3: Authentik application, groups, and enrollment

All of this is manual in the Authentik admin UI (there is no provider or
application blueprint in git; that is the house convention). The app is
named `minecraft`, not `mc-invite`, because it does more than invites
(guest sign-in, and later a live player list).

1. **Groups** (Directory, Groups): create `mc-admin`, `mc-inviter`, and
   `mc-guest`. Add yourself to `mc-admin`. `mc-inviter` is for trusted
   friends who mint invites. `mc-guest` is where self-registered users
   land (see enrollment below); a user in only `mc-guest` (or no group)
   is signed in but sees a "pending" page until you promote them into
   `mc-inviter` or `mc-admin`. (`mc-admin` / `mc-inviter` are the defaults
   `INVITE_ADMIN_GROUP` / `INVITE_INVITER_GROUP`; change both places if
   you rename them. `mc-guest` is not referenced by name in the app;
   "no elevated role" is what triggers the pending page.)
2. **Enrollment flow** (Flows & Stages): enable a self-service
   registration flow so people can create an account without you (homelab
   default is deny-unknown). Bind it as the application's or brand's
   enrollment flow, and add a stage that puts new users into the
   `mc-guest` group. This is what gives you a way to onboard users: they
   self-register into `mc-guest`, then you promote the ones you trust.
3. **Provider** (Applications, Providers, Create, OAuth2/OpenID):
   - Client type: **Confidential**.
   - Grant types: **Authorization Code** (required). Refresh Token is
     fine to leave enabled but the app does not use it (sessions are
     server-side); it does not request `offline_access`.
   - Authorization flow: the default implicit-consent authorize flow.
   - Signing key: the authentik self-signed certificate (RS256).
   - Redirect URI, **strict**: `https://mc.danwolf.net/portal/auth/callback`
     (this is exactly `INVITE_BASE_URL` + `/auth/callback`; note the
     `/portal` path, not `/invite`).
   - Scopes: `openid`, `profile`, `email`. The app reads groups from the
     `profile` mapping and falls back to the userinfo endpoint, so you do
     not have to enable "Include claims in id_token" (you may if you
     prefer groups in the ID token).
4. **Application** (Applications, Create, with the provider above):
   - Slug: **`minecraft`**. This must match `INVITE_OIDC_ISSUER`
     (`https://authlayer.cloud/application/o/minecraft/`). If you use a
     different slug, update `INVITE_OIDC_ISSUER` in
     `deploy/base/invite-deployment.yaml`.
5. The **client ID** is `minecraft` (committed as `INVITE_OIDC_CLIENT_ID`).
   The generated **client secret** goes into OpenBao next.

## 4. Phase 3: OpenBao secret and database

The homelab **manifests** for Phase 3 (the `minecraft` ExternalSecret, the
CNPG `minecraft` Database and managed role, and the `?ref=` pin) are
already committed in the homelab repo. What remains here is the imperative
work that never lives in git: seeding OpenBao and creating the DB
credential Secrets.

### 4a. OIDC client secret in OpenBao (rename to the broad name)

The client secret belongs at `kv/mc/minecraft` (the `minecraft`
ExternalSecret extracts from there). If you already saved it at
`kv/mc/mc-invite`, copy it over and drop the old path:

```sh
bao kv get -field=oidc-client-secret kv/mc/mc-invite \
  | bao kv put kv/mc/minecraft oidc-client-secret=-
bao kv metadata delete kv/mc/mc-invite   # optional cleanup
```

Or seed it fresh:

```sh
bao kv put kv/mc/minecraft oidc-client-secret='<client-secret-from-authentik>'
```

### 4b. Database credential Secrets (imperative, both namespaces)

Per the homelab postgres README. Use a URL-safe password (hex) so it drops
cleanly into the connection `uri` (base64 can contain `/` or `+`, which
would need percent-encoding):

```sh
PGPASS=$(openssl rand -hex 24)

kubectl -n postgres create secret generic minecraft-db-credentials \
  --type=kubernetes.io/basic-auth \
  --from-literal=username=minecraft \
  --from-literal=password="$PGPASS"

kubectl -n mc create secret generic minecraft-db-credentials \
  --type=kubernetes.io/basic-auth \
  --from-literal=username=minecraft \
  --from-literal=password="$PGPASS" \
  --from-literal=uri="postgresql://minecraft:${PGPASS}@postgres-pooler.postgres.svc.cluster.local:5432/minecraft"

unset PGPASS
```

CNPG reconciles the `minecraft` role's password from the `postgres`-namespace
Secret and creates the `minecraft` database (both from the committed
manifests); the app reads the `mc`-namespace Secret's `uri` key. The app
applies its own schema (`invite/migrations/schema.sql`) idempotently on
startup, so no migration job is needed.

## 5. Bump the pin (already committed in homelab)

The `?ref=` pin in `workloads/mc/kustomization.yaml` is bumped to `v0.3.1`
and pushed alongside the `minecraft` ExternalSecret and CNPG Database/role.
ArgoCD applies the base and overlay together. Order is forgiving: if a
Secret is not yet present the pod waits and starts on its own once ESO
(the `minecraft` OpenBao value) or the imperative `kubectl create`
(`minecraft-db-credentials`) has produced it.

## 6. Verify

```sh
# secrets materialized
kubectl -n mc get externalsecret            # mc-secrets, mc-r2, minecraft: SecretSynced / Ready
kubectl -n mc get secret minecraft-db-credentials

# database and role
kubectl -n postgres get database minecraft  # Ready

# pods
kubectl -n mc get pods                       # mc-0, mc-web, mc-invite all Ready
kubectl -n mc logs deploy/mc-invite          # "mc-invite listening"; no config or OIDC errors
```

Then from a browser:

- `https://mc.danwolf.net/` renders the landing page and setup guide.
- `https://mc.danwolf.net/map` loads the BlueMap (give BlueMap a few
  minutes on first boot to download Mojang assets and render spawn; watch
  `kubectl -n mc logs mc-0 -c mc -f` for BlueMap render progress).
- `https://mc.danwolf.net/portal` redirects you through Authentik and,
  once you are in `mc-inviter` or `mc-admin`, shows the dashboard (a user
  in only `mc-guest` sees the pending page). Mint a link, open it in a
  private window, enter a Java username, and confirm the player lands on
  the whitelist:
  ```sh
  kubectl -n mc exec mc-0 -c mc -- rcon-cli whitelist list
  ```

## 7. Rollback

Revert the `?ref=` pin in `workloads/mc/kustomization.yaml` (ArgoCD rolls
the base back). The homelab-side additions (ExternalSecret, Database,
role, imperative Secrets) are additive and safe to leave in place; the
`Database` uses `databaseReclaimPolicy: retain`, so reverting does not
drop data. The Authentik application and groups are likewise safe to
leave. There is no world-data risk in either phase: neither touches the
StatefulSet volumes or the game path.
