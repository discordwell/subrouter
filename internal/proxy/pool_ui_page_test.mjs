// Tests for the pool board page (pool_ui.html) as it draws a pool.json answer.
//
//   node --test internal/proxy/pool_ui_page_test.mjs
//
// The page's own script runs in a vm context with a stand-in document and a fetch that answers
// /_subrouter/pool.json, so a test reads the HTML the board draws from the JSON the daemon sends.
// It uses only Node's own modules. TestTheBoardPagePassesItsNodeTests runs it, so
// `deploy/docker/test.sh ./internal/proxy` does too: the Go test image has node.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import vm from 'node:vm'

const PAGE = readFileSync(new URL('./pool_ui.html', import.meta.url), 'utf8')

function pageScript() {
  const scripts = [...PAGE.matchAll(/<script>([\s\S]*?)<\/script>/g)]
  assert.equal(scripts.length, 1, 'pool_ui.html holds one inline script')
  return scripts[0][1]
}

const escapeText = (text) => text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')

class Element {
  html = ''
  text = ''
  className = ''
  value = ''
  dataset = {}
  classList = { toggle() {}, add() {}, remove() {} }
  get innerHTML() { return this.html }
  set innerHTML(value) { this.html = String(value) }
  get textContent() { return this.text }
  set textContent(value) {
    this.text = String(value)
    this.html = escapeText(this.text)
  }
  addEventListener() {}
  focus() {}
  blur() {}
}

// render loads the page with pool.json answering board, and returns the board's HTML and the
// flash line, which is where the page reports a failed read.
async function render(board) {
  const elements = new Map()
  const byId = (id) => {
    if (!elements.has(id)) elements.set(id, new Element())
    return elements.get(id)
  }
  const document = {
    hidden: false,
    getElementById: byId,
    createElement: () => new Element(),
    querySelectorAll: () => [],
    addEventListener() {},
  }
  const fetch = async (path) => {
    assert.equal(path, '/_subrouter/pool.json')
    const body = JSON.stringify(board)
    return { ok: true, status: 200, json: async () => JSON.parse(body), text: async () => body }
  }
  const context = vm.createContext({ document, fetch, setInterval: () => 0, setTimeout: () => 0, clearTimeout() {} })
  context.window = context
  vm.runInContext(pageScript(), context)
  await new Promise((resolve) => setImmediate(resolve))
  return { board: byId('board').innerHTML, flash: byId('flash').text }
}

function claudeRow(id, fields) {
  return {
    key: 'local/' + id, provider: 'claude', id, pools: ['local'], shared: false,
    auth_mode: 'oauth', auth_valid: true, measured: true, fresh: true,
    state: 'ok', state_note: 'eligible for new sessions', binding_window: 'weekly',
    short_left: 0.9, short_reset_s: 9000, weekly_left: 0.5, weekly_roll_s: 3 * 86400,
    pace: 2.5e-7, weekly_only: false, live: 0, live_known: true, pins: 0,
    rank: 1, reason: 'ranked', pinnable: true,
    ...fields,
  }
}

function boardOf(rows) {
  return {
    generated_at: '2026-11-22T20:00:00Z',
    local: { name: 'local', label: 'laptop', present: true, reachable: true, draining: false, active_proxy_requests: 0, pins: 0 },
    box: { name: 'box', present: false, reachable: false },
    rows,
    spill: [],
    rollup: { accounts: rows.length, instances: rows.length, shared: 0, claude: rows.length, codex: 0,
      ready_local: rows.length, total_local: rows.length, ready_box: 0, total_box: 0, orphaned_pins: 0 },
    sessions: [],
  }
}

// paceCell is the cell that shows the pace (2.5 on every row here) and its marks, in the row
// whose account is id.
function paceCell(html, id) {
  const row = html.split('<tr class="row ').find((part) => part.includes('>' + id + '</span>'))
  assert.ok(row, 'the board draws a row for ' + id)
  const cells = [...row.matchAll(/<td class="num">([\s\S]*?)<\/td>/g)].map((match) => match[1])
  return cells.find((cell) => cell.startsWith('2.5'))
}

