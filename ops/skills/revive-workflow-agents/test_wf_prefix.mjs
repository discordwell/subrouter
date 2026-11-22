// Tests for the standard workflow prefix, run as SKILL.md prints it, so the two cannot drift apart.
//
//   node --test ops/skills/revive-workflow-agents/test_wf_prefix.mjs
//
// The script runs in a vm context with the globals a workflow script gets: agent(), parallel(),
// pipeline(), log(), args, setTimeout and clearTimeout. parallel() and pipeline() turn a throw into
// null as the runtime does. The clock is virtual: it moves only when every promise is waiting on a
// timer, so hours of probe waits take milliseconds and the order of events is the same on every run.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import vm from 'node:vm'

const MINUTE = 60000
const MARK = '// wf-revive:done'
const NO_RESULT = 'no result (an API error, an outage, or a skip)'
const NOT_STARTED = 'not started: the API did not come back'

function prefixFromSkill() {
  const text = readFileSync(new URL('./SKILL.md', import.meta.url), 'utf8')
  const section = text.split(/^## /m).find((part) => part.startsWith('Make a script revivable\n'))
  assert.ok(section, 'SKILL.md has a "Make a script revivable" section')
  const block = section.match(/^```js\n([\s\S]*?)^```$/m)
  assert.ok(block, 'that section holds a js block')
  const marked = block[1].split('\n').filter((line) => line.includes(MARK))
  assert.equal(marked.length, 1, 'wf_revive.py rerun replaces exactly one marked line')
  assert.match(marked[0], /^const DONE = /)
  return block[1]
}

const PREFIX = prefixFromSkill()

// answer(call, minute) is what the agent returns when it ends, one virtual minute after it starts.
async function run(script, { args, answer, prefix = PREFIX }) {
  let now = 0
  let seq = 0
  const timers = []
  const calls = []
  const logs = []
  const setTimeout = (fn, ms = 0) => {
    timers.push({ at: now + Number(ms), seq: ++seq, fn })
    return seq
  }
  const clearTimeout = (id) => {
    const i = timers.findIndex((timer) => timer.seq === id)
    if (i >= 0) timers.splice(i, 1)
  }
  const log = (message) => logs.push(String(message))
  const agent = (prompt, opts) => {
    const call = { label: opts.label, prompt, opts, at: now / MINUTE }
    calls.push(call)
    return new Promise((resolve, reject) => setTimeout(() => {
      try { resolve(answer(call, now / MINUTE)) } catch (e) { reject(e) }
    }, MINUTE))
  }
  const parallel = (thunks) => Promise.all(thunks.map((thunk, i) => Promise.resolve().then(thunk)
    .catch((e) => { log(`parallel[${i}] failed: ${e.message}`); return null })))
  const pipeline = (items, ...stages) => Promise.all(items.map(async (item, i) => {
    try {
      let value = item
      for (const stage of stages) {
        if (value === null) break
        value = await stage(value, item, i)
      }
      return value
    } catch (e) {
      log(`pipeline[${i}] failed: ${e.message}`)
      return null
    }
  }))

  const context = vm.createContext({
    agent, parallel, pipeline, log, setTimeout, clearTimeout,
    args: args === undefined ? undefined : JSON.parse(JSON.stringify(args)),
  })
  vm.runInContext(`Date.now = () => { throw new Error('Date.now() is not available in a workflow script') }
Math.random = () => { throw new Error('Math.random() is not available in a workflow script') }`, context)
  const pending = vm.runInContext(`(async () => {\n${prefix}\n${script}\n})()`, context)

  let outcome = null
  pending.then((result) => { outcome = { result: JSON.parse(JSON.stringify(result)) } },
    (error) => { outcome = { error } })
  while (outcome === null) {
    await new Promise((resolve) => setImmediate(resolve))
    if (outcome !== null) break
    assert.ok(timers.length > 0, 'the script waits on nothing')
    timers.sort((a, b) => a.at - b.at || a.seq - b.seq)
    const timer = timers.shift()
    now = timer.at
    timer.fn()
  }
  return { ...outcome, calls, labels: calls.map((call) => call.label), logs, minutes: now / MINUTE }
}

const word = (prompt) => prompt.match(/word: (\S+)/)[1]

const TWO_UNITS = `
const a = await unit(async () => ({ s1: await step('s1:a', 'word: a1'), s2: await step('s2:a', 'word: a2') }))
const b = await unit(async () => ({ s1: await step('s1:b', 'word: b1'), s2: await step('s2:b', 'word: b2') }))
return finish({ a, b })`

test('a DONE map from an earlier run skips its finished stages and waits for a revived one', async () => {
  const done = { 's1:a': 'A1 from the old run', 's2:a': { $wait: '/reports/a2.json' } }
  const asRerunWritesIt = PREFIX.split('\n')
    .map((line) => line.includes(MARK) ? `const DONE = ${JSON.stringify(done)} ${MARK} (from wf_old)` : line)
    .join('\n')
  for (const [how, options] of [['args.done', { args: { done } }], ['the line rerun writes', { prefix: asRerunWritesIt }]]) {
    const ran = await run(TWO_UNITS, {
      ...options,
      answer: (call) => call.label === 's2:a:wait' ? 'A2 from the report' : word(call.prompt),
    })
    assert.equal(ran.error, undefined, how)
    assert.deepEqual(ran.labels, ['s2:a:wait', 's1:b', 's2:b'], `${how}: no agent for a finished stage`)
    assert.match(ran.calls[0].prompt, /\/reports\/a2\.json/, `${how}: the waiting agent reads the report file`)
    assert.deepEqual(ran.result, {
      a: { s1: 'A1 from the old run', s2: 'A2 from the report' },
      b: { s1: 'b1', s2: 'b2' },
      stopped: [],
    }, how)
  }
})

test('an API that never comes back stops the waiting units after the last probe', async () => {
  const ran = await run(TWO_UNITS, {
    answer: (call) => call.label === 's1:a' || call.label.startsWith('probe:') ? null : word(call.prompt),
  })
  assert.equal(ran.error, undefined)
  assert.deepEqual(ran.result, {
    a: null,
    b: null,
    stopped: [{ label: 's1:a', why: NO_RESULT }, { label: 's1:b', why: NOT_STARTED }],
  })
  const probes = ran.labels.slice(1)
  assert.equal(ran.labels[0], 's1:a')
  assert.ok(probes.length > 1 && probes.every((label, i) => label === `probe:try${i + 1}`),
    `only probes after the dead stage, one label each: ${ran.labels}`)
  assert.ok(!ran.labels.includes('s2:a'), 'the dead unit goes no further')
  assert.ok(!ran.labels.includes('s1:b'), 'no new stage starts into the outage')
  const starts = ran.calls.slice(1).map((call) => call.at)
  assert.ok(starts.every((at, i) => i === 0 || at - starts[i - 1] > 1), 'one probe at a time, with a wait between')
  assert.ok(ran.minutes > 5 * 60, `the waiting unit held on for hours, not ${ran.minutes} minutes`)
  assert.match(ran.logs.at(-1), /^\[stopped\] s1:b: not started/, 'the waiting unit stops after the last probe')
})

test('a stall that throws stops only its unit', async () => {
  const stalled = 'agent stalled on all 6 attempts (no progress for 600000ms each)'
  const ran = await run(`
const [a, b] = await parallel(['a', 'b'].map((u) => async () => ({
  s1: await step('s1:' + u, 'word: ' + u + '1'),
  s2: await step('s2:' + u, 'word: ' + u + '2'),
})))
return finish({ a, b })`, {
    answer: (call) => {
      if (call.label === 's1:a') throw new Error(stalled)
      return word(call.prompt)
    },
  })
  assert.equal(ran.error, undefined)
  assert.deepEqual(ran.result, { a: null, b: { s1: 'b1', s2: 'b2' }, stopped: [{ label: 's1:a', why: stalled }] })
  assert.deepEqual(ran.labels, ['s1:a', 's1:b', 'probe:try1', 's2:b'])
  const at = Object.fromEntries(ran.calls.map((call) => [call.label, call.at]))
  assert.ok(at['s2:b'] > at['probe:try1'], 'the other unit goes on once the probe answers')
})

test('when every unit has stopped, the run ends when a probe answers', async () => {
  const back = 60
  const ran = await run(`
const a = await unit(async () => ({ s1: await step('s1:a', 'word: a1') }))
return finish({ a })`, {
    answer: (call, minute) => call.label.startsWith('probe:') && minute >= back ? 'ok' : null,
  })
  assert.equal(ran.error, undefined)
  assert.deepEqual(ran.result, { a: null, stopped: [{ label: 's1:a', why: NO_RESULT }] })
  assert.ok(ran.minutes >= back, `the run ended at minute ${ran.minutes}, before the API came back`)
  assert.match(ran.logs.at(-1), /the API answers again/)
})

test('a script error is thrown, not taken for an outage', async () => {
  const ran = await run(TWO_UNITS, {
    answer: (call) => {
      if (call.label === 's1:a') throw new Error('agent({schema}) received an invalid JSON Schema: x')
      return word(call.prompt)
    },
  })
  assert.equal(ran.result, undefined)
  assert.equal(ran.error.message, 'agent({schema}) received an invalid JSON Schema: x')
  assert.deepEqual(ran.labels, ['s1:a'], 'no probe and no later stage')
})

test('in pipeline() a dead item is dropped and the other finishes after a probe answers', async () => {
  const back = 60
  const ran = await run(`
const r = await pipeline(['a', 'b'],
  (_, u) => step('s1:' + u, 'word: ' + u + '1'),
  (prev, u) => step('s2:' + u, 'word: ' + u + '2-after-' + prev))
return finish({ r })`, {
    answer: (call, minute) => {
      if (call.label === 's1:a') return null
      if (call.label.startsWith('probe:')) return minute >= back ? 'ok' : null
      return word(call.prompt)
    },
  })
  assert.equal(ran.error, undefined)
  assert.deepEqual(ran.result, { r: [null, 'b2-after-b1'], stopped: [{ label: 's1:a', why: NO_RESULT }] })
  assert.ok(!ran.labels.includes('s2:a'), 'the dead item gets no later stage')
  const at = Object.fromEntries(ran.calls.map((call) => [call.label, call.at]))
  const probes = ran.calls.filter((call) => call.label.startsWith('probe:'))
  assert.ok(probes.length > 1, 'the API was down for more than one probe')
  assert.ok(at['s2:b'] >= back && at['s2:b'] > probes.at(-1).at,
    `s2:b started at minute ${at['s2:b']}, after the probe that answered`)
})
