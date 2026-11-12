"""Evidence verification regressions, including rehashed but invalid measurements."""
import importlib.util
import json
from pathlib import Path
import shutil
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("dataset_artifacts", ROOT / "scripts/dataset_artifacts.py")
artifacts = importlib.util.module_from_spec(spec)
spec.loader.exec_module(artifacts)
FIXTURE = ROOT / "benchmarks/evidence/engine-metadata-pilot"


class ByteIntegrityTests(unittest.TestCase):
    def test_integrated_bundle_preserves_source_and_accounting(self):
        root = ROOT / "benchmarks/evidence/engine-metadata-integrated"
        summary = artifacts.verify(root)
        self.assertEqual(summary["source_commit"], "290337952751fb40134713deec635cdc87b3d4fe")
        self.assertEqual((summary["verified_gets"], summary["identities"]), (27, 14))
        self.assertEqual(summary["phases"][0]["bytes_logical"], 0)

    def test_crlf_is_not_silently_normalized(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "sample.json").write_bytes(b'{}\n')
            (root / "sha256sums.txt").write_text(artifacts.digest(b'{}\n') + "  sample.json\n")
            artifacts.checksums(root, {"sample.json"})
            (root / "sample.json").write_bytes(b'{}\r\n')
            with self.assertRaisesRegex(ValueError, "byte checksum"):
                artifacts.checksums(root, {"sample.json"})

    def test_parent_checksum_path_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "sha256sums.txt").write_text("a" * 64 + "  ../outside.json\n")
            with self.assertRaisesRegex(ValueError, "invalid checksum"):
                artifacts.checksums(root)


@unittest.skipUnless(FIXTURE.exists(), "published native fixture not present yet")
class MeasuredBundleTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / "bundle"
        shutil.copytree(FIXTURE, self.root)

    def change(self, name, edit):
        path = self.root / name
        value = json.loads(path.read_text())
        edit(value)
        path.write_text(json.dumps(value, indent=2) + "\n")
        # Rehash deliberately: semantic guards must reject an internally
        # inconsistent capture even when the attacker updates byte checksums.
        rows = []
        for line in (self.root / "sha256sums.txt").read_text().splitlines():
            _, member = line.split("  ", 1)
            rows.append(artifacts.digest((self.root / member).read_bytes()) + "  " + member + "\n")
        (self.root / "sha256sums.txt").write_text("".join(rows))

    def test_measured_bundle_and_no_output_overwrite(self):
        summary = artifacts.verify(self.root)
        self.assertEqual(summary["verified_gets"], 27)
        with self.assertRaises(FileExistsError):
            artifacts.publish(self.root, self.root)

    def test_mixed_snapshot_rejected_after_rehash(self):
        self.change("phases/owner-b/bucket-owner-a.json", lambda b: b.update(snapshot_id="another-snapshot"))
        with self.assertRaisesRegex(ValueError, "snapshot/incarnation"):
            artifacts.verify(self.root)

    def test_incomplete_physical_is_not_zero(self):
        self.change("phases/owner-c/global.json", lambda g: g.update(physical_complete=False, bytes_stored=0))
        with self.assertRaisesRegex(ValueError, "incomplete physical"):
            artifacts.verify(self.root)

    def test_wrong_denominator_rejected(self):
        self.change("phases/owner-a/global.json", lambda g: g.update(compression_ratio=g["bytes_unique_stored"] / g["bytes_unique_raw"]))
        with self.assertRaisesRegex(ValueError, "ratio or denominator"):
            artifacts.verify(self.root)

    def test_zero_denominator_must_be_null(self):
        self.change("phases/empty/global.json", lambda g: g.update(dedup_ratio=0))
        with self.assertRaisesRegex(ValueError, "must be null"):
            artifacts.verify(self.root)

    def test_boolean_is_not_a_byte_count(self):
        self.change("phases/empty/global.json", lambda g: g.update(bytes_unique_raw=False))
        with self.assertRaisesRegex(ValueError, "integer"):
            artifacts.verify(self.root)

    def test_bad_get_hash_rejected(self):
        self.change("verified-gets.json", lambda gets: gets[0].update(sha256="0" * 64))
        with self.assertRaisesRegex(ValueError, "GET checksum"):
            artifacts.verify(self.root)

    def test_dirty_source_cannot_be_published(self):
        self.change("run.json", lambda run: run.update(source_dirty=True))
        with self.assertRaisesRegex(ValueError, "dirty-source"):
            artifacts.verify(self.root)

    def test_sensitive_field_not_published(self):
        self.change("phases/owner-a/global.json", lambda g: g.update(secret_access_key="fixture-only-not-a-secret"))
        with self.assertRaisesRegex(ValueError, "sensitive field"):
            artifacts.verify(self.root)

    def test_unexpected_file_rejected(self):
        (self.root / "unexpected.txt").write_text("not a capture artifact")
        with self.assertRaisesRegex(ValueError, "unexpected artifact"):
            artifacts.verify(self.root)


if __name__ == "__main__":
    unittest.main()
