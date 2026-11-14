"use strict";
// Tests of what the forms of the managers turn into documents, and back: a document that is opened
// and saved without a change must come out as it went in, and what a form lets through must be
// what the service reads. Run with: node --test internal/orderservice/web/
const test = require("node:test");
const assert = require("node:assert/strict");
const I18N = require("./i18n.js");
const vi = require("./vi.js");
I18N.register("vi", vi);
const Admin = require("./admin.js");
const P = Admin.pure(I18N.t);

test.afterEach(() => I18N.setLang("en"));

const coffee = {
  id: "coffee", name: "Coffee", category: "drinks", station: "drinks", price: 35000,
  recipe: { beans: 20, milk: 150 },
  time_prices: [{ days: ["mon", "tue"], from: "15:00", to: "17:00", price: 25000 }, { from: "06:00", to: "08:00", price: 30000 }],
  groups: [{ id: "size", name: "Size", min: 1, max: 1, options: [
    { id: "s", name: "Small", delta: 0 },
    { id: "l", name: "Large", delta: 10000, recipe: { beans: 10 } },
    { id: "d", name: "Discount cup", delta: -2000 },
  ] }],
};

test("an item that is opened and saved without a change comes out as it went in", () => {
  const r = P.itemFromDraft(P.draftOfItem(coffee));
  assert.equal(r.err, undefined);
  assert.deepEqual(r.item, coffee);
  const hidden = { id: "tea", name: "Tea", category: "drinks", station: "drinks", price: 20000, active: false };
  assert.deepEqual(P.itemFromDraft(P.draftOfItem(hidden)).item, hidden, "a hidden item stays hidden, and an item for sale has no active key");
  assert.equal("active" in P.itemFromDraft(P.draftOfItem(coffee)).item, false);
});

test("the form is edited as text and the draft is what is sent", () => {
  const d = P.draftOfItem(coffee);
  d.price = " 40000 ";
  d.name = "  Iced coffee ";
  d.recipe.push({ ing: "ice", qty: "30" });
  d.recipe.push({ ing: "", qty: "" }); // an empty line is nothing
  d.time_prices[0].days = ["tue", "mon"]; // the days keep their order of the week
  const it = P.itemFromDraft(d).item;
  assert.equal(it.price, 40000);
  assert.equal(it.name, "Iced coffee");
  assert.deepEqual(it.recipe, { beans: 20, ice: 30, milk: 150 });
  assert.deepEqual(it.time_prices[0].days, ["mon", "tue"]);
});

test("what the form can see to be wrong is refused before it is sent, in the language", () => {
  const bad = (change) => { const d = P.draftOfItem(coffee); change(d); return P.itemFromDraft(d).err; };
  assert.equal(bad((d) => { d.id = " "; }), "Give the item an id.");
  assert.equal(bad((d) => { d.name = ""; }), "Give the item a name.");
  assert.equal(bad((d) => { d.category = ""; }), "Give the item a category.");
  assert.equal(bad((d) => { d.station = ""; }), "Give the item a station.");
  for (const price of ["", "abc", "12.5", "-1", "1e3", "10 000"]) assert.match(bad((d) => { d.price = price; }), /whole number of đồng/, "price " + JSON.stringify(price));
  assert.match(bad((d) => { d.recipe[0].qty = "0"; }), /whole quantity above zero/);
  assert.match(bad((d) => { d.recipe[0].qty = "1.5"; }), /whole quantity above zero/);
  assert.match(bad((d) => { d.recipe[0].ing = ""; }), /whole quantity above zero/);
  assert.match(bad((d) => { d.recipe.push({ ing: "beans", qty: "5" }); }), /used twice in one recipe: beans/);
  assert.match(bad((d) => { d.time_prices[0].to = ""; }), /needs a start, an end/);
  assert.match(bad((d) => { d.groups[0].name = ""; }), /group of options needs an id and a name/);
  assert.match(bad((d) => { d.groups[0].max = "x"; }), /least and the most/);
  assert.match(bad((d) => { d.groups[0].options[0].id = ""; }), /option needs an id and a name/);
  assert.match(bad((d) => { d.groups[0].options[0].delta = "1.5"; }), /extra price of an option/);
  I18N.setLang("vi");
  assert.equal(bad((d) => { d.name = ""; }), "Hãy đặt tên cho món.");
  assert.match(bad((d) => { d.price = "x"; }), /số đồng nguyên/);
});

test("an extra price may be below zero, a price may not", () => {
  const d = P.draftOfItem(coffee);
  d.groups[0].options[0].delta = "-500";
  assert.equal(P.itemFromDraft(d).item.groups[0].options[0].delta, -500);
  d.price = "-500";
  assert.ok(P.itemFromDraft(d).err);
});

test("an item with nothing of the optional parts has none of them in the document", () => {
  const d = P.newItemDraft();
  Object.assign(d, { id: "water", name: "Water", category: "drinks", station: "drinks", price: "10000" });
  assert.deepEqual(P.itemFromDraft(d).item, { id: "water", name: "Water", category: "drinks", station: "drinks", price: 10000 });
});

