const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

// Execute the actual shipped functions, not a second implementation of them.
const source = fs.readFileSync(path.join(__dirname, '..', 'panel.go'), 'utf8');
const functions = source.slice(source.indexOf('  var P = "__PROVIDER__";'), source.indexOf('  function esc('));
assert(functions.includes('function adminKey()'));

function panel({ query = '', stored = '', embedded = '', typed = '', blockedStorage = false, fetchImpl } = {}) {
  const storage = new Map([['__PROVIDER__-mgmt-key', stored]]);
  const location = { search: query, pathname: '/panel', hash: '#account', host: 'fixture.invalid' };
  const replacements = [];
  const timers = new Set();
  const window = {
    location, navigator: { userAgent: 'fixture-browser' }, AbortController,
    atob: value => Buffer.from(value, 'base64').toString('binary'),
    history: { replaceState(_state, _title, url) { replacements.push(url); } },
    sessionStorage: {
      getItem(key) { if (blockedStorage) throw Error('blocked'); return storage.get(key); },
      setItem(key, value) { if (blockedStorage) throw Error('blocked'); storage.set(key, value); },
    },
    localStorage: { getItem() { return JSON.stringify({ state: { managementKey: embedded } }); } },
  };
  window.self = window;
  window.top = embedded ? {} : window;
  const context = vm.createContext({
    window, URLSearchParams, TextEncoder, TextDecoder, Uint8Array,
    document: { getElementById() { return { value: typed }; } },
    fetch: fetchImpl || (() => Promise.resolve({ ok: true, text: () => Promise.resolve('{}') })),
    setTimeout(fn, ms) { const id = setTimeout(fn, ms); timers.add(id); return id; },
    clearTimeout(id) { timers.delete(id); clearTimeout(id); },
  });
  vm.runInContext(functions, context);
  return { context, replacements, storage, timers };
}

test('a URL key overrides cached and embedded keys and is removed immediately', () => {
  const p = panel({ query: '?key=fixture-new&mode=dark&key=duplicate', stored: 'fixture-old', embedded: 'fixture-embedded' });
  assert.deepEqual(p.replacements, ['/panel?mode=dark#account']);
  assert.equal(p.context.adminKey(), 'fixture-new');
  assert.equal(p.context.adminKey(), 'fixture-new');
  assert.equal(p.storage.get('__PROVIDER__-mgmt-key'), 'fixture-new');
});

test('typed key wins, but does not prevent URL cleanup', () => {
  const p = panel({ query: '?key=fixture-url', typed: 'fixture-typed' });
  assert.deepEqual(p.replacements, ['/panel#account']);
  assert.equal(p.context.adminKey(), 'fixture-typed');
});

test('a rotated embedded CPA key overrides stale sessionStorage', () => {
  const p = panel({ stored: 'fixture-old', embedded: 'fixture-current' });
  assert.equal(p.context.adminKey(), 'fixture-current');
});

test('URL key survives repeated calls when sessionStorage is blocked', () => {
  const p = panel({ query: '?key=fixture%2Bencoded+space', blockedStorage: true });
  assert.equal(p.context.adminKey(), 'fixture+encoded space');
  assert.equal(p.context.adminKey(), 'fixture+encoded space');
  assert.equal(p.replacements.length, 1);
});

test('an empty URL key is cleaned, then falls back to remembered key', () => {
  const p = panel({ query: '?key=&mode=dark', stored: 'fixture-old' });
  assert.deepEqual(p.replacements, ['/panel?mode=dark#account']);
  assert.equal(p.context.adminKey(), 'fixture-old');
});

test('optional balance request aborts and releases its deadline timer', async () => {
  let aborted = false;
  const p = panel({ fetchImpl: (_url, opts) => new Promise((_resolve, reject) => {
    assert(opts.signal);
    opts.signal.addEventListener('abort', () => {
      aborted = true;
      const error = new Error('aborted');
      error.name = 'AbortError';
      reject(error);
    });
  }) });
  await assert.rejects(p.context.call('/benefit-balance', { timeout: 10 }), { name: 'AbortError' });
  assert(aborted);
  assert.equal(p.timers.size, 0);
});

test('successful optional request cancels its timer', async () => {
  const p = panel();
  await p.context.call('/benefit-balance', { timeout: 5000 });
  assert.equal(p.timers.size, 0);
});

test('the complete dashboard script remains syntactically valid', () => {
  new vm.Script(source.slice(source.indexOf('<script>') + '<script>'.length, source.indexOf('</script>')));
});

function schedulePanel(callImpl) {
  const elements = { schedule: { innerHTML: '' }, claimDaily: {}, runAll: {} };
  const controls = [{ disabled: false }, { disabled: false }];
  let message;
  const context = vm.createContext({
    document: { getElementById: id => elements[id], querySelectorAll: () => controls, addEventListener() {} },
    BASE: '/management', call: callImpl, say: text => { message = text; },
    setTimeout() {}, load() {},
  });
  vm.runInContext(source.slice(source.indexOf('  function esc('), source.indexOf('  function tokenCount(')) +
    source.slice(source.indexOf('  function renderSchedule('), source.indexOf('  function load()')), context);
  return { context, elements, controls, message: () => message };
}

