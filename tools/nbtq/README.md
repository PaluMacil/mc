# nbtq

Pretty-prints Minecraft NBT files (gzipped or raw) as an indented outline,
optionally filtered to matching subtrees.

Nothing in the itzg image can read NBT, and the interesting server state is all
NBT: player inventories under `world/playerdata/<uuid>.dat`, and the mod-level
stores under `world/data/`. This exists so a "my item vanished" report can be
answered with evidence instead of a guess.

Stdlib only, apart from testify in the tests.

## Use

```sh
go build -o nbtq ./tools/nbtq

nbtq world/playerdata/<uuid>.dat
nbtq -grep storage_uuid world/playerdata/<uuid>.dat
kubectl -n mc exec mc-0 -c mc -- cat /data/world/data/sophisticatedbackpacks.dat | nbtq -
```

`-grep` matches case-insensitively against each tag's dotted path and its
value, and prints the whole enclosing container. Grepping an item id therefore
shows that item stack's full component tree, not just the line that matched.

## Item-loss forensics

Player data files are only rewritten while the player is online, so a backup
taken while they were logged off holds their last *logout* state, which can be
days stale. Diffing a player `.dat` against an old snapshot picks up every
legitimate move since, not just the loss. Prefer a timestamped source.

For Sophisticated Backpacks, that source is
`world/data/sophisticatedbackpacks.dat`. It holds `accessLogRecords` (player
name, backpack name, UUID, `accessTime` in epoch ms) alongside
`backpackContents`, both keyed by backpack UUID. Correlating the last access
time against a crash in the server log usually names the backpack outright.

Contents live in that world-level store, not in the item stack; the item only
carries a `sophisticatedcore:storage_uuid` pointer. So a destroyed backpack
item has lost nothing. Give back a backpack with the same `storage_uuid` and
the contents reattach at their current state, no rollback:

```sh
kubectl -n mc exec mc-0 -c mc -- rcon-cli \
  "give <player> sophisticatedbackpacks:netherite_backpack[\
sophisticatedcore:storage_uuid=[I;<a>,<b>,<c>,<d>],\
sophisticatedcore:number_of_inventory_slots=<n>,\
sophisticatedcore:number_of_upgrade_slots=<m>] 1"
```

Match `number_of_inventory_slots` and `number_of_upgrade_slots` to the two
`Size` values on the store entry, or the backpack opens at the wrong size.
Prefer `give` over `item replace`: it fills the first free slot and never
overwrites, and drops at the player's feet if the inventory is full.

UUID int arrays are four big-endian int32s. `[I; a,b,c,d]` is `%08x` of each
masked to 32 bits, concatenated, then hyphenated 8-4-4-4-12.

## Backups

Backups are a restic repo, not tarballs. The `backups-mc-0` PVC mounts at
`/backups` in the `backup` container of pod `mc-0`, with `RESTIC_REPOSITORY`
and `RESTIC_PASSWORD_FILE` already in that container's environment:

```sh
kubectl -n mc exec mc-0 -c backup -- restic snapshots --no-lock
kubectl -n mc exec mc-0 -c backup -- \
  restic restore <id> --target /tmp/rec --no-lock --include <path>
```
