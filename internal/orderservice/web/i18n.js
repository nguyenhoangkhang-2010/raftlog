// The language of the screens. The text in the code is English, and is also the key: t("Place
// order") says "Place order" in English and what the Vietnamese dictionary (vi.js) has for that
// sentence in Vietnamese. A sentence that has no entry is shown in English, so a missing
// translation is a visible gap and never a blank or an error. A test (i18n.test.js) fails the build
// when a sentence of the code is not in the dictionary, or an entry is not used.
//
// A sentence with something in it that changes is written with names in braces, and the values
// are given: t("Order #{n} placed.", {n: 5}). Both languages use the same names.
//
// The language is chosen by the person (kept in the browser, per browser), else by the browser's
// own language, else English. It is a global script, loaded before logic.js and app.js, and a
// module for the tests.
(function (root, factory) {
  if (typeof module === "object" && module.exports) module.exports = factory();
  else root.KitchenI18n = factory();
})(typeof self !== "undefined" ? self : this, function () {
  "use strict";

  var LANGS = ["en", "vi"];
  var dictionaries = {}; // language -> {sentence: translation}
  var current = "en";

  // register adds the dictionary of a language: its sections (the sentences of the screens, the
  // words that come from data, the messages of the service) are merged into one.
  function register(lang, sections) {
    var all = {};
    Object.keys(sections).forEach(function (name) {
      Object.keys(sections[name]).forEach(function (k) { all[k] = sections[name][k]; });
    });
    dictionaries[lang] = all;
  }

  function fill(text, params) {
    if (!params) return text;
    return text.replace(/\{(\w+)\}/g, function (whole, name) { return params[name] === undefined ? whole : String(params[name]); });
  }

  // t translates a sentence of the screens.
  function t(sentence, params) {
    var d = dictionaries[current];
    return fill((d && Object.prototype.hasOwnProperty.call(d, sentence) && d[sentence]) || sentence, params);
  }

  // has says whether the current language has its own text for a sentence (English always does).
  function has(sentence) {
    if (current === "en") return true;
    var d = dictionaries[current];
    return !!d && Object.prototype.hasOwnProperty.call(d, sentence);
  }

  function setLang(lang) { current = LANGS.indexOf(lang) >= 0 ? lang : "en"; return current; }
  function lang() { return current; }

  // locale is the name that Date and Intl want, so that times are written the way people read them.
  function locale() { return current === "vi" ? "vi-VN" : undefined; }

  // choose picks the language of a person who has not chosen: the browser's, if it is one we have.
  function choose(stored, browser) {
    if (LANGS.indexOf(stored) >= 0) return stored;
    var b = String(browser || "").toLowerCase();
    return b.indexOf("vi") === 0 ? "vi" : "en";
  }

  return { t: t, has: has, register: register, setLang: setLang, lang: lang, locale: locale, choose: choose, LANGS: LANGS };
});
