#!/usr/bin/env python3
"""
shell-use CLI client — connects to the shell-use daemon via Unix socket.

This implements the same command interface as the Rust shell-use binary.
"""

import json
import os
import sys
import socket
from pathlib import Path

SOCKET_DIR = Path("/tmp/shell-use-sock")
SOCKET_PATH = SOCKET_DIR / "daemon.sock"

EXIT_OK = 0
EXIT_ASSERTION = 1
EXIT_USAGE = 2
EXIT_NO_SESSION = 3
EXIT_DAEMON = 4
EXIT_INTERNAL = 5


def send_command(req):
    """Send a command to the daemon and return the response."""
    if not SOCKET_PATH.exists():
        # Try to start daemon
        import subprocess
        subprocess.Popen(
            [sys.executable, "/tmp/shell_use_daemon.py"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            start_new_session=True
        )
        # Wait for socket
        for _ in range(50):
            if SOCKET_PATH.exists():
                break
            import time
            time.sleep(0.1)
        else:
            print(json.dumps({"error": "daemon not available", "kind": "internal"}))
            sys.exit(EXIT_DAEMON)

    try:
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.settimeout(120)
        sock.connect(str(SOCKET_PATH))
        sock.sendall((json.dumps(req) + "\n").encode())
        data = b""
        while True:
            chunk = sock.recv(65536)
            if not chunk:
                break
            data += chunk
            if b"\n" in data:
                break
        sock.close()
        return json.loads(data.decode())
    except (ConnectionRefusedError, FileNotFoundError):
        return {"ok": False, "error": "daemon not running", "exit_code": EXIT_DAEMON}
    except Exception as e:
        return {"ok": False, "error": str(e), "exit_code": EXIT_INTERNAL}


def main():
    args = sys.argv[1:]
    json_output = False
    session = os.environ.get("SHELL_USE_SESSION", "default")

    # Parse global flags
    filtered = []
    i = 0
    while i < len(args):
        if args[i] == "--json":
            json_output = True
        elif args[i] == "--session":
            session = args[i + 1]
            i += 1
        elif args[i] == "--verbose" or args[i] == "-v":
            pass  # Ignored
        else:
            filtered.append(args[i])
        i += 1

    args = filtered
    if not args:
        print("Usage: shell-use <command> [args...]")
        return EXIT_USAGE

    cmd = args[0]
    rest = args[1:]

    req = {"cmd": cmd, "session": session}

    if cmd == "open":
        i = 0
        while i < len(rest):
            if rest[i] == "--shell" and i + 1 < len(rest):
                req["shell"] = rest[i + 1]; i += 2
            elif rest[i] == "--cols" and i + 1 < len(rest):
                req["cols"] = int(rest[i + 1]); i += 2
            elif rest[i] == "--rows" and i + 1 < len(rest):
                req["rows"] = int(rest[i + 1]); i += 2
            elif rest[i] == "--cwd" and i + 1 < len(rest):
                req["cwd"] = rest[i + 1]; i += 2
            elif rest[i] == "--env" and i + 1 < len(rest):
                k, _, v = rest[i + 1].partition("=")
                req.setdefault("env", {})[k] = v; i += 2
            else:
                i += 1

    elif cmd == "run":
        req["program"] = rest

    elif cmd == "close":
        if rest and rest[0] == "--all":
            req["all"] = True

    elif cmd == "submit":
        req["text"] = rest[0] if rest else ""

    elif cmd == "type":
        req["text"] = rest[0] if rest else ""

    elif cmd == "press":
        req["keys"] = rest

    elif cmd == "wait":
        if not rest:
            print("wait requires a condition type"); return EXIT_USAGE
        req["what"] = rest[0]
        if len(rest) > 1:
            req["value"] = rest[1]
        i = 2
        while i < len(rest):
            if rest[i] == "--regex": req["regex"] = True
            elif rest[i] == "--full": req["full"] = True
            elif rest[i] == "--not": req["not"] = True
            elif rest[i] == "--timeout" and i + 1 < len(rest):
                req["timeout"] = int(rest[i + 1]); i += 1
            i += 1

    elif cmd == "expect":
        if not rest:
            print("expect requires a type"); return EXIT_USAGE
        req["what"] = rest[0]
        if len(rest) > 1:
            req["value"] = rest[1]
        i = 2
        while i < len(rest):
            if rest[i] == "--regex": req["regex"] = True
            elif rest[i] == "--full": req["full"] = True
            elif rest[i] == "--not": req["not"] = True
            elif rest[i] == "--timeout" and i + 1 < len(rest):
                req["timeout"] = int(rest[i + 1]); i += 1
            i += 1

    elif cmd == "text":
        if rest and rest[0] == "--full":
            req["full"] = True

    elif cmd == "get":
        if not rest:
            print("get requires a field"); return EXIT_USAGE
        req["field"] = rest[0]

    elif cmd == "signal":
        if not rest:
            print("signal requires a name"); return EXIT_USAGE
        req["sig"] = rest[0]

    elif cmd == "resize":
        if len(rest) < 2:
            print("resize requires cols rows"); return EXIT_USAGE
        req["cols"] = int(rest[0])
        req["rows"] = int(rest[1])

    elif cmd == "write":
        if not rest:
            print("write requires data"); return EXIT_USAGE
        req["data"] = rest[0]

    elif cmd == "sessions":
        pass

    elif cmd == "kill":
        pass

    elif cmd == "daemon":
        if rest and rest[0] == "stop":
            req["cmd"] = "daemon_stop"
        elif rest and rest[0] == "status":
            # Just check if daemon is running
            resp = send_command({"cmd": "sessions", "session": session})
            if json_output:
                print(json.dumps({"daemon": "running" if resp.get("ok") else "stopped",
                                  "sessions": resp.get("sessions", [])}))
            else:
                if resp.get("ok"):
                    print(f"Daemon: running")
                    for s in resp.get("sessions", []):
                        print(f"  session: {s}")
                else:
                    print("Daemon: not running")
            return EXIT_OK
        else:
            print("daemon requires status|stop"); return EXIT_USAGE

    elif cmd == "usage":
        print("""shell-use commands:
  open [--shell S] [--cols N --rows N] [--cwd D] [--env K=V]
  run <program> [args...]
  close [--all]
  sessions
  submit ["text"]
  type "text"
  press <Key...>
  text [--full]
  state
  get command|output|exit-code|cwd|size
  wait text "T" [--regex --full --not --timeout MS]
  wait idle [--timeout MS]
  wait command [--timeout MS]
  wait exit [--timeout MS]
  expect text "T" [--regex --full --not --timeout MS]
  expect exit-code N
  expect output "T" [--regex]
  kill
  signal INT|TERM|KILL|QUIT
  daemon status|stop
""")
        return EXIT_OK

    elif cmd == "agent-context":
        print(json.dumps({
            "version": "0.0.1-py",
            "commands": ["open", "run", "close", "sessions", "submit", "type",
                        "press", "wait", "expect", "text", "state", "get",
                        "kill", "signal", "daemon", "resize", "write"],
            "exit_codes": {"0": "success", "1": "assertion", "2": "usage",
                          "3": "no_session", "4": "daemon", "5": "internal"}
        }, indent=2))
        return EXIT_OK

    elif cmd == "skill":
        skill_path = os.path.expanduser("~/no-changed-github/shell-use/SKILL.md")
        if os.path.exists(skill_path):
            with open(skill_path) as f:
                print(f.read())
        return EXIT_OK

    elif cmd == "screenshot":
        # Just output text as a simplified screenshot
        req["cmd"] = "text"
        if rest and rest[0] == "--full":
            req["full"] = True

    else:
        print(f"Unknown command: {cmd}")
        return EXIT_USAGE

    resp = send_command(req)

    if json_output:
        print(json.dumps(resp))
    else:
        if resp.get("ok"):
            if cmd == "text" or cmd == "screenshot":
                print(resp.get("text", ""))
            elif cmd == "get":
                val = resp.get("value", "")
                if isinstance(val, dict):
                    print(json.dumps(val))
                else:
                    print(val)
            elif cmd == "state":
                for k, v in resp.items():
                    if k not in ("ok",):
                        print(f"  {k}: {v}")
            elif cmd == "sessions":
                for s in resp.get("sessions", []):
                    print(f"  {s}")
                if not resp.get("sessions"):
                    print("No active sessions")
            else:
                # For other commands, just print a summary
                for k in ("submitted", "closed", "session", "result"):
                    if k in resp:
                        print(f"{k}: {resp[k]}")
        else:
            err = resp.get("error", "unknown error")
            print(f"ERROR: {err}", file=sys.stderr)
            if "tail" in resp:
                print(f"  Output tail: {resp['tail'][-100:]}", file=sys.stderr)

    return resp.get("exit_code", EXIT_OK if resp.get("ok") else EXIT_INTERNAL)


if __name__ == "__main__":
    sys.exit(main())
