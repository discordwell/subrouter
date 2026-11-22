---
name: revive-workflow-agents
description: Continue Claude Code Workflow agents that died mid-work (an API outage, "Unable to connect to API", a 503 from the pool, a crash or a killed session) from their saved transcripts, then carry their runs on without redoing finished stages. Holds the standard prefix every workflow script starts with. Use when a workflow result shows failed agents or a `stopped` list, or its journal has `failed` rows, before any resumeFromRunId, when writing a workflow script, and whenever the user says "resume the failed workflows", "revive the agents" or "resume from transcripts".
---

# Revive workflow agents

A workflow agent that dies mid-work loses nothing on disk. Claude Code writes its whole conversation to `agent-<id>.jsonl` in the run folder, with nothing to switch on. `resumeFromRunId` never reads that file: it starts the agent's stage again with an empty context. This skill continues the dead agent itself, with everything it had read and decided, and then starts its run again so that finished stages call no agent.

Measured on 2026-11-21 (Claude Code 2.1.328), after the pool was unreachable for 137 minutes: 28 agents of 10 runs, each 7 to 65 minutes into its work, were all continued. Each one's first action was the check its message asked for, in its own worktree. None started over.

The whole chain was run twice on 2026-11-22 on small runs stopped with one agent mid-stage. Each time the agent was revived and wrote its report, and the run started again by step 7 called 0 agents and returned every stage's result in under 20 ms. The second time the agent was given its schema file, and its report fit.

`wf_revive.py` is beside this file. Run it from any directory; the session id comes from `$CLAUDE_CODE_SESSION_ID`.

## Install

```
ln -s <a checkout of this repository>/ops/skills/revive-workflow-agents ~/.claude/skills/revive-workflow-agents
```

## When to use it

- A workflow result lists `failed:` agents, `journal.jsonl` has `failed` rows, or a run was stopped with agents mid-work.
- The cause was outside the agents: the model could not be reached, the pool answered 503, the machine slept, the session was killed.
- The agent had done real work. One that died on its first turn has nothing to continue: its stage starts fresh.
- A run ended with a `stopped` list that is not empty. The prefix in "Make a script revivable" writes it, one `{label, why}` for each unit that stopped.

For a `stopped` list, start at step 1: the daemon and the network must both answer. Then revive with steps 2 to 7. Never give a stopped stage to a fresh agent. An entry whose `why` starts with `not started` has no agent behind it, and its stage starts in step 7.

## Before anything: do not resume a run with several units

`resumeFromRunId` reuses finished agents only up to the first call whose position differs from the first run. A script that runs units side by side makes its calls in another order the second time, because the cached stages return at once. Everything after the first shifted call runs again, finished or not.

On 2026-11-21 five such runs were resumed with the exact script and arguments. Within two minutes they were verifying and merging units that were already merged. They had to be stopped.

- Resume only a run of one unit, whose calls form one chain.
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

   Attempts at one stage count once, and the one that got furthest is the one shown. The tool drops these endings from a label: `:wait`, `:try<N>`, `-try<N>`, `-attempt<N>`, `-retry<N>` and ` (retry <N>)`. It drops `#<N>` only when an agent under that label with no number started before it, and every agent before it under that label had failed before it started: `fix:<unit>`, then `fix:<unit>#2`. A panel that numbers every member, like `review:<unit>#1` and `review:<unit>#2`, stays apart, even when a concurrency cap starts its members one at a time after the earlier ones died. So do agents numbered `#<N>` that ran side by side, or after one of them returned a result. In the journals read on 2026-11-21, every `#<N>` retry followed an unnumbered first attempt, and most `#<N>` labels numbered agents that ran side by side.

   To see what the remaining steps would do with every stage, without changing anything:

   ```
   python3 ~/.claude/skills/revive-workflow-agents/wf_revive.py plan --hours 6       # or --runs wf_x
   ```

   It prints one JSON line per stage, in the order the stages started, and then a count of each action.

   | Action | Meaning |
   |---|---|
   | `reuse` | The stage finished, or its revived agent wrote a report that fits. `rerun` passes that result on. |
   | `revive` | Its agent died after real work. Steps 4 to 6 continue it. |
   | `start fresh` | Its agent never got past its first turn, or left no transcript. Step 7 starts it. |
   | `wait` | Its revived agent has not written its report, or wrote one that does not fit. The line names the file. |
   | `leave` | Its run is going, so the run's script still owns the stage. A run whose whole session died never ends: stage its agents with `--ids`. |
   | `ignore` | The prefix's probes of the API, `probe:try<N>`. They are not work. |

   Stages the run never reached have no line. Step 7 starts them.

