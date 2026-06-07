# Architecture — openclaw-kubernetes

Multi-tenant OpenClaw on Kubernetes: **one pod per user**, **scale-to-zero** when
idle, woken on demand by the web UI, scheduled cron, or inbound Telegram. A single
Go **control plane** is the always-on front door and lifecycle manager; each user's
OpenClaw **gateway** runs in its own pod and does the actual agent work.

Diagrams are [Mermaid](https://mermaid.js.org/) — they render on GitHub and in most
editors (VS Code: "Markdown Preview Mermaid Support").

---

## 1. Components

```mermaid
flowchart TB
    user["User browser"]
    tg["Telegram"]
    cf["Cloudflare edge"]
    anth["Anthropic API"]

    subgraph cluster["Kubernetes cluster"]
        subgraph sys["namespace: oc-system"]
            cp["Control plane (Go)<br/>auth · provisioner · proxy<br/>reaper · cron waker · /tg receiver"]
            cfd["cloudflared<br/>(quick tunnel)"]
            cpsec["Secret: oc-controlplane<br/>(cookie key)"]
        end
        subgraph usersns["namespace: oc-users"]
            shared["Secret: oc-shared-anthropic"]
            subgraph tenant["per user: oc-&lt;id&gt; (labeled openclaw.io/user)"]
                pod["Gateway pod (0/1 replicas)<br/>:18789 control UI/API · WS<br/>:8787 telegram webhook<br/>in-pod cron scheduler"]
                pvc["PVC oc-state-&lt;id&gt;<br/>config · auth · sessions · cron"]
                usec["Secret oc-user-&lt;id&gt;<br/>pw hash · gateway token<br/>telegram token + secret"]
                svc["Service oc-&lt;id&gt;"]
            end
        end
    end

    user -->|"HTTPS"| cf
    tg -->|"webhook POST"| cf
    cf -->|"tunnel"| cfd --> cp
    user -.->|"local: port-forward :8080"| cp

    cp -->|"k8s API: create/scale/exec/patch"| tenant
    cp -->|"reverse proxy (HTTP+WS)"| svc --> pod
    pod --> pvc
    cp -->|"reads logs for tunnel URL"| cfd
    pod -->|"agent turns"| anth
    pod -. mounts .- shared
    pod -. mounts .- usec
```

Key idea: **the control plane is always on; tenant pods are not.** Everything that
must survive a sleeping pod (receiving webhooks, knowing when to wake, auth) lives
in the control plane. Kubernetes itself is the datastore (a "user" = its labeled
Secret/PVC/Deployment/Service); sessions are stateless signed cookies.

---

## 2. Pod lifecycle (scale-to-zero)

```mermaid
stateDiagram-v2
    [*] --> Asleep: provisioned (replicas 0)
    Asleep --> Waking: web request / Telegram msg / cron slot near
    Waking --> Warm: gateway ready (/healthz)
    Warm --> Busy: cron fire / webhook delivery in flight
    Busy --> Warm: work done (guard cleared)
    Warm --> Asleep: idle > IDLE_TIMEOUT (reaper)
    note right of Warm
        last-activity refreshed by:
        UI request, Telegram message
        (NOT by cron wakes)
    end note
    note right of Busy
        openclaw.io/busy=1
        reaper never sleeps a busy pod
    end note
```

The **reaper** (every `REAPER_TICK`, default 60s) scales a pod to 0 when it's
running, **not** `busy`, and idle longer than `IDLE_TIMEOUT` (default 10m). Just
before sleeping it, it mirrors the pod's next cron time onto the Deployment.

---

## 3. Web access (activating reverse proxy)

```mermaid
sequenceDiagram
    participant B as Browser
    participant CP as Control plane
    participant K as Kubernetes
    participant P as Gateway pod

    B->>CP: GET / (session cookie)
    alt pod asleep
        CP->>K: scale oc-<id> to 1
        CP-->>B: "Waking…" interstitial (polls /__oc-ready)
        Note over P: cold start ~15–90s
    end
    B->>CP: reload once ready
    CP->>P: reverse-proxy HTTP+WS (inject Authorization: Bearer <token>)
    Note over CP,P: HTML is rewritten to seed the gateway token into<br/>localStorage so the Control UI auto-connects (CSP-safe)
    P-->>B: OpenClaw Control UI
```

---

## 4. Cron with scale-to-zero (wake-before-slot)

The **in-pod scheduler** does the firing (no double-fire, no CLI scope issues).
The control plane just ensures the pod is **running before each slot**.

```mermaid
sequenceDiagram
    participant CP as Control plane (cron waker)
    participant K as Kubernetes
    participant P as Gateway pod (in-pod scheduler)

    Note over P: while warm, the in-pod scheduler fires jobs natively
    P-->>K: (on scale-down) control plane mirrors next-fire → openclaw.io/cron-next
    loop every CRON_TICK
        CP->>K: any sleeping pod with cron-next within CRON_WAKE_LEAD?
    end
    CP->>K: scale to 1 (set openclaw.io/busy=1)
    Note over P: ready before the slot (lead > cold start)
    P->>P: in-pod scheduler fires the job at its slot
    CP->>P: refresh mirror → next occurrence; clear busy
    Note over CP,K: reaper sleeps it again (cron wake didn't touch last-activity)
```

Verified by `test/verify-cron.sh`. `CRON_WAKE_LEAD` (default 3m) must exceed cold
start so the pod is up before the slot (missed slots are not caught up).

---

## 5. Telegram with scale-to-zero (wake-on-webhook)

Telegram runs in **webhook mode**; Telegram delivers to the always-on control
plane, which wakes the pod and forwards the update.

```mermaid
sequenceDiagram
    participant TG as Telegram
    participant CF as Cloudflare → cloudflared
    participant CP as Control plane (/tg/<id>)
    participant K as Kubernetes
    participant P as Gateway pod (:8787)

    Note over CP,P: setup: user pastes bot token on /channels →<br/>CP writes channel config, restarts gateway → setWebhook(<tunnel>/tg/<id>)
    TG->>CF: POST update (X-Telegram-Bot-Api-Secret-Token)
    CF->>CP: POST /tg/<id>
    CP->>CP: verify per-user secret
    CP->>K: wake pod (busy=1), wait ready
    CP->>P: forward POST → :8787/telegram-webhook (retry until listener up)
    P->>P: agent turn
    P-->>TG: reply (outbound Bot API call)
    CP-->>TG: 200
    Note over P: first DM from a new sender needs one-time<br/>pairing approval (surfaced on /channels)
```

Verified by `test/verify-telegram.sh`. Only webhook-style channels fit
scale-to-zero; persistent-connection channels (Discord, WhatsApp-web) would need an
always-on shim and are out of scope.

---

## 6. Signup / provisioning

```mermaid
sequenceDiagram
    participant B as Browser
    participant CP as Control plane
    participant K as Kubernetes

    B->>CP: POST /signup (email, password)
    CP->>CP: bcrypt hash; id = sha256(email)[:16]
    CP->>K: create Secret, PVC, Service, Deployment(replicas 0)
    Note over K: Deployment has an idempotent onboarding initContainer<br/>(paste shared key, set model, gateway.mode=local, allowed origins, disable device auth)
    CP-->>B: set session cookie → redirect to /#token=<gateway-token>
```

---

## Namespaces & key labels/annotations

| Thing | Where |
|---|---|
| Control plane, cloudflared, cookie-key secret | `oc-system` |
| Per-user pods/PVCs/secrets/services, shared Anthropic key | `oc-users` |
| Tenant identity | label `openclaw.io/user=<id>` (all `openclaw.io/managed-by=controlplane`) |
| Idle clock | annotation `openclaw.io/last-activity` |
| Work-in-flight guard (reaper skips) | annotation `openclaw.io/busy` |
| Mirrored next cron fire | annotation `openclaw.io/cron-next` |

See [`README.md`](../README.md) for setup, config knobs, and the cloud-portability
notes (HTTPS-required ingress, NetworkPolicy CNI, stable tunnel for prod).
