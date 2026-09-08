#!/usr/bin/env python3
"""
Export one OpenAI Codex CLI session and any referenced child/subagent rollouts
for diagnosis of long-running / high-token sessions.

Uses Python standard library only.

Examples:
  python3 codex-session-export.py \
    01a0775b-7ec2-7732-8c73-6733195a720b \
    --repo ~/nfs/github/hufu

  python3 codex-session-export.py SESSION_ID --repo . --summary-only
"""

from __future__ import annotations

import argparse
import collections
import datetime as dt
import json
import os
import re
import shutil
import subprocess
import sys
import tarfile
from pathlib import Path
from typing import Any

UUID_RE = re.compile(
    rb"(?<![0-9a-fA-F])"
    rb"([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})"
    rb"(?![0-9a-fA-F])"
)

MAX_CHILD_ROLLOUTS = 100
MAX_GIT_DIFF_BYTES = 5 * 1024 * 1024


def eprint(*args: object) -> None:
    print(*args, file=sys.stderr)


def run(cmd: list[str], cwd: Path | None = None, timeout: int = 30) -> tuple[int, str]:
    try:
        p = subprocess.run(
            cmd,
            cwd=str(cwd) if cwd else None,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            errors="replace",
            timeout=timeout,
            check=False,
        )
        return p.returncode, p.stdout
    except Exception as exc:
        return 127, f"{type(exc).__name__}: {exc}\n"


def rollout_id_from_name(path: Path) -> str | None:
    m = re.search(
        r"([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-"
        r"[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\.jsonl$",
        path.name,
    )
    return m.group(1).lower() if m else None


def build_rollout_index(codex_home: Path) -> dict[str, Path]:
    index: dict[str, Path] = {}
    for root_name in ("sessions", "archived_sessions"):
        root = codex_home / root_name
        if not root.is_dir():
            continue
        for path in root.rglob("rollout-*.jsonl"):
            sid = rollout_id_from_name(path)
            if sid:
                # Prefer live sessions if the same id exists in both locations.
                if sid not in index or root_name == "sessions":
                    index[sid] = path
    return index


def referenced_uuids(path: Path) -> set[str]:
    ids: set[str] = set()
    try:
        with path.open("rb") as f:
            for line in f:
                for m in UUID_RE.finditer(line):
                    ids.add(m.group(1).decode("ascii").lower())
    except Exception as exc:
        eprint(f"warning: unable to scan {path}: {exc}")
    return ids


def discover_rollouts(
    parent_sid: str, index: dict[str, Path]
) -> tuple[list[Path], list[str]]:
    parent_sid = parent_sid.lower()
    parent = index.get(parent_sid)
    if not parent:
        return [], []

    found: dict[str, Path] = {parent_sid: parent}
    queue = collections.deque([parent_sid])
    unresolved: set[str] = set()

    while queue and len(found) < MAX_CHILD_ROLLOUTS:
        sid = queue.popleft()
        path = found[sid]
        for ref in referenced_uuids(path):
            if ref in found:
                continue
            child = index.get(ref)
            if child:
                found[ref] = child
                queue.append(ref)
                if len(found) >= MAX_CHILD_ROLLOUTS:
                    break
            else:
                unresolved.add(ref)

    # Parent first, then children by mtime/path for deterministic output.
    paths = [parent]
    children = [p for s, p in found.items() if s != parent_sid]
    children.sort(key=lambda p: (p.stat().st_mtime, str(p)))
    paths.extend(children)
    return paths, sorted(unresolved)


def nested_get(obj: Any, *keys: str) -> Any:
    cur = obj
    for key in keys:
        if not isinstance(cur, dict):
            return None
        cur = cur.get(key)
    return cur


def compact_token_snapshot(payload: Any) -> dict[str, Any] | None:
    if not isinstance(payload, dict):
        return None

    result: dict[str, Any] = {}

    def walk(obj: Any, prefix: str = "", depth: int = 0) -> None:
        if depth > 5:
            return
        if isinstance(obj, dict):
            for k, v in obj.items():
                key = f"{prefix}.{k}" if prefix else str(k)
                lk = str(k).lower()
                if isinstance(v, (int, float)) and "token" in lk:
                    result[key] = v
                elif isinstance(v, (dict, list)):
                    walk(v, key, depth + 1)
        elif isinstance(obj, list):
            for i, v in enumerate(obj[:20]):
                walk(v, f"{prefix}[{i}]", depth + 1)

    walk(payload)
    return result or None


