#!/usr/bin/env python3
"""Validate synthetic contract examples; does not execute or validate Hufu runtime."""
from __future__ import annotations
import hashlib
import json
from pathlib import Path
import sys
try:
    from jsonschema import Draft202012Validator
except ImportError:
    Draft202012Validator = None
ROOT = Path(__file__).resolve().parent
if not __debug__:
    raise SystemExit("Run without -O: invariant checks must remain enabled.")

def load(p: Path):
    def pairs(ps):
        d = {}
        for k, v in ps:
            if k in d:
                raise ValueError(f"duplicate JSON key {k!r}")
            d[k] = v
        return d
    return json.loads(p.read_text(encoding="utf-8"), object_pairs_hook=pairs,
                      parse_constant=lambda x: (_ for _ in ()).throw(ValueError(f"non-finite {x}")))

def canonical(x):
    def check(v):
        if isinstance(v, float): raise ValueError("float in contract metadata")
        if isinstance(v, dict):
            for k, vv in v.items():
                if not isinstance(k, str): raise ValueError("non-string key")
                check(vv)
        elif isinstance(v, list):
            for vv in v: check(vv)
    check(x)
    return json.dumps(x, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8")

def H(d, v): return hashlib.sha256(d.encode()+b"\0"+canonical(v)).hexdigest()

def semantics(v):
    kind=v.get("kind")
    if kind == "primary_decision_evidence":
        c = v["coverage"]
        assert c["inventory_count"] == c["selected_count"]+c["excluded_count"], "coverage count mismatch"
        assert c["selected_count"] == len(v["items"]), "selected count mismatch"
        assert len({x["item_id"] for x in v["items"]}) == len(v["items"]), "duplicate item"
        for x in v["items"]:
            assert x["view_ref"]["sha256"] == hashlib.sha256(x["content"].encode()).hexdigest(), "view digest mismatch"
            assert x["extraction"]["view_bytes"] == len(x["content"].encode()), "view byte mismatch"
            assert not (x["independence"]["counts_toward_minimum"] and not x["independence"]["known_root"]), "unknown counted independent"
            if x["content_format"]=="json": json.loads(x["content"])
    elif kind == "decision_role_binding_plan":
        assert len({x["binding_id"] for x in v["bindings"]})==len(v["bindings"]), "duplicate binding"
        for x in v["bindings"]:
            if x["execution_mode"]=="runtime_reviewer":
                assert x["agent_id"] is None and x["agent_definition_digest"] is None, "fake fallback agent"
                assert x["tool_policy"]=="none" and not x["allowed_tool_ids"], "runtime fallback has tools"
            if x["tool_policy"]=="none": assert not x["allowed_tool_ids"], "none policy has tools"
            if x["role"]!="reference": assert x["tool_policy"]=="none", "reviewer has tools"
    elif kind in ("decision_run_result", "decision_run_preview"):
        if v["outcome"]=="completed":
            assert v["terminal_persisted"] and v["logical_disposition"]=="closed", "unconfirmed success"
            assert v["exit_code"]==0 and v["goal_satisfied"], "inconsistent success"
            assert v["primary"]["state"]=="bound", "success missing primary"
            for k in ("record_ref", "binding_event_id", "decision_id", "view"):
                assert v["primary"][k] is not None, f"missing proof: {k}"
            assert v["acceptance"]["combined"]=="passed" and v["acceptance"]["decision_process"]=="passed", "missing acceptance"
        if v["outcome"]=="preview":
            assert not v["terminal_persisted"] and v["logical_run_id"] is None and v["execution_run_id"] is None, "preview mutated run"
        if v["primary"]["view"]:
            rv=v["primary"]["view"]
            assert rv["selected_option_id"] in {o["id"] for o in rv["options"]}, "invalid selected option"
    elif kind == "runtime_task_occurrence":
        meta=v["runtime"]
        task_hash=H("hufu/primary-task/v1", {
            "branch_id":meta["branch_id"], "logical_run_id":meta["logical_run_id"]})
        decision_hash=H("hufu/primary-decision/v1", {
            "branch_id":meta["branch_id"], "generation":meta["generation"],
            "logical_run_id":meta["logical_run_id"]})
        assert v["task_id"] == "__hufu_pd_"+task_hash, "primary task ID mismatch"
        assert meta["decision_id"] == "pd_"+decision_hash, "primary decision ID mismatch"
    elif kind == "primary_decision_valid":
        task_hash=H("hufu/primary-task/v1", {
            "branch_id":v["branch_id"], "logical_run_id":v["logical_run_id"]})
        decision_hash=H("hufu/primary-decision/v1", {
            "branch_id":v["branch_id"], "generation":v["generation"],
            "logical_run_id":v["logical_run_id"]})
        assert v["primary_task_id"] == "__hufu_pd_"+task_hash, "manifest task ID mismatch"
        assert v["decision_id"] == "pd_"+decision_hash, "manifest decision ID mismatch"
    elif kind == "decision_run_terminal_extension" and v["disposition"] == "closed":
        assert v["generation"] > 0, "closed run has no generation"
        assert v["primary_binding_event_id"] is not None, "closed run has no binding"
        assert v["primary_record_ref"] is not None, "closed run has no record"

S={}
if Draft202012Validator is not None:
    for p in ROOT.glob("*.schema.json"):
        s=load(p); Draft202012Validator.check_schema(s); S[p.name]=Draft202012Validator(s)
valids={
 "evidence.valid.json":"primary-evidence.schema.json",
 "role-plan.valid.json":"role-binding-plan.schema.json",
 "role-constraints.valid.json":"role-constraints.schema.json",
 "event.opened.valid.json":"decision-events.schema.json",
 "logical-run.valid.json":"logical-run.schema.json",
 "result.completed.valid.json":"decision-output.schema.json",
 "result.blocked.valid.json":"decision-output.schema.json",
 "result.preview.valid.json":"decision-output.schema.json",
 "runtime.requirement.valid.json":"runtime-contracts.schema.json",
 "runtime.authority.valid.json":"runtime-contracts.schema.json",
 "runtime.occurrence.valid.json":"runtime-contracts.schema.json",
 "runtime.terminal-proof.valid.json":"runtime-contracts.schema.json",
 "runtime.manifest-proof.valid.json":"runtime-contracts.schema.json",
 "runtime.terminal-extension.valid.json":"runtime-contracts.schema.json"}
invalids={
 "evidence.unknown-field.invalid.json":"primary-evidence.schema.json",
 "evidence.count.invalid.json":"primary-evidence.schema.json",
 "result.unconfirmed.invalid.json":"decision-output.schema.json",
 "result.missing-proof.invalid.json":"decision-output.schema.json",
 "role-plan.tools.invalid.json":"role-binding-plan.schema.json",
 "event.unknown-version.invalid.json":"decision-events.schema.json",
 "bundle.mutated.invalid.json":"profile-bundles.schema.json",
 "runtime.terminal-extension.invalid.json":"runtime-contracts.schema.json"}
for fn,sn in valids.items():
    x=load(ROOT/"fixtures"/fn)
    if S: S[sn].validate(x)
    semantics(x)
semantic_invalids={"evidence.count.invalid.json", "result.unconfirmed.invalid.json",
                   "result.missing-proof.invalid.json", "role-plan.tools.invalid.json"}
rejected=0
for fn,sn in invalids.items():
    if not S and fn not in semantic_invalids:
        continue  # Covered by validate_contract_schemas.go.
    x=load(ROOT/"fixtures"/fn)
    try:
        if S: S[sn].validate(x)
        semantics(x)
    except Exception: pass
    else: raise AssertionError(f"invalid example was accepted: {fn}")
    rejected += 1

# The opened event must point to the exact strict requirement fixture rather
# than merely carrying a syntactically valid opaque reference.
opened=load(ROOT/"fixtures"/"event.opened.valid.json")["payload"]
requirement_path=ROOT/"fixtures"/"runtime.requirement.valid.json"
requirement=load(requirement_path)
requirement_bytes=requirement_path.read_bytes()
assert opened["requirement_ref"]["sha256"] == hashlib.sha256(requirement_bytes).hexdigest()
assert opened["requirement_ref"]["size_bytes"] == len(requirement_bytes)
assert opened["requirement_digest"] == H("hufu/decision-requirement/v1", requirement)
for field in ("logical_run_id", "branch_id", "team_id", "team_definition_digest", "effective_limits"):
    assert opened[field] == requirement[field], f"opened requirement mismatch: {field}"
assert opened["profile_bundle_ref"] == requirement["primary_profile_ref"]
authority_path=ROOT/"fixtures"/"runtime.authority.valid.json"
authority=load(authority_path)
authority_bytes=authority_path.read_bytes()
assert requirement["authority_snapshot_ref"]["sha256"] == hashlib.sha256(authority_bytes).hexdigest()
assert requirement["authority_snapshot_ref"]["size_bytes"] == len(authority_bytes)
assert opened["authority_snapshot_ref"] == requirement["authority_snapshot_ref"]
for field in ("logical_run_id", "branch_id", "team_id", "team_definition_digest"):
    assert authority[field] == requirement[field], f"authority requirement mismatch: {field}"
assert authority["execution_run_id"] == opened["execution_run_id"]
assert authority["policy_digest"] == H("hufu/decision-authority-policy/v1", {
    "security_flags":authority["security_flags"], "targets":authority["targets"]})
assert requirement["team_id"] == "team_"+H("hufu/team-identity/v1", {"name":"fixture-team"})
expected=load(ROOT/"bundle-digests.json")
for fn in ("light-v2.json","standard-v2.json","high-stakes-v2.json"):
    b=load(ROOT/fn)
    if S: S["profile-bundles.schema.json"].validate(b)
    assert H("hufu/decision-bundle/v2", b)==expected[b["ref"]], "bundle digest mismatch"
    p=b["policy"]; r=b["role-resolution"]; e=b["evidence"]; l=b["limits"]
    assert p["independent-judgments"]==r["roles"]["judge"]["count"]==p["min-independent-judgments"]
    assert p["challenge"]["count"]==r["roles"]["challenge"]["count"]
    assert p["max-tokens"]==l["total-decision-tokens"]
    assert p["discipline"]["evidence"]["required-independent-groups"]==e["min-known-independent-groups"]
vectors=load(ROOT/"canonical-test-vectors.json")
required_vector_names={"team_id", "authority_policy_digest", "requirement_digest", "primary_task_id", "decision_id", "lineage_digest",
 "source_index_digest", "support_revision_digest", "preparation_digest", "binding_id",
 "evidence_item_id", "independence_group_id"}
assert required_vector_names <= {v.get("name") for v in vectors}, "digest registry vectors are incomplete"
for vector in vectors:
    value=vector.get("input")
    if value is None:
        value=load(ROOT/vector["input_fixture"])
    if vector.get("canonical_utf8_hex") is not None:
        assert canonical(value).hex()==vector["canonical_utf8_hex"]
    assert H(vector["domain"], value)==vector["digest"], f"digest vector mismatch: {vector.get('name', vector['domain'])}"
artifacts=list((ROOT/"fixtures"/"artifacts").glob("*"))
for p in artifacts: assert hashlib.sha256(p.read_bytes()).hexdigest()==p.stem, "artifact content mismatch"
report={"schemas_valid":len(S),"schema_validation":"python" if S else "delegated_to_go_validator",
 "valid_examples_accepted":len(valids), "invalid_examples_rejected":rejected,
 "catalog_bundles_valid":3,
 "canonical_vectors_valid":len(vectors),"synthetic_artifact_hashes_valid":len(artifacts),
 "scope":"Static contract/schema examples only. Hufu Go tests, backend isolation, fsync and crash recovery were not executed."}
print(json.dumps(report,ensure_ascii=False,indent=2))
