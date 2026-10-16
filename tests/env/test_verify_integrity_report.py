import hashlib
import os
import tempfile
import unittest
from datetime import datetime, timedelta, timezone
from pathlib import Path

from cryptography import x509
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import ed25519
from cryptography.x509.oid import NameOID

import verify_integrity_report as verifier


class ChainMetadataTests(unittest.TestCase):
    def make_result(
        self,
        time_unix_nano: int = 1_700_000_000_000_000_000,
        severity_number: int = 9,
        severity_text: str = "INFO",
    ) -> verifier.EventResult:
        body = "protected metadata"
        payload_hash = hashlib.sha256(body.encode()).digest()
        previous = b"previous-chain"
        seq = 4
        chain = hashlib.sha256(
            b"airgap-log-chain-v1\x00"
            + previous
            + seq.to_bytes(8, "big")
            + time_unix_nano.to_bytes(8, "big")
            + severity_number.to_bytes(4, "big")
            + verifier.framed_text(severity_text)
            + payload_hash
        ).digest()
        return verifier.EventResult(
            index=1,
            attributes={
                verifier.VERSION_ATTRIBUTE: "v1",
                "airgap.log.hash.alg": "SHA256",
                "airgap.log.hash": payload_hash.hex(),
                "airgap.log.seq": seq,
                "airgap.log.run.id": "run",
                "airgap.log.segment.id": "segment",
                "airgap.log.chain.prev": previous.hex(),
                "airgap.log.chain": chain.hex(),
            },
            body=body,
            time_unix_nano=time_unix_nano,
            severity_number=severity_number,
            severity_text=severity_text,
        )

    def test_timestamp_and_severity_are_bound_into_chain(self) -> None:
        baseline = self.make_result()
        verifier.build_chain_record(
            baseline, baseline.attributes, baseline.body, "v1"
        )
        self.assertTrue(baseline.ok)

        for changed in (
            self.make_result(time_unix_nano=1_700_000_000_000_000_001),
            self.make_result(severity_number=17),
            self.make_result(severity_text="ERROR"),
        ):
            changed.attributes["airgap.log.chain"] = baseline.attributes[
                "airgap.log.chain"
            ]
            verifier.build_chain_record(
                changed, changed.attributes, changed.body, "v1"
            )
            self.assertFalse(changed.ok)