def summarize_rollout(path: Path) -> dict[str, Any]:
    type_counts: collections.Counter[str] = collections.Counter()
    payload_type_counts: collections.Counter[str] = collections.Counter()
    byte_counts_by_type: collections.Counter[str] = collections.Counter()

    first_ts = None
    last_ts = None
    session_meta = None
    last_token_snapshot = None
    line_count = 0
    parse_errors = 0
    largest_line_bytes = 0

    with path.open("rb") as f:
        for raw in f:
            line_count += 1
            largest_line_bytes = max(largest_line_bytes, len(raw))
            try:
                row = json.loads(raw)
            except Exception:
                parse_errors += 1
                continue

            ts = row.get("timestamp") if isinstance(row, dict) else None
            if ts:
                first_ts = first_ts or ts
                last_ts = ts

            top_type = row.get("type", "<missing>") if isinstance(row, dict) else "<non-object>"
            type_counts[str(top_type)] += 1
            byte_counts_by_type[str(top_type)] += len(raw)

            payload = row.get("payload") if isinstance(row, dict) else None
            payload_type = payload.get("type") if isinstance(payload, dict) else None
            if payload_type is not None:
                payload_type_counts[str(payload_type)] += 1

            if top_type == "session_meta" and session_meta is None and isinstance(payload, dict):
                session_meta = {
                    k: payload.get(k)
                    for k in (
                        "id",
                        "session_id",
                        "timestamp",
                        "cwd",
                        "originator",
                        "cli_version",
                        "source",
                        "model_provider",
                    )
                    if k in payload
                }

            if (
                top_type == "event_msg"
                and isinstance(payload, dict)
                and payload.get("type") in ("token_count", "token_count_event")
            ):
                snap = compact_token_snapshot(payload)
                if snap:
                    last_token_snapshot = snap

    stat = path.stat()
    return {
        "file": str(path),
        "session_id": rollout_id_from_name(path),
        "size_bytes": stat.st_size,
        "line_count": line_count,
        "parse_errors": parse_errors,
        "largest_line_bytes": largest_line_bytes,
        "first_timestamp": first_ts,
        "last_timestamp": last_ts,
        "session_meta": session_meta,
        "top_level_type_counts": dict(type_counts.most_common()),
        "payload_type_counts": dict(payload_type_counts.most_common()),
        "bytes_by_top_level_type": dict(byte_counts_by_type.most_common()),
        "last_token_snapshot": last_token_snapshot,
    }


