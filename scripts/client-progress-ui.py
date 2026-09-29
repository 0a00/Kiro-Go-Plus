"""Opt-in interactive Claude Code regression against a local fake Kiro upstream."""
import argparse
import fcntl
import json
import os
from pathlib import Path
import pty
import select
import signal
import struct
import subprocess
import tempfile
import termios
import time
from urllib.parse import urlparse

import pyte


def stop(process):
    if process and process.poll() is None:
        os.killpg(process.pid, signal.SIGTERM)
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--artifact-parent", help="Parent directory for a new private fixture directory")
    args = parser.parse_args()
    os.umask(0o077)
    root = Path(tempfile.mkdtemp(prefix="kiro-ui-progress-", dir=args.artifact_parent)).resolve()
    project = Path(__file__).resolve().parent.parent
    workspace = root / "gateway"
    workspace.mkdir()
    env = os.environ.copy()
    # Test with a fresh config, fake credentials, and explicit loopback URL.
    for name in list(env):
        if name.startswith(("ANTHROPIC_", "CLAUDE_", "KIRO_")):
            del env[name]
    fixture = client = None
    master = slave = None
    snapshots = []
    raw = bytearray()
    success = False
    error = ""
    version = subprocess.check_output(["claude", "--version"], text=True).strip()
    log = open(root / "fixture.log", "wb")
    try:
        fixture = subprocess.Popen(
            ["go", "test", "./proxy", "-run", "^TestClaudeCodeProgressUIFixture$", "-count=1", "-timeout=3m"],
            cwd=project, env={**env, "KIRO_DEV_UI_FIXTURE_DIR": str(root)},
            stdout=log, stderr=log, start_new_session=True,
        )
        deadline = time.monotonic() + 60
        while not (root / "gateway-url.txt").exists():
            if fixture.poll() is not None or time.monotonic() > deadline:
                raise RuntimeError("local fixture failed to start")
            time.sleep(0.1)
        base = (root / "gateway-url.txt").read_text().strip()
        parsed = urlparse(base)
        if parsed.scheme != "http" or parsed.hostname != "127.0.0.1" or parsed.username:
            raise RuntimeError("fixture URL is not loopback")
        env["ANTHROPIC_" + "API" + "_KEY"] = "fixture-only"
        env["ANTHROPIC_BASE_URL"] = base
        env["CLAUDE_CONFIG_DIR"] = str(root / "claude-config")
        env["TERM"] = "xterm-256color"
        master, slave = pty.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 32, 120, 0, 0))
        client = subprocess.Popen(
            ["claude", "--bare", "--restricted", "--setting-sources", "project", "--model", "claude-sonnet-4-5",
             "--tools", "Read,Edit", "--allowedTools", "Read,Edit", "--permission-mode", "acceptEdits",
             "Create progress.txt in this disposable workspace."],
            cwd=workspace, env=env, stdin=slave, stdout=slave, stderr=slave, start_new_session=True,
        )
        os.close(slave)
        slave = None
        screen = pyte.Screen(120, 32)
        stream = pyte.ByteStream(screen)
        answered = set()
        deadline = time.monotonic() + 90
        while client.poll() is None and time.monotonic() < deadline:
            ready, _, _ = select.select([master], [], [], 0.2)
            if ready:
                try:
                    data = os.read(master, 65536)
                except OSError:
                    break
                if not data:
                    break
                raw.extend(data)
                if len(raw) > 8 * 1024 * 1024:
                    raise RuntimeError("terminal capture limit exceeded")
                stream.feed(data)
            display = "\n".join(screen.display)
            # Accept only the fresh test workspace and the fake fixture key.
            for name, match, answer in [
                ("theme", "Choose the text style", b"\r"),
                ("key", "Detected a custom API key", b"\x1b[A\r"),
                ("security", "Press Enter to continue", b"\r"),
                ("trust", "Yes, I trust this folder", b"\x1b[B\r"),
            ]:
                if match in display and name not in answered:
                    answered.add(name)
                    time.sleep(0.7)
                    if len(answer) > 1:
                        os.write(master, answer[:-1])
                        time.sleep(0.3)
                    os.write(master, answer[-1:])
            marker = root / "gateway-started.json"
            if not marker.exists():
                continue
            elapsed = time.time() - json.loads(marker.read_text())["at"] / 1000
            for seconds in (4, 8, 12, 17):
                if elapsed >= seconds and seconds not in answered:
                    answered.add(seconds)
                    visible = "expand the file" in display
                    snapshots.append({"atSeconds": seconds, "textVisible": visible, "display": display})
                    print(json.dumps({"seconds": seconds, "textVisible": visible}), flush=True)
            if elapsed > 18 and "FIXTURE_COMPLETED" in display:
                expected = "progress fixture\n" * 10
                success = (len(snapshots) == 4 and all(x["textVisible"] for x in snapshots)
                           and (workspace / "progress.txt").read_text() == expected)
                break
        if not success:
            error = "early text rendering or final file completion was not verified"
    except Exception as exc:
        error = str(exc)
    finally:
        stop(client)
        if master is not None:
            os.close(master)
        if slave is not None:
            os.close(slave)
        (root / "fixture-stop").write_text("done")
        if fixture and fixture.poll() is None:
            try:
                fixture.wait(timeout=5)
            except subprocess.TimeoutExpired:
                stop(fixture)
        if fixture and fixture.returncode != 0:
            success = False
            error = error or "fixture exited unsuccessfully"
        log.close()
        (root / "terminal.ansi").write_bytes(raw)
        (root / "result.json").write_text(json.dumps({"version": version, "pass": success, "error": error, "snapshots": snapshots}, indent=2))
        print(json.dumps({"pass": success, "error": error, "artifactDirectory": str(root)}), flush=True)
    return 0 if success else 1


if __name__ == "__main__":
    raise SystemExit(main())
