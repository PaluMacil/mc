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

1. Cut a tag in **this** repo (the image tags in `deploy/base` are
   currently `v0.3.0`; keep them in step with the tag you push):
   ```sh
   git tag v0.3.0 && git push origin v0.3.0
   ```
   CI (`.github/workflows/ci.yml`) tests both apps and pushes
   `ghcr.io/palumacil/mc-web:v0.3.0` and
   `ghcr.io/palumacil/mc-invite:v0.3.0`.
2. **Make both ghcr packages public** the first time (GitHub, Packages,
   each package, Package settings, Change visibility, Public). The base
   references them without a pull secret; public is the simplest path and
   matches the design. If you would rather keep them private, add a
   per-namespace `ghcr-pull-secret` (OpenBao + ESO, `dockerconfigjson`
   type) and an `imagePullSecrets` patch in the overlay instead.

## 2. Phase 2 only (optional partial cutover)

Phase 2 (landing page + map) has no secret or database dependencies, so
you can ship it before Phase 3 if you want. The single Ingress in the
base also routes `/invite`, so `/invite` will 404 until Phase 3's pods
exist; that is harmless. To do Phase 2 alone, skip to step 5 with the pin
bump; `mc-web` and `mc-map` come up immediately, `mc-invite` sits in
`ContainerCreating`/`CrashLoopBackOff` until its secrets exist (also
harmless, it recovers on its own once they do). Otherwise do Phase 3
first, below, and cut over once.

## 3. Phase 3: Authentik application and groups

All of this is manual in the Authentik admin UI (there is no provider or
application blueprint in git; that is the house convention).

1. **Groups** (Directory, Groups): create `mc-admin` and `mc-inviter`.
   Add yourself to `mc-admin`. Add trusted inviters to `mc-inviter`.
   (These names are the defaults `INVITE_ADMIN_GROUP` /
   `INVITE_INVITER_GROUP`; change both places if you rename them.)
2. **Provider** (Applications, Providers, Create, OAuth2/OpenID):
   - Client type: **Confidential**.
   - Authorization flow: the default implicit-consent authorize flow.
   - Signing key: the authentik self-signed certificate (RS256).
   - Redirect URI, **strict**: `https://mc.danwolf.net/invite/auth/callback`
     (this is exactly `INVITE_BASE_URL` + `/auth/callback`).
   - Scopes: `openid`, `profile`, `email`. The app reads groups from the
     `profile` mapping; it falls back to the userinfo endpoint, so you do
     not have to enable "Include claims in id_token" (you may if you
     prefer groups in the ID token).
3. **Application** (Applications, Create, with the provider above):
   - Slug: **`mc-invite`**. This must match `INVITE_OIDC_ISSUER`
     (`https://authlayer.cloud/application/o/mc-invite/`). If you use a
     different slug, update `INVITE_OIDC_ISSUER` in
     `deploy/base/invite-deployment.yaml`.
4. Note the generated **client ID** and **client secret**. The client ID
   defaults to `mc-invite` in the Deployment env; if Authentik generated a
   different one, set `INVITE_OIDC_CLIENT_ID` accordingly. The client
   secret goes into OpenBao next.

## 4. Phase 3: OpenBao secret + homelab manifests

### 4a. Seed the OIDC client secret in OpenBao

```sh
bao kv put kv/mc/mc-invite oidc-client-secret='<client-secret-from-authentik>'
```

### 4b. Add the ExternalSecret (workloads/mc/externalsecrets.yaml)

Append this block (mirrors the existing `mc-secrets` / `mc-r2` blocks;
`namespace: mc` is supplied by the overlay's kustomization):

```yaml
---
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: mc-invite
spec:
  refreshInterval: 1h
  secretStoreRef:
    kind: ClusterSecretStore
    name: openbao
  target:
    name: mc-invite
    creationPolicy: Owner
  dataFrom:
    - extract:
        key: mc/mc-invite
```

### 4c. Create the database and role (infrastructure/postgres)

Follow the postgres README's "Adding a new app database" recipe.

1. Imperative DB-credential Secrets in **both** namespaces. Use a
   URL-safe password (hex) so it drops cleanly into the connection `uri`
   (the README example uses base64, but base64 can contain `/` or `+`,
   which then need percent-encoding in the URI; hex avoids that):
   ```sh
   PGPASS=$(openssl rand -hex 24)

   kubectl -n postgres create secret generic mc-invite-db-credentials \
     --type=kubernetes.io/basic-auth \
     --from-literal=username=mc_invite \
     --from-literal=password="$PGPASS"

   kubectl -n mc create secret generic mc-invite-db-credentials \
     --type=kubernetes.io/basic-auth \
     --from-literal=username=mc_invite \
     --from-literal=password="$PGPASS" \
     --from-literal=uri="postgresql://mc_invite:${PGPASS}@postgres-pooler.postgres.svc.cluster.local:5432/mc_invite"

   unset PGPASS
   ```
2. Add the managed role in `infrastructure/postgres/cluster.yaml` under
   `spec.managed.roles`:
   ```yaml
       - name: mc_invite
         ensure: present
         login: true
         passwordSecret:
           name: mc-invite-db-credentials
   ```
3. Add `infrastructure/postgres/databases/mc-invite.yaml`:
   ```yaml
   apiVersion: postgresql.cnpg.io/v1
   kind: Database
   metadata:
     name: mc-invite
     namespace: postgres
     annotations:
       argocd.argoproj.io/sync-options: SkipDryRunOnMissingResource=true
   spec:
     cluster:
       name: postgres
     name: mc_invite
     owner: mc_invite
     databaseReclaimPolicy: retain
   ```
   and list it under `resources:` in
   `infrastructure/postgres/kustomization.yaml`.

The app applies its own schema (`invite/migrations/schema.sql`) idempotently
on startup, so no migration job is needed.

## 5. Bump the pin and let ArgoCD reconcile

In `workloads/mc/kustomization.yaml`, bump the pin to the tag from step 1:

```yaml
resources:
  - https://github.com/PaluMacil/mc//deploy/base?ref=v0.3.0
```

Commit and push the homelab changes (externalsecrets, postgres, pin).
ArgoCD applies the base and the overlay together. Order is forgiving: if
a Secret is not yet present the pod waits in `ContainerCreating` and
starts on its own once ESO (or the imperative `kubectl create`) has
produced it.

## 6. Verify

```sh
# secrets materialized
kubectl -n mc get externalsecret            # mc-secrets, mc-r2, mc-invite: SecretSynced / Ready
kubectl -n mc get secret mc-invite-db-credentials

# database and role
kubectl -n postgres get database mc-invite  # Ready

# pods
kubectl -n mc get pods                       # mc-0, mc-web, mc-invite all Ready
kubectl -n mc logs deploy/mc-invite          # "mc-invite listening"; no config or OIDC errors
```

Then from a browser:

- `https://mc.danwolf.net/` renders the landing page and setup guide.
- `https://mc.danwolf.net/map` loads the BlueMap (give BlueMap a few
  minutes on first boot to download Mojang assets and render spawn; watch
  `kubectl -n mc logs mc-0 -c mc -f` for BlueMap render progress).
- `https://mc.danwolf.net/invite` redirects you through Authentik and,
  once you are in `mc-inviter` or `mc-admin`, shows the dashboard. Mint a
  link, open it in a private window, enter a Java username, and confirm
  the player lands on the whitelist:
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
