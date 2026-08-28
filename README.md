# google-health-mcp

An MCP server for the Google Health API v4 (Fitbit, Pixel Watch, and partner devices).

Single static Go binary with no runtime dependencies. Runs over stdio for local editors or streamable HTTP for remote deployments.

## Design

The server handles the transport and data flattening layer only:

* OAuth 2.0 flow, token refresh, and local storage
* Pagination and 14-day chunking limits
* Flattening raw data-point JSON into standard `(date, value)` pairs

It does not interpret, baseline, or score metrics. It exposes structured data to the model.

## Tools

| Tool | Purpose |
| --- | --- |
| `health_auth_status` | Returns auth status, token location, and expiry |
| `health_data_types` | Lists available data types (32 total) and presets |
| `health_daily_metrics` | Fetches daily metrics across types as an aligned table |
| `health_list_datapoints` | Raw data point escape hatch with AIP-160 filter support |

`health_daily_metrics` accepts explicit `data_types` or a preset:

* **`recovery`**: `daily-resting-heart-rate`, `daily-heart-rate-variability`, `daily-sleep-temperature-derivations`, `daily-oxygen-saturation`
* **`training`**: `active-zone-minutes`, `active-energy-burned`, `total-calories`, `steps`, `exercise`
* **`body`**: `weight`, `body-fat`, `vo2-max`

## Setup

Self-hosted OAuth. Tokens stay local on your machine.

### 1. Google Cloud Console

1. Create or select a project in [Google Cloud Console](https://console.cloud.google.com).
2. Enable the **Google Health API** (`health.googleapis.com`).
3. Configure the **OAuth consent screen** (User Type: External, Status: Testing).
4. Under **Test users**, add your Google email address.
5. Create credentials: **OAuth client ID** -> **Web application**.
6. Add the redirect URI:

   ```text
   http://127.0.0.1:3000/callback
   ```

7. Save the Client ID and Client Secret (or download the client credentials JSON).

> **Note:** In Testing mode, refresh tokens expire every 7 days. Switching the OAuth app status to "In Production" (unverified) avoids weekly re-auth while staying within the 100-user limit.

### 2. Authentication

Place your `client.json` in the default config location (or pass `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` via environment variables):

```sh
mkdir -p ~/.config/health-mcp
cp ~/Downloads/client_secret_*.json ~/.config/health-mcp/client.json
chmod 600 ~/.config/health-mcp/client.json

go build -o health-mcp .
./health-mcp login
```

This opens a browser flow and saves tokens to `~/.config/health-mcp/token.json` (0600). Override path via `HEALTH_TOKEN_PATH`.

Scopes requested (read-only):

* `googlehealth.activity_and_fitness.readonly`
* `googlehealth.health_metrics_and_measurements.readonly`
* `googlehealth.sleep.readonly`

Verify local status:

```sh
./health-mcp serve -http :8080 &
curl -s localhost:8080/readyz
```

### 3. Usage

```sh
# stdio mode
./health-mcp serve

# HTTP mode
./health-mcp serve -http :8080
```

Register with Claude Code:

```sh
claude mcp add google-health \
  --env GOOGLE_CLIENT_ID=... \
  --env GOOGLE_CLIENT_SECRET=... \
  -- /path/to/health-mcp serve
```

## Kubernetes / Container Deployment

Images are published to GHCR for `linux/amd64`. No build step needed:

```sh
docker pull ghcr.io/viktorwelbers/google-health-mcp:0.1.0
```

Tags:

* `0.1.0`, `0.1` — released versions, cut from `v*` git tags
* `latest` — current `main`, moves on every push
* `sha-<commit>` — immutable, one per build

Pin a version tag in deployments. To build from source instead:

```sh
docker build -t google-health-mcp:dev .
```

Authorise with `health-mcp login` first, then deploy:

```sh
kubectl create secret generic google-health-mcp \
  --from-literal=client-id="$GOOGLE_CLIENT_ID" \
  --from-literal=client-secret="$GOOGLE_CLIENT_SECRET" \
  --from-file=token.json="$HOME/.config/health-mcp/token.json"

kubectl apply -f k8s/deployment.yaml
```

Container characteristics:

* Runs non-root on `scratch`
* 4.2 MB compressed: two layers, the static binary and the CA roots
* Read-only root filesystem, dropped capabilities
* Endpoints: `/healthz` (liveness), `/readyz` (readiness/auth check), `/mcp` (streamable HTTP)

When running with a read-only token mount, access tokens refresh in memory while the mounted refresh token stays static. The server will log a harmless warning when failing to write refreshed credentials back to disk.

## API Notes & Edge Cases

Implementation quirks observed in Google Health API v4:

**Time interval schema:** `civilTimeInterval` expects `start` and `end` fields (differs from standard `google.type.Interval` `startTime`/`endTime`).

**Aggregation:** Types prefixed with `daily-*` do not support `dailyRollUp`. The client automatically routes requests to `List` for `daily-*` types and `dailyRollUp` for all others.

**Type parsing:** Numeric values may be returned as integers, floats, or quoted strings (e.g., `"beatsPerMinute": "59"`). Parsers handle both string and numeric types.

**Date location:** Date strings vary by metric type (`civilStartTime` top-level vs. nested inside value objects or sample times). Date extraction recursively resolves these structures.

**Field mapping:** All fields inside a point are exposed in `values`. `primary_field` determines which metric maps to the primary output column for table views.

| Data Type | Primary Field |
| --- | --- |
| `daily-resting-heart-rate` | `beatsPerMinute` |
| `daily-heart-rate-variability` | `averageHeartRateVariabilityMilliseconds` |
| `daily-oxygen-saturation` | `averagePercentage` |
| `daily-sleep-temperature-derivations` | `nightlyTemperatureCelsius` |
| `weight` | `weightGrams` |

Run `health_list_datapoints` to view the unmapped schema for any specific type.

## License

MIT
