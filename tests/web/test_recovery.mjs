import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

const source = fs.readFileSync(new URL('../../internal/server/web/app.js', import.meta.url), 'utf8');
const settle = () => new Promise(resolve => setImmediate(resolve));
const file = (name, size = 12, lastModified = 100) => ({name, size, lastModified, contents: 'PRIVATE FILE CONTENTS'});
const owner = id => ({id, username: `owner-${id}`, used: 0, quota: 1000});
const storageKey = id => `pebril-upload-failures-${id}`;
const record = (name, key = name, id = null) => ({key, name, size: 12, lastModified: 100, id});
const response = (value, status = 200) => ({ok: status >= 200 && status < 300, status, json: async () => value});

class Element {
  constructor() { this.children = []; this.listeners = {}; this.files = []; this.disabled = false; this.value = ''; this._text = ''; }
  get textContent() { return this._text + this.children.map(child => typeof child === 'string' ? child : child.textContent).join(''); }
  set textContent(value) { this._text = String(value); this.children = []; }
  addEventListener(name, callback) { this.listeners[name] = callback; }
  append(...values) { this.children.push(...values); }
  replaceChildren(...values) { this._text = ''; this.children = values; }
  setAttribute(name, value) { this[name] = value; }
  removeAttribute(name) { delete this[name]; }
  reset() { this.value = ''; }
}

async function browser({saved = new Map(), user = owner(1), storageError = false} = {}) {
  const elements = new Map();
  const select = selector => {
    if (!elements.has(selector)) elements.set(selector, new Element());
    return elements.get(selector);
  };
  const state = {user, saved, sent: [], fetches: [], uploadReplies: [], clipboard: '', poll: async () => response({id: 'pending', state: 'queued'})};
  const timers = [];
  const context = vm.createContext({
    document: {addEventListener() {}, querySelector: select, createElement: () => new Element()},
    setInterval() {}, setTimeout(callback, delay) { timers.push({callback, delay}); },
    URL, location: {href: 'https://example.invalid/'}, alert() {}, confirm: () => true,
    navigator: {clipboard: {async writeText(value) { state.clipboard = value; }}},
    localStorage: {
      getItem(key) { if (storageError) throw new Error('Storage denied'); return saved.get(key); },
      setItem(key, value) { if (storageError) throw new Error('Storage denied'); saved.set(key, value); }
    },
    FormData: class { append(key, value) { if (key === 'file') this.file = value; } },
    XMLHttpRequest: class {
      constructor() { this.upload = {}; this.headers = {}; }
      open(method, path) { this.method = method; this.path = path; }
      setRequestHeader(name, value) { this.headers[name] = value; }
      abort() { this.aborted = true; this.onabort?.(); }
      send(body) {
        state.sent.push({file: body.file, request: this});
        this.upload.onprogress({lengthComputable: true, loaded: body.file.size, total: body.file.size});
        const reply = state.uploadReplies.shift();
        assert(reply, `Unexpected upload: ${body.file.name}`);
        if (typeof reply === 'function') { reply(this); return; }
        queueMicrotask(() => {
          if (reply.network) { this.onerror(); return; }
          this.status = reply.status ?? 201;
          this.responseText = reply.raw ?? JSON.stringify(reply.body);
          this.onload();
        });
      }
    },
    async fetch(path, options = {}) {
      state.fetches.push({path, options});
      if (path.endsWith('/status')) return response({version: 'test', config_revision: 1, sqlite: true, native: {svg: true, gif: true}});
      if (path.endsWith('/account')) return response({initialized: true, user: state.user, csrf: 'PRIVATE CSRF TOKEN'});
      if (path.endsWith('/logout')) { if (state.logout) return state.logout(); state.user = null; return response({}); }
      if (path.includes('/images?')) return response({images: [], total: 0});
      if (path.includes('/uploads/')) return state.poll(path, options);
      throw new Error(`Unexpected fetch: ${path}`);
    }
  });
  vm.runInContext(source, context);
  await settle();
  return {
    state, context, select, timers,
    async upload(files, replies) {
      state.uploadReplies.push(...replies); select('#files').files = files;
      const pending = select('#upload').listeners.click(); await settle(); return {pending};
    },
    async tick() {
      const timer = timers.shift(); assert(timer, 'Expected a scheduled wait');
      assert.equal(timer.delay, 1000); timer.callback(); await settle();
    },
    async account(user) { state.user = user; await context.loadAccount(); await settle(); },
    records(id = state.user?.id) { return JSON.parse(saved.get(storageKey(id)) || '[]'); }
  };
}
const completed = (id, url = `/image/${id}`) => ({body: {id, state: 'completed', url}});
const names = rows => rows.map(row => row.children[0].textContent);
const retryControls = row => ({input: row.children[3].children[0].children[0], button: row.children[3].children[1], message: row.children[3].children[2]});

