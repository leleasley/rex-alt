![Rex-Alt logo](assets/logo.png)

# Rex-Alt

Rex-Alt is a small, self-contained Docker add-on that publishes the media already sitting on an **Altmount** WebDAV share to **REX** — and therefore to Plex — as an independent REX source.

It walks the Altmount WebDAV share, looks up titles and artwork on TMDB, probes each file with `ffprobe`, and pushes **metadata only** to REX. REX then fetches the media bytes directly from Altmount WebDAV itself. Rex-Alt never stores, copies or proxies media, and it does not need access to your FUSE mount or your Altmount API key.

If you also run Zurg, it is untouched: Rex-Alt is simply a second source beside it.

## What it is / what it is not

**It is:**

- A REX source add-on registered under the stable source name `rex-alt`.
- A publisher of two libraries: **Altmount Movies** (item directory `movies`) and **Altmount TV** (item directory `tv`).
- A metadata-only bridge: WebDAV `PROPFIND` for enumeration, TMDB for titles/artwork, `ffprobe` for `media_info`, and the REX source/feed API for delivery.
- Deployable from a published multi-arch image (`linux/amd64`, `linux/arm64`) with an image-only Compose service — no cloning or building required.

**It is not:**

- **Not a replacement for Zurg.** Other REX sources keep feeding REX exactly as before.
- **Not a media store, downloader or proxy.** No media bytes are copied or written by this container. Its volume holds only a small SQLite database of ids, paths and cached probes.
- **Not an Altmount configuration tool.** It only reads Altmount's WebDAV share.
- **Not an API-key integration.** Enumeration uses WebDAV **Basic auth only**; the Altmount API key does not work here.
- **Not a delete enabler.** REX media deletion must be off before the source is registered (see [Prerequisites](#prerequisites)).

## How it works

```
  Plex clients
       |  play bytes
       v
  +----------+        metadata + ids         +-------------------------+
  |   REX    | <----------------------------- | rex-alt (this add-on)   |
  | rex-alt  |      source/feed API           | - enumerate WebDAV      |
  | (others) |                                | - TMDB lookup (cached)  |
  +----+-----+                                | - ffprobe media_info    |
       |                                      | - SQLite state + API    |
       | bytes fetched directly               +------------+------------+
       | from Altmount WebDAV                               | reads only
       v                                                    v
  +------------------+                            +--------------------+
  | Altmount WebDAV  | <-------------------------- | /webdav/movies/    |
  | Basic auth       |      PROPFIND + GET        | /webdav/tv/        |
  +------------------+                            +--------------------+
```

The container talks to REX at `REX_URL`, to Altmount at `ALTMOUNT_WEBDAV_URL`, and to TMDB. Its admin API listens on `APP_PORT` (default `9982`) and is intended for your LAN only.

## Prerequisites

1. **REX** reachable from the Docker host, with a build that supports the source/feed API. You need its base URL (for example `http://rex:32400`) and the credentials that REX will use when fetching bytes from Altmount (`REX_SOURCE_USER` / `REX_SOURCE_PASSWORD`). These must be the **Altmount WebDAV** credentials — REX itself uses them to fetch each file. If they are placeholders or wrong, playback fails with `401`/upstream errors even though metadata looks fine.

2. **Altmount WebDAV credentials.** The WebDAV base URL **must end in `/webdav/`**, for example `http://altmount:8090/webdav/`, plus the Basic-auth username/password (`ALTMOUNT_WEBDAV_USER` / `ALTMOUNT_WEBDAV_PASSWORD`). `http://host:8090` or `http://host:8090/webdav` (no trailing slash) will be rejected. Never put credentials in the URL.

3. **A TMDB credential.** Either a v4 read-access token (`TMDB_READ_ACCESS_TOKEN`, preferred) or a v3 API key (`TMDB_API_KEY`). One is enough; it supplies titles, summaries, dates, posters and backdrops. Matching scores the first few TMDB search hits, so real-world release names (including dated daily shows such as wrestling programmes) are handled without exact filename matches.

4. **Docker with Compose**, with outbound HTTP from the container to REX, Altmount and TMDB.

5. **REX media deletion OFF (one-time).** With it on, REX forwards a Plex client's delete of a source item as an HTTP `DELETE` to that source's URL — here, a WebDAV `DELETE` against Altmount. Turning it off makes REX answer `403` and never send the `DELETE`.

   - **UI:** in REX, **Settings → Library → Allow media deletion** — uncheck it.
   - **API:** `PUT /:/prefs?allowMediaDeletion=0` against REX, then read `/:/prefs` back and confirm `value="0"`.

   Do this **before** registering the `rex-alt` source.

## Quick start

### 1. Create `.env`

In your project directory:

```bash
cp .env.example .env

openssl rand -hex 24      # -> API_TOKEN
openssl rand -base64 32   # -> ENCRYPTION_KEY (base64 of exactly 32 random bytes)
```

Fill in at least: `API_TOKEN`, `ENCRYPTION_KEY`, `REX_URL`, `REX_SOURCE_USER`, `REX_SOURCE_PASSWORD`, `ALTMOUNT_WEBDAV_URL`, `ALTMOUNT_WEBDAV_USER`, `ALTMOUNT_WEBDAV_PASSWORD`, and one TMDB credential.

`.env` is gitignored — never commit it or paste real credentials into issues or chat.

### 2. Add the Compose service

Use the provided image-only `docker-compose.yml`, or copy just this stanza into your existing Compose file (adjust the port, volume and `env_file` path for that project):

```yaml
services:
  rex-alt:
    image: ghcr.io/leleasley/rex-alt:v0.1.9
    container_name: rex-alt
    restart: unless-stopped
    env_file: .env
    ports: ["9982:9982"]
    volumes: ["rex-alt-data:/data"]
    pids_limit: 64
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"
    deploy:
      resources:
        limits: { cpus: "0.5", memory: 512M, pids: 64 }
    mem_limit: 512m
    memswap_limit: 512m
    read_only: true
    tmpfs: ["/tmp:size=16m"]
volumes:
  rex-alt-data:
```

Keep it **image-only** (no `build:`). Configuration is read at startup: after editing `.env`, recreate with `docker compose up -d`.

### 3. Start it

```bash
docker compose up -d
docker compose ps
docker compose logs -f rex-alt    # Ctrl-C to stop following
```

### 4. Verify

```bash
curl -sS http://<host>:9982/api/v1/health
# {"status":"ok","version":"v0.1.9"}

curl -sS -u "$REX_SOURCE_USER:$REX_SOURCE_PASSWORD" \
  "$REX_URL/rex/sources/rex-alt"
# A JSON object with "name": "rex-alt" and the movies/tv directories.
```

In Plex you should see two new libraries, **Altmount Movies** and **Altmount TV**. Registration and the first enumeration happen in the background, so allow a short moment. For more detail:

```bash
curl -sS -H "Authorization: Bearer $API_TOKEN" http://<host>:9982/api/v1/status
curl -sS -H "Authorization: Bearer $API_TOKEN" -X POST http://<host>:9982/api/v1/sync
```

`POST /sync` runs one pass immediately. `GET /status` reports catalogue counts and recent results.

### 5. Pilot first, then go unlimited

Start with a small cap so only a handful of files are probed and pushed:

```dotenv
SYNC_MAX_MOVIES=10
SYNC_MAX_SERIES=2
REX_SEND_TMDB_IDS=false
```

Recreate, trigger a sync, and check that a few items appear in both libraries and play their bytes from Altmount. Then remove the limits:

```dotenv
SYNC_MAX_MOVIES=0
SYNC_MAX_SERIES=0
```

**Cold-start timing:** an item whose probe is missing or fails validation is **held invisible** and retried on a later sync — it is never pushed as a broken row. A first full pass is slow because a cold Altmount file can take 13–24 seconds to return its first bytes; for a few hundred files this can be tens of minutes. Run the first uncapped pass off-peak (optionally set `SYNC_START_HOUR` to a quiet hour).

### TMDB ids and merging (`REX_SEND_TMDB_IDS`)

Default **`false`**: full display metadata is sent but no TMDB/IMDb ids, so items stay in their own `rex-alt` libraries. With it on, ids and episode ids are attached so Plex names and artworks each item; duplicate titles may merge into rows already present from another source. Keep it off for the pilot; enable it deliberately after observing the merge behaviour you want.

## Configuration

Credentials always live in their own variables, never in a URL. See `.env.example` for the annotated template.

| Variable | Default | Notes |
| --- | --- | --- |
| `API_TOKEN` | *(required)* | Bearer token for the admin API; ≥16 chars. |
| `ENCRYPTION_KEY` | *(required)* | Base64 of exactly 32 random bytes. |
| `REX_URL` | *(required)* | REX base URL, e.g. `http://rex:32400`; http(s), no userinfo. |
| `REX_SOURCE_USER` / `REX_SOURCE_PASSWORD` | empty | Credentials REX uses to fetch bytes from Altmount WebDAV. Must be the WebDAV credentials. |
| `REX_SOURCE_NAME` | `rex-alt` | Stable source name; change only with care. |
| `REX_SEND_TMDB_IDS` | `false` | Attach TMDB/IMDb ids for Plex naming/artwork. See above. |
| `ALTMOUNT_WEBDAV_URL` | *(required)* | WebDAV base URL ending in `/webdav/`, e.g. `http://altmount:8090/webdav/`. |
| `ALTMOUNT_WEBDAV_USER` / `ALTMOUNT_WEBDAV_PASSWORD` | empty | WebDAV Basic auth. Aliases `ALTMOUNT_USER` / `ALTMOUNT_PASSWORD` are accepted. |
| `TMDB_READ_ACCESS_TOKEN` | empty | v4 read-access token (preferred). Alias `TMDB_READ_TOKEN` accepted. |
| `TMDB_API_KEY` | empty | v3 API key alternative. One TMDB credential is enough. |
| `PUBLIC_URL` | `ALTMOUNT_WEBDAV_URL` | URL REX fetches bytes from, if different from the WebDAV base. |
| `APP_PORT` | `9982` | Admin API port. |
| `PROBE_WORKERS` | `4` | Concurrent `ffprobe` workers, 1–8. |
| `PROBE_TIMEOUT` | `45s` | Probe timeout; minimum/default 45s for cold files. |
| `HTTP_TIMEOUT` | `15s` | Per-request HTTP timeout. |
| `SYNC_INTERVAL` | `6h` | Recurring sync interval. |
| `SYNC_START_HOUR` | `-1` | Hour 0–23 to start syncs, or `-1` for no gate. |
| `SYNC_MAX_MOVIES` / `SYNC_MAX_SERIES` | `0` / `0` | Pilot caps; `0` means unlimited. |
| `PUSH_BATCH` | `100` | Items per REX feed POST (large batches are split for REX's 16 MiB cap). |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `DATABASE_PATH` | `/data/rex-alt.db` | SQLite path inside the container. |

## Resource and safety notes

- Compose caps the container at **0.5 CPU / 512 MB**, `pids_limit: 64`, a read-only root filesystem and a small `/tmp` tmpfs.
- Logs rotate at **10 MB × 3 files** (~30 MB ceiling).
- **No media bytes are stored.** The only persistent state is the small SQLite database. `ffprobe` children are short-lived and bounded by `PROBE_TIMEOUT`.
- Feed pushes are batched and split to stay under REX's 16 MiB body cap.
- **Deletion safety:** keep `allowMediaDeletion` off on REX, otherwise a Plex delete becomes a WebDAV `DELETE` against Altmount.
- Run the first uncapped sync off-peak; cold starts are slow but harmless.

## Troubleshooting

- **Container exits immediately.** Config fails closed. Check `docker compose logs rex-alt`: required values, `API_TOKEN` ≥16 chars, `ENCRYPTION_KEY` = base64 of 32 bytes, `PROBE_WORKERS` 1–8, `PROBE_TIMEOUT` ≥45s.
- **Health OK but no source/libraries.** The first pass is asynchronous — check `/api/v1/status` and trigger `POST /sync`. Verify `REX_URL` and that the container can reach REX.
- **WebDAV enumeration fails.** Confirm `ALTMOUNT_WEBDAV_URL` ends in `/webdav/` and the Basic-auth user/password are correct. The API key cannot be used here.
- **Everything is held / items invisible.** Held means no valid `media_info`. Check WebDAV GETs return `206` with real bytes, Basic auth works, and `PROBE_TIMEOUT` is long enough. Held items are retried automatically.
- **Playback fails although items appear.** Rex-Alt publishes metadata only; bytes come from Altmount WebDAV through REX. Check REX can reach the WebDAV base URL and that Range requests work. If playback returns upstream `401`, `REX_SOURCE_USER`/`REX_SOURCE_PASSWORD` are not the Altmount WebDAV credentials.
- **Admin API 401/429.** 401: missing/incorrect `Authorization: Bearer $API_TOKEN`. 429: the 120-per-minute per-IP rate limit — wait and retry.

## Rollback

```bash
docker compose down
# add --volumes to also delete the SQLite state

# Two DELETE calls are required by REX to remove the source:
curl -sS -u "$REX_SOURCE_USER:$REX_SOURCE_PASSWORD" -X DELETE "$REX_URL/rex/sources/rex-alt"
curl -sS -u "$REX_SOURCE_USER:$REX_SOURCE_PASSWORD" -X DELETE "$REX_URL/rex/sources/rex-alt"
# Expect 404 from a GET afterwards.
```

Removing the source from REX removes its two libraries from Plex. Other REX sources are never touched.

## Attribution and licence

The REX client, item mapping, probe and store core are adapted from [rex-xstream](https://github.com/leleasley/rex-xstream). Enumeration targets [AltMount](https://github.com/kipsilabs/altmount) WebDAV. Rex-Alt is released under the **MIT License** — see [LICENSE](LICENSE).
