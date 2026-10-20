import copy
import json
from pathlib import Path
import sys
import unittest
from jsonschema import ValidationError

sys.path.insert(0, str(Path(__file__).parents[1]))
from pricing import API_RATES, EXAMPLE, validate


class PricingContractTests(unittest.TestCase):
    def setUp(self):
        self.data = json.loads(EXAMPLE.read_text())

    def test_example_and_required_verified_rate(self):
        rates = validate(self.data, ['network.nat_hour'])
        self.assertEqual(rates['network.nat_hour']['tiers'][0]['usd'], '0.0590000000')

    def test_unpriced_is_not_zero_or_available_to_costing(self):
        self.assertEqual(validate(self.data)['ec2.compute']['tiers'], [])
        with self.assertRaisesRegex(ValueError, 'Missing verified price'):
            validate(self.data, ['ec2.compute'])
        with self.assertRaisesRegex(ValueError, 'Missing verified price'):
            validate(self.data, ['not-a-price'])

    def test_schema_rejects_invalid_values_and_unknown_fields(self):
        for change in [
            lambda d: d.update(account_id='123456789012'),
            lambda d: d.update(as_of='not-a-date'),
            lambda d: d['rates'][0]['tiers'][0].update(usd=-1),
            lambda d: d['rates'][0]['tiers'][0].update(usd='-1'),
            lambda d: d['rates'][0].update(status='unpriced'),
            lambda d: d['rates'][0].update(tiers=[]),
        ]:
            with self.subTest(change=change):
                d=copy.deepcopy(self.data);change(d)
                with self.assertRaises(ValidationError):validate(d)

    def test_duplicate_source_and_tier_integrity(self):
        for change in [
            lambda d: d['rates'].append(copy.deepcopy(d['rates'][0])),
            lambda d: d['rates'][0].update(source_id='unknown'),
            lambda d: d['rates'][0]['tiers'][0].update(begin='1'),
            lambda d: d['rates'][0]['tiers'][0].update(end='0'),
            lambda d: d['storage_rules'].append(copy.deepcopy(d['storage_rules'][0])),
            lambda d: d['sources']['api'].update(retrieved_at='2099-01-01'),
        ]:
            with self.subTest(change=change):
                d=copy.deepcopy(self.data);change(d)
                with self.assertRaises(ValueError):validate(d)


    def test_rejects_mismatched_region_and_quote_dimensions(self):
        for change in [
            lambda d: d.update(region='us-east-1'),
            lambda d: d['rates'][0]['dimensions'].update(regionCode='us-east-1'),
            lambda d: d['rates'][0]['dimensions'].update(location='US East (N. Virginia)'),
            lambda d: d['rates'][0]['dimensions'].update(usagetype='USE1-TimedStorage-ByteHrs'),
            lambda d: d['rates'][0]['dimensions'].update(usagetype='APS1-Requests-Tier1'),
            lambda d: d['rates'][0]['dimensions'].pop('regionCode'),
            lambda d: d['rates'][0].update(id='s3.unregistered'),
        ]:
            with self.subTest(change=change):
                data = copy.deepcopy(self.data)
                change(data)
                with self.assertRaises(ValueError):
                    validate(data)

    def test_rejects_wrong_units_or_services_for_every_api_rate(self):
        for rate_id in API_RATES:
            for field, value in [('unit', 'wrong-unit'), ('service', 'wrong-service')]:
                with self.subTest(rate=rate_id, field=field):
                    data = copy.deepcopy(self.data)
                    rate = next(r for r in data['rates'] if r['id'] == rate_id)
                    rate[field] = value
                    with self.assertRaisesRegex(ValueError, 'service/unit'):
                        validate(data)
        for source_id in ['api', 's3']:
            self.data['rates'][0].update(unit='Hrs', source_id=source_id)
            with self.assertRaisesRegex(ValueError, 'service/unit'):
                validate(self.data)

    def test_gross_storage_excludes_but_preserves_free_tier(self):
        rate = validate(self.data)['ddb.storage']
        self.assertEqual(rate['tiers'], [{'begin': '0', 'end': None, 'usd': '0.2850000000'}])
        allowance = rate['gross_adjustment']['allowance']
        self.assertEqual(allowance['scope'], 'account_region')
        self.assertEqual(allowance['quantity'], '25')
        self.assertEqual(rate['gross_adjustment']['raw_tiers'][0],
                         {'begin': '0', 'end': '25', 'usd': '0.0000000000'})
        self.assertEqual(validate(self.data)['s3.restore_bulk_gb']['tiers'][0]['usd'], '0.0000000000')

    def test_rejects_inconsistent_gross_adjustments(self):
        for change in [
            lambda r: r.update(tiers=copy.deepcopy(r['gross_adjustment']['raw_tiers'])),
            lambda r: r.pop('gross_adjustment'),
            lambda r: r['tiers'][0].update(usd='0'),
            lambda r: r['gross_adjustment']['allowance'].update(quantity='24'),
            lambda r: r['gross_adjustment']['allowance'].update(quantity='0'),
            lambda r: r['gross_adjustment']['allowance'].update(source_id='unknown'),
            lambda r: r['gross_adjustment']['raw_tiers'][0].update(usd='0.1'),
            lambda r: r['gross_adjustment']['raw_tiers'][1].update(end='100'),
            lambda r: r['gross_adjustment']['allowance'].update(scope='per_trial'),
            lambda r: r.update(status='unpriced', tiers=[]),
        ]:
            with self.subTest(change=change):
                data = copy.deepcopy(self.data)
                change(next(r for r in data['rates'] if r['id'] == 'ddb.storage'))
                with self.assertRaises((ValueError, ValidationError)):
                    validate(data)
        data = copy.deepcopy(self.data)
        data['rates'][0]['gross_adjustment'] = validate(self.data)['ddb.storage']['gross_adjustment']
        with self.assertRaisesRegex(ValueError, 'only supported'):
            validate(data)


if __name__ == '__main__':unittest.main()
