#!/usr/bin/env bash
# One-command local bring-up for multi-tenant OpenClaw on Kubernetes.
#
#   ANTHROPIC_API_KEY=sk-ant-... ./scripts/up.sh      # non-interactive
#   ./scripts/up.sh                                   # prompts for the key
#
# Works with minikube, kind, or k3d (auto-detected; override with CLUSTER=...).
set -euo pipefail
cd "$(dirname "$0")/.."

IMG=openclaw-controlplane:dev
CLUSTER_NAME=openclaw
LOCAL_PORT=8080

say() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31mError:\033[0m %s\n' "$*" >&2; exit 1; }

# --- pick a cluster provider ---
CLUSTER="${CLUSTER:-}"
if [ -z "$CLUSTER" ]; then
  if command -v minikube >/dev/null;   then CLUSTER=minikube
  elif command -v kind >/dev/null;     then CLUSTER=kind
  elif command -v k3d >/dev/null;      then CLUSTER=k3d
  else die "need one of: minikube, kind, or k3d (set CLUSTER= to choose)"; fi
fi
command -v kubectl >/dev/null || die "kubectl not found"
command -v docker  >/dev/null || die "docker not found"
say "Using cluster provider: $CLUSTER"

# --- ensure the cluster exists/running, build + load the image ---
case "$CLUSTER" in
  minikube)
    minikube status >/dev/null 2>&1 || minikube start
    say "Building control-plane image into minikube"
    minikube image build -t "$IMG" ./controlplane ;;
  kind)
    kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME" || kind create cluster --name "$CLUSTER_NAME"
    kubectl config use-context "kind-$CLUSTER_NAME" >/dev/null
    say "Building + loading control-plane image"
    docker build -t "$IMG" ./controlplane
    kind load docker-image "$IMG" --name "$CLUSTER_NAME" ;;
  k3d)
    k3d cluster list 2>/dev/null | grep -q "$CLUSTER_NAME" || k3d cluster create "$CLUSTER_NAME"
    kubectl config use-context "k3d-$CLUSTER_NAME" >/dev/null
    say "Building + importing control-plane image"
    docker build -t "$IMG" ./controlplane
    k3d image import "$IMG" -c "$CLUSTER_NAME" ;;
  *) die "unknown CLUSTER=$CLUSTER (use minikube|kind|k3d)";;
esac

# --- get the shared Anthropic key ---
if [ -z "${ANTHROPIC_API_KEY:-}" ]; then
  read -rsp "Enter your Anthropic API key (sk-ant-...): " ANTHROPIC_API_KEY; echo
fi
[ -n "$ANTHROPIC_API_KEY" ] || die "no Anthropic API key provided"

# --- namespaces + RBAC ---
say "Applying namespaces and RBAC"
kubectl apply -f deploy/00-namespaces.yaml
kubectl apply -f deploy/02-rbac.yaml

# --- secrets (generated; never stored in the repo) ---
say "Creating secrets (cookie key generated, Anthropic key from input)"
kubectl -n oc-system create secret generic oc-controlplane \
  --from-literal=cookie-key="$(openssl rand -hex 32)" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n oc-users create secret generic oc-shared-anthropic \
  --from-literal=anthropic-key="$ANTHROPIC_API_KEY" \
  --dry-run=client -o yaml | kubectl apply -f -

# --- control plane + per-user network policy ---
say "Deploying control plane"
kubectl apply -f deploy/03-controlplane.yaml
kubectl apply -f deploy/04-user-networkpolicy.yaml
kubectl -n oc-system rollout status deploy/controlplane --timeout=180s

printf '\n\033[1;32mReady.\033[0m Open:  http://localhost:%s/signup\n\n' "$LOCAL_PORT"
echo "Port-forwarding (Ctrl-C to stop). Re-run this command any time to restart it."
exec kubectl -n oc-system port-forward svc/controlplane "${LOCAL_PORT}:8080"
