"""Check subject-based filenames for tracked Markdown documentation."""

from pathlib import Path, PurePosixPath
import re
import subprocess
import sys


KEBAB_CASE = re.compile(r"[a-z0-9]+(?:-[a-z0-9]+)*\.md")
TRACKING_CODE = re.compile(r"(?:^|[-_.])(?:w\d+|sprint[-_]?\d+|task[-_]?\d+)(?=[-_.]|$)", re.IGNORECASE)


def invalid_names(paths):
    return [
        path for path in paths
        if path.startswith("docs/") and path.lower().endswith(".md")
        and (not KEBAB_CASE.fullmatch(PurePosixPath(path).name)
             or TRACKING_CODE.search(PurePosixPath(path).name))
    ]


def main():
    root = Path(__file__).resolve().parents[1]
    paths = subprocess.check_output(
        ["git", "ls-files", "-z", "--", "docs"], cwd=root
    ).decode("utf-8").split("\0")
    invalid = invalid_names(paths)
    if invalid:
        print("Use subject-based lowercase kebab-case document names (see CONTRIBUTING.md):", file=sys.stderr)
        for path in invalid:
            print(f"  {path}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
