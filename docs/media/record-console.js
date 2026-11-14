#!/usr/bin/env node
// Records docs/media/failover.gif: the cluster console while its leader is killed and brought back, with a writer
// running the whole time, ending with Verify. The frames are real screenshots of the real page, taken by a
// headless Chromium-based browser (Edge or Chrome) over the DevTools protocol; ffmpeg joins them into the GIF.
//
//   make build
//   ./bin/raftlogconsole -spawn 3 -listen 127.0.0.1:8090      # in one terminal
//   node docs/media/record-console.js http://127.0.0.1:8090 docs/media/failover.gif
//
// Needs Node 22 or later (it uses the built-in WebSocket and fetch), ffmpeg on the PATH (or FFMPEG=...), and a
// browser (BROWSER=... if it is not Edge or Chrome in the usual place).
"use strict";
const { spawn, spawnSync } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const url = process.argv[2] || "http://127.0.0.1:8090";
const out = process.argv[3] || path.join(__dirname, "failover.gif");
const WIDTH = 1100, HEIGHT = 820, FPS = 5, GIF_WIDTH = 880;

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

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function main() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "raftlog-gif-"));
  const frames = path.join(dir, "frames");
  fs.mkdirSync(frames);
  const port = 9300 + Math.floor(Math.random() * 500);
  const browser = spawn(findBrowser(), [
    "--headless=new", "--disable-gpu", "--hide-scrollbars", "--no-first-run", "--no-default-browser-check",
    "--remote-debugging-port=" + port, "--user-data-dir=" + path.join(dir, "profile"),
    "--window-size=" + WIDTH + "," + HEIGHT, "about:blank",
  ], { stdio: "ignore" });
  try {
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
      if (r.exceptionDetails) throw new Error(r.exceptionDetails.text + " in " + expr);
      return r.result.value;
    };

    await cdp("Page.enable");
    await cdp("Emulation.setDeviceMetricsOverride", { width: WIDTH, height: HEIGHT, deviceScaleFactor: 1, mobile: false });
    await cdp("Page.navigate", { url });
    for (let i = 0; i < 100; i++) {
      if (await js("document.querySelectorAll('#nodes .node').length >= 3 && !!document.querySelector('.isleader')")) break;
      await sleep(200);
    }

    // frames, at a steady rate, while the script below acts
    let n = 0, recording = true;
    const grab = (async () => {
      while (recording) {
        const t0 = Date.now();
        const shot = await cdp("Page.captureScreenshot", { format: "png" });
        fs.writeFileSync(path.join(frames, "f" + String(n++).padStart(5, "0") + ".png"), Buffer.from(shot.data, "base64"));
        await sleep(Math.max(0, 1000 / FPS - (Date.now() - t0)));
      }
    })();

    const click = (sel) => js("(function(){var e=document.querySelector(" + JSON.stringify(sel) + ");if(!e||e.disabled)return false;e.click();return true})()");
    const leaderID = () => js("(function(){var c=document.querySelector('.node.isleader .id');return c?c.textContent.replace('Node ',''):''})()");

    await sleep(2000);                                   // a healthy cluster
    await click("#startLoad");                           // writes at 50 a second
    await sleep(4000);
    const victim = await leaderID();
    console.log("killing node", victim);
    await click('button[data-act="kill"][data-id="' + victim + '"]');
    await sleep(8000);                                   // an election, and the writes go on
    console.log("new leader: node", await leaderID());
    await click('button[data-act="start"][data-id="' + victim + '"]');
    await sleep(5000);                                   // it catches up
    await click("#stopLoad");
    await sleep(1000);
    await click("#verify");
    for (let i = 0; i < 100; i++) {                      // the verdict
      if (await js("(function(){var v=document.getElementById('verdict');return !v.hidden && v.textContent.length>0})()")) break;
      await sleep(200);
    }
    console.log("verdict:", await js("document.getElementById('verdict').textContent"));
    await sleep(3500);
    recording = false;
    await grab;
    ws.close();

    const ffmpeg = process.env.FFMPEG || "ffmpeg";
    const filter = "fps=" + FPS + ",scale=" + GIF_WIDTH + ":-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=96:stats_mode=diff[p];[b][p]paletteuse=dither=bayer:bayer_scale=5:diff_mode=rectangle";
    const r = spawnSync(ffmpeg, ["-y", "-loglevel", "error", "-framerate", String(FPS), "-i", path.join(frames, "f%05d.png"), "-vf", filter, "-loop", "0", out], { stdio: "inherit" });
    if (r.status !== 0) throw new Error("ffmpeg failed");
    console.log("wrote", out, n + " frames,", (fs.statSync(out).size / 1024 / 1024).toFixed(2) + " MB");
  } finally {
    browser.kill();
    await sleep(500);
    try { fs.rmSync(dir, { recursive: true, force: true }); } catch (e) { /* the profile may still be in use */ }
  }
}

main().catch((e) => { console.error(e); process.exit(1); });
