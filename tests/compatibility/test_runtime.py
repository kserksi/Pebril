#!/usr/bin/env python3
"""Real bootstrap/download checks in pinned userspaces; shared kernel, no host package installation."""
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import tempfile
import time
import urllib.request

ROOT = Path(__file__).resolve().parents[2]
OUTPUT = Path(os.environ.get('PEBRIL_TEST_OUTPUT', str(ROOT / 'dist')))


def stop_application(process, executable):
    identity = executable.stat()
    for entry in Path('/proc').iterdir():
        if not entry.name.isdigit():
            continue
        try:
            image = (entry / 'exe').stat()
            if (image.st_dev, image.st_ino) == (identity.st_dev, identity.st_ino):
                os.kill(int(entry.name), signal.SIGTERM)
        except (OSError, ProcessLookupError):
            continue
    try:
        process.wait(timeout=10)
    except subprocess.TimeoutExpired:
        os.killpg(process.pid, signal.SIGKILL)
        process.wait()
        raise AssertionError('Service did not exit promptly')
    assert process.returncode == 0, process.returncode


def test_image(entry):
    system = ROOT / '.build/compat' / entry['name']
    assert (system / '.pebril-rootfs-ready').read_text() == entry['digest']
    for directory in ['app', 'dev', 'proc', 'tmp']:
        (system / directory).mkdir(exist_ok=True)
    (system / 'pebril-ca.pem').touch()
    (system / 'etc/resolv.conf').touch(exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='compat-app-', dir=ROOT / '.build') as temporary:
        app = Path(temporary)
        shutil.copy2(OUTPUT / 'pebril', app / 'pebril')
        shutil.copy2(OUTPUT / 'pebril.sh', app / 'pebril.sh')
        shutil.copytree(OUTPUT / 'bin', app / 'bin', symlinks=True)
        base = ['bwrap', '--unshare-user', '--uid', '1000', '--gid', '1000', '--die-with-parent',
                '--ro-bind', str(system), '/', '--dev', '/dev', '--proc', '/proc', '--tmpfs', '/tmp',
                '--bind', str(app), '/app', '--ro-bind', '/etc/resolv.conf', '/etc/resolv.conf',
                '--ro-bind', '/etc/ssl/certs/ca-certificates.crt', '/pebril-ca.pem', '--chdir', '/']
        env = {key: value for key, value in os.environ.items() if not key.startswith('PEBRIL_') and key not in ['LD_LIBRARY_PATH', 'LD_PRELOAD', 'LD_AUDIT']}
        env.update(SSL_CERT_FILE='/pebril-ca.pem', PEBRIL_AVIF='true')
        offline = dict(env, HTTPS_PROXY='http://127.0.0.1:1', HTTP_PROXY='http://127.0.0.1:1', NO_PROXY='')

        def run(args, current=env, timeout=180):
            return subprocess.run(base + args, env=current, stdin=subprocess.DEVNULL,
                                  capture_output=True, text=True, timeout=timeout)

        # Both entry points refuse a missing runtime without implicit network consent.
        failed = run(['/app/pebril', 'check'], offline)
        assert failed.returncode != 0 and 'pebril.sh' in failed.stderr, failed.stderr
        assert not (app / 'bin/vips-current').exists()
        denied = run(['/app/pebril.sh', '--check'], offline)
        assert denied.returncode != 0 and 'not authorized' in denied.stderr, denied.stderr
        assert not (app / 'data').exists()
        assert not (app / 'bin/vips-current').exists()
        exhausted = run(['/app/pebril.sh', '--check', '--allow-downloads'], offline)
        assert exhausted.returncode != 0 and 'All authorized runtime sources failed' in exhausted.stderr, exhausted.stderr
        assert not list((app / 'bin').glob('.pebril-install-*'))
        assert not (app / 'data').exists()

        # Real second-tier download, then offline reuse and embedded-component repair.
        check = run(['/app/pebril.sh', '--check', '--allow-downloads'])
        assert check.returncode == 0, (entry['name'], check.stderr, check.stdout)
        result = run(['/app/pebril', 'check'], offline)
        assert result.returncode == 0, result.stderr
        assert json.loads(result.stdout)['vips_version'] == '8.18.7'
        for name in ['libpebril_native.so', 'pebril-image-worker']:
            (app / 'bin' / name).unlink()
        repeat = run(['/app/pebril.sh', '--check'], offline)
        assert repeat.returncode == 0, repeat.stderr
        assert (app / 'bin/libpebril_native.so').exists()
        assert (app / 'bin/pebril-image-worker').exists()

        # A failed reviewed download must not reinstall the retired vulnerable fallback.
        target = (app / 'bin/vips-current').resolve()
        (app / 'bin/vips-current').unlink()
        shutil.rmtree(target)
        helper = app / 'bin/pebril-runtime'
        helper.rename(app / 'bin/pebril-runtime-real')
        helper.write_text('#!/bin/bash\nif [[ "$1" == install ]]; then\n    printf \'%s\\n\' "$2" >> /app/sources-tested\n    exit 1\nfi\nexec /app/bin/pebril-runtime-real "$@"\n')
        helper.chmod(0o700)
        failure = run(['/app/pebril.sh', '--check', '--allow-downloads'])
        assert failure.returncode != 0, (entry['name'], failure.stderr)
        assert (app / 'sources-tested').read_text().splitlines() == ['upstream']
        helper.unlink()
        (app / 'bin/pebril-runtime-real').rename(helper)
        assert run(['/app/pebril.sh', '--check', '--allow-downloads']).returncode == 0
        result = run(['/app/pebril', 'check'], offline)
        assert result.returncode == 0, result.stderr
        assert json.loads(result.stdout)['vips_version'] == '8.18.7'
        installed = (app / 'bin/vips-current').resolve()
        assert (installed / 'THIRD-PARTY-NOTICES.md').is_file()
        assert run(['/app/pebril.sh', '--check'], offline).returncode == 0
        assert not (app / 'data').exists(), 'Bootstrap/check must not initialize business data'

        # A root entry cannot start the business service or helper.
        root_base = base.copy()
        root_base[root_base.index('--uid') + 1] = '0'
        root_base[root_base.index('--gid') + 1] = '0'
        for executable in ['/app/pebril.sh', '/app/pebril']:
            refused = subprocess.run(root_base + [executable], env=offline, capture_output=True, text=True, timeout=10)
            assert refused.returncode != 0 and 'unprivileged' in refused.stderr, refused.stderr
        assert not (app / 'data').exists()

        with socket.socket() as reservation:
            reservation.bind(('127.0.0.1', 0))
            port = reservation.getsockname()[1]
        env['PEBRIL_LISTEN'] = f'127.0.0.1:{port}'
        with (app / 'test.log').open('w+') as log:
            process = subprocess.Popen(base + ['/app/pebril.sh'], env=env, stdin=subprocess.DEVNULL,
                                       stdout=log, stderr=log, start_new_session=True)
            try:
                deadline = time.monotonic() + 12
                while time.monotonic() < deadline:
                    assert process.poll() is None, (entry['name'], log.read())
                    try:
                        with urllib.request.urlopen(f'http://127.0.0.1:{port}/healthz', timeout=0.5) as response:
                            assert json.load(response)['ready']
                        break
                    except OSError:
                        time.sleep(0.05)
                else:
                    raise AssertionError('Health check timed out')
                assert (app / 'data/db/pebril.db').exists()
            finally:
                stop_application(process, app / 'pebril')
        assert not list((app / 'bin').glob('.probe-*'))
        assert not list((app / 'bin').glob('.pebril-install-*'))
        print(entry['name'], 'Passed: explicit consent, upstream download, retired fallback refusal, offline reuse, independent check, root refusal, HTTP and shutdown; no real privileged package transaction', flush=True)


if __name__ == '__main__':
    for image in json.loads(Path(__file__).resolve().with_name('images.json').read_text()):
        test_image(image)
