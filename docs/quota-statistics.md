# ClickHouse quota statistics

Quota statistics are opt-in observations of provider account balances, separate from
request/token usage and routing cooldown state (`quota.go`). There is no inferred
history, seed data, provider polling daemon, or automatic schema migration.

## Setup

1. Provision your own ClickHouse database. Apply
   [`internal/quotastats/schema.sql`](../internal/quotastats/schema.sql) in that
   database using an administrative connection. Do not run DDL through the proxy.
2. Give the runtime ClickHouse user only `SELECT` and `INSERT` on `quota_samples`.
   Use HTTPS outside a trusted local network. The HTTP client verifies TLS and
   refuses redirects; credentials are sent as HTTP Basic authentication, not URL parameters.
3. Configure the following v8 YAML fields, or their environment overrides. Values
   below are intentionally empty; supply your own endpoint, database and credentials.

   ```yaml
   observability:
     quota:
       clickhouse:
         endpoint: ""
         database: ""
         username: ""
         password: ""
   ```

   Non-empty `CLICKHOUSE_QUOTA_ENDPOINT`, `CLICKHOUSE_QUOTA_DATABASE`,
   `CLICKHOUSE_QUOTA_USERNAME`, and `CLICKHOUSE_QUOTA_PASSWORD` override their YAML
   fields. Prefer environment injection for the password. Endpoint must be an HTTP(S)
   URL without embedded credentials, query parameters or fragment. An empty endpoint
   disables reads/ingest. Database is required when enabled. YAML changes follow
   management config reload; environment changes require restarting the process.
4. Configure the existing management key and remote-access policy. Both read endpoints
   below use the normal management middleware, including IP bans, Home restrictions,
   and `Authorization: Bearer <management-key>`. No new public or v0 routes are added.

This data contains opaque account identifiers and balances, not OAuth tokens, email
addresses, raw provider responses, or request content. Restrict access accordingly.
Use one database per proxy inventory: auth indexes from unrelated instances must not
be combined unless the collector deliberately defines a shared identity scheme.

## Schema and ingestion

`quota_samples` uses `ReplacingMergeTree(received_at)`, monthly partitions and
`ORDER BY (provider, account, at)`. Repeated inserts at the same observation time
replace that observation; reads use `FINAL`, so correctness does not depend on a
background merge. Later `received_at` wins; equal-version conflicts should be avoided.
The 90-day TTL removes expired data during background merges, not at an exact deadline.
No aggregate tables or mutations are needed. Adjust retention through your own DDL
process if required; `CREATE IF NOT EXISTS` does not upgrade an existing table.

Stored fields:

| Field | Meaning |
| --- | --- |
| `provider` | Credential provider identifier (for example `codex`) |
| `account` | Credential `auth_index`; not filename, display name, or OAuth account ID |
| `at` | Observation timestamp, UTC Unix epoch **milliseconds** |
| `remaining` | Account percentage points, **0–100** |
| `resetAt` | Next reset, epoch milliseconds or `null` |
| `window` | Stable selected quota window identity |
| `periodHours` | Known recurring full-reset period, 1–8784 hours, or `null` |
| `received_at` | ClickHouse receipt/version timestamp, default `now64(3)` |

Successful **v8** `GET /plugins/:id/quota?auth_index=...` and
`POST /plugins/:id/quota` fetches automatically append one selected-window sample.
These paths are relative to `/v8/management`. Selection prefers the longest recognized
window (weekly/7d/7-day/7_day, daily/24h/1d, 5h/5-hour/5_hour, hourly/1h), then the
furthest known reset, then original provider order. It never adds overlapping buckets.
`window` is `group.displayName + "/" + bucket.window`. Unknown periods remain null.
RFC3339 reset times are corrected by the provider's `serverTimeOffsetMs`.
This generic selection is not a provider-specific guarantee that a model-scoped limit
is the same as the fleet UI's preferred account window. A collector can select the
provider-specific canonical window instead; do not mix differing selection policies.

The response shape from the quota provider is unchanged. `X-Quota-Statistics` reports
`stored`, `disabled`, `skipped` (no usable numeric bucket), or `failed`. Failed writes
log a generic warning and preserve the successful quota response. Writes are synchronous,
one insert per refresh, best-effort and not retried; they follow request cancellation
with no post-connect timeout. A slow ClickHouse can therefore delay the refresh.
For high-volume collection, batch externally instead of using UI refreshes as a scheduler.

**Coverage limitation:** requests via `/requests/api-call`, old v0 fetches, token
refreshes and executor response headers do not record samples. Resets do not synthesize
a 100% sample; fetch quota again after reset. No reads call provider APIs.

### External collectors

For native-provider UI quota fetches using `api-call`, or unattended collection, insert
normalized observations directly through ClickHouse's HTTP interface:

- POST to **your configured endpoint**, with your own ClickHouse credentials.
- Set `database` to your database and `query` to
  `INSERT INTO quota_samples (provider, account, at, remaining, resetAt, window, periodHours) FORMAT JSONEachRow`.
- Send newline-delimited JSON objects with exactly those seven field names. Use numbers
  for `at`, `remaining` and known metadata, JSON `null` for unknown reset/period values.
