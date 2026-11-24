# Live verification checklist — Rex-Alt

This is the record for the live checks against REX / Altmount. **The controller runs these checks; this file contains no results yet.** Fill every blank field with the observed value at the time of the check. Do not guess or copy expected values into the observed fields.

Placeholders used below:

- `<host>:9982` — host and admin API port of the rex-alt container (`APP_PORT`, default `9982`).
- `<REX admin user>` / `<REX admin password>` — REX admin credentials (never rex-alt's `.env` values).
- `<REX source user>` / `<REX source password>` — values of `REX_SOURCE_USER` / `REX_SOURCE_PASSWORD`.
- `$REX_URL`, `$API_TOKEN`, `$ALTMOUNT_WEBDAV_URL`, `$ALTMOUNT_WEBDAV_USER`, `$ALTMOUNT_WEBDAV_PASSWORD` — variables from the deployed `.env` (source them in your shell; never paste their values here).
- `____` — an observed value to be filled in.

---

## 0. Baseline — capture before registering / starting rex-alt

```bash
cd /opt/rex-alt

# Running containers
docker ps

# Zurg mount (record the filesystem/mount line)
findmnt -T /mnt/zurg-embedded/zurg

# Zurg __magic__/__all__ item count (confirm the mount path on this host first)
find /mnt/zurg-embedded/zurg/__magic__/__all__ -type f | wc -l

# Zurg source as REX sees it
curl -sS -u "<REX source user>:<REX source password>" "$REX_URL/rex/sources/zurg"

# REX media-deletion preference — must be off/0 BEFORE registration
curl -sS -u "<REX admin user>:<REX admin password>" "$REX_URL/:/prefs?allowMediaDeletion"
```

Record baseline:

| Field | Observed value |
| --- | --- |
| Date/time of baseline | `____` |
| `docker ps` container list | `____` |
| `findmnt -T /mnt/zurg-embedded/zurg` | `____` |
| Zurg `__magic__/__all__` file count | `____` |
| `GET /rex/sources/zurg` response | `____` |
| `allowMediaDeletion` value (expect `0`) | `____` |

- [ ] Baseline captured. `allowMediaDeletion` is **off** (`0`). **If it is not off, STOP — do not register the source.**

---

## 1. One-file spike (gate)

Settings for the spike (in `.env`, then recreate the container):

```dotenv
SYNC_MAX_MOVIES=1
SYNC_MAX_SERIES=0
REX_SEND_TMDB_IDS=false
PROBE_TIMEOUT=45s
```

```bash
cd /opt/rex-alt
docker compose up -d
docker compose logs -f rex-alt
```

- [ ] `allowMediaDeletion` re-confirmed off immediately before registration (step 0 command repeated): observed `____`.

Trigger one pass and inspect the outcome:

```bash
curl -sS -H "Authorization: Bearer $API_TOKEN" -X POST http://<host>:9982/api/v1/sync
curl -sS -H "Authorization: Bearer $API_TOKEN" http://<host>:9982/api/v1/status
curl -sS -u "<REX source user>:<REX source password>" "$REX_URL/rex/sources/rex-alt"
```

Record spike results:

| Field | Observed value |
| --- | --- |
| `POST /api/v1/sync` HTTP status / body | `____` |
| `GET /api/v1/status` — `entries` | `____` |
| `GET /api/v1/status` — `last_stats.scanned` | `____` |
| `GET /api/v1/status` — `last_stats.probed` | `____` |
| `GET /api/v1/status` — `last_stats.pushed` | `____` |
| `GET /api/v1/status` — `last_stats.held` (expect `0` for the probed item) | `____` |
| `GET /rex/sources/rex-alt` response (name/directories) | `____` |
| Time taken for the one-file pass | `____` |

- [ ] **Item appears in Plex (not held).** Library: `____`, item title: `____`, visible: `____`.

Confirm a cold ranged probe returns `206` (run against the same file the spike probed, substituting its real relative path):

```bash
curl -sS -o /dev/null -D - \
  -u "$ALTMOUNT_WEBDAV_USER:$ALTMOUNT_WEBDAV_PASSWORD" \
  -H 'Range: bytes=0-4095' \
  "$ALTMOUNT_WEBDAV_URL/movies/<release>/<file>" | head -n 20
```

| Field | Observed value |
| --- | --- |
| HTTP status (expect `206 Partial Content`) | `____` |
| `Content-Range` header | `____` |
| Time to first byte (cold file) | `____` |

- [ ] **206 cold probe works.**

- [ ] **Bytes play in Plex.** Item played: `____`, playback OK: `____`, observed source of bytes: `____`.

Test that a client delete is refused and sends no WebDAV `DELETE`. Use the client-delete request your Plex/REX client sends for a source item (adapt the path to this REX build), then inspect Altmount's WebDAV access log for the duration of the test:

```bash
# Attempt a client delete of the test item via REX (adapt endpoint/body to this REX build):
# curl -sS -o /dev/null -w '%{http_code}\n' -u "<client user>:<client password>" \
#   -X DELETE "<REX client-delete request for the test item>"

# Then check Altmount WebDAV logs for any DELETE to /webdav/ during the test window:
# <inspect Altmount WebDAV access log / request capture here>
```

| Field | Observed value |
| --- | --- |
| Client delete HTTP status (expect `403`) | `____` |
| Any WebDAV `DELETE` to `/webdav/` observed (expect none) | `____` |
| Log/source inspected | `____` |

- [ ] **Client delete returns 403 and no WebDAV `DELETE` is sent.** **If not, STOP and escalate — do not proceed.**

---

## 2. Full pilot and resource check

Settings:

```dotenv
SYNC_MAX_MOVIES=10
SYNC_MAX_SERIES=2
REX_SEND_TMDB_IDS=false
PROBE_TIMEOUT=45s
```

```bash
cd /opt/rex-alt
docker compose up -d
curl -sS -H "Authorization: Bearer $API_TOKEN" -X POST http://<host>:9982/api/v1/sync
curl -sS -H "Authorization: Bearer $API_TOKEN" http://<host>:9982/api/v1/status

# Resource ceiling check during/after the probe pass
docker stats rex-alt --no-stream
```

Record pilot results:

| Field | Observed value |
| --- | --- |
| `last_stats.scanned` / `pushed` / `held` / `rejected` | `____` |
| `docker stats rex-alt --no-stream` CPU % (ceiling 0.5 CPU) | `____` |
| `docker stats rex-alt --no-stream` MEM (ceiling 512 MB) | `____` |
| Items visible in Plex (movies / TV) | `____` / `____` |

- [ ] `docker stats rex-alt --no-stream` stays under **0.5 CPU / 512 MB**.

Confirm the existing stack is unchanged (repeat the step 0 commands):

```bash
docker ps
findmnt -T /mnt/zurg-embedded/zurg
find /mnt/zurg-embedded/zurg/__magic__/__all__ -type f | wc -l
curl -sS -u "<REX source user>:<REX source password>" "$REX_URL/rex/sources/zurg"
```

| Field | Before | After |
| --- | --- | --- |
| `docker ps` container list | `____` | `____` |
| `findmnt -T /mnt/zurg-embedded/zurg` | `____` | `____` |
| Zurg `__magic__/__all__` file count | `____` | `____` |
| `GET /rex/sources/zurg` response | `____` | `____` |
| `allowMediaDeletion` still off | `____` | `____` |

- [ ] Zurg count, `GET /rex/sources/zurg`, and `docker ps` unchanged.

Confirm no media bytes are stored in the add-on's volume:

```bash
docker run --rm -v rex-alt-data:/data alpine du -ah /data | sort -h | tail -n 20
```

| Field | Observed value |
| --- | --- |
| Largest files in `rex-alt-data` (expect only SQLite DB/WAL/SHM) | `____` |

---

## 3. Remove caps — full pass off-peak

```dotenv
SYNC_MAX_MOVIES=0
SYNC_MAX_SERIES=0
```

```bash
cd /opt/rex-alt
docker compose up -d
# Run off-peak (first pass over ~345 files takes ~25-45 min due to 13-24s cold starts):
curl -sS -H "Authorization: Bearer $API_TOKEN" -X POST http://<host>:9982/api/v1/sync
curl -sS -H "Authorization: Bearer $API_TOKEN" http://<host>:9982/api/v1/status
```

| Field | Observed value |
| --- | --- |
| Date/time full pass started | `____` |
| `last_stats.scanned` (expect ~345) | `____` |
| `last_stats.pushed` / `held` / `rejected` | `____` / `____` / `____` |
| Total wall time | `____` |
| `docker stats rex-alt --no-stream` CPU / MEM at peak | `____` / `____` |

- [ ] Full pass completed off-peak; existing stack still unchanged (re-run section 2 checks).

---

## 4. Rollback sanity (only if rolling back)

```bash
cd /opt/rex-alt
docker compose down
curl -sS -u "<REX source user>:<REX source password>" -X DELETE "$REX_URL/rex/sources/rex-alt"
curl -sS -u "<REX source user>:<REX source password>" -X DELETE "$REX_URL/rex/sources/rex-alt"
curl -sS -o /dev/null -w '%{http_code}\n' -u "<REX source user>:<REX source password>" \
  "$REX_URL/rex/sources/rex-alt"
```

| Field | Observed value |
| --- | --- |
| `GET /rex/sources/rex-alt` after two DELETEs (expect `404`) | `____` |
| Two Altmount Movies / Altmount TV libraries removed from Plex | `____` |
| Zurg / stack unchanged | `____` |

---

## Summary (fill at the end)

| Gate | Result |
| --- | --- |
| One-file spike (visible, 206, playback, delete 403, no WebDAV DELETE) | `____` |
| Full pilot (caps 10/2, resources within ceiling) | `____` |
| Existing stack unchanged | `____` |
| Full uncapped pass off-peak | `____` |
| Notes / concerns | `____` |
