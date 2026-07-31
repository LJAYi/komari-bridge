# komari-bridge

`komari-bridge` is an experimental companion for
[Komari](https://github.com/komari-monitor/komari). It discovers resources that
the server and its official
[Komari Agent](https://github.com/komari-monitor/komari-agent) do not model,
including hypervisor guests, WSL distributions, and Slurm state.

This is an independent community project. It is not affiliated with or
endorsed by the Komari maintainers. Features that require first-class server
support are intended to be proposed upstream after validation here.

## Features

- Proxmox VE node, QEMU VM, and LXC discovery
- Durable SQLite mappings between provider identities and Komari clients
- Komari automatic registration and HTTP v2 JSON-RPC reporting
- Guest OS discovery through the QEMU Guest Agent
- Optional Linux guest memory from `/proc/meminfo` through a fixed QGA command
- Optional agentless SSH fallback for hosts that cannot run Komari Agent
- Slurm extension collection without registering a duplicate host client
- Per-user WSL discovery using stable registry GUIDs
- Running-only WSL collection, which does not start stopped distributions
- Provider merging so hypervisor topology and guest metrics share one client
- Read-only Slurm extension API with optional bearer authentication
- Multiple Proxmox sources, including sites with overlapping private subnets

The bridge only uses Komari's `agent.basicInfo` and `agent.report` methods. It
does not implement the Agent's terminal, task, ping, pull, or update features.
Komari Agent remains the authoritative host collector whenever it can be
installed. The bridge will not grow into a second general-purpose Agent.

## Quick start

Go 1.25 or newer is required.

```bash
cp config.example.yaml config.yaml
export KOMARI_AUTODISCOVERY_KEY='replace-me'
export PVE_SITE_A_TOKEN='replace-me'
export BRIDGE_API_KEY='replace-me'
go run ./cmd/komari-bridge -config config.yaml -once
```

Remove `-once` after checking the generated clients. The interval must stay
below Komari's 35-second HTTP presence TTL; 20 seconds is recommended.

To build a container:

```bash
docker build -t komari-bridge:local .
docker run --rm \
  -v "$PWD/config.yaml:/app/config.yaml:ro" \
  -v "$PWD/data:/app/data" \
  komari-bridge:local
```

See [`config.example.yaml`](config.example.yaml) for all provider types.

## Project boundary

```text
Komari Agent                 komari-bridge
------------------------     ---------------------------------
Host CPU/RAM/disk/network    Proxmox node, VM, and LXC discovery
Host OS and virtualization   Windows to WSL discovery
NVIDIA and AMD GPU metrics   Slurm extension metrics
Terminal, tasks, and ping    Agentless SSH only as a fallback
```

Docker, SNMP, and Proxmox storage discovery are possible future additions, but
only as child-resource or infrastructure discovery. General host collectors,
GPU backends, remote tasks, and terminal features are explicitly out of scope.

## Resource identity and current merging

Resources use a stable provider identity:

```text
(source_type, source_id, external_id)
```

For example, a PVE VM may be discovered as `qemu:105`. The optional agentless
SSH fallback can attach to that identity and replace hypervisor-observed guest
metrics without creating a duplicate virtual client. This is
provider-to-provider merging inside the bridge; it cannot bind a resource to
an independently registered Komari Agent UUID.

Do not configure the bridge and an Agent to submit complete host reports with
the same client token. Safe Agent binding requires upstream topology and
extension-metric APIs. See
[`docs/rfcs/0001-resource-binding.md`](docs/rfcs/0001-resource-binding.md).

Komari prepends `Auto-` during automatic registration. Until upstream topology
fields exist, the bridge registers names as:

```text
Auto-<group> / <resource name>
```

## Proxmox permissions

Use a dedicated PVE account and API token with `PVEAuditor` access. Never reuse
an administrator token.

```yaml
token_id: komari-bridge@pve!monitor
token_secret: ${PVE_TOKEN}
```

PVE's VM `mem` metric can describe QEMU process memory rather than guest-usable
memory. For Linux guests with QGA installed, opt into the fixed
`/bin/cat /proc/meminfo` collector:

```yaml
resources:
  "qemu:100":
    guest_memory: qga
```

The bridge then reports `MemTotal - MemAvailable`. PVE classifies guest command
execution as `VM.GuestAgent.Unrestricted`; scope that permission to only the
configured VM paths. With token privilege separation, both the backing user and
token need the role. VM-specific ACLs override inherited access, so retain
`PVEAuditor` at the same path:

```bash
pveum role add KomariBridgeQGA -privs VM.GuestAgent.Unrestricted
pveum acl modify /vms/100 \
  --roles PVEAuditor,KomariBridgeQGA \
  --users 'komari-bridge@pve' \
  --tokens 'komari-bridge@pve!monitor'
```

The command is embedded and cannot be configured by API callers. Collection
falls back to PVE memory if QGA is unavailable.

## Agentless SSH fallback

Use a dedicated key and pin the server's real SHA-256 host-key fingerprint.
`attach_to` must match the PVE provider identity exactly.

```yaml
agentless_ssh:
  - id: gpu-a
    address: gpu-a.example.internal:22
    user: monitor
    private_key_path: /run/secrets/gpu_a_ssh_key
    host_key: SHA256:replace-with-the-real-host-key-fingerprint
    enable_nvidia: true
    attach_to:
      source_type: proxmox
      source_id: site-a
      external_id: qemu:105
```

The first sample initializes CPU and network counters. This mode currently
supports NVIDIA metrics for compatibility, but it is intentionally feature
frozen. Install Komari Agent instead whenever possible.

## Slurm extension

Slurm has its own provider and does not register or report a Komari host
client. The official Agent can therefore remain authoritative for the host:

```yaml
slurm:
  - id: gpu-a
    address: gpu-a.example.internal:22
    user: monitor
    private_key_path: /run/secrets/gpu_a_ssh_key
    host_key: SHA256:replace-with-the-real-host-key-fingerprint
```

Slurm remains an extension API because Komari does not currently expose a
general custom-metric model:

```bash
curl -H "Authorization: Bearer $BRIDGE_API_KEY" \
  http://127.0.0.1:9090/api/v1/slurm/gpu-a
```

`/healthz` is intentionally unauthenticated.

## Windows and WSL discovery

The SSH account must be the Windows user that owns the WSL registrations. WSL
distributions are isolated per Windows user, even for administrators.

```yaml
windows_wsl:
  - id: workstation-a
    address: workstation-a.example.internal:22
    user: monitor
    private_key_path: /run/secrets/workstation_a_ssh_key
    host_key: SHA256:replace-with-the-real-host-key-fingerprint
    wsl_names:
      Ubuntu: wsl-a
    attach_to:
      source_type: proxmox
      source_id: site-a
      external_id: qemu:107
```

WSL identities use `wsl:<registration-guid>`. Registry entries are always
discovered, but inner metrics are queried only for distributions that are
already running. This provider does not report Windows host metrics; install
Komari Agent on Windows whenever possible. WSL inner metrics remain a fallback
until Komari can represent discovered children independently from host reports.

## Configuration migration

The old `linux_ssh` and `windows_ssh` keys remain supported and keep their
original provider identities, preventing duplicate client registration during
upgrades. They are deprecated:

| Legacy key | Replacement | Behavior |
|---|---|---|
| `linux_ssh` | `agentless_ssh` | Optional host-metric fallback |
| `linux_ssh` with `enable_slurm` | `slurm` | Extension-only Slurm data |
| `windows_ssh` with `discover_wsl` | `windows_wsl` | WSL discovery without Windows host reporting |

Changing a legacy key changes the provider identity. Keep the old key until
you have deliberately migrated or removed its existing virtual clients.

## Security notes

- Keep `config.yaml`, SSH keys, and the SQLite database private.
- The database contains Komari client tokens and is created with mode `0600`.
- Prefer environment variables or mounted secrets over literal credentials.
- Pin SSH host keys; do not enable insecure host-key handling in production.
- Keep port `9090` private or place it behind authenticated HTTPS.
- Use HTTPS for Komari and rotate automatic-discovery keys after testing.
- Scope QGA execution permission to the smallest possible set of VMs.

See [`SECURITY.md`](SECURITY.md) for vulnerability reporting.

## Development

```bash
gofmt -w .
go vet ./...
go test ./...
```

Integration tests are opt-in and require explicitly supplied environment
variables. Contributions are welcome; see [`CONTRIBUTING.md`](CONTRIBUTING.md).

## Roadmap

1. Propose upstream resource identity, parent topology, and Agent binding APIs.
2. Propose namespaced extension metrics so Slurm data can enter Komari without
   replacing Agent host reports.
3. Extend Proxmox discovery with storage and health data.
4. Consider opt-in Docker and SNMP child-resource discovery after the server UI
   can represent topology.

## Acknowledgements

- [Komari](https://github.com/komari-monitor/komari) provides the server,
  dashboard, history, and alerting system this bridge reports to.
- [Komari Agent](https://github.com/komari-monitor/komari-agent) is the reference
  implementation for host collection behavior and Komari metric payloads.

Both upstream projects are available under the MIT License. See
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).

## License

MIT — see [`LICENSE`](LICENSE).
