#!/usr/bin/env python3
"""Isolated npm update with a live worker, two workspaces and retained PTYs."""
import datetime
import fcntl
import hashlib
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
        legacy_replacement = temp / "legacy-replacement"
        shutil.copytree(package, legacy_replacement)
        replacement = temp / "replacement"
        shutil.copytree(package, replacement)
        updated = "9.9.9"
        build_env = {**os.environ, "CGO_LDFLAGS_ALLOW": "^-Wl,-rpath,@loader_path/lib$"}
        subprocess.run([os.environ.get("SPYNEL_GO_BINARY", "go"), "build", "-trimpath", "-ldflags=-s -w -X main.version=" + updated, "-o", str(replacement / "npm" / "vendor" / "spynel"), "./cmd/spynel"], cwd=repo, env=build_env, check=True, timeout=180)
        updated_image = replacement / "npm" / "vendor" / "spynel"
        if len(sys.argv) > 2:
            shutil.copy2(updated_image, Path(sys.argv[2]).resolve())
        print(json.dumps({"event": "native_images", "archive_sha256": hashlib.sha256(archive.read_bytes()).hexdigest(), "updated_image_sha256": hashlib.sha256(updated_image.read_bytes()).hexdigest()}), flush=True)
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
if sys.argv[1:] == ["uninstall", "--global", "--prefix", str(package.parents[2]), "spynel"]:
    shutil.rmtree(package)
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
import json, os, pathlib, sys, threading, time
lock = threading.Lock()
def emit(value):
    with lock:
        print(json.dumps(value), flush=True)
def record(event):
    with open(os.environ["INSTANCE_TEST_PROVIDER_EVENTS"], "a") as log:
        log.write(json.dumps({{"event": event, "pid": os.getpid()}}) + "\\n")
def finish(request):
    deadline = time.monotonic() + 240
    while not pathlib.Path(os.environ["INSTANCE_TEST_COMPLETE"]).exists():
        if time.monotonic() >= deadline:
            os._exit(2)
        time.sleep(.05)
    record("completed")
    emit({{"jsonrpc": "2.0", "method": "session/update", "params": {{"sessionId": "synthetic-session", "update": {{"sessionUpdate": "agent_message_chunk", "content": {{"type": "text", "text": "synthetic installed worker result"}}}}}}}})
    emit({{"jsonrpc": "2.0", "id": request["id"], "result": {{"stopReason": "end_turn"}}}})
record("started")
for line in sys.stdin:
    request = json.loads(line)
    method = request["method"]
    if method == "initialize":
        emit({{"jsonrpc": "2.0", "id": request["id"], "result": {{"protocolVersion": 1, "agentCapabilities": {{}}}}}})
    elif method == "session/new":
        emit({{"jsonrpc": "2.0", "id": request["id"], "result": {{"sessionId": "synthetic-session"}}}})
    elif method == "session/prompt":
        record("prompt")
        threading.Thread(target=finish, args=(request,), daemon=True).start()
    else:
        raise AssertionError("unexpected provider operation: " + method)
