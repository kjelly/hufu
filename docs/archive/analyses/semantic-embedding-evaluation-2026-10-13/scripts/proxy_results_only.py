"""Known-item retrieval restricted to task_result_reducer documents.

Corpus: every task result item. Query: a task description. Relevant: that
task's own result items. Removes STM echoes that quote the task goal.
Prints metrics only.
"""
import json
import math
import re
import statistics
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import bench  # noqa: E402

dataset = json.loads(Path(sys.argv[1]).read_text())
docs = [d for d in dataset["docs"] if d["source_type"] == "task_result_reducer"]
index_of = {d["hash"]: i for i, d in enumerate(docs)}
labelled = [q for q in dataset["queries"] if q["relevant"]]
cjk = re.compile(r"[㐀-鿿]")
out = {"corpus_docs": len(docs), "queries": len(labelled),
       "cjk_queries": sum(bool(cjk.search(q["goal"])) for q in labelled)}


def split_metrics(ranks):
    all_m = bench.retrieval_metrics(ranks)
    zh = [r for r, q in zip(ranks, labelled) if cjk.search(q["goal"])]
    en = [r for r, q in zip(ranks, labelled) if not cjk.search(q["goal"])]
    return {"all": all_m, "zh": bench.retrieval_metrics(zh), "en": bench.retrieval_metrics(en)}


# Lexical: BM25 over ASCII terms (approximates hufu ftsQuery OR semantics).
token = re.compile(r"[A-Za-z0-9_]+")
toks = [[t.lower() for t in token.findall(d["text"])] for d in docs]
n, avg = len(toks), statistics.fmean(len(t) for t in toks)
df = {}
for t in toks:
    for w in set(t):
        df[w] = df.get(w, 0) + 1
ranks = []
for q in labelled:
    terms = {t.lower() for t in token.findall(q["goal"])}
    scores = []
    for t in toks:
        tf = {}
        for w in t:
            if w in terms:
                tf[w] = tf.get(w, 0) + 1
        scores.append(sum(math.log(1 + (n - df[w] + .5) / (df[w] + .5)) * f * 2.2 / (f + 1.2 * (.25 + .75 * len(t) / avg))
                          for w, f in tf.items()))
    order = [i for i in sorted(range(n), key=lambda i: -scores[i]) if scores[i] > 0]
    relevant = {index_of[h] for h in q["relevant"]}
    ranks.append(next((r for r, i in enumerate(order, 1) if i in relevant), None))
out["lexical_ascii_bm25"] = split_metrics(ranks)

for model in bench.PROFILES:
    prefix = bench.PROFILES[model]["doc_prefix"]
    vectors = []
    for i in range(0, len(docs), 8):
        res, err, _ = bench.embed(model, [prefix + d["text"] for d in docs[i:i + 8]])
        if err:
            raise SystemExit(f"{model}: {err}")
        vectors.extend(res["embeddings"])
    out[model] = {}
    for variant, goal_only in (("hufu_query", False), ("goal_only", True)):
        ranks = []
        for q in labelled:
            res, err, _ = bench.embed(model, bench.query_text(model, q, goal_only))
            relevant = {index_of[h] for h in q["relevant"]}
            ranks.append(None if err else bench.cos_rank(res["embeddings"][0], vectors, relevant))
        out[model][variant] = split_metrics(ranks)
    bench.log(f"{model} done")

Path(sys.argv[2]).write_text(json.dumps(out, indent=1))
for key in ("lexical_ascii_bm25", *bench.PROFILES):
    block = out[key] if key == "lexical_ascii_bm25" else out[key]["hufu_query"]
    print(key, {k: {m: v for m, v in block[k].items() if m != "ranks"} for k in ("all", "zh", "en")})
