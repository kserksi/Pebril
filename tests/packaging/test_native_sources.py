"""Source delivery checks: reject corruption and paths escaping the destination."""
import importlib.util
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('prepare_native', ROOT / 'scripts/build/prepare_libvips_sources.py')
prepare = importlib.util.module_from_spec(spec)
spec.loader.exec_module(prepare)


class SourcePreparationTests(unittest.TestCase):
    def setUp(self):
        (ROOT / '.build').mkdir(exist_ok=True)
        self.temporary = tempfile.TemporaryDirectory(prefix='source-package-test-', dir=ROOT / '.build')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)

    def test_manifest_paths_cannot_escape(self):
        for name in ['../outside', '/outside', 'nested/../../outside']:
            with self.assertRaises(ValueError):
                prepare.relative_path(self.root, name)
        outside = self.root / 'outside'
        outside.mkdir()
        bundle = self.root / 'bundle'
        bundle.mkdir()
        (bundle / 'linked').symlink_to(outside, target_is_directory=True)
        with self.assertRaises(ValueError):
            prepare.relative_path(bundle, 'linked/payload')

    def test_tar_cannot_write_outside_destination(self):
        archive = self.root / 'bad.tar'
        with tarfile.open(archive, 'w') as output:
            member = tarfile.TarInfo('root/../../escaped')
            member.size = 4
            output.addfile(member, io.BytesIO(b'bad!'))
        with self.assertRaises((ValueError, tarfile.TarError)):
            prepare.unpack(archive, self.root / 'prepared')
        self.assertFalse((self.root / 'escaped').exists())

    def test_checksum_failure_leaves_no_output(self):
        (self.root / 'payload').write_bytes(b'corrupt')
        (self.root / 'source-manifest.json').write_text(json.dumps(
            {'payloads': [{'path': 'payload', 'sha256': '0' * 64}]}))
        output = self.root / 'prepared'
        with self.assertRaisesRegex(ValueError, 'checksum mismatch'):
            prepare.prepare(self.root, output)
        self.assertFalse(output.exists())
        self.assertFalse(list(self.root.glob('.libvips-source-*')))

    def test_existing_destination_is_preserved(self):
        output = self.root / 'prepared'
        output.mkdir()
        marker = output / 'keep'
        marker.write_text('existing data')
        with self.assertRaisesRegex(ValueError, 'must not exist'):
            prepare.prepare(self.root, output)
        self.assertEqual(marker.read_text(), 'existing data')


if __name__ == '__main__':
    unittest.main()
