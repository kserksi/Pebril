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
  const value = await api('account');
  const previousID=account?.id;
  account = value.user || null;
  if(previousID!==account?.id) {
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
  accountStatus.textContent = account ? `Signed in as ${account.username}. Storage: ${account.used} / ${account.quota} bytes.` : initializing ? 'Create the only administrator to initialize this site.' : 'Sign in to upload images.';
  form.hidden = Boolean(account);
  document.querySelector('#account-submit').textContent = initializing ? 'Create administrator' : 'Sign in';
  for (const id of ['logout','token-controls','upload-section']) document.querySelector(`#${id}`).hidden = !account;
}
form.addEventListener('submit', async event => {
  event.preventDefault(); const button = document.querySelector('#account-submit'); button.disabled = true;
  try {await api(initializing ? 'initialize' : 'login', 'POST', Object.fromEntries(new FormData(form))); form.reset(); await loadAccount();}
  catch (error) {accountStatus.textContent = error.message;}
  finally {button.disabled = false;}
});
document.querySelector('#logout').addEventListener('click', async () => {try {await api('logout','POST'); document.querySelector('#upload-token').textContent=''; await loadAccount();} catch(error) {accountStatus.textContent=error.message;}});
document.querySelector('#issue-token').addEventListener('click', async () => {try {const value=await api('upload-token','POST');document.querySelector('#upload-token').textContent=value.token;} catch(error) {accountStatus.textContent=error.message;}});
document.querySelector('#revoke-token').addEventListener('click', async () => {try {await api('upload-token','DELETE');document.querySelector('#upload-token').textContent='Token revoked.';} catch(error) {accountStatus.textContent=error.message;}});
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
