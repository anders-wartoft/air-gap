"""Daemon-free Compose portability checks and broker-file permission regression."""

import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import tempfile
import unittest


HERE = Path(__file__).resolve().parent
TRANSFER_OVERRIDES = {"tc34", "tc35", "tc40"}


@unittest.skipUnless(shutil.which("docker"), "Docker Compose CLI is required")
class ComposePortabilityTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temporary = tempfile.TemporaryDirectory()
        cls.addClassCleanup(cls.temporary.cleanup)
        cls.environment = {
            key: os.environ[key] for key in ("PATH", "HOME") if key in os.environ
        }
        for name in (
            "TC_WORK", "TC27_WORK_DIR", "TC30_WORK", "TC31_WORK", "TC32_WORK",
            "TC33_WORK", "TC35_LEAK_BIN", "TC38_WORK", "TC39_WORK",
        ):
            cls.environment[name] = cls.temporary.name
        cls.models = {}
        for path in sorted(HERE.glob("docker-compose*.yml")):
            command = [
                "docker", "compose", "--env-file", os.devnull, "--profile", "*",
            ]
            if path.stem.split(".")[-1] in TRANSFER_OVERRIDES:
                command += ["-f", str(HERE / "docker-compose.kafka-transfer.yml")]
            command += ["-f", str(path), "config", "--format", "json"]
            completed = subprocess.run(
                command, cwd=HERE, env=cls.environment, capture_output=True,
                text=True, timeout=30,
            )
            if completed.returncode:
                raise AssertionError(f"{path.name}: {completed.stderr}")
            cls.models[path.name] = json.loads(completed.stdout)

    def assert_qualified(self, image):
        registry, separator, _ = image.partition("/")
        self.assertTrue(separator, f"Unqualified image: {image}")
        self.assertTrue(
            registry == "localhost" or "." in registry or ":" in registry,
            f"Unqualified registry: {image}",
        )

    def test_all_images_and_build_defaults_are_qualified(self):
        for filename, model in self.models.items():
            for name, service in model["services"].items():
                with self.subTest(file=filename, service=name):
                    if "image" in service:
                        self.assert_qualified(service["image"])
                    else:
                        self.assertIn("build", service)
                    base = service.get("build", {}).get("args", {}).get("BASE_IMAGE")
                    if base:
                        self.assert_qualified(base)
        for name in ("Dockerfile", "Dockerfile.tc8-systemd", "Dockerfile.integrity-report"):
            text = (HERE / name).read_text()
            for image in re.findall(r"^(?:FROM |ARG BASE_IMAGE=)(\S+)", text, re.M):
                if not image.startswith("${"):
                    self.assert_qualified(image)
        for line in (HERE / ".env.example").read_text().splitlines():
            if re.match(r"(?:UPSTREAM_BASE|DOWNSTREAM_BASE|KAFKA|DEDUP)_IMAGE=", line):
                self.assert_qualified(line.split("=", 1)[1])

    def test_shared_bind_mounts_are_labeled_without_relabeling_cgroups(self):
        for filename, model in self.models.items():
            for name, service in model["services"].items():
                for volume in service.get("volumes", []):
                    if volume["type"] != "bind":
                        continue
                    with self.subTest(file=filename, service=name, mount=volume):
                        label = volume.get("bind", {}).get("selinux")
                        if volume["source"] == "/sys/fs/cgroup":
                            self.assertIsNone(label)
                        else:
                            self.assertEqual(label, "z")

    def test_main_stack_preserves_names_used_by_runner_and_hooks(self):
        main = self.models["docker-compose.yml"]
        for name, service in main["services"].items():
            self.assertEqual(
                service["container_name"], f"airgap-testenv-{name}-1", name
            )
        for filename, model in self.models.items():
            if filename != "docker-compose.yml":
                for service in model["services"].values():
                    self.assertNotIn("container_name", service, filename)

    def test_loggenerator_uses_readonly_script_mount_and_shell(self):
        for filename in ("docker-compose.yml", "docker-compose.tc16.yml"):
            model = self.models[filename]
            self.assertNotIn("configs", model)
            for name, service in model["services"].items():
                if not name.startswith("lg-"):
                    continue
                with self.subTest(file=filename, service=name):
                    self.assertEqual(
                        service["entrypoint"], ["/bin/sh", "/airgap/lg-entrypoint.sh"]
                    )
                    mounts = [
                        volume for volume in service["volumes"]
                        if volume["target"] == "/airgap/lg-entrypoint.sh"
                    ]
                    self.assertEqual(len(mounts), 1)
                    self.assertEqual(mounts[0]["source"], str(HERE / "lg-entrypoint.sh"))
                    self.assertTrue(mounts[0]["read_only"])


