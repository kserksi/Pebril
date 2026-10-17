import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

class Element {
  constructor(tag = '') { this.tagName = tag; this.children = []; this.listeners = {}; this.hidden = false; this.disabled = false; this.checked = false; this.value = ''; this.text = ''; this.parentElement = null; }
  set textContent(value) { this.replaceChildren(); this.text = String(value); }
  get textContent() { return this.text + this.children.map(child => typeof child === 'string' ? child : child.textContent).join(''); }
  set innerHTML(value) { throw new Error('Metadata must be rendered as text'); }
  addEventListener(name, callback) { this.listeners[name] = callback; }
  append(...children) {
    for (const child of children) {
      if (child instanceof Element) {
        if (child.parentElement) child.parentElement.children.splice(child.parentElement.children.indexOf(child), 1);
        child.parentElement = this;
      }
      this.children.push(child);
    }
  }
  replaceChildren(...children) {
    for (const child of this.children) if (child instanceof Element) child.parentElement = null;
    this.text = ''; this.children = []; this.append(...children);
  }
  reset() { this.value = ''; }
  setAttribute(name, value) { this[name] = value; }
  removeAttribute(name) { delete this[name]; }
}
const elements = new Map();
const document = {
  addEventListener() {},
  querySelector(selector) {
    if (!elements.has(selector)) elements.set(selector, new Element());
    const node = elements.get(selector);
    if (selector === '#gallery-detail') {
      let parent = node;
      while (parent && parent !== elements.get('#gallery-section')) parent = parent.parentElement;
      return parent ? node : null;
    }
    return node;
  },
  createElement(tag) { return new Element(tag); }
};
const element = selector => elements.get(selector) || document.querySelector(selector);
const galleryDetail = new Element();
elements.set('#gallery-detail', galleryDetail);
element('#gallery-section').append(element('#gallery-images'), galleryDetail);
const buttons = node => node.children.filter(child => child.tagName === 'button');
const links = node => node.children.filter(child => child.tagName === 'a');
const flush = () => new Promise(resolve => setImmediate(resolve));
const reply = (value, ok = true) => ({ok, json: async () => value});
const admin = {id: 1, username: 'Administrator', admin: true, quota: 100, used: 1};
const ordinary = {id: 2, username: 'Member', admin: false, quota: 100, used: 1};
let user = {...admin};
const image = id => ({id, owner_id: 2, state: 'completed', format: 'webp', storage_bytes: 45, created_at: 1000, width: 8, height: 6, frames: 1, url: `/image/${id}`, cover_url: `/image/${id}/cover`, original_available: true, files: [{role: 'original', format: 'png', bytes: 30, width: 16, height: 12, frames: 1}, {role: 'result', format: 'webp', bytes: 15, width: 8, height: 6, frames: 1}]});
let images = Array.from({length: 21}, (_, index) => image(`record-${index}`));
let detail = image('record-0');
let confirmed = false;
let hold = '';
let resolveHeld;
let failure = '';
const calls = [];
const timers = [];
let uploadReceipt;
let uploadHold = '';
let resolveUpload;
let clipboard = '';
class FormData { append() {} }
class XMLHttpRequest {
  constructor() { this.upload = {}; }
  open() {}
  setRequestHeader() {}
  send() { this.status = 202; this.responseText = JSON.stringify(uploadReceipt); queueMicrotask(() => this.onload()); }
}
const context = vm.createContext({
  document, URL, FormData, XMLHttpRequest, setInterval() {}, setTimeout: callback => timers.push(callback), navigator: {clipboard: {async writeText(value) { clipboard = value; }}}, location: {href: 'https://example.invalid/'}, alert() {}, confirm: () => confirmed,
  localStorage: {getItem() { return null; }, setItem() {}},
  fetch: async (path, options = {}) => {
    calls.push({path, method: options.method || 'GET', csrf: options.headers?.['X-CSRF-Token']});
    if (path.endsWith('/status')) return reply({native: {}});
    if (path.endsWith('/account')) return reply({initialized: true, user, csrf: 'fixture-csrf'});
    if (path.endsWith('/admin/users')) return reply({users: []});
    if (path.endsWith('/logout')) { user = null; return reply({}); }
    if (path.includes('/uploads/')) {
      if (uploadHold === (path.endsWith('/retry') ? 'retry' : 'poll')) return new Promise(resolve => { resolveUpload = resolve; });
      return reply(uploadReceipt);
    }
    if (path.includes('/images')) {
      const kind = options.method === 'DELETE' ? 'delete' : path.includes('?') ? 'list' : 'detail';
      if (hold === kind) return new Promise(resolve => { resolveHeld = resolve; });
      if (failure === kind) return reply({error: `${kind} failed`}, false);
      if (kind === 'delete') { images = images.filter(image => !path.endsWith(image.id)); return reply({deleted: true}); }
      if (kind === 'detail') return reply(detail);
      const offset = Number(new URL(path, 'https://example.invalid').searchParams.get('offset'));
      return reply({images: images.slice(offset, offset + 20), total: images.length, limit: 20, offset});
    }
    throw new Error(`Unexpected API call: ${path}`);
  }
});
vm.runInContext(fs.readFileSync(new URL('../../internal/server/web/app.js', import.meta.url), 'utf8'), context);
await flush();
assert.equal(element('#gallery-section').hidden, false);
assert.equal(element('#gallery-all-label').hidden, false);
assert.equal(element('#gallery-images').children.length, 20);
assert.equal(element('#gallery-status').textContent, 'Images 1–20 of 21.');
assert.equal(element('#gallery-previous').disabled, true);
assert.equal(element('#gallery-next').disabled, false);
await element('#gallery-next').listeners.click();
assert.equal(calls.at(-1).path, '/api/v1/images?limit=20&offset=20');
assert.equal(element('#gallery-images').children.length, 1);
assert.equal(element('#gallery-next').disabled, true);
await element('#gallery-previous').listeners.click();
element('#gallery-all').checked = true;
await element('#gallery-all').listeners.change();
assert.equal(calls.at(-1).path, '/api/v1/images?limit=20&offset=0&scope=all');
assert.match(element('#gallery-images').children[0].textContent, /Owner: 2/);

