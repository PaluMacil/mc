# Deploying

This directory is a Kustomize base. It is not applied from here; the
[homelab repo](https://github.com/PaluMacil/homelab) references
`deploy/base` at a pinned tag from `workloads/mc/kustomization.yaml`,
and ArgoCD reconciles it into the `mc` namespace. The nodeSelector pin
to `jade` also lives in that overlay.

## Required Secrets (imperative, created before first sync)

Two Secrets in the `mc` namespace. Nothing secret is ever committed to
this repo; this is the documented out-of-band step, same pattern as the
other workloads in the homelab README.

```sh
kubectl -n mc create secret generic mc-secrets \
  --from-literal=rcon-password='REPLACE_RANDOM_PASSWORD' \
  --from-literal=restic-password='REPLACE_RANDOM_PASSWORD'

kubectl -n mc create secret generic mc-r2 \
  --from-literal=access-key-id='REPLACE_R2_ACCESS_KEY_ID' \
  --from-literal=secret-access-key='REPLACE_R2_SECRET_ACCESS_KEY'
```

`mc-r2` holds **read-only** Cloudflare R2 credentials for the private
`mc-mods` bucket that stages the server pack zip. The bucket name and
endpoint are plain env in the StatefulSet, not secret material. Never
recreate `mc-secrets` casually: the restic password encrypts every
existing backup. To change a single key, patch it in place, for
example:

```sh
kubectl -n mc patch secret mc-secrets --type=json \
  -p '[{"op":"remove","path":"/data/some-key"}]'
```

- Generate the two passwords with something like `openssl rand -base64 24`.
- `cf-api-key` comes from <https://console.curseforge.com> (a personal
  API key for the CurseForge for Studios API). AUTO_CURSEFORGE cannot
  download the pack without it.
- `rcon-password` is shared by the server and the backup sidecar; both
  read it from the mounted Secret.
- `restic-password` encrypts the backup repository. **Losing it makes
  every backup permanently unreadable.** Store it in the password
  manager, not just in the cluster.

First-sync ordering: ArgoCD creates the namespace and StatefulSet on its
next reconcile after the homelab commit lands. Until the Secret exists,
the pod sits in `ContainerCreating` with a mount error, which is
harmless; create the Secret and the pod starts on its own.

## Rotating the RCON password

```sh
kubectl -n mc delete secret mc-secrets
# recreate with the new value, then restart the pod:
kubectl -n mc rollout restart statefulset mc
```

The restic password must never be rotated casually; restic has no
re-encryption, so a new password means a new repository and a fresh
backup history.