3. **Make sure nothing else works in the same place.** A revived agent and a new agent in one worktree overwrite each other. If a resumed run is redoing a dead agent's stage, stop that run first.

4. **Stage the transcripts.**

   ```
   python3 ~/.claude/skills/revive-workflow-agents/wf_revive.py stage --all-dead      # or --ids a1,a2 --runs wf_x
   ```

   It copies `agent-<id>.jsonl` and `.meta.json` from the run folder into `<session>/subagents/`. For each agent it prints the report file, and the schema file it wrote beside it from the agent's own log. Without the copy, SendMessage answers "No transcript found".

5. **Continue each agent with `SendMessage(to=<agentId>)`.** The tool answers `Resuming agent <id>`. Send several in one turn; revive one first if the mechanism is in doubt. The message says, in this order:
   - What happened and when: the connection dropped at HH:MM and is back. "You are the same agent, continuing the same task with your whole conversation; nothing of your instructions has changed."
   - What to check first: `git status --short`, `git log --oneline -5`, the tail of its notes file. A command that was running when the connection dropped may have been cut off half way.
   - What changed while it was away, if it matters to this agent.
   - How to end: "Finish your task as your instructions say. You no longer have the StructuredOutput tool. Write your final result as one JSON object to `<the report file stage printed>`. It must fit the JSON Schema in `<the schema file stage printed>`, which is the schema that tool had: every required key, each with the type the schema gives. Make your last message at most six lines: done or not, what did not run, and that path." The revived copy's last message lands whole in the main session, so keep it short. For an agent whose result is text, `stage` prints no schema: its report file is that text.

   The schema matters. On 2026-11-21 the message said only "in the JSON shape your instructions asked for", and three of three reports checked afterwards had keys of the agent's own choosing. The next stage could not have read them.

   Say all of it in the first message. A later message to a working agent is queued for its next tool round and moves a tool call it is running to the background. Give each role its own wording: a builder should not merge the main line now, and a lander must fetch the main line again before it trusts anything fetched earlier.

6. **Confirm.**

   ```
   python3 ~/.claude/skills/revive-workflow-agents/wf_revive.py check --ids a1,a2,...
   ```

   Each revived agent shows turns since revival within two minutes, and later `report written`. `report does not fit: lacks ...` means the agent wrote a shape of its own: send it one message that names the schema file and the keys it lacks.

7. **Carry the runs on.** A revived agent is a subagent of the main session. Its run is over, so no later stage follows by itself.

   ```
   python3 ~/.claude/skills/revive-workflow-agents/wf_revive.py rerun --hours 6       # or --runs wf_x
   ```

   For each run that is over and has unfinished stages, it writes a copy of the run's script with a map in it, and prints `{"workflow": {"scriptPath": ..., "args": ...}}`. Call `Workflow` with that input. In the new run:
   - a stage that finished in the old run returns its old result and calls no agent, in any call order;
   - a stage that a revived agent finished returns that agent's report;
   - a stage that never started, and everything after, runs as usual.

   `rerun` holds a run back while one of its revived agents has not written its report, or wrote one that does not fit its schema, and names those stages. Run it again when their notifications arrive. With `--wait` it does not hold back: the new run starts now, and one agent for each such stage waits for the report file and returns it. Use that when a run has units that can go on while a slow builder finishes. Start a run once.

   This needs a script written as the next section says. For a script without the mark, `rerun` says so and writes nothing. Then start each unit's remaining stages by hand: for a revived builder a verifier, for a revived verifier that passed a lander, for one that failed a fix round. `wf_revive.py done` prints every finished result to work from.

## Make a script revivable

Paste this prefix after `export const meta = {...}` in every workflow script, and call `step` wherever the script would call `agent`. A script cannot read files, so the prefix cannot be imported. `test_wf_prefix.mjs` runs this block as it stands here.

