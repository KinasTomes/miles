import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('deploy_identity', Path(__file__).parents[1] / 'check-deploy-identity.py')
gate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate)


class IdentityGateTests(unittest.TestCase):
    role = 'arn:aws:iam::123456789012:role/miles/dev/deploy'

    def test_expected_assumed_role(self):
        gate.check_identity(self.role, {'Account': '123456789012', 'Arn': 'arn:aws:sts::123456789012:assumed-role/deploy/session'})

    def test_rejects_root_user_other_account_and_role(self):
        for account, arn in [
            ('123456789012', 'arn:aws:iam::123456789012:root'),
            ('123456789012', 'arn:aws:iam::123456789012:user/deploy'),
            ('000000000000', 'arn:aws:sts::000000000000:assumed-role/deploy/session'),
            ('123456789012', 'arn:aws:sts::123456789012:assumed-role/other/session'),
        ]:
            with self.subTest(arn=arn), self.assertRaises(ValueError):
                gate.check_identity(self.role, {'Account': account, 'Arn': arn})

    def test_rejects_malformed_expected_role(self):
        with self.assertRaises(ValueError):
            gate.check_identity('arn:aws:iam::123456789012:root', {})


if __name__ == '__main__':
    unittest.main()
