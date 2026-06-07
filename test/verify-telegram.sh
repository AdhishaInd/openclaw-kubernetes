#!/usr/bin/env bash
# Phase 3 behavioral test (no real Telegram needed): proves the wake-on-webhook
# pipeline — a Telegram update delivered to the control plane wakes a slept pod and
# is forwarded to its gateway.
#
# Prereqs:
#   - TELEGRAM_BOT_TOKEN env set (a real @BotFather token, so the channel starts).
#   - Control plane deployed with TELEGRAM_WEBHOOK_BASE=<your cloudflared https url>
#     and reachable at localhost:8080 (port-forward).
#   - Short timings recommended (IDLE_TIMEOUT, REAPER_TICK) so the pod sleeps quickly.
#
# It simulates Telegram by POSTing a synthetic update straight to the control plane
# /tg/<id> with the correct secret. Exit 0 = PASS.
set -uo pipefail
NS=oc-users
log(){ echo "[tg-test $(date -u +%H:%M:%S)] $*"; }
[ -n "${TELEGRAM_BOT_TOKEN:-}" ] || { log "FAIL: set TELEGRAM_BOT_TOKEN"; exit 1; }

EMAIL="tg-$(date +%s)@example.com"
log "signup $EMAIL"
curl -s -o /dev/null -c /tmp/tg-test.cookies -X POST http://localhost:8080/signup \
  --data-urlencode "email=$EMAIL" --data-urlencode "password=tgtest12345"
curl -s -o /dev/null -b /tmp/tg-test.cookies -H "Accept: text/html" http://localhost:8080/
ID=$(kubectl -n $NS get deploy -l openclaw.io/managed-by=controlplane \
      --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1:].metadata.labels.openclaw\.io/user}')
DEP=oc-$ID
log "tenant=$ID; waiting ready…"
kubectl -n $NS rollout status deploy/$DEP --timeout=180s >/dev/null
touch_act(){ kubectl -n $NS patch deploy/$DEP --type=merge \
  -p "{\"metadata\":{\"annotations\":{\"openclaw.io/last-activity\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}}}" >/dev/null; }

log "connecting Telegram bot (registers webhook; restarts gateway)…"
touch_act  # keep the pod warm so the reaper doesn't kill it mid-connect
code=$(curl -s -b /tmp/tg-test.cookies -o /tmp/tg-connect.out -w "%{http_code}" \
  -X POST http://localhost:8080/connect/telegram --data-urlencode "bot_token=$TELEGRAM_BOT_TOKEN")
log "connect -> HTTP $code; $(cat /tmp/tg-connect.out)"
[ "$code" = "200" ] || { log "FAIL: connect failed"; exit 1; }

# Read the per-user webhook secret the control plane generated.
SECRET=$(kubectl -n $NS get secret oc-user-$ID -o jsonpath='{.data.telegram-webhook-secret}' | base64 -d)
[ -n "$SECRET" ] || { log "FAIL: no webhook secret stored"; exit 1; }

# Let the pod scale to zero so we prove wake-on-webhook (not just 'already up').
log "waiting for pod to scale to zero…"
slept=0
for i in $(seq 1 40); do
  [ "$(kubectl -n $NS get deploy/$DEP -o jsonpath='{.spec.replicas}')" = "0" ] && { slept=1; break; }
  sleep 5
done
[ "$slept" = "1" ] || { log "FAIL: pod never slept (lower IDLE_TIMEOUT/REAPER_TICK)"; exit 1; }
log "pod ASLEEP. Sending synthetic Telegram update to control plane…"

NOW=$(date +%s)
UPDATE='{"update_id":'"$NOW"',"message":{"message_id":1,"date":'"$NOW"',"chat":{"id":424242,"type":"private"},"from":{"id":424242,"is_bot":false,"first_name":"Probe"},"text":"ping from test"}}'
code=$(curl -s -o /dev/null -w "%{http_code}" -X POST http://localhost:8080/tg/$ID \
  -H "Content-Type: application/json" -H "X-Telegram-Bot-Api-Secret-Token: $SECRET" -d "$UPDATE")
log "synthetic update -> HTTP $code"

# Assert via control-plane logs (race-free): woke + forwarded to the pod.
fired=0
for i in $(seq 1 24); do
  if kubectl -n oc-system logs deploy/controlplane --since=6m 2>/dev/null | grep -q "telegram delivered user=$ID"; then
    fired=1; break
  fi
  sleep 5
done
if [ "$fired" = "1" ]; then
  log "PASS: control plane woke the slept pod and forwarded the Telegram update."
  exit 0
fi
log "FAIL: no 'telegram delivered' observed."
kubectl -n oc-system logs deploy/controlplane --since=6m 2>/dev/null | grep -i "telegram\|wake" | tail -10
exit 1
