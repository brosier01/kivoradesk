// Live view of the desktop. The gateway decides whether input reaches the
// desktop; viewOnly here only avoids sending input that would be dropped.
import RFB from "/novnc/core/rfb.js";

const ownerEl = document.getElementById("owner");
const linkEl = document.getElementById("link");
const buttons = {
  human: document.getElementById("take"),
  paused: document.getElementById("pause"),
  agent: document.getElementById("give"),
};

let rfb = null;
let owner = "";

function connect() {
  const scheme = location.protocol === "https:" ? "wss" : "ws";
  rfb = new RFB(document.getElementById("screen"), `${scheme}://${location.host}/websockify`, {
    wsProtocols: ["binary"],
  });
  rfb.scaleViewport = true;
  rfb.resizeSession = false;
  rfb.viewOnly = owner !== "human";
  rfb.addEventListener("connect", () => { linkEl.textContent = "live"; });
  rfb.addEventListener("disconnect", () => {
    linkEl.textContent = "disconnected, retrying…";
    setTimeout(connect, 2000);
  });
}

function render(snapshot) {
  owner = snapshot.owner;
  ownerEl.dataset.owner = owner;
  ownerEl.textContent = owner;
  for (const [o, b] of Object.entries(buttons)) b.disabled = o === owner;
  if (rfb) rfb.viewOnly = owner !== "human";
}

async function refresh() {
  try {
    const res = await fetch("/api/control", { cache: "no-store" });
    if (res.ok) render(await res.json());
  } catch {
    // The link status already shows connection problems.
  }
}

async function setOwner(next) {
  const res = await fetch("/api/control", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ owner: next }),
  });
  if (res.ok) render(await res.json());
}

for (const [o, b] of Object.entries(buttons)) b.addEventListener("click", () => setOwner(o));

await refresh();
connect();
setInterval(refresh, 1000);
