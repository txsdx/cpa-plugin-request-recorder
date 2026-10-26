"""Create CPA plugin-store archives (one native library at zip root)."""
import hashlib
from pathlib import Path
import re
import sys
import zipfile

VERSION_PATTERN = re.compile(r'[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?')
EXTENSIONS = {'linux': 'so', 'darwin': 'dylib', 'windows': 'dll'}


def validate_version(version):
    if not VERSION_PATTERN.fullmatch(version):
        raise ValueError('version must be X.Y.Z or X.Y.Z-prerelease')
    return version


def package(version, goos, goarch, ext, root=Path('dist')):
    validate_version(version)
    if EXTENSIONS.get(goos) != ext or goarch not in ('amd64', 'arm64'):
        raise ValueError('unsupported OS/architecture/library extension')
    library = root / f'request-recorder.{ext}'
    archive = root / f'request-recorder_{version}_{goos}_{goarch}.zip'
    with zipfile.ZipFile(archive, 'w', zipfile.ZIP_DEFLATED) as z:
        z.write(library, library.name)
    checksums = []
    for path in sorted(root.glob(f'request-recorder_{version}_*.zip')):
        checksums.append(f'{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}\n')
    (root / 'checksums.txt').write_text(''.join(checksums), encoding='utf-8')
    return archive


if __name__ == '__main__':
    print(package(*sys.argv[1:]))
