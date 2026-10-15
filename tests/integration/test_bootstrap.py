#!/usr/bin/env python3
"""Launcher authorization/lifecycle checks with explicit helper doubles; no package installation."""
import os
from pathlib import Path
import pty
import select
import signal
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / 'scripts/pebril.sh'
HELPER = r'''#!/bin/bash
native="$(dirname "$(dirname "$0")")/bin"
case "$1" in
 settings) printf '%s\0false\0%s\0' "$native" "$(dirname "$native")" ;;
 find)
  if [[ ${MOCK_SLOW:-} == yes ]]; then
    printf '%s' "$$" > "$native/helper.pid"
    exec /bin/sleep 30
  fi
  [[ -e "$native/ready" ]] || exit 1
  printf '%s\n' "$native/ready" ;;
 host) exit 0 ;;
 install)
  printf '%s\n' "$2" >> "$native/attempts"
  [[ ${MOCK_FAIL_ALL:-} != yes && "$2" == pebril ]] || exit 1
  /bin/sleep 0.15
  touch "$native/ready"
  printf '%s\n' "$native/ready" ;;
 *) exit 1 ;;
esac
'''


def main():
    (ROOT / '.build').mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='bootstrap-contract-', dir=ROOT / '.build') as temporary:
        app = Path(temporary)
        native = app / 'bin'
        native.mkdir(mode=0o700)
        launcher = app / 'pebril.sh'
        launcher.write_bytes(SCRIPT.read_bytes())
        launcher.chmod(0o700)
        helper = native / 'pebril-runtime'
        helper.write_text(HELPER)
        helper.chmod(0o700)
        service = app / 'pebril'
        service.write_text('#!/bin/bash\nprintf unexpected > "$(dirname "$0")/service-called"\n')
        service.chmod(0o700)
        env = {k: v for k, v in os.environ.items() if not k.startswith(('PEBRIL_', 'MOCK_'))}

        def run(*args, extra=None):
            return subprocess.run([str(launcher), '--check', *args], stdin=subprocess.DEVNULL,
                                  capture_output=True, text=True, env=dict(env, **(extra or {})), timeout=10)

        for args in [(), ('--allow-system-install',)]:
            result = run(*args)
            assert result.returncode != 0 and 'not authorized' in result.stderr, result.stderr
            assert not (native / 'attempts').exists(), 'No download without explicit consent'
        result = run('--allow-downloads')
        assert result.returncode == 0, result.stderr
        assert (native / 'attempts').read_text().splitlines() == ['upstream', 'pebril']
        assert not (app / 'service-called').exists(), 'Check must be independent of service execution'
        (native / 'attempts').unlink()
        assert run().returncode == 0
        assert not (native / 'attempts').exists(), 'Offline reuse must not install'
        (native / 'ready').unlink()
        result = run('--allow-downloads', extra={'MOCK_FAIL_ALL': 'yes'})
        assert result.returncode != 0 and 'All authorized runtime sources failed' in result.stderr
        (native / 'attempts').unlink()

        # Interactive rejection/approval use a real terminal; EOF remains denial above.
        for answer, success in [(b'n\n', False), (b'y\n', True)]:
            master, slave = pty.openpty()
            process = subprocess.Popen([str(launcher), '--check'], stdin=slave, stdout=slave, stderr=slave, env=env)
            os.close(slave)
            content = b''
            try:
                deadline = time.monotonic() + 10
                sent = False
                while time.monotonic() < deadline:
                    readable, _, _ = select.select([master], [], [], 0.1)
                    if readable:
                        try:
                            chunk = os.read(master, 8192)
                        except OSError:
                            break
                        if not chunk:
                            break
                        content += chunk
                        if b'[y/N]' in content and not sent:
                            os.write(master, answer)
                            sent = True
                    if process.poll() is not None:
                        break
                process.wait(timeout=3)
                assert sent and (process.returncode == 0) == success, content.decode(errors='replace')
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait()
                os.close(master)
            if success:
                (native / 'ready').unlink()
                (native / 'attempts').unlink()
            else:
                assert not (native / 'attempts').exists()

        # Concurrent launchers serialize repairs and reuse the first verified result.
        processes = [subprocess.Popen([str(launcher), '--check', '--allow-downloads'], env=env,
                                      stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE) for _ in range(2)]
        for process in processes:
            stdout, stderr = process.communicate(timeout=10)
            assert process.returncode == 0, stderr.decode()
        assert (native / 'attempts').read_text().splitlines() == ['upstream', 'pebril']
        (native / 'attempts').unlink()
        (native / 'ready').unlink()

        # Signal cancellation reaches an active helper and releases the bootstrap lock.
        process = subprocess.Popen([str(launcher), '--check'], env=dict(env, MOCK_SLOW='yes'),
                                   stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        try:
            deadline = time.monotonic() + 5
            while not (native / 'helper.pid').exists() and time.monotonic() < deadline:
                time.sleep(0.02)
            assert (native / 'helper.pid').exists()
            pid = int((native / 'helper.pid').read_text())
            process.send_signal(signal.SIGTERM)
            process.communicate(timeout=3)
            assert process.returncode != 0
            assert not Path(f'/proc/{pid}').exists(), 'Cancelled helper still running'
        finally:
            if process.poll() is None:
                process.kill()
                process.wait()
        assert run('--allow-downloads').returncode == 0, 'Cancellation left bootstrap locked'
        (native / '.bootstrap.lock').unlink()
        (native / '.bootstrap.lock').symlink_to(app / 'unrelated')
        result = run()
        assert result.returncode != 0 and 'symbolic link' in result.stderr
        assert not (app / 'unrelated').exists()
    print('Bootstrap contract passed: no implicit consent, terminal yes/no, ordered fallback, independent check, concurrent repair, cancellation and lock safety (helper doubles).')


if __name__ == '__main__':
    main()
