<img align="right" height="64" src="_ui/public/favicon.svg">

# kronik

[![License](https://img.shields.io/github/license/rakunlabs/kronik?color=blue&style=flat-square)](https://raw.githubusercontent.com/rakunlabs/kronik/main/LICENSE)

Cron jobs with a web UI. One binary, jobs and runs stored in PostgreSQL. Run
as many replicas as you like; each scheduled time runs exactly once.

A job is an **HTTP request**, a **JavaScript** script (`fetch`, `await`,
`console.log`, `return` a result), a **shell command**, or a **container**
on Docker, Docker Swarm or Kubernetes. It runs on a cron schedule in any time
zone, with timeout, retries and overlap rules, and keeps its output and
result. AI agents can manage jobs over **MCP**. Everything else is set from
*Settings → Configuration* and applies to every replica at once.

## Quick start

```sh
docker compose -f deploy/compose.yaml up -d
```

Open http://localhost:8080 and create a job.

kronik needs only a PostgreSQL connection; its tables start with `kronik_`:

```sh
KRONIK_STORE_DSN='postgres://user:pass@host:5432/kronik' kronik
```

There is no login. Put kronik behind your reverse proxy and have it send
the user in `X-User`: every change is recorded with it.

Docker Swarm and Kubernetes examples are in [deploy/](deploy/). Everything
else (configuration, containers, scripts, secrets, MCP, API, how replicas
share work) is in [DETAILS.md](DETAILS.md).
