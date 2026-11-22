#!/usr/bin/env python3
"""Continue Claude Code workflow agents that died mid-work, and carry their runs on.

  wf_revive.py list  [--session <id>] [--runs wf_a,wf_b] [--hours 6]
  wf_revive.py stage [--runs ...] [--ids a1,a2 | --all-dead] [--reports <dir>]
  wf_revive.py check --ids a1,a2
  wf_revive.py done  [--runs ...] [--reports <dir>]
  wf_revive.py rerun [--runs ...] [--reports <dir>] [--out <dir>] [--wait]

list   one row per agent that started and returned no result, with a state:
         dead           the run gave up on it (a `failed` row in the journal), or the run is over
                        (finished or stopped) and it never returned. Revive it.
         quiet          its run is going, it has no `failed` row, and it wrote nothing for two
                        minutes. It is inside a long tool call, or its whole session died.
         never-started  one turn at most. Nothing to continue: its stage starts fresh.
         revived        its transcript is staged; SendMessage has continued it, or can.
         live           wrote in the last two minutes. Leave it alone.
         retry          a weaker attempt at a stage another agent holds. Ignore it.
         superseded     another agent finished its stage. Ignore it.
stage  copies agent-<id>.jsonl and .meta.json from the run folder into <session>/subagents/, never
       over an existing copy. After that, SendMessage(to=<id>) continues the agent. Prints the file
       each agent's message should name for its report.
check  after the SendMessage calls: how many turns each revived copy has taken since.
done   prints {label: result} for the chosen runs: every finished stage's result from the journal,
       and each revived agent's report, or {"$wait": <file>} while that report is not written.
rerun  for each chosen run that is over and has unfinished stages: writes a copy of its script
       with that map in place of the line marked `// wf-revive:done`, and prints the Workflow
       input that starts it. Finished stages then return from the map and call no agent. It
       waits for every revived agent's report, unless --wait: then the new run waits instead.

The session id defaults to $CLAUDE_CODE_SESSION_ID. Reports default to <session>/subagents/revived/.
Read SKILL.md beside this file first.
"""
from __future__ import annotations

import argparse
import datetime
import glob
import json
import os
import re
import shutil
import sys
import time

TRY = re.compile(r":try\d+$")
ASSISTANT = '"type":"assistant"'     # the record's own type sits near the end of its line
STAMP = re.compile(r'"timestamp":"(\d{4}-\d\d-\d\dT[0-9:]+)')
MARK = "// wf-revive:done"
QUIET_S = 120
IGNORED = ("retry", "superseded")


def subagents_dir(session: str) -> str:
    found = glob.glob(os.path.expanduser(f"~/.claude/projects/*/{session}/subagents"))
    if len(found) != 1:
        sys.exit(f"wf_revive: {len(found)} folders match session {session}; give --session")
    return found[0]


def journal(run: str) -> list[dict]:
    rows = []
    with open(os.path.join(run, "journal.jsonl"), encoding="utf-8") as lines:
        for line in lines:
            try:
                rows.append(json.loads(line))
            except ValueError:
                continue
    return rows


def in_tool(log: str) -> str:
    """The tool an agent's last record calls, or "": tells a long tool call from a wait for the model."""
    with open(log, "rb") as handle:
        handle.seek(max(0, os.path.getsize(log) - 262144))
        tail = handle.read().decode("utf-8", "replace").strip().splitlines()
    try:
        content = json.loads(tail[-1]).get("message", {}).get("content", [])
    except (ValueError, IndexError, AttributeError):
        return ""
    names = [part.get("name", "") for part in content if isinstance(part, dict) and part.get("type") == "tool_use"]
    return names[-1] if names else ""


def record_of(run: str, base: str) -> str:
    """The session writes this file when a run ends, finished or stopped. A run with none is going."""
    return os.path.join(os.path.dirname(base), "workflows", f"{os.path.basename(run)}.json")


