"use strict";
// Tests of the language of the screens: that every sentence of the code has a Vietnamese text and
// every Vietnamese text is used, that the two agree about what is filled in, and that the
// functions that write errors and times speak the language that was chosen.
// Run with: node --test internal/orderservice/web/
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const I18N = require("./i18n.js");
const vi = require("./vi.js");
I18N.register("vi", vi);
const L = require("./logic.js");

// sentencesOf lists what a script passes to T("...").
function sentencesOf(file) {
  const src = fs.readFileSync(path.join(__dirname, file), "utf8");
  const out = new Set();
  const re = /\bT\("((?:[^"\\]|\\.)*)"/g;
  let m;
  while ((m = re.exec(src))) out.add(JSON.parse('"' + m[1] + '"'));
  return out;
}

const used = new Set([...sentencesOf("app.js"), ...sentencesOf("logic.js"), ...sentencesOf("admin.js"), ...sentencesOf("warehouse.js"), ...sentencesOf("shifts.js"), ...sentencesOf("board.js")]);

const placeholders = (s) => [...s.matchAll(/\{(\w+)\}/g)].map((m) => m[1]).sort().join(",");

test.afterEach(() => I18N.setLang("en"));

test("every sentence of the code has a Vietnamese text", () => {
  const missing = [...used].filter((s) => !Object.prototype.hasOwnProperty.call(vi.ui, s));
  assert.deepEqual(missing, [], "sentences with no entry in vi.js (ui)");
});

test("every Vietnamese text of the screens is used by the code", () => {
  const stale = Object.keys(vi.ui).filter((s) => !used.has(s));
  assert.deepEqual(stale, [], "entries of vi.js (ui) that no T() asks for");
});

test("a Vietnamese text is not empty, and fills in the same things as its English", () => {
  for (const section of ["ui", "server", "words"]) {
    for (const [en, text] of Object.entries(vi[section])) {
      assert.ok(typeof text === "string" && text.trim() !== "", section + ": empty text for " + en);
      assert.equal(placeholders(text), placeholders(en), section + ": the {names} of " + JSON.stringify(en) + " differ");
    }
  }
});

test("an entry is in one section only, so that which one wins never matters", () => {
  const seen = new Map();
  for (const section of ["ui", "server", "words"]) {
    for (const k of Object.keys(vi[section])) {
      if (seen.has(k)) assert.fail(JSON.stringify(k) + " is in " + seen.get(k) + " and in " + section);
      seen.set(k, section);
    }
  }
});

test("a sentence is given in English until another language is chosen, and falls back to English", () => {
  assert.equal(I18N.t("Place order"), "Place order");
  I18N.setLang("vi");
  assert.equal(I18N.t("Place order"), "Đặt món");
  assert.equal(I18N.t("A sentence that nobody translated"), "A sentence that nobody translated");
  assert.equal(I18N.has("Place order"), true);
  assert.equal(I18N.has("A sentence that nobody translated"), false);
  I18N.setLang("fr"); // a language that is not there is English
  assert.equal(I18N.lang(), "en");
  assert.equal(I18N.has("A sentence that nobody translated"), true);
});

test("the things that change are filled in, in both languages, and an unknown name is left alone", () => {
  assert.equal(I18N.t("Order #{n} paid.", { n: 7 }), "Order #7 paid.");
  I18N.setLang("vi");
  assert.equal(I18N.t("Order #{n} paid.", { n: 7 }), "Đã thanh toán đơn #7.");
  assert.equal(I18N.t("Order #{n} paid.", {}), "Đã thanh toán đơn #{n}.");
  assert.equal(I18N.t("Order #{n} paid.", { n: 0 }), "Đã thanh toán đơn #0.", "zero is a value");
});

test("the language is the person's choice, else the browser's, else English", () => {
  assert.equal(I18N.choose("en", "vi-VN"), "en");
  assert.equal(I18N.choose("vi", "en-US"), "vi");
  assert.equal(I18N.choose(null, "vi-VN"), "vi");
  assert.equal(I18N.choose(null, "vi"), "vi");
  assert.equal(I18N.choose(null, "en-GB"), "en");
  assert.equal(I18N.choose("klingon", "fr-FR"), "en");
  assert.equal(I18N.choose(undefined, undefined), "en");
  I18N.setLang("vi");
  assert.equal(I18N.locale(), "vi-VN");
  I18N.setLang("en");
  assert.equal(I18N.locale(), undefined);
});

