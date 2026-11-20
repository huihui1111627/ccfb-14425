/* Aquarium life-support continuous profiler UI.
 * Vanilla JS + inline SVG. Flame frames are laid out by the tree's own
 * weights; siblings arrive already sorted by function name from the server.
 */
"use strict";

const $ = (id) => document.getElementById(id);
const state = { tab: "flame", programs: [], zoomNode: null };

// ---------- small utilities ----------
function pct(x, total) {
  if (!total) return "0.00%";
  return (100 * x / total).toFixed(2) + "%";
}
function esc(s) {
  return String(s).replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}
function parseLabels() {
  const raw = $("labels").value.trim();
  if (!raw) return {};
  const out = {};
  for (const part of raw.split(",")) {
    const i = part.indexOf("=");
    if (i < 0) continue;
    out[part.slice(0, i).trim()] = part.slice(i + 1).trim();
  }
  return out;
}
function queryString(extra) {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(extra)) p.set(k, v);
  const s = $("startNs").value;
  const e = $("endNs").value;
  if (s) p.set("start_ns", s);
  if (e) p.set("end_ns", e);
  for (const [k, v] of Object.entries(parseLabels())) p.set("label." + k, v);
  return p.toString();
}
async function getJSON(url) {
  const res = await fetch(url);
  if (!res.ok) {
    const msg = await res.text();
    throw new Error("HTTP " + res.status + ": " + msg);
  }
  return res.json();
}
function postJSON(url, body) {
  return fetch(url, {
    method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  }).then((r) => { if (!r.ok) throw new Error(r.statusText); return r.json(); });
}

const tooltip = $("tooltip");
function showTip(evt, html) {
  tooltip.innerHTML = html;
  tooltip.style.display = "block";
  tooltip.style.left = Math.min(evt.clientX + 14, innerWidth - 400) + "px";
  tooltip.style.top = (evt.clientY + 14) + "px";
}
function hideTip() { tooltip.style.display = "none"; }

// deterministic hue per frame name so the same function keeps the same color
function frameColor(moduleName, funcName) {
  if (funcName === "[folded]") return "#3a4654";
  if (funcName === "[gap]") return "#7a5c1e";
  if (moduleName === "[unknown]") return "#5a2a3a";
  let h = 0;
  const s = moduleName + "!" + funcName;
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) >>> 0;
  const hue = h % 360;
  return `hsl(${hue}, 45%, 55%)`;
}
function diffColor(deltaNorm, oldCount, newCount) {
  if (deltaNorm === 0) return "#3f5163";
  const intensity = Math.min(1, Math.abs(deltaNorm) / 0.05);
  const alpha = 0.35 + 0.55 * intensity;
  if (deltaNorm > 0) return `rgba(255,122,89,${alpha.toFixed(2)})`;
  return `rgba(76,208,122,${alpha.toFixed(2)})`;
}

// ---------- flame graph rendering ----------
const ROW_H = 20, CHAR_W = 6.6, PAD = 4, MIN_W = 0.5;

function treeDepth(n) {
  let d = 1;
  for (const c of n.children || []) d = Math.max(d, 1 + treeDepth(c));
  return d;
}

