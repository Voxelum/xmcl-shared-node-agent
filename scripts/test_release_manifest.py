"""Offline writer regressions; fixtures are not release/version evidence."""
from pathlib import Path
import hashlib
import json
import subprocess
import sys
import tempfile
import unittest

class ManifestReceipt(unittest.TestCase):
    def test_receipt_binding(self):
        writer = Path(__file__).with_name('write_release_manifest.py')
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            def put(name, value):
                path = root / name
                path.write_bytes(value if isinstance(value, bytes) else json.dumps(value).encode())
                return path
            commit = 'a' * 40
            catalog = put('catalog', b'{"fixture":true}')
            tool = put('toolchain', {'catalogRevision':'b'*64, 'runtimeCatalogRevision':hashlib.sha256(catalog.read_bytes()).hexdigest()})
            agent = put('agent', b'fixture-agent-only')
            helper = put('helper', b'fixture-helper-only')
            runtime = put('runtime', b'fixture-runtime-only')
            for case in ['old-version', 'wrong-hash', 'wrong-commit', 'extra-field', 'valid-fixture']:
                with self.subTest(case=case):
                    receipt = {'schema':'xmcl-observed-agent-version-v1', 'gitCommit':commit, 'observedAgentVersion':commit, 'agentSha256':hashlib.sha256(agent.read_bytes()).hexdigest()}
                    if case == 'old-version': receipt['observedAgentVersion'] = 'isolated-tofu-candidate.20261019'
                    if case == 'wrong-hash': receipt['agentSha256'] = '0'*64
                    if case == 'wrong-commit': receipt['gitCommit'] = 'c'*40
                    if case == 'extra-field': receipt['unreviewed'] = True
                    evidence = put('receipt', receipt)
                    output = root / (case+'.json')
                    result = subprocess.run([sys.executable,str(writer),'--commit',commit,'--tag','v0.0.1-fixture','--image-digest','sha256:'+'d'*64,'--builder-image-digest','sha256:'+'e'*64,'--catalog',str(catalog),'--toolchain-catalog',str(tool),'--agent',str(agent),'--quota-helper',str(helper),'--runtime',str(runtime),'--agent-version-evidence',str(evidence),'--output',str(output)], capture_output=True, text=True)
                    if case == 'valid-fixture':
                        self.assertEqual(result.returncode, 0, result.stderr)
                        manifest = json.loads(output.read_text())
                        self.assertEqual(manifest['schemaVersion'], 2)
                        self.assertEqual(manifest['artifacts']['nodeAgent']['version'], commit)
                    else:
                        self.assertNotEqual(result.returncode, 0)
                        self.assertFalse(output.exists())

if __name__ == '__main__': unittest.main(verbosity=2)
