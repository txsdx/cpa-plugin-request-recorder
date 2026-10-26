import hashlib
from pathlib import Path
import tempfile
import unittest
import zipfile

from package import EXTENSIONS, package, validate_version
from generate_registry import generate_registry, validate_registry
import json
import copy
from release_metadata import TARGETS, verify_assets, version_from_ref


class ReleaseTests(unittest.TestCase):
    def test_tag_validation(self):
        self.assertEqual(version_from_ref('tag', 'v1.2.3'), '1.2.3')
        self.assertEqual(version_from_ref('tag', 'v1.2.3-rc.1'), '1.2.3-rc.1')
        self.assertEqual(version_from_ref('branch', 'main'), '0.1.0')
        self.assertEqual(version_from_ref('branch', 'main', tags=['v0.1.0', 'v0.1.2', 'v0.2.0-rc.1']), '0.1.3')
        self.assertEqual(version_from_ref('branch', 'main', tags=['v0.9.9', 'v0.10.0']), '0.10.1')
        self.assertEqual(version_from_ref('branch', 'main', requested='v1.2.3'), '1.2.3')
        for bad in ['v1.2.3', '../1.2.3', '1.2', '1.2.3;echo bad', '1.2.3/asset']:
            with self.assertRaises(ValueError):
                validate_version(bad)

    def test_single_library_and_verified_checksums(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for os_, arch in TARGETS:
                ext = EXTENSIONS[os_]
                (root / f'request-recorder.{ext}').write_bytes(b'native-library-fixture')
                archive = package('1.2.3', os_, arch, ext, root)
                with zipfile.ZipFile(archive) as z:
                    self.assertEqual(z.namelist(), [f'request-recorder.{ext}'])
            verify_assets(root, '1.2.3')
            lines = (root / 'checksums.txt').read_text().splitlines()
            self.assertEqual(len(lines), 5)
            for line in lines:
                digest, name = line.split('  ')
                self.assertEqual(hashlib.sha256((root / name).read_bytes()).hexdigest(), digest)
            (root / 'request-recorder_1.2.3_linux_arm64.zip').unlink()
            with self.assertRaises(ValueError):
                verify_assets(root, '1.2.3')

    def test_rejects_extra_zip_entries(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for os_, arch in TARGETS:
                (root / f'request-recorder.{EXTENSIONS[os_]}').write_bytes(b'library')
                package('1.2.3', os_, arch, EXTENSIONS[os_], root)
            with zipfile.ZipFile(root / 'request-recorder_1.2.3_linux_amd64.zip', 'a') as z:
                z.writestr('unwanted-header.h', 'bad')
            with self.assertRaises(ValueError):
                verify_assets(root, '1.2.3')

    def test_generated_registry_matches_native_package(self):
        source = Path(__file__).resolve().parents[1] / 'registry.json'
        original = source.read_bytes()
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / 'registry.json'
            generated = generate_registry(source, output, '1.2.3')
            self.assertEqual(generated['plugins'][0]['version'], '1.2.3')
            self.assertEqual(generated['plugins'][0]['id'], 'request-recorder')
            self.assertEqual(json.loads(output.read_text(encoding='utf-8')), generated)
        self.assertEqual(source.read_bytes(), original)
        self.assertNotIn('version', json.loads(original)['plugins'][0])
        bad = copy.deepcopy(generated)
        bad['plugins'][0]['id'] = 'wrong-id'
        with self.assertRaises(ValueError):
            validate_registry(bad)


if __name__ == '__main__':
    unittest.main()
