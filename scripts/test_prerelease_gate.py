"""Offline: execute the actual publication guard with stubbed git/gh and a sentinel."""
from pathlib import Path
import os
import subprocess
import shutil
import textwrap
import unittest

WORKFLOW = Path(__file__).resolve().parents[1] / '.github/workflows/publish.yml'
text = WORKFLOW.read_text()
guard = textwrap.dedent(text.split('        run: |\n', 1)[1].split('          go test ./...', 1)[0])
script = '''set -euo pipefail
git() { printf '%s' "$MOCK_TAG_LINE"; return "${MOCK_GIT_STATUS:-0}"; }
gh() { printf '%s' "$MOCK_RELEASES"; return "${MOCK_GH_STATUS:-0}"; }
''' + guard + '\nprintf "REACHED_NEXT_STEP\\n"\n'

class PrereleaseGuard(unittest.TestCase):
    def test_exact_and_rejections(self):
        sha = 'a' * 40
        tag = 'v0.3.22-lightnode.20261019.1'
        env = dict(os.environ, GITHUB_REF='refs/heads/test/lightnode-20261019-r1',
                   GITHUB_SHA=sha, GITHUB_REPOSITORY='Voxelum/xmcl-shared-node-agent',
                   RELEASE_TAG=tag, MOCK_TAG_LINE=sha+'\trefs/tags/'+tag,
                   MOCK_RELEASES='v0.3.21')
        cases = [
            ('exact', {}, True),
            ('main', {'GITHUB_REF':'refs/heads/main'}, False),
            ('tag-ref', {'GITHUB_REF':'refs/tags/'+tag}, False),
            ('stable', {'RELEASE_TAG':'v0.3.22'}, False),
            ('other-prerelease', {'RELEASE_TAG':'v0.3.22-lightnode.20261019.2'}, False),
            ('invalid-sha', {'GITHUB_SHA':'not-a-sha'}, False),
            ('absent-tag', {'MOCK_TAG_LINE':''}, False),
            ('wrong-tag-sha', {'MOCK_TAG_LINE':'b'*40+'\trefs/tags/'+tag}, False),
            ('existing-release', {'MOCK_RELEASES':'v0.3.21\n'+tag+'\nv0.3.20'}, False),
            ('tag-read-error', {'MOCK_GIT_STATUS':'1'}, False),
            ('release-read-error', {'MOCK_GH_STATUS':'1'}, False),
        ]
        for name, change, allowed in cases:
            with self.subTest(name=name):
                result = subprocess.run([shutil.which('bash'),'-c',script], env=env | change, text=True, capture_output=True)
                self.assertEqual(result.returncode == 0, allowed, result.stderr)
                self.assertEqual('REACHED_NEXT_STEP' in result.stdout, allowed)
                print(f'{name}: sentinel={allowed}, exit={result.returncode}')

if __name__ == '__main__':
    unittest.main(verbosity=2)
