#!/usr/bin/env python3
"""Atomically install log-signing settings and optionally signal a service."""

from __future__ import annotations

import argparse
import os
import signal
import tempfile
from pathlib import Path
from typing import Dict, List, Optional

SETTING_ORDER = (
    "log-signing-cert",
    "log-signing-key",
    "log-signer-id",
    "log-signing-eku",
)


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(
        description=(
            "Add or replace log-signing settings in an air-gap service "
            "configuration. The certificate may be CA-issued or self-signed."
        )
    )
    result.add_argument("--config", required=True, help="service configuration file")
    result.add_argument("--certificate", required=True, help="PEM leaf certificate")
    result.add_argument("--key", required=True, help="matching PEM PKCS#8 private key")
    result.add_argument(
        "--signer-id",
        help="URI signer identity; omit to use the certificate's sole URI SAN",
    )
    result.add_argument("--eku", help="required log-signing EKU OID")
    target = result.add_mutually_exclusive_group()
    target.add_argument("--pid", type=int, help="send SIGHUP to this PID after update")
    target.add_argument("--pid-file", help="read PID and send SIGHUP after update")
    return result


def setting_values(args: argparse.Namespace) -> Dict[str, Optional[str]]:
    return {
        "log-signing-cert": str(Path(args.certificate).resolve()),
        "log-signing-key": str(Path(args.key).resolve()),
        "log-signer-id": args.signer_id,
        "log-signing-eku": args.eku,
    }


def update_lines(lines: List[str], values: Dict[str, Optional[str]]) -> List[str]:
    result: List[str] = []
    written = set()
    for line in lines:
        stripped = line.strip()
        if not stripped or stripped.startswith("#") or "=" not in line:
            result.append(line)
            continue
        key = line.split("=", 1)[0].strip()
        if key not in values:
            result.append(line)
            continue
        if key in written:
            continue
        value = values[key]
        if value:
            result.append(f"{key}={value}\n")
        written.add(key)

    if result and not result[-1].endswith("\n"):
        result[-1] += "\n"
    for key in SETTING_ORDER:
        value = values[key]
        if key not in written and value:
            result.append(f"{key}={value}\n")
    return result


def atomic_write(path: Path, content: str) -> None:
    existing = path.stat()
    existing_mode = existing.st_mode & 0o777
    descriptor, temporary = tempfile.mkstemp(
        prefix=f".{path.name}.", dir=str(path.parent), text=True
    )
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            handle.write(content)
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temporary, existing_mode)
        temporary_owner = os.stat(temporary)
        if (
            hasattr(os, "chown")
            and (temporary_owner.st_uid, temporary_owner.st_gid)
            != (existing.st_uid, existing.st_gid)
        ):
            os.chown(temporary, existing.st_uid, existing.st_gid)
        os.replace(temporary, path)
    except BaseException:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass
        raise


def reload_pid(args: argparse.Namespace) -> Optional[int]:
    if args.pid is not None:
        return args.pid
    if args.pid_file:
        value = Path(args.pid_file).read_text(encoding="utf-8").strip()
        try:
            return int(value)
        except ValueError as exc:
            raise ValueError(f"PID file does not contain an integer: {args.pid_file}") from exc
    return None


def main() -> int:
    args = parser().parse_args()
    config = Path(args.config).resolve()
    certificate = Path(args.certificate).resolve()
    key = Path(args.key).resolve()
    if not config.is_file():
        raise ValueError(f"configuration file does not exist: {config}")
    if not certificate.is_file():
        raise ValueError(f"certificate file does not exist: {certificate}")
    if not key.is_file():
        raise ValueError(f"private-key file does not exist: {key}")

    lines = config.read_text(encoding="utf-8").splitlines(keepends=True)
    updated = "".join(update_lines(lines, setting_values(args)))
    atomic_write(config, updated)
    print(f"Updated log-signing configuration: {config}")

    pid = reload_pid(args)
    if pid is not None:
        os.kill(pid, signal.SIGHUP)
        print(f"Sent SIGHUP to PID {pid}")
    else:
        print("Restart the service or send SIGHUP after reviewing the configuration.")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, ValueError) as exc:
        print(f"configure-log-signing: {exc}", file=os.sys.stderr)
        raise SystemExit(1)
