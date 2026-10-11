(() => {
  const d = document.documentElement;
  d.classList.add("js");
  const still = matchMedia("(prefers-reduced-motion: reduce)").matches;

  // Drawers slide out of the wall once, as they scroll into view.
  const drawers = document.querySelectorAll(".drawer");
  if (still || !("IntersectionObserver" in window)) drawers.forEach(x => x.classList.add("out"));
  else {
    const io = new IntersectionObserver(es => es.forEach(e => {
      if (e.isIntersecting) { e.target.classList.add("out"); io.unobserve(e.target); }
    }), { rootMargin: "0px 0px -12% 0px" });
    drawers.forEach(x => io.observe(x));
  }

  // Reading windows flip through their readings: reads need no key.
  if (!still) {
    const wins = [...document.querySelectorAll(".win[data-r]")];
    const idx = new Map(wins.map(w => [w, 0]));
    setInterval(() => {
      if (document.hidden) return;
      for (let i = 0; i < 3; i++) {
        const w = wins[Math.floor(Math.random() * wins.length)];
        if (w.offsetParent === null) continue;
        const r = w.dataset.r.split("|"), n = (idx.get(w) + 1) % r.length;
        idx.set(w, n);
        w.classList.remove("flip"); void w.offsetWidth; w.classList.add("flip");
        setTimeout(() => { w.textContent = r[n]; }, 250);
      }
    }, 1600);
  }

  // The deposit box: two keys, then the register.
  const v = document.getElementById("vault");
  if (!v) return;
  const ka = document.getElementById("ka"), kb = document.getElementById("kb");
  const sig = document.getElementById("sig"), st = document.getElementById("status");
  const cmd = "$ rackhands --allow power power db-01 force-off\n";
  const say = t => { st.textContent = t; };
  function render(jam) {
    const a = v.dataset.a === "1", b = v.dataset.b === "1";
    ka.setAttribute("aria-pressed", a); kb.setAttribute("aria-pressed", b);
    v.classList.toggle("unlocked", a && b);
    sig.disabled = !(a && b);
    if (!a) say('$ rackhands power db-01 force-off\nrackhands: operation not allowed: "power" is off; start with --allow power (dangerous operations are disabled by default)\nexit 3');
    else if (!b) say(cmd + 'rackhands: operation not allowed: target "db-01" does not list "power" under allow: in the config file\nexit 3');
    else say(cmd + "PLAN  power force-off db-01 (Dell PowerEdge R750, ResetType=ForceOff, currently On)\nType the target name (db-01) to confirm:");
    if (jam && !(a && b)) { v.classList.remove("jam"); void v.offsetWidth; v.classList.add("jam"); }
  }
  ka.onclick = () => { v.dataset.a = v.dataset.a === "1" ? "0" : "1"; close(); render(true); };
  kb.onclick = () => { v.dataset.b = v.dataset.b === "1" ? "0" : "1"; close(); render(true); };
  function close() { v.classList.remove("open"); sig.value = ""; }
  sig.addEventListener("keydown", e => {
    if (e.key !== "Enter") return;
    if (sig.value.trim() === "db-01") {
      v.classList.add("open");
      say(cmd + "Type the target name (db-01) to confirm: db-01\nDONE  db-01: accepted (HTTP 204)\n(audit: planned → executed)");
    } else {
      say(cmd + "Type the target name (db-01) to confirm: " + sig.value + "\nrackhands: not confirmed; nothing sent\nexit 3");
    }
  });
  sig.addEventListener("input", () => {
    if (sig.value.trim() === "db-01" && !v.classList.contains("open")) say(cmd + "Type the target name (db-01) to confirm: db-01⏎  (press Enter)");
  });
  document.getElementById("reset").onclick = () => { v.dataset.a = v.dataset.b = "0"; close(); render(false); };
  render(false);
})();
