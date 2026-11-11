"""Read native CloudWatch alarm histories and matching metric samples."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
from scripts.capture_runtime_inventory import aws_json
from scripts.verify_alarm_evidence import CONTRACT, timestamp, validate


def capture(profile, region, prefix, installation, start, end, fetch=aws_json):
    if timestamp(start) >= timestamp(end):
        raise ValueError("invalid evidence window")
    document = {"installation": installation, "start": start, "end": end, "alarms": {}}
    for node_profile in ("cloud", "edge"):
        for kind, (metric, _, statistic, _) in CONTRACT.items():
            name = f"{prefix}-{node_profile}-{kind}"
            response = fetch(profile, region, "cloudwatch", "describe-alarms", "--alarm-names", name)
            alarms = response.get("MetricAlarms")
            if not isinstance(alarms, list) or len(alarms) != 1 or alarms[0].get("AlarmName") != name:
                raise ValueError(f"alarm missing or ambiguous: {name}")
            history = fetch(profile, region, "cloudwatch", "describe-alarm-history", "--alarm-name", name, "--history-item-type", "StateUpdate", "--start-date", start, "--end-date", end).get("AlarmHistoryItems")
            if not isinstance(history, list):
                raise ValueError(f"alarm history missing: {name}")
            dims = {"profile": node_profile, "installation": installation}
            metric_spec = {"Namespace": "Miles/DSG", "MetricName": metric, "Dimensions": [{"Name": key, "Value": value} for key, value in dims.items()]}
            query = [{"Id": "metric", "MetricStat": {"Metric": metric_spec, "Period": 60, "Stat": statistic}, "ReturnData": True}]
            result = fetch(profile, region, "cloudwatch", "get-metric-data", "--metric-data-queries", json.dumps(query), "--start-time", start, "--end-time", end).get("MetricDataResults")
            if not isinstance(result, list) or len(result) != 1 or result[0].get("Id") != "metric":
                raise ValueError(f"metric result missing: {name}")
            document["alarms"][f"{node_profile}/{kind}"] = {"alarm": alarms[0], "history": history, "metric_request": {"MetricName": metric, "Namespace": "Miles/DSG", "Dimensions": dims, "Period": 60, "Stat": statistic}, "metric_result": result[0]}
    return document


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for option in ("profile", "region", "account-id", "prefix", "installation", "start", "end", "output"):
        parser.add_argument("--" + option, required=True)
    args = parser.parse_args()
    if len(args.account_id) != 12 or not args.account_id.isdigit():
        parser.error("12-digit account ID required")
    if aws_json(args.profile, args.region, "sts", "get-caller-identity").get("Account") != args.account_id:
        raise ValueError("AWS account mismatch")
    document = capture(args.profile, args.region, args.prefix, args.installation, args.start, args.end)
    payload = (json.dumps(document, indent=2, sort_keys=True) + "\n").encode()
    fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as output:
        output.write(payload)
        output.flush()
        os.fsync(output.fileno())
    print(hashlib.sha256(payload).hexdigest())
    validate(document)


if __name__ == "__main__":
    main()
