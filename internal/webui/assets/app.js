"use strict";
const CSRF = document.body.dataset.csrf;
const VERSION = document.body.dataset.version;
const GRAFANA = document.body.dataset.grafana;
// grafanaBase trims trailing slashes so a configured grafana_url ending in "/"
// does not produce "//d/llmtap-cost", which Grafana answers with 404.
const grafanaBase = (url) => String(url || "").replace(/\/+$/, "");
const $ = (sel, root = document) => root.querySelector(sel);
const esc = (v) => String(v ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const num = (v) => new Intl.NumberFormat().format(Number(v || 0));
const usd = (v) => "$" + Number(v || 0).toFixed(4);

const state = {
  view: "providers", providers: [], jobs: [], keys: [], onboarding: {},
  agents: [], integrations: [], agentProviders: [], tab: "overview",
  instructions: { agent: "cline", providerID: "", integrationID: "", initialized: false },
  pendingTokens: {}, hideDisconnected: localStorage.getItem("hideDisconnected") === "1",
};

function toast(msg, kind = "") {
  const dialog = document.querySelector("dialog[open]");
  const host = dialog || document.body;
  let box = dialog
    ? dialog.querySelector(":scope > .toast.dialog-toast")
    : document.querySelector("body > .toast:not(.dialog-toast)");
  if (!box) {
    box = document.createElement("div");
    box.className = dialog ? "toast dialog-toast" : "toast";
    box.setAttribute("aria-live", kind === "bad" ? "assertive" : "polite");
    host.append(box);
  }
  const el = document.createElement("div");
  el.className = kind;
  el.setAttribute("role", kind === "bad" ? "alert" : "status");
  const text = document.createElement("span");
  text.textContent = msg;
  const close = document.createElement("button");
  close.type = "button";
  close.className = "toast-close";
  close.setAttribute("aria-label", "Dismiss notification");
  close.textContent = "×";
  close.addEventListener("click", () => el.remove());
  el.append(text, close);
  box.append(el);
  setTimeout(() => el.remove(), kind === "bad" ? 12000 : 8000);
}

async function api(path, opts = {}) {
  const init = { method: opts.method || "GET", headers: {} };
  if (opts.body !== undefined) {
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(opts.body);
  }
  if (init.method !== "GET") init.headers["X-LLMTAP-CSRF"] = CSRF;
  const res = await fetch(path, init);
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = { raw: text }; }
  if (!res.ok) {
    const e = (data && data.error) || {};
    const err = new Error(e.message || res.statusText);
    err.status = res.status;
    err.code = e.code || "internal";
    err.field = e.field;
    throw err;
  }
  return data;
}

function banner(html) {
  const b = $("#banner");
  if (!html) { b.hidden = true; b.innerHTML = ""; return; }
  b.hidden = false; b.innerHTML = html;
}

const TITLES = {
  providers: ["Providers", "Connect the APIs you already pay for."],
  keys: ["Keys", "Stored encrypted, shown masked, never sent anywhere but the provider."],
  collection: ["Collection", "What the collector pulled, and what it could not."],
  integrations: ["Integrations", "Route coding agents through Observable. Verified means real traffic was seen."],
  metrics: ["Metrics", "Measured calls, provider-reported cost, and honest fill rates."],
  settings: ["Settings", "Runtime knobs. Secrets are never displayed."],
};

function go(view) {
  state.view = view;
  document.querySelectorAll("#nav button").forEach((b) => b.classList.toggle("on", b.dataset.view === view));
  const [title, sub] = TITLES[view] || TITLES.providers;
  $("#title").textContent = title;
  $("#subtitle").textContent = sub;
  $("#add").hidden = view !== "providers" && view !== "keys" && view !== "integrations";
  $("#add").textContent = view === "integrations" ? "+ Connect agent" : "+ Add API";
  render();
}

function render() {
  if (state.view === "providers") return renderProviders();
  if (state.view === "keys") return renderKeys();
  if (state.view === "collection") return renderCollection();
  if (state.view === "integrations") return renderIntegrations();
  if (state.view === "metrics") return renderMetrics();
  return renderSettings();
}

async function loadAll() {
  try {
    const [onboarding, providers, jobs, keys, agents, integrations] = await Promise.all([
      api("/api/onboarding"), api("/api/providers"), api("/api/collector/jobs"), api("/api/keys"),
      api("/api/agents"), api("/api/integrations"),
    ]);
    const userProviders = providers.filter((p) => p.origin !== "config");
    Object.assign(state, { onboarding, providers: userProviders, jobs, keys, agents, integrations });
    $("#dashLink").href = grafanaBase(onboarding.grafana_url || GRAFANA) + "/d/llmtap-cost";
    if (onboarding.has_providers === false && !localStorage.getItem("wizardSeen")) openWizard();
    render();
  } catch (err) {
    $("#view").innerHTML = `<div class="empty"><b>Cannot reach the manager API</b>${esc(err.message)}</div>`;
  }
}

document.querySelectorAll("#nav button").forEach((b) => b.addEventListener("click", () => go(b.dataset.view)));
$("#refresh").addEventListener("click", loadAll);

function providerCard(p) {
  const creds = p.credentials || [];
  const chips = (p.capabilities || []).map((c) => `<span class="chip">${esc(c)}</span>`).join("") || `<span class="chip muted">no pull lanes</span>`;
  const keys = creds.map((c) => `
    <div class="key"><span><b>${esc(c.label || "key")}</b> <code>${esc(c.masked)}</code></span>
      <span class="pill ${c.status === "ok" ? "ok" : c.status === "unvalidated" ? "" : "bad"}">${esc(c.status)}</span></div>`).join("");
  const base = `/api/providers/${encodeURIComponent(p.id)}`;
  return `<article class="card" data-id="${esc(p.id)}">
    <header><div><h3>${esc(p.name)} <span class="pill">${esc(p.kind)}</span></h3>
      <div class="meta">${esc(p.base_url)}</div></div>
      <span class="pill ${p.enabled ? "ok" : "warn"}">${p.enabled ? "enabled" : "disabled"}</span></header>
    <div class="row muted" style="font-size:12px">auth ${esc(p.auth_mode)} &middot; key style ${esc(p.auth_style)} &middot; ${esc(p.origin)}${p.reconcile_mode ? " &middot; reconcile " + esc(p.reconcile_mode) : ""}</div>
    <div class="chips">${chips}</div>
    ${keys || `<div class="key muted">No key stored. Managed mode needs one.</div>`}
    <footer>
      <button class="ghost" data-act="test" data-url="${base}/test">Test</button>
      <button class="ghost" data-act="toggle" data-id="${esc(p.id)}" data-enabled="${p.enabled ? 0 : 1}">${p.enabled ? "Disable" : "Enable"}</button>
      <button class="ghost" data-act="key" data-id="${esc(p.id)}" data-name="${esc(p.name)}">Add key</button>
      <button class="ghost" data-act="models" data-id="${esc(p.id)}">Sync catalog</button>
      ${p.origin === "config" ? `<span class="pill">imported from config.toml</span>` :
        `<button class="danger" data-act="delete" data-id="${esc(p.id)}" data-name="${esc(p.name)}">Remove</button>`}
    </footer></article>`;
}

