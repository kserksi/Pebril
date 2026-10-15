#!/usr/bin/env python3
"""Verify and unpack a pinned libvips source companion; no download or compilation."""
import argparse
import copy
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import shutil
import subprocess
import tarfile
import tempfile


def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def relative_path(root, name):
    path = PurePosixPath(name)
    if path.is_absolute() or '..' in path.parts or not path.parts:
        raise ValueError('Invalid source manifest path')
    result = root.joinpath(*path.parts)
    if not result.resolve().is_relative_to(root.resolve()):
        raise ValueError('Source path escapes its directory')
    return result


def unpack(archive, destination):
    destination.mkdir(parents=True)
    with tarfile.open(archive) as source:
        members = source.getmembers()
        roots = {PurePosixPath(member.name).parts[0] for member in members}
        if len(roots) != 1:
            raise ValueError('Expected one source archive root')
        stripped = []
        for member in members:
            parts = PurePosixPath(member.name).parts
            if len(parts) == 1:
                continue
            updated = copy.copy(member)
            updated.name = '/'.join(parts[1:])
            if member.islnk():
                link = PurePosixPath(member.linkname).parts
                if not link or link[0] not in roots:
                    raise ValueError('Invalid source hard link')
                updated.linkname = '/'.join(link[1:])
            stripped.append(updated)
        source.extractall(destination, members=stripped, filter='data')


def replace(path, old, new):
    text = path.read_text()
    if old not in text:
        raise ValueError('Pinned source context changed: ' + path.name)
    path.write_text(text.replace(old, new))


def apply_patch(directory, patch):
    subprocess.run(['patch', '--batch', '--forward', '-p1', '-i', str(patch)],
                   cwd=directory, check=True)


def prepare(bundle, output):
    bundle = bundle.resolve()
    output = output.absolute()
    if output.exists() or output.is_symlink():
        raise ValueError('Destination must not exist')
    manifest = json.loads((bundle / 'source-manifest.json').read_text())
    for entry in manifest['payloads']:
        path = relative_path(bundle, entry['path'])
        if not path.is_file() or digest(path) != entry['sha256']:
            raise ValueError('Source checksum mismatch: ' + entry['path'])
    output.parent.mkdir(parents=True, exist_ok=True)
    temporary = Path(tempfile.mkdtemp(prefix='.libvips-source-', dir=output.parent))
    try:
        native = temporary / 'native'
        for entry in manifest['native_sources']:
            unpack(relative_path(bundle, entry['path']), relative_path(native, entry['name']))
        for entry in manifest['rust_crates']:
            directory = relative_path(temporary / 'vendor', entry['name'] + '-' + entry['version'])
            unpack(relative_path(bundle, entry['path']), directory)
            files = {p.relative_to(directory).as_posix(): digest(p)
                     for p in directory.rglob('*') if p.is_file() and p.name != '.cargo-checksum.json'}
            (directory / '.cargo-checksum.json').write_text(json.dumps(
                {'files': files, 'package': entry['sha256']}, sort_keys=True) + '\n')
        unpack(relative_path(bundle, manifest['rust_std_source']), temporary / 'rust-standard-library')
        apply_patch(native / 'glib', bundle / 'patches/glib-without-gregex.patch')
        apply_patch(native / 'libvips', bundle / 'patches/vips-heif-hbr.patch')
        apply_patch(native / 'librsvg', bundle / 'patches/librsvg-recipe.patch')
        replace(native / 'libheif/CMakeLists.txt', 'cmake_minimum_required (VERSION 3.16.3)',
                'cmake_minimum_required (VERSION 3.12)')
        replace(native / 'harfbuzz/meson.build', "  subdir('util')\n", '')
        pango = native / 'pango/meson.build'
        lines = pango.read_text().splitlines(keepends=True)
        indices = [i for i, line in enumerate(lines) if "subdir('utils')" in line]
        if len(indices) != 1:
            raise ValueError('Pinned Pango source context changed')
        del lines[indices[0]:indices[0] + 2]
        pango.write_text(''.join(lines))
        vips = native / 'libvips/meson.build'
        lines = vips.read_text().splitlines(keepends=True)
        indices = [i for i, line in enumerate(lines) if "subdir('man')" in line]
        if len(indices) != 1:
            raise ValueError('Pinned libvips source context changed')
        del lines[indices[0]:indices[0] + 5]
        vips.write_text(''.join(lines))
        (native / 'libvips/vips.map').write_text('{local:g_param_spec_types;};')
        cargo = native / 'librsvg/.cargo'
        cargo.mkdir(exist_ok=True)
        # Cargo resolves this relative directory from the workspace root.
        existing_config = (cargo / 'config.toml').read_text() if (cargo / 'config.toml').exists() else ''
        (cargo / 'config.toml').write_text(existing_config + '\n' +
            '[source.crates-io]\nreplace-with = "vendored-sources"\n'
            '[source.vendored-sources]\ndirectory = "../../vendor"\n'
            '[net]\noffline = true\n')
        os.rename(temporary, output)
    except BaseException:
        shutil.rmtree(temporary, ignore_errors=True)
        raise
    print('Verified source archives, prepared recipe changes and offline Rust vendor directory.')
    print('Sources are prepared; nothing has been compiled.')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bundle', type=Path, default=Path(__file__).resolve().parent)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    prepare(args.bundle, args.output)


if __name__ == '__main__':
    main()
