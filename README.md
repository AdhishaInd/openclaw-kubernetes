# openclaw-kubernetes

Multi-tenant [OpenClaw](https://openclaw.ai) on Kubernetes: **each user gets their
own pod**, provisioned on self-serve signup, that **scales to zero when idle** and
wakes on demand. A small Go control plane handles signup/login, provisioning,
reverse-proxying users to their own gateway, and idle scale-down.

> Status: **Phase 1** — web UI with scale-to-zero. Messaging/cron for asleep pods
> are future phases (see [Roadmap](#roadmap)).

## Why pod-per-user

OpenClaw's in-process multi-agent mode is explicitly *not a hard sandbox*, and its
agents have shell + browser access — so it's unsafe to put mutually-untrusted users
in one gateway. A pod per user gives real OS/filesystem/network isolation.

## Quick start (local)

Prereqs: Docker, `kubectl`, and one of **minikube**, **kind**, or **k3d**. You'll
need an Anthropic API key (shared across tenants in this setup).

```bash
ANTHROPIC_API_KEY=sk-ant-... make up
#   …or just `make up` and it'll prompt for the key.
```

That builds the control-plane image into your local cluster, generates a cookie
signing key, creates the secrets (never written to the repo), deploys everything,
and port-forwards. Then open **http://localhost:8080/signup**, create an account,
and your private OpenClaw cold-starts (~60–90s the first time) and connects
automatically.

Tear down with `make down` (keeps the cluster). Override the provider with
`CLUSTER=kind make up`.

## Architecture

```
 user ──http──▶ control plane (ns oc-system)
                 ├─ auth        signup/login, bcrypt, signed-cookie sessions (stateless)
                 ├─ provisioner creates per-user resources via the Kubernetes API
                 ├─ proxy       activating reverse proxy (HTTP + WebSocket)
                 └─ reaper      scales idle Deployments to 0
                       ▼
   per-user (ns oc-users, labeled openclaw.io/user=<id>):
     Secret  oc-user-<id>   {password-hash(bcrypt), gateway-token}
     PVC     oc-state-<id>  (2Gi) — persists config/auth/sessions
     Deploy  oc-<id>        replicas 0/1; initContainer onboards the gateway
     Service oc-<id>        :18789
```

- **No external database** — Kubernetes is the datastore. A user *is* its labeled
  resources; sessions are stateless HMAC-signed cookies.
- **Single public origin** — the proxy routes everyone through one hostname by
  session cookie; no per-user subdomains.
- **Onboarding** runs as an idempotent initContainer that configures the gateway
  on first start (shared key, default model, allowed origins, token-only auth).
- **Scale-to-zero**: the proxy wakes a pod on request (showing a readiness-polling
  interstitial) and the reaper scales it back down after `IDLE_TIMEOUT`.

### Cron with scale-to-zero (Phase 2)

OpenClaw's cron scheduler runs *inside* the gateway and does not catch up missed
slots — so a pod scaled to zero would never fire its jobs. Instead, the control
plane is the **sole cron driver**:

- The in-pod scheduler is disabled at onboarding (`cron.enabled=false`), so there
  is no natural firing (hence no double-fire).
- While a pod is awake (and once more just before the reaper sleeps it), the
  control plane reads `cron list --json` and stamps the earliest next-fire time
  onto the Deployment (`openclaw.io/cron-next`).
- A scheduler loop wakes a sleeping pod when a job is due and **force-runs** it
  (`cron run <id>`), then advances the mirror and releases the pod back to the
  reaper. A guard annotation stops the reaper from sleeping a pod mid-run.

Net effect: a cron user's pod only wakes around its scheduled times (plus the run
and a short grace), so **scale-to-zero is preserved**. Cold-start time just delays
a run by a few tens of seconds — it can't cause a miss. Knobs: `CRON_TICK`,
`CRON_WAKE_LEAD`, `REAPER_TICK` (see the Deployment). Verify with `make verify-cron`
(no browser). Note: headless cron should use `--webhook` (or a channel) for
delivery, since the default "post to last chat channel" has nowhere to go.

### How a user connects

`session cookie → per-user gateway token (delivered via #token= redirect, the
official OpenClaw dashboard mechanism) → token-only Control-UI auth → connected`.
Device pairing is disabled per pod (`dangerouslyDisableDeviceAuth`): the control
plane is the trust boundary (authenticated session + per-user token + single-tenant,
network-isolated pods), so per-browser device approval would be redundant.

## Layout

```
controlplane/   Go control plane (auth, provision, proxy, reaper) + Dockerfile
deploy/         namespaces, RBAC, control plane, network policy, secret placeholders
scripts/        up.sh / down.sh
test/           verify-connect.sh — faithful auth-chain test (no browser)
openclaw.yaml   optional standalone single-instance example (not used by the control plane)
```

## Configuration (control-plane env, `deploy/03-controlplane.yaml`)

| Env | Default | Meaning |
|-----|---------|---------|
| `PUBLIC_ORIGIN` | `http://localhost:8080` | Browser-facing origin; baked into pod allowed-origins |
| `USERS_NS` | `oc-users` | Namespace for per-user resources |
| `SHARED_KEY_SECRET` | `oc-shared-anthropic` | Secret (in `USERS_NS`) with key `anthropic-key` |
| `DEFAULT_MODEL` | `anthropic/claude-sonnet-4-6` | Model set during onboarding |
| `IDLE_TIMEOUT` | `15m` | Idle time before scale-to-zero |
| `COLD_START_TIMEOUT` | `90s` | Max wait for a pod to become ready on wake |
| `COOKIE_KEY` | (from secret) | HMAC key for session cookies |

## Testing

`make verify` (or `./test/verify-connect.sh`) signs up a tenant, wakes the pod, and
connects a real OpenClaw **CLI device client** (same crypto handshake as the
browser) — verifying the full auth chain with no browser. Exit 0 = chain works.

## Cloud-Kubernetes portability

The manifests are vanilla Kubernetes; moving to a managed cluster (GKE/EKS/AKS)
mainly changes how the image and ingress are handled:

- **Image**: push `openclaw-controlplane` to a registry (e.g. GHCR) and set the tag
  in `deploy/03-controlplane.yaml` instead of building into the local cluster.
- **HTTPS is required, not optional**: the OpenClaw Control UI generates a device
  identity that browsers only allow in a *secure context*. `localhost` qualifies;
  any other host **must be `https://`**. So front the control plane with an Ingress
  + TLS (cert-manager) and set `PUBLIC_ORIGIN=https://your-host`.
- **NetworkPolicy** (`deploy/04-user-networkpolicy.yaml`) only enforces under a
  CNI that supports it (Calico/Cilium); most managed clusters do. minikube's default
  bridge CNI does **not** — start it with `--cni=calico` for real enforcement.
- **StorageClass**: relies on a default dynamic provisioner (managed clusters ship one).
- **Secrets**: use a real secrets manager / sealed-secrets / external-secrets rather
  than literals.
- **Sizing**: each pod is a full Node + Playwright runtime (~300 MB–1 GB RAM); plan
  node capacity and tune the requests/limits in the Deployment template.

## Security notes

- The per-user gateway token is the user's own per-pod credential and is delivered
  to their browser via the `#token=` redirect (same as OpenClaw's own dashboard).
- Device auth is disabled per pod by design (see [How a user connects](#how-a-user-connects)).
- POC-grade signup auth (email + password, no email verification / reset). Harden
  before real exposure: SSO/OIDC, rate limiting, HTTPS, per-user API-key isolation.
- The control plane runs single-replica (in-memory readiness/activity caches);
  fine for a POC, shard/rework for HA.

## Roadmap

- **Phase 2 — cron**: ✅ done — the control plane wakes sleeping pods and force-runs
  due jobs (see [Cron with scale-to-zero](#cron-with-scale-to-zero-phase-2)).
- **Phase 3 — messaging**: webhook channels (Discord/Slack) can wake-on-webhook;
  long-poll channels (Telegram/WhatsApp) need an always-on per-channel receiver
  shim, which partially defeats scale-to-zero for those users.

## License

MIT — see [LICENSE](LICENSE).