class BrokerPermissionTests(unittest.TestCase):
    def test_existing_and_new_broker_files_readable_without_exposing_ca_key(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            env_dir = root / "tests/env"
            env_dir.mkdir(parents=True)
            script = env_dir / "generate-kafka-certs.sh"
            shutil.copy2(HERE / script.name, script)
            ssl = root / "certs/kafka/ssl"
            ssl.mkdir(parents=True)
            ca = ssl / "testenv-ca.key"
            ca.write_text("private test CA fixture")
            ca.chmod(0o600)
            (ssl / "testenv-ca.crt").write_text(
                "-----BEGIN CERTIFICATE-----\nfixture\n-----END CERTIFICATE-----\n"
            )
            brokers = [
                ssl / f"{name}.keystore.jks"
                for name in ("kafka-upstream", "kafka-upstream-b", "kafka-downstream")
            ]
            credentials = [
                ssl / name for name in (
                    "kafka-keystore-creds", "kafka-key-creds", "kafka-truststore-creds"
                )
            ]
            for path in brokers + credentials:
                path.write_text(f"existing {path.name}")
                path.chmod(0o600)
            client = root / "certs/tmp"
            client.mkdir()
            for name in ("airgap-upstream", "airgap-downstream"):
                for suffix in ("key", "crt", "key.enc", "pw"):
                    (client / f"{name}.{suffix}").write_text("client fixture")

            tools = root / "tools"
            tools.mkdir()
            openssl = tools / "openssl"
            openssl.write_text(
                '#!/bin/sh\n'
                'case "$*" in *-fingerprint*) echo "SHA256 Fingerprint=AB:CD"; exit 0;; esac\n'
                'while [ "$#" -gt 0 ]; do\n'
                '  if [ "$1" = -out ]; then printf generated > "$2"; exit 0; fi\n'
                '  shift\n'
                'done\n'
                'echo "Unexpected openssl invocation" >&2\nexit 1\n'
            )
            keytool = tools / "keytool"
            keytool.write_text(
                '#!/bin/sh\n'
                '[ "$1" != -list ] || exit 1\n'
                '[ "$1" = -importcert ] || exit 2\n'
                'while [ "$#" -gt 0 ]; do\n'
                '  if [ "$1" = -keystore ]; then printf trust > "$2"; exit 0; fi\n'
                '  shift\n'
                'done\nexit 2\n'
            )
            for tool in (openssl, keytool):
                tool.chmod(0o755)
            environment = dict(os.environ)
            environment["PATH"] = f"{tools}:{os.environ['PATH']}"
            for existing in (True, False):
                with self.subTest(existing=existing):
                    if not existing:
                        brokers[0].unlink()
                        credentials[0].unlink()
                    completed = subprocess.run(
                        ["bash", str(script)], env=environment, capture_output=True,
                        text=True, timeout=15,
                    )
                    self.assertEqual(
                        completed.returncode, 0, completed.stdout + completed.stderr
                    )
                    for path in brokers + credentials + [ssl / "kafka-trust.jks"]:
                        self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o644, path)
                    self.assertEqual(stat.S_IMODE(ca.stat().st_mode), 0o600)
                    self.assertEqual(ca.read_text(), "private test CA fixture")
                    self.assertEqual(
                        brokers[1].read_text(), f"existing {brokers[1].name}"
                    )


if __name__ == "__main__":
    unittest.main()
