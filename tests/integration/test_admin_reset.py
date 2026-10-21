#!/usr/bin/env python3
"""Offline reset boundaries use isolated SQLite fixtures and a real terminal."""
import fcntl
import os
from pathlib import Path
import pty
import select
import shutil
import signal
import sqlite3
import subprocess
import tempfile
import termios
import time

ROOT = Path(__file__).resolve().parents[2]
OUTPUT = Path(os.environ.get('PEBRIL_TEST_OUTPUT', str(ROOT / 'dist')))


def fixture(root):
    database = root / 'records'
    database.mkdir(mode=0o700)
    with sqlite3.connect(database / 'pebril.db') as db:
        db.executescript('''
            CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL);
            INSERT INTO schema_migrations VALUES(3,0);
            CREATE TABLE users(id INTEGER PRIMARY KEY,username TEXT,password BLOB,admin INTEGER,status TEXT,quota INTEGER,used INTEGER);
            INSERT INTO users VALUES(1,'owner',X'6f6c64',1,'active',1024,0);
            INSERT INTO users VALUES(2,'member',X'6d656d626572',0,'active',2048,0);
            CREATE TABLE sessions(hash TEXT PRIMARY KEY,user_id INTEGER,csrf TEXT,expires INTEGER);
            INSERT INTO sessions VALUES('owner-session',1,'csrf',9999999999999);
            INSERT INTO sessions VALUES('member-session',2,'csrf',9999999999999);
            CREATE TABLE upload_tokens(user_id INTEGER PRIMARY KEY,hash TEXT);
            INSERT INTO upload_tokens VALUES(1,'owner-token');
            INSERT INTO upload_tokens VALUES(2,'member-token');
            CREATE TABLE images(id TEXT PRIMARY KEY,user_id INTEGER NOT NULL REFERENCES users(id),state TEXT NOT NULL,policy BLOB NOT NULL,created_at INTEGER NOT NULL,format TEXT NOT NULL DEFAULT '',bytes INTEGER NOT NULL DEFAULT 0,width INTEGER NOT NULL DEFAULT 0,height INTEGER NOT NULL DEFAULT 0,frames INTEGER NOT NULL DEFAULT 0,error TEXT NOT NULL DEFAULT '',hash TEXT NOT NULL DEFAULT '',generation INTEGER NOT NULL DEFAULT 0,attempts INTEGER NOT NULL DEFAULT 0,next_run INTEGER NOT NULL DEFAULT 0,fallback INTEGER NOT NULL DEFAULT 0);
            CREATE TABLE image_files(image_id TEXT NOT NULL REFERENCES images(id) ON DELETE CASCADE,role TEXT NOT NULL,hash TEXT NOT NULL,bytes INTEGER NOT NULL,format TEXT NOT NULL,width INTEGER NOT NULL,height INTEGER NOT NULL,frames INTEGER NOT NULL,PRIMARY KEY(image_id,role));

        ''')
    (database / 'pebril.db').chmod(0o600)
    (root / '.env').write_text('PEBRIL_DB_DIR=records\n')
    (root / 'config.json').write_text('invalid JSON is irrelevant to offline reset')
    return database


def state(database):
    with sqlite3.connect(database / 'pebril.db') as db:
        return tuple(db.execute(query).fetchall() for query in (
            'SELECT * FROM users ORDER BY id',
            'SELECT * FROM sessions ORDER BY user_id',
            'SELECT * FROM upload_tokens ORDER BY user_id'))


