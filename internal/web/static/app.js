const APP_W = 1024;
const APP_H = 600;
const DAY = 86400000;
const WEEKDAYS = ["dimanche", "lundi", "mardi", "mercredi", "jeudi", "vendredi", "samedi"];
const WEEKDAYS_SHORT = ["dim.", "lun.", "mar.", "mer.", "jeu.", "ven.", "sam."];
const MONTHS = ["janvier", "février", "mars", "avril", "mai", "juin", "juillet", "août", "septembre", "octobre", "novembre", "décembre"];
const COLLECTOR_ORDER = ["mail", "github", "umami", "stripe", "qonto", "weather", "calendar", "health", "registry", "maintenant", "shm"];

const nfInt = new Intl.NumberFormat("fr-FR", { maximumFractionDigits: 0 });
const nf1 = new Intl.NumberFormat("fr-FR", { minimumFractionDigits: 1, maximumFractionDigits: 1 });
const nf2 = new Intl.NumberFormat("fr-FR", { minimumFractionDigits: 2, maximumFractionDigits: 2 });

const $ = (id) => document.getElementById(id);
const SVG_NS = "http://www.w3.org/2000/svg";

let current = null;
let firstVersion = null;
let pollTimer = null;
let eventSeq = null;
let toastTimer = null;
const TOAST_MS = 20000;
const shown = new Map();

/* ---------- DOM helpers ---------- */

function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (v == null || v === false) continue;
      if (k === "class") el.className = v;
      else if (k === "text") el.textContent = v;
      else el.setAttribute(k, v);
    }
  }
  for (const c of children.flat(Infinity)) {
    if (c == null || c === false) continue;
    el.append(typeof c === "string" || typeof c === "number" ? document.createTextNode(String(c)) : c);
  }
  return el;
}

function svg(tag, attrs, ...children) {
  const el = document.createElementNS(SVG_NS, tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null) continue;
    if (k === "href") el.setAttributeNS("http://www.w3.org/1999/xlink", "href", v);
    el.setAttribute(k, v);
  }
  for (const c of children.flat(Infinity)) if (c) el.append(c);
  return el;
}

function icon(name, cls = "icon") {
  return svg("svg", { class: cls }, svg("use", { href: `#i-${name}` }));
}

function replaceChildren(el, ...children) {
  el.replaceChildren(...children.flat().filter(Boolean));
}

/* ---------- formatting ---------- */

const MINUS = "−";

function fmtInt(n) {
  return nfInt.format(Math.round(n));
}

function fmtSigned(n, formatter = fmtInt) {
  if (n > 0) return "+" + formatter(n);
  if (n < 0) return MINUS + formatter(-n);
  return formatter(0);
}

function fmtEur(cents, opts = {}) {
  const v = cents / 100;
  const abs = Math.abs(v);
  const decimals = opts.decimals ?? (abs < 10000 && !Number.isInteger(abs) ? 2 : 0);
  const f = decimals === 2 ? nf2 : nfInt;
  const sign = v < 0 ? MINUS : opts.plus && v > 0 ? "+" : "";
  return `${sign}${f.format(abs)} €`;
}

function fmtPct(x) {
  if (x == null) return "—";
  const s = nf1.format(Math.abs(x));
  return `${x > 0 ? "+" : x < 0 ? MINUS : ""}${s} %`;
}