- Fetch credentials from `/v8/management/credentials` and use their `provider` and
  `auth_index` values. Select one stable account window per provider/account. Never sum
  overlapping windows; convert fractions to percentages. Only emit successful numeric
  observations, including genuine zero. Do not insert failed fetches as zero.
- Preserve `at` on retries so duplicate delivery replaces rather than doubles samples.
  Omit `received_at` to use the server default. Synchronize collector clocks.
- Set `periodHours` only when the provider contract specifies a recurring full refill;
  a reset timestamp alone does not establish a period. Rolling, partial or expiring
  credit semantics must use null unless safely normalized to a full-reset window.

## Dashboard read contract

Both endpoints are **GET**, relative to `/v8/management`:

| Endpoint | Response |
| --- | --- |
| `/observability/quota/history` | `{from, to, history: Record<string, Sample[]>}` |
| `/observability/quota/pools` | `{from, to, rateHours, usageMultiplier, pools: Pool[]}` |

Common optional query parameters: `provider` (exact provider ID), `account` (exact
auth_index), `from` and `to` (epoch milliseconds). Default range is the 24 hours ending
now. Bounds are inclusive; `0 < from < to <= now`, maximum span 31 days. The default
`from` is 24 hours before the supplied `to`. Results over 100,000 observations or 64 MiB
fail instead of silently truncating: narrow the time range or select a provider/account.

Pools additionally use `rateHours=1|3|6` (default **3**) and
`usageMultiplier=0|0.5|1|2` (default **1**). The same optional parameters are accepted on
history but do not change its contents. Rate evidence is limited to the requested range
intersected with the lookback ending at `to`; ask for at least `rateHours` of history.

```ts
type Sample = {
  provider: string; account: string; at: number; remaining: number;
  resetAt: number | null; window: string; periodHours: number | null;
};
type Point = { at: number; value: number };
type Account = Sample & { name: string; burn: number | null };
type Pool = {
  provider: string;
  accountCount: number; capacity: number; remaining: number;
  burn: number | null; drain: number | null;
  points: Point[]; accounts: Account[];
  projection: {
    available: boolean;
    points: Point[]; refills: Point[];
    emptyAt: number | null;
    emptyAccounts: { name: string; at: number }[];
  };
};
```

History keys and account `name` are `${provider}:${auth_index}`; series are chronological.
Join account display names to the credential list by `auth_index`; do not parse filenames
from the history key. `accounts` carries the latest sample, including `resetAt` and `at`
for reset labels and staleness indicators. Providers and accounts are sorted by ID.

`remaining`, `capacity` and `accountCount` describe **observed accounts in the interval**,
not the configured fleet inventory. Missing accounts are omitted, not zero-filled.
Remaining is the sum of each observed account's latest balance; capacity is 100 per
observed account, with no plan weighting. It is numeric even when some inventory accounts
were never sampled. Clients must compare with inventory before labeling coverage complete.
Pool `points` carries balances forward only within the shared observed time range of all
included accounts; it is empty when the ranges do not overlap.

Burn is account percentage-points/hour, summed across accounts; drain is burn/accountCount
(percent of pool capacity/hour). Rates exclude increases, reset changes, reset-crossing
intervals and window transitions. At least five minutes of qualifying elapsed observations
are required; missing evidence is null, not zero. Any unknown account rate makes provider
burn/drain null. Usage multiplier changes forecasts only, not observed rates.

Projections run from `to` to `to + 14 days`, starting from latest observed balances (not
unobserved consumption since the last fetch). All accounts need a known rate, future reset,
known full-reset period and a latest sample no older than the rate lookback. Otherwise
`available=false`, arrays are empty and `emptyAt=null`. A safety bound of 100,000 forecast
events also makes projection unavailable rather than returning a partial forecast.
Refills restore each account to 100; `refills[].value` is the **amount added**, and `points`
includes both pre- and post-refill balances at the same timestamp. `emptyAccounts` gives
each account's first exhaustion; `emptyAt` is first whole-pool exhaustion, including the
instant immediately before a simultaneous refill. An available forecast with null
`emptyAt` means no exhaustion within the horizon, not unavailable data.

Successful reads set `Cache-Control: no-store`. Errors are `{error: string}`:

| Status | Meaning |
| --- | --- |
| 400 | Invalid range or rate/scenario parameter |
| 401/403 | Normal management authentication/access policy failure |
| 404 | Management disabled/Home restricted, or endpoint absent on an older backend |
| 422 | Too many observations/bytes; narrow query |
| 502 | ClickHouse request/query/decoding failure (no upstream secrets exposed) |
| 503 | ClickHouse disabled or configuration invalid |

A configured empty store returns HTTP 200 with `history: {}` or `pools: []`. The UI
should distinguish that from unavailable storage and from unknown rates/projections.

## Verification

`go test ./internal/quotastats ./internal/config ./internal/api/handlers/management ./internal/api`
uses deterministic observations and mocked HTTP ClickHouse responses. CI needs no
ClickHouse service, credentials or external provider. DDL must also be smoke-tested
against the operator's ClickHouse version before production rollout.