function renderProviders() {
  const list = state.providers;
  if (!list.length) {
    $("#view").innerHTML = `<div class="empty"><b>Connect your first API</b>
      Pick a preset, paste a key, and the proxy will start recording real calls.<br><br>
      <button class="primary" onclick="openWizard()">+ Add API</button></div>`;
    return;
  }
  $("#view").innerHTML = `<div class="cards">${list.map(providerCard).join("")}</div>`;
}

function renderKeys() {
  const rows = [];
  for (const p of state.providers) {
    for (const c of p.credentials || []) rows.push({ ...c, provider: p.name });
  }
  if (!rows.length) {
    $("#view").innerHTML = `<div class="empty"><b>No keys stored</b>A key is encrypted with the local vault key and injected by the proxy on each request.</div>`;
    return;
  }
  $("#view").innerHTML = `<table><thead><tr><th>Provider</th><th>Label</th><th>Key</th><th>Status</th><th>Last used</th><th></th></tr></thead><tbody>
    ${rows.map((c) => `<tr><td>${esc(c.provider)}</td><td>${esc(c.label)}</td>
      <td class="mono">${esc(c.masked)}</td>
      <td><span class="pill ${c.status === "ok" ? "ok" : c.status === "unvalidated" ? "" : "bad"}">${esc(c.status)}</span></td>
      <td class="muted">${c.last_used_at_ms ? new Date(c.last_used_at_ms).toLocaleString() : "never"}</td>
      <td class="row"><button class="ghost" data-act="validate" data-id="${esc(c.id)}">Validate</button>
        <button class="danger" data-act="revoke" data-id="${esc(c.id)}">Revoke</button></td></tr>`).join("")}
  </tbody></table>`;
}

function integrationCard(i) {
  const provider = state.providers.find((p) => p.id === i.provider_config_id);
  const healthy = i.first_seen_at_ms ? "ok" : i.state === "needs_attention" ? "bad" : "warn";
  const actions = i.revoked_at_ms
    ? `<span class="muted">Revoked</span>`
    : `<button class="ghost" data-act="verifyagent" data-id="${esc(i.id)}">Verify</button>
       <button class="ghost" data-act="rotateagent" data-id="${esc(i.id)}">Rotate token</button>
       <button class="ghost" data-act="rollbackagent" data-id="${esc(i.id)}">Rollback</button>
       <button class="danger" data-act="removeagent" data-id="${esc(i.id)}">Disconnect</button>`;
  return `<article class="card">
    <header><div><h3>${esc(i.label)} <span class="chip">${esc(i.agent_type)}</span></h3>
      <div class="meta">${esc(provider?.name || i.provider_config_id)} / ${esc(i.model || "model not set")}</div></div>
      <span class="pill ${healthy}">${esc(i.state_label)}</span></header>
    <div class="row muted" style="font-size:12px">capture ${esc(i.capture_mode)} &middot; token ${esc(i.token_prefix)}&hellip;
      &middot; last seen ${i.last_seen_at_ms ? new Date(i.last_seen_at_ms).toLocaleString() : "never"}</div>
    <div class="cards" style="margin:12px 0">
      <div class="stat"><span>calls 24h</span><b>${num(i.stats.calls_24h)}</b></div>
      <div class="stat"><span>cost 24h</span><b>${usd(i.stats.cost_usd_24h)}</b></div>
      <div class="stat"><span>avg latency</span><b>${Math.round(i.stats.avg_latency_ms_24h)} ms</b></div>
    </div>
    <footer>${actions}</footer></article>`;
}

function renderIntegrations() {
  const disconnected = state.integrations.filter((i) => i.revoked_at_ms);
  const visible = state.hideDisconnected
    ? state.integrations.filter((i) => !i.revoked_at_ms)
    : state.integrations;
  const cards = visible.length ? visible.map(integrationCard).join("")
    : `<div class="empty"><b>${state.integrations.length ? "No active coding agents" : "No coding agents connected"}</b>
      Connect Codex, Cline, or Kilo Code so its model requests are measured.<br><br>
      <button class="primary" data-act="agentnew">+ Connect agent</button></div>`;
  const agents = state.agents.map((a) => `<span class="chip">${esc(a.display_name)}: ${esc(a.setup_mode)}</span>`).join("");
  const disconnectedToggle = disconnected.length
    ? `<div class="row" style="justify-content:flex-end;margin-bottom:10px">
        <button class="ghost" data-act="toggledisconnected">${state.hideDisconnected ? "Show" : "Hide"} disconnected (${disconnected.length})</button>
      </div>`
    : "";
  $("#view").innerHTML = `<div class="chips" style="margin-bottom:14px">${agents}</div>
    ${disconnectedToggle}
    <div class="cards">${cards}</div>
    ${renderInstructions()}`;
}

const AGENT_LABELS = { codex: "Codex", cline: "Cline", "kilo-code": "Kilo Code" };
const WIRE_BY_AGENT = { codex: "responses", cline: "chat", "kilo-code": "chat" };
const WIRES_BY_KIND = {
  openai: ["chat", "responses"],
  deepseek: ["chat", "responses"],
  openrouter: ["chat"],
  groq: ["chat"],
  local: ["chat"],
  "openai-compatible": ["chat"],
};

function instructionSection(title, body) {
  return `<div class="instruction-section"><h5>${title}</h5>${body}</div>`;
}

function instructionField(label, value, action = "") {
  return `<li><span>${esc(label)}</span><div class="instruction-field-value">
    <code class="mono">${esc(value)}</code>${action}</div></li>`;
}

