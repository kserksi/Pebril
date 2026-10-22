#!/usr/bin/env python3
"""Collect pinned dependency declarations and original licenses; maintenance and distribution review remain separate."""
import json
import hashlib
import zipfile
import os
import re
from pathlib import Path
import shutil
import subprocess

ROOT = Path(__file__).resolve().parents[2]
OUTPUT = Path(os.environ.get('PEBRIL_BUILD_OUTPUT', str(ROOT / 'dist')))

def licenses(directory, destination):
    count = 0
    for source in directory.iterdir():
        if source.is_file() and source.name.lower().startswith(('license', 'licence', 'copying', 'notice', 'copyright')):
            destination.mkdir(parents=True, exist_ok=True)
            output = destination / source.name
            if output.exists():
                output.unlink()
            shutil.copy2(source, output)
            count += 1
    if count == 0:
        raise RuntimeError(f'License not found: {directory}')

def mysql_source(module, destination):
    """Ship unmodified covered source with each executable distribution."""
    metadata = json.loads(subprocess.check_output(['go', 'mod', 'download', '-json', module['Path'] + '@' + module['Version']], cwd=ROOT))
    if metadata.get('Sum') != module.get('Sum') or not metadata.get('Zip'):
        raise RuntimeError('MySQL source archive identity mismatch')
    subprocess.run(['go', 'mod', 'verify'], cwd=ROOT, check=True, stdout=subprocess.DEVNULL)
    archive = Path(metadata['Zip'])
    prefix = module['Path'] + '@' + module['Version'] + '/'
    with zipfile.ZipFile(archive) as source:
        if source.testzip() is not None:
            raise RuntimeError('MySQL source archive is corrupt')
        for name in source.namelist():
            if not name.startswith(prefix) or '..' in Path(name).parts:
                raise RuntimeError('Unsafe MySQL source archive')
        for path in Path(module['Dir']).glob('*.go'):
            if source.read(prefix + path.name) != path.read_bytes():
                raise RuntimeError('MySQL covered source differs from compiled source')
        if source.read(prefix + 'LICENSE') != (Path(module['Dir']) / 'LICENSE').read_bytes():
            raise RuntimeError('MySQL source license mismatch')
    destination.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(archive, destination / 'source.zip')
    notice = ('go-sql-driver/mysql ' + module['Version'] + '\n'
              'This unmodified component is covered by the Mozilla Public License 2.0.\n'
              'Its Source Code Form is supplied in source.zip beside this notice.\n'
              'The original LICENSE beside this notice and inside source.zip contains its terms.\n'
              'Pebril-owned files are separately licensed under MIT.\n'
              'Source ZIP SHA256: ' + hashlib.sha256(archive.read_bytes()).hexdigest() + '\n')
    (destination / 'SOURCE-NOTICE.txt').write_text(notice)

def main():
    target = OUTPUT / 'third-party'
    target.mkdir(parents=True, exist_ok=True)
    raw = subprocess.check_output(['go', 'list', '-tags', 'nomsgpack,image_worker,bundled_native', '-deps', '-json', './cmd/pebril', './cmd/pebril-image-worker', './cmd/pebril-runtime'], cwd=ROOT).decode()
    decoder = json.JSONDecoder()
    modules = {}
    while raw.strip():
        data, offset = decoder.raw_decode(raw.lstrip())
        raw = raw.lstrip()[offset:]
        module = data.get('Module')
        if module and not module.get('Main'):
            modules[module['Path']] = module
    registry = {(p['ecosystem'],p['name'],p['version']):p['license'] for p in json.loads((ROOT / 'third-party/dependency-licenses.json').read_text())['packages']}
    inventory = []
    for name, module in sorted(modules.items()):
        if name == 'github.com/davidbyttow/govips/v2':
            source=(Path(module['Dir'])/'vips/icc_profiles.go').read_text()
            profiles=re.findall(r'(\w+ICCProfile)\s*=\s*\[\]byte\{(.*?)\n\s*\}',source,re.S)
            if not profiles: raise RuntimeError('Review changed govips embedded-profile inventory')
            worker=(OUTPUT/'bin/pebril-image-worker').read_bytes()
            for profile,body in profiles:
                payload=bytes(int(value,16) for value in re.findall(r'0x([0-9a-fA-F]+)',body))
                if payload in worker: raise RuntimeError(f'Unreviewed additional govips ICC profile linked: {profile}')
        licenses(Path(module['Dir']), target / 'go' / name)
        if name == 'github.com/go-sql-driver/mysql':
            mysql_source(module, target / 'go' / name)
        key = ('go', name, module['Version'])
        if key not in registry:
            raise RuntimeError(f'Go dependency lacks pinned-version license review: {key}')
        spdx = registry[key]
        inventory.append(dict(ecosystem='go', name=name, version=module['Version'], license=spdx))
    metadata = json.loads(subprocess.check_output(['cargo', 'metadata', '--locked', '--format-version=1', '--manifest-path', 'native/Cargo.toml'], cwd=ROOT))
    for package in metadata['packages']:
        if package['name'] == 'pebril-native':
            continue
        key = ('rust', package['name'], package['version'])
        if registry.get(key) != package['license']:
            raise RuntimeError(f'Rust dependency version or license is not reviewed: {key}')
        licenses(Path(package['manifest_path']).parent, target / 'rust' / f"{package['name']}-{package['version']}")
        inventory.append(dict(ecosystem='rust', name=package['name'], version=package['version'], license=package['license'], checksum_in='native/Cargo.lock'))
    goroot = Path(subprocess.check_output(['go', 'env', 'GOROOT']).decode().strip())
    licenses(goroot, target / 'go-standard-library')
    payload = dict(scope='Actual main/worker/deployment-helper Go runtime modules and Rust lockfile packages; native/native-manifest.json describes the shared reviewed link-time build input and startup download catalog.', review='Pinned declarations and original texts collected; see native/dependency-licenses.json for the quick-error exception and build.json native_build and native-build-NOTICES.txt for the CI build input; native/native-manifest.json is the shared build/installer catalog. Full product release review remains separate.', packages=inventory)
    native_target = target / 'native'
    native_target.mkdir(exist_ok=True)
    for file in (ROOT / 'third-party').rglob('*'):
        if file.is_file():
            output = native_target / file.relative_to(ROOT / 'third-party')
            output.parent.mkdir(parents=True, exist_ok=True)
            if output.exists():
                output.unlink()
            shutil.copy2(file, output)
    (target / 'dependencies.json').write_text(json.dumps(payload, ensure_ascii=False, indent=2) + '\n')
    print(f'Collected {len(inventory)} dependency declarations and original licenses.')

if __name__ == '__main__':
    main()
