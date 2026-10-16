// Tasuki landing page: mobile nav, code tabs, copy buttons, syntax colouring and the hero animation.
// No dependencies; the page reads fine with JavaScript off.

(() => {
  "use strict";

  const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;

  // Mobile nav
  const toggle = document.querySelector(".nav-toggle");
  const links = document.getElementById("nav-links");
  if (toggle && links) {
    const setOpen = (open) => {
      links.classList.toggle("open", open);
      toggle.setAttribute("aria-expanded", String(open));
    };
    toggle.addEventListener("click", () => setOpen(!links.classList.contains("open")));
    links.addEventListener("click", (e) => {
      if (e.target.closest("a")) setOpen(false);
    });
    document.addEventListener("keydown", (e) => {
      if (e.key === "Escape") setOpen(false);
    });
  }

  // Tabs (WAI-ARIA tabs pattern: arrows, Home and End move between tabs)
  document.querySelectorAll("[data-tabs]").forEach((root) => {
    const tabs = Array.from(root.querySelectorAll('[role="tab"]'));
    const select = (tab) => {
      tabs.forEach((t) => {
        const selected = t === tab;
        t.setAttribute("aria-selected", String(selected));
        t.tabIndex = selected ? 0 : -1;
        document.getElementById(t.getAttribute("aria-controls")).hidden = !selected;
      });
    };
    tabs.forEach((tab, i) => {
      tab.addEventListener("click", () => select(tab));
      tab.addEventListener("keydown", (e) => {
        const moves = { ArrowRight: i + 1, ArrowLeft: i - 1, Home: 0, End: tabs.length - 1 };
        if (!(e.key in moves)) return;
        e.preventDefault();
        const next = tabs[(moves[e.key] + tabs.length) % tabs.length];
        select(next);
        next.focus();
      });
    });
  });

  // Syntax colouring: comments, strings and a few keywords. Enough to read, small enough to trust.
  const RULES = {
    bash: /(#[^\n]*)|("(?:[^"\\\n]|\\.)*")|(\$[A-Z_]+|\b(?:curl|make|cp|open)\b)/g,
    js: /(\/\/[^\n]*)|("(?:[^"\\\n]|\\.)*"|`(?:[^`\\]|\\.)*`)|(\b(?:const|new)\b|=>)/g,
  };
  const escapeHtml = (s) => s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");

  document.querySelectorAll("code[data-lang]").forEach((el) => {
    const rule = RULES[el.dataset.lang];
    if (!rule) return;
    const text = el.textContent;
    let html = "";
    let last = 0;
    rule.lastIndex = 0;
    let m;
    while ((m = rule.exec(text))) {
      // In shell, "#" starts a comment only at the start of a word.
      if (m[1] && el.dataset.lang === "bash" && m.index > 0 && !/\s/.test(text[m.index - 1])) {
        rule.lastIndex = m.index + 1;
        continue;
      }
      const cls = m[1] ? "tok-c" : m[2] ? "tok-s" : "tok-k";
      html += escapeHtml(text.slice(last, m.index)) + `<span class="${cls}">${escapeHtml(m[0])}</span>`;
      last = m.index + m[0].length;
    }
    el.innerHTML = html + escapeHtml(text.slice(last));
  });

  // Copy buttons
  document.querySelectorAll(".copy").forEach((btn) => {
    btn.addEventListener("click", async () => {
      const code = btn.parentElement.querySelector("code");
      try {
        await navigator.clipboard.writeText(code.textContent);
        btn.textContent = "Copied";
      } catch {
        const range = document.createRange();
        range.selectNodeContents(code);
        const sel = window.getSelection();
        sel.removeAllRanges();
        sel.addRange(range);
        btn.textContent = "Selected";
      }
      setTimeout(() => (btn.textContent = "Copy"), 1600);
    });
  });

  // Hero: a driver rides to the store, picks up, and delivers while the tracking card follows the
  // same stream events a real tracking page receives (status, position, end).
  const route = document.getElementById("route-done");
  const courier = document.getElementById("courier");
  if (!route || !courier) return;

  const etaEl = document.getElementById("phone-eta");
  const steps = Array.from(document.querySelectorAll("#steps li"));
  const bar = document.getElementById("bar-fill");
  const tickStatus = document.getElementById("ticker-status");
  const tickPos = document.getElementById("ticker-pos");

  const total = route.getTotalLength();
  const storeAt = 90; // the store sits 90 units along the route (M60 160 V250)
  const LOOP = 16000;
  // [until ms, status]. Positions are interpolated inside each segment.
  const T = { confirmed: 1200, accept: 2000, atStore: 5000, pickup: 6200, arrive: 13600 };

  const lerp = (a, b, t) => a + (b - a) * Math.min(1, Math.max(0, t));
  const ease = (t) => (t < 0.5 ? 2 * t * t : 1 - Math.pow(-2 * t + 2, 2) / 2);

  function frame(ms) {
    if (ms < T.confirmed) return { status: "confirmed", len: 0, bar: 6, eta: "Finding a driver…" };
    if (ms < T.accept) return { status: "assigned", len: 0, bar: 22, eta: "Driver assigned" };
    if (ms < T.atStore) {
      const len = lerp(0, storeAt, ease((ms - T.accept) / (T.atStore - T.accept)));
      return { status: "assigned", len, bar: lerp(22, 40, len / storeAt), eta: "Driver heading to the store" };
    }
    if (ms < T.pickup) return { status: "assigned", len: storeAt, bar: 42, eta: "Driver at the store" };
    if (ms < T.arrive) {
      const len = lerp(storeAt, total, (ms - T.pickup) / (T.arrive - T.pickup));
      const mins = Math.max(1, Math.ceil(((total - len) / (total - storeAt)) * 9));
      return { status: "picked_up", len, bar: lerp(45, 98, (len - storeAt) / (total - storeAt)), eta: `Picked up · arriving in ${mins} min` };
    }
    return { status: "delivered", len: total, bar: 100, eta: "Delivered. Enjoy your meal!" };
  }

  const STEP_INDEX = { confirmed: 0, assigned: 1, picked_up: 2, delivered: 3 };
  let shown = { status: null, eta: null, pos: null };
  let lastPosAt = -Infinity;

  function render(ms, now) {
    const f = frame(ms);
    const pt = route.getPointAtLength(f.len);
    courier.setAttribute("transform", `translate(${pt.x.toFixed(1)} ${pt.y.toFixed(1)})`);
    route.style.strokeDasharray = `${f.len.toFixed(1)} ${total.toFixed(1)}`;
    bar.style.width = `${f.bar.toFixed(1)}%`;

    if (f.eta !== shown.eta) {
      etaEl.textContent = f.eta;
      shown.eta = f.eta;
    }
    if (f.status !== shown.status) {
      const idx = STEP_INDEX[f.status];
      steps.forEach((li, i) => {
        li.classList.toggle("done", i < idx || (f.status === "delivered" && i === idx));
        li.classList.toggle("now", i === idx && f.status !== "delivered");
      });
      tickStatus.innerHTML = `<span class="k">event:</span> status <span class="v">{"status":"${f.status}"}</span>`;
      shown.status = f.status;
    }
    // Positions arrive in batches, not every frame, just like the real stream.
    if (now - lastPosAt > 700 || shown.pos === null) {
      const lat = (35.668 - (pt.y / 440) * 0.015).toFixed(4);
      const lng = (139.693 + (pt.x / 560) * 0.02).toFixed(4);
      const pos = `{"lat":${lat},"lng":${lng}}`;
      if (pos !== shown.pos) {
        tickPos.innerHTML = `<span class="k">event:</span> position <span class="v">${pos}</span>`;
        shown.pos = pos;
      }
      lastPosAt = now;
    }
  }

  if (reducedMotion) {
    render(9500, 0);
    return;
  }

  let start = null;
  let elapsedBeforePause = 0;
  let running = false;
  let raf = 0;

  const tick = (now) => {
    if (start === null) start = now - elapsedBeforePause;
    const ms = (now - start) % LOOP;
    render(ms, now);
    raf = requestAnimationFrame(tick);
  };
  const play = () => {
    if (running) return;
    running = true;
    start = null;
    raf = requestAnimationFrame(tick);
  };
  const pause = () => {
    if (!running) return;
    running = false;
    cancelAnimationFrame(raf);
    elapsedBeforePause = start === null ? elapsedBeforePause : (performance.now() - start) % LOOP;
  };

  // Only animate while the hero is on screen and the tab is visible.
  let onScreen = true;
  const sync = () => (onScreen && !document.hidden ? play() : pause());
  if ("IntersectionObserver" in window) {
    new IntersectionObserver((entries) => {
      onScreen = entries[0].isIntersecting;
      sync();
    }).observe(courier.closest("figure"));
  }
  document.addEventListener("visibilitychange", sync);
  render(0, 0);
  sync();
})();