class PublicTestVectorTests(unittest.TestCase):
    def test_version_1_chain_and_checkpoint_vector(self) -> None:
        configured_path = os.environ.get("AIRGAP_INTEGRITY_VECTOR")
        if configured_path:
            path = Path(configured_path)
        else:
            path = (
                Path(__file__).resolve().parents[2]
                / "src/logging/testdata/log-integrity-v1-test-vector.json"
            )
        vector = verifier.json.loads(path.read_text(encoding="utf-8"))
        self.assertEqual(vector["schema"], "airgap-log-integrity-test-vector-v1")

        chain = vector["chain"]
        body = chain["body_utf8"].encode()
        self.assertEqual(body.hex(), chain["body_hex"])
        payload_hash = hashlib.sha256(body).digest()
        self.assertEqual(payload_hash.hex(), chain["payload_hash_hex"])
        chain_input = (
            chain["domain_utf8"].encode()
            + bytes.fromhex(chain["domain_separator_hex"])
            + bytes.fromhex(chain["previous_chain_hex"])
            + int(chain["sequence"]).to_bytes(8, "big")
            + int(chain["time_unix_nano"]).to_bytes(8, "big")
            + int(chain["severity_number"]).to_bytes(4, "big")
            + verifier.framed_text(chain["severity_text"])
            + payload_hash
        )
        self.assertEqual(chain_input.hex(), chain["chain_input_hex"])
        self.assertEqual(
            hashlib.sha256(chain_input).hexdigest(),
            chain["chain_value_hex"],
        )

        checkpoint = vector["checkpoint"]
        result = verifier.EventResult(
            index=1,
            attributes={
                verifier.VERSION_ATTRIBUTE: checkpoint["integrity_version"],
                "airgap.log.hash.alg": checkpoint["hash_algorithm"],
                "airgap.log.signature.alg": checkpoint["signature_algorithm"],
                "airgap.log.signer.id": checkpoint["signer_id"],
                "airgap.log.key.id": checkpoint["key_id"],
                "airgap.log.run.id": checkpoint["run_id"],
                "airgap.log.segment.id": checkpoint["segment_id"],
                "airgap.log.window.id": checkpoint["window_id"],
                "airgap.log.process.start_unix_nano": checkpoint[
                    "process_start_unix_nano"
                ],
                "airgap.log.anchor.from": checkpoint["anchor_from"],
                "airgap.log.anchor.to": checkpoint["anchor_to"],
                "airgap.log.chain.prev": checkpoint[
                    "anchor_previous_chain_hex"
                ],
                "airgap.log.chain": checkpoint["anchor_final_chain_hex"],
            },
            resource={
                "service.name": checkpoint["service_name"],
                "service.instance.id": checkpoint["service_instance_id"],
            },
        )
        statement = verifier.build_checkpoint_statement(result)
        self.assertEqual(
            statement.hex(),
            checkpoint["checkpoint_statement_hex"],
        )
        checkpoint_hash = hashlib.sha256(statement).digest()
        self.assertEqual(
            checkpoint_hash.hex(),
            checkpoint["checkpoint_hash_hex"],
        )
        private_key = ed25519.Ed25519PrivateKey.from_private_bytes(
            bytes.fromhex(checkpoint["test_only_ed25519_seed_hex"])
        )
        public_key = private_key.public_key()
        self.assertEqual(
            public_key.public_bytes(
                serialization.Encoding.Raw,
                serialization.PublicFormat.Raw,
            ).hex(),
            checkpoint["public_key_raw_hex"],
        )
        public_pem = public_key.public_bytes(
            serialization.Encoding.PEM,
            serialization.PublicFormat.SubjectPublicKeyInfo,
        ).decode()
        self.assertEqual(
            verifier.public_key_der(public_pem).hex(),
            checkpoint["public_key_spki_der_hex"],
        )
        self.assertEqual(
            verifier.public_key_id(public_pem),
            checkpoint["key_id"],
        )
        signature = private_key.sign(checkpoint_hash)
        self.assertEqual(signature.hex(), checkpoint["signature_hex"])
        self.assertEqual(
            verifier.base64.b64encode(signature).decode(),
            checkpoint["signature_base64"],
        )
        verifier.verify_ed25519(public_pem, checkpoint_hash, signature)


