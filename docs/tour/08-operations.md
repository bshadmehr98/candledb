# 8. Operations

This page is for whoever runs the server: every configuration setting, what
is in the data directory, how to put the server on the internet (TLS, reverse
proxies, CORS), backup and restore, health checks and metrics, logging,
shutdown, Docker, the built-in limits, a checklist for going to production,
and a plain list of what candledb does not do.

## The command

```text
candledb serve [flags]          run the server
candledb check-rules <file>     validate a security rules file
candledb backup [flags] <dest>  write a consistent copy of the database
candledb version                print the version
```

`candledb <command> -h` prints a command's flags.

## Configuration

Every setting of `serve` is a flag, and each also reads an environment
variable, which is the convenient form in containers. A flag given on the command line
wins over the environment.

| Flag | Environment variable | Default | Meaning |
|---|---|---|---|
| `-addr` | `CANDLEDB_ADDR` | `:8090` | address and port to listen on; `127.0.0.1:8090` accepts local connections only |
| `-data-dir` | `CANDLEDB_DATA_DIR` | `./candledb-data` | directory for the database and generated secrets |
| `-rules` | `CANDLEDB_RULES` | `<data-dir>/candledb.rules` | the security rules file |
| `-dev-open-rules` | `CANDLEDB_DEV_OPEN_RULES` | off | allow every read and write; development only |
| `-jwt-secret` | `CANDLEDB_JWT_SECRET` | generated | secret that signs access tokens; at least 32 bytes |
| `-admin-key` | `CANDLEDB_ADMIN_KEY` | none | bearer key that bypasses the rules; at least 24 characters; empty means no admin access |
| `-cors-origins` | `CANDLEDB_CORS_ORIGINS` | `*` | comma-separated browser origins allowed to call the API |
| `-disable-signup` | `CANDLEDB_DISABLE_SIGNUP` | off | refuse new account registrations |
| `-trust-proxy` | `CANDLEDB_TRUST_PROXY` | off | take client addresses from `X-Forwarded-For` |
| `-tls-cert` | `CANDLEDB_TLS_CERT` | none | TLS certificate file; HTTPS is served when set together with `-tls-key` |
| `-tls-key` | `CANDLEDB_TLS_KEY` | none | TLS private key file |
| `-log-format` | `CANDLEDB_LOG_FORMAT` | `text` | `text` or `json` |
| `-log-level` | `CANDLEDB_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `-access-ttl` | `CANDLEDB_ACCESS_TTL` | `1h` | lifetime of access tokens |
| `-refresh-ttl` | `CANDLEDB_REFRESH_TTL` | `720h` | lifetime of refresh tokens (30 days) |
| `-auth-rate` | `CANDLEDB_AUTH_RATE` | `30` | sign-up, sign-in and refresh attempts per minute per client address |
| `-auth-burst` | `CANDLEDB_AUTH_BURST` | `10` | such attempts allowed in one burst |

Boolean environment variables are on only when set to exactly `true`.
Durations use Go's notation: `90s`, `15m`, `12h`.

The server refuses to start on an invalid configuration: an unknown flag, a
certificate without a key, an admin key or JWT secret that is too short, a
rules file that does not parse, or a `-rules` path that does not exist.

Prefer the environment for the two secrets. Command-line arguments are
visible to other users of the machine in the process list.

## The data directory

```text
candledb-data/
  candledb.db        the database (SQLite)
  candledb.db-wal    recent writes not yet folded into the main file
  candledb.db-shm    index for the -wal file
  jwt.secret         the token signing secret, if you did not supply one
  candledb.rules     your security rules, if you keep them here
