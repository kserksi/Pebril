#!/usr/bin/env bash
# Independent deployment entry: only constrained package-manager steps may elevate.
set -euo pipefail
export PATH=/usr/bin:/bin LC_ALL=C
unset LD_PRELOAD LD_LIBRARY_PATH LD_AUDIT LD_DEBUG LD_PROFILE BASH_ENV ENV CDPATH
umask 077

fail() { printf 'Pebril bootstrap: %s\n' "$*" >&2; exit 1; }
note() { printf 'Pebril bootstrap: %s\n' "$*" >&2; }
usage() {
    cat <<'HELP'
Usage: pebril.sh [--check] [--allow-system-install] [--allow-downloads] [--allow-low-memory] [serve|check|version]
       pebril.sh reset-admin
       pebril.sh recover-database
       pebril.sh migrate --source-root PATH --target-root PATH

Run as the ordinary service user. Deployment checks and dependency repair run
before the service. --check (or check) validates without starting the service.
--allow-system-install authorizes this invocation's eligible system transaction.
--allow-downloads authorizes this invocation's pinned upstream -> Pebril fallback.
--allow-low-memory authorizes restricted serving when the memory allowance is below
40% of system RAM: concurrency 1, AVIF conversion disabled. Check never serves.
Without these options, interactive use asks before each kind of installation;
noninteractive use stops when the next required action is not authorized.
reset-admin resets the existing administrator offline with hidden terminal input;
stop the service first. It accepts no other options and needs no native libraries.
No authorization is saved. Existing valid libraries can be reused offline.
HELP
}
command=serve
command_set=false
allow_system=false
allow_downloads=false
allow_low_memory=false
argument_count=$#
offline_arguments=()
while (($#)); do
    case "$1" in
        --help|-h) usage; exit 0 ;;
        migrate) [[ "$argument_count" == "$#" ]] || fail 'migrate must be the first argument.'; command=migrate; offline_arguments=("$@"); break ;;
        --check) command=check; command_set=true ;;
        --allow-system-install) allow_system=true ;;
        --allow-downloads) allow_downloads=true ;;
        --allow-low-memory) allow_low_memory=true ;;
        serve|check|version|reset-admin|recover-database)
            if $command_set && [[ "$command" != "$1" ]]; then fail 'Choose one command.'; fi
            command=$1; command_set=true ;;
        --) shift; (($# == 0)) || fail 'Extra arguments are not supported.'; break ;;
        *) fail 'Unknown argument; use --help.' ;;
    esac
    shift
done
if [[ ( "$command" == reset-admin || "$command" == recover-database ) && "$argument_count" != 1 ]]; then fail 'Offline commands accept no additional arguments.'; fi
[[ "$EUID" != 0 ]] || fail 'Run as an unprivileged service user; only package installation may use sudo.'
[[ $(uname -s) == Linux ]] || fail 'Linux is required.'
script_path=$(readlink -f -- "${BASH_SOURCE[0]}") || fail 'Cannot resolve the launcher path.'
root=$(dirname -- "$script_path")
service="$root/pebril"
helper="$root/bin/pebril-runtime"
[[ -x "$service" ]] || fail 'The sibling pebril executable is missing; use the extracted release directory.'
if [[ "$command" == migrate ]]; then exec "$service" "${offline_arguments[@]}"; fi
if [[ "$command" == version || "$command" == reset-admin || "$command" == recover-database ]]; then exec "$service" "$command"; fi
[[ -x "$helper" ]] || fail 'bin/pebril-runtime is missing; extract the complete release package.'

# Never source deployment configuration: dotenv values are data, not shell code.
settings=()
settings_args=(settings "$root")
if [[ "$command" == check ]]; then settings_args+=(--check); fi
if $allow_low_memory; then settings_args+=(--allow-low-memory); fi
settings_output=$(mktemp /tmp/pebril-settings.XXXXXXXX) || fail 'Cannot create a private settings-output file.'
helper_pid=
output=
cleanup() {
    rm -f -- "$settings_output"
    if [[ -n "$output" ]]; then rm -f -- "$output"; fi
}
on_signal() {
    local status=$1
    trap '' INT TERM
    if [[ -n "$helper_pid" ]]; then
        kill -TERM "$helper_pid" 2>/dev/null || true
        wait "$helper_pid" 2>/dev/null || true
    fi
    exit "$status"
}
trap cleanup EXIT
trap 'on_signal 130' INT
trap 'on_signal 143' TERM
# Keep terminal input for the prompt while wait allows signal forwarding/reaping.
"$helper" "${settings_args[@]}" <&0 > "$settings_output" &
helper_pid=$!
settings_status=0
wait "$helper_pid" || settings_status=$?
helper_pid=
((settings_status == 0)) || fail 'Deployment preflight failed.'
while IFS= read -r -d '' value; do settings+=("$value"); done < "$settings_output"
rm -f -- "$settings_output"
((${#settings[@]} == 4)) || fail 'Deployment configuration could not be read.'
native=${settings[0]}
avif=${settings[1]}
restricted=${settings[3]}
[[ "${settings[2]}" == "$root" && "$native" == /* && "$avif" =~ ^(true|false)$ && "$restricted" =~ ^(true|false)$ ]] || fail 'Invalid deployment settings protocol.'
[[ "$restricted" != true || "$avif" == false ]] || fail 'Restricted runtime checks require AVIF disabled.'
[[ "$native" != *$'\n'* && "$root" != *$'\n'* ]] || fail 'Newlines in deployment paths are unsupported.'

# One prompt/repair flow per native directory; close the descriptor before exec.
mkdir -p -- "$native" || fail 'The native directory is not writable.'
[[ -O "$native" ]] || fail 'The native directory must belong to the service user.'
native_mode=$(stat -c %a -- "$native")
(( (8#$native_mode & 0022) == 0 )) || fail 'The native directory must not be writable by other users.'
[[ ! -L "$native/.bootstrap.lock" ]] || fail 'The bootstrap lock must not be a symbolic link.'
exec 9>"$native/.bootstrap.lock"
flock -w 60 9 || fail 'Another bootstrap is active; try again after it finishes.'
output=$(mktemp /tmp/pebril-bootstrap.XXXXXXXX) || fail 'Cannot create a private helper-output file.'
run_helper() {
    local status=0
    : > "$output"
    "$helper" "$@" > "$output" &
    helper_pid=$!
    wait "$helper_pid" || status=$?
    helper_pid=
    ((status == 0)) || return "$status"
    selected=$(<"$output")
}

ask() {
    local answer
    [[ -t 0 && -t 2 ]] || return 1
    printf 'Pebril bootstrap: %s [y/N] ' "$1" >&2
    IFS= read -r answer || return 1
    [[ "$answer" == y || "$answer" == Y || "$answer" == yes || "$answer" == YES ]]
}
finish() {
    local selected=$1
    [[ "$selected" == /* && -r "$selected" && "$selected" != *$'\n'* ]] || fail 'Runtime verification returned an invalid path.'
    note "Verified runtime: $selected"
    flock -u 9
    exec 9>&-
    cleanup
    trap - EXIT INT TERM
    if [[ "$command" == check ]]; then
        printf 'Runtime checks passed.\n'
        exit 0
    fi
    if [[ "$restricted" == true ]] || $allow_low_memory; then
        exec "$service" serve --allow-low-memory
    fi
    exec "$service" serve
}

if run_helper find "$native" "$avif"; then finish "$selected"; fi
note 'No existing runtime passed the required capability checks.'

# Parse only the two literal os-release fields needed here, without execution.
distro=
release=
if [[ -r /etc/os-release ]]; then
    while IFS='=' read -r key value; do
        case "$value" in \"*\") value=${value#\"}; value=${value%\"} ;; \'*\') value=${value#\'}; value=${value%\'} ;; esac
        case "$key" in ID) distro=$value ;; VERSION_ID) release=$value ;; esac
    done < /etc/os-release
fi
[[ "$release" =~ ^[0-9]+([.][0-9]+)*$ ]] || fail 'Cannot determine the distribution release.'
case "$distro" in
    debian) [[ ${release%%.*} -ge 12 ]] || fail 'Debian 12 or newer is required.' ;;
    ubuntu) dpkg --compare-versions "$release" ge 22.04 || fail 'Ubuntu 22.04 or newer is required.' ;;
    rocky) [[ $(printf '%s\n%s\n' 9.8 "$release" | sort -V | head -n 1) == 9.8 ]] || fail 'Rocky Linux 9.8 or newer is required.' ;;
    *) fail 'Automatic setup supports Debian, Ubuntu and Rocky Linux only.' ;;
esac
note "Environment: $distro $release, $(uname -m)."

# Automatic selection is fail-closed: official origin and version alone do not
# establish transitive licensing. Add a profile only after its exact review.
reviewed_system_candidate() {
    local kind=$1 package=$2 version=$3 metadata=$4
    # Any future profile must pin package, version, architecture and metadata hash.
    if [[ "$metadata" =~ (libpoppler|poppler-glib|libfftw|fftw-libs|libimagequant) ]]; then
        note "Rejected $package $version: unapproved GPL runtime dependencies."
        return 1
    fi
    # No currently reviewed official package profile satisfies the runtime policy.
    # Keep this gate separate from candidate discovery and installation consent.
    case "$kind:$package:$version" in
        *) note "Rejected $package $version: no reviewed dependency/license profile."; return 1 ;;
    esac
}
package=
package_version=
package_kind=
select_system_package() {
    local candidate policy metadata name architecture origin indexes candidate_policy
    case "$distro" in
        debian|ubuntu)
            command -v apt-cache >/dev/null && command -v apt-get >/dev/null && command -v dpkg >/dev/null || { note 'APT tools are unavailable.'; return 1; }
            architecture=$(dpkg --print-architecture)
            [[ "$architecture" == amd64 ]] || { note 'Only amd64 system packages are supported.'; return 1; }
            # APT policy is read-only; use the existing signed metadata, no refresh.
            policy=$(timeout 30 apt-cache policy) || { note 'Could not inspect APT repository metadata.'; return 1; }
            if [[ "$distro" == debian ]]; then origin=Debian; else origin=Ubuntu; fi
            indexes=$(printf '%s\n' "$policy" | awk -v origin="$origin" '
                /^[[:space:]]*[0-9]+ https?:/ {entry=$0; sub(/^[[:space:]]*[0-9]+[[:space:]]+/, "", entry)}
                /^[[:space:]]*release / && ($0 ~ "o=" origin "(,| Backports,)") {print entry}')
            [[ -n "$indexes" ]] || { note 'No official repository metadata is available.'; return 1; }
            for name in libvips42t64 libvips42; do
                candidate_policy=$(timeout 30 apt-cache policy "$name") || continue
                candidate=$(printf '%s\n' "$candidate_policy" | awk '/Candidate:/ {print $2; exit}')
                [[ -n "$candidate" && "$candidate" != '(none)' ]] || continue
                if ! printf '%s\n' "$candidate_policy" | awk -v wanted="$candidate" -v official="$indexes" '
                    $1 == "***" {version=$2; next}
                    NF == 2 && $2 ~ /^[0-9]+$/ {version=$1; next}
                    version == wanted && $2 ~ /^https?:/ {
                        entry=$0; sub(/^[[:space:]]*[0-9]+[[:space:]]+/, "", entry)
                        count=split(official, indexes, "\n")
                        for (i=1; i<=count; i++) if (entry == indexes[i]) found=1
                    }
                    END {exit !found}'; then
                    note "Rejected $name $candidate: candidate origin is not an official repository."
                    continue
                fi
                dpkg --compare-versions "$candidate" ge 8.16.1 || { note "Rejected $name $candidate: libvips >=8.16.1 is required."; continue; }
                metadata=$(timeout 30 apt-cache show "$name=$candidate") || continue
                [[ "$metadata" == *$'\nArchitecture: amd64'* ]] || { note "Rejected $name: wrong architecture."; continue; }
                note "System candidate: $name $candidate."
                reviewed_system_candidate apt "$name" "$candidate" "$metadata" || continue
                package=$name; package_version=$candidate; package_kind=apt
                return 0
            done ;;
        rocky)
            command -v dnf >/dev/null || { note 'DNF is unavailable.'; return 1; }
            # Existing official Rocky repositories only, never EPEL or added repos.
            metadata=$(timeout 60 dnf --cacheonly --disablerepo='*' --enablerepo=baseos --enablerepo=appstream info --available vips.x86_64 2>/dev/null) || { note 'No cached official Rocky runtime candidate is available.'; return 1; }
            candidate=$(printf '%s\n' "$metadata" | awk '/^Version[[:space:]]*:/ {print $3; exit}')
            [[ "$candidate" =~ ^[0-9]+[.][0-9]+[.][0-9]+$ ]] || return 1
            [[ $(printf '%s\n%s\n' 8.16.1 "$candidate" | sort -V | head -n 1) == 8.16.1 ]] || { note "Rejected vips $candidate: libvips >=8.16.1 is required."; return 1; }
            note "System candidate: vips $candidate."
            reviewed_system_candidate dnf vips "$candidate" "$metadata" || return 1
            package=vips; package_version=$candidate; package_kind=dnf
            return 0 ;;
    esac
    return 1
}
install_system_package() {
    command -v sudo >/dev/null || { note 'sudo is unavailable; the service must remain unprivileged.'; return 1; }
    note 'System transactions run once, wait up to 60 seconds for the package lock, and are not force-killed or retried.'
    note 'If interrupted, let the package manager finish and repair its transaction before retrying bootstrap.'
    if [[ "$package_kind" == apt ]]; then
        if [[ -t 0 && -t 2 ]]; then
            sudo -- /usr/bin/apt-get -o DPkg::Lock::Timeout=60 --no-install-recommends --no-remove -y install "$package=$package_version"
        else
            sudo -n -- /usr/bin/apt-get -o DPkg::Lock::Timeout=60 --no-install-recommends --no-remove -y install "$package=$package_version"
        fi
    else
        if [[ -t 0 && -t 2 ]]; then
            sudo -- /usr/bin/dnf --disablerepo='*' --enablerepo=baseos --enablerepo=appstream --setopt=exit_on_lock=True --setopt=install_weak_deps=False -y install "$package-$package_version.x86_64"
        else
            sudo -n -- /usr/bin/dnf --disablerepo='*' --enablerepo=baseos --enablerepo=appstream --setopt=exit_on_lock=True --setopt=install_weak_deps=False -y install "$package-$package_version.x86_64"
        fi
    fi
}

if select_system_package; then
    if $allow_system || ask "Install the reviewed system package $package $package_version using sudo?"; then
        if install_system_package; then
            if run_helper find "$native" "$avif"; then finish "$selected"; fi
            note 'The installed system runtime failed capability verification.'
        else
            note 'System installation failed or sudo authorization was unavailable.'
        fi
    else
        note 'System installation was not authorized.'
    fi
else
    note 'No eligible reviewed system package is available from the current repository metadata.'
fi

if ! $allow_downloads && ! ask 'Download and privately install a pinned runtime (upstream, then Pebril if needed)?'; then
    fail 'Private downloads were not authorized. Re-run interactively or use --allow-downloads for this invocation.'
fi
run_helper host || fail 'This host does not satisfy the pinned runtime prerequisites.'
for source in upstream pebril; do
    note "Trying pinned $source runtime."
    if run_helper install "$source" "$native" "$avif"; then finish "$selected"; fi
    note "The $source runtime could not be installed and verified."
done
fail 'All authorized runtime sources failed; the service was not started.'
