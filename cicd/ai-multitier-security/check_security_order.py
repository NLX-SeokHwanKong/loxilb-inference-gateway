#!/usr/bin/env python3
"""Static admission-order gate for the AI Gateway HTTP data planes.

This is deliberately narrow.  It does not claim that a source match proves
runtime behaviour; the C unit and remote runtime probes provide those layers.
Its job is to make three security-sensitive wiring regressions impossible to
hide in a large sockproxy diff:

* HTTP/2 captures X-Api-Key in per-stream state.
* HTTP/2 evaluates the mandatory admission gate before any L7, model, tier, or
  fallback endpoint selection.
* A declared AI credential is removed after optional L7 header mutation and
  before nghttp2 submits the request to a backend.
* HTTP/1 and HTTP/2 header ownership is derived only from api_key_auth. SSE/P/D
  may arm accounting but cannot consume a backend-owned X-Api-Key on an
  undeclared service.

The datapath symbols this gate anchors on are ONE gate and ONE removal
primitive, shared by both protocols:

* ai_gw_admit() is the admission gate. It replaced the H2-only
  ai_security_admit() arm, which consulted the API-key check regardless of what
  the service declared and so let the two protocol paths disagree about one
  service's authentication mode.
* ai_security_h2_upstream_hygiene() is the removal primitive and owns the
  ai_security_should_strip_api_key() predicate. ai_security_filter_h2_headers()
  still exists but is now a thin wrapper that delegates to it, so asserting on
  the wrapper would assert on nothing.

Anchoring on a renamed symbol is how this gate fails OPEN, so the names above
are load-bearing: if the datapath renames either one, update this file rather
than relaxing the assertion.

The HTTP/1 denial escape hatch is retained as a fourth invariant: a callback
denial must leave the read loop before setup_proxy_path() can dispatch it.
"""

from __future__ import annotations

import pathlib
import re
import sys


ROOT = pathlib.Path(__file__).resolve().parents[2]
H2 = ROOT / "loxilb-ebpf/common/sockproxy_h2.c"
H1 = ROOT / "loxilb-ebpf/common/sockproxy_http.c"
SECURITY = ROOT / "loxilb-ebpf/common/sockproxy_ai_security.c"


def function_body(text: str, name: str) -> str:
    match = re.search(rf"\n{name}\s*\([^;]*?\)\s*\{{", text, re.S)
    if not match:
        raise AssertionError(f"function {name} not found")
    start = match.end() - 1
    depth = 0
    for index in range(start, len(text)):
        char = text[index]
        if char == "{":
            depth += 1
        elif char == "}":
            depth -= 1
            if depth == 0:
                return text[start : index + 1]
    raise AssertionError(f"function {name} has no closing brace")


def call_site(body: str, name: str) -> int:
    """Offset of a CALL to `name`, never of an identifier that merely contains it.

    The admission gate's own result type is ai_gw_admit_result_t and its
    declaration precedes the call, so a plain substring search would anchor
    "admission" ~30 bytes early. Every "a selector must not precede admission"
    comparison below is measured against that offset, so an early anchor
    silently widens the window in which a selector could hide.
    """
    match = re.search(rf"\b{re.escape(name)}\s*\(", body)
    return match.start() if match else -1


def ordered(body: str, *needles: str) -> None:
    positions = []
    for needle in needles:
        position = body.find(needle)
        if position < 0:
            raise AssertionError(f"missing required call or token: {needle}")
        positions.append(position)
    if positions != sorted(positions):
        pairs = ", ".join(f"{needle}@{pos}" for needle, pos in zip(needles, positions))
        raise AssertionError(f"security-sensitive operations are out of order: {pairs}")


def code_only(text: str) -> str:
    """Remove comments so prose cannot satisfy or reorder a call invariant."""
    text = re.sub(r"/\*.*?\*/", "", text, flags=re.S)
    return re.sub(r"//[^\n]*", "", text)


def h1_denial_branch(text: str) -> tuple[str, int, int]:
    """Return the whole read function and its denial block's code offsets.

    Ignore comments and quoted literals before balancing braces: neither prose
    nor a JSON/log string can supply a dispatch call or terminating return.
    """
    lexical = re.sub(
        r'"(?:\\.|[^"\\])*"|\'(?:\\.|[^\'\\])*\'|/\*.*?\*/|//[^\n]*',
        lambda match: " " * len(match.group()), text, flags=re.S,
    )
    body = function_body(lexical, "handle_client_data")
    marker = re.search(r"\belse\s+if\s*\(\s*pfe->ai_gw_denied\s*\)\s*\{", body)
    if marker is None:
        raise AssertionError("HTTP/1 denial marker branch is missing")
    begin = marker.end()
    depth = 1
    for end in range(begin, len(body)):
        depth += (body[end] == "{") - (body[end] == "}")
        if depth == 0:
            return body, begin, end
    raise AssertionError("HTTP/1 denial branch has no closing brace")


