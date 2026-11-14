// The call board: a screen for the customers who are waiting. It shows the numbers of the orders that
// are ready, large, and of those that are being made, and nothing else: no names, no items. It reads
// the board, then listens for changes and reads it again, like the other screens. Everything shown
// is put on the page as text.
(function (root, factory) {
  if (typeof module === "object" && module.exports) module.exports = factory(null);
  else factory(root);
})(typeof self !== "undefined" ? self : this, function (win) {
  "use strict";

  // newlyReady lists the numbers that are ready now and were not before. The first reading
  // (before is null) announces nothing: a screen that has just been switched on is not told
  // that every waiting order has just become ready.
  function newlyReady(before, now) {
    if (!before) return [];
    var had = {};
    before.forEach(function (e) { had[e.number] = true; });
    return now.filter(function (e) { return !had[e.number]; }).map(function (e) { return e.number; });
  }

  // minutesSince is how many whole minutes ago a moment was, never below zero.
  function minutesSince(iso, nowMs) {
    var t = Date.parse(iso);
    if (isNaN(t)) return 0;
    return Math.max(0, Math.floor((nowMs - t) / 60000));
  }

  // isStale says that the board was not read for too long to be trusted: the screen then says so.
  function isStale(lastOkMs, nowMs, limitMs) { return !lastOkMs || nowMs - lastOkMs > limitMs; }

  var pure = { newlyReady: newlyReady, minutesSince: minutesSince, isStale: isStale };
  if (!win) return pure;

  var I18N = win.KitchenI18n, T = I18N.t;
  var $ = function (id) { return win.document.getElementById(id); };
  function clear(el) { while (el.firstChild) el.removeChild(el.firstChild); return el; }
  function el(tag, cls, text) {
    var e = win.document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  }

  var LANG_KEY = "kitchen.lang", SOUND_KEY = "kitchen.board.sound";
  function get(k) { try { return win.localStorage.getItem(k); } catch (e) { return null; } }
  function put(k, v) { try { win.localStorage.setItem(k, v); } catch (e) { /* kept until the page closes */ } }

  var state = { board: null, ready: null, lastOk: 0, problem: "", fresh: {}, sound: get(SOUND_KEY) === "1", audio: null };
  var STALE_MS = 60 * 1000, FRESH_MS = 12 * 1000;

  // ---- drawing ----------------------------------------------------------------------

  function tile(entry, kind) {
    var t = el("div", "board-tile " + kind);
    t.appendChild(el("b", "", String(entry.number)));
    if (kind === "ready") {
      var mins = minutesSince(entry.since, Date.now());
      if (mins >= 1) t.appendChild(el("small", "", T("{n} min", { n: mins })));
      if (state.fresh[entry.number] && Date.now() - state.fresh[entry.number] < FRESH_MS) t.classList.add("fresh");
    }
    return t;
  }

  function drawColumn(box, list, kind, empty) {
    clear(box);
    if (!list.length) { box.appendChild(el("p", "board-empty", empty)); return; }
    list.forEach(function (e) { box.appendChild(tile(e, kind)); });
  }

  function draw() {
    var b = state.board;
    if (b) {
      drawColumn($("board-ready"), b.ready, "ready", T("Nothing is ready yet."));
      drawColumn($("board-making"), b.preparing, "making", T("Nothing is being made right now."));
    }
    var live = $("board-live");
    var stale = isStale(state.lastOk, Date.now(), STALE_MS);
    live.className = "pill " + (state.problem ? "off" : stale ? "wait" : "live");
    live.textContent = state.problem || (stale ? T("reconnecting…") : T("live"));
    $("board-clock").textContent = new Date().toLocaleTimeString(I18N.locale(), { hour: "2-digit", minute: "2-digit" });
  }

  function applyLanguage() {
    win.document.documentElement.lang = I18N.lang();
    win.document.title = T("Call board");
    $("board-ready-title").textContent = T("Ready");
    $("board-making-title").textContent = T("Preparing");
    $("board-foot").textContent = T("Please collect your order at the counter when your number is ready.");
    $("board-full").textContent = T("Full screen");
    var s = $("board-sound");
    s.textContent = state.sound ? T("Sound on") : T("Sound off");
    s.setAttribute("aria-pressed", state.sound ? "true" : "false");
    $("board-lang").setAttribute("aria-label", T("Language"));
    [].forEach.call($("board-lang").querySelectorAll("button"), function (b) {
      b.setAttribute("aria-pressed", b.getAttribute("data-lang") === I18N.lang() ? "true" : "false");
    });
    draw();
  }

  // ---- the sound ---------------------------------------------------------------------

  // chime plays two short notes. A browser lets a page make sound only after the person has clicked
  // on it, which is why it is a switch and not on by itself.
  function chime() {
    if (!state.sound || !state.audio) return;
    var ctx = state.audio, now = ctx.currentTime;
    [[880, 0], [1175, 0.18]].forEach(function (n) {
      var o = ctx.createOscillator(), g = ctx.createGain();
      o.type = "sine"; o.frequency.value = n[0];
      g.gain.setValueAtTime(0.0001, now + n[1]);
      g.gain.exponentialRampToValueAtTime(0.25, now + n[1] + 0.02);
      g.gain.exponentialRampToValueAtTime(0.0001, now + n[1] + 0.35);
      o.connect(g); g.connect(ctx.destination);
      o.start(now + n[1]); o.stop(now + n[1] + 0.4);
    });
  }

  // ---- reading the board -------------------------------------------------------------

  var loading = false, again = false;
  function load() {
    if (loading) { again = true; return; }
    loading = true;
    win.fetch("/api/board", { headers: { Accept: "application/json" } }).then(function (r) {
      if (r.status === 401 || r.status === 403) throw new Error("closed");
      if (!r.ok) throw new Error("status " + r.status);
      return r.json();
    }).then(function (b) {
      var fresh = newlyReady(state.ready, b.ready);
      fresh.forEach(function (n) { state.fresh[n] = Date.now(); });
      if (fresh.length) chime();
      state.board = b; state.ready = b.ready; state.lastOk = Date.now(); state.problem = "";
      draw();
    }, function (e) {
      state.problem = e && e.message === "closed" ? T("Sign in on the main page, or have the board made public.") : T("The board cannot be reached. Trying again…");
      draw();
    }).then(function () {
      loading = false;
      if (again) { again = false; load(); }
    });
  }

  var timer = null;
  function soon() { win.clearTimeout(timer); timer = win.setTimeout(load, 150); }

  function listen() {
    if (!win.EventSource) return; // the reading every 20 seconds is enough
    var es = new win.EventSource("/api/board/stream");
    es.addEventListener("board", soon);
    es.addEventListener("open", load); // after a break, read what was missed
    es.addEventListener("reset", function () { es.close(); win.setTimeout(listen, 2000); });
  }

  // ---- the controls ------------------------------------------------------------------

  [].forEach.call($("board-lang").querySelectorAll("button"), function (b) {
    b.addEventListener("click", function () {
      I18N.setLang(b.getAttribute("data-lang"));
      put(LANG_KEY, I18N.lang());
      applyLanguage();
    });
  });
  $("board-sound").addEventListener("click", function () {
    state.sound = !state.sound;
    put(SOUND_KEY, state.sound ? "1" : "0");
    if (state.sound) {
      var AC = win.AudioContext || win.webkitAudioContext;
      if (AC && !state.audio) state.audio = new AC();
      if (state.audio && state.audio.resume) state.audio.resume();
      chime();
    }
    applyLanguage();
  });
  $("board-full").addEventListener("click", function () {
    var d = win.document;
    if (d.fullscreenElement) { d.exitFullscreen(); return; }
    if (d.documentElement.requestFullscreen) d.documentElement.requestFullscreen();
  });

  var saved = get(LANG_KEY);
  I18N.setLang(I18N.choose(saved, win.navigator && win.navigator.language));
  applyLanguage();
  load();
  listen();
  win.setInterval(load, 20000); // the minutes on the tiles move, and a missed event is made good
  win.setInterval(draw, 15000);
  return pure;
});