test('schedule renders total and individual switches, escaped IDs, and no automatic claim by default', () => {
  const p = schedulePanel();
  p.context.renderSchedule({ enabled: false, tasks: [{ id: 'daily-benefit-claim', enabled: false }, { id: '<unsafe>"', enabled: true }] });
  const html = p.elements.schedule.innerHTML;
  assert.match(html, /data-schedule-toggle>/);
  assert.match(html, /data-task-toggle="daily-benefit-claim" aria-label/);
  assert.match(html, /data-run="daily-benefit-claim" data-enabled="false"/);
  assert(!html.includes('<unsafe>'));
  p.context.renderSchedule({ enabled: false, tasks: [] });
  assert.match(p.elements.schedule.innerHTML, /定时任务总开关/);
});

test('switch saves serialize and apply the authoritative response', async () => {
  let resolve, calls = 0;
  const p = schedulePanel((url, opts) => {
    calls++;
    assert.equal(url, '/management/schedule/config');
    assert.equal(opts.body.enabled, false);
    return new Promise(r => { resolve = r; });
  });
  const saving = p.context.saveSchedule({ enabled: false });
  assert(p.controls.every(c => c.disabled));
  await p.context.saveSchedule({ enabled: true });
  assert.equal(calls, 1);
  resolve({ enabled: false, persistent: true, tasks: [], note: 'saved' });
  await saving;
  assert.equal(p.context.scheduleSaving, false);
  assert.equal(p.message(), 'saved');
});

test('failed save reloads server flags instead of presenting a false saved state', async () => {
  const p = schedulePanel(url => url.endsWith('/config') ? Promise.reject(Error('read only')) : Promise.resolve({ enabled: true, tasks: [] }));
  await p.context.saveSchedule({ enabled: false });
  assert.match(p.elements.schedule.innerHTML, /data-schedule-toggle checked/);
  assert.match(p.message(), /read only/);
});

test('manual claim names the built-in task and bulk run excludes disabled tasks', async () => {
  const calls = [];
  const p = schedulePanel((url, opts) => { calls.push({url, opts}); return Promise.resolve({}); });
  vm.runInContext(source.slice(source.indexOf("  document.getElementById('claimDaily').onclick"), source.indexOf('  document.addEventListener("click"')), p.context);
  p.elements.claimDaily.onclick();
  assert.equal(calls[0].opts.body.task, 'daily-benefit-claim');
  assert.equal(calls[0].url, '/management/checkin');
  assert(source.includes('[data-run][data-enabled="true"]:not(:disabled)'));
  assert(source.includes('领取每日活动积分'));
  assert(source.includes('/models?include_benefit=true&auth_index='));
  assert(source.includes("call('/v0/management/plugins/' + P + '/config', { method: 'PATCH', body: {} })"));
});

test('credit balances preserve decimal credits and do not label them as tokens', () => {
  const context = vm.createContext({});
  vm.runInContext(source.slice(source.indexOf('  function creditCount('),source.indexOf('  // The benefit pool')),context);
  assert.equal(context.creditCount(4999.21),'4,999.21');
  assert(source.includes("'剩余积分 ' + creditCount(m.credit_remaining)"));
});

function concurrencyPanel(callImpl) {
  const input = { value: '5', disabled: false };
  const status = { textContent: '' };
  const button = { disabled: false, parentNode: { querySelector: selector => selector === 'input' ? input : status } };
  const context = vm.createContext({ BASE: '/management', call: callImpl });
  vm.runInContext(source.slice(source.indexOf('  function esc('), source.indexOf('  function tokenCount(')), context);
  return { input, status, button, context };
}

test('concurrency controls render inherited/default limits, occupancy and escaped account ID', () => {
  const p = concurrencyPanel();
  const html = p.context.sessionConcurrencyBlock({ auth_index: '<account"', concurrency: { limit: 5, default: 3, override: 5, active: 2 } });
  assert.match(html, /value="5"/);
  assert.match(html, /0 继承默认 3/);
  assert.match(html, /本插件占用 2 \/ 5/);
  assert.match(html, /data-concurrency-save="&lt;account&quot;"/);
  assert(source.includes('if (concurrencyAccount) { saveSessionConcurrency(t, concurrencyAccount); return; }'));
});

test('concurrency save validates integers and restores controls after persistent save', async () => {
  let resolve, calls = 0;
  const p = concurrencyPanel((url, opts) => {
    calls++;
    assert.equal(url, '/management/concurrency');
    assert.equal(opts.method, 'POST');
    assert.equal(opts.body.auth_index, 'account');
    assert.equal(opts.body.limit, 5);
    return new Promise(r => { resolve = r; });
  });
  for (const value of ['', '-1', '65', '3.5', 'abc']) {
    p.input.value = value;
    await p.context.saveSessionConcurrency(p.button, 'account');
    assert.equal(calls, 0);
    assert.match(p.status.textContent, /请输入/);
  }
  p.input.value = '5';
  const saving = p.context.saveSessionConcurrency(p.button, 'account');
  assert(p.button.disabled && p.input.disabled);
  resolve({ persistent: true, concurrency: { limit: 5, override: 5, active: 2 } });
  await saving;
  assert(!p.button.disabled && !p.input.disabled);
  assert.match(p.status.textContent, /已保存，当前占用 2 \/ 5/);
});

test('concurrency zero restores inheritance and failed save does not claim success', async () => {
  const p = concurrencyPanel((_url, opts) => {
    assert.equal(opts.body.limit, 0);
    return Promise.reject(Error('read only'));
  });
  p.input.value = '0';
  await p.context.saveSessionConcurrency(p.button, 'account');
  assert.match(p.status.textContent, /保存失败：read only/);
  assert(!p.button.disabled && !p.input.disabled);
});
