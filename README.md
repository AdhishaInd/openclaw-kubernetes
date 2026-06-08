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

Prereqs: `git`, Docker, `kubectl`, `openssl`, and one of **minikube**, **kind**, or
**k3d**. You'll need an Anthropic API key (shared across tenants in this setup).

From nothing — clone, enter, and bring up in one line:

```bash
git clone https://github.com/AdhishaInd/openclaw-kubernetes.git && cd openclaw-kubernetes && ANTHROPIC_API_KEY=sk-ant-... make up
```

(Already cloned? Just `ANTHROPIC_API_KEY=sk-ant-... make up`, or `make up` to be
prompted for the key. Pick a provider with `CLUSTER=kind make up`.)

That builds the control-plane image into your local cluster, generates a cookie
signing key, creates the secrets (never written to the repo), deploys everything,
and port-forwards. Then open **http://localhost:8080/signup**, create an account,
and your private OpenClaw cold-starts (~60–90s the first time) and connects
automatically.

Tear down with `make down` (keeps the cluster).

## Architecture

> Full diagrams (components, lifecycle, web/cron/telegram flows): **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)**.

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
slots — so a pod scaled to zero would never fire its jobs. The gateway's own
scheduler still does the firing (no double-fire, no CLI scope issues); the control
plane just makes sure the pod is **running before each slot**:

- The in-pod scheduler stays enabled, so **warm pods fire their jobs natively**.
- While a pod is awake — and once more just before the reaper sleeps it — the
  control plane reads `cron list --json` and stamps the earliest next-fire time
  onto the Deployment (`openclaw.io/cron-next`).
- For a **sleeping** pod, a scheduler loop wakes it `CRON_WAKE_LEAD` *before* its
  slot and holds it (a guard annotation stops the reaper from sleeping it) until
  the slot passes, so the in-pod scheduler fires it; then it advances the mirror and
  releases the pod.

Net effect: a cron user's pod only wakes around its scheduled times, so
**scale-to-zero is preserved**. `CRON_WAKE_LEAD` (default 3m) must exceed cold start
so the pod is ready before the slot. Knobs: `CRON_TICK`, `CRON_WAKE_LEAD`,
`REAPER_TICK` (see the Deployment). Verify with `make verify-cron` (no browser).
Headless cron should use `--webhook` (or a channel) for delivery, since the default
"post to last chat channel" has nowhere to go.

### Telegram with scale-to-zero (Phase 3)

A sleeping pod can't long-poll Telegram, so we use **webhook mode + wake-on-webhook**:

- A user connects their own bot on the **`/channels`** page (paste the @BotFather
  token). The control plane stores it, writes the channel config into the pod with
  `webhookUrl` pointing back at the control plane, and restarts the gateway so it
  registers the webhook with Telegram (`setWebhook`).
- Telegram then POSTs updates to **`/tg/<userId>`** on the control plane (always-on,
  public HTTPS). The control plane verifies the per-user secret, **wakes the pod**
  (same primitive as cron), and forwards the update to the pod's
  `:8787/telegram-webhook` listener (retrying until it's up after cold start). The
  agent replies to Telegram directly; the reaper sleeps the pod again afterward.

OpenClaw also requires a one-time **DM pairing** approval for each new sender (so
randoms can't use someone's assistant). The `/channels` page **surfaces pending
pairing requests with an Approve button** (the control plane runs `pairing approve`
in the pod), so it's self-serve — no shell access needed.

Public HTTPS is provided by an **in-cluster cloudflared quick tunnel**
(`deploy/05-cloudflared.yaml`); the control plane auto-discovers the
`https://….trycloudflare.com` URL from cloudflared's logs — no manual step. Verify
with `make verify-telegram` (set `TELEGRAM_BOT_TOKEN`; it POSTs a synthetic update
and asserts wake+forward). First message after idle has the usual ~30–60s cold-start
delay. Only **webhook-mode** channels (Telegram, Slack, Teams, Line, …) fit
scale-to-zero; persistent-connection ones (Discord, WhatsApp-web) would need an
always-on shim and are out of scope.

> The cloudflared quick-tunnel URL changes on restart; users re-connect (re-register)
> their bot afterward. Use a named Cloudflare tunnel for a stable URL in production.

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
| `IDLE_TIMEOUT` | `10m` | Idle time before scale-to-zero |
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
- **Phase 3 — messaging**: ✅ Telegram via webhook + wake-on-webhook (see
  [Telegram with scale-to-zero](#telegram-with-scale-to-zero-phase-3)). Other
  webhook channels (Slack/Teams/Line) follow the same pattern; persistent-connection
  channels (Discord/WhatsApp-web) need an always-on shim and remain out of scope.

## License

MIT — see [LICENSE](LICENSE).
