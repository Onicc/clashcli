"""Offline installer failure tests. Every write is redirected into TemporaryDirectory.

The actual privileged Linux installation is tested separately by install_linux.py.
"""
import hashlib
import os
import pathlib
import shutil
import subprocess
import sys
import tempfile
import unittest

INSTALLER = pathlib.Path(__file__).resolve().parents[1] / "install.sh"
FAKE = r'''
import json, os, pathlib, shlex, shutil, subprocess, sys
name = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]
root = pathlib.Path(os.environ["INSTALL_TEST_ROOT"])
mode = os.environ.get("INSTALL_TEST_MODE", "")
with (root / "calls").open("a") as log:
    log.write(json.dumps([name, *args]) + "\n")
if name == "uname":
    print(os.environ.get("INSTALL_TEST_OS", "Linux") if args == ["-s"] else os.environ.get("INSTALL_TEST_ARCH", "x86_64"))
elif name == "id":
    print(os.environ.get("INSTALL_TEST_UID", "0"))
elif name == "curl":
    assert args[0] == "-q"
    for flag, value in [("--proto", "=https"), ("--proto-redir", "=https"), ("--tlsv1.2", None), ("--fail", None), ("--retry-all-errors", None), ("--max-time", "180")]:
        assert flag in args
        if value is not None:
            assert args[args.index(flag) + 1] == value
    url = args[-1]
    assert url.startswith("https://github.com/Onicc/clashcli/releases/")
    if url.endswith("/latest"):
        print("https://evil.invalid/v9.9.9" if mode == "redirect" else "https://github.com/Onicc/clashcli/releases/tag/v0.1.1", end="")
    else:
        dest = pathlib.Path(args[args.index("--output") + 1])
        if mode == "network":
            dest.write_bytes(b"partial")
            sys.exit(22)
        if url.endswith("SHA256SUMS"):
            dest.write_bytes((root / "manifest").read_bytes())
        else:
            assert url.endswith("/clashcli-linux-" + os.environ.get("INSTALL_TEST_EXPECT_ARCH", "amd64"))
            assert "/download/" + os.environ.get("INSTALL_TEST_EXPECT_VERSION", "v0.1.1") + "/" in url
            dest.write_bytes((root / "binary").read_bytes())
elif name == "mktemp":
    args = [a.replace("/tmp/clashcli-install.", str(root / "downloads" / "clashcli-install.")) for a in args]
    os.execv(os.environ["INSTALL_TEST_MKTEMP"], ["mktemp", *args])
elif name == "sudo":
    assert args[0:2] == ["sh", "-c"]
    if mode == "sudo":
        sys.exit(1)
    os.execv(str(root / "tools" / "sh"), ["sh", *args[1:]])
elif name == "sh":
    assert args[0] == "-c"
    script = args[1].replace("/usr/local/bin", str(root / "bin"))
    assert "/usr/local" not in script
    script = script.replace("PATH=/usr/sbin:/usr/bin:/sbin:/bin", "PATH=" + shlex.quote(os.environ["PATH"]))
    if mode == "tamper":
        pathlib.Path(args[3]).write_bytes(b"tampered after first verification")
    os.execv("/bin/sh", ["sh", "-c", script, *args[2:]])
elif name == "install":
    if mode == "disk":
        sys.exit(1)
    source, target = map(pathlib.Path, args[-2:])
    assert root in target.parents
    shutil.copyfile(source, target)
    target.chmod(0o755)
elif name == "mv":
    if mode == "rename":
        sys.exit(1)
    source, target = map(pathlib.Path, args[-2:])
    assert root in target.parents
    os.replace(source, target)
else:
    raise AssertionError(name)
'''


class InstallerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="clashcli-installer-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = pathlib.Path(self.temp.name)
        for name in ["tools", "downloads", "bin"]:
            (self.root / name).mkdir()
        self.env = dict(os.environ, INSTALL_TEST_ROOT=str(self.root), INSTALL_TEST_MKTEMP=shutil.which("mktemp"))
        for name in ["curl", "uname", "id", "sudo", "sh", "mktemp", "install", "mv"]:
            tool = self.root / "tools" / name
            tool.write_text("#!" + sys.executable + "\n" + FAKE)
            tool.chmod(0o755)
        self.env["PATH"] = str(self.root / "tools") + os.pathsep + os.environ["PATH"]
        self.target = self.root / "bin" / "clashcli"
        self.target.write_bytes(b"original binary")
        self.binary = b'#!/bin/sh\nprintf "%s\\n" "clashcli version v0.1.1"\n'
        (self.root / "binary").write_bytes(self.binary)
        self.digest = hashlib.sha256(self.binary).hexdigest()
        self.manifest()

    def manifest(self, content=None):
        if content is None:
            content = self.digest + "  clashcli-linux-amd64\n" + self.digest + "  clashcli-linux-arm64\n"
        (self.root / "manifest").write_text(content)

    def invoke(self, *args, success=True, **env):
        result = subprocess.run(["/bin/sh", str(INSTALLER), *args], env=dict(self.env, **env), stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=30)
        self.assertEqual(result.returncode == 0, success, result.stdout.decode())
        self.assertEqual(list((self.root / "downloads").iterdir()), [])
        self.assertEqual(list((self.root / "bin").glob(".clashcli-install.*")), [])
        if not success:
            self.assertEqual(self.target.read_bytes(), b"original binary")
        return result

    def test_latest_and_atomic_upgrade(self):
        with self.target.open("rb") as old:
            self.invoke()
            self.assertEqual(old.read(), b"original binary")
        self.assertEqual(self.target.read_bytes(), self.binary)
        self.assertEqual(self.target.stat().st_mode & 0o777, 0o755)
        self.invoke()  # Idempotent reinstall.

    def test_first_install(self):
        self.target.unlink()
        self.invoke()
        self.assertEqual(self.target.read_bytes(), self.binary)

    def test_pinned_version_and_sudo(self):
        self.invoke("--version", "v0.1.1", INSTALL_TEST_UID="1000")
        log = (self.root / "calls").read_text()
        self.assertNotIn("/releases/latest", log)
        self.assertIn('["sudo", "sh", "-c"', log)

    def test_arm64_aliases(self):
        for arch in ["arm64", "aarch64"]:
            with self.subTest(arch=arch):
                self.invoke(INSTALL_TEST_ARCH=arch, INSTALL_TEST_EXPECT_ARCH="arm64")

    def test_unsupported_platforms(self):
        self.invoke(success=False, INSTALL_TEST_OS="Darwin")
        self.invoke(success=False, INSTALL_TEST_ARCH="riscv64")

    def test_bad_arguments(self):
        for args in [["--version"], ["--version", "../../evil"], ["--version", "v1.2.3\nevil"], ["--unknown"]]:
            with self.subTest(args=args):
                self.invoke(*args, success=False)

    def test_help_is_offline(self):
        self.invoke("--help", INSTALL_TEST_OS="Darwin")
        self.assertFalse((self.root / "calls").exists())

    def test_bad_checksums(self):
        entry = self.digest + "  clashcli-linux-amd64\n"
        for content in ["", "0" * 64 + "  clashcli-linux-amd64\n", "invalid  clashcli-linux-amd64\n", entry * 2, entry.strip() + " extra\n", self.digest + "  /etc/passwd\n"]:
            with self.subTest(content=content):
                self.manifest(content)
                self.invoke(success=False)

    def test_failure_preserves_old_binary_and_cleans_up(self):
        for mode in ["redirect", "network", "disk", "rename", "tamper", "sudo"]:
            with self.subTest(mode=mode):
                self.invoke(success=False, INSTALL_TEST_MODE=mode, INSTALL_TEST_UID="1000")

    def test_version_mismatch(self):
        self.invoke("--version", "v0.1.0", success=False, INSTALL_TEST_EXPECT_VERSION="v0.1.0")

    def test_symlink_refused(self):
        self.target.rename(self.root / "original")
        self.target.symlink_to(self.root / "original")
        self.invoke(success=False)
        self.assertTrue(self.target.is_symlink())

    def test_bad_binary_not_installed(self):
        body = b"not an executable"
        (self.root / "binary").write_bytes(body)
        self.manifest(hashlib.sha256(body).hexdigest() + "  clashcli-linux-amd64\n")
        self.invoke(success=False)


if __name__ == "__main__":
    unittest.main()