function instructionFields(rows) {
  return `<ul class="instruction-fields">${rows
    .map(([label, value, action]) => instructionField(label, value, action))
    .join("")}</ul>`;
}

function integrationShortID(id) {
  return String(id || "").replace(/-/g, "").slice(0, 8);
}

function safeLabel(label) {
  return String(label || "codex").toLowerCase()
    .replace(/[^a-z0-9]+/g, "_")
    .replace(/^_+|_+$/g, "") || "agent";
}

function codexProviderID(integration) {
  const short = integrationShortID(integration?.id) || "<integration-id>";
  return `observable_${safeLabel(integration?.label)}_${short}`;
}

function codexEnvName(integration) {
  const short = integrationShortID(integration?.id) || "<integration-id>";
  return `OBSERVABLE_KEY_CODEX_${short.toUpperCase()}`;
}

function vendorLink(label, url) {
  return url ? `<a class="instruction-link" href="${esc(url)}" target="_blank" rel="noreferrer">${esc(label)}</a>` : "";
}

function instructionSelection() {
  const selected = state.integrations.find((i) => i.id === state.instructions.integrationID && !i.revoked_at_ms);
  if (selected) {
    state.instructions.agent = selected.agent_type;
    state.instructions.providerID = selected.provider_config_id;
    return { integration: selected };
  }
  state.instructions.integrationID = "";
  if (!state.instructions.initialized) {
    const first = state.integrations.find((i) => !i.revoked_at_ms);
    if (first) {
      state.instructions.initialized = true;
      state.instructions.agent = first.agent_type;
      state.instructions.providerID = first.provider_config_id;
      state.instructions.integrationID = first.id;
      return { integration: first };
    }
  }
  if (!state.instructions.agent) state.instructions.agent = "cline";
  if (!state.instructions.providerID && state.providers.length) {
    state.instructions.providerID = state.providers[0].id;
  }
  state.instructions.initialized = true;
  return { integration: null };
}

function renderInstructions() {
  const { integration } = instructionSelection();
  const agent = state.instructions.agent;
  const provider = state.providers.find((p) => p.id === state.instructions.providerID);
  const requiredWire = WIRE_BY_AGENT[agent] || "chat";
  const supportedWires = provider ? (WIRES_BY_KIND[provider.kind] || []) : [];
  const compatible = supportedWires.includes(requiredWire);
  const matching = state.integrations.filter((i) => !i.revoked_at_ms &&
    i.agent_type === agent && i.provider_config_id === state.instructions.providerID);
  const integrationOptions = matching.map((i) =>
    `<option value="${esc(i.id)}" ${i.id === state.instructions.integrationID ? "selected" : ""}>${esc(i.label)} · ${esc(i.model || "model not set")}</option>`
  ).join("");
  const providerOptions = state.providers.map((p) =>
    `<option value="${esc(p.id)}" ${p.id === state.instructions.providerID ? "selected" : ""}>${esc(p.name)} (${esc(p.kind)})</option>`
  ).join("");
  const agentOptions = Object.entries(AGENT_LABELS).map(([id, label]) =>
    `<option value="${id}" ${id === agent ? "selected" : ""}>${label}</option>`
  ).join("");
  const gateway = integration?.gateway_base_url || `http://${location.hostname || "127.0.0.1"}:7878/gw/v1`;
  const model = integration?.model || "Choose the exact model ID used by the provider";
  const token = integration ? state.pendingTokens[integration.id] : "";
  const tokenValue = token || "Available only in the response when the integration is created. Rotate the token if it was lost.";
  const copy = (value, label = "Copy") => value
    ? `<button class="ghost" type="button" data-act="copy" data-copy="${esc(value)}">${esc(label)}</button>`
    : "";
  const missingTokenNote = "Create a new integration or rotate this integration's token if it was lost.";
  let steps = "";
  if (agent === "codex") {
    const codexFields = instructionFields([
      ["Config file", "~/.codex/config.toml (Windows: %USERPROFILE%\\.codex\\config.toml)"],
      ["Default provider", codexProviderID(integration)],
      ["Wire API", "responses"],
      ["Base URL", gateway, copy(gateway)],
      ["Model", model, integration ? copy(model) : ""],
      ["Token environment variable", codexEnvName(integration)],
    ]);
    steps = instructionSection("What Observable writes for you", codexFields) +
      instructionSection("Steps for Codex", `<ol class="instruction-steps">
      <li>Make sure the Codex CLI or Codex IDE extension is installed and signed in.</li>
      <li>Choose a Responses-capable provider above, then create the Codex integration.</li>
      <li>In the integration dialog, click <b>Apply and wait for traffic</b>. Observable backs up the current file, writes the values above to your user-level <code>config.toml</code>, and sets the token environment variable.</li>
      <li>Close every running Codex CLI session. Fully quit all VS Code windows, then reopen VS Code so the extension host reloads the user environment.</li>
      <li>Start a new Codex chat and send a simple prompt. On Windows, if Codex still cannot authenticate, confirm the environment variable from the table above exists under your Windows user environment.</li>
      <li>After the request reaches Observable, open this integration and run <b>Verify</b>. The state changes to <b>Verified (traffic seen)</b> only after that first real inference request.</li>
    </ol>`);
  } else {
    const agentName = AGENT_LABELS[agent] || agent;
    if (agent === "cline") {
      steps = instructionSection("Steps for Cline", `<ol class="instruction-steps">
      <li>Open Cline in VS Code and click the gear icon to open <b>Settings</b>.</li>
      <li>Set <b>API Provider</b> to <b>OpenAI Compatible</b>.</li>
      <li>Set <b>Base URL</b> to the gateway URL below. ${copy(gateway)}</li>
      <li>Set <b>API Key</b> to the gateway token. ${token ? copy(token, "Copy token") : missingTokenNote}</li>
      <li>Set <b>Model</b> to <code>${esc(model)}</code>. ${integration ? copy(model) : ""}</li>
      <li>Leave <b>Use Azure Identity Authentication</b> unchecked. Expand <b>Model Configuration</b> only if this model needs custom context or output limits.</li>
      <li>Save or verify the provider. If the provider list does not refresh, reload the VS Code window.</li>
      <li>Send a test prompt. The integration becomes <b>Verified (traffic seen)</b> only after that request reaches Observable.</li>
    </ol>${vendorLink("Cline: OpenAI Compatible guide", "https://docs.cline.bot/provider-config/openai-compatible")}`);
    } else {
      steps = instructionSection("Steps for Kilo Code", `<ol class="instruction-steps">
      <li>Open Kilo Code in VS Code, click the gear icon, and go to <b>Settings &rarr; Providers</b>.</li>
      <li>Scroll to the bottom of the provider list and click <b>Custom provider</b>.</li>
      <li>Set <b>Provider ID</b> to <code>observable</code>.</li>
      <li>Set <b>Display name</b> to <code>AI-Observable</code>.</li>
      <li>Set <b>Provider API</b> to <b>OpenAI Compatible</b>.</li>
      <li>Set <b>Base URL</b> to the gateway URL below. ${copy(gateway)}</li>
      <li>Set <b>API key</b> to the gateway token. ${token ? copy(token, "Copy token") : missingTokenNote}</li>
      <li>Add the model in <b>Models</b> with ID <code>${esc(model)}</code> and the same display name, or click <b>Fetch models</b> when the endpoint exposes a models list.</li>
      <li>Click <b>Submit</b>. Then select <b>AI-Observable</b> in the model picker and choose <code>${esc(model)}</code>.</li>
      <li>Send a test prompt. The integration becomes <b>Verified (traffic seen)</b> only after that request reaches Observable.</li>
    </ol>${vendorLink("Kilo Code: custom provider guide", "https://kilo.ai/docs/code-with-ai/agents/custom-models")}`);
    }
  }
  return `<section class="card instructions" id="instructions">
    <header><div><h3>Instructions</h3><div class="meta">Choose the agent and provider you are connecting.</div></div>
      <span class="pill ${compatible ? "ok" : "warn"}">${compatible ? "Compatible wire protocol" : "Incompatible provider"}</span></header>
    <div class="instruction-selects">
      <label class="field">Coding agent<select data-instruction="agent">${agentOptions}</select></label>
      <label class="field">Provider<select data-instruction="provider">${providerOptions}</select></label>
      <label class="field">Configured integration<select data-instruction="integration">
        <option value="">No integration yet</option>${integrationOptions}
      </select></label>
    </div>
    ${compatible ? "" : `<div class="result bad">${esc(AGENT_LABELS[agent] || agent)} requires ${requiredWire === "responses" ? "the Responses API" : "Chat Completions"}. Choose a compatible provider before configuring the agent.</div>`}
    <div class="result instruction-token"><b>Gateway token</b><code class="mono">${esc(tokenValue)}</code>${token ? copy(token, "Copy token") : ""}
      <div class="muted">The plaintext token is shown only once. Observable stores its hash, not the token.</div></div>
    ${steps}
    <p class="muted">Observable must remain running whenever the coding agent is used. ${agent === "codex"
      ? vendorLink("Codex config reference", "https://developers.openai.com/codex/config-reference")
      : ""}</p>
  </section>`;
}