const inspect = (index = 0) => buttons(element('#gallery-images').children[index])[0].listeners.click();
await inspect();
assert.match(element('#gallery-detail').textContent, /original: png, 30 bytes, 16 × 12, 1 frames/);
assert.match(element('#gallery-detail').textContent, /result: webp, 15 bytes, 8 × 6, 1 frames/);
assert.equal(links(galleryDetail).at(-1).href, '/api/v1/images/record-0/original');
const publicResult = links(galleryDetail).find(link => link.textContent === 'Open public result');
assert.equal(publicResult.href, '/image/record-0');
assert.equal(publicResult.target, '_blank', 'Opening a public result keeps the management page available');
assert(publicResult.rel.split(/\s+/).includes('noopener'), 'The result tab cannot access the management window');
assert.equal(galleryDetail.parentElement, element('#gallery-images').children[0], 'Details expand under the selected image');

// A newer selection moves the one panel immediately and owns its final contents.
hold = 'detail'; const pendingOlderDetail = inspect(); await flush(); const finishOlderDetail = resolveHeld;
const firstRow = element('#gallery-images').children[0];
const secondRow = element('#gallery-images').children[1];
hold = ''; detail = image('record-1');
const pendingNewerDetail = inspect(1);
assert.equal(galleryDetail.parentElement, secondRow, 'The loading panel moves before the detail request finishes');
assert.equal(galleryDetail.textContent, 'Loading image details...');
assert(!firstRow.children.includes(galleryDetail), 'The previous row no longer contains the panel');
await pendingNewerDetail;
finishOlderDetail(reply({...image('record-0'), error: 'superseded detail'})); await pendingOlderDetail;
assert.equal(galleryDetail.parentElement, secondRow);
assert.match(galleryDetail.textContent, /Image record-1/);
assert(!galleryDetail.textContent.includes('superseded detail'), 'An older selection cannot replace the selected image');
detail = image('record-0'); await inspect();
let remove = buttons(element('#gallery-detail'))[0];
const beforeCancel = calls.length;
await remove.listeners.click();
assert.equal(calls.length, beforeCancel, 'Cancellation sends no delete request');
confirmed = true;
failure = 'delete'; await remove.listeners.click(); failure = '';
assert.equal(element('#gallery-status').textContent, 'delete failed');
assert.equal(remove.disabled, false);
await remove.listeners.click();
assert(calls.some(call => call.method === 'DELETE' && call.csrf === 'fixture-csrf'));
assert.equal(element('#gallery-images').children.length, 20);
assert.match(element('#gallery-images').children[0].textContent, /record-1/);
assert.equal(galleryDetail.textContent, '', 'Reloading after deletion clears the previous details');
assert.equal(document.querySelector('#gallery-detail'), null, 'Removed rows detach the panel from document lookup');

// A disappeared last-page record returns the view to the remaining page.
images.push(image('record-last'));
await element('#refresh-gallery').listeners.click();
await element('#gallery-next').listeners.click();
detail = image('record-last'); await inspect();
assert.equal(galleryDetail.parentElement, element('#gallery-images').children[0], 'The retained panel reattaches on another page');
await buttons(element('#gallery-detail'))[0].listeners.click();
assert.equal(element('#gallery-previous').disabled, true);
assert.equal(element('#gallery-status').textContent, 'Images 1–20 of 20.');

