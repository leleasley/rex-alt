# Verification checklist

Use this after deploying Rex-Alt. Replace `<host>` with the Docker host, and use the
same shell that has your `.env` loaded (`set -a; . ./.env; set +a`) so `$REX_URL`,
`$API_TOKEN` and `$REX_SOURCE_*` are available.

Record actual values in the blank fields — this document is a template, not a report.

## 0. Before you start

- [ ] REX media deletion is OFF. `GET /:/prefs` shows `allowMediaDeletion` with `value="0"`.
      If not: `PUT /:/prefs?allowMediaDeletion=0` and read it back.
- [ ] `REX_SOURCE_USER` / `REX_SOURCE_PASSWORD` are the **Altmount WebDAV** credentials
      (REX uses them to fetch bytes), not arbitrary values.
- [ ] `ALTMOUNT_WEBDAV_URL` ends in `/webdav/` (trailing slash required).
- [ ] Note your baseline: other REX sources, Plex libraries, and (if you use Zurg) the
      mount point and `__magic__/__all__` counts.

| Check | Before | After |
| --- | --- | --- |
| REX sources present | `____` | `____` |
| Plex libraries present | `____` | `____` |
| Zurg `__magic__/__all__` count (if applicable) | `____` | `____` |

## 1. Container and source registration

```bash
docker compose ps
curl -sS http://<host>:9982/api/v1/health
curl -sS -u "$REX_SOURCE_USER:$REX_SOURCE_PASSWORD" "$REX_URL/rex/sources/rex-alt"
```

| Check | Expected | Observed |
| --- | --- | --- |
| Container status | Up | `____` |
| Health | `{"status":"ok","version":"..."}` | `____` |
| Source name | `rex-alt` | `____` |
| Directories | `movies` → `Altmount Movies`, `tv` → `Altmount TV` | `____` |
| Auth kind | `basic` (never the credential value) | `____` |

## 2. Pilot pass

Set small caps in `.env`, recreate, and run one pass:

```dotenv
SYNC_MAX_MOVIES=10
SYNC_MAX_SERIES=2
```

```bash
docker compose up -d
curl -sS -H "Authorization: Bearer $API_TOKEN" -X POST http://<host>:9982/api/v1/sync
curl -sS -H "Authorization: Bearer $API_TOKEN" http://<host>:9982/api/v1/status
curl -sS "$REX_URL/rex/feed/rex-alt/items"
curl -sS "$REX_URL/rex/feed/rex-alt/held"
```

| Check | Expected | Observed |
| --- | --- | --- |
| Entries in catalogue | > 0 | `____` |
| Feed items | > 0 | `____` |
| Held items | 0 or small (held items are retried) | `____` |
| Plex libraries | `Altmount Movies` and `Altmount TV` exist | `____` |
| Titles | Recognisable TMDB titles, not raw filenames | `____` |

- [ ] Play one movie and one episode in a Plex client. Bytes must stream from Altmount WebDAV.
- [ ] Check it again in Plex Web and a native app if possible.

## 3. Delete safety gate

With a throwaway item from this source:

```bash
curl -sS -o /dev/null -w '%{http_code}\n' -X DELETE \
  -H "X-Plex-Token: <your Plex token>" \
  "$REX_URL/library/metadata/<ratingKey>"
```

| Check | Expected | Observed |
| --- | --- | --- |
| HTTP status | `403` | `____` |
| Altmount file still present | yes | `____` |
| REX feed item still present | yes | `____` |

If the delete succeeds, stop: turn `allowMediaDeletion` off before going further.

## 4. Full pass

Remove the caps (`SYNC_MAX_MOVIES=0`, `SYNC_MAX_SERIES=0`), recreate, and run off-peak:

```bash
docker compose up -d
curl -sS -H "Authorization: Bearer $API_TOKEN" -X POST http://<host>:9982/api/v1/sync
```

| Check | Expected | Observed |
| --- | --- | --- |
| Probed / pushed items | grows over the pass | `____` |
| Resource use | below 0.5 CPU / 512 MB | `____` |
| Log size | below ~30 MB (rotated) | `____` |
| Other REX sources / Plex libraries | unchanged | `____` |

## 5. Resource and additive checks

```bash
docker stats rex-alt --no-stream
docker compose logs --tail 50 rex-alt
```

- [ ] CPU and memory stay within the Compose limits.
- [ ] No media bytes appear in the `rex-alt-data` volume (only the SQLite database).
- [ ] No other container is restarted or reconfigured.
- [ ] If you use Zurg: its source, mount and library counts are unchanged.

## 6. Rollback test (optional)

```bash
docker compose down
curl -sS -u "$REX_SOURCE_USER:$REX_SOURCE_PASSWORD" -X DELETE "$REX_URL/rex/sources/rex-alt"
curl -sS -u "$REX_SOURCE_USER:$REX_SOURCE_PASSWORD" -X DELETE "$REX_URL/rex/sources/rex-alt"
curl -sS -o /dev/null -w '%{http_code}\n' -u "$REX_SOURCE_USER:$REX_SOURCE_PASSWORD" \
  "$REX_URL/rex/sources/rex-alt"   # expect 404
```
