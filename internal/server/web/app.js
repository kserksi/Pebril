'use strict';
const box = document.querySelector('#status');
const button = document.querySelector('#refresh');
async function refresh() {
  button.disabled = true;
  try {
    const response = await fetch('/api/v1/status', {cache: 'no-store'});
    if (!response.ok) throw new Error(`HTTP ${response.status}`);
    const data = await response.json();
    const list = document.createElement('dl');
    const rows = [['Version', data.version], ['Configuration revision', data.config_revision], ['SQLite', data.sqlite ? 'Available' : 'Failed'], ['libvips', data.native.libvips_version], ['SVG sanitization', data.native.svg ? 'Self-test passed' : 'Failed'], ['GIF animation', data.native.gif ? 'Self-test passed' : 'Failed'], ['AVIF', data.native.avif_enabled ? 'Enabled' : 'Disabled']];
    for (const [name, value] of rows) {
      const label = document.createElement('dt');
      const result = document.createElement('dd');
      label.textContent = name;
      result.textContent = String(value);
      list.append(label, result);
    }
    box.className = '';
    box.replaceChildren(list);
  } catch (error) {
    box.className = 'error';
    box.textContent = `Failed to read status: ${error.message}`;
  } finally { button.disabled = false; }
}
button.addEventListener('click', refresh);
refresh();

