# Contributing

Thanks for helping out, or for maintaining your own fork. This page covers
getting a dev setup running and the few rules that keep the project easy to
work on. For how the code is organized, read [ARCHITECTURE.md](ARCHITECTURE.md).

## Setup

You need [Go](https://go.dev/dl/) (the version in `go.mod`), `make`, `curl` and
git. Everything else is pinned and fetched for you: templ is a Go tool
dependency (`go tool templ`), and the Tailwind CLI is downloaded into `bin/`.

```sh
make setup   # Tailwind CLI, .env from .env.example, generated templates and CSS
make dev     # live reload; open http://localhost:7331
```

To click through the app you need a Google OAuth client and an AI endpoint in
`.env` (see the [README](README.md#quick-start)). Any OpenAI-compatible
endpoint works, including a local one such as Ollama or LM Studio.

You don't need either to run the tests.

## Making a change

1. Branch off `main`.
2. Make the change. [ARCHITECTURE.md](ARCHITECTURE.md#common-changes) lists
   where the common kinds of change go.
3. Run `make check`. It formats, vets, tests (with the race detector) and
   builds, the same checks as CI.
4. Commit the generated files along with their sources (see below).
5. Open a pull request saying what changed and why.

### Generated files are committed

| You edited | Run | Commit |
| --- | --- | --- |
| `views/*.templ` | `make generate` | `views/*_templ.go` |
| `styles/input.css`, or Tailwind classes in `views/` | `make css` | `static/app.css` |

`make dev` regenerates both as you save. CI fails if they're out of date, so
the binary builds with a plain `go build`, without templ or Tailwind installed.

### Tests

- `go test ./...` runs everything. Tests never touch the network or your `data/` folder.
- **Server tests** (`internal/server/*_test.go`): `newTestServer(t, fakeAI)`
  gives you a server on a temporary database. Fake YouTube by pointing
  `s.ytBase` at an `httptest.Server`, as `fakeYouTube` in `playlists_test.go`
  does. Background jobs run to completion with `runJob(t, s, handler, path, id)`.
- **AI tests** (`internal/ai`) check prompt input and output parsing against a
  fake HTTP endpoint.
- **Store tests** open a real SQLite database in `t.TempDir()`.

New behaviour should come with a test at the lowest layer that can show it.

## Conventions

- **Keep the stack small.** Standard library first. Each new dependency
  (Go module, JS library or build tool) is one more thing for every fork to
  maintain, so explain why it's worth it in the PR.
- **No inline scripts.** The Content-Security-Policy blocks them. Behaviour
  goes in HTMX attributes, or in `static/app.js` if it really needs JS.
- **Messages people see** (errors, warnings, confirmations) say what happened
  and what to do next, in plain words: "only about 40 units are left today;
  quota resets at midnight Pacific time", not "quota error".
- **Destructive YouTube calls** check the quota first (`ensureQuota`),
  report auth failures through `checkAuth`, and leave local data matching
  what actually happened on YouTube when they stop partway.
- **Comments say why**, not what. Each package has a doc comment, and
  `internal/server/server.go` describes each file in that package.
- **Schema changes** are new migrations, never edits to old ones.
- `gofmt` and `go tool templ fmt` formatting (`make fmt`).

## Releasing

Push a version tag. The release workflow builds a multi-arch image and
publishes it to `ghcr.io/<owner>/<repo>` for the repository it runs in:

```sh
git tag v1.2.0 && git push origin v1.2.0
```

## Maintaining a fork

- The [fork customization table](ARCHITECTURE.md#customizing-a-fork) lists
  where branding, prompts and limits live.
- To pull in upstream changes:
  ```sh
  git remote add upstream <upstream-repo-url>   # once
  git fetch upstream && git merge upstream/main
  make check
  ```
  If generated files conflict, take either side, run `make generate css`, and
  commit the result.
- Keep your changes in the places the table lists where you can. That keeps
  merges small.
