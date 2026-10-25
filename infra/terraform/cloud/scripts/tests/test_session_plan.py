"""Adversarial plans must not turn cost control into data destruction."""
import copy
import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('session_plan', Path(__file__).parents[1] / 'check-session-plan.py')
gate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate)
ACCOUNT = '123456789012'


def fixture(action='pause'):
    settings = {'ami_id': 'ami-0123456789abcdef0', 'egress_mode': 'nat', 'nat_per_az': False,
                'enable_alb': False, 'certificate_arn': None, 'enable_compute': True}
    return {
        'format_version': '1.2', 'complete': True, 'errored': False,
        'variables': {k: {'value': v} for k, v in {
            'account_id': ACCOUNT, 'region': 'ap-southeast-1', 'paused': action == 'pause',
            'deploy_role_arn': None,
            'runtime_permissions_boundary_arn': f'arn:aws:iam::{ACCOUNT}:policy/miles/bootstrap/MilesDevRuntimeBoundary',
        }.items()},
        'planned_values': {'outputs': {
            'session_configuration': {'value': copy.deepcopy(settings)},
            'network_inventory': {'value': {'paused': True, 'nat_count': 0, 'egress_mode': 'isolated', 'interface_endpoints': {}}},
            'runtime_inventory': {'value': {'alb_dns': None, 'compute': {'instance_id': None}}},
        }},
        'prior_state': {'values': {'outputs': {'session_configuration': {'value': settings}}}},
        'resource_changes': [],
    }


def change(address, actions):
    return {'address': address, 'mode': 'managed', 'change': {'actions': actions}}


class SessionPlanTests(unittest.TestCase):
    def test_pause_and_resume_only_session_resources(self):
        addresses = ['module.network.aws_eip.nat["ap-southeast-1a"]',
                     'module.network.aws_nat_gateway.this["ap-southeast-1a"]',
                     'module.network.aws_route.nat["ap-southeast-1b"]',
                     'module.endpoints.aws_vpc_endpoint.interface["ssmmessages"]',
                     'module.endpoints.aws_security_group.interfaces[0]',
                     'module.alb.aws_lb_listener.https[0]',
                     'module.compute.aws_instance.runtime[0]',
                     'module.compute.aws_volume_attachment.data[0]']
        for action, operation in [('pause', 'delete'), ('resume', 'create')]:
            plan = fixture(action)
            plan['resource_changes'] = [change(a, [operation]) for a in addresses]
            self.assertEqual(gate.check_plan(plan, action, ACCOUNT), addresses)

    def test_retained_and_unknown_changes_are_blocked(self):
        for address in ['module.data.aws_s3_bucket.data', 'module.data.aws_dynamodb_table.metadata["Objects"]',
                        'module.compute.aws_ebs_volume.data[0]', 'module.security.aws_kms_key.runtime',
                        'module.iam.aws_iam_role.runtime["gateway"]', 'aws_s3_bucket.state',
                        'module.network.aws_vpc.this', 'module.endpoints.aws_vpc_endpoint.gateway["s3"]',
                        'module.future.aws_instance.runtime[0]']:
            with self.subTest(address=address), self.assertRaises(ValueError):
                plan = fixture()
                plan['resource_changes'] = [change(address, ['delete'])]
                gate.check_plan(plan, 'pause', ACCOUNT)

    def test_cli_boolean_values_match_the_requested_transition(self):
        for action, value in [('pause', 'true'), ('resume', 'false')]:
            with self.subTest(action=action):
                plan = fixture(action)
                plan['variables']['paused']['value'] = value
                self.assertEqual(gate.check_plan(plan, action, ACCOUNT), [])
                with self.assertRaises(ValueError):
                    gate.check_plan(plan, 'resume' if action == 'pause' else 'pause', ACCOUNT)
        for value in ['TRUE', 'False', '1', '', ' true ', 1, 0, None]:
            with self.subTest(value=value), self.assertRaises(ValueError):
                plan = fixture()
                plan['variables']['paused']['value'] = value
                gate.check_plan(plan, 'pause', ACCOUNT)

    def test_replacements_updates_and_wrong_direction_are_blocked(self):
        for actions in [['delete', 'create'], ['create', 'delete'], ['update'], ['create']]:
            with self.subTest(actions=actions), self.assertRaises(ValueError):
                plan = fixture()
                plan['resource_changes'] = [change('module.compute.aws_instance.runtime[0]', actions)]
                gate.check_plan(plan, 'pause', ACCOUNT)

    def test_drift_incomplete_wrong_identity_and_failed_checks_are_blocked(self):
        original = fixture()
        mutations = [
            lambda p: p.update(complete=False), lambda p: p.update(errored=True),
            lambda p: p.update(resource_drift=[{}]), lambda p: p.update(deferred_changes=[{}]),
            lambda p: p.update(checks=[{'status': 'unknown'}]),
            lambda p: p['variables']['paused'].update(value=False),
            lambda p: p['variables']['account_id'].update(value='000000000000'),
            lambda p: p['variables']['region'].update(value='ap-southeast-2'),
            lambda p: p['variables']['deploy_role_arn'].update(value='arn:aws:iam::123456789012:role/other'),
            lambda p: p['planned_values']['outputs']['runtime_inventory']['value']['compute'].update(instance_id='i-running'),
        ]
        for i, mutate in enumerate(mutations):
            with self.subTest(case=i), self.assertRaises(ValueError):
                plan = copy.deepcopy(original)
                mutate(plan)
                gate.check_plan(plan, 'pause', ACCOUNT)

    def test_idempotent_pause_and_retained_noops_are_allowed(self):
        plan = fixture()
        plan['resource_changes'] = [change('module.data.aws_s3_bucket.data', ['no-op'])]
        self.assertEqual(gate.check_plan(plan, 'pause', ACCOUNT), [])

    def test_new_topology_or_ami_requires_separate_review(self):
        for key, value in [('ami_id', 'ami-00000000000000000'), ('egress_mode', 'interface'),
                           ('nat_per_az', True), ('enable_compute', False)]:
            with self.subTest(key=key), self.assertRaises(ValueError):
                plan = fixture('resume')
                plan['planned_values']['outputs']['session_configuration']['value'][key] = value
                gate.check_plan(plan, 'resume', ACCOUNT)
        with self.assertRaises(ValueError):
            plan = fixture()
            plan.pop('prior_state')
            gate.check_plan(plan, 'pause', ACCOUNT)
