#!/usr/bin/env python3
"""Read-only guard for a saved dev pause/resume plan. Never applies a plan."""
import argparse
import json
from pathlib import Path
import re
import subprocess

# Only resources whose lifetime belongs to a running dev session.
SESSION_RESOURCES = re.compile(
    r'(?:module\.network\.(?:aws_nat_gateway\.this|aws_eip\.nat|aws_route\.nat)\["[a-z0-9-]+[a-z]"\]'
    r'|module\.endpoints\.aws_vpc_endpoint\.interface\["[a-z]+"\]'
    r'|module\.endpoints\.aws_security_group\.interfaces\[0\]'
    r'|module\.endpoints\.aws_vpc_security_group_ingress_rule\.interfaces\["(?:gateway|jobs)"\]'
    r'|module\.alb\.(?:aws_lb\.this|aws_lb_target_group\.s3|aws_lb_listener\.https)\[0\]'
    r'|module\.compute\.(?:aws_instance\.runtime|aws_volume_attachment\.data)\[0\]'
    r'|module\.fleet\.(?:aws_autoscaling_group\.gateway|aws_launch_template\.gateway)\[0\])'
)


def check_plan(plan, action, account, region='ap-southeast-1', installation_name='miles-dev', boundary_arn=None):
    if not re.fullmatch(r'[0-9]{12}', account):
        raise ValueError('Expected account ID must have 12 digits.')
    if action not in ('pause', 'resume'):
        raise ValueError('Choose pause or resume.')
    if not str(plan.get('format_version', '')).startswith('1.') or plan.get('errored') is not False or plan.get('complete') is not True:
        raise ValueError('Require a complete, non-errored Terraform JSON plan (format 1.x).')
    values = {key: value.get('value') for key, value in plan.get('variables', {}).items()}
    expected_boundary = boundary_arn or f'arn:aws:iam::{account}:policy/miles/bootstrap/MilesDevRuntimeBoundary'
    if not re.fullmatch(r'arn:aws:iam::' + account + r':policy/[A-Za-z0-9_+=,.@/-]+', expected_boundary):
        raise ValueError('Expected boundary must belong to the target account.')
    if (values.get('account_id') != account or values.get('region') != region
            or values.get('installation_name', 'miles-dev') != installation_name):
        raise ValueError('Plan account/Region/installation differs from the expected deployment.')
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
        if address.startswith('module.fleet.') and not values.get('enable_asg'):
            raise ValueError('Fleet transition requires the reviewed ASG configuration.')
        zone = re.search(r'\["([a-z0-9-]+[a-z])"\]$', address)
        if address.startswith('module.network.') and zone and not re.fullmatch(re.escape(region) + '[a-z]', zone[1]):
            raise ValueError('Session resource belongs to another Region.')
        changes.append(address)
    if action == 'pause':
        outputs = plan.get('planned_values', {}).get('outputs', {})
        network = outputs.get('network_inventory', {}).get('value', {})
        runtime = outputs.get('runtime_inventory', {}).get('value', {})
        if (network.get('paused') is not True or network.get('nat_count') != 0
                or network.get('egress_mode') != 'isolated' or network.get('interface_endpoints') != {}
                or 'alb_dns' not in runtime or runtime['alb_dns'] is not None
                or runtime.get('compute', {}).get('instance_id', 'unknown') is not None
                or outputs.get('fleet_contract', {}).get('value') is not None
                or values.get('enable_asg') and 'value' not in outputs.get('fleet_contract', {})):
            raise ValueError('Paused outputs must show no EC2, NAT, interface endpoints or ALB.')
    return changes


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['pause', 'resume'])
    parser.add_argument('plan', type=Path, help='Private saved binary Terraform plan, not a hand-edited JSON file.')
    parser.add_argument('--account-id', required=True)
    parser.add_argument('--region', default='ap-southeast-1')
    parser.add_argument('--installation-name', default='miles-dev')
    parser.add_argument('--boundary-arn', help='Expected reviewed boundary ARN in the target account.')
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
        changes = check_plan(json.loads(result.stdout), args.action, args.account_id,
                             args.region, args.installation_name, args.boundary_arn)
    except (ValueError, KeyError, TypeError) as error:
        raise SystemExit(f'BLOCKED: {error}')
    print(f'PASS: {args.action} plan changes only {len(changes)} session resources. No apply performed.')


if __name__ == '__main__':
    main()