def write_git_report(repo: Path, outdir: Path) -> None:
    report_dir = outdir / "git"
    report_dir.mkdir(parents=True, exist_ok=True)

    commands = {
        "status.txt": ["git", "status", "--short", "--branch"],
        "head.txt": ["git", "rev-parse", "HEAD"],
        "remote.txt": ["git", "remote", "-v"],
        "recent-log.txt": [
            "git",
            "log",
            "-n",
            "40",
            "--date=iso-strict",
            "--pretty=format:%H%x09%ad%x09%an%x09%s",
        ],
        "diff-stat.txt": ["git", "diff", "--stat"],
        "diff-name-status.txt": ["git", "diff", "--name-status"],
        "cached-diff-stat.txt": ["git", "diff", "--cached", "--stat"],
        "cached-diff-name-status.txt": ["git", "diff", "--cached", "--name-status"],
    }

    for name, cmd in commands.items():
        rc, text = run(cmd, cwd=repo, timeout=30)
        (report_dir / name).write_text(
            f"$ {' '.join(cmd)}\nexit={rc}\n\n{text}",
            encoding="utf-8",
        )

    # Full working-tree diff is extremely useful for "spent lots, produced little"
    # diagnosis, but cap it to avoid giant archives.
    rc, diff = run(["git", "diff", "--no-ext-diff"], cwd=repo, timeout=60)
    encoded = diff.encode("utf-8", errors="replace")
    if len(encoded) <= MAX_GIT_DIFF_BYTES:
        (report_dir / "working-tree.diff").write_bytes(encoded)
    else:
        (report_dir / "working-tree.diff.omitted.txt").write_text(
            f"git diff omitted: {len(encoded)} bytes exceeds "
            f"{MAX_GIT_DIFF_BYTES} byte safety cap.\n",
            encoding="utf-8",
        )


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("session_id", help="Codex session/thread UUID")
    ap.add_argument(
        "--repo",
        type=Path,
        default=Path.cwd(),
        help="Hufu repository path (default: current directory)",
    )
    ap.add_argument(
        "--codex-home",
        type=Path,
        default=Path(os.environ.get("CODEX_HOME", "~/.codex")).expanduser(),
        help="Codex home (default: $CODEX_HOME or ~/.codex)",
    )
    ap.add_argument(
        "--summary-only",
        action="store_true",
        help="Do not include raw rollout JSONL files",
    )
    ap.add_argument(
        "--output",
        type=Path,
        default=None,
        help="Output .tar.gz path",
    )
    args = ap.parse_args()

    sid = args.session_id.lower()
    repo = args.repo.expanduser().resolve()
    codex_home = args.codex_home.expanduser().resolve()

    if not re.fullmatch(
        r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}", sid
    ):
        eprint("error: session_id is not a UUID")
        return 2

    if not codex_home.is_dir():
        eprint(f"error: Codex home does not exist: {codex_home}")
        return 2

    print(f"Indexing rollouts under: {codex_home}")
    index = build_rollout_index(codex_home)
    print(f"Indexed rollout files: {len(index)}")

    rollouts, unresolved = discover_rollouts(sid, index)
    if not rollouts:
        eprint(f"error: session {sid} not found under sessions/ or archived_sessions/")
        eprint("Try:")
        eprint(
            f"  find '{codex_home}/sessions' '{codex_home}/archived_sessions' "
            f"-type f -name '*{sid}*.jsonl' -print 2>/dev/null"
        )
        return 3

    stamp = dt.datetime.now().strftime("%Y%m%d-%H%M%S")
    work = Path.cwd() / f"codex-session-{sid[:8]}-{stamp}"
    if work.exists():
        shutil.rmtree(work)
    work.mkdir(parents=True)

    summaries = []
    raw_dir = work / "rollouts"
    if not args.summary_only:
        raw_dir.mkdir()

    for path in rollouts:
        print(f"Analyzing: {path}")
        summaries.append(summarize_rollout(path))
        if not args.summary_only:
            # Prefix with session ID to avoid filename collisions.
            rid = rollout_id_from_name(path) or "unknown"
            shutil.copy2(path, raw_dir / f"{rid}--{path.name}")

    (work / "rollout-summary.json").write_text(
        json.dumps(
            {
                "requested_session_id": sid,
                "codex_home": str(codex_home),
                "rollout_count": len(rollouts),
                "max_child_rollouts_cap": MAX_CHILD_ROLLOUTS,
                "unresolved_uuid_reference_count": len(unresolved),
                "rollouts": summaries,
            },
            indent=2,
            ensure_ascii=False,
        ),
        encoding="utf-8",
    )

    rc, version = run(["codex", "--version"], timeout=10)
    (work / "codex-version.txt").write_text(
        f"exit={rc}\n{version}", encoding="utf-8"
    )

    if repo.is_dir():
        rc, inside = run(["git", "rev-parse", "--is-inside-work-tree"], cwd=repo)
        if rc == 0 and "true" in inside.lower():
            write_git_report(repo, work)
        else:
            (work / "git-not-collected.txt").write_text(
                f"Not a Git work tree: {repo}\n{inside}",
                encoding="utf-8",
            )

    (work / "README.txt").write_text(
        f"""Codex session diagnostic export

Requested session:
  {sid}

Collected rollouts:
  {len(rollouts)}

Contains raw rollout JSONL:
  {not args.summary_only}

IMPORTANT PRIVACY NOTE
Raw Codex rollout JSONL can contain prompts, source-code excerpts, shell commands,
tool output, local paths, and potentially sensitive values that appeared in the
session. Review the archive before sharing it outside a trusted context.

For the strongest diagnosis of:
- parent/subagent context duplication
- repeated tool/test calls
- compaction churn
- model/reasoning routing
- no-progress loops
- actual code/diff progress

the raw rollout JSONLs are preferable to --summary-only.
""",
        encoding="utf-8",
    )

    out = args.output
    if out is None:
        out = Path.cwd() / f"codex-session-{sid[:8]}-{stamp}.tar.gz"
    out = out.expanduser().resolve()

    with tarfile.open(out, "w:gz") as tf:
        tf.add(work, arcname=work.name)

    shutil.rmtree(work)
    print()
    print(f"Created: {out}")
    print(f"Rollouts included: {len(rollouts)}")
    if args.summary_only:
        print("Mode: summary-only")
    else:
        print("Mode: raw rollouts + summary + Git state")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
