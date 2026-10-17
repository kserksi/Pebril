import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

class Element {
  constructor(tag = '') { this.tagName = tag; this.children = []; this.listeners = {}; this.value = ''; this.disabled = false; this.hidden = false; this.textContent = ''; }
  addEventListener(name, callback) { this.listeners[name] = callback; }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this.children = children; }
  reset() { this.value = ''; for (const child of this.children) child.reset?.(); }
}
const elements = new Map();
const intervals = [];
const documentEvents = {};
const document = {
  hidden: false,
  addEventListener(name, callback) { documentEvents[name] = callback; },
  querySelector(selector) { if (!elements.has(selector)) elements.set(selector, new Element()); return elements.get(selector); },
  createElement(tag) { return new Element(tag); }
};
const admin = {id: 1, username: 'Administrator', admin: true, status: 'active', quota: 1000, used: 2};
const ordinary = {id: 2, username: '<script>fixture</script>', admin: false, status: 'active', quota: 100, used: 10};
let users = [{...admin, quota: '1000', used: '2'}, {...ordinary, quota: '9007199254740993', used: '9007199254740993'}];
let user = null;
let initialized = false;
let holdUsers = false;
let resolveUsers;
let holdLogout = false;
let resolveLogout;
let rejectMutation = false;
const calls = [];
const reply = (value, ok = true) => ({ok, json: async () => value});
class FormData {
  constructor(form) { this.entries = Object.entries(form?.values || {}); }
  [Symbol.iterator]() { return this.entries[Symbol.iterator](); }
}
const context = vm.createContext({
  document, FormData, setInterval(callback, delay) { intervals.push({callback, delay}); }, alert() {}, URL, location: {href: 'https://example.invalid/'},
  localStorage: {getItem() { return null; }},
  fetch: async (path, options = {}) => {
    const data = options.body ? JSON.parse(options.body) : undefined;
    calls.push({path, method: options.method || 'GET', data, csrf: options.headers?.['X-CSRF-Token']});
    if (path.endsWith('/status')) return reply({native: {}});
    if (path.endsWith('/account')) return reply({initialized, user, csrf: user ? 'session-csrf' : ''});
    if (path.includes('/images?')) return reply({images: [], total: 0});
    if (path.endsWith('/admin/users')) {
      if (holdUsers) return new Promise(resolve => { resolveUsers = resolve; });
      return reply({users: users.map(value => ({...value}))});
    }
    if (/\/admin\/users\/\d+/.test(path)) {
      if (rejectMutation) return reply({error: 'Change rejected'}, false);
      const target = users.find(value => value.id === Number(path.split('/')[5]));
      if (!path.endsWith('/password')) Object.assign(target, data);
      return reply({user: target});
    }
    if (path.endsWith('/logout')) {
      if (holdLogout) return new Promise(resolve => { resolveLogout = () => { user = null; resolve(reply({})); }; });
      user = null; return reply({});
    }
    if (path.endsWith('/initialize')) { initialized = true; user = {...admin}; return reply({user}); }
    if (path.endsWith('/register')) { user = {...ordinary, username: data.username}; return reply({user}); }
    if (path.endsWith('/upload-token')) return reply(options.method === 'POST' ? {token: 'fixture-token'} : {});
    return reply({});
  }
});
vm.runInContext(fs.readFileSync(new URL('../../internal/server/web/app.js', import.meta.url), 'utf8'), context);
const flush = () => new Promise(resolve => setImmediate(resolve));
const element = selector => document.querySelector(selector);
const event = {preventDefault() {}};
await flush();
assert.equal(element('#account-mode').hidden, true, 'Registration is unavailable before initialization');
assert.equal(element('#issue-token').disabled, true, 'Signed-out visitors cannot issue tokens');
assert.equal(element('#account-submit').textContent, 'Create administrator');
element('#account-form').values = {username: 'Administrator', password: 'init-password'};
await element('#account-form').listeners.submit(event);
assert.equal(calls.find(call => call.path.endsWith('/initialize')).data.username, 'Administrator');
assert.equal(element('#admin-section').hidden, false);
let rows = element('#admin-users').children;
assert.equal(rows[0].children.length, 1, 'The administrator has no status, quota, password or role controls');
assert.equal(rows[1].children[0].textContent.startsWith('<script>fixture</script>'), true, 'Usernames are rendered as text');
let quotaForm = rows[1].children[2];
let quota = quotaForm.children[0].children[0];
let units = quotaForm.children[1].children[0];
units.value = 'B'; units.listeners.change();
assert.equal(quota.value, '9007199254740993', 'Stored quota retains its exact decimal value');
assert.match(rows[1].children[0].textContent, /storage 9007199254740993 \/ 9007199254740993 bytes/);
for (const value of ['-1', '1.5', '', '01', '+1', '1e3', '9223372036854775808']) {
  const before = calls.length;
  quota.value = value; await quotaForm.listeners.submit(event);
  assert.equal(calls.length, before, `Invalid quota ${value} must not be sent`);
}
for (const value of ['9007199254740993', '9223372036854775807', '0']) {
  quotaForm = element('#admin-users').children[1].children[2];
  quota = quotaForm.children[0].children[0];
  units = quotaForm.children[1].children[0]; units.value = 'B'; units.listeners.change();
  quota.value = value; await quotaForm.listeners.submit(event);
  assert.equal(users[1].quota, value, 'Valid quota is sent as an exact decimal string');
  const updatedForm = element('#admin-users').children[1].children[2];
  const updatedUnits = updatedForm.children[1].children[0]; updatedUnits.value = 'B'; updatedUnits.listeners.change();
  assert.equal(updatedForm.children[0].children[0].value, value);
}
assert.equal(calls.find(call => call.method === 'PATCH').csrf, 'session-csrf');
rows = element('#admin-users').children;
await rows[1].children[1].listeners.click();
assert.equal(users[1].status, 'banned');
await element('#admin-users').children[1].children[1].listeners.click();
assert.equal(users[1].status, 'active');
let passwordForm = element('#admin-users').children[1].children[3];
let password = passwordForm.children[0].children[0];
let confirmation = passwordForm.children[1].children[0];
password.value = 'new-password'; confirmation.value = 'different-password';
await passwordForm.listeners.submit(event);
assert.equal(calls.some(call => call.path.endsWith('/password')), false);
assert.equal(password.value, ''); assert.equal(confirmation.value, '');
password.value = confirmation.value = 'new-password';
await passwordForm.listeners.submit(event);
const reset = calls.find(call => call.path.endsWith('/password'));
assert.deepEqual(reset.data, {password: 'new-password'});
assert.equal(reset.method, 'POST');
assert.equal(password.value, ''); assert.equal(confirmation.value, '');
assert.match(element('#admin-status').textContent, /Share the new password outside Pebril/);
passwordForm = element('#admin-users').children[1].children[3];
password = passwordForm.children[0].children[0]; confirmation = passwordForm.children[1].children[0];
password.value = confirmation.value = 'failed-password'; rejectMutation = true;
await passwordForm.listeners.submit(event); rejectMutation = false;
assert.equal(password.value, ''); assert.equal(confirmation.value, '');
assert.equal(element('#admin-status').textContent, 'Change rejected');

