---
name: revive-workflow-agents
description: Continue Claude Code Workflow agents that died mid-work (an API outage, "Unable to connect to API", a 503 from the pool, a crash or a killed session) from their saved transcripts, then carry their runs on without redoing finished stages. Use when a workflow result or its journal shows failed agents, before any resumeFromRunId, and whenever the user says "resume the failed workflows", "revive the agents" or "resume from transcripts".
---

# Revive workflow agents

A workflow agent that dies mid-work loses nothing on disk. Claude Code writes its whole conversation to `agent-<id>.jsonl` in the run folder, with nothing to switch on. `resumeFromRunId` never reads that file: it starts the agent's stage again with an empty context. This skill continues the dead agent itself, with everything it had read and decided, and then starts its run again so that finished stages call no agent.

Measured on 2026-11-21 (Claude Code 2.1.328), after the pool was unreachable for 137 minutes: 28 agents of 10 runs, each 7 to 65 minutes into its work, were all continued. Each one's first action was the check its message asked for, in its own worktree. None started over.

The whole chain was run on 2026-11-22 on a run of two units and four stages, stopped with one agent mid-stage: the agent was revived and wrote its report, and the run started again by step 7 called 0 agents and returned all four results in 16 ms.

`wf_revive.py` is beside this file. Run it from any directory; the session id comes from `$CLAUDE_CODE_SESSION_ID`.

## Install

```
ln -s <a checkout of this repository>/ops/skills/revive-workflow-agents ~/.claude/skills/revive-workflow-agents
```

## When to use it

- A workflow result lists `failed:` agents, `journal.jsonl` has `failed` rows, or a run was stopped with agents mid-work.
- The cause was outside the agents: the model could not be reached, the pool answered 503, the machine slept, the session was killed.
- The agent had done real work. One that died on its first turn has nothing to continue: its stage starts fresh.

## Before anything: do not resume a run with several units

`resumeFromRunId` reuses finished agents only up to the first call whose position differs from the first run. A script that runs units side by side makes its calls in another order the second time, because the cached stages return at once. Everything after the first shifted call runs again, finished or not.

On 2026-11-21 five such runs were resumed with the exact script and arguments. Within two minutes they were verifying and merging units that were already merged. They had to be stopped.

- Resume is safe for a run with one chain of calls (one unit, or `maxActive` 1 from the start).
- For a run with several units, use the steps below.
- If a resume was already started wrongly: stop it with `TaskStop` on its task id, check that no `agent-*.jsonl` in its run folder was written after the stop, and remove any worktree or folder its agents made.

## Steps

1. **Confirm the cause is over.** For the pool: `curl -sm8 --unix-socket "$ANTHROPIC_UNIX_SOCKET" -o /dev/null -w '%{http_code}\n' http://localhost/_subrouter/health` prints 200. For the network: `curl -sm5 -o /dev/null -w '%{http_code}\n' https://api.anthropic.com` answers. Never restart the router from an agent session: `ops/LAPTOP.md` says who does, and how.

2. **List the agents that returned no result.**

   ```
   python3 ~/.claude/skills/revive-workflow-agents/wf_revive.py list --hours 6
   ```

   | State | Meaning | Do |
   |---|---|---|
   | `dead` | The run gave up on it (its journal has a `failed` row), or the run is over, finished or stopped, and the agent never returned. | Revive it. |
   | `quiet` | Its run is going, it has no `failed` row, and it wrote nothing for two minutes. The row says `in Bash` (a long tool call) or `waiting for the model`. | Leave it. Revive it with `--ids` only when its whole session died, which leaves no record that the run ended. |
   | `never-started` | One turn at most. | Nothing to continue. Its stage starts fresh in step 7. |
   | `revived` | Its transcript is staged. | Already continued, or ready for step 5. |
   | `live` | Wrote in the last two minutes. | Leave it alone. |

   An agent ten minutes into one test run is `quiet`, not dead. A message to it starts a second copy in the same folder.

3. **Make sure nothing else works in the same place.** A revived agent and a new agent in one worktree overwrite each other. If a resumed run is redoing a dead agent's stage, stop that run first.

4. **Stage the transcripts.**

   ```
   python3 ~/.claude/skills/revive-workflow-agents/wf_revive.py stage --all-dead      # or --ids a1,a2 --runs wf_x
   ```

   It copies `agent-<id>.jsonl` and `.meta.json` from the run folder into `<session>/subagents/`, and prints the report file for each agent. Without the copy, SendMessage answers "No transcript found".