// Persistent metadata survives page closure, account switches and explicit logout.
const saved = new Map([[storageKey(1), JSON.stringify([record('old.png'), {...record('old.png', 'legacy-independent-record'), lastModified: 200}])], [storageKey(2), JSON.stringify([record('other.png')])]]);
let app = await browser({saved});
assert.deepEqual(names(app.select('#upload-history').children), ['old.png', 'old.png']);
await app.upload([file('new.png')], [{network: true}]);
assert.deepEqual(app.records().map(item => item.name), ['old.png', 'old.png', 'new.png']);
assert.deepEqual(names(app.select('#upload-results').children), ['new.png']);
const serialized = saved.get(storageKey(1));
assert(!serialized.includes('PRIVATE'), 'History must not persist file contents or credentials');
for (const item of app.records()) assert.deepEqual(Object.keys(item).sort(), ['id', 'key', 'lastModified', 'name', 'size']);
await app.account(owner(2));
assert.deepEqual(names(app.select('#upload-history').children), ['other.png']);
assert.equal(app.select('#upload-results').children.length, 0);
await app.account(owner(1));
assert.deepEqual(names(app.select('#upload-history').children), ['old.png', 'old.png', 'new.png']);
await app.select('#logout').listeners.click();
assert.equal(app.select('#upload-history').children.length, 0);
assert.deepEqual(app.records(1).map(item => item.name), ['old.png', 'old.png', 'new.png']);
app = await browser({saved});
assert.deepEqual(names(app.select('#upload-history').children), ['old.png', 'old.png', 'new.png']);

// Reupload requires the original metadata, starts a fresh request and removes only its associated entry.
const restored = app.select('#upload-history').children[0];
const controls = retryControls(restored);
for (const candidate of [undefined, file('different.png'), file('old.png', 13), file('old.png', 12, 101)]) {
  controls.input.files = candidate ? [candidate] : [];
  await controls.button.listeners.click();
  assert.equal(app.state.sent.length, 0);
  assert.match(controls.message.textContent, /same name, size and modification time/);
}
controls.input.files = [file('old.png')];
app.state.uploadReplies.push(request => {
  assert.equal(restored.children[2].value, 99, 'The new request reports its own transfer progress');
  assert.equal(request.method, 'POST'); assert.equal(request.path, '/api/v1/uploads');
  queueMicrotask(() => { request.status = 201; request.responseText = JSON.stringify({id: 'fresh', state: 'completed', url: '/image/fresh'}); request.onload(); });
});
await controls.button.listeners.click();
assert.equal(app.state.sent.length, 1);
assert.equal(app.state.sent[0].file, controls.input.files[0]);
assert.equal(restored.children[2].value, 100);
assert.deepEqual(app.records().map(item => item.name), ['old.png', 'new.png']);
assert.equal(app.records()[0].lastModified, 200, 'Same-name independent records survive associated success');
assert(!saved.get(storageKey(1)).includes('PRIVATE'));

// Old records have no expiry and malformed/unavailable browser storage does not break uploading.
const ancient = new Map([[storageKey(1), JSON.stringify([{...record('ancient.png'), created_at: 1, expires_at: 2}])]]);
const old = await browser({saved: ancient});
assert.deepEqual(names(old.select('#upload-history').children), ['ancient.png']);
await old.upload([file('today.png')], [completed('today')]);
assert.deepEqual(old.records().map(item => item.name), ['ancient.png']);
for (const value of ['{broken', '{}', 'null', '[null,2,{"name":3}]']) {
  const damaged = await browser({saved: new Map([[storageKey(1), value]])});
  await damaged.upload([file('valid.png')], [completed('valid')]);
  assert.equal(damaged.select('#upload-results').children[0].children[2].value, 100);
}
const denied = await browser({storageError: true});
await denied.upload([file('valid.png')], [completed('valid')]);
assert.equal(denied.select('#upload-results').children[0].children[2].value, 100);
assert.match(denied.select('#history-status').textContent, /unavailable|cannot be restored/i, 'Failed storage must leave a visible recovery warning');

