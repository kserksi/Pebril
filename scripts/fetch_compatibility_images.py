#!/usr/bin/env python3
"""Pinned official Linux userspaces; extract only to the WSL build directory without installing Docker."""
import hashlib
import json
import os
from pathlib import Path
import posixpath
import shutil
import tarfile
import urllib.request
ROOT = Path(__file__).resolve().parent.parent
ACCEPT = 'application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json'
def filtered(member, destination):
    if member.isdev():
        return None
    if member.issym() and member.linkname.startswith('/'):
        member = member.replace(linkname=posixpath.relpath(member.linkname.lstrip('/'), posixpath.dirname(member.name)))
    return tarfile.data_filter(member, destination)

def fetch(entry):
    directory = ROOT / '.build/compat' / entry['name']
    if (directory / '.pebril-rootfs-ready').exists():
        return
    directory.mkdir(parents=True, exist_ok=True)
    repo = entry['repository']
    token_url = f'https://auth.docker.io/token?service=registry.docker.io&scope=repository:{repo}:pull'
    with urllib.request.urlopen(token_url, timeout=30) as response:
        token = json.load(response)['token']
    base = f'https://registry-1.docker.io/v2/{repo}/'
    headers = {'Authorization': 'Bearer ' + token, 'Accept': ACCEPT}
    with urllib.request.urlopen(urllib.request.Request(base + 'manifests/' + entry['digest'], headers=headers), timeout=30) as response:
        raw = response.read()
    assert 'sha256:' + hashlib.sha256(raw).hexdigest() == entry['digest']
    manifest = json.loads(raw)
    for layer in manifest['layers']:
        archive = directory.parent / (layer['digest'].split(':')[1] + '.tar')
        with urllib.request.urlopen(urllib.request.Request(base + 'blobs/' + layer['digest'], headers=headers), timeout=90) as response, archive.open('wb') as output:
            shutil.copyfileobj(response, output)
        assert 'sha256:' + hashlib.sha256(archive.read_bytes()).hexdigest() == layer['digest']
        with tarfile.open(archive) as tar:
            tar.extractall(directory, filter=filtered)
        archive.unlink()
    release = (directory / 'etc/os-release').read_text()
    values = dict(line.split('=', 1) for line in release.splitlines() if '=' in line)
    assert values['ID'].strip('"') == entry['expected_id'], release
    assert values['VERSION_ID'].strip('"') == entry['expected_version'], release
    (directory / '.pebril-rootfs-ready').write_text(entry['digest'])
    print(entry['name'], entry['digest'], 'Official userspace prepared', flush=True)

if __name__ == '__main__':
    from concurrent.futures import ThreadPoolExecutor
    entries = json.loads((ROOT / 'scripts/compatibility-images.json').read_text())
    with ThreadPoolExecutor(max_workers=3) as executor:
        list(executor.map(fetch, entries))
