#!/usr/bin/env node
// Records docs/media/kitchen-failover.gif: the kitchen screen of a running kitchen system below, the cluster console
// above it. While customers' orders are placed and paid, the leader of the raftlog cluster is killed and
// started again; the kitchen screen goes on, and at the end the script checks that every order the service
// acknowledged to it can be read back. The frames are real screenshots of the two real pages, taken by headless
// Chromium-based browsers (Edge or Chrome) over the DevTools protocol; ffmpeg joins them into the GIF.
//
// The one thing in the kitchen screen that the page itself does not draw is the green line at the end: the script counts the
// orders that were acknowledged to it (placed and paid, HTTP 2xx), reads the orders back through the API as a
// manager, and prints what it found. The numbers are the script's, from the API's answers.
//
//   go build -o bin/ ./cmd/...                     # raftlogd, raftlogconsole, orderservice
//   ./bin/raftlogconsole -spawn 3 -listen 127.0.0.1:8090 -data-dir <empty dir>
//   ORDERSERVICE_DATABASE_URL=... ./bin/orderservice -addr 127.0.0.1:19091,127.0.0.1:19092,127.0.0.1:19093 \
//       -menu examples/kitchen/menu.json -promotions examples/kitchen/promotions.json
//   KITCHEN_PASSWORD=... node docs/media/record-kitchen.js docs/media/kitchen-failover.gif
//
// The accounts must exist (make them on the Users screen) and share one password, KITCHEN_PASSWORD:
//   storekeeper (default thu-kho-1), customer (khach-1), cashier (thu-ngan-1), kitchen for the hot station (bep-nong),
//   manager (quan-ly-1); override with KITCHEN_STOREKEEPER, _CUSTOMER, _CASHIER, _KITCHEN, _MANAGER.
// The menu is the one in examples/kitchen. Needs Node 22 or later, ffmpeg on the PATH (or FFMPEG=...), and a browser
// (BROWSER=...). PROBE=1 only signs in, places three orders and keeps two screenshots, to see the layout.
"use strict";
const { spawn, spawnSync } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const KITCHEN = process.env.KITCHEN_URL || "http://127.0.0.1:8100";
const CONSOLE = process.env.CONSOLE_URL || "http://127.0.0.1:8090";
const out = process.argv[2] || path.join(__dirname, "kitchen-failover.gif");
const PASSWORD = process.env.KITCHEN_PASSWORD;
const WHO = {
  storekeeper: process.env.KITCHEN_STOREKEEPER || "thu-kho-1",
  customer: process.env.KITCHEN_CUSTOMER || "khach-1",
  cashier: process.env.KITCHEN_CASHIER || "thu-ngan-1",
  kitchen: process.env.KITCHEN_KITCHEN || "bep-nong",
  manager: process.env.KITCHEN_MANAGER || "quan-ly-1",
};
const W = 1000, KITCHEN_H = 560, CONSOLE_H = 330, FPS = 5, GIF_WIDTH = 880;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
if (!PASSWORD) { console.error("set KITCHEN_PASSWORD"); process.exit(1); }

function findBrowser() {
  const candidates = [
    process.env.BROWSER,
    "C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe",
    "C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe",
    "C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe",
    "/usr/bin/google-chrome", "/usr/bin/chromium", "/usr/bin/chromium-browser",
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
  ].filter(Boolean);
  const found = candidates.find((p) => fs.existsSync(p));
  if (!found) throw new Error("no browser found; set BROWSER to a Chrome or Edge executable");
  return found;
}

// ---- the API, as a person signed in
async function signIn(username) {
  const res = await fetch(KITCHEN + "/api/login", {
    method: "POST", headers: { "content-type": "application/json" },
    body: JSON.stringify({ username, password: PASSWORD }),
  });
  if (!res.ok) throw new Error("sign-in of " + username + " answered " + res.status);
  const cookie = res.headers.getSetCookie().map((c) => c.split(";")[0]).join("; ");
  return async (method, url, body) => {
    const r = await fetch(KITCHEN + url, {
      method, headers: { "content-type": "application/json", cookie },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(12000),
    });
    let json = null;
    try { json = await r.json(); } catch (e) { /* no body */ }
    return { status: r.status, json };
  };
}

let seq = 0;
const runID = Date.now().toString(36);
const cmd = (what) => "rec-" + runID + "-" + what + "-" + String(++seq).padStart(4, "0");

// A command is sent again, with the same ID, when the answer does not come: that is what the ID is for.
async function send(api, method, url, body) {
  let last = { status: 0, json: null };
  for (let attempt = 0; attempt < 6; attempt++) {
    try { last = await api(method, url, body); } catch (e) { last = { status: 0, json: null }; }
    if (last.status >= 200 && last.status < 300) return last;
    if (last.status >= 400 && last.status < 500) return last; // refused: sending again will not change it
    await sleep(1000);
  }
  return last;
}

