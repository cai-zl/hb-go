# Repository Guidelines

## Project Overview

`github.com/cai-zl/hb-go` is a Go **library** (no `main` package, no binary): an HTTP response envelope for Gin plus a one-call service starter, `boot.Serve`. Business routes use native Gin handlers and apply the consuming project's response and error policy. It is the Go counterpart of a Rust library named hb, and its `crypto` and `password` formats are byte-compatible with that version.

- The module root is package `hb` (directory `hb-go` ≠ package name). Import `"github.com/cai-zl/hb-go"` and reference `hb.`.
- `README.md` (Chinese) is the only documentation. There are no scripts, Makefile, CI, linter config or `examples/`.

## Architecture & Data Flow

```text
boot ──► internal/api, auth, db, nacos, redis, s3, ws, hb   wiring only
internal/api ──► auth, hb                                built-in HTTP endpoints
auth ──► crypto, hb                                      sessions and auth middleware
crypto, db, nacos, password, redis, s3, ws ──► hb   leaf packages; never import each other
```

Keep this direction. Dependencies are passed as concrete types; `auth.AuthChecker` is the business login extension point, and `boot` is the only assembler.

**Request.** Register a native handler: `rg.GET("/x", func(c *gin.Context) { … })`.

- Reuse the project's shared HTTP response and error handling; only define it in the project if none exists. `hb.Write` writes an `hb.Envelope` at the caller's HTTP status.
- When retaining the old business protocol, success and business failures use HTTP 200 with the result in `code`; omit `data` when nil.
- Mask unknown errors with `hb.Internal`, and expose only the recognized `*hb.Error` code and message, not the underlying cause.
- Built-in endpoints retain their own statuses: 404/405 from `hb.New()`, 401 from `auth.Guard` and `/auth/me`, and 503 from `/readyz`.

**Startup** (`Boot.run` in `boot/boot.go`). `Serve(mounts, opts...)` reads the `NACOS_*` env, pulls the TOML config from Nacos, then runs with registration and hot reload. `ServeWith(cfg, mounts, opts...)` takes an explicit `Config` and runs standalone unless `WithNacos` is given. Order: validate `WithAuth` has `[auth]` → logging → connect configured sections (`[db]`, migrations if `WithMigrator`, `[redis]`, `[s3]`, `[auth]`) → hooks in order (first error aborts before listening) → routes (`hb.New()`, auth endpoints if enabled, business mounts, WS endpoints, `/healthz`, `/readyz`) → listen → register → config listener → SIGINT/SIGTERM → `http.Server.Shutdown` → deregister.

**Auth.** `internal/api` implements `POST /auth/token`, `POST /auth/refresh`, `POST /auth/revoke` and `GET /auth/me`. `boot.WithAuth(checker)` tells boot to mount them before business routes. The business implements `auth.AuthChecker`, checking credentials and returning already encoded `json.RawMessage` session data. The option requires `[auth]` and `[redis]`; without it, `[auth]` still assembles `app.Sessions` but exposes no auth endpoints. `Sessions` keeps two Redis keys, `{prefix}a:{token}` and `{prefix}r:{token}`, each naming the other as `peer`; key TTL is the refresh TTL and access expiry comes from the record's `exp`. `auth.Guard` checks `Authorization: Bearer`, then sets gin keys `hb.session` and `hb.token`; handlers read them with `auth.From[T](c)` / `auth.Token(c)`.

**WebSocket.** `ws.Upgrade` registers a `*ws.Conn` in the `Hub` and starts one read goroutine (calls `onMessage` serially) and one write goroutine (256-slot queue, drop + warn when full, ping every `ws.Heartbeat` = 30s).

**Config.** TOML. A section present means "connect and check"; absent means off. Business settings go in `[extra]`.

## Key Directories

| Path | Package | Responsibility |
|---|---|---|
| `hb.go` | `hb` | `Envelope`, `Err`/`Errf`/`Internal`, `Write`, `Page`, `New`, `Recovery`, `Response[T]`, `PageData[T]` |
| `boot/` | `boot` | `Serve`/`ServeWith`, options, `Config`/`ParseConfig`, `App`, `Mount`, `Hook`, `Snapshot[T]` |
|`auth/`|`auth`|`AuthChecker`, Redis-backed sessions, `Guard`, `From[T]`, `Token`|
|`internal/api/`|`api`|Built-in auth endpoints and `/healthz` / `/readyz` probes|
| `db/` | `db` | `*sql.DB` pool for `pg`/`mysql`/`sqlite`, up-only file migrations |
| `nacos/` | `nacos` | nacos-sdk-go/v2 wrapper: config fetch/listen, register/deregister |
| `redis/` | `redis` | go-redis v9 client factory (`Connect`) |
| `s3/` | `s3` | minio-go wrapper: put/get/delete/exists/presign, `ErrNotFound` |
| `ws/` | `ws` | WebSocket hub on gorilla/websocket |
| `crypto/` | `crypto` | SHA-256, HMAC-SHA256, `RandomHex`, AES-256-GCM `Encrypt`/`Decrypt` |
| `password/` | `password` | argon2id (default) / bcrypt / pbkdf2 `Hash`, `Verify`, `Algorithm` |
| `skills/hb-go/` | — | Skill for consumer projects using hb-go |