function renderFlame(root, totalWeight, mount) {
  const zoomRoot = state.zoomNode || root;
  const zoomTotal = zoomRoot.total_ns || totalWeight;
  const depth = treeDepth(zoomRoot);
  const width = Math.max(900, mount.clientWidth - 20);
  const height = depth * ROW_H + 2 * PAD;
  let svg = `<svg width="${width}" height="${height}" viewBox="0 0 ${width} ${height}">`;

  function draw(node, x, w, level) {
    if (w < MIN_W) return;
    const y = height - PAD - (level + 1) * ROW_H;
    const name = node.func || "(root)";
    const full = node.module ? `${esc(node.module)}!${esc(name)}` : esc(name);
    const hit = $("highlight").value.trim() &&
      name.toLowerCase().includes($("highlight").value.trim().toLowerCase());
    svg += `<rect class="bar ${hit ? "search-hit" : ""}" data-id="${node._id}" x="${x.toFixed(2)}" y="${y}" ` +
      `width="${Math.max(0.5, w - 0.5).toFixed(2)}" height="${ROW_H - 1}" fill="${frameColor(node.module, name)}"/>`;
    const labelChars = Math.floor((w - 6) / CHAR_W);
    if (labelChars >= 3) {
      const label = name.length > labelChars ? name.slice(0, labelChars - 1) + "…" : name;
      svg += `<text class="barlabel" x="${(x + 3).toFixed(2)}" y="${y + 13}">${esc(label)}</text>`;
    }
    let cx = x;
    for (const c of node.children || []) {
      const cw = w * c.total_ns / node.total_ns;
      draw(c, cx, cw, level + 1);
      cx += cw;
    }
  }

  // assign stable ids and collect node refs for tooltips/zoom
  let idSeq = 0;
  (function tag(n) { n._id = idSeq++; for (const c of n.children || []) tag(c); })(zoomRoot);
  draw(zoomRoot, 0, width, 0);
  svg += "</svg>";
  mount.innerHTML = svg;

  const byId = new Map();
  (function collect(n) { byId.set(n._id, n); for (const c of n.children || []) collect(c); })(zoomRoot);
  mount.querySelectorAll("rect.bar").forEach((rect) => {
    const node = byId.get(Number(rect.dataset.id));
    rect.addEventListener("mousemove", (evt) => {
      showTip(evt, `<b>${esc(node.module || "")}!${esc(node.func || "(root)")}</b><br>` +
        `自身: ${node.self_ns} ns (${pct(node.self_ns, totalWeight)})<br>` +
        `累计: ${node.total_ns} ns (${pct(node.total_ns, totalWeight)})<br>` +
        `样本数: ${node.count}` +
        `<div class="hint">点击放大此节点；再次点击根节点还原</div>`);
    });
    rect.addEventListener("mouseleave", hideTip);
    rect.addEventListener("click", () => { state.zoomNode = node; renderFlame(root, totalWeight, mount); });
  });
}

function renderDiffFlame(tree, totals, mount) {
  // width is driven by the larger normalized share so both versions align
  const depth = treeDepth(tree);
  const width = Math.max(900, mount.clientWidth - 20);
  const height = depth * ROW_H + 2 * PAD;
  let svg = `<svg width="${width}" height="${height}" viewBox="0 0 ${width} ${height}">`;
  const scale = (node) =>
    Math.max(node.old_count / Math.max(1, totals.old), node.new_count / Math.max(1, totals.new));

  function draw(node, x, w, level) {
    if (w < MIN_W) return;
    const y = height - PAD - (level + 1) * ROW_H;
    const name = node.func || "(root)";
    const hit = $("highlight").value.trim() &&
      name.toLowerCase().includes($("highlight").value.trim().toLowerCase());
    svg += `<rect class="bar ${hit ? "search-hit" : ""}" data-id="${node._id}" x="${x.toFixed(2)}" y="${y}" ` +
      `width="${Math.max(0.5, w - 0.5).toFixed(2)}" height="${ROW_H - 1}" ` +
      `fill="${diffColor(node.delta_norm, node.old_count, node.new_count)}"/>`;
    const labelChars = Math.floor((w - 6) / CHAR_W);
    if (labelChars >= 3) {
      const label = name.length > labelChars ? name.slice(0, labelChars - 1) + "…" : name;
      svg += `<text class="barlabel" x="${(x + 3).toFixed(2)}" y="${y + 13}" ` +
        (node.delta_norm === 0 ? 'style="fill:#cfe0f0"' : "") + `>${esc(label)}</text>`;
    }
    let cx = x;
    const parentScale = scale(node) || 1;
    for (const c of node.children || []) {
      const cw = w * scale(c) / parentScale;
      draw(c, cx, cw, level + 1);
      cx += cw;
    }
  }

  let idSeq = 0;
  (function tag(n) { n._id = idSeq++; for (const c of n.children || []) tag(c); })(tree);
  draw(tree, 0, width, 0);
  svg += "</svg>";
  mount.innerHTML = svg;

  const byId = new Map();
  (function collect(n) { byId.set(n._id, n); for (const c of n.children || []) collect(c); })(tree);
  mount.querySelectorAll("rect.bar").forEach((rect) => {
    const node = byId.get(Number(rect.dataset.id));
    rect.addEventListener("mousemove", (evt) => {
      const oldShare = totals.old ? (100 * node.old_count / totals.old).toFixed(2) : "0.00";
      const newShare = totals.new ? (100 * node.new_count / totals.new).toFixed(2) : "0.00";
      const dir = node.delta_norm > 0 ? '<span style="color:var(--slow)">变慢</span>'
        : node.delta_norm < 0 ? '<span style="color:var(--fast)">变快</span>' : "持平";
      showTip(evt, `<b>${esc(node.module || "")}!${esc(node.func || "(root)")}</b><br>` +
        `旧版本: ${node.old_count} 样本 (${oldShare}%)<br>` +
        `新版本: ${node.new_count} 样本 (${newShare}%)<br>` +
        `归一化变化: ${(node.delta_norm * 100).toFixed(3)} 个百分点 · ${dir}`);
    });
    rect.addEventListener("mouseleave", hideTip);
  });
}

