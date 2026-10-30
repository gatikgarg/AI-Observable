# Changelog

## 2026-10-28 - Agent integration

- Added `/gw/v1` gateway-token routing inside the existing proxy for Chat
  Completions, Responses, and model discovery.
- Added additive `integration_id` and `agent_type` call attribution and a
  per-integration metadata registry in `control.db`.
- Added Codex automatic configuration planning/apply/backup/rollback and guided
  Cline/Kilo Code setup.
- Added the Integrations API Manager page with one-time tokens, restart notices,
  verification checks, and honest traffic-verified state.
- Gateway traffic defaults to metadata-only capture.
- Added current DeepSeek Flash/V4 Pro price estimates with peak/off-peak caveats.
- Fixed streamed usage/normalization, content-based TTFT, truncated-stream
  success, empty model handling, and silent queue-overflow diagnostics.
- Added dashboard hash freeze and 40-query golden tests. Dashboard and Grafana
  provisioning files are unchanged.
