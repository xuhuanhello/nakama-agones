# Deploy the regional node controller

Fleet Console submits node tasks to the **existing HTTPS Kubernetes API**. A fixed controller Pod on that region's K3s manager performs SSH only to the new worker. There is no manager SSH command channel, root HTTP/Unix broker, hostPath mount or additional public API. The initial implementation supports one cluster/region per Console onboarding configuration.

The controller is optional and independent of Nakama, Agones and the Console processes. Installing it does not restart Nakama or alter active games. Node joining makes schedulable capacity available; it does not override `max_instances`, capacity policy, resource requests or public UDP/firewall requirements. Creating a Kubernetes Node record alone cannot install a machine.

See [Console workflow](console-node-onboarding.md), [resource protocol](node-enrollment-protocol.md), and [manual node prerequisites](nodes.md).

## Preconditions

- Use the repository's pinned K3s/Agones baseline. The manifests use Kubernetes `v1` CRDs and ValidatingAdmissionPolicy, verified by server-side dry-run on K3s 1.35; this does not replace real installation or network acceptance.
- Inspect `sudo k3s secrets-encrypt status` on the manager. Set `secrets_encryption_verified:true` in the operator config only after confirming encryption is enabled and healthy. Enabling it on an existing server can require a K3s restart; do not assume every installation has it. See [K3s encryption](https://docs.k3s.io/security/secrets-encryption).
- Secret API deletion is not erasure of encrypted backups. Restrict backup access and ensure Kubernetes audit policy records Secret metadata rather than request/response bodies.
- The manager must have enough reserved capacity for a 50m CPU / 128Mi request (500m CPU / 256Mi limit). Game processes remain on game nodes. The controller needs access to the private Kubernetes API and the new VPS's public SSH port. Worker-to-manager, overlay and game UDP rules are the operator's responsibility.
- Maintain an **agent-only secure K3s token** (`K10…::node:…`), not the server/admin token. New workers use read-only private registry credentials and a verified registry CA.

## Build and render

Build from the same reviewed source revision as the Console. Publish the image through your registry and deploy its immutable digest:

```sh
docker build --platform linux/amd64 \
  --build-arg "PYTHON_IMAGE=$(cat deploy/enrollment/base-image.txt)" \
  --build-arg "VCS_REF=$(git rev-parse HEAD)" \
  -f deploy/enrollment/Dockerfile -t YOUR_REGISTRY/node-enrollment:YOUR_VERSION .
```

Copy `deploy/enrollment/config.example.json` into a private operator directory. Set the region/cluster IDs, private API address/CIDRs and protected Nakama/manager public and private IPs. No passwords, tokens or registry contents belong in this JSON. The public example intentionally leaves encryption verification and retirement disabled.

```sh
python3 scripts/render_node_enrollment.py \
  --config /private/operator/controller.json \
  --image YOUR_REGISTRY/node-enrollment@sha256:IMAGE_DIGEST \
  --output /private/operator/rendered-controller
```

For separately approved offline retirement, set `retirement_enabled:true` and explicitly add `--enable-retirement`. The renderer refuses a mutable image tag, unverified encryption, changed output profile or permissive output directory. Output files are `0600` in an owned `0700` directory. A repeated identical render is safe; use a new directory for changed revisions. The standard renderer fixes resource namespaces and mount paths. It does not connect to any machine, create credentials or deploy anything.

The two namespaces are `fleet-enrollment-system` (controller and fixed bootstrap credentials) and `fleet-enrollment-requests` (CR tasks and temporary SSH Secrets). Requests have object quotas and `pods:0`. The controller runs UID/GID 10001, drops all capabilities, uses a read-only root filesystem and memory-backed temporary files, and is pinned to an `amd64` control-role node in the selected cluster. Its manager taint toleration matches the supplied K3s installer. No root container or privileged workload is needed.

The generated NetworkPolicy has no ingress. It permits API/DNS traffic and TCP to public worker addresses; custom SSH ports require the public TCP port range rather than a port-22-only rule. This is not an SSH protocol firewall. The controller itself restricts destinations and sends only the reviewed fixed probe/installer.

## Apply in stages

Review rendered files and use a trusted administrative kubeconfig. Apply namespaces first; namespace-scoped dry-runs need those namespaces to exist. Apply CRDs and wait for `Established`, then RBAC and Secret admission. The submission identity cannot create Pods/Jobs, read Secrets, write Nodes or grant permissions.

The retirement files are deliberately separate. Apply and verify `30-retirement-admission.yaml` **before** granting `31-retirement-rbac.yaml`. Confirm the policy's cluster ID, type-check status, binding and negative authorization cases on this cluster. Do not grant Node delete permission if admission setup fails. RBAC itself cannot express label-restricted Nodes deletion; the additional admission policy enforces that boundary for the controller identity.

Before starting the Pod, create `enrollment-bootstrap` in `fleet-enrollment-system` from private files:

```sh
kubectl -n fleet-enrollment-system create secret generic enrollment-bootstrap \
  --from-file=agent.token=/private/operator/agent.token \
  --from-file=registry.json=/private/operator/registry.json \
  --from-file=registry-ca.crt=/private/operator/registry-ca.crt
```

Use the operator's real private files. Both registry files may be omitted only when workers need no private registry. The pair is mandatory when either is supplied. Do not commit generated Secret YAML or expose these values in command arguments. Mounts are read-only; the controller validates/copies them into owner-only temporary files for the fixed installer. Provide an image-pull Secret to the Deployment if the controller image itself is private; the Console must not get access to that Secret.

Apply `40-controller.yaml` and `50-network-policy.yaml`. Check the Deployment, Lease and readiness. The controller's projected Kubernetes token rotates automatically; it reads that token for every request, validates the CA, ignores environment proxies, and never follows redirects. A pre-created Lease admits one active controller; losing it prevents further tasks or mutations. Restarted uncertain installations require manual review.

Only fixed paths from the private configuration and the reviewed image are used. `node_onboarding.py` is now an internal installer module; it cannot start the removed root/socket service. Do not install old `fleet-node-onboarding.service` templates.

## External Console identity and rotation

Issue a **separate** short-lived identity:

`system:serviceaccount:fleet-enrollment-system:fleet-enrollment-submitter`

The controller identity is different: `fleet-enrollment-system/fleet-node-enrollment`. Do not reuse the runtime, observer, or administrator token for submission.

Reuse [the issuer](credentials.md) and [the bounded token synchronization service](token-sync.md), with separate unit names, a dedicated delivery key and output directory. Example issuer arguments:

```sh
python3 scripts/issue_nakama_token.py \
  --kubeconfig /etc/rancher/k3s/k3s.yaml \
  --api-url https://MANAGER_PRIVATE_IP:6443 \
  --namespace fleet-enrollment-system \
  --service-account fleet-enrollment-submitter \
  --output /var/lib/nakama-agones/token-export/enrollment.json
```

Use the existing one-hour TokenRequest, renew on the manager every 15 minutes and fetch every five minutes. `deploy/enrollment/submitter-token-sync.env.example` supplies the distinct non-secret identity/path settings. A duplicated receiver unit must run as `fleet-console` and change both `--output-dir` and `ReadWritePaths` to `/var/lib/fleet-console/enrollment-credentials`; this directory is service-owned `0700`, with `token` and metadata `0600`. Use an independent known-hosts file and dedicated, forced-command credential-delivery key. That key can read only the fixed token bundle; it does **not** perform node operations.

The Console configuration selects the cluster and submission token:

```json
"node_onboarding": {
  "region": "example-us",
  "cluster_id": "example-us",
  "api_url": "https://MANAGER_PRIVATE_IP:6443",
  "ca_file": "/etc/fleet-console/kubernetes-ca.crt",
  "token_file": "/var/lib/fleet-console/enrollment-credentials/token",
  "game_port_min": 20000,
  "game_port_max": 20999
}
```

Mount credential directories rather than individual token files if the Console is containerized, so atomic rotation is visible. The browser sees neither submission tokens nor bootstrap Secrets. The Console checks the expected JWT subject and time bounds to catch wrong-file configuration; the Kubernetes API performs signature validation and authorization. Restart only the Console when enabling/changing its configuration; routine token refresh does not require a restart.

## Acceptance and limits

First verify RBAC denial of Secret reads, Pod/Job creation, Nodes writes and unrelated namespaces for the submission identity. For the controller, validate that admission rejects deleting a live, foreign-cluster or control-plane Node. Do not test these denials against an unreviewed production target using a real delete.

A scan/preflight against an existing worker must refuse reinstallation and discard its temporary Secret. This checks the guard but is **not** successful fresh-node acceptance. A complete acceptance needs a new dedicated VPS, fingerprint verification, approved preflight, successful agent registration and a real UDP game on that node. `Ready` alone does not prove overlay DNS or game connectivity.

Permanent offline retirement removes only the Node API record after Fleet/identity/workload checks. Residual verified system DaemonSet Pods are reported and left to Kubernetes; game resources and other workloads block the action. For a dead machine with remaining GameServers or game Pods, first verify that no active allocation or unpersisted result remains, then follow a separately reviewed workload cleanup procedure. This version does not force-delete or automatically evict those resources. It does not delete the cloud VPS, uninstall an agent or prevent a machine that returns from registering again.

Verification performed for this implementation: isolated controller/core regression tests; Linux amd64 image import/bootstrap with UID 10001, no network, read-only filesystem and no Linux capabilities; and server-side schema/admission dry-run on the target K3s baseline. These checks do not claim a real new-VPS join or production retirement was executed.