test("the names that come from data are translated by value, and unknown ones show as they are", () => {
  I18N.setLang("vi");
  for (const s of ["placed", "paid", "preparing", "ready", "completed", "cancelled", "pending", "started"]) assert.notEqual(I18N.t(s), s, "status " + s);
  for (const r of ["customer", "cashier", "kitchen", "runner", "manager", "storekeeper", "admin"]) assert.notEqual(I18N.t(r), r, "role " + r);
  for (const p of ["cash", "card", "transfer"]) assert.notEqual(I18N.t(p), p, "payment " + p);
  assert.equal(I18N.t("desserts"), "desserts", "a category of the venue's own");
});

test("an error of the service is said in the language of the person", () => {
  I18N.setLang("vi");
  assert.match(L.describeError(409, { code: "out_of_stock", error: "x", shortages: [{ ingredient: "bread", need: 2, have: 0 }] }), /^Không đủ hàng trong kho: bread \(cần 2, còn 0\)$/);
  assert.equal(L.describeError(422, { code: "cart", error: "x", lines: [{ line: 2, error: "menu: sold out: Tea" }] }), "Dòng 2: hết hàng: Tea");
  assert.equal(L.describeError(401, { code: "bad_credentials", error: "Wrong login name or password." }), "Sai tên đăng nhập hoặc mật khẩu.");
  assert.match(L.describeError(429, { code: "locked", error: "x", retry_after: 901 }), /thử lại sau 16 phút/);
  assert.match(L.describeError(429, { code: "locked", error: "x", retry_after: 30 }), /thử lại sau 1 phút/, "never zero minutes");
  assert.equal(L.describeError(400, { code: "weak_password", error: "a password is at least 10 characters" }), "Mật khẩu phải có ít nhất 10 ký tự.", "a fixed message of the service");
  assert.equal(L.describeError(409, { code: "conflict", error: "that login name is taken" }), "Tên đăng nhập này đã có người dùng.");
  assert.equal(L.describeError(500, null), "Có lỗi xảy ra (500)");
});

test("a message of the service that has no translation keeps its English, after a sentence in the language", () => {
  I18N.setLang("vi");
  assert.equal(L.describeError(403, { code: "forbidden", error: "order: this person may not do that" }), "Bạn không có quyền làm việc này. (order: this person may not do that)");
  assert.equal(L.describeError(409, { code: "conflict", error: "order: wrong state" }), "Hiện chưa thể làm việc này. (order: wrong state)");
  I18N.setLang("en");
  assert.equal(L.describeError(403, { code: "forbidden", error: "order: this person may not do that" }), "order: this person may not do that", "in English the message is as it was");
});

test("times and states are written in the language", () => {
  const now = Date.parse("2026-11-20T10:00:00Z");
  const at = (mins) => new Date(now - mins * 60000).toISOString();
  assert.equal(L.ageText(at(0), now), "just now");
  assert.equal(L.ageText(at(5), now), "5 min");
  assert.equal(L.ageText(at(83), now), "1 h 23 min");
  assert.equal(L.agoText(at(5), now), "5 min ago");
  I18N.setLang("vi");
  assert.equal(L.ageText(at(0), now), "vừa xong");
  assert.equal(L.ageText(at(5), now), "5 phút");
  assert.equal(L.ageText(at(83), now), "1 giờ 23 phút");
  assert.equal(L.agoText(at(5), now), "5 phút trước");
  assert.equal(L.agoText(at(0), now), "vừa xong");
  assert.equal(L.userState({ disabled: true }, now).label, "đã tắt");
  assert.equal(L.userState({ must_change_password: true }, now).label, "phải đổi mật khẩu");
  assert.equal(L.statusLabel("placed"), "Chờ thanh toán");
  assert.equal(L.shortAgent("curl/8.4.0"), "curl");
  assert.equal(L.shortAgent("Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/124.0 Safari/537.36"), "Chrome trên Windows");
});

test("the trail and the password rules are written in the language", () => {
  I18N.setLang("vi");
  assert.equal(L.describeEvent({ action: "user.disable", actor: "admin-1", subject: "thu-ngan" }), "admin-1 đã tắt thu-ngan");
  assert.equal(L.describeEvent({ action: "login.failure", actor: "", subject: "thu-ngan" }), "Sai mật khẩu của thu-ngan");
  assert.equal(L.describeEvent({ action: "login.unknown", actor: "", subject: "", detail: { login: "nobody" } }), "Đăng nhập bằng tên không tồn tại: nobody");
  assert.equal(L.describeEvent({ action: "shift.open", actor: "cash-1", subject: "till-2" }), "cash-1 shift.open till-2", "an action that is not known is still shown");
  assert.equal(L.passwordProblem("short", "short", "x"), "Dùng ít nhất 10 ký tự.");
  assert.equal(L.passwordProblem("a long enough one", "another long one", "x"), "Hai mật khẩu không giống nhau.");
  assert.equal(L.groupProblem({ id: "size", name: "Size", min: 1, max: 1 }, {}), "Chọn một: Size");
});