class ArchivalCertificateTests(unittest.TestCase):
    def setUp(self) -> None:
        self.now = datetime.now(timezone.utc)
        self.event_time = self.now - timedelta(days=2)
        self.signer_id = "urn:airgap:test:archive"

        self.ca_key = ed25519.Ed25519PrivateKey.generate()
        ca_name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "Test CA")])
        self.ca = (
            x509.CertificateBuilder()
            .subject_name(ca_name)
            .issuer_name(ca_name)
            .public_key(self.ca_key.public_key())
            .serial_number(1)
            .not_valid_before(self.now - timedelta(days=10))
            .not_valid_after(self.now + timedelta(days=10))
            .add_extension(x509.BasicConstraints(ca=True, path_length=None), True)
            .add_extension(
                x509.KeyUsage(
                    digital_signature=True,
                    content_commitment=False,
                    key_encipherment=False,
                    data_encipherment=False,
                    key_agreement=False,
                    key_cert_sign=True,
                    crl_sign=True,
                    encipher_only=None,
                    decipher_only=None,
                ),
                True,
            )
            .sign(self.ca_key, algorithm=None)
        )

        self.leaf_key = ed25519.Ed25519PrivateKey.generate()
        leaf_name = x509.Name(
            [x509.NameAttribute(NameOID.COMMON_NAME, "Expired log signer")]
        )
        self.leaf = (
            x509.CertificateBuilder()
            .subject_name(leaf_name)
            .issuer_name(self.ca.subject)
            .public_key(self.leaf_key.public_key())
            .serial_number(42)
            .not_valid_before(self.now - timedelta(days=3))
            .not_valid_after(self.now - timedelta(days=1))
            .add_extension(
                x509.SubjectAlternativeName(
                    [x509.UniformResourceIdentifier(self.signer_id)]
                ),
                False,
            )
            .add_extension(
                x509.KeyUsage(
                    digital_signature=True,
                    content_commitment=False,
                    key_encipherment=False,
                    data_encipherment=False,
                    key_agreement=False,
                    key_cert_sign=False,
                    crl_sign=False,
                    encipher_only=None,
                    decipher_only=None,
                ),
                True,
            )
            .sign(self.ca_key, algorithm=None)
        )

    def test_expired_now_is_warning_when_valid_at_event_time(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            ca_path = Path(directory) / "ca.pem"
            ca_path.write_bytes(
                self.ca.public_bytes(serialization.Encoding.PEM)
            )
            material = verifier.certificate_public_key(
                self.leaf.public_bytes(serialization.Encoding.PEM).decode(),
                self.signer_id,
                "42",
                self.event_time,
                str(ca_path),
            )

        key_id = verifier.public_key_id(material.public_key_pem)
        attributes = {
            verifier.VERSION_ATTRIBUTE: "v1",
            "airgap.log.hash.alg": "SHA256",
            "airgap.log.signature.alg": "Ed25519",
            "airgap.log.signer.id": self.signer_id,
            "airgap.log.key.id": key_id,
            "airgap.log.run.id": "run",
            "airgap.log.segment.id": "segment",
            "airgap.log.window.id": "window",
            "airgap.log.process.start_unix_nano": 1,
            "airgap.log.anchor.from": 0,
            "airgap.log.anchor.to": 1,
            "airgap.log.chain.prev": (b"a" * 32).hex(),
            "airgap.log.chain": (b"b" * 32).hex(),
        }
        result = verifier.EventResult(
            index=1,
            attributes=attributes,
            resource={
                "service.name": "airgap-test",
                "service.instance.id": "instance",
            },
            time_unix_nano=int(self.event_time.timestamp() * 1_000_000_000),
        )
        statement = b"airgap-log-checkpoint-v1\x00"
        for value in (
            "v1",
            "SHA256",
            "Ed25519",
            self.signer_id,
            key_id,
            "run",
            "segment",
            "window",
            "airgap-test",
            "instance",
        ):
            statement += verifier.frame_text(value)
        statement += (1).to_bytes(8, "big")
        statement += (0).to_bytes(8, "big")
        statement += (1).to_bytes(8, "big")
        statement += verifier.frame(b"a" * 32)
        statement += verifier.frame(b"b" * 32)
        checkpoint = hashlib.sha256(statement).digest()
        attributes["airgap.log.checkpoint.hash"] = checkpoint.hex()
        attributes["airgap.log.signature"] = verifier.base64.b64encode(
            self.leaf_key.sign(checkpoint)
        ).decode()

        verifier.verify_signature(
            result,
            {
                key_id: verifier.TrustedKey(
                    key_id=key_id,
                    signer_id=self.signer_id,
                    public_key_pem=material.public_key_pem,
                    source="test CA",
                    certificate_not_before=material.not_before,
                    certificate_not_after=material.not_after,
                )
            },
        )

        self.assertTrue(result.ok)
        self.assertEqual(result.warning_count, 1)
        self.assertTrue(any("🧣 expired now" in line for line in result.lines))
        self.assertIn("1 warning(s) 🧣", verifier.make_summary([result]))
        self.assertEqual(verifier.overall_result(result.ok), "Overall result: ✅")

    def test_certificate_expired_at_event_time_fails(self) -> None:
        with self.assertRaisesRegex(
            verifier.VerificationFailure,
            "had expired at event time",
        ):
            verifier.certificate_public_key(
                self.leaf.public_bytes(serialization.Encoding.PEM).decode(),
                self.signer_id,
                "42",
                self.now,
            )


if __name__ == "__main__":
    unittest.main()