const WEEK_HOLD_S = 18 * 86400
const HOLD = '<span class="hold">2x</span>'
const fableBadge = (n) => '<span class="freset">F' + n + '</span>'

// The rows carry what TestTheWeekHoldTravelsApartFromFableResets checks pool.json sends for the
// same three accounts: a week reset, two Fable resets, and a week reset with one Fable reset
// behind it.
test('the board marks Fable resets apart from the week hold (PATCHES §64)', async () => {
  const { board, flash } = await render(boardOf([
    claudeRow('a-week', { reset_hold: true, reset_hold_s: WEEK_HOLD_S }),
    claudeRow('b-fable', { fable_resets: 2 }),
    claudeRow('c-both', { reset_hold: true, reset_hold_s: WEEK_HOLD_S, fable_resets: 1 }),
    claudeRow('d-plain', {}),
  ]))
  assert.equal(flash, '', 'the page read pool.json')
  assert.equal(paceCell(board, 'a-week'), '2.5 ' + HOLD, 'a week holder shows the 2x alone')
  assert.equal(paceCell(board, 'b-fable'), '2.5 ' + fableBadge(2), 'a Fable-only account shows F2 and no 2x')
  assert.equal(paceCell(board, 'c-both'), '2.5 ' + HOLD + ' ' + fableBadge(1), 'a week holder with a Fable reset shows both')
  assert.equal(paceCell(board, 'd-plain'), '2.5', 'an account with no reset shows neither')
})

test('the legend explains the Fable reset mark (PATCHES §64)', () => {
  const legend = PAGE.match(/<div class="legend" id="legend">([\s\S]*?)<\/div>/)
  assert.ok(legend, 'the page has its legend')
  const entry = legend[1].split('\n').find((line) => line.includes('class="freset"'))
  assert.ok(entry, 'the legend has an entry for the Fable reset mark')
  assert.match(entry, /no 2x and no early burndown/, 'the entry says a Fable reset holds nothing')
})

const monthDay = (iso) => { const d = new Date(iso); return (d.getMonth() + 1) + '/' + d.getDate() }
const WEEK_ENDS = '2026-12-10T22:00:00Z'
const FABLE_ENDS = '2027-03-09T07:59:00Z'
const CREDIT_A = '2026-12-14T09:13:00Z'
const CREDIT_B = '2026-12-21T09:13:00Z'

function rowHTML(html, id) {
  const row = html.split('<tr class="row ').find((part) => part.includes('>' + id + '</span>'))
  assert.ok(row, 'the board draws a row for ' + id)
  return row.split('</tr>')[0]
}