const KIND_HINT = { models: "catalog + context", usage: "requests + tokens", costs: "provider-reported spend", balance: "prepaid credit" };

async function renderCollection() {
  const names = Object.fromEntries(state.providers.map((p) => [p.id, p.name]));
  const jobs = state.jobs.map((j) => `<tr><td>${esc(names[j.provider_id] || j.provider_id)}</td>
    <td>${esc(j.kind)}<div class="muted">${esc(KIND_HINT[j.kind] || "")}</div></td>
    <td><span class="pill ${j.last_status === "ok" ? "ok" : j.last_status ? "warn" : ""}">${esc(j.last_status || "new")}</span>
      ${j.last_error ? `<div class="muted" style="max-width:280px">${esc(j.last_error)}</div>` : ""}</td>
    <td class="num">${j.interval_seconds}s</td>
    <td class="muted">${j.next_run_at_ms ? new Date(j.next_run_at_ms).toLocaleString() : "due"}</td>
    <td class="muted mono">${j.cursor_ms ? new Date(j.cursor_ms).toISOString().slice(0, 16) : "—"}</td>
    <td class="row"><button class="ghost" data-act="runjob" data-id="${esc(j.provider_id)}" data-kind="${esc(j.kind)}">Run now</button>
      <button class="ghost" data-act="jobtoggle" data-id="${esc(j.id)}" data-enabled="${j.enabled ? 0 : 1}">${j.enabled ? "Pause" : "Resume"}</button></td></tr>`).join("");
  $("#view").innerHTML = `<div class="cards" style="margin-bottom:16px">
      <div class="card"><div class="stat"><span>lanes</span><b>${state.jobs.length}</b></div></div>
      <div class="card"><div class="stat"><span>last success</span><b>${lastOK()}</b></div></div>
    </div>
    ${jobs ? `<div class="card"><table><thead><tr><th>Provider</th><th>Lane</th><th>Status</th><th class="num">Every</th><th>Next run</th><th>Cursor</th><th></th></tr></thead><tbody>${jobs}</tbody></table></div>`
      : `<div class="empty"><b>No collector lanes</b>Add a provider that exposes usage or cost APIs, then run a sync.</div>`}
    <h2 style="font-size:15px;margin:22px 0 10px">Import ledger</h2><div id="runs" class="card muted">loading&hellip;</div>`;
  try {
    const runs = await api("/api/collector/runs?limit=25");
    $("#runs").innerHTML = runs.length ? `<table><thead><tr><th>When</th><th>Provider</th><th>Lane</th><th>Status</th><th class="num">Rows</th><th class="num">Requests</th><th>Detail</th></tr></thead><tbody>
      ${runs.map((r) => `<tr><td class="muted">${new Date(r.started_at_ms).toLocaleString()}</td><td>${esc(r.provider)}</td><td>${esc(r.kind)}</td>
        <td><span class="pill ${r.status === "ok" ? "ok" : "warn"}">${esc(r.status)}</span></td>
        <td class="num">${num(r.rows_imported)}</td><td class="num">${num(r.requests_imported)}</td>
        <td class="muted">${esc(r.error || "")}</td></tr>`).join("")}
      </tbody></table>` : "<p class='muted'>No imports yet. The collector records every attempt here.</p>";
  } catch (err) { $("#runs").textContent = err.message; }
}

function lastOK() {
  const ok = state.jobs.filter((j) => j.last_status === "ok" && j.last_run_at_ms).sort((a, b) => b.last_run_at_ms - a.last_run_at_ms)[0];
  return ok ? new Date(ok.last_run_at_ms).toLocaleTimeString() : "never";
}


const RANGES = { "24h": 24, "7d": 24 * 7, "30d": 24 * 30 };

