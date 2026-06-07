#!/usr/bin/env bash
# Tear down the control plane and all tenant resources (keeps the cluster).
set -euo pipefail
cd "$(dirname "$0")/.."

echo "Deleting tenant resources…"
kubectl -n oc-users delete deploy,svc,pvc,secret -l openclaw.io/managed-by=controlplane --ignore-not-found --wait=false 2>/dev/null || true
echo "Deleting control plane + namespaces…"
kubectl delete -f deploy/04-user-networkpolicy.yaml --ignore-not-found 2>/dev/null || true
kubectl delete -f deploy/03-controlplane.yaml --ignore-not-found 2>/dev/null || true
kubectl delete namespace oc-system oc-users --ignore-not-found 2>/dev/null || true
echo "Done. (The Kubernetes cluster itself was left running.)"
