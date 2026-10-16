#!/usr/bin/env python3
"""Real resource-control integration with namespace-only metadata, not enforced RAM limits."""
import json
import os
from pathlib import Path
import secrets
import shutil
import signal
import socket
import sqlite3
import subprocess
import tempfile
import time
from test_uploads import Client, ROOT, OUTPUT, png, raster_fixtures, wait_image


def main():
    membership = next((line[3:] for line in Path('/proc/self/cgroup').read_text().splitlines() if line.startswith('0::')), None)
    mounts = [line.split() for line in Path('/proc/self/mountinfo').read_text().splitlines() if ' - cgroup2 ' in line]
    if not membership or membership == '/' or not mounts or mounts[0][3] != '/':
        print('Resource integration SKIPPED: no visible non-root unified cgroup leaf; metadata/unit tests remain required.')
        return
    leaf = Path(mounts[0][4]) / membership.lstrip('/') / 'memory.max'
    if not leaf.is_file() or '\\' in str(leaf):
        print('Resource integration SKIPPED: no safe existing cgroup leaf memory.max to isolate.')
        return
    runtime = Path(os.environ['PEBRIL_TEST_RUNTIME']).resolve()
    with tempfile.TemporaryDirectory(prefix='resources-integration-', dir=ROOT / '.build') as temporary:
        root = Path(temporary)
        shutil.copy2(OUTPUT / 'pebril', root / 'pebril')
        shutil.copy2(OUTPUT / 'pebril.sh', root / 'pebril.sh')
        shutil.copytree(OUTPUT / 'bin', root / 'bin', symlinks=True)
        shutil.copy2(runtime, root / 'bin/libvips.so.42')
        metadata = root / 'metadata'
        metadata.mkdir()
        maximum = metadata / 'memory.max'
        maximum.write_text(str(1 << 30))
        (metadata / 'meminfo').write_text('MemTotal: 4194304 kB\n')
        sandbox = ['bwrap', '--unshare-user', '--uid', str(os.getuid()), '--gid', str(os.getgid()),
                   '--die-with-parent', '--ro-bind', '/', '/', '--bind', str(root), str(root),
                   '--proc', '/proc', '--ro-bind', str(metadata), str(metadata),
                   '--ro-bind', str(maximum), str(leaf), '--ro-bind', str(metadata / 'meminfo'), '/proc/meminfo',
                   '--dev', '/dev', '--tmpfs', '/tmp', '--chdir', '/']
        env = {k: v for k, v in os.environ.items() if not k.startswith('PEBRIL_')}
        with socket.socket() as reservation:
            reservation.bind(('127.0.0.1', 0))
            port = reservation.getsockname()[1]
        (root / '.env').write_text(f'PEBRIL_LISTEN=127.0.0.1:{port}\nPEBRIL_AVIF=true\n')
        policy = json.loads((ROOT / 'configs/config.example.json').read_text())
        policy['upload_formats']['avif'] = True
        (root / 'config.json').write_text(json.dumps(policy))
        database = root / 'data/db/pebril.db'

        def run(program, *args):
            return subprocess.run(sandbox + [str(root / program), *args], env=env, stdin=subprocess.DEVNULL,
                                  capture_output=True, text=True, timeout=90)

        for program, options in [('pebril', ()), ('pebril.sh', ()), ('pebril.sh', ('--allow-downloads', '--allow-system-install'))]:
            result = run(program, *options)
            assert result.returncode != 0 and '--allow-low-memory' in result.stderr, result.stderr
            assert not database.exists(), 'Unconsented startup initialized the business database'
        settings = run('bin/pebril-runtime', 'settings', str(root), '--check')
        assert settings.returncode == 0 and settings.stdout.split('\0')[1::2] == ['false', 'true'], settings.stderr
        for program, options in [('pebril', ('check',)), ('pebril.sh', ('--check',))]:
            result = run(program, *options)
            assert result.returncode == 0 and not database.exists(), result.stderr
            if program == 'pebril':
                assert not json.loads(result.stdout)['avif_enabled'], 'Restricted probe required AVIF'
        fixtures = raster_fixtures(str(root / 'bin/libvips.so.42'), root)
        origin = f'http://127.0.0.1:{port}'
        admin, public = Client(origin), Client(origin)
        credentials = {'username': secrets.token_hex(8), 'password': secrets.token_urlsafe(24)}
        process = None
        service_pid = None
        log = (root / 'server.log').open('w+')

        def start(restricted=True):
            nonlocal process, service_pid
            options = ['--allow-low-memory'] if restricted else []
            read_fd, write_fd = os.pipe()
            try:
                process = subprocess.Popen(sandbox + ['--info-fd', str(write_fd), str(root / 'pebril.sh'), *options], env=env,
                                           stdin=subprocess.DEVNULL, stdout=log, stderr=log, pass_fds=(write_fd,))
            finally:
                os.close(write_fd)
            with os.fdopen(read_fd) as information:
                service_pid = json.load(information)['child-pid']
            deadline = time.monotonic() + 20
            while time.monotonic() < deadline:
                assert process.poll() is None, 'Service failed startup: ' + (root / 'server.log').read_text()
                try:
                    public.request('/healthz')
                    return
                except OSError:
                    time.sleep(.05)
            raise AssertionError('Resource service readiness timeout')

        def stop():
            nonlocal process
            if process and process.poll() is None:
                os.kill(service_pid, signal.SIGTERM)
                try:
                    process.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)
                    raise
                assert process.returncode == 0, 'Service failed graceful shutdown'
            process = None

        def state():
            return admin.request('/api/v1/account')[0]['resources']

        def configure(**changes):
            revision = public.request('/api/v1/status')[0]['config_revision']
            policy.update(changes)
            (root / 'replacement.json').write_text(json.dumps(policy))
            (root / 'replacement.json').replace(root / 'config.json')
            deadline = time.monotonic() + 4
            while time.monotonic() < deadline:
                if public.request('/api/v1/status')[0]['config_revision'] > revision:
                    return
                time.sleep(.05)
            raise AssertionError('Resource configuration reload timed out')

        def sql(statement, values=()):
            with sqlite3.connect(database) as connection:
                return connection.execute(statement, values).fetchall()

        try:
            start()
            initial = admin.request('/api/v1/initialize', 'POST', credentials)[0]
            assert 'restricted-start' in [warning['code'] for warning in initial['resource_warnings']]
            current = state()
            assert current['restricted'] and current['concurrency'] == 1 and current['memory']['limit_bytes'] == 1 << 30
            assert 'resources' not in public.request('/api/v1/account')[0]
            assert 'resources' not in public.request('/api/v1/status')[0]
            assert not public.request('/api/v1/status')[0]['native']['avif_enabled']
            configure(conversion_concurrency=30)
            assert state()['concurrency'] == 1, 'Reload raised restricted concurrency'
            image = wait_image(admin, admin.upload(png())['id'])
            assert image['format'] == 'webp'
            original_public = public.request(image['url'])[0]
            admin.upload(fixtures['avif'], expected=422)
            configure(static_format='avif')
            rejected = admin.upload(png(), expected=422)
            assert 'AVIF conversion is disabled' in rejected['error']
            assert sql('SELECT state,format,attempts FROM images WHERE id=?', (rejected['id'],)) == [('failed', '', 0)]
            configure(static_format='webp')
            stop()
            # Seed a valid original with its AVIF upload-time snapshot while the service is stopped.
            original = sql("SELECT hash,bytes,format,width,height,frames FROM image_files WHERE image_id=? AND role='original'", (image['id'],))[0]
            snapshot = json.loads(sql('SELECT policy FROM images WHERE id=?', (image['id'],))[0][0])
            snapshot['policy']['static_format'] = 'avif'
            sql('UPDATE images SET state=?,format=?,bytes=?,width=?,height=?,frames=?,hash=?,policy=? WHERE id=?',
                ('queued', original[2], original[1], original[3], original[4], original[5], original[0], json.dumps(snapshot), rejected['id']))
            sql('INSERT INTO image_files VALUES(?,?,?,?,?,?,?,?)', (rejected['id'], 'original', *original))
            sql('UPDATE users SET used=used+? WHERE id=(SELECT user_id FROM images WHERE id=?)', (original[1], rejected['id']))
            # Fully received input from before restart must survive without restricted AVIF decoding.
            restored = []
            snapshot['policy']['upload_formats']['avif'] = True
            for payload in [fixtures['avif'], png()]:
                identifier = secrets.token_hex(16)
                scratch = root / 'data/.incoming' / identifier
                scratch.mkdir(mode=0o700)
                (scratch / 'input').write_bytes(payload)
                captured = json.dumps(snapshot)
                sql("INSERT INTO images(id,user_id,state,policy,created_at) VALUES(?,?,'received',?,?)",
                    (identifier, initial['user']['id'], captured, int(time.time() * 1000)))
                restored.append((identifier, payload, captured))
            start()
            assert state()['restricted']
            maximum.write_text('invalid')
            deadline = time.monotonic() + 14
            while not state()['cooldown'] and time.monotonic() < deadline:
                time.sleep(.1)
            assert state()['cooldown'] and any(w['code'] == 'memory-monitor' for w in state()['warnings'])
            count = sql('SELECT count(*) FROM images')[0][0]
            assert admin.upload(png(), expected=429)['code'] == 'resource_cooldown'
            assert sql('SELECT count(*) FROM images')[0][0] == count
            assert public.request(image['url'])[0] == original_public, 'Cooldown hid an existing completed result'
            maximum.write_text(str(1 << 30))
            deadline = time.monotonic() + 14
            while state()['cooldown'] and time.monotonic() < deadline:
                time.sleep(.1)
            assert not state()['cooldown'] and state()['concurrency'] == 1
            assert sql('SELECT state,attempts FROM images WHERE id=?', (rejected['id'],)) == [('queued', 0)]
            for identifier, payload, captured in restored:
                assert sql('SELECT state,attempts,policy FROM images WHERE id=?', (identifier,)) == [('received', 0, captured)]
                assert (root / 'data/.incoming' / identifier / 'input').read_bytes() == payload
                assert 'normal startup' in admin.request('/api/v1/uploads/' + identifier)[0]['error']
            assert wait_image(admin, admin.upload(png())['id'])['format'] == 'webp'
            stop()
            maximum.write_text('max')
            start(restricted=False)
            assert not state()['restricted']
            assert wait_image(admin, rejected['id'])['format'] == 'avif', 'Normal restart did not resume the original AVIF snapshot'
            for identifier, _, captured in restored:
                assert wait_image(admin, identifier)['format'] == 'avif', 'Normal startup lost a received AVIF task'
                assert sql('SELECT policy FROM images WHERE id=?', (identifier,)) == [(captured,)]
            assert public.request(image['url'])[0] == original_public
        finally:
            stop()
            log.close()
    print('Resource integration passed: real service/helper/Bash, restricted reload and AVIF boundaries, monitor fault/recovery, queued/received AVIF resume (read-only metadata fixtures; no enforced-memory or independent-system benchmark).')


if __name__ == '__main__':
    main()
