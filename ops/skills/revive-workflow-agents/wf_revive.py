#!/usr/bin/env python3
"""Continue Claude Code workflow agents that died mid-work, and carry their runs on.

  wf_revive.py list  [--session <id>] [--runs wf_a,wf_b] [--hours 6] [--restarted <time>]
  wf_revive.py plan  [--runs ...] [--reports <dir>]
  wf_revive.py stage [--runs ...] [--ids a1,a2 | --all-dead] [--reports <dir>]
  wf_revive.py check --ids a1,a2
  wf_revive.py done  [--runs ...] [--reports <dir>]
  wf_revive.py rerun [--runs ...] [--reports <dir>] [--out <dir>] [--wait]

list   one row per agent that started and returned no result, with a state:
         dead           the run gave up on it (a `failed` row in the journal), or the run is over
                        (finished, stopped, or dead with its session) and it never returned. Revive it.
         quiet          its run is going, it has no `failed` row, and it wrote nothing for two
                        minutes. It is inside a long tool call, or waits for the model.
         never-started  one turn at most. Nothing to continue: its stage starts fresh.
         revived        its transcript is staged; SendMessage has continued it, or can.
         live           wrote in the last two minutes. Leave it alone.
         retry          a weaker attempt at a stage another agent holds. Ignore it.
         superseded     another agent finished its stage. Ignore it.
plan   one JSON line per stage of each chosen run: what reviving it would do. It changes nothing.
         reuse          the stage finished, or its revived agent wrote a report that fits: rerun reuses it.
         revive         its agent died after real work: stage it and SendMessage it.
         start fresh    its agent never got past its first turn, or left no transcript: rerun starts it.
         wait           its revived agent has not written a report that fits.
         leave          its run is going: the run's script still owns the stage.
         ignore         probes of the API (`probe:try<N>`), not work.
stage  copies agent-<id>.jsonl and .meta.json from the run folder into <session>/subagents/, never
       over an existing copy. After that, SendMessage(to=<id>) continues the agent. Prints the file
       each agent's message should name for its report, and writes beside it the JSON Schema that
       report must fit: the one the agent's own StructuredOutput tool had.
check  after the SendMessage calls: how many turns each revived copy has taken since.
done   prints {label: result} for the chosen runs: every finished stage's result from the journal,
       and each revived agent's report, or {"$wait": <file>} while that report is not written.
rerun  for each chosen run that is over and has unfinished stages: writes a copy of its script
       with that map in place of the line marked `// wf-revive:done`, and prints the Workflow
       input that starts it. Finished stages then return from the map and call no agent. It
       waits for every revived agent's report, unless --wait: then the new run waits instead.

A stage is an agent's label without the endings that mark another attempt at it: `:wait`, `:try<N>`,
`-try<N>`, `-attempt<N>`, `-retry<N>` (or with a colon), ` (retry <N>)`, and `#<N>` when an agent under
that label with no number started before it and every agent before it under that label had failed before
it started. Agents numbered `#<N>` with no unnumbered first, like a review panel, or that ran side by
side, or after one of them returned, are stages of their own.

A run is over when the session wrote its record, or when it has none and nothing in its folder was written
since the session's Claude Code process started: a run dies with the process that started it, and a session
that dies writes no record. The tool reads that start from the process it runs under when --session is its own
session. Give it with --restarted <time>, as 2026-11-22T16:44:14Z, when the tool runs anywhere else. For such
a run, rerun takes the script and the arguments from the Workflow call in the session's transcript.

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
import subprocess
import sys
import time

RETRY = re.compile(r"(?::wait|[:-](?:try|attempt|retry)\d+| \(retry \d+\))+$")   # another attempt at a stage, or its waiting agent
COUNTER = re.compile(r"#\d+$")      # numbers retries in some scripts and side-by-side agents in others
PROBE = "probe"                     # the standard prefix's probes of the API, `probe:try<N>`
ASSISTANT = '"type":"assistant"'     # the record's own type sits near the end of its line
STAMP = re.compile(r'"timestamp":"(\d{4}-\d\d-\d\dT[0-9:]+)')
MARK = "// wf-revive:done"
QUIET_S = 120
IGNORED = ("retry", "superseded")
RESTARTED: float | None = None      # when this session's Claude Code process started; main() sets it


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


def stages(rows: list[dict]) -> dict[str, str]:
    """Each started agent's stage, from the journal's row order.

    `#<N>` marks a retry only when an agent under the same label with no number started before it, and
    every agent before it under that label had failed before it started: `fix:x`, then `fix:x#2`. A panel
    numbers every member, so it stays apart even when a concurrency cap starts its members one at a time
    after the earlier ones died. In the journals read on 2026-11-21, every `#<N>` retry followed an
    unnumbered first attempt, and most `#<N>` labels numbered agents that ran side by side."""
    named, base, running, returned, bare, separate = {}, {}, {}, set(), set(), set()
    for row in rows:
        kind, agent = row.get("type"), row.get("agentId")
        if kind == "started":
            named[agent] = RETRY.sub("", row.get("label", ""))
            base[agent] = COUNTER.sub("", named[agent])
            numbered = named[agent] != base[agent]
            others = running.setdefault(base[agent], set())
            if others - {agent} or base[agent] in returned or (numbered and base[agent] not in bare):
                separate.add(base[agent])
            if not numbered:
                bare.add(base[agent])
            others.add(agent)
        elif kind in ("result", "failed") and agent in base:
            running[base[agent]].discard(agent)
            if kind == "result":
                returned.add(base[agent])
    return {agent: named[agent] if base[agent] in separate else base[agent] for agent in named}


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
    """The session writes this file when a run ends, finished or stopped."""
    return os.path.join(os.path.dirname(base), "workflows", f"{os.path.basename(run)}.json")


def process_started() -> float | None:
    """When the Claude Code process this tool runs under started, or None when it runs under none."""
    pid = os.getppid()
    for _ in range(16):
        found = subprocess.run(["ps", "-o", "ppid=,lstart=,comm=", "-p", str(pid)], capture_output=True, text=True,
                               env={**os.environ, "LC_ALL": "C"}).stdout.split()
        if len(found) < 7:
            return None
        if os.path.basename(" ".join(found[6:])) == "claude":
            return time.mktime(time.strptime(" ".join(found[1:6]), "%a %b %d %H:%M:%S %Y"))
        pid = int(found[0])
        if pid <= 1:
            return None
    return None


def over(run: str, base: str) -> bool:
    """Whether a run has ended. A run lives in the process that started it, and a session that dies writes no
    record. So a run with no record is also over when nothing in its folder was written since the session's
    process started. Silence alone never ends a run: one written since that start is going."""
    if os.path.exists(record_of(run, base)):
        return True
    if RESTARTED is None:
        return False
    written = [os.path.join(run, "journal.jsonl"), *glob.glob(os.path.join(run, "agent-*.jsonl"))]
    return max(os.path.getmtime(path) for path in written) < RESTARTED


def launch_of(run: str, base: str) -> dict | None:
    """What started a run: its record, or for a run that died with its session, the input of the Workflow
    call in the session's transcript whose result names the run (`Run ID: <run>`, Claude Code 2.1.328)."""
    record = record_of(run, base)
    if os.path.exists(record):
        return json.load(open(record, encoding="utf-8"))
    name, calls = os.path.basename(run), {}
    try:
        lines = open(os.path.dirname(base) + ".jsonl", encoding="utf-8", errors="replace")
    except OSError:
        return None
    with lines:
        for line in lines:
            if '"Workflow"' not in line and name not in line:
                continue
            try:
                content = json.loads(line)["message"]["content"]
            except (ValueError, KeyError, TypeError):
                continue
            for part in content if isinstance(content, list) else []:
                if not isinstance(part, dict):
                    continue
                if part.get("type") == "tool_use" and part.get("name") == "Workflow":
                    calls[part.get("id")] = part.get("input") or {}
                elif part.get("tool_use_id") in calls and f"Run ID: {name}" in json.dumps(part.get("content")):
                    return calls[part["tool_use_id"]]
    return None


