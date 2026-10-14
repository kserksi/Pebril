#!/usr/bin/env python3
"""Verify startup and runtime installation in official userspaces sharing the WSL kernel; this is not independent VM acceptance."""
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
        executable = app / 'pebril'
        shutil.copy2(ROOT / 'dist/pebril', executable)
        base = ['bwrap', '--unshare-user', '--uid', '0', '--gid', '0', '--die-with-parent', '--ro-bind', str(system), '/', '--dev', '/dev', '--proc', '/proc', '--tmpfs', '/tmp', '--bind', str(app), '/app', '--ro-bind', '/etc/resolv.conf', '/etc/resolv.conf', '--ro-bind', '/etc/ssl/certs/ca-certificates.crt', '/pebril-ca.pem', '--chdir', '/app']
        env = {key: value for key, value in os.environ.items() if not key.startswith('PEBRIL_') and key not in ['LD_LIBRARY_PATH', 'LD_PRELOAD']}
        env.update(SSL_CERT_FILE='/pebril-ca.pem', PEBRIL_AVIF='true')
        offline = dict(env, HTTPS_PROXY='http://127.0.0.1:1', HTTP_PROXY='http://127.0.0.1:1', NO_PROXY='')
        failed = subprocess.run(base + ['/app/pebril'], env=offline, capture_output=True, text=True, timeout=35)
        assert failed.returncode != 0 and 'Core runtime installation failed' in failed.stderr, failed.stderr
        assert not (app / 'data').exists(), 'Application data was created after runtime installation failed'
        assert not (app / 'bin/vips-current').exists()
        assert not list((app / 'bin').glob('.pebril-install-*'))
        check = subprocess.run(base + ['/app/pebril', 'check'], env=env, capture_output=True, text=True, timeout=180)
        assert check.returncode == 0, (entry['name'], check.stderr, check.stdout)
        result = json.loads(check.stdout)
        assert result['ready'] and result['avif_enabled'] and result['vips_version'] == '8.18.5'
        assert (app / 'bin/vips-current').is_symlink()
        assert not (app / 'data').exists()
        (app / 'bin/libpebril_native.so').unlink()
        repeat = subprocess.run(base + ['/app/pebril', 'check'], env=offline, capture_output=True, text=True, timeout=35)
        assert repeat.returncode == 0, repeat.stderr
        assert (app / 'bin/libpebril_native.so').exists()
        with socket.socket() as reservation:
            reservation.bind(('127.0.0.1', 0))
            port = reservation.getsockname()[1]
        env['PEBRIL_LISTEN'] = f'127.0.0.1:{port}'
        with (app / 'test.log').open('w+') as log:
            process = subprocess.Popen(base + ['/app/pebril'], env=env, stdout=log, stderr=log, start_new_session=True)
            try:
                deadline = time.monotonic() + 10
                while time.monotonic() < deadline:
                    assert process.poll() is None, (entry['name'], 'Service exited early')
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
                stop_application(process, executable)
        assert not list((app / 'bin').glob('.probe-*'))
        assert not list((app / 'bin').glob('.pebril-install-*'))
        print(entry['name'], 'Passed: offline missing-runtime refusal, fresh runtime download, AVIF self-tests, offline reuse, internal-library recovery, SQLite/HTTP startup, and SIGTERM shutdown', flush=True)

if __name__ == '__main__':
    for image in json.loads(Path(__file__).resolve().with_name('images.json').read_text()):
        test_image(image)
