#!/usr/bin/env python3
"""Upload acceptance checks. Credentials are generated or supplied through private stdin, never stored in source."""
import argparse
import atexit
import ctypes
import hashlib
import http.cookiejar
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import sqlite3
import struct
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import zlib

ROOT = Path(__file__).resolve().parents[2]
OUTPUT = Path(os.environ.get('PEBRIL_TEST_OUTPUT', str(ROOT / 'dist')))


def png():
    def chunk(kind, data):
        return struct.pack('>I', len(data)) + kind + data + struct.pack('>I', zlib.crc32(kind + data))
    return b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('>IIBBBBB', 2, 2, 8, 2, 0, 0, 0)) + chunk(b'IDAT', zlib.compress(b'\0' + b'\xff\0\0' * 2 + b'\0' + b'\0\xff\0' * 2)) + chunk(b'IEND', b'')

def raster_fixtures(library, directory):
    vips = ctypes.CDLL(library)
    vips.vips_init.argtypes = [ctypes.c_char_p]
    assert vips.vips_init(b'pebril-test-fixtures') == 0
    vips.vips_image_new_from_memory.argtypes = [ctypes.c_void_p, ctypes.c_size_t, ctypes.c_int, ctypes.c_int, ctypes.c_int, ctypes.c_int]
    vips.vips_image_new_from_memory.restype = ctypes.c_void_p
    vips.vips_image_write_to_file.argtypes = [ctypes.c_void_p, ctypes.c_char_p]
    vips.vips_image_set_int.argtypes = [ctypes.c_void_p, ctypes.c_char_p, ctypes.c_int]
    vips.vips_image_set_array_int.argtypes = [ctypes.c_void_p, ctypes.c_char_p, ctypes.POINTER(ctypes.c_int), ctypes.c_int]
    vips.g_object_unref.argtypes = [ctypes.c_void_p]
    output = {}
    for animated in (False, True):
        pixels = ctypes.create_string_buffer(bytes([255, 0, 0, 0, 255, 0]) * 2 + (bytes([0, 0, 255, 255, 255, 0]) * 2 if animated else b''))
        image = vips.vips_image_new_from_memory(pixels, 24 if animated else 12, 2, 4 if animated else 2, 3, 0)
        assert image
        if animated:
            vips.vips_image_set_int(image, b'page-height', 2)
            vips.vips_image_set_int(image, b'loop', 0)
            delay = (ctypes.c_int * 2)(50, 50)
            vips.vips_image_set_array_int(image, b'delay', delay, 2)
        for extension in (('gif', 'webp') if animated else ('jpg', 'webp', 'avif', 'tiff', 'v')):
            name = ('animated-' if animated else '') + extension
            path = directory / name
            path = path.with_suffix('.' + extension)
            assert vips.vips_image_write_to_file(image, os.fsencode(path), None) == 0, name
            output['gif' if animated and extension == 'gif' else name] = path.read_bytes()
        vips.g_object_unref(image)
    return output

class Client:
    def __init__(self, origin):
        self.origin = origin
        self.jar = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar))
        self.csrf = ''
    def request(self, path, method='GET', body=None, token=None, expected=200, origin=True, csrf=True, content_type='application/json'):
        headers = {'Content-Type': content_type}
        if origin: headers['Origin'] = self.origin
        if csrf: headers['X-CSRF-Token'] = self.csrf
        if token: headers['Authorization'] = 'Bearer ' + token
        if isinstance(body, dict): body = json.dumps(body).encode()
        request = urllib.request.Request(self.origin + path, data=body, headers=headers, method=method)
        try:
            response = self.opener.open(request, timeout=25)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            data = response.read()
            assert response.code in (expected if isinstance(expected, tuple) else (expected,)), (path, response.code, expected)
            value = json.loads(data) if 'application/json' in response.headers.get('Content-Type', '') else data
            if isinstance(value, dict) and 'csrf' in value: self.csrf = value['csrf']
            return value, response.headers
    def upload(self, data, expected=(201,202), token=None, extra=False, name='image.bin'):
        boundary = secrets.token_hex(16)
        body = f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="{name}"\r\nContent-Type: application/octet-stream\r\n\r\n'.encode() + data + b'\r\n'
        if extra: body += f'--{boundary}\r\nContent-Disposition: form-data; name="other"\r\n\r\ninvalid\r\n'.encode()
        body += f'--{boundary}--\r\n'.encode()
        return self.request('/api/v1/uploads', 'POST', body, token=token, expected=expected, content_type='multipart/form-data; boundary=' + boundary)[0]

