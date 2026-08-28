# google-health-mcp

A small MCP server for the [Google Health API v4](https://developers.google.com/health) — the successor to the Fitbit Web API, covering Fitbit, Pixel Watch and partner devices.

Single static Go binary, no runtime dependencies, ~15 MB container. Runs on stdio for local editors or over streamable HTTP for a cluster.

## Design

The server does **mechanical** work only:

- OAuth, token refresh and persistence
- Pagination, and chunking around the API's 14-day rollup ceiling
- Collapsing verbose data-point JSON into `date, value` pairs

It deliberately does **not** interpret. There are no baselines, thresholds or verdicts baked in — it returns numbers and the agent draws the conclusions. An agent asking about recovery already knows things the server can't: training load, symptoms, what happened last week. Hard-coding "resting HR is 5 bpm up, you may be ill" is both less accurate and un-changeable without a redeploy.

## Tools

| Tool | Purpose |
| --- | --- |
| `health_auth_status` | Is the server authorised, where is the token, when does it expire |
| `health_data_types` | All 32 data types plus the named presets |
| `health_daily_metrics` | Day-by-day values for one or more data types, as an aligned table |
| `health_list_datapoints` | Raw data points with a pass-through AIP-160 `filter` — escape hatch |

`health_daily_metrics` accepts either explicit `data_types` or a `preset`:

- **`recovery`** — `daily-resting-heart-rate`, `daily-heart-rate-variability`, `daily-sleep-temperature-derivations`, `daily-oxygen-saturation`, `sleep`
- **`training`** — `active-zone-minutes`, `active-energy-burned`, `total-calories`, `steps`, `exercise`
- **`body`** — `weight`, `body-fat`, `vo2-max`, `blood-glucose`

## Setup

Each user self-hosts and registers their own OAuth client, so their token never
leaves their machine. No app verification, no shared data custody.

### 1. Google Cloud (about five minutes, free)

You are not deploying anything to Google Cloud — the console is just where OAuth
clients are registered. No billing card is needed for consumer OAuth access.

1. Create or select a project at [console.cloud.google.com](https://console.cloud.google.com).
2. Enable the **Google Health API** (`health.googleapis.com`) from the API Library.
3. **OAuth consent screen** → User Type **External**. Leave publishing status on **Testing**.
4. Under **Test users**, click **+ Add users** and add your own Google account. Without this your own sign-in is rejected.
5. **Credentials → Create credentials → OAuth client ID → Web application**.
6. Under **Authorised redirect URIs** add exactly:

   ```
   http://127.0.0.1:3000/callback
   ```

   (Google's own quickstart suggests `https://www.google.com` — that is for the
   OAuth Playground, not for this server.)
7. Copy the **Client ID** and **Client Secret**.

> **Testing-mode caveat.** While publishing status is *Testing*, Google expires
> refresh tokens after 7 days, so you would re-run `login` weekly. Switching the
> status to *In production* — still unverified — is expected to lift that while
> keeping the "unverified app" warning screen and the 100-user cap. Worth
> confirming for your own project before relying on it.
>
> Health API scopes are *restricted*, so a publicly verified app would need an
> annual third-party CASA security assessment. Self-hosting sidesteps that
> entirely: an unverified client supports up to 100 users, and you only ever
> need one — yourself.

### 2. Authorise

```sh
export GOOGLE_CLIENT_ID=...
export GOOGLE_CLIENT_SECRET=...

go build -o health-mcp .
./health-mcp login
```

This opens a browser, and writes a token to `~/.config/health-mcp/token.json`
with mode `0600`. Override the location with `HEALTH_TOKEN_PATH`.

Scopes requested (all read-only — the server never writes health data):

```
googlehealth.activity_and_fitness.readonly
googlehealth.health_metrics_and_measurements.readonly
googlehealth.sleep.readonly
```

Check it worked:

```sh
./health-mcp serve -http :8080 &
curl -s localhost:8080/readyz     # "ready" once a usable token exists
```

### 3. Run

```sh
./health-mcp serve                    # stdio
./health-mcp serve -http :8080        # streamable HTTP at /mcp
```

Register with Claude Code:

```sh
claude mcp add google-health \
  --env GOOGLE_CLIENT_ID=... \
  --env GOOGLE_CLIENT_SECRET=... \
  -- /path/to/health-mcp serve
```

## Cluster deployment

```sh
docker build -t ghcr.io/viktorwelbers/google-health-mcp:latest .

kubectl create secret generic google-health-mcp \
  --from-literal=client-id="$GOOGLE_CLIENT_ID" \
  --from-literal=client-secret="$GOOGLE_CLIENT_SECRET" \
  --from-file=token.json="$HOME/.config/health-mcp/token.json"

kubectl apply -f k8s/deployment.yaml
```

Runs as non-root on `scratch` with a read-only root filesystem and all capabilities dropped.

- `/healthz` — process is alive
- `/readyz` — a usable token is present, so an unauthorised pod shows `NotReady` rather than failing silently
- `/mcp` — the MCP endpoint

The token is mounted read-only. That's intentional: only the short-lived access token rotates and it's kept in memory, while the refresh token never changes, so a restart re-derives a valid access token. The server logs one warning per rotation about being unable to persist — expected and harmless.

## API notes

Verified against the live API, because the reference docs leave these implicit:

**`civilTimeInterval` uses `start` / `end`** — not the `startTime`/`endTime` of
`google.type.Interval`, despite being documented as its counterpart.

**`daily-*` types do not support `dailyRollUp`.** They are already one point per
day, and the API rejects rollup with *"supported: list, reconcile"*. The client
picks the method per type: `List` for `daily-*`, `dailyRollUp` for the rest.

**Some numeric fields are quoted strings.** `daily-resting-heart-rate` returns
`"beatsPerMinute": "59"`. Number parsing accepts both.

**The date sits at a different depth per type.** Rollup points carry
`civilStartTime` at the top level; daily types nest `date` inside their value
object; sampled types bury it under `weight.sampleTime.civilTime.date`. Date
extraction searches recursively rather than assuming a path.

**Every numeric field is returned, not just one.** A data point often carries
several useful numbers — `daily-heart-rate-variability` has an average, a
deep-sleep RMSSD, an entropy value and a non-REM heart rate. All of them appear
in `values`; `primary_field` only chooses which one fills the rendered column,
so an unmapped type still returns its full data.

Observed field names:

| Data type | Primary field |
| --- | --- |
| `daily-resting-heart-rate` | `beatsPerMinute` |
| `daily-heart-rate-variability` | `averageHeartRateVariabilityMilliseconds` |
| `daily-oxygen-saturation` | `averagePercentage` |
| `daily-sleep-temperature-derivations` | `nightlyTemperatureCelsius` (compare against `baselineTemperatureCelsius` in the same point) |
| `weight` | `weightGrams` |

Use `health_list_datapoints` to inspect the raw shape of any type.

## Licence

MIT
