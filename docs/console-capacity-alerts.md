# Capacity policy and Feishu alerts

The console can save a desired template for **future game processes** without restarting Nakama. Running processes retain their own room limit, CPU request/limit and policy revision. Saving does not drain a process, terminate a room, buy a VPS or promise that Kubernetes can schedule the new template.

## Install the matching components

Upgrade the plugin, console, control broker and status exporter from the same release. The first plugin upgrade needs the usual Nakama restart; subsequent policy saves do not. Keep the existing deployment configuration unchanged during this upgrade. The plugin preserves the immutable profile and transactionally adds the initial policy and each existing worker's exact original CPU quantities to the same state record.

After any policy update, do not downgrade to a plugin that predates persisted capacity policies: an older JSON writer can drop fields it does not understand and cannot reproduce the frozen worker template. Restore only a compatible binary/state combination after safe drain; keep private database backups.

The existing [restricted control broker](console-api.md#browser-actions-and-the-restricted-broker) adds one typed policy operation. Its upstream path is fixed; the console still never holds the Fleet admin token. Keep `source.allow_management=true` and the protected `control_socket` for policy writes. Without a socket, existing observation pages continue to work but the policy endpoint is unavailable.

To enable the optional alert engine, create its directory once and install the public template (monitoring enabled, notification delivery disabled) on the console host:

```sh
install -d -o fleet-console -g fleet-console -m 0700 /var/lib/fleet-console/operations
# Initial installation only: do not overwrite an existing operations file.
test -e /var/lib/fleet-console/operations/operations.json || \
  install -o fleet-console -g fleet-console -m 0600 deploy/console/operations.example.json \
  /var/lib/fleet-console/operations/operations.json
```

Set `source.operations_file` to `/var/lib/fleet-console/operations/operations.json` in the private console configuration. Use the updated systemd unit, whose only new writable path is that directory. Restart only `fleet-console` after installing its configuration/unit. If the file is absent, the process creates a default file with monitoring enabled and notification delivery disabled; its parent must already be owned by the service user and `0700`. An absent `operations_file` disables this capability entirely. A malformed, unsafe or already locked file prevents console startup rather than losing alert state.

The private file contains the webhook/signing secret **and** durable incident, cooldown, delivery and audit state. It is `0600`, written by atomic replacement, and protected by one process-held file lock. Back it up as a secret. Do not edit/replace it while the service is running: the writer detects external changes and refuses to overwrite them. Use the authenticated UI/API for live changes; stop the service before an operator file recovery. Alert thresholds and webhook updates through the API require no restart.

## CPU and room policy

`GET api/v1/policy` reports `desired`, each `effective_instances` entry, and `requires_replacement_count`. Revision 1 reflects the deployment baseline, including an old unlimited CPU limit shown as `0`. It does not silently invent a new cap. Saving requires an explicit room count and CPU budget.

New policies enforce CPU **request equal to limit**, 100–64000 millicores, and 1–512 rooms. For example, 1500/1500 requests and limits 1.5 CPU cores for the game container. This reserves the same scheduler budget it can use; it is not a promise of 1.5 physical dedicated cores or additional simulation threads. `simulation_workers` and `audit_workers` display actual process settings when reported; this policy does not change them. Memory, image, node selectors and `max_instances` remain deployment settings.

On a 2-core node with 1700m allocatable, a 1500m game container leaves only 200m **allocatable** for its Agones sidecar and other Pods. Node reservations were already deducted to obtain 1700m. Check all container requests before expecting it to fit. Two 1500m processes cannot fit on that node. Extra room capacity does not add compute; validate it with representative gameplay pressure and timing samples.

After saving, explicitly drain an old instance when appropriate. Drain stops new room admission and preserves the current match, reconnect reservations and pending results. Normal lifecycle cleanup removes the process after those finish. A replacement is created only when demand or the configured minimum requires it; it uses the new template. The static `max_instances` includes draining processes and can prevent a simultaneous replacement until a slot is free. There is no force-close-room or automatic rolling restart in this feature.

Concurrent updates use `expected_revision`; one wins and stale writers receive `409 policy_conflict`. An ambiguous network result should be resolved by reading the desired revision before retrying. The most recent 100 successful changes retain timestamp, administrator, before and after values in the same transactional Fleet state. Each requested worker freezes its own revision/resources, so provider creation retries cannot adopt a different template accidentally.

Changing the deployment image, region, compatibility version or other immutable profile settings still requires the documented new deployment identity. Do not edit the stored profile or delete history. A fresh deployment ID uses a new row in the same database; the old row remains but the console does not combine its history with the new row.

## Capacity evidence

`snapshot.capacity` and `api/v1/overview` show physical and allocatable game-node CPU, node reservations and desired CPU. They explicitly describe the namespace scope. Observed Pod counts are **not all cluster Pods**, and allocatable CPU is not currently unreserved CPU. Kubernetes is the final scheduling authority.

`physical_capacity_shortage` requires a Pending Pod whose scheduler condition is `Unschedulable` with `Insufficient cpu` or `Insufficient memory`. Permission errors, missing telemetry, image pull errors and a busy game process are not labeled physical shortage. Fix the resource request or add suitable capacity; retrying creation alone does not make unavailable capacity appear. Fleet creation timeout/backoff and `max_instances` still apply.

## Timing and alert rules

The process samples independently of browser refreshes every ten seconds. A rule must remain over its threshold for `hold_seconds` before firing. Healthy observations must persist for the same interval before recovery. Missing, stale or insufficient samples are unknown, never zero or a successful recovery. A sampling gap resets a pending hold. Incidents, cooldowns and pending recovery survive console restarts.

| Kind | Metric and units |
| --- | --- |
| `client_wait_high` | `client_presentation_to_ready_ms.p95`, milliseconds after the client reports visual playback finished until it can act again |
| `client_settlement_wait_high` | `client_presentation_to_settlement_ms.p95`, milliseconds after visual playback until the final settlement is received |
| `frame_p99_high` | Game process `frame_p99_ms`, milliseconds; a different phase from visual waiting |
| `physical_capacity_shortage` | Count of Pending Pods with the scheduler evidence above; threshold is zero |

Both client presentation metrics are client-reported observations, not trusted server execution time. They require a 60-second window, at least `min_samples`, fresh worker heartbeat and latest sample age within the window. No rule automatically changes policy, drains instances or creates cloud resources.

Additional diagnosis windows are `server_first_ack_to_ready_ms`, `server_first_ack_to_settlement_ms`, `server_last_ack_to_ready_ms`, `server_last_ack_to_settlement_ms`, `simulation_queue_ms` and `simulation_work_ms`. The first-ACK stage includes waiting for the other player; the last-ACK stage starts after both valid playback acknowledgements. Neither represents the complete rendered player experience. All eight windows provide `window_seconds=60`, `count`, `p50`, `p95`, `p99`, `max`, `last_sample_age_seconds`. With count zero, quantiles/age are null. Missing windows mean the game build does not report them.

## Feishu setup

The template enables local console monitoring and disables Feishu delivery (`enabled:true`, `feishu_enabled:false`). No destination is present and no notification is sent. Create a [Feishu custom bot](https://open.feishu.cn/document/client-docs/bot-v3/add-custom-bot) when ready. Only its `https://open.feishu.cn/open-apis/bot/v2/hook/…` URL is accepted. Lark domains, arbitrary destinations, redirects, query strings and ambient proxies are unsupported. Optional signing follows Feishu's [official timestamp and HMAC example](https://www.feishu.cn/content/7271149634339422210): the HMAC-SHA256 key is `timestamp + "\n" + secret`, the message is empty, and the result is Base64 encoded. HTTPS verification remains enabled.

Enter the webhook and optional signing secret through the SSH-only console, then enable **Feishu delivery**. Monitoring can remain enabled without a bot. The API returns only configured booleans; logs, audit, errors and API responses omit both secrets. Omit a secret field to retain its value, send an empty string only to clear it. `enabled` controls observation; `feishu_enabled` separately controls delivery. A disabled monitor sends nothing even if notification settings are retained. Protect the SSH browser session and private backups like other administrator credentials.

Notifications merge up to 20 due incidents per message. Each incident's reminders observe `cooldown_seconds`; failed attempts are spaced at least 30 seconds and return only `feishu_delivery_failed`. Recovery messages are sent only for incidents previously delivered. Provider downtime is retried, but exactly-once delivery is not guaranteed: a process crash after Feishu accepts a message and before local acknowledgement can cause a duplicate. Browser closure does not stop observation. The engine stores the most recent 200 transitions and 100 configuration audits; game logs retain their separate seven-day policy.

See [the API contract](console-api.md#capacity-and-alert-configuration) for ranges, CAS bodies and read-only CLI examples.
