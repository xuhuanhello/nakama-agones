# Game-node tasks from Fleet Console

Fleet Console submits namespaced Kubernetes tasks over the existing pinned HTTPS API. A non-root controller on the regional manager performs the SSH installation. Console node operations use no manager SSH command channel or root node daemon. The Console has no join or registry credential; optional restricted SSH delivery only refreshes its scoped API token. Instance policy/drain continues to use its separate restricted broker.

This first version binds one console to one region/cluster. It enrolls a new dedicated Debian/Ubuntu amd64 VPS using the pinned [node installer](nodes.md). It does not purchase VPSs, change cloud firewalls, overwrite existing Kubernetes installations or restart Nakama.

## Configure

Deploy the CRDs, dedicated service accounts, admission policies and regional controller from `deploy/enrollment/` using [the manager controller guide](node-enrollment-controller.md). The exact CRD, Secret and admission contracts are in [the task protocol](node-enrollment-protocol.md). Enable Kubernetes Secret encryption at rest and verify it before enabling submissions. Installation/retirement permissions belong to that controller, not the console observer.

Remove the old `node_onboarding_socket` field and disable the obsolete `fleet-node-onboarding` service. Keep any existing `source.control_socket` for capacity policy/drain. Set this top-level fragment in the private console configuration:

```json
"node_onboarding": {
  "region": "us-west",
  "cluster_id": "example-cluster",
  "api_url": "https://manager.example.internal:6443",
  "ca_file": "/etc/fleet-console/kubernetes-ca.crt",
  "token_file": "/var/lib/fleet-console/enrollment-credentials/token",
  "game_port_min": 20000,
  "game_port_max": 20999
}
```

Region/API/CA must match the same `source.regions` entry; cluster ID and UDP range must match the manager's enrollment configuration. The token must be a separate file/inode from the observer and Fleet credentials. Omit this object or set it to null to disable node actions. After configuration changes, restart only Fleet Console.

The submission identity is fixed to `system:serviceaccount:fleet-enrollment-system:fleet-enrollment-submitter`. Its Role is limited to `fleet-enrollment-requests`:

| Resource | Verbs |
| --- | --- |
| `nodeenrollments.infrastructure.nakama-agones.io` | get/list/watch/create/patch |
| `noderetirements.infrastructure.nakama-agones.io` | get/list/watch/create |
| Secrets | create/delete only; no read/list |

It has no Pod, exec or Node permissions. The observer stays read-only and supplies fresh Node/Fleet checks before a retirement is submitted.

Use the existing short-lived TokenRequest issuer/rotation infrastructure with `--namespace fleet-enrollment-system --service-account fleet-enrollment-submitter`, a separate export and destination. Request one hour; rotate before expiry. A privileged provisioning process installs the **raw token**, not the issuer bundle, by a same-directory atomic rename as a regular `0600` file owned by `fleet-console` in a private `0700` directory. Do not place an administrator kubeconfig, token, password or filled configuration in Git, command arguments or logs. The CA must be a verified regular file not writable by group/others.

Console rereads the token on every API operation; an atomic rotation needs no restart. Symlinks, multiple hard links, wrong owner or permissions fail closed. It defensively checks JWT subject, at least 30 seconds remaining and at most two hours from issued time to expiry. These claim checks prevent accidental credential substitution; **Kubernetes performs signature/audience validation**. HTTPS trusts only the configured CA. Redirects and ambient proxies are disabled, responses are bounded to 1 MiB, and individual API calls time out after four seconds.

## Enroll a VPS

1. Prepare the source-restricted firewalls in [nodes.md](nodes.md). The target must provide public IPv4, root password SSH, Python 3.9+, systemd, disabled swap, at least 2 vCPU, about 2 GiB RAM and 10 GiB free disk. Existing Docker is preserved.
2. Select the region and enter public IPv4/SSH port. Scan the Ed25519 fingerprint and compare it through a trusted channel. Scan sends no password.
3. Confirm that fingerprint and provide the password for read-only preflight. Review detected private interface/resources, checks and the installation plan.
4. Confirm join. The manager controller installs the pinned agent, sets owned game-node labels/reservations and checks identity/IPs/Ready. Repeated join for the same task returns its existing job. A timeout or controller interruption requires inspection; it does not silently reinstall.
5. Verify actual cross-node networking and a real game UDP session. Kubernetes Ready alone is not gameplay acceptance.

The temporary immutable Secret holds only the SSH password, is owned by that exact enrollment CR UID, and expires 600 seconds after creation. The controller deletes it on failure/expiry, or immediately after loading it for approved installation. Creation time is never extended by retry. Its short lifetime depends on the controller cleanup loop being operational. Passwords never enter CR status, browser storage or console responses/logs. SSH askpass files exist only in the controller's memory-backed private runtime directory and are removed after use. This is encrypted-at-rest Kubernetes storage, not a claim that passwords exist only in memory.

## Permanently retired offline nodes

This is an explicit operator action, not automatic cleanup of disconnected nodes. The form requires exact node name, a current Kubernetes status IP, and `permanent_retirement:true`. An operator IP annotation is display-only and cannot satisfy confirmation.

