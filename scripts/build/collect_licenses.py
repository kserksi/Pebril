#!/usr/bin/env python3
"""Collect pinned dependency declarations and original licenses; maintenance and distribution review remain separate."""
import json
import os
import re
from pathlib import Path
import shutil
import subprocess

ROOT = Path(__file__).resolve().parents[2]

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

def main():
    target = ROOT / 'dist/third-party'
    target.mkdir(parents=True, exist_ok=True)
    raw = subprocess.check_output(['go', 'list', '-tags', 'nomsgpack,image_worker', '-deps', '-json', './cmd/pebril', './cmd/pebril-image-worker'], cwd=ROOT).decode()
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
            worker=(ROOT/'dist/bin/pebril-image-worker').read_bytes()
            for profile,body in profiles:
                payload=bytes(int(value,16) for value in re.findall(r'0x([0-9a-fA-F]+)',body))
                if payload in worker: raise RuntimeError(f'Unreviewed additional govips ICC profile linked: {profile}')
        licenses(Path(module['Dir']), target / 'go' / name)
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
    payload = dict(scope='Actual main/worker Go runtime modules and Rust lockfile packages; see native/native-manifest.json for the native runtime.', review='Pinned declarations and original texts collected; see native/dependency-licenses.json for the quick-error exception and native/native-manifest.json for runtime scope. Full product release review remains separate.', packages=inventory)
    native_target = target / 'native'
    native_target.mkdir(exist_ok=True)
    for file in (ROOT / 'third-party').iterdir():
        if file.is_file():
            output = native_target / file.name
            if output.exists():
                output.unlink()
            shutil.copy2(file, output)
    (target / 'dependencies.json').write_text(json.dumps(payload, ensure_ascii=False, indent=2) + '\n')
    print(f'Collected {len(inventory)} dependency declarations and original licenses.')

if __name__ == '__main__':
    main()
