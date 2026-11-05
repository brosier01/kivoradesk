// Live view of the desktop. The gateway decides whether input reaches the
// desktop; viewOnly here only avoids sending input that would be dropped.
import RFB from "/novnc/core/rfb.js";

const ownerEl = document.getElementById("owner");
const linkEl = document.getElementById("link");
const requestEl = document.getElementById("request");
const errorEl = document.getElementById("error");
const keyboardEl = document.getElementById("keyboard");
const typingEl = document.getElementById("typing");
const textEl = document.getElementById("text");
const buttons = {
  human: document.getElementById("take"),
  paused: document.getElementById("pause"),
  agent: document.getElementById("give"),
};
const specialKeys = { enter: 0xff0d, erase: 0xff08 };

let rfb = null;
let live = false;
let owner = "";
let ended = false;
let ticker = null;
let errorTimer = null;

function connect() {
  if (ended) return;
  const scheme = location.protocol === "https:" ? "wss" : "ws";
  rfb = new RFB(document.getElementById("screen"), `${scheme}://${location.host}/websockify`, {
    wsProtocols: ["binary"],
  });
  rfb.scaleViewport = true;
  rfb.resizeSession = false;
  rfb.viewOnly = owner !== "human";
  rfb.addEventListener("connect", () => {
    live = true;
    linkEl.textContent = "live";
  });
  rfb.addEventListener("disconnect", async () => {
    live = false;
    if (ended) return;
    linkEl.textContent = "disconnected, retrying…";
    // The end of a session closes the connection too: ask before trying again.
    await refresh();
    if (!ended) setTimeout(connect, 2000);
  });
}

function showError(message) {
  errorEl.textContent = message;
  errorEl.hidden = !message;
}

// A failure of the moment goes away by itself. The end of a session stays.
function showFailure(message) {
  showError(message);
  clearTimeout(errorTimer);
  errorTimer = setTimeout(() => {
    if (!ended) showError("");
  }, 10000);
}

function setOwnerDisplay(name, shown) {
  owner = name;
  ownerEl.dataset.owner = name;
  ownerEl.textContent = shown;
  if (rfb) rfb.viewOnly = name !== "human";
  keyboardEl.hidden = name !== "human";
  if (name !== "human") typingEl.hidden = true;
}

function render(snapshot) {
  if (ended) return;
  setOwnerDisplay(snapshot.owner, snapshot.owner);
  for (const [o, b] of Object.entries(buttons)) b.disabled = o === owner;
  requestEl.hidden = !snapshot.request;
  requestEl.textContent = snapshot.request ? `The agent asks for help: ${snapshot.request}` : "";
  document.title = snapshot.request ? "(!) KivoraDesk" : "KivoraDesk";
}

// The owner cannot be known: showing the last one could be wrong.
function renderUnknown() {
  if (ended) return;
  setOwnerDisplay("", "unknown");
  for (const b of Object.values(buttons)) b.disabled = false;
}

function sessionEnded() {
  if (ended) return;
  renderUnknown();
  ended = true;
  clearInterval(ticker);
  clearTimeout(errorTimer);
  for (const b of Object.values(buttons)) b.disabled = true;
  linkEl.textContent = "signed out";
  showError("Your session has ended. To sign in again, run this on the server: kivora web-url");
  if (live) rfb.disconnect();
}

async function refresh() {
  if (ended) return;
  try {
    const res = await fetch("/api/control", { cache: "no-store" });
    if (res.status === 401) return sessionEnded();
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    render(await res.json());
  } catch {
    renderUnknown();
  }
}

async function setOwner(next) {
  try {
    const res = await fetch("/api/control", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ owner: next }),
    });
    if (res.status === 401) return sessionEnded();
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    render(await res.json());
    clearTimeout(errorTimer);
    showError("");
  } catch (e) {
    showFailure(`Control did not change (${e.message}). Check the SSH tunnel.`);
    await refresh();
  }
}

for (const [o, b] of Object.entries(buttons)) b.addEventListener("click", () => setOwner(o));

// Phones only open their keyboard for a text field, and the desktop is a canvas.
// This also pastes from the laptop. The keys take the viewer's way, through the
// gateway's filter. Loaded on use: the live view works without it.
async function sendText() {
  const text = textEl.value;
  textEl.value = "";
  const { default: keysyms } = await import("/novnc/core/input/keysymdef.js");
  for (const ch of text) rfb.sendKey(keysyms.lookup(ch.codePointAt(0)), null);
  textEl.focus();
}

keyboardEl.addEventListener("click", () => {
  typingEl.hidden = !typingEl.hidden;
  if (!typingEl.hidden) textEl.focus();
});
document.getElementById("send").addEventListener("click", sendText);
textEl.addEventListener("keydown", (e) => {
  if (e.key === "Enter") {
    e.preventDefault();
    sendText();
  }
});
for (const [id, key] of Object.entries(specialKeys)) {
  document.getElementById(id).addEventListener("click", () => rfb.sendKey(key, null));
}

await refresh();
connect();
ticker = setInterval(refresh, 1000);
