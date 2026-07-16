#!/usr/bin/env python3
"""
shell-use daemon — persistent PTY session manager.

Runs as a background daemon, holding PTY sessions. CLI clients connect
via Unix socket to send commands and receive responses.
"""

import json
import os
import pty
import re
import select
import signal
import struct
import sys
import time
import fcntl
import termios
import socket
import threading
from pathlib import Path

SOCKET_DIR = Path("/tmp/shell-use-sock")
SOCKET_DIR.mkdir(parents=True, exist_ok=True)
SOCKET_PATH = SOCKET_DIR / "daemon.sock"

class Session:
    def __init__(self, name, pid, master_fd, cols=80, rows=30, cwd=None):
        self.name = name
        self.pid = pid
        self.master_fd = master_fd
        self.cols = cols
        self.rows = rows
        self.cwd = cwd or os.getcwd()
        self.output_buffer = ""
        self.scrollback = ""
        self.last_command = ""
        self.last_exit_code = None
        self.recording = []
        self.start_time = time.time()
        self._last_change = time.time()
        self._alive = True

    def read_available(self, timeout=0.05):
        data = b""
        while True:
            try:
                r, _, _ = select.select([self.master_fd], [], [], timeout)
            except (OSError, ValueError):
                self._alive = False
                break
            if r:
                try:
                    chunk = os.read(self.master_fd, 65536)
                    if not chunk:
                        self._alive = False
                        break
                    data += chunk
                    self._last_change = time.time()
                    self.recording.append((time.time() - self.start_time, "o", chunk.decode("utf-8", errors="replace")))
                    timeout = 0.01
                except OSError:
                    self._alive = False
                    break
            else:
                break
        if data:
            text = data.decode("utf-8", errors="replace")
            self.output_buffer += text
            self.scrollback += text
            if len(self.scrollback) > 200000:
                self.scrollback = self.scrollback[-100000:]
        return data

    def write(self, data: bytes):
        try:
            os.write(self.master_fd, data)
        except OSError:
            self._alive = False

    def strip_ansi(self, text):
        ansi_re = re.compile(r'\x1b\[[0-9;]*[a-zA-Z]|\x1b\][^\x07]*\x07|\x1b\][^\x1b]*\x1b\\|\x1b[()][AB012]|\r')
        text = ansi_re.sub('', text)
        osc_re = re.compile(r'\x1b\]133[^\x1b]*\x1b\\|\x1b\]133;.*?\x07')
        text = osc_re.sub('', text)
        return text

    def get_text(self, full=False):
        self.read_available(0.05)
        return self.strip_ansi(self.scrollback if full else self.output_buffer)

    def is_idle(self, quiet_ms=250):
        self.read_available(0.01)
        return (time.time() - self._last_change) * 1000 >= quiet_ms

    def is_alive(self):
        if not self._alive:
            return False
        try:
            os.kill(self.pid, 0)
            return True
        except OSError:
            self._alive = False
            return False

    def close(self):
        try:
            os.close(self.master_fd)
        except OSError:
            pass
        try:
            os.kill(self.pid, signal.SIGKILL)
        except OSError:
            pass
        try:
            os.waitpid(self.pid, os.WNOHANG)
        except OSError:
            pass
        self._alive = False


