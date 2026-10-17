#!/usr/bin/env python3
"""Real account/ban/reset checks use generated credentials and isolated deployments."""
import hashlib
import http.client
import os
from pathlib import Path
import secrets
import shutil
import socket
import sqlite3
import subprocess
import tempfile
import time
import urllib.request

from test_uploads import Client, png, wait_image

ROOT = Path(__file__).resolve().parents[2]
OUTPUT = Path(os.environ.get('PEBRIL_TEST_OUTPUT', str(ROOT / 'dist')))


def main():
    (ROOT / '.build').mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='accounts-', dir=ROOT / '.build') as temporary:
        root = Path(temporary)
        executable = root / 'pebril'
        shutil.copy2(OUTPUT / 'pebril', executable)
        shutil.copytree(OUTPUT / 'bin', root / 'bin', symlinks=True)
        if runtime := os.environ.get('PEBRIL_TEST_RUNTIME'):
            shutil.copy2(runtime, root / 'bin/libvips.so.42')
        with socket.socket() as reserved:
            reserved.bind(('127.0.0.1', 0))
            port = reserved.getsockname()[1]
        (root / '.env').write_text(f'PEBRIL_LISTEN=127.0.0.1:{port}\n')
        environment = {key: value for key, value in os.environ.items() if not key.startswith('PEBRIL_')}
        origin = f'http://127.0.0.1:{port}'
        database = root / 'data/db/pebril.db'
        process = None

        def start(log):
            nonlocal process
            process = subprocess.Popen([str(executable)], cwd='/', env=environment, stdout=log, stderr=log)
            deadline = time.monotonic() + 15
            while time.monotonic() < deadline:
                assert process.poll() is None, 'Service stopped before readiness'
                try:
                    with urllib.request.urlopen(origin + '/healthz', timeout=.3):
                        return
                except OSError:
                    time.sleep(.05)
            raise AssertionError('Service readiness timeout')

        def stop():
            nonlocal process
            if process is not None:
                process.terminate()
                try:
                    assert process.wait(timeout=15) == 0
                finally:
                    if process.poll() is None:
                        process.kill()
                        process.wait()
                    process = None

        with (root / 'service.log').open('w+') as log:
            try:
                start(log)
                admin, member, second = Client(origin), Client(origin), Client(origin)
                admin_credentials = {'username': secrets.token_hex(8), 'password': secrets.token_urlsafe(24)}
                member_credentials = {'username': secrets.token_hex(8), 'password': secrets.token_urlsafe(24)}
                member.request('/api/v1/register', 'POST', member_credentials, expected=409)
                admin_user = admin.request('/api/v1/initialize', 'POST', admin_credentials)[0]['user']
                user = member.request('/api/v1/register', 'POST', member_credentials)[0]['user']
                assert not user['admin'] and user['status'] == 'active' and user['quota'] == 1 << 30
                second.request('/api/v1/login', 'POST', member_credentials)
                member.request('/api/v1/admin/users', expected=403)
                token = member.request('/api/v1/upload-token', 'POST', expected=201)[0]['token']
                member.request('/api/v1/admin/users', token=token, expected=403)
                target = f"/api/v1/admin/users/{user['id']}"
                own_target = f"/api/v1/admin/users/{admin_user['id']}"
                admin.request(own_target, 'PATCH', {'quota': '0'}, expected=403)
                admin.request(own_target, 'PATCH', {'status': 'banned'}, expected=403)
                admin.request(target, 'PATCH', {'quota': '-1'}, expected=400)
                exact = '9007199254740993'
                updated = admin.request(target, 'PATCH', {'quota': exact})[0]['user']
                assert updated['quota'] == exact and isinstance(updated['used'], str)
                admin.request(target, 'PATCH', {'quota': 1 << 30})

                completed = wait_image(member, member.upload(png())['id'])
                public = member.request(completed['url'])[0]
                original_hash = hashlib.sha256(public).digest()
                with sqlite3.connect(database) as db:
                    before = {row[0] for row in db.execute('SELECT id FROM images')}
                boundary = secrets.token_hex(12)
                prefix = (f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="image.png"'
                          '\r\nContent-Type: image/png\r\n\r\n').encode()
                payload = prefix + png() + f'\r\n--{boundary}--\r\n'.encode()
                connection = http.client.HTTPConnection('127.0.0.1', port, timeout=15)
                try:
                    connection.putrequest('POST', '/api/v1/uploads')
                    for name, value in {
                        'Content-Type': 'multipart/form-data; boundary=' + boundary,
                        'Content-Length': str(len(payload)),
                        'Origin': origin,
                        'X-CSRF-Token': member.csrf,
                        'Cookie': '; '.join(cookie.name + '=' + cookie.value for cookie in member.jar),
                    }.items():
                        connection.putheader(name, value)
                    connection.endheaders()
                    split = len(prefix) + 8
                    connection.send(payload[:split])
                    deadline = time.monotonic() + 5
                    while time.monotonic() < deadline:
                        with sqlite3.connect(database) as db:
                            receiving = [row[0] for row in db.execute("SELECT id FROM images WHERE state='receiving'") if row[0] not in before]
                        if receiving:
                            break
                        time.sleep(.02)
                    else:
                        raise AssertionError('Partial upload was not durably accepted')
                    admin.request(target, 'PATCH', {'status': 'banned', 'quota': '0'})
                    assert member.request('/api/v1/account')[0]['user']['status'] == 'banned'
                    member.request('/api/v1/upload-token', 'POST', expected=201)
                    member.request('/api/v1/upload-token', 'DELETE')
                    token = member.request('/api/v1/upload-token', 'POST', expected=201)[0]['token']
                    member.upload(png(), expected=403)
                    member.upload(png(), expected=401, token=token)
                    Client(origin).request('/api/v1/login', 'POST', member_credentials, expected=401)
                    assert hashlib.sha256(member.request(completed['url'])[0]).digest() == original_hash
                    connection.send(payload[split:])
                    response = connection.getresponse()
                    assert response.status in (201, 202), 'Ban cancelled accepted reception'
                    response.read()
                finally:
                    connection.close()
                received = wait_image(member, receiving[0])
                assert received['state'] == 'completed'
                assert member.request('/api/v1/account')[0]['user']['used'] > 0

                stop()
                start(log)
                assert member.request('/api/v1/account')[0]['user']['status'] == 'banned'
                assert hashlib.sha256(member.request(completed['url'])[0]).digest() == original_hash
                assert member.request('/api/v1/uploads/' + received['id'])[0]['state'] == 'completed'
                admin.request(target, 'PATCH', {'status': 'active'})
                member.upload(png(), expected=429)
                member.upload(png(), expected=429, token=token)
                new_password = secrets.token_urlsafe(24)
                admin.request(target + '/password', 'POST', {'password': new_password})
                assert 'user' not in member.request('/api/v1/account')[0]
                assert 'user' not in second.request('/api/v1/account')[0]
                member.request('/api/v1/upload-token', 'POST', expected=401)
                member.upload(png(), expected=401, token=token)
                member.request('/api/v1/login', 'POST', member_credentials, expected=401)
                member.request('/api/v1/login', 'POST', dict(member_credentials, password=new_password))
                assert member.request('/api/v1/account')[0]['user']['quota'] == 0
                admin.request(target, 'PATCH', {'quota': '1073741824'})
                assert wait_image(member, member.upload(png())['id'])['state'] == 'completed'
                print('Account integration passed: registration, exact quotas, permissions, ban during real reception/conversion, existing links/restart, zero quota and reset revocation.')
            finally:
                stop()


if __name__ == '__main__':
    main()