async function renderMetrics() {
  const hours = RANGES[state.tab] || 24;
  const to = Date.now();
  const from = to - hours * 3600_000;
  const q = `from=${from}&to=${to}`;
  $("#view").innerHTML = `<div class="tabs">${Object.keys(RANGES).map((k) => `<button data-act="range" data-range="${k}" class="${state.tab === k ? "on" : ""}">${k}</button>`).join("")}</div><div id="metrics" class="muted">loading&hellip;</div>`;
  try {
    const [s, b, pricing] = await Promise.all([
      api(`/api/usage/summary?${q}`), api(`/api/usage/buckets?${q}&granularity=day`), api("/api/pricing/status"),
    ]);
    const fill = Object.entries(s.fill_rate || {}).sort((a, c) => c[1] - a[1]);
    $("#metrics").innerHTML = `<div class="cards" style="margin-bottom:16px">
      <div class="card"><div class="stat"><span>calls measured</span><b>${num(s.calls)}</b><div class="muted">${num(s.failures)} failures &middot; ${num(s.synthetic)} gap-fill rows</div></div></div>
      <div class="card"><div class="stat"><span>tokens</span><b>${num(s.input_tokens)} in / ${num(s.output_tokens)} out</b><div class="muted">${num(s.cache_read_tokens)} cache read &middot; ${num(s.reasoning_tokens)} reasoning</div></div></div>
      <div class="card"><div class="stat"><span>cost (computed)</span><b>${usd(s.cost_usd)}</b><div class="muted">${num(s.pricing_missing)} calls without a price</div></div></div>
      <div class="card"><div class="stat"><span>cost (provider-reported)</span><b>${usd(s.cost_reported_usd)}</b><div class="muted">lane B, never double counted</div></div></div>
    </div>
    <div class="card" style="margin-bottom:16px"><h3 style="margin-top:0">Column fill rate</h3>
      ${fill.map(([k, v]) => `<div style="margin:8px 0"><div class="row" style="justify-content:space-between"><span>${esc(k)}</span><span class="muted">${(v * 100).toFixed(0)}%</span></div>
        <div class="bar"><i style="width:${Math.round(v * 100)}%"></i></div></div>`).join("") || "<p class='muted'>No calls in this range.</p>"}</div>
    <div class="card"><h3 style="margin-top:0">Daily buckets</h3>
      ${b.length ? `<table><thead><tr><th>Day</th><th>Provider</th><th>Model</th><th class="num">Calls</th><th class="num">In</th><th class="num">Out</th><th class="num">Cost</th></tr></thead><tbody>
        ${b.slice(-40).map((x) => `<tr><td>${new Date(x.start_ms).toISOString().slice(0, 10)}</td><td>${esc(x.provider)}</td><td>${esc(x.model)}</td>
          <td class="num">${num(x.calls)}</td><td class="num">${num(x.input)}</td><td class="num">${num(x.output)}</td><td class="num">${usd(x.cost_usd)}</td></tr>`).join("")}
        </tbody></table>` : "<p class='muted'>No rows yet. Send a request through the proxy.</p>"}</div>
    <p class="muted" style="margin-top:14px">Prices: ${pricing.models} models from ${esc(pricing.source)}${pricing.last_error ? " &middot; " + esc(pricing.last_error) : ""}</p>`;
  } catch (err) { $("#metrics").textContent = err.message; }
}


async function renderSettings() {
  $("#view").innerHTML = `<div id="settings" class="muted">loading&hellip;</div>`;
  try {
    const [cfg, pricing, logs] = await Promise.all([api("/api/config"), api("/api/pricing/status"), api("/api/logs?limit=60")]);
    $("#settings").innerHTML = `<div class="cards" style="margin-bottom:16px">
      <div class="card"><h3 style="margin-top:0">Capture &amp; retention</h3>
        <label class="field">Bodies<select id="cfgBodies">
          ${["none", "truncated", "full"].map((v) => `<option ${cfg.capture_bodies === v ? "selected" : ""}>${v}</option>`).join("")}
        </select></label>
        <label class="field">Retention days<input id="cfgRetention" type="number" min="0" max="3650" value="${cfg.retention_days}"></label>
        <button class="primary" data-act="saveconfig">Save</button>
        <p class="muted">Retention is enforced by a daily sweep; a snapshot is taken after each prune.</p></div>
      <div class="card"><h3 style="margin-top:0">Prices</h3>
        <p class="muted">${pricing.models} models &middot; ${esc(pricing.source)}</p>
        <p class="muted mono" style="font-size:11px">${esc(pricing.file)}</p>
        <button class="ghost" data-act="refreshprices">Reload price file</button></div>
      <div class="card"><h3 style="margin-top:0">Snapshot</h3>
        <p class="muted">Grafana reads a published copy, never the live WAL database.</p>
        <p class="muted">Last: ${cfg.last_snapshot_at ? new Date(cfg.last_snapshot_at).toLocaleString() : "pending"}</p>
        <button class="ghost" data-act="snapshot">Refresh now</button></div>
      <div class="card"><h3 style="margin-top:0">Endpoints</h3>
        <div class="key"><span>proxy</span><code>${esc(cfg.proxy_addr)}</code></div>
        <div class="key"><span>admin</span><code>${esc(cfg.admin_addr)}</code></div>
        <div class="key"><span>grafana</span><code>${esc(cfg.grafana_url)}</code></div>
        <div class="key"><span>vault</span><code>${esc(cfg.vault_mode)}</code></div></div>
    </div>
    <div class="card"><h3 style="margin-top:0">Recent activity</h3>
      ${logs.length ? `<table><tbody>${logs.map((l) => `<tr><td class="muted mono">${new Date(l.ts_ms).toLocaleTimeString()}</td>
        <td><span class="pill ${l.level === "error" ? "bad" : ""}">${esc(l.level)}</span></td>
        <td>${esc(l.message)}<div class="muted mono">${esc((l.fields || []).join(" "))}</div></td></tr>`).join("")}</tbody></table>`
        : "<p class='muted'>Nothing logged yet. Secrets are scrubbed before they reach this buffer.</p>"}</div>`;
  } catch (err) { $("#settings").textContent = err.message; }
}

