"""TC-31 negative-control verdict regressions without a Docker daemon."""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


HERE = Path(__file__).resolve().parent
RUNNER = (HERE / "tc31-unsafe-memory-test.sh").read_text()


class TC31ControlTests(unittest.TestCase):
    def verdict(self, *, killed, log):
        with tempfile.TemporaryDirectory() as work:
            path = Path(work)
            (path / "captured.log").write_text(log)
            environment = os.environ.copy()
            environment.update(
                WORK=work, OOM="true" if killed else "false",
                CODE="137" if killed else "0",
                STATE="exited" if killed else "running", NO_COLOR="1",
            )
            start = RUNNER.rindex('compose logs --no-color --no-log-prefix upstream >')
            script = """
set -euo pipefail
source "$HERE/lib/report.sh"
compose() { cat "$WORK/captured.log"; }
docker() {
    case "$*" in
        *OOMKilled*) echo "$OOM" ;;
        *ExitCode*) echo "$CODE" ;;
        *Status*) echo "$STATE" ;;
        *) return 1 ;;
    esac
}
UPSTREAM_CID=fixture
CAP_BYTES=$((192 * 1024 * 1024))
max_memory=$CAP_BYTES
TC_PAUSE=1
""" + RUNNER[start:]
            environment["HERE"] = str(HERE)
            return subprocess.run(
                ["bash", "-c", script], env=environment, text=True,
                capture_output=True, timeout=5,
            )

    def test_survivor_is_failure_even_at_cap_with_template_logged(self):
        result = self.verdict(
            killed=False,
            log="[INFO] memoryHighWatermarkLog: WARN:Upstream {upstreamId} "
                "entering high memory watermark. Pausing Kafka record admission "
                "at {memoryCurrentBytes}\n",
        )
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("no watermark transition observed; consumer survived", result.stdout)
        self.assertNotIn("watermark observed; consumer killed", result.stdout)

    def test_kill_without_transition_passes(self):
        result = self.verdict(killed=True, log="[INFO] starting upstream\n")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("no watermark transition observed; consumer killed", result.stdout)

    def test_actual_transition_is_reported_without_changing_kill_requirement(self):
        log = "[WARN] Upstream Upstream_31 entering high memory watermark. " \
              "Pausing Kafka record admission at 201326592 of 201326592 bytes.\n"
        for killed in (False, True):
            with self.subTest(killed=killed):
                result = self.verdict(killed=killed, log=log)
                self.assertEqual(result.returncode, 0 if killed else 1)
                self.assertIn("watermark observed;", result.stdout)

    def test_backlog_and_pressure_precede_consumer_release(self):
        producer = RUNNER.index("compose run --rm producer")
        container = RUNNER.index("compose up -d upstream")
        pressure = RUNNER.index('grep -q "pressure-ready" "$WORK/pressure.log" ||')
        sampler = RUNNER.index("SAMPLER_PID=$!")
        release = RUNNER.index('touch "$WORK/start-upstream"')
        self.assertLess(producer, container)
        self.assertLess(container, pressure)
        self.assertLess(pressure, sampler)
        self.assertLess(sampler, release)
        self.assertIn("Kafka backlog was not fully acknowledged", RUNNER)

    def test_fetch_leaves_room_for_response_framing(self):
        configuration = (HERE / "../../config/testcases/upstream-airgap-31-docker.properties").read_text()
        settings = dict(
            line.split("=", 1) for line in configuration.splitlines()
            if line and not line.startswith("#")
        )
        fetch = int(settings["kafkaFetchDefaultBytes"])
        self.assertEqual(fetch, 96 * 1024 * 1024)
        self.assertLessEqual(fetch + 4 * 1024 * 1024, 100 * 1024 * 1024)
        self.assertGreater(fetch * 3, 192 * 1024 * 1024)


if __name__ == "__main__":
    unittest.main()
