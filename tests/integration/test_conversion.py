#!/usr/bin/env python3
"""End-to-end conversion checks with isolated storage and private optional account input."""
import argparse
import ctypes
import hashlib
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
import urllib.request
import zlib
from test_uploads import Client, ROOT, raster_fixtures, wait_image


def chunk(kind, value):
    return struct.pack('>I', len(value))+kind+value+struct.pack('>I',zlib.crc32(kind+value))


def rgba_png(width=8, height=4, transparent=False, depth=8):
    pixel=struct.pack('>HHHH',65535,0,0,0 if transparent else 65535) if depth==16 else (b'\xff\0\0\0' if transparent else b'\xff\0\0\xff')
    return (b'\x89PNG\r\n\x1a\n'+chunk(b'IHDR',struct.pack('>IIBBBBB',width,height,depth,6,0,0,0))
            +chunk(b'tEXt',b'Description\0private-fixture-marker')
            +chunk(b'IDAT',zlib.compress((b'\0'+pixel*width)*height))+chunk(b'IEND',b''))


class Inspector:
    def __init__(self, library):
        self.v=ctypes.CDLL(library)
        self.v.vips_init.argtypes=[ctypes.c_char_p]
        assert self.v.vips_init(b'conversion-acceptance')==0
        self.v.vips_cache_set_max(0)
        self.v.vips_image_new_from_file.argtypes=[ctypes.c_char_p]
        self.v.vips_image_new_from_file.restype=ctypes.c_void_p
        for name in ['width','height','bands','n_pages','page_height']:
            getattr(self.v,'vips_image_get_'+name).argtypes=[ctypes.c_void_p]
        self.v.vips_image_get_int.argtypes=[ctypes.c_void_p,ctypes.c_char_p,ctypes.POINTER(ctypes.c_int)]
        self.v.vips_image_get_array_int.argtypes=[ctypes.c_void_p,ctypes.c_char_p,ctypes.POINTER(ctypes.POINTER(ctypes.c_int)),ctypes.POINTER(ctypes.c_int)]
        self.v.vips_image_get_typeof.argtypes=[ctypes.c_void_p,ctypes.c_char_p]
        self.v.vips_image_get_typeof.restype=ctypes.c_size_t
        self.v.vips_image_write_to_memory.argtypes=[ctypes.c_void_p,ctypes.POINTER(ctypes.c_size_t)]
        self.v.vips_image_write_to_memory.restype=ctypes.c_void_p
        self.v.g_free.argtypes=[ctypes.c_void_p]
        self.v.g_object_unref.argtypes=[ctypes.c_void_p]
    def read(self, path, animated=False):
        image=self.v.vips_image_new_from_file(os.fsencode(path),b'n',ctypes.c_int(-1),None) if animated else self.v.vips_image_new_from_file(os.fsencode(path),None)
        assert image, 'Cannot decode generated output'
        try:
            result={name:getattr(self.v,'vips_image_get_'+name)(image) for name in ['width','height','bands','n_pages','page_height']}
            result['icc']=bool(self.v.vips_image_get_typeof(image,b'icc-profile-data'))
            loop=ctypes.c_int(); self.v.vips_image_get_int(image,b'loop',ctypes.byref(loop));result['loop']=loop.value
            delay=ctypes.POINTER(ctypes.c_int)(); count=ctypes.c_int()
            self.v.vips_image_get_array_int(image,b'delay',ctypes.byref(delay),ctypes.byref(count))
            result['delay']=[delay[i] for i in range(count.value)]
            size=ctypes.c_size_t();memory=self.v.vips_image_write_to_memory(image,ctypes.byref(size))
            assert memory
            result['pixels']=ctypes.string_at(memory,size.value);self.v.g_free(memory)
            return result
        finally:self.v.g_object_unref(image)


def metadata_jpeg(inspect, directory):
    v=inspect.v
    source=directory/'metadata.png';source.write_bytes(rgba_png())
    image=v.vips_image_new_from_file(os.fsencode(source),None)
    v.vips_icc_transform.argtypes=[ctypes.c_void_p,ctypes.POINTER(ctypes.c_void_p),ctypes.c_char_p]
    converted=ctypes.c_void_p()
    assert v.vips_icc_transform(image,ctypes.byref(converted),b'srgb',b'input_profile',ctypes.c_char_p(b'srgb'),None)==0
    v.vips_image_set_int.argtypes=[ctypes.c_void_p,ctypes.c_char_p,ctypes.c_int]
    v.vips_image_set_string.argtypes=[ctypes.c_void_p,ctypes.c_char_p,ctypes.c_char_p]
    v.vips_image_set_int(converted,b'orientation',6)
    v.vips_image_set_string(converted,b'exif-ifd0-ImageDescription',b'private-fixture-marker')
    target=directory/'metadata.jpg'
    v.vips_image_write_to_file.argtypes=[ctypes.c_void_p,ctypes.c_char_p]
    assert v.vips_image_write_to_file(converted,os.fsencode(target),None)==0
    v.g_object_unref(converted);v.g_object_unref(image)
    content=target.read_bytes();assert b'private-fixture-marker' in content
    assert inspect.read(target)['icc']
    return content


