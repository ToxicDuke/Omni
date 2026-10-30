# Routine Omni updates and CI/CD

A published stable `v*` release follows one pipeline: CI → configuration and
artifact checks → live canary → manual fleet approval → remaining approved nodes.
Ordinary pushes and pull requests do not deploy nodes. GitHub environments,
secrets, deployment variables and approved target inventory must be configured
before the first run;
the workflow does not create or populate them.

This runbook is for ordinary updates to running Omni nodes. There is no separate
staging or test node. The canary, `root@31.130.204.250:2222`, is a live production
node: its agent is updated and restarted before fleet approval. Treat the canary
step as a live change, not an isolated test.

## Workflows and release identity

- [push.yaml](../../.github/workflows/push.yaml) runs on `develop` pushes, pull
  requests and manual requests, and is reusable through `workflow_call`. It
  retains the Go vulnerability scan, tests, race tests and Linux `amd64`/`arm64`
  builds with the pinned Go toolchain.
  Tests and race checks run on the native amd64 runner; arm64 is cross-compiled,
  not executed on ARM hardware by this workflow.
- [deploy.yaml](../../.github/workflows/deploy.yaml) runs on `release: published`
  for stable `v*` tags. Draft releases, prereleases and ordinary branch pushes
  do not start deployment.
- [docker.yaml](../../.github/workflows/docker.yaml) remains a separate image
  publishing pipeline. Publishing an image does not update node services through
  this binary rollout.

CI produces `omni_linux_amd64` and `omni_linux_arm64` workflow artifacts, each
containing `omni`, `SHA256SUMS` and `REVISION`. Preflight binds both artifacts to
IDs and digests from the same workflow run. Canary and fleet use those artifacts
without rebuilding. Every deployment job rechecks environment protection and
artifact identity; download digest mismatches are fatal, and the binary checksum
and recorded revision are checked again before installation. `REVISION` records
the full source commit SHA. Release binaries embed the validated tag without the
leading `v`; ordinary branch builds use `dev-<short-SHA>`.

