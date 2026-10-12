#!/usr/bin/env python3
"""Regression of the H1 denied-request static wiring gate, not runtime proof."""
import unittest
from check_security_order import H1, check_h1_denial_escape, self_test_h1_denial_escape


def source(denial="pfe->ai_gw_denied = 0; return -1;", trailing="setup_proxy_path();"):
    return "\nhandle_client_data(void) { if (valid) { forward_valid(); } else if (pfe->ai_gw_denied) {" + denial + "} else { recover(); }" + trailing + "}"


class DenialOrderTests(unittest.TestCase):
    def test_actual_source_and_negative_mutations(self):
        text = H1.read_text(encoding="utf-8")
        check_h1_denial_escape(text)
        self_test_h1_denial_escape(text)

    def test_padding_and_braces_in_literals_are_not_structure(self):
        text = source() + '\n/* } fake call setup_proxy_path() */'
        text = text.replace("recover();", 'log_info("{not a brace}");' + "/* padding */" * 1000 + "recover();")
        check_h1_denial_escape(text)

    def test_nested_return_is_not_terminal(self):
        for denial in ["if (condition) { return -1; }", "if (condition) return -1;"]:
            with self.subTest(denial=denial), self.assertRaises(AssertionError):
                check_h1_denial_escape(source(denial))

    def test_comment_or_string_cannot_supply_return(self):
        for denial in ['/* return -1; */', 'log_info("return -1;");']:
            with self.subTest(denial=denial), self.assertRaises(AssertionError):
                check_h1_denial_escape(source(denial))

    def test_comment_or_string_cannot_supply_dispatch(self):
        for trailing in ['/* setup_proxy_path(); */', 'log_info("setup_proxy_path();");']:
            with self.subTest(trailing=trailing), self.assertRaises(AssertionError):
                check_h1_denial_escape(source(trailing=trailing))

    def test_both_dispatch_paths_are_forbidden_on_denial(self):
        for call in ["setup_proxy_path();", "pd_setup_and_forward();"]:
            with self.subTest(call=call), self.assertRaises(AssertionError):
                check_h1_denial_escape(source(call + "return -1;"))


if __name__ == "__main__":
    unittest.main()