document.addEventListener("click", async (ev) => {
  const el = ev.target.closest("[data-act]");
  if (!el) return;
  const act = el.dataset.act;
  try {
    if (act === "test") {
      el.disabled = true;
      const r = await api(el.dataset.url, { method: "POST", body: {} });
      toast(r.ok ? `Connected in ${r.latency_ms}ms` : `Failed: ${r.status || r.detail || "no detail"}`, r.ok ? "ok" : "bad");
    } else if (act === "toggle") {
      await api(`/api/providers/${encodeURIComponent(el.dataset.id)}`, { method: "PATCH", body: { enabled: el.dataset.enabled === "1" } });
      await loadAll();
    } else if (act === "delete") {
      if (!confirm(`Remove ${el.dataset.name}? Its stored keys and lanes go with it.`)) return;
      await api(`/api/providers/${encodeURIComponent(el.dataset.id)}`, { method: "DELETE" });
      toast("Provider removed", "ok");
      await loadAll();
    } else if (act === "key") {
      openKeyDialog(el.dataset.id, el.dataset.name);
    } else if (act === "models") {
      await api("/api/collector/run", { method: "POST", body: { provider_id: el.dataset.id, kind: "models" } });
      toast("Catalog sync queued", "ok");
      setTimeout(loadAll, 2500);
    } else if (act === "validate") {
      const r = await api(`/api/credentials/${encodeURIComponent(el.dataset.id)}/validate`, { method: "POST", body: {} });
      toast(`Key status: ${r.status}`, r.status === "ok" ? "ok" : "bad");
      await loadAll();
    } else if (act === "revoke") {
      if (!confirm("Revoke this key? The row stays for audit.")) return;
      await api(`/api/credentials/${encodeURIComponent(el.dataset.id)}/revoke`, { method: "POST", body: {} });
      await loadAll();
    } else if (act === "runjob") {
      await api("/api/collector/run", { method: "POST", body: { provider_id: el.dataset.id, kind: el.dataset.kind } });
      toast("Run queued", "ok");
      setTimeout(loadAll, 2500);
    } else if (act === "jobtoggle") {
      await api(`/api/collector/jobs/${encodeURIComponent(el.dataset.id)}`, { method: "PATCH", body: { enabled: el.dataset.enabled === "1" } });
      await loadAll();
    } else if (act === "agentnew") {
      await openAgentWizard();
    } else if (act === "toggledisconnected") {
      state.hideDisconnected = !state.hideDisconnected;
      localStorage.setItem("hideDisconnected", state.hideDisconnected ? "1" : "0");
      renderIntegrations();
    } else if (act === "agentpick") {
      agentWizard.agent = el.dataset.agent;
      agentWizard.providerID = "";
      await loadAgentProviders();
      drawAgentWizard();
    } else if (act === "agentapply") {
      await applyAgentIntegration();
    } else if (act === "copy") {
      await navigator.clipboard?.writeText(el.dataset.copy || "");
      toast("Copied", "ok");
    } else if (act === "verifyagent") {
      const r = await api(`/api/integrations/${encodeURIComponent(el.dataset.id)}/verify`, { method: "POST", body: {} });
      const failed = (r.checks || []).filter((c) => c.status === "fail");
      toast(failed.length ? `Needs attention: ${failed.map((c) => c.name).join(", ")}` : r.state,
        failed.length ? "bad" : r.state === "verified" ? "ok" : "");
      await loadAll();
    } else if (act === "rotateagent") {
      if (!confirm("Rotate this gateway token? The old token stops working immediately.")) return;
      const r = await api(`/api/integrations/${encodeURIComponent(el.dataset.id)}/rotate-token`, { method: "POST", body: {} });
      state.pendingTokens[el.dataset.id] = r.token;
      state.instructions.integrationID = el.dataset.id;
      state.instructions.initialized = true;
      showOneTimeToken(r.token, el.dataset.id);
      await loadAll();
    } else if (act === "rollbackagent") {
      const r = await api(`/api/integrations/${encodeURIComponent(el.dataset.id)}/rollback`, { method: "POST", body: {} });
      toast(r.conflicts?.length ? "Rollback conflict; user edits were preserved" : "Rolled back",
        r.conflicts?.length ? "bad" : "ok");
      await loadAll();
    } else if (act === "removeagent") {
      if (!confirm("Disconnect this agent and revoke its gateway token?")) return;
      const rollback = confirm("Also roll back its config/backup now? Choose Cancel to leave the agent config in place.");
      await api(`/api/integrations/${encodeURIComponent(el.dataset.id)}`, { method: "DELETE", body: { rollback } });
      delete state.pendingTokens[el.dataset.id];
      if (state.instructions.integrationID === el.dataset.id) state.instructions.integrationID = "";
      toast("Integration disconnected", "ok");
      await loadAll();
    } else if (act === "range") {
      state.tab = el.dataset.range;
      render();
    } else if (act === "refreshprices") {
      await api("/api/prices/refresh", { method: "POST", body: {} });
      toast("Price file reloaded", "ok");
      render();
    } else if (act === "snapshot") {
      const r = await api("/api/snapshot", { method: "POST", body: {} });
      toast("Snapshot at " + new Date(r.snapshot_at).toLocaleTimeString(), "ok");
    } else if (act === "saveconfig") {
      await api("/api/config", { method: "PATCH", body: {
        capture_bodies: $("#cfgBodies").value,
        retention_days: Number($("#cfgRetention").value),
      } });
      toast("Saved", "ok");
      render();
    }
  } catch (err) {
    toast(err.message, "bad");
  } finally {
    if (el.tagName === "BUTTON") el.disabled = false;
  }
});


const wizard = { draft: { kind: "openai", name: "openai", base_url: "", auth_style: "bearer", auth_mode: "managed", label: "default", secret: "" } };

function presetList() { return state.onboarding.presets || []; }

// applyPreset copies a preset's defaults into the draft without erasing the
// project name, label or key the user has already typed.
function applyPreset(p) {
  Object.assign(wizard.draft, { kind: p.kind, name: p.kind, base_url: p.base_url, auth_style: p.auth_style, auth_mode: "managed" });
}

function openWizard() {
  const p = presetList()[0];
  if (p) applyPreset(p);
  wizard.draft.label = "default";
  wizard.draft.secret = "";
  drawWizard();
  $("#wizard").showModal();
}

