#!/usr/bin/env python3
"""Prepare reviewed native build inputs and package only checked application files."""
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import urllib.request

ROOT = Path(__file__).resolve().parents[2]
PAYLOAD = ['pebril', 'bin/pebril-image-worker', 'bin/pebril-runtime', 'bin/libpebril_native.so',
           'pebril.sh', 'VERSION', 'build.json', '.env.example', 'config.example.json', 'config.s3.example.json', 'LICENSE', 'README.md']


def prepare(root):
    manifest = json.loads((root / 'third-party/native-manifest.json').read_text())
    upstream = {'url': manifest['archive_url'], 'bytes': manifest['archive_bytes'], 'archive_sha256': manifest['archive_sha256']}
    native = root / '.build/ci/native'
    if native.exists():
        raise RuntimeError('Use a fresh .build/ci/native directory')
    archive = root / '.build/ci/native.tar.gz'
    archive.parent.mkdir(parents=True, exist_ok=True)
    with urllib.request.urlopen(upstream['url'], timeout=120) as response:
        data = response.read(upstream['bytes'] + 1)
    if len(data) != upstream['bytes'] or hashlib.sha256(data).hexdigest() != upstream['archive_sha256']:
        raise RuntimeError('Native build archive checksum or size mismatch')
    archive.write_bytes(data)
    try:
        with tarfile.open(archive) as source:
            members = [m for m in source if m.name.startswith(('include/', 'lib/glib-2.0/'))
                       or m.name in ['lib/libvips.so.42', 'versions.json', 'THIRD-PARTY-NOTICES.md']]
            source.extractall(native, members=members, filter='data')
    finally:
        archive.unlink()
    library = native / 'lib/libvips.so.42'
    if hashlib.sha256(library.read_bytes()).hexdigest() != manifest['library_sha256']:
        raise RuntimeError('Native build library checksum mismatch')
    (native / 'lib/libvips.so').symlink_to('libvips.so.42')
    pkgconfig = native / 'lib/pkgconfig'
    pkgconfig.mkdir()
    (pkgconfig / 'vips.pc').write_text(f"""prefix={native}
Name: vips
Description: Reviewed native build input
Version: {next(c['version'] for c in manifest['components'] if c['name'] == 'vips')}
Libs: -L${{prefix}}/lib -lvips
Cflags: -I${{prefix}}/include -I${{prefix}}/include/glib-2.0 -I${{prefix}}/lib/glib-2.0/include
""")


def package(root):
    output = root / '.build/ci/output'
    native = root / '.build/ci/native'
    version = (root / 'VERSION').read_text().strip()
    if not re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?', version):
        raise RuntimeError('Invalid package version')
    identity = json.loads((output / 'build.json').read_text())
    if identity['version'] != version or not re.fullmatch(r'[0-9a-f]{40}', identity['source_commit']):
        raise RuntimeError('Package identity does not match VERSION/source')
    expected_native = json.loads((root / 'third-party/native-manifest.json').read_text())
    actual_native = json.loads((native / 'versions.json').read_text())
    expected_version = next(c['version'] for c in expected_native['components'] if c['name'] == 'vips')
    if (identity.get('native_build', {}).get('pkg_config_version') != expected_version
            or actual_native.get('vips') != expected_version
            or hashlib.sha256((native / 'lib/libvips.so.42').read_bytes()).hexdigest() != expected_native['library_sha256']):
        raise RuntimeError('CI native build provenance mismatch; build/runtime library must match the reviewed artifact')
    shutil.copy2(root / 'README.md', output / 'README.md')
    shutil.copy2(native / 'THIRD-PARTY-NOTICES.md', output / 'third-party/native-build-NOTICES.txt')
    files = sorted(PAYLOAD + [str(p.relative_to(output)) for p in (output / 'third-party').rglob('*') if p.is_file()])
    checksums = {}
    for name in files:
        path = output / name
        if path.is_symlink() or not path.is_file() or not path.resolve().is_relative_to(output.resolve()):
            raise RuntimeError(f'Missing or unsafe package file: {name}')
        checksums[name] = hashlib.sha256(path.read_bytes()).hexdigest()
    (output / 'checksums.json').write_text(json.dumps(checksums, indent=2) + '\n')
    artifacts = root / '.build/ci/artifacts'
    artifacts.mkdir()
    name = f"pebril-{version}-linux-amd64-{identity['source_commit'][:12]}"
    archive = artifacts / (name + '.tar.gz')

    def metadata(info):
        info.uid = info.gid = 0
        info.uname = info.gname = ''
        info.mtime = 0
        return info

    with tarfile.open(archive, 'w:gz') as target:
        for path in files + ['checksums.json']:
            target.add(output / path, arcname=f'{name}/{path}', recursive=False, filter=metadata)
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    (artifacts / 'SHA256SUMS').write_text(f'{digest}  {archive.name}\n')
    # Verify bytes from the finished archive, rather than trusting its source list.
    with tarfile.open(archive) as source:
        for member in source:
            relative = member.name.removeprefix(name + '/')
            if relative in checksums and hashlib.sha256(source.extractfile(member).read()).hexdigest() != checksums[relative]:
                raise RuntimeError(f'Archive checksum mismatch: {relative}')
    print(f'Checked package: {archive.relative_to(root)}')


def smoke(root):
    archive = next((root / '.build/ci/artifacts').glob('*.tar.gz'))
    with tempfile.TemporaryDirectory(prefix='ci-unpack-', dir=root / '.build') as temporary:
        directory = Path(temporary)
        with tarfile.open(archive) as source:
            source.extractall(directory, filter='data')
        app = next(directory.iterdir())
        env = {k: v for k, v in os.environ.items() if not k.startswith('PEBRIL_')}
        env['PEBRIL_AVIF'] = 'true'
        version = subprocess.check_output([str(app / 'pebril'), 'version'], env=env, cwd='/', text=True)
        if version.strip() != (root / 'VERSION').read_text().strip():
            raise RuntimeError('Unpacked program version mismatch')
        # Supply the reviewed external runtime only to this disposable deployment.
        shutil.copy2(root / '.build/ci/native/lib/libvips.so.42', app / 'bin/libvips.so.42')
        subprocess.run([str(app / 'pebril.sh'), '--check'], env=env, cwd='/', check=True, timeout=180)
        if (app / 'data').exists():
            raise RuntimeError('Package check initialized business data')
    print('Unpacked CI package passed version and offline runtime checks.')


if __name__ == '__main__':
    if sys.argv[1:] == ['prepare']:
        prepare(ROOT)
    elif sys.argv[1:] == ['package']:
        package(ROOT)
        smoke(ROOT)
    else:
        sys.exit('Usage: ci.py prepare|package')
