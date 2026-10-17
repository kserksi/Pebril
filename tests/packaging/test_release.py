#!/usr/bin/env python3
"""Release source/checksum gates, draft verification, retry and immutable publication."""
import copy
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import tarfile
import tempfile
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('release', ROOT / 'scripts/build/release.py')
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)
COMMIT = 'a' * 40
VERSION = '0.0.7-dev'


def rejected(action, message):
    try:
        action()
    except (ValueError, RuntimeError) as error:
        assert message in str(error), error
    else:
        raise AssertionError('Expected rejection: ' + message)


def package(directory, *, dirty=False, wrong_file=False, extra=None):
    files = {'VERSION': (VERSION + '\n').encode(), 'pebril': b'program',
             'build.json': json.dumps({'version': VERSION, 'source_commit': COMMIT,
                                       'source_dirty': dirty}).encode()}
    inventory = {n: hashlib.sha256(data).hexdigest() for n, data in files.items()}
    files['checksums.json'] = json.dumps(inventory).encode()
    if wrong_file:
        files['pebril'] = b'changed program'
    if extra:
        files[extra] = b'extra'
    name = f'pebril-{VERSION}-linux-amd64-{COMMIT[:12]}'
    archive = directory / (name + '.tar.gz')
    with tarfile.open(archive, 'w:gz') as target:
        for name_in_archive, data in files.items():
            member = tarfile.TarInfo(name + '/' + name_in_archive)
            member.size = len(data)
            target.addfile(member, io.BytesIO(data))
    (directory / 'SHA256SUMS').write_text(hashlib.sha256(archive.read_bytes()).hexdigest() + '  ' + archive.name + '\n')


class GitHub:
    def __init__(self, expected):
        self.expected = expected
        self.reference = None
        self.current = None
        self.events = []
        self.corrupt = False

    def api(self, endpoint, *, method='GET', data=None, optional=False):
        self.events.append((method, endpoint))
        if endpoint.endswith('/git/refs'):
            self.reference = {'object': {'type': 'commit', 'sha': data['sha']}}
            return copy.deepcopy(self.reference)
        if '/git/ref/' in endpoint:
            return copy.deepcopy(self.reference)
        if method == 'POST':
            assert data['draft'] and data['prerelease'] and data['make_latest'] == 'false'
            self.current = dict(data, id=123, assets=[], html_url='https://example.test/release')
        if method == 'PATCH':
            release.verify_assets(self.current, self.expected)
            self.current.update(data)
        return copy.deepcopy(self.current)

    def upload(self, command, **kwargs):
        assert self.current['draft']
        assert '--clobber' not in command
        path = Path(command[4])
        metadata = self.expected[path]
        self.current['assets'].append(dict(name=path.name, state='uploaded', **metadata))
        if self.corrupt:
            self.current['assets'][-1]['digest'] = 'sha256:wrong'
        return subprocess.CompletedProcess(command, 0)


def main():
    (ROOT / '.build').mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='release-test-', dir=ROOT / '.build') as temporary:
        directory = Path(temporary)
        package(directory)
        expected = release.verify_package(directory, VERSION, COMMIT)
        rejected(lambda: release.verify_package(directory, '0.0.7', COMMIT), 'development')
        rejected(lambda: release.verify_package(directory, VERSION, 'b' * 40), 'exactly')
        (directory / 'SHA256SUMS').write_text('invalid\n')
        rejected(lambda: release.verify_package(directory, VERSION, COMMIT), 'Archive checksum')
        package(directory, dirty=True)
        rejected(lambda: release.verify_package(directory, VERSION, COMMIT), 'identity mismatch')
        package(directory, wrong_file=True)
        rejected(lambda: release.verify_package(directory, VERSION, COMMIT), 'file checksum')
        package(directory, extra='../private')
        rejected(lambda: release.verify_package(directory, VERSION, COMMIT), 'archive path')
        package(directory, extra='unlisted')
        rejected(lambda: release.verify_package(directory, VERSION, COMMIT), 'inventory mismatch')
        package(directory)
        expected = release.verify_package(directory, VERSION, COMMIT)
        github = GitHub(expected)
        with patch.object(release, 'api', github.api), patch.object(release.subprocess, 'run', github.upload):
            release.publish('owner/repo', VERSION, COMMIT, expected, release.DEFAULT_NOTES)
            assert not github.current['draft'] and github.current['prerelease']
            before = copy.deepcopy(github.current)
            github.events.clear()
            release.publish('owner/repo', VERSION, 'b' * 40, expected, release.DEFAULT_NOTES)
            assert github.current == before and all(m == 'GET' for m, _ in github.events)
            github.current['draft'] = True
            release.publish('owner/repo', VERSION, COMMIT, expected, release.DEFAULT_NOTES)
            assert len(github.current['assets']) == 2
            github.current['draft'] = True
            github.current['body'] = 'different draft'
            rejected(lambda: release.publish('owner/repo', VERSION, COMMIT, expected, release.DEFAULT_NOTES), 'draft does not match')
        github = GitHub(expected)
        github.reference = {'object': {'type': 'commit', 'sha': 'b' * 40}}
        with patch.object(release, 'api', github.api):
            rejected(lambda: release.publish('owner/repo', VERSION, COMMIT, expected, release.DEFAULT_NOTES), 'Existing tag')
            assert all(m == 'GET' for m, _ in github.events)
        github = GitHub(expected)
        github.corrupt = True
        with patch.object(release, 'api', github.api), patch.object(release.subprocess, 'run', github.upload):
            rejected(lambda: release.publish('owner/repo', VERSION, COMMIT, expected, release.DEFAULT_NOTES), 'asset checksum')
            assert github.current['draft'] and not any(m == 'PATCH' for m, _ in github.events)
        for status in (403, 404, 500):
            result = subprocess.CompletedProcess([], 1, '', f'gh: failure (HTTP {status})')
            with patch.object(release.subprocess, 'run', return_value=result):
                if status == 404:
                    assert release.api('endpoint', optional=True) is None
                else:
                    rejected(lambda: release.api('endpoint', optional=True), f'HTTP {status}')
    print('Release checks passed: identity/hashes, verified drafts, immutable tags/assets and retries.')


if __name__ == '__main__':
    main()
