#!/usr/bin/env python3
"""Exercise only fixture-owned native/npm instances and disposable PTYs."""
import fcntl
import ctypes
import json
import os
from pathlib import Path
import pty
import shutil
import signal
import struct
import subprocess
import sys
import tarfile
import tempfile
import termios
import threading
import time


def wait_for(check, timeout=45):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if check():
            return
        time.sleep(0.03)
    raise AssertionError("fixture did not reach expected state")


def assert_terminal_restored(master, original):
    actual = termios.tcgetattr(master)
    # Darwin sets PENDIN on raw-to-canonical restoration; it is kernel state.
    pending = termios.PENDIN if sys.platform == "darwin" else 0
    assert (actual[3] ^ original[3]) & ~pending == 0, (
        f"terminal local flags differ: actual={actual[3]:#x} "
        f"expected={original[3]:#x} ignored={pending:#x}"
    )
    assert actual[:3] + actual[4:] == original[:3] + original[4:], "terminal input/output/control state was not restored"


def main():
    repo = Path(__file__).resolve().parent.parent
    if sys.platform == "linux":
        # Adopt only this fixture's orphaned provider so cleanup can wait it.
        assert ctypes.CDLL(None).prctl(36, 1, 0, 0, 0) == 0
    with tempfile.TemporaryDirectory(prefix=".tmp-native-supervisor-", dir=repo) as temporary:
        root = Path(temporary)
        package = root / "prefix" / "lib" / "node_modules" / "spynel"
        vendor = package / "npm" / "vendor"
        vendor.mkdir(parents=True)
        with tarfile.open(sys.argv[1]) as archive:
            for member in archive.getmembers():
                assert member.isfile() or member.isdir()
                assert not Path(member.name).is_absolute() and ".." not in Path(member.name).parts
            archive.extractall(vendor)
        binary = vendor / "spynel"
        version = subprocess.check_output([str(binary), "--version"], text=True).strip().split()[1]
        for name in ("bin/spynel.js", "platform.js", "update.js"):
            target = package / "npm" / name
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(repo / "npm" / name, target)
        (package / "package.json").write_text(json.dumps({"name": "spynel", "version": version}))
        (vendor / ".installed.json").write_text(json.dumps({"version": version}))
        home = root / "home"
        home.mkdir()
        env = {k: v for k, v in os.environ.items() if not k.startswith("SPYNEL_")}
        env.update(HOME=str(home), XDG_CONFIG_HOME=str(home / ".config"), TERM="xterm-256color", SPYNEL_SKIP_UPDATE_CHECK="1")
        registry = home / ("Library/Application Support" if sys.platform == "darwin" else ".config") / "spynel" / "processes"
        processes, terminals, providers, adopted = [], [], [], []

        def records():
            result = []
            for path in registry.glob("*.json"):
                try:
                    result.append(json.loads(path.read_text()))
                except (FileNotFoundError, json.JSONDecodeError):
                    pass
            return result

        def child():
            current = records()
            parent = next((r for r in current if r.get("supervisor") and r.get("ready")), None)
            return next((r for r in current if parent and r["pid"] == parent.get("executor_pid") and r["generation"] == parent.get("executor_generation") and r.get("ready")), None)

        def workspace(name, command=None):
            directory = root / name
            directory.mkdir()
            subprocess.run([str(binary), "init", "--no-start"], cwd=directory, env=env, check=True, stdout=subprocess.DEVNULL)
            config = directory / ".spynel" / "config.yaml"
            config.write_text('harness:\n  name: acp\n  acp_command: ' + json.dumps(str(command or root / "missing-provider")) + '\norchestrator:\n  enabled: false\n  semantic_heartbeat_minutes: 0\nspeech:\n  enabled: false\n')
            return directory

        def launch(launcher, directory, args=None):
            master, slave = pty.openpty()
            fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 34, 120, 0, 0))
            original = termios.tcgetattr(slave)
            output = bytearray()

            def drain():
                answered = 0
                try:
                    while True:
                        data = os.read(master, 65536)
                        if not data:
                            return
                        output.extend(data)
                        queries = output.count(b"\x1b]11;?")
                        if queries > answered:
                            os.write(master, b"\x1b]11;rgb:0000/0000/0000\x1b\\\x1b[1;1R")
                            answered = queries
                        del output[:-1_000_000]
                except OSError:
                    pass

            def foreground():
                os.setsid()
                fcntl.ioctl(0, termios.TIOCSCTTY, 0)

            launched = time.monotonic()
            process = subprocess.Popen([*launcher, *(args if args is not None else ["serve", "--tui"])], cwd=directory, env=env, stdin=slave, stdout=slave, stderr=slave, preexec_fn=foreground)
            processes.append(process)
            terminals.append(master)
            os.close(slave)
            threading.Thread(target=drain, daemon=True).start()
            if args == []:
                wait_for(lambda: b"Use parent workspace" in output, 15)
                os.write(master, b"\r")
            screens = 2 if args == [] else 1
            wait_for(lambda: child() is not None and output.count(b"\x1b[?1049h") >= screens)
            assert process.poll() is None
            assert sum(not r.get("provider_guardian") for r in records()) == 2, "supervised capture spawned another reader/runtime"
            if sys.platform == "linux" and len(processes) == 1:
                parent = next(r for r in records() if r.get("supervisor"))
                def memory(pid):
                    values = {}
                    for line in Path(f"/proc/{pid}/smaps_rollup").read_text().splitlines():
                        if line.startswith(("Rss:", "Pss:", "Private_Clean:", "Private_Dirty:")):
                            key, value, _ = line.split()
                            values[key.rstrip(":")] = int(value)
                    return values
                def cpu(pid):
                    fields = Path(f"/proc/{pid}/stat").read_text().split(")", 1)[1].split()
                    return int(fields[11]) + int(fields[12])
                before = cpu(parent["pid"])
                time.sleep(1)
                print(json.dumps({"classification": "observed-linux-fixture", "startup_to_admission_ms": round((time.monotonic()-launched-1)*1000), "parent_memory_kib": memory(parent["pid"]), "executor_memory_kib": memory(child()["pid"]), "parent_idle_cpu_ticks_over_one_second": cpu(parent["pid"])-before}), flush=True)
            return process, master, output, original

        def stopped(process, master, original, status=0):
            assert process.wait(timeout=15) == status
            wait_for(lambda: not records(), 3)
            # Check real kernel termios, not escape emission alone.
            assert_terminal_restored(master, original)

        try:
            launchers = ([str(binary)], [shutil.which("node"), str(package / "npm" / "bin" / "spynel.js")])
            for index, launcher in enumerate(launchers):
                directory = workspace("ordinary " + str(index))
                process, master, output, original = launch(launcher, directory)
                os.write(master, b"draft\x03")
                time.sleep(0.2)
                assert process.poll() is None, "Ctrl+C clearing input exited"
                # F6, return, real SIGWINCH, then deliberate quit.
                os.write(master, b"copy fixture\x01\x1b[17~")
                wait_for(lambda: output.count(b"\x1b[?1049l") >= 1, 3)
                os.write(master, b"\r")
                wait_for(lambda: output.count(b"\x1b[?1049h") >= 2, 3)
                fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack("HHHH", 28, 90, 0, 0))
                os.killpg(process.pid, signal.SIGWINCH)
                os.write(master, b"\x1b")
                time.sleep(0.1)
                os.write(master, b"\x03")
                time.sleep(0.1)
                os.write(master, b"/quit\r")
                stopped(process, master, original)
                process, master, output, original = launch(launcher, directory)
                os.write(master, b"\x03")
                stopped(process, master, original)

            directory = workspace("primary recovery")
            uninitialized = directory / "uninitialized child"
            uninitialized.mkdir()
            process, master, output, original = launch(launchers[0], uninitialized, args=[])
            old = child()["pid"]
            os.kill(old, signal.SIGKILL)
            wait_for(lambda: b"bounded delay" in output, 5)
            assert_terminal_restored(master, original)
            try:
                wait_for(lambda: child() is not None and child()["pid"] != old and output.count(b"\x1b[?1049h") >= 3)
            except AssertionError:
                raise AssertionError((process.poll(), records(), bytes(output[-2500:])))
            assert len(records()) == 2, "dead executor registration was not retired"
            assert not (uninitialized / ".spynel").exists(), "recovery repeated workspace initialization"
            if sys.platform == "linux":
                current_pid = child()["pid"]
                assert Path(f"/proc/{current_pid}/cwd").resolve() == directory
            assert not (directory / ".spynel" / "runtime" / "harness-owner").exists()
            # The direct child was waited, so its PID is no longer present.
            try:
                os.kill(old, 0)
                raise AssertionError("executor was not reaped")
            except ProcessLookupError:
                pass
            os.kill(process.pid, signal.SIGTERM)
            stopped(process, master, original, status=1)

            directory = workspace("parent loss")
            # Keep the shell's controlling session alive when its supervisor dies.
            session_owner = root / "session-owner.py"
            session_owner.write_text('''import signal, subprocess, sys
def stop(*_):
    raise SystemExit(0)
signal.signal(signal.SIGTERM, stop)
child = subprocess.Popen(sys.argv[1:])
try:
    child.wait()
    signal.pause()
finally:
    if child.poll() is None:
        child.terminate()
        try:
            child.wait(timeout=15)
        except subprocess.TimeoutExpired:
            child.kill()
            child.wait()
''')
            process, master, output, original = launch([sys.executable, str(session_owner), str(binary)], directory)
            supervisor_pid = next(r["pid"] for r in records() if r.get("supervisor"))
            assert supervisor_pid != process.pid and os.getsid(supervisor_pid) == process.pid
            executor_pid = child()["pid"]
            adopted.append(executor_pid)
            os.kill(supervisor_pid, signal.SIGKILL)
            wait_for(lambda: not any(r["pid"] == executor_pid for r in records()), 12)
            assert process.poll() is None, "fixture shell died with its supervisor"
            assert_terminal_restored(master, original)
            if sys.platform == "linux":
                os.waitpid(executor_pid, 0)
                adopted.remove(executor_pid)
            # Only the killed supervisor's inert record may remain.
            for path in registry.glob("*.json"):
                record = json.loads(path.read_text())
                assert record["pid"] == supervisor_pid
                path.unlink()
            process.terminate()
            assert process.wait(timeout=3) == 0

            directory = workspace("external service")
            managed = subprocess.Popen([str(binary), "serve", "--automatic-startup"], cwd=directory, env=env, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            processes.append(managed)
            wait_for(lambda: len(records()) == 2 and any(r.get("ready") for r in records()), 5)
            assert all(not r.get("supervisor") for r in records()), "external service acquired a competing native supervisor"
            managed.terminate()
            assert managed.wait(timeout=12) == 0
            wait_for(lambda: not records(), 3)

            provider = root / "orphan-provider"
            provider.write_text(f'''#!{sys.executable}
import json, os, pathlib, signal, sys, time
signal.signal(signal.SIGHUP, signal.SIG_IGN)
signal.signal(signal.SIGTERM, signal.SIG_IGN)
root = pathlib.Path({str(root)!r})
with (root / "provider-starts").open("a") as log: log.write(str(os.getpid()) + "\\n")
request = json.loads(sys.stdin.readline())
assert request["method"] == "initialize"
print(json.dumps({{"jsonrpc": "2.0", "id": request["id"], "result": {{"protocolVersion": 1, "agentCapabilities": {{}}}}}}), flush=True)
while True:
    with (root / "synthetic-effects").open("a") as log: log.write("effect\\n")
    time.sleep(0.05)
''')
            provider.chmod(0o700)
            directory = workspace("orphan ownership", provider)
            process, master, output, original = launch(launchers[0], directory)
            wait_for(lambda: (root / "provider-starts").exists())
            providers.extend(int(pid) for pid in (root / "provider-starts").read_text().split())
            old = child()["pid"]
            guardian = next(r["pid"] for r in records() if r.get("provider_guardian"))
            adopted.append(guardian)
            os.kill(old, signal.SIGKILL)
            wait_for(lambda: b"bounded delay" in output, 5)
            assert_terminal_restored(master, original)
            # Recovery waits out election ownership, then admits exactly one replacement.
            wait_for(lambda: child() is not None and child()["pid"] != old and len((root / "provider-starts").read_text().split()) == 2)
            prior = providers.pop()
            try:
                os.kill(prior, 0)
                raise AssertionError("successor admitted before prior provider was reaped")
            except ProcessLookupError:
                pass
            if sys.platform == "linux":
                os.waitpid(guardian, 0)
                adopted.remove(guardian)
            providers.append(int((root / "provider-starts").read_text().split()[-1]))
            assert process.poll() is None, "native supervisor did not recover"
            before = (root / "synthetic-effects").stat().st_size
            wait_for(lambda: (root / "synthetic-effects").stat().st_size > before, 3)
            os.kill(process.pid, signal.SIGTERM)
            stopped(process, master, original, status=1)
            assert not (directory / ".spynel" / "runtime" / "harness-owner").exists()

            failure = subprocess.run([str(binary), "serve", "--invalid-fixture-flag"], cwd=directory, env=env, capture_output=True, timeout=5)
            assert failure.returncode != 0 and b"bounded delay" not in failure.stderr
            assert not records(), "startup failure left running instances"
            print("Verified native/npm PTY quit and Ctrl+C, F6/resize, kernel terminal restoration, primary crash/takeover, guarded provider reaping and automatic replacement after owner loss.")
        finally:
            for process in processes:
                if process.poll() is None:
                    process.terminate()
            for process in processes:
                try:
                    process.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
            for pid in providers:
                try:
                    os.kill(pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                if sys.platform == "linux":
                    try:
                        os.waitpid(pid, 0)
                    except ChildProcessError:
                        pass
            if sys.platform == "linux":
                for pid in adopted:
                    try:
                        if os.waitpid(pid, os.WNOHANG) == (0, 0):
                            os.kill(pid, signal.SIGKILL)
                            os.waitpid(pid, 0)
                    except ChildProcessError:
                        pass
            for master in terminals:
                os.close(master)


if __name__ == "__main__":
    main()
