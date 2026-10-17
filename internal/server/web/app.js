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
const accountStatus = document.querySelector('#account-status');
const form = document.querySelector('#account-form');
const results = [];
async function api(path, method = 'GET', data) {
  const response = await fetch(`/api/v1/${path}`, {method, cache:'no-store', headers:{'Content-Type':'application/json', 'X-CSRF-Token':csrf}, body:data ? JSON.stringify(data) : undefined});
  const value = await response.json();
  if (!response.ok) throw new Error(value.error || `HTTP ${response.status}`);
  return value;
}
async function loadAccount() {
  const request = ++accountRequest;
  const revision = sessionRevision;
  const value = await api('account');
  if (request !== accountRequest || revision !== sessionRevision) return;
  const previousID=account?.id;
  account = value.user || null;
  if(previousID!==account?.id) {
    sessionRevision++;
    clearAdmin();
    results.splice(0);
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
  form.hidden = Boolean(account);
  if (initializing || account) registering = false;
  renderAccountMode();
  for (const id of ['logout','token-controls','upload-section']) document.querySelector(`#${id}`).hidden = !account;
  const uploadBlocked = !account || account.status === 'banned' || account.quota === 0;
  for (const id of ['files','upload']) document.querySelector(`#${id}`).disabled = uploadBlocked;
  document.querySelector('#issue-token').disabled = !account;
  document.querySelector('#admin-section').hidden = !account?.admin;
  if (account?.admin) await loadUsers();
}
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
document.querySelector('#logout').addEventListener('click', async () => {sessionRevision++; accountRequest++; clearAdmin(); try {await api('logout','POST'); document.querySelector('#upload-token').textContent=''; await loadAccount();} catch(error) {accountStatus.textContent=error.message;}});
document.querySelector('#issue-token').addEventListener('click', async () => {if(!account)return;const revision=sessionRevision;try {const value=await api('upload-token','POST');if(revision===sessionRevision)document.querySelector('#upload-token').textContent=value.token;} catch(error) {if(revision===sessionRevision)accountStatus.textContent=error.message;}});
document.querySelector('#revoke-token').addEventListener('click', async () => {try {await api('upload-token','DELETE');document.querySelector('#upload-token').textContent='Token revoked.';} catch(error) {accountStatus.textContent=error.message;}});
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
  quotaLabel.textContent = 'Quota in bytes ';
  const quota = document.createElement('input');
  quota.type = 'text'; quota.inputMode = 'numeric'; quota.pattern = '0|[1-9][0-9]*'; quota.maxLength = 19; quota.required = true; quota.value = user.quota;
  quotaLabel.append(quota);
  const saveQuota = document.createElement('button');
  saveQuota.type = 'submit'; saveQuota.textContent = 'Save quota';
  quotaForm.append(quotaLabel, saveQuota);
  quotaForm.addEventListener('submit', event => {
    event.preventDefault();
    if (!current()) return;
    const value = quota.value;
    if (!/^(0|[1-9][0-9]*)$/.test(value) || BigInt(value) > 9223372036854775807n) {
      document.querySelector('#admin-status').textContent = 'Quota must be a whole number of bytes from 0 to 9223372036854775807, without leading zeros.';
      return;
    }
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
    const request=new XMLHttpRequest(); request.open('POST','/api/v1/uploads'); request.setRequestHeader('X-CSRF-Token',csrf);
    request.upload.onprogress=event => {if(event.lengthComputable) {progress.removeAttribute('value');row.status.textContent=`Received ${event.loaded}/${event.total} bytes; final image pending.`;}};
    request.onload=() => {try {const value=JSON.parse(request.responseText);if(request.status>=200 && request.status<300) resolve(value);else reject(new Error(value.error || 'Upload failed'));}catch(error){reject(error);}};
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
  const button=document.querySelector('#upload');button.disabled=true;document.querySelector('#logout').disabled=true;
  for(const file of files){const item=document.createElement('li');const name=document.createElement('span');name.textContent=file.name;const status=document.createElement('p');status.textContent='0% pending upload';const progress=document.createElement('progress');progress.max=100;progress.value=0;progress.setAttribute('aria-label',`Upload progress for ${file.name}`);item.append(name,status,progress);document.querySelector('#upload-results').append(item);results.push({name:file.name,file,item,status,progress});}
  savePending();updateTotal();
  for(const row of results.filter(row=>row.file && !row.image && !row.failed)){
    try {row.image=await uploadFile(row.file,row,row.progress);renderImage(row);if(!['completed','failed'].includes(row.image.state)){watchImage(row,account.id);}}
    catch(error){row.failed=true;row.status.textContent=error.message;}
    row.file=null;savePending();updateTotal();
  }
  button.disabled=false;document.querySelector('#logout').disabled=false;document.querySelector('#files').value='';await loadAccount();
});
async function copyLinks(markdown) {
  const text=results.filter(row=>row.image?.url).map(row=>{const url=new URL(row.image.url,location.href).href;return markdown ? `![](${url})` : url;}).join('\n');
  try {await navigator.clipboard.writeText(text);} catch {document.querySelector('#total-progress').textContent='Clipboard unavailable. Copy the displayed links manually.';}
}
document.querySelector('#copy-urls').addEventListener('click',()=>copyLinks(false));
document.querySelector('#copy-markdown').addEventListener('click',()=>copyLinks(true));
loadAccount().catch(error => {accountStatus.textContent=error.message;});

function renderImage(row) {
  const image=row.image;
  row.failed=['failed','retry_wait'].includes(image.state);
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
        button.disabled=true;
        try {row.image=await api(`uploads/${image.id}/retry`,'POST');row.failed=false;renderImage(row);watchImage(row,account.id);}
        catch(error){row.status.textContent=error.message;}finally{button.disabled=false;}
      });
      row.status.append(' ',button);
    }
  }
  savePending();updateTotal();
}
async function watchImage(row,owner) {
  while(account?.id===owner && !['completed','failed'].includes(row.image.state)) {
    await new Promise(resolve=>setTimeout(resolve,1000));
    if(account?.id!==owner)return;
    try {const image=await api(`uploads/${row.image.id}`);if(account?.id!==owner)return;row.image=image;renderImage(row);}
    catch(error){row.status.textContent=`Cannot refresh processing result: ${error.message}`;return;}
  }
  if(account?.id===owner)loadAccount().catch(()=>{});
}