async function main() {
  const storekeeper = await signIn(WHO.storekeeper);
  const customer = await signIn(WHO.customer);
  const cashier = await signIn(WHO.cashier);
  const kitchen = await signIn(WHO.kitchen);
  const manager = await signIn(WHO.manager);

  // goods come in, so that the menu is not sold out
  for (const ing of ["coffee-beans", "condensed-milk", "ice", "tea-leaves", "peach-syrup", "baguette", "pate", "cucumber", "egg"]) {
    const r = await send(storekeeper, "POST", "/api/stock/receive", { command_id: cmd("recv-" + ing), ingredient: ing, qty: 50000 });
    if (r.status !== 201 && r.status !== 200) throw new Error("receiving " + ing + " answered " + r.status + " " + JSON.stringify(r.json));
  }
  const menu = (await customer("GET", "/api/menu")).json;
  const pick = (it) => {
    const options = {};
    for (const g of it.groups || []) if (g.min >= 1) options[g.id] = [g.options[0].id];
    return { item: it.id, quantity: 1, options };
  };
  const hot = menu.items.filter((i) => i.station === "hot" && !i.sold_out);
  const drinks = menu.items.filter((i) => i.station === "drinks" && !i.sold_out);
  if (!hot.length) throw new Error("no hot item on the menu is on sale");

  const dir = process.env.RECORD_DIR || fs.mkdtempSync(path.join(os.tmpdir(), "raftlog-kitchen-gif-"));
  fs.mkdirSync(dir, { recursive: true });
  const framesL = path.join(dir, "L"), framesR = path.join(dir, "R");
  fs.mkdirSync(framesL, { recursive: true });
  fs.mkdirSync(framesR, { recursive: true });

  async function openPage(url, height) {
    const port = 9300 + Math.floor(Math.random() * 500);
    const proc = spawn(findBrowser(), [
      "--headless=new", "--disable-gpu", "--hide-scrollbars", "--no-first-run", "--no-default-browser-check",
      "--remote-debugging-port=" + port, "--user-data-dir=" + path.join(dir, "profile-" + port),
      "--window-size=" + W + "," + height, "about:blank",
    ], { stdio: "ignore" });
    let target;
    for (let i = 0; i < 100 && !target; i++) {
      try {
        const list = await (await fetch("http://127.0.0.1:" + port + "/json")).json();
        target = list.find((t) => t.type === "page");
      } catch (e) { /* not up yet */ }
      if (!target) await sleep(100);
    }
    if (!target) throw new Error("the browser did not start");
    const ws = new WebSocket(target.webSocketDebuggerUrl);
    await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
    let nextId = 0;
    const pending = new Map();
    ws.onmessage = (m) => {
      const d = JSON.parse(m.data);
      if (d.id && pending.has(d.id)) { pending.get(d.id)(d); pending.delete(d.id); }
    };
    const cdp = (method, params) => new Promise((res, rej) => {
      const id = ++nextId;
      pending.set(id, (d) => (d.error ? rej(new Error(method + ": " + d.error.message)) : res(d.result)));
      ws.send(JSON.stringify({ id, method, params }));
    });
    const js = async (expr) => {
      const r = await cdp("Runtime.evaluate", { expression: expr, returnByValue: true, awaitPromise: true });
      if (r.exceptionDetails) throw new Error(r.exceptionDetails.text + " in " + expr.slice(0, 80));
      return r.result.value;
    };
    await cdp("Page.enable");
    await cdp("Emulation.setDeviceMetricsOverride", { width: W, height: height, deviceScaleFactor: 1, mobile: false });
    await cdp("Page.navigate", { url });
    return { cdp, js, close: () => { try { ws.close(); } catch (e) { /* closed */ } proc.kill(); } };
  }

  const left = await openPage(KITCHEN + "/", KITCHEN_H);
  const right = await openPage(CONSOLE, CONSOLE_H);
  try {
    await sleep(1500);
    // the kitchen person signs in on the page, then the page is loaded again as that person
    await left.js("fetch('/api/login',{method:'POST',headers:{'content-type':'application/json'},body:" +
      JSON.stringify(JSON.stringify({ username: WHO.kitchen, password: PASSWORD })) + "}).then(r=>r.status)");
    await left.cdp("Page.navigate", { url: KITCHEN + "/" });
    for (let i = 0; i < 100; i++) {
      if (await right.js("document.querySelectorAll('#nodes .node').length >= 3 && !!document.querySelector('.isleader')")) break;
      await sleep(200);
    }
    for (let i = 0; i < 50; i++) {                       // the Kitchen tab, once the page has drawn it
      const onKitchen = await left.js("(function(){var e=Array.from(document.querySelectorAll('a,button,[role=tab],nav *')).find(function(x){return x.textContent.trim()==='Kitchen'});if(e)e.click();return /station/i.test(document.body.innerText)})()");
      if (onKitchen) break;
      await sleep(200);
    }
    await sleep(1500);

    let n = 0, recording = true;
    const acked = [];          // orders acknowledged to the script: placed and paid
    let notAcked = 0, loading = false;
    const shoot = async (page, dirName) => {
      const shot = await page.cdp("Page.captureScreenshot", { format: "png" });
      fs.writeFileSync(path.join(dirName, "f" + String(n).padStart(5, "0") + ".png"), Buffer.from(shot.data, "base64"));
    };
    if (process.env.PROBE) {
      for (let i = 0; i < 3; i++) await placeAndPay(i);
      await sleep(2500);
      await shoot(left, framesL); await shoot(right, framesR);
      console.log("probe frames in", dir);
      return;
    }
    const grab = (async () => {
      while (recording) {
        const t0 = Date.now();
        await Promise.all([shoot(left, framesL), shoot(right, framesR)]);
        n++;
        await sleep(Math.max(0, 1000 / FPS - (Date.now() - t0)));
      }
    })();

    // ---- the orders: placed by a customer, paid at the till, the hot station starts and finishes its part
    async function placeAndPay(i) {
      const items = [pick(hot[i % hot.length])];
      if (drinks.length && i % 2 === 0) items.push(pick(drinks[i % drinks.length]));
      const placed = await send(customer, "POST", "/api/orders", { command_id: cmd("order"), items });
      if (placed.status !== 201) { notAcked++; return; }
      const id = placed.json.order.id, total = placed.json.order.total;
      const paid = await send(cashier, "POST", "/api/orders/" + id + "/pay", { command_id: cmd("pay"), method: "cash", tendered: Math.ceil(total / 10000) * 10000 });
      if (paid.status !== 200) { notAcked++; return; }
      acked.push(id);
      (async () => {                                   // the station works on it a little later
        await sleep(1800);
        await send(kitchen, "POST", "/api/orders/" + id + "/tickets/hot/start", { command_id: cmd("start") });
        await sleep(2500);
        await send(kitchen, "POST", "/api/orders/" + id + "/tickets/hot/finish", { command_id: cmd("finish") });
      })();
    }
    loading = true;
    const loader = (async () => {
      for (let i = 0; loading; i++) {
        await placeAndPay(i).catch(() => { notAcked++; });
        await sleep(650);
      }
    })();

    const click = (sel) => right.js("(function(){var e=document.querySelector(" + JSON.stringify(sel) + ");if(!e||e.disabled)return false;e.click();return true})()");
    const leaderID = () => right.js("(function(){var c=document.querySelector('.node.isleader .id');return c?c.textContent.replace('Node ','').trim():''})()");

    await sleep(6000);                                   // a healthy cluster, orders flowing
    const victim = await leaderID();
    console.log("killing node", victim);
    await click('button[data-act="kill"][data-id="' + victim + '"]');
    await sleep(9000);                                   // an election; the orders go on
    console.log("new leader: node", await leaderID());
    await click('button[data-act="start"][data-id="' + victim + '"]');
    await sleep(6000);                                   // the node catches up
    loading = false;
    await loader;
    await sleep(4500);                                   // the last tickets are started and finished

    // ---- read the orders back as a manager and say what was found
    const back = await send(manager, "GET", "/api/orders?limit=5000");
    const found = new Set(((back.json && back.json.orders) || []).map((o) => o.id));
    const missing = acked.filter((id) => !found.has(id));
    const line = acked.length + " orders were acknowledged to this script (placed and paid); " +
      (acked.length - missing.length) + " are read back from the service; " + missing.length + " missing";
    console.log(line, "(not acknowledged:", notAcked + ")");
    await left.js("(function(){var d=document.createElement('div');d.textContent=" + JSON.stringify(line) +
      ";d.style.cssText='position:fixed;left:0;right:0;bottom:0;z-index:99999;padding:12px 16px;font:600 14px system-ui,sans-serif;" +
      "background:" + (missing.length ? "#fde2e4" : "#d9f2e3") + ";color:" + (missing.length ? "#7a1020" : "#0b4d2a") + ";text-align:center';" +
      "document.body.appendChild(d)})()");
    await sleep(4500);
    recording = false;
    await grab;

    const ffmpeg = process.env.FFMPEG || "ffmpeg";
    const filter = "[0:v][1:v]vstack=inputs=2,scale=" + GIF_WIDTH + ":-1:flags=lanczos,split[a][b];" +
      "[a]palettegen=max_colors=96:stats_mode=diff[p];[b][p]paletteuse=dither=bayer:bayer_scale=5:diff_mode=rectangle";
    const r = spawnSync(ffmpeg, ["-y", "-loglevel", "error",
      "-framerate", String(FPS), "-i", path.join(framesR, "f%05d.png"),
      "-framerate", String(FPS), "-i", path.join(framesL, "f%05d.png"),
      "-filter_complex", filter, "-loop", "0", out], { stdio: "inherit" });
    if (r.status !== 0) throw new Error("ffmpeg failed");
    console.log("wrote", out, n + " frames,", (fs.statSync(out).size / 1024 / 1024).toFixed(2) + " MB");
  } finally {
    left.close(); right.close();
    await sleep(500);
    if (!process.env.RECORD_DIR && !process.env.PROBE) { try { fs.rmSync(dir, { recursive: true, force: true }); } catch (e) { /* in use */ } }
  }
}

main().catch((e) => { console.error(e); process.exit(1); });
