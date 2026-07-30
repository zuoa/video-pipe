// Front-end logic for the video-pipe management UI (vanilla JS, no framework).
// The shell is server-rendered; the stream table is refreshed from the JSON API.

const body = document.getElementById("streams-body");
const form = document.getElementById("create-form");
const formMsg = document.getElementById("form-msg");

async function api(method, path, payload) {
  const opts = { method, headers: {} };
  if (payload !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(payload);
  }
  const res = await fetch(path, opts);
  if (res.status === 204) return null;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || "HTTP " + res.status);
  return data;
}

function esc(s) {
  return String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
}

function statusBadge(s) {
  const running = s.state === "running";
  let label, cls;
  if (s.state === "error") { label = "错误"; cls = "red"; }
  else if (s.state === "stopped") { label = "已停止"; cls = "grey"; }
  else if (s.state === "restarting") { label = "重启中"; cls = "yellow"; }
  else if (running && s.mtx_online) { label = "在线"; cls = "green"; }
  else if (running) { label = "推流中"; cls = "yellow"; }
  else { label = s.state || "未知"; cls = "grey"; }
  return `<span class="dot ${cls}"></span>${label}`;
}

function urlRows(urls) {
  return ["rtsp", "rtmp", "hls", "webrtc", "srt"]
    .map((k) => {
      const u = urls[k] || "";
      return `<div class="u"><span class="proto">${k.toUpperCase()}</span>` +
        `<code title="${esc(u)}">${esc(u)}</code>` +
        `<button class="mini" data-copy="${esc(u)}">复制</button></div>`;
    })
    .join("");
}

function render(streams) {
  if (!streams.length) {
    body.innerHTML = `<tr><td colspan="8" class="muted">还没有流，先在上方创建一路吧（类型选 test 可免输入源做冒烟测试）</td></tr>`;
    return;
  }
  body.innerHTML = streams
    .map((s) => {
      const canStart = s.state === "stopped" || s.state === "error";
      const canStop = s.state !== "stopped";
      const name = esc(s.name);
      return `<tr>
        <td>${statusBadge(s)}</td>
        <td class="mono">${name}</td>
        <td>${esc(s.source_type)}</td>
        <td class="src" title="${esc(s.source_url)}">${esc(s.source_url || "(test pattern)")}</td>
        <td>${s.restart_count}</td>
        <td>${s.readers}</td>
        <td>${urlRows(s.urls)}</td>
        <td class="actions">
          <button class="mini" data-action="start" data-name="${name}" ${canStart ? "" : "disabled"}>启动</button>
          <button class="mini" data-action="stop" data-name="${name}" ${canStop ? "" : "disabled"}>停止</button>
          <button class="mini danger" data-action="del" data-name="${name}">删除</button>
        </td>
      </tr>`;
    })
    .join("");
}

async function refresh() {
  try {
    render(await api("GET", "/api/streams"));
  } catch (e) {
    console.warn("refresh failed", e);
  }
}

async function copy(text) {
  // navigator.clipboard requires a secure context (HTTPS or http://localhost).
  // A deployment reached over plain HTTP on a LAN IP / remote host has no
  // clipboard API, so fall back to a hidden textarea + execCommand.
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      toast("已复制");
      return;
    } catch {
      // fall through to legacy path
    }
  }
  toast(legacyCopy(text) ? "已复制" : "复制失败，请手动选择");
}

// legacyCopy copies text via a transient textarea + execCommand('copy'). Returns
// false when the browser blocks it; used as the non-secure-context fallback.
function legacyCopy(text) {
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.setAttribute("readonly", "");
  ta.style.position = "fixed";
  ta.style.top = "-9999px";
  ta.style.opacity = "0";
  document.body.appendChild(ta);
  ta.select();
  ta.setSelectionRange(0, ta.value.length); // iOS Safari
  let ok = false;
  try { ok = document.execCommand("copy"); } catch { ok = false; }
  document.body.removeChild(ta);
  return ok;
}

// Event delegation for copy / start / stop / delete buttons.
body.addEventListener("click", async (e) => {
  const cp = e.target.closest("[data-copy]");
  if (cp) {
    await copy(cp.getAttribute("data-copy"));
    return;
  }
  const btn = e.target.closest("[data-action]");
  if (!btn || btn.disabled) return;
  const op = btn.getAttribute("data-action");
  const name = btn.getAttribute("data-name");
  if (op === "del" && !confirm(`确认删除流 ${name}？此操作不可恢复。`)) return;
  try {
    const enc = encodeURIComponent(name);
    if (op === "start") await api("POST", `/api/streams/${enc}/start`);
    else if (op === "stop") await api("POST", `/api/streams/${enc}/stop`);
    else if (op === "del") await api("DELETE", `/api/streams/${enc}`);
    await refresh();
  } catch (err) {
    toast(err.message);
  }
});

form.addEventListener("submit", async (e) => {
  e.preventDefault();
  formMsg.textContent = "";
  formMsg.className = "msg";
  const fd = new FormData(form);
  const payload = {
    name: fd.get("name"),
    source_url: fd.get("source_url"),
    source_type: fd.get("source_type"),
  };
  try {
    await api("POST", "/api/streams", payload);
    form.reset();
    toast("已创建并启动");
    await refresh();
  } catch (err) {
    formMsg.textContent = err.message;
    formMsg.className = "msg err";
  }
});

let toastTimer;
function toast(msg) {
  let el = document.getElementById("toast");
  if (!el) {
    el = document.createElement("div");
    el.id = "toast";
    el.className = "toast";
    document.body.appendChild(el);
  }
  el.textContent = msg;
  el.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.remove("show"), 1800);
}

refresh();
setInterval(refresh, 5000);