```

- The directory is created with mode `0700`, readable only by the server's
  user. `jwt.secret` and the database files are `0600`. Windows has no
  such permission bits: there, restrict access to the data directory
  yourself.
- All three `candledb.db*` files belong together. Never copy `candledb.db`
  alone from a running server; use `candledb backup`.
- The database holds the documents, their indexes, the user accounts (with
  password hashes) and the hashes of refresh tokens.
- `jwt.secret` is generated on first start when neither `-jwt-secret` nor
  `CANDLEDB_JWT_SECRET` is given. Deleting it invalidates every access token.
- The directory must be on a local disk. SQLite's locking is unreliable on
  network file systems such as NFS.
- Only one server process may use a data directory.

## TLS

Access tokens and passwords travel in every request, so a server reachable
from the internet must be behind HTTPS. There are two ways.

**Directly.** Give the server a certificate and key:

```sh
candledb serve -addr :443 -tls-cert /etc/candledb/cert.pem -tls-key /etc/candledb/key.pem
```

The files are read at start. After renewing a certificate, restart the
server.

**Behind a reverse proxy** such as Caddy or nginx, which holds the
certificate and forwards plain HTTP to candledb on a local port. This is the
usual set-up. Three settings matter:

1. Bind candledb to the loopback address (`-addr 127.0.0.1:8090`) so that it
   cannot be reached around the proxy.
2. Start it with `-trust-proxy`, and make sure the proxy sets
   `X-Forwarded-For`. Otherwise every user appears to come from the proxy's
   address and they all share one [rate limit](06-auth.md#rate-limiting).
   Do not use `-trust-proxy` without a proxy: clients could then forge their
   address.
3. Turn off response buffering for `/v1/listen` and raise the proxy's read
   timeout for it. The listen stream is a response that never ends; a
   buffering proxy would hold its messages back. CandleDB sends
   `X-Accel-Buffering: no`, which nginx honours, and a keep-alive line every
   20 seconds, so a timeout comfortably above that is enough.

A minimal nginx location for the whole API:

```nginx
location / {
    proxy_pass http://127.0.0.1:8090;
    proxy_http_version 1.1;
    proxy_set_header Connection "";
    proxy_set_header X-Forwarded-For $remote_addr;
    proxy_buffering off;
    proxy_read_timeout 1h;
}
```

## CORS

A browser page may call an API on another origin only if the API permits it
(cross-origin resource sharing). An **origin** is scheme, host and port, for
example `https://app.example.com`.

By default candledb permits every origin (`*`). That is safe for this API
because it authenticates with a bearer token in a header, never with cookies,
so a hostile page has no ambient credentials to abuse. To restrict it anyway:

```sh
candledb serve -cors-origins "https://app.example.com,https://admin.example.com"
```

Requests from other origins then receive no `Access-Control-Allow-Origin`
header and the browser blocks them. CORS is enforced by browsers only; it is
not access control, and `curl` ignores it. The preflight answer is:

```text
HTTP/1.1 204 No Content
Access-Control-Allow-Headers: Authorization, Content-Type, X-Request-Id
Access-Control-Allow-Methods: GET, POST, PUT, PATCH, DELETE
Access-Control-Allow-Origin: *
Access-Control-Max-Age: 600
Vary: Origin
```

## Backup and restore

```sh
candledb backup -data-dir ./candledb-data /backups/candledb-2026-11-24.db
```

```text
backup written to /backups/candledb-2026-11-24.db
```

- The backup is a single, complete, consistent SQLite file: the state of the
  database at one moment.
- It can be taken **while the server is running**. Writes wait briefly while
  it is made.
- The destination must not exist; the command refuses to overwrite a file.
- The file contains password hashes and refresh-token hashes. It is created
  readable and writable by its owner only (mode 0600); keep it that way
  wherever you copy it.
- `jwt.secret` and `candledb.rules` are not part of it. Back those up
  separately (or supply them through configuration).

To restore, stop the server, and put the backup in place as `candledb.db` in
an otherwise empty data directory:

```sh
mkdir restored
cp /backups/candledb-2026-11-24.db restored/candledb.db
cp /backups/jwt.secret restored/          # to keep existing tokens valid
candledb serve -data-dir restored
```

Do not leave old `candledb.db-wal` or `candledb.db-shm` files next to a
restored database.

There is no built-in schedule; run the command from cron or a similar tool
and copy the result off the machine. A backup you have never restored is not
yet a backup: try the restore once.

## Health checks

| Endpoint | Answers 200 when | Use it for |
|---|---|---|
| `GET /healthz` | the process is serving HTTP | liveness: restart the process if it fails |
| `GET /readyz` | the database also answers (within 2 seconds) | readiness: send traffic only if it succeeds |

Both return `{"status":"ok"}`. `/readyz` returns 503 with
`{"status":"unavailable"}` when the database cannot be reached.