function drawWizard() {
  const d = wizard.draft;
  const preset = presetList().find((x) => x.kind === d.kind);
  const styles = (preset && preset.auth_styles && preset.auth_styles.length) ? preset.auth_styles : ["bearer"];
  const needsKey = d.auth_style !== "none";
  $("#wizBody").innerHTML = `
    <p class="muted">Pick the family you are connecting. The preset fills the base URL and auth style; you can change both below.</p>
    <h4>Family</h4>
    <div class="presets">${presetList().map((p) => `<button type="button" class="${p.kind === d.kind ? "on" : ""}" data-act="preset" data-kind="${esc(p.kind)}">
      <b>${esc(p.label)}</b><small>${esc(p.base_url || "custom endpoint")}</small></button>`).join("")}</div>
    <div class="row2">
      <label class="field">Project name<input id="wizName" value="${esc(d.name)}" placeholder="openai" autocomplete="off" spellcheck="false"></label>
      <label class="field">Label<input id="wizLabel" value="${esc(d.label)}" placeholder="default" autocomplete="off"></label>
    </div>
    <label class="field">Base URL<input id="wizBase" value="${esc(d.base_url)}" placeholder="https://api.openai.com"></label>
    <div class="row2">
      <label class="field">Auth style<select id="wizStyle">${styles.map((v) => `<option ${d.auth_style === v ? "selected" : ""}>${v}</option>`).join("")}</select></label>
      <label class="field">API key${needsKey ? "" : ` <span class="muted">(optional)</span>`}<input id="wizSecret" type="password" autocomplete="off" spellcheck="false" placeholder="${needsKey ? "paste the key" : "none needed"}"></label>
    </div>
    <div id="wizResult" class="result" hidden></div>`;
}


$("#wizSave").addEventListener("click", async () => {
  const d = wizard.draft;
  const result = $("#wizResult");
  const btn = $("#wizSave");
  try {
    d.name = $("#wizName").value.trim();
    d.label = $("#wizLabel").value.trim() || "default";
    d.base_url = $("#wizBase").value.trim();
    d.auth_style = $("#wizStyle").value;
    d.secret = $("#wizSecret").value.trim();
    if (!/^[a-z0-9][a-z0-9._-]{1,31}$/.test(d.name)) throw new Error("Project name must be 2-32 chars: a-z, 0-9, dot, dash, underscore.");
    if (!d.base_url) throw new Error("Base URL is required.");
    if (d.auth_style !== "none" && !d.secret) throw new Error("Paste an API key.");
    btn.disabled = true;
    result.hidden = false; result.className = "result"; result.textContent = "Saving…";
    const p = await api("/api/providers", { method: "POST", body: {
      name: d.name, kind: d.kind, base_url: d.base_url, auth_style: d.auth_style,
      auth_mode: "managed", label: d.label, secret: d.secret,
    } });
    result.className = "result ok";
    result.innerHTML = `<b>Saved.</b> Proxy path <code class="mono">/${esc(p.name)}/v1</code><br>
      <button class="ghost" type="button" data-act="test" data-url="/api/providers/${encodeURIComponent(p.id)}/test">Test connection</button>`;
    localStorage.setItem("wizardSeen", "1");
    toast("Provider saved", "ok");
    await loadAll();
  } catch (err) {
    result.hidden = false; result.className = "result bad"; result.textContent = err.message;
    toast(err.message, "bad");
  } finally {
    btn.disabled = false;
  }
});

const agentWizard = {
  agent: "cline", providerID: "", model: "", capture: "none", label: "",
  providers: [], creating: false, result: null,
};

async function openAgentWizard() {
  Object.assign(agentWizard, {
    agent: "cline", providerID: "", model: "", capture: "none", label: "",
    providers: [], creating: false, result: null,
  });
  $("#agentTitle").textContent = "Connect a coding agent";
  $("#agentCreate").hidden = false;
  $("#agentCreate").disabled = false;
  $("#agentBody").innerHTML = `<p class="muted">Choose the installed agent, then select an API Manager provider that speaks its required wire protocol.</p><div class="muted">Loading compatibility&hellip;</div>`;
  $("#agentDialog").showModal();
  await loadAgentProviders();
  drawAgentWizard();
}

async function loadAgentProviders() {
  try {
    agentWizard.providers = await api(`/api/agents/${encodeURIComponent(agentWizard.agent)}/compatible-providers`);
    const first = agentWizard.providers.find((p) => p.compatible);
    if (!agentWizard.providerID && first) agentWizard.providerID = first.id;
  } catch (err) {
    agentWizard.providers = [];
    toast(err.message, "bad");
  }
}

function drawAgentWizard() {
  const selected = agentWizard.providers.find((p) => p.id === agentWizard.providerID);
  const models = (selected?.models || []).map((m) => `<option value="${esc(m)}"></option>`).join("");
  const agentCards = state.agents.map((a) => `<button type="button" class="${a.id === agentWizard.agent ? "on" : ""}"
      data-act="agentpick" data-agent="${esc(a.id)}">
      <b>${esc(a.display_name)}</b><small>${esc(a.setup_mode)} &middot; ${esc(a.required_wire)} &middot; ${a.detection?.installed ? "detected" : "not detected"}</small>
    </button>`).join("");
  const providers = agentWizard.providers.map((p) => `<option value="${esc(p.id)}"
      ${p.id === agentWizard.providerID ? "selected" : ""} ${p.compatible ? "" : "disabled"}>
      ${esc(p.name)}${p.compatible ? "" : " - not compatible"}</option>`).join("");
  const reasons = agentWizard.providers.filter((p) => !p.compatible && p.reason)
    .map((p) => `<div class="muted">${esc(p.name)}: ${esc(p.reason)}</div>`).join("");
  $("#agentBody").innerHTML = `
    <h4>Agent</h4><div class="presets">${agentCards}</div>
    <label class="field">Provider<select id="agentProvider">${providers || `<option>No compatible providers</option>`}</select></label>
    ${reasons}
    <div class="row2">
      <label class="field">Integration label<input id="agentLabel" value="${esc(agentWizard.label)}" placeholder="codex"></label>
      <label class="field">Model ID<input id="agentModel" list="agentModels" value="${esc(agentWizard.model)}" placeholder="exact provider model"><datalist id="agentModels">${models}</datalist></label>
    </div>
    <label class="field">Captured content<select id="agentCapture">
      <option value="none" ${agentWizard.capture === "none" ? "selected" : ""}>metadata only (recommended)</option>
      <option value="truncated" ${agentWizard.capture === "truncated" ? "selected" : ""}>truncated request/response preview</option>
    </select></label>
    <p class="muted">The provider key stays encrypted in Observable. The agent receives a separate gateway token. Observable must be running whenever the agent is used.</p>
    <div id="agentResult" class="result" hidden></div>`;
}