Actions are pinned to full commit SHAs. Review those pins when upgrading actions;
a version tag alone is not an immutable reference. GitHub documents the
[security implications of action pinning](https://docs.github.com/en/actions/reference/security/secure-use).
Its general [artifact validation guide](https://docs.github.com/en/actions/tutorials/store-and-share-data)
describes warning-only digest handling; this workflow requires an error on a
mismatch in addition to its explicit file checks.

## One-time GitHub setup

An administrator must create **both** environments before running deployment:

| Environment | Deployment refs | Approval | Environment secrets | Environment variables |
| --- | --- | --- | --- | --- |
| `omni-canary` | Selected **Tags**: `v*` | No production approval gate | `SSH_PRIVATE_KEY`, `SSH_KNOWN_HOSTS` | `OMNI_CANARY_API_URL` |
| `omni-production` | Selected **Tags**: `v*` | At least one configured required reviewer must approve | `SSH_PRIVATE_KEY`, `SSH_KNOWN_HOSTS` | `OMNI_DEPLOY_NODES` |

1. In repository **Settings → Environments**, create the exact names above.
2. Select **Selected branches and tags**, then add a **Tag** rule for `v*` to
   each environment. Remove other branch/tag rules: preflight requires only the
   `v*` tag policy. A branch rule with the same text is a different rule.
3. Configure named maintainers or teams under **Required reviewers** for
   `omni-production`. Preventing self-review is optional; enable it when another
   maintainer can approve. It is not a mandatory two-person rule in this pipeline.
   Decide whether administrators may bypass protection under your release policy.
4. Add each secret and variable to its environment, using the table below.
5. Protect `develop` and release tag creation, update and deletion with repository
   rules. Require review of workflows and deployment scripts by the responsible
   code owners. Restrict who can publish releases. The `v*` pattern controls names;
   it does not make a tag, its author or its workflow trustworthy.

Do not select **Protected branches only** as a substitute for the tag rule: if no
branch protection exists, that setting permits all branches. Environment rules
match the workflow's `GITHUB_REF`, not an arbitrary input containing a tag name.
This pipeline uses the release tag ref. See GitHub's
[deployment environment rules](https://docs.github.com/en/actions/reference/workflows-and-actions/deployments-and-environments).

The repository is public, where required reviewers are available on current
GitHub plans. For private repositories, Pro/Team support environments and secrets,
but required reviewers are not available on those plans; private use of this
approval mechanism requires an appropriate Enterprise plan. Recheck capabilities
before changing visibility. GitHub can automatically create an unprotected
missing environment when a workflow references it; this pipeline first validates
the existing fixed environments and only then passes their names to deployment
jobs. Missing environments or missing production reviewers stop preflight. See
[GitHub's environment setup and availability documentation](https://docs.github.com/en/actions/how-tos/deploy/configure-and-manage-deployments/manage-environments).

### Secrets and variables

| Name | Value and scope |
| --- | --- |
| `SSH_PRIVATE_KEY` | Operator-provided deployment key, configured separately in each environment. Use a key authorized for the corresponding targets and any required jump host. The workflow does not discover or upload a workstation key. |
| `SSH_KNOWN_HOSTS` | Verified OpenSSH host-key entries for that environment's targets and jump hosts. Nondefault-port entries use `[host]:2222`. Verify fingerprints through an already trusted channel before saving them. |
| `OMNI_CANARY_API_URL` | In `omni-canary`: the canary's HTTP(S) URL ending exactly in `/api/system`, matching its local bind port and TLS configuration. HTTPS requires a hostname covered by its valid certificate. |
| `OMNI_DEPLOY_NODES` | In `omni-production`: an explicit JSON array of approved fleet nodes, each with `host`, `user` and `api_url`. `host` must be an IPv4 address; the SSH port is fixed at `2222`. |

Keep deployment keys out of repository files, workflow inputs, logs and artifacts.
Do not automatically copy the local `mikasahost` key into GitHub. Runtime SSH uses
strict host-key verification; an unexpected host key must fail rather than being
accepted by a first-use prompt or an unverified scan.

The canary is fixed at `root@31.130.204.250:2222`. It is not selected by inventory.
The production inventory is an explicit approval boundary: the list of machines
an operator can SSH into is **not** an Omni fleet inventory, and nodes are not
automatically discovered.

For example, replace every documentation placeholder with an approved target:

```json
[
  {
    "host": "203.0.113.10",
    "user": "omni-deploy",
    "api_url": "https://node-1.example.com/api/system"
  }
]
```

`203.0.113.10` and `example.com` above are documentation placeholders, not deployment
targets. The deployment code always uses `root@31.130.204.250:2222` as ProxyJump
for `5.166.219.51` through `5.166.219.56`, and for `95.79.98.110`. Their environment
must trust both the jump host and the target host keys. Inventory does not accept
arbitrary SSH options or override the fixed port/jump policy.

## Node prerequisites

The pipeline supports the existing systemd installation only:

- `omni.service` is already active and uses the existing `/usr/local/bin/omni`;
  the workflow does not adopt a different service or binary layout.
- Python 3.8 or newer, `curl` and the Docker CLI are installed on the node.
- The configured SSH account can run the installation and checks as root, either
  directly or using noninteractive, passwordless `sudo`.
- The required `api_url` ends exactly in `/api/system` and matches the node
  listener's scheme and port; that listener must be reachable on
  `127.0.0.1:<port>`. HTTP is supported for existing SSL-disabled nodes.
  HTTPS needs a hostname covered by the node's valid certificate; do not use
  `https://localhost` unless the certificate covers `localhost`.
- Running game containers carry the existing labels `Service=Pterodactyl` and
  `ContainerType=server_process`, which define the container guard's scope.

The probe resolves the configured API hostname to `127.0.0.1` on the node with
`curl --resolve` and bypasses proxies: it does not call an external API endpoint.
For HTTPS it preserves full certificate and hostname verification, without `-k`.
It requires the expected unauthenticated `401` response, a Bearer challenge and
an `X-Request-Id`. It does not need the node's Panel token.

## Release and approval

1. Capture the canary baseline: installed version/revision, service state, running
   game servers and their normal console/metrics behavior. Note any servers marked
   suspended in the Panel. Use the reviewed release process to publish a stable
   `v*` release from an approved commit. CI must pass before deployment proceeds.
2. Let preflight validate environments, tag restrictions, reviewers and immutable
   artifact identities. Correct missing setup rather than creating an unprotected
   replacement environment or bypassing a failed check.
3. Canary automatically updates and restarts the agent on the live node
   `root@31.130.204.250:2222`. It checks the service PID, installed binary hash and
   local API readiness: three successful checks with the same PID, waiting five
   seconds between checks, within a 300-second readiness deadline.
4. Before approving the remaining fleet, inspect the live canary through the
   Panel: open its server console and statistics. Confirm normal operation and
   expected metrics; match the deployed version, revision and checksum to this
   release/run, using its job summary and per-node binary-hash log. Compare with
   the captured baseline and investigate any changed server state.
   The local automated probe does not replace these authenticated checks.
5. In the same workflow run, open **Review deployments**, select
   `omni-production`, and approve only after the canary evidence is satisfactory.
   Reject the deployment if it is not.
6. Fleet installation runs sequentially against `OMNI_DEPLOY_NODES`, using the
   same release artifacts and checks for each target. The first failure stops
   subsequent targets. Inspect the workflow result before declaring the rollout
   complete.

## Installation, failure and rollback

[deploy/omni.py](../../deploy/omni.py) uses Python's standard library and native SSH;
there is no Ansible requirement. It verifies the artifact, backs up the previous
binary, replaces `/usr/local/bin/omni` atomically and restarts only `omni.service`.
The installer does not issue Docker-daemon restarts or game-container lifecycle
commands, or replace node configuration and server data. Restarting the agent
can still affect games: [startup synchronization](../../cmd/root.go) refreshes
running servers from the Panel, and [environment synchronization](../../server/update.go)
applies limits and can stop a running server marked suspended. The guard detects
resulting changes; it cannot prevent them or guarantee uninterrupted games.

The installer captures the initially running game containers, then checks those
baseline IDs, running state, `StartedAt` and `RestartCount` after agent readiness.
Newly created containers are outside that baseline. Agent readiness needs three
consecutive checks with the same PID and matching binary/API, with a 300-second
deadline; it is not a five-minute soak test. An intentional lifecycle change to
a baseline game can also fail the comparison; investigate rather than bypassing
the guard.

On installation or health failure, the installer restores the previous binary,
restarts Omni and checks agent recovery. The job still fails and the remaining
fleet is not updated. A failed recovery requires operator intervention; rollback is
not reported as a successful deployment. Already updated nodes remain at their
installed version, so inspect the resulting mixed-version fleet before retrying.
Restoring the old agent binary does not automatically restart a game stopped by
startup synchronization or restore its previous runtime state. Compare the
baseline and review recovery with the operator, respecting the Panel's intended
suspension state; the installer does not attempt game-container recovery.

Automatic rollback requires the installer to remain running: it cannot execute
after `SIGKILL`, power loss or a host outage. A backup may remain for manual
recovery. If a rollback restoration fails, the helper preserves the old backup
where possible and reports its actual path in the Actions log; use that
diagnostic for operator recovery rather than assuming a successful rollback.

Binary rollback does not undo application data changes or establish that every
future version supports a downgrade. Keep your normal node-state backups and
canary review; the workflow intentionally leaves configuration and game data
alone. Runtime compatibility, public routing, authenticated Panel callbacks and
actual game sessions require operator verification on the canary.

## Verification status

Run the repository's workflow-contract checks and installation tests with local
test doubles before first use. This implementation has not connected to production
nodes, configured GitHub environments/secrets, uploaded private keys or performed
a rollout. Run the first real deployment only after the setup and canary checks
above have been completed by the operator.