// ---------- tab loaders ----------
async function loadFlame() {
  const mount = $("tab-flame");
  const q = queryString({ program: $("program").value, version: $("version").value });
  const data = await getJSON("/api/flame?" + q);
  const gapRows = [...data.gaps.map((g) => ({ ...g, kind: "gap" })),
                   ...data.unknowns.map((g) => ({ ...g, kind: "unknown" }))];
  const gapTotal = gapRows.reduce((a, g) => a + g.samples, 0);
  mount.innerHTML = `
    <div class="card">
      <h2>火焰图 · ${esc(data.program)} @ ${esc(data.version)}
        <span class="pill">${data.sample_count} 样本 · ${data.total_weight_ns} ns</span></h2>
      <div class="legend"><span>兄弟节点按函数名排序；宽度 &lt; 1/1000 的节点折叠为 [folded]；点击节点放大</span></div>
      <div id="flameWrap"></div>
    </div>
    <div class="card">
      <h2>符号表空隙 / 未知地址样本
        <span class="pill">合计 ${gapTotal} 样本，占 ${pct(gapTotal, data.sample_count)}</span></h2>
      <table><thead><tr><th>类别</th><th>模块</th><th>帧</th><th class="num">样本数</th>
      <th class="num">占比</th><th class="num">耗时 ns</th></tr></thead><tbody>
      ${gapRows.length ? gapRows.map((g) => `<tr><td><span class="pill ${g.kind}">${g.kind}</span></td>
        <td>${esc(g.module)}</td><td>${esc(g.func)}</td>
        <td class="num">${g.samples}</td><td class="num">${pct(g.samples, data.sample_count)}</td>
        <td class="num">${g.weight_ns}</td></tr>`).join("") :
        '<tr><td colspan="6" class="muted">无空隙样本</td></tr>'}</tbody></table>
    </div>`;
  state.zoomNode = null;
  renderFlame(data.root, data.total_weight_ns, $("flameWrap"));
}

async function loadDiff() {
  const mount = $("tab-diff");
  const q = queryString({
    program: $("program").value,
    old_version: $("oldVersion").value, new_version: $("newVersion").value,
  });
  const data = await getJSON("/api/diff?" + q);
  const totals = { old: data.old_total_samples, new: data.new_total_samples };
  const top = data.rows.slice(0, 30);
  mount.innerHTML = `
    <div class="card">
      <h2>差分火焰图 · ${esc(data.old_version)} → ${esc(data.new_version)}
        <span class="pill">旧 ${data.old_total_samples} 样本 / 新 ${data.new_total_samples} 样本，已按各自总样本数归一化</span></h2>
      <div class="legend">
        <span><i class="swatch" style="background:var(--slow)"></i>变慢（归一化占比上升）</span>
        <span><i class="swatch" style="background:var(--fast)"></i>变快（占比下降）</span>
        <span><i class="swatch" style="background:#3f5163"></i>持平</span>
      </div>
      <div id="diffFlameWrap"></div>
    </div>
    <div class="card">
      <h2>变化最大的函数（按归一化变化绝对值排序）</h2>
      <table><thead><tr><th>方向</th><th>模块</th><th>函数</th>
      <th class="num">旧样本</th><th class="num">新样本</th>
      <th class="num">旧占比</th><th class="num">新占比</th><th class="num">Δ 占比</th></tr></thead><tbody>
      ${top.map((r) => `<tr class="${r.direction}"><td class="dir"><span class="pill ${r.direction}">${
        r.direction === "slower" ? "变慢" : r.direction === "faster" ? "变快" : "持平"}</span></td>
        <td>${esc(r.module)}</td><td>${esc(r.func)}</td>
        <td class="num">${r.old_count}</td><td class="num">${r.new_count}</td>
        <td class="num">${(r.old_share * 100).toFixed(2)}%</td>
        <td class="num">${(r.new_share * 100).toFixed(2)}%</td>
        <td class="num">${(r.delta_share * 100).toFixed(3)} pp</td></tr>`).join("")}
      </tbody></table>
    </div>`;
  renderDiffFlame(data.tree, totals, $("diffFlameWrap"));
}

