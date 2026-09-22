# Regional node task API

The Kubernetes API serves `infrastructure.nakama-agones.io/v1alpha1` in the fixed `fleet-enrollment-requests` namespace. Each regional cluster has its own controller, credentials and task history. The initial Console configuration selects one cluster/region. The browser never receives Kubernetes tokens or submits arbitrary Kubernetes objects.

## Enrollment

Resource: `nodeenrollments`, kind `NodeEnrollment`, name `enroll-<32 lowercase hex characters>`. The Console-generated ID remains the `scan_id`, `preflight_id` and job `id` throughout the flow.

```json
{
  "apiVersion": "infrastructure.nakama-agones.io/v1alpha1",
  "kind": "NodeEnrollment",
  "metadata": {"name": "enroll-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "namespace": "fleet-enrollment-requests"},
  "spec": {"region": "example-us", "host": "PUBLIC_IPV4", "port": 22, "action": "Scan"}
}
```

`spec.region`, `host` and `port` are immutable. Supported actions advance only `Scan → Preflight → Join`. Creating a Join resource directly cannot bypass the controller's verified scan/preflight states. CR status uses its own subresource: the submission identity cannot write it.

1. `Scan`: the controller obtains an Ed25519 fingerprint without a password. `status.scan` contains the existing UI response (`scan_id`, `host`, `port`, `region`, Unix `expires_at`, `fingerprints`). `status.sshHostKey` is the public known-hosts line used internally; the Console omits it from UI responses.
2. After independently verifying the fingerprint, the Console creates `ssh-<id>`: an immutable `Opaque` Secret with only `data.password` (base64 UTF-8, decoded size 1–1024 bytes). Label `nakama-agones.io/enrollment-id=<id>`. Exactly one owner reference identifies the CR's apiVersion, kind, name and **UID**, with `controller:false`, `blockOwnerDeletion:false`.
3. Using the current CR `resourceVersion`, the Console patches `action:"Preflight"`, `hostFingerprint`, and `credentialSecretRef:{name,uid}`. The controller checks the Secret UID, owner UID, shape and creation-time TTL, then runs only the fixed read-only probe.
4. `status.preflight` contains `preflight_id`, Unix `expires_at`, `host`, `region`, `node_name`, `private_ip`, `external_ip`, `interface`, CPU/memory/disk observations, `checks`, `can_join`, and `plan`. `status.preflightDigest` is a SHA-256 digest of the complete canonical preflight view. Failed checks never authorize installation.
5. After user confirmation, a CAS patch sets `action:"Join"` and `approvedPreflightDigest`. The controller verifies expiry/digest and rechecks the machine before installation. A durable `Installing` status is committed before SSH starts. The temporary Secret is deleted with its UID precondition before the installation; the active operation holds its password only in memory and short-lived private tmpfs askpass files.

The maximum pending credential/preflight lifetime is ten minutes from Secret creation, including time spent waiting. Attempts do not extend it. Scans expire after ten minutes. Each controller manages one region and allows one active machine installation at a time. CR history lasts seven days after a terminal state; quotas bound retained requests and Secrets. Terminal/error/expiry handling and periodic/startup cleanup delete only matching owned Secrets. Backup copies may outlive API deletion; see [encryption requirements](console-node-onboarding.md).

Phases: `Scanning`, `AwaitingPreflight`, `Preflighting`, `AwaitingApproval`, `Installing`, `Verifying`, `Ready`, `Failed`, `NeedsReview`, `Expired`. `status.observedGeneration` and RFC3339 `updatedAt` identify the last processed revision. `error` is a stable safe code. `status.job` keeps the UI fields `id`, `host`, `region`, `node_name`, `state`, `stage`, Unix `created_at`/`updated_at`, optional `checks` and `error`; states are `installing`, `verifying`, `ready`, `failed`, `needs_review`, `expired`.