let account = null;
let csrf = '';
let initializing = false;
let registering = false;
let sessionRevision = 0;
let accountRequest = 0;
let usersRequest = 0;
let galleryOffset = 0;
let galleryRequest = 0;
let detailRequest = 0;
const galleryLimit = 20;
const accountStatus = document.querySelector('#account-status');
const form = document.querySelector('#account-form');
const results = [];
const galleryDetail = document.querySelector('#gallery-detail');
let uploading = false;
let accountRefreshing = false;
let signingOut = false;
async function api(path, method = 'GET', data) {
  const revision = sessionRevision;
  const authenticated = Boolean(account);
  const response = await fetch(`/api/v1/${path}`, {method, cache:'no-store', headers:{'Content-Type':'application/json', 'X-CSRF-Token':csrf}, body:data ? JSON.stringify(data) : undefined});
  if (response.status === 401 && authenticated && revision === sessionRevision) {
    expireSession();
    throw new Error('Your session has expired. Sign in again.');
  }
  const value = await response.json();
  if (!response.ok) throw new Error(value.error || `HTTP ${response.status}`);
  return value;
}
async function loadAccount(refreshLists = true) {
  const request = ++accountRequest;
  const revision = sessionRevision;
  const value = await api('account');
  if (request !== accountRequest || revision !== sessionRevision) return;
  const previousID = account?.id;
  applyAccount(value);
  if (refreshLists || previousID !== account?.id) {
    await Promise.all([account?.admin ? loadUsers() : undefined, account ? loadGallery() : undefined]);
  }
}
function applyAccount(value) {
  const previousID=account?.id;
  account = value.user || null;
  if(previousID!==account?.id) {
    sessionRevision++;
    clearAdmin();
    clearGallery();
    for (const row of results) row.deleted = true;
    results.splice(0);
    uploading = false;
    document.querySelector('#upload-results').replaceChildren();
    document.querySelector('#files').value='';
    document.querySelector('#upload-token').textContent='';
    document.querySelector('#total-progress').textContent='';
    if(account) {
      try {const failures=JSON.parse(localStorage.getItem(`pebril-upload-failures-${account.id}`) || '[]');if(failures.length) {document.querySelector('#total-progress').textContent=`Previous incomplete uploads: ${failures.map(item=>item.name).join(', ')}. Select the files again to retry from the beginning.`;}} catch { /* Cleared or unavailable browser storage has no recoverable history. */ }
    }
  }
  csrf = value.csrf || '';
  initializing = !value.initialized;
  accountStatus.textContent = account ? `Signed in as ${account.username}. Status: ${account.status || 'active'}. Storage: ${account.used} / ${account.quota} bytes.${account.status === 'banned' ? ' New uploads are disabled.' : ''}` : initializing ? 'Create the only administrator to initialize this site.' : 'Sign in or create an account to upload images.';
  accountStatus.className = account?.status === 'banned' ? 'error' : '';
  form.hidden = Boolean(account);
  if (initializing || account) registering = false;
  renderAccountMode();
  for (const id of ['logout','token-controls','upload-section']) document.querySelector(`#${id}`).hidden = !account;
  const uploadBlocked = !account || account.status === 'banned' || account.quota === 0;
  for (const id of ['files','upload']) document.querySelector(`#${id}`).disabled = uploadBlocked || uploading;
  document.querySelector('#logout').disabled = uploading;
  document.querySelector('#issue-token').disabled = !account;
  document.querySelector('#admin-section').hidden = !account?.admin;
  document.querySelector('#gallery-section').hidden = !account;
  document.querySelector('#gallery-all-label').hidden = !account?.admin;
  if (!account?.admin) document.querySelector('#gallery-all').checked = false;
}
function expireSession() {
  accountRequest++;
  applyAccount({initialized: !initializing});
  accountStatus.textContent = 'Your session has expired. Sign in again.';
}
async function refreshAccount() {
  if (!account || accountRefreshing || signingOut) return;
  accountRefreshing = true;
  try { await loadAccount(false); } catch { /* A network failure does not prove session revocation. */ }
  finally { accountRefreshing = false; }
}
setInterval(() => { if (!document.hidden) refreshAccount(); }, 5000);
document.addEventListener('visibilitychange', () => { if (!document.hidden) refreshAccount(); });
function renderAccountMode() {
  document.querySelector('#account-submit').textContent = initializing ? 'Create administrator' : registering ? 'Register' : 'Sign in';
  document.querySelector('#account-mode').hidden = initializing || Boolean(account);
  document.querySelector('#account-mode').textContent = registering ? 'Use an existing account' : 'Create an account';
  document.querySelector('#account-form input[name="password"]').autocomplete = initializing || registering ? 'new-password' : 'current-password';
}
document.querySelector('#account-mode').addEventListener('click', () => {
  registering = !registering;
  form.reset();
  renderAccountMode();
});
function showLoginWarnings(value) {
  if (value.user?.admin && Array.isArray(value.resource_warnings) && value.resource_warnings.length) {
    alert(value.resource_warnings.map(warning => warning.message).join('\n'));
  }
}
form.addEventListener('submit', async event => {
  event.preventDefault(); const button = document.querySelector('#account-submit'); button.disabled = true;
  try {const value=await api(initializing ? 'initialize' : registering ? 'register' : 'login', 'POST', Object.fromEntries(new FormData(form))); form.reset(); await loadAccount(); showLoginWarnings(value);}
  catch (error) {accountStatus.textContent = error.message;}
  finally {button.disabled = false;}
});
document.querySelector('#logout').addEventListener('click', async () => {signingOut=true;sessionRevision++; accountRequest++; clearAdmin(); clearGallery(); try {await api('logout','POST'); document.querySelector('#upload-token').textContent=''; await loadAccount();} catch(error) {accountStatus.textContent=error.message;} finally {signingOut=false;}});
document.querySelector('#issue-token').addEventListener('click', async () => {if(!account)return;const revision=sessionRevision;try {const value=await api('upload-token','POST');if(revision===sessionRevision)document.querySelector('#upload-token').textContent=value.token;} catch(error) {if(revision===sessionRevision)accountStatus.textContent=error.message;}});
document.querySelector('#revoke-token').addEventListener('click', async () => {const revision=sessionRevision;try {await api('upload-token','DELETE');if(revision===sessionRevision)document.querySelector('#upload-token').textContent='Token revoked.';} catch(error) {if(revision===sessionRevision)accountStatus.textContent=error.message;}});
function clearAdmin() {
  usersRequest++;
  document.querySelector('#admin-section').hidden = true;
  document.querySelector('#admin-users').replaceChildren();
  document.querySelector('#admin-status').textContent = '';
}
async function loadUsers() {
  if (!account?.admin) return;
  const revision = sessionRevision;
  const request = ++usersRequest;
  const current = () => revision === sessionRevision && request === usersRequest && account?.admin;
  document.querySelector('#admin-status').textContent = 'Loading users...';
  try {
    const value = await api('admin/users');
    if (!current()) return;
    const rows = value.users.map(user => userRow(user, revision));
    document.querySelector('#admin-users').replaceChildren(...rows);
    document.querySelector('#admin-status').textContent = `${rows.length} users.`;
  } catch (error) {
    if (current()) document.querySelector('#admin-status').textContent = error.message;
  }
}
const quotaUnits = {B: 1n, KiB: 1024n, MiB: 1048576n, GiB: 1073741824n};
function quotaBytes(value, unit) {
  if (!/^(0|[1-9][0-9]*)([.][0-9]+)?$/.test(value)) return null;
  const [whole, fraction = ''] = value.split('.');
  const scale = 10n ** BigInt(fraction.length);
  const amount = BigInt(whole + fraction) * quotaUnits[unit];
  if (amount % scale !== 0n || amount / scale > 9223372036854775807n) return null;
  return String(amount / scale);
}
function quotaValue(bytes, unit) {
  const divisor = quotaUnits[unit];
  const whole = BigInt(bytes) / divisor;
  let remainder = BigInt(bytes) % divisor;
  let fraction = '';
  while (remainder) {
    remainder *= 10n;
    fraction += String(remainder / divisor);
    remainder %= divisor;
  }
  return String(whole) + (fraction ? `.${fraction}` : '');
}
function userRow(user, revision) {
  const row = document.createElement('li');
  const summary = document.createElement('p');
  summary.textContent = `${user.username} — ${user.admin ? 'administrator' : 'user'}, ${user.status}, storage ${user.used} / ${user.quota} bytes.`;
  row.append(summary);
  if (user.admin) return row;
  const current = () => revision === sessionRevision && account?.admin;
  async function update(button, suffix, method, data) {
    if (!current()) return;
    button.disabled = true;
    try {
      await api(`admin/users/${user.id}${suffix}`, method, data);
      if (!current()) return;
      await loadUsers();
      if (current()) document.querySelector('#admin-status').textContent = `${user.username}: ${suffix ? 'Password reset. Share the new password outside Pebril.' : 'Account updated.'}`;
    } catch (error) {
      if (current()) document.querySelector('#admin-status').textContent = error.message;
    } finally { button.disabled = false; }
  }
  const status = document.createElement('button');
  status.type = 'button';
  status.textContent = user.status === 'banned' ? 'Unban user' : 'Ban user';
  status.addEventListener('click', () => update(status, '', 'PATCH', {status: user.status === 'banned' ? 'active' : 'banned'}));
  row.append(status);
  const quotaForm = document.createElement('form');
  const quotaLabel = document.createElement('label');
  quotaLabel.textContent = 'Storage quota ';
  const quota = document.createElement('input');
  let unit = Object.keys(quotaUnits).reverse().find(name => BigInt(user.quota) >= quotaUnits[name]) || 'B';
  quota.type = 'text'; quota.inputMode = 'decimal'; quota.pattern = '(0|[1-9][0-9]*)([.][0-9]+)?'; quota.maxLength = 64; quota.required = true; quota.value = quotaValue(user.quota, unit);
  quotaLabel.append(quota);
  const unitLabel = document.createElement('label'); unitLabel.textContent = 'Unit ';
  const units = document.createElement('select');
  for (const name of Object.keys(quotaUnits)) {
    const option = document.createElement('option'); option.value = name; option.textContent = name; units.append(option);
  }
  units.value = unit; unitLabel.append(units);
  const quotaError = () => { document.querySelector('#admin-status').textContent = 'Quota must convert exactly to whole bytes from 0 to 9223372036854775807. No rounding is applied.'; };
  units.addEventListener('change', () => {
    const bytes = quotaBytes(quota.value, unit);
    if (bytes === null) { units.value = unit; quotaError(); return; }
    unit = units.value; quota.value = quotaValue(bytes, unit);
  });
  const saveQuota = document.createElement('button');
  saveQuota.type = 'submit'; saveQuota.textContent = 'Save quota';
  quotaForm.append(quotaLabel, unitLabel, saveQuota);
  quotaForm.addEventListener('submit', event => {
    event.preventDefault();
    if (!current()) return;
    const value = quotaBytes(quota.value, unit);
    if (value === null) { quotaError(); return; }
    return update(saveQuota, '', 'PATCH', {quota: value});
  });
  row.append(quotaForm);
  const passwordForm = document.createElement('form');
  const passwordLabel = document.createElement('label');
  passwordLabel.textContent = 'New password ';
  const password = document.createElement('input');
  password.type = 'password'; password.autocomplete = 'new-password'; password.maxLength = 72; password.required = true;
  passwordLabel.append(password);
  const confirmLabel = document.createElement('label');
  confirmLabel.textContent = 'Confirm new password ';
  const confirmation = document.createElement('input');
  confirmation.type = 'password'; confirmation.autocomplete = 'new-password'; confirmation.maxLength = 72; confirmation.required = true;
  confirmLabel.append(confirmation);
  const reset = document.createElement('button');
  reset.type = 'submit'; reset.textContent = 'Reset password';
  passwordForm.append(passwordLabel, confirmLabel, reset);
  passwordForm.addEventListener('submit', event => {
    event.preventDefault();
    if (!current()) { passwordForm.reset(); return; }
    const value = password.value;
    const matches = value !== '' && value === confirmation.value;
    passwordForm.reset();
    if (!matches) {
      document.querySelector('#admin-status').textContent = 'Enter and confirm the same new password.';
      return;
    }
    return update(reset, '/password', 'POST', {password: value});
  });
  row.append(passwordForm);
  return row;
}
document.querySelector('#refresh-users').addEventListener('click', loadUsers);
function uploadFile(file, row, progress) {
  return new Promise((resolve,reject) => {
    const revision = sessionRevision;
    const request=new XMLHttpRequest(); request.open('POST','/api/v1/uploads'); request.setRequestHeader('X-CSRF-Token',csrf);
    request.upload.onprogress=event => {if(!row.deleted && revision===sessionRevision && event.lengthComputable) {progress.removeAttribute('value');row.status.textContent=`Received ${event.loaded}/${event.total} bytes; final image pending.`;}};
    request.onload=() => {if(request.status===401 && revision===sessionRevision){expireSession();reject(new Error('Your session has expired. Sign in again.'));return;}try {const value=JSON.parse(request.responseText);if(request.status>=200 && request.status<300) resolve(value);else reject(new Error(value.error || 'Upload failed'));}catch(error){reject(error);}};
    request.onerror=() => reject(new Error('Network failure; upload outcome may require checking this page after signing in again.'));
    const body=new FormData();body.append('file',file);request.send(body);
  });
}
function updateTotal() {
  const completed=results.filter(row => row.image?.state==='completed').length;
  const failed=results.filter(row => row.failed).length;
  document.querySelector('#total-progress').textContent=`Completed: ${completed}/${results.length}. Failed: ${failed}.`;
}
function savePending() {
  if(!account)return;
  try {localStorage.setItem(`pebril-upload-failures-${account.id}`,JSON.stringify(results.filter(row=>row.failed || !row.image).map(row=>({name:row.name,id:row.image?.id || null}))));} catch { /* Browser storage is optional. */ }
}
document.querySelector('#upload').addEventListener('click', async () => {
  if(!account || account.status==='banned' || account.quota===0)return;
  const files=Array.from(document.querySelector('#files').files);if(!files.length)return;
  const revision = sessionRevision;
  uploading = true;
  const button=document.querySelector('#upload');button.disabled=true;document.querySelector('#logout').disabled=true;
  for(const file of files){const item=document.createElement('li');const name=document.createElement('span');name.textContent=file.name;const status=document.createElement('p');status.textContent='0% pending upload';const progress=document.createElement('progress');progress.max=100;progress.value=0;progress.setAttribute('aria-label',`Upload progress for ${file.name}`);item.append(name,status,progress);document.querySelector('#upload-results').append(item);results.push({name:file.name,file,item,status,progress});}
  savePending();updateTotal();
  try {
    for(const row of results.filter(row=>row.file && !row.image && !row.failed)){
      if (revision !== sessionRevision) break;
      try {const image=await uploadFile(row.file,row,row.progress);if(revision!==sessionRevision)break;row.image=image;renderImage(row);if(!['completed','failed'].includes(row.image.state)){watchImage(row,account.id);}}
      catch(error){if(revision!==sessionRevision)break;row.failed=true;row.progress.hidden=true;row.status.textContent=error.message;}
      row.file=null;savePending();updateTotal();
    }
  } finally {
    if (revision === sessionRevision) {
      uploading=false;button.disabled=false;document.querySelector('#logout').disabled=false;document.querySelector('#files').value='';await loadAccount();
    }
  }
});
async function copyLinks(markdown) {
  const text=results.filter(row=>row.image?.url).map(row=>{const url=new URL(row.image.url,location.href).href;return markdown ? `![](${url})` : url;}).join('\n');
  try {await navigator.clipboard.writeText(text);} catch {document.querySelector('#total-progress').textContent='Clipboard unavailable. Copy the displayed links manually.';}
}
document.querySelector('#copy-urls').addEventListener('click',()=>copyLinks(false));
document.querySelector('#copy-markdown').addEventListener('click',()=>copyLinks(true));
loadAccount().catch(error => {accountStatus.textContent=error.message;});

