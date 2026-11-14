"""Offline contract check for a proposed benchmark workload matrix.

This program only reads a local JSON file. It does not run workloads, contact
AWS, grant approval, or certify that a threshold was achieved.
"""

import argparse
import json
from pathlib import Path
import sys


REQUIRED = (
    {('warm', topology, f'warm-c{c}') for topology in ('cloud', 'edge') for c in (1, 4)}
    | {('overwrite', 'cloud', 'overwrite-versions')}
    | {('domain', 'cloud', v) for v in ('shared-default', 'separated')}
    | {('multipart', 'cloud', v) for v in ('threshold-minus-one', 'threshold-equal', 'threshold-plus-one')}
    | {('cold', t, v) for t in ('cloud', 'edge') for v in ('ir', 'flexible')}
    | {('edge-clone', 'edge', v) for v in ('clone-download', 'clone-upload')}
    | {('recovery', t, v) for t in ('cloud', 'edge') for v in ('r1', 'r2')}
)
CASE_KEYS = {'id', 'scenario', 'topology', 'variant', 'concurrency', 'domain',
             'cache_state', 'storage_class', 'comparator', 'dataset', 'runner'}
TOP_KEYS = {'schema_version', 'status', 'benchmark_executed', 'aws_approval',
            'dataset_groups', 'protocol', 'cases'}
PROTOCOL = {'region': 'ap-southeast-1', 'repetitions': 3, 'read_repeats': 3,
            'max_concurrency': 4, 'isolation': 'fresh-backend-or-verified-reset',
            'failure_policy': 'stop-the-line', 'raw_evidence': 'required'}


def ensure(condition, message):
    if not condition:
        raise ValueError(message)


def strict_dict(obj, keys, name):
    ensure(type(obj) is dict and set(obj) == keys, f'{name}: required/unknown keys')


def expected_case_id(scenario, topology, variant):
    if scenario == 'warm':
        return f'warm-{topology}-c{variant[-1]}'
    if scenario == 'overwrite':
        return 'overwrite-cloud'
    if scenario in ('domain', 'multipart'):
        return f'{scenario}-{variant}'
    if scenario == 'cold':
        return f'cold-{topology}-{variant}'
    if scenario == 'edge-clone':
        return f'edge-{variant}'
    return f'recovery-{topology}-{variant}'


def expected_fields(scenario, topology, variant):
    """Canonical row contract. Preserve benchmark semantics across edits."""
    if scenario == 'warm':
        c = int(variant[-1])
        return c, 'default', 'verify-before-run', 'STANDARD', 'direct-s3', 'backup30', 'warmbench'
    if scenario == 'domain':
        return 1, variant, 'not-verified', 'STANDARD', 'domain-contrast', 'backup30', 'warmbench'
    if scenario == 'cold':
        return 1, 'default', 'not-verified', 'GLACIER_IR' if variant == 'ir' else 'GLACIER', 'policy-control', 'backup30', 'requires-runner'
    if scenario == 'multipart':
        return 1, 'default', 'not-verified', 'STANDARD', 'direct-s3', 'boundary-8mib', 'warmbench'
    if scenario == 'edge-clone':
        return 1, 'cross-domain', 'not-verified', 'STANDARD', 'matched-get-put', 'backup30', 'warmbench'
    if scenario == 'recovery':
        return 1, 'default', 'not-verified', 'STANDARD', 'fault-contract', 'backup30', 'requires-runner'
    return 1, 'default', 'not-verified', 'STANDARD', 'direct-s3', 'backup30', 'warmbench'


def validate(matrix):
    strict_dict(matrix, TOP_KEYS, 'matrix')
    ensure(type(matrix['schema_version']) is int and matrix['schema_version'] == 1, 'unsupported schema version')
    ensure(matrix['status'] == 'review-candidate', 'cannot mark thresholds approved from a matrix')
    ensure(matrix['benchmark_executed'] is False and matrix['aws_approval'] is False,
           'a proposed matrix is not evidence of execution or approval')
    ensure(matrix['dataset_groups'] == ['low-entropy', 'compressed-identical', 'random-distinct'],
           'incorrect or missing dataset entropy groups')
    strict_dict(matrix['protocol'], set(PROTOCOL), 'protocol')
    ensure(all(type(matrix['protocol'][name]) is type(value) and matrix['protocol'][name] == value
               for name, value in PROTOCOL.items()),
           'repetition, client, isolation or failure protocol changed')
    cases = matrix['cases']
    ensure(type(cases) is list and len(cases) == len(REQUIRED), 'matrix has missing/extra cases')
    ids, found = set(), set()
    for row in cases:
        strict_dict(row, CASE_KEYS, 'case')
        ensure(type(row['id']) is str and row['id'].isascii() and row['id'].replace('-', '').isalnum()
               and 1 <= len(row['id']) <= 64 and row['id'] not in ids,
               'duplicate/invalid case id')
        ids.add(row['id'])
        key = (row['scenario'], row['topology'], row['variant'])
        ensure(key in REQUIRED and key not in found, 'missing/duplicate/unexpected scenario')
        ensure(row['id'] == expected_case_id(*key), 'unstable case ID')
        found.add(key)
        expected = expected_fields(*key)
        fields = ('concurrency', 'domain', 'cache_state', 'storage_class', 'comparator', 'dataset', 'runner')
        ensure(tuple(row[f] for f in fields) == expected, f'case {row["id"]}: scenario contract mismatch')
        ensure(type(row['concurrency']) is int, f'case {row["id"]}: concurrency must be integer')
    ensure(found == REQUIRED, 'matrix incomplete')
    return {'validated_cases': len(found), 'available_warm_cases': sum(c['runner'] == 'warmbench' for c in cases),
            'requires_runner': sum(c['runner'] != 'warmbench' for c in cases), 'approval': 'not-granted',
            'benchmark_status': 'not-executed'}


def load(path):
    p = Path(path)
    if p.is_symlink() or not p.is_file() or p.stat().st_size > 128 * 1024:
        raise ValueError('matrix must be a regular local file <=128 KiB')
    def no_duplicates(pairs):
        d = {}
        for k, v in pairs:
            if k in d:
                raise ValueError('duplicate JSON key')
            d[k] = v
        return d
    def no_constants(_):
        raise ValueError('non-finite JSON number')
    return json.loads(p.read_text(encoding='utf-8'), object_pairs_hook=no_duplicates,
                      parse_constant=no_constants)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('matrix', help='proposed workload-matrix.json (read only)')
    args = parser.parse_args(argv)
    try:
        result = validate(load(args.matrix))
    except (ValueError, OSError, UnicodeError, TypeError, KeyError) as exc:
        print(f'invalid benchmark matrix: {exc}', file=sys.stderr)
        return 1
    print(json.dumps(result, sort_keys=True))
    return 0


if __name__ == '__main__':
    sys.exit(main())