def wait_image(client, identifier, state='completed', timeout=20):
    deadline=time.monotonic()+timeout
    while time.monotonic()<deadline:
        image=client.request('/api/v1/uploads/'+identifier)[0]
        if image['state']==state: return image
        if state=='completed' and image['state'] in ('failed','retry_wait'): raise AssertionError(('Conversion failed',image))
        time.sleep(.05)
    raise AssertionError(('Image state timeout',state,image))

class RemoteStorage:
    """One isolated S3 namespace prefix. Credentials stay in a private file and are never printed."""
    def __init__(self, connection):
        self.directory = Path(tempfile.mkdtemp(prefix='s3-fixture-', dir=ROOT / '.build'))
        details = json.loads(Path(connection).read_text())
        missing = {'endpoint', 'bucket', 'region', 'access_key_id', 'secret_access_key', 'addressing'} - details.keys()
        assert not missing, f'S3 connection file is missing {sorted(missing)}'
        self.prefix = details.get('prefix', 'pebril-test/').rstrip('/') + '/uploads-' + secrets.token_hex(6) + '/'
        self.details = details
        self.file = self.directory / 'connection.json'
        self.file.write_text(json.dumps(dict(details, prefix=self.prefix)))
        self.file.chmod(0o600)
        self.helper = self.directory / 'storage-fixture'
        build = {key: value for key, value in os.environ.items() if not key.startswith('PEBRIL_')}
        build.update(GOCACHE=str(ROOT / '.build/go-cache'), GOMODCACHE=str(ROOT / '.build/go-mod'), GOPATH=str(ROOT / '.build/go-path'))
        subprocess.run(['go', 'build', '-o', str(self.helper), './tests/integration/storage_fixture'], cwd=ROOT, env=build, check=True)
        atexit.register(self.close)
    def prepare_config(self, target):
        template = json.loads((OUTPUT / 'config.example.json').read_text())
        template['upload_formats']['avif'] = False
        template['storage'] = {'backend': 's3', 's3': dict({key: self.details[key] for key in ('endpoint', 'bucket', 'region', 'access_key_id', 'secret_access_key', 'addressing')}, prefix=self.prefix)}
        target.write_text(json.dumps(template, indent=2))
        target.chmod(0o600)
    def run(self, *arguments):
        return subprocess.run([str(self.helper), '-config', str(self.file), '-prefix', self.prefix, *arguments], capture_output=True, text=True)
    def close(self):
        try: self.run('cleanup')
        except OSError: pass
        shutil.rmtree(self.directory, ignore_errors=True)

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--credentials-stdin', action='store_true')
    parser.add_argument('--data-dir', type=Path, help='Authorized isolated development data; credentials remain hashed here')
    parser.add_argument('--storage', choices=('local', 's3'), default=os.environ.get('PEBRIL_TEST_STORAGE', 'local'), help='Storage backend exercised by this run')
    parser.add_argument('--s3-config', type=Path, help='Private JSON connection file required by --storage s3')
    args = parser.parse_args()
    if args.data_dir and (not args.credentials_stdin or args.data_dir.resolve() != (ROOT / 'data/devtest').resolve()):
        parser.error('Retained development data requires private credentials and the designated isolated directory')
    if (args.storage == 's3') != (args.s3_config is not None):
        parser.error('--storage s3 needs exactly one private --s3-config connection file')
    if args.storage == 's3' and args.data_dir:
        parser.error('Remote storage runs use their own disposable data directory')
    credentials = json.load(__import__('sys').stdin) if args.credentials_stdin else {'admin_user': secrets.token_hex(8), 'admin_password': secrets.token_urlsafe(24), 'user': secrets.token_hex(8), 'password': secrets.token_urlsafe(24)}
    (ROOT / '.build').mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='upload-integration-', dir=ROOT / '.build') as temporary:
        root = Path(temporary)
        executable = root / 'pebril'
        shutil.copy2(OUTPUT / 'pebril', executable)
        shutil.copytree(OUTPUT / 'bin', root / 'bin', symlinks=True)
        if runtime := os.environ.get('PEBRIL_TEST_RUNTIME'):
            shutil.copy2(runtime, root / 'bin/libvips.so.42')
        with socket.socket() as reservation:
            reservation.bind(('127.0.0.1', 0)); port = reservation.getsockname()[1]
        data = args.data_dir.resolve() if args.data_dir else root / 'data'
        remote = RemoteStorage(args.s3_config) if args.storage == 's3' else None
        if remote: remote.prepare_config(root / 'config.json')
        (root / '.env').write_text(f'PEBRIL_LISTEN=127.0.0.1:{port}\nPEBRIL_DATA_DIR={data}\n')
        env = {key:value for key,value in os.environ.items() if not key.startswith('PEBRIL_')}
        check = subprocess.run([str(executable), 'check'], cwd='/', env=env, capture_output=True, timeout=180)
        assert check.returncode == 0, check.stderr
        capability = json.loads(check.stdout)
        fixtures = raster_fixtures(capability['vips_path'], root)
        fixtures['png'] = png()
        origin = f'http://127.0.0.1:{port}'
        def start(log):
            process = subprocess.Popen([str(executable)], cwd='/', env=env, stdout=log, stderr=log)
            deadline = time.monotonic() + 15
            while time.monotonic() < deadline:
                assert process.poll() is None, 'Service exited before readiness'
                try:
                    with urllib.request.urlopen(origin + '/healthz', timeout=.3): return process
                except OSError: time.sleep(.05)
            process.kill(); process.wait(); raise AssertionError('Readiness timeout')
        database = data / 'db/pebril.db'
        if args.data_dir and database.exists():
            with sqlite3.connect(database) as connection:
                connection.execute('DELETE FROM image_files') if connection.execute("SELECT 1 FROM sqlite_master WHERE name='image_files'").fetchone() else None; connection.execute('DELETE FROM images'); connection.execute('DELETE FROM sessions'); connection.execute('DELETE FROM upload_tokens'); connection.execute('UPDATE users SET used=0,quota=?',(1<<30,))
            for relative in ['.incoming','.converting','image']: shutil.rmtree(data / relative,ignore_errors=True)
        seed_helper = root / 'account-fixture'
        build_env = dict(env, GOCACHE=str(ROOT / '.build/go-cache'), GOMODCACHE=str(ROOT / '.build/go-mod'), GOPATH=str(ROOT / '.build/go-path'))
        subprocess.run(['go','build','-o',str(seed_helper),'./tests/integration/account_fixture'],cwd=ROOT,env=build_env,check=True)
        def update(sql, values=()):
            with sqlite3.connect(database) as connection: connection.execute(sql, values)
        with (root / 'server.log').open('w+') as log:
            process = start(log)
            try:
                admin = Client(origin); ordinary = Client(origin); anonymous = Client(origin)
                initialized = admin.request('/api/v1/account')[0]['initialized']
                if not initialized:
                    admin.request('/api/v1/initialize','POST',{'username':credentials['admin_user'],'password':credentials['admin_password']})
                else:
                    admin.request('/api/v1/login','POST',{'username':credentials['admin_user'],'password':credentials['admin_password']})
                admin.request('/api/v1/initialize','POST',{'username':credentials['admin_user'],'password':credentials['admin_password']},expected=409)
                for cookie in admin.jar: assert cookie._rest.get('HttpOnly') is not None or 'HttpOnly' in cookie._rest
                seed = subprocess.run([str(seed_helper), str(database.parent)], input=json.dumps({'username':credentials['user'],'password':credentials['password']}).encode(), capture_output=True)
                assert seed.returncode == 0, 'Ordinary-account fixture preparation failed'
                ordinary.request('/api/v1/login','POST',{'username':credentials['user'],'password':credentials['password']})
                ordinary_id = ordinary.request('/api/v1/account')[0]['user']['id']
                admin_id = admin.request('/api/v1/account')[0]['user']['id']
                anonymous.upload(fixtures['png'], expected=401)
                ordinary.request('/api/v1/upload-token','POST',expected=403,csrf=False)
                token = ordinary.request('/api/v1/upload-token','POST',expected=201)[0]['token']
                anonymous.request('/api/v1/account',token=token,expected=403)
                ordinary.request('/api/v1/upload-token','POST',token=token,expected=403)
                first = ordinary.upload(fixtures['png'],token=token)
                second = ordinary.upload(fixtures['png'],name='../../untrusted.jpg')
                assert first['id'] != second['id']
                first = wait_image(ordinary, first['id'])
                second = wait_image(ordinary, second['id'])
                assert first['format'] == second['format'] == 'webp'
                # Lose a real HTTP response while conversion is held, then rediscover the result.
                previous_ids = {image['id'] for image in ordinary.request('/api/v1/images?limit=100')[0]['images']}
                worker = root / 'bin/pebril-image-worker'
                real_worker = worker.with_name('pebril-image-worker.real')
                hold = worker.with_name('hold-disconnected-worker')
                worker.rename(real_worker)
                worker.write_text('#!/bin/bash\nwhile [ -e "$(dirname "$0")/hold-disconnected-worker" ]; do sleep 0.05; done\nexec "$(dirname "$0")/pebril-image-worker.real" "$@"\n')
                worker.chmod(0o700)
                hold.touch()
                try:
                    boundary = secrets.token_hex(16)
                    body = f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="lost-response.png"\r\nContent-Type: image/png\r\n\r\n'.encode() + fixtures['png'] + f'\r\n--{boundary}--\r\n'.encode()
                    headers = f'POST /api/v1/uploads HTTP/1.1\r\nHost: 127.0.0.1:{port}\r\nAuthorization: Bearer {token}\r\nContent-Type: multipart/form-data; boundary={boundary}\r\nContent-Length: {len(body)}\r\n\r\n'.encode()
                    with socket.create_connection(('127.0.0.1', port), timeout=5) as connection:
                        connection.sendall(headers + body)
                        deadline = time.monotonic() + 10
                        while time.monotonic() < deadline:
                            records = ordinary.request('/api/v1/images?limit=100')[0]['images']
                            discovered = [image for image in records if image['id'] not in previous_ids]
                            if len(discovered) == 1 and discovered[0]['state'] == 'processing': break
                            time.sleep(.05)
                        assert len(discovered) == 1 and discovered[0]['state'] == 'processing', discovered
                        lost_id = discovered[0]['id']
                    # The connection closes without consuming any response bytes.
                    hold.unlink()
                    complete = wait_image(ordinary, lost_id)
                    assert complete['state'] == 'completed' and complete['format'] == 'webp'
                    recovered = ordinary.request('/api/v1/images/' + lost_id)[0]
                    assert recovered['state'] == 'completed' and recovered['url'] == complete['url']
                    assert anonymous.request(recovered['url'])[0]
                    ordinary.request('/api/v1/images/' + lost_id, 'DELETE')
                finally:
                    hold.unlink(missing_ok=True)
                    worker.unlink()
                    real_worker.rename(worker)
                ordinary.request('/api/v1/uploads/' + first['id'])
                admin.request('/api/v1/uploads/' + first['id'],expected=404)
                anonymous.request('/api/v1/uploads/' + first['id'],token=token,expected=403)
                svg = b'<svg xmlns="http://www.w3.org/2000/svg" width="2" height="2"><script>alert(1)</script><rect width="2" height="2" onload="alert(1)"/></svg>'
                safe = ordinary.upload(svg, expected=201)
                content, headers = anonymous.request(safe['url'])
                assert b'<script' not in content and b'onload=' not in content and b'<rect' in content
                assert headers['Cache-Control'] == 'public, max-age=259200, s-maxage=1296000'
                assert 'sandbox' in headers['Content-Security-Policy']
                digest = hashlib.sha256(content).hexdigest()
                if remote:
                    assert remote.run('exists', digest).returncode == 0, 'Sanitized copy is missing from remote storage'
                else:
                    assert (data / 'image/public/.objects' / digest).exists()
                assert anonymous.request('/image/' + first['id'])[1]['Content-Type'] == 'image/webp'
                with sqlite3.connect(database) as connection:
                    hashes = connection.execute('SELECT hash FROM images WHERE id IN (?,?)',(first['id'],second['id'])).fetchall()
                    assert hashes[0] == hashes[1]
                    assert connection.execute('SELECT used FROM users WHERE id=?',(ordinary_id,)).fetchone()[0] == first['storage_bytes']+second['storage_bytes']+len(content)
                    assert connection.execute("SELECT count(*) FROM image_files WHERE role='original' AND hash=?",(hashlib.sha256(fixtures['png']).hexdigest(),)).fetchone()[0] == 2
                for key in ['jpg','webp','gif','animated-webp']:
                    uploaded = wait_image(ordinary, ordinary.upload(fixtures[key])['id'])
                    assert uploaded['frames'] == (2 if key in ('gif','animated-webp') else 1), (key,uploaded)
                ordinary.upload(fixtures['avif'],expected=422)
                for unsupported in ['tiff', 'v']:
                    ordinary.upload(fixtures[unsupported],name='disguised.png',expected=422)
                ordinary.upload(b'<svg xmlns="http://www.w3.org/2000/svg"><rect>',expected=422)
                ordinary.upload(b'<html><script>alert(1)</script></html>',expected=422)
                ordinary.upload(fixtures['png'][:-5],expected=422)
                ordinary.upload(fixtures['gif'][:-5],expected=422)
                ordinary.upload(fixtures['jpg'][:-20],expected=422)
                ordinary.upload(fixtures['png'],expected=400,extra=True)
                ordinary.upload(b'',expected=400)
                policy = json.loads((root / 'config.json').read_text())
                def configure(**changes):
                    policy.update(changes)
                    revision = admin.request('/api/v1/status')[0]['config_revision']
                    (root / 'config.json').write_text(json.dumps(policy))
                    deadline=time.monotonic()+3
                    while time.monotonic()<deadline:
                        if admin.request('/api/v1/status')[0]['config_revision']>revision: return
                        time.sleep(.05)
                    raise AssertionError('Configuration change was not applied')
                configure(static_max_pixels=3,animated_max_pixels=7)
                ordinary.upload(fixtures['png'],expected=422)
                ordinary.upload(fixtures['animated-webp'],expected=422)
                ordinary.upload(fixtures['gif'],expected=422)
                configure(static_max_pixels=50000000,animated_max_pixels=50000000,static_max_bytes=10,animated_max_bytes=10)
                ordinary.upload(fixtures['png'],expected=413)
                configure(static_max_bytes=31457280,animated_max_bytes=15728640)
                revoked = token
                token = ordinary.request('/api/v1/upload-token','POST',expected=201)[0]['token']
                ordinary.upload(fixtures['png'],token=revoked,expected=401)
                ordinary.request('/api/v1/upload-token','DELETE')
                ordinary.upload(fixtures['png'],token=token,expected=401)
                update("UPDATE users SET status='banned' WHERE id=?",(ordinary_id,))
                ordinary.upload(svg,expected=403)
                banned = ordinary.request('/api/v1/login','POST',{'username':credentials['user'],'password':credentials['password']},expected=403)[0]
                assert banned['code'] == 'account_banned' and 'banned' in banned['error']
                update("UPDATE users SET status='active' WHERE id=?",(ordinary_id,))
                used = admin.request('/api/v1/account')[0]['user']['used']
                update('UPDATE users SET quota=? WHERE id=?',(used+1,admin_id))
                admin.upload(svg,expected=201)
                assert admin.upload(svg,expected=429)['code'] == 'storage_quota'
                update('UPDATE users SET quota=? WHERE id=?',(1<<30,admin_id))
                for _ in range(10):
                    update("INSERT INTO images(id,user_id,state,policy,created_at) VALUES(?,?,'receiving',?,?)",(secrets.token_hex(16),ordinary_id,json.dumps({'revision':1,'policy':policy}),int(time.time()*1000)))
                assert ordinary.upload(svg,expected=429)['code'] == 'upload_capacity'
                ordinary.request('/api/v1/logout','POST')
                ordinary.upload(svg,expected=401)
                assert not list((data / '.incoming').iterdir()), 'Reception or validation left temporary files'
            finally:
                process.terminate()
                try: process.wait(timeout=15)
                except subprocess.TimeoutExpired: process.kill(); process.wait(); raise
            assert process.returncode == 0
            received_id, partial_id, orphan_id = (secrets.token_hex(16) for _ in range(3))
            for identifier in [received_id,partial_id,orphan_id]:
                scratch = data / '.incoming' / identifier; scratch.mkdir(parents=True)
                (scratch / 'input').write_bytes(fixtures['png'])
            snapshot = json.dumps({'revision':1,'policy':policy})
            update("INSERT INTO images(id,user_id,state,policy,created_at) VALUES(?,?,'received',?,?)",(received_id,admin_id,snapshot,int(time.time()*1000)))
            update("INSERT INTO images(id,user_id,state,policy,created_at) VALUES(?,?,'receiving',?,?)",(partial_id,admin_id,snapshot,int(time.time()*1000)))
            policy['upload_formats']['avif']=True
            (root / 'config.json').write_text(json.dumps(policy))
            (root / '.env').write_text((root / '.env').read_text()+'PEBRIL_AVIF=true\n')
            process = start(log)
            try:
                ordinary.request('/api/v1/login','POST',{'username':credentials['user'],'password':credentials['password']})
                recovered = ordinary.request('/api/v1/uploads/' + first['id'])[0]
                assert recovered['state'] == 'completed' and recovered['id'] == first['id']
                assert anonymous.request(safe['url'])[0] == content
                deadline = time.monotonic()+10
                while time.monotonic()<deadline:
                    with sqlite3.connect(database) as connection:
                        states = dict(connection.execute('SELECT id,state FROM images WHERE id IN (?,?)',(received_id,partial_id)).fetchall())
                    if states.get(received_id)=='completed': break
                    time.sleep(.05)
                assert states == {received_id:'completed',partial_id:'failed'}, states
                assert not list((data / '.incoming').iterdir()), 'Recovery left task-owned temporary files'
                admin.request('/api/v1/login','POST',{'username':credentials['admin_user'],'password':credentials['admin_password']})
                avif_image=wait_image(admin,admin.upload(fixtures['avif'])['id'])
                assert avif_image['format']=='webp' and avif_image['state']=='completed'
                assert admin.request('/api/v1/status')[0]['native']['avif_enabled']
            finally:
                process.terminate(); process.wait(timeout=15)
            policy['upload_formats']['avif']=False
            (root / 'config.json').write_text(json.dumps(policy))
            (root / '.env').write_text((root / '.env').read_text().replace('PEBRIL_AVIF=true\n',''))
            process=start(log)
            try:
                admin.request('/api/v1/login','POST',{'username':credentials['admin_user'],'password':credentials['admin_password']})
                admin.upload(fixtures['avif'],expected=422)
                assert admin.request('/api/v1/uploads/'+avif_image['id'])[0]['format']=='webp'
                assert anonymous.request(avif_image['url'])[1]['Content-Type']=='image/webp'
            finally:
                process.terminate();process.wait(timeout=15)
        if args.data_dir:
            # Preserve only the explicitly authorized hashed development accounts.
            with sqlite3.connect(database) as connection:
                connection.execute('DELETE FROM image_files') if connection.execute("SELECT 1 FROM sqlite_master WHERE name='image_files'").fetchone() else None; connection.execute('DELETE FROM images'); connection.execute('DELETE FROM sessions'); connection.execute('DELETE FROM upload_tokens'); connection.execute('UPDATE users SET used=0,quota=?',(1<<30,))
            with sqlite3.connect(database) as connection: connection.execute('PRAGMA wal_checkpoint(TRUNCATE)')
            for relative in ['.incoming','.converting','image']:
                shutil.rmtree(data / relative,ignore_errors=True)
        if remote:
            cleaned = remote.run('cleanup')
            assert cleaned.returncode == 0, cleaned.stderr
            counted = json.loads(remote.run('count').stdout)
            assert counted['objects'] == 0, ('Remote storage cleanup left objects', counted)
    print(f"Upload integration passed ({args.storage} storage): authentication, scopes, content safety, limits, SVG delivery, deduplication, quota, revocation, lost HTTP response recovery, restart persistence and cleanup.")

if __name__ == '__main__': main()