def terminal_reset(command, env, first=b'new secret password', second=None, interrupt=False):
    master, slave = pty.openpty()
    before = termios.tcgetattr(slave)
    process = subprocess.Popen(command, stdin=slave, stdout=slave, stderr=slave, cwd='/', env=env)
    transcript = bytearray()

    def wait_for(marker):
        deadline = time.monotonic() + 8
        while marker not in transcript:
            if time.monotonic() > deadline:
                raise AssertionError(f'Reset prompt timeout: {transcript!r}')
            ready, _, _ = select.select([master], [], [], .1)
            if ready:
                transcript.extend(os.read(master, 4096))
            elif process.poll() is not None:
                raise AssertionError(f'Reset stopped before prompt: {transcript!r}')

    try:
        wait_for(b'New administrator password: ')
        assert not termios.tcgetattr(slave)[3] & termios.ECHO
        os.write(master, first + b'\n')
        if len(first) <= 72 and first:
            wait_for(b'Confirm new password: ')
            if interrupt:
                process.send_signal(signal.SIGTERM)
            else:
                os.write(master, (first if second is None else second) + b'\n')
        process.wait(timeout=8)
        while select.select([master], [], [], .05)[0]:
            transcript.extend(os.read(master, 4096))
        assert termios.tcgetattr(slave) == before, 'Terminal settings were not restored'
        if first:
            assert first not in transcript, 'Password appeared in terminal output'
        if second:
            assert second not in transcript
        return process.returncode, bytes(transcript)
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()
        os.close(master)
        os.close(slave)


def main():
    assert os.geteuid() != 0, 'Run integration as the ordinary service user'
    (ROOT / '.build').mkdir(exist_ok=True)
    env = {k: v for k, v in os.environ.items() if not k.startswith('PEBRIL_')}
    with tempfile.TemporaryDirectory(prefix='admin-reset-', dir=ROOT / '.build') as temporary:
        root = Path(temporary)
        executable = root / 'pebril'
        launcher = root / 'pebril.sh'
        shutil.copy2(OUTPUT / 'pebril', executable)
        shutil.copy2(ROOT / 'scripts/pebril.sh', launcher)
        database = fixture(root)
        original = state(database)
        command = [str(executable), 'reset-admin']
        for args in ([str(executable), 'reset-admin', 'do-not-log-this-secret'],
                     [str(launcher), 'reset-admin', '--allow-downloads'],
                     [str(launcher), 'reset-admin', '--allow-low-memory'],
                     command):
            result = subprocess.run(args, input=b'never-read-this-secret\n', capture_output=True, cwd='/', env=env, timeout=8)
            assert result.returncode != 0
            assert b'never-read-this-secret' not in result.stdout + result.stderr
            assert b'do-not-log-this-secret' not in result.stdout + result.stderr
            assert state(database) == original
        with (database / 'pebril.lock').open('a') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            result = subprocess.run(command, capture_output=True, env=env, cwd='/', timeout=8)
            assert result.returncode != 0 and b'already used by another instance' in result.stderr
            assert b'password:' not in result.stderr
            assert state(database) == original
        for first, second, interrupt in ((b'mismatch first', b'mismatch second', False),
                                         (b'interrupted password', None, True),
                                         (b'x' * 73, None, False),
                                         (b'', None, False)):
            code, _ = terminal_reset(command, env, first, second, interrupt)
            assert code != 0 and state(database) == original
        code, output = terminal_reset([str(launcher), 'reset-admin'], env, b'x' * 72)
        assert code == 0, output
        changed = state(database)
        assert changed[0][0][2].startswith(b'$2') and changed[0][0][2] != original[0][0][2]
        assert changed[0][1] == original[0][1]
        assert changed[1] == [original[1][1]] and changed[2] == [original[2][1]]
        assert not (root / 'bin').exists(), 'Offline reset prepared native runtimes'
        assert (root / 'config.json').read_text() == 'invalid JSON is irrelevant to offline reset'
        assert not (root / 'data').exists(), 'Offline reset initialized service paths'
        # Root refusal does not need sudo or privileged integration setup.
        if shutil.which('unshare'):
            root_check = subprocess.run(['unshare', '--user', '--map-root-user', str(executable), 'reset-admin'],
                                        capture_output=True, env=env, cwd='/', timeout=8)
            if root_check.returncode and b'Operation not permitted' in root_check.stderr:
                print('Root namespace check unavailable; root refusal was not exercised.')
            else:
                assert root_check.returncode != 0 and b'unprivileged user' in root_check.stderr
    print('Administrator reset passed: offline launcher, hidden PTY input/restoration, mismatch/interruption/length, lock exclusion, no-argument/pipe boundaries and credential revocation.')


if __name__ == '__main__':
    main()
