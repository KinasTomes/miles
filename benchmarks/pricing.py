"""Validate the shared pricing contract; never substitutes unpriced items with zero."""
import argparse
from datetime import date
from decimal import Decimal
import json
from pathlib import Path

from jsonschema import Draft202012Validator, FormatChecker

ROOT = Path(__file__).resolve().parents[1]
SCHEMA = ROOT / 'configs/pricing/pricing.schema.json'
EXAMPLE = ROOT / 'configs/pricing/pricing.ap-southeast-1.example.json'


# New Regions and verified rate IDs require an explicit qualifier/unit mapping.
REGIONS = {'ap-southeast-1': ('Asia Pacific (Singapore)', 'APS1-')}
API_RATES = {
    's3.storage.standard': ('AmazonS3', 'GB-Mo', 'TimedStorage-ByteHrs'),
    's3.storage.standard_ia': ('AmazonS3', 'GB-Mo', 'TimedStorage-SIA-ByteHrs'),
    's3.storage.glacier_ir': ('AmazonS3', 'GB-Mo', 'TimedStorage-GIR-ByteHrs'),
    's3.storage.glacier': ('AmazonS3', 'GB-Mo', 'TimedStorage-GlacierByteHrs'),
    's3.put_standard': ('AmazonS3', 'Requests', 'Requests-Tier1'),
    's3.get_standard': ('AmazonS3', 'Requests', 'Requests-Tier2'),
    's3.retrieval_ia': ('AmazonS3', 'GB', 'Retrieval-SIA'),
    's3.retrieval_ir': ('AmazonS3', 'GB', 'Retrieval-GIR'),
    's3.restore_standard_gb': ('AmazonS3', 'GB', 'Standard-Retrieval-Bytes'),
    's3.put_ia': ('AmazonS3', 'Requests', 'Requests-SIA-Tier1'),
    's3.put_ir': ('AmazonS3', 'Requests', 'Requests-GIR-Tier1'),
    's3.put_glacier': ('AmazonS3', 'Requests', 'Requests-GLACIER-Tier1'),
    's3.get_ia': ('AmazonS3', 'Requests', 'Requests-SIA-Tier2'),
    's3.get_ir': ('AmazonS3', 'Requests', 'Requests-GIR-Tier2'),
    's3.restore_standard_requests': ('AmazonS3', 'Requests', 'Requests-Tier3'),
    's3.restore_bulk_requests': ('AmazonS3', 'Requests', 'Requests-Tier5'),
    's3.restore_bulk_gb': ('AmazonS3', 'GB', 'Bulk-Retrieval-Bytes'),
    's3.transition_ir': ('AmazonS3', 'Requests', 'Requests-Tier4'),
    's3.transition_glacier': ('AmazonS3', 'Requests', 'Requests-Tier3'),
    'ddb.storage': ('AmazonDynamoDB', 'GB-Mo', 'TimedStorage-ByteHrs'),
    'ddb.pitr': ('AmazonDynamoDB', 'GB-Mo', 'TimedPITRStorage-ByteHrs'),
    'ddb.backup': ('AmazonDynamoDB', 'GB-Month', 'TimedBackupStorage-ByteHrs'),
    'ddb.write': ('AmazonDynamoDB', 'WriteRequestUnits', 'WriteRequestUnits'),
    'ddb.read': ('AmazonDynamoDB', 'ReadRequestUnits', 'ReadRequestUnits'),
    'network.nat_hour': ('AmazonEC2', 'Hrs', 'NatGateway-Hours'),
    'network.nat_gb': ('AmazonEC2', 'GB', 'NatGateway-Bytes'),
    'network.endpoint_hour': ('AmazonVPC', 'Hrs', 'VpcEndpoint-Hours'),
    'network.public_ipv4': ('AmazonVPC', 'Hrs', 'PublicIPv4:InUseAddress'),
    'alb.hour': ('AWSELB', 'Hrs', 'LoadBalancerUsage'),
    'alb.lcu': ('AWSELB', 'LCU-Hrs', 'LCUUsage'),
}