def transparent_animation(inspect, directory, width=2, height=2):
    v=inspect.v
    pixels=ctypes.create_string_buffer(bytes([255,0,0,255,0,0,0,0])*(width*height//2)+bytes([0,0,0,0,0,255,0,255])*(width*height//2))
    v.vips_image_new_from_memory.argtypes=[ctypes.c_void_p,ctypes.c_size_t,ctypes.c_int,ctypes.c_int,ctypes.c_int,ctypes.c_int]
    v.vips_image_new_from_memory.restype=ctypes.c_void_p
    image=v.vips_image_new_from_memory(pixels,width*height*8,width,height*2,4,0)
    v.vips_image_set_int(image,b'page-height',height);v.vips_image_set_int(image,b'loop',3)
    delay=(ctypes.c_int*2)(70,130)
    v.vips_image_set_array_int.argtypes=[ctypes.c_void_p,ctypes.c_char_p,ctypes.POINTER(ctypes.c_int),ctypes.c_int]
    v.vips_image_set_array_int(image,b'delay',delay,2)
    target=directory/'transparent-animation.webp'
    assert v.vips_image_write_to_file(image,os.fsencode(target),None)==0
    v.g_object_unref(image)
    return target.read_bytes()


def main():
    parser=argparse.ArgumentParser()
    parser.add_argument('--credentials-stdin',action='store_true')
    parser.add_argument('--data-dir',type=Path)
    args=parser.parse_args()
    if args.data_dir and (not args.credentials_stdin or args.data_dir.resolve()!=(ROOT/'data/devtest').resolve()):
        parser.error('Retained data requires private input and the designated development directory')
    credentials=json.load(__import__('sys').stdin) if args.credentials_stdin else {'admin_user':secrets.token_hex(8),'admin_password':secrets.token_urlsafe(24),'user':secrets.token_hex(8),'password':secrets.token_urlsafe(24)}
    with tempfile.TemporaryDirectory(prefix='conversion-integration-',dir=ROOT/'.build') as temporary:
        root=Path(temporary);executable=root/'pebril'
        shutil.copy2(ROOT/'dist/pebril',executable);shutil.copytree(ROOT/'dist/bin',root/'bin',symlinks=True)
        data=args.data_dir.resolve() if args.data_dir else root/'data'
        database=data/'db/pebril.db'
        def clean():
            if database.exists():
                with sqlite3.connect(database) as db:
                    db.execute('DELETE FROM image_files') if db.execute("SELECT 1 FROM sqlite_master WHERE name='image_files'").fetchone() else None;db.execute('DELETE FROM images');db.execute('DELETE FROM sessions');db.execute('DELETE FROM upload_tokens');db.execute("UPDATE users SET used=0,quota=?,status='active'",(1<<30,))
                with sqlite3.connect(database) as db:db.execute('PRAGMA wal_checkpoint(TRUNCATE)')
                for name in ['image','.incoming','.converting']:shutil.rmtree(data/name,ignore_errors=True)
        if args.data_dir:clean()
        with socket.socket() as reservation:reservation.bind(('127.0.0.1',0));port=reservation.getsockname()[1]
        (root/'.env').write_text(f'PEBRIL_LISTEN=127.0.0.1:{port}\nPEBRIL_DATA_DIR={data}\nPEBRIL_AVIF=true\n')
        env={k:v for k,v in os.environ.items() if not k.startswith('PEBRIL_')}
        capability=json.loads(subprocess.check_output([str(executable),'check'],cwd='/',env=env,timeout=180))
        inspect=Inspector(capability['vips_path']);fixtures=raster_fixtures(capability['vips_path'],root)
        fixtures['png']=rgba_png();fixtures['transparent']=rgba_png(transparent=True)
        fixtures['metadata']=metadata_jpeg(inspect,root)
        fixtures['transparent-animation']=transparent_animation(inspect,root)
        policy=json.loads((ROOT/'configs/config.example.json').read_text());policy['upload_formats']['avif']=True
        (root/'config.json').write_text(json.dumps(policy))
        origin=f'http://127.0.0.1:{port}';admin=Client(origin);user=Client(origin);public=Client(origin)
        process=None
        def start(log):
            nonlocal process
            process=subprocess.Popen([str(executable)],cwd='/',env=env,stdout=log,stderr=log)
            deadline=time.monotonic()+15
            while time.monotonic()<deadline:
                assert process.poll() is None,'Startup failed'
                try:
                    with urllib.request.urlopen(origin+'/healthz',timeout=.3):return
                except OSError:time.sleep(.05)
            raise AssertionError('Readiness timeout')
        def stop():
            nonlocal process
            if process and process.poll() is None:
                process.terminate();process.wait(timeout=20);assert process.returncode==0
            process=None
        def sql(statement,parameters=()):
            with sqlite3.connect(database) as db:return db.execute(statement,parameters).fetchall()
        def configure(**values):
            revision=admin.request('/api/v1/status')[0]['config_revision'];policy.update(values)
            (root/'replacement.json').write_text(json.dumps(policy));(root/'replacement.json').replace(root/'config.json')
            deadline=time.monotonic()+4
            while time.monotonic()<deadline:
                if admin.request('/api/v1/status')[0]['config_revision']>revision:return
                time.sleep(.05)
            raise AssertionError('Configuration was not applied')
        def result(image,animated=False):
            content,headers=public.request(image['url']);path=root/'decoded';path.write_bytes(content)
            return content,headers,inspect.read(path,animated)
        worker=root/'bin/pebril-image-worker';saved=root/'bin/worker.real'
        def wrap(body):
            if not saved.exists():worker.rename(saved)
            worker.write_text('#!/bin/sh\n'+body+'\n');worker.chmod(0o700)
        def restore():
            if saved.exists():worker.unlink(missing_ok=True);saved.rename(worker)
        try:
            with (root/'server.log').open('w+') as log:
                start(log)
                if not admin.request('/api/v1/account')[0]['initialized']:
                    admin.request('/api/v1/initialize','POST',{'username':credentials['admin_user'],'password':credentials['admin_password']})
                else:admin.request('/api/v1/login','POST',{'username':credentials['admin_user'],'password':credentials['admin_password']})
                helper=root/'account-fixture'
                build_env=dict(env,GOCACHE=str(ROOT/'.build/go-cache'),GOMODCACHE=str(ROOT/'.build/go-mod'),GOPATH=str(ROOT/'.build/go-path'))
                subprocess.run(['go','build','-o',str(helper),'./tests/integration/account_fixture'],cwd=ROOT,env=build_env,check=True)
                seed=subprocess.run([str(helper),str(database.parent)],input=json.dumps({'username':credentials['user'],'password':credentials['password']}).encode(),capture_output=True)
                assert seed.returncode==0,'Account preparation failed'
                user.request('/api/v1/login','POST',{'username':credentials['user'],'password':credentials['password']})
                user_id=user.request('/api/v1/account')[0]['user']['id']
                for name in ['png','jpg','webp','avif','gif','animated-webp']:
                    image=wait_image(user,user.upload(fixtures[name])['id'])
                    animated=name in ('gif','animated-webp')
                    assert image['format']=='webp' and image['frames']==(2 if animated else 1)
                    content,headers,decoded=result(image,animated)
                    assert headers['Content-Type']=='image/webp' and headers['Cache-Control']=='public, max-age=259200, s-maxage=1296000'
                    assert decoded['n_pages']==(2 if animated else 1)
                    source=sql("SELECT hash FROM image_files WHERE image_id=? AND role='original'",(image['id'],))[0][0]
                    assert (data/'image/public/.objects'/source).read_bytes()==fixtures[name]
                    assert b'private-fixture-marker' not in content
                    if animated:
                        assert decoded['delay']==[50,50] and decoded['loop']==0,decoded
                        cover,cover_headers=public.request(image['cover_url']);(root/'cover').write_bytes(cover)
                        assert cover_headers['Content-Type']=='image/webp' and inspect.read(root/'cover')['n_pages']==1
                large=rgba_png(3840,2160)
                assert len(large)<=10*1024*1024
                image=wait_image(user,user.upload(large)['id'])
                assert (image['width'],image['height'])==(3840,2160)
                animated720=transparent_animation(inspect,root,1280,720)
                assert len(animated720)<=10*1024*1024
                image=wait_image(user,user.upload(animated720)['id'])
                assert (image['width'],image['height'],image['frames'])==(1280,720,2)
                oriented=wait_image(user,user.upload(fixtures['metadata'])['id'])
                content,_,decoded=result(oriented)
                assert (decoded['width'],decoded['height'])==(4,8) and decoded['icc'],{k:v for k,v in decoded.items() if k!='pixels'}
                assert b'private-fixture-marker' not in content
                configure(preserve_orientation=False,preserve_color=False)
                image=wait_image(user,user.upload(fixtures['metadata'])['id']);decoded=result(image)[2]
                assert (decoded['width'],decoded['height'])==(8,4) and not decoded['icc']
                configure(preserve_orientation=True,preserve_color=True)
                transparent=wait_image(user,user.upload(fixtures['transparent'])['id'])
                decoded=result(transparent)[2]
                assert decoded['bands']==4 and all(decoded['pixels'][i]==0 for i in range(3,len(decoded['pixels']),4))
                for format in ['png','jpeg','avif']:
                    configure(static_format=format,output_width=3,output_height=3)
                    image=wait_image(user,user.upload(fixtures['transparent'] if format=='jpeg' else fixtures['png'])['id'])
                    content,headers,decoded=result(image)
                    assert (decoded['width'],decoded['height'])==(3,2),(format,decoded)
                    assert image['format']==format and image['frames']==1
                    if format=='avif':existing_avif=image
                    if format=='jpeg':assert min(decoded['pixels'])>=250,'Transparent JPEG background is not white'
                configure(static_format='jpeg',output_width=0,output_height=0)
                image=wait_image(user,user.upload(rgba_png(transparent=True,depth=16))['id'])
                assert min(result(image)[2]['pixels'])>=250,'16-bit alpha did not composite onto white'
                for format in ['webp','jpeg','avif']:
                    configure(static_format=format,static_quality=0)
                    image=wait_image(user,user.upload(fixtures['transparent'])['id'])
                    assert image['format']==format
                    if format=='jpeg':assert min(result(image)[2]['pixels'])>=245
                configure(static_format='webp',static_quality=80,output_width=0,output_height=0,animated_format='gif')
                image=wait_image(user,user.upload(fixtures['animated-webp'])['id']);decoded=result(image,True)[2]
                assert image['format']=='gif' and decoded['n_pages']==2 and decoded['delay']==[50,50] and decoded['loop']==0
                for format in ['gif','webp']:
                    configure(animated_format=format)
                    image=wait_image(user,user.upload(fixtures['transparent-animation'])['id']);decoded=result(image,True)[2]
                    assert decoded['delay']==[70,130] and decoded['loop']==3 and decoded['bands']==4
                    alpha=decoded['pixels'][3::4]
                    assert alpha[:4]==bytes([255,0,255,0]) and alpha[4:]==bytes([0,255,0,255]),(format,alpha)
                configure(static_max_pixels=3,animated_max_pixels=8)
                image=wait_image(user,user.upload(fixtures['animated-webp'])['id'])
                assert image.get('cover_url')
                configure(static_max_pixels=50000000,animated_max_pixels=50000000)
                configure(keep_original=False,animated_format='webp')
                unique=rgba_png(11,7);source_hash=hashlib.sha256(unique).hexdigest()
                image=wait_image(user,user.upload(unique)['id'])
                assert sql('SELECT role FROM image_files WHERE image_id=?',(image['id'],))==[('result',)]
                assert not (data/'image/public/.objects'/source_hash).exists()
                assert user.request('/api/v1/account')[0]['user']['used']==sql('SELECT sum(bytes) FROM (SELECT image_id,image_files.hash,max(image_files.bytes) AS bytes FROM image_files JOIN images ON images.id=image_id WHERE user_id=? GROUP BY image_id,image_files.hash)',(user_id,))[0][0]
                configure(keep_original=False)
                wrap('exit 1')
                failed=user.upload(fixtures['png']);identifier=failed['id']
                failed=wait_image(user,identifier,'retry_wait')
                assert failed['attempts']==1 and 29000<=failed['next_retry_at']-int(time.time()*1000)<=31000
                original,headers=public.request(failed['url'])
                assert original==fixtures['png'] and headers['Cache-Control']=='no-store'
                assert headers['CDN-Cache-Control']=='public, max-age=3600' and headers['Cloudflare-CDN-Cache-Control']=='public, max-age=3600'
                old_etag=headers['Etag'] if 'Etag' in headers else headers['ETag']
                for attempt in range(2,7):
                    sql('UPDATE images SET next_run=0 WHERE id=?',(identifier,))
                    deadline=time.monotonic()+6
                    while time.monotonic()<deadline:
                        failed=user.request('/api/v1/uploads/'+identifier)[0]
                        if failed.get('attempts')==attempt and failed['state'] in ('retry_wait','failed'):break
                        time.sleep(.05)
                    assert failed['attempts']==attempt
                assert failed['state']=='failed' and failed['retryable'] and failed['temporary']
                token=user.request('/api/v1/upload-token','POST',expected=201)[0]['token']
                user.request('/api/v1/uploads/'+identifier+'/retry','POST',token=token,expected=403)
                admin.request('/api/v1/uploads/'+identifier+'/retry','POST',expected=400)
                wrap('sleep 1\nexec "$(dirname "$0")/worker.real"')
                retried=user.request('/api/v1/uploads/'+identifier+'/retry','POST',expected=202)[0]
                assert retried['temporary'] and retried['url']==failed['url']
                assert public.request(retried['url'])[0]==original
                complete=wait_image(user,identifier);content,headers,_=result(complete)
                assert complete['url']==failed['url'] and not complete.get('temporary') and content!=original
                assert headers.get('ETag',headers.get('Etag'))!=old_etag
                assert sql("SELECT role FROM image_files WHERE image_id=?",(identifier,))==[('result',)]
                restore()
                configure(automatic_retry=False,original_fallback=False)
                wrap('exit 1')
                unique=rgba_png(13,9);digest=hashlib.sha256(unique).hexdigest()
                failed=wait_image(user,user.upload(unique)['id'],'failed')
                assert not failed.get('url') and not failed.get('retryable') and failed['storage_bytes']==0
                assert not (data/'image/public/.objects'/digest).exists()
                user.request('/api/v1/uploads/'+failed['id']+'/retry','POST',expected=400)
                restore()
                configure(automatic_retry=True,original_fallback=True,conversion_concurrency=3,static_format='webp')
                wrap('sleep 3\nexec "$(dirname "$0")/worker.real"')
                queued=[user.upload(fixtures['png'])['id'] for _ in range(3)]
                deadline=time.monotonic()+5
                while time.monotonic()<deadline:
                    if sql("SELECT count(*) FROM images WHERE id IN (?,?,?) AND state='processing'",queued)[0][0]==3:break
                    time.sleep(.05)
                assert sql("SELECT count(*) FROM images WHERE id IN (?,?,?) AND state='processing'",queued)[0][0]==3
                configure(conversion_concurrency=1,static_format='jpeg')
                deadline=time.monotonic()+2
                while time.monotonic()<deadline:
                    if sql("SELECT count(*) FROM images WHERE state='processing'")[0][0]<=1:break
                    time.sleep(.05)
                assert sql("SELECT count(*) FROM images WHERE state='processing'")[0][0]<=1
                for identifier in queued:
                    image=wait_image(user,identifier,timeout=20)
                    assert image['format']=='webp' and image.get('attempts',0)==0,'Policy changed or cancellation consumed retries'
                restore()
                configure(conversion_concurrency=1,static_format='webp')
                wrap('sleep 10\nexec "$(dirname "$0")/worker.real"')
                interrupted=user.upload(fixtures['png'])['id'];wait_image(user,interrupted,'processing')
                stop();restore();start(log)
                recovered=wait_image(user,interrupted)
                assert recovered['format']=='webp' and recovered.get('attempts',0)==0
                # The complete aggregate charge remains consistent after recovery and failures.
                used=sql('SELECT used FROM users WHERE id=?',(user_id,))[0][0]
                logical=sql('SELECT sum(bytes) FROM (SELECT image_id,image_files.hash,max(image_files.bytes) AS bytes FROM image_files JOIN images ON images.id=image_id WHERE user_id=? GROUP BY image_id,image_files.hash)',(user_id,))[0][0]
                assert used==logical,(used,logical)
                assert not list((data/'.incoming').iterdir()) and not list((data/'.converting').iterdir())
                configure(upload_formats=dict(policy['upload_formats'],avif=False))
                stop()
                (root/'.env').write_text((root/'.env').read_text().replace('PEBRIL_AVIF=true\n',''))
                start(log)
                assert not admin.request('/api/v1/status')[0]['native']['avif_enabled']
                user.upload(fixtures['avif'],expected=422)
                assert public.request(existing_avif['url'])[1]['Content-Type']=='image/avif'
                stop()
        finally:
            stop();restore()
            if args.data_dir:clean()
    print('Conversion integration passed: formats, animation and covers, transparency, white JPEG, fit sizing, original retention, quota, retry and fallback continuity, cancellation, captured policy and restart recovery.')

if __name__=='__main__':main()
