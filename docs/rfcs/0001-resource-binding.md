# RFC 0001: External resources, topology, and Agent binding

- Status: Draft
- Target: Komari Server
- Implementation: Not yet implemented

## Summary

Add server-owned external resources and relationships so discovery tools can
associate Proxmox, WSL, Docker, or scheduler resources with an existing Komari
Agent client without submitting a competing host report.

## Problem

A bridge identity is currently:

```text
(source_type, source_id, external_id)
```

Komari Agent independently owns a server-generated client UUID. There is no
standard API for saying that `proxmox/site-a/qemu:105` describes that client,
or that a WSL distribution is its child. Reusing the Agent token is unsafe:
both writers can replace basic information, metrics, and online presence.

## Goals

- Bind an external resource identity to an existing client UUID.
- Store typed parent-child relationships independently from display names.
- Let extensions write namespaced metrics without replacing the host report.
- Make metric ownership explicit and auditable.
- Allow idempotent upserts from multiple bridge instances.

## Non-goals

- Sharing an Agent client token with the bridge.
- Reimplementing terminal, task, ping, or update protocols.
- Turning every discovered child into a full Komari client.
- Encoding topology into client names.

## Proposed model

```text
ExternalSource
  id, type, display_name

ExternalResource
  source_id, external_id, resource_type, display_name
  bound_client_uuid (nullable)
  parent_source_id (nullable)
  parent_external_id (nullable)
  attributes

ExtensionMetric
  resource identity or client_uuid
  namespace, metric, value, labels, observed_at, ttl
```

The unique key for a resource is `(source_id, external_id)`. A source is scoped
to one authenticated discovery integration, preventing unrelated bridges from
claiming the same namespace.

## Proposed API behavior

Exact routes should follow Komari's conventions; the semantic operations are:

1. Upsert an external source and its resources.
2. Bind or unbind one resource to a client UUID with an explicit permission.
3. Upsert parent relationships between resources.
4. Submit namespaced extension metrics with timestamps and TTLs.
5. Read resources and relationships for tree or graph rendering.

Writes must be idempotent. Removing a resource from one discovery cycle should
mark it stale after a source-level TTL, not immediately delete history.

## Metric ownership

The Agent remains the only writer for the standard host report. The bridge may
write only namespaced data such as:

```text
slurm.jobs.running
slurm.jobs.pending
slurm.gpus.allocated
proxmox.guest.status
proxmox.storage.health
```

Extension metrics cannot overwrite standard CPU, memory, disk, network, GPU,
or presence fields.

## Security

- Use a distinct integration token, never an Agent client token.
- Scope source writes, binding, and metric writes separately.
- Record binding changes in an audit log.
- Reject parent cycles and cross-source relationships unless explicitly
  authorized.
- Treat Slurm users, job identifiers, and reasons as potentially sensitive.

## Bridge migration

After server support exists:

1. Import existing provider identities as external resources.
2. Configure or approve bindings to Agent UUIDs.
3. Stop registering virtual clients for bound host resources.
4. Keep Agent host reports unchanged.
5. Move Slurm data from the bridge HTTP endpoint to extension metrics.

Unbound appliances and discovered children may remain virtual clients until the
Komari UI can render non-client resources directly.
