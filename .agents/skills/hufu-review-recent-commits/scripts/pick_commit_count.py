#!/usr/bin/env python3
"""Pick the largest recent-commit count hufu-code-review can review.

The script builds the team's own workset producer (reviewprep) and runs it for
candidate counts. A count passes only when the producer accepts the scope and
the resulting worksets also fit the limits the run enforces later. The chosen
count is always one the producer actually accepted.
"""

import argparse
import json
import os
import shlex
import shutil
import subprocess
import sys
import tempfile
import time

TEAM = "hufu-code-review"
DEFAULT_MODEL = "ollama/minimax-m3.1-flash:cloud"
PROMPT = "review 最近{count}個的 git commit，告訴我這些修改有無潛在問題"
# Producer errors that reject this scope; any other error means the tool broke.
REJECTIONS = ("scope_too_large", "workset_item_too_large", "documentation_verification_failed")
DEPENDENCY_FILES = {"go.mod", "go.sum", "go.work", "go.work.sum"}
DRIVER = """package main

import (
\t"context"
\t"fmt"
\t"os"
)

func main() {
\tif err := run(context.Background(), os.Stdin, os.Stdout); err != nil {
\t\tfmt.Fprintln(os.Stderr, err)
\t\tos.Exit(1)
\t}
}
"""


def log(message):
    print(message, file=sys.stderr, flush=True)


def git(repo, *args):
    result = subprocess.run(["git", "-C", repo, *args], capture_output=True, text=True)
    if result.returncode != 0:
        raise SystemExit(f"git {' '.join(args)} failed: {result.stderr.strip()}")
    return result.stdout


def walk_enums(node, key, found):
    if isinstance(node, dict):
        for name, value in node.items():
            if name == key and isinstance(value, dict) and "enum" in value:
                found.append(len(value["enum"]))
            walk_enums(value, key, found)
    elif isinstance(node, list):
        for value in node:
            walk_enums(value, key, found)


def load_limits(team_dir):
    try:
        import yaml
    except ImportError:
        raise SystemExit("PyYAML is required to read team.yaml (pip install pyyaml)")
    with open(os.path.join(team_dir, "team.yaml"), encoding="utf-8") as handle:
        spec = yaml.safe_load(handle)["spec"]
    contracts = {contract["id"]: contract for contract in spec["contracts"]}
    key_limits = []
    schemas = sorted({c["result-contract"]["schema"] for c in spec["contracts"] if "result-contract" in c})
    for schema in schemas:
        with open(os.path.join(team_dir, schema), encoding="utf-8") as handle:
            walk_enums(json.load(handle), "workset_key", key_limits)
    if not key_limits:
        raise SystemExit("no workset_key enum found in the team's result schemas")
    return {
        "payload": json.loads(contracts["produce-workset"]["action"]["payload"]),
        "evidence_sources": contracts["synthesize-review"]["execution"]["max-evidence-sources"],
        "max_count": spec["inputs"]["review.scope"]["properties"]["count"]["maximum"],
        "workset_keys": min(key_limits),
    }


def build_producer(repo, team_dir, workdir):
    # reviewprep is a main package without func main: the Hufu runtime calls
    # run() directly. A copy with a small driver gives a standalone binary.
    source = os.path.join(workdir, "reviewprep")
    os.makedirs(source)
    producer_dir = os.path.join(team_dir, "reviewprep")
    for name in os.listdir(producer_dir):
        if name.endswith(".go") and not name.endswith("_test.go"):
            shutil.copy(os.path.join(producer_dir, name), source)
    with open(os.path.join(source, "zz_driver.go"), "w", encoding="utf-8") as handle:
        handle.write(DRIVER)
    go_version = next(
        line.split()[1]
        for line in open(os.path.join(repo, "go.mod"), encoding="utf-8")
        if line.startswith("go ")
    )
    with open(os.path.join(source, "go.mod"), "w", encoding="utf-8") as handle:
        handle.write(f"module reviewprepdriver\n\ngo {go_version}\n")
    binary = os.path.join(workdir, "reviewprep-driver")
    if not shutil.which("mise"):
        raise SystemExit("mise is required; install the repository's pinned Go toolchain with mise install")
    env = dict(os.environ, GOTOOLCHAIN="local", GOWORK="off", GOMAXPROCS="2", TMPDIR=workdir)
    result = subprocess.run(
        ["mise", "exec", "--", "go", "-C", source, "build", "-p", "2", "-o", binary, "."],
        cwd=repo, env=env, capture_output=True, text=True
    )
    if result.returncode != 0:
        raise SystemExit(f"building reviewprep failed:\n{result.stderr}")
    return binary


def evaluate(outputs, limits):
    """Return the problems a producer-accepted workset would hit later in the run."""
    routes = {name[: -len("_item_count")]: count for name, count in outputs.items() if name.endswith("_item_count")}
    problems = []
    for route, items in sorted(routes.items()):
        # Item keys restart at unit-0000 in each route, including a no-op item.
        if items > limits["workset_keys"]:
            problems.append(
                f"{route} route has {items} items; the review schemas accept only {limits['workset_keys']} workset keys"
            )
    reviews = sum(routes.values())
    # Each real (non-no-op) review may need one critic; the synthesizer reads all of them.
    sources = reviews + outputs["item_count"]
    if sources > limits["evidence_sources"]:
        problems.append(
            f"synthesis may need {sources} evidence sources ({reviews} reviews + up to {outputs['item_count']} critics); "
            f"the limit is {limits['evidence_sources']}"
        )
    if not outputs["scope"]["satisfied"]:
        problems.append("producer reported an unsatisfied scope")
    return routes, reviews, sources, problems


