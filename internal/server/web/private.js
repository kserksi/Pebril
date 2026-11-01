'use strict';

// The page grant is kept only in this variable. Nothing here persists it, so
// reloading, restoring or reopening this page requires the access key again.
let grant = '';

const page = document.querySelector('#private-page');
const imageID = page.dataset.imageId;
const form = document.querySelector('#private-form');
const key = document.querySelector('#key');
const submit = document.querySelector('#private-submit');
const status = document.querySelector('#private-status');
const content = document.querySelector('#private-content');
const image = document.querySelector('#private-image');
const original = document.querySelector('#private-original');
const note = document.querySelector('#private-note');
const retry = document.querySelector('#private-retry');

let verifying = false;
let downloading = false;
let contentRequest = 0;
let imageURL = '';
let originalURL = '';

function setStatus(text, failed) {
  status.textContent = text;
  status.className = failed ? 'error' : '';
}

function releaseImage() {
  if (!imageURL) return;
  image.removeAttribute('src');
  image.hidden = true;
  URL.revokeObjectURL(imageURL);
  imageURL = '';
}

function releaseOriginal() {
  if (!originalURL) return;
  URL.revokeObjectURL(originalURL);
  originalURL = '';
}

function clearContent() {
  content.hidden = true;
  original.hidden = true;
  retry.hidden = true;
  note.textContent = '';
  note.className = '';
  releaseOriginal();
}

function showKeyForm(text) {
  grant = '';
  contentRequest++;
  releaseImage();
  clearContent();
  form.hidden = false;
  submit.disabled = false;
  key.value = '';
  setStatus(text, true);
  key.focus();
}

function showUnavailable(text) {
  grant = '';
  contentRequest++;
  releaseImage();
  clearContent();
  form.hidden = true;
  setStatus(text, true);
}

function showRetry(text) {
  setStatus('', false);
  content.hidden = false;
  note.textContent = text;
  note.className = 'error';
  retry.hidden = false;
}

function verifyMessage(code, error) {
  if (code === 400) return 'Enter a valid access key.';
  if (code === 401) return 'That access key is not valid.';
  if (code === 403) {
    if (error === 'key_revoked') return 'This access key has been revoked.';
    if (error === 'key_expired') return 'This access key has expired.';
    return 'The access key could not be verified. Try again.';
  }
  if (code === 429) return 'Too many attempts. Try again later.';
  if (code === 503) return 'The private page is temporarily unavailable. Try again later.';
  return 'The access key could not be verified. Try again.';
}

function downloadName(response) {
  const disposition = response.headers.get('Content-Disposition') || '';
  const match = /filename="?([^";]+)"?/.exec(disposition);
  return match ? match[1] : 'image';
}

async function loadContent() {
  const request = ++contentRequest;
  releaseImage();
  clearContent();
  setStatus('Loading the private image...', false);
  try {
    const response = await fetch(`/p/${encodeURIComponent(imageID)}/content`, {
      cache: 'no-store',
      credentials: 'omit',
      headers: {'X-Pebril-Grant': grant},
    });
    if (request !== contentRequest) return;
    if (response.status === 401) {
      showKeyForm('Your access has expired. Enter the access key again.');
      return;
    }
    if (response.status === 404) {
      showUnavailable('This image is no longer available.');
      return;
    }
    if (response.status === 503) {
      showRetry('The image storage is temporarily unavailable. Try again later.');
      return;
    }
    if (!response.ok) {
      showRetry('The private image could not be loaded. Try again later.');
      return;
    }
    const blob = await response.blob();
    if (request !== contentRequest) return;
    if (!blob.type.startsWith('image/')) {
      showRetry('The private image could not be displayed. Try again later.');
      return;
    }
    imageURL = URL.createObjectURL(blob);
    image.src = imageURL;
    image.hidden = false;
    content.hidden = false;
    setStatus('', false);
    await checkOriginal(request);
  } catch (error) {
    if (request !== contentRequest) return;
    showRetry('The private image could not be loaded. Check your connection and try again.');
  }
}

async function checkOriginal(request) {
  try {
    const response = await fetch(`/p/${encodeURIComponent(imageID)}/content/original`, {
      method: 'HEAD',
      cache: 'no-store',
      credentials: 'omit',
      headers: {'X-Pebril-Grant': grant},
    });
    if (request !== contentRequest) return;
    original.hidden = !response.ok;
  } catch (error) {
    original.hidden = true;
  }
}

async function verify(event) {
  event.preventDefault();
  if (verifying) return;
  const value = key.value.trim();
  if (!value) {
    setStatus('Enter the access key.', true);
    key.focus();
    return;
  }
  verifying = true;
  submit.disabled = true;
  setStatus('Checking the access key...', false);
  try {
    const response = await fetch(`/api/v1/private/${encodeURIComponent(imageID)}/verify`, {
      method: 'POST',
      cache: 'no-store',
      credentials: 'omit',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({key: value}),
    });
    if (response.status === 429) {
      const seconds = Number(response.headers.get('Retry-After'));
      setStatus(seconds > 0 ? `Too many attempts. Try again in ${Math.ceil(seconds)} seconds.` : 'Too many attempts. Try again later.', true);
      return;
    }
    if (!response.ok) {
      let error = '';
      try {
        error = (await response.json()).error || '';
      } catch (ignored) {
        // A non-JSON failure still uses the generic message for its status.
      }
      setStatus(verifyMessage(response.status, error), true);
      return;
    }
    let data = null;
    try {
      data = await response.json();
    } catch (ignored) {
      // Handled by the grant shape check below.
    }
    if (!data || typeof data.grant !== 'string' || !/^[0-9a-f]{64}$/.test(data.grant)) {
      setStatus('The access key could not be verified. Try again.', true);
      return;
    }
    grant = data.grant;
    key.value = '';
    form.hidden = true;
    await loadContent();
  } catch (error) {
    setStatus('The request could not be completed. Check your connection and try again.', true);
  } finally {
    verifying = false;
    submit.disabled = false;
  }
}

form.addEventListener('submit', verify);
retry.addEventListener('click', () => {
  if (grant) loadContent();
});
original.addEventListener('click', async (event) => {
  event.preventDefault();
  if (!grant || downloading) return;
  downloading = true;
  note.textContent = 'Preparing the original download...';
  note.className = '';
  try {
    const response = await fetch(`/p/${encodeURIComponent(imageID)}/content/original`, {
      cache: 'no-store',
      credentials: 'omit',
      headers: {'X-Pebril-Grant': grant},
    });
    if (response.status === 401) {
      showKeyForm('Your access has expired. Enter the access key again.');
      return;
    }
    if (response.status === 404) {
      original.hidden = true;
      note.textContent = 'The original download is not available.';
      note.className = 'error';
      return;
    }
    if (!response.ok) {
      note.textContent = 'The original download could not be prepared. Try again later.';
      note.className = 'error';
      return;
    }
    const blob = await response.blob();
    releaseOriginal();
    originalURL = URL.createObjectURL(blob);
    const link = document.createElement('a');
    link.href = originalURL;
    link.download = downloadName(response);
    document.body.append(link);
    link.click();
    link.remove();
    note.textContent = 'The original image download has started.';
  } catch (error) {
    note.textContent = 'The original download could not be completed. Try again later.';
    note.className = 'error';
  } finally {
    downloading = false;
  }
});
window.addEventListener('pageshow', (event) => {
  if (!event.persisted) return;
  showKeyForm('Enter the access key again to load the private image.');
});
form.hidden = false;
key.focus();
