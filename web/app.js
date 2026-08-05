"use strict";

// Same-origin by default. If the frontend is hosted separately (e.g. Vercel)
// pointing at a Railway backend, set window.API_BASE in a <script> before this.
const API = window.API_BASE || "";

const view = document.getElementById("view");
let currentTab = "feed";
let feedState = { source: "", q: "", unread: false };
let searchTimer = null;
// What the chat agent should treat as "the current page".
let currentContext = { kind: "feed", id: "", title: "" };

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
function sourceLabel(t) { return { arxiv: "arXiv", hackernews: "Hacker News", blog: "Blog", bluesky: "Bluesky" }[t] || t; }
function truncStr(s, n) { s = String(s || ""); return s.length > n ? s.slice(0, n) + "…" : s; }
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
  const chips = ["", "arxiv", "hackernews", "blog", "bluesky"];
  const chipLabels = { "": "All", arxiv: "arXiv", hackernews: "HN", blog: "Blogs", bluesky: "Bluesky" };
  const controls = saved ? "" : `<div class="controls">
    <div class="search"><input id="q" type="search" placeholder="Search your collection…" value="${esc(feedState.q)}" /></div>
    <div class="chips">${chips.map((c) =>
      `<button class="chip ${feedState.source === c ? "active" : ""}" data-src="${c}">${chipLabels[c]}</button>`).join("")}
      <button class="chip unread-chip ${feedState.unread ? "active" : ""}" id="unreadChip">Unread</button></div>
  </div>`;
  view.innerHTML = controls + `<div id="list"><div class="loading">Loading…</div></div>`;

  if (!saved) {
    const input = document.getElementById("q");
    input.addEventListener("input", () => {
      clearTimeout(searchTimer);
      searchTimer = setTimeout(() => { feedState.q = input.value.trim(); loadList(false); }, 320);
    });
    view.querySelectorAll(".chip[data-src]").forEach((c) =>
      c.addEventListener("click", () => { feedState.source = c.dataset.src; renderFeed(false); }));
    document.getElementById("unreadChip").addEventListener("click", () => {
      feedState.unread = !feedState.unread; renderFeed(false);
    });
  }
  // Awaited so the router restores scroll against the finished list rather than
  // the empty placeholder, where the page is too short and the scroll clamps to 0.
  await loadList(saved);
}

async function loadList(saved) {
  const list = document.getElementById("list");
  try {
    const params = new URLSearchParams();
    if (saved) params.set("saved", "1");
    else {
      if (feedState.source) params.set("source", feedState.source);
      if (feedState.q) params.set("q", feedState.q);
      if (feedState.unread) params.set("state", "unread");
    }
    params.set("limit", "60");
    const { items } = await api("/api/feed?" + params.toString());
    if (!items || !items.length) {
      list.innerHTML = `<div class="empty">${saved ? "Nothing saved yet." : "No items yet — tap ⟳ to fetch, or run <code>news all</code>."}</div>`;
      return;
    }
    list.innerHTML = items.map(itemCard).join("");
    list.querySelectorAll(".card").forEach((c) =>
      c.addEventListener("click", () => go("item", c.dataset.id)));
  } catch (e) {
    list.innerHTML = `<div class="empty">Error: ${esc(e.message)}</div>`;
  }
}

// ---------- item detail ----------

// convoDigest renders the rolled-up summary of the reader's chat about an item.
// Written by the background sweep once the thread has been idle a while, so it
// only appears after the conversation has settled.
function convoDigest(cv) {
  if (!cv || !cv.summary) return "";
  const when = relTime(cv.last_activity);
  return `<div class="section convo">
    <h4>Your conversation${when ? ` · ${when}` : ""}</h4>
    <p>${mdLite(cv.summary)}</p>
    <div class="convo-meta">${cv.turns} message${cv.turns === 1 ? "" : "s"}</div>
  </div>`;
}

