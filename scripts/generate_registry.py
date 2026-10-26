"""Generate a versioned release registry from the stable CPA source metadata.

The repository registry deliberately omits the optional display-fallback version:
CPA obtains current versions and updates from GitHub's latest published release.
"""
import argparse
import copy
import json
from pathlib import Path

from package import validate_version

PLUGIN_ID = 'request-recorder'
REPOSITORY = 'https://github.com/txsdx/cpa-plugin-request-recorder'


def validate_registry(registry):
    if registry.get('schema_version') != 1 or len(registry.get('plugins', [])) != 1:
        raise ValueError('expected a CPA schema 1 registry with exactly one plugin')
    plugin = registry['plugins'][0]
    if plugin.get('id') != PLUGIN_ID or plugin.get('repository') != REPOSITORY:
        raise ValueError('registry identity must match the native library and release repository')
    for key in ['name', 'description', 'author']:
        if not isinstance(plugin.get(key), str) or not plugin[key].strip():
            raise ValueError(f'missing registry field {key}')
    if 'version' in plugin:
        validate_version(plugin['version'])
    return registry


def generate_registry(source, destination, version):
    validate_version(version)
    registry = copy.deepcopy(validate_registry(json.loads(source.read_text(encoding='utf-8'))))
    registry['plugins'][0]['version'] = version
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_text(json.dumps(registry, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    return registry


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--version', required=True)
    parser.add_argument('--source', type=Path, default=Path('registry.json'))
    parser.add_argument('--output', type=Path, default=Path('dist/registry.json'))
    args = parser.parse_args()
    generate_registry(args.source, args.output, args.version)
