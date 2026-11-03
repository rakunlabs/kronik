<img align="right" height="64" src="_ui/public/favicon.svg">

# kronik

[![License](https://img.shields.io/github/license/rakunlabs/kronik?color=blue&style=flat-square)](https://raw.githubusercontent.com/rakunlabs/kronik/main/LICENSE)
[![Coverage](https://img.shields.io/sonar/coverage/rakunlabs_kronik?logo=sonarcloud&server=https%3A%2F%2Fsonarcloud.io&style=flat-square)](https://sonarcloud.io/summary/overall?id=rakunlabs_kronik)
[![GitHub Workflow Status](https://img.shields.io/github/actions/workflow/status/rakunlabs/kronik/test.yml?branch=main&logo=github&style=flat-square&label=ci)](https://github.com/rakunlabs/kronik/actions)

Cron jobs with a web UI. One binary, jobs and runs stored in PostgreSQL. Run
as many replicas as you like; each scheduled time runs exactly once.

## Features

- **Jobs of many kinds**: HTTP requests, JavaScript scripts (`fetch`,
  `await`, `console.log`, `return` a result), shell commands, and containers
  on Docker, Docker Swarm or Kubernetes.
- **Scheduling**: five-field cron or `@every`, in any time zone, with
  timeout, retries, overlap rules (`forbid`, `allow`, `replace`), catch-up
  and a random delay.
- **Replicas**: no leader; due jobs and queued runs are claimed with
  `SKIP LOCKED`, so the work spreads and each run is taken once. Changes
  reach every replica at once over `LISTEN/NOTIFY`.
- **Runs**: output followed live, results kept as JSON, a chart of the last
  runs, search and filters, cancel and run again.
- **Notifications**: Slack, Teams, webhooks or e-mail when a job fails,
  recovers, succeeds or does not run in time.
- **Encrypted secrets** set from the UI (AES-256-GCM, envelope encryption),
  used as `secrets.NAME` and `{{secret.NAME}}`, hidden in output.
- **Chains and webhooks**: run a job after others, or from a URL; report runs
  made elsewhere so kronik watches them too.
- **Maintenance**: pause all schedules, maintenance windows, runs at once per
  tag.
- **Settings from the UI**, shared by every replica, with an audit log of
  every change and job versions you can restore.
- **Calendar** of upcoming runs, **OpenTelemetry** metrics, and **MCP** so AI
  agents can manage jobs.

## Quick start

```sh
docker compose -f deploy/compose.yaml up -d
```

Open http://localhost:8080 and create a job.

kronik needs only a PostgreSQL connection; its tables start with `kronik_`:

```sh
KRONIK_STORE_DSN='postgres://user:pass@host:5432/kronik' \
KRONIK_SECRETS_KEY='any secret text' kronik
```

There is no login. Put kronik behind your reverse proxy and have it send
the user in `X-User`: every change is recorded with it.

## Configuration

Configuration is loaded with [chu](https://github.com/rakunlabs/chu). Each
source is read in order and a later one wins over an earlier one:

1. **Defaults**
2. **Secret stores** below, each used only when its address, region or
   project is set.
3. **HTTP**: `CONFIG_HTTP_ADDR` fetches it from a config server.
4. **File**: `kronik.yaml` (or `.yml`, `.json`, `.toml`) in the working
   directory, or the path in `CONFIG_FILE`.
5. **Environment**: `KRONIK_` and the keys joined with `_`;
   `store.dsn` is `KRONIK_STORE_DSN`. `.env` and `.env.local` are read too.

| Source | Turned on by | Also |
| --- | --- | --- |
| HashiCorp Vault | `VAULT_ADDR` or `VAULT_AGENT_ADDR` | `VAULT_SECRET_BASE_PATH`, AppRole: `VAULT_ROLE_ID`, `VAULT_SECRET_ID`, `VAULT_APPROLE_BASE_PATH` |
| Consul | `CONSUL_HTTP_ADDR` | `CONSUL_CONFIG_PATH_PREFIX` |
| AWS Secrets Manager | `AWS_REGION` or `AWS_DEFAULT_REGION` | `AWS_SECRETS_PREFIX` |
| AWS SSM Parameter Store | `AWS_REGION` or `AWS_DEFAULT_REGION` | `AWS_SSM_PREFIX` |
| Azure Key Vault | `AZURE_KEYVAULT_NAME` or `AZURE_KEYVAULT_URL` | |
| GCP Secret Manager | `GCP_PROJECT_ID` | `GCP_SECRET_LOCATION`, `GCP_SECRET_VERSION`, `GCP_SECRET_ADDITIONAL` |
| GCP Parameter Manager | `GCP_PROJECT_ID` | `GCP_PARAMETER_LOCATION`, `GCP_PARAMETER_VERSION`, `GCP_PARAMETER_FALLBACK` |

Each store reads the configuration named `kronik` (the secret, key or
parameter `kronik` under the prefix), in the same shape as the file. A source can be skipped with `CONFIG_SET_<NAME>=false`
(`CONFIG_SET_VAULT`, `CONSUL`, `AWSSECRETS`, `AWSSSM`, `AZUREKEYVAULT`,
`GCPSECRET`, `GCPPARAMETER`, `HTTP`, `FILE`, `ENV`), e.g. when it is down.
Keeping `store.dsn` and `secrets.key` in a secret store keeps them out of the
environment.

A store that is turned on but cannot be read (no access, wrong path) stops
kronik from starting; a missing `kronik` entry is skipped. `AWS_REGION` and
`GCP_PROJECT_ID` are often set by the platform (EKS, ECS, GKE): if you do not
keep the configuration there, turn those stores off, e.g.
`CONFIG_SET_AWSSECRETS=false CONFIG_SET_AWSSSM=false`. Vault also needs
`VAULT_SECRET_BASE_PATH` (the mount) once `VAULT_ADDR` is set.

Values marked *UI* below are only the starting point: *Settings →
Configuration* changes them while kronik runs, for every replica.

```yaml
log_level: info
node_name: ""               # replica name; empty is the host name + random
environment: prod           # UI: label at the top (prod, preprod, sandbox)
environment_color: ""       # UI: its colour; empty picks one from the name

server:
  host: ""
  port: "8080"
  base_path: ""             # serve under a path, e.g. /cron
  user_header: X-User       # header with the user, from the proxy

store:
  dsn: postgres://kronik:secret@db:5432/kronik?sslmode=require
  # Used only to create and change the tables, when the dsn user may not.
  migrate_dsn: postgres://kronik_owner:secret@db:5432/kronik?sslmode=require
  table_prefix: kronik_
  max_open_conns: 10

secrets:
  # Encrypts the secrets set from the UI: any text, even empty; its SHA-256
  # is the key. The same on every replica. When it is changed from the UI
  # (Settings → Secrets), set the new one here before the next restart.
  key: a long random text

scheduler:
  poll_interval: 1s
  max_concurrent: 50        # runs at once per replica
  max_output: 1048576       # output bytes kept per run
  shutdown_timeout: 30s
  node_timeout: 45s
  run_retention: 720h       # UI
  runs_per_job: 200         # UI
  audit_retention: 2160h    # UI
  cleanup_at: "01:00"       # UI
  cleanup_timezone: UTC     # UI
  tag_limits: []            # UI: ["reports=2"]

executor:                   # UI: every value
  http: { enabled: true }
  command: { enabled: true, shell: /bin/sh }
  js:
    enabled: true
    max_duration: 10m
    max_requests: 1000
    max_response: 10485760
    secrets:                # secrets.NAME, or KRONIK_SECRET_<NAME>
      API_TOKEN: "..."
  docker: { enabled: true, host: "", allowed_images: [], network: "", pull: missing }
  swarm: { enabled: true, host: "", allowed_images: [], networks: [] }
  kubernetes: { enabled: true, namespace: "", allowed_namespaces: [], ttl: 10m }

notify:                     # UI
  enabled: true
  base_url: https://cron.example.com
  smtp_host: smtp.example.com
  smtp_port: 587
  smtp_user: kronik
  smtp_password_secret: SMTP_PASSWORD   # name of a secret
  smtp_from: kronik@example.com
  smtp_tls: starttls

maintenance:                # UI
  paused: false
  windows: ["0 2 * * 0|2h"]
  timezone: UTC

metrics: { enabled: true, per_job: true }   # UI
mcp: { enabled: true, read_only: false }

telemetry:                  # OpenTelemetry, or OTEL_EXPORTER_OTLP_ENDPOINT
  collector: ""
```

Every key, with its environment variable, is in
[DETAILS.md](DETAILS.md#configuration).

Docker Swarm and Kubernetes examples are in [deploy/](deploy/). Everything
else (containers, scripts, secrets, MCP, API, how replicas share work) is in
[DETAILS.md](DETAILS.md).
