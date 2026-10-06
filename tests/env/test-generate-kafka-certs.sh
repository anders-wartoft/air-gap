#!/usr/bin/env bash
# Exercise certificate setup without Docker or existing local credentials.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
FIXTURE="$(mktemp -d "${TMPDIR:-/tmp}/airgap-certs-test.XXXXXX")"
trap 'rm -rf "$FIXTURE"' EXIT
mkdir -p "$FIXTURE/tests/env" "$FIXTURE/certs/kafka/ssl"
cp "$HERE/generate-kafka-certs.sh" "$FIXTURE/tests/env/"
cp "$HERE/../../certs/kafka/ssl/kafka-downstream."*.jks \
    "$FIXTURE/certs/kafka/ssl/"
cd "$FIXTURE"
export KAFKA_KEYSTORE_PASSWORD=changeit

generate() {
    bash tests/env/generate-kafka-certs.sh
}

check_client() {
    local base="certs/tmp/airgap-$1"
    openssl verify -purpose sslclient -CAfile certs/tmp/kafka-ca.crt "$base.crt"
    openssl x509 -in "$base.crt" -pubkey -noout > cert.pub
    openssl pkey -in "$base.key" -pubout > plain.pub
    openssl pkey -in "$base.key.enc" -passin "file:$base.pw" -pubout > encrypted.pub
    cmp cert.pub plain.pub
    cmp cert.pub encrypted.pub
    if openssl pkey -in "$base.key.enc" -passin pass:wrong-password \
        -noout >/dev/null 2>&1; then
        echo "Encrypted key unexpectedly accepted a wrong password: $base" >&2
        exit 1
    fi
}

generate
for client in upstream downstream; do
    check_client "$client"
done
for broker in kafka-upstream kafka-upstream-b kafka-downstream; do
    keytool -list -rfc -storepass changeit \
        -keystore "certs/kafka/ssl/$broker.keystore.jks" |
        awk '
            /-----BEGIN CERTIFICATE-----/ { capture = 1 }
            capture && !done { print }
            /-----END CERTIFICATE-----/ { done = 1 }
        ' > broker.crt
    openssl verify -purpose sslserver \
        -CAfile certs/tmp/kafka-ca.crt broker.crt
    openssl x509 -in broker.crt -text -noout |
        grep 'DNS:' | tr ',' '\n' | sed 's/^[[:space:]]*//' |
        grep -Fx "DNS:$broker.sitia.nu"
done
cksum certs/tmp/* certs/kafka/ssl/*.keystore.jks \
    certs/kafka/ssl/*-creds > before
generate
cksum certs/tmp/* certs/kafka/ssl/*.keystore.jks \
    certs/kafka/ssl/*-creds > after
cmp before after

rm certs/tmp/airgap-upstream.pw certs/tmp/airgap-downstream.key
generate
check_client upstream
check_client downstream

: > certs/tmp/airgap-upstream.key.enc
generate
check_client upstream
echo "PASS: fresh setup, broker trust, encrypted keys, repeat setup, and partial/empty-key recovery"
