#!/usr/bin/env python3
"""CI archive boundaries: identity, payload hashes, private-file exclusion and symlinks."""
import hashlib
import importlib.util
import json
from pathlib import Path
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('ci', ROOT / 'scripts/build/ci.py')
ci = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ci)


def main():
    (ROOT / '.build').mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='ci-package-', dir=ROOT / '.build') as directory:
        root = Path(directory)
        output = root / '.build/ci/output'
        for name in ci.PAYLOAD:
            path = output / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(name)
        identity={'version':'0.0.5-dev','source_commit':'a'*40,'native_build':{'pkg_config_version':'8.18.7'}}
        (output / 'build.json').write_text(json.dumps(identity))
        (root / 'VERSION').write_text('0.0.5-dev\n')
        (root / 'README.md').write_text('Public readme')
        notice = root / '.build/ci/native/THIRD-PARTY-NOTICES.md'
        notice.parent.mkdir(parents=True)
        notice.write_text('Original third-party notice')
        (notice.parent / 'versions.json').write_text(json.dumps({'vips':'8.18.7'}))
        manifest=root/'third-party/native-manifest.json'
        manifest.parent.mkdir(parents=True)
        library=notice.parent/'lib/libvips.so.42'
        library.parent.mkdir()
        library.write_bytes(b'reviewed-native')
        manifest.write_text(json.dumps({'components':[{'name':'vips','version':'8.18.7'}],
                                        'library_sha256':hashlib.sha256(library.read_bytes()).hexdigest()}))
        identity['native_build']['pkg_config_version']='8.18.5'
        (output/'build.json').write_text(json.dumps(identity))
        try:
            ci.package(root)
        except RuntimeError as error:
            assert 'provenance mismatch' in str(error)
        else:
            raise AssertionError('Mismatched CI build headers were accepted')
        identity['native_build']['pkg_config_version']='8.18.7'
        (output/'build.json').write_text(json.dumps(identity))

        (output / 'third-party').mkdir()
        for name in ['.env', 'config.json', 'private.db', 'AGENTS.md']:
            (output / name).write_text('private fixture')
        ci.package(root)
        artifacts = root / '.build/ci/artifacts'
        archive = next(artifacts.glob('*.tar.gz'))
        assert (artifacts / 'SHA256SUMS').read_text().split()[0] == hashlib.sha256(archive.read_bytes()).hexdigest()
        with tarfile.open(archive) as source:
            members = {m.name.split('/', 1)[1]: m for m in source}
            assert not set(members) & {'.env', 'config.json', 'private.db', 'AGENTS.md'}
            inventory = json.loads(source.extractfile(members['checksums.json']).read())
            assert set(members) == set(inventory) | {'checksums.json'}
            for name, digest in inventory.items():
                assert hashlib.sha256(source.extractfile(members[name]).read()).hexdigest() == digest
            assert all(not m.uname and not m.gname for m in members.values())
        (output / 'pebril').unlink()
        (output / 'pebril').symlink_to(root / 'README.md')
        try:
            ci.package(root)
        except RuntimeError as error:
            assert 'unsafe package file' in str(error)
        else:
            raise AssertionError('A symlink entered the package')
    print('CI packaging passed: exact payload hashes, identity, private-file exclusion and symlink refusal.')


if __name__ == '__main__':
    main()
