#!/usr/bin/env bash
# Read-only health snapshot for the mc game server. Mutates nothing: no
# restarts, no config writes, no RCON commands other than `list`. Safe to run
# with players online.
#
# The RCON password is read from the mc-secrets Secret into a variable and
# never printed, never passed on a visible command line beyond the exec, and
# unset immediately after. Nothing here belongs in the repo; see CLAUDE.md.
#
# Usage:
#   scripts/mc-health.sh            # workload checks only (fast, no node access)
#   scripts/mc-health.sh --node     # adds cgroup + network path probes on jade
#                                   # (creates a temporary node-debugger pod)
set -uo pipefail

NS=mc
POD=mc-0
NODE=jade
WITH_NODE=0
[ "${1:-}" = "--node" ] && WITH_NODE=1

ok()   { printf '  \033[32mOK\033[0m    %s\n' "$*"; }
warn() { printf '  \033[33mWARN\033[0m  %s\n' "$*"; }
bad()  { printf '  \033[31mPROBLEM\033[0m %s\n' "$*"; }
hdr()  { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }

rcon() {
  local pw out
  pw=$(kubectl -n "$NS" get secret mc-secrets -o jsonpath='{.data.rcon-password}' 2>/dev/null | base64 -d)
  [ -z "$pw" ] && return 1
  out=$(kubectl -n "$NS" exec "$POD" -c mc -- rcon-cli --password "$pw" "$@" 2>/dev/null)
  unset pw
  printf '%s' "$out" | tr -d '\r'
}

# ---------------------------------------------------------------- pod state
hdr "Pod / restarts"
kubectl -n "$NS" get pod "$POD" -o wide --no-headers 2>/dev/null || { bad "pod $POD not found"; exit 1; }
kubectl -n "$NS" get pod "$POD" -o json 2>/dev/null > /tmp/mc-pod.$$
python3 - /tmp/mc-pod.$$ <<'PODEOF'
import json,sys
d=json.load(open(sys.argv[1]))
for cs in d["status"]["containerStatuses"]:
    st=list(cs["state"])[0]
    since=cs["state"][st].get("startedAt","?")
    print("  %-8s restarts=%-3d state=%s since=%s" % (cs["name"],cs["restartCount"],st,since))
    t=cs.get("lastState",{}).get("terminated")
    if t:
        # exitCode 0 = clean stop (the nightly RCON restart). 137 = SIGKILL/OOM.
        print("           last exit=%s reason=%s at=%s" % (t.get("exitCode"),t.get("reason"),t.get("finishedAt")))
PODEOF
rm -f /tmp/mc-pod.$$

# ------------------------------------------------------------------ players
hdr "Players"
LIST=$(rcon list)
echo "  ${LIST:-<no RCON response>}"
NOW_N=$(printf '%s' "$LIST" | sed -n 's/.*There are \([0-9][0-9]*\) of a max.*/\1/p')

# ------------------------------------------- sessions, disconnects, lag
hdr "Sessions and disconnects (this boot)"
LOG=$(kubectl -n "$NS" logs "$POD" -c mc 2>/dev/null)
TO=$(printf '%s' "$LOG" | grep -c "lost connection: Timed out")
DC=$(printf '%s' "$LOG" | grep -c "lost connection: Disconnected")
JOIN=$(printf '%s' "$LOG" | grep -c "joined the game")
KEEP=$(printf '%s' "$LOG" | grep -c "Can't keep up")
echo "  joins=$JOIN  clean-quits=$DC  timeouts=$TO  cant-keep-up=$KEEP"
printf '%s' "$LOG" | grep -E "joined the game|lost connection" \
  | sed -E 's/^\[([0-9:]+)[.0-9]*\] \[[^]]*\] \[[^]]*\]: */  \1  /' | tail -12
if [ "$TO" -gt 0 ]; then
  warn "$TO timeout(s). Before blaming a player's ISP, check how many players were"
  echo "         online at each one -- a lone player timing out proves nothing."
else
  [ "$JOIN" -gt 0 ] && ok "no timeouts across $JOIN session(s)" || echo "  (nobody has connected this boot; timeout count is uninformative)"
fi

# ------------------------------------------------------------- tick / heap
hdr "Tick, heap, GC"
kubectl -n "$NS" run mchealth-$RANDOM --rm -i --restart=Never --image=curlimages/curl:latest --quiet \
  -- -s --max-time 20 http://mc-metrics:19565/metrics 2>/dev/null > /tmp/mc-metrics.$$ || true
