# Architecture

AI-Observable is local-first. The proxy on `127.0.0.1:7878` forwards requests
to an OpenAI-compatible provider, asynchronously writes sanitized metadata to
SQLite WAL, and periodically creates a `VACUUM INTO` snapshot. Grafana reads
only that non-WAL snapshot from a read-only directory mount, preventing host/
container WAL sharing problems on Docker Desktop.

The admin service on `127.0.0.1:7879` exposes health and status only for local
hosts. API credentials are forwarded but never stored.
