#!/usr/bin/env python3
"""Tests for wf_revive.py against a session folder built in a temp HOME.

  python3 ops/skills/revive-workflow-agents/test_wf_revive.py
"""
import json
import os
import subprocess
import sys
import tempfile
import time
import unittest

TOOL = os.path.join(os.path.dirname(os.path.abspath(__file__)), "wf_revive.py")
SESSION = "11111111-2222-3333-4444-555555555555"
RUN = "wf_aaaaaaaa-001"
SCRIPT = """export const meta = { name: 't', description: 't' }
const DONE = (typeof args === 'object' && args && args.done) || {} // wf-revive:done
return DONE
"""


def turn(kind: str, content, minute: int) -> str:
    return json.dumps({"message": {"content": content}, "timestamp": f"2026-01-01T10:{minute:02d}:00.000Z",
                       "type": kind}, separators=(",", ":"))


class Revive(unittest.TestCase):
    def setUp(self):
        self.home = tempfile.TemporaryDirectory()
        self.session = os.path.join(self.home.name, ".claude", "projects", "-work", SESSION)
        self.base = os.path.join(self.session, "subagents")
        self.run = os.path.join(self.base, "workflows", RUN)
        os.makedirs(self.run)
        self.rows = []

    def tearDown(self):
        self.home.cleanup()

    def agent(self, agent, label, turns, outcome=None, last_tool="", age_s=600, schema=None):
        """One agent of the run: its started row, its log, and how the run saw it end."""
        self.rows.append({"type": "started", "agentId": agent, "label": label})
        if outcome == "result":
            self.rows.append({"type": "result", "agentId": agent, "result": {"value": label}})
        elif outcome == "failed":
            self.rows.append({"type": "failed", "agentId": agent})
        lines = [turn("user", "go", 0)]
        if schema:
            lines.append(json.dumps({"type": "api-request-shape", "shape": {"tools": [
                {"name": "Bash"}, {"name": "StructuredOutput", "input_schema": schema}]}}, separators=(",", ":")))
        for n in range(turns):
            last = n == turns - 1
            content = [{"type": "tool_use", "name": last_tool, "input": {}}] if last and last_tool else [{"type": "text", "text": "x"}]
            lines.append(turn("assistant", content, n + 1))
        log = os.path.join(self.run, f"agent-{agent}.jsonl")
        with open(log, "w") as handle:
            handle.write("\n".join(lines) + "\n")
        with open(log[:-len(".jsonl")] + ".meta.json", "w") as handle:
            json.dump({"description": label}, handle)
        os.utime(log, (time.time() - age_s,) * 2)

    def end(self, agent, outcome):
        """The journal row that ends an agent started with no outcome, written after other agents started."""
        label = next(row["label"] for row in self.rows if row.get("agentId") == agent and row["type"] == "started")
        self.rows.append({"type": outcome, "agentId": agent, **({"result": {"value": label}} if outcome == "result" else {})})

    def tree(self):
        found = {}
        for folder, _, names in os.walk(self.home.name):
            for name in names:
                path = os.path.join(folder, name)
                found[path] = (os.path.getsize(path), os.stat(path).st_mtime_ns)
            found[folder] = "folder"
        return found

    def plan(self):
        lines = self.tool("plan").splitlines()
        steps = [json.loads(line) for line in lines[:-1]]
        self.assertTrue(all(step["run"] == RUN and step["why"] for step in steps), steps)
        return {step["stage"]: (step["action"], step.get("id")) for step in steps}, json.loads(lines[-1])

    def write(self, finished=True):
        with open(os.path.join(self.run, "journal.jsonl"), "w") as handle:
            handle.write("\n".join(json.dumps(row) for row in self.rows) + "\n")
        if finished:
            os.makedirs(os.path.join(self.session, "workflows"), exist_ok=True)
            script = os.path.join(self.home.name, "script.js")
            with open(script, "w") as handle:
                handle.write(SCRIPT)
            with open(os.path.join(self.session, "workflows", f"{RUN}.json"), "w") as handle:
                json.dump({"runId": RUN, "scriptPath": script, "script": SCRIPT, "args": {"units": ["a", "b"]}}, handle)

    def tool(self, *args):
        ran = subprocess.run([sys.executable, TOOL, *args, "--session", SESSION, "--runs", RUN],
                             capture_output=True, text=True, env={**os.environ, "HOME": self.home.name})
        self.assertEqual(ran.returncode, 0, ran.stderr)
        return ran.stdout

    def states(self):
        """Agent id to state, for each row `list` shows. A label can hold spaces, so find the id by name."""
        ids, found = {row["agentId"] for row in self.rows}, {}
        for line in self.tool("list").splitlines()[:-1]:
            words = line.split()
            at = next(n for n, word in enumerate(words) if word in ids)
            found[words[at]] = words[at + 1]
        return found

    def test_an_agent_in_a_long_tool_call_is_not_dead(self):
        self.agent("a1", "build:u1", 5, "failed")
        self.agent("a2", "build:u2", 5, None, last_tool="Bash")     # ten minutes inside one Bash call
        self.agent("a3", "build:u3", 5, "result")
        self.agent("a4", "build:u4", 5, None, age_s=5)
        self.write(finished=False)
        self.assertEqual(self.states(), {"a1": "dead", "a2": "quiet", "a4": "live"})
        self.assertIn("in Bash", self.tool("list"))
        staged = self.tool("stage", "--all-dead")
        self.assertIn("1 staged", staged)
        self.assertTrue(os.path.exists(os.path.join(self.base, "agent-a1.jsonl")))
        self.assertFalse(os.path.exists(os.path.join(self.base, "agent-a2.jsonl")))
        self.assertEqual(self.states()["a1"], "revived")

    def test_when_the_run_is_over_every_agent_that_never_returned_is_dead(self):
        self.agent("a1", "build:u1", 5, None, last_tool="Bash", age_s=5)    # the run was stopped a moment ago
        self.agent("a2", "build:u2", 1, None)
        self.write()
        self.assertEqual(self.states(), {"a1": "dead", "a2": "never-started"})

    def test_the_attempt_that_got_furthest_is_the_one_to_continue(self):
        self.agent("a1", "build:u1", 1, "failed")
        self.agent("a2", "build:u1:try2", 9, "failed")
        self.agent("a3", "build:u1:try3", 1, "failed")
        self.agent("b1", "build:u2", 7, "failed")
        self.agent("b2", "build:u2:try2", 1, "failed")
        self.agent("c1", "build:u3", 1, "failed")
        self.write()
        self.assertEqual(self.states(), {"a2": "dead", "b1": "dead", "c1": "never-started"})

    def test_done_holds_finished_results_and_waits_for_revived_agents(self):
        self.agent("a1", "build:u1:wait:try2", 3, "result")    # a waiting agent's result is its stage's
        verdict = {"type": "object", "properties": {"verdict": {"type": "string"}}, "required": ["verdict"]}
        built = {"type": "object", "properties": {"value": {"type": "string"}}, "required": ["value"]}
        self.agent("a2", "verify:u1", 4, "failed", schema=verdict)
        self.agent("b1", "build:u2", 3, "failed", schema=built)
        self.agent("b2", "build:u2:try2", 1, "failed")
        self.agent("c1", "build:u3", 1, "failed")
        self.write()
        self.tool("stage", "--all-dead")
        reports = os.path.join(self.base, "revived")
        with open(os.path.join(reports, "a2.json"), "w") as handle:
            json.dump({"verdict": "pass"}, handle)
        done = json.loads(self.tool("done"))
        self.assertEqual(done, {"build:u1": {"value": "build:u1:wait:try2"}, "verify:u1": {"verdict": "pass"},
                                "build:u2": {"$wait": os.path.join(reports, "b1.json")}})

        held = json.loads(self.tool("rerun"))
        self.assertEqual((held["waiting"], "workflow" in held), (["build:u2"], False))
        self.assertIn("have not written their reports", held["skipped"])

        plan = json.loads(self.tool("rerun", "--wait"))
        self.assertEqual((plan["finished"], plan["waiting"], plan["starts fresh"]), (2, ["build:u2"], ["build:u3"]))
        self.assertEqual(plan["workflow"]["args"], {"units": ["a", "b"]})
        with open(plan["workflow"]["scriptPath"]) as handle:
            copy = handle.read()
        self.assertTrue(copy.startswith("export const meta"))
        line = [l for l in copy.splitlines() if "wf-revive:done" in l]
        self.assertEqual(len(line), 1)
        self.assertEqual(json.loads(line[0][len("const DONE = "):line[0].index(" // wf-revive:done")]), done)

        with open(os.path.join(reports, "b1.json"), "w") as handle:
            json.dump({"value": "built"}, handle)
        plan = json.loads(self.tool("rerun"))
        self.assertEqual((plan["finished"], plan["waiting"]), (3, []))
        with open(plan["workflow"]["scriptPath"]) as handle:
            self.assertNotIn("$wait", handle.read())

    def test_a_report_in_a_shape_of_its_own_holds_the_rerun(self):
        schema = {"type": "object", "properties": {"done": {"type": "array"}, "lines": {"type": "integer"}},
                  "required": ["done", "lines"]}
        self.agent("a1", "build:u1", 4, "failed", schema=schema)
        self.agent("b1", "notes:u1", 4, "failed")                 # no schema: its result is text
        self.write()
        staged = self.tool("stage", "--all-dead")
        reports = os.path.join(self.base, "revived")
        with open(os.path.join(reports, "a1.schema.json")) as handle:
            self.assertEqual(json.load(handle), schema)
        self.assertIn("(its result is text)", staged)
        with open(os.path.join(reports, "a1.json"), "w") as handle:
            json.dump({"status": "all good", "lines": "many"}, handle)
        with open(os.path.join(reports, "b1.json"), "w") as handle:
            handle.write("Three notes, as plain text.")
        held = json.loads(self.tool("rerun", "--wait"))
        self.assertEqual(held["misfit"], {"build:u1": ["lacks done", "wrong type for lines"]})
        self.assertNotIn("workflow", held)
        self.assertIn("report does not fit: lacks done; wrong type for lines", self.tool("check", "--ids", "a1"))

        with open(os.path.join(reports, "a1.json"), "w") as handle:
            json.dump({"done": ["x"], "lines": 3}, handle)
        self.assertEqual(json.loads(self.tool("done")),
                         {"build:u1": {"done": ["x"], "lines": 3}, "notes:u1": "Three notes, as plain text."})

    def test_every_retry_ending_that_scripts_use_groups_with_its_stage(self):
        self.agent("a1", "build:x", 1, "failed")
        self.agent("a2", "build:x-try2", 8, "failed")
        self.agent("b1", "write-design", 3, "failed")
        self.agent("b2", "write-design-try2", 1, "failed")
        self.agent("c1", "verify:x", 5, "failed")
        self.agent("c2", "verify:x (retry 1)", 6, "failed")
        self.agent("d1", "land:x", 1, "failed")
        self.agent("d2", "land:x-attempt2", 6, "failed")
        self.agent("d3", "land:x-attempt3", 1, "failed")
        self.agent("e1", "notes:x", 0, "failed")
        self.agent("e2", "notes:x-retry2", 1, "failed")
        self.agent("f1", "fix:x", 1, "failed")
        self.agent("f2", "fix:x#2", 7, "failed")       # one after another: a counted retry
        self.agent("f3", "fix:x#3", 1, "failed")
        self.agent("g1", "verify:x:r2", 1, "failed")   # a second round is a stage of its own
        self.agent("g2", "verify:x:r2-attempt2", 4, "failed")
        self.agent("h1", "build:y", 1, "failed")
        self.agent("h2", "build:y-try2", 4, "result")
        self.agent("p1", "probe:try1", 0, "failed")
        self.agent("p2", "probe:try2", 1, "failed")
        self.write()
        self.assertEqual(self.states(), {"a2": "dead", "b1": "dead", "c2": "dead", "d2": "dead", "e2": "never-started",
                                         "f2": "dead", "g2": "dead", "p2": "never-started"})
        self.assertEqual(json.loads(self.tool("done")), {"build:y": {"value": "build:y-try2"}})

    def test_agents_numbered_side_by_side_are_stages_of_their_own(self):
        for agent, turns in (("r0", 4), ("r1", 5), ("r2", 1)):
            self.agent(agent, f"review:x#{agent[1]}", turns)
        self.agent("s1", "synthesize", 3)
        self.agent("s2", "synthesize#2", 3)
        self.end("r0", "result")
        self.end("r1", "failed")
        self.end("r2", "failed")
        self.end("s2", "result")
        self.end("s1", "result")
        self.agent("q1", "check:x#1", 3, "result")     # a panel that a concurrency cap ran one at a time
        self.agent("q2", "check:x#2", 3, "result")
        self.agent("q3", "check:x#3", 4, "failed")
        self.write()
        self.assertEqual(self.states(), {"r1": "dead", "r2": "never-started", "q3": "dead"})
        self.assertEqual(json.loads(self.tool("done")), {"review:x#0": {"value": "review:x#0"},
                                                         "synthesize#2": {"value": "synthesize#2"},
                                                         "synthesize": {"value": "synthesize"},
                                                         "check:x#1": {"value": "check:x#1"},
                                                         "check:x#2": {"value": "check:x#2"}})

    def test_plan_says_what_a_revive_would_do_for_each_stage_and_changes_nothing(self):
        self.agent("a1", "build:u1", 5, "result")
        self.agent("b1", "build:u2", 1, "failed")
        self.agent("b2", "build:u2-try2", 9, "failed")
        self.agent("c1", "build:u3", 1, "failed")
        self.agent("d1", "verify:u1", 3, "failed")
        os.remove(os.path.join(self.run, "agent-d1.jsonl"))
        self.agent("p1", "probe:try1", 0, "failed")
        self.agent("p2", "probe:try2", 1, "result")
        self.agent("e1", "verify:u2", 4, "failed")
        self.agent("f1", "verify:u3", 4, "failed")
        self.write()
        self.tool("stage", "--ids", "e1,f1")
        reports = os.path.join(self.base, "revived")
        with open(os.path.join(reports, "e1.json"), "w") as handle:
            handle.write("Verified.")
        before = self.tree()
        steps, counts = self.plan()
        self.assertEqual(steps, {"build:u1": ("reuse", "a1"), "build:u2": ("revive", "b2"), "build:u3": ("start fresh", "c1"),
                                 "verify:u1": ("start fresh", "d1"), "probe": ("ignore", None),
                                 "verify:u2": ("reuse", "e1"), "verify:u3": ("wait", "f1")})
        self.assertEqual(counts, {"ignore": 1, "reuse": 2, "revive": 1, "start fresh": 2, "wait": 1})
        self.assertEqual(self.tree(), before)

    def test_plan_leaves_the_stages_of_a_run_that_is_going(self):
        self.agent("a1", "build:u1", 5, "result")
        self.agent("b1", "build:u2", 5, "failed")
        self.agent("c1", "build:u3", 5, None, last_tool="Bash")
        self.agent("d1", "build:u4", 5, None, age_s=5)
        self.agent("p1", "probe:try1", 0, "failed")
        self.write(finished=False)
        steps, _ = self.plan()
        self.assertEqual(steps, {"build:u1": ("reuse", "a1"), "build:u2": ("leave", "b1"), "build:u3": ("leave", "c1"),
                                 "build:u4": ("leave", "d1"), "probe": ("ignore", None)})

    def test_rerun_leaves_a_run_that_is_still_going_and_a_script_with_no_mark(self):
        self.agent("a1", "build:u1", 5, "failed")
        self.write(finished=False)
        self.assertIn("still going", self.tool("rerun"))
        record = os.path.join(self.session, "workflows", f"{RUN}.json")
        os.makedirs(os.path.dirname(record))
        with open(record, "w") as handle:
            json.dump({"runId": RUN, "scriptPath": "/nowhere.js", "script": "export const meta = {}\nreturn 1\n"}, handle)
        plan = json.loads(self.tool("rerun"))
        self.assertIn("finished stages would run again", plan["skipped"])
        self.assertNotIn("workflow", plan)


if __name__ == "__main__":
    unittest.main()
