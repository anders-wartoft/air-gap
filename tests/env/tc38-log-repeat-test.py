"""TC-38: real Go services over Docker TLS, without Kafka or mocked log events."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time


HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent


def report(passed, description):
    color = "" if os.environ.get("NO_COLOR") else ("\033[0;32m" if passed else "\033[0;31m")
    reset = "\033[0m" if color else ""
    status = "PASS" if passed else "FAIL"
    print(f"{color}{status}: {description}{reset}", flush=True)


def records(path):
    if not path.exists():
        return []
    # A running process may be appending its final line.
    lines = path.read_bytes().split(b"\n")[:-1]
    return [
        record
        for line in lines
        for resource in json.loads(line)["resourceLogs"]
        for scope in resource["scopeLogs"]
        for record in scope["logRecords"]
    ]


def attributes(record):
    return {item["key"]: item["value"] for item in record.get("attributes", [])}


def wait_for(description, predicate, timeout=30):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.1)
    check(False, f"Timed out waiting for {description}")


def check(condition, description):
    report(condition, description)
    if not condition:
        raise AssertionError(description)


def main():
    work = Path(tempfile.mkdtemp(prefix="airgap-tc38-")).resolve()
    env = dict(os.environ, TC38_WORK=str(work), COMPOSE_PROGRESS="plain")
    project = work.name.lower()
    command = [
        "docker", "compose", "-p", project,
        "-f", str(HERE / "docker-compose.tc38.yml"),
    ]

    def compose(*args, capture=False):
        return subprocess.run(
            command + list(args), env=env, cwd=HERE, check=True,
            text=True, stdout=subprocess.PIPE if capture else None,
        ).stdout

    success = False
    try:
        build_env = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH="amd64")
        for role in ("upstream", "downstream"):
            subprocess.run(
                ["go", "build", "-o", str(work / role), f"./src/cmd/{role}"],
                cwd=ROOT, env=build_env, check=True,
            )
        subprocess.run([
            "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
            "-keyout", str(work / "identity.key"),
            "-out", str(work / "identity.crt"), "-days", "1",
            "-subj", "/CN=tc38",
        ], check=True)
        compose("build", "downstream")
        compose("up", "-d", "downstream")
        wait_for("TLS listener", lambda: any(
            r["eventName"] == "tcp.listen" for r in records(work / "downstream.jsonl")
        ))

        for phase, interval in (("enabled", "2000"), ("disabled", "0")):
            env.update(TC38_PHASE=phase, TC38_REPEAT_INTERVAL=interval)
            compose("up", "-d", "--force-recreate", "upstream")
            path = work / f"{phase}.jsonl"
            # The client verifies the same server on every dial, generating a
            # stable real event interleaved with changing retry diagnostics.
            def handshakes():
                return [
                    r for r in records(path)
                    if r["eventName"] == "tls.handshake"
                    and "Authenticated server CN" in r["body"]["stringValue"]
                ]

            if phase == "enabled":
                wait_for("log rate limiting: JSON repeat count greater than one", lambda: any(
                    int(attributes(r).get("airgap.repeat.count", {}).get("intValue", "1")) > 1
                    for r in handshakes()
                ))
            else:
                wait_for("unsuppressed handshakes", lambda: len(handshakes()) >= 5)
            compose("stop", "-t", "10", "upstream")
            events = handshakes()
            check(len(events) >= 2, f"{phase}: multiple real TLS handshake events")
            original = events[0]
            if phase == "enabled":
                check(any(
                    int(attributes(r).get("airgap.repeat.count", {}).get("intValue", "1")) > 1
                    for r in events
                ), "Log rate limiting: repeated events grouped at the 2000 ms interval")
            check("airgap.repeat.count" not in attributes(original),
                  f"{phase}: initial occurrence omits repeat count")
            for event in events:
                attrs = attributes(event)
                repeat = attrs.pop("airgap.repeat.count", None)
                if repeat is not None:
                    check("intValue" in repeat and int(repeat["intValue"]) > 1,
                          f"{phase}: repeat count is an OTLP integer and not one")
                check(event["body"] == original["body"]
                      and event["severityNumber"] == original["severityNumber"]
                      and event["severityText"] == original["severityText"]
                      and attrs == attributes(original),
                      f"{phase}: original event body/severity/attributes preserved")
            if phase == "disabled":
                check(all("airgap.repeat.count" not in attributes(r) for r in records(path)),
                      "zero interval disables repeat attributes")

        failures = [
            r for r in records(work / "downstream.jsonl")
            if r["eventName"] == "tls.handshake"
            and "client didn't provide a certificate" in r["body"]["stringValue"]
        ]
        check(len(failures) >= 8, "downstream observed repeated missing-client-certificate failures")
        times = [int(r["timeUnixNano"]) for r in failures]
        # Allow 30 ms scheduling/network jitter, but never a tight reconnect loop.
        check(all(b - a >= 270_000_000 for a, b in zip(times, times[1:])),
              "Sender rate limiting: 300 ms reconnect interval respected (30 ms timing tolerance)")
        check(all("Random message" not in r["body"]["stringValue"]
                  for r in records(work / "downstream.jsonl")),
              "unauthenticated clients delivered no random messages")

        env.update(
            TC38_PHASE="recovered", TC38_REPEAT_INTERVAL="2000",
            TC38_CLIENT_CERT="/airgap/work/identity.crt",
            TC38_CLIENT_KEY="/airgap/work/identity.key",
        )
        compose("up", "-d", "--force-recreate", "upstream")
        wait_for("authenticated payload delivery", lambda: "Random message" in
                 compose("logs", "--no-color", "downstream", capture=True))
        check(any(
            r["eventName"] == "tls.handshake"
            and "Authenticated client CN" in r["body"]["stringValue"]
            for r in records(work / "downstream.jsonl")
        ), "corrected client certificate authenticates and real payload delivery resumes")
        success = True
    finally:
        if not success:
            compose("logs", "--no-color", "--tail", "30")
        if os.environ.get("TC_PAUSE") == "1":
            print(f"TC-38 containers retained: project={project}, artifacts={work}")
        else:
            compose("down", "-v", "--remove-orphans")
            if success:
                shutil.rmtree(work)
            else:
                print(f"TC-38 failure artifacts retained at {work}")
    report(True, "TC-38 repeated JSON logs and failed-mTLS reconnect rate")


if __name__ == "__main__":
    try:
        main()
    except (AssertionError, subprocess.CalledProcessError, OSError) as error:
        report(False, f"TC-38 repeated JSON logs and failed-mTLS reconnect rate: {error}")
        raise
