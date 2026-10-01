/* cliproxy-costs dashboard. Vanilla ES2020, no build step; depends only on the
 * vendored uPlot global. All server strings go through textContent. The token
 * lives in this closure only (never storage, cookies or the URL); only the
 * chosen display currency is kept in sessionStorage. */
(function () {
  "use strict";

  const REFRESH_MS = 60000;
  const RECENT_PAGE = 50;
  const TOP_N = 10;
  const FAIL_WARN = 0.01;
  const FAIL_BAD = 0.05;
  const DAY_MS = 86400000;
  const MONTH_DAYS = 30.4375;
  const MAX_SERIES = 8;
  const ADMIN_BASE = "/v0/management/cliproxy-costs/v1/";
  const READ_BASE = "api/v1/";
  const TZ = (function () {
    try {
      return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
    } catch (_) {
      return "UTC";
    }
  })();

  const NOTICE_TEXT = {
    client_fingerprints_disabled:
      "Client fingerprints are disabled (no fingerprint secret configured), so per-client grouping is unavailable.",
    client_fingerprints_changed:
      "The client fingerprint secret changed. Client ids recorded before and after the change do not match.",
  };
  const BUCKET_LABEL = { hour: "Hour", day: "Day", week: "Week", month: "Month" };
  const HOUR_MS = 3600000;

  const $ = (id) => document.getElementById(id);

  // ---------------------------------------------------------------- state

  const auth = { mode: "admin", token: null };
  const state = {
    gen: 0,
    loading: false,
    lastRefresh: 0,
    range: 30,
    stack: "model",
    bucket: "hour",
    since: null,
    until: null,
    timeSummary: null,
    modelSummary: null,
    credSummary: null,
    clientSummary: null,
    periodSummary: null,
    top: null,
    recentFailed: false,
    quota: null,
    labels: new Map(),
    subscriptions: [],
    sort: { key: "cost", dir: "desc" },
    modelRows: [],
    recent: [],
    recentNext: null,
    recentPages: 0,
    recentBusy: false,
    detailTrace: null,
    lookupTraces: null,
    abort: null,
  };
  const charts = { spend: null, tokens: null };

  // ---------------------------------------------------------------- DOM helpers

  function h(tag, props, ...children) {
    const el = document.createElement(tag);
    if (props) {
      for (const k of Object.keys(props)) {
        const v = props[k];
        if (v == null || v === false) continue;
        if (k === "class") el.className = v;
        else if (k === "text") el.textContent = v;
        else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
        else el.setAttribute(k, v === true ? "" : String(v));
      }
    }
    for (const c of children) {
      if (c == null || c === false) continue;
      el.append(c instanceof Node ? c : document.createTextNode(String(c)));
    }
    return el;
  }

  function clear(el) {
    el.replaceChildren();
    return el;
  }

  function na(title) {
    return h("span", { class: "na", title: title || null, text: "—" });
  }

  // ---------------------------------------------------------------- formatting

  const nf0 = new Intl.NumberFormat(undefined, { maximumFractionDigits: 0 });
  const nf1 = new Intl.NumberFormat(undefined, { maximumFractionDigits: 1 });
  const nfCompact = new Intl.NumberFormat(undefined, { notation: "compact", maximumFractionDigits: 1 });

  // ---- currency: USD is the stored unit; conversion is display-only at the
  // latest rate from /fx. fx.currency is the effective display currency.
  const CURRENCY_KEY = "cliproxy-costs.currency";
  const fx = { info: null, currency: "USD", rate: 1, wanted: null, missing: null };
  const moneyFormats = new Map();

  function moneyFormat(cur, maxDigits, compact) {
    const key = cur + "|" + maxDigits + "|" + (compact ? 1 : 0);
    let f = moneyFormats.get(key);
    if (!f) {
      const base = new Intl.NumberFormat(undefined, { style: "currency", currency: cur }).resolvedOptions();
      const opts = { style: "currency", currency: cur };
      if (compact) {
        opts.notation = "compact";
        opts.maximumFractionDigits = 1;
      } else {
        opts.minimumFractionDigits = Math.min(base.minimumFractionDigits, maxDigits);
        opts.maximumFractionDigits = Math.max(opts.minimumFractionDigits, maxDigits);
      }
      f = new Intl.NumberFormat(undefined, opts);
      moneyFormats.set(key, f);
    }
    return f;
  }

  // Formats an amount already expressed in cur.
  function fmtAmount(v, cur) {
    if (v == null || !isFinite(v)) return "—";
    const a = Math.abs(v);
    const d0 = moneyFormat(cur, 2).resolvedOptions().minimumFractionDigits;
    let digits = Math.max(2, d0);
    if (a > 0 && a < 0.01) digits = 5;
    else if (a > 0 && a < 1) digits = 4;
    return moneyFormat(cur, digits).format(v);
  }

  function fmtUSD(usd) {
    return fmtAmount(usd, "USD");
  }

  // Formats a USD amount in the display currency.
  function fmtMoney(usd) {
    if (usd == null || !isFinite(usd)) return "—";
    return fmtAmount(usd * fx.rate, fx.currency);
  }

  // Display-currency text node with the original USD on hover.
  function money(usd) {
    const text = fmtMoney(usd);
    if (fx.currency === "USD" || usd == null || !isFinite(usd)) return text;
    return h("span", { class: "money", title: fmtUSD(usd) + " (USD)" }, text);
  }

  // Chart legend value: v is already converted; USD appended when converted.
  function fmtChartMoney(v) {
    if (v == null || !isFinite(v)) return "—";
    const t = fmtAmount(v, fx.currency);
    return fx.currency === "USD" ? t : t + " (" + fmtUSD(v / fx.rate) + ")";
  }

  // Axis tick label: v is already in the display currency.
  function fmtMoneyAxis(v) {
    if (v == null) return "";
    const a = Math.abs(v);
    if (a >= 1000) return moneyFormat(fx.currency, 1, true).format(v);
    if (Number.isInteger(v)) return moneyFormat(fx.currency, 0).format(v);
    if (a >= 1) return moneyFormat(fx.currency, Number.isInteger(v * 10) ? 1 : 2).format(v);
    return moneyFormat(fx.currency, a >= 0.01 ? 2 : 3).format(v);
  }

  function fmtInt(v) {
    return v == null ? "—" : nf0.format(v);
  }

  function fmtTokens(v) {
    if (v == null) return "—";
    return Math.abs(v) >= 10000 ? nfCompact.format(v) : nf0.format(v);
  }

  function fmtMs(v) {
    if (v == null || !isFinite(v)) return null;
    if (v < 10) return v.toFixed(2) + " ms";
    if (v < 1000) return nf0.format(v) + " ms";
    return (v / 1000).toFixed(v < 10000 ? 2 : 1) + " s";
  }

  function fmtDuration(ms) {
    if (ms <= 0) return "now";
    const s = Math.floor(ms / 1000);
    const d = Math.floor(s / 86400);
    const hh = Math.floor((s % 86400) / 3600);
    const mm = Math.floor((s % 3600) / 60);
    const ss = s % 60;
    if (d > 0) return d + "d " + hh + "h " + mm + "m";
    if (hh > 0) return hh + "h " + String(mm).padStart(2, "0") + "m " + String(ss).padStart(2, "0") + "s";
    if (mm > 0) return mm + "m " + String(ss).padStart(2, "0") + "s";
    return ss + "s";
  }

  function fmtAgo(iso) {
    const t = Date.parse(iso);
    if (!isFinite(t)) return "—";
    const ms = Date.now() - t;
    if (ms < 45000) return "just now";
    const m = Math.round(ms / 60000);
    if (m < 60) return m + " min ago";
    const hr = ms / 3600000;
    if (hr < 48) return nf1.format(hr) + " h ago";
    return nf0.format(ms / DAY_MS) + " d ago";
  }

  function fmtTime(iso) {
    const t = Date.parse(iso);
    if (!isFinite(t)) return "—";
    return new Date(t).toLocaleString(undefined, {
      month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", second: "2-digit",
    });
  }

  // Time of day for today's entries, date + time otherwise.
  function fmtTimeShort(iso) {
    const t = Date.parse(iso);
    if (!isFinite(t)) return "—";
    const d = new Date(t);
    const today = startOfDay(new Date()).getTime() === startOfDay(d).getTime();
    return today
      ? d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" })
      : d.toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
  }

  function fmtBucket(sec, bucket) {
    if (sec == null) return "—";
    const d = new Date(sec * 1000);
    if (bucket === "month") return d.toLocaleDateString(undefined, { month: "short", year: "numeric" });
    if (bucket === "hour") {
      const end = new Date(d.getTime() + HOUR_MS);
      const t = { hour: "2-digit", minute: "2-digit" };
      return d.toLocaleDateString(undefined, { month: "short", day: "numeric" }) + " " + d.toLocaleTimeString(undefined, t) + "–" + end.toLocaleTimeString(undefined, t);
    }
    const s = d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
    return bucket === "week" ? "Week of " + s : s;
  }

  // dateTicks: hourly ticks that fall on whole days show the date only.
  function fmtBucketShort(sec, bucket, dateTicks) {
    const d = new Date(sec * 1000);
    if (bucket === "month") return d.toLocaleDateString(undefined, { month: "short", year: "2-digit" });
    if (bucket === "hour" && !dateTicks) {
      const t = d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
      return d.getHours() === 0 ? d.toLocaleDateString(undefined, { month: "short", day: "numeric" }) : t;
    }
    return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
  }

  function labelFor(kind, key, label) {
    if (label) return label;
    if (key == null || key === "") return kind === "client" ? "(no client)" : "(none)";
    if ((kind === "credential" || kind === "client") && state.labels.has(key)) return state.labels.get(key);
    return key;
  }

  function sumTokens(t) {
    if (!t) return 0;
    return (t.input || 0) + (t.cache_read || 0) + (t.cache_write || 0) + (t.output || 0);
  }

  // ---------------------------------------------------------------- time buckets (browser tz)

  function startOfDay(d) {
    return new Date(d.getFullYear(), d.getMonth(), d.getDate());
  }

  function bucketStart(d, bucket) {
    if (bucket === "hour") return new Date(d.getFullYear(), d.getMonth(), d.getDate(), d.getHours());
    if (bucket === "month") return new Date(d.getFullYear(), d.getMonth(), 1);
    const day = startOfDay(d);
    if (bucket === "week") {
      const dow = (day.getDay() + 6) % 7; // Monday = 0
      return new Date(day.getFullYear(), day.getMonth(), day.getDate() - dow);
    }
    return day;
  }

  function nextBucket(d, bucket) {
    if (bucket === "hour") return new Date(d.getTime() + HOUR_MS);
    if (bucket === "month") return new Date(d.getFullYear(), d.getMonth() + 1, 1);
    return new Date(d.getFullYear(), d.getMonth(), d.getDate() + (bucket === "week" ? 7 : 1));
  }

  function bucketKey(d, bucket) {
    const y = d.getFullYear();
    const m = String(d.getMonth() + 1).padStart(2, "0");
    if (bucket === "month") return y + "-" + m;
    const day = y + "-" + m + "-" + String(d.getDate()).padStart(2, "0");
    return bucket === "hour" ? day + "T" + String(d.getHours()).padStart(2, "0") : day;
  }

  // Parses server bucket keys: YYYY-MM, YYYY-MM-DD or YYYY-MM-DDTHH (local time).
  function parseBucketKey(key) {
    const [date, hour] = String(key).split("T");
    const p = date.split("-").map(Number);
    if (p.length < 2 || p.some((n) => !isFinite(n))) return null;
    const hh = hour === undefined ? 0 : Number(hour);
    if (!isFinite(hh)) return null;
    return new Date(p[0], p[1] - 1, p[2] || 1, hh);
  }

  function bucketFactor(bucket) {
    if (bucket === "month") return 1;
    if (bucket === "week") return 7 / MONTH_DAYS;
    if (bucket === "hour") return 1 / (MONTH_DAYS * 24);
    return 1 / MONTH_DAYS;
  }

  function computeRange() {
    const now = new Date();
    const since = new Date(now.getFullYear(), now.getMonth(), now.getDate() - (state.range - 1));
    state.since = since;
    state.until = now;
  }

  // The time charts' range: the selected range, except the Hour bucket, which
  // zooms to the last 24 hours (the current hour plus the 23 before it).
  function chartRange(bucket) {
    if (bucket !== "hour") return { since: state.since, until: state.until };
    const h = bucketStart(state.until, "hour");
    return { since: new Date(h.getTime() - 23 * HOUR_MS), until: state.until };
  }

  function chartQuery() {
    const r = chartRange(state.bucket);
    return { since: r.since.toISOString(), until: r.until.toISOString(), tz: TZ, group: state.bucket, series: state.stack };
  }

  function weekStart(d) {
    return bucketStart(d, "week");
  }

  function monthStart(d) {
    return bucketStart(d, "month");
  }

  // Start of the today/week/month KPI query: the earlier of week and month start.
  function periodStart() {
    const now = state.until || new Date();
    const w = weekStart(now);
    const m = monthStart(now);
    return w < m ? w : m;
  }

  // ---------------------------------------------------------------- API

  class ApiError extends Error {
    constructor(status, code, message) {
      super(message || code || "HTTP " + status);
      this.status = status;
      this.code = code || "";
    }
  }

  async function api(ep, params, signal) {
    if (!auth.token) throw new ApiError(401, "unauthorized", "Not signed in.");
    const qs = new URLSearchParams();
    for (const k of Object.keys(params || {})) {
      if (params[k] != null && params[k] !== "") qs.set(k, params[k]);
    }
    const base = auth.mode === "admin" ? ADMIN_BASE : READ_BASE;
    const url = base + ep + (qs.toString() ? "?" + qs : "");
    let res;
    try {
      res = await fetch(url, {
        headers: { Authorization: "Bearer " + auth.token, Accept: "application/json" },
        cache: "no-store",
        credentials: "same-origin",
        referrerPolicy: "no-referrer",
        signal,
      });
    } catch (e) {
      if (e && e.name === "AbortError") throw e;
      throw new ApiError(0, "network", "Network error: " + (e && e.message ? e.message : "request failed"));
    }
    let body = null;
    const text = await res.text();
    if (text) {
      try {
        body = JSON.parse(text);
      } catch (_) {
        body = null;
      }
    }
    if (!res.ok) {
      const err = body && body.error ? body.error : {};
      throw new ApiError(res.status, err.code, err.message || res.status + " " + res.statusText);
    }
    if (!body || typeof body !== "object") throw new ApiError(res.status, "bad_response", "Unexpected response from " + ep + ".");
    return body;
  }

  function isAuthError(e) {
    return e instanceof ApiError && (e.status === 401 || e.status === 403 || (e.status === 503 && e.code === "read_api_disabled"));
  }

  function authMessage(e) {
    if (e.status === 503) {
      return "Sign in with the management key: the read API is disabled because no read token is configured on the server (environment variable CLIPROXY_COSTS_READ_TOKEN by default, at least 32 bytes). Configure a read token to use the read-token option.";
    }
    if (e.status === 403) {
      // The plugin never answers 403; CLIProxyAPI does, for management
      // routes, after repeated wrong keys from one IP (or with remote
      // management disabled). Retrying during the ban does not help.
      return "CLIProxyAPI refused access (403). After 5 wrong management keys from one IP it blocks management access, including its own control panel, for about 30 minutes. Wait for the block to expire (or restart CLIProxyAPI), then sign in with the correct key — or use the Read token option, which is not affected.";
    }
    return auth.mode === "admin"
      ? "The management key was rejected (401). Check the key and try again. (The read token only works with the Read token option.)"
      : "The read token was rejected (401). Check the token and try again.";
  }

  function handleError(e) {
    if (e && e.name === "AbortError") return true;
    if (isAuthError(e)) {
      promptAuth(authMessage(e));
      return true;
    }
    return false;
  }

  // ---------------------------------------------------------------- auth dialog

  function promptAuth(message) {
    auth.token = null;
    if (state.abort) state.abort.abort();
    state.abort = null;
    state.gen++;
    stopTimers();
    $("controls").hidden = true;
    $("app").hidden = true;
    setLoading(false);
    $("banner").hidden = true;
    $("fx-notice").hidden = true;
    $("fx-footer").hidden = true;
    destroyCharts();
    // Everything rendered from the previous credential's responses (admin-only
    // auth_id in quota tooltips, lookup rows, the attempt dialog) is dropped
    // before anyone else signs in, not just hidden.
    state.lookupTraces = null;
    state.detailTrace = null;
    $("lookup-id").value = "";
    for (const id of ["lookup-result", "quota", "top", "reliability", "value", "notices", "health", "unpriced", "attention", "detail-body"]) clear($(id));
    for (const id of ["recent", "by-credential", "by-client", "models"]) clear($(id).querySelector("tbody"));
    if ($("detail").open) $("detail").close();
    const msg = $("auth-msg");
    msg.hidden = !message;
    msg.textContent = message || "";
    const tokenInput = $("auth-token");
    tokenInput.value = "";
    const dlg = $("auth");
    if (!dlg.open) dlg.showModal();
    tokenInput.focus();
  }

  function syncAuthLabel() {
    const mode = document.querySelector('input[name="mode"]:checked').value;
    $("auth-label").textContent = mode === "admin" ? "Management key" : "Read token";
  }

  // Checks the credential with ONE request before the parallel refresh: CPA
  // bans an IP after 5 failed management-key attempts, so a typo must not
  // cost one failure per dashboard panel.
  async function onAuthSubmit(ev) {
    ev.preventDefault();
    const token = $("auth-token").value.trim();
    if (!token) return;
    const submit = $("auth").querySelector('button[type="submit"]');
    if (submit) submit.disabled = true;
    auth.mode = document.querySelector('input[name="mode"]:checked').value;
    auth.token = token;
    const gen = ++state.gen;
    try {
      fx.info = await api("fx");
    } catch (e) {
      if (gen !== state.gen) return;
      if (!handleError(e)) promptAuth("Could not reach the server: " + (e && e.message ? e.message : "request failed"));
      return;
    } finally {
      if (submit) submit.disabled = false;
    }
    if (gen !== state.gen) return;
    $("auth-token").value = "";
    $("auth-msg").hidden = true;
    $("auth").close();
    $("controls").hidden = false;
    $("app").hidden = false;
    setLoading(true);
    refresh(true);
    startTimers();
  }

  function signOut() {
    state.timeSummary = state.modelSummary = state.credSummary = state.quota = null;
    state.clientSummary = state.periodSummary = state.top = null;
    state.recentFailed = false;
    $("recent-failed").checked = false;
    state.recent = [];
    state.recentNext = null;
    state.recentPages = 0;
    promptAuth("Signed out. The token was cleared from memory.");
  }

  // ---------------------------------------------------------------- refresh / timers

  let refreshTimer = null;
  let tickTimer = null;

  function startTimers() {
    stopTimers();
    refreshTimer = setInterval(() => {
      if (document.visibilityState === "visible" && auth.token) refresh(false);
    }, REFRESH_MS);
    tickTimer = setInterval(() => {
      if (document.visibilityState === "visible") tick();
    }, 1000);
  }

  function stopTimers() {
    clearInterval(refreshTimer);
    clearInterval(tickTimer);
    refreshTimer = tickTimer = null;
  }

  function onVisibility() {
    if (document.visibilityState === "visible" && auth.token && Date.now() - state.lastRefresh >= REFRESH_MS) {
      refresh(false);
    }
  }

  function setBusy(busy) {
    state.loading = busy;
    $("refresh").disabled = busy;
    $("refresh").textContent = busy ? "Refreshing…" : "Refresh";
  }

  function setLoading(on) {
    const app = $("app");
    app.classList.toggle("is-loading", on);
    if (on) app.setAttribute("aria-busy", "true");
    else app.removeAttribute("aria-busy");
  }

  function showBanner(text) {
    const b = $("banner");
    b.hidden = !text;
    b.textContent = text || "";
  }

  async function refresh(full) {
    if (!auth.token) return;
    if (state.abort) state.abort.abort();
    const ctrl = new AbortController();
    state.abort = ctrl;
    const gen = ++state.gen;
    computeRange();
    setBusy(true);
    const range = { since: state.since.toISOString(), until: state.until.toISOString(), tz: TZ };
    const timeQuery = chartQuery();
    const reloadRecent = full || state.recentPages <= 1;
    const failedParam = state.recentFailed ? 1 : null;
    const specs = [
      ["time", "spend over time", () => api("summary", timeQuery, ctrl.signal)],
      ["model", "per-model summary", () => api("summary", { ...range, group: "model" }, ctrl.signal)],
      ["cred", "per-credential summary", () => api("summary", { ...range, group: "credential" }, ctrl.signal)],
      ["client", "per-client summary", () => api("summary", { ...range, group: "client" }, ctrl.signal)],
      [
        "period",
        "today/week/month",
        () => api("summary", { since: periodStart().toISOString(), until: state.until.toISOString(), tz: TZ, group: "day" }, ctrl.signal),
      ],
      ["quota", "quota", () => api("quota", null, ctrl.signal)],
      ["fx", "exchange rates", () => api("fx", null, ctrl.signal)],
      ["recent", "recent requests", () => (reloadRecent ? api("requests", { recent: RECENT_PAGE, failed: failedParam }, ctrl.signal) : Promise.resolve(null))],
      ["top", "most expensive requests", () => api("requests", { top: TOP_N, since: range.since, until: range.until }, ctrl.signal)],
    ];
    const results = await Promise.allSettled(specs.map((s) => s[2]()));
    if (gen !== state.gen) return;
    state.abort = null;
    setBusy(false);
    for (const r of results) {
      if (r.status === "rejected" && handleError(r.reason)) return;
    }
    const errors = [];
    const got = {};
    specs.forEach(([name, what], i) => {
      const r = results[i];
      if (r.status === "fulfilled") got[name] = r.value;
      else errors.push(what + ": " + (r.reason && r.reason.message ? r.reason.message : "failed"));
    });

    if (got.time !== undefined) state.timeSummary = got.time;
    if (got.model !== undefined) state.modelSummary = got.model;
    if (got.cred !== undefined) state.credSummary = got.cred;
    if (got.client !== undefined) state.clientSummary = got.client;
    if (got.period !== undefined) state.periodSummary = got.period;
    if (got.quota !== undefined) state.quota = got.quota;
    if (got.top !== undefined) state.top = got.top;
    if (got.fx !== undefined) fx.info = got.fx;
    rebuildLabels();
    applyCurrency();

    renderSpendAndTokens();
    renderModels();
    renderQuota();
    renderValue();
    renderHealth();
    renderBreakdown("credential");
    renderBreakdown("client");
    renderTop();
    renderReliability();
    // Toggling "Failures only" mid-refresh starts its own reload; a page
    // fetched with the old filter must not overwrite it.
    const recent = failedParam === (state.recentFailed ? 1 : null) ? got.recent : null;
    if (recent) {
      state.recent = Array.isArray(recent.attempts) ? recent.attempts : [];
      state.recentNext = recent.next_before || null;
      state.recentPages = 1;
      renderRecent();
    } else if (reloadRecent && got.recent === undefined) {
      renderRecent();
    }
    renderKpis();
    renderAttention();

    state.lastRefresh = Date.now();
    $("updated").textContent = "Updated " + new Date().toLocaleTimeString();
    showBanner(errors.length ? "Some data failed to load — " + errors.join("; ") : "");
    setLoading(false);
  }

  async function refreshTimeSeries() {
    if (!auth.token) return;
    const gen = state.gen;
    const q = chartQuery();
    try {
      const s = await api("summary", q);
      if (gen !== state.gen) return;
      state.timeSummary = s;
      rebuildLabels();
      renderSpendAndTokens();
    } catch (e) {
      if (!handleError(e)) showBanner("Spend over time failed to load — " + e.message);
    }
  }

  function rebuildLabels() {
    const m = new Map();
    const add = (k, l) => {
      if (k && l && !m.has(k)) m.set(k, l);
    };
    if (state.credSummary && Array.isArray(state.credSummary.groups)) {
      for (const g of state.credSummary.groups) add(g.key, g.label);
    }
    if (state.quota && Array.isArray(state.quota.credentials)) {
      for (const c of state.quota.credentials) add(c.credential, c.label);
    }
    if (state.clientSummary && Array.isArray(state.clientSummary.groups)) {
      for (const g of state.clientSummary.groups) add(g.key, g.label);
    }
    // Series entries carry labels too (e.g. keys outside the top groups).
    if (state.timeSummary && Array.isArray(state.timeSummary.groups) && (state.stack === "client" || state.stack === "credential")) {
      for (const g of state.timeSummary.groups) for (const se of g.series || []) add(se.key, se.label);
    }
    const subs = (state.modelSummary && state.modelSummary.subscriptions) || [];
    for (const s of subs) add(s.credential, s.label);
    state.labels = m;
    state.subscriptions = Array.isArray(subs) ? subs : [];
  }

  // ---------------------------------------------------------------- currency

  function storedCurrency() {
    try {
      return sessionStorage.getItem(CURRENCY_KEY);
    } catch (_) {
      return null;
    }
  }

  // Resolves the effective currency from the user's choice (or the server
  // default) and the available rates, then syncs the selector, notice and footer.
  function applyCurrency() {
    const info = fx.info;
    const list = info && Array.isArray(info.currencies) && info.currencies.length ? info.currencies : ["USD"];
    const rates = (info && info.rates) || { USD: 1 };
    let wanted = fx.wanted || storedCurrency() || (info && info.display_currency) || "USD";
    if (!list.includes(wanted)) wanted = list.includes("USD") ? "USD" : list[0];
    fx.wanted = wanted;
    const rate = Number(rates[wanted]);
    const ok = wanted === "USD" || (isFinite(rate) && rate > 0);
    fx.currency = ok ? wanted : "USD";
    fx.rate = ok ? (wanted === "USD" ? 1 : rate) : 1;
    fx.missing = ok ? null : wanted;

    const sel = $("currency");
    const have = Array.from(sel.options).map((o) => o.value).join(",");
    if (have !== list.join(",")) {
      clear(sel);
      for (const c of list) sel.append(h("option", { value: c, text: c }));
    }
    sel.value = wanted;

    const notice = $("fx-notice");
    notice.hidden = !fx.missing;
    notice.textContent = fx.missing ? "No exchange rate for " + fx.missing + " is available; amounts are shown in USD." : "";
    renderFxFooter();
  }

  function fxStateText(info) {
    if (!info) return "Rates unavailable";
    if (info.source === "off") return "Currency conversion off; amounts in USD";
    let t = info.source === "fixed" ? "Rates: fixed (configured)" : "Rates: ECB " + (info.as_of || "—");
    if (info.status === "stale") t += " (stale)";
    else if (info.status === "error") t += " (last fetch failed" + (info.error ? ": " + info.error : "") + ")";
    return t;
  }

  function renderFxFooter() {
    const foot = $("fx-footer");
    const info = fx.info;
    foot.hidden = !auth.token;
    const parts = [fxStateText(info)];
    if (fx.currency !== "USD") {
      parts.push("1 USD = " + fx.rate + " " + fx.currency + " · amounts stored and computed in USD, converted at the latest rate; hover a value for USD");
    } else {
      parts.push("amounts in USD");
    }
    clear(foot).append(
      h("span", { class: info && (info.status === "stale" || info.status === "error") ? "fx-warn" : null }, parts[0]),
      " · " + parts.slice(1).join(" · ")
    );
  }

  function onCurrencyChange(ev) {
    fx.wanted = ev.target.value;
    try {
      sessionStorage.setItem(CURRENCY_KEY, fx.wanted);
    } catch (_) {
      // storage unavailable: the choice lasts for this page only
    }
    applyCurrency();
    renderSpendAndTokens();
    drawModelTable();
    renderValue();
    renderHealth();
    renderRecent();
    renderQuota();
    renderBreakdown("credential");
    renderBreakdown("client");
    renderTop();
    renderReliability();
    renderKpis();
    renderAttention();
    if (state.lookupTraces) renderLookup(state.lookupTraces);
  }

  // ---------------------------------------------------------------- charts

  function cssVar(name) {
    return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  }

  function palette() {
    const out = [];
    for (let i = 0; i < 9; i++) out.push(cssVar("--c" + i));
    return out;
  }

  function chartHeight() {
    return window.matchMedia("(max-width: 640px)").matches ? 220 : 300;
  }

  function destroyCharts() {
    for (const k of Object.keys(charts)) {
      if (charts[k]) charts[k].destroy();
      charts[k] = null;
    }
  }

  function mountChart(key, container, opts, data) {
    if (charts[key]) charts[key].destroy();
    charts[key] = null;
    clear(container);
    if (typeof uPlot !== "function") {
      container.append(h("p", { class: "empty", text: "Chart library failed to load." }));
      return;
    }
    opts.width = Math.max(200, container.clientWidth);
    opts.height = chartHeight();
    charts[key] = new uPlot(opts, data, container);
  }

  function emptyChart(key, container, text) {
    if (charts[key]) charts[key].destroy();
    charts[key] = null;
    clear(container).append(h("p", { class: "empty", text }));
  }

  // Builds the full bucket axis for the selected range and indexes groups by key.
  function timeAxis(summary, bucket) {
    const xs = [];
    const keys = [];
    const r = chartRange(bucket);
    let d = bucketStart(r.since, bucket);
    const end = r.until;
    let guard = 0;
    while (d <= end && guard++ < 20000) {
      const k = bucketKey(d, bucket);
      // DST fall-back repeats a local hour; the server merges it into one key.
      if (keys[keys.length - 1] !== k) {
        xs.push(d.getTime() / 1000);
        keys.push(k);
      }
      d = nextBucket(d, bucket);
    }
    const byKey = new Map();
    for (const g of summary.groups || []) byKey.set(String(g.key), g);
    // Server buckets outside the locally generated axis (e.g. tz fallback) are appended in order.
    for (const g of summary.groups || []) {
      const k = String(g.key);
      if (keys.indexOf(k) === -1) {
        const pd = parseBucketKey(k);
        if (pd) {
          keys.push(k);
          xs.push(pd.getTime() / 1000);
        }
      }
    }
    const order = xs.map((x, i) => i).sort((a, b) => xs[a] - xs[b]);
    return { xs: order.map((i) => xs[i]), keys: order.map((i) => keys[i]), byKey };
  }

  function xAxisOpts(bucket, xs, colors) {
    // Hourly axes tick on whole hours that divide the day, or on whole days.
    const hourSteps = [1, 2, 3, 4, 6, 12, 24, 48, 72, 168, 336, 720];
    let dateTicks = false;
    return {
      stroke: colors.muted,
      grid: { show: false },
      ticks: { stroke: colors.grid },
      splits: (u) => {
        const maxTicks = Math.max(2, Math.floor(u.width / 72));
        let step = Math.max(1, Math.ceil(xs.length / maxTicks));
        const out = [];
        if (bucket === "hour") {
          step = hourSteps.find((s) => s >= step) || step;
          dateTicks = step >= 24;
          for (const x of xs) {
            const d = new Date(x * 1000);
            const idx = Math.round((d - startOfDay(d)) / HOUR_MS) + (dateTicks ? Math.round(startOfDay(d) / DAY_MS) * 24 : 0);
            if (idx % step === 0) out.push(x);
          }
          return out;
        }
        for (let i = 0; i < xs.length; i += step) out.push(xs[i]);
        return out;
      },
      values: (u, splits) => splits.map((v) => fmtBucketShort(v, bucket, dateTicks)),
    };
  }

  function xScale(xs) {
    let step = DAY_MS / 1000;
    if (xs.length > 1) step = (xs[xs.length - 1] - xs[0]) / (xs.length - 1);
    return {
      time: false,
      range: () => [xs[0] - step / 2, xs[xs.length - 1] + step / 2],
    };
  }

  function themeColors() {
    return {
      text: cssVar("--text"),
      muted: cssVar("--muted"),
      grid: cssVar("--grid"),
      sub: cssVar("--c-sub"),
      ratio: cssVar("--c-ratio"),
      reasoning: cssVar("--c-reasoning"),
    };
  }

  function renderSpendAndTokens() {
    const s = state.timeSummary;
    const bucket = s && s.group ? s.group : state.bucket;
    const spendEl = $("chart-spend");
    const tokEl = $("chart-tokens");
    if (!s || !Array.isArray(s.groups)) {
      emptyChart("spend", spendEl, "No data.");
      emptyChart("tokens", tokEl, "No data.");
      $("spend-sub").textContent = "";
      $("tokens-sub").textContent = "";
      return;
    }
    const axis = timeAxis(s, bucket);
    const colors = themeColors();
    const pal = palette();
    const hasData = s.groups.some((g) => (g.requests || 0) > 0);

    // ---- spend
    const totals = s.totals || {};
    const subsMonthly = state.subscriptions.reduce((a, x) => a + (Number(x.usd_per_month) || 0), 0);
    const sub = [
      "Total " + fmtMoney(totals.cost_usd),
      fmtInt(totals.requests) + " requests",
    ];
    if (totals.unpriced_requests) sub.push(fmtInt(totals.unpriced_requests) + " unpriced (excluded)");
    if (subsMonthly > 0) sub.push("subscriptions " + fmtMoney(subsMonthly) + "/month (prorated per " + bucket + ")");
    if (bucket === "hour") sub.unshift("Last 24 hours");
    $("spend-sub").textContent = sub.join(" · ");
    $("spend-sub").title = fx.currency === "USD" ? "" : "Total " + fmtUSD(totals.cost_usd) + " (USD)";

    if (!hasData) {
      emptyChart("spend", spendEl, "No usage in this range.");
    } else {
      const agg = new Map();
      for (const g of s.groups) {
        for (const se of g.series || []) {
          const k = se.key == null ? "" : String(se.key);
          const cur = agg.get(k) || { key: se.key, label: se.label, cost: 0, requests: 0 };
          cur.cost += se.cost_usd || 0;
          cur.requests += se.requests || 0;
          if (!cur.label && se.label) cur.label = se.label;
          agg.set(k, cur);
        }
      }
      const ranked = Array.from(agg.values()).sort((a, b) => b.cost - a.cost || b.requests - a.requests);
      const top = ranked.slice(0, ranked.length > MAX_SERIES ? MAX_SERIES - 1 : MAX_SERIES);
      const topKeys = new Set(top.map((t) => (t.key == null ? "" : String(t.key))));
      const layers = top.map((t) => ({ name: labelFor(state.stack, t.key, t.label), key: t.key == null ? "" : String(t.key) }));
      const hasOther = ranked.length > top.length;
      if (hasOther) layers.push({ name: "Other (" + (ranked.length - top.length) + ")", key: null });

      // raw[layer][bucket]; null = no priced cost for that cell
      const raw = layers.map(() => axis.keys.map(() => null));
      axis.keys.forEach((k, bi) => {
        const g = axis.byKey.get(k);
        if (!g) return;
        for (const se of g.series || []) {
          const sk = se.key == null ? "" : String(se.key);
          const li = topKeys.has(sk) ? layers.findIndex((l) => l.key === sk) : layers.length - 1;
          if (li < 0) continue;
          if (se.cost_usd != null) raw[li][bi] = (raw[li][bi] || 0) + se.cost_usd * fx.rate;
        }
      });
      // Stack: layer 0 is drawn first and tallest (cumulative of all layers below it).
      const stacked = layers.map(() => axis.keys.map(() => 0));
      for (let bi = 0; bi < axis.keys.length; bi++) {
        let acc = 0;
        for (let li = layers.length - 1; li >= 0; li--) {
          acc += raw[li][bi] || 0;
          stacked[li][bi] = acc;
        }
      }
      const bars = uPlotBars();
      const series = [{ label: BUCKET_LABEL[bucket] || "Bucket", value: (u, v) => fmtBucket(v, bucket) }];
      layers.forEach((l, li) => {
        const c = hasOther && li === layers.length - 1 ? pal[8] : pal[li % 8];
        series.push({
          label: l.name,
          stroke: c,
          fill: c,
          width: 0,
          paths: bars,
          points: { show: false },
          value: (u, v, si, di) => (di == null ? "—" : fmtChartMoney(raw[li][di])),
        });
      });
      const data = [axis.xs, ...stacked];
      if (subsMonthly > 0) {
        const f = bucketFactor(bucket);
        data.push(axis.xs.map(() => subsMonthly * f * fx.rate));
        series.push({
          label: "Subscription price",
          stroke: colors.sub,
          width: 2,
          dash: [6, 4],
          points: { show: false },
          value: (u, v) => (v == null ? "—" : fmtChartMoney(v)),
        });
      }
      mountChart(
        "spend",
        spendEl,
        {
          scales: { x: xScale(axis.xs), y: { range: (u, min, max) => [0, max > 0 ? max * 1.08 : 1] } },
          axes: [
            xAxisOpts(bucket, axis.xs, colors),
            {
              stroke: colors.muted,
              grid: { stroke: colors.grid },
              ticks: { stroke: colors.grid },
              size: 56,
              values: (u, vals) => vals.map(fmtMoneyAxis),
            },
          ],
          series,
          cursor: { points: { show: false }, drag: { x: false, y: false } },
          legend: { live: true },
        },
        data
      );
    }

    // ---- token mix
    const tt = totals.tokens || {};
    const ratioAll = cacheRatio(tt);
    $("tokens-sub").textContent =
      (bucket === "hour" ? "Last 24 hours · " : "") +
      "Uncached input " + fmtTokens(tt.input) +
      " · cache read " + fmtTokens(tt.cache_read) +
      " · cache write " + fmtTokens(tt.cache_write) +
      " · output " + fmtTokens(tt.output) +
      " (reasoning " + fmtTokens(tt.reasoning) + ", included in output)" +
      " · cache hit " + (ratioAll == null ? "—" : nf1.format(ratioAll) + "%") +
      (totals.cache_savings_usd != null ? " · saved " + fmtMoney(totals.cache_savings_usd) : "");

    if (!hasData) {
      emptyChart("tokens", tokEl, "No usage in this range.");
      return;
    }
    const kinds = [
      { k: "output", name: "Output", c: pal[1] },
      { k: "cache_write", name: "Cache write", c: pal[3] },
      { k: "cache_read", name: "Cache read", c: pal[2] },
      { k: "input", name: "Uncached input", c: pal[0] },
    ];
    const rawT = kinds.map((kd) =>
      axis.keys.map((key) => {
        const g = axis.byKey.get(key);
        return g && g.tokens ? g.tokens[kd.k] || 0 : 0;
      })
    );
    const stackedT = kinds.map(() => axis.keys.map(() => 0));
    for (let bi = 0; bi < axis.keys.length; bi++) {
      let acc = 0;
      for (let li = kinds.length - 1; li >= 0; li--) {
        acc += rawT[li][bi];
        stackedT[li][bi] = acc;
      }
    }
    const reasoning = axis.keys.map((key) => {
      const g = axis.byKey.get(key);
      return g && g.tokens ? g.tokens.reasoning || 0 : 0;
    });
    const ratio = axis.keys.map((key) => {
      const g = axis.byKey.get(key);
      return g ? cacheRatio(g.tokens) : null;
    });
    const bars = uPlotBars();
    const series = [{ label: BUCKET_LABEL[bucket] || "Bucket", value: (u, v) => fmtBucket(v, bucket) }];
    kinds.forEach((kd, li) => {
      series.push({
        label: kd.name,
        stroke: kd.c,
        fill: kd.c,
        width: 0,
        paths: bars,
        points: { show: false },
        value: (u, v, si, di) => (di == null ? "—" : fmtTokens(rawT[li][di])),
      });
    });
    series.push({
      label: "Reasoning (in output)",
      stroke: colors.reasoning,
      width: 2,
      dash: [3, 3],
      points: { show: false },
      value: (u, v) => (v == null ? "—" : fmtTokens(v)),
    });
    series.push({
      label: "Cache hit %",
      stroke: colors.ratio,
      width: 2,
      scale: "pct",
      spanGaps: true,
      points: { show: true, size: 4 },
      value: (u, v) => (v == null ? "—" : nf1.format(v) + "%"),
    });
    mountChart(
      "tokens",
      tokEl,
      {
        scales: {
          x: xScale(axis.xs),
          y: { range: (u, min, max) => [0, max > 0 ? max * 1.08 : 1] },
          pct: { range: [0, 100] },
        },
        axes: [
          xAxisOpts(bucket, axis.xs, colors),
          {
            stroke: colors.muted,
            grid: { stroke: colors.grid },
            ticks: { stroke: colors.grid },
            size: 56,
            values: (u, vals) => vals.map((v) => nfCompact.format(v)),
          },
          {
            scale: "pct",
            side: 1,
            stroke: colors.muted,
            grid: { show: false },
            ticks: { stroke: colors.grid },
            size: 52,
            values: (u, vals) => vals.map((v) => v + "%"),
          },
        ],
        series,
        cursor: { points: { show: false }, drag: { x: false, y: false } },
        legend: { live: true },
      },
      [axis.xs, ...stackedT, reasoning, ratio]
    );
  }

  function cacheRatio(t) {
    if (!t) return null;
    const denom = (t.input || 0) + (t.cache_read || 0) + (t.cache_write || 0);
    return denom > 0 ? ((t.cache_read || 0) / denom) * 100 : null;
  }

  function uPlotBars() {
    return uPlot.paths.bars({ size: [0.72, 64], align: 0 });
  }

  function resizeCharts() {
    const hgt = chartHeight();
    if (charts.spend) charts.spend.setSize({ width: Math.max(200, $("chart-spend").clientWidth), height: hgt });
    if (charts.tokens) charts.tokens.setSize({ width: Math.max(200, $("chart-tokens").clientWidth), height: hgt });
  }

  // ---------------------------------------------------------------- per-model table

  function renderModels() {
    const s = state.modelSummary;
    const groups = s && Array.isArray(s.groups) ? s.groups : [];
    state.modelRows = groups.map((g) => {
      const tokens = sumTokens(g.tokens);
      const lat = g.latency_ms || {};
      const ttft = g.ttft_ms || {};
      const priced = (g.requests || 0) - (g.unpriced_requests || 0);
      return {
        g,
        label: labelFor("model", g.key, g.label),
        requests: g.requests || 0,
        failed: g.failed || 0,
        tokens,
        cache_hit: cacheRatio(g.tokens),
        cost: g.cost_usd,
        saved: g.cache_savings_usd == null ? null : g.cache_savings_usd,
        per_req: g.cost_usd != null && priced > 0 ? g.cost_usd / priced : null,
        lat50: lat.p50 == null ? null : lat.p50,
        lat95: lat.p95 == null ? null : lat.p95,
        ttft50: ttft.p50 == null ? null : ttft.p50,
        ttft95: ttft.p95 == null ? null : ttft.p95,
      };
    });
    drawModelTable();
  }

  function drawModelTable() {
    const table = $("models");
    const tbody = clear(table.querySelector("tbody"));
    const oldFoot = table.querySelector("tfoot");
    if (oldFoot) oldFoot.remove();
    const rows = state.modelRows.slice();
    const { key, dir } = state.sort;
    const mul = dir === "asc" ? 1 : -1;
    rows.sort((a, b) => {
      const va = a[key];
      const vb = b[key];
      if (va == null && vb == null) return 0;
      if (va == null) return 1;
      if (vb == null) return -1;
      if (typeof va === "string") return va.localeCompare(vb) * mul;
      return (va - vb) * mul;
    });
    for (const th of table.querySelectorAll("th[data-key]")) {
      th.setAttribute("aria-sort", th.dataset.key === key ? (dir === "asc" ? "ascending" : "descending") : "none");
    }
    $("models-empty").hidden = rows.length > 0;
    const pctTitle = "outside raw retention";
    for (const r of rows) {
      const unpriced = r.g.unpriced_requests || 0;
      const costCell = h("td", { class: "num" });
      if (r.cost == null) costCell.append(h("span", { class: "unpriced", title: "No rate card for this model", text: "unpriced" }));
      else costCell.append(money(r.cost));
      if (r.cost != null && unpriced > 0) {
        costCell.append(" ", h("span", { class: "unpriced", title: unpriced + " unpriced request(s) excluded", text: "*" }));
      }
      const ms = (v) => {
        const t = fmtMs(v);
        return t == null ? na(pctTitle) : t;
      };
      tbody.append(
        h(
          "tr",
          null,
          h("td", { class: "wrap", title: r.g.key }, r.label),
          h("td", { class: "num" }, fmtInt(r.requests)),
          h("td", { class: "num" }, fmtInt(r.failed)),
          h("td", { class: "num", title: fmtInt(r.tokens) }, fmtTokens(r.tokens)),
          h("td", { class: "num opt" }, r.cache_hit == null ? na() : nf1.format(r.cache_hit) + "%"),
          costCell,
          h("td", { class: "num opt" }, r.saved == null ? na("needs per-request rows") : money(r.saved)),
          h("td", { class: "num" }, r.per_req == null ? na() : money(r.per_req)),
          h("td", { class: "num" }, ms(r.lat50)),
          h("td", { class: "num opt" }, ms(r.lat95)),
          h("td", { class: "num opt" }, ms(r.ttft50)),
          h("td", { class: "num opt" }, ms(r.ttft95))
        )
      );
    }
    const t = state.modelSummary && state.modelSummary.totals;
    if (rows.length && t) {
      const priced = (t.requests || 0) - (t.unpriced_requests || 0);
      table.append(
        h(
          "tfoot",
          null,
          h(
            "tr",
            null,
            h("td", null, "Total"),
            h("td", { class: "num" }, fmtInt(t.requests)),
            h("td", { class: "num" }, fmtInt(t.failed || 0)),
            h("td", { class: "num" }, fmtTokens(sumTokens(t.tokens))),
            h("td", { class: "num opt" }, cacheRatio(t.tokens) == null ? na() : nf1.format(cacheRatio(t.tokens)) + "%"),
            h("td", { class: "num" }, money(t.cost_usd)),
            h("td", { class: "num opt" }, t.cache_savings_usd == null ? na("needs per-request rows") : money(t.cache_savings_usd)),
            h("td", { class: "num" }, t.cost_usd != null && priced > 0 ? money(t.cost_usd / priced) : "—"),
            h("td", { colspan: 4 })
          )
        )
      );
    }
  }

  function onSortClick(ev) {
    const th = ev.target.closest("th[data-key]");
    if (!th) return;
    const key = th.dataset.key;
    if (state.sort.key === key) state.sort.dir = state.sort.dir === "asc" ? "desc" : "asc";
    else state.sort = { key, dir: th.dataset.type === "text" ? "asc" : "desc" };
    drawModelTable();
  }

  // ---------------------------------------------------------------- quota gauges

  // Projects a quota window from its average burn rate since the window started.
  function windowForecast(w, observedAt, stale) {
    if (stale) return { kind: "stale" };
    const used = w.used_percent;
    const reset = Date.parse(w.resets_at);
    const dur = w.duration_ms;
    const obs = Date.parse(observedAt);
    const start = reset - dur;
    const elapsed = obs - start;
    if (w.status === "exhausted" || used >= 100) return { kind: "exhausted" };
    if (used == null || !isFinite(used) || !isFinite(reset) || !isFinite(obs) || !(dur > 0) || !(elapsed > 0)) {
      return { kind: "unknown" };
    }
    const elapsedPct = Math.max(0, Math.min(100, (elapsed / dur) * 100));
    if (elapsed < Math.max(600000, 0.02 * dur)) return { kind: "early", elapsedPct };
    const rate = used / elapsed;
    const ratePerHour = rate * 3.6e6;
    const projected = (used * dur) / elapsed;
    if (projected >= 100) {
      return { kind: "out", elapsedPct, ratePerHour, projected, exhaustAt: obs + (100 - used) / rate, reset };
    }
    return { kind: "ok", elapsedPct, ratePerHour, projected, reset };
  }

  function quotaSeverity(c) {
    let sev = 0;
    for (const w of Array.isArray(c.windows) ? c.windows : []) {
      const k = windowForecast(w, c.observed_at, c.stale).kind;
      if (k === "exhausted") sev = Math.max(sev, 3);
      else if (k === "out") sev = Math.max(sev, 2);
      else if (w.status === "warning") sev = Math.max(sev, 1);
    }
    return sev;
  }

  function maxUsed(c) {
    let m = -1;
    for (const w of Array.isArray(c.windows) ? c.windows : []) if (w.used_percent != null && w.used_percent > m) m = w.used_percent;
    return m;
  }

  // One short line per window; the rate sits in the tooltip.
  function forecastLine(f) {
    const rate = f.ratePerHour != null ? nf1.format(f.ratePerHour) + " % per hour on average since the window started" : null;
    switch (f.kind) {
      case "stale":
        return h("div", { class: "forecast" }, "pace unknown (stale)");
      case "exhausted":
        return h("div", { class: "forecast bad" }, "limit reached");
      case "unknown":
        return h("div", { class: "forecast" }, "no projection");
      case "early":
        return h("div", { class: "forecast" }, "too early to project");
      case "out": {
        const iso = new Date(f.exhaustAt).toISOString();
        return h(
          "div",
          { class: "forecast warn", title: rate + "; runs out " + fmtDuration(f.reset - f.exhaustAt) + " before the reset" },
          "runs out in ",
          h("span", { "data-until": iso, title: fmtTime(iso) }, fmtDuration(f.exhaustAt - Date.now()))
        );
      }
      default:
        return h("div", { class: "forecast", title: rate }, "on pace for " + nf0.format(f.projected) + "% at reset");
    }
  }

  function renderQuota() {
    const root = clear($("quota"));
    const q = state.quota;
    const creds = q && Array.isArray(q.credentials) ? q.credentials.slice() : [];
    $("quota-sub").textContent = q && q.generated_at ? "as of " + fmtTime(q.generated_at) : "";
    if (!creds.length) {
      root.append(
        h("p", {
          class: "empty",
          text: "No quota observations yet. Gauges appear after a subscription credential (e.g. Codex or Claude) serves a request.",
        })
      );
      return;
    }
    const staleAfter = q.stale_after_ms || 1800000;
    const byCred = new Map();
    for (const g of state.credSummary && Array.isArray(state.credSummary.groups) ? state.credSummary.groups : []) byCred.set(g.key, g);
    const ranked = creds.map((c) => ({
      c,
      stale: c.stale ? 1 : 0,
      sev: quotaSeverity(c),
      used: maxUsed(c),
      label: labelFor("credential", c.credential, c.label),
    }));
    ranked.sort((a, b) => a.stale - b.stale || b.sev - a.sev || b.used - a.used || String(a.label).localeCompare(String(b.label)));
    for (const { c, label } of ranked) {
      const card = h("div", { class: "gauge-card" + (c.stale ? " is-stale" : "") });
      const title = h(
        "div",
        { class: "gauge-title" },
        h("strong", { title: c.credential }, label),
        c.provider ? h("span", { class: "badge neutral", text: c.provider }) : null,
        c.plan ? h("span", { class: "badge neutral", text: "plan: " + c.plan }) : null
      );
      const staleBadge = h("span", {
        class: "badge " + (c.stale ? "unknown" : "ok"),
        "data-observed": c.observed_at || "",
        "data-stale-after": staleAfter,
        "data-stale": c.stale ? "1" : "",
        text: c.stale ? "stale" : "fresh",
      });
      title.append(staleBadge);
      card.append(title);
      card.append(
        h(
          "div",
          { class: "gauge-meta", title: c.auth_id ? "CPA auth file: " + c.auth_id : null },
          "observed ",
          h("span", { "data-ago": c.observed_at || "", title: c.observed_at ? fmtTime(c.observed_at) : null }, fmtAgo(c.observed_at))
        )
      );
      const g = byCred.get(c.credential);
      if (g) {
        card.append(
          h(
            "div",
            { class: "gauge-spend" },
            h("span", { class: "gauge-spend-cost" }, costNode(g.cost_usd)),
            " API-equivalent · " + fmtInt(g.requests) + " req" + (g.failed ? " · " + fmtInt(g.failed) + " failed" : "")
          )
        );
      }
      const windows = Array.isArray(c.windows) ? c.windows : [];
      if (!windows.length) card.append(h("p", { class: "empty", text: "No rate-limit windows reported." }));
      for (const w of windows) {
        const status = c.stale ? "unknown" : w.status || "unknown";
        const pct = w.used_percent;
        const f = windowForecast(w, c.observed_at, c.stale);
        const fill = h("div", { class: "bar-fill " + status });
        fill.style.width = Math.max(0, Math.min(100, pct || 0)) + "%";
        const usedText = (pct == null ? "unknown" : nf1.format(pct) + "%") + " used";
        const bar = h(
          "div",
          {
            class: "bar",
            role: "meter",
            "aria-valuemin": 0,
            "aria-valuemax": 100,
            "aria-valuenow": pct == null ? null : pct,
            "aria-valuetext": f.elapsedPct == null ? usedText : usedText + ", " + nf0.format(f.elapsedPct) + "% of window elapsed",
            "aria-label": (w.label || w.id || "window") + " used",
          },
          fill
        );
        if (f.elapsedPct != null) {
          const pace = h("span", { class: "bar-pace", title: nf0.format(f.elapsedPct) + "% of the window elapsed" });
          pace.style.left = f.elapsedPct + "%";
          bar.append(pace);
        }
        const resetMs = Date.parse(w.resets_at);
        card.append(
          h(
            "div",
            { class: "window" },
            h(
              "div",
              { class: "window-head" },
              h("span", null, w.label || w.id || "window", " ", h("span", { class: "badge " + status, text: status })),
              h("span", { class: "pct" }, pct == null ? "—" : nf1.format(pct) + "%")
            ),
            bar,
            h(
              "div",
              { class: "window-foot" },
              isFinite(resetMs)
                ? h("span", { "data-reset": w.resets_at, title: fmtTime(w.resets_at) }, "resets in " + fmtDuration(resetMs - Date.now()))
                : h("span", null, "reset time unknown"),
              f.kind === "exhausted" || f.kind === "unknown" ? null : forecastLine(f)
            )
          )
        );
      }
      if (c.credits) {
        const cr = c.credits;
        let txt = "Credits: ";
        if (cr.unlimited) txt += "unlimited";
        else if (cr.has_credits === false) txt += "none";
        else txt += cr.balance != null ? String(cr.balance) : "available";
        card.append(h("div", { class: "credits muted" }, txt));
      }
      root.append(card);
    }
  }

  function tick() {
    const now = Date.now();
    for (const el of document.querySelectorAll("[data-reset]")) {
      const t = Date.parse(el.getAttribute("data-reset"));
      if (isFinite(t)) el.textContent = t - now > 0 ? "resets in " + fmtDuration(t - now) : "reset passed — awaiting new observation";
    }
    for (const el of document.querySelectorAll("[data-ago]")) {
      el.textContent = fmtAgo(el.getAttribute("data-ago"));
    }
    for (const el of document.querySelectorAll("[data-observed]")) {
      if (el.getAttribute("data-stale")) continue;
      const obs = Date.parse(el.getAttribute("data-observed"));
      const after = Number(el.getAttribute("data-stale-after"));
      if (isFinite(obs) && now - obs > after) {
        el.className = "badge unknown";
        el.textContent = "stale";
      }
    }
    for (const el of document.querySelectorAll("[data-until]")) {
      el.textContent = fmtDuration(Date.parse(el.dataset.until) - now);
    }
  }

  // ---------------------------------------------------------------- value panel

  // Subscription spend vs price for the selected range (shared by the value panel and KPI).
  function subscriptionValue() {
    const months = (state.until - state.since) / (MONTH_DAYS * DAY_MS);
    const byCred = new Map();
    const groups = state.credSummary && Array.isArray(state.credSummary.groups) ? state.credSummary.groups : [];
    for (const g of groups) byCred.set(g.key, g);
    let spend = 0;
    let price = 0;
    let anyUnknown = false;
    const rows = state.subscriptions.map((s) => {
      const g = byCred.get(s.credential);
      const sp = g ? g.cost_usd : 0;
      const pr = (Number(s.usd_per_month) || 0) * months;
      if (sp == null) anyUnknown = true;
      else spend += sp;
      price += pr;
      return { s, g, spend: sp, price: pr, ratio: sp != null && pr > 0 ? sp / pr : null };
    });
    return { rows, spend, price, anyUnknown, ratio: price > 0 ? spend / price : null };
  }

  function renderValue() {
    const root = clear($("value"));
    const rangeMs = state.until - state.since;
    $("value-sub").textContent =
      "API-equivalent spend in the selected range ÷ subscription price prorated to " + nf1.format(rangeMs / DAY_MS) + " days.";
    if (!state.subscriptions.length) {
      root.append(
        h("p", {
          class: "empty",
          text: "No subscriptions configured. Add subscriptions (credential + USD per month) to the plugin config to compare API-equivalent spend with what you pay.",
        })
      );
      return;
    }
    const v = subscriptionValue();
    const tbody = h("tbody");
    for (const { s, g, spend, price, ratio } of v.rows) {
      tbody.append(
        h(
          "tr",
          null,
          h("td", { class: "wrap", title: s.credential }, labelFor("credential", s.credential, s.label)),
          h("td", { class: "num" }, fmtInt(g ? g.requests : 0)),
          h("td", { class: "num" }, spend == null ? h("span", { class: "unpriced", text: "unpriced" }) : money(spend)),
          h(
            "td",
            { class: "num", title: fmtMoney(Number(s.usd_per_month) || 0) + "/month" + (fx.currency === "USD" ? "" : " · " + fmtUSD(price) + " (USD) for the range, " + fmtUSD(Number(s.usd_per_month) || 0) + "/month") },
            fmtMoney(price)
          ),
          h("td", { class: "num" }, ratio == null ? na() : h("strong", null, nf1.format(ratio) + "×"))
        )
      );
    }
    root.append(
      h(
        "div",
        { class: "value-total" },
        h("span", { class: "big" }, v.ratio == null ? "—" : nf1.format(v.ratio) + "× value"),
        h(
          "span",
          { class: "muted", title: fx.currency === "USD" ? null : fmtUSD(v.spend) + " vs " + fmtUSD(v.price) + " (USD)" },
          fmtMoney(v.spend) + " API-equivalent vs " + fmtMoney(v.price) + " subscription" + (v.anyUnknown ? " (some spend unpriced)" : "")
        )
      ),
      h(
        "div",
        { class: "table-wrap" },
        h(
          "table",
          { class: "data" },
          h("caption", { class: "sr-only" }, "Subscription value by credential"),
          h(
            "thead",
            null,
            h(
              "tr",
              null,
              h("th", null, "Credential"),
              h("th", { class: "num" }, "Requests"),
              h("th", { class: "num" }, "API-equivalent"),
              h("th", { class: "num" }, "Price (range)"),
              h("th", { class: "num" }, "Value")
            )
          ),
          tbody
        )
      )
    );
  }

  // ---------------------------------------------------------------- KPI tiles + attention strip

  function setKpi(id, value, sub, cls, title) {
    const tile = $(id);
    tile.className = "kpi" + (cls ? " " + cls : "");
    clear(tile.querySelector(".kpi-value")).append(value == null ? "—" : value);
    tile.querySelector(".kpi-sub").textContent = sub || "";
    if (title) tile.title = title;
    else tile.removeAttribute("title");
  }

  // Sums the day groups of the period summary from fromKey (a local day key) on.
  function periodTotals(fromKey) {
    let sum = 0;
    let priced = false;
    let requests = 0;
    let failed = 0;
    let unpriced = 0;
    const groups = state.periodSummary && Array.isArray(state.periodSummary.groups) ? state.periodSummary.groups : [];
    for (const g of groups) {
      if (String(g.key) < fromKey) continue;
      if (g.cost_usd != null) {
        priced = true;
        sum += g.cost_usd;
      }
      requests += g.requests || 0;
      failed += g.failed || 0;
      unpriced += g.unpriced_requests || 0;
    }
    return { cost: priced ? sum : requests ? null : 0, requests, failed, unpriced };
  }

  function unpricedTitle(n) {
    return n > 0 ? fmtInt(n) + " unpriced request(s) not included" : null;
  }

  function rawRetentionDays() {
    const s = state.modelSummary;
    return s && s.health && s.health.raw_retention_days != null ? s.health.raw_retention_days : null;
  }

  function renderKpis() {
    const now = new Date();
    $("kpi-range-l").textContent = "Last " + state.range + " days";

    if (state.periodSummary && Array.isArray(state.periodSummary.groups)) {
      const today = periodTotals(bucketKey(startOfDay(now), "day"));
      setKpi(
        "kpi-today",
        money(today.cost),
        fmtInt(today.requests) + " requests · " + fmtInt(today.failed) + " failed",
        null,
        unpricedTitle(today.unpriced)
      );
      const ws = weekStart(now);
      const week = periodTotals(bucketKey(ws, "day"));
      setKpi(
        "kpi-week",
        money(week.cost),
        "since " + ws.toLocaleDateString(undefined, { weekday: "short", month: "short", day: "numeric" }),
        null,
        unpricedTitle(week.unpriced)
      );
      const ms = monthStart(now);
      const month = periodTotals(bucketKey(ms, "day"));
      const elapsed = now - ms;
      const len = nextBucket(ms, "month") - ms;
      let sub =
        elapsed >= DAY_MS
          ? month.cost == null
            ? "pace unknown (unpriced)"
            : "on pace for " + fmtMoney((month.cost * len) / elapsed)
          : "pace available after the first day";
      const subsMonthly = state.subscriptions.reduce((a, x) => a + (Number(x.usd_per_month) || 0), 0);
      if (state.subscriptions.length) sub += " · subscriptions " + fmtMoney(subsMonthly) + "/mo";
      setKpi("kpi-month", money(month.cost), sub, null, unpricedTitle(month.unpriced));
    } else {
      for (const id of ["kpi-today", "kpi-week", "kpi-month"]) setKpi(id, null, "not loaded");
    }

    const s = state.modelSummary;
    const t = s && s.totals;
    if (!t) {
      for (const id of ["kpi-range", "kpi-cache", "kpi-value", "kpi-failures"]) setKpi(id, null, "not loaded");
      return;
    }
    const tokens = t.tokens || {};
    setKpi(
      "kpi-range",
      money(t.cost_usd),
      fmtInt(t.requests) + " req · " + fmtTokens(sumTokens(tokens)) + " tokens · " +
        fmtMoney(t.cost_usd == null ? null : t.cost_usd / state.range) + "/day",
      null,
      unpricedTitle(t.unpriced_requests || 0)
    );

    if (t.cache_savings_usd != null) {
      const ratio = cacheRatio(tokens);
      setKpi(
        "kpi-cache",
        money(t.cache_savings_usd),
        "cache hit " + (ratio == null ? "—" : nf1.format(ratio)) + "% · " + fmtTokens(tokens.cache_read || 0) + " read"
      );
    } else {
      const raw = rawRetentionDays();
      setKpi(
        "kpi-cache",
        null,
        raw != null && state.range > raw ? "needs per-request rows (last " + raw + " days)" : "no priced cached requests"
      );
    }

    if (state.subscriptions.length) {
      const v = subscriptionValue();
      setKpi(
        "kpi-value",
        v.ratio == null ? null : nf1.format(v.ratio) + "×",
        fmtMoney(v.spend) + " API-equivalent vs " + fmtMoney(v.price) + " paid",
        null,
        v.anyUnknown ? "Some subscription spend is unpriced" : null
      );
    } else {
      setKpi("kpi-value", null, "no subscriptions configured");
    }

    const req = t.requests || 0;
    const failed = t.failed || 0;
    if (!req) {
      setKpi("kpi-failures", "0%", "no requests");
    } else {
      const rate = failed / req;
      let sub = fmtInt(failed) + " failed";
      const rl = Array.isArray(s.failures) ? s.failures.find((f) => f.status === 429) : null;
      if (rl) sub += " · " + fmtInt(rl.requests) + " rate-limited (429)";
      setKpi("kpi-failures", nf1.format(rate * 100) + "%", sub, rate >= FAIL_BAD ? "bad" : rate >= FAIL_WARN ? "warn" : null);
    }
  }

  function renderAttention() {
    const list = clear($("attention"));
    // Each alert: an optional bold heading (the account) over its detail.
    const chip = (error, title, ...parts) =>
      list.append(
        h(
          "li",
          { class: "chip" + (error ? " error" : "") },
          title ? h("strong", { class: "chip-title" }, title) : null,
          h("span", { class: "chip-detail" }, ...parts)
        )
      );
    const untilSpan = (ms) => {
      const iso = new Date(ms).toISOString();
      return h("span", { "data-until": iso, title: fmtTime(iso) }, fmtDuration(ms - Date.now()));
    };

    const creds = state.quota && Array.isArray(state.quota.credentials) ? state.quota.credentials : [];
    const exhausted = [];
    const running = [];
    for (const c of creds) {
      if (c.stale) continue;
      const label = labelFor("credential", c.credential, c.label);
      for (const w of Array.isArray(c.windows) ? c.windows : []) {
        const f = windowForecast(w, c.observed_at, false);
        const name = w.label || w.id || "window";
        if (f.kind === "exhausted") exhausted.push({ label, name, reset: Date.parse(w.resets_at) });
        else if (f.kind === "out") running.push({ label, name, at: f.exhaustAt });
      }
    }
    // "Claude (sage) · Fable 7 Day" as the heading; the detail stays one short line.
    for (const x of exhausted) {
      const title = x.label + " · " + x.name;
      if (isFinite(x.reset)) chip(true, title, "Limit reached · resets in ", untilSpan(x.reset));
      else chip(true, title, "Limit reached");
    }
    for (const x of running) {
      chip(false, x.label + " · " + x.name, "Runs out in ", untilSpan(x.at), " at this pace");
    }

    const s = state.modelSummary;
    const t = s && s.totals;
    if (t && t.requests > 0 && (t.failed || 0) >= 5 && t.failed / t.requests >= FAIL_BAD) {
      chip(false, "Failures", nf1.format((t.failed / t.requests) * 100) + "% of requests failed in this range");
    }
    const hl = (s && s.health) || {};
    if (hl.feed_status === "error") chip(true, "Pricing", "Pricing feed error");
    else if (hl.feed_status === "stale") chip(false, "Pricing", "Pricing feed is stale");
    if (hl.write_error != null) chip(true, "Ledger", "Write error: " + hl.write_error);
    if (hl.dropped_records > 0) chip(false, "Ledger", fmtInt(hl.dropped_records) + " usage record(s) dropped (queue full)");
    if (fx.currency !== "USD" && fx.info && (fx.info.status === "stale" || fx.info.status === "error")) {
      chip(false, "Currency", "Exchange rates " + fx.info.status);
    }
    if (s && Array.isArray(s.notices) && s.notices.includes("client_fingerprints_changed")) {
      chip(false, "Clients", "Client fingerprint secret changed");
    }
    list.hidden = list.childElementCount === 0;
  }

  // ---------------------------------------------------------------- by credential / by client

  function renderBreakdown(kind) {
    const s = kind === "credential" ? state.credSummary : state.clientSummary;
    const id = kind === "credential" ? "by-credential" : "by-client";
    const table = $(id);
    const wrap = table.parentElement;
    const empty = $(id + "-empty");
    const tbody = clear(table.querySelector("tbody"));
    const notices = s && Array.isArray(s.notices) ? s.notices : [];
    if (kind === "client" && notices.includes("client_fingerprints_disabled")) {
      wrap.hidden = true;
      empty.hidden = false;
      empty.textContent = NOTICE_TEXT.client_fingerprints_disabled;
      return;
    }
    const groups = s && Array.isArray(s.groups) ? s.groups : [];
    wrap.hidden = groups.length === 0;
    empty.hidden = groups.length > 0;
    empty.textContent = s ? "No requests in this range." : "No data.";
    const total = s && s.totals ? s.totals.cost_usd : null;
    for (const g of groups) {
      const req = g.requests || 0;
      const failed = g.failed || 0;
      const ratio = cacheRatio(g.tokens);
      const share = total > 0 && g.cost_usd != null ? g.cost_usd / total : null;
      const shareCell = h("td");
      if (share == null) {
        shareCell.append(na());
      } else {
        const fill = h("span", { class: "share-fill" });
        fill.style.width = Math.max(0, Math.min(100, share * 100)) + "%";
        shareCell.append(nf0.format(share * 100) + "%", h("span", { class: "share", "aria-hidden": "true" }, fill));
      }
      const tokens = sumTokens(g.tokens);
      tbody.append(
        h(
          "tr",
          null,
          h("td", { class: "wrap", title: g.key || null }, labelFor(kind, g.key, g.label)),
          h("td", { class: "num" }, fmtInt(req)),
          h("td", { class: "num" }, fmtInt(failed) + (failed > 0 && req > 0 ? " (" + nf1.format((failed / req) * 100) + "%)" : "")),
          h("td", { class: "num opt" }, ratio == null ? na() : nf1.format(ratio) + "%"),
          h("td", { class: "num opt", title: fmtInt(tokens) }, fmtTokens(tokens)),
          h("td", { class: "num" }, costNode(g.cost_usd)),
          shareCell
        )
      );
    }
  }

  // ---------------------------------------------------------------- most expensive + failures

  function renderTop() {
    const root = clear($("top"));
    const sub = $("top-sub");
    const t = state.top;
    if (!t) {
      sub.textContent = "";
      root.append(h("p", { class: "empty", text: "No data." }));
      return;
    }
    const attempts = Array.isArray(t.attempts) ? t.attempts : [];
    const raw = rawRetentionDays();
    const note = raw != null && state.range > raw ? "individual requests are kept for " + raw + " days" : "";
    if (!attempts.length) {
      sub.textContent = note ? note.charAt(0).toUpperCase() + note.slice(1) + "." : "";
      root.append(h("p", { class: "empty", text: "No priced requests in this range." }));
      return;
    }
    const sum = attempts.reduce((a, x) => a + ((x.cost && x.cost.total) || 0), 0);
    const total = state.modelSummary && state.modelSummary.totals ? state.modelSummary.totals.cost_usd : null;
    sub.textContent =
      "Top " + attempts.length + " priced attempts in the range · together " + fmtMoney(sum) +
      (total > 0 ? " (" + nf1.format((sum / total) * 100) + "% of spend)" : "") +
      (note ? " · " + note : "");
    root.append(attemptsTable(attempts.map((a) => attemptRow(a, a.trace_id)), "Most expensive requests"));
  }

  const STATUS_TEXT = {
    400: "Bad request",
    401: "Unauthorized",
    403: "Forbidden",
    404: "Not found",
    408: "Timeout",
    429: "Rate limited",
    500: "Upstream error",
    502: "Bad gateway",
    503: "Unavailable",
    504: "Gateway timeout",
    529: "Overloaded",
  };

  function statusMeaning(st) {
    if (st == null) return "No status reported";
    if (STATUS_TEXT[st]) return STATUS_TEXT[st];
    if (st >= 500) return "Server error";
    if (st >= 400) return "Client error";
    return "";
  }

  function simpleTable(caption, head, rows) {
    return h(
      "div",
      { class: "table-wrap" },
      h(
        "table",
        { class: "data" },
        h("caption", { class: "sr-only" }, caption),
        h("thead", null, h("tr", null, ...head.map(([text, cls]) => h("th", { class: cls || null }, text)))),
        h("tbody", null, ...rows)
      )
    );
  }

  function renderReliability() {
    const root = clear($("reliability"));
    const sub = $("reliability-sub");
    const btn = $("show-failed");
    const s = state.modelSummary;
    const t = s && s.totals;
    if (!t) {
      sub.textContent = "";
      btn.hidden = true;
      root.append(h("p", { class: "empty", text: "No data." }));
      return;
    }
    const req = t.requests || 0;
    const failed = t.failed || 0;
    sub.textContent = fmtInt(failed) + " of " + fmtInt(req) + " requests failed (" + nf1.format(req ? (failed / req) * 100 : 0) + "%)";
    btn.hidden = failed === 0;
    if (failed === 0) {
      root.append(h("p", { class: "empty", text: "No failed requests in this range." }));
      return;
    }
    if (s.failures == null) {
      const raw = rawRetentionDays();
      root.append(
        h("p", {
          class: "empty",
          text: "Status breakdown needs individual requests; this range starts before the " + (raw == null ? "raw" : raw + "-day raw") + " retention.",
        })
      );
    } else {
      const list = Array.isArray(s.failures) ? s.failures : [];
      const denom = list.reduce((a, f) => a + (f.requests || 0), 0);
      root.append(
        h("h3", null, "By status"),
        simpleTable(
          "Failures by upstream status",
          [["Status"], ["Meaning"], ["Requests", "num"], ["Share of failures", "num"]],
          list.map((f) =>
            h(
              "tr",
              null,
              h("td", null, f.status == null ? "—" : String(f.status)),
              h("td", null, statusMeaning(f.status)),
              h("td", { class: "num" }, fmtInt(f.requests)),
              h("td", { class: "num" }, denom > 0 ? nf1.format((f.requests / denom) * 100) + "%" : "—")
            )
          )
        )
      );
    }
    const creds = (state.credSummary && Array.isArray(state.credSummary.groups) ? state.credSummary.groups : [])
      .filter((g) => (g.failed || 0) > 0)
      .sort((a, b) => b.failed - a.failed);
    if (creds.length) {
      root.append(
        h("h3", null, "By credential"),
        simpleTable(
          "Failures by credential",
          [["Credential"], ["Failed", "num"], ["Rate", "num"]],
          creds.map((g) =>
            h(
              "tr",
              null,
              h("td", { class: "wrap", title: g.key || null }, labelFor("credential", g.key, g.label)),
              h("td", { class: "num" }, fmtInt(g.failed)),
              h("td", { class: "num" }, g.requests ? nf1.format((g.failed / g.requests) * 100) + "%" : "—")
            )
          )
        )
      );
    }
  }

  // ---------------------------------------------------------------- health + unpriced

  function renderHealth() {
    const s = state.modelSummary;
    const notices = clear($("notices"));
    const kv = clear($("health"));
    const up = clear($("unpriced"));
    if (!s) {
      up.append(h("p", { class: "empty", text: "No data." }));
      return;
    }
    const hl = s.health || {};
    if (hl.feed_error) notices.append(h("div", { class: "notice error" }, "Pricing feed error: " + hl.feed_error));
    for (const n of Array.isArray(s.notices) ? s.notices : []) {
      notices.append(h("div", { class: "notice" }, NOTICE_TEXT[n] || String(n)));
    }
    if (hl.token_mismatch) {
      notices.append(
        h("div", { class: "notice" }, fmtInt(hl.token_mismatch) + " request(s) where interceptor and ledger token counts disagreed.")
      );
    }
    const row = (k, v) => {
      if (v === undefined) return;
      kv.append(h("dt", null, k), h("dd", null, v == null ? na() : v));
    };
    const st = hl.feed_status;
    row(
      "Feed status",
      st == null ? null : h("span", { class: "badge " + (st === "ok" ? "ok" : st === "stale" ? "warning" : "error"), text: st })
    );
    if (hl.feed_fetched_at !== undefined) {
      row(
        "Last fetch",
        hl.feed_fetched_at
          ? h("span", { title: fmtTime(hl.feed_fetched_at), "data-ago": hl.feed_fetched_at }, fmtAgo(hl.feed_fetched_at))
          : null
      );
    }
    if (hl.feed_etag !== undefined) row("ETag", hl.feed_etag ? h("span", { class: "mono" }, hl.feed_etag) : null);
    if (hl.feed_url !== undefined) row("Feed URL", hl.feed_url ? h("span", { class: "mono" }, hl.feed_url) : null);
    if (hl.feed_error !== undefined) row("Last error", hl.feed_error ? String(hl.feed_error) : "none");
    const fi = fx.info;
    if (fi) {
      const fs = fi.status;
      row("FX source", fi.source === "ecb" ? "ECB euro reference rates" : fi.source);
      row("FX status", h("span", { class: "badge " + (fs === "ok" ? "ok" : fs === "off" ? "neutral" : fs === "stale" ? "warning" : "error"), text: fs }));
      if (fi.source === "ecb") {
        row("FX reference date", fi.as_of || null);
        row(
          "FX last fetch",
          fi.fetched_at ? h("span", { title: fmtTime(fi.fetched_at), "data-ago": fi.fetched_at }, fmtAgo(fi.fetched_at)) : null
        );
        row("FX last error", fi.error ? String(fi.error) : "none");
      }
      const missing = (fi.currencies || []).filter((c) => !(fi.rates && fi.rates[c] > 0));
      if (missing.length) row("FX no rate", missing.join(", "));
      if (fi.error && fi.source === "ecb") notices.append(h("div", { class: "notice error" }, "Exchange-rate feed error: " + fi.error));
    }
    if (hl.dropped_records !== undefined) row("Dropped records", fmtInt(hl.dropped_records));
    if (hl.queue_depth !== undefined) row("Queue depth", fmtInt(hl.queue_depth));
    if (hl.token_mismatch !== undefined) row("Token mismatches", fmtInt(hl.token_mismatch));
    if (hl.raw_retention_days !== undefined) row("Raw retention", fmtInt(hl.raw_retention_days) + " days");

    const list = Array.isArray(s.unpriced_models) ? s.unpriced_models : [];
    if (!list.length) {
      up.append(h("p", { class: "empty", text: "Every model in this range is priced." }));
      return;
    }
    const tbody = h("tbody");
    for (const m of list) {
      tbody.append(
        h(
          "tr",
          null,
          h("td", { class: "wrap" }, m.model || "—"),
          h("td", { class: "wrap" }, m.provider || "—"),
          h("td", { class: "num" }, fmtInt(m.requests)),
          h("td", null, h("span", { class: "badge " + (m.status === "unsupported" ? "error" : "warning"), text: m.status || "unknown" }))
        )
      );
    }
    up.append(
      h(
        "div",
        { class: "table-wrap" },
        h(
          "table",
          { class: "data" },
          h("caption", { class: "sr-only" }, "Unpriced or unsupported models"),
          h(
            "thead",
            null,
            h("tr", null, h("th", null, "Model"), h("th", null, "Provider"), h("th", { class: "num" }, "Requests"), h("th", null, "Status"))
          ),
          tbody
        )
      )
    );
  }

  // ---------------------------------------------------------------- drill-down

  function attemptStatus(a) {
    if (a.failed) return h("span", { class: "badge failed", text: "failed" + (a.failure_status ? " " + a.failure_status : "") });
    const ps = a.pricing_status || "unknown";
    const cls = ps === "ok" || ps === "override" ? "ok" : ps === "partial" ? "warning" : "unknown";
    return h("span", { class: "badge " + cls, text: ps });
  }

  function costNode(v) {
    return v == null ? h("span", { class: "unpriced", text: "unpriced" }) : money(v);
  }

  function attemptRow(a, traceId) {
    const tr = h(
      "tr",
      { tabindex: 0 },
      h("td", { title: fmtTime(a.requested_at) }, fmtTimeShort(a.requested_at)),
      h("td", { class: "wrap" }, a.model || "—"),
      h("td", { class: "opt" }, a.provider || "—"),
      h("td", { class: "opt", title: a.credential || "" }, a.credential ? labelFor("credential", a.credential, null) : "—"),
      h("td", { class: "num" }, fmtTokens(sumTokens(a.tokens))),
      h("td", { class: "num" }, costNode(a.cost ? a.cost.total : null)),
      h("td", { class: "opt num" }, fmtMs(a.latency_ms) || na()),
      h("td", null, attemptStatus(a))
    );
    const open = () => showDetail(a, traceId || a.trace_id || null);
    tr.addEventListener("click", open);
    tr.addEventListener("keydown", (ev) => {
      if (ev.key === "Enter" || ev.key === " ") {
        ev.preventDefault();
        open();
      }
    });
    return tr;
  }

  function attemptsTable(rows, caption) {
    return h(
      "div",
      { class: "table-wrap" },
      h(
        "table",
        { class: "data clickable" },
        h("caption", { class: "sr-only" }, caption || "Attempts"),
        h(
          "thead",
          null,
          h(
            "tr",
            null,
            h("th", null, "Time"),
            h("th", null, "Model"),
            h("th", { class: "opt" }, "Provider"),
            h("th", { class: "opt" }, "Credential"),
            h("th", { class: "num" }, "Tokens"),
            h("th", { class: "num" }, "Cost"),
            h("th", { class: "opt num" }, "Latency"),
            h("th", null, "Status")
          )
        ),
        h("tbody", null, ...rows)
      )
    );
  }

  function renderRecent() {
    const tbody = clear($("recent").querySelector("tbody"));
    for (const a of state.recent) tbody.append(attemptRow(a, a.trace_id));
    const empty = $("recent-empty");
    empty.hidden = state.recent.length > 0;
    empty.textContent = state.recentFailed ? "No failed requests." : "No recent requests.";
    $("recent-more").hidden = !state.recentNext;
  }

  async function loadMoreRecent() {
    if (!state.recentNext || state.recentBusy) return;
    state.recentBusy = true;
    const btn = $("recent-more");
    btn.disabled = true;
    btn.textContent = "Loading…";
    const gen = state.gen;
    try {
      const failed = state.recentFailed;
      const r = await api("requests", { recent: RECENT_PAGE, before: state.recentNext, failed: failed ? 1 : null });
      if (gen !== state.gen || failed !== state.recentFailed) return;
      const rows = Array.isArray(r.attempts) ? r.attempts : [];
      state.recent = state.recent.concat(rows);
      state.recentNext = r.next_before || null;
      state.recentPages++;
      renderRecent();
    } catch (e) {
      if (!handleError(e)) showBanner("Loading more requests failed — " + e.message);
    } finally {
      state.recentBusy = false;
      btn.disabled = false;
      btn.textContent = "Load more";
    }
  }

  // Reloads the first recent page after the failures-only filter changed.
  async function reloadRecentFiltered() {
    const failed = state.recentFailed;
    const gen = state.gen;
    try {
      const r = await api("requests", { recent: RECENT_PAGE, failed: failed ? 1 : null });
      if (gen !== state.gen || failed !== state.recentFailed) return;
      state.recent = Array.isArray(r.attempts) ? r.attempts : [];
      state.recentNext = r.next_before || null;
      state.recentPages = 1;
      renderRecent();
    } catch (e) {
      if (!handleError(e)) showBanner("Loading recent requests failed — " + e.message);
    }
  }

  function setRecentFailed(on) {
    $("recent-failed").checked = on;
    state.recentFailed = on;
    return reloadRecentFiltered();
  }

  function traceBlock(t) {
    const attempts = Array.isArray(t.attempts) ? t.attempts : [];
    const statusCls = t.status === "complete" ? "ok" : t.status === "pending" ? "warning" : "unknown";
    const block = h(
      "div",
      null,
      h(
        "div",
        { class: "trace-head" },
        h("span", null, "Trace ", h("span", { class: "mono" }, t.trace_id || "—")),
        h("span", { class: "badge " + statusCls, text: t.status || "unknown" }),
        h("span", null, "Cost ", costNode(t.cost_usd)),
        h("span", { class: "muted" }, attempts.length + " attempt" + (attempts.length === 1 ? "" : "s"))
      )
    );
    if (attempts.length) block.append(attemptsTable(attempts.map((a) => attemptRow(a, t.trace_id))));
    else if (t.status === "expired") block.append(h("p", { class: "empty", text: "Older than raw retention; attempt rows were deleted." }));
    return block;
  }

  function renderLookup(hits) {
    const out = clear($("lookup-result"));
    for (const t of hits) out.append(traceBlock(t));
  }

  async function lookup(id) {
    const out = clear($("lookup-result"));
    state.lookupTraces = null;
    const value = String(id || "").trim();
    if (!value) return;
    out.append(h("p", { class: "muted", text: "Looking up…" }));
    const gen = state.gen;
    try {
      let traces = [];
      const r1 = await api("requests", { trace_id: value });
      traces = Array.isArray(r1.traces) ? r1.traces : [];
      const found = traces.some((t) => t.status === "complete" || t.status === "expired");
      if (!found) {
        try {
          const r2 = await api("requests", { request_id: value });
          const t2 = Array.isArray(r2.traces) ? r2.traces : [];
          if (t2.some((t) => t.status === "complete")) traces = t2;
        } catch (e) {
          if (isAuthError(e)) throw e;
          if (!(e instanceof ApiError && (e.status === 404 || e.status === 400))) throw e;
        }
      }
      if (gen !== state.gen) return;
      clear(out);
      const hits = traces.filter((t) => t.status !== "pending");
      if (!hits.length) {
        out.append(
          h("p", { class: "empty", text: "No attempts found for that id yet (pending). Ledger rows may arrive a few seconds after the response." })
        );
        return;
      }
      state.lookupTraces = hits;
      renderLookup(hits);
    } catch (e) {
      if (handleError(e)) return;
      clear(out).append(h("div", { class: "notice error" }, "Lookup failed: " + e.message));
    }
  }

  function showDetail(a, traceId) {
    const dl = clear($("detail-body"));
    const row = (k, v) => dl.append(h("dt", null, k), h("dd", null, v == null || v === "" ? na() : v));
    const mono = (v) => (v == null ? null : h("span", { class: "mono" }, String(v)));
    const t = a.tokens || {};
    const c = a.cost || {};
    row("Request id", mono(a.request_id));
    row("Trace id", mono(traceId));
    row("Requested at", a.requested_at ? fmtTime(a.requested_at) + " (" + a.requested_at + ")" : null);
    row("Provider", a.provider);
    row("Model", a.model);
    row("Response model", a.response_model);
    row("Credential", a.credential ? h("span", null, labelFor("credential", a.credential, null) + " ", mono(a.credential)) : null);
    if (a.auth_id !== undefined) row("Auth id", mono(a.auth_id));
    row("Client", a.client ? labelFor("client", a.client, null) : "(none)");
    row("Stream", a.stream ? "yes" : "no");
    row("Status", h("span", null, attemptStatus(a)));
    row("Latency", fmtMs(a.latency_ms));
    row("TTFT", fmtMs(a.ttft_ms));
    row(
      "Tokens",
      "uncached input " + fmtInt(t.input) +
        " · cache read " + fmtInt(t.cache_read) +
        " · cache write " + fmtInt(t.cache_write) +
        " · output " + fmtInt(t.output) +
        " (reasoning " + fmtInt(t.reasoning) + ")"
    );
    row("Cost", costNode(c.total));
    const breakdown = (f) => "input " + f(c.input) + " · cache read " + f(c.cache_read) + " · cache write " + f(c.cache_write) + " · output " + f(c.output);
    row("Cost breakdown", fx.currency === "USD" ? breakdown(fmtMoney) : h("span", { title: breakdown(fmtUSD) + " (USD)" }, breakdown(fmtMoney)));
    row("Pricing", a.pricing_status);
    row("Rate card", mono(a.rate_card_id));
    row("Tier", a.tier == null ? "base" : typeof a.tier === "object" ? JSON.stringify(a.tier) : String(a.tier));
    state.detailTrace = traceId;
    $("detail-trace").hidden = !traceId;
    const dlg = $("detail");
    if (!dlg.open) dlg.showModal();
  }

  // ---------------------------------------------------------------- wiring

  function init() {
    $("auth-form").addEventListener("submit", onAuthSubmit);
    $("auth").addEventListener("cancel", (ev) => ev.preventDefault());
    for (const r of document.querySelectorAll('input[name="mode"]')) r.addEventListener("change", syncAuthLabel);
    $("refresh").addEventListener("click", () => refresh(true));
    $("currency").addEventListener("change", onCurrencyChange);
    $("signout").addEventListener("click", signOut);
    $("range").addEventListener("change", (ev) => {
      state.range = Number(ev.target.value) || 30;
      refresh(true);
    });
    $("stack").addEventListener("change", (ev) => {
      state.stack = ev.target.value;
      refreshTimeSeries();
    });
    $("bucket").addEventListener("change", (ev) => {
      state.bucket = ev.target.value;
      refreshTimeSeries();
    });
    const modelHead = $("models").querySelector("thead");
    modelHead.addEventListener("click", onSortClick);
    for (const th of modelHead.querySelectorAll("th[data-key]")) {
      th.tabIndex = 0;
      th.addEventListener("keydown", (ev) => {
        if (ev.key === "Enter" || ev.key === " ") {
          ev.preventDefault();
          onSortClick(ev);
        }
      });
    }
    $("lookup").addEventListener("submit", (ev) => {
      ev.preventDefault();
      lookup($("lookup-id").value);
    });
    $("recent-more").addEventListener("click", loadMoreRecent);
    $("recent-failed").addEventListener("change", (ev) => setRecentFailed(ev.target.checked));
    $("show-failed").addEventListener("click", () => {
      setRecentFailed(true);
      const reduce = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
      $("card-drill").scrollIntoView({ behavior: reduce ? "auto" : "smooth", block: "start" });
    });
    $("detail-close").addEventListener("click", () => $("detail").close());
    $("detail").addEventListener("click", (ev) => {
      if (ev.target === $("detail")) $("detail").close();
    });
    $("detail-trace").addEventListener("click", () => {
      const id = state.detailTrace;
      $("detail").close();
      if (!id) return;
      $("lookup-id").value = id;
      lookup(id);
      $("card-drill").scrollIntoView({ behavior: "smooth", block: "start" });
    });
    document.addEventListener("visibilitychange", onVisibility);

    let resizeFrame = 0;
    const ro = new ResizeObserver(() => {
      cancelAnimationFrame(resizeFrame);
      resizeFrame = requestAnimationFrame(resizeCharts);
    });
    ro.observe($("chart-spend"));
    ro.observe($("chart-tokens"));

    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    const onScheme = () => {
      if (state.timeSummary) renderSpendAndTokens();
    };
    if (mq.addEventListener) mq.addEventListener("change", onScheme);
    else if (mq.addListener) mq.addListener(onScheme);

    syncAuthLabel();
    promptAuth(null);
  }

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", init);
  else init();
})();
