#!/usr/bin/env bash

airgap_load_udp_calibration() {
    local settings pattern
    settings="$(cat "$1")" || return 1
    pattern='^udpPacingDatagramsPerSecond=([1-9][0-9]*)'$'\n''udpPacingBytesPerSecond=([1-9][0-9]*)'$'\n''udpPacingBurstDatagrams=([1-9][0-9]{0,2})$'
    if [[ ! "$settings" =~ $pattern ]] || (( BASH_REMATCH[3] > 256 )); then
        echo "Invalid UDP pacing recommendation in $1" >&2
        return 1
    fi
    export AIRGAP_TEST_UDP_PACING="${BASH_REMATCH[1]},${BASH_REMATCH[2]},${BASH_REMATCH[3]}"
}

airgap_write_stress_downstream_config() {
    local transport="$1" batch_size="$2"
    cat <<EOF
id=TC52-DS
nic=eth0
targetIP=0.0.0.0
targetPort=1234
transport=$transport
target=kafka
bootstrapServers=target-kafka:9092
topic=transfer
topicTranslations={"stress":"transfer"}
internalTopic=airgap-internal
clientId=tc52-downstream
mtu=1500
numReceivers=10
channelBufferSize=65536
batchSize=$batch_size
readBufferMultiplier=32
rcvBufSize=4194304
logLevel=INFO
logStatistics=1
EOF
    if [[ "$transport" == udp ]]; then
        echo 'enableRxqOvfl=true'
    fi
}
