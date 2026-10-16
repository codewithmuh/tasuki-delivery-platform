// Tasuki landing page. One order followed end to end as the visitor scrolls.
// No dependencies; the page reads fine with JavaScript off (every section is plain HTML).
//
// Pieces, in order: helpers, mobile nav, scroll progress + stations, sound, city generator, hero ambience,
// the journey (pinned scene driven by scroll position), reveals, code tabs, architecture trace, counters,
// game day.

(() => {
  "use strict";

  document.documentElement.classList.replace("no-js", "js");

  const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  const stacked = window.matchMedia("(max-width: 900px)");
  const SVG = "http://www.w3.org/2000/svg";

  const clamp = (v, a, b) => Math.min(b, Math.max(a, v));
  const lerp = (a, b, t) => a + (b - a) * clamp(t, 0, 1);
  const ease = (t) => (t < 0.5 ? 2 * t * t : 1 - Math.pow(-2 * t + 2, 2) / 2);
  const easeOut = (t) => 1 - Math.pow(1 - t, 3);
  const svgEl = (name, attrs = {}, parent = null) => {
    const el = document.createElementNS(SVG, name);
    for (const k in attrs) el.setAttribute(k, attrs[k]);
    if (parent) parent.appendChild(el);
    return el;
  };
  // Deterministic pseudo-random, so the city looks the same on every visit.
  const mulberry = (seed) => () => {
    seed = (seed + 0x6d2b79f5) | 0;
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
  const onScreen = (el, margin = 0) => {
    const r = el.getBoundingClientRect();
    return r.bottom > -margin && r.top < window.innerHeight + margin;
  };

  // ------------------------------------------------------------------ Mobile nav
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

  // ------------------------------------------------------------------ Sound
  // On by default, mutable, and silent until the visitor's first click, tap or key press (browsers block audio
  // before one). Every sound is synthesised with the Web Audio API: no files, notes from one pentatonic scale.
  const sound = (() => {
    const AudioCtx = window.AudioContext || window.webkitAudioContext;
    let ctx = null;
    let out = null;
    let on = false;
    const lastAt = {};

    const start = () => {
      if (!AudioCtx || !on) return Promise.resolve(false);
      if (!ctx) {
        ctx = new AudioCtx();
        // notes -> out -> compressor -> speakers, plus a quiet echo (out -> delay -> filter -> wet) for some room
        const comp = ctx.createDynamicsCompressor();
        comp.threshold.value = -18;
        comp.knee.value = 12;
        comp.ratio.value = 4;
        comp.attack.value = 0.005;
        comp.release.value = 0.2;
        comp.connect(ctx.destination);
        out = ctx.createGain();
        out.gain.value = 0.75;
        out.connect(comp);
        const delay = ctx.createDelay(1);
        delay.delayTime.value = 0.26;
        const tone = ctx.createBiquadFilter();
        tone.type = "lowpass";
        tone.frequency.value = 2200;
        const feedback = ctx.createGain();
        feedback.gain.value = 0.28;
        const wet = ctx.createGain();
        wet.gain.value = 0.22;
        out.connect(delay);
        delay.connect(tone);
        tone.connect(feedback);
        feedback.connect(delay);
        tone.connect(wet);
        wet.connect(comp);
      }
      if (ctx.state === "running") return Promise.resolve(true);
      return ctx.resume().then(
        () => ctx.state === "running",
        () => false,
      );
    };

    const note = (freq, at, dur, gain = 0.07, type = "sine", glideTo = 0) => {
      const t = ctx.currentTime + at;
      const osc = ctx.createOscillator();
      const env = ctx.createGain();
      osc.type = type;
      osc.frequency.setValueAtTime(freq, t);
      if (glideTo) osc.frequency.exponentialRampToValueAtTime(glideTo, t + dur);
      env.gain.setValueAtTime(0.0001, t);
      env.gain.exponentialRampToValueAtTime(gain, t + 0.012);
      env.gain.exponentialRampToValueAtTime(0.0001, t + dur);
      osc.connect(env);
      env.connect(out);
      osc.start(t);
      osc.stop(t + dur + 0.05);
    };

    const SOUNDS = {
      welcome: () => [392, 523.25, 659.25, 783.99].forEach((f, i) => note(f, i * 0.08, 0.6, 0.05, "triangle")),
      ping: () => note(1480, 0, 0.16, 0.022, "sine", 1180),
      hop: () => note(1318.5, 0, 0.1, 0.014, "sine", 1100),
      tick: () => note(1760, 0, 0.07, 0.012),
      confirmed: () => note(523.25, 0, 0.45, 0.05),
      offer: () => [659.25, 987.77].forEach((f, i) => note(f, i * 0.09, 0.3, 0.04)),
      decline: () => note(246.94, 0, 0.4, 0.04, "triangle", 196),
      assigned: () => [587.33, 880].forEach((f, i) => note(f, i * 0.12, 0.5)),
      picked_up: () => [659.25, 783.99, 880].forEach((f, i) => note(f, i * 0.1, 0.55)),
      delivered: () => [523.25, 659.25, 783.99, 1046.5].forEach((f, i) => note(f, i * 0.1, 1.1, 0.06, "triangle")),
      webhook: () => [783.99, 1046.5].forEach((f, i) => note(f, i * 0.07, 0.35, 0.035, "triangle")),
      alert: () => [196, 174.61].forEach((f, i) => note(f, i * 0.14, 0.42, 0.035, "triangle")),
      handled: () => [392, 523.25].forEach((f, i) => note(f, i * 0.1, 0.5, 0.03, "triangle")),
      sweep: () => note(392, 0, 0.32, 0.035, "sine", 880),
      resolve: () => [523.25, 659.25, 783.99].forEach((f, i) => note(f, i * 0.03, 0.9, 0.026, "triangle")),
      count: () => [523.25, 659.25, 783.99, 1046.5].forEach((f, i) => note(f, i * 0.09, 0.18, 0.025)),
      // Architecture accents: what each kind of node sounds like when a packet reaches it.
      kafka: () => note(130.81, 0, 0.7, 0.07, "triangle"), // the log: a bass note under everything
      commit: () => [1567.98, 1318.5].forEach((f, i) => note(f, i * 0.07, 0.05, 0.03, "triangle")), // a write: two clicks
      route: () => note(440, 0, 0.26, 0.03, "sine", 880), // routing: a glide along a road
      arrive: () => [1046.5, 1318.5].forEach((f, i) => note(f, i * 0.08, 0.45, 0.035, "triangle")), // a phone lights up
      blip: () => note(1174.66, 0, 0.12, 0.025, "triangle", 1046.5), // a cache or a console: short and dry
    };
    // The melody of a packet crossing the diagram: one pentatonic step per hop, rising. Orders speak in a low
    // sine voice; GPS points in a brighter triangle an octave and a half up. Two systems, two voices.
    const SCALE = [523.25, 587.33, 659.25, 783.99, 880, 1046.5, 1174.66, 1318.5, 1567.98];
    const hop = (step, voice) => {
      const f = SCALE[Math.min(step, SCALE.length - 1)];
      if (voice === "gps") note(f * 1.5, 0, 0.16, 0.022, "triangle");
      else note(f, 0, 0.3, 0.03, "sine");
    };

    return {
      supported: Boolean(AudioCtx),
      get on() {
        return on;
      },
      enable() {
        on = true;
        return start();
      },
      disable() {
        on = false;
        if (ctx) ctx.suspend();
      },
      arm() {
        on = true;
      },
      start,
      // minGap: never repeat the same sound within that many ms (scrolling fast must not become a drum roll).
      get running() {
        return Boolean(on && ctx && ctx.state === "running");
      },
      play(name, minGap = 0) {
        if (!(on && ctx && ctx.state === "running")) return;
        const now = performance.now();
        if (minGap && now - (lastAt[name] || -Infinity) < minGap) return;
        lastAt[name] = now;
        SOUNDS[name]();
      },
      hop(step, voice) {
        if (!(on && ctx && ctx.state === "running")) return;
        const now = performance.now();
        if (now - (lastAt.hop || -Infinity) < 110) return;
        lastAt.hop = now;
        hop(step, voice);
      },
    };
  })();

  const soundBtn = document.querySelector(".sound-toggle");
  if (soundBtn && sound.supported && !reducedMotion) {
    const label = soundBtn.querySelector(".sound-label");
    const show = (on) => {
      soundBtn.setAttribute("aria-pressed", String(on));
      label.textContent = on ? "Sound on" : "Sound off";
      soundBtn.title = on ? "Mute the page" : "Play the page with sound";
    };
    const save = (value) => {
      try {
        localStorage.setItem("tasuki-sound", value);
      } catch {
        // private mode or blocked storage: the choice just isn't remembered
      }
    };
    let saved = null;
    try {
      saved = localStorage.getItem("tasuki-sound");
    } catch {
      saved = null;
    }
    // Scrolling with a wheel or trackpad is not a gesture browsers accept for audio, so a visitor who only
    // scrolls hears nothing. Until the first real gesture, the toggle pulses and a note explains.
    const nudge = document.getElementById("sound-nudge");
    let nudgeTimer = 0;
    const hideNudge = () => {
      if (nudge) nudge.hidden = true;
      soundBtn.classList.remove("pulse");
      clearTimeout(nudgeTimer);
    };
    const showNudge = (ms) => {
      if (woken || !sound.on || !nudge) return;
      nudge.hidden = false;
      soundBtn.classList.add("pulse");
      clearTimeout(nudgeTimer);
      nudgeTimer = setTimeout(hideNudge, ms);
    };

    const GESTURES = ["pointerdown", "pointerup", "touchend", "keydown"];
    let woken = false;
    const stopListening = () => GESTURES.forEach((g) => document.removeEventListener(g, wake, true));
    const awake = () => {
      woken = true;
      stopListening();
      hideNudge();
      sound.play("welcome");
    };
    function wake(e) {
      if (woken || !sound.on || soundBtn.contains(e.target)) return;
      sound.start().then((running) => {
        if (running && !woken) awake();
      });
    }
    if (saved !== "off") {
      sound.arm();
      GESTURES.forEach((g) => document.addEventListener(g, wake, true));
      setTimeout(() => showNudge(14000), 1200);
      // A second reminder when the story starts, if nothing has been clicked yet.
      const journeySection = document.getElementById("journey");
      if (journeySection && "IntersectionObserver" in window) {
        const once = new IntersectionObserver((entries) => {
          if (!entries[0].isIntersecting) return;
          once.disconnect();
          showNudge(9000);
        }, { threshold: 0.15 });
        once.observe(journeySection);
      }
    }
    show(sound.on);
    soundBtn.addEventListener("click", () => {
      if (sound.on && !woken) {
        // Armed but not yet allowed to play: this click is the permission, not a mute.
        sound.start().then((running) => {
          if (running) awake();
        });
        return;
      }
      if (sound.on) {
        sound.disable();
        show(false);
        save("off");
        hideNudge();
      } else {
        show(true);
        save("on");
        sound.enable().then((running) => {
          if (!running) return;
          awake();
        });
      }
    });
    soundBtn.hidden = false;
  }

  // ------------------------------------------------------------------ City generator
  // Streets on a grid every `step` units; blocks fill the cells, some split, some parks.
  function buildCity(group, { w, h, step, origin, seed, river }) {
    const rnd = mulberry(seed);
    svgEl("rect", { class: "street-bg", x: -40, y: -40, width: w + 80, height: h + 80 }, group);
    const gap = 7;
    for (let sy = origin - step; sy < h + step; sy += step) {
      for (let sx = origin - step; sx < w + step; sx += step) {
        const x = sx + gap;
        const y = sy + gap;
        const size = step - gap * 2;
        const r = rnd();
        if (r < 0.1) {
          svgEl("rect", { class: "park", x, y, width: size, height: size, rx: 6 }, group);
        } else if (r < 0.34) {
          const half = (size - 6) / 2;
          svgEl("rect", { class: "blk", x, y, width: half, height: size, rx: 5 }, group);
          svgEl("rect", { class: "blk", x: x + half + 6, y, width: half, height: size, rx: 5 }, group);
        } else if (r < 0.5) {
          const half = (size - 6) / 2;
          svgEl("rect", { class: "blk", x, y, width: size, height: half, rx: 5 }, group);
          svgEl("rect", { class: "blk", x, y: y + half + 6, width: size, height: half, rx: 5 }, group);
        } else {
          svgEl("rect", { class: "blk", x, y, width: size, height: size, rx: 6 }, group);
        }
      }
    }
    if (river) svgEl("path", { class: "river", d: river }, group);
  }

  // ------------------------------------------------------------------ Hero ambience
  // Four drivers riding loops through the city, orders popping up, and a feed of the events they make.
  const heroMap = document.getElementById("hero-map");
  const hero = document.querySelector(".hero");
  if (heroMap) {
    const W = 1200;
    const H = 640;
    const city = svgEl("g", {}, heroMap);
    buildCity(city, { w: W, h: H, step: 60, origin: 30, seed: 7, river: "M-20 598 C 200 560, 420 640, 640 600 S 1000 560, 1220 590" });

    const LOOPS = [
      { d: "M90 90 H510 V330 H90 Z", speed: 70, cls: "hero-courier", dir: 1 },
      { d: "M630 150 H1110 V450 H630 Z", speed: 82, cls: "hero-courier hero-courier-b", dir: -1 },
      { d: "M270 390 H810 V570 H270 Z", speed: 64, cls: "hero-courier", dir: -1 },
      { d: "M450 30 H930 V270 H450 Z", speed: 76, cls: "hero-courier hero-courier-b", dir: 1 },
    ];
    const pops = svgEl("g", {}, heroMap);
    const riders = LOOPS.map((l, i) => {
      const path = svgEl("path", { d: l.d, fill: "none", stroke: "none" }, heroMap);
      const dot = svgEl("circle", { class: l.cls, r: 7 }, heroMap);
      return { path, dot, len: path.getTotalLength(), speed: l.speed, dir: l.dir, offset: i * 173 };
    });

    const feed = document.getElementById("hero-feed");
    const liveText = document.getElementById("hero-live-text");
    const rnd = mulberry(42);
    const id = () => {
      const chars = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";
      let s = "";
      for (let i = 0; i < 4; i++) s += chars[Math.floor(rnd() * chars.length)];
      return s;
    };
    const EVENTS = [
      (o) => `<span class="k">OrderCreated</span> <span class="v">ord_01JA${o}</span> · shibuya`,
      (o) => `<span class="k">OrderAssigned</span> <span class="v">ord_01JA${o}</span> · drv_…${o.slice(0, 2).toLowerCase()}`,
      (o) => `<span class="k">OrderPickedUp</span> <span class="v">ord_01JA${o}</span>`,
      (o) => `<span class="k">OrderDelivered</span> <span class="v">ord_01JA${o}</span> · 38 m`,
    ];
    let ordersToday = 1284;
    let clock = 12 * 3600 + 4 * 60 + 2;
    const hhmmss = (s) => [Math.floor(s / 3600), Math.floor((s / 60) % 60), Math.floor(s % 60)].map((n) => String(n).padStart(2, "0")).join(":");

    let last = null;
    let nextPop = 0;
    let running = false;
    let raf = 0;
    const tick = (now) => {
      if (last === null) last = now;
      const dt = Math.min(0.1, (now - last) / 1000);
      last = now;
      clock += dt * 4; // the demo clock runs a little fast
      riders.forEach((r) => {
        r.offset = (r.offset + r.dir * r.speed * dt + r.len) % r.len;
        const p = r.path.getPointAtLength(r.offset);
        r.dot.setAttribute("cx", p.x.toFixed(1));
        r.dot.setAttribute("cy", p.y.toFixed(1));
      });
      if (now >= nextPop) {
        nextPop = now + 2400 + rnd() * 2200;
        const kind = Math.floor(rnd() * EVENTS.length);
        if (kind === 0) {
          const x = 30 + 60 * Math.floor(1 + rnd() * 18);
          const y = 30 + 60 * Math.floor(1 + rnd() * 9);
          const ring = svgEl("circle", { class: "order-pop", cx: x, cy: y, r: 11 }, pops);
          setTimeout(() => ring.remove(), 1700);
          ordersToday++;
          sound.play("ping", 2000);
        }
        if (feed) {
          const line = document.createElement("div");
          line.innerHTML = `<span class="t">${hhmmss(clock)}</span> ${EVENTS[kind](id())}`;
          feed.prepend(line);
          while (feed.children.length > 2) feed.lastElementChild.remove();
        }
        if (liveText) liveText.textContent = `Tokyo · 4 drivers moving · ${ordersToday.toLocaleString("en-US")} orders today`;
      }
      raf = requestAnimationFrame(tick);
    };
    const play = () => {
      if (running) return;
      running = true;
      last = null;
      raf = requestAnimationFrame(tick);
    };
    const pause = () => {
      running = false;
      cancelAnimationFrame(raf);
    };
    if (reducedMotion) {
      riders.forEach((r) => {
        const p = r.path.getPointAtLength(r.offset);
        r.dot.setAttribute("cx", p.x);
        r.dot.setAttribute("cy", p.y);
      });
    } else {
      let visible = true;
      const sync = () => (visible && !document.hidden ? play() : pause());
      new IntersectionObserver((entries) => {
        visible = entries[0].isIntersecting;
        sync();
      }).observe(hero);
      document.addEventListener("visibilitychange", sync);
      sync();
    }
  }

  // ------------------------------------------------------------------ Journey: one order, driven by scroll
  const scene = document.getElementById("scene");
  const journey = (() => {
    if (!scene) return null;
    const map = document.getElementById("journey-map");
    buildCity(document.getElementById("jm-city"), { w: 600, h: 420, step: 60, origin: 30, seed: 3, river: "M-20 392 C 120 362, 240 420, 360 388 S 540 356, 620 384" });

    const routeQuote = document.getElementById("route-quote");
    const route1 = document.getElementById("route1");
    const route2 = document.getElementById("route2");
    const route1Base = document.getElementById("route1-base");
    const route2Base = document.getElementById("route2-base");
    const r1Len = route1.getTotalLength();
    const r2Len = route2.getTotalLength();
    const drvA = document.getElementById("drv-a");
    const drvB = document.getElementById("drv-b");
    const arcA = drvA.querySelector(".offer-arc");
    const arcB = drvB.querySelector(".offer-arc");
    const pings = document.getElementById("gps-pings");

    const steps = Array.from(document.querySelectorAll("#steps-col .jstep"));
    const sysViews = {};
    document.querySelectorAll(".sys-view").forEach((v) => (sysViews[v.dataset.view] = v));
    const sysSub = document.getElementById("sys-sub");
    const phoneViews = {};
    document.querySelectorAll(".phone-view").forEach((v) => (phoneViews[v.dataset.view] = v));
    const phoneWho = document.getElementById("phone-who");
    const phoneEta = document.getElementById("phone-eta");
    const phoneSteps = Array.from(document.querySelectorAll("#phone-steps li"));
    const barFill = document.getElementById("bar-fill");
    const phoneStream = document.getElementById("phone-stream");
    const offerPickup = document.getElementById("offer-pickup");
    const offerTimerFill = document.getElementById("offer-timer-fill");
    const offerCount = document.getElementById("offer-count");
    const offerAccept = document.getElementById("offer-accept");
    const offerDecline = document.getElementById("offer-decline");
    const phoneDriver = phoneViews.driver;
    const clockEl = document.getElementById("scene-clock");

    const LOG = [
      ["12:04:02", "POST /v1/quotes", "201 · ¥742 · ETA 32 min"],
      ["12:04:05", "OrderCreated", "ord_01JA2S… · confirmed"],
      ["12:04:06", "OfferSent", "drv_…b4 · 1.2 km · 30 s to answer"],
      ["12:04:36", "OfferExpired", "drv_…b4 · no answer"],
      ["12:04:37", "OfferSent", "drv_…a9 · 1.6 km · 30 s to answer"],
      ["12:04:44", "OrderAssigned", "drv_…a9 accepted in 7 s"],
      ["12:09:51", "OrderPickedUp", "at the store · ETA 11 min"],
      ["12:10:02", "position ×226", "35.6621, 139.6975 · every 3 s"],
      ["12:21:08", "OrderDelivered", "41 m from the drop-off"],
    ];
    const logEl = document.getElementById("sys-log");
    const logItems = LOG.map(([t, ev, d]) => {
      const li = document.createElement("li");
      li.innerHTML = `<span class="t">${t}</span><span class="ev">${ev}</span><span class="d">${d}</span>`;
      logEl.appendChild(li);
      return li;
    });

    const STAGES = ["quote", "created", "offer_b", "declined", "offer_a", "assigned", "to_store", "at_store", "picked_up", "en_route", "arriving", "delivered", "webhook"];
    const STAGE_SOUND = { created: "confirmed", offer_b: "offer", declined: "decline", offer_a: "offer", assigned: "assigned", picked_up: "picked_up", delivered: "delivered", webhook: "webhook" };
    const SYS_SUB = { quote: "REST · JSON", order: "REST · idempotent", events: "orders.events · Kafka", webhook: "signed webhook · next" };
    const secs = (hms) => hms.split(":").reduce((a, b) => a * 60 + Number(b), 0);
    const hms = (s) => [Math.floor(s / 3600), Math.floor((s / 60) % 60), Math.floor(s % 60)].map((n) => String(n).padStart(2, "0")).join(":");

    // Everything on screen is a pure function of p in [0, 1]: five steps, each one fifth of the scroll.
    function sceneAt(p) {
      const s = clamp(p, 0, 0.999999) * 5;
      const k = Math.floor(s);
      const q = s - k;
      const o = {
        step: k, stage: "quote", quote: 0, r1: 0, r2: 0, r1base: false, r2base: false,
        offerA: null, offerB: null, declined: false, accepted: false, active: false,
        sys: "quote", logN: 0, phone: "customer", eta: "Order placed · finding a driver", done: 0, bar: 6,
        expired: false, acceptHit: false, clock: secs("12:04:02"), gps: false, who: null,
      };
      if (k === 0) {
        if (q < 0.5) {
          o.quote = easeOut(q / 0.5);
          o.logN = q > 0.2 ? 1 : 0;
          o.clock = secs("12:04:02") + q * 6;
        } else {
          o.stage = "created";
          o.quote = 1;
          o.r2base = true;
          o.sys = "order";
          o.logN = 2;
          o.clock = secs("12:04:05");
        }
      } else if (k === 1) {
        o.r2base = true;
        o.sys = "events";
        o.phone = "driver";
        if (q < 0.3) {
          o.stage = "offer_b";
          o.offerB = 1 - q / 0.3;
          o.who = "b";
          o.logN = 3;
          o.clock = secs("12:04:06") + (1 - o.offerB) * 30;
        } else if (q < 0.45) {
          o.stage = "declined";
          o.offerB = 0;
          o.declined = true;
          o.expired = true;
          o.who = "b";
          o.logN = 4;
          o.clock = secs("12:04:36");
        } else if (q < 0.8) {
          o.stage = "offer_a";
          o.declined = true;
          const t = (q - 0.45) / 0.35;
          o.offerA = 1 - t * 0.23; // answers after 7 of the 30 seconds
          o.who = "a";
          o.logN = 5;
          o.clock = secs("12:04:37") + t * 7;
        } else {
          o.stage = "assigned";
          o.declined = true;
          o.accepted = true;
          o.acceptHit = true;
          o.offerA = 0.77;
          o.who = "a";
          o.logN = 6;
          o.r1base = true;
          o.clock = secs("12:04:44");
        }
      } else if (k === 2) {
        o.declined = true;
        o.active = true;
        o.r1base = true;
        o.r2base = true;
        o.sys = "events";
        o.logN = 6;
        o.done = 1;
        if (q < 0.6) {
          o.stage = "to_store";
          o.r1 = ease(q / 0.6);
          o.eta = "Driver assigned · heading to the store";
          o.bar = lerp(22, 40, o.r1);
          o.gps = true;
          o.clock = secs("12:04:44") + o.r1 * (secs("12:09:40") - secs("12:04:44"));
        } else if (q < 0.75) {
          o.stage = "at_store";
          o.r1 = 1;
          o.eta = "Driver at the store";
          o.bar = 42;
          o.clock = secs("12:09:40") + ((q - 0.6) / 0.15) * 11;
        } else {
          o.stage = "picked_up";
          o.r1 = 1;
          o.r2 = ((q - 0.75) / 0.25) * 0.12;
          o.eta = "Picked up · arriving in 11 min";
          o.done = 2;
          o.bar = lerp(45, 50, o.r2 / 0.12);
          o.gps = true;
          o.logN = 7;
          o.clock = secs("12:09:51") + o.r2 * (secs("12:21:08") - secs("12:09:51"));
        }
      } else if (k === 3) {
        o.stage = "en_route";
        o.declined = true;
        o.active = true;
        o.r1 = 1;
        o.r1base = true;
        o.r2base = true;
        o.r2 = lerp(0.12, 0.88, q);
        o.sys = "events";
        o.logN = 8;
        o.done = 2;
        const mins = Math.max(2, Math.ceil((1 - o.r2) * 11));
        o.eta = `Picked up · arriving in ${mins} min`;
        o.bar = lerp(50, 92, q);
        o.gps = true;
        o.clock = secs("12:09:51") + o.r2 * (secs("12:21:08") - secs("12:09:51"));
      } else {
        o.declined = true;
        o.active = true;
        o.r1 = 1;
        o.r1base = true;
        o.r2base = true;
        o.sys = "events";
        o.logN = 8;
        o.done = 2;
        if (q < 0.3) {
          o.stage = "arriving";
          o.r2 = lerp(0.88, 1, q / 0.3);
          o.eta = "Arriving now";
          o.bar = lerp(92, 99, q / 0.3);
          o.gps = true;
          o.clock = secs("12:09:51") + o.r2 * (secs("12:21:08") - secs("12:09:51"));
        } else {
          o.stage = q < 0.7 ? "delivered" : "webhook";
          o.r2 = 1;
          o.eta = "Delivered. Enjoy your meal!";
          o.done = 4;
          o.bar = 100;
          o.logN = 9;
          o.clock = secs("12:21:08") + (q >= 0.7 ? 1 : 0);
          if (q >= 0.7) o.sys = "webhook";
        }
      }
      return o;
    }

    const shown = { stage: null, step: -1, sys: null, phone: null, eta: null, logN: -1, done: -1, stream: null, clock: null };
    let lastPingAt = -Infinity;
    let lastPingPos = null;
    let lastStageIndex = 0;

    function render(p, now) {
      const o = sceneAt(p);
      const pt = o.stage === "quote" || o.stage === "created" || o.offerA !== null || o.stage === "declined" || o.stage === "offer_b"
        ? route1.getPointAtLength(0)
        : o.r2 > 0 || o.stage === "at_store" || o.stage === "delivered" || o.stage === "webhook" || o.stage === "arriving" || o.stage === "en_route"
          ? route2.getPointAtLength(o.r2 * r2Len)
          : route1.getPointAtLength(o.r1 * r1Len);

      // Map
      routeQuote.style.strokeDasharray = `${o.quote.toFixed(3)} 1`;
      routeQuote.style.strokeOpacity = o.stage === "quote" ? 0.55 : 0;
      route1Base.classList.toggle("on", o.r1base);
      route2Base.classList.toggle("on", o.r2base);
      route1.style.strokeDasharray = `${o.r1.toFixed(3)} 1`;
      route2.style.strokeDasharray = `${o.r2.toFixed(3)} 1`;
      drvB.classList.toggle("offering", o.offerB !== null && o.offerB > 0);
      drvB.classList.toggle("declined", o.declined);
      if (o.offerB !== null) arcB.style.strokeDashoffset = String(100 * (1 - o.offerB));
      drvA.classList.toggle("offering", o.offerA !== null && !o.accepted);
      drvA.classList.toggle("accepted", o.accepted && !o.active);
      drvA.classList.toggle("active", o.active);
      if (o.offerA !== null) arcA.style.strokeDashoffset = String(100 * (1 - o.offerA));
      drvA.setAttribute("transform", `translate(${pt.x.toFixed(1)} ${pt.y.toFixed(1)})`);

      // GPS pings: a ring at the driver's position every so often while moving, like batches arriving.
      if (o.gps && !reducedMotion && now - lastPingAt > 650) {
        const key = `${pt.x.toFixed(0)},${pt.y.toFixed(0)}`;
        if (key !== lastPingPos) {
          const ring = svgEl("circle", { class: "gps-ping", cx: pt.x.toFixed(1), cy: pt.y.toFixed(1), r: 9 }, pings);
          setTimeout(() => ring.remove(), 950);
          lastPingAt = now;
          lastPingPos = key;
          sound.play("ping", 1300);
        }
      }

      // Scene state and step highlight
      if (o.stage !== shown.stage) {
        scene.dataset.stage = o.stage;
        const idx = STAGES.indexOf(o.stage);
        if (idx > lastStageIndex && STAGE_SOUND[o.stage]) sound.play(STAGE_SOUND[o.stage], 350);
        lastStageIndex = idx;
        shown.stage = o.stage;
      }
      if (o.step !== shown.step) {
        steps.forEach((li, i) => li.classList.toggle("is-active", i === o.step));
        scene.dataset.step = String(o.step);
        shown.step = o.step;
      }

      // Your system
      if (o.sys !== shown.sys) {
        for (const k in sysViews) sysViews[k].classList.toggle("active", k === o.sys);
        sysSub.textContent = SYS_SUB[o.sys];
        shown.sys = o.sys;
      }
      if (o.logN !== shown.logN) {
        logItems.forEach((li, i) => {
          li.classList.toggle("in", i < o.logN);
          li.classList.toggle("hot", i === o.logN - 1);
        });
        shown.logN = o.logN;
      }

      // The phone
      if (o.phone !== shown.phone) {
        for (const k in phoneViews) phoneViews[k].classList.toggle("active", k === o.phone);
        phoneWho.textContent = o.phone === "driver" ? "Driver's phone" : "Customer's phone";
        shown.phone = o.phone;
      }
      if (o.phone === "driver") {
        const remaining = o.who === "b" ? o.offerB : o.offerA;
        const secondsLeft = Math.ceil(remaining * 30);
        offerPickup.textContent = o.who === "b" ? "Pickup 1.2 km" : "Pickup 1.6 km";
        offerTimerFill.style.width = `${(remaining * 100).toFixed(1)}%`;
        offerCount.textContent = o.acceptHit ? "Accepted" : o.expired ? "Expired · offered to the next driver" : `${secondsLeft} s to answer`;
        if (!o.acceptHit && !o.expired && secondsLeft !== shown.offerSeconds) {
          if (shown.offerSeconds !== undefined) sound.play("tick", 90);
          shown.offerSeconds = secondsLeft;
        }
        phoneDriver.classList.toggle("expired", o.expired);
        offerAccept.classList.toggle("hit", o.acceptHit);
        offerDecline.classList.remove("hit");
      } else {
        if (o.eta !== shown.eta) {
          phoneEta.textContent = o.eta;
          shown.eta = o.eta;
        }
        if (o.done !== shown.done) {
          phoneSteps.forEach((li, i) => {
            li.classList.toggle("done", i < o.done);
            li.classList.toggle("now", i === o.done && o.done < 4);
          });
          shown.done = o.done;
        }
        barFill.style.width = `${o.bar.toFixed(1)}%`;
        const lat = (35.668 - (pt.y / 420) * 0.015).toFixed(4);
        const lng = (139.693 + (pt.x / 600) * 0.02).toFixed(4);
        const stream = o.gps
          ? `<span class="k">event:</span> position <span class="v">{"lat":${lat},"lng":${lng}}</span>`
          : `<span class="k">event:</span> status <span class="v">{"status":"${o.done >= 4 ? "delivered" : o.done >= 2 ? "picked_up" : o.done >= 1 ? "assigned" : "confirmed"}"}</span>`;
        if (stream !== shown.stream) {
          phoneStream.innerHTML = stream;
          shown.stream = stream;
        }
      }

      const clock = hms(o.clock);
      if (clock !== shown.clock) {
        clockEl.textContent = clock;
        shown.clock = clock;
      }
    }

    // Progress through the steps column: 0 when the first step's top reaches the focus line, 1 when the last
    // step's bottom does. On narrow screens the scene is pinned above the text, so the focus line sits in the
    // space below it.
    function progress() {
      const first = steps[0].getBoundingClientRect();
      const last = steps[steps.length - 1].getBoundingClientRect();
      let focus = window.innerHeight * 0.5;
      if (stacked.matches) {
        const sr = scene.getBoundingClientRect();
        focus = sr.bottom + (window.innerHeight - sr.bottom) * 0.45;
      }
      return clamp((focus - first.top) / (last.bottom - first.top), 0, 1);
    }

    const section = document.getElementById("journey");
    let lastP = -1;
    return {
      update(now) {
        if (!onScreen(section, 200)) return;
        const p = progress();
        if (p === lastP && !sceneAt(p).gps) return;
        lastP = p;
        render(p, now);
      },
      init() {
        render(progress(), performance.now());
      },
    };
  })();

  // ------------------------------------------------------------------ Scroll: progress bar, stations, journey
  const progressFill = document.getElementById("progress-fill");
  const stationLinks = Array.from(document.querySelectorAll(".stations a"));
  const stationTargets = stationLinks.map((a) => document.querySelector(a.getAttribute("href")));
  const stationsList = document.querySelector(".stations ol");
  const scrollCue = document.querySelector(".scroll-cue");
  let lastActive = -1;

  function onScroll(now) {
    const doc = document.documentElement;
    const max = doc.scrollHeight - window.innerHeight;
    const p = max > 0 ? window.scrollY / max : 0;
    if (progressFill) progressFill.style.transform = `scaleX(${p.toFixed(4)})`;
    if (scrollCue) scrollCue.style.opacity = window.scrollY > 120 ? "0" : "1";

    if (stationLinks.length) {
      let active = 0;
      stationTargets.forEach((t, i) => {
        if (t && t.getBoundingClientRect().top <= window.innerHeight * 0.45) active = i;
      });
      if (window.scrollY + window.innerHeight >= doc.scrollHeight - 4) active = stationLinks.length - 1;
      if (active !== lastActive) {
        stationLinks.forEach((a, i) => {
          a.classList.toggle("active", i === active);
          a.classList.toggle("passed", i < active);
        });
        stationsList.style.setProperty("--fill", (active / (stationLinks.length - 1)).toFixed(3));
        if (lastActive !== -1 && active > lastActive) sound.play("tick", 600);
        lastActive = active;
      }
    }
    if (journey) journey.update(now);
  }

  let scrollRaf = 0;
  const requestScroll = () => {
    if (scrollRaf) return;
    scrollRaf = requestAnimationFrame((now) => {
      scrollRaf = 0;
      onScroll(now);
    });
  };
  window.addEventListener("scroll", requestScroll, { passive: true });
  window.addEventListener("resize", requestScroll);
  if (journey) journey.init();
  onScroll(performance.now());
  // The journey keeps pinging while the driver moves even if the visitor holds still.
  if (journey && !reducedMotion) setInterval(() => journey.update(performance.now()), 700);

  // ------------------------------------------------------------------ Reveals
  document.querySelectorAll(".reveal-group").forEach((group) => {
    Array.from(group.querySelectorAll(":scope > .reveal")).forEach((el, i) => el.style.setProperty("--i", String(Math.min(i, 7))));
  });
  const reveals = Array.from(document.querySelectorAll(".reveal"));
  if ("IntersectionObserver" in window && !reducedMotion) {
    const io = new IntersectionObserver(
      (entries) => {
        entries.forEach((e) => {
          if (e.isIntersecting) {
            e.target.classList.add("in");
            io.unobserve(e.target);
          }
        });
      },
      { threshold: 0.1, rootMargin: "0px 0px -6% 0px" },
    );
    reveals.forEach((el) => io.observe(el));
  } else {
    reveals.forEach((el) => el.classList.add("in"));
  }

  // ------------------------------------------------------------------ Code tabs, syntax colouring, copy
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
      tab.addEventListener("click", () => {
        select(tab);
        sound.play("tick", 150);
      });
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

  // ------------------------------------------------------------------ Architecture trace
  // A packet travels the real edges of the diagram, one trace for an order and one for a GPS point. The caption
  // explains each hop as the packet makes it.
  const arch = document.getElementById("arch");
  if (arch) {
    const packetsG = document.getElementById("packets");
    const traceText = document.getElementById("trace-text");
    const traceStep = document.getElementById("trace-step");
    const caption = document.querySelector(".trace-caption");
    const edgeOf = (id) => document.getElementById(id);
    const SPEED = 150; // units per second
    const DWELL = 0.45; // pause at each node, in seconds

    const TRACES = {
      order: {
        lanes: [
          { edges: ["e-app-orders", "e-orders-pg", "e-pg-relay", "e-relay-kafka", "e-kafka-dispatch", "e-dispatch-osrm", "e-dispatch-driver"] },
          { after: 4, edges: ["e-kafka-tracking", "e-tracking-phone"] },
          { after: 4, delay: 1.1, edges: ["e-kafka-backend"] },
        ],
        captions: [
          [0, 0, "Your checkout calls POST /v1/orders with an Idempotency-Key. Same key again, same order back."],
          [0, 1, "The order row and its OrderCreated event commit in one PostgreSQL transaction: the outbox."],
          [0, 2, "The outbox relay reads committed events and publishes them to Kafka, in order, per order."],
          [0, 4, "Kafka fans the event out. Dispatch consumes it and starts looking for a driver."],
          [0, 5, "Dispatch asks OSRM for real-road travel times, so \"nearest\" means minutes, not metres."],
          [0, 6, "The nearest free driver gets an offer over SSE with a 30-second timer. Decline or silence: the next one."],
          [1, 1, "Meanwhile tracking streams every status change to your customer's phone the moment it happens."],
          [2, 0, "Next on the roadmap: the same event, signed and retried, pushed to your backend as a webhook."],
        ],
        start: "n-app",
      },
      gps: {
        lanes: [
          { edges: ["e-driver-ingest", "e-ingest-kafka", "e-kafka-state", "e-state-valkey", "e-valkey-dispatch"] },
          { after: 2, edges: ["e-kafka-tracking", "e-tracking-phone"] },
          { after: 2, delay: 0.5, edges: ["e-kafka-tracking", "e-tracking-ops"] },
        ],
        captions: [
          [0, 0, "The driver's phone POSTs a batch of 1 to 50 GPS points to fleet-ingest, every few seconds."],
          [0, 1, "fleet-ingest checks the token and publishes to courier.locations, keyed by driver. Under overload: 503 + Retry-After."],
          [0, 2, "fleet-state consumes the stream and keeps only the latest point per driver. A missed point is harmless."],
          [0, 3, "Valkey holds that live state, geo-indexed. Lose it, and fleet-state rebuilds it from Kafka."],
          [0, 4, "Dispatch queries Valkey for the nearest free drivers when an order needs one."],
          [1, 0, "At the same time, tracking fans the point out over server-sent events."],
          [1, 1, "Your customer's marker moves. The ops console sees the whole fleet the same way."],
        ],
        start: "n-driver",
      },
    };
    // What a packet sounds like when it reaches each node. Nodes not listed get the next step of the melody.
    const ACCENT = {
      "n-kafka": "kafka", "n-pg": "commit", "n-osrm": "route", "n-phone": "arrive", "n-backend": "webhook",
      "n-driver": "offer", "n-valkey": "blip", "n-ops": "blip",
    };
    const END_NODE = {
      "e-app-orders": "n-orders", "e-orders-pg": "n-pg", "e-pg-relay": "n-relay", "e-relay-kafka": "n-kafka", "e-kafka-orders": "n-orders",
      "e-driver-ingest": "n-ingest", "e-ingest-kafka": "n-kafka", "e-kafka-state": "n-state", "e-kafka-dispatch": "n-dispatch",
      "e-kafka-tracking": "n-tracking", "e-state-valkey": "n-valkey", "e-valkey-dispatch": "n-dispatch", "e-dispatch-osrm": "n-osrm",
      "e-dispatch-driver": "n-driver", "e-tracking-ops": "n-ops", "e-tracking-phone": "n-phone", "e-kafka-backend": "n-backend",
    };

    let current = "order";
    let plan = null;
    let packets = [];
    let t0 = null;
    let raf = 0;
    let running = false;
    let shownCaption = -1;
    let litCount = 0;

    function build(name) {
      const trace = TRACES[name];
      const lanes = trace.lanes.map((lane) => {
        const segs = [];
        let t = 0;
        lane.edges.forEach((id) => {
          const path = edgeOf(id);
          const len = path.getTotalLength();
          segs.push({ id, path, len, start: t, end: t + len / SPEED });
          t += len / SPEED + DWELL;
        });
        return { segs, offset: 0, after: lane.after || 0, delay: lane.delay || 0 };
      });
      lanes.forEach((lane) => {
        if (lane.after) lane.offset = lanes[0].segs[lane.after - 1].end + DWELL + lane.delay;
      });
      const segsAbs = [];
      lanes.forEach((lane, li) =>
        lane.segs.forEach((s, si) => segsAbs.push({ ...s, lane: li, index: si, start: s.start + lane.offset, end: s.end + lane.offset })),
      );
      const captions = trace.captions
        .map(([li, si, text]) => ({ at: lanes[li].offset + lanes[li].segs[si].start, text }))
        .sort((a, b) => a.at - b.at);
      const total = Math.max(...segsAbs.map((s) => s.end)) + 1.6;
      return { lanes, segsAbs, captions, total, start: trace.start };
    }

    function reset() {
      arch.querySelectorAll(".lit").forEach((el) => el.classList.remove("lit"));
      arch.querySelectorAll(".edges path").forEach((p) => p.setAttribute("marker-end", "url(#arrow)"));
      packets.forEach((p) => p.remove());
      packets = plan.lanes.map(() => svgEl("circle", { class: `packet packet-${current}`, r: 6.5, opacity: 0 }, packetsG));
      document.getElementById(plan.start).classList.add("lit");
      shownCaption = -1;
      litCount = 0;
      t0 = null;
    }

    function setCaption(i) {
      const c = plan.captions[i];
      if (!c) return;
      traceText.classList.add("swap");
      setTimeout(() => {
        traceText.textContent = c.text;
        traceText.classList.remove("swap");
      }, 180);
      traceStep.textContent = `${i + 1} / ${plan.captions.length}`;
    }

    function frame(now) {
      if (t0 === null) t0 = now;
      const t = ((now - t0) / 1000) % plan.total;
      if (t < 0.05 && litCount > 0) {
        // a new cycle
        reset();
        t0 = now;
        sound.play("resolve", 2000);
      }
      let lit = 0;
      plan.segsAbs.forEach((s) => {
        const packet = packets[s.lane];
        if (t >= s.start && t <= s.end) {
          const pt = s.path.getPointAtLength(((t - s.start) / (s.end - s.start)) * s.len);
          packet.setAttribute("cx", pt.x.toFixed(1));
          packet.setAttribute("cy", pt.y.toFixed(1));
          packet.setAttribute("opacity", "1");
          if (!s.path.classList.contains("lit")) {
            s.path.classList.add("lit");
            s.path.setAttribute("marker-end", `url(#arrow-${current})`);
          }
        }
        if (t > s.end) {
          lit++;
          const node = document.getElementById(END_NODE[s.id]);
          if (node && !node.classList.contains("lit")) {
            node.classList.add("lit");
            const accent = ACCENT[END_NODE[s.id]];
            if (accent) sound.play(accent, 150);
            else sound.hop(s.index + (s.lane ? plan.lanes[s.lane].after : 0), current);
            if (accent === "kafka") sound.hop(s.index, current); // the bass note carries the melody too
          }
        }
      });
      litCount = lit;
      // hide packets whose lane is between segments or finished
      plan.lanes.forEach((lane, li) => {
        const active = lane.segs.some((s) => t >= s.start + lane.offset && t <= s.end + lane.offset);
        if (!active) packets[li].setAttribute("opacity", "0");
      });
      let ci = -1;
      plan.captions.forEach((c, i) => {
        if (t >= c.at) ci = i;
      });
      if (ci !== shownCaption && ci >= 0) {
        shownCaption = ci;
        setCaption(ci);
      }
      raf = requestAnimationFrame(frame);
    }

    function select(name) {
      current = name;
      arch.dataset.trace = name;
      caption.style.setProperty("--trace-color", name === "order" ? "var(--ai)" : "var(--shu)");
      document.querySelectorAll(".trace-btn").forEach((b) => b.setAttribute("aria-selected", String(b.dataset.trace === name)));
      plan = build(name);
      reset();
      setCaption(0);
      shownCaption = 0;
      if (reducedMotion) {
        // Static: light the whole trace and show the first caption.
        plan.segsAbs.forEach((s) => {
          s.path.classList.add("lit");
          s.path.setAttribute("marker-end", `url(#arrow-${name})`);
          document.getElementById(END_NODE[s.id]).classList.add("lit");
        });
      }
    }

    document.querySelectorAll(".trace-btn").forEach((b) =>
      b.addEventListener("click", () => {
        if (b.dataset.trace === current) return;
        select(b.dataset.trace);
        sound.play("sweep", 300);
      }),
    );
    select("order");

    if (!reducedMotion) {
      const play = () => {
        if (running) return;
        running = true;
        t0 = null;
        raf = requestAnimationFrame(frame);
      };
      const pause = () => {
        running = false;
        cancelAnimationFrame(raf);
      };
      let visible = false;
      const sync = () => (visible && !document.hidden ? play() : pause());
      new IntersectionObserver(
        (entries) => {
          visible = entries[0].isIntersecting;
          sync();
        },
        { threshold: 0.2 },
      ).observe(arch);
      document.addEventListener("visibilitychange", sync);
    }
  }

  // ------------------------------------------------------------------ Counters and bars
  const fmt = (n) => Math.round(n).toLocaleString("en-US");
  const counters = Array.from(document.querySelectorAll("[data-count]"));
  const bars = Array.from(document.querySelectorAll(".sbar-fill"));
  const runCounter = (el) => {
    const target = Number(el.dataset.count);
    const suffix = el.dataset.suffix || "";
    if (reducedMotion) {
      el.textContent = fmt(target) + suffix;
      return;
    }
    const start = performance.now();
    const dur = 1400;
    const step = (now) => {
      const t = clamp((now - start) / dur, 0, 1);
      el.textContent = fmt(target * easeOut(t)) + suffix;
      if (t < 1) requestAnimationFrame(step);
    };
    requestAnimationFrame(step);
  };
  if ("IntersectionObserver" in window) {
    const io = new IntersectionObserver(
      (entries) => {
        entries.forEach((e) => {
          if (!e.isIntersecting) return;
          if (e.target.dataset.count) {
            runCounter(e.target);
            sound.play("count", 4000);
          } else e.target.style.width = `${e.target.dataset.w}%`;
          io.unobserve(e.target);
        });
      },
      { threshold: 0.4 },
    );
    counters.forEach((el) => io.observe(el));
    bars.forEach((el) => io.observe(el));
  } else {
    counters.forEach(runCounter);
    bars.forEach((el) => (el.style.width = `${el.dataset.w}%`));
  }

  // ------------------------------------------------------------------ Game day
  const FAILS = {
    valkey: {
      see: "Nothing on orders or tracking. New driver assignments pause for a few seconds.",
      do: "Tracking reads Kafka, not the cache, so customers notice nothing. fleet-state replays recent locations to rebuild Valkey, and dispatch waits for the rebuild instead of guessing.",
      state: { valkey: "down", dispatch: "degraded" },
    },
    kafka: {
      see: "Orders are still accepted. Status updates and the live map arrive late.",
      do: "Outbox rows wait in PostgreSQL and drain in order when Kafka returns. Fleet ingest sheds load with 503 and Retry-After, and driver apps back off.",
      state: { kafka: "down", dispatch: "degraded", tracking: "degraded" },
    },
    db: {
      see: "Creating an order fails for one to two minutes. Tracking keeps working.",
      do: "Clients get 503 with Retry-After. Idempotency keys make every retry safe, so nothing is created twice once the new primary is up.",
      state: { pg: "down", orders: "degraded" },
    },
    osrm: {
      see: "ETAs are a little less exact.",
      do: "Dispatch falls back to straight-line distance with a detour factor and widens quotes until routing is back. Nothing stops.",
      state: { osrm: "down", dispatch: "degraded" },
    },
    surge: {
      see: "Longer ETAs, fewer bookable windows, a surge fee. The map keeps moving.",
      do: "Quotes follow driver supply. If capacity runs out, shedding goes in priority order: orders first, then dispatch, then tracking, then history.",
      state: { dispatch: "degraded", tracking: "degraded" },
    },
    az: {
      see: "A few retries, then business as usual.",
      do: "Pods spread across three zones with disruption budgets, a Multi-AZ database, and Kafka with replication factor 3. One zone can disappear.",
      state: { orders: "degraded", kafka: "degraded" },
    },
  };
  const gdButtons = Array.from(document.querySelectorAll(".gd-buttons button"));
  if (gdButtons.length) {
    const see = document.getElementById("gd-see");
    const doEl = document.getElementById("gd-do");
    const cols = document.querySelector(".gd-cols");
    const chain = Array.from(document.querySelectorAll(".gd-chain li"));
    const show = (key, withSound) => {
      const f = FAILS[key];
      gdButtons.forEach((b) => b.setAttribute("aria-selected", String(b.dataset.fail === key)));
      chain.forEach((li) => li.setAttribute("data-state", f.state[li.dataset.c] || "ok"));
      cols.classList.add("swap");
      setTimeout(() => {
        see.textContent = f.see;
        doEl.textContent = f.do;
        cols.classList.remove("swap");
      }, 200);
      if (withSound) {
        sound.play("alert", 300);
        setTimeout(() => sound.play("handled", 300), 700);
      }
    };
    gdButtons.forEach((b) => b.addEventListener("click", () => show(b.dataset.fail, true)));
    show("valkey", false);
  }
})();
