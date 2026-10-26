"""Create CPA plugin-store archives (one native library at zip root)."""
import hashlib
from pathlib import Path
import sys
import zipfile

version, goos, goarch, ext = sys.argv[1:]
root = Path('dist')
library = root / f'request-recorder.{ext}'
archive = root / f'request-recorder_{version}_{goos}_{goarch}.zip'
with zipfile.ZipFile(archive, 'w', zipfile.ZIP_DEFLATED) as z:
    z.write(library, library.name)
checksums = []
for path in sorted(root.glob(f'request-recorder_{version}_*.zip')):
    checksums.append(f'{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}\n')
(root / 'checksums.txt').write_text(''.join(checksums))
print(archive)