// The rows carry resets as TestThePoolBoardCarriesEveryResetOfThisDaemonAndItsPeer checks pool.json
// sends them: each kind with its count and expiry, read, from the last good read, not read, or none
// given, and from a daemon that publishes no list, marked partial.
test('the board marks each kind of saved reset with its count and soonest expiry (PATCHES §91.2)', async () => {
  const codexRow = (id, fields) => claudeRow(id, { provider: 'codex', pace: undefined, ...fields })
  const { board, flash } = await render(boardOf([
    claudeRow('a-both', { reset_hold: true, reset_hold_s: WEEK_HOLD_S, fable_resets: 2, resets: [
      { kind: 'week', count: 1, expires_at: WEEK_ENDS, expiry: 'read' },
      { kind: 'fable', count: 2, expires_at: FABLE_ENDS, expiry: 'read' }] }),
    claudeRow('b-none', { resets: [] }),
    claudeRow('c-unknown', { resets: [{ kind: 'unknown', count: 1, expiry: 'none_given' }] }),
    claudeRow('d-old', { fable_resets: 1, resets_partial: true, resets: [
      { kind: 'week', count: 1, expires_at: WEEK_ENDS, expiry: 'read' },
      { kind: 'fable', count: 1, expiry: 'none_given' }] }),
    codexRow('e-credits', { resets: [
      { kind: 'codex_rate_limits', count: 1, expires_at: CREDIT_B, expiry: 'read' },
      { kind: 'codex_rate_limits', count: 1, expires_at: CREDIT_A, expiry: 'read' },
      { kind: 'codex_rate_limits', count: 1, expiry: 'not_read' }] }),
    codexRow('f-last-good', { resets: [{ kind: 'codex_rate_limits', count: 2, expires_at: CREDIT_A, expiry: 'last_good' }] }),
    codexRow('g-unread', { resets: [{ kind: 'codex_rate_limits', count: 3, expiry: 'not_read' }] }),
    codexRow('h-none', {}),
  ]))
  assert.equal(flash, '', 'the page read pool.json')
  const text = (id) => [...rowHTML(board, id).matchAll(/class="([wfcu]reset|rpart)"[^>]*>([^<]*)</g)].map((m) => m[1] + ':' + m[2])
  assert.deepEqual(text('a-both'), ['wreset:W1 ' + monthDay(WEEK_ENDS), 'freset:F2 ' + monthDay(FABLE_ENDS)], 'a week reset and Fable resets, each dated')
  assert.match(paceCell(board, 'a-both'), /^2\.5 <span class="hold">2x<\/span> <span class="wreset"/, 'the marks follow the 2x')
  assert.deepEqual(text('b-none'), [], 'a row that holds none is not marked')
  assert.deepEqual(text('c-unknown'), ['ureset:U1'], 'a kind the daemon does not know is U')
  assert.deepEqual(text('d-old'), ['wreset:W1 ' + monthDay(WEEK_ENDS), 'freset:F1', 'rpart:~'], 'a partial list is marked ~, the F badge not drawn twice')
  assert.deepEqual(text('e-credits'), ['creset:R3 ' + monthDay(CREDIT_A)], 'Codex credits: the count with the soonest date')
  assert.deepEqual(text('f-last-good'), ['creset:R2 ' + monthDay(CREDIT_A) + '*'], 'a date from the last good read is starred')
  assert.deepEqual(text('g-unread'), ['creset:R3 ?'], 'a count whose dates were not read says ?')
  assert.deepEqual(text('h-none'), [], 'a row with no list is not marked')
  assert.match(rowHTML(board, 'e-credits'), /title="1 codex_rate_limits ending 2026-12-21T09:13:00Z \(read\); 1 codex_rate_limits ending 2026-12-14T09:13:00Z \(read\); 1 codex_rate_limits \(not read\)"/,
    'the mark\'s title lists every entry')
})

// The rows carry the mark as TestPoolJSONMarksABlockFromAClaim checks pool.json sends it.
test('the board marks resets known from a claim, not a reading (PATCHES §102)', async () => {
  const { board, flash } = await render(boardOf([
    claudeRow('a-claimed', { resets_from_claim: true, resets: [{ kind: 'fable', count: 1, expires_at: FABLE_ENDS, expiry: 'read' }] }),
    claudeRow('b-used-last', { resets_from_claim: true, resets: [] }),
    claudeRow('c-read', { resets: [{ kind: 'fable', count: 1, expires_at: FABLE_ENDS, expiry: 'read' }] }),
  ]))
  assert.equal(flash, '', 'the page read pool.json')
  const text = (id) => [...rowHTML(board, id).matchAll(/class="(freset|rclaim)"[^>]*>([^<]*)</g)].map((m) => m[1] + ':' + m[2])
  assert.deepEqual(text('a-claimed'), ['freset:F1 ' + monthDay(FABLE_ENDS), 'rclaim:^'], 'the marks, then ^')
  assert.deepEqual(text('b-used-last'), ['rclaim:0^'], 'a claim that used the last reset reads 0^')
  assert.deepEqual(text('c-read'), ['freset:F1 ' + monthDay(FABLE_ENDS)], 'a reading is not marked')
  assert.match(PAGE, /<b class="rclaim">\^<\/b> known from the claim just made/, 'the legend says what ^ means')
})

