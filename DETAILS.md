# kronik — details

The [README](README.md) has the short version.

- [Run](#run) · [Configuration](#configuration) · [Settings from the UI](#settings-from-the-ui) · [Behind a reverse proxy](#behind-a-reverse-proxy) · [Deploy](#deploy)
- [Jobs](#jobs) · [JavaScript](#javascript) · [Containers](#containers) · [Runs](#runs) · [Retention](#retention) · [Replicas](#replicas)
- [Audit log](#audit-log) · [MCP](#mcp) · [API](#api) · [Development](#development)

## Run

```sh
KRONIK_STORE_DSN='postgres://kronik:secret@db:5432/kronik?sslmode=require' kronik
```

Tables are created on start; every one starts with `kronik_`
(`store.table_prefix`), so kronik can share a schema with other applications.
Migrations take a lock, so replicas can start together. `GET /healthz` answers `OK` while the database is reachable.

On SIGTERM a replica stops taking new runs and gives the runs it has going
`scheduler.shutdown_timeout` to finish. Then it cancels them, waits up to 10s
for commands to stop and saves the results. Set the orchestrator's grace
period to `shutdown_timeout` + 30s (the examples use 60s).

## Configuration

Environment variables with the `KRONIK_` prefix, or a `kronik.yaml` (or
`CONFIG_FILE`). Nested keys are joined with `_`. Values marked **UI** are
only the starting point: they can be changed from *Settings → Configuration*
(see below), and a value set there wins.

| Variable | Default | |
| --- | --- | --- |
| `KRONIK_STORE_DSN` | | PostgreSQL connection string, required |
| `KRONIK_STORE_TABLE_PREFIX` | `kronik_` | Prefix of every table (letters, digits, `_`) |
| `KRONIK_STORE_MAX_OPEN_CONNS` | `10` | Connection pool size |
| `KRONIK_SERVER_HOST` / `_PORT` | `` / `8080` | Listen address |
| `KRONIK_SERVER_BASE_PATH` | | Serve under a path, e.g. `/cron` |
| `KRONIK_SERVER_USER_HEADER` | `X-User` | Header with the user, from the proxy |
| `KRONIK_NODE_NAME` | host name + random | Replica name shown in runs; unique per replica |
| `KRONIK_SCHEDULER_POLL_INTERVAL` | `1s` | How often due jobs are looked for |
| `KRONIK_SCHEDULER_MAX_CONCURRENT` | `50` | Runs at once per replica |
| `KRONIK_SCHEDULER_MAX_OUTPUT` | `1048576` | Output bytes kept per run (the end) |
| `KRONIK_SCHEDULER_SHUTDOWN_TIMEOUT` | `30s` | Time runs get to finish on stop |
| `KRONIK_SCHEDULER_NODE_TIMEOUT` | `45s` | A replica silent this long is gone |
| `KRONIK_ENVIRONMENT` | | **UI** Name shown at the top (prod, preprod, sandbox) |
| `KRONIK_ENVIRONMENT_COLOR` | from the name | **UI** Colour of that label |
| `KRONIK_SCHEDULER_RUN_RETENTION` | `720h` | **UI** Delete finished runs older than this; `0` keeps |
| `KRONIK_SCHEDULER_RUNS_PER_JOB` | `200` | **UI** Keep at most this many finished runs per job; `0` keeps |
| `KRONIK_SCHEDULER_AUDIT_RETENTION` | `2160h` | **UI** Delete audit entries older than this; `0` keeps |
| `KRONIK_SCHEDULER_CLEANUP_AT` | `01:00` | **UI** Time of the daily cleanup |
| `KRONIK_SCHEDULER_CLEANUP_TIMEZONE` | `UTC` | **UI** Its time zone |
| `KRONIK_EXECUTOR_<TYPE>_ENABLED` | `true` | **UI** Turn an executor on or off: `HTTP`, `JS`, `COMMAND`, `DOCKER`, `SWARM`, `KUBERNETES` |
| `KRONIK_EXECUTOR_COMMAND_SHELL` | `/bin/sh` | Shell that runs commands |
| `KRONIK_EXECUTOR_JS_MAX_DURATION` | `10m` | **UI** Longest a script runs when its job has no timeout |
| `KRONIK_EXECUTOR_JS_MAX_REQUESTS` | `1000` | **UI** `fetch` calls per run; `0` is no limit |
| `KRONIK_EXECUTOR_JS_MAX_RESPONSE` | `10485760` | **UI** Largest response body `fetch` reads |
| `KRONIK_SECRET_<NAME>` | | A script secret, `secrets.<NAME>` |
| `KRONIK_EXECUTOR_DOCKER_HOST` | `DOCKER_HOST` or the socket | **UI** Docker Engine address |
| `KRONIK_EXECUTOR_DOCKER_ALLOWED_IMAGES` | any | **UI** Image patterns (`alpine:*`, `ghcr.io/acme/*`) |
| `KRONIK_EXECUTOR_DOCKER_NETWORK` | | **UI** Default network |
| `KRONIK_EXECUTOR_DOCKER_ALLOW_PRIVILEGED` / `_ALLOW_MOUNTS` | `false` | **UI** |
| `KRONIK_EXECUTOR_DOCKER_PULL` | `missing` | **UI** `missing`, `always` or `never` |
| `KRONIK_EXECUTOR_SWARM_HOST` | the Docker host | **UI** A Swarm manager's engine |
| `KRONIK_EXECUTOR_SWARM_ALLOWED_IMAGES` / `_NETWORKS` / `_ALLOW_MOUNTS` | | **UI** |
| `KRONIK_EXECUTOR_KUBERNETES_KUBECONFIG` / `_CONTEXT` | in-cluster | **UI** |
| `KRONIK_EXECUTOR_KUBERNETES_NAMESPACE` | kronik's own | **UI** Default namespace |
| `KRONIK_EXECUTOR_KUBERNETES_ALLOWED_NAMESPACES` / `_ALLOWED_IMAGES` / `_SERVICE_ACCOUNT` | | **UI** |
| `KRONIK_EXECUTOR_KUBERNETES_TTL` | `10m` | **UI** Finished Jobs are deleted after this |
| `KRONIK_MCP_ENABLED` | `true` | **UI** Serve MCP on `<base_path>/mcp` |
| `KRONIK_MCP_READ_ONLY` | `false` | **UI** Only the MCP tools that read |
| `LOG_LEVEL` | `info` | |

Lists in the environment are comma-separated:
`KRONIK_EXECUTOR_DOCKER_ALLOWED_IMAGES=alpine:*,ghcr.io/acme/*`.

Telemetry: set `OTEL_EXPORTER_OTLP_ENDPOINT` to send traces and metrics.

A replica runs only the executors it has turned on and leaves the runs of the
others to replicas that have them.

## Settings from the UI

*Settings → Configuration* changes everything marked **UI** above while
kronik runs, grouped as General, Retention, Executors, JavaScript, Docker,
Docker Swarm, Kubernetes and MCP. A change is stored in PostgreSQL
(`kronik_settings`), so every replica applies it within five seconds; it is
in the audit log with its old and new value and the user who made it.
Resetting a setting goes back to the configuration's value.

- Executors turned off stop taking runs; jobs of that type stay and run
  again when it is turned back on. Runs going on finish.
- Docker, Swarm and Kubernetes have *Test connection*, which connects with
  the saved settings and, for Kubernetes, checks that kronik may create Jobs.
- Turning on an executor that reaches the host or cluster (commands,
  Docker, Swarm, Kubernetes, privileged containers, mounts) asks first.

What differs per replica or needs a restart stays in the environment: the
database, the listen address, the user header, the poll interval, the
number of runs at once, the shutdown wait and the script secrets.

## Behind a reverse proxy

kronik has no login of its own. Anyone who reaches it can create command
jobs, which run shell commands in the kronik container, and read script
secrets through a script. So:

- Only the proxy may reach kronik (bind it to an internal network, or use a
  NetworkPolicy).
- The proxy signs users in and sets the user header (`X-User`), **replacing**
  any the client sent. A request with two values is treated as having none.
- kronik refuses cross-site writes (`Sec-Fetch-Site`/`Origin`) and bodies not
  sent as `application/json`, so another site cannot make a signed-in
  browser change jobs.

The user is recorded with every change, shown under *Settings → About* and
at the bottom of the sidebar.

## Deploy

| | File | |
| --- | --- | --- |
| Docker | [deploy/compose.yaml](deploy/compose.yaml) | One replica with its PostgreSQL |
| Docker Swarm | [deploy/swarm-stack.yaml](deploy/swarm-stack.yaml) | 3 replicas, DSN from a secret |
| Kubernetes | [deploy/kubernetes.yaml](deploy/kubernetes.yaml) | Deployment + Service, DSN and script secrets from a Secret |

Image: `ghcr.io/rakunlabs/kronik` (linux/amd64, linux/arm64). It has `sh`,
`curl`, `ca-certificates` and `tzdata`. For command jobs that need more
tools, build on top of it:

```dockerfile
FROM ghcr.io/rakunlabs/kronik:latest
USER root
RUN apk add --no-cache postgresql-client aws-cli
USER 65532:65532
```

## Jobs

| Field | |
| --- | --- |
| `schedule` | Five-field cron (`*/5 * * * *`), or `@hourly`, `@daily`, `@weekly`, `@monthly`, `@yearly`, `@every 90s` |
| `timezone` | IANA name, default `UTC`. A `CRON_TZ=` prefix in the schedule wins. `Local` is refused |
| `enabled` | Paused jobs keep their runs but are not scheduled or retried; *Run now* still works |
| `concurrency` | When a run is due (or *Run now*) while another is queued or running: `forbid` (skip it; *Run now* answers 409, default), `allow` (run both), `replace` (cancel the old one) |
| `catch_up` | Times missed while no replica ran: `false` runs only the latest, `true` runs each (up to 100) |
| `timeout_seconds` | `0` means none (scripts: `executor.js.max_duration`, HTTP: 10 minutes) |
| `retries`, `retry_delay_seconds` | Failed or timed-out runs are tried again as new runs |

Cron is parsed by [hardloop](https://github.com/rakunlabs/hardloop).
Day-of-month and day-of-week must **both** match (`0 0 13 * 5` is Friday
the 13th), unlike Vixie cron. Times that do not exist in a DST change are
skipped and repeated ones run once. A schedule with no time in the next five
years is refused. Editing a job without changing its schedule keeps its next
run time.

*Test run* (`POST /api/v1/jobs/test`) runs a job once, as given, without
saving it, for a minute at most; four at a time per replica. HTTP and
command tests do their real work.

### HTTP

```json
{
  "method": "POST",
  "url": "https://api.example.com/jobs/cleanup",
  "headers": { "Authorization": "Bearer …" },
  "body": "{\"days\": 30}",
  "expected_status": [200, 202],
  "insecure_skip_verify": false,
  "no_redirect": false
}
```

Success is any 2xx, or one of `expected_status`. The response status line
and body are the run output. Every request carries `X-Kronik-Job`,
`X-Kronik-Run`, `X-Kronik-Attempt` and `X-Kronik-Scheduled-At`; the run id
lets the receiver drop a duplicate. The job's headers are not sent on a
redirect to another host.

### Command

```json
{ "command": "pg_dump \"$DB\" | gzip > /backup/db.sql.gz", "env": { "DB": "…" }, "dir": "/" }
```

Runs with `sh -c` in the replica that took the run. Exit code `0` is
success; stdout and stderr are the output. The environment is the replica's
without kronik's own settings (`KRONIK_*`, `CONFIG_*`, `OTEL_*`, `VAULT_*`,
`CONSUL_*`), plus the job's `env` and `KRONIK_JOB_ID`, `KRONIK_JOB_NAME`,
`KRONIK_RUN_ID`, `KRONIK_ATTEMPT`, `KRONIK_SCHEDULED_AT`. A cancel or timeout
sends SIGTERM to the command's process group and SIGKILL 10 seconds later.

## JavaScript

```json
{ "script": "const r = await fetch(env.URL)\nreturn { ok: r.ok }", "env": { "URL": "https://…" } }
```

The script is the body of an async function, run by
[goja](https://github.com/dop251/goja) (ES2020-ish, no Node or browser APIs).

```js
const [users, orders] = await Promise.all([
  fetch('https://api.example.com/users').then((r) => r.json()),
  fetch('https://internal.example/orders', {
    headers: { Authorization: 'Bearer ' + secrets.API_TOKEN },
    insecure: true, // skip the TLS certificate check
  }).then((r) => r.json()),
])

const late = orders.filter((o) => o.status === 'late')
console.log(users.length, 'users,', late.length, 'late')

if (late.length) {
  await fetch(secrets.SLACK_URL, { method: 'POST', body: { text: `${late.length} late orders` } })
}

return { users: users.length, late: late.length }
```

- `return` sets the run's **result**, kept as JSON and shown on the run, in
  the run list and as the job's last result. A result over 64 KiB is not kept.
- `throw` (or a rejected `await`) fails the run; `console.log`, `warn`,
  `error` write the output.
- `fetch(url, init)` works as in browsers: `init` has `method`, `headers`,
  `body` (an object is sent as JSON), `redirect` (`follow`, `manual`,
  `error`), plus `insecure: true` to skip the TLS check and `timeout` (ms,
  default 5 minutes). The response has `ok`, `status`, `statusText`, `url`,
  `headers.get()`, `text()` and `json()`. `Headers` and `Response` exist too.
- `sleep(ms)`, `job` (`id`, `name`, `runId`, `attempt`, `scheduledAt`), `env`
  (the job's own values, stored with it) and `secrets`.
- Limits: `executor.js.max_requests` fetches per run, 16 at once,
  `executor.js.max_response` per body, and the job timeout. There is no
  memory limit per script: set one on the container.

**Secrets** come from the replica's configuration, `KRONIK_SECRET_API_TOKEN`
is `secrets.API_TOKEN` (or `executor.js.secrets` in the config file). They
are not stored with jobs and the UI shows only their names. Their values
(4 bytes or longer) are replaced by `[secret]` in the output, the result and
errors. That hides a mistake, not a script that sets out to show one
(encoded, split up): anyone who can edit or test a script can read every
secret.

## Containers

`docker`, `swarm` and `kubernetes` jobs run an image and take the same
config:

```json
{
  "image": "ghcr.io/acme/report:1.4",
  "command": ["sh", "-c"],
  "args": ["generate --since \"$KRONIK_SCHEDULED_AT\""],
  "env": { "REGION": "eu" },
  "cpu": "0.5",
  "memory": "512Mi",
  "pull": "missing",
  "workdir": "/app",
  "user": "1000:1000",
  "labels": { "team": "data" }
}
```

`command` replaces the image's ENTRYPOINT, `args` its CMD. The output is the
container's stdout and stderr, followed live; exit code 0 is success. A
cancel or timeout stops it (SIGTERM, SIGKILL 10 seconds later). What kronik
starts is labelled `kronik.run`, `kronik.job` and `kronik.managed=true` and
removed when the run ends.

| | Docker | Swarm | Kubernetes |
| --- | --- | --- | --- |
| Runs as | a container on the engine | a `replicated-job` service, placed by Swarm | a `batch/v1` Job, one pod, `backoffLimit: 0` |
| Removed | the container, at once | the service, at once | the Job, at once (and `ttlSecondsAfterFinished`) |
| Network | `network` | `networks` (overlay) | the pod network |
| Place | the engine | `constraints` (`node.role==worker`) | `node_selector` |
| Also | `mounts`, `privileged` | `mounts` | `namespace`, `service_account`, `secret_env` |

`secret_env` sets variables from Kubernetes Secrets in the Job's namespace
(`{"TOKEN": "api-credentials/token"}`); their values never pass through
kronik. `mounts` (`{"type": "bind", "source": "/srv/data", "target": "/data",
"read_only": true}` or `"type": "volume"`) and `privileged` are refused
unless the settings allow them; `allowed_images` limits the images and
`allowed_namespaces` the namespaces.

**Access.** Docker and Swarm need a Docker Engine: mount
`/var/run/docker.sock` (see `deploy/compose.yaml` and
`deploy/swarm-stack.yaml`, Swarm needs a manager) or set the host to a remote
engine (`tcp://…` with `DOCKER_TLS_VERIFY`/`DOCKER_CERT_PATH`). Access to an
engine is root on its host. Kubernetes uses the pod's service account
(in-cluster) or a kubeconfig; `deploy/kubernetes.yaml` has the Role it needs
(create, get, list, delete Jobs; list pods; read pod logs).

## Runs

| Status | |
| --- | --- |
| `queued` | Waiting for a replica (or its retry delay) |
| `running` | Output is saved every 2 seconds while it runs |
| `succeeded`, `failed`, `timed_out` | |
| `canceled` | From the UI/API, or replaced by a newer run |
| `skipped` | Due while a previous run was still active (`forbid`) |
| `lost` | The replica running it stopped, restarted, or could not save the result |

*Runs* searches by job name, error, who started a run or a run id, filters
by job, status and time (last hour to last 30 days, or between two dates),
and *Mine* keeps the runs of the jobs you created or changed and the runs you
started. The filters are in the address, so a search can be shared.

Runs are delivered at most once: a lost run is not run again. Make jobs
that must not be missed idempotent and add retries on your side, or watch
for `lost` runs.

## Retention

Once a day at `cleanup_at` (01:00 UTC by default) one replica deletes finished
runs older than the run retention (30 days) and beyond the runs kept per job
(200), and audit entries older than the audit retention (90 days). Whichever
replica gets there first takes the day; the others skip it. *Settings →
Configuration → Retention* changes all of this, and *Clean up now* runs it at
once.

## Replicas

All replicas are equal; there is no leader. Every poll interval each one:

1. takes due jobs with `SELECT … FOR UPDATE SKIP LOCKED`, creates their runs
   and moves `next_run_at` forward. A unique index on `(job, scheduled time)`
   keeps a time from being created twice. A job that cannot be scheduled (a
   time zone this replica does not know) is tried again a minute later.
2. claims queued runs of its executors the same way, up to its free slots,
   and runs them.

So each run is taken by one replica, and the work spreads over all of them.
Replicas write a heartbeat; the runs of a replica silent for
`scheduler.node_timeout` are marked `lost`, checked every `node_timeout/3`.
A replica that starts under a name seen before (a restarted pod) marks the
runs that name left running as `lost`. Results that cannot be saved are
retried for two minutes.

*Settings → Replicas* in the UI lists them; *Settings → About* shows the
version, commit and build date of the replica serving the page.

Clocks matter: run times are compared with each replica's own clock. Keep
them in sync (NTP), as usual.

## Audit log

Every change is recorded with the user from the proxy's header and where it
came from (`ui`, `api` or `mcp`): creating, changing (which fields), pausing,
resuming, deleting and running a job, canceling a run, and test runs (with
the script or command). It is in *Settings → Audit log*, on each job's page,
in `GET /api/v1/audit` and in the replica's log (`msg=audit`). Jobs show who
created and last changed them, runs who started and canceled them.

## MCP

AI agents (Claude, Cursor, any MCP client) can manage jobs on
`<base_path>/mcp` (Streamable HTTP):

```json
{ "mcpServers": { "kronik": { "type": "http", "url": "https://kronik.example.com/mcp" } } }
```

Tools, each prefixed `kronik_`: `list_jobs`, `get_job`, `list_runs`, `get_run`,
`schedule_preview`, `audit_log`, and unless `mcp.read_only`: `create_job`,
`update_job`, `pause_job`, `resume_job`, `trigger_job`, `cancel_run`,
`delete_job`, `test_job`. Jobs can be named by id or name. Changes go through
the same checks as the UI and are in the audit log as `mcp` with the user
the proxy sent: put the MCP endpoint behind the same proxy.

## API

JSON under `/api/v1` (below `server.base_path`). Bodies must be sent as
`application/json`. Errors are `{"message": "…"}`.

| | |
| --- | --- |
| `GET /jobs?search=&tag=` | Jobs with their last run and result |
| `POST /jobs` | Create (body: the job fields above, with `executor` and `config`) |
| `GET /jobs/{id}` · `PUT /jobs/{id}` · `DELETE /jobs/{id}` | `PUT` replaces the job; send `updated_at` to refuse it (409) when the job changed since |
| `POST /jobs/{id}/enable` · `/disable` | |
| `POST /jobs/{id}/trigger` | Queue a run now, returns `{"run_id"}`; 409 for a `forbid` job with a run active |
| `POST /jobs/test` | Run a job once without saving it; 429 when four are going on |
| `POST /script/check` | `{"script"}` → `{"problems": [{line, column, message}]}` |
| `GET /runs?job_id=&status=failed,lost&q=&user=&mine=true&from=&to=&limit=&before=` | Newest first; `from`/`to` are RFC 3339 or dates; `before` is a run id for paging |
| `GET /runs/{id}` | With output and result |
| `POST /runs/{id}/cancel` | |
| `GET /audit?job_id=&user=&limit=&before=` | Newest first |
| `GET /settings` · `PUT /settings` | `{"values": {"retention.runs": "30d"}, "reset": ["environment"]}` |
| `POST /settings/check/{docker,swarm,kubernetes}` | Try the connection |
| `POST /settings/cleanup` | Run the retention cleanup now |
| `GET /schedule/next?schedule=&timezone=&n=` | Next run times of an expression |
| `GET /stats` · `/nodes` · `/tags` | |
| `GET /info` | Version, build, replica, user, executors, secret names, scheduler settings |

```sh
curl -X POST localhost:8080/api/v1/jobs -H 'Content-Type: application/json' -d '{
  "name": "ping", "schedule": "*/5 * * * *", "executor": "http",
  "config": {"url": "https://example.com/health"}, "retries": 2
}'
```

## Development

```sh
make env       # PostgreSQL on :5432
make run       # API on :8080
make run-ui    # UI on :3000 with hot reload
make test      # Go tests, store and scheduler tests against `make env`
make build     # UI + binary into bin/
```

Container tests run against a real engine and cluster when asked:
`KRONIK_TEST_DOCKER=1` (Docker, and Swarm on a manager) and
`KRONIK_TEST_KUBECONFIG=…` (e.g. a `kind create cluster`).

Layout: `cmd/kronik` (wiring), `internal/store` (PostgreSQL, migrations),
`internal/scheduler` (claiming, running, cleanup), `internal/executor` (HTTP,
command, JavaScript, Docker, Swarm, Kubernetes), `internal/settings` (what
the UI changes), `internal/schedule` (cron via hardloop), `internal/service`
(checks and audit), `internal/server` (API, MCP and the embedded UI), `_ui`
(Svelte).