## Metrics

`GET /metrics` serves counters in the Prometheus text format. It requires the
admin key as bearer token, so it does not exist unless `-admin-key` is set.

```sh
curl -s http://localhost:8090/metrics -H "Authorization: Bearer $CANDLEDB_ADMIN_KEY"
```

```text
# TYPE candledb_http_requests_total counter
candledb_http_requests_total{class="2xx_3xx"} 7
candledb_http_requests_total{class="4xx"} 19
candledb_http_requests_total{class="5xx"} 0
# TYPE candledb_database_version gauge
candledb_database_version 2
# TYPE candledb_listen_sessions gauge
candledb_listen_sessions 0
# TYPE candledb_listen_targets gauge
candledb_listen_targets 0
# TYPE candledb_store_operations_total counter
# TYPE candledb_store_errors_total counter
# TYPE candledb_store_seconds_total counter
candledb_store_operations_total{op="get"} 6
candledb_store_errors_total{op="get"} 0
candledb_store_seconds_total{op="get"} 0.000684772
...
```

| Metric | Type | Meaning |
|---|---|---|
| `candledb_http_requests_total{class}` | counter | responses by status class: `2xx_3xx`, `4xx`, `5xx` |
| `candledb_database_version` | gauge | the current database version, which is the number of commits ever made |
| `candledb_listen_sessions` | gauge | open listen streams |
| `candledb_listen_targets` | gauge | listeners across all streams |
| `candledb_store_operations_total{op}` | counter | storage operations: `get`, `query`, `apply` (one per commit) |
| `candledb_store_errors_total{op}` | counter | storage operations that failed; "not found" is not a failure |
| `candledb_store_seconds_total{op}` | counter | total time spent in storage operations |

Useful derived figures: the rate of `candledb_database_version` is commits
per second; `seconds_total / operations_total` is the mean duration of an
operation; any growth in `5xx` or `store_errors` deserves a look at the log.

In a Prometheus scrape configuration, give the key as the bearer token:

```yaml
scrape_configs:
  - job_name: candledb
    authorization:
      credentials: <the admin key>
    static_configs:
      - targets: ["localhost:8090"]
```

## Logs

Logs go to standard error, one line per event.

```text
time=2026-11-24T00:43:45.369+03:00 level=INFO msg=request method=DELETE path=/v1/documents/users/bob status=200 duration_ms=0 request_id=ONJ5N3K5OHXD
```

- Every request is logged at `info` with method, path, status, duration and
  a **request ID**. The ID is also returned in the `X-Request-Id` response
  header. If the client sends that header (up to 64 characters), its value is
  used, which lets you trace one request through several systems.
- Errors the server caused (status 500) are logged at `error` with the real
  cause and the request ID. The client only ever sees `"internal error"`, so
  the ID is how you find the cause.
- `-log-format json` writes one JSON object per line for log collectors.
- `-log-level warn` drops the per-request lines.

Request bodies, tokens and passwords are never logged. Paths are, and the
path of a listen-target request contains a session ID. A session ID is not a
credential on its own: it is only accepted together with the credentials
that opened the stream.

## Signals and shutdown

