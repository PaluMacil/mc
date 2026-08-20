---
name: mc-health
description: Diagnose the ATM10 Minecraft server in the mc namespace - lag, disconnects, timeouts, memory pressure, item loss, error-log triage, backup and nightly-restart verification. Use when someone reports the server feeling bad (desync, rubber-banding, timeouts, vanishing items, AE2 weirdness), when checking whether a deploy or restart landed cleanly, or for a routine health pass. Read before running kubectl against mc, because several of these metrics are actively misleading read raw.
---

# mc server health

Run `scripts/mc-health.sh` (add `--node` for cgroup and network-path probes,
which create and clean up a temporary node-debugger pod on jade). It is
strictly read-only. Then interpret with the rules below.

The commands are the easy part. **Most of the numbers here are misleading if
you read them raw**, and each trap below cost real debugging time.

## Interpretation traps, in order of how badly they mislead

### 1. A player-specific symptom proves nothing until you check who else was online

The single most expensive mistake made on this server: concluding "only
Nividica times out, so it is his ISP" when he was the *only player online*
during every timeout cluster. Population of one.

Before attributing anything to one player's connection, count concurrent
players in that window:

```
kubectl -n mc logs mc-0 -c mc | grep -E "joined the game|left the game"
```

Say plainly when the sample cannot support the conclusion.

### 2. `Timed out` and `Disconnected` are completely different events

- `lost connection: Disconnected` is the player quitting. Normal, ignore.
- `lost connection: Timed out` is the server getting no keepalive response
  for 15s. That is a network or stall event and is worth chasing.

Never lump them together. A day of long clean sessions ending in
`Disconnected` is a healthy day.

### 3. Raw ERROR counts are meaningless; only a delta matters

ATM10 emits ~180 ERROR lines *at every boot* from broken pack loot tables
(`LootDataType`), mod init ordering (`Cannot get config value before config
is loaded`), and registry mismatches (`Tried to load invalid item`). These are
pack bugs, not our problem, and they never change.

The script compares against the previous boot's `debug-1.log.gz`. Baseline as
of 2026-08: **81 / 145 / 49**. Identical counts mean nothing is wrong; only a
delta is a finding.

### 4. `memory.events max` is the metric that actually matters

Not RSS, not heap, not `kubectl top`. `memory.events max` counts how many
times the cgroup hit its hard limit and the kernel forced *direct reclaim*,
which stalls whichever thread allocated, frequently the server thread. It
produces multi-second freezes with a perfectly healthy tick average and
**never triggers an OOM kill**, because the page cache always yields. Nothing
appears in kube events. It is invisible unless you look here.

- `max = 0` -> healthy, regardless of how high utilization looks.
- `max` nonzero and climbing -> this is your lag, whatever else looks fine.

For scale: at the old 16Gi limit this read **55,044** over 11 days, with the
cgroup sitting ~7MB under its ceiling.

### 5. A healthy tick average does not rule out stalls

Mean tick is typically ~5-8ms with 99.98% under 50ms *while the server is
intermittently freezing for seconds*. Averages hide it. Check the far tail
(`over 1000 ms`) and correlate with wall-clock events instead.

To rule a server-side stall in or out at a specific timestamp, check whether
background threads kept logging through it. BlueMap's region watcher ticks
every ~10s and is an excellent heartbeat:

```
kubectl -n mc exec mc-0 -c mc -- grep -E "^\[19Aug2026 19:1[78]:" /data/logs/debug.log
```

Continuous BlueMap lines across the moment of a disconnect means the server
never stalled, so the cause is in the network path.

### 6. Do not assume leaks

Modded-Minecraft folklore says restart daily for leaks. Verified 2026-08 on
this server: heap flat sawtooth over 11 days, `G1 Old Generation` collections
**0**, RSS +10MB/day and decelerating. There is no meaningful leak.

The container is ~14.4GB *at boot* because Aikar's flags set `-Xms=-Xmx`, so
`MEMORY=12G` is committed immediately. Restarting reclaims almost nothing.
If someone proposes a restart as a memory fix, check `G1 Old Generation`
first: nonzero would be real heap pressure, zero means look elsewhere.

## Network path

Game traffic is `player -> tin (nginx stream, public) -> tailnet -> jade
NodePort 30565`. `nginx stream` terminates TCP, so there are **two independent
connections**. A player's home router and ISP cannot affect the tin-to-jade
leg, and their ping tests cannot see it.

