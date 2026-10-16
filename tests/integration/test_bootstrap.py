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
 settings) printf '%s\0false\0%s\0false\0' "$native" "$(dirname "$native")" ;;
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



MEMORY_HELPER = r'''#!/bin/bash
# Explicit low-memory preflight double: no host memory overrides or real installation.
native="$(dirname "$(dirname "$0")")/bin"
case "$1" in
 settings)
  printf '%s' "$$" > "$native/preflight.pid"
  readlink "/proc/$$/fd/1" > "$native/preflight-output"
  case ${MOCK_SETTINGS:-valid} in
   failure) exit 1 ;;
   empty) exit 0 ;;
   partial-failure) printf '%s\0false\0%s\0true\0' "$native" "$(dirname "$native")"; exit 1 ;;
  esac
  allow=false
  check=false
  for arg in "${@:3}"; do
   case "$arg" in --allow-low-memory) allow=true ;; --check) check=true ;; *) exit 1 ;; esac
  done
  printf 'Memory preflight double: 39 bytes of 100 system bytes (39%%), concurrency 1, AVIF conversion disabled.\n' >&2
  if ! $allow && ! $check; then
   [[ -t 0 && -t 2 ]] || { printf 'Restricted startup requires --allow-low-memory.\n' >&2; exit 1; }
   printf 'Start in restricted mode? [y/N] ' >&2
   IFS= read -r answer || exit 1
   [[ "$answer" == y || "$answer" == yes ]] || exit 1
  fi
  printf '%s\0false\0%s\0true\0' "$native" "$(dirname "$native")" ;;
 find)
  printf '%s\n' "$3" >> "$native/probe-avif"
  [[ -e "$native/ready" ]] || exit 1
  printf '%s\n' "$native/ready" ;;
 host) exit 0 ;;
 install) printf '%s\n' "$2" >> "$native/attempts"; exit 1 ;;
 *) exit 1 ;;
esac
'''


