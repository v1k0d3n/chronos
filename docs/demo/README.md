# Chronos demo — the "Acme Storefront"

A presentation-ready, three-tier sample app for showing off Chronos end-to-end:
a website you can **roll out**, **break**, and **roll back** — with every change
attributed to *who did it* and revertable from the OpenShift console.

```
  ┌── web ──┐     ┌── api ──┐     ┌── db ───┐
  │ httpd   │ ──▶ │ catalog │ ──▶ │ postgres│
  │ +Route  │     │ +config │     │ +Secret │
  └─────────┘     └─────────┘     └─────────┘
   presentation     application       data
```

Everything lives in the **`demo-chronos`** namespace. The live website is served
from a **ConfigMap**, so changing it changes the site — and Chronos captures the
change with a clean before/after diff you can revert.

---

## Prerequisites (one-time)

1. **Chronos is installed** (operator + console plugin). See the repo root README.
2. **`demo-chronos` is in Chronos's watch scope.** Chronos only watches the
   namespaces in `CHRONOS_WATCH_NAMESPACES`. Add `demo-chronos`:

   ```sh
   # NOTE: use a patch, not `oc set env` — oc set env truncates comma lists!
   oc patch deploy chronos-controller-manager -n chronos-system --type=strategic \
     -p '{"spec":{"template":{"spec":{"containers":[{"name":"manager","env":[{"name":"CHRONOS_WATCH_NAMESPACES","value":"demo-chronos,<your,other,namespaces>"}]}]}}}}'
   ```

   *(Optional)* add a `ChronosConfig` override to tune noise for `demo-chronos` —
   not required to watch it. See [docs/OPERATIONS.md](../OPERATIONS.md).

---

## Setup

```sh
oc apply -k docs/demo                       # deploy the three tiers
oc rollout status deploy/web -n demo-chronos   # wait for the web tier
oc get route web -n demo-chronos -o jsonpath='https://{.spec.host}{"\n"}'
```

Open that URL — you should see the **Acme Storefront, v1.0** (blue theme).
Leave it open in a browser tab; you'll refresh it during the demo.

> Tip: run each demo step from a terminal beside the browser, and keep the
> Chronos timeline (**Observe ▸ Timeline**, Project: `demo-chronos`) in a third
> tab. Three tabs: **the site**, **the terminal**, **Chronos**.

**Note:** the `web` pod serves the ConfigMap from the httpd DocumentRoot
`/var/www/html` — verified working with `ubi9/httpd-24`, no change needed. Do a
full dry run before presenting so the site and timeline are warm.

---

## The demo (≈ 6 minutes)

### Act 0 — "Here's production" (15s)
- Show the storefront tab (**v1.0**, blue). "This is our live site, running on
  OpenShift. Three tiers — web, API, database."

### Act 1 — The timeline & verified attribution (60s)
- Switch to **Chronos ▸ Timeline**, Project **`demo-chronos`**.
- "Chronos recorded the whole deployment — every object, newest first."
- Click the **`Deployment/web`** create. In the drawer:
  > "It knows exactly **who** deployed this — a verified identity, source IP, and
  > client — not just *that* it happened."
- Scroll to the **`Secret/db-credentials`** row — it's tagged **redacted**.
  > "It captured the database Secret changing, but **never stored the value** —
  > you see *that* it changed, never *what* it is."
- Point at the **`RoleBinding`** — tagged **high** risk.
  > "RBAC changes are flagged high-risk automatically."

### Act 2 — The rollout (90s)
- "Marketing wants a summer-sale banner. Here's the change."

  ```sh
  oc apply -f docs/demo/web-content-v2.yaml -n demo-chronos
  oc rollout restart deploy/web -n demo-chronos
  oc rollout status deploy/web -n demo-chronos
  ```
- **Refresh the storefront tab** → the **v2.0 "Summer Sale"** (orange, 30% off).
  The site visibly changed.
- In **Chronos**: the new **`ConfigMap/web-content` update** is at the top.
  Click it → the bottom panel shows the **before/after diff** (v1 HTML → v2 HTML),
  attributed to you, verified.
  > "Every rollout is on the record — who, when, and exactly what changed."

### Act 3 — The rollback (the "wow", 90s)
- "The sale page shipped with a pricing bug. In a normal shop this is a scramble —
  find the old ConfigMap, hope someone has it. Watch this."
- In **Chronos**, on the `ConfigMap/web-content` change: the **From/To** selectors
  are already set to **v2 → v1**. Click **Revert to <the v1 time>** → confirm.
  > "Chronos re-applies the known-good version with server-side apply."
- Back in the terminal, push it to the pods and refresh:

  ```sh
  oc rollout restart deploy/web -n demo-chronos
  ```
- **Refresh the storefront** → back to **v1.0**. Crisis over.
- In **Chronos**: a **new** change appears — the revert itself, attributed to
  `chronos`. "The undo is auditable too. Nothing happens off the record."

### Act 4 — Secrets, safely (45s)
- "Security rotates the DB password."

  ```sh
  oc patch secret db-credentials -n demo-chronos --type=merge \
    -p '{"stringData":{"database-password":"rotated-still-not-real"}}'
  ```
- In **Chronos**: the **`Secret/db-credentials` update** shows a changed field —
  but the value is **redacted**.
  > "Chronos proves the password rotated without ever knowing it. Two-layer
  > safety: redacted at capture, and RBAC-gated on read."

### Act 5 — Deletes are covered too (optional, 30s)
- "What if someone deletes something?"

  ```sh
  oc delete configmap api-config -n demo-chronos   # (then re-apply after)
  ```
- In **Chronos**: the **delete** is captured *and attributed* (audit tells us who),
  with the object's last state snapshotted — so it's revertable (recreatable).
- Restore it: `oc apply -f docs/demo/api.yaml -n demo-chronos`

---

## Reset between runs

```sh
# back to v1 (if you left it on v2)
oc apply -k docs/demo -n demo-chronos
oc rollout restart deploy/web -n demo-chronos
```

## Teardown

```sh
oc delete -k docs/demo
# and remove demo-chronos from CHRONOS_WATCH_NAMESPACES (patch, as above)
```

---

## Why this lands (the pitch beneath the demo)

| Act | Chronos capability | The "so what" |
|-----|--------------------|----------------|
| 1 | Verified attribution | *Who* changed production — not a guess |
| 1 | Secret redaction | Forensics without a secrets honeypot |
| 2 | Change timeline + diff | Every rollout on the record, with the exact diff |
| 3 | Surgical revert | One-click, auditable rollback to a known-good state |
| 4 | Two-layer secret safety | Prove a rotation happened without seeing the value |
| 5 | Attributed deletes | Even removals are captured, attributed, and recreatable |

All of it **inside the OpenShift console** — no new tool, riding RBAC, server-side
apply, and the platform's own monitoring.