function fmtTime(d) {
  return `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
}

function startOfDay(d) {
  const x = new Date(d);
  x.setHours(0, 0, 0, 0);
  return x;
}

function ageShort(at, now) {
  const s = Math.max(0, (now - at) / 1000);
  if (s < 60) return "à l'instant";
  if (s < 3600) return `${Math.floor(s / 60)} min`;
  if (s < 86400 && startOfDay(now).getTime() <= at.getTime()) return `${Math.floor(s / 3600)} h`;
  const days = Math.round((startOfDay(now) - startOfDay(at)) / DAY);
  if (days <= 1) return "hier";
  if (days < 7) return `${days} j`;
  return `${at.getDate()} ${MONTHS[at.getMonth()].slice(0, 4)}`;
}

function ageLong(at, now) {
  const s = Math.max(0, (now - at) / 1000);
  if (s < 90) return "à l'instant";
  if (s < 3600) return `il y a ${Math.floor(s / 60)} min`;
  if (s < 86400) return `il y a ${Math.floor(s / 3600)} h`;
  return `il y a ${Math.floor(s / 86400)} j`;
}

function dayLabel(d, now) {
  const diff = Math.round((startOfDay(d) - startOfDay(now)) / DAY);
  if (diff === 0) return "aujourd'hui";
  if (diff === 1) return "demain";
  return `${WEEKDAYS[d.getDay()]} ${d.getDate()}`;
}

/* ---------- animated numbers ---------- */

function num(key, value, format, el) {
  const prev = shown.get(key);
  shown.set(key, value);
  if (prev == null || prev === value || !Number.isFinite(prev) || !Number.isFinite(value)) {
    el.textContent = format(value);
    return el;
  }
  const start = performance.now();
  const dur = 700;
  el.classList.add("tick");
  const step = (t) => {
    const p = Math.min(1, (t - start) / dur);
    const e = 1 - Math.pow(1 - p, 3);
    el.textContent = format(prev + (value - prev) * e);
    if (p < 1) requestAnimationFrame(step);
    else el.classList.remove("tick");
  };
  requestAnimationFrame(step);
  return el;
}

/* ---------- charts ---------- */

function spark(points, w, hgt, opts = {}) {
  const vals = (points || []).map((p) => p.v).filter((v) => Number.isFinite(v));
  const el = svg("svg", { class: "spark", width: w, height: hgt, viewBox: `0 0 ${w} ${hgt}` });
  if (vals.length < 2) return el;
  let min = Math.min(...vals);
  let max = Math.max(...vals);
  if (max === min) {
    min -= 1;
    max += 1;
  }
  const pad = (max - min) * 0.08;
  min -= pad;
  max += pad;
  const sx = (i) => (i / (vals.length - 1)) * (w - 4) + 2;
  const sy = (v) => hgt - 3 - ((v - min) / (max - min)) * (hgt - 6);
  let d = "";
  vals.forEach((v, i) => {
    d += `${i === 0 ? "M" : "L"}${sx(i).toFixed(1)} ${sy(v).toFixed(1)}`;
  });
  const area = `${d}L${sx(vals.length - 1).toFixed(1)} ${hgt}L${sx(0).toFixed(1)} ${hgt}Z`;
  el.append(svg("path", { class: "area", d: area }));
  el.append(svg("path", { class: `line${opts.accent ? " accent" : ""}`, d }));
  el.append(svg("circle", { cx: sx(vals.length - 1).toFixed(1), cy: sy(vals[vals.length - 1]).toFixed(1), r: 2.2 }));
  return el;
}

function bars(days, w, hgt) {
  const el = svg("svg", { class: "bars", width: w, height: hgt, viewBox: `0 0 ${w} ${hgt}` });
  if (!days || !days.length) return el;
  const max = Math.max(1, ...days.map((d) => d.v));
  const gap = 2;
  const bw = (w - gap * (days.length - 1)) / days.length;
  days.forEach((d, i) => {
    const bh = Math.max(1.5, (d.v / max) * (hgt - 1));
    const cls = [];
    if (i === days.length - 1) cls.push("last");
    const wd = new Date(d.d + "T12:00:00").getDay();
    if (wd === 0 || wd === 6) cls.push("weekend");
    el.append(svg("rect", { x: (i * (bw + gap)).toFixed(1), y: (hgt - bh).toFixed(1), width: bw.toFixed(1), height: bh.toFixed(1), rx: 1.5, class: cls.join(" ") || null }));
  });
  return el;
}

/* ---------- tile scaffolding ---------- */

function entryOf(st, name) {
  return st.collectors && st.collectors[name];
}

function worstState(...entries) {
  const states = entries.filter(Boolean).map((e) => e.status.state);
  if (!states.length) return "off";
  if (states.includes("error")) return "error";
  if (states.includes("stale")) return "stale";
  if (states.includes("init")) return "init";
  return "ok";
}

function tileHead(iconName, title, entry, now, extra) {
  const head = h("div", { class: "tile-head" }, icon(iconName), h("span", { text: title }), h("span", { class: "spacer" }));
  if (extra) head.append(extra);
  if (entry) {
    const st = entry.status;
    let age = "";
    if (st.state === "stale" && st.last_ok) age = ageLong(new Date(st.last_ok), now);
    else if (st.state === "error") age = st.last_ok && new Date(st.last_ok).getTime() > 0 ? `erreur · ${ageLong(new Date(st.last_ok), now)}` : "erreur";
    else if (st.state === "init") age = "en attente";
    if (age) head.append(h("span", { class: "age", text: age }));
  }
  head.append(h("span", { class: "dot" }));
  return head;
}

function setTile(id, state, head, body) {
  const tile = $(id);
  tile.dataset.state = state;
  replaceChildren(tile, head, h("div", { class: "tile-body" }, body));
}

function emptyBody(entry, fallback = "désactivé") {
  if (!entry) return h("div", { class: "tile-empty", text: fallback });
  if (entry.status.state === "init") return h("div", { class: "tile-empty", text: "première collecte en cours…" });
  const err = entry.status.error || "indisponible";
  return h("div", { class: "tile-empty", text: err.length > 90 ? err.slice(0, 90) + "…" : err });
}

/* ---------- top bar ---------- */

function renderClock(now) {
  const t = $("time");
  replaceChildren(t, String(now.getHours()).padStart(2, "0"), h("span", { class: "colon", text: ":" }), String(now.getMinutes()).padStart(2, "0"));
  $("date").textContent = `${WEEKDAYS[now.getDay()]} ${now.getDate()} ${MONTHS[now.getMonth()]}`;
}

function wmoIcon(code, isDay) {
  if (code === 0) return isDay ? "sun" : "moon";
  if (code <= 2) return isDay ? "cloud-sun" : "cloud-moon";
  if (code === 3) return "cloud";
  if (code <= 48) return "cloud-fog";
  if (code <= 57) return "cloud-drizzle";
  if (code <= 67) return "cloud-rain";
  if (code <= 77) return "snow";
  if (code <= 82) return "cloud-rain";
  if (code <= 86) return "snow";
  return "cloud-lightning";
}

function wmoLabel(code) {
  if (code === 0) return "dégagé";
  if (code <= 2) return "peu nuageux";
  if (code === 3) return "couvert";
  if (code <= 48) return "brouillard";
  if (code <= 57) return "bruine";
  if (code <= 67) return "pluie";
  if (code <= 77) return "neige";
  if (code <= 82) return "averses";
  if (code <= 86) return "neige";
  return "orage";
}

function renderWeather(entry) {
  const el = $("weather");
  const d = entry && entry.data;
  if (!d) {
    replaceChildren(el);
    return;
  }
  const temp = h("span", { class: "wx-temp" });
  num("wx.temp", d.temp, (v) => `${Math.round(v)}°`, temp);
  replaceChildren(
    el,
    h("span", { class: "wx-icon", title: wmoLabel(d.code) }, icon(wmoIcon(d.code, d.is_day), "")),
    temp,
    h("span", { class: "wx-meta" },
      h("span", { class: "wx-range", text: `↑${Math.round(d.tmax)}° ↓${Math.round(d.tmin)}°` }),
      h("span", { class: "wx-rain", text: d.rain_pct > 0 ? `${wmoLabel(d.code)} · pluie ${d.rain_pct} %` : wmoLabel(d.code) }),
    ),
  );
}

function renderMailBadge(entry) {
  const el = $("mailbadge");
  const d = entry && entry.data;
  if (!d) {
    replaceChildren(el);
    return;
  }
  const count = h("span", { class: `count${d.unseen === 0 ? " zero" : ""}` });
  num("badge.unseen", d.unseen, fmtInt, count);
  replaceChildren(el, icon("mail"), count);
}

/* ---------- tiles ---------- */

function renderQonto(st, now) {
  const e = entryOf(st, "qonto");
  const head = tileHead("wallet", "Trésorerie", e, now);
  const d = e && e.data;
  if (!d) return setTile("tile-qonto", e ? e.status.state : "off", head, emptyBody(e));
  const total = h("div", { class: "hero-num" });
  num("qonto.total", d.total_cents, (v) => fmtEur(v, { decimals: 0 }), total);
  const orgs = (d.orgs || []).map((o) =>
    h("div", { class: "row" },
      h("span", { class: "name", text: o.name }),
      o.error ? h("span", { class: "sub", text: "indispo." }) : null,
      h("span", { class: `num${o.warn ? " warn" : ""}`, text: fmtEur(o.total_cents, { decimals: 0 }) }),
    ),
  );
  const monthIn = (d.orgs || []).reduce((s, o) => s + (o.month ? o.month.in_cents : 0), 0);
  const monthOut = (d.orgs || []).reduce((s, o) => s + (o.month ? o.month.out_cents : 0), 0);
  const recent = (d.orgs || [])
    .flatMap((o) => (o.recent || []).map((t) => ({ ...t, org: o.name })))
    .sort((a, b) => new Date(b.at) - new Date(a.at))
    .slice(0, 3)
    .map((t) =>
      h("div", { class: "row dim" },
        h("span", { class: "name", text: t.label || t.counterparty || t.type || "—" }),
        h("span", { class: "sub", text: ageShort(new Date(t.at), now) }),
        h("span", { class: `num${t.side === "credit" ? " pos" : ""}`, text: fmtEur(t.side === "credit" ? t.amount_cents : -t.amount_cents, { plus: true }) }),
      ),
    );
  const body = [
    h("div", { class: "hero" },
      h("div", null, total, h("div", { class: "hero-label", text: `Qonto · ${(d.orgs || []).length} compte${(d.orgs || []).length > 1 ? "s" : ""}` })),
      h("div", { class: "hero-side" }, spark(d.series30, 110, 34)),
    ),
    h("div", { class: "rows" }, orgs),
    h("div", { class: "sep" }),
    h("div", { class: "kv" },
      h("span", { class: "k", text: "ce mois" }),
      h("span", { class: "v" }, h("span", { class: "pos", text: fmtEur(monthIn, { plus: true, decimals: 0 }) }), h("span", { class: "dim", text: " · " }), h("span", { text: fmtEur(-monthOut, { decimals: 0 }) })),
    ),
    recent.length ? h("div", { class: "rows" }, recent) : null,
  ];
  setTile("tile-qonto", e.status.state, head, body);
}

function renderStripe(st, now) {
  const e = entryOf(st, "stripe");
  const head = tileHead("card", "Revenus", e, now);
  const d = e && e.data;
  if (!d) return setTile("tile-stripe", e ? e.status.state : "off", head, emptyBody(e));
  const t = d.total || {};
  const mrr = h("div", { class: "hero-num" });
  num("stripe.mrr", t.mrr_cents, (v) => fmtEur(v, { decimals: 0 }), mrr);
  const subs = (d.accounts || []).reduce((s, a) => s + (a.subs || 0), 0);
  const label = h("div", { class: "hero-label" }, h("span", { text: `MRR · ${subs} abonnement${subs > 1 ? "s" : ""}` }));
  if (t.mrr_delta30_cents != null && t.mrr_delta30_cents !== 0) {
    label.append(h("span", { class: `chip ${t.mrr_delta30_cents > 0 ? "up" : "down"}`, text: `${fmtEur(t.mrr_delta30_cents, { plus: true, decimals: 0 })} / 30 j` }));
  }
  const monthNum = h("div", { class: "kpi-num" });
  num("stripe.month", t.month_net_cents, (v) => fmtEur(v, { decimals: 0 }), monthNum);
  const accounts = (d.accounts || []).map((a) =>
    h("div", { class: `row${a.mrr_cents ? "" : " dim"}` },
      h("span", { class: "name", text: a.name }),
      a.error ? h("span", { class: "sub", text: a.stale ? "ancien" : "indispo." }) : a.livemode === false ? h("span", { class: "sub", text: "test" }) : null,
      h("span", { class: "num small", text: a.month_net_cents ? fmtEur(a.month_net_cents, { decimals: 0 }) : "" }),
      h("span", { class: `num${a.mrr_cents ? "" : " muted"}`, text: `${fmtEur(a.mrr_cents, { decimals: 0 })}/m` }),
    ),
  );
  const last = (d.accounts || []).map((a) => a.last && { ...a.last, account: a.name }).filter(Boolean).sort((a, b) => new Date(b.at) - new Date(a.at))[0];
  const body = [
    h("div", { class: "hero" },
      h("div", null, mrr, label),
      h("div", { class: "hero-side" }, spark(d.series30, 110, 34, { accent: true })),
    ),
    h("div", { class: "kpi2" },
      h("div", { class: "kpi" }, monthNum, h("div", { class: "kpi-label", text: `ce mois · ${t.payments || 0} paiement${t.payments > 1 ? "s" : ""}` })),
      h("div", { class: "kpi" },
        h("div", { class: "kpi-num", text: fmtEur(t.available_cents || 0, { decimals: 0 }) }),
        h("div", { class: "kpi-label", text: `disponible · ${fmtEur(t.pending_cents || 0, { decimals: 0 })} en attente` }),
      ),
    ),
    h("div", { class: "rows" }, accounts),
    last
      ? h("div", { class: "kv" },
          h("span", { class: "k", text: "dernier paiement" }),
          h("span", { class: "v" }, h("span", { text: `${last.label} · ${fmtEur(last.amount_cents)}` }), h("span", { class: "dim", text: ` · ${ageShort(new Date(last.at), now)}` })),
        )
      : null,
  ];
  setTile("tile-stripe", e.status.state, head, body);
}

function renderStars(st, now) {
  const e = entryOf(st, "github");
  const head = tileHead("star", "GitHub", e, now);
  const d = e && e.data;
  if (!d) return setTile("tile-stars", e ? e.status.state : "off", head, emptyBody(e));
  const total = h("div", { class: "hero-num" });
  num("github.stars", d.stars, fmtInt, total);
  total.append(h("span", { class: "unit", text: "★" }));
  const label = h("div", { class: "hero-label" }, h("span", { text: `étoiles · ${(d.repos || []).length} dépôts` }));
  if (d.delta7 != null) label.append(h("span", { class: `chip ${d.delta7 > 0 ? "up" : "down"}`, text: `${fmtSigned(d.delta7)} / 7 j` }));
  const repos = (d.repos || []).slice(0, 3).map((r) =>
    h("div", { class: "row" },
      h("span", { class: "name", text: r.name }),
      h("span", { class: "sub", text: r.issues || r.prs ? `${r.issues} issues · ${r.prs} PR` : "" }),
      h("span", { class: `num small${r.delta7 > 0 ? " up" : ""}`, text: r.delta7 != null ? fmtSigned(r.delta7) : "" }),
      h("span", { class: "num", text: fmtInt(r.stars) }),
    ),
  );
  const body = [
    h("div", { class: "hero" }, h("div", null, total, label), h("div", { class: "hero-side" }, spark(d.series30, 110, 34, { accent: true }))),
    h("div", { class: "rows" }, repos),
  ];
  setTile("tile-stars", e.status.state, head, body);
}

function renderVisits(st, now) {
  const e = entryOf(st, "umami");
  const head = tileHead("eye", "Visites · 7 jours", e, now);
  const d = e && e.data;
  if (!d) return setTile("tile-visits", e ? e.status.state : "off", head, emptyBody(e));
  const total = h("div", { class: "hero-num" });
  num("umami.visits", d.visits7, fmtInt, total);
  const label = h("div", { class: "hero-label" });
  if (d.delta_pct != null) label.append(h("span", { class: `chip ${d.delta_pct >= 0 ? "up" : "down"}`, text: fmtPct(d.delta_pct) }));
  label.append(h("span", { class: `chip${d.live > 0 ? " live" : ""}` }, d.live > 0 ? h("span", { class: "pulse" }) : null, `${d.live} en direct`));
  const sites = (d.sites || []).slice(0, 2).map((s) =>
    h("div", { class: "row" },
      h("span", { class: "name", text: s.name }),
      h("span", { class: `num small${s.delta_pct > 0 ? " up" : ""}`, text: s.delta_pct != null ? fmtPct(s.delta_pct) : "" }),
      h("span", { class: "num", text: fmtInt(s.visits7) }),
    ),
  );
  const body = [
    h("div", { class: "hero" }, h("div", null, total, label), h("div", { class: "hero-side" }, bars(d.bars14, 140, 40))),
    h("div", { class: "rows" }, sites),
  ];
  setTile("tile-visits", e.status.state, head, body);
}

function renderTraction(st, now) {
  const reg = entryOf(st, "registry");
  const shm = entryOf(st, "shm");
  const gh = entryOf(st, "github");
  const mnt = entryOf(st, "maintenant");
  const state = worstState(reg, shm, mnt);
  const head = tileHead("package", "Traction Maintenant", reg || mnt || shm, now);
  if (!reg && !shm && !mnt) return setTile("tile-maint", "off", head, emptyBody(null));

  const minis = [];
  const pulls = reg && reg.data && (reg.data.items || []).find((i) => i.name === "maintenant");
  if (pulls) {
    const n = h("div", { class: "mini-num" });
    num("reg.pulls", pulls.total, fmtInt, n);
    minis.push(h("div", { class: "mini" }, n, h("div", { class: "mini-label", text: `pulls ${pulls.kind === "ghcr" ? "GHCR" : "Docker Hub"}` }),
      h("div", { class: `mini-delta${pulls.delta7 ? "" : " flat"}`, text: pulls.delta7 != null ? `${fmtSigned(pulls.delta7)} / 7 j` : "—" })));
  }
  if (shm && shm.data) {
    const items = shm.data.items || [];
    const inst = items.find((i) => i.key === "instances") || items[0];
    const active = items.find((i) => i.key === "active");
    if (inst) {
      const n = h("div", { class: "mini-num" });
      num("shm.instances", inst.value, fmtInt, n);
      minis.push(h("div", { class: "mini" }, n, h("div", { class: "mini-label", text: (inst.label || "instances").toLowerCase() }),
        h("div", { class: `mini-delta${active ? "" : " flat"}`, text: active ? `${fmtInt(active.value)} actives` : inst.delta7 != null ? `${fmtSigned(inst.delta7)} / 7 j` : "—" })));
    }
  }
  const repo = gh && gh.data && (gh.data.repos || []).find((r) => r.name === "maintenant");
  if (repo && repo.views14 != null) {
    const n = h("div", { class: "mini-num" });
    num("gh.views", repo.views14, fmtInt, n);
    minis.push(h("div", { class: "mini" }, n, h("div", { class: "mini-label", text: "vues dépôt · 14 j" }),
      h("div", { class: "mini-delta flat", text: repo.clones14 != null ? `${fmtInt(repo.clones14)} clones` : "" })));
  }

  const lines = [];
  if (mnt && mnt.data) {
    for (const i of mnt.data.instances || []) {
      const ok = i.status === "operational";
      const degraded = i.status === "degraded" || i.status === "under_maintenance";
      const statusFr = { operational: "opérationnel", degraded: "dégradé", partial_outage: "panne partielle", major_outage: "panne majeure", under_maintenance: "maintenance" }[i.status] || i.status || "inconnu";
      const parts = [h("span", { class: `dot ${i.error ? "stale" : ok ? "ok" : degraded ? "warn" : "err"}` }), h("span", { class: "name", text: i.name }), h("span", { text: statusFr })];
      if (i.alerts && (i.alerts.critical || i.alerts.warning)) {
        const n = i.alerts.critical + i.alerts.warning;
        parts.push(h("span", { text: "·" }), h("span", { class: i.alerts.critical ? "err" : "warn", text: `${n} alerte${n > 1 ? "s" : ""}` }));
      }
      if (i.incidents) parts.push(h("span", { text: "·" }), h("span", { class: "err", text: `${i.incidents} incident${i.incidents > 1 ? "s" : ""}` }));
      if (i.error) parts.push(h("span", { text: "·" }), h("span", { class: "warn", text: "injoignable" }));
      lines.push(h("div", { class: "instance" }, parts));
      const detail = [];
      if (i.hosts != null) detail.push(h("span", { class: "num", text: String(i.hosts) }), h("span", { text: `hôte${i.hosts > 1 ? "s" : ""}` }));
      if (i.containers_total != null) detail.push(detail.length ? h("span", { text: "·" }) : null, h("span", { class: "num", text: `${i.containers_running}/${i.containers_total}` }), h("span", { text: "conteneurs" }));
      if (i.components_total) detail.push(detail.length ? h("span", { text: "·" }) : null, h("span", { class: "num", text: `${i.components_total - i.components_down}/${i.components_total}` }), h("span", { text: "composants" }));
      if (detail.length) lines.push(h("div", { class: "instance detail" }, detail));
    }
  }
  const body = [minis.length ? h("div", { class: "trio" }, minis) : h("div", { class: "tile-empty", text: "aucune source" }), lines];
  setTile("tile-maint", state, head, body);
}

function renderInbox(st, now) {
  const e = entryOf(st, "mail");
  const head = tileHead("mail", "Boîte de réception", e, now);
  const d = e && e.data;
  if (!d) return setTile("tile-inbox", e ? e.status.state : "off", head, emptyBody(e));
  const n = h("span", { class: "hero-num" });
  num("mail.unseen", d.unseen, fmtInt, n);
  const mails = (d.items || []).slice(0, 5).map((m) =>
    h("div", { class: "mail" },
      h("span", { class: "from", text: m.from || "—" }),
      h("span", { class: "age", text: ageShort(new Date(m.at), now) }),
      h("span", { class: "subject", text: m.subject || "(sans objet)" }),
    ),
  );
  const body = [
    h("div", { class: "inbox-hero" }, n, h("span", { class: "label", text: d.unseen > 1 ? "non lus" : "non lu" }), h("span", { class: "account", text: d.account || "" })),
    mails.length ? h("div", { class: "mails" }, mails) : h("div", { class: "tile-empty", text: "rien en attente" }),
  ];
  setTile("tile-inbox", e.status.state, head, body);
}

function eventRow(ev, now, withDay) {
  const start = new Date(ev.start);
  const end = new Date(ev.end);
  const cls = ["event"];
  if (ev.in_progress || (start <= now && now < end)) cls.push("now");
  else if (end <= now) cls.push("past");
  const when = ev.all_day ? "journée" : fmtTime(start);
  const title = h("span", { class: "title", text: ev.title });
  if (ev.location) title.append(h("span", { class: "loc", text: ` · ${ev.location}` }));
  return h("div", { class: cls.join(" ") }, h("span", { class: "when", text: withDay ? `${WEEKDAYS_SHORT[start.getDay()]} ${when}` : when }), title);
}

function renderAgenda(st, now) {
  const e = entryOf(st, "calendar");
  const head = tileHead("calendar", "Agenda", e, now);
  const d = e && e.data;
  if (!d) return setTile("tile-agenda", e ? e.status.state : "off", head, emptyBody(e));
  const today = (d.today || []).filter((ev) => new Date(ev.end) > now || ev.in_progress).slice(0, 4);
  const upcoming = (d.upcoming || []).slice(0, 4);
  const groups = [];
  groups.push(h("div", { class: "agenda-group" },
    h("div", { class: "agenda-title", text: "aujourd'hui" }),
    today.length ? today.map((ev) => eventRow(ev, now, false)) : h("div", { class: "tile-empty", text: "rien de prévu" }),
  ));
  if (upcoming.length) {
    let lastDay = "";
    const rows = [];
    for (const ev of upcoming) {
      const label = dayLabel(new Date(ev.start), now);
      if (label !== lastDay) {
        rows.push(h("div", { class: "agenda-title", text: label }));
        lastDay = label;
      }
      rows.push(eventRow(ev, now, false));
    }
    groups.push(h("div", { class: "agenda-group" }, rows));
  }
  setTile("tile-agenda", e.status.state, head, groups);
}

function renderHealth(st) {
  const e = entryOf(st, "health");
  const el = $("health");
  const d = e && e.data;
  if (!d) {
    replaceChildren(el, h("span", { class: "label", text: "sites" }), h("span", { class: "empty", text: e ? emptyBody(e).textContent : "désactivé" }));
    return;
  }
  const sites = (d.sites || []).map((s) => {
    const parts = [h("span", { class: `dot ${s.up ? "ok" : "err"}` }), h("span", { class: "name", text: s.name })];
    if (s.up) parts.push(h("span", { class: "lat", text: `${s.latency_ms} ms` }));
    else parts.push(h("span", { class: "tls err", text: s.status ? `HTTP ${s.status}` : "injoignable" }));
    if (s.cert_days != null && s.cert_days < 14) parts.push(h("span", { class: `tls ${s.cert_days < 7 ? "err" : "warn"}`, text: `TLS ${s.cert_days} j` }));
    return h("span", { class: "site" }, parts);
  });
  replaceChildren(el, h("span", { class: "label", text: "sites" }), sites);
}

function renderFooter(st, now) {
  const left = $("foot-collectors");
  const names = Object.keys(st.collectors || {}).sort((a, b) => {
    const ia = COLLECTOR_ORDER.indexOf(a);
    const ib = COLLECTOR_ORDER.indexOf(b);
    return (ia < 0 ? 99 : ia) - (ib < 0 ? 99 : ib) || a.localeCompare(b);
  });
  replaceChildren(left, names.map((n) => {
    const s = st.collectors[n].status;
    return h("span", { class: "c", title: s.error || "" }, h("span", { class: `dot ${s.state === "ok" ? "ok" : s.state === "stale" ? "stale" : s.state === "error" ? "err" : "init"}` }), n);
  }));
  const right = [];
  if (st.demo) right.push(h("span", { class: "demo", text: "démo" }));
  right.push(h("span", { text: /^\d/.test(st.version || "") ? `v${st.version}` : st.version || "" }));
  right.push(h("span", { text: `maj ${fmtTime(now)}:${String(now.getSeconds()).padStart(2, "0")}` }));
  replaceChildren($("foot-right"), right);
}

/* ---------- events ---------- */

function renderEvents(st) {
  const events = st.events || [];
  const last = events.length ? events[events.length - 1].seq : 0;
  if (eventSeq == null) {
    eventSeq = last;
    return;
  }
  if (last === eventSeq) return;
  const fresh = events.filter((e) => e.seq > eventSeq).slice(-3);
  eventSeq = last;
  if (!fresh.length) return;
  const toast = $("toast");
  replaceChildren(toast, fresh.map((e) => h("div", { class: "line", text: e.label })));
  toast.hidden = false;
  if (toastTimer) clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { toast.hidden = true; }, TOAST_MS);
}

/* ---------- night, scale, data flow ---------- */

function parseHM(s) {
  const m = /^(\d{1,2}):(\d{2})$/.exec(s || "");
  return m ? Number(m[1]) * 60 + Number(m[2]) : null;
}

const nightOverride = new URLSearchParams(location.search).get("night");

function applyNight(st, now) {
  const n = st && st.ui && st.ui.night;
  let on = false;
  if (nightOverride != null) on = nightOverride === "1";
  else if (n && n.enabled) {
    const from = parseHM(n.from);
    const to = parseHM(n.to);
    const m = now.getHours() * 60 + now.getMinutes();
    if (from != null && to != null && from !== to) on = from < to ? m >= from && m < to : m >= from || m < to;
    document.documentElement.style.setProperty("--night-brightness", String(n.brightness || 0.35));
  }
  document.documentElement.toggleAttribute("data-night", on);
}

function fit() {
  const app = $("app");
  const auto = !current || !current.ui || current.ui.scale === "auto" || !current.ui.scale;
  const z = auto ? Math.min(window.innerWidth / APP_W, window.innerHeight / APP_H) : Number(current.ui.scale) || 1;
  const ox = Math.max(0, (window.innerWidth - APP_W * z) / 2);
  const oy = Math.max(0, (window.innerHeight - APP_H * z) / 2);
  app.style.transform = `translate(${ox}px, ${oy}px) scale(${z})`;
}

function render() {
  if (!current) return;
  const now = new Date();
  renderClock(now);
  renderWeather(entryOf(current, "weather"));
  renderMailBadge(entryOf(current, "mail"));
  renderQonto(current, now);
  renderStripe(current, now);
  renderStars(current, now);
  renderVisits(current, now);
  renderTraction(current, now);
  renderInbox(current, now);
  renderAgenda(current, now);
  renderHealth(current);
  renderFooter(current, now);
  applyNight(current, now);
}

function apply(st) {
  if (firstVersion == null) firstVersion = st.version;
  else if (st.version !== firstVersion) {
    location.reload();
    return;
  }
  current = st;
  $("splash").hidden = true;
  if (pollTimer) {
    clearInterval(pollTimer);
    pollTimer = null;
  }
  render();
  renderEvents(st);
}

async function poll() {
  try {
    const r = await fetch("/api/state", { cache: "no-store" });
    if (r.ok) apply(await r.json());
  } catch {
    if (!current) $("splash").hidden = false;
  }
}

function connect() {
  const es = new EventSource("/api/events");
  es.addEventListener("state", (ev) => apply(JSON.parse(ev.data)));
  es.onerror = () => {
    if (!pollTimer) pollTimer = setInterval(poll, 3000);
  };
}

$("brand-sub").textContent = location.hostname === "127.0.0.1" || location.hostname === "localhost" ? "pi-dash" : location.hostname;
renderClock(new Date());
fit();
window.addEventListener("resize", fit);
setInterval(() => renderClock(new Date()), 1000);
setInterval(() => {
  const now = new Date();
  applyNight(current, now);
  if (current) render();
}, 30000);
poll();
connect();
