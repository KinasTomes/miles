"""Reject unsafe IAM-plan export paths before loading AWS credentials."""
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "plan-local.py"
REPO = SCRIPT.parents[4]


class PrivatePlanTest(unittest.TestCase):
    def reject(self, path):
        result = subprocess.run(
            ["python3", str(SCRIPT), "--policy-plan-json", str(path)],
            capture_output=True, text=True,
        )
        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertNotIn("AWS credential", result.stderr)

    def test_relative_and_repository_paths(self):
        self.reject("plan.json")
        self.reject(REPO / "plan.json")

    def test_existing_file_and_symlink_are_not_overwritten(self):
        with tempfile.TemporaryDirectory() as directory:
            file = Path(directory) / "plan.json"
            file.write_text("keep")
            link = Path(directory) / "link.json"
            link.symlink_to(file)
            self.reject(file)
            self.reject(link)
            self.assertEqual(file.read_text(), "keep")
