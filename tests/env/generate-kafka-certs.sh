#!/usr/bin/env bash
# generate-kafka-certs.sh — populate certs/kafka/ssl/ with Kafka broker keystores
# plus a combined truststore, so every broker can advertise an SSL listener
# alongside the PLAINTEXT listener.
#
# Idempotent: an existing keystore is kept; the truststore is always rebuilt
# so new CAs (ours + any pre-existing kafka-*.truststore.jks) are visible.
#
# Prerequisites on the host:
#   - openssl
#   - keytool (ships with any JDK; Temurin or system JRE both work)
#
# After running once, Kafka brokers can be started with --profile kafka-tls (or
# the SSL listener bits unconditionally present in docker-compose.yml).

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SSL_DIR="$HERE/../../certs/kafka/ssl"
mkdir -p "$SSL_DIR"
cd "$SSL_DIR"

STOREPASS="${KAFKA_KEYSTORE_PASSWORD:-changeit}"
VALID_DAYS=3650
# Force keytool output into English so parsing is predictable regardless of
# the host locale.
export JAVA_TOOL_OPTIONS="-Duser.language=en -Duser.country=US"

CA_KEY="testenv-ca.key"
CA_CRT="testenv-ca.crt"

if [[ ! -f "$CA_KEY" || ! -f "$CA_CRT" ]]; then
    echo "[certs] generating test-env CA"
    openssl genrsa -out "$CA_KEY" 2048 >/dev/null 2>&1
    openssl req -x509 -new -nodes -key "$CA_KEY" -sha256 -days "$VALID_DAYS" \
        -subj "/C=SE/O=Air-gap test env/CN=airgap-testenv-ca" \
        -out "$CA_CRT" >/dev/null 2>&1
else
    echo "[certs] test-env CA already present"
fi

gen_cluster_keystore() {
    local name="$1"       # kafka-upstream / kafka-downstream / kafka-upstream-b
    local san_list="$2"   # DNS SAN list, comma-separated
    local ks="${name}.keystore.jks"
    if [[ -f "$ks" ]]; then
        echo "[certs] keep existing $ks"
        return
    fi
    echo "[certs] generating $ks"
    local tmp
    tmp="$(mktemp -d)"
    trap 'rm -rf "$tmp"' RETURN
    openssl genrsa -out "$tmp/server.key" 2048 >/dev/null 2>&1
    openssl req -new -key "$tmp/server.key" \
        -subj "/CN=${name}.sitia.nu" \
        -out "$tmp/server.csr" >/dev/null 2>&1
    cat >"$tmp/ext" <<EOF
subjectAltName=${san_list}
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
EOF
    openssl x509 -req -CA "$CA_CRT" -CAkey "$CA_KEY" -in "$tmp/server.csr" \
        -out "$tmp/server.crt" -days "$VALID_DAYS" \
        -CAcreateserial -extfile "$tmp/ext" >/dev/null 2>&1
    openssl pkcs12 -export \
        -in "$tmp/server.crt" -inkey "$tmp/server.key" \
        -CAfile "$CA_CRT" -caname testenv-ca -chain \
        -name "$name" \
        -password "pass:$STOREPASS" \
        -out "$ks"
}

gen_cluster_keystore kafka-upstream \
    "DNS:kafka-upstream.sitia.nu,DNS:kafka-upstream,DNS:kafka-upstream-2,DNS:localhost,IP:127.0.0.1"
gen_cluster_keystore kafka-upstream-b \
    "DNS:kafka-upstream-b.sitia.nu,DNS:kafka-upstream-b,DNS:kafka-upstream-b-2,DNS:localhost,IP:127.0.0.1"
# kafka-downstream.keystore.jks is expected to ship with the repo. If it is
# missing (fresh checkout without the sample cert) generate it too.
gen_cluster_keystore kafka-downstream \
    "DNS:kafka-downstream.sitia.nu,DNS:kafka-downstream,DNS:kafka-downstream-2,DNS:localhost,IP:127.0.0.1"

# ------------------------------------------------------------------------------
# Combined truststore — contains our test-env CA plus any CA already present
# in a pre-existing kafka-*.truststore.jks (so the shipped kafka-downstream
# keystore signed by its original CA still validates for its clients).
# ------------------------------------------------------------------------------
TRUST="kafka-trust.jks"
echo "[certs] rebuilding $TRUST"
rm -f "$TRUST"

tmp_all_pem="$(mktemp)"
trap 'rm -f "$tmp_all_pem"' EXIT
cat "$CA_CRT" >"$tmp_all_pem"

for jks in kafka-*.truststore.jks; do
    [[ -f "$jks" ]] || continue
    openssl pkcs12 -in "$jks" -nokeys -passin "pass:$STOREPASS" 2>/dev/null \
        >> "$tmp_all_pem" || true
done

tmp_split="$(mktemp -d)"
awk -v outdir="$tmp_split" '
    BEGIN { n = 0; out = "" }
    /-----BEGIN CERTIFICATE-----/ { n++; out = outdir "/cert_" n ".pem"; capture = 1 }
    capture { print > out }
    /-----END CERTIFICATE-----/   { capture = 0 }
' "$tmp_all_pem"

for f in "$tmp_split"/cert_*.pem; do
    [[ -f "$f" ]] || continue
    # Alias: short fingerprint hash so duplicates don't double-import.
    alias="ca-$(openssl x509 -in "$f" -noout -fingerprint -sha256 2>/dev/null \
        | sed 's/.*=//; s/://g' | tr '[:upper:]' '[:lower:]' | cut -c1-16)"
    if keytool -list -storepass "$STOREPASS" -keystore "$TRUST" \
        -alias "$alias" >/dev/null 2>&1; then
        continue
    fi
    keytool -importcert -noprompt -storepass "$STOREPASS" \
        -alias "$alias" -file "$f" -keystore "$TRUST" >/dev/null
done
rm -rf "$tmp_split"

echo "[certs] done:"
ls -1 "$SSL_DIR" | sed 's/^/  /'

# ------------------------------------------------------------------------------
# Password files for the Confluent image's _CREDENTIALS env vars.
# The confluent-local / cp-kafka image expects KAFKA_SSL_*_CREDENTIALS to name
# a file inside /etc/kafka/secrets (which is where we mount this directory).
# ------------------------------------------------------------------------------
for f in kafka-keystore-creds kafka-key-creds kafka-truststore-creds; do
    if [[ ! -f "$f" ]]; then
        printf '%s' "$STOREPASS" > "$f"
        chmod 644 "$f"
        echo "[certs] wrote $f"
    fi
done