A restart with `Installing`/`Verifying`, or an ambiguous installation result, produces `NeedsReview`; it does not repeat SSH installation. Restart during preflight requires a new scan. Repeated Join confirmation cannot create another install. Losing the controller Lease fences new probes, state writes and deletion operations; an already running remote command may have completed, so its result requires inspection.

The existing Console `/fleet-admin/api/node-onboarding` scan/preflight/join/jobs routes are projections of these resources. The asynchronous task may outlive a browser request; a pending response is not an installation failure. No request accepts a command, image, kubeconfig, arbitrary API path, registry credentials or manager SSH credentials.

## Permanent offline retirement

Resource: `noderetirements`, kind `NodeRetirement`, name `retire-<32 lowercase hex characters>`. The immutable spec contains:

```json
{
  "region": "example-us",
  "nodeName": "game-example",
  "nodeUID": "UID_FROM_FRESH_NODE_READ",
  "internalIPs": ["10.0.0.5"],
  "externalIPs": ["PUBLIC_IPV4"],
  "confirmedNodeName": "game-example",
  "confirmedIP": "PUBLIC_IPV4",
  "permanentRetirement": true
}
```

The Console checks current Fleet allocations and resource observations before submission. The controller independently requires matching name, UID, complete IP sets and cluster ownership; labels `nakama-agones.io/game-node=true` and `nakama-agones.io/role=game`; and an explicit Ready condition of `False` or `Unknown`. Control-plane/master nodes and the controller's own node are rejected.

Any game Pod, associated GameServer or other workload blocks deletion. A residual Pod is allowed only when its namespace is in the configured system allowlist, it has one controlling `apps/v1` DaemonSet owner, and that live DaemonSet has the same UID. Allowed residual system agents are reported; the controller does not force-delete them. Default namespaces: `kube-system`, `agones-system`, `agones-observability`.

After rechecking identity, the controller deletes only the **Node API object**, with UID and resourceVersion preconditions. It never cordons, evicts, deletes Pods/GameServers, SSH-uninstalls, buys/deletes a VPS or changes cloud firewalls. Revision/identity changes or uncertain results require a new review. There is no automatic retirement triggered by an offline alert. The operator must ensure the machine will not return; its agent could otherwise register again.

Phases: `Pending`, `Checking`, `Deleted`, `Blocked`, `NeedsReview`. `status.job` contains `id`, `node_name`, `node_uid`, `region`, `state`, `stage`, timestamps and optional `error`. `status.blockers` and `allowedSystemPods` are bounded arrays of `{kind,namespace,name,reason}`; `blockersTruncated` marks an incomplete blocker list. The upper bound is 200 entries per list. History retention is seven days.

RBAC grants the controller cluster-scoped `nodes/delete` because RBAC cannot select Nodes by label. A separate fail-closed ValidatingAdmissionPolicy restricts **this controller identity** to owned, offline game nodes in the configured cluster; ordinary administrators are unaffected. The policy does not make Pod inventory and Node deletion a multi-object transaction. This API is for permanently offline machines, after Fleet/workload review, not online draining or force cleanup.

## Failure handling

Errors and status exclude SSH output, API error bodies, passwords and tokens. Examples: `preflight_expired`, `credential_identity_mismatch`, `preflight_approval_mismatch`, `controller_interrupted_check_node`, `node_identity_or_ownership_mismatch`, `node_must_be_offline`, `node_has_remaining_workloads`, `node_changed_reconfirm_retirement`. Kubernetes API conflicts use normal CAS/reconciliation; an unverified Secret-create result is not blindly adopted.

CRDs, `/status`, validation and controller reconciliation use standard Kubernetes APIs. See [custom resources](https://kubernetes.io/docs/concepts/extend-kubernetes/api-extension/custom-resources/) and [resource-version updates](https://kubernetes.io/docs/reference/using-api/api-concepts/#updates-to-existing-resources). Creating a Node object alone does not install an agent; K3s must run on the actual machine and register successfully. See [Nodes](https://kubernetes.io/docs/concepts/architecture/nodes/#management) and [K3s agents](https://docs.k3s.io/quick-start).