function renderImage(row) {
  if (row.deleted) return;
  const image=row.image;
  row.failed=['failed','retry_wait'].includes(image.state);
  row.progress.hidden = image.state === 'failed';
  if(image.state==='completed' && image.url) {
    row.progress.value=100;
    const link=document.createElement('a');link.href=image.url;link.textContent=new URL(image.url,location.href).href;
    row.status.replaceChildren('100% ready: ',link);
  } else {
    row.progress.removeAttribute('value');
    row.status.textContent=image.error ? `${image.error}${image.state==='retry_wait' ? ' Automatic retry pending.' : ''}` : `Processing: ${image.state}.`;
    if(image.temporary && image.url) {
      const link=document.createElement('a');link.href=image.url;link.textContent='Temporary original link';row.status.append(' ',link);
    }
    if(image.state==='failed' && image.retryable) {
      const button=document.createElement('button');button.type='button';button.textContent='Retry conversion';
      button.addEventListener('click',async()=>{
        if (row.deleted || !account) return;
        const revision = sessionRevision;
        button.disabled=true;
        try {const updated=await api(`uploads/${image.id}/retry`,'POST');if(row.deleted || revision!==sessionRevision)return;row.image=updated;row.failed=false;renderImage(row);watchImage(row,account.id);}
        catch(error){if(!row.deleted && revision===sessionRevision)row.status.textContent=error.message;}finally{button.disabled=false;}
      });
      row.status.append(' ',button);
    }
  }
  savePending();updateTotal();
}
async function watchImage(row,owner) {
  const revision = sessionRevision;
  while(!row.deleted && revision===sessionRevision && account?.id===owner && !['completed','failed'].includes(row.image.state)) {
    await new Promise(resolve=>setTimeout(resolve,1000));
    if(row.deleted || revision!==sessionRevision || account?.id!==owner)return;
    try {const image=await api(`uploads/${row.image.id}`);if(row.deleted || revision!==sessionRevision || account?.id!==owner)return;row.image=image;renderImage(row);}
    catch(error){if(!row.deleted && revision===sessionRevision && account?.id===owner)row.status.textContent=`Cannot refresh processing result: ${error.message}`;return;}
  }
  if(!row.deleted && revision===sessionRevision && account?.id===owner)loadAccount().catch(()=>{});
}