if [ -s /tmp/mc-metrics.$$ ]; then
python3 - /tmp/mc-metrics.$$ <<'PY'
import re,sys
d={}
for l in open(sys.argv[1]):
    if l.startswith('#'): continue
    m=re.match(r'^(\w+)(\{.*?\})?\s+(\S+)$', l.strip())
    if m: d.setdefault(m.group(1),[]).append((m.group(2) or '', m.group(3)))
def one(n):
    v=d.get(n)
    return float(v[0][1]) if v else None
c,s=one('mc_server_tick_seconds_count'),one('mc_server_tick_seconds_sum')
if c and s:
    mean=s/c*1000
    b={k:float(v) for k,v in d.get('mc_server_tick_seconds_bucket',[])}
    def over(le):
        for k,v in b.items():
            if f'le="{le}"' in k: return c-v
    print(f"  ticks={c:,.0f}  mean={mean:.2f} ms")
    for le in ('0.05','0.1','1.0'):
        o=over(le)
        if o is not None: print(f"    over {float(le)*1000:>4.0f} ms: {o:,.0f} ({o/c*100:.4f}%)")
    print("  VERDICT_TICK", "WARN" if mean>25 else "OK", f"{mean:.2f}")
h=[float(v) for k,v in d.get('jvm_memory_bytes_used',[]) if 'nonheap' not in k and 'heap' in k]
if h: print(f"  heap used = {h[0]/1e9:.2f} GB")
for k,v in d.get('jvm_gc_collection_seconds_count',[]):
    if 'Old Generation' in k:
        print(f"  G1 Old Generation collections = {float(v):,.0f}")
        print("  VERDICT_GC", "WARN" if float(v)>0 else "OK")
PY
else
  warn "could not scrape mc-metrics"
fi
rm -f /tmp/mc-metrics.$$

# ------------------------------------------------------------ error triage
hdr "Error log triage (vs previous boot baseline)"
# ATM10 emits ~180 ERROR lines at every boot from broken pack loot tables and
# mod init ordering. Raw counts are meaningless; only a DELTA against the
# previous boot's log means anything.
kubectl -n "$NS" exec "$POD" -c mc -- sh -lc '
for pat in "LootDataType.*Couldn.t parse element" "Cannot get config value before config" "Tried to load invalid item"; do
  prev=$(zcat /data/logs/debug-1.log.gz 2>/dev/null | grep -cE "$pat")
  echo "PREV|$pat|$prev"
done' 2>/dev/null > /tmp/mc-base.$$ || true
while IFS='|' read -r _ pat prev; do
  [ -z "${pat:-}" ] && continue
  cur=$(printf '%s' "$LOG" | grep -cE "$pat")
  if [ "$cur" = "$prev" ]; then ok "$(printf '%.42s' "$pat")  $cur (same as previous boot)"
  else warn "$(printf '%.42s' "$pat")  now=$cur previous=$prev  <-- DELTA, investigate"; fi
done < /tmp/mc-base.$$
rm -f /tmp/mc-base.$$
SAVEFAIL=$(printf '%s' "$LOG" | grep -cE "Error saving|will not persist|Value must be within range")
if [ "$SAVEFAIL" -gt 0 ]; then
  bad "$SAVEFAIL silent item-loss event(s) (vanilla 1.21.1 stack>99 codec bug)"
  printf '%s' "$LOG" | grep -E "Error saving|will not persist" | tail -3 | cut -c1-150
else ok "no item/block-entity save failures"; fi
CRASH=$(printf '%s' "$LOG" | grep -ciE "OutOfMemory|crash report|Exception in server tick")
[ "$CRASH" -gt 0 ] && bad "$CRASH crash/OOM indicator(s)" || ok "no crash/OOM indicators"

# ---------------------------------------------------------------- backups
hdr "Backups"
kubectl -n "$NS" logs "$POD" -c backup --tail=40 2>/dev/null | grep -vE "No players online" | tail -4
LASTSNAP=$(kubectl -n "$NS" exec "$POD" -c backup -- sh -lc \
  'RESTIC_PASSWORD_FILE=/secrets/restic-password restic -r /backups snapshots --latest 1 2>/dev/null | grep -E "^[0-9a-f]{8} " | tail -1' 2>/dev/null)
echo "  latest snapshot: ${LASTSNAP:-<none / restic unavailable>}"
[ "${NOW_N:-1}" = "0" ] && echo "  (server empty; PAUSE_IF_NO_PLAYERS means backups idle by design)"

