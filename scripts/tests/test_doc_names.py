"""Regression checks for the documentation filename policy."""

import importlib.util
from pathlib import Path
import unittest


spec = importlib.util.spec_from_file_location(
    "check_doc_names", Path(__file__).resolve().parents[1] / "check_doc_names.py"
)
check_doc_names = importlib.util.module_from_spec(spec)
spec.loader.exec_module(check_doc_names)


class DocumentNamesTest(unittest.TestCase):
    def test_subject_names(self):
        paths = ["docs/contracts/pack-format-freeze.md", "docs/contracts/r2-recovery.md"]
        self.assertEqual(check_doc_names.invalid_names(paths), [])

    def test_tracking_identifiers(self):
        for name in ("w1-plan.md", "w2-plan.md", "w02-hai-02-format-freeze.md",
                     "plan-w03.md", "sprint-2-plan.md", "task-123-plan.md", "W02-plan.md"):
            path = "docs/contracts/" + name
            with self.subTest(name=name):
                self.assertEqual(check_doc_names.invalid_names([path]), [path])

    def test_scope_and_case(self):
        self.assertEqual(check_doc_names.invalid_names(["internal/w02_test.go"]), [])
        self.assertEqual(check_doc_names.invalid_names(["docs/Pack_Format.md"]), ["docs/Pack_Format.md"])
