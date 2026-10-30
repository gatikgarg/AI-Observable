<p align="center">
  <img
    src="gemini-svg (5).svg"
    alt="AI-Observable Logo"
    width="700"
  />
</p>
# AI-Observable

Monitor and track costs, efficiency and other metrics for all your AI/LLM API
usage in one place from your own computer. AI-Observable runs a local proxy in
front of OpenAI-compatible apps and coding agents, then shows cost, latency,
token usage, errors, sessions and context usage in Grafana without sending
monitoring data to the cloud.

## Quick start

All you need is [Docker Desktop](https://www.docker.com/products/docker-desktop/)
with Docker Compose v2. You do not need Go or a prebuilt executable.

```sh
git clone https://github.com/gatikgarg/AI-Observable.git
cd AI-Observable
docker compose up --build -d
```

On Windows, the included launcher also waits for both services to become
healthy:

```powershell
.\start-all.ps1
```

The first run builds the app image and installs the Grafana SQLite plugin, so
it can take a minute or two. Later starts reuse Docker's build cache.

Open:

- API Manager: <http://localhost:7879>
- Grafana: <http://localhost:3000> (`admin` / `admin`)
- Proxy: `http://localhost:7878/<provider>/v1`

Data and Grafana snapshots persist in the `llmtap-data` Docker volume:

```sh
docker compose logs -f      # follow app and Grafana logs
docker compose down         # stop containers, keep data
docker compose down -v      # stop and delete all collected data
```

## Local development

Go 1.24 or newer is only required when developing outside containers:

```sh
make test
make build
```
