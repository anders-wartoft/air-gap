"""Serialize completed batch testcase results for CI (standard library only)."""

import csv
import json
import os
from pathlib import Path
import re
import sys
import tempfile
import xml.etree.ElementTree as ET


def xml_text(text):
    # XML 1.0 cannot contain ANSI escapes or most control characters.
    text = re.sub(r"\x1b\[[0-?]*[ -/]*[@-~]", "", text)
    return "".join(
        char for char in text
        if char in "\t\n\r" or "\x20" <= char <= "\ud7ff"
        or "\ue000" <= char <= "\ufffd" or "\U00010000" <= char <= "\U0010ffff"
    )


def write_atomic(path, content):
    destination = Path(path)
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(dir=destination.parent, delete=False) as output:
            temporary = Path(output.name)
            output.write(content)
        os.replace(temporary, destination)
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)


def write_reports(rows_path, log_dir, json_path, junit_path):
    results = {}
    with Path(rows_path).open(newline="") as source:
        for number, code, seconds, reason in csv.reader(source, delimiter="\t"):
            code = int(code)
            result = {
                "status": "skipped" if code == -1 else "passed" if code == 0 else "failed",
                "code": code,
                "durationSeconds": int(seconds),
            }
            if reason:
                result["reason"] = reason
            results[f"TC-{number}"] = result

    summary = {
        status: sum(result["status"] == status for result in results.values())
        for status in ("passed", "failed", "skipped")
    }
    if json_path:
        write_atomic(json_path, (json.dumps(
            {"results": results, "summary": summary}, indent=2
        ) + "\n").encode())

    if junit_path:
        suite = ET.Element("testsuite", {
            "name": "airgap-docker",
            "tests": str(len(results)),
            "failures": str(summary["failed"]),
            "errors": "0",
            "skipped": str(summary["skipped"]),
            "time": str(sum(result["durationSeconds"] for result in results.values())),
        })
        for name, result in results.items():
            case = ET.SubElement(suite, "testcase", {
                "classname": "airgap.docker", "name": name,
                "time": str(result["durationSeconds"]),
            })
            if result["status"] == "skipped":
                ET.SubElement(case, "skipped", {
                    "message": xml_text(result["reason"]),
                })
            elif result["status"] == "failed":
                failure = ET.SubElement(case, "failure", {
                    "message": f"Testcase exited with code {result['code']}",
                    "type": "TestcaseFailure",
                })
                # Keep CI reports bounded; complete logs can be retained separately.
                log = Path(log_dir) / f"tc-{name[3:]}.log"
                with log.open("rb") as source:
                    source.seek(0, os.SEEK_END)
                    source.seek(max(0, source.tell() - 65536))
                    failure.text = xml_text(source.read().decode("utf-8", errors="replace"))
        write_atomic(junit_path, ET.tostring(suite, encoding="utf-8", xml_declaration=True))


if __name__ == "__main__":
    write_reports(*sys.argv[1:])
