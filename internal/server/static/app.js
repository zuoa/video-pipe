// Front-end logic for the video-pipe management UI (vanilla JS, no framework).
// The shell is server-rendered; the stream table is refreshed from the JSON API.

const body = document.getElementById("streams-body");
const form = document.getElementById("create-form");
const formMsg = document.getElementById("form-msg");

// The UI intentionally exposes one input type. Provider-backed sources are
// translated back into the API's provider + source_type fields on submit.
const inputType = document.getElementById("input-type");
const srcUrl = document.getElementById("source-url");
const srcUrlWrap = document.getElementById("source-url-wrap");
const srcUrlLabel = document.getElementById("source-url-label");
const srcUrlHelp = document.getElementById("source-url-help");
const typeHelp = document.getElementById("type-help");
const srcFileWrap = document.getElementById("source-file-wrap");
const testNote = document.getElementById("test-note");
const srcFile = document.getElementById("source-file");
const uploadBtn = document.getElementById("upload-btn");
const uploadStatus = document.getElementById("upload-status");
const createSubmit = document.getElementById("create-submit");

const typeCopy = {
  auto: ["根据地址自动识别协议", "输入地址", "粘贴 RTSP、RTMP 或 HTTP 地址", "支持 rtsp://、rtmp://、http:// 和 https://"],
  rtsp: ["网络摄像机与实时视频源", "RTSP 地址", "rtsp://user:password@host:554/path", "支持 rtsp:// 和 rtsps://"],
  rtmp: ["直播推流或拉流地址", "RTMP 地址", "rtmp://host/app/stream", "支持 rtmp:// 和 rtmps://"],
  http: ["HTTP 视频流或 HLS 播放列表", "HTTP / HLS 地址", "https://example.com/live/index.m3u8", "支持 HTTP 视频与 .m3u8 地址"],
  "provider:bilibili": ["自动解析视频或直播间", "B站地址", "粘贴视频页或直播间地址", "支持 bilibili.com 视频页与 live.bilibili.com 直播间"],
  "provider:douyu": ["实验性解析直播间", "斗鱼房间地址", "https://www.douyu.com/12345", "粘贴完整的斗鱼直播间地址"],
  file: ["上传服务器本地播放", "", "", ""],
  test: ["使用系统内置测试信号", "", "", ""],
};

let sourceRevision = 0;

function syncSourceUI() {
  sourceRevision += 1;
  const selectedType = inputType.value;
  const isFile = selectedType === "file";
  const isTest = selectedType === "test";
  const copy = typeCopy[selectedType] || typeCopy.auto;

  srcFileWrap.hidden = !isFile;
  srcUrlWrap.hidden = isFile || isTest;
  testNote.hidden = !isTest;
  srcUrl.required = !isFile && !isTest;

  typeHelp.textContent = copy[0];
  if (!isFile && !isTest) {
    srcUrlLabel.textContent = copy[1];
    srcUrl.placeholder = copy[2];
    srcUrlHelp.textContent = copy[3];
  }

  srcUrl.value = ""; // changing source invalidates the previous entry
  srcFile.value = "";
  uploadStatus.textContent = "";
  uploadStatus.className = "";
  if (isFile) uploadStatus.textContent = "选择后将立即上传";
  uploadBtn.disabled = false;
  createSubmit.disabled = isFile;
}
inputType.addEventListener("change", syncSourceUI);
syncSourceUI();

uploadBtn.addEventListener("click", () => srcFile.click());

srcFile.addEventListener("change", async () => {
  const file = srcFile.files[0];
  if (!file) return;
  const uploadRevision = sourceRevision;
  uploadStatus.textContent = "上传中…";
  uploadStatus.className = "";
  uploadBtn.disabled = true;
  createSubmit.disabled = true;
  const fd = new FormData();
  fd.append("file", file);
  try {
    const res = await fetch("/api/uploads", { method: "POST", body: fd });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || "HTTP " + res.status);
    if (uploadRevision !== sourceRevision || inputType.value !== "file") return;
    srcUrl.value = data.path; // hidden holder the submit handler reads
    uploadStatus.textContent = `${file.name}（${formatSize(data.size)}）已就绪`;
    uploadStatus.className = "ok";
    createSubmit.disabled = false;
  } catch (e) {
    if (uploadRevision !== sourceRevision || inputType.value !== "file") return;
    uploadStatus.textContent = "上传失败：" + e.message;
    uploadStatus.className = "err";
  } finally {
    uploadBtn.disabled = false;
  }
});

