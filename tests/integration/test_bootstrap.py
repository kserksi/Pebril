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


def system_candidate_rejections(app, env):
    """Isolate OS/package metadata and sudo doubles; never install host packages."""
    native = app / 'bin'
    fixtures = app / 'system-fixtures'
    fixtures.mkdir()
    release = fixtures / 'os-release'
    apt = fixtures / 'apt-cache'
    dnf = fixtures / 'dnf'
    sudo = fixtures / 'sudo'
    sudo.write_text('#!/bin/bash\ntouch "' + str(native / 'sudo-called') + '"\nexit 99\n')
    sudo.chmod(0o700)
    real_bin = fixtures / 'real-bin'
    test_bin = fixtures / 'test-bin'
    real_bin.mkdir()
    test_bin.mkdir()
    for program in Path('/usr/bin').iterdir():
        (test_bin / program.name).symlink_to(real_bin / program.name)
    for name, fixture in [('apt-cache', apt), ('dnf', dnf), ('sudo', sudo)]:
        (test_bin / name).unlink(missing_ok=True)
        (test_bin / name).symlink_to(fixture)
    base = ['bwrap', '--unshare-user', '--uid', '1000', '--gid', '1000', '--die-with-parent',
            '--ro-bind', '/', '/', '--bind', str(app), str(app),
            '--ro-bind', str(release), str(Path('/etc/os-release').resolve()),
            '--ro-bind', '/usr/bin', str(real_bin), '--ro-bind', str(test_bin), '/usr/bin',
            '--dev', '/dev', '--proc', '/proc', '--tmpfs', '/tmp']
    cases = [
        ('debian', 'none', 'No eligible reviewed system package'),
        ('debian', 'old', 'libvips >=8.16.1 is required'),
        ('debian', 'foreign', 'candidate origin is not an official repository'),
        ('debian', 'architecture', 'wrong architecture'),
        ('debian', 'gpl', 'unapproved GPL runtime dependencies'),
        ('debian', 'unknown', 'no reviewed dependency/license profile'),
        ('ubuntu', 'gpl', 'unapproved GPL runtime dependencies'),
        ('ubuntu', 'unknown', 'no reviewed dependency/license profile'),
        ('rocky', 'none', 'No cached official Rocky runtime candidate'),
        ('rocky', 'old', 'libvips >=8.16.1 is required'),
        ('rocky', 'gpl', 'unapproved GPL runtime dependencies'),
        ('rocky', 'unknown', 'no reviewed dependency/license profile'),
    ]
    for distro, candidate, diagnostic in cases:
        version = '8.15.0-1' if candidate == 'old' else '8.16.1-1'
        origin = 'Ubuntu' if distro == 'ubuntu' else 'Debian'
        repo = 'https://official.invalid/repository stable/main amd64 Packages'
        source = repo.replace('official.invalid', 'foreign.invalid') if candidate == 'foreign' else repo
        arch = 'arm64' if candidate == 'architecture' else 'amd64'
        dependencies = 'libpoppler-glib8t64, libfftw3-double3' if candidate == 'gpl' else 'libc6'
        os_version = {'debian': '13', 'ubuntu': '26.04', 'rocky': '9.8'}[distro]
        release.write_text(f'ID={distro}\nVERSION_ID="{os_version}"\n')
        apt.write_text(f"""#!/bin/bash
case "$1:${{2-}}" in
 policy:) cat <<'POLICY'
 500 {repo}
     release o={origin},a=stable,n=stable,l={origin},c=main,b=amd64
POLICY
 ;;
 policy:libvips42t64) cat <<'CANDIDATE'
libvips42t64:
  Candidate: {'(none)' if candidate == 'none' else version}
  Version table:
     {version} 500
        500 {source}
CANDIDATE
 ;;
 policy:libvips42) printf '  Candidate: (none)\\n' ;;
 show:*) cat <<'METADATA'
Package: libvips42t64
Version: {version}
Architecture: {arch}
Depends: {dependencies}
METADATA
 ;;
 *) exit 1 ;;
esac
""")
        dnf.write_text('#!/bin/bash\nexit 1\n' if candidate == 'none' else
                       f'#!/bin/bash\nprintf "%s\\n" "Version : {version.split("-")[0]}" "Requires : {dependencies}"\n')
        apt.chmod(0o700)
        dnf.chmod(0o700)
        for grants in [[], ['--allow-system-install'], ['--allow-system-install', '--allow-downloads']]:
            result = subprocess.run(base + [str(app / 'pebril.sh'), '--check', *grants], env=env,
                                    stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=10)
            assert diagnostic in result.stderr, (distro, candidate, result.stderr)
            assert not (native / 'sudo-called').exists(), 'Rejected package reached sudo'
            if '--allow-downloads' in grants:
                assert result.returncode == 0, result.stderr
                assert (native / 'attempts').read_text().splitlines() == ['upstream', 'pebril']
                (native / 'attempts').unlink()
                (native / 'ready').unlink()
            else:
                assert result.returncode != 0 and 'not authorized' in result.stderr, result.stderr
                assert not (native / 'attempts').exists(), 'System grant authorized a private download'
            assert not (app / 'service-called').exists(), 'Rejected candidate started the service'
    print('System-candidate rejection passed: 12 OS/metadata cases, sudo never reached, fallback needs a separate grant (metadata/helper doubles).')


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

        system_candidate_rejections(app, env)

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
