#!/usr/bin/env python3
"""Real npm 0.13.3 -> host candidate; private mirror, homes, processes and ACP."""
from datetime import datetime, timezone
import hashlib
import http.server
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tarfile
import tempfile
import threading
import time


def main():
    old_package, old_archive, candidate_archive = [Path(p).resolve() for p in sys.argv[1:]]
    repo = Path(__file__).resolve().parent.parent
    with tempfile.TemporaryDirectory(prefix=".tmp-affected-upgrade-", dir=repo) as temporary:
        temp = Path(temporary)
        home, workspace, zombie_workspace = [temp / name for name in ("home", "workspace", "zombie-workspace")]
        for path in (home, workspace, zombie_workspace):
            path.mkdir()
        prefix = temp / "prefix"
        package = prefix / "lib/node_modules/spynel"
        cleanup_runtime = temp / "cleanup-runtime"
        cleanup_runtime.mkdir()
        with tarfile.open(candidate_archive) as archive:
            assert all((m.isfile() or m.isdir()) and not Path(m.name).is_absolute() and ".." not in Path(m.name).parts for m in archive.getmembers())
            archive.extractall(cleanup_runtime)
        registry = home / ("Library/Application Support" if sys.platform == "darwin" else ".config") / "spynel/processes"
        env = {k: v for k, v in os.environ.items() if not k.startswith(("SPYNEL_", "npm_config_"))}
        env.update(HOME=str(home), XDG_CONFIG_HOME=str(home / ".config"), XDG_RUNTIME_DIR=str(temp / "run"), npm_config_cache=str(temp / "npm-cache"), npm_config_userconfig=str(temp / "npmrc"), npm_config_audit="false", npm_config_fund="false", SPYNEL_SKIP_UPDATE_CHECK="1")
        # Adopt only this fixture's orphan helpers on Linux; retain the deliberate
        # zombie until every upgrade assertion has finished, then join our children.
        if sys.platform == "linux":
            import ctypes
            assert ctypes.CDLL(None, use_errno=True).prctl(36, 1, 0, 0, 0) == 0
        archive_data = {p.name: p.read_bytes() for p in (old_archive, candidate_archive)}
        sums = "".join(f"{hashlib.sha256(data).hexdigest()}  {name}\n" for name, data in archive_data.items()).encode()
        candidate_version = candidate_archive.name.split("_")[1]
        fixture = {"failure": False}

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                name = self.path.rsplit("/", 1)[-1]
                if name == "latest":
                    data = json.dumps({"version": candidate_version}).encode()
                elif name == "checksums.txt":
                    data = sums
                elif name in archive_data:
                    if fixture["failure"]:
                        self.send_error(503)
                        return
                    data = archive_data[name]
                else:
                    self.send_error(404)
                    return
                self.send_response(200)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        base = f"http://127.0.0.1:{server.server_port}"
        env.update(SPYNEL_DOWNLOAD_BASE=base, SPYNEL_NPM_REGISTRY_URL=base + "/latest")
        launcher = [shutil.which("node"), str(package / "npm/bin/spynel.js")]
        children = []
        zombie = None
        logs = []

        def evidence(event, **fields):
            print(json.dumps({"at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"), "event": event, **fields}), flush=True)

        def run(args, *, cwd=workspace, success=True, timeout=80):
            result = subprocess.run(args, cwd=cwd, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
            assert (result.returncode == 0) == success, (args, result.stdout[-2000:], result.stderr[-2000:])
            return result

        def installed_files():
            return {str(p.relative_to(package)): hashlib.sha256(p.read_bytes()).hexdigest() for p in package.rglob("*") if p.is_file()}

        def records():
            result = []
            for path in registry.glob("*.json"):
                try:
                    record = json.loads(path.read_text())
                    record.pop("worker_update_token", None)
                    result.append(record)
                except FileNotFoundError:
                    pass  # A process may unregister after the directory snapshot.
            return result

        def wait_for(predicate, seconds=45):
            deadline = time.monotonic() + seconds
            while time.monotonic() < deadline:
                if predicate():
                    return
                time.sleep(0.05)
            raise AssertionError("fixture condition timed out")

        def start(args, cwd):
            log = (temp / f"process-{len(logs)}.log").open("w+")
            logs.append(log)
            process = subprocess.Popen(args, cwd=cwd, env=env, stdin=subprocess.DEVNULL, stdout=log, stderr=log)
            children.append(process)
            return process

        def kernel_zombie(pid):
            if sys.platform == "linux":
                return (Path("/proc") / str(pid) / "stat").read_text().rsplit(")", 1)[1].split()[0] == "Z"
            return subprocess.check_output(["ps", "-o", "stat=", "-p", str(pid)], text=True).strip().startswith("Z")

        def native_start(pid):
            result = subprocess.run(["ps", "-p", str(pid), "-o", "stat=", "-o", "lstart="], text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=5)
            assert not result.stderr, result.stderr
            if result.returncode == 1 and not result.stdout.strip():
                return None
            assert result.returncode == 0, result.stdout
            state, *start = result.stdout.split()
            return None if state.startswith(("Z", "X")) else " ".join(start)

        try:
            run(["npm", "install", "--global", "--prefix", str(prefix), str(old_package)])
            native = package / "npm/vendor/spynel"
            with tarfile.open(old_archive) as archive:
                old_binary = archive.extractfile(next(m for m in archive.getmembers() if m.name.lstrip("./") == "spynel" and m.isfile())).read()
            assert native.read_bytes() == old_binary, "old install differs from authentic archive"
            old_files = installed_files()
            metadata = subprocess.check_output([os.environ.get("SPYNEL_GO_BINARY", "go"), "version", "-m", str(native)], text=True)
            assert "vcs.revision=f44b827704f80ef16282dd0a0e3b3a23c14abade" in metadata, metadata
            harness = temp / "acp-fixture"
            harness.write_text(f'''#!{sys.executable}
import json,sys,time
from pathlib import Path
for line in sys.stdin:
 r=json.loads(line); method=r.get("method"); result={{}}
 if method=="initialize": result={{"protocolVersion":1,"agentCapabilities":{{"loadSession":True}}}}
 elif method in ("session/new","session/load"): result={{"sessionId":"upgrade-fixture"}}
 elif method=="session/prompt":
  Path({str(temp / 'admitted')!r}).write_text("admitted")
  while not Path({str(temp / 'release')!r}).exists(): time.sleep(.05)
  print(json.dumps({{"jsonrpc":"2.0","method":"session/update","params":{{"sessionId":"upgrade-fixture","update":{{"sessionUpdate":"agent_message_chunk","content":{{"type":"text","text":"fixture work completed"}}}}}}}}),flush=True)
  result={{"stopReason":"end_turn"}}
 if "id" in r: print(json.dumps({{"jsonrpc":"2.0","id":r["id"],"result":result}}),flush=True)
''')
            harness.chmod(0o700)
            for cwd in (workspace, zombie_workspace):
                run([*launcher, "init", "--no-start"], cwd=cwd)
                (cwd / ".spynel/config.yaml").write_text("harness:\n  name: acp\n  acp_command: " + json.dumps(str(harness)) + "\n  sandbox: danger-full-access\norchestrator:\n  enabled: false\n  semantic_heartbeat_minutes: 0\nspeech:\n  enabled: false\n")
            live = start([*launcher, "serve"], workspace)
            wait_for(lambda: any(r.get("supervisor") and r["ready"] for r in records()))
            before = next(r for r in records() if r.get("supervisor"))
            zombie = start([str(native), "serve", "--automatic-startup"], zombie_workspace)
            children.remove(zombie)  # Never poll/wait it until after upgrade assertions.
            wait_for(lambda: any(r["pid"] == zombie.pid and r["ready"] for r in records()))
            os.kill(zombie.pid, signal.SIGKILL)
            wait_for(lambda: kernel_zombie(zombie.pid))
            zombie_record = (registry / f"{zombie.pid}.json").read_bytes()
            send = start([*launcher, "send", "--conversation", "upgrade", "fixture work"], workspace)
            wait_for(lambda: (temp / "admitted").exists())
            refused = run([*launcher, "update"], success=False)
            assert "cannot verify executable of registered Spynel process" in refused.stderr, refused.stderr
            assert live.poll() is None and send.poll() is None
            assert native.read_bytes() == old_binary
            assert next(r for r in records() if r["pid"] == before["pid"])["generation"] == before["generation"]
            # Drain admitted work through the normal terminal boundary before installing.
            (temp / "release").write_text("release")
            assert send.wait(timeout=20) == 0
            run([*launcher, "conversations", "show", "cli", "upgrade"])
            evidence("old-preflight-refusal", authentic_revision=True, admitted_work_preserved=True, work_drained=True, installed_bytes_preserved=True)

            candidate = temp / "candidate-package"
            candidate.mkdir()
            for name in ("package.json", "README.md", "LICENSE", "THIRD_PARTY_NOTICES.md"):
                shutil.copy2(repo / name, candidate / name)
            shutil.copytree(repo / "npm", candidate / "npm", ignore=shutil.ignore_patterns("vendor"))
            run(["node", "-e", "require(process.argv[1]).prepareRelease(process.argv[2],process.argv[3],process.argv[4])", str(repo / "npm/prepare-release.js"), str(candidate), "v" + candidate_version, str("-" in candidate_version.split("+", 1)[0]).lower()])
            packed = run(["npm", "pack", "--json"], cwd=candidate)
            tarball = candidate / json.loads(packed.stdout)[0]["filename"]
            fixture["failure"] = True
            run(["npm", "install", "--global", "--prefix", str(prefix), str(tarball)], success=False)
            fixture["failure"] = False
            assert installed_files() == old_files, "failed npm download changed the old package inventory"
            assert native.read_bytes() == old_binary, "failed npm download did not restore old installation"
            assert live.poll() is None
            evidence("failed-download-rollback", old_package_restored=True, old_server_live=True)
            run(["npm", "install", "--global", "--prefix", str(prefix), str(tarball)])
            installed_hash = hashlib.sha256(native.read_bytes()).hexdigest()
            with tarfile.open(candidate_archive) as archive:
                assert native.read_bytes() == archive.extractfile(next(m for m in archive.getmembers() if m.name.lstrip("./") == "spynel" and m.isfile())).read()
            assert next(r for r in records() if r["pid"] == before["pid"])["generation"] == before["generation"], "external install unexpectedly restarted active server"
            run([*launcher, "update"], timeout=90)
            after = next(r for r in records() if r["pid"] == before["pid"])
            assert after["ready"] and after["version"] == candidate_version and after["generation"] != before["generation"]
            executor = next(r for r in records() if r["pid"] == after["executor_pid"])
            assert executor["ready"] and executor["version"] == candidate_version and executor["generation"] == after["executor_generation"]
            assert executor["supervisor_generation"] == after["generation"]
            workers = [r for r in records() if r.get("harness_worker")]
            assert len(workers) == 1 and workers[0]["version"] == candidate_version and native_start(workers[0]["pid"]) is not None
            assert (registry / f"{zombie.pid}.json").read_bytes() == zombie_record
            assert kernel_zombie(zombie.pid), "upgrade depended on reaping the zombie"
            status = run([*launcher, "status"]).stdout
            assert candidate_version in status, status
            history = [json.loads(line) for p in (workspace / ".spynel/history/cli").glob("*") for line in p.read_text().splitlines()]
            assert sum(r.get("role") == "assistant" and r.get("terminal") and r.get("content") == "fixture work completed" for r in history) == 1, history
            candidate_metadata = subprocess.check_output([os.environ.get("SPYNEL_GO_BINARY", "go"), "version", "-m", str(native)], text=True)
            if sys.platform == "linux":
                for pid in (after["pid"], executor["pid"]):
                    assert hashlib.sha256((Path("/proc") / str(pid) / "exe").read_bytes()).hexdigest() == installed_hash
            evidence(
                "upgrade-complete", result="pass", platform=sys.platform,
                npm_version=run(["npm", "--version"]).stdout.strip(),
                old_revision="f44b827704f80ef16282dd0a0e3b3a23c14abade",
                old_archive_sha256=hashlib.sha256(old_archive.read_bytes()).hexdigest(),
                old_binary_sha256=hashlib.sha256(old_binary).hexdigest(),
                candidate_archive_sha256=hashlib.sha256(candidate_archive.read_bytes()).hexdigest(),
                installed_binary_sha256=installed_hash,
                fixture_source=subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=repo, text=True).strip(),
                candidate_vcs=[line.strip() for line in candidate_metadata.splitlines() if "build\tvcs" in line],
                ready_supervisor_and_executor=True, zombie_retained=True, work_terminal_once=True,
                before_instance={k: before[k] for k in ("pid", "generation", "version", "ready")},
                after_instance={k: after[k] for k in ("pid", "generation", "version", "ready")},
                executor={k: executor[k] for k in ("pid", "generation", "version", "ready")},
            )
        except BaseException:
            for log in logs:
                log.flush()
                log.seek(0)
                print(log.read()[-2000:], file=sys.stderr)
            raise
        finally:
            # Controller exit detaches workers; stop the whole owned installation
            # before removing its images, registry and worker journal directories.
            cleanup_records = records()
            cleanup_starts = {r["pid"]: native_start(r["pid"]) for r in cleanup_records}
            worker_roots = []
            for path in (registry.parent / "harness").glob("*.lock.worker.json"):
                info = json.loads(path.read_text())["Info"]
                worker_roots.extend((path, Path(info["Socket"]).parent))
            evidence("cleanup-owned-identities", records=cleanup_records, process_starts=cleanup_starts, worker_roots=[str(path) for path in worker_roots])
            preserved = {p: p.read_bytes() for cwd in (workspace, zombie_workspace) for p in [cwd / ".spynel/config.yaml"] if p.exists()}
            preserved.update({p: p.read_bytes() for p in (workspace / ".spynel/history/cli").glob("*") if p.is_file()})
            cleanup_error = None
            if package.exists():
                try:
                    cleaned = subprocess.run([str(cleanup_runtime / "spynel"), "uninstall-bundles", "--root", str(temp / "absent-standalone"), "--npm-root", str(package)], cwd=workspace, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=40)
                    if cleaned.returncode != 0:
                        cleanup_error = cleaned.stderr
                except subprocess.TimeoutExpired as error:
                    cleanup_error = str(error)
            for child in children:
                if child.poll() is None:
                    child.terminate()
            for child in children:
                try:
                    child.wait(timeout=20)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait(timeout=10)
            if zombie is not None:
                zombie.wait(timeout=10)
            if sys.platform == "linux":
                deadline = time.monotonic() + 10
                while time.monotonic() < deadline:
                    try:
                        if os.waitpid(-1, os.WNOHANG)[0] == 0:
                            time.sleep(.05)
                    except ChildProcessError:
                        break
            for log in logs:
                log.close()
            server.shutdown()
            server.server_close()
            assert cleanup_error is None, cleanup_error
            assert not package.exists(), "fixture package survived uninstall"
            assert all(start is None or native_start(pid) != start for pid, start in cleanup_starts.items()), "recorded fixture process survived uninstall"
            assert all(not path.exists() for path in worker_roots), "worker endpoint or rendezvous survived uninstall"
            assert all(path.read_bytes() == data for path, data in preserved.items()), "uninstall changed workspace configuration or history"
            evidence("installation-owned-cleanup", recorded_processes_stopped=True, worker_roots_removed=True, workspace_bytes_preserved=True)


if __name__ == "__main__":
    main()
