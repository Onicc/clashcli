"""Exercise the real release installer only inside the disposable Linux subject."""
import os
import pathlib
import pwd
import shutil
import subprocess
import time


def run(args):
    result = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=600)
    if result.returncode:
        # This phase has no subscriptions or private credentials. Preserve PAM
        # diagnostics from the disposable account when a distro fixture fails.
        journal = subprocess.run(["journalctl", "--no-pager", "-t", "sudo", "-n", "15"], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=15)
        helper = subprocess.run(["journalctl", "--no-pager", "_COMM=unix_chkpwd", "-n", "15"], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=15)
        capabilities = "\n".join(line for line in pathlib.Path("/proc/self/status").read_text().splitlines() if line.startswith(("Cap", "NoNewPrivs")))
        identity = []
        for lookup in [["getent", "shadow", "clashcli-installer-test"], ["getent", "-s", "files", "shadow", "clashcli-installer-test"], ["/usr/sbin/unix_chkpwd", "clashcli-installer-test", "chkexpiry"]]:
            if pathlib.Path(lookup[0]).is_absolute() and not pathlib.Path(lookup[0]).exists():
                continue
            probe = subprocess.run(lookup, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10)
            # Never log shadow records, even for this disposable user.
            identity.append(" ".join(lookup) + f": exit={probe.returncode}, records={len(probe.stdout.splitlines())}")
        trace_text = ""
        if shutil.which("strace") and pwd.getpwnam("clashcli-installer-test"):
            trace = subprocess.run(["strace", "-f", "-e", "trace=openat,execve,setuid,setresuid,setfsuid,capset", "runuser", "-u", "clashcli-installer-test", "--", "sudo", "-n", "true"], stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=20)
            trace_text = "\n".join(line for line in trace.stderr.decode(errors="replace").splitlines() if any(key in line for key in ["/etc/shadow", "unix_chkpwd", "setuid(", "setresuid(", "capset("]))[-2500:]
        raise AssertionError(result.stdout.decode(errors="replace")[-3000:] + "\n" + journal.stdout.decode(errors="replace")[-1500:] + "\n" + helper.stdout.decode(errors="replace")[-1000:] + "\n" + capabilities + "\n" + "\n".join(identity) + "\n" + trace_text)
    return result.stdout


assert os.environ.get("CLASHCLI_INSTALL_TEST_CONTAINER") == "1"
target = pathlib.Path("/usr/local/bin/clashcli")
assert not target.exists()
baseline = set(pathlib.Path("/tmp").glob("clashcli-install.*"))
# Root install from an immutable published tag.
run(["sh", "/install.sh", "--version", "v0.1.0"])
assert run([str(target), "--version"]).strip() == b"clashcli version v0.1.0"
assert target.stat().st_uid == 0 and target.stat().st_mode & 0o777 == 0o755
assert not pathlib.Path("/etc/clashcli/config.yaml").exists()
assert not pathlib.Path("/etc/systemd/system/clashcli.service").exists()

# A non-root upgrade exercises the real sudo path. Only this disposable user
# receives temporary non-interactive sudo; clashcli itself never creates it.
run(["useradd", "-m", "-s", "/bin/bash", "clashcli-installer-test"])
run(["usermod", "-p", "*", "clashcli-installer-test"])
sudoers = pathlib.Path("/etc/sudoers.d/clashcli-installer-test")
sudoers.write_text("clashcli-installer-test ALL=(root) NOPASSWD: ALL\n")
sudoers.chmod(0o440)
environment = pathlib.Path("/etc/environment")
before = environment.read_bytes() if environment.exists() else None
try:
    # A booted image may have asynchronous NSS/password-record services. The
    # disposable user must become visible to PAM before testing installation.
    preflight = ["runuser", "-u", "clashcli-installer-test", "--", "sudo", "-n", "true"]
    deadline = time.monotonic() + 30
    while subprocess.run(preflight, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10).returncode:
        if time.monotonic() >= deadline:
            run(preflight)  # Fail with diagnostics, never skip a broken fixture.
        time.sleep(1)
    with target.open("rb") as old:
        old_inode = os.fstat(old.fileno()).st_ino
        run(["runuser", "-u", "clashcli-installer-test", "--", "sh", "/install.sh"])
        assert target.stat().st_ino != old_inode
        assert old.read(4) == b"\x7fELF"  # Old inode remains usable through upgrade.
    assert run([str(target), "--version"]).startswith(b"clashcli version v")
    assert target.stat().st_uid == 0 and target.stat().st_mode & 0o777 == 0o755
    assert (environment.read_bytes() if environment.exists() else None) == before
    assert not pathlib.Path("/etc/clashcli/config.yaml").exists()
    assert not pathlib.Path("/etc/systemd/system/clashcli.service").exists()
    assert set(pathlib.Path("/tmp").glob("clashcli-install.*")) == baseline
    assert not list(target.parent.glob(".clashcli-install.*"))
finally:
    sudoers.unlink()
    # PAM may start a lingering systemd user manager (notably on Arch).
    uid = str(pwd.getpwnam("clashcli-installer-test").pw_uid)
    run(["systemctl", "stop", "user@" + uid + ".service", "user-runtime-dir@" + uid + ".service"])
    run(["userdel", "-r", "clashcli-installer-test"])
    target.unlink()
print("PASS real HTTPS release install, version pin, sudo upgrade, atomic inode replacement and cleanup", flush=True)
