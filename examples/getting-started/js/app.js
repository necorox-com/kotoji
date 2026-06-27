/* ──────────────────────────────────────────────────────────────────────────
   kotoji · Getting Started — progressive enhancement
   Runs under the data-plane CSP `script-src 'self' 'unsafe-inline'`: this file
   is loaded from the site's own origin (allowed by 'self'), uses no external
   scripts, and attaches behaviour via addEventListener (no eval, no remote
   fetch). The page is fully usable with JS disabled — this only enhances it.
   ────────────────────────────────────────────────────────────────────────── */
(function () {
  "use strict";

  // localStorage key + values for the persisted language choice.
  var LANG_KEY = "kotoji-guide-lang";
  var LANG_JA = "ja";
  var LANG_EN = "en";

  // (0) Language toggle — Japanese is primary, English is a secondary toggle.
  //     The page authors both languages inline (.lang-ja / .lang-en); flipping
  //     `body.lang-en` swaps which set CSS shows. We persist the choice in
  //     localStorage and re-apply it on every load, and we also keep
  //     document.documentElement.lang in sync for assistive tech & search.
  //     CSP: this only reads localStorage and toggles a class via
  //     addEventListener — no eval, no remote fetch — so it runs cleanly under
  //     `script-src 'self' 'unsafe-inline'`. With JS disabled the page stays in
  //     Japanese (the default), so nothing is lost.
  function readLang() {
    try {
      var v = window.localStorage.getItem(LANG_KEY);
      return v === LANG_EN ? LANG_EN : LANG_JA; // default to Japanese
    } catch (e) {
      return LANG_JA; // private mode / storage blocked → default Japanese
    }
  }

  function saveLang(lang) {
    try {
      window.localStorage.setItem(LANG_KEY, lang);
    } catch (e) {
      /* storage unavailable — toggle still works for this session */
    }
  }

  function applyLang(lang) {
    var isEn = lang === LANG_EN;
    document.body.classList.toggle("lang-en", isEn);
    document.documentElement.lang = isEn ? "en" : "ja";

    // Reflect current state on every toggle button for screen readers.
    var btns = document.querySelectorAll("[data-lang-toggle]");
    btns.forEach(function (btn) {
      // The button always shows the language it switches TO, set as plain text
      // so the control can never collapse to zero width (it must not depend on
      // the .lang-ja/.lang-en content-swap that the body sections use).
      btn.textContent = isEn ? "日本語" : "English";
      btn.setAttribute("aria-pressed", isEn ? "true" : "false");
      btn.setAttribute(
        "aria-label",
        isEn ? "Switch to Japanese" : "英語に切り替える"
      );
    });
  }

  function wireLangToggle() {
    applyLang(readLang()); // honour the stored choice on load

    var btns = document.querySelectorAll("[data-lang-toggle]");
    btns.forEach(function (btn) {
      btn.addEventListener("click", function () {
        var next = document.body.classList.contains("lang-en")
          ? LANG_JA
          : LANG_EN;
        saveLang(next);
        applyLang(next);
      });
    });
  }

  // (1) Footer year stamp — keeps the copyright current with no rebuild.
  function stampYear() {
    var el = document.querySelector("[data-year]");
    if (el) el.textContent = String(new Date().getFullYear());
  }

  // (2) Copy-to-clipboard for every code block that opts in via .code blocks.
  //     We inject the button in JS so non-JS users never see a dead control.
  function wireCopyButtons() {
    var blocks = document.querySelectorAll(".code");
    blocks.forEach(function (block) {
      var pre = block.querySelector("pre");
      if (!pre) return;

      var btn = document.createElement("button");
      btn.type = "button";
      btn.className = "copy-btn";
      // Bilingual label: build JA + EN spans so the active language shows the
      // right word via the same .lang-ja/.lang-en CSS the rest of the page uses.
      setBtnLabel(btn, "コピー", "Copy");
      btn.setAttribute("aria-label", "コードをクリップボードにコピー");

      btn.addEventListener("click", function () {
        var text = pre.innerText;
        copyText(text).then(function (ok) {
          if (ok) {
            setBtnLabel(btn, "コピーしました！", "Copied!");
          } else {
            setBtnLabel(btn, "Ctrl+C を押してください", "Press Ctrl+C");
          }
          btn.classList.toggle("copied", ok);
          window.setTimeout(function () {
            setBtnLabel(btn, "コピー", "Copy");
            btn.classList.remove("copied");
          }, 1600);
        });
      });

      block.appendChild(btn);
    });
  }

  // Sets a button's visible label as paired JA/EN spans (no innerHTML — we
  // build nodes so this stays safe under any CSP and never injects markup).
  function setBtnLabel(btn, ja, en) {
    btn.textContent = "";
    var jaEl = document.createElement("span");
    jaEl.className = "lang-ja";
    jaEl.textContent = ja;
    var enEl = document.createElement("span");
    enEl.className = "lang-en";
    enEl.textContent = en;
    btn.appendChild(jaEl);
    btn.appendChild(enEl);
  }

  // Clipboard with a legacy fallback (older browsers / non-secure contexts).
  function copyText(text) {
    if (navigator.clipboard && window.isSecureContext) {
      return navigator.clipboard.writeText(text).then(
        function () { return true; },
        function () { return legacyCopy(text); }
      );
    }
    return Promise.resolve(legacyCopy(text));
  }

  function legacyCopy(text) {
    try {
      var ta = document.createElement("textarea");
      ta.value = text;
      ta.setAttribute("readonly", "");
      ta.style.position = "absolute";
      ta.style.left = "-9999px";
      document.body.appendChild(ta);
      ta.select();
      var ok = document.execCommand("copy");
      document.body.removeChild(ta);
      return ok;
    } catch (e) {
      return false;
    }
  }

  // (3) Active in-page nav highlight — highlights the section the reader is on.
  //     Uses IntersectionObserver where available; degrades to no-op otherwise.
  function wireActiveNav() {
    var links = Array.prototype.slice.call(
      document.querySelectorAll('.nav a[href^="#"]')
    );
    if (!links.length || !("IntersectionObserver" in window)) return;

    var byId = {};
    links.forEach(function (a) {
      var id = a.getAttribute("href").slice(1);
      if (id) byId[id] = a;
    });

    var observer = new IntersectionObserver(
      function (entries) {
        entries.forEach(function (entry) {
          if (!entry.isIntersecting) return;
          var link = byId[entry.target.id];
          if (!link) return;
          links.forEach(function (a) { a.classList.remove("active"); });
          link.classList.add("active");
        });
      },
      // Trigger when a section is near the top third of the viewport.
      { rootMargin: "-40% 0px -55% 0px", threshold: 0 }
    );

    Object.keys(byId).forEach(function (id) {
      var section = document.getElementById(id);
      if (section) observer.observe(section);
    });
  }

  document.addEventListener("DOMContentLoaded", function () {
    wireLangToggle();
    stampYear();
    wireCopyButtons();
    wireActiveNav();
  });
})();
