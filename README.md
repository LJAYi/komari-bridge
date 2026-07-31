# komari-bridge

`komari-bridge` discovers infrastructure through provider APIs and represents
each resource as a standard [Komari](https://github.com/komari-monitor/komari)
client. It is designed for hypervisors, appliances, WSL distributions, and
other systems where installing the official
[Komari Agent](https://github.com/komari-monitor/komari-agent) is undesirable
or impossible.

This is an independent community project. It is not affiliated with or
endorsed by the Komari maintainers.

## Features

- Proxmox VE node, QEMU VM, and LXC discovery
- Durable SQLite mappings between provider identities and Komari clients
- Komari automatic registration and HTTP v2 JSON-RPC reporting
- Guest OS discovery through the QEMU Guest Agent
- Optional Linux guest memory from `/proc/meminfo` through a fixed QGA command
- Linux SSH enrichment for CPU, memory, disk, network, NVIDIA GPU, and Slurm
- Windows OpenSSH enrichment with an eight-second `nvidia-smi` timeout
- Per-user WSL discovery using stable registry GUIDs
- Running-only WSL collection, which does not start stopped distributions
- Provider merging so hypervisor topology and guest metrics share one client
- Read-only Slurm extension API with optional bearer authentication
- Multiple Proxmox sources, including sites with overlapping private subnets

The HTTP payloads intentionally follow the behavior and metric shapes used by
Komari Agent. The agent remains the preferred option when it can be installed;
the bridge is a compatibility layer for resources that need centralized or
agentless collection.

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

## Resource identity and merging

Resources use a stable provider identity:

```text
(source_type, source_id, external_id)
```

For example, a PVE VM may be discovered as `qemu:105`. An SSH provider can
attach to that identity and replace hypervisor-observed guest metrics without
creating a duplicate Komari client.

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

## Linux SSH, GPU, and Slurm

Use a dedicated key and pin the server's real SHA-256 host-key fingerprint.
`attach_to` must match the PVE provider identity exactly.

```yaml
linux_ssh:
  - id: gpu-a
    address: gpu-a.example.internal:22
    user: monitor
    private_key_path: /run/secrets/gpu_a_ssh_key
    host_key: SHA256:replace-with-the-real-host-key-fingerprint
    enable_nvidia: true
    enable_slurm: true
    attach_to:
      source_type: proxmox
      source_id: site-a
      external_id: qemu:105
```

The first sample initializes CPU and network counters. GPU metrics use Komari's
existing GPU payload. Slurm remains an extension API because Komari does not
currently expose a general custom-metric model:

```bash
curl -H "Authorization: Bearer $BRIDGE_API_KEY" \
  http://127.0.0.1:9090/api/v1/slurm/gpu-a
```

`/healthz` is intentionally unauthenticated.

## Windows SSH and WSL

The SSH account must be the Windows user that owns the WSL registrations. WSL
distributions are isolated per Windows user, even for administrators.

```yaml
windows_ssh:
  - id: workstation-a
    address: workstation-a.example.internal:22
    user: monitor
    private_key_path: /run/secrets/workstation_a_ssh_key
    host_key: SHA256:replace-with-the-real-host-key-fingerprint
    enable_nvidia: true
    discover_wsl: true
    wsl_names:
      Ubuntu: wsl-a
    attach_to:
      source_type: proxmox
      source_id: site-a
      external_id: qemu:107
```

WSL identities use `wsl:<registration-guid>`. Registry entries are always
discovered, but inner metrics are queried only for distributions that are
already running. If Windows `nvidia-smi` times out, the bridge can use GPU data
from a running WSL distribution.

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

1. Add explicit topology fields to a custom Komari UI.
2. Keep Slurm metrics in the extension API until a general custom-metric model
   is available.
3. Propose generic parent and resource-source fields upstream after broader
   production validation.

## Acknowledgements

- [Komari](https://github.com/komari-monitor/komari) provides the server,
  dashboard, history, and alerting system this bridge reports to.
- [Komari Agent](https://github.com/komari-monitor/komari-agent) is the reference
  implementation for host collection behavior and Komari metric payloads.

Both upstream projects are available under the MIT License. See
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).

## License

MIT — see [`LICENSE`](LICENSE).
