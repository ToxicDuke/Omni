# Operating Panel failover

This runbook covers the first production rollout of Omni's single-active Panel
endpoint failover.

## Preconditions

- Every endpoint serves the same logical Panel database.
- The database has one writable primary. Promotion of a regional replica fences
  the previous writer before accepting writes.
- Each Panel accepts the same Omni node UUID and token.
- Redis is disposable regional cache only; SSO owns user sessions.
- DNS, certificates, firewalls, and Panel-to-node routing work independently in
  every region.

Do not configure independent Panel databases or two writable database primaries.

## Node configuration

Durations are integer seconds.

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

Keep `remote` during the first rollout for rollback compatibility. Omni uses
`remotes.endpoints` when that list is present and otherwise treats `remote` as a
single legacy endpoint.

Endpoint URLs must be unique HTTP(S) base URLs without credentials, query
parameters, or fragments. Lower numeric priority is preferred.

## Rollout

1. Back up `config.yml` and the node's existing `wings.db`.
2. Verify all Panel endpoints manually with the node's normal network path.
3. Deploy Omni to one canary node with at least two endpoints.
4. Query authenticated `GET /api/omni` and confirm the preferred endpoint is
   active and all endpoints become healthy.
5. Block only the active Panel route. Confirm a switch is recorded and game
   containers continue running.
6. Trigger a status-producing operation while every Panel route is blocked.
   Confirm `outbox_depth` increases.
7. Restore a route and confirm the outbox drains in ID order.
8. Restore the preferred route and confirm failback happens only after recovery
   threshold and cooldown.
9. Restart the canary and confirm the selected endpoint, journal, cache, and
   outbox survive.
10. Roll out progressively by failure domain.

## Observability

Authenticated `GET /api/omni` returns:

- active endpoint and priority;
- health and consecutive success/failure counts per endpoint;
- persistent switch count and the 20 most recent switch records;
- outbox depth, oldest event timestamp, and age in seconds;
- cached server count, oldest cache timestamp, and age in seconds.

Alert when:

- the preferred endpoint remains unhealthy beyond the planned maintenance;
- switch frequency suggests flapping;
- outbox depth or oldest-event age continues growing;
- cache age exceeds the expected Panel recovery window.

## Failure semantics

- DNS, connection, TLS, client-side transport timeout, and HTTP 5xx failures
  count against endpoint health.
- HTTP 4xx does not cause passive failover. A different replica should not hide
  invalid credentials or invalid requests.
- Active health checks require a successful existing Panel API response; HTTP
  429 proves reachability and is considered healthy.
- Cached server configuration is used only when the logical Panel is
  unavailable. A successful full server list prunes deleted UUIDs from cache.
- Status events are delivered at least once. The unchanged Panel API cannot
  provide exactly-once deduplication.
- New SFTP authentication still requires a reachable Panel; existing game
  containers and established connections are not stopped by Panel loss.

## Rollback

Stop Omni, restore the previous binary, and keep the scalar `remote` value.
The additional SQLite tables are ignored by Wings. Do not delete `wings.db`
until the outbox is empty, otherwise queued status events will be lost.
