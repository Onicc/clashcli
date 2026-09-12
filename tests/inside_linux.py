"""Acceptance assertions executed inside the disposable systemd container."""
import json
import os
import pathlib
import re
import socket
import struct
import subprocess
import sys
import time
import urllib.request

FIXTURE = "http://10.231.78.2:8000"
CLIENT = urllib.request.build_opener(urllib.request.ProxyHandler({}))
LIVE = json.load(sys.stdin)


def run(args, data=None, ok=True, timeout=240):
    result = subprocess.run(args, input=data, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=timeout)
    if ok and result.returncode:
        # The deterministic suite contains no real credentials. Live failures are handled separately.
        raise AssertionError(" ".join(args[:4]) + "\n" + result.stdout.decode(errors="replace")[-6000:])
    return result


def cli(*args, **kwargs):
    return run(["clashcli", *args], **kwargs)


def request(path, state=None):
    req = urllib.request.Request(FIXTURE + path, data=json.dumps(state).encode() if state else None, headers={"Content-Type": "application/json"})
    with CLIENT.open(req, timeout=10) as response:
        return response.read()


def status():
    return json.loads(cli("status", "--json").stdout)


def passed(label):
    print("PASS " + label, flush=True)


run(["useradd", "-m", "-s", "/bin/bash", "fixtureuser"])
pathlib.Path("/etc/environment").write_text("# original\nCLASHCLI_TEST_SENTINEL=preserved\nhttp_proxy=http://previous.invalid:8080\n")
original_env = pathlib.Path("/etc/environment").read_bytes()
request("/state", {"mode": "invalid"})
result = cli("init", "--name", "fixture", "--url-stdin", "--no-timer", "--desktop", "none", data=(FIXTURE + "/subscription\n").encode(), timeout=600, ok=False)
assert result.returncode != 0 and not pathlib.Path("/var/lib/clashcli/current").exists()
request("/state", {"mode": "yaml"})
cli("init", timeout=600)
assert status()["core_running"]
assert not status()["desired"]["system_proxy"] and not status()["tun_interface"]
passed("failed-init recovery, verified downloads, systemd start, actual config")

for path in ["/etc/clashcli/config.yaml", "/var/lib/clashcli/current/source"]:
    assert os.stat(path).st_mode & 0o077 == 0
with CLIENT.open("http://127.0.0.1:9090/ui/", timeout=10) as r:
    assert b"<html" in r.read().lower()
try:
    CLIENT.open("http://127.0.0.1:9090/configs", timeout=10)
    raise AssertionError("unprotected controller")
except urllib.error.HTTPError as error:
    assert error.code == 401
assert "Secret:" not in cli("ui").stdout.decode()
passed("external UI, API authentication, private file permissions")

