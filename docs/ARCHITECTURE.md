# Architecture

AI-Observable is local-first. The Compose stack starts the Go proxy/API Manager
and Grafana together. Host ports are published only on `127.0.0.1`; inside the
app container, loopback listeners are exposed through a local TCP forwarder so
the API Manager keeps its local-only trust boundary.

The proxy on `127.0.0.1:7878` forwards requests to an OpenAI-compatible
provider, asynchronously writes sanitized metadata to SQLite WAL, and
periodically creates a `VACUUM INTO` snapshot. Grafana mounts the shared Docker
volume read-only and reads only that non-WAL snapshot, avoiding host/container
WAL sharing problems. The admin service on `127.0.0.1:7879` serves the API
Manager, health and status. API credentials are sealed in the local vault.
