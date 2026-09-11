#!/usr/bin/env python3
"""Isolated npm update: two workspaces, headless primary, secondary TUI and primary TUI."""
import fcntl
import http.server
import json
import os
from pathlib import Path
import pty
import select
import shutil
import struct
import subprocess
import sys
import tarfile
import tempfile
import termios
import threading
import time


def main():
    repo = Path(__file__).resolve().parent.parent
    archive = Path(sys.argv[1]).resolve()
    with tempfile.TemporaryDirectory(prefix=".tmp-instance-updates-", dir=repo) as temporary:
        temp = Path(temporary)
        package = temp / "prefix" / "lib" / "node_modules" / "spynel"
        vendor = package / "npm" / "vendor"
        vendor.mkdir(parents=True)
        with tarfile.open(archive) as bundle:
            for member in bundle.getmembers():
                assert member.isfile() or member.isdir(), "unexpected archive member"
                assert not Path(member.name).is_absolute() and ".." not in Path(member.name).parts
            bundle.extractall(vendor)
        version = subprocess.check_output([str(vendor / "spynel"), "--version"], text=True).strip().split()[1]
        for name in ("bin/spynel.js", "platform.js", "update.js"):
            target = package / "npm" / name
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(repo / "npm" / name, target)
        (package / "package.json").write_text(json.dumps({"name": "spynel", "version": version}))
        (vendor / ".installed.json").write_text(json.dumps({"version": version}))
        replacement = temp / "replacement"
        shutil.copytree(package, replacement)
        updated = "9.9.9"
        build_env = {**os.environ, "CGO_LDFLAGS_ALLOW": "^-Wl,-rpath,@loader_path/lib$"}
        subprocess.run([os.environ.get("SPYNEL_GO_BINARY", "go"), "build", "-trimpath", "-ldflags=-s -w -X main.version=" + updated, "-o", str(replacement / "npm" / "vendor" / "spynel"), "./cmd/spynel"], cwd=repo, env=build_env, check=True, timeout=180)
        (replacement / "package.json").write_text(json.dumps({"name": "spynel", "version": updated}))
        (replacement / "npm" / "vendor" / ".installed.json").write_text(json.dumps({"version": updated}))
        tools = temp / "tools"
        tools.mkdir()
        npm = tools / "npm"
        npm.write_text(f'''#!{sys.executable}
import json, os, pathlib, shutil, sys
package = pathlib.Path(os.environ["INSTANCE_TEST_PACKAGE"])
if sys.argv[1:] == ["root", "--global"]:
    print(package.parent)
    sys.exit(0)
assert sys.argv[1] == "update", sys.argv
with open(os.environ["INSTANCE_TEST_CALLS"], "a") as log:
    log.write("update\\n")
old = package.with_name(".spynel-replaced")
package.rename(old)
shutil.copytree(os.environ["INSTANCE_TEST_REPLACEMENT"], package)
shutil.rmtree(old)
''')
        npm.chmod(0o700)
        harness = tools / "acp-fixture"
        harness.write_text(f'''#!{sys.executable}
import json, sys
for line in sys.stdin:
    request = json.loads(line)
    assert request["method"] == "initialize", "test must not dispatch inference"
    print(json.dumps({{"jsonrpc": "2.0", "id": request["id"], "result": {{"protocolVersion": 1, "agentCapabilities": {{}}}}}}), flush=True)
''')
        harness.chmod(0o700)
        home = temp / "home"
        home.mkdir()
        env = {k: v for k, v in os.environ.items() if not k.startswith("SPYNEL_")}
        env.update(HOME=str(home), XDG_CONFIG_HOME=str(home / ".config"), XDG_RUNTIME_DIR=str(temp / "run"), TERM="xterm-256color", SPYNEL_SKIP_UPDATE_CHECK="1", PATH=str(tools) + os.pathsep + os.environ["PATH"], INSTANCE_TEST_PACKAGE=str(package), INSTANCE_TEST_REPLACEMENT=str(replacement), INSTANCE_TEST_CALLS=str(temp / "npm-calls"))
        processes, terminals, logs = [], [], []
        registry = home / ("Library/Application Support" if sys.platform == "darwin" else ".config") / "spynel" / "processes"

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                data = json.dumps({"version": updated}).encode()
                self.send_response(200)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        env["SPYNEL_NPM_REGISTRY_URL"] = f"http://127.0.0.1:{server.server_port}/latest"
        launcher = [shutil.which("node"), str(package / "npm" / "bin" / "spynel.js")]

        def records():
            result = []
            for path in registry.glob("*.json"):
                try:
                    result.append(json.loads(path.read_text()))
                except (FileNotFoundError, json.JSONDecodeError):
                    pass
            return result

        def wait_for(predicate):
            deadline = time.monotonic() + 40
            while time.monotonic() < deadline:
                assert all(p.poll() is None for p in processes), [(p.poll(), bytes(output[-800:]).decode(errors="replace")) for p, (_, output) in zip(processes[1:], terminals)]
                if predicate():
                    return
                time.sleep(0.05)
            raise AssertionError("instance update did not reach expected state")

        def drain(fd, output):
            try:
                while True:
                    if select.select([fd], [], [], 1)[0]:
                        data = os.read(fd, 65536)
                        if not data:
                            return
                        output.extend(data)
                        del output[:-1_000_000]
            except OSError:
                pass

        try:
            workspaces = [temp / "workspace one", temp / "workspace two", temp / "older launcher workspace"]
            for workspace in workspaces:
                workspace.mkdir()
                subprocess.run([*launcher, "init", "--no-start"], cwd=workspace, env=env, check=True, stdout=subprocess.DEVNULL)
                (workspace / ".spynel" / "config.yaml").write_text('harness:\n  name: acp\n  acp_command: ' + json.dumps(str(harness)) + '\n  sandbox: danger-full-access\norchestrator:\n  enabled: false\n  semantic_heartbeat_minutes: 0\nspeech:\n  enabled: false\n')
            for index, (workspace, tui) in enumerate(((workspaces[0], False), (workspaces[0], True), (workspaces[1], True))):
                if tui:
                    master, slave = pty.openpty()
                    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 34, 120, 0, 0))
                    output = bytearray()
                    threading.Thread(target=drain, args=(master, output), daemon=True).start()
                    terminals.append((master, output))
                    process = subprocess.Popen([*launcher, "serve", "--tui"], cwd=workspace, env=env, stdin=slave, stdout=slave, stderr=slave, start_new_session=True)
                    os.close(slave)
                else:
                    log = (temp / "headless.log").open("w")
                    logs.append(log)
                    process = subprocess.Popen([*launcher, "serve"], cwd=workspace, env=env, stdin=subprocess.DEVNULL, stdout=log, stderr=log)
                processes.append(process)
                wait_for(lambda: len(records()) == index + 1)
            wait_for(lambda: all(b"\x1b[?1049h" in output for _, output in terminals))
            before = {record["pid"]: record["generation"] for record in records()}
            result = subprocess.run([*launcher, "update"], cwd=temp, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=60)
            assert result.returncode == 0, result.stderr
            wait_for(lambda: len(records()) == 3 and all(record["version"] == updated and before.get(record["pid"]) != record["generation"] for record in records()))
            assert set(before) == {record["pid"] for record in records()}, "restart lost native PIDs or terminal attachment"
            wait_for(lambda: all(output.count(b"\x1b[?1049h") >= 2 for _, output in terminals))
            wait_for(lambda: all(updated.encode() in output for _, output in terminals))
            for fd, output in terminals:
                output.clear()
                os.write(fd, b"/status\r")
            wait_for(lambda: all(b"Instance ID" in output for _, output in terminals))
            before = {record["generation"] for record in records()}
            os.write(terminals[0][0], b"/update\r")
            wait_for(lambda: len(records()) == 3 and all(record["generation"] not in before for record in records()))
            assert (temp / "npm-calls").read_text() == "update\n", "current-version restart invoked npm replacement"
            # A registered native process made legacy must block publication without a signal.
            record = records()[0]
            path = registry / (str(record["pid"]) + ".json")
            saved = path.read_bytes()
            path.unlink()
            try:
                failed = subprocess.run([*launcher, "update"], cwd=temp, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30)
                assert failed.returncode != 0 and "spynel killall" in failed.stderr, failed.stderr
                assert all(p.poll() is None for p in processes)
            finally:
                path.write_bytes(saved)
                path.chmod(0o600)
            # A new native binary can still have an older supervising Node launcher.
            old_environment = {**env, "SPYNEL_NPM_PACKAGE_ROOT": str(package), "SPYNEL_NPM_LAUNCHER_MANAGED": "1"}
            log = (temp / "older-launcher.log").open("w")
            logs.append(log)
            older = subprocess.Popen([str(vendor / "spynel"), "serve"], cwd=workspaces[2], env=old_environment, stdin=subprocess.DEVNULL, stdout=log, stderr=log)
            processes.append(older)
            wait_for(lambda: len(records()) == 4)
            failed = subprocess.run([*launcher, "update"], cwd=temp, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30)
            assert failed.returncode != 0 and "spynel killall" in failed.stderr, failed.stderr
            assert all(p.poll() is None for p in processes), "preflight stopped an instance"
            print("Verified npm replacement, all-instance restart across two workspaces, preserved PTYs, channel update, and rejection of older processes/launchers.")
        finally:
            for process in processes:
                if process.poll() is None:
                    process.terminate()
            for process in processes:
                try:
                    process.wait(timeout=30)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
            for fd, _ in terminals:
                os.close(fd)
            for log in logs:
                log.close()
            server.shutdown()
            server.server_close()


if __name__ == "__main__":
    main()