''')
        harness.chmod(0o700)
        home = temp / "home"
        home.mkdir()
        env = {k: v for k, v in os.environ.items() if not k.startswith("SPYNEL_")}
        env.update(HOME=str(home), XDG_CONFIG_HOME=str(home / ".config"), XDG_RUNTIME_DIR=str(temp / "run"), TERM="xterm-256color", SPYNEL_SKIP_UPDATE_CHECK="1", PATH=str(tools) + os.pathsep + os.environ["PATH"], INSTANCE_TEST_PACKAGE=str(package), INSTANCE_TEST_REPLACEMENT=str(replacement), INSTANCE_TEST_CALLS=str(temp / "npm-calls"), INSTANCE_TEST_PROVIDER_EVENTS=str(temp / "provider-events"), INSTANCE_TEST_COMPLETE=str(temp / "complete"))
        processes, terminals, logs = [], [], []
        requests = []
        unrelated = None
        registry = home / ("Library/Application Support" if sys.platform == "darwin" else ".config") / "spynel" / "processes"

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                data = json.dumps({"version": version if self.path == "/legacy/latest" else updated}).encode()
                self.send_response(200)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        env["SPYNEL_NPM_REGISTRY_URL"] = f"http://127.0.0.1:{server.server_port}/latest"
        launcher = [shutil.which("node"), str(package / "npm" / "bin" / "spynel.js")]

        def records(workers=False):
            result = []
            for path in registry.glob("*.json"):
                try:
                    record = json.loads(path.read_text())
                    if workers is None or (bool(record.get("harness_worker")) == workers and not record.get("diagnostic_reader") and not record.get("provider_guardian") and not record.get("application_client") and not record.get("supervisor_pid")):
                        result.append(record)
                except (FileNotFoundError, json.JSONDecodeError):
                    pass
            return result

        def evidence(event, **fields):
            print(json.dumps({"at": datetime.datetime.now(datetime.timezone.utc).isoformat(), "event": event, **fields}), flush=True)

        def provider_events():
            path = Path(env["INSTANCE_TEST_PROVIDER_EVENTS"])
            return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

        def worker_identity():
            return {record["pid"]: record for record in records(workers=True)}

        def native_start(pid):
            result = subprocess.run(["ps", "-p", str(pid), "-o", "stat=", "-o", "lstart="], text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=5)
            assert not result.stderr, result.stderr
            if result.returncode == 1 and not result.stdout.strip():
                return None
            assert result.returncode == 0, result.returncode
            state, start = result.stdout.strip().split(maxsplit=1)
            return None if state.startswith(("Z", "X")) else start

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
            unrelated = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(300)"], stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
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
            wait_for(lambda: len(worker_identity()) == 2)
            workers_before = worker_identity()
            evidence("workers_before_update", workers=list(workers_before.values()))
            request_log = (temp / "request.log").open("w")
            logs.append(request_log)
            request = subprocess.Popen([*launcher, "send", "--conversation", "installation-continuity", "synthetic installation continuity request"], cwd=workspaces[0], env=env, stdin=subprocess.DEVNULL, stdout=request_log, stderr=request_log)
            requests.append(request)
            wait_for(lambda: len([e for e in provider_events() if e["event"] == "prompt"]) == 1)
            active_provider = next(e["pid"] for e in provider_events() if e["event"] == "prompt")
            provider_starts = [e for e in provider_events() if e["event"] == "started"]
            provider_start = native_start(active_provider)
            assert provider_start is not None, "admitted provider already exited"
            evidence("provider_admitted", pid=active_provider, native_start=provider_start, starts=provider_starts)
            before = {record["pid"]: record["generation"] for record in records()}
            result = subprocess.run([*launcher, "update"], cwd=temp, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=60)
            assert result.returncode == 0, result.stderr
            wait_for(lambda: len(records()) == 3 and all(record["ready"] and record["version"] == updated and before.get(record["pid"]) != record["generation"] for record in records()))
            assert set(before) == {record["pid"] for record in records()}, "restart lost logical instance PIDs or terminal attachment"
            assert worker_identity() == workers_before, "npm image replacement replaced a surviving worker"
            assert not any(e["event"] == "completed" for e in provider_events()), "provider ended during update"
            assert native_start(active_provider) == provider_start, "update lost the native provider"
            evidence("npm_image_replaced", applications=records(), workers=list(worker_identity().values()))
            wait_for(lambda: all(output.count(b"\x1b[?1049h") >= 2 for _, output in terminals))
            wait_for(lambda: all(updated.encode() in output for _, output in terminals))
            for fd, output in terminals:
                output.clear()
                os.write(fd, b"/status\r")
            wait_for(lambda: all(b"Instance ID" in output for _, output in terminals))
            before = {record["generation"] for record in records()}
            os.write(terminals[0][0], b"/update\r")
            wait_for(lambda: len(records()) == 3 and all(record["ready"] and record["generation"] not in before for record in records()))
            assert (temp / "npm-calls").read_text() == "update\n", "current-version restart invoked npm replacement"
            assert worker_identity() == workers_before, "current-version channel update replaced a surviving worker"
            assert native_start(active_provider) == provider_start, "channel restart lost the native provider"
            Path(env["INSTANCE_TEST_COMPLETE"]).touch()

            def completed_history():
                return [json.loads(line) for path in (workspaces[0] / ".spynel" / "history").rglob("*.jsonl") for line in path.read_text().splitlines(keepends=True) if line.endswith("\n") and line.strip()]

            wait_for(lambda: sum(e.get("role") == "assistant" and e.get("content") == "synthetic installed worker result" for e in completed_history()) == 1)
            prompts = [e for e in provider_events() if e["event"] == "prompt"]
            completions = [e for e in provider_events() if e["event"] == "completed"]
            assert prompts == [{"event": "prompt", "pid": active_provider}] and completions == [{"event": "completed", "pid": active_provider}], "update duplicated or replaced the active provider turn"
            assert [e for e in provider_events() if e["event"] == "started"] == provider_starts, "update restarted a provider"
            evidence("retained_turn_completed_once", provider_pid=active_provider, workers=list(worker_identity().values()))
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
            # Run the exact old wrapper through package replacement; its loaded code stays old.
            legacy = temp / "legacy" / "lib" / "node_modules" / "spynel"
            shutil.copytree(legacy_replacement, legacy)
            for name in ("bin/spynel.js", "platform.js", "update.js"):
                source = subprocess.check_output(["git", "show", "v0.12.14:npm/" + name], cwd=repo)
                (legacy / "npm" / name).write_bytes(source)
            (legacy / "package.json").write_text(json.dumps({"name": "spynel", "version": "0.12.14"}))
            (legacy / "npm" / "vendor" / ".installed.json").write_text(json.dumps({"version": "0.12.14"}))
            legacy_native = legacy / "npm" / "vendor" / "spynel"
            legacy_native.write_text(f'''#!{sys.executable}