// Originals never gain an ordinary public link, including temporary fallback records.
detail = {...image('record-1'), state: 'retry_wait', temporary: true, original_available: false, cover_url: '', error: '<script>fixture</script>'};
await inspect();
assert.equal(links(element('#gallery-detail')).length, 0);
assert.match(element('#gallery-detail').textContent, /<script>fixture<\/script>/);
failure = 'detail'; await inspect(); failure = '';
assert.equal(element('#gallery-detail').textContent, 'detail failed');
assert.equal(galleryDetail.parentElement, element('#gallery-images').children[0], 'Detail errors remain under their selected row');
failure = 'list'; await element('#refresh-gallery').listeners.click(); failure = '';
assert.equal(element('#gallery-status').textContent, 'list failed');
assert.equal(galleryDetail.textContent, '', 'A failed gallery reload also clears detached details');
await element('#refresh-gallery').listeners.click();

// Deleting an uploaded image removes its copyable URL and rejects a pending poll.
uploadReceipt = {...image('record-1'), state: 'processing', url: undefined};
element('#files').files = [{name: 'pending.png'}];
await element('#upload').listeners.click();
uploadHold = 'poll'; timers.shift()(); await flush();
const finishPoll = resolveUpload;
detail = image('record-1'); await inspect();
await buttons(element('#gallery-detail'))[0].listeners.click();
assert.equal(element('#upload-results').children.length, 0);
finishPoll(reply(image('record-1'))); await flush(); uploadHold = '';
await element('#copy-urls').listeners.click();
assert.equal(clipboard, '', 'A late poll cannot make the deleted image copyable');
assert.equal(element('#total-progress').textContent, 'Completed: 0/0. Failed: 0.');

// Pending retries and detached retry controls cannot restore a deleted upload.
uploadReceipt = {...image('record-2'), state: 'failed', retryable: true, temporary: true};
element('#files').files = [{name: 'retry.png'}];
await element('#upload').listeners.click();
const retryStatus = element('#upload-results').children[0].children[1];
const retryButton = buttons(retryStatus)[0];
uploadHold = 'retry'; const pendingRetry = retryButton.listeners.click(); await flush();
const finishRetry = resolveUpload;
detail = image('record-2'); await inspect();
await buttons(element('#gallery-detail'))[0].listeners.click();
finishRetry(reply({...image('record-2'), state: 'queued'})); await pendingRetry; uploadHold = '';
assert.equal(element('#upload-results').children.length, 0);
await element('#copy-markdown').listeners.click();
assert.equal(clipboard, '', 'A late retry cannot restore the deleted URL');
const afterRetryDelete = calls.length;
await retryButton.listeners.click();
assert.equal(calls.length, afterRetryDelete, 'Detached retry buttons cannot restart deleted records');

// A stale deletion cannot refresh the next account or display its error there.
detail = image('record-1'); await inspect();
hold = 'delete';
const pendingDelete = buttons(element('#gallery-detail'))[0].listeners.click(); await flush();
const finishDelete = resolveHeld;
user = {...ordinary}; hold = ''; await context.loadAccount();
assert.equal(element('#gallery-all-label').hidden, true);
assert.equal(element('#gallery-all').checked, false);
assert(!calls.at(-1).path.includes('scope=all'));
const afterSwitch = calls.length;
finishDelete(reply({error: 'old account delete error'}, false)); await pendingDelete;
assert.equal(calls.length, afterSwitch);
assert(!element('#gallery-status').textContent.includes('old account'));

// A detail response from a different account is discarded.
hold = 'detail'; const pendingDetail = inspect(); await flush(); const finishDetail = resolveHeld;
user = {...admin}; hold = ''; await context.loadAccount();
assert.equal(galleryDetail.textContent, '', 'Switching accounts clears pending details immediately');
finishDetail(reply({...image('private-detail'), error: 'private metadata'})); await pendingDetail;
assert.equal(element('#gallery-detail').textContent, '');

// Superseded list responses and logout cannot restore earlier metadata.
hold = 'list'; const pendingList = element('#refresh-gallery').listeners.click(); await flush(); const finishList = resolveHeld;
hold = ''; await element('#refresh-gallery').listeners.click();
finishList(reply({images: [image('stale-list')], total: 1})); await pendingList;
assert(!element('#gallery-images').textContent.includes('stale-list'));
hold = 'list'; const pendingLogoutList = element('#refresh-gallery').listeners.click(); await flush();
const logout = element('#logout').listeners.click();
assert.equal(element('#gallery-section').hidden, true, 'Gallery disappears before logout completes');
assert.equal(element('#gallery-images').children.length, 0);
resolveHeld(reply({images: [image('private-list')], total: 1}));
await pendingLogoutList; await logout;
assert.equal(element('#gallery-images').children.length, 0);
assert.equal(element('#gallery-detail').textContent, '');
console.log('Gallery browser logic passed: paging, admin scope, selected-row detail placement and reparenting, isolated public result tabs, metadata safety, protected originals, confirmed CSRF deletion, last-page recovery and stale response isolation.');
