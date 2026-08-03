"use strict";

// Same-origin by default. If the frontend is hosted separately (e.g. Vercel)
// pointing at a Railway backend, set window.API_BASE in a <script> before this.
const API = window.API_BASE || "";

const view = document.getElementById("view");
let currentTab = "feed";
let feedState = { source: "", q: "" };
let searchTimer = null;

// ---------- api helpers ----------
async function api(path, opts) {
  const r = await fetch(API + path, opts);
  if (!r.ok) throw new Error((await r.text()) || r.statusText);
  return r.json();
}
const post = (path, body) =>
  api(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body || {}) });

// ---------- utils ----------
function esc(s) {
  return String(s == null ? "" : s).replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}
function scoreClass(n) { return n >= 65 ? "hi" : n >= 45 ? "mid" : "lo"; }
function relTime(iso) {
  if (!iso) return "";
  const d = new Date(iso);
  if (isNaN(d) || d.getFullYear() < 1971) return "";
  const s = (Date.now() - d.getTime()) / 1000;
  if (s < 3600) return Math.max(1, Math.floor(s / 60)) + "m";
  if (s < 86400) return Math.floor(s / 3600) + "h";
  if (s < 86400 * 30) return Math.floor(s / 86400) + "d";
  return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
}
function sourceLabel(t) { return { arxiv: "arXiv", hackernews: "Hacker News", blog: "Blog" }[t] || t; }
function toast(msg) {
  let el = document.querySelector(".toast");
  if (!el) { el = document.createElement("div"); el.className = "toast"; document.body.appendChild(el); }
  el.textContent = msg; el.classList.add("show");
  clearTimeout(el._t); el._t = setTimeout(() => el.classList.remove("show"), 1600);
}
function mdLite(t) {
  const paras = esc(t).split(/\n\s*\n/);
  return paras.map((p) => {
    p = p
      .replace(/\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)/g, '<a href="$2" target="_blank" rel="noopener">$1</a>')
      .replace(/(^|[\s(])(https?:\/\/[^\s)]+)/g, '$1<a href="$2" target="_blank" rel="noopener">$2</a>')
      .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
      .replace(/`([^`]+)`/g, "<code>$1</code>")
      .replace(/\n/g, "<br>");
    return "<p>" + p + "</p>";
  }).join("");
}

// ---------- stats ----------
async function loadStats() {
  try {
    const s = await api("/api/stats");
    document.getElementById("statsBar").innerHTML =
      `<span><b>${s.enriched}</b> analyzed</span>` +
      `<span><b>${s.unread}</b> unread</span>` +
      `<span><b>${s.saved}</b> saved</span>` +
      `<span><b>${s.themes}</b> themes</span>`;
  } catch (_) {}
}

// ---------- item card ----------
function itemCard(it) {
  const badge = it.enriched
    ? `<span class="score ${scoreClass(it.score)}">${Math.round(it.score)}</span>`
    : `<span class="score lo">·</span>`;
  const stateBadge =
    it.state === "saved" ? `<span class="state-badge saved">★ saved</span>` :
    it.state === "read" ? `<span class="state-badge read">read</span>` : "";
  const tags = (it.tags || []).slice(0, 4).map((t) => `<span class="tag">${esc(t)}</span>`).join("");
  return `<div class="card" data-id="${esc(it.id)}">
    <div class="meta">${badge}
      <span class="src">${esc(sourceLabel(it.source_type))}</span>
      <span class="dot"></span><span>${esc(it.source)}</span>
      ${relTime(it.published_at) ? `<span class="dot"></span><span>${relTime(it.published_at)}</span>` : ""}
      ${stateBadge}
    </div>
    <h3>${esc(it.title)}</h3>
    ${it.one_liner ? `<p class="one">${esc(it.one_liner)}</p>` : ""}
    ${tags ? `<div class="tags">${tags}</div>` : ""}
  </div>`;
}

// ---------- feed view ----------
async function renderFeed(saved) {
  const chips = ["", "arxiv", "hackernews", "blog"];
  const chipLabels = { "": "All", arxiv: "arXiv", hackernews: "HN", blog: "Blogs" };
  const controls = saved ? "" : `<div class="controls">
    <div class="search"><input id="q" type="search" placeholder="Search your collection…" value="${esc(feedState.q)}" /></div>
    <div class="chips">${chips.map((c) =>
      `<button class="chip ${feedState.source === c ? "active" : ""}" data-src="${c}">${chipLabels[c]}</button>`).join("")}</div>
  </div>`;
  view.innerHTML = controls + `<div id="list"><div class="loading">Loading…</div></div>`;

  if (!saved) {
    const input = document.getElementById("q");
    input.addEventListener("input", () => {
      clearTimeout(searchTimer);
      searchTimer = setTimeout(() => { feedState.q = input.value.trim(); loadList(false); }, 320);
    });
    view.querySelectorAll(".chip").forEach((c) =>
      c.addEventListener("click", () => { feedState.source = c.dataset.src; renderFeed(false); }));
  }
  loadList(saved);
}

async function loadList(saved) {
  const list = document.getElementById("list");
  try {
    const params = new URLSearchParams();
    if (saved) params.set("saved", "1");
    else {
      if (feedState.source) params.set("source", feedState.source);
      if (feedState.q) params.set("q", feedState.q);
    }
    params.set("limit", "60");
    const { items } = await api("/api/feed?" + params.toString());
    if (!items || !items.length) {
      list.innerHTML = `<div class="empty">${saved ? "Nothing saved yet." : "No items yet — tap ⟳ to fetch, or run <code>news all</code>."}</div>`;
      return;
    }
    list.innerHTML = items.map(itemCard).join("");
    list.querySelectorAll(".card").forEach((c) =>
      c.addEventListener("click", () => openItem(c.dataset.id)));
  } catch (e) {
    list.innerHTML = `<div class="empty">Error: ${esc(e.message)}</div>`;
  }
}

// ---------- item detail ----------
function sectionList(title, arr, dim) {
  if (!arr || !arr.length) return "";
  return `<div class="section ${dim ? "dim" : ""}"><h4>${title}</h4><ul>${arr.map((x) => `<li>${esc(x)}</li>`).join("")}</ul></div>`;
}
async function openItem(id) {
  view.innerHTML = `<div class="loading">Loading…</div>`;
  window.scrollTo(0, 0);
  let it;
  try { it = await api("/api/item/" + encodeURIComponent(id)); }
  catch (e) { view.innerHTML = `<div class="empty">Error: ${esc(e.message)}</div>`; return; }

  // Mark read on open (best-effort).
  if (it.state === "unread") { post(`/api/item/${id}/state`, { state: "read" }).catch(() => {}); }

  const scores = it.enriched
    ? `<span class="score ${scoreClass(it.score)}">${Math.round(it.score)}</span>
       <span>rel ${it.relevance} · nov ${it.novelty} · imp ${it.importance}</span>` : "";
  const theme = it.theme_id
    ? `<a href="#" id="themeLink">🕸 ${esc(it.theme_title || "theme")}</a>` : "";

  view.innerHTML = `<button class="back">‹ Back</button>
    <div class="detail">
      <div class="meta">${scores}</div>
      <h1>${esc(it.title)}</h1>
      <div class="meta">
        <span class="src">${esc(it.source)}</span>
        ${it.authors && it.authors.length ? `<span class="dot"></span><span>${esc(it.authors.slice(0, 4).join(", "))}${it.authors.length > 4 ? " et al." : ""}</span>` : ""}
        ${relTime(it.published_at) ? `<span class="dot"></span><span>${relTime(it.published_at)}</span>` : ""}
        ${theme}
      </div>
      <a class="linkout" href="${esc(it.url)}" target="_blank" rel="noopener">Open source ↗</a>
      ${it.summary ? `<div class="section"><h4>Summary</h4><p>${esc(it.summary)}</p></div>` : ""}
      ${(it.tags && it.tags.length) ? `<div class="tags">${it.tags.map((t) => `<span class="tag">${esc(t)}</span>`).join("")}</div>` : ""}
      ${sectionList("Key ideas", it.key_ideas)}
      ${sectionList("Claims", it.claims)}
      ${sectionList("Evidence", it.evidence, true)}
      ${sectionList("Limitations", it.limitations, true)}
      ${sectionList("Open questions", it.open_questions, true)}
      <div class="actions">
        <button class="act save ${it.state === "saved" ? "on save" : ""}" data-act="save">★ Save</button>
        <button class="act up ${it.feedback === 1 ? "on up" : ""}" data-act="up">▲</button>
        <button class="act down ${it.feedback === -1 ? "on down" : ""}" data-act="down">▼</button>
        <button class="act" data-act="dismiss">Dismiss</button>
      </div>
    </div>`;

  view.querySelector(".back").addEventListener("click", () => switchTab(currentTab, true));
  const themeA = document.getElementById("themeLink");
  if (themeA) themeA.addEventListener("click", (e) => { e.preventDefault(); openTheme(it.theme_id); });

  view.querySelectorAll(".act").forEach((btn) => btn.addEventListener("click", async () => {
    const act = btn.dataset.act;
    try {
      if (act === "save") {
        const now = btn.classList.contains("on") ? "unread" : "saved";
        await post(`/api/item/${id}/state`, { state: now });
        btn.classList.toggle("on"); btn.classList.toggle("save");
        toast(now === "saved" ? "Saved" : "Removed");
      } else if (act === "dismiss") {
        await post(`/api/item/${id}/state`, { state: "dismissed" });
        toast("Dismissed"); switchTab(currentTab, true);
      } else if (act === "up" || act === "down") {
        const val = act === "up" ? 1 : -1;
        const on = btn.classList.contains("on");
        await post(`/api/item/${id}/feedback`, { feedback: on ? 0 : val });
        view.querySelectorAll('[data-act="up"],[data-act="down"]').forEach((b) => b.className = "act " + b.dataset.act);
        if (!on) btn.classList.add("on", act);
        toast("Thanks — noted");
      }
    } catch (e) { toast("Error: " + e.message); }
  }));
}

// ---------- themes ----------
async function renderThemes() {
  view.innerHTML = `<div class="loading">Loading themes…</div>`;
  try {
    const { themes } = await api("/api/themes");
    if (!themes || !themes.length) {
      view.innerHTML = `<div class="empty">No themes yet. They're built after enrichment — run <code>news themes</code> or ⟳.</div>`;
      return;
    }
    view.innerHTML = themes.map((t) =>
      `<div class="theme-card" data-id="${t.id}">
        <h3>${esc(t.title)}</h3>
        <p>${esc(t.description)}</p>
        <div class="count">${t.item_count} item${t.item_count === 1 ? "" : "s"}</div>
      </div>`).join("");
    view.querySelectorAll(".theme-card").forEach((c) =>
      c.addEventListener("click", () => openTheme(parseInt(c.dataset.id, 10))));
  } catch (e) { view.innerHTML = `<div class="empty">Error: ${esc(e.message)}</div>`; }
}
async function openTheme(id) {
  view.innerHTML = `<div class="loading">Loading…</div>`;
  window.scrollTo(0, 0);
  try {
    const t = await api("/api/theme/" + id);
    const synth = `<div class="synth">
      ${t.connections ? `<h4>Connections</h4><p>${esc(t.connections)}</p>` : ""}
      ${t.disagreements ? `<h4>Disagreements</h4><p>${esc(t.disagreements)}</p>` : ""}
      ${t.trajectory ? `<h4>Where it's heading</h4><p>${esc(t.trajectory)}</p>` : ""}
    </div>`;
    view.innerHTML = `<button class="back">‹ Themes</button>
      <div class="detail">
        <h1>${esc(t.title)}</h1>
        <div class="section"><p>${esc(t.description)}</p></div>
        ${synth}
        <div class="section"><h4>${t.item_count} items</h4></div>
        <div id="list">${(t.items || []).map(itemCard).join("")}</div>
      </div>`;
    view.querySelector(".back").addEventListener("click", () => switchTab("themes", true));
    view.querySelectorAll(".card").forEach((c) => c.addEventListener("click", () => openItem(c.dataset.id)));
  } catch (e) { view.innerHTML = `<div class="empty">Error: ${esc(e.message)}</div>`; }
}

// ---------- ask ----------
const askExamples = [
  "What are the latest approaches to inference-time compute?",
  "Summarize recent work on coding agents and where it's heading.",
  "What do my sources disagree about regarding agent memory?",
];
let lastAsk = "";
function renderAsk() {
  view.innerHTML = `<div class="ask-box">
      <textarea id="askInput" placeholder="Ask across your collection — the agent searches your items, then reads the actual sources on the web to answer.">${esc(lastAsk)}</textarea>
      <button id="askBtn" class="ask-btn">✦ Ask the research agent</button>
      <div class="ask-hint">Uses web search + fetch to follow up on real sources. Can take up to a minute.</div>
      <div class="examples">${askExamples.map((e) => `<button class="example">${esc(e)}</button>`).join("")}</div>
    </div>
    <div id="answer"></div>`;
  const input = document.getElementById("askInput");
  const btn = document.getElementById("askBtn");
  view.querySelectorAll(".example").forEach((e) =>
    e.addEventListener("click", () => { input.value = e.textContent; input.focus(); }));
  btn.addEventListener("click", () => runAsk(input.value.trim()));
  input.addEventListener("keydown", (ev) => {
    if ((ev.metaKey || ev.ctrlKey) && ev.key === "Enter") runAsk(input.value.trim());
  });
}
async function runAsk(q) {
  if (!q) return;
  lastAsk = q;
  const ansEl = document.getElementById("answer");
  const btn = document.getElementById("askBtn");
  btn.disabled = true;
  ansEl.innerHTML = `<div class="thinking"><span class="pulse"></span> Researching — searching your collection and the web…</div>`;
  try {
    const a = await post("/api/ask", { question: q });
    const cites = (a.citations || []).length
      ? `<div class="cites"><h4>Sources from your collection</h4>${a.citations.map((c) =>
          `<a href="${esc(c.url)}" target="_blank" rel="noopener">${esc(c.title)} — ${esc(c.source)}</a>`).join("")}</div>` : "";
    ansEl.innerHTML = `<div class="answer">${mdLite(a.text)}${cites}
      <div class="ask-hint" style="margin-top:12px">${a.turns} research step${a.turns === 1 ? "" : "s"}${a.used_web ? " · web enabled" : " · corpus only"}</div>
    </div>`;
  } catch (e) {
    ansEl.innerHTML = `<div class="answer">Error: ${esc(e.message)}</div>`;
  } finally { btn.disabled = false; }
}

// ---------- tabs / routing ----------
function switchTab(tab, keepState) {
  currentTab = tab;
  document.querySelectorAll(".tab").forEach((t) => t.classList.toggle("active", t.dataset.tab === tab));
  window.scrollTo(0, 0);
  if (tab === "feed") renderFeed(false);
  else if (tab === "saved") renderFeed(true);
  else if (tab === "themes") renderThemes();
  else if (tab === "ask") renderAsk();
}
document.querySelectorAll(".tab").forEach((t) =>
  t.addEventListener("click", () => switchTab(t.dataset.tab)));

// refresh
document.getElementById("refreshBtn").addEventListener("click", async (e) => {
  const btn = e.currentTarget;
  btn.classList.add("spin");
  try {
    const r = await post("/api/refresh");
    toast(r.status === "started" ? "Fetching new items…" : "Refresh already running");
  } catch (err) { toast("Error: " + err.message); }
  setTimeout(() => { btn.classList.remove("spin"); loadStats(); }, 4000);
});

// init
loadStats();
switchTab("feed");
