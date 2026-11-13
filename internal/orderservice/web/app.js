// The screens of the kitchen system. Everything that is shown is put on the page with textContent
// and element creation, never as HTML, because a note that a customer typed is shown on the
// kitchen's screen. The page keeps the token in the tab (sessionStorage) and sends it in a header.
(function () {
  "use strict";
  var L = window.KitchenLogic;
  var I18N = window.KitchenI18n;
  var T = I18N.t;

  // ---- small helpers --------------------------------------------------------------------

  var $ = function (id) { return document.getElementById(id); };

  // h makes an element: h("div", {class: "x", onclick: fn}, child, "text", ...). A child that is
  // a string becomes a text node, an array is flattened, null and false are skipped.
  function h(tag, attrs) {
    var el = document.createElement(tag);
    Object.keys(attrs || {}).forEach(function (k) {
      var v = attrs[k];
      if (v === null || v === undefined || v === false) return;
      if (k.slice(0, 2) === "on" && typeof v === "function") el.addEventListener(k.slice(2), v);
      else if (k === "class") el.className = v;
      else if (k === "value") el.value = v;
      else if (v === true) el.setAttribute(k, "");
      else el.setAttribute(k, v);
    });
    for (var i = 2; i < arguments.length; i++) add(el, arguments[i]);
    return el;
  }
  function add(el, c) {
    if (c === null || c === undefined || c === false) return;
    if (Array.isArray(c)) c.forEach(function (x) { add(el, x); });
    else el.appendChild(typeof c === "object" ? c : document.createTextNode(String(c)));
  }
  function clear(el) { while (el.firstChild) el.removeChild(el.firstChild); return el; }

  // svg builds an inline drawing. Nothing is loaded from elsewhere, and what is drawn is not data.
  var SVGNS = "http://www.w3.org/2000/svg";
  function svg(paths, size) {
    var el = document.createElementNS(SVGNS, "svg");
    el.setAttribute("viewBox", "0 0 " + (size || 48) + " " + (size || 48));
    el.setAttribute("aria-hidden", "true");
    paths.forEach(function (d) {
      var p = document.createElementNS(SVGNS, "path");
      p.setAttribute("d", d);
      el.appendChild(p);
    });
    return el;
  }
  var ART = {
    drink: ["M10 14h24l-3 24a3 3 0 0 1-3 2H16a3 3 0 0 1-3-2z", "M34 18h3a4 4 0 0 1 0 8h-4", "M16 8c0 2 2 2 2 4M22 8c0 2 2 2 2 4"],
    food: ["M8 20c0-8 7-12 16-12s16 4 16 12v18a2 2 0 0 1-2 2H10a2 2 0 0 1-2-2z", "M8 24h32M16 32h16"],
    combo: ["M6 18h22l-2 20a3 3 0 0 1-3 2H11a3 3 0 0 1-3-2z", "M30 24c8-4 14 0 12 8s-8 8-12 6", "M12 10c0 2 2 2 2 4M18 10c0 2 2 2 2 4"],
  };
  // The icons of the screens, on a 24 by 24 grid, drawn with a line like the pictures of the items.
  var ICONS = {
    order: ["M3 4h2l2.4 10.2a1 1 0 0 0 1 .8h8.8a1 1 0 0 0 1-.8L20 8H6", "M9 17.8a1.2 1.2 0 1 0 0 2.4a1.2 1.2 0 1 0 0-2.4", "M17 17.8a1.2 1.2 0 1 0 0 2.4a1.2 1.2 0 1 0 0-2.4"],
    myorders: ["M7 3h10a1 1 0 0 1 1 1v16l-2-1.2-2 1.2-2-1.2-2 1.2-2-1.2L6 20V4a1 1 0 0 1 1-1z", "M9 8h6M9 12h6"],
    till: ["M3 7h18v10H3z", "M12 9.5a2.5 2.5 0 1 0 0 5a2.5 2.5 0 1 0 0-5", "M6 10v4M18 10v4"],
    kitchen: ["M12 3c1 3 4 4.5 4 8.5a4 4 0 0 1-8 0c0-1.5.5-2.5 1.5-3.5.5 1 1.5 2 2.5 2 0-3-.5-5 0-7z"],
    manager: ["M5 20V10M12 20V4M19 20v-7"],
    stock: ["M3 8l9-5 9 5v8l-9 5-9-5z", "M3 8l9 5 9-5", "M12 13v8"],
    users: ["M10 4.8a3.2 3.2 0 1 0 0 6.4a3.2 3.2 0 1 0 0-6.4", "M16 20v-1.5a3.5 3.5 0 0 0-3.5-3.5h-5A3.5 3.5 0 0 0 4 18.5V20", "M17 5a3 3 0 0 1 0 5.8M21 20v-1.5a3.5 3.5 0 0 0-2.5-3.3"],
    account: ["M12 4.5a3.5 3.5 0 1 0 0 7a3.5 3.5 0 1 0 0-7", "M5 20a7 7 0 0 1 14 0"],
  };
  function icon(id) {
    var el = svg(ICONS[id] || [], 24);
    el.setAttribute("class", "ico");
    return el;
  }

  // art is the picture of an item: its photo when the menu has one, else a drawing by kind. The
  // menu does not carry photos yet; the day it does, a card shows it without a change here.
  function art(item, kind) {
    if (item && item.image) return h("div", { class: "im photo" }, h("img", { src: item.image, alt: "", loading: "lazy" }));
    var k = kind || (item && item.station === "drinks" ? "drink" : "food");
    return h("div", { class: "im art-" + k }, svg(ART[k] || ART.food));
  }
  function money(n) { return L.formatMoney(n); }
  function randBytes(n) { var a = new Uint8Array(n); (window.crypto || window.msCrypto).getRandomValues(a); return a; }
  function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }

  function store(key, v) {
    try { if (v === null) sessionStorage.removeItem(key); else sessionStorage.setItem(key, v); } catch (e) { /* storage may be blocked */ }
  }
  function load(key) { try { return sessionStorage.getItem(key); } catch (e) { return null; } }

  // ---- state ----------------------------------------------------------------------------

  var S = {
    // mode is how people sign in here: "password" (an account and a session cookie) or "token"
    // (the tokens of a file). token is the bearer in token mode; in password mode it only marks
    // that someone is signed in, since the session itself is in a cookie.
    mode: "token", token: null, me: null, menu: null, stock: null,
    orders: new Map(), screens: [], screen: null, current: null,
    live: "off", stream: null,
    cart: [], draft: null, placed: null, codes: "", counterCustomer: "",
    ui: { payFor: null, reasonFor: null, refundFor: null, station: null, audit: null },
    pending: {}, // command IDs of actions that were sent and not yet answered, by action
  };

  // ---- motion ---------------------------------------------------------------------------

  // The lists are drawn again whenever something changes, so a card must not play its entrance
  // each time. motion remembers the state in which each card was last drawn and says what is
  // new: "enter" for a card that was not on the screen, "stamp" for one whose status changed,
  // and "" for the rest. Nothing moves for what is drawn when the screens first load.
  var shown = {}, settled = false;
  function motion(key, state) {
    var before = shown[key];
    shown[key] = state;
    if (!settled) return "";
    return before === undefined ? "enter" : before !== state ? "stamp" : "";
  }

  // ---- the service ----------------------------------------------------------------------

  function ApiError(status, body) {
    this.status = status;
    this.body = body || {};
    this.message = L.describeError(status, body);
  }

  // With accounts, the browser holds the session in a cookie that the page cannot read and sends
  // it by itself; with the tokens of a file, the token is sent as a bearer. quiet says that a 401
  // is an answer to this request (a wrong password), not the end of the session.
  function api(method, path, body, quiet) {
    var opts = { method: method, headers: {} };
    if (S.token && S.mode === "token") opts.headers.Authorization = "Bearer " + S.token;
    if (body !== undefined) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    return fetch(path, opts).then(function (r) {
      return r.text().then(function (text) {
        var data = null;
        try { data = text ? JSON.parse(text) : null; } catch (e) { data = null; }
        if (r.status === 401 && !quiet) { signOut(S.mode === "password" ? T("Your session has ended. Sign in again.") : T("Your token was not accepted. Sign in again.")); }
        if (!r.ok) throw new ApiError(r.status, data);
        return data;
      });
    });
  }

  // cmd gives the ID of a command. The same action keeps the same ID until it is answered, so
  // pressing a button again after a timeout does not do the thing twice.
  function cmd(key) {
    if (!S.pending[key]) S.pending[key] = L.newCommandID(key.split(":")[0], randBytes);
    return S.pending[key];
  }

  // run sends a command for an action. The button shows that it is busy. A definite refusal
  // ends the action; no answer, or a server that could not, keeps its ID for the next press.
  function run(key, btn, send) {
    if (btn && btn.disabled) return Promise.resolve(null);
    if (btn) btn.disabled = true;
    return send(cmd(key)).then(function (res) {
      delete S.pending[key];
      return res;
    }, function (err) {
      if (err && err.status >= 400 && err.status < 500) delete S.pending[key];
      toast(err && err.message ? err.message : T("The request did not get an answer. Press again to retry."), "bad");
      return null;
    }).then(function (res) {
      if (btn) btn.disabled = false;
      return res;
    });
  }

  function putOrder(o) {
    if (!o || !o.id) return;
    S.orders.set(o.id, o);
    changed();
  }

  // ---- toasts ---------------------------------------------------------------------------

  function toast(msg, kind) {
    var el = h("div", { class: "toast " + (kind || ""), role: kind === "bad" ? "alert" : "status" }, msg);
    $("toasts").appendChild(el);
    setTimeout(function () { if (el.parentNode) el.parentNode.removeChild(el); }, kind === "bad" ? 9000 : 4500);
  }

  // ---- loading --------------------------------------------------------------------------

  var isStaff = function () { return !!(S.me && S.me.permissions["order.view_all"]); };

  function loadMenu() { return api("GET", "/api/menu").then(function (m) { S.menu = m; changed(); }); }
  function loadStock() {
    if (!S.me || !S.me.permissions["stock.view"]) return Promise.resolve();
    return api("GET", "/api/stock").then(function (s) { S.stock = s; changed(); });
  }
  function loadOrders() {
    // An administrator manages accounts and has no orders to read.
    if (!S.me.permissions["order.view_all"] && !S.me.permissions["order.view_own"]) { S.orders = new Map(); return Promise.resolve(); }
    var path = isStaff() ? "/api/orders?status=placed,paid,preparing,ready&limit=500" : "/api/orders?limit=200";
    var jobs = [api("GET", path)];
    if (S.me.permissions["order.approve_cancel"] || S.me.permissions["order.issue_refund"]) {
      jobs.push(api("GET", "/api/orders?status=completed,cancelled&limit=300"));
    }
    return Promise.all(jobs).then(function (rs) {
      var fresh = new Map();
      rs.forEach(function (r) { r.orders.forEach(function (o) { fresh.set(o.id, o); }); });
      S.orders = fresh;
      changed();
    });
  }
  function refreshAll() { return Promise.all([loadMenu(), loadOrders(), loadStock()]); }
  function refreshOrder(id) {
    return api("GET", "/api/orders/" + encodeURIComponent(id)).then(putOrder, function () { /* not for this person */ });
  }

  var menuTimer = null;
  function onEvent(ev) {
    var data = {};
    try { data = JSON.parse(ev.data); } catch (e) { return; }
    if (ev.event === "order" && data.order) refreshOrder(data.order);
    if (ev.event === "stock") {
      loadStock();
      clearTimeout(menuTimer);
      menuTimer = setTimeout(loadMenu, 300); // what is sold out may have changed
    }
    if (ev.event === "reset") throw new Error("reset");
  }

  // ---- the stream of changes ------------------------------------------------------------

  function setLive(state) {
    S.live = state;
    var el = $("live");
    el.hidden = !S.token;
    el.className = "pill " + (state === "live" ? "live" : state === "wait" ? "wait" : "off");
    el.textContent = state === "live" ? T("live") : state === "wait" ? T("reconnecting…") : T("offline");
  }

  // streamLoop listens for changes with fetch, because EventSource cannot send a header. After
  // every (re)connection it reads the state first: an event only says that something changed.
  function streamLoop(token) {
    var backoff = 500;
    (function again() {
      if (S.token !== token) return;
      var ctl = new AbortController(), last = Date.now(), dog = null;
      S.stream = ctl;
      setLive("wait");
      var headers = S.mode === "token" ? { Authorization: "Bearer " + token } : {};
      fetch("/api/stream", { headers: headers, signal: ctl.signal }).then(function (r) {
        if (r.status === 401) { signOut(S.mode === "password" ? T("Your session has ended. Sign in again.") : T("Your token was not accepted. Sign in again.")); return; }
        if (!r.ok) throw new Error("status " + r.status);
        setLive("live");
        backoff = 500;
        // A stream that ended with "reset" may have done so because the roles changed.
        if (S.mode === "password") refreshMe();
        refreshAll().catch(function () {});
        dog = setInterval(function () { if (Date.now() - last > 45000) ctl.abort(); }, 5000); // keep-alives come every 15 s
        var reader = r.body.getReader(), dec = new TextDecoder(), buf = "";
        return (function pump() {
          return reader.read().then(function (chunk) {
            if (chunk.done) return;
            last = Date.now();
            buf += dec.decode(chunk.value, { stream: true });
            var parsed = L.parseStream(buf);
            buf = parsed.rest;
            parsed.events.forEach(onEvent);
            return pump();
          });
        })();
      }).catch(function () { /* reconnect below */ }).then(function () {
        clearInterval(dog);
        if (S.token !== token) return;
        setLive("wait");
        setTimeout(again, backoff);
        backoff = Math.min(backoff * 2, 8000);
      });
    })();
  }

  // ---- signing in and out ---------------------------------------------------------------

  // begin shows the screens of a person who is signed in, whichever way they did it.
  function begin(me) {
    S.me = me;
    S.screens = L.screensFor(me.permissions);
    if (S.mode === "password") S.screens.push("account");
    S.who = me;
    $("who").textContent = whoText(me);
    $("signout").hidden = false;
    if (me.must_change_password) {
      // A temporary password opens nothing but the page that replaces it.
      S.screens = []; S.screen = null;
      clear($("tabs"));
      renderMustChange();
      return Promise.resolve();
    }
    var saved = load("kitchen.screen");
    S.screen = S.screens.indexOf(saved) >= 0 ? saved : S.screens[0] || null;
    return refreshAll().then(function () {
      streamLoop(S.token);
      render();
      setTimeout(function () { settled = true; }, 700);
    });
  }

  // whoText says who is signed in, under the name of the system.
  function whoText(me) {
    var name = me.display_name && me.display_name !== me.id ? me.display_name + " (" + me.id + ")" : me.id;
    var roles = me.roles.map(function (r) { return T(r); }).join(", ");
    return name + " · " + roles + (me.station ? " · " + T("station {name}", { name: me.station }) : "");
  }

  // start signs in with a token of the users file.
  function start(token) {
    S.token = token;
    return api("GET", "/api/me").then(function (me) {
      store("kitchen.token", token);
      return begin(me);
    });
  }

  // startSession picks up the session that the cookie holds, or signs in with a password.
  function startSession(me) {
    S.token = "session";
    return begin(me);
  }

  // refreshMe asks again who the person is: after a stream was cut, their roles may have changed,
  // and the tabs and what the screens offer have to follow.
  function refreshMe() {
    return api("GET", "/api/me").then(function (me) {
      var before = JSON.stringify([S.me && S.me.permissions, S.me && S.me.must_change_password]);
      if (before === JSON.stringify([me.permissions, me.must_change_password])) return;
      S.me = me;
      S.screens = L.screensFor(me.permissions);
      if (S.mode === "password") S.screens.push("account");
      if (me.must_change_password) { begin(me); return; }
      if (S.screens.indexOf(S.screen) < 0) S.screen = S.screens[0] || null;
      refreshAll().catch(function () {});
      render();
    }, function () { /* the stream will try again */ });
  }

  function signOut(message) {
    if (S.stream) S.stream.abort();
    S.token = null; S.me = null; S.orders = new Map(); S.menu = null; S.stock = null; S.cart = []; S.draft = null;
    shown = {}; settled = false;
    S.pending = {}; S.current = null; S.ui = { payFor: null, reasonFor: null, refundFor: null, station: null, audit: null };
    store("kitchen.token", null);
    $("who").textContent = T("not signed in");
    $("signout").hidden = true;
    setLive("off");
    $("live").hidden = true;
    clear($("tabs"));
    renderSignIn(message);
  }

  // userSignOut is the person asking to leave: with accounts the session ends on the server too,
  // so that a copy of the cookie is of no use.
  function userSignOut() {
    if (S.mode !== "password") { signOut(""); return; }
    api("POST", "/api/logout", {}, true).then(null, function () { /* the cookie goes with the tab's state anyway */ }).then(function () { signOut(""); });
  }

  function renderSignIn(message) {
    if (S.mode === "password") { renderPasswordSignIn(message); return; }
    var input = h("input", { type: "password", id: "token", autocomplete: "off", spellcheck: "false", "aria-describedby": "signin-err" });
    var err = h("p", { class: "problem", id: "signin-err", role: "alert" }, message || "");
    var btn = h("button", { type: "submit", class: "primary" }, T("Sign in"));
    var form = h("form", { class: "card signin", onsubmit: function (e) {
      e.preventDefault();
      var token = input.value.trim();
      if (!token) { err.textContent = T("Enter your token."); return; }
      btn.disabled = true;
      start(token).catch(function (ex) {
        S.token = null;
        err.textContent = ex && ex.status === 401 ? T("That token was not accepted.") : T("Could not reach the kitchen system. Try again.");
      }).then(function () { btn.disabled = false; });
    } },
      h("h1", {}, T("Sign in")),
      h("div", { class: "field" }, h("label", { for: "token" }, T("Your token")), input),
      err,
      btn,
      h("p", { class: "fine" }, T("Ask the manager for your token. It is kept in this tab only, and goes away when you close it.")));
    clear($("view")).appendChild(authShell(form));
    input.focus();
  }

  function fieldOf(id, label, input) { return h("div", { class: "field" }, h("label", { for: id }, label), input); }

  function renderPasswordSignIn(message) {
    var user = h("input", { type: "text", id: "username", autocomplete: "username", autocapitalize: "none", spellcheck: "false", maxlength: "32", "aria-describedby": "signin-err" });
    var pass = h("input", { type: "password", id: "password", autocomplete: "current-password", "aria-describedby": "signin-err" });
    var err = h("p", { class: "problem", id: "signin-err", role: "alert" }, message || "");
    var btn = h("button", { type: "submit", class: "primary" }, T("Sign in"));
    var form = h("form", { class: "card signin", onsubmit: function (e) {
      e.preventDefault();
      if (!user.value.trim() || !pass.value) { err.textContent = T("Enter your login name and password."); return; }
      btn.disabled = true; err.textContent = "";
      // quiet: a 401 here is "wrong password", not an ended session
      api("POST", "/api/login", { username: user.value.trim(), password: pass.value }, true).then(function (me) {
        pass.value = "";
        return startSession(me);
      }).catch(function (ex) {
        pass.value = "";
        err.textContent = ex && ex.body && ex.body.error ? ex.body.error : T("Could not reach the kitchen system. Try again.");
        pass.focus();
      }).then(function () { btn.disabled = false; });
    } },
      h("h1", {}, T("Sign in")),
      fieldOf("username", T("Login name"), user),
      fieldOf("password", T("Password"), pass),
      err, btn,
      h("p", { class: "fine" }, T("Forgot your password? Ask an administrator to reset it.")));
    clear($("view")).appendChild(authShell(form));
    (message ? pass : user).focus();
  }

  // passwordForm changes the password of the person signed in. forced is the page that a
  // temporary password leads to, where it is the only thing to do.
  function passwordForm(onChanged, forced) {
    var cur = h("input", { type: "password", id: "pw-current", autocomplete: "current-password" });
    var nw = h("input", { type: "password", id: "pw-new", autocomplete: "new-password" });
    var again = h("input", { type: "password", id: "pw-again", autocomplete: "new-password" });
    var err = h("p", { class: "problem", role: "alert" });
    var btn = h("button", { type: "submit", class: forced ? "primary" : "" }, T("Change password"));
    var form = h("form", { class: forced ? "stack" : "card stack", onsubmit: function (e) {
      e.preventDefault();
      var problem = !cur.value ? T("Enter your current password.") : L.passwordProblem(nw.value, again.value, S.me.id);
      if (problem) { err.textContent = problem; return; }
      err.textContent = "";
      btn.disabled = true;
      api("POST", "/api/me/password", { current: cur.value, new: nw.value }).then(function (me) {
        cur.value = nw.value = again.value = "";
        toast(forced ? T("Password set.") : T("Password changed. Your other sessions have ended."), "ok");
        return onChanged(me);
      }, function (ex) {
        err.textContent = ex && ex.body && ex.body.error ? ex.body.error : T("Could not reach the kitchen system. Try again.");
      }).then(function () { btn.disabled = false; });
    } },
      forced ? null : h("h2", {}, T("Change your password")),
      fieldOf("pw-current", forced ? T("Temporary password") : T("Current password"), cur),
      fieldOf("pw-new", T("New password"), nw),
      fieldOf("pw-again", T("New password again"), again),
      h("p", { class: "fine" }, T("At least 10 characters, not your login name. A phrase of several words is easier to remember than a jumble.")),
      err, btn);
    return { form: form, focus: function () { cur.focus(); } };
  }

  // renderMustChange is the only page for an account whose password is a temporary one.
  function renderMustChange() {
    var pf = passwordForm(function (me) { return begin(me); }, true);
    clear($("view")).appendChild(authShell(h("div", { class: "card signin" },
      h("h1", {}, T("Choose your own password")),
      h("p", {}, T("The password you were given is temporary. Choose one that only you know before you go on.")),
      pf.form)));
    pf.focus();
  }

  // ---- the frame: tabs and the current screen -------------------------------------------

  // title is the name of a screen in the language of the moment (it cannot be a table made at
  // load, which would keep the language of the page's first second).
  function title(id) {
    switch (id) {
      case "order": return T("Order");
      case "myorders": return T("My orders");
      case "till": return T("Till");
      case "kitchen": return T("Kitchen");
      case "manager": return T("Manager");
      case "stock": return T("Stock");
      case "users": return T("Users");
      case "account": return T("Account");
    }
    return id;
  }

  function renderTabs() {
    var tabs = clear($("tabs"));
    S.screens.forEach(function (id) {
      tabs.appendChild(h("button", { type: "button", "aria-current": id === S.screen ? "page" : null, onclick: function () { go(id); } }, icon(id), h("span", {}, title(id))));
    });
  }

  function go(id) {
    if (S.screen === "order" && id !== "order") S.placed = null; // the "order placed" banner belongs to the visit that placed it
    S.screen = id;
    store("kitchen.screen", id);
    render();
  }

  function render() {
    renderTabs();
    var view = clear($("view"));
    if (!S.screen) {
      view.appendChild(h("div", { class: "card signin" }, h("h1", {}, T("Nothing here for you")), h("p", { class: "fine" }, T("This account has no screen in the kitchen system."))));
      return;
    }
    var banner = bannerFor(S.screen);
    if (banner) view.appendChild(banner);
    S.current = SCREENS[S.screen](view);
    S.current.update();
  }

  // The two pages that a customer lives on open with a banner, like the front page of a shop; the
  // screens of the staff are for working in and have a plain title.
  function bannerFor(screen) {
    var customer = S.me && !S.me.permissions["order.view_all"];
    if (screen === "order" && customer) {
      return h("section", { class: "hero" }, h("div", { class: "hero-in" },
        h("h1", {}, T("Order fast."), h("br"), T("Pick up on time.")),
        h("p", {}, T("Choose your items, pay at the till and follow each step of the preparation here.")),
        h("button", { type: "button", class: "primary", onclick: function () {
          var m = $("menu");
          if (m) m.scrollIntoView({ behavior: window.matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth", block: "start" });
        } }, T("See the menu"))));
    }
    if (screen === "myorders") {
      return h("section", { class: "hero slim" }, h("div", { class: "hero-in" }, h("h1", {}, T("My orders")), h("p", {}, T("What is being made, and what is ready, live."))));
    }
    return null;
  }

  // authShell puts a sign-in form beside the name of the system, as the front door of a site.
  function authShell(card) {
    return h("div", { class: "auth" },
      h("aside", { class: "auth-art" },
        h("div", { class: "wordmark" }, "KITCHEN", h("i", {}, ".")),
        h("h1", {}, T("From the counter"), h("br"), T("to the kitchen")),
        h("p", {}, T("One screen for each person on the team: the customer, the till, the kitchen, the manager."))),
      card);
  }

  var pendingFrame = false;
  function changed() {
    if (pendingFrame) return;
    pendingFrame = true;
    window.requestAnimationFrame(function () {
      pendingFrame = false;
      if (S.current) S.current.update();
    });
  }

  // ---- pieces shared by the screens -----------------------------------------------------

  function orderList() {
    var list = [];
    S.orders.forEach(function (o) { list.push(o); });
    return list;
  }

  // orderCard shows an order. actions is a list of elements for its footer.
  function orderCard(o, actions, extra) {
    var tickets = (o.tickets || []).map(function (t) {
      return h("span", { class: "chip " + t.status }, t.station + ": " + T(t.status));
    });
    var move = motion("o:" + o.id, o.status);
    var cls = "order" + (o.cancel_request ? " attention" : "") + (move === "enter" ? " enter" : "");
    return h("article", { class: cls, "aria-label": T("Order {n}", { n: o.number }) },
      h("div", { class: "head" },
        h("span", { class: "num" }, "#" + o.number),
        h("span", { class: "badge " + o.status + (move === "stamp" ? " stamp" : "") }, T(o.status)),
        o.channel === "counter" ? h("span", { class: "chip" }, "counter") : null,
        h("span", { class: "age", title: o.placed_at }, L.ageText(o.placed_at))),
      h("ul", { class: "lines" }, (o.lines || []).map(function (l) {
        return h("li", {}, h("span", {}, l.quantity + " × " + l.name,
          // A combo lists what is in it; any other line lists its chosen options.
          (l.pieces && l.pieces.length > 1) ? h("small", {}, l.pieces.map(function (p) { return p.name; }).join(" + "))
            : (l.options && l.options.length) ? h("small", {}, l.options.join(", ")) : null,
          l.note ? h("small", { class: "note" }, "“" + l.note + "”") : null),
          h("span", { class: "money" }, money(l.total)));
      })),
      (o.discounts || []).map(function (d) {
        return h("div", { class: "row" }, h("span", { class: "fine" }, d.name), h("span", { class: "grow" }), h("span", { class: "money fine" }, "−" + money(d.amount)));
      }),
      h("div", { class: "row" }, h("b", {}, T("Total")), h("span", { class: "grow" }), h("b", { class: "money" }, money(o.total))),
      tickets.length ? h("div", { class: "tickets" }, tickets) : null,
      o.payment ? h("div", { class: "fine" }, T("Paid by {method}", { method: T(o.payment.method) }) + (o.payment.change ? ", " + T("change {amount}", { amount: money(o.payment.change) }) : "") + (o.payment.reference ? " · " + o.payment.reference : "")) : null,
      o.cancel_request ? h("div", { class: "banner warn" }, T("Cancellation requested by {who}: {reason}", { who: o.cancel_request.by, reason: o.cancel_request.reason })) : null,
      o.status === "cancelled" ? h("div", { class: "banner bad" }, T("Cancelled") + (o.cancel_reason ? ": " + o.cancel_reason : "") + (o.refunded ? " · " + T("refunded {amount}", { amount: money(o.refunded) }) : "")) : null,
      o.status === "completed" && o.refunded ? h("div", { class: "banner warn" }, T("Refunded {amount}", { amount: money(o.refunded) })) : null,
      extra || null,
      actions && actions.length ? h("div", { class: "row" }, actions) : null);
  }

  // ask asks for a reason in the card, and calls send(reason, button) when it is given.
  function reasonForm(key, label, send) {
    var input = h("input", { type: "text", maxlength: "200", placeholder: label, "aria-label": label });
    var go = h("button", { type: "submit", class: "small" }, T("Send"));
    return h("form", { class: "row", onsubmit: function (e) {
      e.preventDefault();
      if (!input.value.trim()) { input.focus(); return; }
      send(input.value.trim(), go);
    } }, input, go, h("button", { type: "button", class: "small ghost", onclick: function () { S.ui.reasonFor = null; changed(); } }, T("Back")));
  }


  // cancelAction offers the right way to cancel: directly while unpaid, as a request afterwards.
  function cancelActions(o) {
    var acts = [];
    var key = (o.status === "placed" ? "cancel:" : "cancel-request:") + o.id;
    // The button is replaced by its form while the form is open; "Back" in the form brings it back.
    if (S.ui.reasonFor !== key && (o.status === "placed" || ((o.status === "paid" || o.status === "preparing" || o.status === "ready") && !o.cancel_request))) {
      acts.push(h("button", { type: "button", class: "small ghost", onclick: function () { S.ui.reasonFor = key; changed(); } },
        o.status === "placed" ? T("Cancel order") : T("Ask to cancel")));
    }
    var form = S.ui.reasonFor === key ? reasonForm(key, T("Why?"), function (reason, btn) {
      var ep = o.status === "placed" ? "cancel" : "cancel-request";
      run(key, btn, function (id) { return api("POST", "/api/orders/" + encodeURIComponent(o.id) + "/" + ep, { command_id: id, reason: reason }); }).then(function (v) {
        if (v) { S.ui.reasonFor = null; putOrder(v); toast(o.status === "placed" ? T("Order cancelled.") : T("Cancellation requested. A manager will decide."), "ok"); }
      });
    }) : null;
    return { buttons: acts, form: form };
  }

  // ---- the order screen (customer, and the cashier at the counter) ----------------------

  function cartSame(a, b) {
    return JSON.stringify([a.item, a.combo, a.options, a.picks, a.note || ""]) === JSON.stringify([b.item, b.combo, b.options, b.picks, b.note || ""]);
  }
  function addToCart(line) {
    for (var i = 0; i < S.cart.length; i++) {
      if (cartSame(S.cart[i], line)) { S.cart[i].quantity += line.quantity; cartChanged(); return; }
    }
    S.cart.push(line);
    cartChanged();
  }
  function cartChanged() { delete S.pending.place; S.placed = null; changed(); }

  function findItem(id) { return (S.menu.items || []).filter(function (i) { return i.id === id; })[0]; }

  function lineTitle(l) {
    if (l.item) {
      var it = findItem(l.item);
      return it ? it.name : l.item;
    }
    var c = (S.menu.combos || []).filter(function (x) { return x.id === l.combo; })[0];
    return c ? c.name : l.combo;
  }
  function lineDetail(l) {
    if (l.item) {
      var it = findItem(l.item);
      return it ? L.optionNames(it, l.options).join(", ") : "";
    }
    return (l.picks || []).map(function (p) {
      var pit = findItem(p.item);
      if (!pit) return p.item;
      var names = L.optionNames(pit, p.options);
      return pit.name + (names.length ? " (" + names.join(", ") + ")" : "");
    }).join(" + ");
  }

  // optionGroups draws the choices of an item. onchange gets the new choice.
  function optionGroups(item, choice, onchange) {
    return (item.groups || []).map(function (g) {
      var picked = (choice && choice[g.id]) || [];
      var problem = L.groupProblem(g, choice);
      return h("fieldset", {},
        h("legend", {}, g.name + (g.min > 0 ? " (" + T("required") + ")" : "")),
        h("div", { class: "opts" }, g.options.map(function (o) {
          var on = picked.indexOf(o.id) >= 0;
          var input = h("input", { type: g.max === 1 ? "radio" : "checkbox", name: "g-" + item.id + "-" + g.id, checked: on || null,
            onclick: function () { onchange(L.toggleOption(choice, g, o.id)); } });
          return h("label", { class: "opt" + (on ? " on" : "") }, input, o.name + (o.delta ? " (" + (o.delta > 0 ? "+" : "") + money(o.delta) + ")" : ""));
        })),
        problem ? h("div", { class: "problem" }, problem) : null);
    });
  }

  function itemCard(item) {
    var open = S.draft && S.draft.item === item.id;
    var kids = [h("div", { class: "row" }, h("span", { class: "name" }, item.name), h("span", { class: "grow" }), h("span", { class: "price" }, money(item.price)))];
    if (item.sold_out) kids.push(h("div", {}, h("span", { class: "badge soldout" }, T("sold out"))));
    var needsChoice = (item.groups || []).length > 0;
    var addBtn = h("button", { type: "button", class: "small", disabled: item.sold_out || null, onclick: function () {
      if (!needsChoice) { addToCart({ item: item.id, options: {}, quantity: 1, note: "" }); return; }
      S.draft = open ? null : { item: item.id, choice: {}, quantity: 1, note: "" };
      changed();
    } }, needsChoice ? (open ? T("Close") : T("Choose…")) : T("Add"));
    kids.push(h("div", { class: "row" }, addBtn));
    if (open) {
      var d = S.draft;
      var problems = L.itemProblems(item, d.choice);
      kids.push(h("div", { class: "panel" },
        optionGroups(item, d.choice, function (c) { d.choice = c; changed(); }),
        h("div", { class: "row" }, quantityStepper(d.quantity, function (q) { d.quantity = q; changed(); }),
          h("span", { class: "grow" }), h("b", { class: "money" }, money(L.unitPrice(item, d.choice) * d.quantity))),
        h("button", { type: "button", disabled: problems.length ? true : null, onclick: function () {
          addToCart({ item: item.id, options: d.choice, quantity: d.quantity, note: "" });
          S.draft = null;
          changed();
        } }, T("Add to cart"))));
    }
    return h("div", { class: "item" + (item.sold_out ? " soldout" : "") }, art(item), h("div", { class: "cb" }, kids));
  }

  function quantityStepper(q, set) {
    return h("span", { class: "stepper" },
      h("button", { type: "button", class: "small ghost", "aria-label": T("One less"), disabled: q <= 1 || null, onclick: function () { set(q - 1); } }, "−"),
      h("b", { "aria-live": "polite" }, String(q)),
      h("button", { type: "button", class: "small ghost", "aria-label": T("One more"), disabled: q >= 99 || null, onclick: function () { set(q + 1); } }, "+"));
  }

  function comboCard(combo) {
    var open = S.draft && S.draft.combo === combo.id;
    var kids = [h("div", { class: "row" }, h("span", { class: "name" }, combo.name), h("span", { class: "grow" }), h("span", { class: "price" }, money(combo.price)))];
    kids.push(h("div", { class: "row" }, h("button", { type: "button", class: "small", onclick: function () {
      S.draft = open ? null : { combo: combo.id, picks: combo.slots.map(function (s, i) { return { slot: i, item: s.items[0], options: {} }; }), quantity: 1 };
      changed();
    } }, open ? T("Close") : T("Choose…"))));
    if (open) {
      var d = S.draft, problems = [];
      var slots = combo.slots.map(function (slot, i) {
        var pick = d.picks[i], item = findItem(pick.item);
        if (item) L.itemProblems(item, pick.options).forEach(function (p) { problems.push(p); });
        return h("fieldset", {},
          h("legend", {}, slot.name),
          h("div", { class: "opts" }, slot.items.map(function (id) {
            var it = findItem(id);
            if (!it) return null;
            var on = pick.item === id;
            return h("label", { class: "opt" + (on ? " on" : "") },
              h("input", { type: "radio", name: "slot-" + combo.id + "-" + i, checked: on || null, disabled: it.sold_out || null,
                onclick: function () { pick.item = id; pick.options = {}; changed(); } }),
              it.name + (it.sold_out ? " (" + T("sold out") + ")" : ""));
          })),
          item ? optionGroups(item, pick.options, function (c) { pick.options = c; changed(); }) : null);
      });
      kids.push(h("div", { class: "panel" }, slots,
        h("div", { class: "row" }, quantityStepper(d.quantity, function (q) { d.quantity = q; changed(); }),
          h("span", { class: "grow" }), h("b", { class: "money" }, money(L.cartEstimate(S.menu, [{ combo: combo.id, picks: d.picks, quantity: d.quantity }])))),
        h("button", { type: "button", disabled: problems.length ? true : null, onclick: function () {
          addToCart({ combo: combo.id, picks: d.picks, quantity: d.quantity, note: "" });
          S.draft = null;
          changed();
        } }, T("Add to cart"))));
    }
    return h("div", { class: "item" }, art(combo, "combo"), h("div", { class: "cb" }, kids));
  }

  function screenOrder(root) {
    var menuBox = h("div", { id: "menu" }), cartBox = h("div", { class: "card cart" });
    // A customer's page opens with the banner of render(); the till's has a plain title.
    if (S.me.permissions["order.view_all"]) root.appendChild(h("h1", {}, T("New order at the counter")));
    root.appendChild(h("div", { class: "cols two" }, menuBox, cartBox));

    function update() {
      if (!S.menu) { clear(menuBox).appendChild(h("p", { class: "empty" }, T("Loading the menu…"))); return; }
      // The menu and the cart are drawn again; what is typed in the cart's fields is kept.
      var keep = document.activeElement && cartBox.contains(document.activeElement) ? document.activeElement.id : null;
      clear(menuBox);
      (S.menu.categories || []).forEach(function (cat) {
        var items = S.menu.items.filter(function (i) { return i.category === cat; });
        if (!items.length) return;
        menuBox.appendChild(h("section", { class: "menu-cat" }, h("h2", {}, T(cat)), h("div", { class: "items" }, items.map(itemCard))));
      });
      if ((S.menu.combos || []).length) {
        menuBox.appendChild(h("section", { class: "menu-cat" }, h("h2", {}, T("combos")), h("div", { class: "items" }, S.menu.combos.map(comboCard))));
      }
      drawCart();
      if (keep && $(keep)) $(keep).focus();
    }

    function drawCart() {
      clear(cartBox);
      cartBox.appendChild(h("h2", {}, T("Cart")));
      if (S.placed) {
        var o = S.placed;
        cartBox.appendChild(h("div", { class: "banner ok", role: "status" }, T("Order #{n} placed. Total {total}.", { n: o.order.number, total: money(o.order.total) })));
        (o.order.discounts || []).forEach(function (d) { cartBox.appendChild(h("p", { class: "fine" }, d.name + ": −" + money(d.amount))); });
        (o.rejected_codes || []).forEach(function (r) { cartBox.appendChild(h("p", { class: "problem" }, r)); });
        var next = [];
        if (S.me.permissions["order.record_payment"]) {
          next.push(h("button", { type: "button", onclick: function () { S.ui.payFor = o.order.id; go("till"); } }, T("Take payment")));
        } else if (!S.me.permissions["order.view_all"]) {
          next.push(h("button", { type: "button", onclick: function () { go("myorders"); } }, T("Follow my order")));
        }
        next.push(h("button", { type: "button", class: "ghost", onclick: function () { S.placed = null; changed(); } }, T("New order")));
        cartBox.appendChild(h("div", { class: "row" }, next));
        return;
      }
      if (!S.cart.length) { cartBox.appendChild(h("p", { class: "empty" }, T("Nothing in the cart yet."))); return; }
      S.cart.forEach(function (l, i) {
        var estimate = L.cartEstimate(S.menu, [l]);
        cartBox.appendChild(h("div", { class: "cart-line" },
          h("div", { class: "top2" }, h("div", {}, h("b", {}, lineTitle(l)), h("div", { class: "fine" }, lineDetail(l))), h("b", { class: "money" }, money(estimate))),
          h("div", { class: "row" }, quantityStepper(l.quantity, function (q) { l.quantity = q; cartChanged(); }),
            h("button", { type: "button", class: "small ghost", onclick: function () { S.cart.splice(i, 1); cartChanged(); } }, T("Remove"))),
          h("input", { type: "text", id: "note-" + i, maxlength: "200", placeholder: T("Note for the kitchen"), "aria-label": T("Note for {item}", { item: lineTitle(l) }), value: l.note || "",
            oninput: function (e) { l.note = e.target.value; delete S.pending.place; } })));
      });
      cartBox.appendChild(h("div", { class: "total" }, h("span", {}, T("Estimate")), h("span", {}, money(L.cartEstimate(S.menu, S.cart)))));
      cartBox.appendChild(h("p", { class: "fine" }, T("The kitchen system works out the price, with any offer, when the order is placed.")));
      cartBox.appendChild(h("div", { class: "field" }, h("label", { for: "codes" }, T("Promotion code")),
        h("input", { type: "text", id: "codes", value: S.codes, autocomplete: "off", oninput: function (e) { S.codes = e.target.value; delete S.pending.place; } })));
      if (S.me.permissions["order.view_all"]) {
        cartBox.appendChild(h("div", { class: "field" }, h("label", { for: "cust" }, T("Customer ID (optional)")),
          h("input", { type: "text", id: "cust", value: S.counterCustomer, autocomplete: "off", oninput: function (e) { S.counterCustomer = e.target.value; delete S.pending.place; } })));
      }
      var btn = h("button", { type: "button", class: "big primary", onclick: function () {
        var codes = S.codes.split(/[\s,]+/).filter(Boolean);
        run("place", btn, function (id) { return api("POST", "/api/orders", L.cartBody(id, S.cart, codes, S.counterCustomer.trim())); }).then(function (res) {
          if (!res) return;
          S.placed = res;
          S.cart = []; S.codes = ""; S.counterCustomer = "";
          putOrder(res.order);
        });
      } }, T("Place order"));
      cartBox.appendChild(btn);
    }
    return { update: update };
  }

  // ---- my orders (customer) -------------------------------------------------------------

  function screenMyOrders(root) {
    var box = h("div", { class: "stack" });
    root.appendChild(box);
    function update() {
      clear(box);
      var list = orderList().sort(function (a, b) { return b.number - a.number; });
      if (!list.length) { box.appendChild(h("p", { class: "empty" }, T("You have no orders yet."))); return; }
      list.forEach(function (o) {
        var c = (o.status === "completed" || o.status === "cancelled") ? { buttons: [], form: null } : cancelActions(o);
        var note = null;
        if (o.status === "placed") note = h("div", { class: "banner warn" }, T("Show this number at the till to pay: #{n}", { n: o.number }));
        else if (o.status === "ready") note = h("div", { class: "banner ok" }, T("Your order is ready."));
        box.appendChild(orderCard(o, c.buttons, [note, c.form]));
      });
    }
    return { update: update };
  }

  // ---- the till -------------------------------------------------------------------------

  function payForm(o) {
    var method = h("select", { id: "method-" + o.id, "aria-label": T("Payment method") },
      h("option", { value: "cash" }, T("Cash")), h("option", { value: "card" }, T("Card")), h("option", { value: "transfer" }, T("Transfer")));
    var tendered = h("input", { type: "number", id: "tendered-" + o.id, min: String(o.total), step: "1000", value: String(o.total), "aria-label": T("Cash handed over") });
    var reference = h("input", { type: "text", id: "ref-" + o.id, maxlength: "60", placeholder: T("Card slip or transfer reference"), "aria-label": T("Reference") });
    var change = h("b", { class: "money", "aria-live": "polite" });
    var cashBox = h("div", { class: "stack" }, h("div", { class: "row" }, h("label", { for: tendered.id }, T("Amount handed over")), tendered, h("span", {}, T("Change:")), change),
      h("div", { class: "quick" }, L.quickAmounts(o.total).map(function (a) {
        return h("button", { type: "button", class: "small ghost", onclick: function () { tendered.value = String(a); show(); } }, money(a));
      })));
    var refBox = h("div", { class: "row" }, h("label", { for: reference.id }, T("Reference")), reference);
    var confirm = h("button", { type: "button", class: "ok" }, T("Confirm payment of {amount}", { amount: money(o.total) }));
    function show() {
      var cash = method.value === "cash";
      cashBox.hidden = !cash; refBox.hidden = cash;
      var ch = L.cashChange(o.total, parseInt(tendered.value, 10) || 0);
      change.textContent = ch === null ? T("not enough") : money(ch);
      confirm.disabled = cash ? ch === null : !reference.value.trim();
    }
    method.addEventListener("change", show); tendered.addEventListener("input", show); reference.addEventListener("input", show);
    confirm.addEventListener("click", function () {
      var cash = method.value === "cash";
      var body = { method: method.value };
      if (cash) body.tendered = parseInt(tendered.value, 10); else body.reference = reference.value.trim();
      run("pay:" + o.id, confirm, function (id) { body.command_id = id; return api("POST", "/api/orders/" + encodeURIComponent(o.id) + "/pay", body); }).then(function (v) {
        if (v) { S.ui.payFor = null; putOrder(v); toast(T("Order #{n} paid.", { n: v.number }), "ok"); }
      });
    });
    show();
    return h("div", { class: "pay" }, h("div", { class: "row" }, h("label", { for: method.id }, T("Method")), method), cashBox, refBox,
      h("div", { class: "row" }, confirm, h("button", { type: "button", class: "ghost", onclick: function () { S.ui.payFor = null; changed(); } }, T("Back"))));
  }

  function tillCard(o) {
    var acts = [], extra = [];
    // While the payment form is open it is the only thing on the card; "Back" returns to the buttons.
    if (S.ui.payFor === o.id && o.status === "placed" && S.me.permissions["order.record_payment"]) return orderCard(o, [], [payForm(o)]);
    if (o.status === "placed" && S.me.permissions["order.record_payment"]) {
      acts.push(h("button", { type: "button", onclick: function () { S.ui.payFor = S.ui.payFor === o.id ? null : o.id; changed(); } }, T("Take payment")));
    }
    if (o.status === "ready" && S.me.permissions["order.hand_over"]) {
      var btn = h("button", { type: "button", class: "ok", onclick: function () {
        run("handover:" + o.id, btn, function (id) { return api("POST", "/api/orders/" + encodeURIComponent(o.id) + "/handover", { command_id: id }); }).then(function (v) {
          if (v) { putOrder(v); toast(T("Order #{n} handed over.", { n: v.number }), "ok"); }
        });
      } }, T("Hand over"));
      acts.push(btn);
    }
    if (S.me.permissions["order.cancel_unpaid"] || S.me.permissions["order.request_cancel"]) {
      var c = cancelActions(o);
      acts = acts.concat(c.buttons);
      if (c.form) extra.push(c.form);
    }
    if (S.ui.payFor === o.id && o.status === "placed") extra.push(payForm(o));
    return orderCard(o, acts, extra);
  }

  function screenTill(root) {
    var toPay = h("div", { class: "stack" }), inKitchen = h("div", { class: "stack" }), ready = h("div", { class: "stack" });
    var canPay = S.me.permissions["order.record_payment"];
    root.appendChild(h("div", { class: "row" }, h("h1", {}, canPay ? T("Till") : T("Hand over")), h("span", { class: "grow" }),
      S.me.permissions["order.place"] ? h("button", { type: "button", onclick: function () { go("order"); } }, T("New counter order")) : null));
    root.appendChild(h("div", { class: "cols three" },
      canPay ? h("section", {}, h("h2", {}, T("To pay")), toPay) : null,
      canPay ? h("section", {}, h("h2", {}, T("In the kitchen")), inKitchen) : null,
      h("section", {}, h("h2", {}, T("Ready to hand over")), ready)));
    function fill(box, list, empty) {
      clear(box);
      if (!list.length) box.appendChild(h("p", { class: "empty" }, empty));
      list.forEach(function (o) { box.appendChild(tillCard(o)); });
    }
    function update() {
      var cols = L.groupForTill(orderList());
      fill(toPay, cols.toPay, T("Nothing waiting for payment."));
      fill(inKitchen, cols.inKitchen, T("Nothing in the kitchen."));
      fill(ready, cols.ready, T("Nothing ready."));
    }
    return { update: update };
  }

  // ---- the kitchen board ----------------------------------------------------------------

  function ticketCard(w, station) {
    var o = w.order, cooking = w.ticket.status === "started";
    var key = (cooking ? "finish:" : "start:") + o.id + ":" + station;
    var btn = h("button", { type: "button", class: "big " + (cooking ? "ok" : ""), disabled: w.cancelling || null, onclick: function () {
      run(key, btn, function (id) {
        return api("POST", "/api/orders/" + encodeURIComponent(o.id) + "/tickets/" + encodeURIComponent(station) + "/" + (cooking ? "finish" : "start"), { command_id: id });
      }).then(function (v) { if (v) putOrder(v); });
    } }, cooking ? T("Done") : T("Start"));
    var minutes = (Date.now() - Date.parse(w.since)) / 60000;
    var move = motion("t:" + o.id + ":" + station, cooking ? "started" : "pending");
    return h("article", { class: "order ticket" + (move ? " enter" : "") + (cooking ? " cooking" : "") + (w.cancelling ? " cancelling" : "") + (minutes > 10 ? " late" : ""), "aria-label": T("Order {n}", { n: o.number }) },
      h("div", { class: "head" }, h("span", { class: "num" }, "#" + o.number), minutes > 10 ? h("span", { class: "badge cancelled" }, T("late")) : null,
        o.channel === "counter" ? h("span", { class: "chip" }, T("counter")) : null,
        h("span", { class: "age" }, (cooking ? T("started {age}", { age: L.ageText(w.since) }) : T("waiting {age}", { age: L.ageText(w.since) })))),
      h("ul", { class: "pieces" }, w.pieces.map(function (p) { return h("li", {}, h("b", {}, p.quantity + "×"), p.name); })),
      w.notes.map(function (n) { return h("div", { class: "note" }, "“" + n + "”"); }),
      w.cancelling ? h("div", { class: "banner warn" }, T("A cancellation is waiting for the manager. Hold on.")) : null,
      h("div", { class: "row" }, btn));
  }

  function screenKitchen(root) {
    var waiting = h("div", { class: "stack" }), cooking = h("div", { class: "stack" });
    var head = h("div", { class: "row" });
    root.appendChild(head);
    root.appendChild(h("div", { class: "cols board" }, h("section", {}, h("h2", { id: "wh" }, T("Waiting")), waiting), h("section", {}, h("h2", { id: "ch" }, T("Being made")), cooking)));
    function update() {
      var orders = orderList();
      var stations = L.stationsOf(orders);
      var mine = S.me.station;
      var station = mine || S.ui.station || stations[0];
      clear(head);
      head.appendChild(h("h1", {}, mine ? T("Station: {name}", { name: mine }) : T("Kitchen board")));
      head.appendChild(h("span", { class: "grow" }));
      if (!mine && stations.length) {
        var sel = h("select", { "aria-label": T("Station"), onchange: function (e) { S.ui.station = e.target.value; changed(); } },
          stations.map(function (s) { return h("option", { value: s, selected: s === station || null }, s); }));
        head.appendChild(sel);
        head.appendChild(h("span", { class: "fine" }, T("You can look at any station; only its own staff can start or finish a ticket.")));
      }
      var work = L.stationWork(orders, station || "");
      clear(waiting); clear(cooking);
      if (!work.waiting.length) waiting.appendChild(h("p", { class: "empty" }, T("Nothing waiting.")));
      if (!work.cooking.length) cooking.appendChild(h("p", { class: "empty" }, T("Nothing being made.")));
      work.waiting.forEach(function (w) { waiting.appendChild(ticketCard(w, station)); });
      work.cooking.forEach(function (w) { cooking.appendChild(ticketCard(w, station)); });
      $("wh").textContent = T("Waiting ({n})", { n: work.waiting.length });
      $("ch").textContent = T("Being made ({n})", { n: work.cooking.length });
    }
    return { update: update };
  }

  // ---- the manager ----------------------------------------------------------------------

  function decisionCard(o) {
    var policy = L.refundPolicy(o);
    var refund = h("input", { type: "number", id: "refund-" + o.id, min: String(policy.min), max: String(policy.max), step: "1000", value: String(policy.suggested), disabled: policy.fixed || null, "aria-label": T("Refund") });
    var approve = h("button", { type: "button", class: "danger", onclick: function () {
      run("approve:" + o.id, approve, function (id) {
        return api("POST", "/api/orders/" + encodeURIComponent(o.id) + "/cancel-approve", { command_id: id, refund: parseInt(refund.value, 10) || 0 });
      }).then(function (v) { if (v) { putOrder(v); toast(T("Order #{n} cancelled.", { n: v.number }), "ok"); } });
    } }, T("Approve and cancel"));
    var rejectKey = "reject:" + o.id;
    var extra = [h("div", { class: "pay" },
      h("div", { class: "row" }, h("label", { for: refund.id }, T("Refund")), refund,
        h("span", { class: "fine" }, policy.fixed ? T("No station has started: the whole amount goes back and the ingredients return to the stock.") : T("Work has started: set what goes back, from 0 to {max}. The ingredients stay used.", { max: money(policy.max) }))),
      h("div", { class: "row" }, approve, h("button", { type: "button", class: "ghost", onclick: function () { S.ui.reasonFor = S.ui.reasonFor === rejectKey ? null : rejectKey; changed(); } }, T("Reject…"))))];
    if (S.ui.reasonFor === rejectKey) {
      extra.push(reasonForm(rejectKey, T("Why not?"), function (reason, btn) {
        run(rejectKey, btn, function (id) { return api("POST", "/api/orders/" + encodeURIComponent(o.id) + "/cancel-reject", { command_id: id, reason: reason }); }).then(function (v) {
          if (v) { S.ui.reasonFor = null; putOrder(v); toast(T("Request rejected. The order goes on."), "ok"); }
        });
      }));
    }
    return orderCard(o, [], extra);
  }

  function refundCard(o) {
    var key = "refund:" + o.id;
    var extra = null;
    if (S.ui.refundFor === o.id) {
      var amount = h("input", { type: "number", min: "1", max: String(o.total - (o.refunded || 0)), step: "1000", "aria-label": T("Amount") });
      var reason = h("input", { type: "text", maxlength: "200", placeholder: T("Why?"), "aria-label": T("Reason") });
      var send = h("button", { type: "submit", class: "small danger" }, T("Refund"));
      extra = h("form", { class: "row", onsubmit: function (e) {
        e.preventDefault();
        run(key, send, function (id) { return api("POST", "/api/orders/" + encodeURIComponent(o.id) + "/refund", { command_id: id, amount: parseInt(amount.value, 10) || 0, reason: reason.value.trim() }); }).then(function (v) {
          if (v) { S.ui.refundFor = null; putOrder(v); toast(T("Refund recorded."), "ok"); }
        });
      } }, amount, reason, send, h("button", { type: "button", class: "small ghost", onclick: function () { S.ui.refundFor = null; changed(); } }, T("Back")));
    }
    var open = h("button", { type: "button", class: "small ghost", onclick: function () { S.ui.refundFor = S.ui.refundFor === o.id ? null : o.id; changed(); } }, T("Refund…"));
    return orderCard(o, o.status === "completed" && (o.refunded || 0) < o.total ? [open] : [], extra);
  }

  function screenManager(root) {
    var tiles = h("div", { class: "tiles" }), queue = h("div", { class: "stack" }), recent = h("div", { class: "stack" });
    var lookup = h("input", { type: "text", id: "lookup", placeholder: T("Order number"), inputmode: "numeric", "aria-label": T("Order number") });
    var auditBox = h("div", { class: "card" });
    root.appendChild(h("h1", {}, T("Manager")));
    root.appendChild(tiles);
    root.appendChild(h("div", { class: "cols two" },
      h("section", {}, h("h2", {}, T("Cancellation requests")), queue, h("h2", {}, T("Completed orders (refunds)")), recent),
      h("section", {}, h("h2", {}, T("Look up an order")),
        h("form", { class: "row", onsubmit: function (e) {
          e.preventDefault();
          var n = parseInt(lookup.value, 10), found = null;
          S.orders.forEach(function (o) { if (o.number === n) found = o; });
          if (!found) { toast(T("No order with that number among the ones loaded."), "bad"); return; }
          api("GET", "/api/orders/" + encodeURIComponent(found.id) + "/events").then(function (r) { S.ui.audit = { order: found, events: r.events }; changed(); }, function (ex) { toast(ex.message, "bad"); });
        } }, lookup, h("button", { type: "submit" }, T("Show"))),
        auditBox)));
    function update() {
      var all = orderList();
      var s = L.summary(all);
      clear(tiles);
      [[T("Orders"), s.orders], [T("In progress"), s.active], [T("Handed over"), s.completed], [T("Cancelled"), s.cancelled], [T("Taken"), money(s.takings)], [T("Refunded"), money(s.refunded)], [T("Net"), money(s.net)]].forEach(function (t) {
        tiles.appendChild(h("div", { class: "tile" }, h("div", { class: "k" }, t[0]), h("div", { class: "v" }, String(t[1]))));
      });
      var q = L.cancelQueue(all);
      clear(queue);
      if (!q.length) queue.appendChild(h("p", { class: "empty" }, T("No cancellation is waiting.")));
      q.forEach(function (o) { queue.appendChild(decisionCard(o)); });
      clear(recent);
      var done = all.filter(function (o) { return o.status === "completed"; }).sort(function (a, b) { return b.number - a.number; }).slice(0, 8);
      if (!done.length) recent.appendChild(h("p", { class: "empty" }, T("No order has been handed over yet.")));
      done.forEach(function (o) { recent.appendChild(refundCard(o)); });
      clear(auditBox);
      if (S.ui.audit) {
        auditBox.appendChild(h("h3", {}, T("Order #{n}", { n: S.ui.audit.order.number }) + " · " + T(S.ui.audit.order.status)));
        auditBox.appendChild(h("table", {}, h("thead", {}, h("tr", {}, h("th", {}, "#"), h("th", {}, T("Event")), h("th", {}, T("By")), h("th", {}, T("When")))),
          h("tbody", {}, S.ui.audit.events.map(function (e) {
            return h("tr", {}, h("td", { class: "n" }, String(e.seq)), h("td", {}, e.type), h("td", {}, e.by + " (" + e.roles + ")"), h("td", {}, new Date(e.at).toLocaleTimeString(I18N.locale())));
          }))));
      } else {
        auditBox.appendChild(h("p", { class: "fine" }, T("Type an order number to see who did what, and when.")));
      }
    }
    return { update: update };
  }

  // ---- the stock ------------------------------------------------------------------------

  function stockForm(kind, title, withReason) {
    var ing = h("select", { id: kind + "-ing", "aria-label": T("Ingredient") });
    var qty = h("input", { type: "number", id: kind + "-qty", min: kind === "count" ? "0" : "1", step: "1", "aria-label": T("Quantity") });
    var reason = withReason ? h("input", { type: "text", id: kind + "-reason", maxlength: "200", placeholder: T("Reason"), "aria-label": T("Reason") }) : null;
    var btn = h("button", { type: "submit" }, title);
    var form = h("form", { class: "card stack", onsubmit: function (e) {
      e.preventDefault();
      var key = kind + ":" + ing.value + ":" + qty.value + ":" + (reason ? reason.value : "");
      run("stock:" + key, btn, function (id) {
        var body = { command_id: id, ingredient: ing.value, qty: parseInt(qty.value, 10) || 0 };
        if (reason) body.reason = reason.value.trim();
        return api("POST", "/api/stock/" + kind, body);
      }).then(function (v) { if (v) { S.stock = v; qty.value = ""; if (reason) reason.value = ""; toast(T("Stock updated."), "ok"); changed(); } });
    } }, h("h2", {}, title), h("div", { class: "row" }, ing, qty, reason, btn));
    form.fill = function () {
      var cur = ing.value;
      clear(ing);
      ((S.stock && S.stock.levels) || []).forEach(function (l) { ing.appendChild(h("option", { value: l.ingredient, selected: l.ingredient === cur || null }, l.ingredient)); });
    };
    return form;
  }

  function screenStock(root) {
    var table = h("div", { class: "card" });
    var forms = [];
    var perms = S.me.permissions;
    root.appendChild(h("h1", {}, T("Stock")));
    root.appendChild(h("div", { class: "cols two" }, table, h("div", { class: "stack" },
      perms["stock.receive"] ? (forms[forms.length] = stockForm("receive", T("Receive goods"), false)) : null,
      perms["stock.adjust"] ? (forms[forms.length] = stockForm("write-off", T("Write off"), true)) : null,
      perms["stock.adjust"] ? (forms[forms.length] = stockForm("count", T("Stocktake (set the real quantity)"), true)) : null,
      (!perms["stock.receive"] && !perms["stock.adjust"]) ? h("p", { class: "fine" }, T("You can look at the stock; the storekeeper changes it.")) : null)));
    var filled = false;
    function update() {
      clear(table);
      table.appendChild(h("h2", {}, T("Ingredients")));
      var levels = (S.stock && S.stock.levels) || [];
      if (!levels.length) { table.appendChild(h("p", { class: "empty" }, T("Loading the stock…"))); return; }
      table.appendChild(h("table", {}, h("thead", {}, h("tr", {}, h("th", {}, T("Ingredient")), h("th", { class: "n" }, T("Quantity")), h("th", {}, ""))),
        h("tbody", {}, levels.map(function (l) {
          return h("tr", {}, h("td", {}, l.ingredient), h("td", { class: "n" }, String(l.qty)), h("td", {}, l.qty === 0 ? h("span", { class: "badge soldout" }, T("none")) : l.low ? h("span", { class: "badge low" }, T("low")) : null));
        }))));
      // the ingredient lists of the forms are filled when the stock arrives, and kept after
      if (!filled) { forms.forEach(function (f) { f.fill(); }); filled = true; }
    }
    return { update: update };
  }

  // ---- your account ---------------------------------------------------------------------

  function when(iso) { return new Date(iso).toLocaleString(I18N.locale()); }
  function kv(rows) {
    return h("dl", { class: "kv" }, rows.filter(Boolean).map(function (r) { return [h("dt", {}, r[0]), h("dd", {}, r[1])]; }));
  }

  // twoStep makes a button that needs a second click within a few seconds, for what is not easy to undo.
  function twoStep(btn, label, go) {
    var armed = false, timer = null;
    function reset() { armed = false; btn.textContent = label; btn.classList.remove("danger"); }
    btn.addEventListener("click", function () {
      if (!armed) { armed = true; btn.textContent = T("Sure?"); btn.classList.add("danger"); timer = setTimeout(reset, 4000); return; }
      clearTimeout(timer); reset(); go(btn);
    });
    return btn;
  }

  function screenAccount(root) {
    var sessBox = h("div", { class: "card" });
    var sessions = null;
    function loadSessions() {
      return api("GET", "/api/me/sessions").then(function (r) { sessions = r.sessions; changed(); }, function () { /* shown as it was */ });
    }
    var pf = passwordForm(function (me) { S.me = me; return loadSessions(); }, false);
    var me = S.me;
    root.appendChild(h("h1", {}, T("Your account")));
    root.appendChild(h("div", { class: "cols two" },
      h("div", { class: "stack" },
        h("div", { class: "card stack" }, h("h2", {}, T("Who you are")), kv([
          [T("Login name"), me.id], [T("Name"), me.display_name || me.id], [T("Roles"), me.roles.join(", ")],
          me.station ? [T("Station"), me.station] : null,
          me.session_expires_at ? [T("This session ends"), T("{time} if it is not used", { time: when(me.session_expires_at) })] : null,
        ])),
        pf.form),
      sessBox));
    loadSessions();

    function update() {
      clear(sessBox);
      sessBox.appendChild(h("h2", {}, T("Where you are signed in")));
      if (!sessions) { sessBox.appendChild(h("p", { class: "empty" }, T("Loading…"))); return; }
      var others = sessions.filter(function (s) { return !s.current; });
      sessBox.appendChild(h("table", {}, h("thead", {}, h("tr", {}, h("th", {}, T("Browser")), h("th", {}, T("Last used")), h("th", {}, ""))),
        h("tbody", {}, sessions.map(function (s) {
          var end = s.current ? h("span", { class: "chip ready" }, T("this one")) : twoStep(h("button", { type: "button", class: "small ghost" }, T("End")), T("End"), function (btn) {
            run("revoke:" + s.id, btn, function () { return api("POST", "/api/me/sessions/" + s.id + "/revoke", {}); }).then(function (v) { if (v) { toast(T("Session ended."), "ok"); loadSessions(); } });
          });
          return h("tr", {}, h("td", {}, L.shortAgent(s.user_agent), h("small", { class: "fine" }, " " + s.ip)), h("td", {}, L.ageText(s.last_seen_at)), h("td", {}, end));
        }))));
      if (others.length) {
        sessBox.appendChild(twoStep(h("button", { type: "button", class: "small ghost" }, T("End all other sessions")), T("End all other sessions"), function (btn) {
          run("revoke-others", btn, function () { return api("POST", "/api/me/sessions/revoke-others", {}); }).then(function (v) { if (v) { toast(T("{n} session(s) ended.", { n: v.revoked }), "ok"); loadSessions(); } });
        }));
      }
      sessBox.appendChild(h("p", { class: "fine" }, T("A session ends after a period without use, or at the latest after a few days. Changing your password ends all the others.")));
    }
    return { update: update, reload: loadSessions };
  }

  // ---- users (the administrator) --------------------------------------------------------

  function screenUsers(root) {
    var st = { users: null, roles: null, events: null, subject: "", editing: null, temp: null };
    var tempBox = h("div"), listBox = h("div", { class: "card scroll" }), eventsBox = h("div", { class: "card" });

    // the form: one for creating and for editing
    var idIn = h("input", { type: "text", id: "nu-id", maxlength: "32", autocomplete: "off", autocapitalize: "none", spellcheck: "false" });
    var nameIn = h("input", { type: "text", id: "nu-name", maxlength: "60", autocomplete: "off" });
    var stationIn = h("input", { type: "text", id: "nu-station", maxlength: "20", autocomplete: "off", autocapitalize: "none", placeholder: "drinks" });
    var stationField = fieldOf("nu-station", T("Station (kitchen workers)"), stationIn);
    stationField.hidden = true;
    var roleBox = h("div", { class: "opts" });
    var checks = {};
    var formErr = h("p", { class: "problem", role: "alert" });
    var formTitle = h("h2", {}, T("New user"));
    var submit = h("button", { type: "submit", class: "primary" }, T("Create user"));
    var cancel = h("button", { type: "button", class: "ghost", hidden: true, onclick: function () { edit(null); } }, T("Cancel"));
    var form = h("form", { class: "card stack", onsubmit: function (e) {
      e.preventDefault();
      var roles = Object.keys(checks).filter(function (r) { return checks[r].checked; });
      var body = { display_name: nameIn.value.trim(), roles: roles, station: stationField.hidden ? "" : stationIn.value.trim() };
      if (!st.editing) body.id = idIn.value.trim();
      formErr.textContent = !st.editing && !body.id ? T("Choose a login name.") : !body.display_name ? T("Give a name.") : !roles.length ? T("Choose at least one role.") : "";
      if (formErr.textContent) return;
      run("user-save:" + (st.editing || body.id), submit, function () {
        return api(st.editing ? "PUT" : "POST", "/api/admin/users" + (st.editing ? "/" + encodeURIComponent(st.editing) : ""), body);
      }).then(function (v) {
        if (!v) return;
        if (v.temporary_password) st.temp = { id: v.user.id, password: v.temporary_password };
        toast(st.editing ? T("Saved.") : T("User created."), "ok");
        edit(null);
        reload();
      });
    } },
      formTitle,
      fieldOf("nu-id", T("Login name"), idIn),
      fieldOf("nu-name", T("Name"), nameIn),
      h("fieldset", {}, h("legend", {}, T("Roles")), roleBox),
      stationField,
      formErr,
      h("div", { class: "row" }, submit, cancel),
      h("p", { class: "fine" }, T("A new user gets a temporary password, shown once, and chooses their own at the first sign-in.")));

    function syncStation() { stationField.hidden = !(checks.kitchen && checks.kitchen.checked); }
    function buildRoles() {
      clear(roleBox);
      checks = {};
      st.roles.forEach(function (r) {
        var input = h("input", { type: "checkbox", value: r.role, onchange: syncStation });
        checks[r.role] = input;
        roleBox.appendChild(h("label", { class: "opt", title: r.permissions.join(", ") }, input, r.role));
      });
    }
    function edit(id) {
      st.editing = id;
      var u = id && (st.users || []).filter(function (x) { return x.id === id; })[0];
      idIn.value = u ? u.id : ""; idIn.disabled = !!u;
      nameIn.value = u ? u.display_name : "";
      stationIn.value = u ? u.station : "";
      Object.keys(checks).forEach(function (r) { checks[r].checked = !!u && u.roles.indexOf(r) >= 0; });
      syncStation();
      formTitle.textContent = u ? T("Edit {name}", { name: u.id }) : T("New user");
      submit.textContent = u ? T("Save changes") : T("Create user");
      cancel.hidden = !u;
      formErr.textContent = "";
      if (u) { form.scrollIntoView({ block: "nearest" }); nameIn.focus(); }
    }

    function reload() {
      var jobs = [api("GET", "/api/admin/users"), api("GET", "/api/admin/events?limit=60" + (st.subject ? "&subject=" + encodeURIComponent(st.subject) : ""))];
      if (!st.roles) jobs.push(api("GET", "/api/admin/roles"));
      return Promise.all(jobs).then(function (rs) {
        st.users = rs[0].users; st.events = rs[1].events;
        if (rs[2]) { st.roles = rs[2].roles; buildRoles(); }
        changed();
      }, function () { /* shown as it was */ });
    }

    // an action on an account: POST to its path, show what came back
    function act(user, path, key, ok) {
      return function (btn) {
        run(key + ":" + user.id, btn, function () { return api("POST", "/api/admin/users/" + encodeURIComponent(user.id) + path, {}); }).then(function (v) {
          if (!v) return;
          if (v.temporary_password) st.temp = { id: user.id, password: v.temporary_password };
          toast(typeof ok === "function" ? ok(v) : ok, "ok");
          reload();
        });
      };
    }

    function drawTemp() {
      clear(tempBox);
      if (!st.temp) return;
      var pw = h("code", { class: "temp-pw", id: "temp-pw" }, st.temp.password);
      tempBox.appendChild(h("div", { class: "card temp stack", role: "status" },
        h("h2", {}, T("Temporary password for {name}", { name: st.temp.id })),
        pw,
        h("p", { class: "fine" }, T("This is shown once and is not kept anywhere in the clear. Give it to them in person; they must choose their own when they sign in.")),
        h("div", { class: "row" },
          h("button", { type: "button", class: "small", onclick: function () {
            if (navigator.clipboard && navigator.clipboard.writeText) {
              navigator.clipboard.writeText(st.temp.password).then(function () { toast(T("Copied."), "ok"); }, function () { toast(T("Could not copy: select it and copy by hand."), "bad"); });
            } else {
              var r = document.createRange(); r.selectNodeContents(pw); var sel = window.getSelection(); sel.removeAllRanges(); sel.addRange(r);
              toast(T("Selected: copy it by hand."), "ok");
            }
          } }, T("Copy")),
          h("button", { type: "button", class: "small ghost", onclick: function () { st.temp = null; changed(); } }, T("Hide")))));
    }

    function drawList() {
      clear(listBox);
      listBox.appendChild(h("h2", {}, T("Users")));
      if (!st.users) { listBox.appendChild(h("p", { class: "empty" }, T("Loading…"))); return; }
      listBox.appendChild(h("table", { class: "stackrows" }, h("thead", {}, h("tr", {}, h("th", {}, T("User")), h("th", {}, T("Roles")), h("th", {}, T("State")), h("th", {}, T("Last sign-in")), h("th", {}, ""))),
        h("tbody", {}, st.users.map(function (u) {
          var s = L.userState(u);
          var mine = u.id === S.me.id;
          var acts = [h("button", { type: "button", class: "small ghost", onclick: function () { edit(u.id); } }, T("Edit"))];
          if (u.locked_until && s.key === "locked") acts.push(h("button", { type: "button", class: "small ghost", onclick: function (e) { act(u, "/unlock", "unlock", T("Unlocked."))(e.currentTarget); } }, T("Unlock")));
          if (!mine) {
            acts.push(twoStep(h("button", { type: "button", class: "small ghost" }, u.disabled ? T("Enable") : T("Disable")), u.disabled ? T("Enable") : T("Disable"), act(u, u.disabled ? "/enable" : "/disable", u.disabled ? "enable" : "disable", u.disabled ? T("Enabled.") : T("Disabled. Their sessions have ended."))));
            acts.push(twoStep(h("button", { type: "button", class: "small ghost" }, T("Reset password")), T("Reset password"), act(u, "/reset-password", "reset", T("Password reset. Their sessions have ended."))));
          }
          if (u.active_sessions > 0) acts.push(twoStep(h("button", { type: "button", class: "small ghost" }, T("End sessions")), T("End sessions"), act(u, "/revoke-sessions", "revoke", function (v) { return T("{n} session(s) ended.", { n: v.revoked }); })));
          acts.push(h("button", { type: "button", class: "small ghost", onclick: function () { st.subject = u.id; reload(); } }, T("History")));
          return h("tr", {},
            h("td", {}, h("b", {}, u.display_name), h("small", { class: "fine" }, " " + u.id + (mine ? " (" + T("you") + ")" : ""))),
            h("td", { "data-label": T("Roles") }, u.roles.join(", "), u.station ? h("small", { class: "fine" }, " · " + u.station) : null),
            h("td", { "data-label": T("State") }, h("span", { class: "badge " + (s.key === "active" ? "ready" : s.key === "change" ? "placed" : "cancelled") }, s.label), u.active_sessions ? h("small", { class: "fine" }, " " + T("{n} signed in", { n: u.active_sessions })) : null),
            h("td", { "data-label": T("Last sign-in") }, u.last_login_at ? L.agoText(u.last_login_at) : T("never")),
            h("td", {}, h("div", { class: "acts" }, acts)));
        }))));
    }

    function drawEvents() {
      clear(eventsBox);
      eventsBox.appendChild(h("div", { class: "row" }, h("h2", { class: "grow" }, st.subject ? T("Activity of {name}", { name: st.subject }) : T("Activity")),
        st.subject ? h("button", { type: "button", class: "small ghost", onclick: function () { st.subject = ""; reload(); } }, T("Show everyone")) : null));
      if (!st.events) { eventsBox.appendChild(h("p", { class: "empty" }, T("Loading…"))); return; }
      if (!st.events.length) { eventsBox.appendChild(h("p", { class: "empty" }, T("Nothing yet."))); return; }
      eventsBox.appendChild(h("ul", { class: "events" }, st.events.map(function (e) {
        return h("li", {}, h("span", { class: "when" }, when(e.at)), " " + L.describeEvent(e), e.ip ? h("small", { class: "fine" }, " · " + e.ip) : null);
      })));
    }

    root.appendChild(h("h1", {}, T("Users")));
    // The table has the width of the page, so that the buttons of a row stay on one line.
    root.appendChild(h("div", { class: "stack" }, tempBox, listBox, h("div", { class: "cols two" }, eventsBox, form)));
    reload();
    function update() { drawTemp(); drawList(); drawEvents(); }
    return { update: update, reload: reload };
  }

  var SCREENS = { order: screenOrder, myorders: screenMyOrders, till: screenTill, kitchen: screenKitchen, manager: screenManager, stock: screenStock, users: screenUsers, account: screenAccount };

  // ---- start ----------------------------------------------------------------------------

  // ---- language -------------------------------------------------------------------------

  var LANG_KEY = "kitchen.lang";
  function savedLang() { try { return window.localStorage.getItem(LANG_KEY); } catch (e) { return null; } }
  function saveLang(l) { try { window.localStorage.setItem(LANG_KEY, l); } catch (e) { /* the choice lasts until the page closes */ } }

  // applyLanguage puts the text that the page itself has (the bar, the switch) in the language.
  function applyLanguage() {
    document.documentElement.lang = I18N.lang();
    $("signout").textContent = T("Sign out");
    $("tabs").setAttribute("aria-label", T("Screens"));
    $("langsw").setAttribute("aria-label", T("Language"));
    [].forEach.call($("langsw").querySelectorAll("button"), function (b) {
      b.setAttribute("aria-pressed", b.getAttribute("data-lang") === I18N.lang() ? "true" : "false");
    });
    $("who").textContent = S.me ? whoText(S.me) : T("not signed in");
    setLive(S.live);
  }

  // setLanguage changes the language and draws again what is on the screen. What was typed in a
  // sign-in form is kept.
  function setLanguage(lang) {
    if (lang === I18N.lang()) return;
    var typed = {};
    ["username", "password", "token", "pw-current", "pw-new", "pw-again"].forEach(function (id) { if ($(id)) typed[id] = $(id).value; });
    I18N.setLang(lang);
    saveLang(I18N.lang());
    applyLanguage();
    if (S.me && S.me.must_change_password) renderMustChange();
    else if (S.me) render();
    else renderSignIn("");
    Object.keys(typed).forEach(function (id) { if ($(id)) $(id).value = typed[id]; });
  }

  [].forEach.call($("langsw").querySelectorAll("button"), function (b) {
    b.addEventListener("click", function () { setLanguage(b.getAttribute("data-lang")); });
  });
  I18N.setLang(I18N.choose(savedLang(), window.navigator && window.navigator.language));
  applyLanguage();

  $("signout").addEventListener("click", userSignOut);
  // ages and the late marks change with time, not only with events; screens that read their own
  // data (accounts) read it again
  setInterval(function () {
    if (!S.current || !S.token) return;
    S.current.update();
    if (S.current.reload) S.current.reload();
  }, 20000);

  // How do people sign in here? With accounts, a session cookie may already be there.
  fetch("/api/auth").then(function (r) { return r.json(); }).then(function (a) { return a.mode; }, function () { return "token"; }).then(function (mode) {
    S.mode = mode === "password" ? "password" : "token";
    if (S.mode === "password") {
      api("GET", "/api/me", undefined, true).then(startSession, function () { renderSignIn(""); });
      return;
    }
    var saved = load("kitchen.token");
    if (saved) {
      start(saved).catch(function () { signOut(""); });
    } else {
      renderSignIn("");
    }
  });
})();