holdUsers = true;
const pending = context.loadUsers(); await flush();
holdLogout = true;
const logout = element('#logout').listeners.click();
const beforeLogoutRefresh = calls.length;
await context.refreshAccount();
assert.equal(calls.length, beforeLogoutRefresh, 'Account polling must pause during logout');
assert.equal(element('#admin-section').hidden, true, 'Administration is hidden immediately at logout');
assert.equal(element('#admin-users').children.length, 0);
resolveUsers(reply({users: [{...admin}, {...ordinary}]})); await pending;
assert.equal(element('#admin-users').children.length, 0, 'A stale administrator response cannot repopulate the page');
resolveLogout(); await logout; holdLogout = holdUsers = false;
assert.equal(element('#account-mode').hidden, false);
await element('#account-mode').listeners.click();
assert.equal(element('#account-submit').textContent, 'Register');
assert.equal(element('#account-form input[name="password"]').autocomplete, 'new-password');
element('#account-form').values = {username: 'Alice', password: 'registration-password'};
await element('#account-form').listeners.submit(event);
const registration = calls.find(call => call.path.endsWith('/register'));
assert.deepEqual(registration.data, {username: 'Alice', password: 'registration-password'});
assert.equal(element('#admin-section').hidden, true, 'Ordinary users cannot see administration');
assert.equal(element('#admin-users').children.length, 0);
assert.equal(element('#account-form').hidden, true, 'Registration signs the user in');
for (const restricted of [{...ordinary, status: 'banned'}, {...ordinary, quota: 0}]) {
  user = restricted; await context.loadAccount();
  if (user.status === 'banned') assert.match(element('#account-status').textContent, /banned/);
  for (const selector of ['#files', '#upload']) assert.equal(element(selector).disabled, true);
  const before = calls.length;
  await element('#upload').listeners.click();
  assert.equal(calls.length, before, 'Banned and zero-quota sessions cannot start uploads');
  assert.equal(element('#token-controls').hidden, false);
  assert.equal(element('#issue-token').disabled, false, 'An existing session retains token management');
  await element('#issue-token').listeners.click();
  assert.equal(calls.at(-1).path, '/api/v1/upload-token');
  assert.equal(calls.at(-1).method, 'POST');
  assert.equal(calls.at(-1).csrf, 'session-csrf');
  assert.equal(element('#upload-token').textContent, 'fixture-token');
  await element('#revoke-token').listeners.click();
  assert.equal(calls.at(-1).path, '/api/v1/upload-token');
  assert.equal(calls.at(-1).method, 'DELETE');
  assert.equal(element('#upload-token').textContent, 'Token revoked.');
}
user = {...ordinary}; await context.loadAccount();
assert.equal(element('#upload').disabled, false);
console.log('Account browser logic passed: registration, administrator-only controls, quota boundaries, ban/unban, confirmed reset and failure clearing, CSRF, literal usernames, and stale-response isolation.');

