#!/usr/bin/env python3
"""Foundation end-to-end checks use automatically cleaned temporary directories, preserving user configuration and databases."""
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.request

ROOT = Path(__file__).resolve().parent.parent

def wait_status(port, process):
    deadline = time.monotonic() + 8
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise AssertionError(f'Service exited early: {process.returncode}')
        try:
            with urllib.request.urlopen(f'http://127.0.0.1:{port}/api/v1/status', timeout=0.4) as response:
                assert response.headers['Cache-Control'] == 'no-store'
                return json.load(response)
        except OSError:
            time.sleep(0.05)
    raise AssertionError('Service did not become ready promptly')

def main():
    (ROOT / '.build').mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='integration-', dir=ROOT / '.build') as temporary:
        root = Path(temporary)
        executable = root / 'pebril'
        shutil.copy2(ROOT / 'dist/pebril', executable)
        shutil.copytree(ROOT / 'dist/bin', root / 'bin', symlinks=False)
        # Start from another working directory to verify paths remain relative to the executable.
        env = {k: v for k, v in os.environ.items() if not k.startswith('PEBRIL_')}
        assert subprocess.check_output([str(executable), 'version'], env=env, cwd='/').decode().strip() == (ROOT / 'VERSION').read_text().strip()
        check = subprocess.run([str(executable), 'check'], env=env, cwd='/', capture_output=True, text=True, timeout=180)
        assert check.returncode == 0, check.stderr
        capability = json.loads(check.stdout)
        assert capability['ready'] and capability['raster'] and capability['svg'] and capability['gif']
        assert not list((root / 'bin').glob('.probe-*'))
        assert not (root / 'data/db/pebril.db').exists(), 'check must not create the application database'
        with socket.socket() as reservation:
            reservation.bind(('127.0.0.1', 0))
            port = reservation.getsockname()[1]
        (root / '.env').write_text(f'PEBRIL_LISTEN=127.0.0.1:{port}\n')
        with (root / 'server.log').open('w+') as log:
            process = subprocess.Popen([str(executable)], env=env, cwd='/', stdout=log, stderr=log)
            try:
                status = wait_status(port, process)
                assert status['sqlite'] and status['stage'] == 'foundation'
                revision = status['config_revision']
                (root / 'replacement.json').write_text('{"static_quality":73}')
                (root / 'replacement.json').replace(root / 'config.json')
                deadline = time.monotonic() + 3
                while time.monotonic() < deadline:
                    if wait_status(port, process)['config_revision'] > revision:
                        break
                    time.sleep(0.05)
                else:
                    raise AssertionError('Hot reload was not applied')
                revision = wait_status(port, process)['config_revision']
                (root / 'config.json').write_text('{"static_quality":101}')
                time.sleep(0.3)
                assert wait_status(port, process)['config_revision'] == revision
                assert (root / 'data/db/pebril.db').exists()
            finally:
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
                    raise AssertionError('SIGTERM did not terminate the service promptly')
            assert process.returncode == 0
        invalid = subprocess.run([str(executable)], env=env, cwd='/', capture_output=True, timeout=5)
        assert invalid.returncode != 0, 'Invalid startup configuration must fail'
        (root / 'config.json').write_text('{}')
        avif_env = dict(env, PEBRIL_AVIF='true')
        avif = subprocess.run([str(executable), 'check'], env=avif_env, cwd='/', capture_output=True, timeout=180)
        assert avif.returncode == 0, avif.stderr
        assert json.loads(avif.stdout)['avif_enabled']
        rust = root / 'bin/libpebril_native.so'
        rust.rename(root / 'bin/library.hidden')
        missing = subprocess.run([str(executable), 'check'], env=env, cwd='/', capture_output=True, timeout=180)
        assert missing.returncode == 0, missing.stderr
        assert rust.exists(), 'Embedded Rust library was not restored'
        assert not list((root / 'bin').glob('.probe-*'))
    print('Integration passed: native probes, startup, cwd-independent paths, hot reload, invalid configuration, library recovery, enabled AVIF, and SIGTERM shutdown.')

if __name__ == '__main__':
    main()
