# Architecture and scope

## Purpose

komari-bridge complements Komari Agent; it does not replace it. The Agent owns
metrics and operations for an OS on which it can run. The bridge owns discovery
and relationships for resources that cannot be represented by one Agent.

## Ownership

| Capability | Owner |
|---|---|
| Host CPU, memory, disk, network, OS, GPU | Komari Agent |
| Terminal, tasks, ping, pull, updates | Komari Agent |
| Proxmox nodes, QEMU guests, and LXC discovery | Bridge |
| Windows-to-WSL discovery | Bridge |
| Slurm scheduler state | Bridge extension |
| Host metrics when Agent cannot run | Optional `agentless_ssh` fallback |
| Parent topology and Agent identity binding | Requires Komari Server support |

## Provider rules

A new provider should satisfy at least one of these conditions:

1. It discovers resources or parent-child relationships that an Agent cannot.
2. It observes an appliance on which an Agent cannot reasonably run.
3. It supplies domain metrics outside the generic host model.

A provider should not be added merely to collect another OS's generic host or
GPU metrics remotely. Docker and SNMP, if added, should be discovery-oriented;
short-lived containers should not become Komari clients by default.

## Current compatibility limitation

The bridge currently registers virtual clients and calls only
`agent.basicInfo` and `agent.report`. `attach_to` merges two bridge provider
snapshots that use the same provider identity. It does not associate a resource
with a client independently registered by Komari Agent.

For bridge-owned virtual clients, parent and site metadata is mirrored into
Komari's existing `group` and `tags` fields as a compatibility transport. This
allows topology-aware themes to group PVE guests and WSL children, but it does
not create server-side resource objects, enforce relationship integrity, or
provide Agent identity binding.

Until the server exposes topology and extension APIs, the bridge must not share
an Agent client token or submit a second complete host report to that client.
Doing so would make online state and metric ownership nondeterministic.

## Migration policy

Legacy `linux_ssh` and `windows_ssh` configurations remain readable and keep
their historical source identities. New configuration uses:

- `agentless_ssh` for the frozen host fallback;
- `windows_wsl` for WSL discovery without a Windows host report;
- `slurm` for extension-only scheduler data.

Changing provider keys is an explicit identity migration and may require
removing old virtual clients from Komari.
