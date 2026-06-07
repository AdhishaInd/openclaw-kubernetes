#!/usr/bin/env bash
# Faithful end-to-end auth-chain test WITHOUT a browser.
#
# Uses OpenClaw's own CLI as a remote device client (real device-identity crypto,
# challenge-response, pairing — exactly what the browser does) to verify whether a
# tenant gateway will accept a connection with the per-user token.
#
# Exit 0 = client connected (auth chain OK). Non-zero = failed; the gateway's
# error (pairing required / NOT_PAIRED / unauthorized) is printed.
#
# Usage: test/verify-connect.sh [userId]
#   With no userId, signs up a throwaway test account via the control plane
#   (requires `kubectl -n oc-system port-forward svc/controlplane 8080:8080`).
set -euo pipefail
NS=oc-users
IMG=ghcr.io/openclaw/openclaw:latest
CLIENT=oc-testclient

log() { echo "[verify] $*"; }

ID="${1:-}"
if [ -z "$ID" ]; then
  EMAIL="verify-$(date +%s)@example.com"
  log "signing up $EMAIL via control plane (localhost:8080)…"
  curl -s -o /dev/null -c /tmp/verify.cookies -X POST http://127.0.0.1:8080/signup \
    --data-urlencode "email=$EMAIL" --data-urlencode "password=verifyverify12"
  curl -s -o /dev/null -b /tmp/verify.cookies -H "Accept: text/html" http://127.0.0.1:8080/
  ID=$(kubectl -n "$NS" get deploy -l openclaw.io/managed-by=controlplane \
        --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1:].metadata.labels.openclaw\.io/user}')
fi
log "tenant user id: $ID"

log "waking pod…"
kubectl -n "$NS" scale deploy/oc-"$ID" --replicas=1 >/dev/null
kubectl -n "$NS" rollout status deploy/oc-"$ID" --timeout=150s >/dev/null

TOKEN=$(kubectl -n "$NS" get secret oc-user-"$ID" -o jsonpath='{.data.gateway-token}' | base64 -d)
GW="ws://oc-$ID.$NS.svc:18789"

# Ensure a fresh remote client pod (fresh device identity each run).
kubectl -n "$NS" delete pod "$CLIENT" --ignore-not-found >/dev/null 2>&1 || true
kubectl -n "$NS" run "$CLIENT" --image="$IMG" --restart=Never \
  --command -- sleep 3600 >/dev/null
kubectl -n "$NS" wait --for=condition=Ready pod/"$CLIENT" --timeout=90s >/dev/null

log "connecting as a faithful remote device client → $GW"
set +e
OUT=$(kubectl -n "$NS" exec "$CLIENT" -- env \
  OPENCLAW_STATE_DIR=/tmp/clientstate OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1 \
  node openclaw.mjs status --url "$GW" --token "$TOKEN" 2>&1)
RC=$?
set -e

echo "----- client output -----"
echo "$OUT" | grep -iE "pairing|not_paired|unauthorized|ready|gateway|connect|error|status" | head -12 || true
echo "-------------------------"
kubectl -n "$NS" delete pod "$CLIENT" --ignore-not-found >/dev/null 2>&1 || true

if [ $RC -eq 0 ] && ! echo "$OUT" | grep -qiE "pairing required|not_paired|unauthorized"; then
  log "PASS: device client connected (auth chain works)."
  exit 0
fi
log "FAIL: client could not connect (see error above)."
exit 1