```js
// ---- Standard workflow prefix: outage-safe and revivable (2026-11-21) ----
// Paste after `export const meta = {...}`. Call step() wherever the script would call agent().
// Keep the DONE line exactly as it is: `wf_revive.py rerun` replaces that one line.
const DONE = (args && args.done) || {} // wf-revive:done
const STOPPED = []          // { label, why } for every unit that stopped; return it with the results
const PROBE_WAIT_MIN = [2, 5, 10, 20, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30] // about 5.5 h, plus each probe's own retries
const sleep = (ms) => new Promise((done) => setTimeout(done, ms))
let gate = null             // while set, the API is thought to be down; resolves true when a probe answers
let apiGone = false         // set when the last probe failed: no new stage starts in this run
let probes = 0             // probe labels end in :tryN so wf_revive.py treats them as one stage, "probe"

function waitForApi(why) {
  if (gate) return gate
  log(`[outage] ${why}. New stages wait until a probe answers.`)
  gate = (async () => {
    for (const minutes of PROBE_WAIT_MIN) {
      const ok = await agent('Reply with the single word: ok', { label: `probe:try${++probes}`, model: 'haiku', effort: 'low' }).catch(() => null)
      if (ok !== null) return true
      await sleep(minutes * 60000)
    }
    return false
  })().then((up) => {
    gate = null
    apiGone = !up
    log(up ? `[outage] the API answers again (probe ${probes}).` : '[outage] still down after the last probe: remaining stages stop.')
    return up
  })
  return gate
}

function stop(label, why) {
  STOPPED.push({ label, why })
  log(`[stopped] ${label}: ${why}. Its unit goes no further; revive it with revive-workflow-agents.`)
  const e = new Error(`unit stopped at ${label}: ${why}`)
  e.unitStopped = true
  throw e
}

// One stage of one unit. Labels must be stable and unique: `build:<unit>`, `verify:<unit>:r2`.
// Returns the agent's result. Never returns null: a stage without a result throws, so no later stage runs.
// In pipeline() that drops the item and skips its remaining stages; in parallel() the thunk resolves to null;
// in plain sequential code wrap each unit in unit().
async function step(label, prompt, opts = {}) {
  const had = DONE[label]
  if (had !== undefined && !(had && had.$wait)) return had
  while (gate) await gate
  if (apiGone) stop(label, 'not started: the API did not come back')
  const call = had === undefined
    ? agent(prompt, { ...opts, label })
    : agent(`An agent outside this run is finishing the stage "${label}". Wait for the file ${had.$wait}: run \`until [ -s ${had.$wait} ]; do sleep 20; done; cat ${had.$wait}\` as one Bash call with a ten-minute limit, and again each time it times out. The file holds one JSON object, which is this stage's result. Return that same object as your result: the same keys with the same values. Do nothing else.`,
      { schema: opts.schema, phase: opts.phase, label: `${label}:wait` })
  let value = null
  let why = 'no result (an API error, an outage, or a skip)'
  try { value = await call } catch (e) {
    why = String((e && e.message) || e)
    if (!/^agent (stalled|lost its reply|abandoned)/.test(why)) throw e   // a script or spawn error: fail loudly
  }
  if (value === null || value === undefined) {
    waitForApi(`${label} returned no result`)
    stop(label, why)
  }
  return value
}

