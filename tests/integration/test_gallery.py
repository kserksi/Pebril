#!/usr/bin/env python3
"""Gallery/lifecycle HTTP acceptance with disposable native storage and random accounts."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import secrets
import shlex
import shutil
import socket
import sqlite3
import struct
import subprocess
import sys
import tempfile
import time
import urllib.request
import zlib

sys.dont_write_bytecode = True
from test_uploads import Client, ROOT, OUTPUT, png, raster_fixtures, wait_image
from test_conversion import Inspector, chunk, rgba_png


def wait_for(check, description, timeout=8):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = check()
        if value:
            return value
        time.sleep(.05)
    raise AssertionError(description)


def fixture(root, separate_filesystems):
    executable = root / 'pebril'
    shutil.copy2(OUTPUT / 'pebril', executable)
    shutil.copytree(OUTPUT / 'bin', root / 'bin', symlinks=True)
    if runtime := os.environ.get('PEBRIL_TEST_RUNTIME'):
        shutil.copy2(runtime, root / 'bin/libvips.so.42')
    data, public = root / 'data', root / 'public'
    data.mkdir()
    public.mkdir(exist_ok=True)
    assert (data.stat().st_dev != public.stat().st_dev) == separate_filesystems
    with socket.socket() as reservation:
        reservation.bind(('127.0.0.1', 0))
        port = reservation.getsockname()[1]
    (root / '.env').write_text(f'PEBRIL_LISTEN=127.0.0.1:{port}\nPEBRIL_DATA_DIR={data}\nPEBRIL_PUBLIC_DIR={public}\n')
    env = {key: value for key, value in os.environ.items() if not key.startswith('PEBRIL_')}
    env['PEBRIL_AVIF'] = 'true'
    capability = json.loads(subprocess.check_output([str(executable), 'check'], cwd='/', env=env, timeout=180))
    fixtures = raster_fixtures(capability['vips_path'], root)
    fixtures['png'] = png()
    inspect = Inspector(capability['vips_path'])
    policy = json.loads((ROOT / 'configs/config.example.json').read_text())
    policy['upload_formats']['avif'] = True
    (root / 'config.json').write_text(json.dumps(policy))
    database = data / 'db/pebril.db'
    objects = public / '.objects'
    origin = f'http://127.0.0.1:{port}'
    admin, owner, other, anonymous = (Client(origin) for _ in range(4))
    process = None
    worker, real_worker = root / 'bin/pebril-image-worker', root / 'bin/worker.real'

    def sql(statement, parameters=()):
        with sqlite3.connect(database) as connection:
            return connection.execute(statement, parameters).fetchall()

    def configure(**changes):
        revision = admin.request('/api/v1/status')[0]['config_revision']
        policy.update(changes)
        replacement = root / 'replacement.json'
        replacement.write_text(json.dumps(policy))
        replacement.replace(root / 'config.json')
        wait_for(lambda: admin.request('/api/v1/status')[0]['config_revision'] > revision, 'Configuration reload timed out')

    def start(log):
        nonlocal process
        process = subprocess.Popen([str(executable)], cwd='/', env=env, stdout=log, stderr=log)
        def ready():
            assert process.poll() is None, 'Service exited before readiness'
            try:
                anonymous.request('/healthz')
                return True
            except OSError:
                return False
        wait_for(ready, 'Service readiness timeout', 20)

    def stop():
        nonlocal process
        if process and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
                raise
            assert process.returncode == 0, 'Service did not stop cleanly'
        process = None

    def details(client, identifier):
        value, headers = client.request('/api/v1/images/' + identifier)
        assert headers['Cache-Control'] == 'no-store'
        assert not {'hash', 'policy', 'generation', 'user_id'} & value.keys()
        for file in value['files']:
            assert set(file) == {'role', 'format', 'bytes', 'width', 'height', 'frames'}
        return value

    def used():
        return owner.request('/api/v1/account')[0]['user']['used']

    def charge_consistent():
        logical = sql('SELECT coalesce(sum(bytes),0) FROM (SELECT image_id,image_files.hash,max(image_files.bytes) AS bytes FROM image_files JOIN images ON images.id=image_id WHERE user_id=? GROUP BY image_id,image_files.hash)', (owner_id,))[0][0]
        assert used() == logical, 'Account charge differs from distinct per-record file bytes'

    def original(client, image, expected):
        content, headers = client.request('/api/v1/images/' + image['id'] + '/original')
        assert content == expected
        for name in ['Cache-Control', 'CDN-Cache-Control', 'Cloudflare-CDN-Cache-Control']:
            assert headers[name] == 'no-store', (name, headers[name])
        assert headers['Content-Disposition'].startswith('attachment;')
        assert image['id'] in headers['Content-Disposition']
        assert 'sandbox' in headers['Content-Security-Policy']

    def remove(client, identifier):
        started = time.monotonic()
        assert client.request('/api/v1/images/' + identifier, 'DELETE')[0] == {'deleted': True}
        assert time.monotonic() - started < 5, 'Deletion did not promptly cancel the image task'
        owner.request('/api/v1/images/' + identifier, expected=404)
        anonymous.request('/image/' + identifier, expected=404)
        anonymous.request('/image/' + identifier + '/cover', expected=404)

    def restore_worker():
        if real_worker.exists():
            worker.unlink(missing_ok=True)
            real_worker.rename(worker)

    try:
        with (root / 'server.log').open('w+') as log:
            start(log)
            admin.request('/api/v1/initialize', 'POST', {'username': secrets.token_hex(8), 'password': secrets.token_urlsafe(24)})
            owner.request('/api/v1/register', 'POST', {'username': secrets.token_hex(8), 'password': secrets.token_urlsafe(24)})
            other.request('/api/v1/register', 'POST', {'username': secrets.token_hex(8), 'password': secrets.token_urlsafe(24)})
            owner_id = owner.request('/api/v1/account')[0]['user']['id']
            token = owner.request('/api/v1/upload-token', 'POST', expected=201)[0]['token']
            first = wait_image(owner, owner.upload(fixtures['png'])['id'])
            second = wait_image(owner, owner.upload(fixtures['png'])['id'])
            assert first['id'] != second['id'] and first['url'] != second['url']
            assert details(owner, first['id'])['original_available']
            assert details(admin, first['id'])['owner_id'] == owner_id
            for actor, expected in [(anonymous, 401), (other, 404)]:
                for suffix in ['', '/original']:
                    actor.request('/api/v1/images/' + first['id'] + suffix, expected=expected)
                actor.request('/api/v1/images/' + first['id'], 'DELETE', expected=expected)
            for path, method in [('/api/v1/images', 'GET'), ('/api/v1/images/' + first['id'], 'GET'), ('/api/v1/images/' + first['id'] + '/original', 'GET'), ('/api/v1/images/' + first['id'], 'DELETE')]:
                anonymous.request(path, method, token=token, expected=403)
            owner.request('/api/v1/images/' + first['id'], 'DELETE', csrf=False, expected=403)
            owner.request('/api/v1/images/' + first['id'], 'DELETE', origin=False, expected=403)
            owner.request('/api/v1/images?scope=all', expected=403)
            anonymous.request('/api/v1/images', expected=401)
            assert other.request('/api/v1/images')[0]['images'] == []
            other_shared = wait_image(other, other.upload(fixtures['png'])['id'])
            page = owner.request('/api/v1/images?limit=1&offset=0')[0]
            assert page['total'] == 2 and page['limit'] == 1 and page['offset'] == 0
            assert page['images'][0]['id'] == second['id']
            assert owner.request('/api/v1/images?limit=1&offset=1')[0]['images'][0]['id'] == first['id']
            assert {image['id'] for image in admin.request('/api/v1/images?scope=all')[0]['images']} == {first['id'], second['id'], other_shared['id']}
            assert admin.request('/api/v1/images')[0]['images'] == []
            for query in ['limit=0', 'limit=101', 'offset=-1', 'offset=01', 'scope=unknown', 'limit=1&limit=2', 'other=1']:
                owner.request('/api/v1/images?' + query, expected=400)
            original(owner, first, fixtures['png'])
            original(admin, first, fixtures['png'])
            anonymous.request('/image/' + first['id'] + '/original', expected=404)
            configure(allow_original_download=False)
            assert not details(owner, first['id'])['original_available']
            owner.request('/api/v1/images/' + first['id'] + '/original', expected=403)
            configure(allow_original_download=True)
            original(owner, first, fixtures['png'])

            # Deleting one independent record preserves shared objects and releases its own full charge.
            shared = {row[0] for row in sql('SELECT hash FROM image_files WHERE image_id=?', (first['id'],))}
            assert shared == {row[0] for row in sql('SELECT hash FROM image_files WHERE image_id=?', (second['id'],))}
            before = used()
            remove(admin, first['id'])
            assert used() == before - first['storage_bytes']
            for digest in shared:
                assert (objects / digest).exists()
            assert anonymous.request(second['url'])[1]['Content-Type'] == 'image/webp'
            original(owner, second, fixtures['png'])
            remove(owner, second['id'])
            assert used() == 0
            assert all((objects / digest).exists() for digest in shared), "Deleting one owner removed another owner's files"
            original(other, other_shared, fixtures['png'])
            remove(other, other_shared['id'])
            assert other.request('/api/v1/account')[0]['user']['used'] == 0
            assert all(not (objects / digest).exists() for digest in shared)

            # Stable PNG encoding gives identical original/result bytes, charged once per record.
            configure(static_format='png')
            normalized = wait_image(owner, owner.upload(rgba_png())['id'])
            normalized_bytes = anonymous.request(normalized['url'])[0]
            identical = wait_image(owner, owner.upload(normalized_bytes)['id'])
            assert anonymous.request(identical['url'])[0] == normalized_bytes
            roles = details(owner, identical['id'])['files']
            assert {file['role'] for file in roles} == {'original', 'result'}
            assert identical['storage_bytes'] == len(normalized_bytes)
            assert len(sql('SELECT DISTINCT hash FROM image_files WHERE image_id=?', (identical['id'],))) == 1
            remove(owner, identical['id'])
            assert anonymous.request(normalized['url'])[0] == normalized_bytes

            configure(static_format='webp')
            avif = wait_image(owner, owner.upload(fixtures['avif'])['id'])
            assert avif['format'] == 'webp'
            original(owner, avif, fixtures['avif'])
            assert owner.request('/api/v1/images/' + avif['id'] + '/original')[1]['Content-Type'] == 'image/avif'
            animated = wait_image(owner, owner.upload(fixtures['gif'])['id'])
            assert {file['role'] for file in details(owner, animated['id'])['files']} == {'original', 'result', 'cover'}
            original(owner, animated, fixtures['gif'])
            assert anonymous.request(animated['cover_url'])[1]['Content-Type'] == 'image/webp'
            svg_bytes = b'<svg xmlns="http://www.w3.org/2000/svg" width="2" height="2"><script>bad()</script><rect width="2" height="2"/></svg>'
            svg = owner.upload(svg_bytes, expected=201)
            safe = anonymous.request(svg['url'])[0]
            assert b'<script' not in safe
            svg_detail = details(owner, svg['id'])
            assert [file['role'] for file in svg_detail['files']] == ['result'] and not svg_detail['original_available']
            owner.request('/api/v1/images/' + svg['id'] + '/original', expected=404)

            configure(keep_original=False, output_width=3, output_height=3)
            resized = wait_image(owner, owner.upload(rgba_png(8, 4))['id'])
            value = details(owner, resized['id'])
            assert len(value['files']) == 1 and value['files'][0]['role'] == 'result' and not value['original_available']
            assert (value['format'], value['width'], value['height']) == ('webp', 3, 2)
            owner.request('/api/v1/images/' + resized['id'] + '/original', expected=404)
            downloaded = root / 'resized.webp'
            downloaded.write_bytes(anonymous.request(resized['url'])[0])
            decoded = inspect.read(downloaded)
            assert (decoded['width'], decoded['height']) == (3, 2)
            charge_consistent()
            configure(keep_original=True, output_width=0, output_height=0)

            # Admission may overshoot quota for an accepted image; deletion makes room again.
            initial_charge = used()
            admin.request('/api/v1/admin/users/' + str(owner_id), 'PATCH', {'quota': str(initial_charge + 1)})
            overshoot = wait_image(owner, owner.upload(rgba_png(9, 5))['id'])
            assert used() > initial_charge + 1
            owner.upload(fixtures['png'], expected=429)
            remove(owner, overshoot['id'])
            assert used() == initial_charge
            reopened = wait_image(owner, owner.upload(fixtures['png'])['id'])
            remove(owner, reopened['id'])
            admin.request('/api/v1/admin/users/' + str(owner_id), 'PATCH', {'quota': str(1 << 30)})

            # A real incomplete HTTP upload is cancelled while its sender remains blocked.
            boundary = secrets.token_hex(16)
            cookie = '; '.join(f'{cookie.name}={cookie.value}' for cookie in owner.jar)
            multipart = f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="pending.png"\r\nContent-Type: image/png\r\n\r\n'.encode()
            with socket.create_connection(('127.0.0.1', port), timeout=5) as slow:
                slow.settimeout(5)
                headers = (f'POST /api/v1/uploads HTTP/1.1\r\nHost: 127.0.0.1:{port}\r\nOrigin: {origin}\r\nCookie: {cookie}\r\nX-CSRF-Token: {owner.csrf}\r\nContent-Type: multipart/form-data; boundary={boundary}\r\nContent-Length: 1048576\r\nConnection: close\r\n\r\n').encode()
                slow.sendall(headers + multipart + fixtures['png'][:16])
                def receiving():
                    return next((image for image in admin.request('/api/v1/images?scope=all')[0]['images'] if image['state'] == 'receiving'), None)
                pending = wait_for(receiving, 'Slow multipart upload was not discoverable')
                remove(admin, pending['id'])
                try:
                    response = slow.recv(4096)
                except ConnectionResetError:
                    response = b''
                assert not response or response.startswith(b'HTTP/1.1 400'), 'Cancelled upload did not fail its HTTP request'
            assert not (data / '.incoming' / pending['id']).exists()
            assert not sql('SELECT id FROM images WHERE id=?', (pending['id'],))

            # Exercise cancellation and reaping of an actual controlled worker process.
            worker.rename(real_worker)
            marker = root / 'worker.pid'
            worker.write_text('#!/bin/sh\nif [ "$1" = check ]; then exec ' + shlex.quote(str(real_worker)) + ' "$@"; fi\necho "$$" > ' + shlex.quote(str(marker)) + '\nexec sleep 60\n')
            worker.chmod(0o700)
            converting = owner.upload(rgba_png(10, 6))
            wait_image(owner, converting['id'], 'processing')
            wait_for(marker.exists, 'Controlled worker did not start')
            worker_pid = int(marker.read_text())
            remove(admin, converting['id'])
            wait_for(lambda: not Path(f'/proc/{worker_pid}').exists(), 'Deleted image worker was not reaped')
            assert not (data / '.converting' / converting['id']).exists()
            restore_worker()
            charge_consistent()

            # Open original downloads finish after deletion; future requests are revoked.
            width, height = 1024, 1024
            noise = b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('>IIBBBBB', width, height, 8, 2, 0, 0, 0)) + chunk(b'IDAT', zlib.compress(b''.join(b'\0' + secrets.token_bytes(width * 3) for _ in range(height)))) + chunk(b'IEND', b'')
            streaming = wait_image(owner, owner.upload(noise)['id'])
            request = urllib.request.Request(origin + '/api/v1/images/' + streaming['id'] + '/original')
            with owner.opener.open(request, timeout=15) as stream:
                assert stream.headers['Content-Disposition'].startswith('attachment;')
                prefix = stream.read(64)
                remove(owner, streaming['id'])
                assert prefix + stream.read() == noise
            owner.request('/api/v1/images/' + streaming['id'] + '/original', expected=404)
            assert not (objects / hashlib.sha256(noise).hexdigest()).exists()
            charge_consistent()

            # Startup reconciles only unreferenced/task-owned leftovers and keeps live roles.
            preserved_content = anonymous.request(normalized['url'])[0]
            live_hashes = {row[0] for row in sql('SELECT DISTINCT hash FROM image_files')}
            stop()
            env['PEBRIL_AVIF'] = 'false'
            policy['upload_formats']['avif'] = False
            policy['static_format'] = 'webp'
            (root / 'config.json').write_text(json.dumps(policy))
            orphan_hash = hashlib.sha256(secrets.token_bytes(32)).hexdigest()
            (objects / orphan_hash).write_bytes(b'unpublished object')
            (objects / '.object-interrupted').write_bytes(b'interrupted staging')
            (objects / 'foreign-file').write_bytes(b'outside owned filename patterns')
            orphan_id = secrets.token_hex(16)
            for directory in [data / '.incoming' / orphan_id, data / '.converting' / orphan_id]:
                directory.mkdir(parents=True)
                (directory / 'input').write_bytes(fixtures['png'])
            start(log)
            assert not (objects / orphan_hash).exists() and not (objects / '.object-interrupted').exists()
            assert (objects / 'foreign-file').read_bytes() == b'outside owned filename patterns'
            assert all((objects / digest).exists() for digest in live_hashes)
            assert not (data / '.incoming' / orphan_id).exists() and not (data / '.converting' / orphan_id).exists()
            assert anonymous.request(normalized['url'])[0] == preserved_content
            assert details(owner, resized['id'])['format'] == 'webp'
            assert not admin.request('/api/v1/status')[0]['native']['avif_enabled']
            owner.upload(fixtures['avif'], expected=422)
            avif_detail = details(owner, avif['id'])
            assert avif_detail['original_available']
            assert next(file for file in avif_detail['files'] if file['role'] == 'original')['format'] == 'avif'
            original(owner, avif, fixtures['avif'])
            assert owner.request('/api/v1/images/' + avif['id'] + '/original')[1]['Content-Type'] == 'image/avif'
            charge_consistent()
            stop()
    finally:
        stop()
        restore_worker()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--fixture-root', type=Path, help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.fixture_root:
        fixture(args.fixture_root, True)
        return
    (ROOT / '.build').mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='gallery-integration-', dir=ROOT / '.build') as temporary:
        root = Path(temporary)
        (root / 'public').mkdir()
        if shutil.which('bwrap'):
            sandbox = ['bwrap', '--unshare-user', '--uid', str(os.getuid()), '--gid', str(os.getgid()), '--die-with-parent',
                       '--ro-bind', '/', '/', '--bind', str(root), str(root), '--tmpfs', str(root / 'public'),
                       '--proc', '/proc', '--dev', '/dev', '--chdir', '/']
            subprocess.run(sandbox + [sys.executable, str(Path(__file__).resolve()), '--fixture-root', str(root)], check=True, env=dict(os.environ, PYTHONDONTWRITEBYTECODE='1'))
            storage = 'separate native/public filesystems'
        else:
            fixture(root, False)
            storage = 'same filesystem; separate-filesystem coverage SKIPPED (bubblewrap unavailable)'
    print('Gallery integration passed: owner/admin/token authorization, paging, protected original downloads/live switches and retained AVIF after disablement, byte identity and distinct-role charging, animation/SVG/resize roles, shared deletion, quota reopening, real slow multipart cancellation, controlled worker reaping, concurrent original deletion and restart reconciliation; ' + storage + '.')


if __name__ == '__main__':
    main()