def check_h1_denial_escape(text: str) -> None:
    body, begin, end = h1_denial_branch(text)
    denial = body[begin:end]
    if call_site(denial, "setup_proxy_path") >= 0 or call_site(denial, "pd_setup_and_forward") >= 0:
        raise AssertionError("HTTP/1 denial branch dispatches a refused request")
    if not re.search(r"(?:^|[;}])\s*return\s+-1\s*;\s*$", denial):
        raise AssertionError("HTTP/1 denial branch must unconditionally terminate with return -1")
    if call_site(body[end + 1:], "setup_proxy_path") < 0:
        raise AssertionError("HTTP/1 compatibility dispatch after the denial branch is missing")


def self_test_h1_denial_escape(text: str) -> None:
    """Actual-source mutations keep this static gate from silently weakening."""
    body, begin, end = h1_denial_branch(text)
    wrapper = "\nhandle_client_data(void)"
    # The malformed-HTTP compatibility branch may grow without changing the
    # security decision. No fixed character window is part of the contract.
    padded = wrapper + body[:end + 1] + ("\n/* harmless padding */\n" * 1000) + body[end + 1:]
    check_h1_denial_escape(padded)
    without_return = re.sub(r"\breturn\s+-1\s*;\s*$", "", body[begin:end])
    mutants = [
        wrapper + body[:begin] + without_return + body[end:],
        wrapper + body[:begin] + "setup_proxy_path();" + body[begin:],
        wrapper + body[:begin] + "pd_setup_and_forward();" + body[begin:],
    ]
    for mutant in mutants:
        try:
            check_h1_denial_escape(mutant)
        except AssertionError:
            continue
        raise AssertionError("HTTP/1 denial checker accepted a security-breaking source mutation")


def main() -> int:
    h2 = H2.read_text(encoding="utf-8")
    h1 = H1.read_text(encoding="utf-8")
    security = SECURITY.read_text(encoding="utf-8")

    header = code_only(function_body(h2, "proxy_h2_on_header_callback"))
    if 'HEADER_MATCHES("x-api-key")' not in header:
        raise AssertionError("HTTP/2 does not capture x-api-key into stream state")
    if "ai_security_copy_api_key" not in header:
        raise AssertionError("HTTP/2 x-api-key capture does not use the bounded helper")

    forward = code_only(function_body(h2, "proxy_h2_forward_to_backend"))
    admission = call_site(forward, "ai_gw_admit")
    if admission < 0:
        raise AssertionError("HTTP/2 forward path has no AI security admission gate")
    for call in re.findall(r"log_[a-z]+\s*\(.*?\);", forward, flags=re.S):
        if "x_api_key_raw" in call and "x_api_key_raw[0]" not in call:
            raise AssertionError("HTTP/2 logs credential bytes instead of presence only")
    selectors = {
        token: forward.find(token)
        for token in ("l7_route_dispatch", "find_endpoint_lpm", "chwbl_ring_lookup")
        if forward.find(token) >= 0
    }
    late = {token: pos for token, pos in selectors.items() if pos < admission}
    if late:
        raise AssertionError(f"HTTP/2 selector(s) execute before admission: {late}")

    ordered(
        forward,
        "proxy_h2_build_l7_req_headers",
        "ai_security_h2_upstream_hygiene",
        "nghttp2_submit_request",
    )

    strip_h2 = code_only(function_body(security, "ai_security_h2_upstream_hygiene"))
    if "ai_security_should_strip_api_key(policy)" not in strip_h2:
        raise AssertionError("HTTP/2 header filter bypasses the shared policy-ownership predicate")
    if "ai_gw_mode" in strip_h2:
        raise AssertionError("HTTP/2 header ownership still depends on ai_gw_mode")

    strip_h1 = code_only(function_body(h1, "ai_strip_upstream_api_key"))
    if "ai_security_should_strip_api_key(node->val.ephash->apikey_auth)" not in strip_h1:
        raise AssertionError("HTTP/1 header strip bypasses the shared policy-ownership predicate")
    if "ai_gw_mode" in strip_h1:
        raise AssertionError("HTTP/1 header ownership still depends on ai_gw_mode")

    if "security_ai_mode" in forward:
        raise AssertionError("HTTP/2 forwarding still keys credential stripping on ai_gw_mode")

    # The H1 parser callback marks ai_gw_denied; the read loop must consume the
    # marker and return before its malformed-HTTP compatibility dispatch.
    check_h1_denial_escape(h1)
    self_test_h1_denial_escape(h1)

    print("PASS: mandatory admission order and policy-owned H1/H2 credential stripping")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except AssertionError as exc:
        print(f"FAIL: {exc}", file=sys.stderr)
        raise SystemExit(1)
