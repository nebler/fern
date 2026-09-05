"""Offline GPG fixtures exercise the build-only verifier, without Docker."""

import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


SOURCE = Path(__file__).resolve().parents[2] / "images/opencode-background-source"
PIN = "968479A1AFF927E37D1A566BB5690EEEBB952194"


@unittest.skipUnless(shutil.which("git") and shutil.which("gpg") and shutil.which("gpgconf"),
                     "requires git, gpg, and gpgconf")
class SourceSignatureTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory(prefix="fern-signature-")
        cls.root = Path(cls.temp.name)
        cls.home = cls.root / "gpg"
        cls.home.mkdir(mode=0o700)
        cls.env = dict(os.environ, GNUPGHOME=str(cls.home), GIT_CONFIG_NOSYSTEM="1",
                       GIT_CONFIG_GLOBAL=os.devnull)
        cls.run_command("gpg", "--batch", "--pinentry-mode", "loopback", "--passphrase", "",
                        "--quick-generate-key", "Offline fixture <fixture@example.invalid>",
                        "ed25519", "sign", "0")
        listing = cls.run_command("gpg", "--with-colons", "--list-keys").stdout
        cls.fingerprint = next(line.split(":")[9] for line in listing.splitlines()
                               if line.startswith("fpr:"))
        cls.key = cls.root / "fixture.asc"
        cls.key.write_text(cls.run_command("gpg", "--armor", "--export", cls.fingerprint).stdout)
        cls.run_command("git", "init", "-q")
        cls.run_command("git", "config", "user.name", "Offline fixture")
        cls.run_command("git", "config", "user.email", "fixture@example.invalid")
        cls.run_command("git", "config", "gpg.program", shutil.which("gpg"))
        cls.run_command("git", "commit", "--allow-empty", "--no-gpg-sign", "-m", "unsigned")
        cls.unsigned = cls.run_command("git", "rev-parse", "HEAD").stdout.strip()
        cls.run_command("git", "-c", "gpg.format=openpgp", "commit", "--allow-empty",
                        f"-S{cls.fingerprint}", "-m", "signed")
        cls.signed = cls.run_command("git", "rev-parse", "HEAD").stdout.strip()
        raw = cls.run_command("git", "cat-file", "commit", cls.signed).stdout
        cls.invalid = cls.run_command("git", "hash-object", "-t", "commit", "-w", "--stdin",
                                      input=raw.replace("\nsigned\n", "\ntampered\n")).stdout.strip()
        # Only the test copy trusts an ephemeral key; production has no override.
        cls.verifier = cls.root / "fixture-verifier"
        cls.verifier.write_text((SOURCE / "verify-source-signature").read_text().replace(
            f"expected={PIN}", f"expected={cls.fingerprint}"))

    @classmethod
    def run_command(cls, *args, **kwargs):
        return subprocess.run(args, cwd=cls.root, env=cls.env, text=True,
                              capture_output=True, check=True, **kwargs)

    @classmethod
    def tearDownClass(cls):
        subprocess.run(["gpgconf", "--homedir", str(cls.home), "--kill", "gpg-agent"],
                       capture_output=True)
        cls.temp.cleanup()

    def verify(self, commit, key=None, verifier=None):
        return subprocess.run(["sh", str(verifier or self.verifier), str(key or self.key), commit],
                              cwd=self.root, env=self.env, text=True, capture_output=True)

    def test_valid_signature(self):
        result = self.verify(self.signed)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(self.fingerprint, result.stdout)

    def test_missing_signature(self):
        result = self.verify(self.unsigned)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("signature verification failed", result.stderr)

    def test_invalid_signature(self):
        result = self.verify(self.invalid)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("signature verification failed", result.stderr)

    def test_unapproved_signature_ignores_ambient_keyring(self):
        result = self.verify(self.signed, SOURCE / "github-web-flow.asc",
                             SOURCE / "verify-source-signature")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("NO_PUBKEY", result.stderr)

    def test_unapproved_key(self):
        result = self.verify(self.signed, verifier=SOURCE / "verify-source-signature")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unapproved source signing key", result.stderr)

    def test_extra_primary_key(self):
        bundle = self.root / "bundle.asc"
        bundle.write_text(self.key.read_text() + (SOURCE / "github-web-flow.asc").read_text())
        result = self.verify(self.signed, bundle)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unapproved source signing key", result.stderr)

    def test_malformed_key(self):
        key = self.root / "malformed.asc"
        key.write_text("not a public key\n")
        result = self.verify(self.signed, key)
        self.assertNotEqual(result.returncode, 0)

    def test_missing_key(self):
        result = self.verify(self.signed, self.root / "missing.asc")
        self.assertNotEqual(result.returncode, 0)

    def test_raw_status_requires_exact_full_fingerprints(self):
        # Real GPG covers crypto above. Stub Git here to exercise strict status
        # parsing even if a verifier returned success with unexpected output.
        bin_dir = self.root / "status-bin"
        bin_dir.mkdir()
        git = bin_dir / "git"
        git.write_text('#!/bin/sh\nprintf "%s\\n" "$FIXTURE_STATUS"\n')
        git.chmod(0o755)
        fingerprint = self.fingerprint
        valid = f"[GNUPG:] VALIDSIG {fingerprint} 2026-01-01 1 0 4 0 22 8 00 {fingerprint}"
        cases = {
            "exact": (valid, True),
            "short signer": (valid.replace(fingerprint, fingerprint[-16:], 1), False),
            "wrong primary": (valid.rsplit(" ", 1)[0] + " " + PIN, False),
            "duplicate": (valid + "\n" + valid, False),
            "goodsig only": (f"[GNUPG:] GOODSIG {fingerprint[-16:]} Fixture", False),
            "extra field": (valid + " extra", False),
        }
        for name, (status, accepted) in cases.items():
            with self.subTest(name=name):
                env = dict(self.env, PATH=str(bin_dir) + os.pathsep + os.environ["PATH"],
                           FIXTURE_STATUS=status)
                result = subprocess.run(["sh", str(self.verifier), str(self.key), self.signed],
                                        cwd=self.root, env=env, text=True, capture_output=True)
                self.assertEqual(result.returncode == 0, accepted, result.stderr)


if __name__ == "__main__":
    unittest.main()
