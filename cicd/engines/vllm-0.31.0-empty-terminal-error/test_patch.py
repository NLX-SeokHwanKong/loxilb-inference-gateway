#!/usr/bin/env python3
"""Check apply_patch.py against a stock vLLM 0.31.0 serving.py.

Usage: python3 test_patch.py <path to the v0.31.0 serving.py>
       (git -C <vllm checkout> show v0.31.0:vllm/entrypoints/openai/chat_completion/serving.py)
Needs the `patch` tool to compare against upstream-40986.patch.
"""
import json
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from apply_patch import HERE, transform

SOURCE_INFO = json.loads((HERE / "SOURCE.json").read_text())
ORIGINAL = b""


def stream_order(source: bytes):
    """Line numbers of the empty-chunk skip and the first error check."""
    lines = source.decode().splitlines()
    skip = next(i for i, l in enumerate(lines) if "Chunked prefill case, don't return empty chunks" in l)
    check = next(i for i, l in enumerate(lines) if "self._raise_if_error(output.finish_reason, request_id)" in l)
    return skip, check


class PatchTests(unittest.TestCase):
    def test_matches_upstream_patch(self):
        if shutil.which("patch") is None:
            self.skipTest("patch tool not installed")
        with tempfile.TemporaryDirectory() as tmp:
            target = Path(tmp) / SOURCE_INFO["path"]
            target.parent.mkdir(parents=True)
            target.write_bytes(ORIGINAL)
            subprocess.run(["patch", "-s", "-p1", "-i", str(HERE / "upstream-40986.patch")],
                           cwd=tmp, check=True)
            self.assertEqual(target.read_bytes(), transform(ORIGINAL, SOURCE_INFO))

    def test_stock_file_skips_before_the_error_check(self):
        skip, check = stream_order(ORIGINAL)
        self.assertLess(skip, check, "stock 0.31.0 no longer shows the defect")

    def test_patched_file_checks_errors_first(self):
        skip, check = stream_order(transform(ORIGINAL, SOURCE_INFO))
        self.assertLess(check, skip)

    def test_refuses_other_files(self):
        patched = transform(ORIGINAL, SOURCE_INFO)
        with self.assertRaises(ValueError):
            transform(patched, SOURCE_INFO)
        with self.assertRaises(ValueError):
            transform(ORIGINAL + b"\n", SOURCE_INFO)


if __name__ == "__main__":
    ORIGINAL = Path(sys.argv.pop(1)).read_bytes()
    unittest.main(verbosity=2)
