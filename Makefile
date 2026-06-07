.PHONY: up down verify

# Bring up the whole stack locally (minikube/kind/k3d). Prompts for the Anthropic
# key, or pass it inline:  make up ANTHROPIC_API_KEY=sk-ant-...
up:
	@ANTHROPIC_API_KEY="$(ANTHROPIC_API_KEY)" ./scripts/up.sh

# Tear down control plane + tenants (keeps the cluster).
down:
	@./scripts/down.sh

# Faithful auth-chain test using a real OpenClaw CLI device client (no browser).
verify:
	@./test/verify-connect.sh

# Phase 2 behavioral test: cron fires on schedule despite scale-to-zero.
# Needs short timings; set them first, e.g.:
#   kubectl -n oc-system set env deploy/controlplane IDLE_TIMEOUT=45s REAPER_TICK=10s CRON_TICK=15s
verify-cron:
	@./test/verify-cron.sh
