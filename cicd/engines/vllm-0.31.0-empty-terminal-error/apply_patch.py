#!/usr/bin/env python3
"""Backport vLLM #40986 into the installed vLLM 0.31.0 chat serving module.

The edit is keyed on the sha256 of the installed file, not on the version
string: an unreviewed file fails the build instead of being patched.
"""
import hashlib
import json
import py_compile
import sys
from importlib.metadata import distribution
from pathlib import Path

HERE = Path(__file__).resolve().parent

# The two hunks of upstream-40986.patch, as exact text.
RAISE_EARLY_OLD = """\
                    if finish_reason_sent[i]:
                        continue

                    if request.logprobs and (
"""
RAISE_EARLY_NEW = """\
                    if finish_reason_sent[i]:
                        continue

                    self._raise_if_error(output.finish_reason, request_id)

                    if request.logprobs and (
"""
RAISE_LATE_OLD = """\
                    else:
                        # check for error finish reason and abort streaming
                        # finish_reason='error' indicates a retryable error
                        self._raise_if_error(output.finish_reason, request_id)

                        # Send the finish response for each request.n only once
"""
RAISE_LATE_NEW = """\
                    else:
                        # Send the finish response for each request.n only once
"""


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def transform(source: bytes, source_info: dict) -> bytes:
    """Return the patched module; raise ValueError on any other input."""
    if sha256(source) != source_info["original_sha256"]:
        raise ValueError("serving.py is not the reviewed vLLM 0.31.0 file")
    text = source.decode()
    for old, new in ((RAISE_EARLY_OLD, RAISE_EARLY_NEW), (RAISE_LATE_OLD, RAISE_LATE_NEW)):
        if text.count(old) != 1:
            raise ValueError("hunk context is not unique")
        text = text.replace(old, new, 1)
    result = text.encode()
    if sha256(result) != source_info["patched_sha256"]:
        raise ValueError("patched serving.py does not match the reviewed result")
    return result


def main() -> int:
    source_info = json.loads((HERE / "SOURCE.json").read_text())
    vllm = distribution("vllm")
    target = Path(vllm.locate_file(source_info["path"]))
    target.write_bytes(transform(target.read_bytes(), source_info))
    py_compile.compile(str(target), doraise=True)
    print(f"patched {target} (vllm {vllm.version}): {source_info['patched_sha256']}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