Console bypasses the UI cache and freshly checks the Node UID/revision, owned game labels, non-Ready condition, game Pods/GameServers, and a Fleet snapshot no older than 20 seconds. Active allocations, processing workers, incomplete inventory or unresolved worker placement block submission. The controller independently rechecks ownership/UID/IPs/NotReady and all node workloads. Only verified DaemonSet-owned Pods in its fixed system namespace allowlist may remain. It then uses UID/resourceVersion preconditions for **Node DELETE only**. It neither cordons nor force-deletes Pods, uninstalls K3s or deletes the cloud VPS. A late identity change or ambiguous result becomes blocked/manual review.

## HTTP contracts

These fixed routes use the SSH-only console listener. All GET routes below accept a browser session or the independent read-only `fcro_` token described in [console-api.md](console-api.md). POST requires the admin session, exact Origin, JSON and `X-CSRF-Token`; any Bearer header on a write is rejected. Tokens in queries, arbitrary resources, commands and SSH payloads are unsupported.

| Route under `/fleet-admin/api` | Contract |
| --- | --- |
| `GET /node-onboarding` | `enabled`, `transport:"kubernetes_api"`, one region, up to 100 projected jobs and `jobs_truncated` |
| `POST /node-onboarding/scan` | `{region,host,port}` → `200 {scan_id,host,port,region,fingerprints,expires_at}` or pending 202 |
| `POST /node-onboarding/preflight` | `{scan_id,fingerprint,username:"root",password}` → `200 {preflight_id,host,region,node_name,expires_at,can_join,private_ip,external_ip,interface,cpu_cores,memory_mib,disk_free_gib,existing_installation,checks,plan}` or pending 202 |
| `POST /node-onboarding/join` | `{preflight_id}` → existing/new job; the server binds approval to the observed preflight digest |
| `GET /node-onboarding/jobs?id=ID` | Projected enrollment job, optional safe `scan`/`preflight` once its current generation completes |
| `GET /node-retirement?region=REGION&node=NAME` | `{enabled,eligible,reason,region,identity:{name,uid,resource_version,internal_ips,external_ips}}` |
| `POST /node-retirement` | `{region,node_name,node_uid,resource_version,confirmed_node_name,confirmed_ip,permanent_retirement:true}` → 202 projected retirement job after another fresh identity check |
| `GET /node-retirement/jobs` | Up to 100 projected jobs and `jobs_truncated` |
| `GET /node-retirement/jobs?id=ID` | Projected retirement job, blockers and permitted system DaemonSet remnants |

IDs are 32 lowercase hex characters. No client can choose an API resource name. Scan/preflight wait at most four seconds for the controller; a 202 body includes `pending:true,id,phase,state` and safe context. Poll the same job instead of resubmitting a password. A current completed scan/preflight appears in `job.scan`/`job.preflight`. Lists sort the returned page by creation time; `jobs_truncated:true` means the first Kubernetes page is incomplete, not an empty/complete history. Retrieve a known ID directly when missing from that page.

Enrollment phases: `Scanning`, `AwaitingPreflight`, `Preflighting`, `AwaitingApproval`, `Installing`, `Verifying`, `Ready`, `Failed`, `NeedsReview`, `Expired`. Job states: `queued`, `installing`, `verifying`, `ready`, `failed`, `needs_review`, `expired`. Wait/approval phases use `queued`. Terminal failures never pretend to be a successful preflight.

Retirement phases: `Pending`, `Checking`, `Deleted`, `Blocked`, `NeedsReview`; states: `queued`, `checking`, `deleted`, `blocked`, `needs_review`. There is no cordon stage. Jobs include `id,region,node_name,node_uid,phase,state,stage,created_at,updated_at,error?` and optional `blockers`, `allowed_system_pods`, `blockers_truncated`. Each resource entry is `{kind,namespace,name,reason}`, capped at 200. It never includes raw Node, Secret, controller config or SSH output.

Lifecycle times are Unix seconds; task projection only trusts status whose `observedGeneration` matches the CR. Outdated status becomes pending. Errors use `{"error":"stable_code"}` without upstream response bodies. Common codes:

- `scan_expired`, `host_fingerprint_mismatch`, `preflight_expired`, `preflight_checks_failed`: start/review the appropriate step.
- `onboarding_state_changed`: a CAS write conflicted; reread before retrying. A timed-out Secret create is never adopted. The controller TTL cleans unknown/orphaned creates.
- `node_identity_changed`, `invalid_retirement_confirmation`: reread identity and obtain a new explicit confirmation.
- `node_still_ready`, `control_node_protected`, `node_not_owned_worker`, `node_readiness_unknown`, `node_has_pods`, `node_has_gameservers`, `node_has_processing_worker`, `node_has_active_allocations`, `worker_location_unavailable`: retirement is blocked.
- `node_inventory_incomplete`, `fleet_snapshot_stale`, `fleet_snapshot_invalid`: observation cannot safely authorize deletion.
- `node_onboarding_not_configured`, `node_onboarding_credential_unavailable`, `node_onboarding_access_denied`, `node_onboarding_unavailable`: check server-side configuration/rotation/controller/API, without distributing its token.

Controller-specific stable failure codes may be added. Keep unknown codes visible as text and inspect job blockers; never convert unknown state into success or an automatic retry.
