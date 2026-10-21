import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import proof


def good_report(target, source):
    checks = []
    for name, code in proof.CHECKS.items():
        out, err = proof.expected(name, target)
        checks.append(dict(name=name, exit=code, stdout=out, stderr=err, passed=True,
                           byte_jsonl_valid=True, marker_valid=True))
    return dict(schema=1, mode="native", target=target, source=source, go_version=proof.GO_VERSION,
                cgo_enabled="0", errors=[], build=dict(passed=True, exit=0, sha256="a" * 64),
                checks=checks, native_passed=True)


class EvidenceTests(unittest.TestCase):
    def setUp(self):
        self.source = proof.identity("a" * 40)
        self.reports = [good_report(t, self.source) for t in proof.TARGETS]

    def test_all_exact_native_targets_pass(self):
        self.assertTrue(proof.aggregate(self.reports, self.source)["passed"])

    def test_missing_duplicate_unknown_or_failed_target_rejected(self):
        variants = [self.reports[:-1], self.reports + [self.reports[0]],
                    self.reports[:-1] + [self.reports[0]]]
        for field, value in (("target", "windows/386"), ("native_passed", False),
                             ("go_version", "go0"), ("errors", ["timeout"])):
            altered = copy.deepcopy(self.reports)
            altered[0][field] = value
            variants.append(altered)
        for reports in variants:
            with self.subTest(reports=reports), self.assertRaises(proof.InvalidEvidence):
                proof.aggregate(reports, self.source)

    def test_source_mismatch_rejected(self):
        altered = copy.deepcopy(self.reports)
        altered[0]["source"]["files"]["main.go"] = "b" * 64
        with self.assertRaises(proof.InvalidEvidence):
            proof.aggregate(altered, self.source)

    def test_build_and_native_are_independent(self):
        report = self.reports[0]
        report["build"]["exit"] = 1
        with self.assertRaises(proof.InvalidEvidence):
            proof.validate_report(report, self.source)

    def test_check_presence_or_pass_flag_is_insufficient(self):
        for change in (lambda c: c.update(passed=False), lambda c: c.update(exit=9),
                       lambda c: c.update(stdout=[{"unexpected": True}]),
                       lambda c: c.update(marker_valid=False), lambda c: c.update(byte_jsonl_valid=False)):
            report = copy.deepcopy(self.reports[0])
            change(report["checks"][0])
            with self.assertRaises(proof.InvalidEvidence):
                proof.validate_report(report, self.source)

    def test_duplicate_and_missing_checks_rejected(self):
        for checks in (self.reports[0]["checks"][:-1], [self.reports[0]["checks"][0]] * 7):
            report = copy.deepcopy(self.reports[0])
            report["checks"] = checks
            with self.assertRaises(proof.InvalidEvidence):
                proof.validate_report(report, self.source)

    def test_windows_emulation_rejected(self):
        report = self.reports[3]
        report["checks"][1]["stdout"][0]["emulated"] = True
        with self.assertRaises(proof.InvalidEvidence):
            proof.validate_report(report, self.source)

    def test_byte_jsonl_is_exact(self):
        self.assertEqual(proof.jsonl(b'{"ok":true}\n', 1), [{"ok": True}])
        self.assertEqual(proof.jsonl(b"", 0), [])
        for raw in (b'{}', b'{}\r\n', b'{}\n{}\n', b'{}\n\n', b'\xef\xbb\xbf{}\n',
                    b'{"bad":"\xff"}\n', b'[]\n', b'null\n', b"x" * (proof.LIMIT + 1)):
            with self.subTest(raw=raw[:40]), self.assertRaises(proof.InvalidEvidence):
                proof.jsonl(raw, 1)

    def test_existing_evidence_not_overwritten(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "evidence.json"
            proof.write_report(path, {"old": True})
            with self.assertRaises(FileExistsError):
                proof.write_report(path, {"new": True})
            self.assertEqual(json.loads(path.read_text()), {"old": True})

    def test_missing_toolchain_reports_failure(self):
        with patch("proof.platform.system", return_value="Linux"), patch("proof.shutil.which", return_value=None):
            report = proof.native_run("linux/amd64", "a" * 40)
        self.assertFalse(report["native_passed"])
        self.assertFalse(report["build"]["passed"])
        self.assertEqual(report["checks"], [])
        self.assertEqual(report["errors"], ["Go toolchain unavailable"])

    def test_timeout_is_failure(self):
        with patch("proof.subprocess.run", side_effect=proof.subprocess.TimeoutExpired("fixture", 120)):
            with self.assertRaisesRegex(proof.InvalidEvidence, "timed out"):
                proof.capture(["fixture"], {}, proof.ROOT)


if __name__ == "__main__":
    unittest.main()
