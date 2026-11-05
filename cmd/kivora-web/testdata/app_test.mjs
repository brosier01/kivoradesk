// Runs the real app.js with a fake page and a fake noVNC, to check what the human
// is shown when things go wrong. Run by TestViewerPage.
import { readFileSync } from "node:fs";
import assert from "node:assert/strict";

const source = readFileSync(process.argv[2], "utf8");

function element() {
  const listeners = {};
  return {
    dataset: {}, textContent: "", disabled: false, hidden: true, value: "", focused: false,
    addEventListener(type, fn) { listeners[type] = fn; },
    focus() { this.focused = true; },
    click() { return listeners.click?.(); },
    fire(type, event) { return listeners[type]?.(event); },
  };
}
const elements = {};
globalThis.document = { title: "", getElementById: (id) => (elements[id] ??= element()) };
globalThis.location = { protocol: "http:", host: "kivora.localhost:6080" };

const viewers = [];
class RFB {
  constructor() { viewers.push(this); this.listeners = {}; this.keys = []; this.disconnected = false; }
  addEventListener(type, fn) { this.listeners[type] = fn; }
  sendKey(key, code) { this.keys.push(key); }
  disconnect() { this.disconnected = true; }
}
globalThis.__RFB = RFB;
globalThis.__keysyms = { default: { lookup: (c) => c } };

// What the gateway answers.
let mode = "ok";
let owner = "agent";
let request = "";
const posts = [];
globalThis.fetch = async (path, options = {}) => {
  if (options.method === "POST") posts.push(JSON.parse(options.body).owner);
  if (mode === "network") throw new TypeError("Failed to fetch");
  if (mode === "401") return { ok: false, status: 401, json: async () => ({}) };
  if (mode === "500") return { ok: false, status: 500, json: async () => ({}) };
  if (options.method === "POST") owner = JSON.parse(options.body).owner;
  return { ok: true, status: 200, json: async () => ({ owner, seq: 1, request }) };
};

// Timers run by hand.
let timers = [];
globalThis.setTimeout = (fn, ms) => { const t = { fn, ms }; timers.push(t); return t; };
globalThis.clearTimeout = (t) => { timers = timers.filter((x) => x !== t); };
let ticker = null;
globalThis.setInterval = (fn) => { ticker = fn; return fn; };
globalThis.clearInterval = (fn) => { if (ticker === fn) ticker = null; };
const reconnects = () => timers.filter((t) => t.ms === 2000);

const unhandled = [];
process.on("unhandledRejection", (e) => unhandled.push(String(e)));

const module = source
  .replace(/^import RFB from .*$/m, "const RFB = globalThis.__RFB;")
  .replace(/import\("\/novnc\/core\/input\/keysymdef\.js"\)/, "Promise.resolve(globalThis.__keysyms)");
let loads = 0;
// Each load is a page of its own.
async function load() {
  for (const id of Object.keys(elements)) delete elements[id];
  viewers.length = 0;
  timers = [];
  loads++;
  await import("data:text/javascript;base64," + Buffer.from(`${module}\n// ${loads}`).toString("base64"));
}
await load();
const settle = () => new Promise((r) => setImmediate(r));
const pill = () => elements.owner.textContent;

// Signed in, the agent owns the desktop.
viewers[0].listeners.connect();
assert.equal(pill(), "agent");
assert.equal(elements.link.textContent, "live");
assert.equal(viewers[0].viewOnly, true);
assert.equal(elements.give.disabled, true);
assert.equal(elements.keyboard.hidden, true, "no keyboard while the human has no control");

// A long request is set as text, never as HTML.
assert.ok(!source.includes("innerHTML"));
request = "<img src=x onerror=alert(1)>";
await ticker(); await settle();
assert.equal(elements.request.textContent, "The agent asks for help: <img src=x onerror=alert(1)>");
assert.equal(elements.request.hidden, false);
request = "";
await ticker(); await settle();
assert.equal(elements.request.hidden, true);

