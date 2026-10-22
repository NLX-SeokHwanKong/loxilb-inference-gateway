# vLLM 0.31.0 with the upstream "streaming errors before the first token" fix

A test-bed engine image, not a customer engine. Production uses the stock
vLLM image; this recipe exists to reproduce and test the gateway against a
0.31.0 engine that reports a terminal error correctly.

## The defect

In vLLM 0.31.0 a chat stream whose first output is empty and already terminal
with `finish_reason="error"` is dropped by the empty-chunk filter before the
error check runs. The client gets a normal end of stream (usage, `[DONE]`)
instead of an error, so the gateway sees a success with no content.

Upstream fixed it in [vllm-project/vllm#40986](https://github.com/vllm-project/vllm/pull/40986)
(commit `b0eb87fe`): the error check moves ahead of the filter. No release
tag contains it yet.

## What the recipe does

`upstream-40986.patch` is the upstream change to
`vllm/entrypoints/openai/chat_completion/serving.py`, unmodified; it applies
cleanly to v0.31.0. `apply_patch.py` makes the same two edits at image build
time, only to a file whose sha256 is the reviewed v0.31.0 file in
`SOURCE.json`, and checks the result's sha256. Any other file, including an
already patched one, fails the build.

```sh
docker build --pull=false -t vllm-0.31.0-upstream-40986 .
```

Record the resulting image ID with any run that uses it.

## Checking the recipe

```sh
git -C <vllm checkout> show v0.31.0:vllm/entrypoints/openai/chat_completion/serving.py > /tmp/serving-0.31.0.py
python3 test_patch.py /tmp/serving-0.31.0.py
```

The test proves that the build-time edits equal the upstream patch, that the
stock file runs the empty-chunk filter before the error check (the defect)
and the patched file the other way round, and that other files are refused.