// Clearing persisted history also prevents an active batch from recreating forgotten records.
const clearing = await browser({saved: new Map([[storageKey(1), JSON.stringify([record('retained.png')])]])});
let finishCleared;
const {pending: clearedBatch} = await clearing.upload([file('active.png')], [request => {
  finishCleared = () => request.onerror();
}]);
assert.deepEqual(clearing.records().map(item => item.name), ['retained.png', 'active.png']);
await clearing.select('#clear-upload-history').listeners.click();
assert.equal(clearing.select('#upload-history').children.length, 0);
assert.deepEqual(clearing.records(), []);
finishCleared(); await clearedBatch;
assert.deepEqual(clearing.records(), [], 'Late upload results must not restore manually cleared history');
const clearedPage = await browser({saved: clearing.state.saved});
assert.equal(clearedPage.select('#upload-history').children.length, 0);
await clearing.upload([file('later.png')], [{network: true}]);
assert.deepEqual(clearing.records().map(item => item.name), ['later.png']);

// Known temporary admission failures wait without advancing the selection order.
for (const code of ['upload_capacity', 'resource_cooldown']) {
  const waiting = await browser();
  const {pending} = await waiting.upload([file('first.png'), file('second.png')], [
    {status: 429, body: {error: 'Busy', code}}, completed('first'), completed('second')
  ]);
  assert.deepEqual(waiting.state.sent.map(item => item.file.name), ['first.png']);
  assert.match(waiting.select('#upload-results').children[0].children[1].textContent, /Waiting/);
  assert.equal(waiting.select('#logout').disabled, true);
  await waiting.tick(); await pending;
  assert.deepEqual(waiting.state.sent.map(item => item.file.name), ['first.png', 'first.png', 'second.png']);
  assert.deepEqual(names(waiting.select('#upload-results').children), ['first.png', 'second.png']);
  assert.equal(waiting.select('#logout').disabled, false);
}
for (const rejected of [
  {status: 429, body: {error: 'No storage', code: 'storage_quota'}},
  {network: true}, {status: 200, raw: '<html>unknown outcome</html>'}
]) {
  const failed = await browser();
  const {pending} = await failed.upload([file('failed.png'), file('next.png')], [rejected, completed('next')]);
  await pending;
  assert.deepEqual(failed.state.sent.map(item => item.file.name), ['failed.png', 'next.png']);
  assert.equal(failed.timers.length, 0, 'Unknown outcomes and quota must not automatically resubmit');
  assert.equal(failed.select('#upload-results').children[0].children[2].hidden, true);
  assert.equal(failed.records()[0].name, 'failed.png');
}

// Only authoritative ready links reach 100; fallback copying follows selection order across batches.
const links = await browser();
await links.upload([file('first.png'), file('second.png')], [
  {status: 202, body: {id: 'first', state: 'retry_wait', temporary: true, url: '/temporary/first'}},
  {status: 202, body: {id: 'second', state: 'retry_wait', temporary: true, url: '/temporary/second'}}
]);
const rows = links.select('#upload-results').children;
assert.notEqual(rows[0].children[2].value, 100);
assert.notEqual(rows[1].children[2].value, 100);
await links.select('#copy-urls').listeners.click();
assert.equal(links.state.clipboard, 'https://example.invalid/temporary/first\nhttps://example.invalid/temporary/second');
links.state.poll = async path => response(path.endsWith('/first') ? {id: 'first', state: 'completed'} : {id: 'second', state: 'completed', url: '/image/second'});
await links.tick();
assert.notEqual(rows[0].children[2].value, 100, 'Completed without a URL is not ready');
await links.tick();
assert.equal(rows[1].children[2].value, 100);
links.state.poll = async () => response({id: 'first', state: 'completed', url: '/image/first'});
await links.tick();
assert.equal(rows[0].children[2].value, 100, 'Polling must acquire a late final URL');
await links.upload([file('third.png')], [completed('third')]);
assert.deepEqual(names(links.select('#upload-results').children), ['first.png', 'second.png', 'third.png']);
await links.select('#copy-markdown').listeners.click();
assert.equal(links.state.clipboard, '![](https://example.invalid/image/first)\n![](https://example.invalid/image/second)\n![](https://example.invalid/image/third)');

