# Deploying

This directory is a Kustomize base. It is not applied from here; the
[homelab repo](https://github.com/PaluMacil/homelab) references
`deploy/base` at a pinned tag from `workloads/mc/kustomization.yaml`,
and ArgoCD reconciles it into the `mc` namespace. The nodeSelector pin
to `jade` also lives in that overlay.

## Secrets

Two Secrets in the `mc` namespace, `mc-secrets` and `mc-r2`. Nothing
secret is ever committed to this repo, and the values are no longer
created by hand: they live in the cluster's OpenBao and are materialized
into Kubernetes Secrets by the External Secrets Operator (ESO).

Responsibilities split across two repos:

- **This repo** stays the source of truth for **which fields mc
  expects** (the Secret and key names the StatefulSet mounts). That
  contract is documented below.
- **The homelab repo** owns the wiring: an `ExternalSecret` per Secret
  in its `workloads/mc/` overlay (`externalsecrets.yaml`). ESO reads
  OpenBao through the cluster-wide `openbao` `ClusterSecretStore` and
  writes `mc-secrets` and `mc-r2` into the `mc` namespace on a refresh
  interval (currently 1h). ArgoCD drift suppression for the CRD-defaulted
  fields on those `ExternalSecret`s is handled centrally in the homelab
  repo's `bootstrap/argocd/values.yaml`. Nothing about any of this lives
  in `deploy/base`, and materializing it needs no version-tag bump here.

### Fields mc expects

This repo defines these names; OpenBao holds one kv field per Secret key
(kv-v2, paths `kv/mc/mc-secrets`, `kv/mc/mc-r2`, `kv/mc/minecraft`).

- `mc-secrets/rcon-password`: shared by the server, the backup sidecar,
  and (Phase 3) the `mc-invite` app; all read it from a mounted Secret.
- `mc-secrets/restic-password`: encrypts the backup repository.
  **Losing it makes every backup permanently unreadable.** It lives in
  OpenBao and in the password manager. restic has no re-encryption, so
  it must never be rotated casually: a new password means a new
  repository and a fresh backup history.
- `mc-r2/access-key-id`, `mc-r2/secret-access-key`: **read-only**
  Cloudflare R2 credentials for the private `mc-mods` bucket that stages
  the server pack zip. The bucket name and endpoint are plain env in the
  StatefulSet, not secret material.
- `minecraft/oidc-client-secret` (Phase 3): the confidential OIDC client
  secret Authentik generates for the `minecraft` application. The client
  ID is not secret (it is plain env in the Deployment,
  `INVITE_OIDC_CLIENT_ID=minecraft`); only this secret goes through
  OpenBao + ESO. The Secret and its OpenBao path are named `minecraft`
  (not `mc-invite`) because the app is broader than invites (guest
  sign-in, and later a live player list).

One more Phase 3 Secret does **not** come from OpenBao:
`minecraft-db-credentials` is created imperatively in both the `postgres`
and `mc` namespaces per the homelab postgres README (a
`kubernetes.io/basic-auth` Secret; CNPG reconciles the role password from
the `postgres`-namespace copy, and the app mounts the `mc`-namespace
copy's `uri` key, which points at the pooler). See `DEPLOY-PHASES-2-3.md`
for the exact commands. Do not fold the DB password into OpenBao; follow
the cluster convention.

### Creating or rotating a value

Values live in OpenBao; the normal path is the `bao` CLI from a tailnet
workstation (CLI install, `BAO_ADDR`, and OIDC login are documented in
the homelab repo's `dev-env.md`). `bao kv put` writes a complete new
version of the path, so to change one field without dropping the others
use `bao kv patch`:

```sh
# rotate a single field, leaving the rest of the path intact
bao kv patch kv/mc/mc-secrets rcon-password='...'

# seed or replace an entire secret (every field at once)
bao kv put kv/mc/mc-r2 access-key-id='...' secret-access-key='...'
```

Generate passwords with something like `openssl rand -base64 24`. ESO
re-reads OpenBao on its refresh interval and updates the cluster Secret
(force it sooner by annotating the `ExternalSecret`, per the homelab
ESO README); the mc pod picks the change up on its next restart:

```sh
kubectl -n mc rollout restart statefulset mc
```

The one-time seeding/cutover recipe (copying a live Secret into OpenBao
1:1 so ESO can take ownership) lives in the homelab repo's
`infrastructure/external-secrets/README.md`.

### First-sync ordering

On a fresh sync ArgoCD applies this base and the homelab overlay's
`ExternalSecret`s together. As long as the values are already in
OpenBao, ESO materializes `mc-secrets` and `mc-r2` and the pod starts.
Until the Secret exists (values not yet in OpenBao, or ESO not yet
Ready) the pod sits in `ContainerCreating` with a mount error, which is
harmless; it starts on its own once the Secret appears. Check sync
state with:

```sh
kubectl -n mc get externalsecret   # want STATUS=SecretSynced, READY=True
```

### Disaster-recovery fallback (imperative create)

Only if OpenBao or ESO is unavailable and the server must come up
anyway, the two Secrets can be created imperatively as a stopgap. This
is break-glass, not the normal path: the `ExternalSecret`s use
`creationPolicy: Owner`, so once ESO recovers it reconciles these
Secrets back to the OpenBao values. Seed OpenBao and let ESO take over
as soon as it is healthy.

```sh
kubectl -n mc create secret generic mc-secrets \
  --from-literal=rcon-password='REPLACE_RANDOM_PASSWORD' \
  --from-literal=restic-password='REPLACE_RANDOM_PASSWORD'

kubectl -n mc create secret generic mc-r2 \
  --from-literal=access-key-id='REPLACE_R2_ACCESS_KEY_ID' \
  --from-literal=secret-access-key='REPLACE_R2_SECRET_ACCESS_KEY'
```

For a real recovery, reuse the existing `restic-password` from the
password manager, never a fresh value, or every prior backup becomes
unreadable.
