# Architecture

How the code fits together, and where to make common changes. For setup and
workflow, see [CONTRIBUTING.md](CONTRIBUTING.md).

## The big picture

```
browser (HTMX)
   │  GET pages, POST/DELETE actions, poll GET /job
   ▼
internal/server ── middleware: /healthz, security headers, password, CSRF
   │  handlers ──► views (templ) ──► HTML (full pages or fragments)
   │
   ├──► internal/store    SQLite: channels, playlists, analyses, decisions, token, quota
   ├──► internal/youtube  YouTube Data API v3 (+ Takeout watch-history parser)
   └──► internal/ai       OpenAI-compatible chat completions (prompts live here)
```

The server renders HTML on the server and the browser swaps it in with HTMX.
There is no JSON API and no client-side framework. `static/app.js` does two
small things: it turns failed requests into toasts, and it runs row selection
for bulk actions (see below).

## Layout

```
cmd/yt-subscriptions/   main: flags, wiring, graceful shutdown. No logic.
internal/
  config/               every environment variable, its default and validation
  server/               HTTP: routes, handlers, background jobs, auth
    server.go             Server, Options, the route table, shared helpers
    middleware.go         password, CSRF, security headers, /healthz, static files
    jobs.go               the single background-job slot and its progress banner
    auth.go               Google sign-in, token refresh and storage
    subscriptions.go      Subscriptions page and actions
    playlists.go          Playlists page and actions
    analyze.go            batched, retried AI analysis used by both pages
  store/                SQLite persistence; migrations.go holds the schema
  youtube/              YouTube API client; takeout.go parses watch history
  ai/                   prompts, output parsing and validation
views/                  templ components (*.templ) and view models (*.go)
  layout.*                page shell, header, notices, job banner, shared bits
  subscriptions.*         Subscriptions page
  playlists.*             Playlists page
static/                 embedded assets: generated app.css, app.js, htmx, fonts
styles/input.css        Tailwind source and design tokens (colors, fonts)
deploy/, Dockerfile, docker-compose.yml   container and HTTPS deployment
```

Each feature is split the same way across layers: `server/playlists.go`,
`views/playlists.go`, `views/playlists.templ`, `store/playlists.go`,
`youtube/playlists.go`, `ai/playlists.go`. To understand a feature, open its
file in each layer.

Dependencies only point downward: `server` uses `views`, `store`, `youtube`
and `ai`; `youtube`, `ai` and `views` use `store` types; `store` depends on
none of them.

## How a request works

1. **Middleware** (`server/middleware.go`), outermost first: `/healthz` is
   answered before anything else; every response gets security headers;
   then the dashboard password (if set) and Go's cross-origin protection.
2. **Route table** (`Server.routes` in `server/server.go`): every URL is listed
   there, grouped by page.
3. **Handler** reads from the store, maybe calls YouTube, and renders a templ
   component.

HTMX conventions used throughout:

- **Fragments.** Actions return only the HTML that changed, such as a row. The
  summary at the top is sent alongside with `hx-swap-oob` (see `Summary(stats, true)`).
- **Empty body = remove.** Unsubscribe and delete return no row, so HTMX removes it.
- **`HX-Refresh: true`** reloads the page when too much changed to patch.
- **Errors** go through `fail(w, status, title, err)`: plain-text body plus an
  `X-Error-Title` header, which `static/app.js` shows as a toast.
- **Full page loads of fragment URLs** (e.g. a refresh after `hx-push-url`)
  check the `HX-Request` header and render the whole page instead.

## Bulk actions

Each list row has a checkbox (`name="ids"`, `data-select="row"`). Group
headings (`data-select="group"`) and **Select all N shown**
(`data-select="all"`, scoped by `data-select-scope` on the list) tick every
row in their scope. `static/app.js` shows the `#bulk` bar (`bulkBar` in
`views/layout.templ`) while anything is ticked. It fills each button's
`hx-confirm` from its `data-confirm` template, using the quota left on
`#summary`.

The bar's buttons post `action` plus the ticked `ids` (via `hx-include`) to
`POST /channels/bulk` or `POST /playlists/bulk`. Decisions are saved in one
transaction and the page refreshes. YouTube changes run as a background job
that goes through the same quota-limited loops as **Unsubscribe from all** and
**Clean up all playlists**.

To add a bulk action, add a case to `handleChannelBulk` or
`handlePlaylistBulk` and a button to the page's bulk bar.

## Background jobs

Anything slow (sync, AI analysis, bulk unsubscribe, playlist cleanup and
merges) runs through `Server.startJob` (`server/jobs.go`):

