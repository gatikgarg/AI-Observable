# Coding-Agent Compatibility

Checked: 2026-10-28

Automated tests use mock upstreams. "Verified (mock only)" proves the gateway,
recording, configuration, and protocol path, but it is not a real-agent claim.
No result is marked "Verified (traffic seen, real agent)" until a human records
the agent version, provider, model, date, a call count, and dashboard evidence.

| Agent | Provider | Wire | Status | Evidence / caveat |
|---|---|---|---|---|
| Codex | OpenAI-compatible Responses provider | Responses | Verified (mock only); real-agent verification pending (human) | `TestGatewayResponsesStreamUsesTerminalUsage`; current official Codex config uses `wire_api = "responses"` |
| Codex | DeepSeek | Responses | Verified (mock only); real-agent verification pending (human) | DeepSeek now advertises Responses on its official pricing page; no real Codex run or screenshot exists in this checkout |
| Cline | DeepSeek or another compatible provider | Chat Completions | Gateway verified (mock only); real-agent verification pending (human) | `TestIntegrationCreateApplyAndGatewayTraffic`; Cline settings are guided and were not driven through VS Code |
| Kilo Code | DeepSeek or another compatible provider | Chat Completions | Gateway verified (mock only); real-agent verification pending (human) | Models passthrough and attributed Chat traffic tested; Kilo config-file location/schema remains unverified |

## Real-agent checklist

1. Use a dedicated small-balance test key, never a production key. Do not put it
   in agent config; keep it in the API Manager.
2. Configure the agent with the one-time gateway token and
   `http://127.0.0.1:7878/gw/v1`.
3. Send a trivial inference request and a tool-call request.
4. Confirm an attributed `calls` row, token usage, TTFT, estimated cost, and
   `Verified (traffic seen)` in Integrations.
5. Confirm the unchanged Cost by Project panel displays the integration label.
6. Cancel a stream and confirm a 499 row.
7. Rotate the token and confirm the old token receives a readable 401.
8. Roll back and confirm the agent can use its original configuration.

At most 10-15 requests should be sent per verification. Token totals are the
primary reconciliation check; estimated cost is secondary because DeepSeek has
peak/off-peak rates.
