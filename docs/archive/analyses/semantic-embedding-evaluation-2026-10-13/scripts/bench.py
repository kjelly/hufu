"""CPU benchmark of Ollama embedding models on hufu history data.

Usage: python3 -I bench.py <dataset.json> <out-dir> [model ...]
Writes <out-dir>/<model>.json and progress to stdout. Never prints content.
"""
import concurrent.futures
import json
import math
import os
import pwd
import random
import re
import statistics
import sys
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path

HOST = "http://127.0.0.1:11434"
QWEN_INSTRUCT = ("Instruct: Retrieve relevant verified technical decisions, failure analyses, "
                 "operational constraints and reusable lessons from an AI agent's memory.\nQuery:")
PROFILES = {
    "qwen3-embedding:0.6b": {"num_ctx": 4096, "query_prefix": QWEN_INSTRUCT, "doc_prefix": "",
                             "length_targets": [128, 512, 1024, 2048, 4000]},
    "embeddinggemma:latest": {"num_ctx": 2048, "query_prefix": "task: search result | query: ",
                              "doc_prefix": "title: none | text: ", "length_targets": [128, 512, 1024, 1900]},
}
OWN_MODELS = set(PROFILES) | {"nomic-embed-text:latest"}
CLK_TCK = os.sysconf("SC_CLK_TCK")
OLLAMA_UID = pwd.getpwnam("ollama").pw_uid
SEED = 20261013
QUICK = os.environ.get("BENCH_QUICK") == "1"
COLD_REPS, DIRECT_REPS, WARM_N, CONC_N, SWEEP_N = (1, 1, 5, 4, 16) if QUICK else (10, 5, 300, 48, 128)


def log(msg):
    print(time.strftime("%H:%M:%S"), msg, flush=True)


