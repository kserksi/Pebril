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
