import copy
import json
import unittest

from scripts.verify_alarm_evidence import CONTRACT, validate
from scripts.capture_alarm_evidence import capture


class AlarmEvidenceTests(unittest.TestCase):
    def fixture(self):
        document = {"installation": "miles-dev", "start": "2026-11-10T00:00:00Z", "end": "2026-11-10T00:06:00Z", "alarms": {}}
        for profile in ("cloud", "edge"):
            for kind, (metric, threshold, statistic, missing) in CONTRACT.items():
                dims = {"profile": profile, "installation": "miles-dev"}
                name = f"miles-dev-{profile}-{kind}"
                document["alarms"][f"{profile}/{kind}"] = {
                    "alarm": {"AlarmName": name, "MetricName": metric, "Namespace": "Miles/DSG", "Threshold": threshold, "Period": 60, "EvaluationPeriods": 3, "DatapointsToAlarm": 1, "Statistic": statistic, "TreatMissingData": missing, "ComparisonOperator": "GreaterThanThreshold", "StateValue": "OK", "Dimensions": [{"Name": k, "Value": v} for k, v in dims.items()]},
                    "history": [{"AlarmName": name, "Timestamp": "2026-11-10T00:01:00Z", "HistoryData": json.dumps({"oldState": {"stateValue": "OK"}, "newState": {"stateValue": "ALARM"}})}, {"AlarmName": name, "Timestamp": "2026-11-10T00:03:00Z", "HistoryData": json.dumps({"oldState": {"stateValue": "ALARM"}, "newState": {"stateValue": "OK"}})}],
                    "metric_request": {"MetricName": metric, "Namespace": "Miles/DSG", "Dimensions": dims, "Period": 60, "Stat": statistic},
                    "metric_result": {"StatusCode": "Complete", "Timestamps": ["2026-11-10T00:00:00Z", "2026-11-10T00:02:00Z"], "Values": [threshold + 1, 0]},
                }
        return document

    def test_complete_eight_alarm_matrix(self):
        validate(self.fixture())

    def test_missing_wrong_identity_and_incomplete_metric_are_rejected(self):
        original = self.fixture()
        missing = copy.deepcopy(original)
        del missing["alarms"]["edge/integrity"]
        wrong = copy.deepcopy(original)
        wrong["alarms"]["edge/integrity"]["history"][0]["AlarmName"] = "other"
        incomplete = copy.deepcopy(original)
        incomplete["alarms"]["edge/integrity"]["metric_result"]["StatusCode"] = "PartialData"
        for case in (missing, wrong, incomplete):
            with self.subTest(case=case), self.assertRaises(ValueError):
                validate(case)

    def test_samples_outside_alarm_window_are_rejected(self):
        document = self.fixture()
        document["alarms"]["cloud/integrity"]["metric_result"]["Timestamps"] = ["2026-11-10T00:04:00Z", "2026-11-10T00:05:00Z"]
        with self.assertRaises(ValueError):
            validate(document)

    def test_counter_recovery_uses_missing_data_and_live_telemetry(self):
        document = self.fixture()
        for key in ("cloud/integrity", "edge/throttling"):
            document["alarms"][key]["metric_result"]["Timestamps"] = ["2026-11-10T00:00:00Z"]
            document["alarms"][key]["metric_result"]["Values"] = [1]
        validate(document)
        document["alarms"]["edge/telemetry_age"]["metric_result"]["Timestamps"] = ["2026-11-10T00:00:00Z", "2026-11-10T00:00:30Z"]
        with self.assertRaises(ValueError):
            validate(document)

    def test_counter_can_keep_breaching_before_a_quiet_recovery_window(self):
        document = self.fixture()
        counter = document["alarms"]["edge/throttling"]
        recovery = counter["history"][1]
        recovery["Timestamp"] = "2026-11-10T00:05:00Z"
        counter["metric_result"]["Timestamps"] = ["2026-11-10T00:00:00Z", "2026-11-10T00:02:00Z"]
        counter["metric_result"]["Values"] = [1, 2]
        telemetry = document["alarms"]["edge/telemetry_age"]["metric_result"]
        telemetry["Timestamps"].append("2026-11-10T00:04:00Z")
        telemetry["Values"].append(0)
        validate(document)
        counter["metric_result"]["Timestamps"].append("2026-11-10T00:04:00Z")
        counter["metric_result"]["Values"].append(1)
        with self.assertRaisesRegex(ValueError, "recovery window"):
            validate(document)

    def test_native_capture_collects_eight_matched_alarms(self):
        fixture = self.fixture()
        by_name = {item["alarm"]["AlarmName"]: item for item in fixture["alarms"].values()}
        current = [None]
        def fetch(profile, region, service, operation, *args):
            if operation == "describe-alarms":
                current[0] = by_name[args[1]]
                return {"MetricAlarms": [current[0]["alarm"]]}
            if operation == "describe-alarm-history":
                return {"AlarmHistoryItems": current[0]["history"]}
            if operation == "get-metric-data":
                return {"MetricDataResults": [{"Id": "metric", **current[0]["metric_result"]}]}
            self.fail(f"unexpected operation {operation}")
        captured = capture("profile", "region", "miles-dev", "miles-dev", fixture["start"], fixture["end"], fetch)
        validate(captured)


if __name__ == "__main__":
    unittest.main()
