#!/usr/bin/env python3
"""Read-only AWS plans in a temporary local-backend copy; never apply or export secrets."""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--mode', choices=['isolated', 'nat', 'interface'], default='isolated')
parser.add_argument('--stack', choices=['dev', 'state'], default='dev')
parser.add_argument('--policy-plan-json', type=Path,
                    help='Write a private plan JSON outside the repository for IAM policy analysis.')
args = parser.parse_args()
source = Path(__file__).resolve().parents[1]
if args.policy_plan_json:
    target = args.policy_plan_json.absolute()
    if not args.policy_plan_json.is_absolute() or target.resolve().is_relative_to(source.parents[2]):
        parser.error('--policy-plan-json must be an absolute path outside the repository')
    if target.exists() or target.is_symlink():
        parser.error('--policy-plan-json must name a new file')
env = os.environ.copy()
# AWS CLI supports aws login even when the pinned provider SDK lacks that credential provider.
result = subprocess.run(['aws', 'configure', 'export-credentials', '--format', 'process'],
                        capture_output=True, text=True)
if result.returncode:
    raise SystemExit('AWS credential export failed; sign in using your existing AWS CLI workflow.')
credentials = json.loads(result.stdout)
for key, value in [('AWS_ACCESS_KEY_ID', 'AccessKeyId'), ('AWS_SECRET_ACCESS_KEY', 'SecretAccessKey'),
                   ('AWS_SESSION_TOKEN', 'SessionToken')]:
    if value in credentials:
        env[key] = credentials[value]
env['TF_IN_AUTOMATION'] = '1'
identity = json.loads(subprocess.run(['aws', 'sts', 'get-caller-identity', '--output', 'json', '--no-cli-pager'],
                                     env=env, capture_output=True, text=True, check=True).stdout)
account_id = identity['Account']
with tempfile.TemporaryDirectory(prefix='miles-plan-') as work:
    cloud = Path(work) / 'cloud'
    shutil.copytree(source, cloud, symlinks=True,
                    ignore=shutil.ignore_patterns('.terraform', 'terraform.tfstate*', '*.tfplan', 'terraform.tfvars', '*.auto.tfvars*'))
    stack_path = 'environments/dev' if args.stack == 'dev' else 'bootstrap/state'
    dev = cloud / stack_path
    # Reuse verified provider packages without copying hundreds of MB or backend metadata.
    providers = source / stack_path / '.terraform/providers'
    if providers.is_dir():
        (dev / '.terraform').mkdir(exist_ok=True)
        (dev / '.terraform/providers').symlink_to(providers, target_is_directory=True)
    (dev / 'backend.tf').write_text('terraform {\n  backend "local" {}\n}\n')
    def run(*command):
        return subprocess.run(command, cwd=dev, env=env, check=True, capture_output=True, text=True).stdout
    try:
        run('terraform', 'init', '-reconfigure', '-input=false', '-no-color', '-lockfile=readonly')
        variables = ['-var=account_id=' + account_id]
        if args.stack == 'dev':
            variables.append('-var=egress_mode=' + args.mode)
        run('terraform', 'plan', '-input=false', '-no-color', '-lock-timeout=30s',
            *variables, '-out=review.tfplan')
        plan = json.loads(run('terraform', 'show', '-json', 'review.tfplan'))
    except subprocess.CalledProcessError as error:
        # Terraform diagnostics may contain resource configuration. Keep them outside Git and inspect locally.
        diagnostic = Path(tempfile.gettempdir()) / 'miles-plan-error.log'
        fd = os.open(diagnostic, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, 'w') as stream:
            stream.write(error.stdout + error.stderr)
        raise SystemExit('Plan failed; inspect /tmp/miles-plan-error.log (not a committed artifact).')
    changes = [{'address': c['address'], 'actions': c['change']['actions']}
               for c in plan.get('resource_changes', []) if c['mode'] == 'managed']
    assert all(c['actions'] == ['create'] for c in changes), 'Fresh local plan unexpectedly modifies/deletes resources'
    if args.policy_plan_json:
        fd = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, 'w') as stream:
            json.dump(plan, stream)
    print(json.dumps({'stack': args.stack, 'mode': args.mode, 'backend': 'temporary local, empty state',
                      'applied': False, 'create_count': len(changes), 'changes': changes}, indent=2))
