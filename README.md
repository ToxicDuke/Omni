# Omni

Omni is the resilient game-server node agent developed for
[Mikasa Host](https://mikasa.host). It is based on Pterodactyl Wings and remains
compatible with the existing Pterodactyl Panel API while adding the foundations
for a geographically redundant control plane.

The name is inspired by the omni-directional mobility gear used by the Scout
Regiment: a node should be able to change its point of attachment without
stopping the servers it protects.

## Why Omni

A standard Wings installation is attached to a single Panel URL. An outage of
that Panel or its region can therefore interrupt management of otherwise healthy
game servers. Omni is designed to connect a node to multiple equivalent Panel
endpoints and automatically select a healthy endpoint without introducing an
active-active Panel protocol.

The first MVP is intentionally constrained:

- one active Panel endpoint per Omni node at any time;
- ordered endpoints with health checks and automatic failover;
- a locally persisted selected endpoint and switch journal;
- a cache of the last known working server configuration;
- a durable outbox for events that could not be delivered;
- metrics for endpoint health, switches, cache age, and outbox depth;
- no changes to the Pterodactyl Panel API;
- no concurrent delivery to multiple Panels.

The Panel endpoints must represent one logical control plane. Mikasa Host's
target deployment uses one logical MySQL database with a single writer and a
managed regional replica, independent Redis caches per Panel region, and a
separate highly available SSO service.

See [docs/architecture/panel-failover-mvp.md](docs/architecture/panel-failover-mvp.md)
for the architecture and failure semantics.

## Upstream and branches

Omni development follows the `develop` branch because the upstream Pterodactyl
Wings project also uses `develop` as its integration branch. The repository uses:

- `origin` for the Mikasa Host Omni fork;
- `upstream` for `https://github.com/pterodactyl/wings.git`.

Keeping the upstream Go module path during the first MVP limits merge conflicts
and is not an ownership claim. A module-path migration can be evaluated as a
separate compatibility change.

## Development

Omni currently retains the Wings build and runtime requirements. Build and test
the project with:

```bash
go build ./...
go test ./...
```

## Upstream attribution

Omni is a fork of [Pterodactyl Wings](https://github.com/pterodactyl/wings),
originally created by Dane Everitt and contributors. Both projects are provided
under the MIT License; see [LICENSE](LICENSE).