def unfinished(run: str, base: str) -> list[dict]:
    """The agents of one run folder that started and have no result row, each with its state."""
    label, done, failed, order = {}, set(), set(), []
    for row in journal(run):
        kind, agent = row.get("type"), row.get("agentId")
        if kind == "started":
            label[agent] = row.get("label", "")
            order.append(agent)
        elif kind == "result":
            done.add(agent)
        elif kind == "failed":
            failed.add(agent)
    finished = {TRY.sub("", label[a]) for a in done if a in label}
    over = os.path.exists(record_of(run, base))
    rows = []
    for agent in order:
        if agent in done:
            continue
        log = os.path.join(run, f"agent-{agent}.jsonl")
        row = {"run": os.path.basename(run), "label": label[agent], "stage": TRY.sub("", label[agent]),
               "id": agent, "log": log, "turns": 0, "minutes": 0, "last": "", "note": ""}
        rows.append(row)
        if not os.path.exists(log):
            row["state"] = "no-transcript"
            continue
        stamps = []
        with open(log, encoding="utf-8", errors="replace") as lines:
            for line in lines:
                row["turns"] += ASSISTANT in line
                match = STAMP.search(line)
                if match:
                    stamps.append(match.group(1))
        if stamps:
            first, last = (datetime.datetime.fromisoformat(s) for s in (min(stamps), max(stamps)))
            row["minutes"], row["last"] = int((last - first).total_seconds() // 60), max(stamps)[11:16]
        ended = over or agent in failed
        if os.path.exists(os.path.join(base, f"agent-{agent}.jsonl")):
            row["state"] = "revived"
        elif not over and time.time() - os.path.getmtime(log) < QUIET_S:
            row["state"] = "live"
        elif row["stage"] in finished:
            row["state"] = "superseded"
        elif ended:
            row["state"] = "never-started" if row["turns"] <= 1 else "dead"
        else:
            row["state"], tool = "quiet", in_tool(log)
            row["note"] = f"in {tool}" if tool else "waiting for the model"
    # Of several attempts at one stage, the one that got furthest is the one to continue.
    by_stage: dict[str, list[dict]] = {}
    for row in rows:
        if row["state"] not in ("superseded", "no-transcript"):
            by_stage.setdefault(row["stage"], []).append(row)
    for attempts in by_stage.values():
        rank = lambda r: (r["state"] in ("revived", "live"), r["turns"])
        keep = max(attempts, key=rank)
        for row in attempts:
            if row is not keep:
                row["state"] = "retry"
    return rows


def runs_of(base: str, wanted: str, hours: float) -> list[str]:
    folders = [f for f in glob.glob(os.path.join(base, "workflows", "wf_*"))
               if os.path.exists(os.path.join(f, "journal.jsonl"))]
    folders.sort(key=lambda f: os.path.getmtime(os.path.join(f, "journal.jsonl")))
    if wanted:
        names = set(wanted.split(","))
        return [f for f in folders if os.path.basename(f) in names]
    return [f for f in folders if time.time() - os.path.getmtime(os.path.join(f, "journal.jsonl")) < hours * 3600]


def turns_since_revival(log: str, original: str) -> int:
    """Assistant turns in the revived copy beyond those of the run folder's log."""
    count = lambda path: sum(1 for line in open(path, encoding="utf-8", errors="replace") if ASSISTANT in line)
    return count(log) - count(original)


def report_of(reports: str, agent: str) -> str:
    return os.path.join(reports, f"{agent}.json")


def done_map(run: str, base: str, reports: str) -> dict:
    """A run's stages that need no agent: label to result, or to the report a revived agent will write."""
    label, out = {}, {}
    for row in journal(run):
        if row.get("type") == "started":
            label[row.get("agentId")] = TRY.sub("", row.get("label", ""))
        elif row.get("type") == "result" and row.get("result") is not None and row.get("agentId") in label:
            out[label[row["agentId"]]] = row["result"]
    for row in unfinished(run, base):
        if row["state"] != "revived" or row["stage"] in out:
            continue
        report = report_of(reports, row["id"])
        try:
            out[row["stage"]] = json.load(open(report, encoding="utf-8"))
        except (OSError, ValueError):      # not written yet, or half written
            out[row["stage"]] = {"$wait": report}
    return out


def rerun(run: str, base: str, reports: str, out_dir: str, wait: bool) -> dict | None:
    """The Workflow input that carries one finished run on, or why there is none."""
    name = os.path.basename(run)
    record_path = record_of(run, base)
    open_rows = [r for r in unfinished(run, base) if r["state"] not in IGNORED]
    if not open_rows:
        return None
    if not os.path.exists(record_path):
        return {"run": name, "skipped": "the run is still going: leave it alone"}
    record = json.load(open(record_path, encoding="utf-8"))
    source_path = record.get("scriptPath", "")
    source = open(source_path, encoding="utf-8").read() if os.path.exists(source_path) else record.get("script", "")
    finished = done_map(run, base, reports)
    waiting = [label for label, value in finished.items() if isinstance(value, dict) and "$wait" in value]
    plan = {"run": name, "finished": len(finished) - len(waiting), "waiting": waiting,
            "starts fresh": [r["stage"] for r in open_rows if r["state"] != "revived"]}
    if waiting and not wait:
        return {**plan, "skipped": "the revived agents of these stages have not written their reports. "
                "Run rerun again when they have, or pass --wait to start now with one waiting agent a stage."}
    marked = [line for line in source.splitlines() if MARK in line]
    if len(marked) != 1:
        return {**plan, "skipped": f"the script has {len(marked)} lines marked `{MARK}` (it needs one): "
                "finished stages would run again. Add the step() wrapper from SKILL.md, or start the "
                "remaining stages by hand."}
    os.makedirs(out_dir, exist_ok=True)
    target = os.path.join(out_dir, f"{name}-rerun.js")
    line = f"const DONE = {json.dumps(finished, ensure_ascii=False)} {MARK} (from {name})"
    with open(target, "w", encoding="utf-8") as handle:
        handle.write(source.replace(marked[0], line))
    workflow = {"scriptPath": target}
    if record.get("args") is not None:
        workflow["args"] = record["args"]
    return {**plan, "workflow": workflow}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("verb", choices=["list", "stage", "check", "done", "rerun"])
    parser.add_argument("--session", default=os.environ.get("CLAUDE_CODE_SESSION_ID", ""))
    parser.add_argument("--runs", default="")
    parser.add_argument("--hours", type=float, default=6)
    parser.add_argument("--ids", default="")
    parser.add_argument("--all-dead", action="store_true")
    parser.add_argument("--reports", default="")
    parser.add_argument("--out", default="")
    parser.add_argument("--wait", action="store_true")
    args = parser.parse_args()
    if not args.session:
        sys.exit("wf_revive: no session id; give --session")
    base = subagents_dir(args.session)
    reports = args.reports or os.path.join(base, "revived")
    runs = runs_of(base, args.runs, args.hours)
    ids = set(filter(None, args.ids.split(",")))

    if args.verb == "done":
        merged: dict = {}
        for run in runs:
            merged.update(done_map(run, base, reports))
        print(json.dumps(merged, ensure_ascii=False))
        return 0

    if args.verb == "rerun":
        for run in runs:
            plan = rerun(run, base, reports, args.out or reports, args.wait)
            if plan:
                print(json.dumps(plan, ensure_ascii=False))
        return 0

    rows = [row for run in runs for row in unfinished(run, base)]

    if args.verb == "list":
        for row in rows:
            if row["state"] in IGNORED:
                continue
            print(f"{row['run'][:14]}  {row['label']:26} {row['id']}  {row['state']:13} "
                  f"{row['minutes']:4} min {row['turns']:4} turns  last {row['last']}  {row['note']}".rstrip())
        states = [row["state"] for row in rows]
        print(json.dumps({state: states.count(state) for state in sorted(set(states))}))
        return 0

    if args.verb == "stage":
        chosen = [row for row in rows if (row["id"] in ids) or (args.all_dead and row["state"] == "dead")]
        refused = [row for row in chosen if row["state"] in ("live", "no-transcript")]
        for row in refused:
            print(f"NOT STAGED {row['label']} {row['id']}: it is {row['state']}")
        os.makedirs(reports, exist_ok=True)
        for row in chosen:
            if row in refused:
                continue
            for suffix in (".jsonl", ".meta.json"):
                source = row["log"][:-len(".jsonl")] + suffix
                target = os.path.join(base, os.path.basename(source))
                if os.path.exists(source) and not os.path.exists(target):
                    shutil.copy2(source, target)
            print(f"staged  {row['label']:26} {row['id']}  ({row['minutes']} min, {row['turns']} turns)  "
                  f"report: {report_of(reports, row['id'])}")
        print(f"{len(chosen) - len(refused)} staged in {base}. Now: SendMessage(to=<id>) for each, "
              "naming its report file.")
        return 0

    for agent in sorted(ids):
        log = os.path.join(base, f"agent-{agent}.jsonl")
        original = glob.glob(os.path.join(base, "workflows", "*", f"agent-{agent}.jsonl"))
        if not os.path.exists(log) or not original:
            print(f"{agent}  not staged")
            continue
        meta_path = log[:-len(".jsonl")] + ".meta.json"
        meta = json.load(open(meta_path)) if os.path.exists(meta_path) else {}
        written = "report written" if os.path.exists(report_of(reports, agent)) else "no report yet"
        print(f"{meta.get('description', ''):26} {agent}  {turns_since_revival(log, original[0]):4} turns since revival  {written}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
