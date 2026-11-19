# YouTube Subscription Cleanup

A small local dashboard (Go + Templ + HTMX + Tailwind) that pulls your YouTube
subscriptions, uses an OpenAI-compatible model to group them by **category** and
**kind of videos**, and suggests **keep / review / unsubscribe** for each one.
You decide; the dashboard unsubscribes for you.

## Setup

1. **Google OAuth client**
   - In [Google Cloud Console](https://console.cloud.google.com/), create a project and enable **YouTube Data API v3**.
   - On the OAuth consent screen, choose External, then add yourself as a **test user**.
   - Under Credentials, create an OAuth client ID of type **Web application** with this redirect URI:
     `http://localhost:8080/auth/callback`
2. Copy `.env.example` to `.env` and fill in the Google client and AI settings.
3. Run `make run` and open http://localhost:8080.

`make run` builds the templates and CSS (it downloads the pinned Tailwind CLI into `bin/` the first time) and starts the server.
`make dev` does the same with live reload; open the proxy at http://localhost:7331.
`make help` lists every target.

### Docker

```sh
make docker-build
make docker-run   # uses .env, publishes on 127.0.0.1 only, keeps data in the "yt-subscriptions-data" volume
```

If you change the port (`make docker-run PORT=9090`), add the new redirect URI to your Google OAuth client too.

## Using it

1. **Connect YouTube**, then **Sync**. This fetches your channels, their stats and recent uploads.
2. Optional, recommended: open **Tune the suggestions**.
   - Describe what you're into now.
   - Import your watch history from [Google Takeout](https://takeout.google.com/) (YouTube → history). Both the default HTML export and the JSON export work. YouTube's API can't tell which channels you actually watch; this can.
3. Click **Analyze new**. It only sends channels that were never analyzed, or whose suggestion is **outdated** because your interests or watch history changed since. **Re-analyze all** redoes everything.
4. Group by category, kind of videos or suggestion; filter and search.
5. Decide:
   - **Keep** pins a channel. Your decision beats the AI's, survives re-syncs, and those channels are never re-analyzed. **Undo** clears it.
   - **Unsubscribe** on a row, or **Unsubscribe from all N** for every channel still marked Unsubscribe (channels you kept are skipped).

## How it behaves

- **Storage:** everything lives in SQLite at `data/app.db` (`DATA_DIR`), created private to your user. Schema changes are numbered migrations. Data from the old `state.json`/`token.json` format is imported on first start.
- **Quota:** YouTube allows 10,000 API units a day by default (`YOUTUBE_DAILY_QUOTA`). A sync costs about 1 unit per channel and each unsubscribe costs 50. The dashboard keeps a running estimate for the current quota day (midnight to midnight Pacific time), refuses unsubscribes that wouldn't fit, and stops a bulk unsubscribe before the quota runs out. If YouTube reports the quota is used up mid-sync, nothing is overwritten.
- **Errors:** failed actions show as toasts. A sync that couldn't fetch uploads for a few channels keeps their previous data and says so.
- **Weekly re-login:** while your Google Cloud app is in *Testing* status, Google expires its login after 7 days. The dashboard notices, forgets the token and shows **Connect YouTube** again. Publishing the app (it can stay unverified for personal use) removes the limit. **Disconnect** revokes the token.
- **AI calls:** batches of 25 channels, a few at a time, with retries for rate limits, server errors and channels the model skipped. Set `AI_JSON_MODE=1` if your provider supports `response_format: json_object`. Channel titles and descriptions are treated as untrusted data in the prompt.

## Security

The dashboard can unsubscribe you from channels, so:

- It listens on `127.0.0.1` unless you set `HOST`. `make docker-run` publishes on `127.0.0.1` only.
- Requests from other websites are rejected (Go's `http.CrossOriginProtection`).
- Set `DASHBOARD_PASSWORD` to require a password (HTTP basic auth, any username). The app warns at startup if it's reachable from other machines without one.
- The OAuth scope is the full `youtube` scope because unsubscribing requires it.

## Development

- `make check` formats, vets, tests and builds.
- Templates are in `views/*.templ`; run `make generate` (or `make dev`) after editing them. Styles and design tokens are in `styles/input.css`; `make css` rebuilds `static/app.css`. Commit the generated files: CI fails if they're out of date.
- Fonts: [Geist and Geist Mono](https://vercel.com/font), SIL Open Font License (`static/fonts/OFL.txt`).
