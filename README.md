# YouTube Cleanup

A small, self-hosted dashboard for tidying your YouTube account with help from
any OpenAI-compatible model:

- **Subscriptions:** grouped by **category** and **kind of videos**, each with a
  **keep / review / unsubscribe** suggestion.
- **Playlists:** grouped by topic, each with a **keep / review / delete / merge**
  suggestion, plus deleted or private videos, duplicates and overlaps flagged.

You decide; the dashboard makes the changes for you.

Built with Go, [templ](https://templ.guide), [HTMX](https://htmx.org) and
Tailwind, with SQLite for storage. It ships as one binary with nothing else to install.

- **Using it:** read on.
- **Changing it or maintaining a fork:** [CONTRIBUTING.md](CONTRIBUTING.md) for
  setup and workflow, [ARCHITECTURE.md](ARCHITECTURE.md) for how the code fits together.

## Quick start

You need [Go](https://go.dev/dl/) (the version in `go.mod`), `make` and `curl`.

1. **Get a Google OAuth client** (one time, about 5 minutes):
   - In [Google Cloud Console](https://console.cloud.google.com/), create a project and enable **YouTube Data API v3**.
   - On the OAuth consent screen, choose External, then add yourself as a **test user**.
   - Under Credentials, create an OAuth client ID of type **Web application** with this redirect URI:
     `http://localhost:8080/auth/callback`
2. **Set up and configure:**
   ```sh
   make setup   # downloads the Tailwind CLI into bin/, creates .env, builds generated files
   ```
   Then fill in `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET` and the `AI_*` settings in `.env`.
   Every setting is described in [.env.example](.env.example).
3. **Run it:** `make run`, then open http://localhost:8080.

`make help` lists every target. Prefer Docker? `make docker-build && make docker-run` does the same in a container.

## Using it

### Subscriptions

1. **Connect YouTube**, then **Sync**. This fetches your channels, their stats and recent uploads.
2. Optional, recommended: open **Tune the suggestions**.
   - Describe what you're into now.
   - Import your watch history from [Google Takeout](https://takeout.google.com/) (YouTube → history). Both the default HTML export and the JSON export work. YouTube's API can't tell which channels you actually watch; this can.
3. Click **Analyze new**. It only sends channels that were never analyzed, or whose suggestion is **outdated** because your interests or watch history changed since. **Re-analyze all** redoes everything.
4. Group by category, kind of videos or suggestion; filter and search.
5. Decide:
   - **Keep** pins a channel. Your decision beats the AI's, survives re-syncs, and those channels are never re-analyzed. **Undo** clears it.
   - **Unsubscribe** on a row, or **Unsubscribe from all N** for every channel still marked Unsubscribe (channels you kept are skipped).

### Playlists

1. Open **Playlists** and click **Sync**. It imports every playlist and all of its videos (about 1 quota unit per 50 videos).
2. **Analyze new** asks the AI for a topic and a verdict per playlist, using the same interests as Subscriptions. A merge suggestion always names an existing target playlist.
3. Each row shows problems in red (unavailable videos, duplicates, empty) and, muted, the playlist it shares the most videos with. The **Has problems** filter shows only those.
4. Act:
   - **Keep** / **Undo**, as for subscriptions.
   - **Clean up** removes unavailable videos and duplicates from one playlist; **Clean up all playlists** does every one. Each removal is 50 units.
   - **Merge** copies the videos the target doesn't already have, deletes the source playlist, then re-syncs. The confirmation shows the exact cost.
   - **Delete** removes the playlist from YouTube (the videos themselves are unaffected).

YouTube's API doesn't expose Liked videos or Watch Later, so those aren't included.

## How it behaves

- **Storage:** everything lives in SQLite at `data/app.db` (`DATA_DIR`), created private to your user. Schema changes are numbered migrations. Data from the old `state.json`/`token.json` format is imported on first start.
- **Quota:** YouTube allows 10,000 API units a day by default (`YOUTUBE_DAILY_QUOTA`). Reading is cheap (a subscriptions sync is about 1 unit per channel; a playlists sync about 1 per 50 videos). Every change costs 50: an unsubscribe, a playlist deletion, and each video removed from or added to a playlist. The dashboard keeps a running estimate for the current quota day (midnight to midnight Pacific time), refuses actions that wouldn't fit, and stops bulk actions before the quota runs out. If YouTube reports the quota is used up mid-sync, nothing is overwritten.
- **Errors:** failed actions show as toasts. A sync that couldn't fetch uploads for a few channels keeps their previous data and says so.
- **Weekly re-login:** while your Google Cloud app is in *Testing* status, Google expires its login after 7 days. The dashboard notices, forgets the token and shows **Connect YouTube** again. Publishing the app (it can stay unverified for personal use) removes the limit. **Disconnect** revokes the token.
- **AI calls:** batches of 25 channels or 20 playlists, a few at a time, with retries for rate limits, server errors and items the model skipped. Set `AI_JSON_MODE=1` if your provider supports `response_format: json_object`. Channel and playlist titles and descriptions are treated as untrusted data in the prompt.

## Deploying

The image is a small distroless container (non-root, no shell) for `linux/amd64` and `linux/arm64`. It stores everything in SQLite on the `/data` volume, so **run exactly one instance** and back up that volume.

### On a server with Docker Compose (HTTPS included)

1. Point a DNS record (e.g. `subs.example.com`) at the server; open ports 80 and 443.
2. Copy `.env.example` to `.env` and set at least:
   - `PUBLIC_URL=https://subs.example.com`
   - `DASHBOARD_PASSWORD` (the app refuses to start publicly without one)
   - `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`, and the `AI_*` settings
3. In your Google OAuth client, add the redirect URI `https://subs.example.com/auth/callback`.
4. Start it:
   ```sh
   DOMAIN=subs.example.com docker compose --profile https up -d --build
   ```
   Caddy gets and renews the TLS certificate and proxies to the app. Without `--profile https`, the app is only reachable on `127.0.0.1:8080` of that machine.

The compose file runs the app read-only, with all capabilities dropped and `no-new-privileges`, and restarts it unless stopped. `make up` / `make down` wrap compose for local use.

### Docker, locally

```sh
make docker-build
make docker-run   # uses .env, publishes on 127.0.0.1 only, keeps data in the "yt-subscriptions-data" volume
```

If you change the port (`make docker-run PORT=9090`), add the new redirect URI to your Google OAuth client too.

### Anywhere else (Fly.io, Railway, Kubernetes, a VM...)

- **Image:** build with `docker build --build-arg VERSION=v1.0.0 .`, or push a tag (`git tag v1.0.0 && git push origin v1.0.0`) and the release workflow publishes `ghcr.io/<owner>/<repo>:1.0.0` and `:latest` for whichever GitHub repository (or fork) it runs in.
- **Port:** the container listens on `8080` (`PORT`).
- **Volume:** mount persistent storage at `/data`.
- **Health:** `GET /healthz` returns 200 when the database answers; it skips the password. The image's `HEALTHCHECK` already uses it.
- **TLS:** terminate HTTPS at your platform or proxy. Keep the original `Host` header (most proxies do); the cross-site request check compares it with the browser's `Origin`.
- **Shutdown:** `SIGTERM` stops new requests, cancels running jobs and closes the database within about 10 seconds.

## Security

The dashboard can unsubscribe you from channels and delete playlists, so:

- It listens on `127.0.0.1` unless you set `HOST`. `make docker-run` and the compose file publish on `127.0.0.1` only; the `https` profile puts Caddy in front.
- `DASHBOARD_PASSWORD` requires a password (HTTP basic auth, any username; use it only over HTTPS). On any non-loopback address the app **refuses to start** without it, unless you set `ALLOW_NO_PASSWORD=1` because access is restricted some other way.
- Requests from other websites are rejected (Go's `http.CrossOriginProtection`), and responses carry a strict Content-Security-Policy, `X-Frame-Options: DENY` and `nosniff`.
- The OAuth scope is the full `youtube` scope because unsubscribing requires it.

## Credits

Fonts: [Geist and Geist Mono](https://vercel.com/font), SIL Open Font License (`static/fonts/OFL.txt`).
