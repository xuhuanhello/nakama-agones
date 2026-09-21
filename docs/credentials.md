# External Nakama Kubernetes credentials

The plugin uses `agones-control/nakama-agones`. Apply `deploy/kubernetes/rbac.yaml` using the intended administrator kubeconfig. Its Role is confined to `agones-games`: GameServer get/list/create/delete and Secret get/create/update/delete. It cannot administer nodes, read all cluster secrets or change RBAC. Keep this namespace dedicated to this pool's trust boundary; Kubernetes does not enforce the provider's ownership labels as an RBAC restriction.

Use an API token issued with the [Kubernetes TokenRequest API](https://kubernetes.io/docs/tasks/configure-pod-container/configure-service-account/#manually-create-an-api-token-for-a-serviceaccount). The requested one-hour lifetime is checked against the returned expiry. Avoid a manually created, non-expiring ServiceAccount token Secret. Neither the K3s join token nor the administrator kubeconfig belongs on the external Nakama host.

## Issue and rotate

On the control node only, run:

```sh
install -d -m 700 /var/lib/nakama-agones/token-export
python3 scripts/issue_nakama_token.py \
  --kubeconfig /etc/rancher/k3s/k3s.yaml \
  --api-url https://10.0.0.2:6443 \
  --output /var/lib/nakama-agones/token-export/nakama.json
```

The helper requests `agones-control/nakama-agones` using the explicit API/kubeconfig, validates identity and expiry, then atomically replaces a root-owned `0600` bundle. It does not print or transfer the token. Errors preserve the previous bundle. The bundle fields are `schema`, `api_url`, `namespace`, `service_account`, `token`, `expires_at`.

The systemd service/timer examples refresh every 15 minutes. Install both scripts under the selected root-owned `/opt/nakama-agones/scripts/` path; edit only the non-secret API URL/path in the service example, remove its `.example` suffix, and create the output directory before enabling the timer. No secrets belong in a unit or command line. Observe actual issued lifetime and alert when refresh has failed or less than 15 minutes remain.

Use the [pinned SSH token receiver and installation steps](token-sync.md) to deliver that bundle to Nakama. Its dedicated SSH key has a forced command that can only read this one export, no PTY/forwarding/shell, and a source restriction for Nakama. The receiver validates the expected API URL, namespace, ServiceAccount and expiry, then atomically writes `token` and expiry metadata as `0600` files in a `0700` credential directory. Do not log the response/body. An operator still installs and configures both issuer and receiver units; running the issuer alone does not install an end-to-end refresh service.

Distribute the public Kubernetes CA once through that verified administrative channel and pin it. Nakama configuration points to the private URL and credential directory:

```text
AGONES_KUBERNETES_API_URL=https://10.0.0.2:6443
AGONES_KUBERNETES_TOKEN_FILE=/run/agones-kubernetes/token
AGONES_KUBERNETES_CA_FILE=/run/agones-kubernetes/ca.crt
```

Bind-mount the **directory**, read-only, into the runtime container. Mounting an individual file can keep the old inode after an atomic replacement. The Go provider reads the token file on each HTTP request, so rotation requires no Nakama restart. It loads the CA when the provider starts; changing the trusted CA needs a planned trust-bundle update/restart. Never disable certificate verification to work around rotation.

The initial root issuer keeps administrator credentials only on the control host. For an in-cluster issuer, `deploy/kubernetes/token-issuer-rbac.yaml` supplies an optional identity allowed to mint only this runtime ServiceAccount's token. Give that issuer a short-lived projected API token and trusted transport; do not grant the runtime permission to mint tokens itself. The optional manifest does not deploy an issuer workload.

## Acceptance and failure handling

Verify the runtime identity can list the dedicated GameServers and cannot list nodes, secrets in another namespace, or roles. Refresh once while Nakama is running, confirm the token file changes and a fresh provider request succeeds without a restart. Monitor both issue and delivery timestamps: a healthy issue timer with a broken transfer still expires on the Nakama host.

An expired/missing token must fail API calls; retain the old file on transient refresh failure and alert before expiry. Do not fall back to an administrator or long-lived credential. For compromise, remove the runtime RoleBinding first to stop access, then replace the ServiceAccount/receiver key as needed and issue a new credential through the verified channel. Restore only the scoped RoleBinding after recovery. Rotation must never be coupled to deleting live GameServers or the fleet database.

If a create operation encountered an expired/missing token or a permissions failure, inspect `creation_blocked_reason` after restoring access. That persisted safety block requires the authenticated `POST /agones/fleet/v1/admin/retry-creation` operation after verification; token replacement alone does not clear it. Normal rotation before expiry needs neither this recovery step nor a Nakama restart.
