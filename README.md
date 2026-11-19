# YouTube Subscription Cleanup

A small local dashboard (Go + Templ + HTMX + Tailwind) that pulls your YouTube
subscriptions, uses an OpenAI-compatible model to group them by **category** and
**kind of videos**, and suggests **keep / review / unsubscribe** for each one.
You can unsubscribe straight from the dashboard.

## Setup

1. **Google OAuth client**
   - In [Google Cloud Console](https://console.cloud.google.com/), create a project and enable **YouTube Data API v3**.
   - On the OAuth consent screen, choose External, then add yourself as a **test user**.
   - Under Credentials, create an OAuth client ID of type **Web application** with this redirect URI:
     `http://localhost:8080/auth/callback`
2. Copy `.env.example` to `.env` and fill in the Google client and AI settings.
3. Run:
   ```sh
   go tool templ generate
   go run .
   ```
   Open http://localhost:8080.

### Make

`make run` builds and runs the app. `make dev` runs it with live reload. `make help` lists every target.

### Docker

```sh
make docker-build
make docker-run   # uses .env and stores data in the "yt-subscriptions-data" volume
```

If you change the port (`make docker-run PORT=9090`), add the new redirect URI to your Google OAuth client too.

## Using it

1. **Connect YouTube**, then **Sync subscriptions**. This fetches channels, stats, and their recent uploads.
2. Optional: open *Personalize the suggestions*.
   - Describe your current interests.
   - Upload `watch-history.json` from [Google Takeout](https://takeout.google.com/) (YouTube, history, JSON format) so the AI knows what you actually watch.
3. Click **Analyze new**. Only channels that haven't been analyzed yet are sent. Use **Re-analyze all** after changing your interests or history.
4. Group by Category, Kind of videos, or Suggestion. Filter, search, and unsubscribe.

Everything (channels, AI analyses, watch stats, settings, OAuth token) is stored in SQLite at `data/app.db`.
Analyses are kept per channel in their own table, so syncing, unsubscribing or re-subscribing never throws them away.
Only **Re-analyze all** spends AI calls on channels that were already categorized.
If you used an earlier version that stored data in `state.json`/`token.json`, that data is imported automatically on first start.

## Notes

- **API quota:** a sync uses roughly 1 unit per channel. Each unsubscribe costs 50 units. The default daily quota is 10,000.
- **OAuth scope:** the app asks for the full `youtube` scope because unsubscribing requires it.
- **Weekly re-login:** while your Google Cloud app is in *Testing* status, Google expires its login after 7 days. The dashboard notices this, forgets the token and shows **Connect YouTube** again. Publishing the app (it can stay unverified for personal use) removes the limit. **Disconnect** in the header revokes the token.
- **Tailwind:** loaded with the Tailwind browser build to keep things simple. For anything beyond local use, switch to the Tailwind CLI.
- **Templates:** after editing `.templ` files, re-run `go tool templ generate`, or run `go tool templ generate --watch --proxy=http://localhost:8080 --cmd="go run ."` for live reload.