## Development Commands

```bash
go build ./...   # compile check; a library, so no binary is produced
go vet ./...
go test ./...    # unit tests run; integration tests skip unless TEST_* vars are set
gofmt -l .       # files that need formatting
```

- No Makefile, linter or CI: `go vet`, `gofmt` and `go test` are the only gates. `go run` does not apply.
- **Windows**: `boot`'s tests call `syscall.Kill` (`TestServeStandaloneLifecycle`), which is undefined there, so `go vet ./...` and `go test ./...` fail with `[build failed]` for `boot` while the other 9 packages pass. Use `go test $(go list ./... | grep -v '/boot$')` and `GOOS=linux go vet ./...` (type-checks every package, `boot` tests included).
- `gofmt -l .` listing every file means CRLF line endings from `core.autocrlf=true` (the index is LF), not formatting drift. Check committed content with `git show HEAD:<file> | gofmt -l`.
- Test output includes expected `WARN`/`ERROR` slog lines from the error-masking and `Recovery` tests.

Integration tests are env-gated and hit real services; see Testing & QA.

## Code Conventions & Common Patterns

### Errors and responses (`hb.go` is the contract)

- Business handlers own queries, validation and authorization. Use the project's shared response and error handling; for custom responses, use Gin (e.g. `c.Data(…)`) and return after writing.
- HTTP boundary only: `hb.Err(code, msg)` / `hb.Errf(code, fmt, args...)` build errors meant for an HTTP client (code `0` is normalized to `500`); `hb.Internal(err)` logs and masks an unknown error for the response and keeps the cause for `errors.As`/`Unwrap`.
- Config, startup, connect and other non-HTTP code (`boot`, `db`, `redis`, `s3`, `nacos`, `auth.Sessions`, `crypto`, `password`, `ws.Upgrade`) returns plain Go `error`s (`errors.New` / `fmt.Errorf` with `%w`) without logging them; the HTTP layer that consumes the error logs and masks it (`auth.Guard`, `internal/api.writeAuthRouteError`).
- Return `error`, never `*hb.Error` (typed-nil pitfall).
- Config problems: `errors.New("<pkg> config: … is required")`; TOML parse errors are `fmt.Errorf("config: %w", err)`. Connection strings and credentials never appear in error text (`redis config: invalid url`, `db config: invalid pg url`, `s3 config: failed to initialize client`).
- The only sentinel is `s3.ErrNotFound` (`errors.Is`; a plain `error`, a handler that wants a 404 maps it to `hb.Err(404, …)` itself). `*hb.Error` is inspected with `var e *hb.Error; errors.As(err, &e)` only at the HTTP boundary.
- Auth semantics: wrong credentials → HTTP 200 + code 401; missing or invalid token → HTTP 401; a Redis failure stays code 500, never 401.
- `hb.Recovery()` (panic → `500 internal error` envelope) is opt-in; `boot` does not install it.

### Config