5. **Continue each agent with `SendMessage(to=<agentId>)`.** The tool answers `Resuming agent <id>`. Send several in one turn; revive one first if the mechanism is in doubt. The message says, in this order:
   - What happened and when: the connection dropped at HH:MM and is back. "You are the same agent, continuing the same task with your whole conversation; nothing of your instructions has changed."
   - What to check first: `git status --short`, `git log --oneline -5`, the tail of its notes file. A command that was running when the connection dropped may have been cut off half way.
   - What changed while it was away, if it matters to this agent.
   - How to end: "Finish your task as your instructions say. Write your final result, in the JSON shape your instructions asked for and nothing else, to `<the report file stage printed>`. Make your last message at most six lines: done or not, what did not run, and that path." The revived copy has no StructuredOutput tool, and its last message lands whole in the main session.

   Say all of it in the first message. A later message to a working agent is queued for its next tool round and moves a tool call it is running to the background. Give each role its own wording: a builder should not merge the main line now, and a lander must fetch the main line again before it trusts anything fetched earlier.

6. **Confirm.**

   ```
   python3 ~/.claude/skills/revive-workflow-agents/wf_revive.py check --ids a1,a2,...
   ```

   Each revived agent shows turns since revival within two minutes, and later `report written`.

7. **Carry the runs on.** A revived agent is a subagent of the main session. Its run is over, so no later stage follows by itself.

   ```
   python3 ~/.claude/skills/revive-workflow-agents/wf_revive.py rerun --hours 6       # or --runs wf_x
   ```

   For each run that is over and has unfinished stages, it writes a copy of the run's script with a map in it, and prints `{"workflow": {"scriptPath": ..., "args": ...}}`. Call `Workflow` with that input. In the new run:
   - a stage that finished in the old run returns its old result and calls no agent, in any call order;
   - a stage that a revived agent finished returns that agent's report;
   - a stage that never started, and everything after, runs as usual.

   `rerun` holds a run back while one of its revived agents has not written its report, and names the stages it waits for. Run it again when their notifications arrive. With `--wait` it does not hold back: the new run starts now, and one agent for each such stage waits for the report file and returns it. Use that when a run has units that can go on while a slow builder finishes. Start a run once.

   This needs a script written as the next section says. For a script without the mark, `rerun` says so and writes nothing. Then start each unit's remaining stages by hand: for a revived builder a verifier, for a revived verifier that passed a lander, for one that failed a fix round. `wf_revive.py done` prints every finished result to work from.

## Make a script revivable

A few lines at the top of the script body, and `step` wherever the script called `agent`:

```js
const DONE = (args && args.done) || {} // wf-revive:done
const step = (label, prompt, opts = {}) => {
  const had = DONE[label]
  if (had === undefined) return agent(prompt, { ...opts, label })
  if (!had || !had.$wait) return Promise.resolve(had)
  return agent(`An agent outside this run is finishing the stage "${label}". Wait for the file ${had.$wait}: run \`until [ -s ${had.$wait} ]; do sleep 20; done; cat ${had.$wait}\` as one Bash call with a ten-minute limit, and again each time it times out. The file holds one JSON object, which is this stage's result. Return that same object as your result: the same keys with the same values. Do nothing else.`,
    { schema: opts.schema, phase: opts.phase, label: `${label}:wait` })
}
```

- Keep the comment `// wf-revive:done` on the `DONE` line. `rerun` replaces that one line.
- The waiting agent runs on the session's model, on purpose. On 2026-11-22 the smallest model at low effort copied a one-key report wrongly three times out of three (the file's text inside a string field twice, the bare value once). The default model copied it right once out of once. Without `--wait` no model copies anything.
- Give every stage a label that is the same on each run and names one stage of one unit: `build:<unit>`, `verify:<unit>:r2`. A retry may add `:try2`; the tool ignores that suffix.
- A stage's result must be all the next stage needs. The new run has the old result, not the old agent.
- The map can also be passed as `args.done`, from `wf_revive.py done`.

## What it costs

- One full cache write of the agent's context for each revived agent. A workflow agent's cache lasts five minutes and is gone by the time anyone notices an outage.
- Each revived agent's last message lands in the main session. Thirty long ones are tens of thousands of tokens, which is why step 5 asks for six lines and a file.
- With `--wait`, one agent for each stage that is waited for: about 55,000 tokens of fixed prompt each.

## Make the next outage cheaper

- **Retry waits must outlast an outage.** Waits of 1, 3 and 10 minutes gave up after 25 minutes of a 137-minute outage, and each script then went on to its next stage, which failed too. Use waits that add up to hours, or end the unit at the first stage that returns null.
- **Agents keep a notes file** and write to it as they go, so that even a fresh agent can pick up.
- **Long units run as separate runs.** A run with one chain of calls can also be resumed safely.

## Test

```
python3 ops/skills/revive-workflow-agents/test_wf_revive.py
```

## Related

- The daemon's `--transcripts` capture is a fallback for a damaged agent log. This skill does not read it.
- A `SendMessage` to an agent that is still running starts a second copy of it. Check its state before sending.
