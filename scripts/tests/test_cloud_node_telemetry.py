"""Pinned-node diagnostics and coverage rejection, using local fixtures only."""

import copy
import datetime as dt
import gzip
import hashlib
import io
import json
from pathlib import Path
import unittest
import tempfile
from unittest import mock

from deploy.acceptance import cloud_telemetry as cloud
from deploy.acceptance.common import ROOT
from scripts import cloud_node_telemetry as node


class CloudTelemetryTests(unittest.TestCase):
    def test_path_and_source_parameters_fail_closed(self):
        args = ["probe", "fixture", "cold-cloud-ir-rep1", "a"*16, "b"*40, "0"]
        self.assertEqual(node.arguments(args).offset, 0)
        for index, value in ((1, "../other"), (2, "$(id)"), (3, "../../elsewhere"),
                             (4, "moving-main"), (5, str(node.MAX_BYTES+1))):
            bad = list(args); bad[index] = value
            with self.subTest(index=index), mock.patch("sys.stderr"), self.assertRaises(SystemExit):
                node.arguments(bad)

    def bundle(self):
        start = dt.datetime(2026, 11, 13, tzinfo=dt.timezone.utc)
        records, metrics = [], []
        for index in range(3):
            when = (start+dt.timedelta(seconds=2*index)).isoformat()
            records.append({"sequence": index+1, "timestamp_utc": when,
                            "os_pressure": {"stat": {"cpu": 1}, "loadavg": {"load1": 0}, "meminfo": {"MemTotal": 1}}})
            metrics.append({"sequence": index+1, "at": when, "text": "miles_requests_total 1"})
        return {"identity": {"instance": "i-fixture", "source": "a"*40, "boot_id": "boot", "pid": "1"},
                "records": records, "metrics": metrics}, (start+dt.timedelta(seconds=1)).isoformat(), (start+dt.timedelta(seconds=3)).isoformat()

    def test_complete_per_node_window_accepts_and_incomplete_or_restarted_rejects(self):
        payload, start, end = self.bundle()
        expected = payload["identity"]
        cloud.verify_bundle(payload, expected, start, end)
        for mutate in (
            lambda v: v["identity"].update(boot_id="restart"),
            lambda v: v["metrics"].pop(),
            lambda v: v["records"].pop(0),
            lambda v: v["metrics"][1].update(sequence=100),
            lambda v: v["metrics"][1].update(text="gateway_unrelated 1"),
            lambda v: v["records"][1]["os_pressure"]["stat"].update(error="unavailable"),
        ):
            v = copy.deepcopy(payload); mutate(v)
            with self.subTest(mutate=mutate), self.assertRaises(ValueError):
                cloud.verify_bundle(v, expected, start, end)
        with self.assertRaisesRegex(ValueError, "cover"):
            cloud.verify_bundle(payload, expected, start, "2026-11-13T00:00:05+00:00")

    def test_document_is_fixed_env_var_command_with_no_shell_parameter(self):
        source = (ROOT/"infra/terraform/cloud/modules/acceptance/telemetry.tf").read_text()
        self.assertIn('document_type   = "Command"', source)
        self.assertIn('interpolationType = "ENV_VAR"', source)
        self.assertIn("$SSM_action", source)
        self.assertNotIn("{{", source)
        self.assertNotIn('commands =', source)
        self.assertIn("cloud_node_telemetry.py", source)

    def test_send_command_is_hash_pinned_and_never_uses_generic_shell_document(self):
        r = mock.Mock()
        r.id = "fixture"; r.meta = {"source": "a"*40}
        r.aws.return_value = {"Command": {"CommandId": "fixed"}}
        receipt = {"slot": "cold-cloud-ir-rep1", "segment": "b"*16,
                   "prefix": "fixture", "document": {"name": "own-document", "hash": "c"*64, "version": "1"}}
        self.assertEqual(cloud.send(r, receipt, "i-fixture", "read", 12000), "fixed")
        argv = r.aws.call_args.args
        self.assertEqual(argv[0:2], ("ssm", "send-command"))
        self.assertIn("--document-hash", argv)
        self.assertIn("c"*64, argv)
        self.assertNotIn("AWS-RunShellScript", argv)
        params = json.loads(argv[argv.index("--parameters")+1])
        self.assertEqual(params["offset"], ["12000"])
        self.assertNotIn("commands", params)
        self.assertTrue(r.write.call_args_list[0].args[0].endswith("-intent.json"))

    def test_iam_only_allows_named_diagnostic_and_tagged_installation(self):
        deploy = json.loads((ROOT/"deploy/acceptance/policies/deploy.json.tmpl").read_text())
        sends = [s for s in deploy["Statement"] if "ssm:SendCommand" in s["Action"]]
        self.assertEqual(len(sends), 2)
        docs = [s for s in sends if any(":document/" in a for a in s["Resource"])]
        self.assertEqual(len(docs), 1)
        self.assertTrue(all(a.endswith("/miles-dev-benchmark-telemetry") for a in docs[0]["Resource"]))
        targets = next(s for s in sends if s not in docs)
        self.assertEqual(targets["Condition"]["StringEquals"]["ssm:resourceTag/Installation"], "miles-dev")
        for name, limit in (("services", 6144), ("deploy", 10240)):
            raw = (ROOT/f"deploy/acceptance/policies/{name}.json.tmpl").read_text()
            self.assertLess(len(json.dumps(json.loads(raw), separators=(",", ":"))), limit)

    def test_node_sampler_bundles_exact_segment_and_never_overwrites_it(self):
        from scripts.edge_host_telemetry import BoundedRingBuffer
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            args = ["run", "fixture", "cold-cloud-ir-rep1", "a"*16, "b"*40, "0"]
            folder = root/"fixture"/(args[2]+"-"+args[3])
            identity = {"instance": "i-fixture", "source": "b"*40, "address": "10.0.0.1:9100"}
            class FixtureRecorder:
                def __init__(self, path, **kw):
                    self.output_file = path
                    self.ring_buffer = BoundedRingBuffer(path)
                    self.chrony_probe = object()
                    self.sequence = 0
                def sample_now(self):
                    self.sequence += 1
                    result = {"sequence": self.sequence, "timestamp_utc": str(self.sequence)}
                    self.ring_buffer.write_record(result)
                    return result
            with mock.patch.object(node, "BASE", root), mock.patch.object(node, "identity", return_value=identity), \
                 mock.patch.object(node, "preflight_check", return_value=(True, [])), \
                 mock.patch.object(node, "HostTelemetryRecorder", FixtureRecorder), \
                 mock.patch.object(node, "fetch", return_value=b"miles_requests_total 1"), \
                 mock.patch.object(node.time, "sleep", side_effect=lambda _: (folder/"stop").touch()), \
                 mock.patch("sys.stdout", new_callable=io.StringIO):
                node.main(args)
                with self.assertRaises(FileExistsError): node.main(args)
            summary = json.loads((folder/"summary.json").read_text())
            compressed = (folder/"samples.json.gz").read_bytes()
            bundle = json.loads(gzip.decompress(compressed))
            self.assertEqual(summary["samples"], 2)
            self.assertEqual(summary["sha256"], hashlib.sha256(compressed).hexdigest())
            self.assertEqual(len(bundle["metrics"]), 2)
            self.assertEqual(bundle["identity"], identity)


if __name__ == "__main__":
    unittest.main()