Two node-level facts, both on jade:

- `tailscale0` is MTU **1280** (WireGuard overhead) while pod/cni is **1450**.
- flannel masquerades the source to `10.42.2.1`, so the pod cannot learn the
  real path MTU; it believes its peer is a local 1450 neighbour.

Without an MSS clamp this black-holes large server-to-client packets, which
looks like: client keeps sending (visible as a discarded inbound netty buffer
at the moment of disconnect) while the server times out on its keepalive.
Symptoms are size-dependent, so small pings stay clean: chests rendering
stale or empty, AE2 counts disagreeing, timeouts when opening big GUIs.

The clamp lives in the **homelab** repo (`scripts/tailscale-mss-clamp.sh`),
along with a pinned WireGuard port (`tailscale-wg-port.sh`). The script warns
if the rule is missing. Expected:

```
-A FORWARD -i tailscale0 -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss 1240
```

To probe PMTU directly from the pod:

```
kubectl -n mc exec mc-0 -c mc -- ping -M do -c 2 -s 1300 <tin-tailnet-ip>
```

**Never "fix" this by raising tailscale0's MTU or lowering flannel's.** Both
are risky and neither is the right lever; clamping MSS is, precisely because
it leaves every MTU alone.

## Known silent item loss

Vanilla 1.21.1 fails to serialize stacks with count >99 (or 0/air):

```
Error saving [122 irons_spellbooks:arcane_essence]
  IllegalStateException: Value must be within range [1;99]: 122
  An Entity type entity.minecraft.item ... It will not persist.
```

The containing block entity or item entity is dropped on save with no
in-game indication. Rare (3 events in ~5 weeks) and upstream, so it is not
the explanation for large-scale loss, but it is real and worth reporting when
a player asks where something went.

## AE2 desync: MEGA cells, not infrastructure

Diagnosed in-game by Nividica 2026-08-19, and it is invisible from our side.

`megacells-4.11.0.jar` has a failsafe: when a cell's storage used/free
calculation cannot finish **within a single tick**, the cell deactivates and
stops updating. It keeps telling the AE2 grid that items are stored and
withdrawn, to stop them flooding other cells, so the grid and the cell drift
apart. Recovering means pulling and re-inserting the cell, **which voids any
excess items on it**. Warn players before suggesting it.

It writes **nothing** to the server log. Do not go looking for it in
`kubectl logs`; the only evidence is a player watching it happen.

There is no tunable: `megacells-common.toml` exposes exactly one unrelated
option (`spentNuclearWasteAllowed`). The fix is to move large stores off MEGA
cells, which is what Nividica did.

What our side controls is the **tick budget**. The failsafe trips when a
calculation overruns one tick (50ms), so anything stealing server-thread time
raises the odds: cgroup reclaim stalls, backup save freezes, chunk-gen spikes.
Keeping mean tick low and the far tail short is genuine mitigation even though
it cannot fix the mod. Current healthy baseline is ~7-8ms mean.

This also gives the nightly restart a real justification. Nividica: "it gets
worse and worse with time, which is why restarting seemed to have fixed it."
The restart still reclaims almost no *memory* (see the no-leak note above),
but it does reset accumulated AE2 state, so **do not remove the CronJob on the
grounds that memory does not need it**.

## Chest contents vanishing and reappearing

Distinct from the MEGA cell issue and often confused with it. If contents
vanish and later **reappear**, no data was lost: the server had them the whole
time and the client rendered stale. That is a dropped server-to-client packet,
and container-contents packets are large, which points at the MTU/MSS path
issue above rather than at any mod. Real item loss does not come back.

## Restart and deploy checks

- Nightly restart is `mc-nightly-restart`, 04:00 America/New_York, driving
  RCON `stop`. A clean run shows job duration **5m3s**, mc container
  `last exit=0 reason=Completed`, and backup container `restarts=0` (it is
  deliberately untouched).
- `exit=137` would mean SIGKILL/OOM, not our restart. Investigate.
- A release only restarts `mc-0` if it changes the StatefulSet pod spec.
  Image-tag-only bumps do not. See CLAUDE.md.
- Confirm the server is empty before any restart: `rcon-cli list`.

## Reporting

State what the evidence supports and no more. If nobody has connected since
the last restart, say the timeout count is uninformative rather than calling
it a win. If a fix cannot be separated from another change landed at the same
time, say so.