// Capacity edits and unit changes preserve exact bytes, including beyond Number precision.
user = {...admin}; users[1].quota = '1073741824'; await context.loadAccount();
quotaForm = element('#admin-users').children[1].children[2];
quota = quotaForm.children[0].children[0]; units = quotaForm.children[1].children[0];
assert.equal(quota.value, '1'); assert.equal(units.value, 'GiB');
const beforeUnits = calls.length;
quota.value = '1.5'; units.value = 'MiB'; units.listeners.change();
assert.equal(quota.value, '1536');
units.value = 'KiB'; units.listeners.change(); assert.equal(quota.value, '1572864');
units.value = 'GiB'; units.listeners.change(); assert.equal(quota.value, '1.5');
assert.equal(calls.length, beforeUnits, 'Changing units does not mutate the account');
await quotaForm.listeners.submit(event);
assert.equal(users[1].quota, '1610612736');
quotaForm = element('#admin-users').children[1].children[2];
quota = quotaForm.children[0].children[0]; units = quotaForm.children[1].children[0];
units.value = 'KiB'; units.listeners.change(); quota.value = '0.1';
const beforeFraction = calls.length;
await quotaForm.listeners.submit(event);
assert.equal(calls.length, beforeFraction, 'Fractional bytes must be rejected without rounding');
units.value = 'B'; units.listeners.change();
assert.equal(units.value, 'KiB', 'Invalid current input prevents a lossy unit change');
assert.equal(quota.value, '0.1');
quota.value = '0.0009765625'; await quotaForm.listeners.submit(event);
assert.equal(users[1].quota, '1', 'An exact fractional KiB is accepted as one byte');

// Polling refreshes eligibility without replacing edited administrator forms or details.
assert.equal(intervals.length, 1); assert.equal(intervals[0].delay, 5000);
quotaForm = element('#admin-users').children[1].children[2];
quotaForm.children[0].children[0].value = '123';
const usersCalls = calls.filter(call => call.path.endsWith('/admin/users')).length;
await context.refreshAccount();
assert.equal(element('#admin-users').children[1].children[2], quotaForm);
assert.equal(quotaForm.children[0].children[0].value, '123');
assert.equal(calls.filter(call => call.path.endsWith('/admin/users')).length, usersCalls);
user = {...ordinary}; await context.loadAccount();
user = {...ordinary, status: 'banned'}; intervals[0].callback(); await flush();
assert.match(element('#account-status').textContent, /banned/);
assert.equal(element('#account-status').className, 'error');
assert.equal(element('#upload').disabled, true);
assert.equal(element('#gallery-section').hidden, false, 'A ban retains the existing session and gallery');
user = {...ordinary}; documentEvents.visibilitychange(); await flush();
assert.equal(element('#upload').disabled, false, 'Unban restores upload eligibility');
const beforeHidden = calls.length;
document.hidden = true; intervals[0].callback(); documentEvents.visibilitychange(); await flush();
assert.equal(calls.length, beforeHidden); document.hidden = false;

// Revocation on an idle page clears token, history, gallery and the account form state.
element('#upload-token').textContent = 'private-fixture-token';
user = null; intervals[0].callback(); await flush();
assert.equal(element('#account-form').hidden, false);
assert.equal(element('#upload-token').textContent, '');
assert.equal(element('#gallery-section').hidden, true);
assert.equal(element('#upload-results').children.length, 0);
user = {...ordinary}; await context.loadAccount();
const normalFetch = context.fetch;
context.fetch = async (path, options) => path.endsWith('/upload-token') ? {ok: false, status: 401, json: async () => { throw new Error('Unparseable expired response'); }} : normalFetch(path, options);
await element('#issue-token').listeners.click();
assert.equal(element('#account-form').hidden, false, 'Protected 401 signs out before parsing its body');
assert.match(element('#account-status').textContent, /session has expired/);
context.fetch = normalFetch;

// A late 401 for the previous session must not sign out a newly authenticated user.
user = {...ordinary}; await context.loadAccount();
let finishOld;
context.fetch = async (path, options) => path === '/api/v1/upload-token' ? new Promise(resolve => { finishOld = resolve; }) : normalFetch(path, options);
const oldRequest = context.api('upload-token').catch(() => {}); await flush();
user = null; await context.loadAccount(); user = {...admin}; await context.loadAccount();
finishOld({ok: false, status: 401, json: async () => ({error: 'Sign in is required'})}); await oldRequest;
assert.equal(element('#admin-section').hidden, false);
assert.equal(element('#account-form').hidden, true);
context.fetch = async (path, options) => path.endsWith('/account') ? Promise.reject(new Error('Offline')) : normalFetch(path, options);
await context.refreshAccount();
assert.equal(element('#account-form').hidden, true, 'A network failure does not prove revocation');
context.fetch = normalFetch;
console.log('Issue regressions passed: exact quota units, fractional-byte rejection, idle/401 revocation, stale-401 isolation and ban/unban refresh.');