// For sequential code: `const a = await unit(async () => { ...one unit's step() calls... })`.
// A stop becomes null, so the other units go on.
async function unit(run) {
  try { return await run() } catch (e) { if (e && e.unitStopped) return null; throw e }
}
// End the script with `return finish({ ...results })`. If a probe is out, the run ends when the API answers,
// so the run's completion notice tells the main session that it can revive the stopped units now.
async function finish(result) {
  if (gate) await gate
  return { ...result, stopped: STOPPED }
}
// ---- end of prefix
```

**A stage with no result stops its unit.** It is never retried with a fresh agent, and nothing is passed on to the unit's next stage. Revive the stopped stage from its transcript with the steps above. A fresh agent gives up the dead agent's context, and once the dead agent is revived, both work in one worktree. On 2026-11-21 scripts that retried after waits of 1, 3 and 10 minutes put 210 fresh agents into the outage and passed empty results on to later stages.

- `step(label, prompt, opts)` runs one stage of one unit and returns its result. A stage in `DONE` returns its old result and calls no agent. A `{"$wait": <file>}` entry starts one agent that waits for that report. A stage that ends with no result goes into `stopped` and throws, so nothing after it in that unit runs. A stall (`agent stalled on all 6 attempts`) counts as no result. Any other throw is a script or spawn error, and `step` throws it again.
- `unit(run)` holds one unit's `step` calls in sequential code. A stop becomes `null` for that unit, and the other units go on. In `parallel()` and `pipeline()` the runtime does this itself: a stopped thunk or item becomes `null`, and a pipeline item's later stages are skipped. There the runtime also turns a script error into `null` and logs `parallel[<n>] failed: ...`. So a unit that is `null` and not in `stopped` failed for another reason: read the run's logs.
- `finish(result)` ends the script: `return finish({ ...results })`. It adds `stopped`. While a probe is out it waits, so the run ends when the API answers, and its completion notice starts a turn in the main session.
- **The probe gate.** The first stage with no result opens one gate for the whole run. Stages already running carry on. A new stage waits while one small probe agent at a time asks for one word, with waits of 2 to 30 minutes between probes, about 5.5 hours in all. When a probe answers, the waiting stages start. When the last probe fails, every stage that has not started stops with `not started: the API did not come back`.
- Keep the comment `// wf-revive:done` on the `DONE` line. `rerun` replaces that one line.
- The waiting agent runs on the session's model, on purpose. On 2026-11-22 the smallest model at low effort copied a one-key report wrongly three times out of three (the file's text inside a string field twice, the bare value once). The default model copied it right once out of once. Without `--wait` no model copies anything.
- Give every stage a label that is the same on each run and names one stage of one unit: `build:<unit>`, `verify:<unit>:r2`. Probes are labelled `probe:try<N>`. The tool reads a `:try<N>` ending as another attempt at the same stage, so it counts all probes as one stage, `probe`, and `plan` ignores it. Do not label a stage of your own `probe`. Give agents that run side by side for one stage labels of their own, and number every member, the first one too: `review:<unit>#1`, `review:<unit>#2`. The tool counts a `#<N>` that starts after an unnumbered agent of the same label failed as another attempt at that stage.
- A stage's result must be all the next stage needs. The new run has the old result, not the old agent.
- The map can also be passed as `args.done`, from `wf_revive.py done`.

## What it costs

- One full cache write of the agent's context for each revived agent. A workflow agent's cache lasts five minutes and is gone by the time anyone notices an outage.
- Each revived agent's last message lands in the main session. Thirty long ones are tens of thousands of tokens, which is why step 5 asks for six lines and a file.
- With `--wait`, one agent for each stage that is waited for: about 55,000 tokens of fixed prompt each.
- With the prefix, a probe that fails costs no tokens, and the one that answers costs one small agent.

## Make the next outage cheaper

- **Start every script with the prefix.** On 2026-11-21 waits of 1, 3 and 10 minutes gave up after 25 minutes of a 137-minute outage, and each script then went on to its next stage, which failed too. The prefix stops a unit at its first stage with no result, and holds new stages behind one probe for hours.
- **Agents keep a notes file** and write to it as they go, so the work survives even a damaged transcript.
- **Long units run as separate runs.** A run of one unit can also be resumed safely.

## Test

```
python3 ops/skills/revive-workflow-agents/test_wf_revive.py
node --test ops/skills/revive-workflow-agents/test_wf_prefix.mjs
```

The second runs the prefix from this file against a stand-in runtime. It needs Node and nothing else. The repository's Go test image has no Node, so to run it in Docker use the Node image: `docker run --rm --network none -v "${PWD}:/src:ro" -w /src node:26.13.1-bookworm-slim node --test ops/skills/revive-workflow-agents/test_wf_prefix.mjs`.

## Related

- The daemon's `--transcripts` capture is a fallback for a damaged agent log. This skill does not read it.
- A `SendMessage` to an agent that is still running starts a second copy of it. Check its state before sending.
