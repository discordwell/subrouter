// Runs the pool board page's own script on one pool.json, as a browser does
// after its first poll, and prints what the page drew as JSON: {"board": the
// HTML of #board, "flash": the page's error line, empty when there is none}.
// TestTheBoardPageShowsTheDaemonsOwnStart drives it:
//
//   node pool_ui_render.mjs PAGE.html POOL.json
//
// The DOM is only what the script touches: elements by ID that hold HTML and
// text, and a div whose text reads back escaped, which the page's esc() uses.
import fs from "node:fs";
import vm from "node:vm";

const [page, pool] = process.argv.slice(2).map((path) => fs.readFileSync(path, "utf8"));
const script = page.match(/<script>([\s\S]*?)<\/script>/);
if (!script) {
  throw new Error("the page has no <script>");
}

function element() {
  let text = "";
  return {
    innerHTML: "",
    className: "",
    value: "",
    dataset: {},
    get textContent() {
      return text;
    },
    set textContent(value) {
      text = String(value);
      this.innerHTML = text.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
    },
    classList: { add() {}, remove() {}, toggle() {} },
    addEventListener() {},
    focus() {},
    blur() {},
  };
}

const byID = new Map();
const document = {
  hidden: false,
  getElementById(id) {
    if (!byID.has(id)) {
      byID.set(id, element());
    }
    return byID.get(id);
  },
  createElement: element,
  addEventListener() {},
  querySelectorAll() {
    return [];
  },
};

vm.runInContext(script[1], vm.createContext({
  document,
  window: {},
  console,
  setInterval() {},
  setTimeout() {},
  clearTimeout() {},
  fetch: async () => ({ ok: true, status: 200, json: async () => JSON.parse(pool) }),
}));
// The page's refresh() awaits the poll and its JSON, then draws; both settle
// before the next turn of the event loop.
await new Promise((resolve) => setImmediate(resolve));
process.stdout.write(JSON.stringify({
  board: document.getElementById("board").innerHTML,
  flash: document.getElementById("flash").textContent,
}));
