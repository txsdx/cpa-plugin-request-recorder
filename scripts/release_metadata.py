"""Validate tag-derived versions and the complete set of release archives."""
import hashlib
import os
from pathlib import Path
import sys
import zipfile

from package import EXTENSIONS, validate_version

TARGETS = [('linux', 'amd64'), ('linux', 'arm64'), ('darwin', 'amd64'),
           ('darwin', 'arm64'), ('windows', 'amd64')]


def version_from_ref(ref_type, ref_name):
    if ref_type != 'tag':
        return '0.0.0-dev'
    if not ref_name.startswith('v'):
        raise ValueError('release tags must start with v')
    return validate_version(ref_name[1:])


def verify_assets(root, version):
    validate_version(version)
    expected = {f'request-recorder_{version}_{os_}_{arch}.zip' for os_, arch in TARGETS}
    actual = {path.name for path in root.glob('*.zip')}
    if actual != expected:
        raise ValueError(f'wrong release asset set: missing={expected-actual}, extra={actual-expected}')
    checksums = []
    for os_, arch in sorted(TARGETS):
        name = f'request-recorder_{version}_{os_}_{arch}.zip'
        path = root / name
        with zipfile.ZipFile(path) as z:
            if z.namelist() != [f'request-recorder.{EXTENSIONS[os_]}']:
                raise ValueError(f'{name}: archive must contain exactly one native library at root')
            if z.getinfo(z.namelist()[0]).file_size == 0 or z.testzip() is not None:
                raise ValueError(f'{name}: empty or corrupt library')
        checksums.append(f'{hashlib.sha256(path.read_bytes()).hexdigest()}  {name}\n')
    (root / 'checksums.txt').write_text(''.join(checksums), encoding='utf-8')


if __name__ == '__main__':
    if sys.argv[1:] == ['--verify-assets']:
        verify_assets(Path('dist'), os.environ['VERSION'])
    elif not sys.argv[1:]:
        print('version=' + version_from_ref(os.environ['GITHUB_REF_TYPE'], os.environ['GITHUB_REF_NAME']))
    else:
        raise SystemExit('usage: release_metadata.py [--verify-assets]')
