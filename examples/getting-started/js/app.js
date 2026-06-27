/* ──────────────────────────────────────────────────────────────────────────
   kotoji · Getting Started — progressive enhancement
   Runs under the data-plane CSP `script-src 'self' 'unsafe-inline'`: this file
   is loaded from the site's own origin (allowed by 'self'), uses no external
   scripts, and attaches behaviour via addEventListener (no eval, no remote
   fetch). The page is fully usable with JS disabled — this only enhances it.
   ────────────────────────────────────────────────────────────────────────── */
(function () {
  "use strict";

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
      btn.textContent = "Copy";
      btn.setAttribute("aria-label", "Copy code to clipboard");

      btn.addEventListener("click", function () {
        var text = pre.innerText;
        copyText(text).then(function (ok) {
          btn.textContent = ok ? "Copied!" : "Press Ctrl+C";
          btn.classList.toggle("copied", ok);
          window.setTimeout(function () {
            btn.textContent = "Copy";
            btn.classList.remove("copied");
          }, 1600);
        });
      });

      block.appendChild(btn);
    });
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
    stampYear();
    wireCopyButtons();
    wireActiveNav();
  });
})();
