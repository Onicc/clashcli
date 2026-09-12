"""Exercise the real release installer only inside the disposable Linux subject."""
import os
import pathlib
import pwd
import subprocess


def run(args):
    result = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=600)
    if result.returncode:
        # This phase has no subscriptions or private credentials. Preserve PAM
        # diagnostics from the disposable account when a distro fixture fails.
        journal = subprocess.run(["journalctl", "--no-pager", "-t", "sudo", "-n", "15"], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=15)
        helper = subprocess.run(["journalctl", "--no-pager", "_COMM=unix_chkpwd", "-n", "15"], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=15)
        capabilities = "\n".join(line for line in pathlib.Path("/proc/self/status").read_text().splitlines() if line.startswith(("Cap", "NoNewPrivs")))
        raise AssertionError(result.stdout.decode(errors="replace")[-4000:] + "\n" + journal.stdout.decode(errors="replace")[-1500:] + "\n" + helper.stdout.decode(errors="replace")[-1000:] + "\n" + capabilities)
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
shadow = pathlib.Path("/etc/shadow")
shadow_mode = shadow.stat().st_mode & 0o777
try:
    # Ubuntu host AppArmor's unix-chkpwd profile denies the DAC capabilities
    # needed by Fedora's mode-000 shadow file, even in privileged containers.
    # Keep it root-only but directly readable for this disposable sudo fixture;
    # restore afterwards. Never change the host profile or the user's PAM policy.
    # https://gitlab.com/apparmor/apparmor/-/issues/402
    if pathlib.Path("/etc/fedora-release").exists() and shadow_mode == 0:
        shadow.chmod(0o600)
    run(["runuser", "-u", "clashcli-installer-test", "--", "sudo", "-n", "true"])
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
    try:
        sudoers.unlink()
        # PAM may start a lingering systemd user manager (notably on Arch).
        uid = str(pwd.getpwnam("clashcli-installer-test").pw_uid)
        run(["systemctl", "stop", "user@" + uid + ".service", "user-runtime-dir@" + uid + ".service"])
        run(["userdel", "-r", "clashcli-installer-test"])
        target.unlink()
    finally:
        shadow.chmod(shadow_mode)
print("PASS real HTTPS release install, version pin, sudo upgrade, atomic inode replacement and cleanup", flush=True)