- One `Config` struct per package with `toml` tags (snake_case keys). Defaults are applied in the constructor; required fields are validated up front (`boot.ParseConfig` and each package's connect function), which fails fast.
- `boot.Config` top-level keys: `port` (required), `app_name`, `host`, `group`, `log_level`, `advertise_ip`. Sections: `[db]`, `[redis]`, `[s3]`, `[auth]`, `[extra]`. Unknown top-level keys only warn. `[auth]` requires `[redis]`.
- Read `[extra]` through `boot.Snapshot[T](cfg)` (cached per type, refreshed on config push; a failed re-parse keeps the old value) or `cfg.Extra(&v)`.
- Durations are TOML strings (`"5s"`, `"2h"`); integers mean nanoseconds. They are normalized through the `durationKeys` table in `boot/boot.go`: a new `time.Duration` config key must be registered there, or it decodes as zero.
- Adding a `boot.Config` field: add it to `Config`, to the unexported `rc` struct inside `ParseConfig` (decoding goes through it), to `knownKeys`, and to the connect step in `Boot.run` / field on `boot.App`.
- Hot reload (only in `Serve`) reapplies `log_level` and `[extra]`; bad TOML keeps the old values.

### Structure and wiring

- `db.Connect`, `redis.Connect` and `s3.Connect` validate, connect and ping immediately; `nacos.ConnectAuth` only validates the address (the SDK client is created lazily and does not dial). In-memory types use `NewX` constructors.
- `boot.App` holds the wired clients (`DB`, `Redis`, `S3`, `Nacos`, `Sessions`; nil when the section is absent) and `Hub` (always set).
- Business code plugs in through `boot.Mount = func(app *boot.App, rg *gin.RouterGroup)`, `boot.WithHook(func(*boot.App) error)`, `boot.WithAuth(check auth.AuthChecker)` and `boot.WithWS(path, onMessage)`. Long-running tasks start their own goroutine inside a hook.
- Functional options (`type Option func(*Boot)`, `WithX`) are used only in `boot`.
- Generics serve typed helpers: `hb.Response[T]`, `hb.PageData[T]`, `auth.From[T]`, `boot.Snapshot[T]`.
- JSON is camelCase except `auth.TokenResponse` (OAuth2 snake_case: `access_token`, …). TOML keys are snake_case.

### Context, concurrency, logging

- Public APIs take no `ctx`; library calls use `context.Background()`. In handlers, pass gin's `c` to `app.DB.QueryContext(c, …)`. Deadlines exist only for startup checks, `/readyz` (3s) and WS writes (10s).
- Shared state uses `sync.RWMutex` with copy-then-unlock (callbacks run outside the lock, e.g. `ws.Hub.remove`), `sync.Once` for closing channels, and non-blocking channel sends that drop on a full queue.
- Logging is the global `log/slog` with English key-value attrs (`"err", err`). `boot.initLogging` installs a text handler on stderr; level is `log_level` > `LOG_LEVEL` > `info`. Secrets (Nacos, S3 credentials) are never logged; `crypto.Decrypt` logs causes at debug only.

### Comments and language

- Doc and inline comments are **Chinese**; identifiers, error messages and log messages are English. Keep that split.
- Every package opens with `// Package x …` stating its contract. Exported identifiers get a doc comment that starts with the name; config field comments name the TOML key and default.
- Section separators are `// ---- title ----`; known limits are tagged `// shortcut: …`.
- Imports go in three blocks: stdlib, third-party, `github.com/cai-zl/hb-go/…`. go-redis is aliased `goredis`, the local redis package `hbredis`.

### Contracts that must not drift

- `crypto` output (`base64(nonce‖ciphertext‖tag)`, key = SHA-256 of the passphrase) and `password` formats and params (argon2id `m=19456,t=2,p=1`; bcrypt cost 12 normalized to `$2b$`; `$pbkdf2$600000$…`) are byte-compatible with Rust hb. `password/password_test.go` pins Rust-generated vectors (`rustVectors`). `password.Verify` never panics and returns `false` on junk input.
- Password verification compares with `subtle.ConstantTimeCompare`; randomness comes from `crypto/rand`. `crypto.Decrypt` always reports the plain error `crypto: decrypt failed`.
- Persisted and protocol names: ledger table `hb_migrations`, Redis prefix `hb:auth:`, gin keys `hb.session`/`hb.token`, envelope JSON keys.
- Migrations (`db.Migrate`): up-only, one transaction per file, `<number>_<desc>.sql`, SQL written in the chosen dialect and executed verbatim. Applied files are skipped **by number only**; checksums are stored but never compared. MySQL needs `multiStatements=true` in the DSN or `Migrate` fails.
- WebSocket: `CheckOrigin` is always true by design (restrict Origin in business code). `ws.Message` has only `Conn` and `Data`: Text, Binary and Pong payloads arrive identically.

### Sharp edges

- `advertise_ip` unset makes `detectIP` UDP-dial `8.8.8.8:80`, even for standalone `ServeWith`. Set `advertise_ip` in tests, as `boot/boot_test.go` does.
- `WithLogging(false)` also disables config hot reload; the listener is tied to logging initialization.
- `nacos.Register` returns `nil` when the SDK reports `ok=false` without an error.
- Shutdown closes neither the DB pool nor the Redis/S3 clients; close what a test opens (`t.Cleanup`).
- The MySQL and SQLite drivers must be blank-imported by the consuming app (`go-sql-driver/mysql` is not in `go.mod`). `db` itself already imports pgx `stdlib` for `pg`.

## Important Files

- `hb.go`: response envelope, pagination, error helpers and Gin engine setup.
- `boot/boot.go`: `Serve`/`ServeWith`, `Boot.run` startup order, `WithAuth`, `ParseConfig`, `knownKeys`/`durationKeys`, `Snapshot`.
- `internal/api/auth.go`, `internal/api/health.go`: built-in auth endpoints and health probes, mounted by `boot`.
- `auth/auth.go`: `AuthChecker`, session record layout, `Guard`, `From[T]`, `Token`.
- `db/db.go`: `sqlDrivers` (dialect → driver name), `dialects` (ledger DDL per dialect), `Migrate`.
- `ws/ws.go`: hub and per-connection loops.
- `README.md`: full usage reference; see the caveats below.
- `go.mod`: Go 1.27.3 and the direct deps. `.gitignore` ignores `AGENTS.local.md` / `CLAUDE.local.md` for personal notes.

### README examples that do not match the code

- The quick start passes the `//go:embed migrations/*.sql` FS straight to `WithMigrator`. `db.Migrate` lists the FS root and silently applies nothing; pass `fs.Sub(migrations, "migrations")` instead.
- The TOML sample repeats `[db]` and omits the required `url`. `password.Algorithm` returns `"pbkdf2"`, not `"pbkdf2-sha256"`.

## Runtime/Tooling Preferences

- Go 1.27.3 (`go 1.27.3` in `go.mod`, no `toolchain` line). The code relies on `log/slog`, `crypto/pbkdf2` and generics.
- Pure Go: no cgo, no build tags, no `go:generate`, no `vendor/`, no `go.work`.
- Direct deps: gin, gorilla/websocket, pgx/v5, minio-go/v7, nacos-sdk-go/v2, go-toml/v2, go-redis/v9, x/crypto, modernc.org/sqlite (test imports only). Prefer the stdlib over a new dependency.
- Tests use the stdlib `testing` package only: no testify, no mock generators.

## Testing & QA

- **Layout**: tests use an external `<pkg>_test` package by default. Internal tests include `boot/boot_internal_test.go` (`resolveLevel`) and `db/db_test.go` (registers the sqlite driver for tests only).
- **Style**: no `t.Run`, `t.Parallel` or `testing.Short`; no mocks, interfaces or `testdata/`. Fixtures are inline: TOML string literals, `fstest.MapFS` migrations, Rust vectors, `httptest.NewRecorder` / `httptest.NewServer`, sqlite `:memory:` via `sqlitePool`.
- **Names**: `Test<Unit><Behaviour>` without underscores (`TestErrZeroCodeIs500`, `TestMigrateFailureIsAtomic`); one end-to-end test per area (`TestAuthFlow`, `TestServeStandaloneLifecycle`).
- **Assertions**: table cases as a plain loop with the case name folded into the message; `t.Fatalf("x = %v, want …")` for failures; HTTP-boundary errors checked with `errors.As` to `*hb.Error`, then `e.Message` / `e.Code`; plain errors checked by exact `err.Error()` text, `errors.Is` or `errors.As` on the underlying type; JSON decoded to `map[string]any` (numbers are `float64`).
- **Helpers**: unexported, first line `t.Helper()`, duplicated per package rather than shared (`get`, `mustParse`, `do`, `freePort`, `waitFor`, `sqlitePool`). Use `t.Setenv`, `t.Cleanup` and `gin.SetMode(gin.TestMode). Reference tests to copy: `redis/redis_test.go` (unit), `db/db_test.go` (FS + DB), `ws/ws_test.go` (hub).
- **Integration tests** start with the env lookup and `t.Skip`, then connect. They touch real services, so point them at throwaway instances:

| Env var | Tests | What it touches |
|---|---|---|
| `TEST_DATABASE_URL` | `db.TestMigratePg` | runs migrations, then **drops tables `hb_demo` and `hb_migrations`** |
|`TEST_REDIS_URL`|`internal/api.TestAuthFlow`, `redis.TestConnectPing`|session keys under `hb:auth:` (30-day TTL); key `hb_test` without TTL|
| `TEST_S3_ENDPOINT`, `TEST_S3_BUCKET`, `TEST_S3_ACCESS_KEY`, `TEST_S3_SECRET_KEY` | `s3.TestS3RoundTrip`, `s3.TestPresignLimits` | puts and deletes `hb-test/roundtrip.txt` in the bucket |
| `TEST_NACOS_ADDR` | `nacos.TestNacosIntegration` | reads `hb-test.toml` in `DEFAULT_GROUP`; registers and deregisters `hb-test-svc` |

- **Coverage**: every package has tests; there are no thresholds or coverage config. Not exercised by any test: `boot.Serve` against a real Nacos, `WithMigrator`/`WithWS`/`WithNacos`, the MySQL connect and migrate paths, `nacos.LoadConfig`/`ListenConfig`, `auth.Token`.
