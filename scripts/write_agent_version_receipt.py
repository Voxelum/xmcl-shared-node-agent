# Observed-version receipt for the existing isolated CI same-commit build.
# Run only in the existing isolated runtime-release build job, no production env.
import os,json,hashlib,subprocess,re
from pathlib import Path
commit=os.environ['GITHUB_SHA']
if not re.fullmatch('[a-f0-9]{40}',commit): raise SystemExit('invalid commit')
agent=Path('dist/xmcl-shared-node-agent-linux-amd64').resolve()
output=Path('dist/agent-version-evidence.json')
if output.exists(): raise SystemExit('refuse version receipt overwrite')
# Existing main exits at config.Load with an empty environment, before any runtime initialization.
p=subprocess.run([str(agent)],env={},capture_output=True,text=True,timeout=5)
lines=p.stdout.splitlines()
if p.returncode!=1 or len(lines)!=1 or p.stderr: raise SystemExit('unexpected version probe outcome')
record=json.loads(lines[0])
if record.get('message')!='load configuration' or record.get('level')!='error' or record.get('version')!=commit:
    raise SystemExit('observed version does not match build commit')
receipt={'schema':'xmcl-observed-agent-version-v1','gitCommit':commit,'observedAgentVersion':record['version'],'agentSha256':hashlib.sha256(agent.read_bytes()).hexdigest()}
with output.open('x',encoding='utf8') as stream: stream.write(json.dumps(receipt,indent=2)+'\n')
