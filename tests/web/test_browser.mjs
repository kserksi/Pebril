// Real Chromium acceptance. Playwright is isolated test tooling, not a project dependency.
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import {spawn} from 'node:child_process';
import {once} from 'node:events';
import net from 'node:net';
import path from 'node:path';
import {randomBytes} from 'node:crypto';
import {fileURLToPath, pathToFileURL} from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const output = process.env.PEBRIL_TEST_OUTPUT || path.join(root, 'dist');
assert(process.env.PEBRIL_PLAYWRIGHT_MODULE, 'Set PEBRIL_PLAYWRIGHT_MODULE to the reviewed Playwright index.mjs');
const {chromium} = await import(pathToFileURL(process.env.PEBRIL_PLAYWRIGHT_MODULE).href);
const temporary = await fs.mkdtemp(path.join(root, '.build/browser-issues-'));
const reservation = net.createServer();
reservation.listen(0, '127.0.0.1'); await once(reservation, 'listening');
const port = reservation.address().port; await new Promise(resolve => reservation.close(resolve));
const origin = `http://127.0.0.1:${port}`;
const environment = Object.fromEntries(Object.entries(process.env).filter(([name]) => !name.startsWith('PEBRIL_')));
const credentials = () => ({username: randomBytes(8).toString('hex'), password: randomBytes(24).toString('base64url')});
const administrator = credentials(), member = credentials();
const pageErrors = [];
let service, browser;
let serviceLog = '';
const formEvent = async (page, endpoint, action) => {
  const oldRow = endpoint.startsWith('/api/v1/admin/users/') ? await page.locator('#admin-users > li').last().elementHandle() : null;
  const pending = page.waitForResponse(response => response.url() === origin + endpoint && response.request().method() !== 'GET');
  await action();
  const result = await (await pending).json();
  if (oldRow) { await page.waitForFunction(node => !node.isConnected, oldRow); await oldRow.dispose(); }
  return result;
};
const authenticate = async (page, mode, value, success = true) => {
  await page.goto(origin);
  await page.locator('#account-submit').waitFor({state: 'visible'});
  await page.waitForFunction(label => document.querySelector('#account-submit').textContent === label, mode === 'initialize' ? 'Create administrator' : 'Sign in');
  if (mode === 'register') await page.locator('#account-mode').click();
  await page.locator('input[name="username"]').fill(value.username);
  await page.locator('input[name="password"]').fill(value.password);
  const result = await formEvent(page, `/api/v1/${mode}`, () => page.locator('#account-submit').click());
  if (success) await page.locator('#account-form').waitFor({state: 'hidden'});
  else await page.waitForFunction(message => document.querySelector('#account-status').textContent === message, result.error);
  return result;
};
const wait = async (page, predicate, argument) => page.waitForFunction(predicate, argument, {timeout: 10000});
try {
  await fs.copyFile(path.join(output, 'pebril'), path.join(temporary, 'pebril'));
  await fs.chmod(path.join(temporary, 'pebril'), 0o755);
  await fs.cp(path.join(output, 'bin'), path.join(temporary, 'bin'), {recursive: true, verbatimSymlinks: true});
  if (process.env.PEBRIL_TEST_RUNTIME) await fs.copyFile(process.env.PEBRIL_TEST_RUNTIME, path.join(temporary, 'bin/libvips.so.42'));
  await fs.writeFile(path.join(temporary, '.env'), `PEBRIL_LISTEN=127.0.0.1:${port}\n`);
  service = spawn(path.join(temporary, 'pebril'), ['--allow-low-memory'], {cwd: '/', env: environment, stdio: ['ignore', 'pipe', 'pipe']});
  service.stdout.on('data', data => { serviceLog += data; }); service.stderr.on('data', data => { serviceLog += data; });
  let ready = false;
  for (let attempt = 0; attempt < 150; attempt++) {
    assert.equal(service.exitCode, null, 'Isolated test service exited before readiness');
    try { if ((await fetch(origin + '/healthz')).ok) { ready = true; break; } } catch {}
    await new Promise(resolve => setTimeout(resolve, 100));
  }
  assert(ready, 'Isolated service readiness timeout');
  browser = await chromium.launch({headless: true});
  const adminContext = await browser.newContext(), memberContext = await browser.newContext(), secondaryContext = await browser.newContext();
  const admin = await adminContext.newPage(), page = await memberContext.newPage(), secondary = await secondaryContext.newPage();
  for (const candidate of [admin, page, secondary]) candidate.on('pageerror', error => pageErrors.push(error.message));
  // Freeze only the secondary page's timers, to deterministically test request-driven 401 handling.
  await secondary.clock.install(); await secondary.clock.pauseAt(new Date());
  await authenticate(admin, 'initialize', administrator);
  await authenticate(page, 'register', member);
  await authenticate(secondary, 'login', member);
  await admin.locator('#refresh-users').click();
  const row = admin.locator('#admin-users > li').filter({hasText: member.username});
  await row.waitFor();
  const quota = row.locator('input[type="text"]'), unit = row.locator('select');
  assert.equal(await quota.inputValue(), '1'); assert.equal(await unit.inputValue(), 'GiB');
  await quota.fill('1.5'); await unit.selectOption('MiB'); assert.equal(await quota.inputValue(), '1536');
  const quotaForm = row.locator('form').nth(0);
  let result = await formEvent(admin, `/api/v1/admin/users/${(await page.evaluate(async () => (await (await fetch('/api/v1/account')).json()).user.id))}`, () => quotaForm.getByRole('button', {name: 'Save quota'}).click());
  assert.equal(result.user.quota, '1610612736');
  const memberID = result.user.id, target = `/api/v1/admin/users/${memberID}`;
  await unit.selectOption('KiB'); await quota.fill('0.1');
  await quotaForm.getByRole('button', {name: 'Save quota'}).click();
  await wait(admin, () => document.querySelector('#admin-status').textContent.includes('No rounding'));
  assert.equal((await admin.evaluate(async () => (await (await fetch('/api/v1/admin/users')).json()).users)).find(user => user.id === memberID).quota, '1610612736');
  await quota.fill('0.0009765625');
  result = await formEvent(admin, target, () => quotaForm.getByRole('button', {name: 'Save quota'}).click()); assert.equal(result.user.quota, '1');
  await quota.fill('9223372036854775807');
  result = await formEvent(admin, target, () => quotaForm.getByRole('button', {name: 'Save quota'}).click()); assert.equal(result.user.quota, '9223372036854775807');
  await unit.selectOption('B'); assert.equal(await quota.inputValue(), '9223372036854775807');
  await quota.fill('1073741824');
  await formEvent(admin, target, () => quotaForm.getByRole('button', {name: 'Save quota'}).click());

  await page.locator('#files').setInputFiles({name: 'invalid.gif', mimeType: 'image/gif', buffer: Buffer.from('GIF89a invalid fixture')});
  await page.locator('#upload').click();
  await wait(page, () => document.querySelector('#total-progress').textContent === 'Completed: 0/1. Failed: 1.');
  assert.equal(await page.locator('#upload-results progress').isVisible(), false);
  assert.match(await page.locator('#upload-results').innerText(), /invalid|unsafe|pixel/i);
  const svg = Buffer.from('<svg xmlns="http://www.w3.org/2000/svg" width="8" height="6"><rect width="8" height="6" fill="red"/></svg>');
  await page.locator('#files').setInputFiles(Array.from({length: 21}, (_, index) => ({name: `fixture-${index}.svg`, mimeType: 'image/svg+xml', buffer: svg})));
  await page.locator('#upload').click();
  await wait(page, () => document.querySelector('#total-progress').textContent === 'Completed: 21/22. Failed: 1.');
  await wait(page, () => document.querySelector('#gallery-images').children.length === 20);
  const entries = page.locator('#gallery-images > li');
  await entries.nth(2).getByRole('button', {name: 'View image details'}).click();
  await entries.nth(2).locator('#gallery-detail h3').waitFor();
  const publicURL = await entries.nth(2).getByRole('link', {name: 'Open public result'}).getAttribute('href');
  const popupPromise = page.waitForEvent('popup');
  await entries.nth(2).getByRole('link', {name: 'Open public result'}).click();
  const popup = await popupPromise; await popup.waitForLoadState();
  assert.equal(await popup.evaluate(() => window.opener === null), true);
  assert.equal(page.url(), origin + '/'); await popup.close();
  await entries.nth(4).getByRole('button', {name: 'View image details'}).click();
  await entries.nth(4).locator('#gallery-detail h3').waitFor();
  assert.equal(await entries.nth(2).locator('#gallery-detail').count(), 0);
  await page.locator('#gallery-next').click();
  await wait(page, () => document.querySelector('#gallery-images').children.length === 2);
  assert.equal(await page.locator('#gallery-detail').count(), 0);
  await entries.first().getByRole('button', {name: 'View image details'}).click();
  await entries.first().locator('#gallery-detail h3').waitFor();
  await page.locator('#gallery-previous').click();
  await wait(page, () => document.querySelector('#gallery-images').children.length === 20);
  await admin.locator('#gallery-all').check();
  await wait(admin, () => document.querySelector('#gallery-images').children.length === 20);
  assert.match(await admin.locator('#gallery-images > li').first().innerText(), /Owner:/);

  await formEvent(admin, target, () => row.getByRole('button', {name: 'Ban user', exact: true}).click());
  await wait(page, () => document.querySelector('#account-status').textContent.includes('banned'));
  assert.equal(await page.locator('#files').isDisabled(), true);
  assert.equal(await page.locator('#gallery-section').isVisible(), true);
  assert.equal((await memberContext.request.get(origin + publicURL)).status(), 200);
  const bannedContext = await browser.newContext(), bannedPage = await bannedContext.newPage();
  result = await authenticate(bannedPage, 'login', member, false);
  assert.equal(result.code, 'account_banned'); assert.match(result.error, /banned/);
  result = await authenticate(bannedPage, 'login', {...member, password: randomBytes(24).toString('hex')}, false);
  assert.match(result.error, /Invalid username or password/); assert.equal(result.code, undefined);
  await formEvent(admin, target, () => row.getByRole('button', {name: 'Unban user', exact: true}).click());
  await wait(page, () => !document.querySelector('#files').disabled);
  assert.equal((await authenticate(bannedPage, 'login', member)).user.id, memberID);
  await bannedContext.close();

  await secondary.locator('#issue-token').click();
  await wait(secondary, () => document.querySelector('#upload-token').textContent.length > 0);
  const newPassword = randomBytes(24).toString('base64url');
  const passwordForm = row.locator('form').nth(1);
  await passwordForm.locator('input[type="password"]').nth(0).fill(newPassword);
  await passwordForm.locator('input[type="password"]').nth(1).fill(newPassword);
  await formEvent(admin, target + '/password', () => passwordForm.getByRole('button', {name: 'Reset password'}).click());
  await secondary.locator('#issue-token').click();
  await secondary.locator('#account-form').waitFor({state: 'visible', timeout: 1500});
  assert.match(await secondary.locator('#account-status').innerText(), /session has expired/);
  assert.equal(await secondary.locator('#upload-token').innerText(), '');
  await page.locator('#account-form').waitFor({state: 'visible', timeout: 10000});
  assert.equal(await page.locator('#upload-results > li').count(), 0);
  assert.equal(await page.locator('#gallery-section').isVisible(), false);
  assert.equal(await admin.locator('#admin-section').isVisible(), true);
  result = await authenticate(page, 'login', member, false); assert.match(result.error, /Invalid username or password/);
  assert.equal((await authenticate(page, 'login', {...member, password: newPassword})).user.id, memberID);
  assert.deepEqual(pageErrors, []);
  console.log(`Real Chromium ${browser.version()} passed: quota units/precision, rejected-upload progress, isolated result tabs, row details/paging/admin scope, ban/unban/login feedback, idle and request-driven reset sign-out.`);
} catch (error) {
  if (service?.exitCode !== null) console.error('Isolated service diagnostics:', serviceLog);
  throw error;
} finally {
  await browser?.close();
  if (service && service.exitCode === null && service.signalCode === null) {
    const exited = once(service, 'exit'); service.kill('SIGTERM');
    const forced = setTimeout(() => service.kill('SIGKILL'), 15000);
    await exited; clearTimeout(forced);
  }
  await fs.rm(temporary, {recursive: true, force: true});
}
