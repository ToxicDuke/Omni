# Panel failover MVP

Status: implemented for the first Omni MVP.

## Objective

Omni keeps game-server nodes manageable when an individual Panel instance,
network route, or region becomes unavailable. Running containers must not be
stopped merely because the current Panel endpoint is unreachable.

All configured endpoints are entrances to one logical Mikasa Host control plane:

- one logical MySQL database;
- a single writable database primary;
- a regional replica promoted through a fenced, managed failover procedure;
- independent, disposable Redis caches in each Panel region;
- authentication handled by a separate highly available SSO service.

Independent Panel databases and cross-region multi-writer operation are outside
the design because they would require conflict resolution that the Panel API does
not provide.

## Runtime model

Each endpoint has a stable name, URL, and priority. Lower numeric
priority is preferred. Omni selects exactly one endpoint and sends all Panel API
traffic to it.

The selection state machine is:

1. Restore the previously selected endpoint, but do not assume it is healthy.
2. Prefer the highest-priority healthy endpoint.
3. Record passive failures from normal requests and active health-check results.
4. Mark an endpoint unhealthy after the configured consecutive failure threshold.
5. Select the next healthy endpoint and persist the switch before exposing it as
   active.
6. Keep the new endpoint active for at least the switch cooldown.
7. Fail back only after the preferred endpoint reaches the recovery threshold.

Endpoint order is deterministic: priority first and configuration order second.
This prevents nodes from oscillating randomly between equivalent endpoints.

## Failure classification

Failures that count against endpoint health:

- DNS, connection, and TLS failures;
- request timeouts not caused by caller cancellation;
- HTTP 5xx responses.

HTTP 4xx responses do not trigger endpoint failover because another replica will
normally reject the same invalid request or credentials. HTTP 429 proves that the
Panel is reachable; it remains subject to request backoff but not endpoint
failover.

Retries against one endpoint must be bounded so endpoint-local backoff cannot
consume the entire operation deadline before failover is attempted.

## Configuration compatibility

The existing scalar `remote` setting remains supported. If `remotes.endpoints` is
empty, Omni converts `remote` into one implicit endpoint. This preserves existing
Wings installations and Panel-generated configuration.

```yaml
remote: https://panel-ru.example.com

remotes:
  health_check_interval: 15
  failure_threshold: 3
  recovery_threshold: 2
  switch_cooldown: 30
  endpoints:
    - name: ru
      url: https://panel-ru.example.com
      priority: 10
    - name: pl
      url: https://panel-pl.example.com
      priority: 20
```

Node credentials remain shared by all endpoints in the MVP because the endpoints
use the same logical database and node identity.

## Persistence

Omni extends its local SQLite database with four responsibilities:

- selected endpoint state;
- append-only endpoint switch journal;
- last known working server configuration, keyed by server UUID;
- typed durable event outbox.

Outbox records are created before delivery and removed only after a successful
response. With an unchanged Panel API this provides at-least-once delivery, not
exactly-once delivery. Only explicitly classified event operations may enter the
outbox; reads and arbitrary control requests are never replayed.

Cached server configuration is a degraded-mode fallback. It never overwrites a
newer successful Panel response and its age is exposed as a metric. Cache and
database files retain the node data directory's restrictive permissions.

## Observability

The MVP exposes or logs:

- active endpoint name and priority;
- endpoint health and consecutive failure/success counters;
- switch count, reason, source, destination, and timestamp;
- age of the oldest outbox record and total outbox depth;
- age and count of cached server configurations.

Authorization headers and tokens must never appear in health logs, metrics, or
the switch journal.

The current snapshot is available to authenticated callers at `GET /api/omni`.

## Non-goals

- active-active request delivery;
- independent Panel databases;
- cross-region multi-writer MySQL;
- changes to Panel API routes or payloads;
- exactly-once event processing;
- using cached configuration as a permanent source of truth.