async function loadHot() {
  const mount = $("tab-hot");
  const q = queryString({ program: $("program").value, version: $("version").value });
  const data = await getJSON("/api/hot?" + q);
  mount.innerHTML = `
    <div class="card"><h2>热点函数排行 · ${esc(data.program)} @ ${esc(data.version)}
      <span class="pill">${data.sample_count} 样本 · ${data.total_weight_ns} ns</span></h2>
      <table><thead><tr><th>模块</th><th>函数</th><th class="num">自身 ns</th>
      <th class="num">自身占比</th><th class="num">累计 ns</th><th class="num">累计占比</th>
      <th class="num">样本数</th><th class="num">叶子样本</th></tr></thead><tbody>
      ${data.rows.map((r) => `<tr><td>${esc(r.module)}</td><td>${esc(r.func)}</td>
        <td class="num">${r.self_ns}</td><td class="num">${(r.self_share * 100).toFixed(2)}%</td>
        <td class="num">${r.total_ns}</td><td class="num">${(r.total_share * 100).toFixed(2)}%</td>
        <td class="num">${r.count}</td><td class="num">${r.self_count}</td></tr>`).join("")}
      </tbody></table>
      <div class="hint">累计耗时按“样本是否经过该函数”统计，递归帧同一样本只计一次。</div>
    </div>`;
}

async function loadStatus() {
  const mount = $("tab-status");
  const data = await getJSON("/api/status");
  mount.innerHTML = `
    <div class="card"><h2>上报情况 · 共 ${data.total_samples} 条样本，${data.processes.length} 个进程</h2>
      <table><thead><tr><th>程序</th><th>版本</th><th class="num">PID</th>
      <th class="num">采样周期 ns</th><th class="num">样本数</th><th class="num">模块数</th>
      <th>时间范围 (ns)</th><th>标签</th></tr></thead><tbody>
      ${data.processes.map((p) => `<tr><td>${esc(p.program)}</td><td>${esc(p.version)}</td>
        <td class="num">${p.pid}</td><td class="num">${p.period_ns}</td>
        <td class="num">${p.samples}</td><td class="num">${p.modules}</td>
        <td class="num">${p.first_ns} → ${p.last_ns}</td>
        <td>${p.labels.map((l) => `<span class="pill">${esc(l)}</span>`).join(" ")}</td></tr>`).join("")}
      </tbody></table></div>`;
}

// ---------- wiring ----------
async function refreshPrograms(keep) {
  const data = await getJSON("/api/programs");
  state.programs = data.programs;
  const sel = $("program");
  sel.innerHTML = data.programs.map((p) =>
    `<option value="${esc(p.name)}">${esc(p.name)}</option>`).join("");
  if (keep && data.programs.some((p) => p.name === keep)) sel.value = keep;
  refreshVersions();
}
function refreshVersions() {
  const prog = state.programs.find((p) => p.name === $("program").value);
  const versions = prog ? prog.versions : [];
  for (const id of ["version", "oldVersion", "newVersion"]) {
    $(id).innerHTML = versions.map((v) => `<option>${esc(v)}</option>`).join("");
  }
  if (versions.length >= 2) {
    $("oldVersion").value = versions[0];
    $("newVersion").value = versions[versions.length - 1];
  }
}

function switchTab(tab) {
  state.tab = tab;
  document.querySelectorAll(".tabs button").forEach((b) =>
    b.classList.toggle("active", b.dataset.tab === tab));
  for (const t of ["flame", "diff", "hot", "status"])
    $("tab-" + t).style.display = t === tab ? "" : "none";
  const isDiff = tab === "diff";
  $("versionLabel").style.display = isDiff ? "none" : "";
  $("oldVersionLabel").style.display = isDiff ? "" : "none";
  $("newVersionLabel").style.display = isDiff ? "" : "none";
  loaders[tab]();
}
const loaders = { flame: loadFlame, diff: loadDiff, hot: loadHot, status: loadStatus };

document.querySelectorAll(".tabs button").forEach((b) =>
  b.addEventListener("click", () => switchTab(b.dataset.tab)));
$("program").addEventListener("change", refreshVersions);
$("refresh").addEventListener("click", () => loaders[state.tab]().catch(alertErr));
$("resetZoom").addEventListener("click", () => {
  if (state.tab === "flame") loadFlame().catch(alertErr);
});
$("highlight").addEventListener("input", () => loaders[state.tab]().catch(() => {}));
function alertErr(e) { console.error(e); alert(e.message); }

refreshPrograms().then(() => switchTab("flame")).catch(alertErr);
