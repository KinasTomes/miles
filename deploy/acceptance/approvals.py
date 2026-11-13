"""Hash-bound operator approvals shared by standalone commands and optional recipes."""

import datetime as dt
from pathlib import Path

from deploy.acceptance.common import ROOT, digest, now, require

COST_REVIEW = {"approval-review.md", "cost-review.json"}


def required_review(command):
    action = command[0]
    if action == "apply":
        return COST_REVIEW | {command[1] + "-plan.json"}
    if action == "iam-activate":
        return (
            COST_REVIEW
            | {"iam-review.json"}
            | {
                "iam/" + p.name.removesuffix(".tmpl")
                for p in (ROOT / "deploy/acceptance/policies").glob("*.tmpl")
            }
        )
    if action in {"cleanup-grant", "unprotect"}:
        return {"cleanup-review.json", "iam/cleanup.json", "frozen/manifest.json"}
    if action == "cleanup":
        return {
            "destroy-reviewed.json",
            "destroy.tfplan",
            "destroy-plan.json",
            "frozen/manifest.json",
        }
    if action in {"finish", "logout"}:
        return {"frozen/manifest.json", "destroy-reviewed.json"}
    return COST_REVIEW


def files(r, action):
    command = action.split(":")
    names = set(required_review(command))
    if command[0] in {"benchmark-case", "direct-provision"}:
        names |= {"direct-plan.json"}
    if command[0] == "direct-cleanup":
        names = {"direct-cleanup-plan.json"}
    if command[0] == "cold-step":
        names |= {"cold-plan.json"}
    for name in list(names):
        if name.endswith("-plan.json") and name.split("-")[0] in {"foundation", "hosts", "active"}:
            plan = r.read(name)
            names.update((plan["plan"], plan["vars"], str(Path(plan["plan"]).with_suffix(".json"))))
    return sorted(names)


def binding(r, action):
    hashes = {}
    for name in files(r, action):
        path = r.file(name)
        require(path.is_file() and not path.is_symlink()
                and path.resolve().is_relative_to(r.path), "Missing/private review artifact: " + name)
        hashes[name] = digest(path)
    return {"run_id": r.id, "source": r.meta["source"],
            "config_sha256": digest(r.file("config.json")), "action": action, "files": hashes}


def record(r, action):
    """Only an explicit approve command or an approved process checkpoint calls this."""
    receipt = {"at": now().isoformat(), "review": binding(r, action)}
    r.write("approvals/" + action.replace(":", "-") + ".json", receipt)
    return receipt


def check(r, action):
    receipt = r.read("approvals/" + action.replace(":", "-") + ".json")
    require(receipt["review"] == binding(r, action), "Review inputs changed; obtain a fresh operator approval")
    require(dt.timedelta(0) <= now() - dt.datetime.fromisoformat(receipt["at"])
            < dt.timedelta(minutes=45), "Approval expired; review current evidence again")
