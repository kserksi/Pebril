#!/usr/bin/env python3
"""Publish checked development packages without replacing published releases or tags."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess
import tarfile

DEFAULT_NOTES = '''Early development prerelease. Not ready for production use.

Built and tested by GitHub Actions. Download the Linux x64 archive and SHA256SUMS below.
'''


def verify_package(directory, version, commit):
    if not re.fullmatch(r'(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-dev', version):
        raise ValueError('Expected a development VERSION')
    if not re.fullmatch(r'[0-9a-f]{40}', commit):
        raise ValueError('Expected a full source commit')
    name = f'pebril-{version}-linux-amd64-{commit[:12]}'
    archive = directory / (name + '.tar.gz')
    sums = directory / 'SHA256SUMS'
    if set(directory.iterdir()) != {archive, sums} or any(p.is_symlink() or not p.is_file() for p in (archive, sums)):
        raise ValueError('Expected exactly one archive and SHA256SUMS')
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    if sums.read_text() != f'{digest}  {archive.name}\n':
        raise ValueError('Archive checksum mismatch')
    with tarfile.open(archive) as source:
        members = source.getmembers()
        if any(not m.isfile() or not m.name.startswith(name + '/') for m in members):
            raise ValueError('Unsafe archive member')
        files = {m.name[len(name) + 1:]: m for m in members}
        if len(files) != len(members) or any(not n or '..' in n.split('/') or n.startswith('/') for n in files):
            raise ValueError('Unsafe or duplicate archive path')
        inventory = json.loads(source.extractfile(files['checksums.json']).read())
        if set(files) != set(inventory) | {'checksums.json'}:
            raise ValueError('Package inventory mismatch')
        for path, checksum in inventory.items():
            if hashlib.sha256(source.extractfile(files[path]).read()).hexdigest() != checksum:
                raise ValueError(f'Package file checksum mismatch: {path}')
        identity = json.loads(source.extractfile(files['build.json']).read())
        if (identity.get('source_commit') != commit or identity.get('version') != version
                or identity.get('source_dirty') is not False
                or source.extractfile(files['VERSION']).read().decode().strip() != version):
            raise ValueError('Package source/version identity mismatch')
    return {p: {'size': p.stat().st_size, 'digest': 'sha256:' + hashlib.sha256(p.read_bytes()).hexdigest()}
            for p in (archive, sums)}


def api(endpoint, *, method='GET', data=None, optional=False):
    command = ['gh', 'api', endpoint, '--method', method]
    if data is not None:
        command += ['--input', '-']
    result = subprocess.run(command, input=json.dumps(data) if data is not None else None,
                            text=True, capture_output=True)
    if result.returncode:
        if optional and '(HTTP 404)' in result.stderr:
            return None
        raise RuntimeError(result.stderr.strip() or 'GitHub API request failed')
    return json.loads(result.stdout)


def verify_assets(release, expected):
    assets = {a['name']: a for a in release['assets']}
    if len(assets) != len(release['assets']) or set(assets) != {p.name for p in expected}:
        raise ValueError('Release asset inventory mismatch')
    for path, metadata in expected.items():
        asset = assets[path.name]
        if asset.get('state') != 'uploaded' or any(asset.get(k) != v for k, v in metadata.items()):
            raise ValueError(f'Release asset checksum or size mismatch: {path.name}')


def publish(repo, version, commit, expected, notes):
    endpoint = f'repos/{repo}'
    tag = 'v' + version
    release = api(f'{endpoint}/releases/tags/{tag}', optional=True)
    if release and not release['draft']:
        print(f'Already published; leaving {tag} and its assets unchanged: {release["html_url"]}')
        return
    reference = api(f'{endpoint}/git/ref/tags/{tag}', optional=True)
    if reference is None:
        api(f'{endpoint}/git/refs', method='POST', data={'ref': f'refs/tags/{tag}', 'sha': commit})
    elif reference['object']['type'] != 'commit' or reference['object']['sha'] != commit:
        raise ValueError('Existing tag does not point directly to this source commit')
    # Re-read the immutable target before creating or resuming a draft.
    reference = api(f'{endpoint}/git/ref/tags/{tag}')
    if reference['object']['type'] != 'commit' or reference['object']['sha'] != commit:
        raise ValueError('Release tag target mismatch')
    title = f'Pebril {version}'
    if release is None:
        release = api(f'{endpoint}/releases', method='POST', data={
            'tag_name': tag, 'target_commitish': commit, 'name': title, 'body': notes,
            'draft': True, 'prerelease': True, 'make_latest': 'false'})
    if (release['tag_name'] != tag or release['target_commitish'] != commit
            or release['name'] != title or release['body'] != notes or not release['prerelease']):
        raise ValueError('Existing draft does not match the requested release')
    assets = {a['name']: a for a in release['assets']}
    if not set(assets) <= {p.name for p in expected}:
        raise ValueError('Unexpected draft assets; refusing to delete or replace them')
    for path, metadata in expected.items():
        if path.name in assets:
            asset = assets[path.name]
            if asset.get('state') != 'uploaded' or any(asset.get(k) != v for k, v in metadata.items()):
                raise ValueError('Existing draft asset differs; refusing to overwrite it')
        else:
            subprocess.run(['gh', 'release', 'upload', tag, str(path), '--repo', repo], check=True)
    release = api(f'{endpoint}/releases/{release["id"]}')
    verify_assets(release, expected)
    release = api(f'{endpoint}/releases/{release["id"]}', method='PATCH', data={
        'draft': False, 'prerelease': True, 'make_latest': 'false'})
    verify_assets(release, expected)
    if release['draft'] or not release['prerelease']:
        raise ValueError('Release publication state mismatch')
    print(f'Published verified prerelease: {release["html_url"]}')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--artifacts', type=Path, required=True)
    parser.add_argument('--commit', required=True)
    parser.add_argument('--version', required=True)
    parser.add_argument('--repo', required=True)
    parser.add_argument('--notes', type=Path)
    args = parser.parse_args()
    if not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', args.repo):
        parser.error('Expected owner/repository')
    expected = verify_package(args.artifacts, args.version, args.commit)
    publish(args.repo, args.version, args.commit, expected,
            args.notes.read_text() if args.notes else DEFAULT_NOTES)


if __name__ == '__main__':
    main()