- One job at a time. Starting a second shows the running job's banner instead.
- The banner polls `GET /job` every second. When the job ends, `/job` answers
  `HX-Refresh` and the page reloads with the results.
- A returned error becomes the page's error notice. `s.warn(...)` records a
  non-fatal warning, and `s.setFlash(...)` a one-time success message.
- Jobs get a context that is cancelled on shutdown (`Server.Close`), with a
  30-minute cap.

## Data

SQLite, opened by `store.Open(DATA_DIR)`. The schema is a list of numbered
migrations in `store/migrations.go`. **Never edit a released migration; append
a new one.** The database's `PRAGMA user_version` records how many have run.

What's kept apart on purpose:

- **Synced data** (`channels`, `playlists`, `playlist_items`) is replaced
  wholesale on each sync (`ReplaceChannels`, `ReplacePlaylists`).
- **AI analyses**, **your decisions** and **watch stats** live in their own
  tables keyed by ID, so they survive re-syncs.
- **A decision beats the AI's verdict**: `Channel.Verdict()` and
  `Playlist.Verdict()` return the effective verdict. Decided items are never
  sent to the AI again.
- **Outdated suggestions**: each analysis stores a `Basis`, a hash of the
  inputs that shaped it (your interests, watch-history import). When those
  change, the analysis is marked `Outdated` and **Analyze new** redoes it.

`store.Snapshot` and `store.Playlists` load each page's whole state in one go.
They're fast enough for thousands of channels, and they keep handlers simple.

## YouTube quota

Every API call reports its cost through `youtube.Client.OnCost`, which the
server records with `store.AddQuotaUsage` for the current quota day (Pacific
time). Costs are constants in `internal/youtube` (`CostRead`, `CostWrite`).
Before a write, handlers call `ensureQuota`, and bulk jobs work out how many
writes fit. The views use the same constants to show costs.

## AI

- `internal/ai` builds the prompts (`system := ...` in `ai.go` and
  `playlists.go`), calls the chat completions API and validates the JSON it
  gets back. Unknown categories become "Other", and results for IDs that
  weren't asked about are dropped.
- Channel and playlist text is untrusted: it's sanitized, truncated and
  marked as data in the prompt.
- The allowed categories and kinds of videos are `ai.Categories` and
  `ai.ContentTypes`.
- `server/analyze.go` sends batches with limited concurrency, retries with
  backoff and asks again for items the model skipped. It's generic:
  `channelAnalysis` and `playlistAnalysis` are the two instances.
- The server only depends on the `server.Analyzer` interface, so tests use a fake.

## Common changes

**Add a setting.** Add a field to `config.Config`, read it in `config.FromEnv`,
document it in `.env.example`, and pass it to where it's used from
`cmd/yt-subscriptions/main.go`.

**Add an action to a page.**
1. Add a handler in `server/<page>.go` and its route in `Server.routes`.
2. If it changes YouTube, call `ensureQuota` first and `checkAuth(err)` on
   failure, so an expired sign-in shows **Connect YouTube** again.
3. If it's slow, wrap it in `startJob`.
4. Add the button in `views/<page>.templ` with `hx-post`/`hx-delete`, then run
   `make generate`.
5. Add a test in `server/<page>_test.go`. `newTestServer` and an `httptest`
   fake of the YouTube endpoints you call (see `fakeYouTube`) cover most cases.

**Change the database.** Append a migration to `store/migrations.go`, update
the queries in `store/`, and add a test in `store/*_test.go`.

**Change what the AI suggests.** Edit the prompt and the allowed values in
`internal/ai`. The tests in `internal/ai` pin down the parsing and
validation. Prompt wording isn't tested, so try it against your model.

**Add a new page** (say, a third kind of YouTube data). Follow the playlists
feature through each layer: types and tables in `store/`, fetching in
`youtube/`, a prompt in `ai/` (plus a method on `server.Analyzer`), an
`analysis[...]` constructor in `server/analyze.go`, handlers and routes in
`server/`, a page in `views/`, and a `navLink` in `views/layout.templ`.

## Customizing a fork

| To change | Look in |
| --- | --- |
| App name and page title | `views/layout.templ` (`<title>`, header) |
| Colors, fonts, spacing tokens | `styles/input.css`, then `make css` |
| Categories, kinds of videos, verdict rules | `internal/ai/ai.go`, `internal/ai/playlists.go` |
| Batch sizes, concurrency, retries | constants at the top of `internal/server/analyze.go` |
| Default quota and other settings | `internal/config/config.go`, `.env.example` |
| Container image name | automatic: the release workflow publishes `ghcr.io/<your-account>/<repo>` |

The Go module is named `yt-subscriptions`, not a GitHub path, so imports don't
change when you fork or rename the repository.