import json, os, pathlib, sys
if sys.argv[1] == "serve":
    state = pathlib.Path(os.environ["SPYNEL_NPM_UPDATE_STATE"])
    state.write_text(json.dumps({{"args": ["serve", "--automatic-startup"], "install": True, "version": "{version}"}}))
    state.chmod(0o600)
    sys.exit(75)
assert sys.argv[1] == "check-restartable", sys.argv
''')
            legacy_native.chmod(0o700)
            legacy_calls = temp / "legacy-npm-calls"
            old_environment = {**env, "INSTANCE_TEST_PACKAGE": str(legacy), "INSTANCE_TEST_CALLS": str(legacy_calls), "INSTANCE_TEST_REPLACEMENT": str(legacy_replacement), "SPYNEL_NPM_REGISTRY_URL": f"http://127.0.0.1:{server.server_port}/legacy/latest"}
            legacy_launcher = [shutil.which("node"), str(legacy / "npm" / "bin" / "spynel.js")]
            log = (temp / "older-launcher.log").open("w")
            logs.append(log)
            older = subprocess.Popen([*legacy_launcher, "serve", "--automatic-startup"], cwd=workspaces[2], env=old_environment, stdin=subprocess.DEVNULL, stdout=log, stderr=log)
            processes.append(older)
            wait_for(lambda: len(records()) == 4 and all(record["ready"] for record in records()))
            legacy_record = next(record for record in records() if record["installation"] == str(legacy))
            assert legacy_record["version"] == version and not legacy_record["coordinated_updates"], legacy_record
            assert json.loads((legacy / "package.json").read_text())["version"] == version
            refusal = "this npm launcher does not support coordinated updates; run the installed spynel killall command once, then relaunch Spynel"
            assert refusal in (temp / "older-launcher.log").read_text()
            failed = subprocess.run([*legacy_launcher, "command", "update"], cwd=workspaces[2], env=old_environment, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30)
            assert refusal in failed.stdout + failed.stderr, failed
            assert all(p.poll() is None for p in processes), "preflight stopped an instance"
            assert legacy_calls.read_text() == "update\n", "refused update replaced the package again"
            for protocol in ("", "unsupported"):
                unsupported = {**old_environment, "SPYNEL_NPM_PACKAGE_ROOT": str(legacy), "SPYNEL_NPM_LAUNCHER_MANAGED": "1", "SPYNEL_NPM_COORDINATED_UPDATES": protocol}
                failed = subprocess.run([str(legacy_native), "check-restartable"], env=unsupported, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10)
                assert failed.returncode != 0 and refusal in failed.stderr, failed
            # Stop only fixture-owned processes, then start the installed wrapper afresh.
            older.terminate()
            older.wait(timeout=30)
            processes.remove(older)
            wait_for(lambda: len(records()) == 3)
            fresh = subprocess.Popen([*legacy_launcher, "serve", "--automatic-startup"], cwd=workspaces[2], env=old_environment, stdin=subprocess.DEVNULL, stdout=log, stderr=log)
            processes.append(fresh)
            wait_for(lambda: len(records()) == 4 and all(record["ready"] for record in records()))
            before_recovery = next(record for record in records() if record["installation"] == str(legacy))
            assert before_recovery["coordinated_updates"], before_recovery
            # Use the other installation as the channel client, proving primary-owned selection.
            recovered = subprocess.run([*launcher, "command", "update"], cwd=workspaces[2], env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30)
            assert recovered.returncode == 0 and "Restarting" in recovered.stdout, recovered
            wait_for(lambda: any(record["installation"] == str(legacy) and record["ready"] and record["version"] == version and record["generation"] != before_recovery["generation"] for record in records()))
            assert legacy_calls.read_text() == "update\n", "already-current recovery invoked npm replacement"

            # Explicit teardown must stop live work, unlike controller restart.
            Path(env["INSTANCE_TEST_COMPLETE"]).unlink()
            for command, workspace in ((launcher, workspaces[0]), (legacy_launcher, workspaces[2])):
                requests.append(subprocess.Popen([*command, "send", "--conversation", "active-teardown", "synthetic active teardown request"], cwd=workspace, env=env, stdin=subprocess.DEVNULL, stdout=request_log, stderr=request_log))
            wait_for(lambda: len([e for e in provider_events() if e["event"] == "prompt"]) == 3)
            assert len([e for e in provider_events() if e["event"] == "completed"]) == 1
            active = [e["pid"] for e in provider_events() if e["event"] == "prompt"][-2:]
            active_starts = {pid: native_start(pid) for pid in active}
            assert all(active_starts.values()), "teardown provider already exited"
            before_uninstall = records(workers=None)
            owned = [r for r in before_uninstall if r["installation"] == str(package)]
            other = [r for r in before_uninstall if r["installation"] == str(legacy)]
            assert owned and other and any(r.get("harness_worker") for r in owned) and any(r.get("harness_worker") for r in other)
            worker_roots = {}
            for path in (registry.parent / "harness").glob("*.lock.worker.json"):
                info = json.loads(path.read_text())["Info"]
                worker_roots[info["PID"]] = (path, Path(info["Socket"]).parent)
            assert set(worker_roots) == {r["pid"] for r in before_uninstall if r.get("harness_worker")}
            configurations = {workspace / ".spynel" / "config.yaml": (workspace / ".spynel" / "config.yaml").read_bytes() for workspace in workspaces}
            evidence("active_teardown_before_uninstall", records=before_uninstall, provider_starts=active_starts, worker_roots={pid: [str(p) for p in paths] for pid, paths in worker_roots.items()}, unrelated_pid=unrelated.pid, unrelated_start=native_start(unrelated.pid))
            binary = replacement / "npm" / "vendor" / "spynel"
            removed = subprocess.run([str(binary), "uninstall-bundles", "--root", str(temp / "absent-standalone"), "--npm-root", str(package)], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=40)
            assert removed.returncode == 0, removed.stderr
            assert not package.exists(), "npm installation was not removed"
            assert all(native_start(r["pid"]) is None for r in owned), "uninstall left an owned process alive"
            assert all(native_start(r["pid"]) is not None for r in other), "uninstall stopped another installation"
            assert {r["pid"]: r for r in records(workers=None) if r["installation"] == str(legacy)} == {r["pid"]: r for r in other}, "uninstall changed another installation's generations"
            assert all(not p.exists() for r in owned if r.get("harness_worker") for p in worker_roots[r["pid"]]), "uninstall retained an owned endpoint or rendezvous"
            assert all(p.exists() for r in other if r.get("harness_worker") for p in worker_roots[r["pid"]]), "uninstall removed another worker's state"
            assert sum(native_start(pid) == start for pid, start in active_starts.items()) == 1, "uninstall did not preserve exactly the other installation's provider"
            assert unrelated.poll() is None, "uninstall stopped an unrelated process"
            assert all(path.read_bytes() == content for path, content in configurations.items())
            for process in processes[:3]:
                process.wait(timeout=10)
            evidence("active_uninstall_passed", stopped=owned, preserved=other)

            # Discovery sees the host's native processes despite private HOME;
            # exercise the same stop boundary through this installation only.
            stopped = subprocess.run([str(binary), "uninstall-bundles", "--root", str(temp / "absent-standalone"), "--npm-root", str(legacy)], cwd=workspaces[2], env={**env, "INSTANCE_TEST_PACKAGE": str(legacy)}, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=40)
            assert stopped.returncode == 0, stopped.stderr
            assert all(native_start(r["pid"]) is None for r in other), "second uninstall left a registered process alive"
            assert all(native_start(pid) is None for pid in active), "active provider survived teardown"
            assert not records(workers=True), "active worker survived teardown"
            assert all(not p.exists() for paths in worker_roots.values() for p in paths), "active worker endpoint or rendezvous survived teardown"
            assert not legacy.exists() and unrelated.poll() is None, "second uninstall failed or stopped an unrelated process"
            assert all(path.read_bytes() == content for path, content in configurations.items())
            assert sum(e.get("role") == "assistant" and e.get("content") == "synthetic installed worker result" for e in completed_history()) == 1, "teardown lost or duplicated the completed result"
            assert len([e for e in provider_events() if e["event"] == "prompt"]) == 3
            assert len([e for e in provider_events() if e["event"] == "completed"]) == 1, "teardown invented provider completion"
            for process in processes:
                process.wait(timeout=10)
            evidence("second_active_uninstall_passed", stopped=other, provider_pids=active)
            print("Verified npm worker/turn continuity, cross-workspace ready generations/PTYs, stale-launcher refusal/recovery, and installation-scoped active uninstall with unrelated-process/workspace preservation.")
        finally:
            if unrelated is not None:
                unrelated.terminate()
                unrelated.wait(timeout=10)
            cleanup_records = records(workers=None)
            cleanup_providers = {e["pid"] for e in provider_events() if e["event"] == "started"}
            evidence("cleanup_owned_identities", records=cleanup_records, provider_pids=sorted(cleanup_providers))
            for request in requests:
                if request.poll() is None:
                    request.terminate()
                request.wait(timeout=10)
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
            # Controller exit deliberately detaches workers. Use the owning
            # installation boundary, never a host-wide killall or PID sweep.
            cleanup_errors = []
            for root in (package, temp / "legacy" / "lib" / "node_modules" / "spynel"):
                # The retained candidate also handles partially installed or
                # synthetic legacy launchers and remains outside removed trees.
                binary = replacement / "npm" / "vendor" / "spynel"
                if root.exists():
                    cleaned = subprocess.run([str(binary), "uninstall-bundles", "--root", str(temp / "absent-standalone"), "--npm-root", str(root)], env={**env, "INSTANCE_TEST_PACKAGE": str(root)}, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=40)
                    if cleaned.returncode != 0:
                        cleanup_errors.append(cleaned.stderr)
            assert not cleanup_errors, cleanup_errors
            assert not records(workers=True), "owned workers survived installation cleanup"
            for pid in {record["pid"] for record in cleanup_records} | cleanup_providers:
                assert native_start(pid) is None, "recorded fixture process survived uninstall: " + str(pid)
            evidence("installation_owned_cleanup_passed")
            server.shutdown()
            server.server_close()


if __name__ == "__main__":
    main()
