// Progressive enhancement for Zumble-Zay. Loaded from /static (same-origin), so
// it works under the strict CSP (default-src 'self') with no inline script.
//
// It intercepts submission of any form carrying a data-confirm attribute and
// asks for confirmation first — used by the per-item "Hide" button.
(function () {
  "use strict";
  document.addEventListener("submit", function (e) {
    var form = e.target;
    if (!form || typeof form.getAttribute !== "function") {
      return;
    }
    var message = form.getAttribute("data-confirm");
    if (message && !window.confirm(message)) {
      e.preventDefault();
    }
  });
})();
