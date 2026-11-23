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