def unfinished(run: str, base: str) -> list[dict]:
    """The agents of one run folder that started and have no result row, each with its state."""
    label, done, failed, order = {}, set(), set(), []
    entries = journal(run)
    stage = stages(entries)
    for row in entries:
        kind, agent = row.get("type"), row.get("agentId")
        if kind == "started":
            label[agent] = row.get("label", "")
            order.append(agent)
        elif kind == "result":
            done.add(agent)
        elif kind == "failed":
            failed.add(agent)
    finished = {stage[a] for a in done if a in stage}
    ended_run = over(run, base)
    rows = []
    for agent in order:
        if agent in done:
            continue
        log = os.path.join(run, f"agent-{agent}.jsonl")
        row = {"run": os.path.basename(run), "label": label[agent], "stage": stage[agent],
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
        ended = ended_run or agent in failed
        if os.path.exists(os.path.join(base, f"agent-{agent}.jsonl")):
            row["state"] = "revived"
        elif not ended_run and time.time() - os.path.getmtime(log) < QUIET_S:
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


def schema_of(log: str) -> dict | None:
    """The JSON Schema of the result an agent owes, from the StructuredOutput tool its log records."""
    with open(log, encoding="utf-8", errors="replace") as lines:
        for line in lines:
            if '"api-request-shape"' not in line or "StructuredOutput" not in line:
                continue
            try:
                tools = json.loads(line)["shape"]["tools"]
            except (ValueError, KeyError, TypeError):
                continue
            for tool in tools:
                if isinstance(tool, dict) and tool.get("name") == "StructuredOutput":
                    return tool.get("input_schema")
    return None


KINDS = {"string": str, "integer": int, "number": (int, float), "boolean": bool, "array": list, "object": dict}


def misfit(value, schema: dict) -> list[str]:
    """Why a report cannot stand in for the stage's result: its top level against the schema's."""
    if not isinstance(value, dict):
        return ["it is not a JSON object"]
    lacks = [key for key in schema.get("required", []) if key not in value]
    wrong = [key for key, rule in schema.get("properties", {}).items()
             if key in value and isinstance(rule, dict) and rule.get("type") in KINDS
             and (not isinstance(value[key], KINDS[rule["type"]]) or isinstance(value[key], bool) != (rule["type"] == "boolean"))]
    return ([f"lacks {', '.join(lacks)}"] if lacks else []) + ([f"wrong type for {', '.join(wrong)}"] if wrong else [])


def report(reports: str, row: dict):
    """A revived agent's result, or {"$wait": file} while its report is not there or does not fit."""
    path, schema = report_of(reports, row["id"]), schema_of(row["log"])
    try:
        text = open(path, encoding="utf-8").read()
    except OSError:
        return {"$wait": path}
    if schema is None:              # the stage returns its last message as text
        return text if text.strip() else {"$wait": path}
    try:
        value = json.loads(text)
    except ValueError:
        return {"$wait": path, "$misfit": ["it is not JSON"]}
    problems = misfit(value, schema)
    return {"$wait": path, "$misfit": problems} if problems else value


def done_map(run: str, base: str, reports: str) -> dict:
    """A run's stages that need no agent: label to result, or to the report a revived agent will write."""
    rows = journal(run)
    stage, out = stages(rows), {}
    for row in rows:
        if row.get("type") == "result" and row.get("result") is not None and row.get("agentId") in stage:
            out[stage[row["agentId"]]] = row["result"]
    for row in unfinished(run, base):
        if row["state"] != "revived" or row["stage"] in out:
            continue
        out[row["stage"]] = report(reports, row)
    return out


def plan_of(run: str, base: str, reports: str) -> list[dict]:
    """What reviving one run would do for each of its stages, in the order they started. Reads only."""
    rows = journal(run)
    stage = stages(rows)
    finished = {stage[r["agentId"]]: r["agentId"] for r in rows if r.get("type") == "result" and r.get("agentId") in stage}
    held: dict[str, dict] = {}           # the attempt each unfinished stage would continue from
    for row in unfinished(run, base):
        if row["state"] in IGNORED:
            continue
        if row["stage"] not in held or held[row["stage"]]["state"] == "no-transcript":
            held[row["stage"]] = row
    ended_run = over(run, base)
    steps = []
    for name in dict.fromkeys(stage.values()):
        row = held.get(name, {})
        if name == PROBE:
            step = {"action": "ignore", "why": "probes of the API are not work"}
        elif name in finished:
            step = {"action": "reuse", "id": finished[name], "why": "it finished"}
        elif row["state"] == "revived":
            got = report(reports, row)
            if isinstance(got, dict) and "$wait" in got:
                why = f"does not fit: {'; '.join(got['$misfit'])}" if "$misfit" in got else "is not written"
                step = {"action": "wait", "id": row["id"], "why": f"its revived agent's report {got['$wait']} {why}"}
            else:
                step = {"action": "reuse", "id": row["id"], "why": "its revived agent's report"}
        elif not ended_run:
            step = {"action": "leave", "id": row["id"], "why": f"its run is going and its script owns it ({row['state']})"}
        elif row["state"] == "dead":
            step = {"action": "revive", "id": row["id"], "why": f"it died after {row['turns']} turns"}
        else:
            step = {"action": "start fresh", "id": row["id"],
                    "why": "it left no transcript" if row["state"] == "no-transcript" else "it never got past its first turn"}
        steps.append({"run": os.path.basename(run), "stage": name, **step})
    return steps


def rerun(run: str, base: str, reports: str, out_dir: str, wait: bool) -> dict | None:
    """The Workflow input that carries one finished run on, or why there is none."""
    name = os.path.basename(run)
    open_rows = [r for r in unfinished(run, base) if r["state"] not in IGNORED]
    if not open_rows:
        return None
    if not over(run, base):
        return {"run": name, "skipped": "the run is still going: leave it alone"}
    record = launch_of(run, base)
    if record is None:
        return {"run": name, "skipped": "the run died with its session, and the session's transcript holds no Workflow "
                "call that started it. Start each unit's remaining stages by hand; `done` prints the finished results."}
    source_path = record.get("scriptPath", "")
    source = open(source_path, encoding="utf-8").read() if os.path.exists(source_path) else record.get("script", "")
    finished = done_map(run, base, reports)
    waiting = [label for label, value in finished.items() if isinstance(value, dict) and "$wait" in value]
    plan = {"run": name, "finished": len(finished) - len(waiting), "waiting": waiting,
            "starts fresh": [r["stage"] for r in open_rows if r["state"] != "revived"]}
    bad = {label: finished[label]["$misfit"] for label in waiting if "$misfit" in finished[label]}
    if bad:
        return {**plan, "misfit": bad, "skipped": "these reports do not fit their stage's schema, so the next "
                "stage could not read them. Send each agent one message: its report must fit the schema "
                "file that stage printed for it."}
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
    parser.add_argument("verb", choices=["list", "plan", "stage", "check", "done", "rerun"])
    parser.add_argument("--session", default=os.environ.get("CLAUDE_CODE_SESSION_ID", ""))
    parser.add_argument("--runs", default="")
    parser.add_argument("--hours", type=float, default=6)
    parser.add_argument("--ids", default="")
    parser.add_argument("--all-dead", action="store_true")
    parser.add_argument("--reports", default="")
    parser.add_argument("--out", default="")
    parser.add_argument("--wait", action="store_true")
    parser.add_argument("--restarted", default="")
    args = parser.parse_args()
    if not args.session:
        sys.exit("wf_revive: no session id; give --session")
    base = subagents_dir(args.session)
    global RESTARTED
    if args.restarted:
        RESTARTED = datetime.datetime.fromisoformat(args.restarted.replace("Z", "+00:00")).timestamp()
    elif args.session == os.environ.get("CLAUDE_CODE_SESSION_ID"):
        RESTARTED = process_started()
    reports = args.reports or os.path.join(base, "revived")
    runs = runs_of(base, args.runs, args.hours)
    ids = set(filter(None, args.ids.split(",")))

    if args.verb == "done":
        merged: dict = {}
        for run in runs:
            merged.update(done_map(run, base, reports))
        print(json.dumps(merged, ensure_ascii=False))
        return 0

    if args.verb == "plan":
        steps = [step for run in runs for step in plan_of(run, base, reports)]
        for step in steps:
            print(json.dumps(step, ensure_ascii=False))
        actions = [step["action"] for step in steps]
        print(json.dumps({action: actions.count(action) for action in sorted(set(actions))}))
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
            where, schema = report_of(reports, row["id"]), schema_of(row["log"])
            if schema is not None:
                with open(where[:-len(".json")] + ".schema.json", "w", encoding="utf-8") as handle:
                    json.dump(schema, handle, indent=1)
            print(f"staged  {row['label']:26} {row['id']}  ({row['minutes']} min, {row['turns']} turns)  report: {where}"
                  + (f"  schema: {where[:-len('.json')]}.schema.json" if schema is not None else "  (its result is text)"))
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
        got = report(reports, {"id": agent, "log": original[0]})
        written = ("no report yet" if got == {"$wait": report_of(reports, agent)} else
                   f"report does not fit: {'; '.join(got['$misfit'])}" if isinstance(got, dict) and "$misfit" in got
                   else "report written")
        print(f"{meta.get('description', ''):26} {agent}  {turns_since_revival(log, original[0]):4} turns since revival  {written}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
