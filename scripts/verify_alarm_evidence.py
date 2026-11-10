"""Validate captured native alarm transitions and corresponding metric samples."""

import argparse
import datetime
import json
import math
from pathlib import Path


CONTRACT = {
    "recovery_cut_age": ("miles_recovery_cut_age_seconds", 3600, "Maximum", "breaching"),
    "integrity": ("miles_warm_read_integrity_failures_total", 0, "Sum", "notBreaching"),
    "throttling": ("miles_ddb_throttles_total", 0, "Sum", "notBreaching"),
    "telemetry_age": ("miles_runtime_observation_age_seconds", 60, "Maximum", "breaching"),
}


def timestamp(value):
    if not isinstance(value, str):
        raise ValueError("missing timestamp")
    parsed = datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        raise ValueError("timestamp lacks timezone")
    return parsed.astimezone(datetime.timezone.utc)


def validate_alarm(item, profile, kind, installation, start, end):
    name, threshold, statistic, missing = CONTRACT[kind]
    alarm = item["alarm"]
    dims = {d["Name"]: d["Value"] for d in alarm["Dimensions"]}
    if len(alarm["Dimensions"]) != 2 or (alarm["MetricName"], alarm["Namespace"], alarm["Threshold"], alarm["Period"], alarm["EvaluationPeriods"], alarm["DatapointsToAlarm"], alarm["Statistic"], alarm["TreatMissingData"], alarm["ComparisonOperator"], dims) != (name, "Miles/DSG", threshold, 60, 3, 1, statistic, missing, "GreaterThanThreshold", {"profile": profile, "installation": installation}):
        raise ValueError("alarm definition mismatch")
    if alarm.get("StateValue") != "OK":
        raise ValueError("alarm has not returned to OK")
    events = []
    for event in item["history"]:
        if event.get("AlarmName") != alarm["AlarmName"]:
            raise ValueError("history belongs to a different alarm")
        details = json.loads(event["HistoryData"])
        old, new = details.get("oldState", {}).get("stateValue"), details.get("newState", {}).get("stateValue")
        event_time = timestamp(event["Timestamp"])
        if not start <= event_time <= end:
            raise ValueError("alarm history outside requested window")
        events.append((event_time, old, new))
    events.sort()
    alarms = [t for t, old, new in events if old == "OK" and new == "ALARM"]
    recoveries = [t for t, old, new in events if old == "ALARM" and new == "OK"]
    if not any(alarm_time < recovery for alarm_time in alarms for recovery in recoveries):
        raise ValueError("missing OK to ALARM to OK history")
    request, series = item["metric_request"], item["metric_result"]
    if request.get("MetricName") != name or request.get("Namespace") != alarm["Namespace"] or request.get("Dimensions") != dims or request.get("Period") != 60 or request.get("Stat") != statistic:
        raise ValueError("metric request does not match alarm")
    if series.get("StatusCode") != "Complete" or series.get("Messages"):
        raise ValueError("metric data incomplete")
    times, values = series["Timestamps"], series["Values"]
    if not isinstance(times, list) or not isinstance(values, list) or len(times) != len(values) or not values:
        raise ValueError("invalid metric data")
    samples = [(timestamp(t), float(v)) for t, v in zip(times, values)]
    if any(not start <= t <= end or not math.isfinite(value) or value < 0 for t, value in samples) or len({t for t, _ in samples}) != len(samples):
        raise ValueError("invalid metric sample")
    pairs = [(at, recovery) for at in alarms for recovery in recoveries if at < recovery and any(at - datetime.timedelta(seconds=240) <= t <= at and value > threshold for t, value in samples)]
    if not pairs:
        raise ValueError("no breaching metric in the alarm window")
    if kind in ("integrity", "throttling"):
        # Zero counter deltas are intentionally not exported; notBreaching
        # recovery may therefore have no datapoints instead of a fake zero.
        pairs = [(at, recovery) for at, recovery in pairs if (recovery - at).total_seconds() >= 120 and not any(at < t <= recovery and value > threshold for t, value in samples)]
    else:
        pairs = [(at, recovery) for at, recovery in pairs if any(at <= t <= recovery and value <= threshold for t, value in samples)]
    if not pairs:
        raise ValueError("metric samples do not cover the recovery window")
    return pairs


def validate(document):
    installation = document["installation"]
    if not isinstance(installation, str) or not installation:
        raise ValueError("missing installation")
    start, end = timestamp(document["start"]), timestamp(document["end"])
    if start >= end:
        raise ValueError("invalid evidence window")
    cases = document["alarms"]
    if set(cases) != {f"{profile}/{kind}" for profile in ("cloud", "edge") for kind in CONTRACT}:
        raise ValueError("expected exactly eight alarms")
    transitions = {}
    for key, item in cases.items():
        profile, kind = key.split("/", 1)
        transitions[key] = validate_alarm(item, profile, kind, installation, start, end)
    for profile in ("cloud", "edge"):
        telemetry = cases[f"{profile}/telemetry_age"]["metric_result"]
        healthy = [(timestamp(t), float(value)) for t, value in zip(telemetry["Timestamps"], telemetry["Values"]) if float(value) <= 60]
        for kind in ("integrity", "throttling"):
            if not any(any(alarm_time <= t <= recovery and (recovery - t).total_seconds() <= 120 for t, _ in healthy) for alarm_time, recovery in transitions[f"{profile}/{kind}"]):
                raise ValueError("counter recovery lacks fresh runtime telemetry")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("evidence", type=Path, help="Private captured alarm definitions, histories, requests and metric results")
    args = parser.parse_args()
    try:
        validate(json.loads(args.evidence.read_text(encoding="utf-8")))
    except (KeyError, TypeError, ValueError, json.JSONDecodeError) as exc:
        parser.exit(1, f"UNVERIFIED: {exc}\n")
    print("PASS: eight alarm histories and metric samples match the declared contract; independently verify metric provenance")


if __name__ == "__main__":
    main()
