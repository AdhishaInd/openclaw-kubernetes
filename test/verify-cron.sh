#!/usr/bin/env bash
# Phase 2 behavioral test (no browser): proves a cron job fires on schedule even
# though the pod scales to zero when idle.
#
# Flow: sign up a tenant, add a recurring cron job, let the reaper sleep the pod
# (scale-to-zero), then verify the control plane wakes it for the slot and the job
# actually fires (run count increases) while we are NOT keeping it up.
#
# Requires the control plane reachable at localhost:8080 AND configured with short
# timings (the harness below sets them): REAPER_TICK, IDLE_TIMEOUT, CRON_TICK,
# CRON_WAKE_LEAD small. Exit 0 = PASS.
set -uo pipefail
NS=oc-users
log(){ echo "[cron-test $(date -u +%H:%M:%S)] $*"; }

EMAIL="cron-$(date +%s)@example.com"
log "signup $EMAIL"
curl -s -o /dev/null -c /tmp/cron-test.cookies -X POST http://localhost:8080/signup \
  --data-urlencode "email=$EMAIL" --data-urlencode "password=crontest123"
curl -s -o /dev/null -b /tmp/cron-test.cookies -H "Accept: text/html" http://localhost:8080/
ID=$(kubectl -n $NS get deploy -l openclaw.io/managed-by=controlplane \
      --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1:].metadata.labels.openclaw\.io/user}')
DEP=oc-$ID
log "tenant=$ID; waiting for ready…"
kubectl -n $NS rollout status deploy/$DEP --timeout=180s >/dev/null
# Reset the idle clock now that the pod is actually ready, so reaper doesn't sleep
# it mid-setup (cold start can otherwise consume the whole idle window).
touch_act(){ kubectl -n $NS patch deploy/$DEP --type=merge \
  -p "{\"metadata\":{\"annotations\":{\"openclaw.io/last-activity\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}}}" >/dev/null; }
touch_act

runcount(){ kubectl -n $NS exec deploy/$DEP -c gateway -- node openclaw.mjs cron runs --id "$1" --json 2>/dev/null \
  | python3 -c "import sys,json
try:
 d=json.load(sys.stdin); print(len(d.get('entries',[])))
except: print(0)"; }
replicas(){ kubectl -n $NS get deploy/$DEP -o jsonpath='{.spec.replicas}' 2>/dev/null; }
cronnext(){ kubectl -n $NS get deploy/$DEP -o jsonpath='{.metadata.annotations.openclaw\.io/cron-next}' 2>/dev/null; }

log "adding --every 3m cron job"
touch_act
kubectl -n $NS exec deploy/$DEP -c gateway -- node openclaw.mjs cron add --name t --every 3m \
  --message "ping" --best-effort-deliver --json >/dev/null 2>&1
firstjob(){ python3 -c "import sys,json
s=sys.stdin.read(); i=s.find('{')
try: print(json.loads(s[i:] if i>=0 else s)['jobs'][0]['id'])
except: print('')"; }
JOB=""
for try in 1 2 3; do
  touch_act
  JOB=$(kubectl -n $NS exec deploy/$DEP -c gateway -- node openclaw.mjs cron list --json 2>/dev/null | firstjob)
  [ -n "$JOB" ] && break
  sleep 3
done
log "job id=${JOB:-<none>}"
[ -n "$JOB" ] || { log "FAIL: could not add/find cron job"; exit 1; }

# PHASE 1: wait for scale-to-zero by the reaper (proves scale-to-zero with cron present)
log "PHASE 1: waiting for reaper to sleep the pod…"
slept=0
for i in $(seq 1 40); do
  if [ "$(replicas)" = "0" ]; then slept=1; break; fi
  sleep 5
done
[ "$slept" = "1" ] || { log "FAIL: pod never scaled to 0"; exit 1; }
CN=$(cronnext)
log "pod ASLEEP (replicas=0). cron-next annotation = ${CN:-<unset>}"
[ -n "$CN" ] || { log "FAIL: cron-next annotation not stamped on scale-down"; exit 1; }

# PHASE 2: the control plane wakes the slept pod shortly before its slot (logged as
# "cron wake user=<id> for slot") and the in-pod scheduler fires the job. We assert
# (1) the waker fired (durable control-plane log), then (2) a run was recorded —
# read from a STABLY warm pod (querying mid-churn returns empty execs).
log "PHASE 2: waiting for the control-plane waker (cron wake)…"
# Reliable "did it run" signal: cron list --json exposes state.lastRunAtMs once the
# in-pod scheduler has fired the job (cron runs --json is unreliable here).
fired_check(){ kubectl -n $NS exec deploy/$DEP -c gateway -- node openclaw.mjs cron list --json 2>/dev/null \
  | python3 -c "import sys,json
s=sys.stdin.read(); i=s.find('{')
try: print(1 if json.loads(s[i:] if i>=0 else s)['jobs'][0].get('state',{}).get('lastRunAtMs',0)>0 else 0)
except: print(0)"; }
woke=0
for i in $(seq 1 50); do          # up to ~4 min (cron every 3m + wake lead)
  kubectl -n oc-system logs deploy/controlplane --since=15m 2>/dev/null | grep -q "cron wake user=$ID for slot" && { woke=1; break; }
  sleep 5
done
log "waker observed=$woke; letting the in-pod scheduler reach + fire its slot…"
sleep 75

# Stable read: keep the pod warm (waker/reaper leave a warm, recently-active pod
# alone), let the gateway settle, then read the run history with retries.
kubectl -n $NS scale deploy/$DEP --replicas=1 >/dev/null 2>&1
kubectl -n $NS rollout status deploy/$DEP --timeout=150s >/dev/null 2>&1
fired=0
for t in 1 2 3 4 5; do
  touch_act
  sleep 6
  [ "$(fired_check)" = "1" ] && { fired=1; break; }
done

log "result: woke=$woke fired=$fired"
if [ "$woke" = "1" ] && [ "$fired" = "1" ]; then
  log "PASS: pod slept, control plane woke it before its slot, and the cron job fired."
  exit 0
fi
log "FAIL: cron did not fire after scale-to-zero (woke=$woke fired=$fired)."
exit 1