# Exercise the documented SSH tunnel while TUN is toggled later in this test.
pathlib.Path("/run/sshd").mkdir(exist_ok=True)
run(["ssh-keygen", "-A"])
run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", "/tmp/clashcli-ssh"])
sshd = subprocess.Popen(["/usr/sbin/sshd", "-D", "-p", "2222", "-o", "ListenAddress=127.0.0.1", "-o", "AuthorizedKeysFile=/tmp/clashcli-ssh.pub", "-o", "StrictModes=no", "-o", "PasswordAuthentication=no"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
time.sleep(0.3)
forward = subprocess.Popen(["ssh", "-N", "-L", "127.0.0.1:19090:127.0.0.1:9090", "-p", "2222", "-i", "/tmp/clashcli-ssh", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/tmp/clashcli-known-hosts", "-o", "ExitOnForwardFailure=yes", "root@127.0.0.1"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
time.sleep(0.5)
with CLIENT.open("http://127.0.0.1:19090/ui/", timeout=10) as response:
    assert b"<html" in response.read().lower()

before = json.loads(request("/state"))["counts"].get("socks_tcp", 0)
response = run(["curl", "--fail", "--silent", "--noproxy", "", "--proxy", "http://127.0.0.1:7890", FIXTURE + "/origin"])
assert json.loads(response.stdout)["origin"] == "clashcli-test"
assert json.loads(request("/state"))["counts"].get("socks_tcp", 0) > before
passed("real HTTP proxy traffic traverses selected SOCKS node")

cli("proxy", "on")
assert status()["system_proxy"]["environment"]
assert b"http_proxy='http://127.0.0.1:7890'" in pathlib.Path("/etc/profile.d/clashcli.sh").read_bytes()
env = run(["su", "-", "fixtureuser", "-c", "env"]).stdout
assert b"http_proxy=http://127.0.0.1:7890" in env and b"CLASHCLI_TEST_SENTINEL=preserved" in env
cli("proxy", "on")
cli("proxy", "off")
assert pathlib.Path("/etc/environment").read_bytes() == original_env
assert not pathlib.Path("/etc/profile.d/clashcli.sh").exists()
passed("persistent system proxy, fresh login, idempotence, exact restoration")

original_generation = status()["generation"]
cli("sub", "update")
assert status()["generation"] == original_generation
request("/state", {"version": 2})
old_pid = run(["systemctl", "show", "clashcli", "-p", "MainPID", "--value"]).stdout
cli("sub", "update")
assert status()["generation"] != original_generation
assert old_pid == run(["systemctl", "show", "clashcli", "-p", "MainPID", "--value"]).stdout
assert "fixture-v2" in cli("nodes").stdout.decode()
cli("rollback")
assert "fixture-v1" in cli("nodes").stdout.decode()
cli("sub", "update", "--force")
assert "fixture-v2" in cli("nodes").stdout.decode()
good_generation = status()["generation"]
workers = [subprocess.Popen(["clashcli", "sub", "update"], stdout=subprocess.PIPE, stderr=subprocess.PIPE) for _ in range(2)]
for worker in workers:
    worker.communicate(timeout=180)
    assert worker.returncode == 0
assert status()["generation"] == good_generation
for state in [{"mode": "invalid", "version": 3}, {"mode": "yaml", "version": 4, "bad_provider": True}]:
    request("/state", state)
    result = cli("sub", "update", "--force", ok=False)
    assert result.returncode != 0 and status()["generation"] == good_generation
    assert status()["core_running"]
request("/state", {"mode": "yaml", "version": 2, "bad_provider": False})
passed("304, atomic reload without restart, invalid config/provider preserves active version")

# A real runtime-only TUN failure must roll back disk and API state.
override = pathlib.Path("/run/systemd/system/clashcli.service.d")
override.mkdir(parents=True, exist_ok=True)
(override / "test-capabilities.conf").write_text("[Service]\nCapabilityBoundingSet=\nCapabilityBoundingSet=CAP_NET_RAW CAP_NET_BIND_SERVICE CAP_DAC_READ_SEARCH\n")
run(["systemctl", "daemon-reload"])
cli("restart")
result = cli("tun", "on", ok=False, timeout=180)
assert result.returncode != 0 and status()["generation"] == good_generation
assert not status()["desired"]["tun"] and status()["core_running"]
(override / "test-capabilities.conf").unlink()
override.rmdir()
run(["systemctl", "daemon-reload"])
run(["systemctl", "reset-failed", "clashcli.service"])
cli("restart")
passed("actual TUN permission failure rolls back active config and runtime")

cli("tun", "on", timeout=180)
assert status()["desired"]["tun"] and status()["tun_interface"]
before = json.loads(request("/state"))["counts"].get("socks_tcp", 0)
# Raw sockets and ProxyHandler({}) never inherit proxy environment variables.
with CLIENT.open("http://192.0.2.2:8000/origin", timeout=10) as response:
    assert json.loads(response.read())["origin"] == "clashcli-test"
assert json.loads(request("/state"))["counts"].get("socks_tcp", 0) > before
udp = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
udp.settimeout(10)
udp.sendto(b"clashcli-udp", ("192.0.2.2", 9999))
assert udp.recvfrom(1024)[0] == b"clashcli-udp"
udp.close()
assert json.loads(request("/state"))["counts"].get("socks_udp", 0) > 0
dns = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
dns.settimeout(10)
query = b"\x12\x34\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00\x06origin\x04test\x00\x00\x01\x00\x01"
dns.sendto(query, ("8.8.8.8", 53))
answer = dns.recvfrom(4096)[0]
dns.close()
assert answer[:2] == b"\x12\x34" and socket.inet_aton("10.231.78.2") in answer
with CLIENT.open("http://[2001:db8::2]:8000/origin", timeout=10) as response:
    assert json.loads(response.read())["origin"] == "clashcli-test"
with CLIENT.open("http://127.0.0.1:19090/ui/", timeout=10) as response:
    assert b"<html" in response.read().lower()
assert forward.poll() is None
cli("tun", "off")
assert not status()["tun_interface"]
rules = run(["ip", "rule", "show"]).stdout
assert b"2022" not in rules
passed("TUN actual IPv4/IPv6 TCP, SOCKS UDP relay, DNS hijack, interface and route cleanup")
forward.terminate()
forward.wait(timeout=10)
sshd.terminate()
sshd.wait(timeout=10)
passed("SSH forwarding remains reachable through TUN toggles")

request("/state", {"mode": "base64", "version": 5})
cli("sub", "add", "base64", "--url-stdin", data=(FIXTURE + "/subscription\n").encode())
assert status()["generation"] == good_generation or "fixture-v2" in cli("nodes").stdout.decode()
cli("sub", "use", "base64")
assert "fixture-v5" in cli("nodes").stdout.decode()
cli("select", "PROXY", "fixture-v5")
cli("sub", "use", "fixture")
cli("sub", "schedule", "base64", "--every", "off")
cli("sub", "remove", "base64")
passed("native Base64 import, inactive update, subscription switch, node selection")

request("/state", {"mode": "yaml", "version": 6})
cli("sub", "schedule", "fixture", "--every", "*-*-* *:*:00/5")
deadline = time.time() + 90
while time.time() < deadline:
    if "fixture-v6" in cli("nodes").stdout.decode():
        break
    time.sleep(2)
else:
    raise AssertionError("real systemd timer did not update")
cli("sub", "schedule", "fixture", "--every", "off")
passed("real systemd calendar-triggered update")

cli("proxy", "on")
cli("stop")
assert pathlib.Path("/etc/environment").read_bytes() == original_env
assert not status()["core_running"]
cli("start")
assert status()["system_proxy"]["environment"]
cli("proxy", "off")
run(["systemctl", "kill", "--signal=KILL", "--kill-whom=main", "clashcli.service"])
deadline = time.time() + 30
while time.time() < deadline:
    if status()["core_running"]:
        break
    time.sleep(1)
assert status()["core_running"]
passed("stop restores proxy, start restores preference, systemd crash restart")

# Use real GNOME and KDE settings in a disposable user's session.
import yaml
settings_file = pathlib.Path("/etc/clashcli/config.yaml")
uid = int(run(["id", "-u", "fixtureuser"]).stdout)
for desktop in ["gnome", "kde"]:
    content = re.sub(r"(?m)^desktop:.*$", "desktop: " + desktop, settings_file.read_text())
    content = re.sub(r"(?m)^desktop_uid:.*$", "desktop_uid: " + str(uid), content)
    settings_file.write_text(content)
    cli("proxy", "on")
    assert status()["system_proxy"]["desktop"] is True
    if desktop == "gnome":
        before = json.loads(request("/state"))["counts"].get("socks_tcp", 0)
        script = "from gi.repository import Gio; r=Gio.ProxyResolver.get_default(); assert '127.0.0.1:7890' in str(r.lookup('http://192.0.2.2:8000',None)); c=Gio.SocketClient().connect_to_uri('http://192.0.2.2:8000',8000,None); c.get_output_stream().write_all(b'GET /origin HTTP/1.0\\r\\nHost: origin.test\\r\\n\\r\\n',None); assert b'200' in c.get_input_stream().read_bytes(1000,None).get_data(); c.close(None)"
        run(["runuser", "-u", "fixtureuser", "--", "env", "-i", "PATH=/usr/bin:/bin", "HOME=/home/fixtureuser", "XDG_CURRENT_DESKTOP=GNOME", "dbus-run-session", "--", "python3", "-c", script])
        assert json.loads(request("/state"))["counts"].get("socks_tcp", 0) > before
        passed("GNOME-aware GIO application actually traverses system proxy")
    cli("proxy", "off")
    assert pathlib.Path("/etc/environment").read_bytes() == original_env
    passed("real " + desktop + " persistence and restore")
settings_file.write_text(re.sub(r"(?m)^desktop:.*$", "desktop: none", settings_file.read_text()))

for index, url in enumerate(LIVE, 1):
    result = cli("sub", "add", "live-" + str(index), "--url-stdin", data=(url + "\n").encode(), ok=False, timeout=600)
    # Never include provider output, URLs or identifiers in public test output.
    assert result.returncode == 0, f"live subscription {index}: import failed (private output suppressed)"
    result = cli("sub", "use", "live-" + str(index), ok=False, timeout=600)
    assert result.returncode == 0, f"live subscription {index}: activation failed (private output suppressed)"
    assert status()["core_running"]
    passed(f"private live subscription {index}: import and reload")
    settings = yaml.safe_load(settings_file.read_text())
    def live_api(path, method="GET", body=None):
        req = urllib.request.Request("http://127.0.0.1:9090" + path, method=method,
            data=json.dumps(body).encode() if body is not None else None,
            headers={"Authorization": "Bearer " + settings["secret"], "Content-Type": "application/json"})
        with CLIENT.open(req, timeout=20) as response:
            data = response.read()
            return json.loads(data) if data else None
    proxies = live_api("/proxies")["proxies"]
    candidates = [name for name, value in proxies.items() if value.get("type") not in
        ("Direct", "Reject", "RejectDrop", "Pass", "Compatible", "Selector", "URLTest", "Fallback", "LoadBalance")]
    connected = False
    live_api("/configs", "PATCH", {"mode": "global"})
    try:
        for node in candidates[:6]:
            if cli("select", "GLOBAL", node, ok=False).returncode:
                continue
            result = run(["curl", "--silent", "--fail", "--max-time", "20", "--noproxy", "", "--proxy", "http://127.0.0.1:7890", "https://www.gstatic.com/generate_204", "-o", "/dev/null", "-w", "%{http_code}"], ok=False, timeout=25)
            if result.returncode == 0 and result.stdout == b"204":
                connected = True
                break
    finally:
        live_api("/configs", "PATCH", {"mode": "rule"})
    if connected:
        passed(f"private live subscription {index}: real HTTPS 204 through selected node")
    else:
        print(f"UNAVAILABLE private live subscription {index}: tested nodes did not complete HTTPS probe", flush=True)
cli("sub", "use", "fixture")

cli("logs", "-n", "5")
cli("doctor")
# Reinstallation after a non-purging uninstall preserves private subscriptions.
run(["cp", "/usr/local/bin/clashcli", "/root/clashcli-reinstall"])
cli("uninstall", "--yes")
assert settings_file.exists() and pathlib.Path("/var/lib/clashcli/current").exists()
run(["/root/clashcli-reinstall", "init"], timeout=600)
assert status()["core_running"]
cli("uninstall", "--purge", "--yes")
assert pathlib.Path("/etc/environment").read_bytes() == original_env
for path in ["/usr/local/bin/clashcli", "/usr/local/lib/clashcli", "/etc/clashcli", "/var/lib/clashcli", "/etc/systemd/system/clashcli.service", "/etc/profile.d/clashcli.sh"]:
    assert not pathlib.Path(path).exists(), "uninstall left " + path
assert not list(pathlib.Path("/var/lib/systemd/timers").glob("*clashcli*"))
passed("logs, doctor, complete uninstall and timer-state cleanup")
