#!/usr/bin/env python3
"""Read-only guard for a saved dev pause/resume plan. Never applies a plan."""
import argparse
import json
from pathlib import Path
import re
import subprocess

# Only resources whose lifetime belongs to a running dev session.
SESSION_RESOURCES = re.compile(
    r'(?:module\.network\.(?:aws_nat_gateway\.this|aws_eip\.nat|aws_route\.nat)\["ap-southeast-1[a-z]"\]'
    r'|module\.endpoints\.aws_vpc_endpoint\.interface\["[a-z]+"\]'
    r'|module\.endpoints\.aws_security_group\.interfaces\[0\]'
    r'|module\.endpoints\.aws_vpc_security_group_ingress_rule\.interfaces\["(?:gateway|jobs)"\]'
    r'|module\.alb\.(?:aws_lb\.this|aws_lb_target_group\.s3|aws_lb_listener\.https)\[0\]'
    r'|module\.compute\.(?:aws_instance\.runtime|aws_volume_attachment\.data)\[0\])'
)


def check_plan(plan, action, account):
    if not re.fullmatch(r'[0-9]{12}', account):
        raise ValueError('Expected account ID must have 12 digits.')
    if action not in ('pause', 'resume'):
        raise ValueError('Choose pause or resume.')
    if not str(plan.get('format_version', '')).startswith('1.') or plan.get('errored') is not False or plan.get('complete') is not True:
        raise ValueError('Require a complete, non-errored Terraform JSON plan (format 1.x).')
    values = {key: value.get('value') for key, value in plan.get('variables', {}).items()}
    expected_boundary = f'arn:aws:iam::{account}:policy/miles/bootstrap/MilesDevRuntimeBoundary'
    if values.get('account_id') != account or values.get('region') != 'ap-southeast-1':
        raise ValueError('Plan account/Region differs from the expected dev deployment.')
    if values.get('deploy_role_arn') is not None or values.get('runtime_permissions_boundary_arn') != expected_boundary:
        raise ValueError('Use the current deploy session and the reviewed runtime boundary.')
    # Terraform preserves CLI -var booleans as raw strings in plan.variables.
    paused = values.get('paused')
    if paused == 'true':
        paused = True
    elif paused == 'false':
        paused = False
    if paused is not (action == 'pause'):
        raise ValueError('Plan paused input does not match the requested action.')
    prior = plan.get('prior_state', {}).get('values', {}).get('outputs', {}).get('session_configuration', {}).get('value')
    configured = plan.get('planned_values', {}).get('outputs', {}).get('session_configuration', {}).get('value')
    if not isinstance(prior, dict) or configured != prior:
        raise ValueError('Initialize/review session configuration first; pause/resume cannot change AMI or topology.')
    if plan.get('resource_drift') or plan.get('deferred_changes'):
        raise ValueError('Review drift or deferred work separately from a session transition.')
    for check in plan.get('checks', []):
        if check.get('status') != 'pass':
            raise ValueError('All Terraform checks must pass before a session transition.')
    allowed_action = ['delete'] if action == 'pause' else ['create']
    changes = []
    for resource in plan.get('resource_changes', []):
        change = resource['change']
        actions = change['actions']
        if resource.get('mode') == 'data' and actions in (['read'], ['no-op'], ['delete']):
            continue
        if change.get('importing') or resource.get('previous_address'):
            raise ValueError('Import/move requires a separate reviewed plan.')
        if actions == ['no-op']:
            continue
        address = resource['address']
        if resource.get('mode') != 'managed' or not SESSION_RESOURCES.fullmatch(address) or actions != allowed_action:
            raise ValueError(f'Session plan changes a retained resource or needs separate review: {address} ({actions}).')
        changes.append(address)
    if action == 'pause':
        outputs = plan.get('planned_values', {}).get('outputs', {})
        network = outputs.get('network_inventory', {}).get('value', {})
        runtime = outputs.get('runtime_inventory', {}).get('value', {})
        if (network.get('paused') is not True or network.get('nat_count') != 0
                or network.get('egress_mode') != 'isolated' or network.get('interface_endpoints') != {}
                or 'alb_dns' not in runtime or runtime['alb_dns'] is not None
                or runtime.get('compute', {}).get('instance_id', 'unknown') is not None):
            raise ValueError('Paused outputs must show no EC2, NAT, interface endpoints or ALB.')
    return changes


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['pause', 'resume'])
    parser.add_argument('plan', type=Path, help='Private saved binary Terraform plan, not a hand-edited JSON file.')
    parser.add_argument('--account-id', required=True)
    args = parser.parse_args()
    source = Path(__file__).resolve().parents[1]
    target = args.plan.resolve()
    if not args.plan.is_absolute() or target.is_relative_to(source.parents[2]) or not target.is_file():
        parser.error('Saved plan must be an existing absolute path outside the repository.')
    result = subprocess.run(['terraform', '-chdir=' + str(source / 'environments/dev'), 'show', '-json', str(target)],
                            capture_output=True, text=True)
    if result.returncode:
        raise SystemExit('Cannot read the saved plan. Terraform diagnostics are suppressed because they may contain private config.')
    try:
        changes = check_plan(json.loads(result.stdout), args.action, args.account_id)
    except (ValueError, KeyError, TypeError) as error:
        raise SystemExit(f'BLOCKED: {error}')
    print(f'PASS: {args.action} plan changes only {len(changes)} session resources. No apply performed.')


if __name__ == '__main__':
    main()
