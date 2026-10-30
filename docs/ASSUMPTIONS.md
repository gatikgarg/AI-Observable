# Assumptions and verification record

Verification was completed on 2026-10-25 after installing the official Go
1.25.0 Windows toolchain and starting Docker Desktop 4.88.1 (Engine 29.7.2).

- Grafana `13.0.1` starts successfully and reports `/api/health` version
  `13.0.1`.
- `GF_INSTALL_PLUGINS=frser-sqlite-datasource 4.0.6` was accepted; the
  datasource health API for `llmtap-sqlite` returned `{"status":"OK"}`.
- All four dashboard JSON files were accepted by provisioning. The Overview
  dashboard API returned the expected panels and datasource UID. Full query
  execution still needs a dedicated verifier implementation; the current
  `cmd/verifydash` performs structural JSON validation.
- `VACUUM INTO` uses modernc SQLite's documented SQL path and is followed by
  `journal_mode=DELETE`; run snapshot tests once Go is installed.
- Provider response field parsing currently supports OpenAI-compatible JSON;
  streaming terminal usage extraction must be expanded and validated with
  recorded vendor fixtures before release.
- Docker Desktop licensing and current Codex setup details must be checked
  against current vendor documentation before making release claims.
- Addendum dashboard time bucketing uses a range-derived SQL expression because
  `$__interval_ms` interpolation was not yet proven against the SQLite plugin.
- Grafana 13.0.1 provisions the core panel types used by the addendum files;
  query execution for every new panel and xychart/heatmap frame compatibility
  remain items for the extended verifier.
- Addendum schema columns (`project`, `request_id`, `trace_id`, `attempt`, and
  cost components) are now created idempotently at startup and included in
  both normal and rolling `calls_v` views. The regenerated demo snapshot is
  below 10 MB.
- Grafana API checks returned HTTP 200 for `llmtap-cost`,
  `llmtap-reliability`, `llmtap-latency`, and `llmtap-efficiency`; the SQLite
  datasource health endpoint also returned HTTP 200/OK.
- The SQLite plugin requires `rawQueryText`, `queryText`, `queryType: "table"`,
  and `timeColumns` on dashboard targets. The prior generic `query` property
  alone rendered empty frames. All current targets now include the plugin fields;
  a Grafana API query returned the five real demo models and all 40 targets ran
  successfully after substituting the selected time range.

## Coding-agent integration (2026-10-28)

- **[verified]** Current OpenAI Codex documentation says custom providers use
  `wire_api = "responses"`; Chat Completions is no longer a supported value.
  Source: <https://developers.openai.com/codex/config-reference>, fetched
  2026-10-28.
- **[verified]** Codex user-level provider settings use
  `~/.codex/config.toml`; project config cannot set `model_provider` or
  `model_providers`.
- **[verified]** Current DeepSeek pricing names `deepseek-flash` and
  `deepseek-v4-pro`, reports a 1M context, and advertises Responses API support.
  Source: <https://api-docs.deepseek.com/quick_start/pricing>, fetched
  2026-10-28.
- **Cost estimate:** The embedded DeepSeek V4 prices use published peak rates.
  Off-peak rates are half those values. Peak hours are 01:00-04:00 and
  06:00-10:00 UTC Monday-Friday, excluding Chinese public holidays. Locally
  calculated cost is therefore an estimate; provider token totals are primary.
- **[assumed]** Codex IDE extension instances pick up a user-scope environment
  variable after fully restarting the extension host. Verification plan: a real
  Codex request after quitting all VS Code windows. The UI requires that restart.
- **[assumed]** Cline and Kilo Code detection by extension-directory names is
  advisory only. Verification plan: use guided setup when detection is absent;
  confirm settings through the real extension UI.
- **[assumed]** Kilo Code's personal config-file location and schema. Verification
  plan: inspect the installed extension's documented configuration; v1 remains
  guided until proven.
- The Windows registry round-trip test is present but can be skipped when the
  process sandbox denies registry writes. CI's `windows-latest` job runs it
  without that sandbox restriction.
- No real provider key or real coding-agent run was performed during automated
  verification. `docs/COMPATIBILITY.md` therefore marks all combinations mock
  only and pending human traffic verification.
