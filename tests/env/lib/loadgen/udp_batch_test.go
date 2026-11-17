package main

import (
	"sync/atomic"
	"testing"
)

func TestUDPBatchOptionsBoundExpectedRecords(t *testing.T) {
	options := udpBatchOptions{
		address: "downstream:1234", brokers: "target-kafka:9092",
		topic: udpBatchSourceTopic, targetTopic: udpBatchTargetTopic,
		rate: 10_000, durationSecs: 2, cycles: 3, size: 600,
		idle: 1, maxSparseLatency: 1, drainTimeout: 1,
	}
	expected, err := validateUDPBatchOptions(options)
	if err != nil || expected != 60_003 {
		t.Fatalf("valid smoke settings: expected=%d, error=%v", expected, err)
	}
	options.rate = udpBatchMaxRecords
	options.durationSecs = 2
	if _, err := validateUDPBatchOptions(options); err == nil {
		t.Fatal("unbounded workload was accepted")
	}
}

func TestValidateUDPBatchRecordIdentityAndBytes(t *testing.T) {
	payload := stressPayload(21, 123456789, 600, 600)
	seq, sent, err := validateUDPBatchRecord("stress_1_2", payload, udpBatchPartitions, 600, 600, 123456768)
	if err != nil || seq != 21 || sent != 123456789 {
		t.Fatalf("valid record: sequence=%d sent=%d error=%v", seq, sent, err)
	}
	for _, id := range []string{"stress_2_2", "stress_1_21", "other_1_2", "stress_x_2"} {
		if _, _, err := validateUDPBatchRecord(id, payload, udpBatchPartitions, 600, 600, 123456768); err == nil {
			t.Errorf("invalid record ID %q was accepted", id)
		}
	}
	if _, _, err := validateUDPBatchRecord("stress_1_2", payload, 0, 600, 600, 123456768); err == nil {
		t.Fatal("record accepted with no target partitions")
	}
	if _, _, err := validateUDPBatchRecord("stress_1_2", payload, udpBatchPartitions, 600, 600, 123456769); err == nil {
		t.Fatal("changed timestamp identity was accepted")
	}
	payload[len(payload)-1] ^= 1
	if _, _, err := validateUDPBatchRecord("stress_1_2", payload, udpBatchPartitions, 600, 600, 123456768); err == nil {
		t.Fatal("corrupted record payload was accepted")
	}
}

func TestUDPBatchBitSetRejectsDuplicate(t *testing.T) {
	seen := make([]atomic.Uint64, 2)
	if !udpBatchBitSet(seen, 65) {
		t.Fatal("first sequence mark failed")
	}

	if udpBatchBitSet(seen, 65) {
		t.Fatal("duplicate sequence mark succeeded")
	}
	if !udpBatchBitSet(seen, 64) {
		t.Fatal("distinct sequence sharing the same word was rejected")
	}
}