def validate_gross_storage(rate, sources):
    adjustment = rate.get('gross_adjustment')
    if rate['id'] != 'ddb.storage' or rate['status'] != 'verified':
        if adjustment is not None:
            raise ValueError('Gross adjustment is only supported for verified ddb.storage')
        return
    if adjustment is None:
        raise ValueError('ddb.storage requires Free Tier provenance for the gross model')
    allowance = adjustment['allowance']
    if allowance['source_id'] not in sources:
        raise ValueError('Unknown allowance source')
    free, paid = adjustment['raw_tiers']
    quantity = Decimal(allowance['quantity'])
    if (quantity <= 0 or Decimal(free['begin']) != 0 or free['end'] is None
            or Decimal(free['end']) != quantity or Decimal(free['usd']) != 0
            or Decimal(paid['begin']) != quantity or paid['end'] is not None
            or Decimal(paid['usd']) <= 0):
        raise ValueError('Invalid raw Free Tier tariff or allowance quantity')
    if (len(rate['tiers']) != 1 or Decimal(rate['tiers'][0]['begin']) != 0
            or rate['tiers'][0]['end'] is not None
            or Decimal(rate['tiers'][0]['usd']) != Decimal(paid['usd'])):
        raise ValueError('Gross storage must use the paid rate from quantity zero')


def validate(document, required_rates=()):
    schema = json.loads(SCHEMA.read_text())
    Draft202012Validator.check_schema(schema)
    Draft202012Validator(schema, format_checker=FormatChecker()).validate(document)
    if document['region'] not in REGIONS:
        raise ValueError('Unsupported pricing Region; add a verified qualifier mapping first')
    location, usage_prefix = REGIONS[document['region']]
    sources = document['sources']
    for source in sources.values():
        if date.fromisoformat(source['retrieved_at']) > date.fromisoformat(document['as_of']):
            raise ValueError('A source retrieval date is after the catalog as_of date')
    classes = [r['class'] for r in document['storage_rules']]
    if len(set(classes)) != len(classes) or set(classes) != {'STANDARD', 'STANDARD_IA', 'GLACIER_IR', 'GLACIER'}:
        raise ValueError('Storage rules must cover the four required classes exactly once')
    for rule in document['storage_rules']:
        if rule['source_id'] not in sources:
            raise ValueError('Storage rule has an unknown source')
    rates = {}
    for rate in document['rates']:
        if rate['id'] in rates:
            raise ValueError('Duplicate rate ID: ' + rate['id'])
        rates[rate['id']] = rate
        if rate['source_id'] not in sources:
            raise ValueError('Unknown pricing source: ' + rate['source_id'])
        validate_gross_storage(rate, sources)
        if rate['status'] == 'verified':
            expected = API_RATES.get(rate['id'])
            if expected is not None and (rate['service'], rate['unit']) != expected[:2]:
                raise ValueError('Quote service/unit does not match the rate ID')
            if sources[rate['source_id']]['kind'] == 'aws_price_api':
                dimensions = rate['dimensions']
                if not all(dimensions.get(k) for k in ('sku', 'regionCode', 'location', 'usagetype')):
                    raise ValueError('API quote is missing identifying dimensions')
                if dimensions['regionCode'] != document['region'] or dimensions['location'] != location:
                    raise ValueError('API quote Region/location does not match the catalog')
                if expected is None:
                    raise ValueError('Unknown API rate ID; register its service/unit/usage mapping first')
                _, _, usage = expected
                if dimensions['usagetype'] != usage_prefix + usage:
                    raise ValueError('API quote usagetype does not match Region/rate ID')
            previous_end = Decimal(0)
            for tier in rate['tiers']:
                begin = Decimal(tier['begin'])
                end = None if tier['end'] is None else Decimal(tier['end'])
                if previous_end is None or begin != previous_end or (end is not None and end <= begin):
                    raise ValueError('Tiers must start at zero, be contiguous and non-empty')
                previous_end = end
            if previous_end is not None:
                raise ValueError('Last pricing tier must have an open end')
    for key in required_rates:
        if key not in rates or rates[key]['status'] != 'verified':
            raise ValueError('Missing verified price: ' + key)
    return rates


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--file', type=Path, default=EXAMPLE)
    parser.add_argument('--require-rate', action='append', default=[])
    args = parser.parse_args()
    try:
        rates = validate(json.loads(args.file.read_text()), args.require_rate)
    except Exception as error:
        parser.exit(1, f'Pricing validation failed: {error}\n')
    verified = sum(r['status'] == 'verified' for r in rates.values())
    print(f'Contract valid: {len(rates)} rates, {verified} verified, {len(rates)-verified} unpriced. No cost/benchmark calculated.')


if __name__ == '__main__':
    main()