// The human takes control, and types through the field.
elements.take.click(); await settle();
assert.equal(pill(), "human");
assert.equal(viewers[0].viewOnly, false);
assert.equal(elements.keyboard.hidden, false);
elements.keyboard.click();
assert.equal(elements.typing.hidden, false);
assert.equal(elements.text.focused, true);
elements.text.value = "aé€";
elements.send.click(); await settle();
assert.deepEqual(viewers[0].keys, [0x61, 0xe9, 0x20ac]);
assert.equal(elements.text.value, "");
elements.enter.click();
elements.erase.click();
assert.deepEqual(viewers[0].keys.slice(3), [0xff0d, 0xff08]);
elements.text.value = "z";
let prevented = false;
elements.text.fire("keydown", { key: "Enter", preventDefault() { prevented = true; } }); await settle();
assert.ok(prevented);
assert.equal(viewers[0].keys.at(-1), 0x7a);

// Control moves away: the field goes away.
owner = "paused";
await ticker(); await settle();
assert.equal(elements.keyboard.hidden, true);
assert.equal(elements.typing.hidden, true);
assert.equal(viewers[0].viewOnly, true);

// The tunnel falls: the owner is not known any more, and the failure is said.
mode = "network";
await ticker(); await settle();
assert.equal(pill(), "unknown");
assert.equal(elements.owner.dataset.owner, "");
assert.equal(viewers[0].viewOnly, true);
assert.equal(elements.pause.disabled, false, "the human can still try");
elements.pause.click(); await settle();
assert.match(elements.error.textContent, /Control did not change/);
assert.equal(elements.error.hidden, false);
// It reconnects while the session may still be good.
viewers[0].listeners.disconnect(); await settle();
assert.equal(elements.link.textContent, "disconnected, retrying…");
assert.equal(reconnects().length, 1);
reconnects()[0].fn();
timers = timers.filter((t) => t.ms !== 2000);
assert.equal(viewers.length, 2);
viewers[1].listeners.connect();

// A server error on a click is said too, and the message goes away by itself.
mode = "500";
elements.pause.click(); await settle();
assert.match(elements.error.textContent, /HTTP 500/);
assert.equal(pill(), "unknown");
mode = "ok";
await ticker(); await settle();
assert.equal(pill(), "paused");
timers.filter((t) => t.ms === 10000).forEach((t) => t.fn());
assert.equal(elements.error.hidden, true);

// The session ends: the human is told what to do, and nothing retries.
mode = "401";
elements.pause.disabled = false;
elements.pause.click(); await settle();
assert.equal(posts.at(-1), "paused");
assert.match(elements.error.textContent, /session has ended.*kivora web-url/);
assert.equal(elements.error.hidden, false);
assert.equal(pill(), "unknown");
assert.equal(elements.link.textContent, "signed out");
for (const b of ["take", "pause", "give"]) assert.equal(elements[b].disabled, true, b);
assert.equal(ticker, null, "stops asking");
assert.equal(viewers[1].disconnected, true, "closes the view");
viewers[1].listeners.disconnect(); await settle();
assert.equal(reconnects().length, 0, "does not reconnect");
assert.equal(viewers.length, 2);
assert.equal(elements.link.textContent, "signed out");
timers.forEach((t) => t.fn());
assert.match(elements.error.textContent, /session has ended/, "the message stays");

// The gateway closes the view of a session that ended: the viewer finds out when
// it asks before reconnecting.
mode = "ok"; owner = "human";
await load();
viewers[0].listeners.connect();
mode = "401";
viewers[0].listeners.disconnect(); await settle();
assert.match(elements.error.textContent, /session has ended/);
assert.equal(elements.link.textContent, "signed out");
assert.equal(reconnects().length, 0);
assert.equal(viewers[0].disconnected, false);
assert.equal(viewers[0].viewOnly, true);

// The periodic question finds out too, and closes the view.
mode = "ok";
await load();
viewers[0].listeners.connect();
mode = "401";
await ticker(); await settle();
assert.match(elements.error.textContent, /session has ended/);
assert.equal(viewers[0].disconnected, true);

assert.deepEqual(unhandled, []);
console.log("ok");