test("a combo is opened and saved without a change, and its slots are checked", () => {
  const combo = { id: "set", name: "Set", price: 60000, slots: [{ name: "Drink", items: ["coffee", "tea"] }, { name: "Food", items: ["toastie"] }] };
  assert.deepEqual(P.comboFromDraft(P.draftOfCombo(combo)).combo, combo);
  const d = P.draftOfCombo(combo);
  d.slots[1].items = [];
  assert.match(P.comboFromDraft(d).err, /name and at least one item/);
  d.slots[1].items = ["toastie"]; d.price = "free";
  assert.match(P.comboFromDraft(d).err, /whole number of đồng/);
  const off = P.draftOfCombo({ ...combo, active: false });
  assert.equal(P.comboFromDraft(off).combo.active, false);
});

const promo = { id: "ten", name: "Ten percent", kind: "percent_off", percent_bp: 1000, code: "TEN", max_discount: 20000, min_subtotal: 50000, single_use: true, priority: 2,
  scope: { categories: ["drinks"], items: ["toastie"] } };

test("a promotion that is opened and saved without a change comes out as it went in", () => {
  assert.deepEqual(P.promoFromDraft(P.draftOfPromo(promo)).promo, promo);
  const buy = { id: "b2g1", name: "Buy two", kind: "buy_x_get_y", buy: 2, free: 1 };
  assert.deepEqual(P.promoFromDraft(P.draftOfPromo(buy)).promo, buy);
  const fixed = { id: "off5", name: "Off five", kind: "fixed_off", amount: 5000, exclusive: true };
  assert.deepEqual(P.promoFromDraft(P.draftOfPromo(fixed)).promo, fixed);
});

test("a percentage is in basis points, with at most two decimals, above 0 and up to 100", () => {
  const at = (pct) => { const d = P.draftOfPromo(promo); d.percent = pct; return P.promoFromDraft(d); };
  assert.equal(at("12.5").promo.percent_bp, 1250);
  assert.equal(at("12,5").promo.percent_bp, 1250, "a comma is a decimal point to many people");
  assert.equal(at("100").promo.percent_bp, 10000);
  assert.equal(at("0.01").promo.percent_bp, 1);
  for (const bad of ["", "0", "-5", "100.01", "abc", "12.345"]) assert.match(at(bad).err, /percentage is above 0/, JSON.stringify(bad));
});

test("the fields of a kind are the only ones that are kept", () => {
  const d = P.draftOfPromo(promo);
  d.kind = "fixed_off"; d.amount = "5000"; // a percentage typed before the kind was changed is left behind
  const p = P.promoFromDraft(d).promo;
  assert.equal(p.amount, 5000);
  assert.equal("percent_bp" in p, false);
  assert.equal("max_discount" in p, false);
  d.kind = "buy_x_get_y";
  assert.match(P.promoFromDraft(d).err, /how many to buy/);
  d.buy = "2"; d.free = "1";
  assert.equal(P.promoFromDraft(d).promo.buy, 2);
  d.kind = "fixed_off"; d.amount = "";
  assert.match(P.promoFromDraft(d).err, /amount taken off/);
});

test("the times of a promotion are the moments that the person meant in their own time", () => {
  const d = P.draftOfPromo(promo);
  d.valid_from = "2026-12-01T08:00"; d.valid_to = "2026-12-31T23:30";
  const p = P.promoFromDraft(d).promo;
  assert.equal(Date.parse(p.valid_from), new Date(2026, 11, 1, 8, 0).getTime());
  assert.equal(Date.parse(p.valid_to), new Date(2026, 11, 31, 23, 30).getTime());
  assert.equal(P.toLocalInput(p.valid_from), "2026-12-01T08:00", "what is shown is what was typed");
  assert.equal(P.toLocalInput(""), "");
  assert.equal(P.toLocalInput("not a time"), "");
  assert.equal(P.fromLocalInput(""), "");
  assert.equal(P.fromLocalInput("garbage"), null);
  d.valid_from = "garbage";
  assert.match(P.promoFromDraft(d).err, /date and time/);
});

test("a promotion with no scope has none in the document", () => {
  const d = P.draftOfPromo({ id: "all", name: "All", kind: "percent_off", percent_bp: 500 });
  const p = P.promoFromDraft(d).promo;
  assert.equal("scope" in p, false);
  assert.deepEqual(p, { id: "all", name: "All", kind: "percent_off", percent_bp: 500 });
});

test("recipes are whole quantities above zero, one line for each ingredient", () => {
  assert.deepEqual(P.recipeOf([]), { obj: {}, n: 0 });
  assert.deepEqual(P.recipeOf([{ ing: " beans ", qty: " 20 " }, { ing: "", qty: "" }]), { obj: { beans: 20 }, n: 1 });
  assert.ok(P.recipeOf([{ ing: "beans", qty: "" }]).err);
  assert.ok(P.recipeOf([{ ing: "", qty: "5" }]).err);
  assert.deepEqual(P.rowsOf({ milk: 150, beans: 20 }), [{ ing: "beans", qty: "20" }, { ing: "milk", qty: "150" }], "sorted, so a form does not reorder between loads");
  assert.deepEqual(P.rowsOf(undefined), []);
});

test("a draft is a copy: changing it does not change the document it came from", () => {
  const d = P.draftOfItem(coffee);
  d.groups[0].options[0].name = "Changed";
  d.time_prices[0].days.push("sun");
  d.recipe[0].qty = "99";
  assert.equal(coffee.groups[0].options[0].name, "Small");
  assert.deepEqual(coffee.time_prices[0].days, ["mon", "tue"]);
  assert.equal(coffee.recipe.beans, 20);
});