document.addEventListener("change", (event) => {
  if (event.target.id === "agentProvider") {
    agentWizard.providerID = event.target.value;
    const p = agentWizard.providers.find((x) => x.id === agentWizard.providerID);
    agentWizard.model = p?.models?.[0] || agentWizard.model;
    drawAgentWizard();
    return;
  }
  const kind = event.target.dataset.instruction;
  if (!kind) return;
  if (kind === "agent") {
    state.instructions.agent = event.target.value;
    state.instructions.integrationID = "";
  } else if (kind === "provider") {
    state.instructions.providerID = event.target.value;
    state.instructions.integrationID = "";
  } else if (kind === "integration") {
    state.instructions.integrationID = event.target.value;
    const integration = state.integrations.find((i) => i.id === event.target.value);
    if (integration) {
      state.instructions.agent = integration.agent_type;
      state.instructions.providerID = integration.provider_config_id;
    }
  }
  renderIntegrations();
});

$("#agentCreate").addEventListener("click", async () => {
  if (agentWizard.creating) return;
  const provider = agentWizard.providers.find((p) => p.id === agentWizard.providerID);
  if (!provider || !provider.compatible) {
    toast("Choose a compatible provider", "bad");
    return;
  }
  agentWizard.label = $("#agentLabel").value.trim() || agentWizard.agent;
  agentWizard.model = $("#agentModel").value.trim();
  agentWizard.capture = $("#agentCapture").value;
  const btn = $("#agentCreate");
  try {
    agentWizard.creating = true;
    btn.disabled = true;
    const result = await api("/api/integrations", { method: "POST", body: {
      agent_type: agentWizard.agent,
      label: agentWizard.label,
      provider_id: agentWizard.providerID,
      model: agentWizard.model,
      capture_mode: agentWizard.capture,
    } });
    agentWizard.result = result;
    state.pendingTokens[result.integration.id] = result.token;
    state.instructions = {
      agent: result.integration.agent_type,
      providerID: result.integration.provider_config_id,
      integrationID: result.integration.id,
      initialized: true,
    };
    btn.hidden = true;
    drawAgentResult();
    await loadAll();
  } catch (err) {
    const box = $("#agentResult");
    if (box) {
      box.hidden = false;
      box.className = "result bad";
      const recovery = err.status === 403
        ? "The manager was restarted since this page loaded. Refresh the entire page with Ctrl+F5, then try again."
        : err.message;
      box.innerHTML = `<b>Integration was not created.</b><br>${esc(recovery)}`;
    }
    toast(err.message, "bad");
  } finally {
    agentWizard.creating = false;
    btn.disabled = false;
  }
});

function drawAgentResult() {
  const { token, integration } = agentWizard.result;
  const box = $("#agentResult");
  box.hidden = false;
  box.className = "result";
  box.innerHTML = `<h4>Gateway token - shown once</h4>
    <div class="key"><code class="mono">${esc(token)}</code>
      <button class="ghost" data-act="copy" data-copy="${esc(token)}">Copy token</button></div>
    <p class="muted">The token is also available in the Integrations page Instructions section for this page session. Lost tokens must be rotated.</p>
    <p><b>Setup steps are now on the Integrations page below.</b> Close this dialog when ready and follow the Instructions section for ${esc(integration.agent_type)}.</p>
    <button class="primary" data-act="agentapply">Apply and wait for traffic</button>
    <span class="pill warn">Not verified yet</span>`;
}

async function applyAgentIntegration() {
  if (!agentWizard.result) return;
  try {
    const { integration, plan } = agentWizard.result;
    const r = await api(`/api/integrations/${encodeURIComponent(integration.id)}/apply`, {
      method: "POST", body: { plan_id: plan.plan_id, plan_hash: plan.plan_hash },
    });
    toast("Applied. Send a real agent request to verify.", "ok");
    $("#agentResult").insertAdjacentHTML("beforeend",
      `<div class="result ok"><b>Applied.</b> Restart the agent as instructed, then send one inference request.
       Observable will show <b>Verified (traffic seen)</b> only after that request.</div>`);
    await loadAll();
  } catch (err) {
    toast(err.message, "bad");
  }
}

function showOneTimeToken(token) {
  agentWizard.result = null;
  $("#agentTitle").textContent = "New gateway token";
  $("#agentCreate").hidden = true;
  $("#agentBody").innerHTML = `<p>This token is shown once. Update the agent configuration or environment now.</p>
    <div class="key"><code class="mono">${esc(token)}</code>
    <button class="ghost" data-act="copy" data-copy="${esc(token)}">Copy token</button></div>`;
  $("#agentDialog").showModal();
}

function askDialog(title, html, okLabel = "Confirm") {
  return new Promise((resolve) => {
    $("#dlgTitle").textContent = title;
    $("#dlgBody").innerHTML = html;
    $("#dlgOk").textContent = okLabel;
    const dlg = $("#dialog");
    const done = () => { dlg.removeEventListener("close", done); resolve(dlg.returnValue === "ok"); };
    dlg.addEventListener("close", done);
    dlg.showModal();
  });
}

async function openKeyDialog(providerID, name) {
  const ok = await askDialog(`Add a key to ${name}`,
    `<label class="field">Label<input id="dlgLabel" value="default"></label>
     <label class="field">API key<input id="dlgSecret" type="password" autocomplete="off"></label>`, "Save key");
  if (!ok) return;
  try {
    await api(`/api/providers/${encodeURIComponent(providerID)}/credentials`, { method: "POST", body: {
      label: $("#dlgLabel")?.value || "default", secret: $("#dlgSecret")?.value || "",
    } });
    toast("Key stored encrypted", "ok");
    await loadAll();
  } catch (err) { toast(err.message, "bad"); }
}

document.addEventListener("click", (ev) => {
  const b = ev.target.closest('[data-act="preset"]');
  if (!b) return;
  const preset = presetList().find((x) => x.kind === b.dataset.kind);
  if (!preset) return;
  applyPreset(preset);
  drawWizard();
});

window.openWizard = openWizard;
go("providers");
loadAll();
setInterval(() => { if (state.view === "collection" || state.view === "integrations") loadAll(); }, 15000);

$("#add").addEventListener("click", () => {
  if (state.view === "integrations") openAgentWizard();
  else openWizard();
});
