#!/usr/bin/env bash
# Generate the dedicated Version 1 signing certificate before containers start.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"

CERT_DIR="../../certs/tmp/tc29"
mkdir -p "$CERT_DIR"
rm -f \
    "$CERT_DIR/signer.crt" "$CERT_DIR/signer.key" "$CERT_DIR/signer.pub" \
    "$CERT_DIR/wrong.crt" "$CERT_DIR/wrong.key" "$CERT_DIR/wrong.pub"

go run ./lib/tc29_integrity.go generate \
    --dir "$CERT_DIR" \
    --name signer \
    --signer-id urn:airgap:test:tc29

go run ./lib/tc29_integrity.go generate \
    --dir "$CERT_DIR" \
    --name wrong \
    --signer-id urn:airgap:test:tc29

test -s "$CERT_DIR/signer.crt"
test -s "$CERT_DIR/signer.key"
test -s "$CERT_DIR/signer.pub"
test -s "$CERT_DIR/wrong.crt"

