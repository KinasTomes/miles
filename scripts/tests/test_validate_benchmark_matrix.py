"""Negative contract tests for the offline benchmark matrix checker."""

import copy
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('benchmark_matrix', ROOT / 'scripts/validate_benchmark_matrix.py')
validator = importlib.util.module_from_spec(spec)
spec.loader.exec_module(validator)


def matrix_fixture():
    return json.loads((ROOT / 'benchmarks/workload-matrix.json').read_text())


class BenchmarkMatrixTests(unittest.TestCase):
    def test_proposed_matrix_is_complete_but_not_approved(self):
        report = validator.validate(matrix_fixture())
        self.assertEqual(report['validated_cases'], 20)
        self.assertEqual(report['available_warm_cases'], 12)
        self.assertEqual(report['requires_runner'], 8)
        self.assertEqual(report['benchmark_status'], 'not-executed')
        self.assertEqual(report['approval'], 'not-granted')

    def test_missing_and_duplicate_scenarios_are_rejected(self):
        for mutate in (lambda m: m['cases'].pop(),
                       lambda m: m['cases'].__setitem__(0, copy.deepcopy(m['cases'][1])),
                       lambda m: m['cases'][0].update(variant='warm-c9')):
            with self.subTest(mutate=mutate):
                m = matrix_fixture()
                mutate(m)
                with self.assertRaises(ValueError):
                    validator.validate(m)

    def test_rejects_false_approval_and_unreviewed_run_protocol(self):
        for field, value in [('status', 'approved'), ('benchmark_executed', True), ('aws_approval', True),
                             ('schema_version', True), ('unexpected_field', 'private-token')]:
            with self.subTest(field=field):
                m = matrix_fixture()
                m[field] = value
                with self.assertRaises(ValueError):
                    validator.validate(m)
        for key, value in [('repetitions', 1), ('failure_policy', 'continue'),
                           ('isolation', 'fresh-prefix-only'), ('read_repeats', 0), ('repetitions', True)]:
            with self.subTest(key=key):
                m = matrix_fixture()
                m['protocol'][key] = value
                with self.assertRaises(ValueError):
                    validator.validate(m)

    def test_rejects_incorrect_comparators_dataset_and_methods(self):
        for case_index, field, value in [(0, 'concurrency', True), (0, 'concurrency', 4),
                                         (0, 'runner', 'requires-runner'),
                                         (4, 'runner', 'requires-runner'),
                                         (6, 'domain', 'shared-default'),
                                         (7, 'dataset', 'smoke'),
                                         (10, 'storage_class', 'STANDARD'),
                                         (10, 'runner', 'warmbench'),
                                         (14, 'comparator', 'direct-s3')]:
            with self.subTest(index=case_index, field=field):
                m = matrix_fixture()
                m['cases'][case_index][field] = value
                with self.assertRaises(ValueError):
                    validator.validate(m)

    def test_rejects_extra_columns_secret_and_duplicate_ids(self):
        for mutate in (lambda m: m['cases'][0].update(credentials_file='/tmp/private'),
                       lambda m: m['cases'][1].update(id=m['cases'][0]['id']),
                       lambda m: m['cases'][1].update(id='arbitrary-name')):
            m = matrix_fixture()
            mutate(m)
            with self.assertRaises(ValueError):
                validator.validate(m)

    def test_loader_rejects_duplicate_json_keys_and_non_finite_values(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / 'matrix.json'
            for value in ('{"schema_version":1,"schema_version":1}', '{"x": NaN}'):
                path.write_text(value)
                with self.assertRaises(ValueError):
                    validator.load(path)

    def test_cli_returns_validation_status_without_running_benchmark(self):
        self.assertEqual(validator.main([str(ROOT / 'benchmarks/workload-matrix.json')]), 0)
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / 'bad.json'
            path.write_text('{}')
            self.assertEqual(validator.main([str(path)]), 1)


if __name__ == '__main__':
    unittest.main()
