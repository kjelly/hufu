"""Build a local benchmark dataset from hufu history.

Reads every context.sqlite (memory items = documents) and session.json
(tasks = queries) under ~/.local/state/hufu, writes dataset.json next to this
script, and prints aggregate statistics only (never content).
"""
import json
import os
import re
import sqlite3
import sys
from pathlib import Path

ROOT = Path.home() / ".local/state/hufu/projects"
OUT = Path(sys.argv[1])
CJK = re.compile(r"[㐀-鿿豈-﫿]")


def norm(text):
    return re.sub(r"\s+", " ", (text or "")).strip()


docs = {}
for db in sorted(ROOT.glob("*/teams/*/context.sqlite")):
    workspace = f"{db.parent.parent.parent.name}/{db.parent.name}"
    con = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    rows = con.execute(
        "SELECT id, content, content_hash, lifecycle, json_extract(source_json,'$.type'), "
        "json_extract(source_json,'$.ref'), COALESCE(session_id,'')<>'' OR COALESCE(agent_id,'')<>'' "
        "OR COALESCE(task_id,'')<>'' FROM context_items")
    for item_id, content, content_hash, lifecycle, source_type, source_ref, scoped in rows:
        content = (content or "").strip()
        if not content:
            continue
        persistent = not scoped and lifecycle in ("confirmed", "candidate")
        if content_hash in docs:
            docs[content_hash]["persistent"] |= persistent
            if source_type == "task_result_reducer":
                docs[content_hash]["source_type"], docs[content_hash]["source_ref"] = source_type, source_ref or ""
            continue
        docs[content_hash] = {
            "id": item_id, "workspace": workspace, "text": content, "lifecycle": lifecycle,
            "source_type": source_type, "source_ref": source_ref or "",
            "persistent": persistent,
        }
    con.close()

def session_tasks(team_dir):
    try:
        data = json.loads((team_dir / "session.json").read_text())
        yield from data.get("tasks") or []
    except (OSError, json.JSONDecodeError):
        pass
    try:
        tree = json.loads((team_dir / "session_tree.json").read_text())
        for branch in (tree.get("branches") or {}).values():
            yield from ((branch or {}).get("state") or {}).get("task_plan") or []
    except (OSError, json.JSONDecodeError):
        pass


queries = {}
team_dirs = {p.parent for p in ROOT.glob("*/teams/*/session.json")} | {p.parent for p in ROOT.glob("*/teams/*/session_tree.json")}
for team_dir in sorted(team_dirs):
    workspace = f"{team_dir.parent.parent.name}/{team_dir.name}"
    for task in session_tasks(team_dir):
        goal = norm(task.get("Desc"))
        if len(goal) < 8:
            continue
        receipt = task.get("execution_receipt") or {}
        key = goal
        entry = queries.setdefault(key, {"goal": goal, "agent": (task.get("Agent") or "").lower(),
                                         "workspace": workspace, "task_refs": []})
        if receipt.get("run_id") and task.get("ID") is not None:
            entry["task_refs"].append(f"{receipt['run_id']}:{task['ID']}:")

# Known-item relevance: task_result_reducer items whose ref is run:task:attempt.
by_prefix = {}
for content_hash, doc in docs.items():
    if doc["source_type"] == "task_result_reducer" and doc["source_ref"].count(":") >= 2:
        run, task, _ = doc["source_ref"].rsplit(":", 2)
        by_prefix.setdefault(f"{run}:{task}:", []).append(content_hash)
for entry in queries.values():
    entry["relevant"] = sorted({h for ref in entry["task_refs"] for h in by_prefix.get(ref, [])})

dataset = {"docs": [{"hash": h, **d} for h, d in docs.items()], "queries": list(queries.values())}
OUT.write_text(json.dumps(dataset, ensure_ascii=False))
os.chmod(OUT, 0o600)

lengths = sorted(len(d["text"]) for d in docs.values())
qlen = sorted(len(q["goal"]) for q in queries.values())
labelled = [q for q in queries.values() if q["relevant"]]
print(json.dumps({
    "docs": len(docs),
    "docs_persistent_retrievable": sum(d["persistent"] for d in docs.values()),
    "docs_with_cjk": sum(bool(CJK.search(d["text"])) for d in docs.values()),
    "doc_chars_p50_p95_max": [lengths[len(lengths) // 2], lengths[int(len(lengths) * .95)], lengths[-1]],
    "doc_source_types": {t: sum(d["source_type"] == t for d in docs.values()) for t in {d["source_type"] for d in docs.values()}},
    "queries_distinct": len(queries),
    "queries_with_cjk": sum(bool(CJK.search(q["goal"])) for q in queries.values()),
    "query_chars_p50_p95_max": [qlen[len(qlen) // 2], qlen[int(len(qlen) * .95)], qlen[-1]] if qlen else None,
    "known_item_queries": len(labelled),
    "known_item_queries_with_cjk": sum(bool(CJK.search(q["goal"])) for q in labelled),
    "workspaces_with_queries": len({q["workspace"] for q in queries.values()}),
}, ensure_ascii=False, indent=1))
