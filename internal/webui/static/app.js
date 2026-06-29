// Progressive enhancement for Zumble-Zay. Loaded from /static (same-origin), so
// it works under the strict CSP (default-src 'self') with no inline script.
(function () {
  "use strict";

  // Confirm submission of any form carrying a data-confirm attribute (the
  // per-item Hide button).
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

  // Per-item assistive conversation (docs/adr/0019): post a turn, then poll for
  // the reply, which a spawned converse runtime writes back asynchronously.
  var chat = document.getElementById("zz-chat");
  var chatForm = document.getElementById("zz-chat-form");
  if (!chat || !chatForm) {
    return;
  }
  var itemId = chat.getAttribute("data-item-id");
  var textarea = chatForm.querySelector("textarea");
  var button = chatForm.querySelector("button");

  // Messages already persisted (server-rendered). A turn is complete once the
  // server thread has grown by two (the user message + the reply) and ends with
  // an agent message.
  var serverCount = chat.querySelectorAll(".zz-msg").length;

  var POLL_MS = 1800;
  var MAX_POLLS = 80; // ~2.4 min, covering the converse job budget
  var READ_DELAY_MS = 3000; // dwell before a reply counts as read (ADR 0018)

  // Register a read receipt for the latest reply once it has been on screen long
  // enough to have been seen; this clears the radar's unread cue. Fire-and-forget.
  function scheduleRead() {
    window.setTimeout(function () {
      fetch("/api/thread/read?id=" + encodeURIComponent(itemId), { method: "POST" }).catch(function () {});
    }, READ_DELAY_MS);
  }

  function addMessage(role, text) {
    var el = document.createElement("div");
    el.className = "zz-msg zz-msg--" + role;
    el.textContent = text;
    chat.appendChild(el);
    el.scrollIntoView({ block: "nearest" });
    return el;
  }

  function done() {
    button.disabled = false;
    textarea.focus();
  }

  function poll(pending, attempts) {
    fetch("/api/thread?id=" + encodeURIComponent(itemId), {
      headers: { Accept: "application/json" }
    })
      .then(function (res) {
        if (!res.ok) {
          throw new Error("poll failed");
        }
        return res.json();
      })
      .then(function (data) {
        var msgs = (data && data.messages) || [];
        var last = msgs[msgs.length - 1];
        if (msgs.length > serverCount && last && last.role === "agent") {
          // last.html is rendered and sanitized server-side (goldmark +
          // bluemonday), so it is safe to inject; fall back to plain text.
          if (last.html) {
            pending.innerHTML = last.html;
          } else {
            pending.textContent = last.content;
          }
          pending.scrollIntoView({ block: "nearest" });
          serverCount = msgs.length;
          done();
          scheduleRead();
          return;
        }
        if (attempts + 1 >= MAX_POLLS) {
          pending.textContent = "Still working\u2026 reload this page to see the reply.";
          done();
          return;
        }
        window.setTimeout(function () {
          poll(pending, attempts + 1);
        }, POLL_MS);
      })
      .catch(function () {
        pending.textContent = "Sorry, the assistant is unavailable right now.";
        done();
      });
  }

  chatForm.addEventListener("submit", function (e) {
    e.preventDefault();
    var text = (textarea.value || "").trim();
    if (!text) {
      return;
    }
    textarea.value = "";
    button.disabled = true;
    addMessage("user", text);
    var pending = addMessage("agent", "\u2026"); // ellipsis while thinking

    fetch("/api/thread?id=" + encodeURIComponent(itemId), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ content: text })
    })
      .then(function (res) {
        if (!res.ok) {
          throw new Error("request failed");
        }
        // Accepted: the reply is computed out-of-process; poll for it.
        window.setTimeout(function () {
          poll(pending, 0);
        }, POLL_MS);
      })
      .catch(function () {
        pending.textContent = "Sorry, the assistant is unavailable right now.";
        done();
      });
  });

  // Reconstitute a pending turn: if we land on the page mid-response (the thread
  // ends with a user message and no reply yet), show the spinner, re-ensure a
  // converse turn (idempotent — deduped if one is running, restarted if it
  // crashed), then poll so the reply auto-populates without a manual refresh.
  var rendered = chat.querySelectorAll(".zz-msg");
  var lastRendered = rendered[rendered.length - 1];
  if (lastRendered && lastRendered.classList.contains("zz-msg--user")) {
    button.disabled = true;
    var resumed = addMessage("agent", "\u2026");
    fetch("/api/thread/resume?id=" + encodeURIComponent(itemId), { method: "POST" })
      .catch(function () {})
      .then(function () {
        poll(resumed, 0);
      });
  } else if (lastRendered && lastRendered.classList.contains("zz-msg--agent")) {
    // Already-rendered reply: dwell, then mark it read so the radar cue clears.
    scheduleRead();
  }
})();