function clearGallery() {
  galleryRequest++;
  detailRequest++;
  galleryOffset = 0;
  document.querySelector('#gallery-section').hidden = true;
  document.querySelector('#gallery-all').checked = false;
  document.querySelector('#gallery-images').replaceChildren();
  galleryDetail.replaceChildren();
  document.querySelector('#gallery-status').textContent = '';
  document.querySelector('#gallery-previous').disabled = true;
  document.querySelector('#gallery-next').disabled = true;
}
async function loadGallery() {
  if (!account || document.querySelector('#gallery-section').hidden) return;
  const revision = sessionRevision;
  const request = ++galleryRequest;
  const current = () => revision === sessionRevision && request === galleryRequest && account;
  detailRequest++;
  document.querySelector('#gallery-images').replaceChildren();
  galleryDetail.replaceChildren();
  document.querySelector('#gallery-status').textContent = 'Loading images...';
  document.querySelector('#gallery-previous').disabled = true;
  document.querySelector('#gallery-next').disabled = true;
  const all = account.admin && document.querySelector('#gallery-all').checked;
  try {
    const value = await api(`images?limit=${galleryLimit}&offset=${galleryOffset}${all ? '&scope=all' : ''}`);
    if (!current()) return;
    if (galleryOffset && galleryOffset >= value.total) {
      galleryOffset = Math.max(0, Math.floor((value.total - 1) / galleryLimit) * galleryLimit);
      return loadGallery();
    }
    document.querySelector('#gallery-images').replaceChildren(...value.images.map(image => {
      const row = document.createElement('li');
      const summary = document.createElement('p');
      summary.textContent = `${image.id} — ${image.state}, ${image.format || 'format pending'}, ${image.storage_bytes} stored bytes, uploaded ${new Date(image.created_at).toISOString()}.${all ? ` Owner: ${image.owner_id}.` : ''}`;
      const inspect = document.createElement('button');
      inspect.type = 'button'; inspect.textContent = 'View image details';
      inspect.addEventListener('click', () => { if (current()) return inspectImage(image.id, current, row); });
      row.append(summary, inspect);
      return row;
    }));
    document.querySelector('#gallery-status').textContent = value.total ? `Images ${galleryOffset + 1}–${galleryOffset + value.images.length} of ${value.total}.` : 'No images.';
    document.querySelector('#gallery-previous').disabled = galleryOffset === 0;
    document.querySelector('#gallery-next').disabled = galleryOffset + galleryLimit >= value.total;
  } catch (error) {
    if (current()) document.querySelector('#gallery-status').textContent = error.message;
  }
}
async function inspectImage(id, listCurrent, row) {
  const revision = sessionRevision;
  const request = ++detailRequest;
  const current = () => listCurrent() && request === detailRequest;
  const detail = galleryDetail;
  row.append(detail);
  detail.textContent = 'Loading image details...';
  try {
    const image = await api(`images/${encodeURIComponent(id)}`);
    if (!current()) return;
    const title = document.createElement('h3'); title.textContent = `Image ${image.id}`;
    const summary = document.createElement('p');
    summary.textContent = `Owner: ${image.owner_id}. State: ${image.state}. Format: ${image.format || 'pending'}. Dimensions: ${image.width || 0} × ${image.height || 0}. Frames: ${image.frames || 0}. Stored bytes: ${image.storage_bytes}. Uploaded: ${new Date(image.created_at).toISOString()}.${image.error ? ` Error: ${image.error}` : ''}`;
    const files = document.createElement('ul');
    for (const file of image.files) {
      const row = document.createElement('li');
      row.textContent = `${file.role}: ${file.format}, ${file.bytes} bytes, ${file.width} × ${file.height}, ${file.frames} frames.`;
      files.append(row);
    }
    detail.replaceChildren(title, summary, files);
    if (image.state === 'completed' && image.url) {
      const link = document.createElement('a'); link.href = image.url; link.textContent = 'Open public result'; link.target = '_blank'; link.rel = 'noopener'; detail.append(link);
    }
    if (image.cover_url) {
      const cover = document.createElement('a'); cover.href = image.cover_url; cover.textContent = 'Open cover'; detail.append(' ', cover);
    }
    if (image.original_available) {
      const original = document.createElement('a'); original.href = `/api/v1/images/${encodeURIComponent(id)}/original`; original.textContent = 'Download original'; detail.append(' ', original);
    }
    const remove = document.createElement('button'); remove.type = 'button'; remove.textContent = 'Delete image';
    remove.addEventListener('click', async () => {
      if (!current() || !confirm(`Delete image ${id} and its links? This cannot be undone.`)) return;
      remove.disabled = true;
      try {
        await api(`images/${encodeURIComponent(id)}`, 'DELETE');
        if (revision !== sessionRevision) return;
        for (let index = results.length - 1; index >= 0; index--) {
          if (results[index].image?.id === id) { results[index].deleted = true; results.splice(index, 1); }
        }
        document.querySelector('#upload-results').replaceChildren(...results.map(row => row.item));
        savePending(); updateTotal();
        if (!current()) return;
        await loadAccount();
      } catch (error) {
        if (current()) document.querySelector('#gallery-status').textContent = error.message;
      } finally { remove.disabled = false; }
    });
    detail.append(remove);
  } catch (error) {
    if (current()) detail.textContent = error.message;
  }
}
document.querySelector('#refresh-gallery').addEventListener('click', loadGallery);
document.querySelector('#gallery-all').addEventListener('change', () => { galleryOffset = 0; return loadGallery(); });
document.querySelector('#gallery-previous').addEventListener('click', () => { galleryOffset = Math.max(0, galleryOffset - galleryLimit); return loadGallery(); });
document.querySelector('#gallery-next').addEventListener('click', () => { galleryOffset += galleryLimit; return loadGallery(); });
