# kubectl-hwhealth

A small Go `kubectl` plugin that checks Linux software RAID status from `/proc/mdstat` and CPU/package temperatures from `/sys/class/thermal` on the machine where it runs.

## Scope and node identity

This is a **local-node scanner**, not a cluster-wide telemetry collector. It reads hardware from the local host and associates that data with exactly one Kubernetes node, matched by node name or the `kubernetes.io/hostname` label. If the match is missing or ambiguous, the reading is not eligible for automated remediation. Remote nodes are reported as unknown because this version does not authenticate remote telemetry. Run the scanner locally on each Linux node for per-node checks.

When the hostname does not match the Kubernetes node name, provide an explicit mapping:

```bash
kubectl hwhealth --node-name=k3s-worker-2
```

The explicit name is an operator assertion; verify it before enabling automated actions.

## Install

Download a release binary and its SHA-256 file from the [Releases page](https://github.com/JemBuildz/kubectl-hwhealth/releases/latest). For Linux amd64:

```bash
set -euo pipefail
curl -fsSLO https://github.com/JemBuildz/kubectl-hwhealth/releases/latest/download/kubectl-hwhealth-linux-amd64
curl -fsSLO https://github.com/JemBuildz/kubectl-hwhealth/releases/latest/download/checksums.txt
grep -F 'kubectl-hwhealth-linux-amd64' checksums.txt | sha256sum -c -
gh attestation verify kubectl-hwhealth-linux-amd64 --repo JemBuildz/kubectl-hwhealth --signer-workflow JemBuildz/kubectl-hwhealth/.github/workflows/release.yml
chmod +x kubectl-hwhealth-linux-amd64
sudo install -m 0755 kubectl-hwhealth-linux-amd64 /usr/local/bin/kubectl-hwhealth
```

The checksum detects accidental corruption. Since the checksum file is distributed with the binaries, it alone does **not** authenticate the publisher or protect against replacement of both files. The `gh attestation verify` command checks the binary’s GitHub artifact provenance against the expected repository; install GitHub CLI if it is not already available.

Run a read-only scan:

```bash
kubectl hwhealth
```

The scanner reports missing or unreadable sensor data as `UNKNOWN`; it does not substitute a fabricated temperature. Non-Linux hosts cannot read Linux RAID status and are not suitable for hardware scanning.

## Failure simulation

Simulation is always dry-run, regardless of license key. It does not run `kubectl` mutations or send webhook requests:

```bash
kubectl hwhealth --context=homelab --node-name=k3s-worker-2 --simulate-raid-fail --auto-drain
```

A simulated failure with no remediation flag only displays the simulated finding.

## Optional remediation

Live actions require a valid Ed25519-signed key and a binary built with the corresponding public key. The private signing key must stay offline and must never be included in the source, binary, workflow, or repository variables. The public key is not secret; configure it as the repository variable `HWHEALTH_ED25519_PUBKEY_HEX` before publishing a release. Until that variable is set, release builds intentionally fail rather than shipping a binary that cannot validate production keys.

The license check is only a commercial feature gate. Kubernetes authorization still comes from the kubeconfig/RBAC identity. Any requested webhook or remediation action requires an explicit `--context` so the target cluster is visible in the command. For example:

```bash
kubectl hwhealth --context=homelab --node-name=k3s-worker-2 --auto-cordon --license-key="$HWHEALTH_LICENSE_KEY"
```

Use a dedicated identity and grant only the permissions required for the selected operation. Read-only scanning does not require node patch permission. Cordon and drain are cluster-changing actions; do not grant broad cluster permissions to an unattended process without reviewing the effect.

`--auto-cordon` prevents new pods from being scheduled on the matched node; it does not evict existing pods. `--auto-drain` runs `kubectl drain --ignore-daemonsets --timeout=60s`, which evicts eligible pods and respects PodDisruptionBudgets. It deliberately does not pass `--delete-emptydir-data`; drain can fail when a pod uses local ephemeral data, and an operator must decide how to handle that case. A drain is disruptive and should be enabled only after validating the node mapping and workload disruption policies.

Webhook URLs are restricted to HTTPS Slack and Discord endpoints. DNS answers are checked at connection time and private/special-use addresses are rejected; redirects are disabled and HTTP status codes checked. Protect webhook URLs because they contain credentials.

## Development

Run tests and static checks with Go 1.23 or newer:

```bash
go test ./...
go vet ./...
```

The release workflow runs checks on pushes, pull requests, and release builds. It publishes Linux amd64 and arm64 binaries only. Build/test jobs have read-only repository permissions. Only the isolated release-publisher job receives release write permission. Workflow actions are pinned to full commit SHAs, and the publisher creates GitHub artifact attestations.