class Prober:
    def __init__(self, binary, repo, workdir, limits):
        self.binary, self.repo, self.workdir, self.limits = binary, repo, workdir, limits
        self.results = {}

    def __call__(self, count):
        if count not in self.results:
            self.results[count] = self.probe(count)
        return self.results[count]["ok"]

    def probe(self, count):
        root = os.path.join(self.workdir, f"workset-{count}")
        os.makedirs(root)
        payload = dict(
            self.limits["payload"],
            scope={"kind": "last_n", "count": count, "history": "first_parent", "head": "HEAD"},
            repository=self.repo,
            artifact_root=root,
            output_dir=os.path.join(root, "workset"),
        )
        request = json.dumps({"type": "prepare_review_workset", "payload": json.dumps(payload)})
        started = time.monotonic()
        result = subprocess.run([self.binary], input=request, capture_output=True, text=True)
        seconds = round(time.monotonic() - started, 1)
        shutil.rmtree(root, ignore_errors=True)
        if result.returncode != 0:
            message = " | ".join(line for line in result.stderr.strip().splitlines()[:4])
            if not any(code in message for code in REJECTIONS):
                raise SystemExit(f"reviewprep failed for count {count} with an unexpected error:\n{result.stderr}")
            log(f"  {count:>3}: rejected by producer ({seconds}s): {message}")
            return {"count": count, "ok": False, "seconds": seconds, "problems": [message]}
        outputs = json.loads(result.stdout)["outputs"]
        routes, reviews, sources, problems = evaluate(outputs, self.limits)
        budget = outputs["scope"]["observed_budget"]
        record = {
            "count": count,
            "ok": not problems,
            "seconds": seconds,
            "changed_paths": budget["changed_paths"],
            "diff_lines": budget["total_diff_lines"],
            "diff_bytes": budget["total_diff_bytes"],
            "route_items": routes,
            "reviews": reviews,
            "max_evidence_sources": sources,
            "problems": problems,
        }
        verdict = "ok" if not problems else "exceeds limits: " + "; ".join(problems)
        log(
            f"  {count:>3}: {budget['changed_paths']} paths, {budget['total_diff_lines']} lines, "
            f"items {routes}, {reviews} reviews ({seconds}s) -> {verdict}"
        )
        return record


def find_count(check, upper):
    """Largest passing count in 1..upper, assuming a larger count never shrinks the scope."""
    good, bad, count = 0, upper + 1, 1
    while bad == upper + 1:
        if check(count):
            good = count
            if count == upper:
                return good
            count = min(count * 2, upper)
        else:
            bad = count
    while bad - good > 1:
        middle = (good + bad) // 2
        if check(middle):
            good = middle
        else:
            bad = middle
    return good


def changed_dependency_files(repo, count):
    names = git(repo, "log", "--first-parent", f"-n{count}", "--name-only", "--format=", "HEAD").split()
    return sorted({name for name in names if os.path.basename(name) in DEPENDENCY_FILES})


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--count", type=int, help="requested commit count; lowered when it would hit a limit")
    parser.add_argument("--model", default=DEFAULT_MODEL, help=f"model for the hufu command (default {DEFAULT_MODEL})")
    parser.add_argument("--repo", default=".", help="repository root (default: the current Git checkout)")
    args = parser.parse_args()

    repo = git(args.repo, "rev-parse", "--show-toplevel").strip()
    team_dir = os.path.join(repo, ".agent-teams", TEAM)
    limits = load_limits(team_dir)
    available = int(git(repo, "rev-list", "--first-parent", "--count", "HEAD"))
    upper = min(limits["max_count"], available)
    notes = []
    if args.count is not None:
        if args.count < 1:
            raise SystemExit("--count must be at least 1")
        if args.count > upper:
            notes.append(f"requested {args.count} commits; the team and history allow at most {upper}")
        upper = min(args.count, upper)

    head = git(repo, "rev-parse", "--short", "HEAD").strip()
    log(f"HEAD {head}: probing hufu-code-review worksets for 1..{upper} commits")
    with tempfile.TemporaryDirectory(prefix="hufu-review-count-") as workdir:
        prober = Prober(build_producer(repo, team_dir, workdir), repo, workdir, limits)
        requested_fits = args.count is not None and prober(upper)
        count = upper if requested_fits else find_count(prober, upper)

    if count == 0:
        raise SystemExit("even the latest commit exceeds the hufu-code-review limits; review it with a narrower scope")
    if args.count is not None and count < args.count:
        notes.append(f"lowered the requested {args.count} commits to {count} to stay within the team limits")
    dependencies = changed_dependency_files(repo, count)
    if dependencies:
        notes.append(
            f"the range changes {', '.join(dependencies)}: the targeted-test step runs go test ./..., "
            "which takes longer (the verifier limit is 1200 s)"
        )
    prompt = PROMPT.format(count=count)
    command = [
        "mise", "exec", "--", "go", "run", "-p", "2", "./cmd/hufu", "--team", TEAM, "--model", args.model,
        "--auto-approve", "--report", "--new", prompt,
    ]
    summary = {
        "head": head,
        "count": count,
        "selected": prober.results[count],
        "limits": {key: value for key, value in limits.items() if key != "payload"},
        "probes": [prober.results[key] for key in sorted(prober.results)],
        "notes": notes,
        "prompt": prompt,
        "command": shlex.join(command),
    }
    json.dump(summary, sys.stdout, ensure_ascii=False, indent=2)
    print()


if __name__ == "__main__":
    main()