// A transient poll failure recovers; delayed responses from the old account never enter the new account.
const polling = await browser();
await polling.upload([file('pending.png')], [{status: 202, body: {id: 'pending', state: 'queued'}}]);
let pollCount = 0;
polling.state.poll = async () => { if (++pollCount === 1) throw new Error('Offline'); return response({id: 'pending', state: 'completed', url: '/image/pending'}); };
await polling.tick();
assert.match(polling.select('#upload-results').children[0].children[1].textContent, /Cannot refresh/);
assert.equal(polling.records()[0].id, 'pending');
await polling.tick();
assert.equal(polling.select('#upload-results').children[0].children[2].value, 100);
assert.deepEqual(polling.records(), []);
await polling.upload([file('old-account.png')], [{status: 202, body: {id: 'old-account', state: 'queued'}}]);
let resolvePoll;
polling.state.poll = () => new Promise(resolve => { resolvePoll = resolve; });
await polling.tick();
await polling.account(owner(2));
resolvePoll(response({id: 'old-account', state: 'completed', url: '/private/old-account'})); await settle();
assert.equal(polling.select('#upload-results').children.length, 0);
assert.equal(polling.select('#upload-history').children.length, 0);
assert.equal(polling.state.saved.has(storageKey(2)), false);
await polling.select('#copy-urls').listeners.click();
assert.equal(polling.state.clipboard, '');
assert.equal(polling.records(1)[0].id, 'old-account');
// Independently opened tabs merge additions, and same-name records retain distinct associations.
const shared = new Map();
const tabA = await browser({saved: shared});
const tabB = await browser({saved: shared});
await tabA.upload([file('same.png')], [{network: true}]);
const firstKey = tabA.records()[0].key;
await tabB.upload([file('same.png')], [{status: 202, body: {id: 'tab-b', state: 'queued'}}]);
assert.equal(tabB.records().length, 2, 'A stale tab must preserve the other tab additions');
assert.equal(new Set(tabB.records().map(item => item.key)).size, 2);
await tabA.upload([file('extra.png')], [{network: true}]);
assert.deepEqual(tabA.records().map(item => item.name), ['same.png', 'same.png', 'extra.png']);
tabB.state.poll = async () => response({id: 'tab-b', state: 'queued'});
await tabB.tick();
assert.equal(tabB.records().length, 3, 'Polling must preserve entries added by another tab');
tabB.state.poll = async () => response({id: 'tab-b', state: 'completed', url: '/image/tab-b'});
await tabB.tick();
assert.deepEqual(tabB.records().map(item => item.key), [firstKey, tabA.records().find(item => item.name === 'extra.png').key]);
await tabA.upload([file('after-success.png')], [{network: true}]);
assert.equal(tabA.records().length, 3, 'Other-tab success removes only its own same-name record');

// Late polling from stale tabs cannot resurrect completed or manually cleared shared entries.
const concurrent = new Map([[storageKey(1), JSON.stringify([record('duplicate.png', 'one', 'one'), record('duplicate.png', 'two', 'two')])]]);
const completedTab = await browser({saved: concurrent});
const staleTab = await browser({saved: concurrent});
completedTab.state.poll = async () => response({id: 'one', state: 'completed', url: '/image/one'});
await completedTab.tick();
assert.deepEqual(completedTab.records().map(item => item.key), ['two']);
staleTab.state.poll = async path => response({id: path.split('/').at(-1), state: 'queued'});
await staleTab.tick();
assert.deepEqual(staleTab.records().map(item => item.key), ['two'], 'Stale polling cannot recreate another tab successful record');
await completedTab.select('#clear-upload-history').listeners.click();
await staleTab.tick();
assert.deepEqual(staleTab.records(), [], 'Stale polling cannot recreate another tab manually cleared records');

// A rejected logout resumes receipt polling; a lost response after server logout still clears account data.
const logoutFailure = await browser();
await logoutFailure.upload([file('pending-logout.png')], [{status: 202, body: {id: 'pending-logout', state: 'queued'}}]);
logoutFailure.state.logout = async () => { throw new Error('Logout network failure'); };
let resumedPolls = 0;
logoutFailure.state.poll = async () => { resumedPolls++; return response({id: 'pending-logout', state: 'completed', url: '/image/pending-logout'}); };
await logoutFailure.select('#logout').listeners.click();
assert.equal(logoutFailure.select('#account-form').hidden, true);
await logoutFailure.tick();
assert.equal(resumedPolls, 0, 'The timer from the previous revision cannot issue a stale request');
await logoutFailure.tick();
assert.equal(resumedPolls, 1);
assert.equal(logoutFailure.select('#upload-results').children[0].children[2].value, 100);
await logoutFailure.upload([file('lost-response.png')], [{status: 202, body: {id: 'lost-response', state: 'queued'}}]);
logoutFailure.state.logout = async () => { logoutFailure.state.user = null; throw new Error('Logout response lost'); };
await logoutFailure.select('#logout').listeners.click();
assert.equal(logoutFailure.select('#account-form').hidden, false);
assert.equal(logoutFailure.select('#upload-results').children.length, 0);
assert.equal(logoutFailure.select('#upload-history').children.length, 0);
assert.equal(logoutFailure.records(1)[0].id, 'lost-response');

console.log('Browser recovery passed: account-bound persistence, selective retry cleanup, safe reselection, capacity waits, ordered links, storage failures and polling isolation.');