def post(path, body, timeout=900):
    req = urllib.request.Request(HOST + path, data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"})
    start = time.monotonic()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return json.load(resp), None, time.monotonic() - start
    except urllib.error.HTTPError as err:
        return None, f"HTTP {err.code}: {err.read().decode()[:120]}", time.monotonic() - start
    except Exception as err:  # noqa: BLE001 - benchmark records every failure
        return None, f"{type(err).__name__}: {err}", time.monotonic() - start


def embed(model, inputs, keep_alive="30m"):
    return post("/api/embed", {"model": model, "input": inputs, "truncate": False, "keep_alive": keep_alive,
                               "options": {"num_ctx": PROFILES[model]["num_ctx"]}})


def loaded():
    with urllib.request.urlopen(HOST + "/api/ps", timeout=30) as resp:
        return {m["name"]: m for m in json.load(resp)["models"]}


def unload(model):
    post("/api/embed", {"model": model, "input": [], "keep_alive": 0})
    deadline = time.monotonic() + 60
    while model in loaded():
        if time.monotonic() > deadline:
            raise RuntimeError(f"{model} did not unload")
        time.sleep(0.3)


def ollama_pids():
    pids = []
    for entry in os.listdir("/proc"):
        if not entry.isdigit():
            continue
        try:
            st = os.stat(f"/proc/{entry}")
        except OSError:
            continue
        if st.st_uid == OLLAMA_UID:
            pids.append(int(entry))
    return pids


def proc_usage():
    rss, ticks = 0, 0
    for pid in ollama_pids():
        try:
            for line in open(f"/proc/{pid}/status"):
                if line.startswith("VmRSS:"):
                    rss += int(line.split()[1]) * 1024
            fields = open(f"/proc/{pid}/stat").read().rsplit(")", 1)[1].split()
            ticks += int(fields[11]) + int(fields[12])
        except OSError:
            pass
    return rss, ticks


class Sampler:
    """Samples Ollama-owned RSS and CPU while a phase runs."""

    def __init__(self):
        self.peak_rss, self.stop = 0, threading.Event()

    def __enter__(self):
        self.start_wall, (_, self.start_ticks) = time.monotonic(), proc_usage()
        self.thread = threading.Thread(target=self._run, daemon=True)
        self.thread.start()
        return self

    def _run(self):
        while not self.stop.is_set():
            self.peak_rss = max(self.peak_rss, proc_usage()[0])
            self.stop.wait(0.5)

    def __exit__(self, *exc):
        self.stop.set()
        self.thread.join()
        wall = time.monotonic() - self.start_wall
        _, ticks = proc_usage()
        self.result = {"wall_s": round(wall, 3), "peak_rss_mb": round(self.peak_rss / 2**20),
                       "avg_cpu_cores": round((ticks - self.start_ticks) / CLK_TCK / wall, 2) if wall else None}


def pct(values, p):
    if not values:
        return None
    ordered = sorted(values)
    k = (len(ordered) - 1) * p
    lo, hi = math.floor(k), math.ceil(k)
    return round(ordered[lo] + (ordered[hi] - ordered[lo]) * (k - lo), 4)


def dist(values):
    return {"n": len(values), "p50": pct(values, .5), "p90": pct(values, .9), "p95": pct(values, .95),
            "p99": pct(values, .99), "max": round(max(values), 4) if values else None,
            "mean": round(statistics.fmean(values), 4) if values else None}


def query_text(model, q, goal_only=False):
    text = q["goal"] if goal_only else "\n".join(
        [q["goal"], "phase:execute", "trigger:task_dispatch"] + ([f"role:{q['agent']}"] if q["agent"] else []))
    return PROFILES[model]["query_prefix"] + text


def cos_rank(qv, doc_vectors, relevant_idx):
    scores = [sum(a * b for a, b in zip(qv, dv)) for dv in doc_vectors]
    order = sorted(range(len(scores)), key=lambda i: -scores[i])
    for rank, idx in enumerate(order, 1):
        if idx in relevant_idx:
            return rank
    return None


def retrieval_metrics(ranks):
    n = len(ranks)
    return {"n": n, **{f"recall@{k}": round(sum(r is not None and r <= k for r in ranks) / n, 3) for k in (1, 5, 10)},
            "mrr@10": round(sum(1 / r for r in ranks if r and r <= 10) / n, 3),
            "ranks": ranks}


def bench_model(model, dataset, out_dir):
    prof = PROFILES[model]
    rng = random.Random(SEED)
    docs, queries = dataset["docs"], dataset["queries"]
    result = {"model": model, "num_ctx": prof["num_ctx"], "query_prefix_chars": len(prof["query_prefix"]),
              "doc_prefix_chars": len(prof["doc_prefix"]), "loadavg_start": os.getloadavg()}
    for other in OWN_MODELS & set(loaded()):
        unload(other)
    result["other_models_loaded"] = sorted(set(loaded()))

    # 1. Cold start: preload (input []) then first query; and direct cold query.
    probe = query_text(model, queries[0])
    cold = []
    for i in range(COLD_REPS):
        unload(model)
        pre, err, pre_wall = embed(model, [])
        first, err2, first_wall = embed(model, probe)
        cold.append({"preload_s": round(pre_wall, 3), "first_query_after_preload_s": round(first_wall, 3),
                     "error": err or err2})
        log(f"{model} cold {i + 1}/{COLD_REPS} preload={pre_wall:.2f}s first={first_wall:.2f}s")
    direct = []
    for i in range(DIRECT_REPS):
        unload(model)
        out, err, wall = embed(model, probe)
        direct.append({"wall_s": round(wall, 3), "load_ms": round((out or {}).get("load_duration", 0) / 1e6),
                       "error": err})
    ps = loaded().get(model, {})
    result["cold"] = {"preload_s": dist([c["preload_s"] for c in cold]),
                      "first_query_after_preload_s": dist([c["first_query_after_preload_s"] for c in cold]),
                      "direct_cold_query_s": dist([d["wall_s"] for d in direct]),
                      "errors": [c["error"] for c in cold + direct if c.get("error")],
                      "ps_size_mb": round(ps.get("size", 0) / 2**20), "ps_context_length": ps.get("context_length")}
    log(f"{model} cold done: {result['cold']['preload_s']['p50']}s preload p50")

    # 2. Warm sequential queries in hufu RetrievalQuery format.
    sample = rng.sample(queries, min(WARM_N, len(queries)))
    embed(model, probe)
    walls, server, tokens, errors = [], [], [], []
    with Sampler() as s:
        for i, q in enumerate(sample):
            out, err, wall = embed(model, query_text(model, q))
            if err:
                errors.append({"chars": len(query_text(model, q)), "error": err})
                continue
            walls.append(wall)
            server.append(out["total_duration"] / 1e9)
            tokens.append(out.get("prompt_eval_count", 0))
            if (i + 1) % 50 == 0:
                log(f"{model} warm {i + 1}/{len(sample)}")
    over_3s = sum(w > 3 for w in walls)
    result["warm_query"] = {"wall_s": dist(walls), "server_s": dist(server), "prompt_tokens": dist(tokens),
                            "over_3s": over_3s, "errors": errors, "resource": s.result,
                            "by_tokens": {}}
    buckets = [(0, 128), (128, 256), (256, 512), (512, 1024), (1024, 99999)]
    for lo, hi in buckets:
        sel = [w for w, t in zip(walls, tokens) if lo <= t < hi]
        if sel:
            result["warm_query"]["by_tokens"][f"{lo}-{hi}"] = dist(sel)
    log(f"{model} warm done p50={pct(walls, .5)} p95={pct(walls, .95)} over3s={over_3s}")

    # 3. Concurrency.
    conc_queries = [query_text(model, q) for q in sample[:CONC_N]]
    result["concurrency"] = {}
    for workers in (1, 2, 4, 8):
        lat = []
        start = time.monotonic()
        with concurrent.futures.ThreadPoolExecutor(workers) as pool:
            for out, err, wall in pool.map(lambda t: embed(model, t), conc_queries):
                if not err:
                    lat.append(wall)
        total = time.monotonic() - start
        result["concurrency"][str(workers)] = {"throughput_qps": round(len(lat) / total, 2),
                                               "latency_s": dist(lat), "ok": len(lat)}
        log(f"{model} concurrency {workers}: {len(lat) / total:.2f} q/s p95={pct(lat, .95)}")

    # 4. Input length scaling with real text.
    corpus_text = "\n".join(d["text"] for d in rng.sample(docs, len(docs)))
    result["length_scaling"] = []
    for target in (prof["length_targets"][:2] if QUICK else prof["length_targets"]):
        reps = 3 if target <= 1024 else 1
        chars = int(target * 1.3)
        for attempt in range(6):
            text = corpus_text[:chars]
            out, err, wall = embed(model, prof["doc_prefix"] + text)
            if err and "exceeds the context length" in err:
                chars = int(chars * 0.85)
                continue
            tok = (out or {}).get("prompt_eval_count") or 0
            if err or not tok or abs(tok - target) <= target * 0.08:
                break
            chars = max(16, int(chars * target / tok))
        runs = [wall] if not err else []
        tok = (out or {}).get("prompt_eval_count")
        for _ in range(reps - 1):
            out2, err2, wall2 = embed(model, prof["doc_prefix"] + text)
            if not err2:
                runs.append(wall2)
        result["length_scaling"].append({"target_tokens": target, "prompt_tokens": tok, "chars": len(text),
                                         "wall_s": [round(r, 3) for r in runs], "error": err})
        log(f"{model} length {target}: tokens={tok} wall={runs}")

    # 5. Full-corpus indexing, batch 8 (spec default); keep vectors for quality.
    texts = [prof["doc_prefix"] + d["text"] for d in docs]
    vectors, batch_walls, doc_tokens, index_errors = [], [], 0, []
    with Sampler() as s:
        for i in range(0, len(texts), 8):
            out, err, wall = embed(model, texts[i:i + 8])
            if err:
                index_errors.append({"batch": i // 8, "error": err})
                vectors.extend([None] * len(texts[i:i + 8]))
                continue
            vectors.extend(out["embeddings"])
            batch_walls.append(wall)
            doc_tokens += out.get("prompt_eval_count", 0)
            if (i // 8 + 1) % 25 == 0:
                log(f"{model} index {i + 8}/{len(texts)}")
    result["index_full"] = {"docs": len(texts), "batch_size": 8, "resource": s.result, "prompt_tokens": doc_tokens,
                            "docs_per_s": round(len(texts) / s.result["wall_s"], 2),
                            "tokens_per_s": round(doc_tokens / s.result["wall_s"], 1),
                            "batch_wall_s": dist(batch_walls), "errors": index_errors,
                            "persistent_retrievable_docs": sum(d["persistent"] for d in docs)}
    log(f"{model} index done {s.result}")

    # 6. Batch-size sweep on a fixed subset.
    subset = rng.sample(texts, SWEEP_N)
    result["batch_sweep"] = {}
    for size in (1, 4, 8, 16, 32):
        start = time.monotonic()
        ok = 0
        for i in range(0, len(subset), size):
            out, err, _ = embed(model, subset[i:i + size])
            ok += 0 if err else len(subset[i:i + size])
        wall = time.monotonic() - start
        result["batch_sweep"][str(size)] = {"docs_per_s": round(ok / wall, 2), "wall_s": round(wall, 2), "ok": ok}
        log(f"{model} batch {size}: {ok / wall:.2f} docs/s")

    # 7. Retrieval proxy: known-item queries (task Desc -> its task_result_reducer item).
    index_of = {d["hash"]: i for i, d in enumerate(docs)}
    valid = [i for i, v in enumerate(vectors) if v is not None]
    doc_vectors = [vectors[i] for i in valid]
    pos = {orig: new for new, orig in enumerate(valid)}
    labelled = [q for q in queries if q["relevant"]]
    result["retrieval_proxy"] = {}
    for variant, goal_only in (("hufu_query", False), ("goal_only", True)):
        ranks = []
        for q in labelled:
            out, err, _ = embed(model, query_text(model, q, goal_only))
            relevant = {pos[index_of[h]] for h in q["relevant"] if index_of.get(h) in pos}
            ranks.append(None if err or not relevant else cos_rank(out["embeddings"][0], doc_vectors, relevant))
        result["retrieval_proxy"][variant] = retrieval_metrics(ranks)
    log(f"{model} retrieval proxy {result['retrieval_proxy']['hufu_query']}")

    result["loadavg_end"] = os.getloadavg()
    path = out_dir / f"{re.sub(r'[^A-Za-z0-9_.-]', '_', model)}.json"
    path.write_text(json.dumps(result, indent=1))
    log(f"wrote {path}")


def lexical_baseline(dataset, out_dir):
    """BM25 over ASCII terms only, approximating hufu ftsQuery (OR of ASCII terms)."""
    token = re.compile(r"[A-Za-z0-9_]+")
    docs = [[t.lower() for t in token.findall(d["text"])] for d in dataset["docs"]]
    n, avg = len(docs), statistics.fmean(len(d) for d in docs)
    df = {}
    for d in docs:
        for t in set(d):
            df[t] = df.get(t, 0) + 1
    index_of = {d["hash"]: i for i, d in enumerate(dataset["docs"])}
    ranks = []
    for q in (q for q in dataset["queries"] if q["relevant"]):
        terms = set(t.lower() for t in token.findall(q["goal"]))
        scores = []
        for d in docs:
            tf = {}
            for t in d:
                if t in terms:
                    tf[t] = tf.get(t, 0) + 1
            s = sum(math.log(1 + (n - df[t] + .5) / (df[t] + .5)) * f * 2.2 / (f + 1.2 * (.25 + .75 * len(d) / avg))
                    for t, f in tf.items())
            scores.append(s)
        relevant = {index_of[h] for h in q["relevant"]}
        order = [i for i in sorted(range(n), key=lambda i: -scores[i]) if scores[i] > 0]
        rank = next((r for r, i in enumerate(order, 1) if i in relevant), None)
        ranks.append(rank)
    (out_dir / "lexical_baseline.json").write_text(json.dumps(retrieval_metrics(ranks), indent=1))
    log(f"lexical baseline {retrieval_metrics(ranks)}")


def main():
    dataset = json.loads(Path(sys.argv[1]).read_text())
    if QUICK:
        keep = {h for q in dataset["queries"] for h in q["relevant"]}
        dataset["docs"] = [d for d in dataset["docs"] if d["hash"] in keep] + dataset["docs"][:40]
        dataset["queries"] = [q for q in dataset["queries"] if q["relevant"]][:3] + dataset["queries"][:10]
    out_dir = Path(sys.argv[2])
    out_dir.mkdir(parents=True, exist_ok=True)
    with urllib.request.urlopen(HOST + "/api/version", timeout=10) as resp:
        meta = {"ollama": json.load(resp)["version"], "nproc": os.cpu_count(), "loadavg": os.getloadavg(),
                "docs": len(dataset["docs"]), "queries": len(dataset["queries"]), "seed": SEED,
                "started": time.strftime("%Y-%m-%dT%H:%M:%S%z")}
    (out_dir / "meta.json").write_text(json.dumps(meta, indent=1))
    lexical_baseline(dataset, out_dir)
    for model in sys.argv[3:] or list(PROFILES):
        bench_model(model, dataset, out_dir)
    log("all done")


if __name__ == "__main__":
    main()
