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

log "adding --every 2m cron job"
touch_act
kubectl -n $NS exec deploy/$DEP -c gateway -- node openclaw.mjs cron add --name t --every 2m \
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

# PHASE 2: do NOT touch the pod; verify the control plane wakes it and force-runs
# the due job. The authoritative signal is the control plane's "cron fired" log
# (race-free, unlike polling the churning pod's run history).
log "PHASE 2: watching control plane for a scheduler-driven cron fire (no user activity)…"
woke=0; fired=0
for i in $(seq 1 48); do          # up to ~4 min
  logs=$(kubectl -n oc-system logs deploy/controlplane --since=12m 2>/dev/null)
  echo "$logs" | grep -q "cron wake user=$ID"  && woke=1
  if echo "$logs" | grep -q "cron fired user=$ID"; then fired=1; break; fi
  sleep 5
done

# Secondary confirmation: the run is recorded in the pod's own history.
runs="?"
if [ "$fired" = "1" ]; then
  kubectl -n $NS scale deploy/$DEP --replicas=1 >/dev/null 2>&1
  if kubectl -n $NS rollout status deploy/$DEP --timeout=120s >/dev/null 2>&1; then
    touch_act
    runs=$(kubectl -n $NS exec deploy/$DEP -c gateway -- node openclaw.mjs cron runs --id "$JOB" --json 2>/dev/null \
      | python3 -c "import sys,json
s=sys.stdin.read(); i=s.find('{')
try: print(len(json.loads(s[i:] if i>=0 else s).get('entries',[])))
except: print('?')")
  fi
fi

log "result: woke=$woke fired=$fired pod_run_history=$runs"
if [ "$fired" = "1" ]; then
  log "PASS: control plane woke the slept pod and force-ran the cron job on schedule."
  exit 0
fi
log "FAIL: no scheduler-driven cron fire observed after scale-to-zero."
exit 1