function sectionList(title, arr, dim) {
  if (!arr || !arr.length) return "";
  return `<div class="section ${dim ? "dim" : ""}"><h4>${title}</h4><ul>${arr.map((x) => `<li>${esc(x)}</li>`).join("")}</ul></div>`;
}
async function openItem(id) {
  // Scroll is the router's job (it restores the list position on back).
  view.innerHTML = `<div class="loading">Loading…</div>`;
  let it;
  try { it = await api("/api/item/" + encodeURIComponent(id)); }
  catch (e) { view.innerHTML = `<div class="empty">Error: ${esc(e.message)}</div>`; return; }

  currentContext = { kind: "item", id: it.id, title: it.title };

  // Mark read on open (best-effort), then refresh the header counts — the DB
  // was always updated here, but nothing re-fetched stats, so the unread number
  // sat stale until a page reload and looked like it wasn't counting down.
  if (it.state === "unread") {
    post(`/api/item/${id}/state`, { state: "read" }).then(loadStats).catch(() => {});
  }

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
      ${convoDigest(it.conversation)}
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

  view.querySelector(".back").addEventListener("click", () => back(currentTab));
  const themeA = document.getElementById("themeLink");
  if (themeA) themeA.addEventListener("click", (e) => { e.preventDefault(); go("theme", it.theme_id); });

  view.querySelectorAll(".act").forEach((btn) => btn.addEventListener("click", async () => {
    const act = btn.dataset.act;
    try {
      if (act === "save") {
        // Un-saving returns to "read", not "unread": the item is open, so it has
        // been seen. Sending it back to "unread" pushed the unread count up.
        const now = btn.classList.contains("on") ? "read" : "saved";
        await post(`/api/item/${id}/state`, { state: now });
        btn.classList.toggle("on"); btn.classList.toggle("save");
        toast(now === "saved" ? "Saved" : "Removed");
        loadStats();
      } else if (act === "dismiss") {
        await post(`/api/item/${id}/state`, { state: "dismissed" });
        // Replace rather than push: a dismissed item shouldn't sit in history
        // waiting for the back button to resurrect it.
        toast("Dismissed"); go(currentTab, "", "", { replace: true }); loadStats();
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
        <button class="star ${t.starred ? "on" : ""}" data-star="${t.id}"
          title="${t.starred ? "Pinned — kept across re-clustering" : "Pin this theme"}"
          aria-label="${t.starred ? "Unpin theme" : "Pin theme"}">${t.starred ? "★" : "☆"}</button>
        <h3>${esc(t.title)}</h3>
        <p>${esc(t.description)}</p>
        <div class="count">${t.item_count} item${t.item_count === 1 ? "" : "s"}${t.starred ? " · pinned" : ""}</div>
      </div>`).join("");
    view.querySelectorAll(".theme-card").forEach((c) =>
      c.addEventListener("click", () => go("theme", c.dataset.id)));
    view.querySelectorAll("[data-star]").forEach((b) =>
      b.addEventListener("click", (e) => { e.stopPropagation(); toggleStar(b); }));
  } catch (e) { view.innerHTML = `<div class="empty">Error: ${esc(e.message)}</div>`; }
}
// toggleStar pins/unpins a theme. Pinned themes survive re-clustering instead of
// being deleted and rebuilt, so they keep tracking their topic over time.
async function toggleStar(btn) {
  const id = btn.dataset.star;
  const starred = !btn.classList.contains("on");
  try {
    await post(`/api/theme/${id}/star`, { starred });
    btn.classList.toggle("on", starred);
    btn.textContent = starred ? "★" : "☆";
    toast(starred ? "Pinned — kept across re-clustering" : "Unpinned");
  } catch (e) { toast("Error: " + e.message); }
}

async function openTheme(id) {
  view.innerHTML = `<div class="loading">Loading…</div>`;
  try {
    const t = await api("/api/theme/" + id);
    currentContext = { kind: "theme", id: String(t.id), title: t.title };
    const synth = `<div class="synth">
      ${t.connections ? `<h4>Connections</h4><p>${esc(t.connections)}</p>` : ""}
      ${t.disagreements ? `<h4>Disagreements</h4><p>${esc(t.disagreements)}</p>` : ""}
      ${t.trajectory ? `<h4>Where it's heading</h4><p>${esc(t.trajectory)}</p>` : ""}
    </div>`;
    view.innerHTML = `<button class="back">‹ Themes</button>
      <div class="detail">
        <h1>${esc(t.title)}</h1>
        <div class="section"><p>${esc(t.description)}</p></div>
        ${convoDigest(t.conversation)}
        ${synth}
        <div class="section"><h4>${t.item_count} items</h4></div>
        <div id="list">${(t.items || []).map(itemCard).join("")}</div>
      </div>`;
    view.querySelector(".back").addEventListener("click", () => back("themes"));
    view.querySelectorAll(".card").forEach((c) => c.addEventListener("click", () => openItem(c.dataset.id)));
  } catch (e) { view.innerHTML = `<div class="empty">Error: ${esc(e.message)}</div>`; }
}

// ---------- chat (FAB, Managed Agents) ----------
let chat = { id: null, messages: [], ctxKey: null, ctx: null };
function ctxKeyOf(c) { return c.kind + ":" + c.id; }

function openChat() {
  const ctx = { ...currentContext };
  // Start a fresh conversation when the topic changes.
  if (chat.ctxKey !== ctxKeyOf(ctx)) chat = { id: null, messages: [], ctxKey: ctxKeyOf(ctx), ctx };
  chat.ctx = ctx;
  const label = (ctx.kind === "item" || ctx.kind === "theme") ? "· " + truncStr(ctx.title, 42) : "· your feed";
  document.getElementById("chatContext").textContent = label;
  renderChat();
  openOverlay("chatOverlay");
  setTimeout(() => document.getElementById("chatInput").focus(), 250);
}
function renderChat() {
  const box = document.getElementById("chatMessages");
  if (!chat.messages.length) {
    const hint = (chat.ctx && (chat.ctx.kind === "item" || chat.ctx.kind === "theme"))
      ? `Ask anything about <b>${esc(truncStr(chat.ctx.title, 60))}</b> — I can read it, find related work in your collection, and search the web.`
      : `Ask across your collection — I can search your items, pull related work, and search the web.`;
    box.innerHTML = `<div class="chat-empty">${hint}</div>`;
    return;
  }
  box.innerHTML = chat.messages.map((m) => {
    if (m.role === "user") return `<div class="msg user">${esc(m.text)}</div>`;
    if (m.role === "thinking") {
      const label = m.tool ? `using <b>${esc(m.tool)}</b>…` : "thinking…";
      // Blocks are keyed by server event id: live deltas append to a block, and
      // the buffered message for that id replaces it (deltas are best-effort and
      // can be a truncated prefix, so the buffered text is authoritative).
      const blocks = m.order.map((id) =>
        `<div class="msg bot${m.final[id] ? "" : " partial"}">${mdLite(m.blocks[id])}</div>`).join("");
      return `${blocks}<div class="msg bot thinking"><span class="pulse"></span> ${label}</div>`;
    }
    return `<div class="msg bot">${mdLite(m.text)}</div>`;
  }).join("");
  box.scrollTop = box.scrollHeight;
}

// Read an SSE stream, invoking onEvent(name, data) per frame. Used instead of
// EventSource because the request needs to POST a body.
async function streamSSE(url, payload, onEvent) {
  const res = await fetch(url, {
    method: "POST",
    headers: { "content-type": "application/json", accept: "text/event-stream" },
    body: JSON.stringify(payload),
  });
  if (!res.ok) throw new Error(await res.text() || res.statusText);
  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buf = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    // Frames are separated by a blank line; keep any partial tail buffered.
    const frames = buf.split("\n\n");
    buf = frames.pop();
    for (const frame of frames) {
      let name = "message", data = "";
      for (const line of frame.split("\n")) {
        if (line.startsWith("event:")) name = line.slice(6).trim();
        else if (line.startsWith("data:")) data += line.slice(5).trim();
      }
      if (data) onEvent(name, JSON.parse(data));
    }
  }
}

async function sendChat() {
  const input = document.getElementById("chatInput");
  const q = input.value.trim();
  if (!q) return;
  input.value = ""; input.style.height = "auto";
  chat.messages.push({ role: "user", text: q });
  // blocks/final/order track per-event-id text so a live delta preview can be
  // replaced by the authoritative buffered message without duplicating it.
  const pending = { role: "thinking", tool: "", blocks: {}, final: {}, order: [] };
  const touch = (id) => { if (!(id in pending.blocks)) { pending.blocks[id] = ""; pending.order.push(id); } };
  const joined = () => pending.order.map((id) => pending.blocks[id]).join("\n\n").trim();
  chat.messages.push(pending);
  renderChat();
  document.getElementById("chatSend").disabled = true;
  try {
    await streamSSE("/api/chat", { session_id: chat.id, message: q, context: chat.ctx }, (name, d) => {
      if (name === "progress" && d.type === "text") {
        touch(d.id); pending.blocks[d.id] += d.text; pending.tool = "";
      } else if (name === "progress" && d.type === "message") {
        touch(d.id); pending.blocks[d.id] = d.text; pending.final[d.id] = true; pending.tool = "";
      } else if (name === "progress" && d.type === "tool") pending.tool = d.tool;
      else if (name === "progress" && d.type === "session") chat.id = d.id;
      else if (name === "done") {
        chat.id = d.session_id || chat.id;
        const i = chat.messages.indexOf(pending);
        if (i >= 0) chat.messages.splice(i, 1, { role: "bot", text: d.text || joined() || "(no reply)" });
      } else if (name === "error") {
        chat.messages.push({ role: "bot", text: "Error: " + d.error });
      } else return;
      renderChat();
    });
    // Stream ended without a terminal frame — keep whatever text arrived.
    const i = chat.messages.indexOf(pending);
    if (i >= 0) chat.messages.splice(i, 1, { role: "bot", text: joined() || "(no reply)" });
  } catch (e) {
    const i = chat.messages.indexOf(pending);
    if (i >= 0) chat.messages.splice(i, 1);
    chat.messages.push({ role: "bot", text: "Error: " + e.message });
  } finally {
    document.getElementById("chatSend").disabled = false;
    renderChat();
  }
}

// ---------- settings (sources) ----------
let sources = { bluesky_handles: [], extra_feeds: [] };
async function openSettings() {
  openOverlay("settingsOverlay");
  const body = document.getElementById("settingsBody");
  body.innerHTML = `<div class="chat-empty">Loading…</div>`;
  try {
    sources = await api("/api/sources");
    sources.bluesky_handles = sources.bluesky_handles || [];
    sources.extra_feeds = sources.extra_feeds || [];
    renderSettings();
  } catch (e) { body.innerHTML = `<div class="chat-empty">Error: ${esc(e.message)}</div>`; }
}
function renderSettings() {
  const bsky = sources.bluesky_handles.map((h, i) =>
    `<span class="editchip">@${esc(h)}<button data-rm-bsky="${i}">✕</button></span>`).join("");
  const feeds = sources.extra_feeds.map((f, i) =>
    `<span class="editchip">${esc(f.name || f.url)}<button data-rm-feed="${i}">✕</button></span>`).join("");
  document.getElementById("settingsBody").innerHTML = `
    <h4>Add an article</h4>
    <p class="hint">Paste any paper or post URL. It's fetched, summarized, scored, and filed under the closest theme. You can also just ask the chat agent to add a URL.</p>
    <div class="add-row"><input id="articleInput" placeholder="https://arxiv.org/abs/…" /><button id="articleAdd">Add</button></div>
    <div class="hint" id="articleResult"></div>

    <h4>Bluesky accounts</h4>
    <p class="hint">Follow people's posts. Enter a handle like <code>simonwillison.net</code> (or paste a profile URL).</p>
    <div class="chip-row">${bsky || '<span class="hint">None yet.</span>'}</div>
    <div class="add-row"><input id="bskyInput" placeholder="handle.bsky.social" /><button id="bskyAdd">Add</button></div>

    <h4>Extra RSS / Atom feeds</h4>
    <p class="hint">Add any blog feed on top of the built-in list.</p>
    <div class="chip-row">${feeds || '<span class="hint">None yet.</span>'}</div>
    <div class="add-row"><input id="feedInput" placeholder="https://blog.example.com/feed.xml" /><button id="feedAdd">Add</button></div>

    <button class="settings-save" id="sourcesSave">Save sources</button>
    <p class="hint" style="margin-top:10px">After saving, tap ⟳ to fetch from the new sources.</p>`;

  const b = document.getElementById("settingsBody");
  b.querySelectorAll("[data-rm-bsky]").forEach((x) => x.addEventListener("click", () => {
    sources.bluesky_handles.splice(+x.dataset.rmBsky, 1); renderSettings();
  }));
  b.querySelectorAll("[data-rm-feed]").forEach((x) => x.addEventListener("click", () => {
    sources.extra_feeds.splice(+x.dataset.rmFeed, 1); renderSettings();
  }));
  const addBsky = () => {
    const v = document.getElementById("bskyInput").value.trim();
    if (v) { sources.bluesky_handles.push(v.replace(/^@/, "")); renderSettings(); }
  };
  const addFeed = () => {
    const v = document.getElementById("feedInput").value.trim();
    if (v) { sources.extra_feeds.push({ name: "", url: v }); renderSettings(); }
  };
  // Adding an article is immediate (fetch + enrich), unlike the source lists
  // which only take effect on save, so it has its own inline result line.
  const addArticle = async () => {
    const input = document.getElementById("articleInput");
    const out = document.getElementById("articleResult");
    const btn = document.getElementById("articleAdd");
    const url = input.value.trim();
    if (!url) return;
    btn.disabled = true;
    out.textContent = "Fetching and summarizing…";
    try {
      const r = await post("/api/articles", { url });
      out.textContent = r.result || "Done.";
      input.value = "";
      loadStats();
    } catch (e) { out.textContent = "Error: " + e.message; }
    finally { btn.disabled = false; }
  };
  document.getElementById("articleAdd").addEventListener("click", addArticle);
  document.getElementById("articleInput").addEventListener("keydown", (e) => { if (e.key === "Enter") addArticle(); });
  document.getElementById("bskyAdd").addEventListener("click", addBsky);
  document.getElementById("feedAdd").addEventListener("click", addFeed);
  document.getElementById("bskyInput").addEventListener("keydown", (e) => { if (e.key === "Enter") addBsky(); });
  document.getElementById("feedInput").addEventListener("keydown", (e) => { if (e.key === "Enter") addFeed(); });
  document.getElementById("sourcesSave").addEventListener("click", async () => {
    try { await post("/api/sources", sources); toast("Sources saved"); closeSheet(); }
    catch (e) { toast("Error: " + e.message); }
  });
}

function openOverlay(id) { document.getElementById(id).classList.add("open"); }
function closeOverlay(id) { document.getElementById(id).classList.remove("open"); }

// ---------- routing ----------
// Hash-based, so every view is deep-linkable and survives a reload. Paths would
// need a server-side SPA fallback; the Go binary serves the embedded files
// directly, so /item/<id> would 404.
//
// Sheets get their own history entry (#/item/x/chat), which makes the back
// gesture close the sheet instead of leaving the page — what a phone user
// expects.
const OVERLAY_ROUTES = { chat: "chatOverlay", settings: "settingsOverlay" };
const LIST_VIEWS = ["feed", "saved", "themes"];
const DETAIL_VIEWS = ["item", "theme"]; // views whose second segment is an id

// Starts as a sentinel, not "feed": the first applyRoute() must register as a
// change so the initial view actually renders. Defaulting this to feed made a
// plain load of "/" (already #/feed) a no-op and left the page blank.
let route = { view: "", id: "", overlay: "" };
let renderToken = 0;   // guards against a slow render landing after a faster nav
let navDepth = 0;      // how many in-app entries we've pushed, for safe back()
const scrollPos = new Map();

function parseHash(hash) {
  const parts = String(hash || "").replace(/^#\/?/, "").split("/").filter(Boolean);
  let overlay = "";
  const last = parts[parts.length - 1];
  if (OVERLAY_ROUTES[last]) {
    // Only treat the last segment as a sheet if what remains is itself a
    // complete route. Otherwise "#/item/chat" (an item whose id happens to be
    // "chat") would parse as the chat sheet with no item.
    const rest = parts.slice(0, -1);
    const complete = rest.length === 0 ||
      (DETAIL_VIEWS.includes(rest[0]) ? rest.length === 2 : rest.length === 1);
    if (complete) { overlay = last; parts.pop(); }
  }
  return {
    view: parts[0] || "feed",
    id: parts[1] ? decodeURIComponent(parts[1]) : "",
    overlay,
  };
}

function hashFor(view, id, overlay) {
  let h = "#/" + view;
  if (id) h += "/" + encodeURIComponent(id);
  if (overlay) h += "/" + overlay;
  return h;
}

// baseKey ignores the overlay so opening a sheet doesn't lose the list's scroll.
const baseKey = (r) => r.view + "/" + r.id;

function go(view, id, overlay, opts) {
  const next = hashFor(view, id, overlay || "");
  if (location.hash === next) return;
  scrollPos.set(baseKey(route), window.scrollY);
  if (opts && opts.replace) {
    history.replaceState(null, "", next); // does not fire hashchange
    applyRoute();
    return;
  }
  navDepth++;
  location.hash = next; // fires hashchange -> applyRoute
}

// back returns to the previous in-app view, or falls back to a list when this
// entry was a deep link (where history.back() would leave the app entirely).
function back(fallbackView) {
  if (navDepth > 0) { navDepth--; history.back(); return; }
  go(fallbackView || "feed", "", "", { replace: true });
}

async function applyRoute() {
  const prev = route;
  const next = parseHash(location.hash);
  route = next;

  const baseChanged = prev.view !== next.view || prev.id !== next.id;
  if (baseChanged) {
    if (LIST_VIEWS.includes(next.view)) {
      currentTab = next.view;
      // openItem/openTheme set this; leaving it stale would point the chat
      // agent at an article the reader has already navigated away from.
      currentContext = { kind: "feed", id: "", title: "" };
    } else if (next.view === "theme") currentTab = "themes";
    highlightTab();

    const token = ++renderToken;
    await renderRoute(next);
    if (token !== renderToken) return; // a newer navigation already won
    // One frame after paint, so the restored offset lands against real layout.
    const y = scrollPos.get(baseKey(next)) || 0;
    requestAnimationFrame(() => { if (token === renderToken) window.scrollTo(0, y); });
  }
  // Sheets sync after the base view renders, so a deep link like
  // #/item/x/chat opens the chat with the item's context already set.
  syncOverlays(next.overlay);
}

async function renderRoute(r) {
  switch (r.view) {
    case "saved":  return renderFeed(true);
    case "themes": return renderThemes();
    case "item":   return openItem(r.id);
    case "theme":  return openTheme(parseInt(r.id, 10));
    default:       return renderFeed(false);
  }
}

function highlightTab() {
  document.querySelectorAll(".tab").forEach((t) =>
    t.classList.toggle("active", t.dataset.tab === currentTab));
}

function syncOverlays(name) {
  for (const [key, id] of Object.entries(OVERLAY_ROUTES)) {
    const el = document.getElementById(id);
    const open = el.classList.contains("open");
    if (key === name && !open) {
      if (key === "chat") openChat(); else openSettings();
    } else if (key !== name && open) {
      closeOverlay(id);
    }
  }
}

// Sheet controls navigate rather than toggling classes, so history stays in step.
const openSheet  = (name) => go(route.view, route.id, name);
const closeSheet = () => (route.overlay ? back(currentTab) : syncOverlays(""));

window.addEventListener("hashchange", applyRoute);

document.querySelectorAll(".tab").forEach((t) =>
  t.addEventListener("click", () => go(t.dataset.tab)));

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

// FAB + chat wiring
document.getElementById("fab").addEventListener("click", () => openSheet("chat"));
document.getElementById("chatClose").addEventListener("click", closeSheet);
document.getElementById("chatSend").addEventListener("click", sendChat);
const chatInput = document.getElementById("chatInput");
chatInput.addEventListener("input", () => { chatInput.style.height = "auto"; chatInput.style.height = Math.min(chatInput.scrollHeight, 120) + "px"; });
chatInput.addEventListener("keydown", (e) => { if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); sendChat(); } });

// Settings wiring
document.getElementById("settingsBtn").addEventListener("click", () => openSheet("settings"));
document.getElementById("settingsClose").addEventListener("click", closeSheet);

// Close sheets on backdrop tap
["chatOverlay", "settingsOverlay"].forEach((id) => {
  document.getElementById(id).addEventListener("click", (e) => { if (e.target.id === id) closeSheet(); });
});

// Escape closes the top sheet, matching the back gesture.
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && route.overlay) closeSheet();
});

// init — render whatever the URL asks for, so a reload or a shared link lands
// on the same view. Normalize a bare "/" to #/feed so the first entry is a real
// route and back behaves predictably from the start.
loadStats();
if (!location.hash) history.replaceState(null, "", hashFor("feed", "", ""));
applyRoute();