class Daemon:
    def __init__(self):
        self.sessions = {}
        self.running = True
        self._lock = threading.Lock()

    def handle(self, req):
        cmd = req.get("cmd")
        session_name = req.get("session", "default")

        if cmd == "open":
            return self._open(req, session_name)
        elif cmd == "run":
            return self._run(req, session_name)
        elif cmd == "close":
            return self._close(req, session_name)
        elif cmd == "sessions":
            return self._sessions()
        elif cmd == "submit":
            return self._submit(req, session_name)
        elif cmd == "type":
            return self._type(req, session_name)
        elif cmd == "press":
            return self._press(req, session_name)
        elif cmd == "wait":
            return self._wait(req, session_name)
        elif cmd == "expect":
            return self._expect(req, session_name)
        elif cmd == "text":
            return self._text(req, session_name)
        elif cmd == "state":
            return self._state(req, session_name)
        elif cmd == "get":
            return self._get(req, session_name)
        elif cmd == "kill":
            return self._kill(session_name)
        elif cmd == "signal":
            return self._signal(req, session_name)
        elif cmd == "daemon_stop":
            self.running = False
            for s in list(self.sessions.values()):
                s.close()
            self.sessions.clear()
            return {"ok": True, "stopped": True}
        elif cmd == "resize":
            return self._resize(req, session_name)
        elif cmd == "write":
            return self._write_raw(req, session_name)
        else:
            return {"ok": False, "error": f"unknown command: {cmd}", "exit_code": 2}

    def _get_session(self, name):
        with self._lock:
            s = self.sessions.get(name)
            if s and s.is_alive():
                return s
            elif s:
                s.close()
                del self.sessions[name]
            return None

    def _open(self, req, name):
        with self._lock:
            if name in self.sessions:
                old = self.sessions[name]
                old.close()
        shell = req.get("shell", os.environ.get("SHELL", "/bin/bash"))
        cols = req.get("cols", 80)
        rows = req.get("rows", 30)
        cwd = req.get("cwd", os.getcwd())
        env_vars = req.get("env", {})

        env = dict(os.environ)
        for k, v in env_vars.items():
            env[k] = v

        # Shell integration for bash
        if "bash" in shell:
            integration = os.path.expanduser("~/no-changed-github/shell-use/shell/shellIntegration.bash")
            if os.path.exists(integration):
                env["BASH_ENV"] = integration

        pid, master_fd = pty.fork()
        if pid == 0:
            os.chdir(cwd)
            winsize = struct.pack("HHHH", rows, cols, 0, 0)
            fcntl.ioctl(sys.stdout.fileno(), termios.TIOCSWINSZ, winsize)
            os.execvpe(shell, [shell], env)
            os._exit(1)

        winsize = struct.pack("HHHH", rows, cols, 0, 0)
        fcntl.ioctl(master_fd, termios.TIOCSWINSZ, winsize)
        flags = fcntl.fcntl(master_fd, fcntl.F_GETFL)
        fcntl.fcntl(master_fd, fcntl.F_SETFL, flags | os.O_NONBLOCK)

        s = Session(name, pid, master_fd, cols, rows, cwd)
        with self._lock:
            self.sessions[name] = s
        time.sleep(0.3)
        s.read_available(0.2)
        return {"ok": True, "session": name, "pid": pid, "cols": cols, "rows": rows}

    def _run(self, req, name):
        with self._lock:
            if name in self.sessions:
                old = self.sessions[name]
                old.close()
        program = req.get("program", [])
        if not program:
            return {"ok": False, "error": "no program", "exit_code": 2}
        cols = req.get("cols", 80)
        rows = req.get("rows", 30)
        cwd = req.get("cwd", os.getcwd())
        env_vars = req.get("env", {})
        env = dict(os.environ)
        for k, v in env_vars.items():
            env[k] = v

        pid, master_fd = pty.fork()
        if pid == 0:
            os.chdir(cwd)
            winsize = struct.pack("HHHH", rows, cols, 0, 0)
            fcntl.ioctl(sys.stdout.fileno(), termios.TIOCSWINSZ, winsize)
            os.execvpe(program[0], program, env)
            os._exit(1)

        winsize = struct.pack("HHHH", rows, cols, 0, 0)
        fcntl.ioctl(master_fd, termios.TIOCSWINSZ, winsize)
        flags = fcntl.fcntl(master_fd, fcntl.F_GETFL)
        fcntl.fcntl(master_fd, fcntl.F_SETFL, flags | os.O_NONBLOCK)

        s = Session(name, pid, master_fd, cols, rows, cwd)
        with self._lock:
            self.sessions[name] = s
        time.sleep(0.2)
        s.read_available(0.1)
        return {"ok": True, "session": name, "pid": pid}

    def _close(self, req, name):
        if req.get("all"):
            with self._lock:
                for s in self.sessions.values():
                    s.close()
                self.sessions.clear()
            return {"ok": True, "closed": "all"}
        s = self._get_session(name)
        if not s:
            return {"ok": False, "error": "no session", "exit_code": 3}
        s.close()
        with self._lock:
            if name in self.sessions:
                del self.sessions[name]
        return {"ok": True, "closed": name}

    def _sessions(self):
        with self._lock:
            names = [n for n, s in self.sessions.items() if s.is_alive()]
        return {"ok": True, "sessions": names}

    def _submit(self, req, name):
        s = self._get_session(name)
        if not s:
            return {"ok": False, "error": "no session", "exit_code": 3}
        text = req.get("text", "")
        s.write((text + "\n").encode())
        s.last_command = text
        s.output_buffer = ""  # Reset for new command output
        s.last_exit_code = None
        return {"ok": True, "submitted": text}

    def _type(self, req, name):
        s = self._get_session(name)
        if not s:
            return {"ok": False, "error": "no session", "exit_code": 3}
        s.write(req.get("text", "").encode())
        return {"ok": True}

    def _press(self, req, name):
        s = self._get_session(name)
        if not s:
            return {"ok": False, "error": "no session", "exit_code": 3}
        key_map = {
            "Enter": "\r", "Return": "\r", "Tab": "\t", "Escape": "\x1b",
            "Backspace": "\x7f", "Space": " ", "Up": "\x1b[A", "Down": "\x1b[B",
            "Right": "\x1b[C", "Left": "\x1b[D", "Home": "\x1b[H", "End": "\x1b[F",
            "PageUp": "\x1b[5~", "PageDown": "\x1b[6~", "Delete": "\x1b[3~",
        }
        output = ""
        for key in req.get("keys", []):
            if key in key_map:
                output += key_map[key]
            elif key.startswith("Ctrl+"):
                ch = key[5:]
                if len(ch) == 1 and ch.isalpha():
                    output += chr(ord(ch.upper()) - 64)
                else:
                    output += chr(ord(ch.lower()) - 96)
            elif len(key) == 1:
                output += key
            else:
                output += key
        s.write(output.encode())
        return {"ok": True}

    def _wait(self, req, name):
        s = self._get_session(name)
        if not s:
            return {"ok": False, "error": "no session", "exit_code": 3}
        what = req.get("what")
        timeout_ms = req.get("timeout", 0)
        if timeout_ms == 0:
            timeout_ms = 5000 if what in ("text", "idle") else 30000
        deadline = time.time() + timeout_ms / 1000.0

        if what == "text":
            target = req.get("value", "")
            is_regex = req.get("regex", False)
            want_not = req.get("not", False)
            full = req.get("full", False)
            while time.time() < deadline:
                s.read_available(0.1)
                text = s.strip_ansi(s.scrollback if full else s.output_buffer)
                found = bool(re.search(target, text)) if is_regex else (target in text)
                if want_not and not found:
                    return {"ok": True, "wait": "text", "result": "ok"}
                elif not want_not and found:
                    return {"ok": True, "wait": "text", "result": "ok"}
                time.sleep(0.05)
            return {"ok": False, "wait": "text", "result": "timeout", "exit_code": 1}

        elif what == "idle":
            while time.time() < deadline:
                if s.is_idle(300):
                    return {"ok": True, "wait": "idle", "result": "ok"}
                time.sleep(0.05)
            return {"ok": False, "wait": "idle", "result": "timeout", "exit_code": 1}

        elif what == "command":
            while time.time() < deadline:
                s.read_available(0.1)
                if s.is_idle(500):
                    s.last_exit_code = 0
                    return {"ok": True, "wait": "command", "result": "ok", "exit_code": 0}
                time.sleep(0.05)
            return {"ok": False, "wait": "command", "result": "timeout", "exit_code": 1}

        elif what == "exit":
            while time.time() < deadline:
                if not s.is_alive():
                    return {"ok": True, "wait": "exit", "result": "ok"}
                time.sleep(0.1)
            return {"ok": False, "wait": "exit", "result": "timeout", "exit_code": 1}

        return {"ok": False, "error": "unknown wait", "exit_code": 2}

    def _expect(self, req, name):
        s = self._get_session(name)
        if not s:
            return {"ok": False, "error": "no session", "exit_code": 3}
        what = req.get("what")
        timeout_ms = req.get("timeout", 5000)
        deadline = time.time() + timeout_ms / 1000.0

        if what == "text":
            target = req.get("value", "")
            is_regex = req.get("regex", False)
            want_not = req.get("not", False)
            full = req.get("full", False)
            while True:
                s.read_available(0.05)
                text = s.strip_ansi(s.scrollback if full else s.output_buffer)
                found = bool(re.search(target, text)) if is_regex else (target in text)
                if want_not and not found:
                    return {"ok": True, "expect": "text", "result": "pass"}
                elif not want_not and found:
                    return {"ok": True, "expect": "text", "result": "pass"}
                if time.time() >= deadline:
                    return {"ok": False, "expect": "text", "result": "fail", "exit_code": 1,
                            "tail": s.strip_ansi(s.output_buffer)[-200:]}
                time.sleep(0.05)

        elif what == "exit-code":
            expected = int(req.get("value", 0))
            s.read_available(0.05)
            actual = s.last_exit_code if s.last_exit_code is not None else 0
            if actual == expected:
                return {"ok": True, "expect": "exit-code", "result": "pass"}
            return {"ok": False, "expect": "exit-code", "result": "fail",
                    "expected": expected, "actual": actual, "exit_code": 1}

        elif what == "output":
            target = req.get("value", "")
            is_regex = req.get("regex", False)
            s.read_available(0.05)
            output = s.strip_ansi(s.output_buffer)
            found = bool(re.search(target, output)) if is_regex else (target in output)
            if found:
                return {"ok": True, "expect": "output", "result": "pass"}
            return {"ok": False, "expect": "output", "result": "fail", "exit_code": 1}

        return {"ok": False, "error": "unknown expect", "exit_code": 2}

    def _text(self, req, name):
        s = self._get_session(name)
        if not s:
            return {"ok": False, "error": "no session", "exit_code": 3}
        return {"ok": True, "text": s.get_text(full=req.get("full", False))}

    def _state(self, req, name):
        s = self._get_session(name)
        if not s:
            return {"ok": False, "error": "no session", "exit_code": 3}
        s.read_available(0.05)
        return {"ok": True, "session": name, "cwd": s.cwd, "cols": s.cols,
                "rows": s.rows, "last_command": s.last_command,
                "last_exit_code": s.last_exit_code,
                "alive": s.is_alive()}

    def _get(self, req, name):
        s = self._get_session(name)
        if not s:
            return {"ok": False, "error": "no session", "exit_code": 3}
        s.read_available(0.05)
        field = req.get("field")
        values = {
            "command": s.last_command,
            "output": s.strip_ansi(s.output_buffer),
            "exit-code": s.last_exit_code,
            "cwd": s.cwd,
            "size": {"cols": s.cols, "rows": s.rows},
        }
        return {"ok": True, "field": field, "value": values.get(field, "")}

    def _kill(self, name):
        s = self._get_session(name)
        if not s:
            return {"ok": False, "error": "no session", "exit_code": 3}
        try:
            os.kill(s.pid, signal.SIGKILL)
        except OSError:
            pass
        return {"ok": True}

    def _signal(self, req, name):
        s = self._get_session(name)
        if not s:
            return {"ok": False, "error": "no session", "exit_code": 3}
        sig_map = {"INT": signal.SIGINT, "TERM": signal.SIGTERM,
                   "KILL": signal.SIGKILL, "QUIT": signal.SIGQUIT}
        sig = sig_map.get(req.get("sig", "TERM").upper(), signal.SIGTERM)
        try:
            os.kill(s.pid, sig)
        except OSError:
            pass
        return {"ok": True}

    def _resize(self, req, name):
        s = self._get_session(name)
        if not s:
            return {"ok": False, "error": "no session", "exit_code": 3}
        cols = req.get("cols", 80)
        rows = req.get("rows", 30)
        s.cols = cols
        s.rows = rows
        winsize = struct.pack("HHHH", rows, cols, 0, 0)
        fcntl.ioctl(s.master_fd, termios.TIOCSWINSZ, winsize)
        return {"ok": True}

    def _write_raw(self, req, name):
        s = self._get_session(name)
        if not s:
            return {"ok": False, "error": "no session", "exit_code": 3}
        s.write(req.get("data", "").encode())
        return {"ok": True}


def main():
    # Remove stale socket
    try:
        os.unlink(SOCKET_PATH)
    except FileNotFoundError:
        pass

    server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    server.bind(str(SOCKET_PATH))
    server.listen(8)
    server.settimeout(1.0)

    daemon = Daemon()

    # Write PID file
    pid_file = SOCKET_DIR / "daemon.pid"
    with open(pid_file, "w") as f:
        f.write(str(os.getpid()))

    while daemon.running:
        try:
            conn, _ = server.accept()
        except socket.timeout:
            continue
        except OSError:
            break

        try:
            data = b""
            while True:
                chunk = conn.recv(65536)
                if not chunk:
                    break
                data += chunk
                if b"\n" in data:
                    break

            req = json.loads(data.decode())
            resp = daemon.handle(req)
            conn.sendall((json.dumps(resp) + "\n").encode())
        except Exception as e:
            try:
                conn.sendall((json.dumps({"ok": False, "error": str(e), "exit_code": 5}) + "\n").encode())
            except:
                pass
        finally:
            conn.close()

    try:
        os.unlink(SOCKET_PATH)
        os.unlink(pid_file)
    except FileNotFoundError:
        pass


if __name__ == "__main__":
    main()
