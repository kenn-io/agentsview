---
title: Hosted Raw Sync
description: Architecture, security boundaries, and delivery status for raw-first hosted sync
---

Hosted raw sync is the planned, in-development path for sending original agent
source artifacts to a hosted AgentsView service. The server will keep the
authoritative raw generation, then derive PostgreSQL sessions and embeddings
from it.

```mermaid
flowchart LR
    Watcher["Laptop watcher"] -->|"authenticated raw upload"| Custody["Immutable raw custody"]
    Custody --> Parser["Server parsing"]
    Parser --> PostgreSQL["PostgreSQL projection"]
    PostgreSQL --> Embeddings["Server embeddings"]
```

This moves long-running parsing and embedding work off laptops and gives the
server enough source material to reparse after parser fixes or rebuild derived
data after loss.

!!! warning "Not available for use yet"

    AgentsView now exposes part of the authenticated raw-sync control plane from
    `agentsview pg serve`, but it does not yet expose device enrollment, object
    upload, or status endpoints. There is also no laptop uploader, server-side
    parsing worker, or server-owned embedding pipeline. No supported command or
    configuration setting enables hosted raw sync end to end.

    The existing [`agentsview pg push`](/pg-sync/) workflow remains supported and
    unchanged. It parses sessions locally and can build embeddings locally before
    pushing derived rows and vectors to PostgreSQL.

The tracked delivery sequence and production acceptance criteria live in
[GitHub issue #1352](https://github.com/kenn-io/agentsview/issues/1352).

## Delivery status

| Layer                  | Status                                                                                       | Current boundary                                                                                                            |
| ---------------------- | -------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------- |
| Raw custody            | Foundation implemented in [#1396](https://github.com/kenn-io/agentsview/pull/1396)           | Validated objects, canonical manifests, durable receipts, source-head fencing, and parse-job creation                       |
| Device authentication  | Foundation implemented in [#1459](https://github.com/kenn-io/agentsview/pull/1459)           | One-time device credentials, scoped short-lived tokens, server-derived identity, and revocation                             |
| HTTP raw transport     | Control plane partly implemented in [#1473](https://github.com/kenn-io/agentsview/pull/1473) | Credential exchange, missing-object negotiation, and manifest commit; enrollment, object upload, and status remain          |
| Laptop capture         | Not implemented                                                                              | Provider watching, append fast paths, SQLite snapshots, spooling, checkpoints, and reconciliation remain future client work |
| Server derivation      | Not implemented                                                                              | Manifest materialization, parsing, transactional PostgreSQL projection, and embeddings are not running                      |
| Operations and cutover | Not implemented                                                                              | Retention, garbage collection, disaster rebuilds, rollout controls, and migration from `pg push` remain future work         |

The broader “device enrollment and authenticated raw transport” delivery item in
#1352 remains incomplete because there is no supported way to enroll a device or
upload the objects that a manifest references.

## HTTP control plane

`agentsview pg serve` registers the raw-sync routes when its PostgreSQL role can
write every raw-sync table and the ingest-job sequence. A read-only role keeps
serving the normal PostgreSQL-backed UI and API without these runtime routes.
There is no separate raw-sync configuration switch.

The implemented routes are:

| Route                                   | Authentication                          | Operation                               |
| --------------------------------------- | --------------------------------------- | --------------------------------------- |
| `POST /api/v1/raw-sync/tokens`          | Device credential and device ID         | Issue a 15-minute scoped access token   |
| `POST /api/v1/raw-sync/objects/missing` | Access token with the `negotiate` scope | Return object references not in custody |
| `POST /api/v1/raw-sync/manifests`       | Access token with the `commit` scope    | Validate and commit one raw generation  |

These machine routes use their own device credentials and scoped tokens. They do
not accept the shared bearer token that can protect the rest of a remote
AgentsView server. The token endpoint accepts the fixed `negotiate`, `upload`,
`commit`, and `status` scope names, although this branch exposes handlers only
for negotiation and commit.

PostgreSQL stores device, token, manifest, receipt, source-head, and parse-job
metadata. The raw object repository is opened lazily under `raw-sync/` in the
configured AgentsView data directory. Because the HTTP surface cannot yet upload
missing objects, it is a server foundation for the future laptop client, not a
complete protocol for integrations.

## Raw custody contract

The custody foundation accepts a complete logical generation of one provider
source. A generation is represented by a canonical manifest containing:

- the provider, configured source-root identity, and logical source key;
- a capture identity and capture time;
- either a snapshot with ordered file-object references or a tombstone; and
- the expected receipt for the preceding accepted generation.

The custody API accepts authenticated tenant and immutable device identity
separately from the manifest. The HTTP handlers derive that identity through
device authentication instead of accepting tenant or device fields in request
bodies. Canonicalization binds it into the manifest envelope. The canonical JSON
digest becomes the manifest ID.

Raw objects are identified by exact SHA-256 and byte length. Custody is
tenant-scoped, verifies content before registering it, treats an identical retry
as a no-op, and rejects conflicting content. Providers that AgentsView does not
recognize or excludes from remote sync are rejected before their bytes enter
custody.

A manifest can commit only after every referenced object exists and verifies.
The PostgreSQL acceptance transaction then:

1. checks the expected parent receipt against the current source head;
1. records the manifest, file entries, and ordered object references;
1. assigns a monotonically increasing generation and durable receipt;
1. creates the corresponding parse job; and
1. advances the source head.

Repeating the same capture returns its existing receipt. Reusing a capture
identity for different content or committing against a stale parent fails
closed. Accepted manifest metadata is append-only. PostgreSQL holds custody
metadata and processing state; the object store holds the authoritative raw
bytes and canonical manifests.

## Device authentication contract

Enrollment creates an immutable device ID and a random credential. The clear
credential is returned once; PostgreSQL retains only its SHA-256 digest.

An active device exchanges that credential for an opaque, short-lived token.
Tokens can be restricted to one or more fixed operations:

- missing-object negotiation;
- object upload;
- manifest commit; and
- status reads.

Token authentication derives the tenant and device from server-side records and
checks the required scope and expiry. PostgreSQL stores only the token digest.
Revoking a device prevents new token issuance and immediately makes its
outstanding tokens unusable. A human-readable device name is display metadata,
not authorization identity.

## Security and deployment boundary

Raw provider files can contain prompts, responses, tool activity, paths, and
other sensitive data. Hosted raw sync is not end-to-end encrypted: the server
must be able to read retained source files to parse them.

The implemented foundations isolate object and metadata identities by tenant and
do not deduplicate across tenants. A production deployment must also provide TLS
in transit, encryption at rest for object storage, PostgreSQL, backups, and
worker scratch space, plus access controls around device enrollment and
revocation. PostgreSQL row-level security remains a planned defense-in-depth
layer; the current foundation does not configure it.

Do not build integrations against the partial HTTP routes, internal Go packages,
or raw PostgreSQL tables yet. The complete public protocol, operator controls,
compatibility policy, and recovery tooling will be documented when those entry
points exist.
