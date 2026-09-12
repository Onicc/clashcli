#!/usr/bin/env python3
"""Run real Linux tests, deleting only resources created by this invocation.

Requires an already running Docker engine. No host networking or host cgroup mounts.
All dependency downloads happen inside disposable containers/builders.
"""
import argparse
import json
import os
import pathlib
import platform
import shutil
import signal
import subprocess
import tempfile
import time
import uuid

REPO = pathlib.Path(__file__).resolve().parents[1]


def run(args, *, data=None, check=True, timeout=900, env=None):
    result = subprocess.run(args, input=data, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=timeout, env=env)
    if check and result.returncode:
        # These commands never contain subscriptions or tokens in argv.
        raise RuntimeError("command failed: " + " ".join(args[:4]) + "\n" + result.stdout.decode(errors="replace")[-10000:])
    return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--platform", default="linux/arm64" if platform.machine() in ("arm64", "aarch64") else "linux/amd64")
    parser.add_argument("--distro", choices=["ubuntu", "debian", "fedora", "arch"], default="ubuntu")
    parser.add_argument("--live-stdin", action="store_true", help="read private JSON array of live URLs from stdin; never print it")
    parser.add_argument("--image", help="use an existing test image; it remains owned by its caller")
    parser.add_argument("--coverage", action="store_true", help="instrument the Linux CLI and report real integration coverage")
    parser.add_argument("--ui-review", action="store_true", help="pause for browser review at localhost:9090; touch /run/clashcli/ui-review-done in the subject to continue")
    options = parser.parse_args()
    live = []
    if options.live_stdin:
        import sys
        live = json.load(sys.stdin)
    token = "clashcli-test-" + uuid.uuid4().hex[:10]
    image, builder, network = token + ":linux", token + "-builder", token + "-net"
    subject, fixture = token + "-subject", token + "-fixture"
    # Fail before allocating resources when Docker is unavailable.
    baseline = {kind: set(run(["docker", kind, "ls", "-q"]).stdout.decode().split()) for kind in ["container", "image", "volume", "network"]}
    temp = pathlib.Path(tempfile.mkdtemp(prefix="clashcli-test-"))
    containers = []
    built, builder_created, network_created = False, False, False
    cleanup_errors = []
    signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))
    try:
        binary = REPO / ("dist/clashcli-linux-" + options.platform.split("/")[-1])
        if options.coverage:
            binary = temp / "clashcli"
            env = dict(os.environ, GOOS="linux", GOARCH=options.platform.split("/")[-1], CGO_ENABLED="0")
            run(["go", "build", "-cover", "-o", str(binary), "./cmd/clashcli"], env=env)
        print(f"TEST {options.distro} {options.platform}: building isolated image", flush=True)
        if options.image:
            image = options.image
        else:
            run(["docker", "buildx", "create", "--name", builder, "--driver", "docker-container", "--driver-opt", "image=moby/buildkit:buildx-stable-1"])
            builder_created = True
            dockerfile = "tests/Dockerfile" if options.distro == "ubuntu" else "tests/Dockerfile." + options.distro
            result = run(["docker", "buildx", "build", "--builder", builder, "--platform", options.platform, "--load", "--label", "io.clashcli.test=" + token, "-t", image, "-f", str(REPO / dockerfile), str(REPO)], timeout=1800)
            built = True
        run(["docker", "network", "create", "--label", "io.clashcli.test=" + token, "--subnet", "10.231.78.0/24", "--ipv6", "--subnet", "fd42:231:78::/64", network])
        network_created = True
        run(["docker", "create", "--platform", options.platform, "--name", fixture, "--label", "io.clashcli.test=" + token, "--network", network, "--ip", "10.231.78.2", "--entrypoint", "python3", image, "/fixture_server.py"])
        containers.append(fixture)
        run(["docker", "cp", str(REPO / "tests/fixture_server.py"), fixture + ":/fixture_server.py"])
        run(["docker", "start", fixture])
        publish = ["--publish", "127.0.0.1:9090:19091"] if options.ui_review else []
        run(["docker", "create", "--platform", options.platform, "--name", subject, "--label", "io.clashcli.test=" + token, "--network", network, "--ip", "10.231.78.3", "--privileged", "--cgroupns=private", "--tmpfs", "/run", "--tmpfs", "/run/lock", "--tmpfs", "/tmp", *publish, image])
        containers.append(subject)
        run(["docker", "start", subject])
        run(["docker", "cp", str(REPO / "install.sh"), subject + ":/install.sh"])
        run(["docker", "cp", str(REPO / "tests/install_linux.py"), subject + ":/install_linux.py"])
        print(run(["docker", "exec", "-e", "CLASHCLI_INSTALL_TEST_CONTAINER=1", subject, "python3", "/install_linux.py"], timeout=1500).stdout.decode(), flush=True)
        run(["docker", "cp", str(binary), subject + ":/usr/local/bin/clashcli"])
        run(["docker", "cp", str(REPO / "tests/inside_linux.py"), subject + ":/inside_linux.py"])
        ready = False
        for _ in range(60):
            result = run(["docker", "exec", subject, "systemctl", "is-system-running"], check=False)
            if result.stdout.strip() in (b"running", b"degraded"):
                ready = True
                break
            time.sleep(1)
        if not ready:
            raise RuntimeError("real systemd unavailable: " + run(["docker", "logs", subject], check=False).stdout.decode()[-3000:])
        print("TEST systemd ready; installing and exercising real mihomo", flush=True)
        # Stream test progress. Live URLs enter the child over stdin only.
        run(["docker", "exec", subject, "mkdir", "-p", "/coverage"])
        proc = subprocess.Popen(["docker", "exec", "-i", "-e", "GOCOVERDIR=/coverage", "-e", "CLASHCLI_UI_REVIEW=" + str(int(options.ui_review)), "-e", "GITHUB_ACTIONS=" + os.environ.get("GITHUB_ACTIONS", ""), subject, "python3", "-u", "/inside_linux.py"], stdin=subprocess.PIPE)
        try:
            proc.communicate(json.dumps(live).encode(), timeout=1800)
        except BaseException:
            proc.terminate()
            proc.wait(timeout=10)
            raise
        if proc.returncode:
            raise RuntimeError("Linux acceptance suite failed")
        if options.coverage:
            run(["docker", "cp", subject + ":/coverage", str(temp / "coverage")])
            print(run(["go", "tool", "covdata", "percent", "-i=" + str(temp / "coverage")]).stdout.decode(), flush=True)
        print(f"PASS {options.distro} {options.platform}", flush=True)
    finally:
        for name in reversed(containers):
            result = run(["docker", "rm", "-fv", name], check=False)
            if result.returncode:
                cleanup_errors.append("container " + name)
        if network_created and run(["docker", "network", "rm", network], check=False).returncode:
            cleanup_errors.append("network " + network)
        if built and run(["docker", "image", "rm", image], check=False).returncode:
            cleanup_errors.append("image " + image)
        if builder_created and run(["docker", "buildx", "rm", builder], check=False).returncode:
            cleanup_errors.append("builder " + builder)
        shutil.rmtree(temp)
        remaining = run(["docker", "ps", "-aq", "--filter", "label=io.clashcli.test=" + token]).stdout.strip()
        if remaining:
            cleanup_errors.append("labeled containers remain")
        # All original Docker resources must still exist. Shared resources are never pruned.
        for kind, original in baseline.items():
            after = set(run(["docker", kind, "ls", "-q"]).stdout.decode().split())
            if not original.issubset(after):
                cleanup_errors.append("pre-existing " + kind + " changed")
        if cleanup_errors:
            raise RuntimeError("cleanup incomplete: " + ", ".join(cleanup_errors))
        print("CLEANUP PASS: dedicated containers/network/image/builder removed; existing resources preserved", flush=True)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        if os.environ.get("GITHUB_ACTIONS") == "true":
            print("::error::" + str(error)[-8000:].replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A"), flush=True)
        raise