# ------------------------------------------------------- nightly restart
hdr "Nightly restart CronJob"
kubectl -n "$NS" get cronjob mc-nightly-restart --no-headers 2>/dev/null || warn "CronJob missing"
LASTJOB=$(kubectl -n "$NS" get jobs -o name 2>/dev/null | grep nightly | tail -1)
if [ -n "$LASTJOB" ]; then
  kubectl -n "$NS" get "$LASTJOB" -o jsonpath='  last run: succeeded={.status.succeeded} failed={.status.failed} start={.status.startTime} done={.status.completionTime}{"\n"}' 2>/dev/null
fi

# --------------------------------------------------------- node / network
if [ "$WITH_NODE" = "1" ]; then
  hdr "Node: cgroup pressure and network path (jade)"
  PU=$(kubectl -n "$NS" get pod "$POD" -o jsonpath='{.metadata.uid}' | tr '-' '_')
  CI=$(kubectl -n "$NS" get pod "$POD" -o jsonpath='{.status.containerStatuses[?(@.name=="mc")].containerID}' | sed 's|containerd://||')
  kubectl debug "node/$NODE" -q -it --image=busybox:1.36 --profile=sysadmin -- chroot /host sh -c "
    p=/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod$PU.slice/cri-containerd-$CI.scope
    echo \"MEMCUR \$(cat \$p/memory.current 2>/dev/null)\"
    echo \"MEMMAX \$(cat \$p/memory.max 2>/dev/null)\"
    grep -E '^(max|oom_kill) ' \$p/memory.events 2>/dev/null | sed 's/^/EV /'
    grep -E '^(anon|file) ' \$p/memory.stat 2>/dev/null | sed 's/^/ST /'
    ip -o link show tailscale0 2>/dev/null | sed -E 's/.*mtu ([0-9]+).*/MTU \1/'
    echo \"MSS \$(iptables-save 2>/dev/null | grep -c -i tcpmss)\"
  " 2>/dev/null | grep -E "^(MEMCUR|MEMMAX|EV|ST|MTU|MSS)" > /tmp/mc-node.$$ || true
  # kubectl debug leaves the pod behind; clean it up.
  kubectl get pods -A --no-headers 2>/dev/null | grep node-debugger \
    | awk '{print $1" "$2}' | while read -r n p; do kubectl -n "$n" delete pod "$p" --wait=false >/dev/null 2>&1; done
  if [ -s /tmp/mc-node.$$ ]; then
    CUR=$(awk '/^MEMCUR/{print $2}' /tmp/mc-node.$$); MAX=$(awk '/^MEMMAX/{print $2}' /tmp/mc-node.$$)
    HITS=$(awk '/^EV max/{print $3}' /tmp/mc-node.$$); OOMK=$(awk '/^EV oom_kill/{print $3}' /tmp/mc-node.$$)
    grep -E "^ST|^MTU" /tmp/mc-node.$$ | sed 's/^/  /'
    if [ -n "${CUR:-}" ] && [ -n "${MAX:-}" ] && [ "$MAX" != "max" ]; then
      PCT=$(( CUR * 100 / MAX ))
      echo "  memory.current=$CUR / memory.max=$MAX  (${PCT}%)"
      [ "$PCT" -ge 90 ] && warn "cgroup above 90% of limit" || ok "cgroup at ${PCT}% of limit"
    fi
    # THE metric: nonzero means the container hit its hard limit and the kernel
    # forced direct reclaim, which stalls whichever thread allocated.
    if [ "${HITS:-0}" != "0" ]; then bad "memory.events max=$HITS (hard-limit hits -> reclaim stalls)"
    else ok "memory.events max=0 (no hard-limit hits)"; fi
    [ "${OOMK:-0}" != "0" ] && bad "oom_kill=$OOMK" || true
    MSSN=$(awk '/^MSS/{print $2}' /tmp/mc-node.$$)
    [ "${MSSN:-0}" -gt 0 ] && ok "MSS clamp rule present ($MSSN)" \
      || warn "no TCPMSS clamp rule; pod MTU 1450 vs tailscale0 1280 can black-hole large packets"
  else
    warn "node probe failed (needs node debug privileges)"
  fi
  rm -f /tmp/mc-node.$$
fi

printf '\n\033[1mDone.\033[0m Interpretation guidance: .claude/skills/mc-health/SKILL.md\n'
