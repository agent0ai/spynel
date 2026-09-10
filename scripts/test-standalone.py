#!/usr/bin/env python3
"""Native isolated install/update/restart check. Pass older and newer host archives."""
import hashlib
import http.server
import json
import os
from pathlib import Path
import re
import select
import shutil
import subprocess
import sys
import tempfile
import threading
import time


def main():
    older, newer = [Path(p).resolve() for p in sys.argv[1:]]
    pattern = r"spynel_([0-9.]+)_(linux|darwin)_(amd64|arm64)\.tar\.gz"
    old_version, target_os, target_arch = re.fullmatch(pattern, older.name).groups()
    new_version, new_os, new_arch = re.fullmatch(pattern, newer.name).groups()
    assert (target_os, target_arch) == (new_os, new_arch)
    repo = Path(__file__).resolve().parent.parent
    script = (repo / "install.sh").read_bytes()
    with tempfile.TemporaryDirectory(prefix=".tmp-standalone-", dir=repo) as temporary:
        temp = Path(temporary)
        install = temp / "installed runtime Ω"
        user_bin = temp / "user bin"
        user_bin.mkdir()
        unrelated = user_bin / "spynel"
        unrelated.write_text("preserve this executable\n")
        workspace = temp / "workspace Ω"
        workspace.mkdir()
        env = {k: v for k, v in os.environ.items() if not k.startswith("SPYNEL_")}
        user_home = temp / "home"
        user_home.mkdir()
        env.update(HOME=str(user_home), SHELL="/bin/bash")
        tools = temp / "system tools"
        tools.mkdir()
        for name in ("sh", "curl", "tar", "awk", "mktemp", "uname", "sha256sum", "shasum", "wc", "mkdir", "rm", "rmdir", "chmod", "readlink", "ln", "gzip", "id", "sed", "grep", "cat", "cmp"):
            source = shutil.which(name)
            if source:
                (tools / name).symlink_to(source)
        env["PATH"] = str(tools)  # No Node, Go, compiler, or live harness on PATH.
        env.update(SPYNEL_INSTALL_DIR=str(install), SPYNEL_BIN_DIR=str(user_bin), SPYNEL_VERSION=old_version)
        fixture = {"new": False, "failure": "", "checks": 0}
        download_started, release_download = threading.Event(), threading.Event()
        files = {p.name: p.read_bytes() for p in (older, newer)}
        sums = "".join(f"{hashlib.sha256(data).hexdigest()}  {name}\n" for name, data in files.items()).encode()

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                if self.path == "/install.sh":
                    data = script
                elif self.path == "/latest":
                    fixture["checks"] += 1
                    data = json.dumps({"tag_name": "v" + (new_version if fixture["new"] else old_version), "prerelease": False, "draft": False}).encode()
                elif self.path == "/checksums.txt":
                    data = sums if fixture["failure"] != "checksum" else re.sub(rb"^[0-9a-f]{64}", b"0" * 64, sums, flags=re.M)
                elif self.path[1:] in files:
                    data = files[self.path[1:]]
                    if fixture["failure"] == "incomplete":
                        self.send_response(200)
                        self.send_header("Content-Length", str(len(data)))
                        self.end_headers()
                        self.wfile.write(data[:1024])
                        self.close_connection = True
                        return
                else:
                    self.send_error(404)
                    return
                self.send_response(200)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                if fixture["failure"] == "pause" and self.path[1:] in files:
                    download_started.set()
                    release_download.wait(15)
                self.wfile.write(data)

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        worker = threading.Thread(target=server.serve_forever, daemon=True)
        worker.start()
        base = f"http://127.0.0.1:{server.server_port}"
        env.update(SPYNEL_DOWNLOAD_BASE=base, SPYNEL_GITHUB_API_URL=base + "/latest", SPYNEL_NPM_REGISTRY_URL=base + "/forbidden-npm")
        process = None
        try:
            def run(*args, executable=None):
                result = subprocess.run([str(executable or install / "spynel"), *args], cwd=workspace, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30)
                assert result.returncode == 0, (args, result.stderr)
                return result.stdout

            # When a writable PATH directory exists, even a bare pipe makes
            # spynel available in the ORIGINAL shell without a profile reload.
            immediate = temp / "immediate installation"
            immediate_env = {**env, "SPYNEL_INSTALL_DIR": str(immediate)}
            immediate_env.pop("SPYNEL_BIN_DIR")
            fixture["failure"] = "pause"
            child = subprocess.Popen(["sh", "-c", 'curl -LsSf "$1/install.sh" | sh && spynel --version', "sh", base], cwd=workspace, env=immediate_env, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            try:
                assert download_started.wait(10), "installer did not start downloading"
                assert select.select([child.stderr], [], [], 5)[0], "installer is silent during the download"
                first_line = child.stderr.readline()
                assert b"Downloading Spynel" in first_line, first_line
                assert child.poll() is None, "fixture did not hold the download open"
            finally:
                release_download.set()
                output, progress = child.communicate(timeout=120)
            assert child.returncode == 0, progress.decode()
            assert ("spynel " + old_version) in output.decode(), output.decode()
            assert b"Run: spynel" in output and b"open a new terminal" not in output
            assert b"%" in progress and b"Verifying" in progress and b"Installing Spynel" in progress, progress.decode()
            assert not (user_home / ".bashrc").exists(), "on-PATH install edited shell profiles"
            fixture["failure"] = ""
            removed = subprocess.run(["sh", "-c", 'curl -LsSf "$1/install.sh" | sh -s -- --uninstall && ! command -v spynel', "sh", base], cwd=workspace, env=immediate_env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30)
            assert removed.returncode == 0, removed.stderr.decode()
            assert not immediate.exists() and not (tools / "spynel").is_symlink()

            # Piped stdin; no checkout-relative imports, Node, Go or compiler.
            result = subprocess.run(["sh"], input=script, cwd=workspace, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=120)
            assert result.returncode == 0, result.stderr.decode()
            assert unrelated.read_text() == "preserve this executable\n"
            assert "Preserved the existing" in result.stdout.decode()
            assert run("--version").strip() == "spynel " + old_version
            assert run("version", "--quiet") == ""
            old_executable = (install / "spynel").resolve()
            assert (old_executable.parent / "licenses" / "onnxruntime" / "LICENSE").is_file()
            assert fixture["checks"] == 0, "noninteractive bootstrap checked for updates"
            run("init", "--no-start", "--dir", str(workspace))
            config = workspace / ".spynel" / "config.yaml"
            config.write_text("harness:\n  name: acp\n  acp_command: " + json.dumps(str(temp / "intentionally-missing-harness")) + "\n  sandbox: danger-full-access\norchestrator:\n  enabled: false\n  semantic_heartbeat_minutes: 0\nspeech:\n  enabled: false\n")
            sentinel = workspace / ".spynel" / "tasks" / "todo" / "preserved.md"
            sentinel.write_text("---\nid: preserved\nstatus: todo\nreview_required: true\n---\nSynthetic task preserved across restart.\n")
            original_config, original_task = config.read_bytes(), sentinel.read_bytes()
            assert "GitHub" in run("update")
            checks = fixture["checks"]
            # An unmanaged copy remains unmanaged even with inherited npm metadata.
            archive_copy = temp / "unmanaged"
            shutil.copytree(old_executable.parent, archive_copy)
            npm_root = temp / "npm installation"
            vendor = npm_root / "npm" / "vendor"
            vendor.mkdir(parents=True)
            (npm_root / "package.json").write_text(json.dumps({"name": "spynel", "version": old_version}))
            (vendor / ".installed.json").write_text(json.dumps({"version": old_version}))
            (vendor / "spynel").write_text("unrelated npm executable\n")
            env.update(SPYNEL_NPM_PACKAGE_ROOT=str(npm_root), SPYNEL_NPM_LAUNCHER_MANAGED="1")
            assert "unmanaged" in run("update", executable=archive_copy / "spynel")
            assert fixture["checks"] == checks
            fixture["new"] = True
            for failure in ("checksum", "incomplete"):
                fixture["failure"] = failure
                assert "failed" in run("update", "install")
                rejected = subprocess.run(["sh"], input=script, cwd=workspace, env={**env, "SPYNEL_VERSION": new_version}, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=120)
                assert rejected.returncode != 0, "bootstrap accepted a corrupt download"
                assert (install / "spynel").resolve() == old_executable
                assert run("--version").strip() == "spynel " + old_version
            fixture["failure"] = ""
            # Headless startup must not query either update source.
            checks = fixture["checks"]
            with (temp / "server.log").open("w") as log:
                process = subprocess.Popen([str(install / "spynel"), "serve", "--automatic-startup", "--config", str(config)], cwd=workspace, env=env, stdin=subprocess.DEVNULL, stdout=log, stderr=log)
                primary = workspace / ".spynel" / "runtime" / "primary.json"

                def wait_for(predicate):
                    deadline = time.monotonic() + 30
                    while time.monotonic() < deadline:
                        assert process.poll() is None, (temp / "server.log").read_text()
                        try:
                            if predicate():
                                return
                        except (FileNotFoundError, json.JSONDecodeError):
                            pass
                        time.sleep(0.1)
                    raise AssertionError("isolated primary did not reach expected state")

                wait_for(lambda: primary.exists())
                first = json.loads(primary.read_text())
                assert fixture["checks"] == checks
                assert new_version in run("update")
                assert "Updating Spynel" in run("update", "install")
                wait_for(lambda: json.loads(primary.read_text()) != first and "current" in run("update"))
                assert run("--version").strip() == "spynel " + new_version
                assert "GitHub" in run("update")
                assert old_executable.exists(), "running process libraries were removed"
                assert (vendor / "spynel").read_text() == "unrelated npm executable\n"
                assert config.read_bytes() == original_config and sentinel.read_bytes() == original_task
                # Ordinary /restart must also follow the stable launcher.
                second = json.loads(primary.read_text())
                run("restart")
                wait_for(lambda: json.loads(primary.read_text()) != second and "GitHub" in run("update"))
                process.terminate()
                process.wait(timeout=30)
                assert not primary.exists(), "graceful shutdown did not release ownership"
                process = None
            # Without a primary, update only the caller's separate installation
            # and restart into version instead of repeating the install command.
            for mode in ("plain", "json"):
                offline_install = temp / (mode + " installation")
                offline_bin = temp / (mode + r" user bin Ω $value 'quote' `ticks` \backslash")
                # The README's current-shell activation also handles an
                # explicitly off-PATH destination, including shell metacharacters.
                result = subprocess.run(["sh", "-c", 'curl -LsSf "$1/install.sh" | sh && . "$SPYNEL_INSTALL_DIR/env" && spynel --version', "sh", base], cwd=workspace, env={**env, "SPYNEL_INSTALL_DIR": str(offline_install), "SPYNEL_BIN_DIR": str(offline_bin)}, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=120)
                assert result.returncode == 0, result.stderr.decode()
                assert ("spynel " + old_version).encode() in result.stdout, result.stdout
                offline_launcher = offline_bin / "spynel"
                assert offline_launcher.is_symlink()
                assert offline_launcher.resolve() == (offline_install / "spynel").resolve()
                assert run("--version", executable=offline_launcher).strip() == "spynel " + old_version
                profile_check = subprocess.run(["sh", "-c", '. "$HOME/.bashrc"; spynel --version'], cwd=workspace, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30)
                assert profile_check.returncode == 0 and profile_check.stdout.strip() == "spynel " + old_version, profile_check.stderr
                previous_primary_bundle = (install / "spynel").resolve()
                output = run("update", *(("--json",) if mode == "json" else ()), "install", executable=offline_launcher)
                if mode == "json":
                    events = [json.loads(line) for line in output.splitlines()]
                    assert len(events) == 1, events
                    assert events[0]["kind"] == "final" and events[0]["done"] and events[0]["request_id"], events
                    assert "Updating Spynel" in events[0]["text"], events
                else:
                    assert "spynel " + new_version in output
                assert run("--version", executable=offline_launcher).strip() == "spynel " + new_version
                assert (install / "spynel").resolve() == previous_primary_bundle
                assert "GitHub" in run("update", executable=offline_launcher)
                removed = subprocess.run(["sh", "-s", "--", "--uninstall"], input=script, cwd=workspace, env={**env, "SPYNEL_INSTALL_DIR": str(offline_install)}, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30)
                assert removed.returncode == 0, removed.stderr.decode()
                assert not offline_install.exists() and not offline_launcher.is_symlink()
                assert config.read_bytes() == original_config and sentinel.read_bytes() == original_task
            # A marker does not authorize removing unrelated siblings or files.
            keep = install / "user-created-file"
            keep.write_text("keep me\n")
            removed = subprocess.run(["sh", "-s", "--", "--uninstall"], input=script, cwd=workspace, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30)
            assert removed.returncode == 0, removed.stderr.decode()
            assert keep.read_text() == "keep me\n" and unrelated.read_text() == "preserve this executable\n"
            assert not (install / "releases").exists()
            rejected = subprocess.run(["sh", "-s", "--", "--uninstall"], input=script, cwd=workspace, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30)
            assert rejected.returncode != 0 and keep.read_text() == "keep me\n"
            print(json.dumps({"result": "passed", "classification": "observed-native", "target": f"{target_os}/{target_arch}", "checks": ["piped bootstrap and immediate parent-shell command", "live download progress", "unrelated executable preserved", "source ownership", "checksum and incomplete download rejected", "headless checks suppressed", "primary update and restart", "ordinary restart", "old libraries retained", "workspace state preserved", "graceful primary release", "plain and NDJSON ownerless updates", "uninstall preserves workspaces and unrelated files", "unmanaged uninstall rejected"]}))
        finally:
            if process is not None and process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=30)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
            server.shutdown()
            server.server_close()
            worker.join()


if __name__ == "__main__":
    main()
