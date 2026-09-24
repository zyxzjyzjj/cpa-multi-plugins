package main

import "strings"

// panelHTML returns the browser dashboard served at
// /v0/resource/plugins/codearts-provider/panel.
//
// The page follows the VS Code extension's own model: there is exactly one way
// to sign in, the browser authorization the extension performs ("登录" ->
// authorize in the CodeArts console -> the account appears). Everything else on
// the page reports state (accounts, quota, scheduled tasks), it is not another
// way to log in. The credential-import routes still exist for operators who
// already hold an AK/SK pair, but they are not part of the authorization flow
// and are documented in README.md instead of being a second button here.
//
// Resource routes are NOT admin-authenticated, so this page holds no secrets of
// its own: it reads the admin key from the management UI's localStorage (as
// documented for same-origin admin deployments) and calls the authenticated
// /v0/management/... routes. Scripts are inlined rather than loaded from a CDN so
// the admin page never pulls third-party code into an admin context.
func panelHTML() string {
	return strings.ReplaceAll(panelTemplate, "__PROVIDER__", providerID)
}

const panelTemplate = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>CodeArts 订阅</title>
<style>
:root{
  --bg:#faf9f5;--card:#ffffff;--border:#e3e1db;--fg:#2d2a26;--mut:#6d6760;
  --acc:#4c8dff;--on-acc:#ffffff;--ok:#10b981;--warn:#e0aa14;--err:#c65746;
  --surface:#f0eee8;--surface-muted:#e9e6df;--btn-sec:#eef1f6;--btn-sec-hover:#e4e8ef;
  --input-bg:#ffffff;--code-bg:#f3f4f6;--code-fg:#374151;--placeholder:rgba(109,103,96,.55);
  --ok-soft:rgba(16,185,129,.10);--ok-border:rgba(16,185,129,.30);
  --warn-soft:rgba(224,170,20,.12);--warn-border:rgba(224,170,20,.32);
  --err-soft:rgba(198,87,70,.10);--err-border:rgba(198,87,70,.30);
  --mut-soft:rgba(109,103,96,.12);--mut-border:rgba(109,103,96,.28);
  --scrim:rgba(45,42,38,.35);--toast-shadow:0 10px 28px rgba(0,0,0,.12),0 0 0 1px rgba(0,0,0,.04);
  --modal-shadow:0 24px 56px rgba(0,0,0,.18),0 0 0 1px rgba(0,0,0,.04);
  --acc-ring:rgba(76,141,255,.18);--radius-md:12px;--radius-lg:16px;
  color-scheme:light
}
:root[data-theme="white"]{
  --bg:#f5f5f5;--card:#ffffff;--border:#e6e6e6;--fg:#1f1f1f;--mut:#707070;
  --surface:#fafafa;--surface-muted:#efefef;--btn-sec:#f0f0f0;--btn-sec-hover:#e8e8e8;
  --input-bg:#ffffff;--code-bg:#f3f4f6;--code-fg:#374151;--placeholder:rgba(112,112,112,.55);
  --scrim:rgba(0,0,0,.32);--toast-shadow:0 10px 28px rgba(0,0,0,.10),0 0 0 1px rgba(0,0,0,.04);
  --modal-shadow:0 24px 56px rgba(0,0,0,.16),0 0 0 1px rgba(0,0,0,.04);
  color-scheme:light
}
:root[data-theme="dark"]{
  --bg:#1e1d1b;--card:#26241f;--border:#3a3833;--fg:#ede9e3;--mut:#9a948c;
  --acc:#4c8dff;--on-acc:#ffffff;--ok:#34d399;--warn:#f0c050;--err:#e07a6a;
  --surface:#2f2d28;--surface-muted:#35332e;--btn-sec:#35332e;--btn-sec-hover:#3f3c36;
  --input-bg:#1e1d1b;--code-bg:#35332e;--code-fg:#d6d1c9;--placeholder:rgba(154,148,140,.55);
  --ok-soft:rgba(52,211,153,.12);--ok-border:rgba(52,211,153,.38);
  --warn-soft:rgba(240,192,80,.14);--warn-border:rgba(240,192,80,.38);
  --err-soft:rgba(224,122,106,.12);--err-border:rgba(224,122,106,.38);
  --mut-soft:rgba(154,148,140,.16);--mut-border:rgba(154,148,140,.32);
  --scrim:rgba(0,0,0,.55);--toast-shadow:0 10px 28px rgba(0,0,0,.55),0 0 0 1px rgba(255,255,255,.03);
  --modal-shadow:0 24px 56px rgba(0,0,0,.45),0 0 0 1px rgba(255,255,255,.04);
  --acc-ring:rgba(76,141,255,.28);
  color-scheme:dark
}
:root { --card2:var(--surface); --line:var(--border); --dim:var(--mut); --accent:var(--acc); }
  * { box-sizing: border-box; }
  body { margin:0; background:var(--bg); color:var(--fg);
         font:14px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,"PingFang SC","Microsoft YaHei",sans-serif; }
  .wrap { max-width:1100px; margin:0 auto; padding:24px 16px 60px; }
  header { display:flex; align-items:center; gap:12px; flex-wrap:wrap; margin-bottom:6px; }
  h1 { font-size:20px; margin:0; font-weight:600; }
  .badge { font-size:12px; padding:2px 8px; border-radius:999px;
           background:var(--card2); color:var(--dim); border:1px solid var(--line); }
  .sub { color:var(--dim); font-size:13px; margin-bottom:18px; }
  .bar { display:flex; gap:8px; flex-wrap:wrap; margin-bottom:14px; }
  button { font:inherit; padding:7px 13px; border-radius:8px; cursor:pointer;
           background:var(--btn-sec); color:var(--fg); border:1px solid var(--line); }
  button:hover { background:var(--btn-sec-hover); border-color:var(--accent); }
  button.primary { background:var(--accent); border-color:var(--accent); color:#fff; }
  button:disabled { opacity:.55; cursor:not-allowed; }
  button.danger:hover { border-color:var(--err); color:var(--err); }
  .card { background:var(--card); border:1px solid var(--line); border-radius:12px;
          padding:16px 18px; margin-bottom:16px; }
  .card h2 { font-size:14px; margin:0 0 12px; font-weight:600; color:var(--dim); }
  .grid { display:grid; grid-template-columns:repeat(auto-fill,minmax(250px,1fr)); gap:12px; }
  .acct { background:var(--card2); border:1px solid var(--line); border-radius:10px; padding:13px 14px; }
  .acct .top { display:flex; justify-content:space-between; gap:8px; align-items:baseline; }
  .acct .who { font-weight:600; word-break:break-all; }
  .acct .meta { color:var(--dim); font-size:12px; margin-top:3px; word-break:break-all; }
  .pill { font-size:11px; padding:2px 7px; border-radius:999px; border:1px solid var(--line); color:var(--dim); white-space:nowrap; }
  .pill.on { color:var(--ok); border-color:var(--ok); }
  .pill.off { color:var(--err); border-color:var(--err); }
  .meter { margin-top:10px; }
  .meter .lbl { display:flex; justify-content:space-between; font-size:12px; color:var(--dim); }
  .track { height:6px; border-radius:999px; background:var(--line); overflow:hidden; margin-top:4px; }
  .fill { height:100%; background:var(--accent); border-radius:999px; }
  .fill.hi { background:var(--warn); }
  .fill.max { background:var(--err); }
  table { width:100%; border-collapse:collapse; font-size:13px; }
  th,td { text-align:left; padding:7px 8px; border-bottom:1px solid var(--line); vertical-align:top; }
  th { color:var(--dim); font-weight:500; font-size:12px; }
  td.mono,.mono { font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace; font-size:12px; }
  .err { color:var(--err); }
  .ok { color:var(--ok); }
  .empty { color:var(--dim); padding:8px 0; }
  #msg { min-height:20px; font-size:13px; margin-bottom:12px; }
  details { margin-top:10px; }
  summary { cursor:pointer; color:var(--dim); font-size:13px; }
  pre { background:var(--card2); border:1px solid var(--line); border-radius:8px;
        padding:10px; overflow:auto; font-size:12px; max-height:320px; }
  input,textarea { font:inherit; background:var(--card2); color:var(--fg);
        border:1px solid var(--line); border-radius:8px; padding:7px 9px; width:100%; }
  textarea { font-family:ui-monospace,Menlo,Consolas,monospace; font-size:12px; min-height:80px; resize:vertical; }
  .row { display:flex; gap:8px; flex-wrap:wrap; align-items:flex-end; }
  .row > div { flex:1 1 190px; }
  label { display:block; font-size:12px; color:var(--dim); margin-bottom:3px; }
  .steps { margin:0; padding-left:20px; color:var(--dim); font-size:13px; }
  .steps li { margin:3px 0; }
  .status { border:1px dashed var(--line); border-radius:8px; padding:9px 11px;
            font-size:13px; margin-top:10px; }
  .toggle { display:inline-flex; align-items:center; gap:8px; color:var(--fg); cursor:pointer; white-space:nowrap; }
  .toggle input { width:18px; height:18px; margin:0; padding:0; accent-color:var(--accent); }
  input:focus-visible,textarea:focus-visible,select:focus-visible,button:focus-visible,summary:focus-visible { outline:2px solid var(--accent); outline-offset:3px; }
  .task-table { overflow-x:auto; }
  .schedule-head { display:flex; gap:16px; flex-wrap:wrap; align-items:center; margin-bottom:10px; }
  .session-controls { margin-top:12px; padding-top:10px; border-top:1px solid var(--line); }
  .session-controls input { width:76px; margin-right:6px; }
</style>
<script>
/* Early theme sync (before body paint): mirror CPA parent data-theme when
   embedded; standalone falls back to prefers-color-scheme. Same semantics as
   cpa-plugin-key-policy themeSync (DOM data-theme is source of truth). */
(function(){
  var ATTR="data-theme";
  function embedded(){try{return window.self!==window.top}catch(e){return false}}
  function apply(t){
    var el=document.documentElement;
    if(t==="white"||t==="dark")el.setAttribute(ATTR,t);
    else el.removeAttribute(ATTR);
  }
  function parentTheme(){
    if(!embedded())return null;
    try{
      var raw=window.parent.document.documentElement.getAttribute(ATTR);
      if(raw==="white"||raw==="dark")return raw;
      return "light";
    }catch(e){return null}
  }
  function systemTheme(){
    try{return window.matchMedia&&window.matchMedia("(prefers-color-scheme: dark)").matches?"dark":"light"}catch(e){return "light"}
  }
  function sync(){apply(parentTheme()||systemTheme())}
  sync();
  window.__codeartsThemeSync=function(){
    sync();
    if(!embedded()){
      try{
        var mq=window.matchMedia("(prefers-color-scheme: dark)");
        var on=function(){sync()};
        if(mq.addEventListener)mq.addEventListener("change",on);
        else if(mq.addListener)mq.addListener(on);
      }catch(e){}
      return;
    }
    try{
      var parentEl=window.parent.document.documentElement;
      new MutationObserver(sync).observe(parentEl,{attributes:true,attributeFilter:[ATTR]});
      window.addEventListener("storage",function(e){if(e.key==="cli-proxy-theme")sync()});
    }catch(e){}
  };
})();

window.__codeartsThemeSync();
</script>
</head>
<body>
<div class="wrap">
  <header>
    <h1>CodeArts 订阅</h1>
    <span class="badge">__PROVIDER__</span>
    <span class="badge" id="ver">…</span>
    <span class="badge" id="mode">…</span>
  </header>
  <div class="sub">把华为 CodeArts Doer（CodeArts Agent / 编程助手）账号授权给 CLIProxyAPI，供 Claude Code、OpenAI 兼容客户端等调用。</div>

  <div id="msg" role="status" aria-live="polite"></div>

  <div class="card">
    <h2>授权</h2>
    <label for="managementKey">CPA 管理密钥（从 CPA 主面板打开时自动获取；手动输入后本标签页记住，刷新不用再输）</label>
    <input id="managementKey" type="password" autocomplete="off" placeholder="自动获取成功时可留空">
    <div class="bar" style="margin-top:10px">
      <button class="primary" id="login">开始授权</button>
      <button id="refresh">刷新状态</button>
    </div>
    <ol class="steps">
      <li>点击「开始授权」，下方会出现登录链接。</li>
      <li>打开链接，在 CodeArts 控制台完成登录授权。</li>
      <li>本机 CPA 会自动接收回调；远程 CPA 请复制浏览器最终的 localhost 地址并在本页下方提交。请保持本页打开。</li>
    </ol>
    <div class="status" id="loginStatus">尚未开始。请先填写管理密钥。</div>
    <div id="loginLinkBox" hidden style="margin-top:8px">
      <a id="loginURL" target="_blank" rel="noopener noreferrer">点此打开华为授权页面</a>
    </div>

    <details open>
      <summary>授权页面打不开 127.0.0.1（CPA 在容器 / 远程服务器上）</summary>
      <p>OAuth 完成后会跳到一个无法打开的 <span class="mono">http://127.0.0.1:端口/oauth/callback?code=...&amp;state=...</span> 页面。这是远程部署的预期行为：把地址栏里的完整地址复制下来，粘贴到下面，CPA 会用其中的一次性 code 换取并保存凭证。</p>
      <textarea id="callbackURL" autocomplete="off" placeholder="http://127.0.0.1:40000/oauth/callback?code=...&state=..."></textarea>
      <div style="margin-top:8px"><button id="submitCallback">提交回调地址，完成授权</button></div>
      <p class="sub" style="margin:8px 0 0">authorization code 为一次性凭证，请立即提交且不要分享；无需把回调端口映射到公网。</p>
      <p class="sub" style="margin:8px 0 0">请在本页开始授权并提交回调。CPA 主界面的通用回调框无法匹配华为返回的登录状态，可能误报「请更新」。</p>
    </details>
  </div>

  <div class="card">
    <h2>账号</h2>
    <div id="accounts" class="grid"><div class="empty">加载中…</div></div>
  </div>

  <div class="card">
    <h2>定时任务</h2>
    <div class="bar">
      <button id="claimDaily" class="primary">领取每日活动积分</button>
      <button id="runAll">执行已启用任务</button>
      <button id="refreshQuota">刷新额度</button>
    </div>
    <div id="schedule"><div class="empty">加载中…</div></div>
  </div>

  <div class="card">
    <h2>诊断</h2>
    <div class="bar" style="margin:0">
      <button id="showStatus">查看运行状态</button>
      <button id="showExport">查看凭证导出（含密钥）</button>
    </div>
    <pre id="raw" hidden></pre>
  </div>
</div>

<script>
(function () {
  "use strict";
  var P = "__PROVIDER__";
  var BASE = "/v0/management/" + P;

  // Privileged calls carry the management key because the resource page itself
  // is unauthenticated. Acquisition follows the CPA panel convention shared by
  // the qoder/trae dashboards: a value typed here (remembered for the tab), the
  // a one-time ?key= parameter, the same-origin CPA main panel store when
  // embedded, then tab sessionStorage. Previous keys ("apiKey", "adminKey", …)
  // are never written by CPA, so automatic acquisition always missed.
  var PANEL_STORE = "cli-proxy-auth";
  var ENC_PREFIX = "enc::v1::";
  var SECRET_SALT = "cli-proxy-api-webui::secure-storage";
  var SS_KEY = P + "-mgmt-key";

  function encBytes(text) { return new TextEncoder().encode(text); }
  function xorBytes(data, key) {
    var out = new Uint8Array(data.length);
    for (var i = 0; i < data.length; i++) out[i] = data[i] ^ key[i % key.length];
    return out;
  }
  function base64Bytes(text) {
    var bin = window.atob(text);
    var out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  }
  function deobfuscate(value) {
    if (!value || value.indexOf(ENC_PREFIX) !== 0) return value;
    try {
      var key = encBytes(SECRET_SALT + "|" + window.location.host + "|" + window.navigator.userAgent);
      return new TextDecoder().decode(xorBytes(base64Bytes(value.slice(ENC_PREFIX.length)), key));
    } catch (e) { return value; }
  }
  function store(key, value) {
    try { window.sessionStorage.setItem(key, value); } catch (e) {}
  }
  function recall(key) {
    try { return window.sessionStorage.getItem(key); } catch (e) { return null; }
  }
  function embeddedKey() {
    try { if (window.self === window.top) return null; } catch (e) { return null; }
    var raw;
    try { raw = window.localStorage.getItem(PANEL_STORE); } catch (e) { return null; }
    if (!raw) return null;
    try {
      var parsed = JSON.parse(deobfuscate(raw));
      var state = (parsed && parsed.state) || parsed || {};
      return typeof state.managementKey === "string" && state.managementKey ? state.managementKey : null;
    } catch (e) { return null; }
  }
  function urlKey() {
    var query = new URLSearchParams(window.location.search);
    if (!query.has("key")) return null;
    var value = (query.get("key") || "").trim();
    query.delete("key");
    // Drop the secret from the address bar and browser history.
    var rest = query.toString();
    window.history.replaceState(null, "", window.location.pathname +
      (rest ? "?" + rest : "") + window.location.hash);
    return value || null;
  }

  // Consume/clean the URL once, even if a typed or cached key wins later.
  // Keep it in memory too: restricted browsers can reject sessionStorage.
  var initialURLKey = urlKey();
  function adminKey() {
    var entered = document.getElementById("managementKey").value.trim();
    if (entered) { store(SS_KEY, entered); return entered; }
    if (initialURLKey) { store(SS_KEY, initialURLKey); return initialURLKey; }
    var embedded = embeddedKey();
    if (embedded) return embedded;
    var remembered = recall(SS_KEY);
    if (remembered) return remembered;
    return "";
  }

  function headers() {
    var h = { "Content-Type": "application/json" };
    var k = adminKey();
    if (k) { h["Authorization"] = "Bearer " + k; h["X-API-Key"] = k; }
    return h;
  }

  function say(text, kind) {
    var el = document.getElementById("msg");
    el.textContent = text || "";
    el.className = kind || "";
  }

  function call(path, opts) {
    opts = opts || {};
    var controller = null, timer = null;
    if (opts.timeout) {
      if (!window.AbortController) return Promise.reject(new Error("浏览器不支持可取消请求，请更新浏览器"));
      controller = new window.AbortController();
      timer = setTimeout(function () { controller.abort(); }, opts.timeout);
    }
    return fetch(path, {
      method: opts.method || "GET",
      headers: headers(),
      signal: controller ? controller.signal : undefined,
      body: opts.body ? JSON.stringify(opts.body) : undefined
    }).then(function (r) {
      return r.text().then(function (t) {
        var data = null;
        try { data = t ? JSON.parse(t) : null; } catch (e) { data = { raw: t }; }
        if (!r.ok) {
          var m = (data && (data.error || data.message)) || ("HTTP " + r.status);
          throw new Error(m);
        }
        return data;
      });
    }).finally(function () { if (timer !== null) clearTimeout(timer); });
  }

  function esc(s) {
    return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }

  function sessionConcurrencyBlock(a) {
    var c = a.concurrency || { limit: 3, default: 3, override: 0, active: 0 };
    return '<div class="session-controls"><label>会话并发上限（0 继承默认 ' + esc(c.default) + '）' +
      '<input type="number" min="0" max="64" step="1" value="' + esc(c.override) + '" aria-label="会话并发上限"></label>' +
      '<button data-concurrency-save="' + esc(a.auth_index) + '">保存并发上限</button>' +
      '<div class="meta" data-concurrency-status role="status">' +
      (c.error ? esc(c.error) : '本插件占用 ' + esc(c.active) + ' / ' + esc(c.limit) + '；按订阅设置，例如 3 或 5。') +
      '</div></div>';
  }

  function saveSessionConcurrency(button, authIndex) {
    var input = button.parentNode.querySelector('input');
    var status = button.parentNode.querySelector('[data-concurrency-status]');
    var value = Number(input.value);
    if (input.value.trim() === '' || !Number.isInteger(value) || value < 0 || value > 64) {
      status.textContent = '请输入 0–64 的整数；0 表示继承默认值。';
      return Promise.resolve();
    }
    button.disabled = true; input.disabled = true;
    status.textContent = '正在保存…';
    return call(BASE + '/concurrency', { method: 'POST', body: { auth_index: authIndex, limit: value } })
      .then(function (r) {
        var c = r.concurrency;
        input.value = String(c.override);
        status.textContent = '已保存，当前占用 ' + c.active + ' / ' + c.limit + '。重启保留，不中断已有会话。';
      }).catch(function (e) { status.textContent = '保存失败：' + e.message; })
      .finally(function () { button.disabled = false; input.disabled = false; });
  }

  function tokenCount(n) {
    var v = Number(n);
    if (!isFinite(v)) return "?";
    if (Math.abs(v) >= 1e8) return (v / 1e8).toFixed(2) + " 亿";
    if (Math.abs(v) >= 1e4) return (v / 1e4).toFixed(1) + " 万";
    return String(Math.round(v));
  }

  function creditCount(n) {
    var v = Number(n);
    return isFinite(v) ? v.toLocaleString('zh-CN', { maximumFractionDigits: 2 }) : '?';
  }

  // The benefit pool is a separate upstream account from the meters above, and
  // an uncapped dimension must not be drawn as an empty bar.
  function benefitBlock(a) {
    var b = a.benefit;
    if (!b) {
      return a.benefit_error ? '<div class="meta err">福利额度查询失败：' + esc(a.benefit_error) + '</div>' : "";
    }
    var limit = Number(b.daily_token_limit);
    var used = Number(b.daily_tokens_used);
    var capped = limit > 0;
    var out = capped ? meter("福利每日 token", Math.min(used / limit * 100, 100)) : "";
    var bits = [];
    if (capped) {
      var left = limit - used;
      bits.push("今日 " + tokenCount(used) + " / " + tokenCount(limit) +
        (left <= 0 ? "（今日已用尽，次日重新领取后恢复）" : "（剩 " + tokenCount(left) + "）"));
    } else {
      bits.push("今日已用 " + tokenCount(used) + "（无日上限）");
    }
    var monthLimit = Number(b.monthly_token_limit);
    bits.push(monthLimit > 0
      ? "本月 " + tokenCount(b.monthly_tokens_used) + " / " + tokenCount(monthLimit)
      : "本月累计 " + tokenCount(b.monthly_tokens_used) + "（无月上限）");
    out += '<div class="meta">' + bits.join(" · ") + '</div>';
    if (a.benefit_error) out += '<div class="meta err">福利额度刷新失败：' + esc(a.benefit_error) + '</div>';
    return out;
  }

  // Upstream decides which usage rows exist, so render what it reports instead of
  // two hard-coded names: a renamed metric then still shows up rather than
  // silently disappearing from the card.
  function meterRows(a) {
    return (a.meters || []).map(function (m) {
      var label = m.label || m.name;
      var has = m.used_percent !== undefined && m.used_percent !== null;
      var out = has ? meter(label, Number(m.used_percent)) : "";
      var bits = [];
      if (!out) bits.push(label + "：上游未给出百分比");
      var used = Number(m.used_tokens) || 0, allow = Number(m.allowance_tokens) || 0;
      // An allowance below the usage is an upstream sentinel rather than a real
      // cap (a trial row reports 1 token against 1408 used), so it is not drawn
      // as a fraction the account has somehow exceeded.
      if (allow > 0 && used <= allow) bits.push("token " + tokenCount(used) + " / " + tokenCount(allow));
      else if (used > 0) bits.push("已用 " + tokenCount(used) + " token");
      if (m.credit_total != null) bits.push('积分总额 ' + creditCount(m.credit_total));
      if (m.credit_used != null) bits.push('已用积分 ' + creditCount(m.credit_used));
      if (m.credit_remaining != null) bits.push('剩余积分 ' + creditCount(m.credit_remaining));
      return out + (bits.length ? '<div class="meta">' + bits.map(esc).join(" · ") + '</div>' : "");
    }).join("");
  }

  function meter(label, pct) {
    if (pct === null || pct === undefined || pct < 0) return "";
    var v = Number(pct);
    if (!isFinite(v)) return "";
    var cls = v >= 90 ? "max" : (v >= 70 ? "hi" : "");
    return '<div class="meter"><div class="lbl"><span>' + esc(label) + '</span><span>已用 ' +
      v.toFixed(1) + '%</span></div><div class="track"><div class="fill ' + cls +
      '" style="width:' + Math.max(0, Math.min(100, v)) + '%"></div></div></div>';
  }

  function renderAccounts(list) {
    var host = document.getElementById("accounts");
    if (!list || !list.length) {
      host.innerHTML = '<div class="empty">还没有账号。点击上方「开始授权」完成登录。</div>';
      return;
    }
    host.innerHTML = list.map(function (a) {
      var pill = a.disabled ? '<span class="pill off">已停用</span>'
                            : '<span class="pill on">可用</span>';
      var head = esc(a.user_name || a.label || a.name || a.auth_index);
      var bits = [];
      if (a.plan || a.plan_name) bits.push("套餐：" + esc(a.plan_name || a.plan));
      if (a.login_type) bits.push("登录方式：" + esc(a.login_type));
      if (a.expires_at) bits.push("凭证到期：" + esc(a.expires_at));
      if (a.reset_date) bits.push("额度重置：" + esc(a.reset_date));
      return '<div class="acct"><div class="top"><span class="who">' + head + '</span>' + pill + '</div>' +
        (bits.length ? '<div class="meta">' + bits.join(" · ") + '</div>' : '') +
        meterRows(a) +
        '<div data-benefit-result="' + esc(a.auth_index) + '">' + benefitBlock(a) + '</div>' +
        '<div style="margin-top:9px"><button data-benefit="' + esc(a.auth_index) + '">刷新福利额度</button></div>' +
        (a.quota_error ? '<div class="meta err">额度查询失败：' + esc(a.quota_error) + '</div>' : '') +
        '<div style="margin-top:9px"><button data-models="' + esc(a.auth_index) + '">刷新并同步模型</button></div>' +
        '<div data-model-list="' + esc(a.auth_index) + '" class="meta">模型列表从此账号的华为服务获取。</div>' +
        '<div class="meta mono">' + esc(a.auth_index) + '</div>' +
        sessionConcurrencyBlock(a) +
        '<div style="margin-top:9px"><button class="danger" data-del="' + esc(a.auth_index) + '">删除账号</button></div>' +
        '</div>';
    }).join("");
  }

  function renderSchedule(s) {
    var host = document.getElementById("schedule");
    var tasks = (s && s.tasks) || [];
    s = s || {};
    var rows = tasks.map(function (t) {
      var state = t.running ? '执行中…' : t.last_error ? '<span class="err">' + esc(t.last_error) + '</span>'
                  : (t.last_result ? '<span class="ok">' + esc(t.last_result) + '</span>' : "—");
      return '<tr><td class="mono">' + esc(t.id) + '</td><td class="mono">' + esc(t.type) + '</td>' +
        '<td class="mono">' + esc(t.cron) + '</td>' +
        '<td><label class="toggle"><input type="checkbox" data-task-toggle="' + esc(t.id) + '"' +
        (t.enabled !== false ? ' checked' : '') + ' aria-label="自动执行 ' + esc(t.id) + '">自动</label></td>' +
        '<td class="mono">' + esc(t.next_run || "—") + '</td>' +
        '<td class="mono">' + esc(t.last_run || "—") + '</td>' +
        '<td>' + state + '</td>' +
        '<td><button data-run="' + esc(t.id) + '" data-enabled="' + (t.enabled !== false) + '"' +
        (t.running ? ' disabled' : '') + '>执行一次</button></td></tr>';
    }).join("");
    host.innerHTML = '<div class="schedule-head"><label class="toggle"><input type="checkbox" data-schedule-toggle' +
      (s.enabled ? ' checked' : '') + '>定时任务总开关</label><span class="sub" style="margin:0">' +
      (s.enabled ? '自动调度已开启' : '自动调度已暂停') + ' · 时区 ' + esc(s.timezone || "本机时区") + '</span></div>' +
      '<div class="sub">开关保存后重启仍保留。总开关只控制自动执行，手动按钮始终可用；已开始的任务会执行完毕。<br>' +
      '每日活动按北京时间检查：只领取官方标记可领的登录积分活动，完成领取、确认并复查官方状态后才记成功。积分显示在账号的套餐/赠送积分，不是福利模型 token 池。</div>' +
      (s.pending ? '<div class="sub">正在等待账号目录，自动调度暂未启动；请先添加账号。</div>' : '') +
      (s.persistence_error ? '<div class="err">开关恢复失败：' + esc(s.persistence_error) + '，已暂停自动调度。</div>' : '') +
      '<div class="task-table"><table><thead><tr><th>任务</th><th>类型</th><th>Cron</th><th>自动执行</th>' +
      '<th>下次执行</th><th>上次执行</th><th>结果</th><th></th></tr></thead><tbody>' +
      rows + '</tbody></table></div>';
  }

  var scheduleSaving = false, scheduleRevision = 0;
  function saveSchedule(change) {
    if (scheduleSaving) return Promise.resolve();
    scheduleSaving = true;
    scheduleRevision++;
    var controls = document.querySelectorAll('[data-schedule-toggle],[data-task-toggle]');
    for (var i = 0; i < controls.length; i++) controls[i].disabled = true;
    say('正在保存开关…');
    return call(BASE + '/schedule/config', { method: 'POST', body: change })
      .then(function (s) { renderSchedule(s); say(s.note || '开关已保存，重启后保留。', 'ok'); })
      .catch(function (e) {
        // Re-read authoritative state, including a save whose response was lost.
        return call(BASE + '/schedule').then(renderSchedule).catch(function () {
          for (var i = 0; i < controls.length; i++) controls[i].disabled = true;
        }).then(function () { say('开关保存失败：' + e.message + '；请刷新确认当前状态。', 'err'); });
      }).finally(function () { scheduleSaving = false; });
  }

  document.addEventListener('change', function (ev) {
    var t = ev.target;
    if (t.hasAttribute('data-schedule-toggle')) saveSchedule({ enabled: t.checked });
    else if (t.hasAttribute('data-task-toggle')) saveSchedule({ tasks: [{ id: t.getAttribute('data-task-toggle'), enabled: t.checked }] });
  });

  function load() {
    var revision = scheduleRevision;
    say("加载中…");
    Promise.all([
      call(BASE + "/accounts"),
      call(BASE + "/schedule")
    ]).then(function (out) {
      renderAccounts(out[0] && out[0].accounts);
      if (!scheduleSaving && revision === scheduleRevision) { renderSchedule(out[1]); say(""); }
    }).catch(function (e) {
      say("加载失败：" + e.message + "（管理接口需要 CPA 管理密钥，请填写后重试）", "err");
    });
  }

  function loadMeta() {
    // Public status resource: no admin key needed.
    fetch("/v0/resource/plugins/" + P + "/status").then(function (r) { return r.json(); })
      .then(function (d) {
        var p = (d && d.plugin) || {};
        document.getElementById("ver").textContent = "v" + (p.version || "?");
        var e = (d && d.endpoint) || {};
        document.getElementById("mode").textContent = (e.protocol_mode || "?") + " · " + (e.base_url || "");
      }).catch(function () {});
  }

  document.getElementById("refresh").onclick = load;

  var loginState = "", loginTimer = null, loginDeadline = 0;
  function setLoginStatus(text, kind) {
    var el = document.getElementById("loginStatus");
    el.textContent = text;
    el.className = "status" + (kind ? " " + kind : "");
  }
  function pollLogin() {
    loginTimer = null;
    if (!loginState) return;
    var polledState = loginState;
    if (Date.now() > loginDeadline) {
      setLoginStatus("授权已超时，请重新点击「开始授权」。", "err");
      loginState = ""; return;
    }
    call("/v0/management/get-auth-status?state=" + encodeURIComponent(loginState)).then(function (r) {
      if (loginState !== polledState) return;
      if (r.status === "ok") {
        setLoginStatus("授权成功，账号已写入 CPA。", "ok");
        document.getElementById("callbackURL").value = "";
        loginState = ""; load(); return;
      }
      if (r.status === "error") {
        setLoginStatus("授权失败：" + (r.error || r.message || "未知原因"), "err");
        loginState = ""; return;
      }
      // "wait" carries no detail, so the plugin's own poll route explains which
      // stage the flow is in (waiting for the browser, or exchanging the code).
      call(BASE + "/login/status?state=" + encodeURIComponent(loginState)).then(function (d) {
        if (loginState === polledState && d && d.message) setLoginStatus(d.message);
      }).catch(function () {});
      loginTimer = setTimeout(pollLogin, 2000);
    }).catch(function (e) {
      if (loginState !== polledState) return;
      setLoginStatus("查询授权状态失败：" + e.message, "err");
      loginTimer = setTimeout(pollLogin, 2000);
    });
  }
  document.getElementById("login").onclick = function () {
    clearTimeout(loginTimer);
    loginTimer = null; loginState = "";
    document.getElementById("callbackURL").value = "";
    setLoginStatus("正在创建授权链接…");
    call("/v0/management/" + P + "-auth-url").then(function (r) {
      loginState = r.state; loginDeadline = Date.now() + 300000;
      var link = document.getElementById("loginURL");
      link.href = r.url;
      document.getElementById("loginLinkBox").hidden = false;
      window.open(r.url, "_blank", "noopener");
      setLoginStatus("已打开授权页面。若浏览器没有自动打开，请点击下方链接。");
      pollLogin();
    }).catch(function (e) { setLoginStatus("创建授权链接失败：" + e.message, "err"); });
  };
  document.getElementById("submitCallback").onclick = function () {
    if (!loginState) { say("请先点击「开始授权」。", "err"); return; }
    var raw = document.getElementById("callbackURL").value.trim();
    if (!raw) { say("请粘贴浏览器地址栏中的回调地址。", "err"); return; }
    call(BASE + "/login/callback", { method: "POST", body: { state: loginState, callback_url: raw } })
      .then(function () {
        document.getElementById("callbackURL").value = "";
        setLoginStatus("已收到回调地址，正在换取凭证…");
        // The existing polling loop keeps running, including transient errors.
        // Do not start another poll while the current request is still in flight.
      })
      .catch(function (e) { say("提交回调地址失败：" + e.message, "err"); });
  };

  document.getElementById("refreshQuota").onclick = function () {
    say("正在刷新额度…");
    call(BASE + "/quota/refresh", { method: "POST", body: {} })
      .then(function (r) {
        say("额度已刷新：成功 " + (r.refreshed || 0) + " 个账号" +
            (r.failed ? "，失败 " + r.failed + " 个" : ""), r.failed ? "err" : "");
        setTimeout(load, 400);
      }).catch(function (e) { say("刷新额度失败：" + e.message, "err"); });
  };

  document.getElementById("runAll").onclick = function () {
    var btns = document.querySelectorAll('[data-run][data-enabled="true"]:not(:disabled)');
    if (!btns.length) { say("没有可执行的任务。", "err"); return; }
    var ids = [];
    for (var i = 0; i < btns.length; i++) ids.push(btns[i].getAttribute("data-run"));
    say("正在执行 " + ids.length + " 个任务…");
    Promise.all(ids.map(function (id) {
      return call(BASE + "/schedule/run", { method: "POST", body: { task: id } });
    })).then(function () {
      say("任务已启动，正在刷新状态…");
      setTimeout(load, 1200);
    }).catch(function (e) { say('部分任务可能已启动，其他任务启动失败：' + e.message, 'err'); });
  };

  document.getElementById('claimDaily').onclick = function () {
    var button = this;
    button.disabled = true;
    say('正在启动每日领取…');
    call(BASE + '/checkin', { method: 'POST', body: { task: 'daily-benefit-claim' } })
      .then(function () {
        say('每日活动领取已启动；将完成领取、确认和状态复查，结果见下方任务记录。');
        setTimeout(load, 1500);
      }).catch(function (e) { say('启动领取失败：' + e.message, 'err'); })
      .finally(function () { button.disabled = false; });
  };

  document.addEventListener("click", function (ev) {
    var t = ev.target;
    if (!t || t.tagName !== "BUTTON") return;
    var concurrencyAccount = t.getAttribute('data-concurrency-save');
    if (concurrencyAccount) { saveSessionConcurrency(t, concurrencyAccount); return; }
    var benefitAccount = t.getAttribute("data-benefit");
    if (benefitAccount) {
      var benefitBox = t.parentNode.previousElementSibling;
      t.disabled = true;
      benefitBox.textContent = "正在查询福利额度（最多等待 5 秒）…";
      call(BASE + "/benefit-balance?auth_index=" + encodeURIComponent(benefitAccount), { timeout: 5000 })
        .then(function (data) { benefitBox.innerHTML = benefitBlock(data); })
        .catch(function (e) {
          benefitBox.textContent = e.name === "AbortError" ? "福利查询超时，可重试；套餐额度不受影响。" : "福利查询失败：" + e.message;
        })
        .finally(function () { t.disabled = false; });
      return;
    }
    var modelAccount = t.getAttribute("data-models");
    if (modelAccount) {
      var modelBox = t.parentNode.nextElementSibling;
      t.disabled = true;
      modelBox.textContent = "正在获取模型…";
      call(BASE + "/models?include_benefit=true&auth_index=" + encodeURIComponent(modelAccount), { timeout: 10000 }).then(function (catalog) {
        // Reconfigure without changing any fields: CPA re-registers saved account
        // catalogues. Never rewrite credentials to force discovery notifications.
        return call('/v0/management/plugins/' + P + '/config', { method: 'PATCH', body: {} }).then(function () { return catalog; });
      }).then(function (catalog) {
        var items = (catalog && catalog.models) || [];
        var warnings = (catalog && catalog.warnings) || [];
        var source = catalog && catalog.source;
        var note = source === "configured" ? "配置中的模型（本次未从账号发现）" : "已刷新并请求 CPA 同步的账号模型";
        modelBox.innerHTML = '<div>' + esc(note) + '：' + items.length + ' 个</div>' +
          items.map(function (m) {
            var origin = m.source === "benefit" ? "福利网关" : (m.source === "agent" ? "CodeArts" : "手动配置");
            return '<div style="margin-top:5px">' + esc(m.display_name || m.id) + ' · ' + esc(origin) +
              '<div class="mono">' + esc(m.id) + '</div></div>';
          }).join("") +
          (!items.length ? '<div>暂无可用模型，请检查授权和下方发现结果。</div>' : '') +
          warnings.map(function (warning) { return '<div class="err">' + esc(warning) + '</div>'; }).join("");
      }).catch(function (e) { modelBox.textContent = "模型获取失败：" + e.message; })
        .finally(function () { t.disabled = false; });
      return;
    }
    var del = t.getAttribute("data-del");
    var run = t.getAttribute("data-run");
    if (del) {
      if (!window.confirm("确定删除该账号及其凭证文件？\n\n" + del)) return;
      t.disabled = true;
      call(BASE + "/delete", { method: "POST", body: { auth_index: del } })
        .then(function () { say("账号已删除。"); load(); })
        .catch(function (e) { say("删除失败：" + e.message, "err"); t.disabled = false; });
    } else if (run) {
      t.disabled = true;
      call(BASE + "/schedule/run", { method: "POST", body: { task: run } })
        .then(function () { say("任务 " + run + " 已启动。"); setTimeout(load, 1200); })
        .catch(function (e) { say("执行失败：" + e.message, "err"); t.disabled = false; });
    }
  });

  document.getElementById("showStatus").onclick = function () {
    fetch("/v0/resource/plugins/" + P + "/status").then(function (r) { return r.text(); })
      .then(function (t) {
        var pre = document.getElementById("raw");
        pre.hidden = false; pre.textContent = t;
      });
  };

  document.getElementById("showExport").onclick = function () {
    say("正在获取导出内容…");
    call(BASE + "/export").then(function (d) {
      var pre = document.getElementById("raw");
      pre.hidden = false; pre.textContent = JSON.stringify(d, null, 2);
      say("导出内容包含真实密钥，请谨慎处理。", "err");
    }).catch(function (e) { say("导出失败：" + e.message, "err"); });
  };

  loadMeta();
  load();
})();
</script>
</body>
</html>
`
