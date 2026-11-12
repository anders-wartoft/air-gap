"""Batch runner/report regression tests using isolated manifests and stub cases."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import unittest
import xml.etree.ElementTree as ET


HERE = Path(__file__).resolve().parent


class BatchRunnerTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.runner = self.root / "env"
        self.runner.mkdir()
        (self.runner / "testcases").mkdir()
        (self.runner / "lib").mkdir()
        shutil.copy2(HERE / "run-testcases.sh", self.runner)
        shutil.copy2(HERE / "lib/chain-report.py", self.runner / "lib")
        for name in ("docker-test.sh", "docker-images.sh"):
            shutil.copy2(HERE / "lib" / name, self.runner / "lib")
        for number in (1, 2, 3):
            (self.runner / f"testcases/{number:02}.env").write_text("TC_RUNNER=stub\n")
        (self.runner / "testcases/16.env").write_text(
            'TC_MANUAL_REASON="Manual performance benchmark <manual> & review"\n'
            "TC_RUNNER=must-not-run\n"
        )
        (self.runner / "testcases/17.env").write_text("# No auto-exit configured\n")
        stub = self.runner / "run-testcase.sh"
        stub.write_text(
            '#!/usr/bin/env bash\nprintf "%s\\n" "$*" >> calls\n'
            'printf "\\033[31mcase <failure> & output\\001\\033[0m\\n"\n'
            'if [[ "$1" == 2 ]]; then exit 7; fi\n'
        )
        stub.chmod(0o755)
        self.environment = dict(os.environ)
        # A stub case must never reach Docker, even on a machine without it.
        docker = self.root / "docker"
        docker.write_text('#!/usr/bin/env bash\nexit 0\n')
        docker.chmod(0o755)
        self.environment["PATH"] = f"{self.root}:{os.environ['PATH']}"

    def run_batch(self, *args):
        return subprocess.run(
            ["bash", str(self.runner / "run-testcases.sh"), *args],
            cwd=self.root, env=self.environment, capture_output=True, text=True,
            timeout=15,
        )

    def test_mixed_results_both_formats_and_logs(self):
        completed = self.run_batch(
            "--json", "reports/results.json", "--junit", "reports/junit.xml",
            "--log-dir", "logs", "1,2,16,17,3,1", "--", "--build",
        )
        self.assertEqual(completed.returncode, 1, completed.stdout + completed.stderr)
        report = json.loads((self.root / "reports/results.json").read_text())
        self.assertEqual(list(report["results"]), ["TC-1", "TC-2", "TC-16", "TC-17", "TC-3"])
        self.assertEqual(
            [result["code"] for result in report["results"].values()], [0, 7, -1, -1, 0]
        )
        self.assertEqual(report["summary"], {"passed": 2, "failed": 1, "skipped": 2})
        self.assertIn("Manual performance", report["results"]["TC-16"]["reason"])
        self.assertEqual((self.runner / "calls").read_text().splitlines(), [
            "1 --build", "2 --build", "3 --build",
        ])
        self.assertTrue((self.root / "logs/tc-2.log").is_file())
        overview = (self.root / "logs/overview.log").read_text()
        lines = overview.splitlines()
        markers = [line for line in lines if line.startswith(("---", "───", "✅", "❌", "🤔"))]
        self.assertEqual(len(markers), 10)
        self.assertEqual(markers[0], "--- batch started: 1 2 16 17 3 ---")
        self.assertEqual(markers[1], "─── testcase 1 RUNNING ───")
        self.assertRegex(markers[2], r"^✅ testcase 1 PASS \(\d+s\)$")
        self.assertEqual(markers[3], "─── testcase 2 RUNNING ───")
        self.assertRegex(markers[4], r"^❌ testcase 2 FAIL \(\d+s\)$")
        self.assertEqual(markers[5:7], ["🤔 testcase 16 SKIP", "🤔 testcase 17 SKIP"])
        self.assertEqual(markers[7], "─── testcase 3 RUNNING ───")
        self.assertRegex(markers[8], r"^✅ testcase 3 PASS \(\d+s\)$")
        self.assertEqual(markers[9], "--- batch complete: 2 passed, 1 failed, 2 skipped ---")
        self.assertIn("exit code: 7;", overview)
        self.assertIn("log: tc-2.log", overview)
        self.assertIn("reason: Manual performance", overview)
        self.assertNotIn("\x1b", overview)
        suite = ET.parse(self.root / "reports/junit.xml").getroot()
        self.assertEqual(suite.get("tests"), "5")
        self.assertEqual(suite.get("failures"), "1")
        self.assertEqual(suite.get("skipped"), "2")
        self.assertIsNone(suite.find("./testcase[@name='TC-1']/failure"))
        failed = suite.find("./testcase[@name='TC-2']/failure")
        self.assertIn("<failure> & output", failed.text)
        self.assertNotIn("\x1b", failed.text)
        self.assertNotIn("\x01", failed.text)
        self.assertEqual(
            suite.find("./testcase[@name='TC-16']/skipped").get("message"),
            "Manual performance benchmark <manual> & review",
        )

    def test_pass_and_skip_exit_zero(self):
        for selection in ("1", "16", "1,16", "1-1,16-17"):
            with self.subTest(selection=selection):
                completed = self.run_batch("--json", "results.json", selection)
                self.assertEqual(completed.returncode, 0, completed.stdout + completed.stderr)
                report = json.loads((self.root / "results.json").read_text())
                self.assertEqual(report["summary"]["failed"], 0)

    def test_junit_only_and_default_selection(self):
        completed = self.run_batch("--junit", "junit.xml")
        self.assertEqual(completed.returncode, 1, completed.stdout + completed.stderr)
        suite = ET.parse(self.root / "junit.xml").getroot()
        self.assertEqual(suite.get("tests"), "5")

    def test_existing_no_report_mode(self):
        completed = self.run_batch("1,16")
        self.assertEqual(completed.returncode, 0, completed.stdout + completed.stderr)
        self.assertIn("chain OK", completed.stdout)

    def test_overview_is_live_and_overwritten_on_next_run(self):
        stub = self.runner / "run-testcase.sh"
        stub.write_text(
            '#!/usr/bin/env bash\n'
            'touch waiting\n'
            'read -r release\n'
        )
        logs = self.root / "logs"
        logs.mkdir()
        overview = logs / "overview.log"
        overview.write_text("previous run\n")
        with subprocess.Popen(
            ["bash", str(self.runner / "run-testcases.sh"), "--log-dir", "logs", "1"],
            cwd=self.root, env=self.environment, stdin=subprocess.PIPE,
            stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True,
        ) as process:
            try:
                deadline = time.monotonic() + 5
                while not (self.runner / "waiting").exists() and process.poll() is None:
                    self.assertLess(time.monotonic(), deadline, "stub did not start")
                    time.sleep(0.02)
                self.assertIsNone(process.poll())
                live = overview.read_text()
                self.assertIn("─── testcase 1 RUNNING ───", live)
                self.assertNotIn("previous run", live)
                self.assertNotIn("PASS", live)
                self.assertNotIn("batch complete", live)
                _, error = process.communicate("continue\n", timeout=5)
                self.assertEqual(process.returncode, 0, error)
            finally:
                if process.poll() is None:
                    process.kill()
                    process.communicate()
        self.assertIn("✅ testcase 1 PASS", overview.read_text())
        self.assertIn("--- batch complete: 1 passed, 0 failed, 0 skipped ---", overview.read_text())

    def test_unwritable_overview_is_runner_error(self):
        (self.root / "logs/overview.log").mkdir(parents=True)
        completed = self.run_batch("--log-dir", "logs", "1")
        self.assertEqual(completed.returncode, 2, completed.stdout + completed.stderr)
        self.assertIn("cannot initialize overview log", completed.stderr)
        self.assertFalse((self.runner / "calls").exists())

    def test_report_failure_is_runner_error(self):
        (self.runner / "lib/chain-report.py").write_text("raise OSError('report failure')\n")
        completed = self.run_batch("--json", "results.json", "1")
        self.assertEqual(completed.returncode, 2, completed.stdout + completed.stderr)
        self.assertIn("failed to write results report", completed.stderr)

    def test_invalid_options_selections_and_paths(self):
        for args in (
            ("--json",), ("--junit", ""), ("--bogus",),
            ("1,99",), ("3-1",), ("99-100",),
            ("--json", "same", "--junit", "same", "1"),
            ("--json", str(self.runner), "1"),
        ):
            with self.subTest(args=args):
                completed = self.run_batch(*args)
                self.assertEqual(completed.returncode, 2, completed.stdout + completed.stderr)
                self.assertFalse((self.runner / "calls").exists())


if __name__ == "__main__":
    unittest.main()