def restricted_preflight_contract(env):
    """Real Bash/PTY lifecycle with an explicit memory/helper double, no changed host limits."""
    with tempfile.TemporaryDirectory(prefix='bootstrap-memory-', dir=ROOT / '.build') as temporary:
        app = Path(temporary)
        native = app / 'bin'
        native.mkdir(mode=0o700)
        launcher = app / 'pebril.sh'
        launcher.write_bytes(SCRIPT.read_bytes())
        launcher.chmod(0o700)
        helper = native / 'pebril-runtime'
        helper.write_text(MEMORY_HELPER)
        helper.chmod(0o700)
        service = app / 'pebril'
        service.write_text('#!/bin/bash\nprintf "%s\\n" "$@" > "$(dirname "$0")/service-called"\n')
        service.chmod(0o700)
        ready = native / 'ready'
        ready.touch()
        called = app / 'service-called'

        def run(*args, extra=None):
            return subprocess.run([str(launcher), *args], env=dict(env, **(extra or {})),
                                  stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=10)

        # Deployment-install grants and ambient values must never authorize low-memory serving.
        for args in [(), ('--allow-system-install',), ('--allow-downloads',),
                     ('--allow-system-install', '--allow-downloads')]:
            result = run(*args, extra={'PEBRIL_ALLOW_LOW_MEMORY': 'true'})
            assert result.returncode != 0 and '--allow-low-memory' in result.stderr, result.stderr
            assert not called.exists() and not (native / 'probe-avif').exists(), 'Unconsented preflight reached probes/service'

        result = run('--allow-low-memory')
        assert result.returncode == 0, result.stderr
        assert called.read_text().splitlines() == ['serve', '--allow-low-memory'], 'Restricted grant was not forwarded'
        assert (native / 'probe-avif').read_text().splitlines() == ['false'], 'Restricted native probe required AVIF'
        called.unlink()
        result = run()
        assert result.returncode != 0 and not called.exists(), 'Invocation consent persisted'

        result = run('--check')
        assert result.returncode == 0 and not called.exists(), 'Check started serving or required a serve grant'
        assert set((native / 'probe-avif').read_text().splitlines()) == {'false'}

        # Even complete protocol bytes from a failed helper cannot start serving.
        for mode in ['failure', 'empty', 'partial-failure']:
            result = run('--allow-low-memory', extra={'MOCK_SETTINGS': mode})
            assert result.returncode != 0 and not called.exists(), (mode, result.stderr)
            assert not Path((native / 'preflight-output').read_text().strip()).exists(), 'Failed settings leaked a temp file'

        # A low-memory grant cannot silently grant runtime downloads.
        ready.unlink()
        result = run('--allow-low-memory')
        assert result.returncode != 0 and 'not authorized' in result.stderr, result.stderr
        assert not (native / 'attempts').exists() and not called.exists(), 'Low-memory grant authorized a dependency download'
        ready.touch()

        def terminal(answer=None, terminate=False):
            master, slave = pty.openpty()
            process = subprocess.Popen([str(launcher)], stdin=slave, stdout=slave, stderr=slave,
                                       env=env, start_new_session=True)
            os.close(slave)
            content = b''
            sent = False
            helper_pid = None
            settings_output = None
            terminated_at = None
            try:
                deadline = time.monotonic() + 5
                while time.monotonic() < deadline:
                    readable, _, _ = select.select([master], [], [], 0.05)
                    if readable:
                        try:
                            chunk = os.read(master, 8192)
                        except OSError:
                            break
                        if not chunk:
                            break
                        content += chunk
                        if b'[y/N]' in content and not sent:
                            helper_pid = int((native / 'preflight.pid').read_text())
                            settings_output = Path((native / 'preflight-output').read_text().strip())
                            if terminate:
                                assert process.poll() is None and Path(f'/proc/{helper_pid}').exists(), 'Preflight was not active at the prompt'
                                assert settings_output.is_file(), 'Settings output was not active at the prompt'
                                terminated_at = time.monotonic()
                                process.send_signal(signal.SIGTERM)
                            else:
                                os.write(master, answer)
                            sent = True
                    if process.poll() is not None:
                        break
                process.wait(timeout=2)
                assert sent, content.decode(errors='replace')
                if terminate:
                    assert process.returncode == 143, 'Launcher did not handle SIGTERM and reap its preflight helper'
                    assert not Path(f'/proc/{helper_pid}').exists(), 'Cancelled preflight helper still running'
                    assert not settings_output.exists(), 'Cancelled preflight leaked settings output'
                    print(f'Preflight SIGTERM verified: launcher status {process.returncode}, helper gone, temp removed in {time.monotonic() - terminated_at:.3f}s.')
                return process.returncode, content.decode(errors='replace')
            finally:
                # Reap only this isolated test process group if the contract fails.
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                if process.poll() is None:
                    process.wait(timeout=2)
                os.close(master)
                if settings_output is not None:
                    settings_output.unlink(missing_ok=True)

        for answer, success in [(b'n\n', False), (b'\x04', False), (b'y\n', True)]:
            status, content = terminal(answer=answer)
            assert (status == 0) == success, content
            if success:
                assert called.read_text().splitlines() == ['serve', '--allow-low-memory']
                called.unlink()
            else:
                assert not called.exists(), 'Denied preflight reached the service'
        terminal(terminate=True)
        assert not called.exists(), 'Cancelled preflight reached the service'
    print('Restricted-start launcher passed: independent invocation consent, terminal yes/no/EOF, effective AVIF, service handoff, check isolation, failed protocol and cancellation (memory/helper doubles).')

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
    restricted_preflight_contract(env)
    print('Bootstrap contract passed: no implicit consent, terminal yes/no, ordered fallback, independent check, concurrent repair, cancellation and lock safety (helper doubles).')


if __name__ == '__main__':
    main()