function formatSize(n) {
  if (n < 1024) return n + " B";
  if (n < 1048576) return (n / 1024).toFixed(1) + " KB";
  return (n / 1048576).toFixed(1) + " MB";
}

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

function typeLabel(s) {
  if (s.provider === "bilibili") return "B站";
  if (s.provider === "douyu") return "斗鱼（实验）";
  const labels = {
    file: "本地文件",
    rtsp: "RTSP",
    rtmp: "RTMP",
    http: "HTTP / HLS",
    test: "测试画面",
  };
  return labels[s.source_type] || esc(s.source_type);
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

// Protocols a browser can play inline (HLS via hls.js / native; WebRTC via
// WHEP). RTSP/RTMP/SRT have no browser player, so they get no play button.
const PLAYABLE = { hls: true, webrtc: true };

function urlRows(urls, name) {
  return ["rtsp", "rtmp", "hls", "webrtc", "srt"]
    .map((k) => {
      const u = urls[k] || "";
      const play = PLAYABLE[k] && u
        ? `<button class="mini play" data-play data-proto="${k}"` +
          ` data-url="${esc(u)}" data-name="${esc(name)}">播放</button>`
        : "";
      return `<div class="u"><span class="proto">${k.toUpperCase()}</span>` +
        `<code title="${esc(u)}">${esc(u)}</code>` +
        `<button class="mini" data-copy="${esc(u)}">复制</button>` +
        play + `</div>`;
    })
    .join("");
}

function render(streams) {
  if (!streams.length) {
    body.innerHTML = `<tr><td colspan="8" class="empty-cell">还没有流。可以先创建一路“测试画面”检查服务状态。</td></tr>`;
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
        <td>${typeLabel(s)}</td>
        <td class="src" title="${esc(s.source_url)}">${esc(s.source_url || "(test pattern)")}</td>
        <td>${s.restart_count}</td>
        <td>${s.readers}</td>
        <td>${urlRows(s.urls, s.name)}</td>
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

// Event delegation for copy / start / stop / delete / play buttons.
body.addEventListener("click", async (e) => {
  const play = e.target.closest("[data-play]");
  if (play) {
    openPlayer(play.getAttribute("data-name"), play.getAttribute("data-proto"), play.getAttribute("data-url"));
    return;
  }
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
  const selectedType = fd.get("input_type");
  const provider = selectedType.startsWith("provider:") ? selectedType.split(":")[1] : "";
  const payload = {
    name: fd.get("name"),
    source_url: srcUrl.value,
    source_type: provider ? "http" : selectedType,
    provider,
  };
  if (!payload.provider && payload.source_type === "file" && !payload.source_url) {
    formMsg.textContent = "请先选择并上传文件";
    formMsg.className = "msg err";
    return;
  }
  try {
    await api("POST", "/api/streams", payload);
    form.reset();
    syncSourceUI();
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

// ---------------------------------------------------------------------------
// Inline player modal: plays HLS (hls.js, or native on Safari/iOS) and WebRTC
// (minimal WHEP — POST a full SDP offer to the stream's /whep endpoint, apply
// the answer). hls.js is vendored at /static/hls.min.js and loaded on demand so
// the listing page stays light and works on a browser with no internet.
// ---------------------------------------------------------------------------

const modal = document.getElementById("player-modal");
const modalTitle = document.getElementById("player-title");
const playerVideo = document.getElementById("player-video");
const playerStatus = document.getElementById("player-status");
let activePlayer = null; // { destroy() } for the current playback, or null

function setPlayerStatus(msg, isErr) {
  if (!msg) { playerStatus.hidden = true; playerStatus.textContent = ""; return; }
  playerStatus.hidden = false;
  playerStatus.textContent = msg;
  playerStatus.classList.toggle("err", !!isErr);
}

function openPlayer(name, proto, baseUrl) {
  closePlayer();
  modalTitle.textContent = `${name} · ${proto.toUpperCase()}`;
  setPlayerStatus(proto === "webrtc" ? "正在建立 WebRTC 连接…" : "正在加载 HLS…");
  modal.hidden = false;
  document.body.style.overflow = "hidden";
  if (proto === "hls") activePlayer = playHLS(baseUrl);
  else if (proto === "webrtc") activePlayer = playWebRTC(baseUrl.replace(/\/$/, "") + "/whep");
}

function closePlayer() {
  if (activePlayer) { try { activePlayer.destroy(); } catch {} activePlayer = null; }
  modal.hidden = true;
  document.body.style.overflow = "";
  setPlayerStatus("");
}

// hls.js is loaded once, on first HLS playback, then cached on window.Hls.
let hlsJsPromise = null;
function loadHlsJs() {
  if (window.Hls) return Promise.resolve(window.Hls);
  if (!hlsJsPromise) {
    hlsJsPromise = new Promise((resolve, reject) => {
      const s = document.createElement("script");
      s.src = "/static/hls.min.js";
      s.onload = () => resolve(window.Hls);
      s.onerror = () => { hlsJsPromise = null; reject(new Error("播放库 hls.js 加载失败")); };
      document.head.appendChild(s);
    });
  }
  return hlsJsPromise;
}

function playHLS(url) {
  // Safari / iOS play HLS natively; everything else needs hls.js.
  if (playerVideo.canPlayType("application/vnd.apple.mpegurl")) {
    playerVideo.src = url;
    playerVideo.play().catch(() => {});
    setPlayerStatus("");
    return { destroy() { playerVideo.removeAttribute("src"); playerVideo.load(); } };
  }
  let hls = null;
  loadHlsJs()
    .then((Hls) => {
      if (!Hls.isSupported()) { setPlayerStatus("当前浏览器不支持 HLS 播放", true); return; }
      hls = new Hls({ lowLatencyMode: true, enableWorker: true });
      hls.loadSource(url);
      hls.attachMedia(playerVideo);
      hls.on(Hls.Events.MANIFEST_PARSED, () => { setPlayerStatus(""); playerVideo.play().catch(() => {}); });
      hls.on(Hls.Events.ERROR, (_evt, data) => {
        if (data.fatal) setPlayerStatus("HLS 播放失败：" + (data.details || data.type), true);
      });
    })
    .catch((e) => setPlayerStatus(e.message, true));
  return { destroy() { if (hls) hls.destroy(); playerVideo.removeAttribute("src"); playerVideo.load(); } };
}

// Minimal WHEP reader: gather all ICE candidates into the offer (non-trickle),
// POST it, apply the answer. No STUN — on a LAN the server's host candidate
// (configured via webrtcAdditionalHosts) and the browser's host candidate
// connect directly.
function playWebRTC(whepUrl) {
  let pc = null;
  let closed = false;
  const status = (m, err) => { if (!closed) setPlayerStatus(m, err); };

  pc = new RTCPeerConnection({ iceServers: [] });
  pc.addTransceiver("video", { direction: "recvonly" });
  pc.addTransceiver("audio", { direction: "recvonly" });
  pc.ontrack = (evt) => {
    playerVideo.srcObject = evt.streams[0];
    playerVideo.play().catch(() => {});
    status("");
  };

  pc.createOffer()
    .then((offer) => pc.setLocalDescription(offer))
    .then(() => waitForIce(pc))
    .then(() => fetch(whepUrl, {
      method: "POST",
      headers: { "Content-Type": "application/sdp" },
      body: pc.localDescription.sdp,
    }))
    .then((res) => {
      if (res.status === 404) throw new Error("流不在线或不存在");
      if (!res.ok) throw new Error("信令失败 (HTTP " + res.status + ")");
      return res.text();
    })
    .then((answer) => pc.setRemoteDescription({ type: "answer", sdp: answer }))
    .catch((err) => { if (!closed) status("WebRTC 播放失败：" + err.message, true); });

  return {
    destroy() {
      closed = true;
      if (pc) pc.close();
      playerVideo.srcObject = null;
    },
  };
}

// Resolve once ICE gathering is complete (candidates are in the local SDP), or
// after a timeout so a stalled gather can't hang playback forever.
function waitForIce(pc) {
  return new Promise((resolve) => {
    if (pc.iceGatheringState === "complete") return resolve();
    const done = () => {
      if (pc.iceGatheringState === "complete") {
        pc.removeEventListener("icegatheringstatechange", done);
        resolve();
      }
    };
    pc.addEventListener("icegatheringstatechange", done);
    setTimeout(() => { pc.removeEventListener("icegatheringstatechange", done); resolve(); }, 3000);
  });
}

// Close on backdrop / × button / Escape.
modal.addEventListener("click", (e) => { if (e.target.closest("[data-close]")) closePlayer(); });
document.addEventListener("keydown", (e) => { if (e.key === "Escape" && !modal.hidden) closePlayer(); });
