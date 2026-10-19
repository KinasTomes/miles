#!/usr/bin/env python3
"""Read-only operational gate; run before every bootstrap/dev apply. Never invokes Terraform."""
import argparse
import json
import re
import subprocess


def check_identity(role_arn, identity):
    role = re.fullmatch(r'arn:aws:iam::([0-9]{12}):role/(.+)', role_arn)
    if not role:
        raise ValueError('Expected deployment IAM role ARN is invalid.')
    # IAM role names are unique within an account; STS session ARNs omit the IAM path.
    account, path = role.groups()
    name = path.rsplit('/', 1)[-1]
    session = re.fullmatch(r'arn:aws:sts::([0-9]{12}):assumed-role/([^/]+)/[^/]+', identity.get('Arn', ''))
    if not session or identity.get('Account') != account or session.groups() != (account, name):
        raise ValueError('Apply blocked: credentials must be a session of the expected deployment role; root/user/wrong-account/wrong-role rejected.')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--role-arn', required=True)
    args = parser.parse_args()
    result = subprocess.run(['aws', 'sts', 'get-caller-identity', '--output', 'json', '--no-cli-pager'],
                            capture_output=True, text=True)
    if result.returncode:
        raise SystemExit('Apply blocked: AWS identity cannot be verified.')
    try:
        check_identity(args.role_arn, json.loads(result.stdout))
    except (ValueError, KeyError) as error:
        raise SystemExit(str(error))
    print('PASS: active AWS identity matches the expected deployment role. No resources changed.')


if __name__ == '__main__':
    main()