| Signal | Effect |
|---|---|
| `SIGINT` (Ctrl-C), `SIGTERM` | graceful shutdown |
| `SIGHUP` | reload the rules file; see [Security rules](07-security-rules.md#checking-and-reloading) |

On shutdown the server stops accepting connections, ends every listen stream
with a `session_closed` message (clients reconnect on their own once a server
is back), waits up to 15 seconds for requests in progress, and closes the
database. Give your process manager a stop timeout a little above 15 seconds.

A commit is durable in the sense that it survives the process being killed
at any moment: it is either wholly in the database or not at all. The
database runs SQLite with `synchronous=NORMAL` in WAL mode, which means the
most recent commits can be lost if the *machine* loses power or the operating
system crashes, although the database itself stays consistent.

## Docker

The repository's [`Dockerfile`](../../Dockerfile) builds a small image that
contains only the static binary and runs as an unprivileged user.

```sh
docker build -t candledb .

docker run -d --name candledb \
  -p 8090:8090 \
  -v candledb-data:/data \
  -e CANDLEDB_ADMIN_KEY="$(openssl rand -hex 24)" \
  candledb
```

- The data directory is `/data` (the image sets `CANDLEDB_DATA_DIR`). Mount
  a volume there or the data is lost with the container.
- Put the rules in the volume as `/data/candledb.rules`, or mount them
  elsewhere and set `CANDLEDB_RULES`.
- Reload the rules with `docker kill --signal=HUP candledb`.
- The image has no shell. Run the other commands by overriding the command,
  for example a backup into the same volume:
  `docker run --rm -v candledb-data:/data candledb backup /data/backup.db`.
  The container runs as an unprivileged user, so a destination outside the
  volume must be writable by that user.
- `docker stop` sends `SIGTERM`; its default 10-second grace period is
  shorter than the server's 15, so use `docker stop -t 20`.

## Limits

These are fixed in this version.

| What | Limit |
|---|---|
| Document size | about 1 MiB |
| Nesting depth of a document | 20 |
| Field name, path segment | 1500 bytes |
| Path length | 100 segments |
| Request body | 10 MiB |
| Writes in one commit | 500 |
| Documents returned by one query, or in one listener snapshot | 10,000 |
| Filters in a query | 30 |
| Orderings in a query | 10 |
| Values in an `in`, `not-in` or `array-contains-any` list | 30 |
| Listen sessions | 10,000 |
| Listeners per session | 100 |
| Commits a listen session may fall behind | 256 |
| Document lookups in one rule evaluation | 10 |

Timeouts: 10 seconds to send request headers, 60 seconds to send a request
and 60 to receive the response (the listen stream is exempt), 120 seconds for
an idle keep-alive connection.

There is no limit on the number of documents, collections or users other
than disk space.

## Performance expectations

All commits are applied one after another by a single writer. That is what
makes transactions and rules simple and correct, and it puts a ceiling on
write throughput: on the order of thousands of small commits per second on
ordinary hardware, fewer when documents are large, have many fields (each
field is indexed) or when write rules call `get()`. Batching many writes
into one commit is much faster than committing them one by one.

Reads and queries run concurrently with each other and with the writer.
`make bench` runs the storage benchmarks on your own machine.

## Production checklist

- [ ] A rules file is in place, and it was checked with `candledb
      check-rules`. `-dev-open-rules` is **not** set.
- [ ] The server is reachable only over HTTPS (own certificate or a proxy).
- [ ] Behind a proxy: bound to loopback, `-trust-proxy` on, buffering off for
      the listen stream.
- [ ] `CANDLEDB_JWT_SECRET` is set from a secret store, or `jwt.secret` is
      included in backups.
- [ ] An admin key is set only if you need it, is long and random, and lives
      only on servers.
- [ ] `-cors-origins` lists your frontend's origins.
- [ ] `-disable-signup` is set if users are not meant to register themselves.
- [ ] Backups run on a schedule, are copied off the machine, and a restore
      has been tested.
- [ ] `/healthz` and `/readyz` are wired to your process manager or load
      balancer; `/metrics` is scraped.
- [ ] Logs are collected (`-log-format json`).
- [ ] The data directory is on a local disk with free space monitored.

## What candledb does not do

Knowing the boundaries is part of operating a system.

- **One node.** There is no replication, clustering or failover. Availability
  is that of one process on one machine. Scaling is vertical: a bigger
  machine.
- **One writer.** Commits are serialised (see above).
- **No point-in-time recovery.** You can restore to a backup, not to an
  arbitrary moment between backups.
- **No change history.** A reconnecting listener is re-sent its whole
  result; the server keeps no log of past changes.
- **No collection-group queries, joins, aggregations or full-text search.**
- **No server-side functions or triggers.** Logic that must run on a trusted
  machine runs in your own process, using the admin key.
- **No file storage.** Documents hold JSON values up to 1 MiB.
- **No account management beyond sign-up and sign-in:** no email
  verification, password reset, account deletion endpoint, or third-party
  login. Use [your own tokens](06-auth.md#bringing-your-own-tokens) if you
  need them.
- **No rate limiting of data endpoints.** Only the account endpoints are
  limited. Put a proxy in front if you need more.
- **No schema migrations for your data.** The server migrates its own
  storage format between versions; the shape of your documents is yours.

Next: [9. HTTP API](09-http-api.md)