// The rows carry choices as TestPoolJSONCarriesEachRowsResetChoice checks pool.json sends them:
// reset_choice from a daemon in real mode, would_choose from one in shadow mode.
test('the board marks an account chosen for a reset, with its meter, its order and why (PATCHES §95.3)', async () => {
  const codexRow = (id, fields) => claudeRow(id, { provider: 'codex', pace: undefined, ...fields })
  const why = 'the pool needs its saved resets on the Claude week (the 24-hour pressure is 1.20, at or above 1.0); a claim there adds 0.51 of a week'
  const { board, flash } = await render(boardOf([
    claudeRow('a-chosen', { reset_hold: true, reset_hold_s: WEEK_HOLD_S, resets: [{ kind: 'week', count: 1, expires_at: WEEK_ENDS, expiry: 'read' }],
      reset_choice: { meter: 'week', order: 1, why } }),
    claudeRow('b-would', { would_choose: { meter: 'week', order: 2, why: 'a <shadow> reason & more' } }),
    claudeRow('c-fable', { reset_choice: { meter: 'fable', order: 1, why: 'its Fable reset' } }),
    codexRow('d-codex', { resets: [{ kind: 'codex_rate_limits', count: 1, expires_at: CREDIT_A, expiry: 'read' }],
      reset_choice: { meter: 'codex_week', order: 1, why: 'Codex accounts are loaded one at a time' } }),
    claudeRow('e-none', {}),
  ]))
  assert.equal(flash, '', 'the page read pool.json')
  const marks = (id) => [...rowHTML(board, id).matchAll(/<span class="(chosen|wouldchoose)" title="([^"]*)">([^<]*)<\/span>/g)]
    .map((m) => ({ cls: m[1], title: m[2], text: m[3] }))
  assert.deepEqual(marks('a-chosen'), [{ cls: 'chosen', title: why, text: 'chosen week #1' }], 'a chosen account, its reason in the title')
  assert.match(paceCell(board, 'a-chosen'), /<span class="wreset"[^>]*>W1 [^<]*<\/span> <span class="chosen"/, 'the mark follows the reset marks')
  assert.deepEqual(marks('b-would'), [{ cls: 'wouldchoose', title: 'shadow mode, nothing ranks on it: a &lt;shadow&gt; reason &amp; more',
    text: 'would choose week #2' }], 'a would-be choice in shadow mode says so, escaped')
  assert.deepEqual(marks('c-fable').map((m) => m.text), ['chosen Fable #1'], 'a choice for the Fable meter')
  assert.deepEqual(marks('d-codex').map((m) => m.text), ['chosen week #1'], 'a Codex account chosen for its week')
  assert.match(rowHTML(board, 'd-codex'), /<span class="creset"[^>]*>R1 [^<]*<\/span> <span class="chosen"/, 'on a Codex row, after its credits')
  assert.deepEqual(marks('e-none'), [], 'an account no daemon chose is not marked')
})

test('the legend explains the reset choice marks (PATCHES §95.3)', () => {
  const legend = PAGE.match(/<div class="legend" id="legend">([\s\S]*?)<\/div>/)
  assert.ok(legend, 'the page has its legend')
  const entry = (cls) => legend[1].split('\n').find((line) => line.includes('class="' + cls + '"'))
  assert.match(entry('chosen') || '', /run to its limit.*new sessions go there first.*title says why/, 'chosen')
  assert.match(entry('wouldchoose') || '', /shadow mode.*nothing ranks on it/, 'would choose')
})

test('the legend explains every reset mark (PATCHES §91.2)', () => {
  const legend = PAGE.match(/<div class="legend" id="legend">([\s\S]*?)<\/div>/)
  assert.ok(legend, 'the page has its legend')
  const entry = (cls) => legend[1].split('\n').find((line) => line.includes('class="' + cls + '"'))
  assert.match(entry('wreset') || '', /unclaimed week reset/, 'W')
  assert.match(entry('creset') || '', /Codex reset credits.*not read.*last good read/, 'R, ? and *')
  assert.match(entry('ureset') || '', /never claimed automatically/, 'U')
  assert.match(entry('rpart') || '', /no full list/, '~')
})
