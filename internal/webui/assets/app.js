"use strict";

const TOKEN = document.querySelector('meta[name="token"]').content;

const urlInput = document.getElementById("url");
const goBtn = document.getElementById("go");
const urlErr = document.getElementById("urlerr");
const healthBar = document.getElementById("healthbar");
const logsBox = document.getElementById("logs");
const logsBody = document.getElementById("logs-body");
const readerStatus = document.getElementById("reader-status");
const readerContent = document.getElementById("reader-content");
const historyList = document.getElementById("history-list");

const MAX_LOG_LINES = 5000;
// 渲染上限 5000 行：服务端 broker 为 SSE 重连保留最多 50000 行全量重放，
// 浏览器侧仅保留最新 5000 个节点防止 DOM 膨胀；reset/重连时清空容器并把计数器归零。
const STEP_NAMES = {
  "metadata": "获取信息",
  "download-subtitle": "下载字幕",
  "parse-subtitle": "解析字幕",
  "download-audio": "下载音频",
  "transcode": "转码音频",
  "transcribe": "语音识别",
  "parse-asr": "解析识别结果",
  "format": "生成文稿",
};

const state = {
  running: false,
  lastURL: "",
  autoStick: true,
  logCount: 0,
};

const stepEls = new Map();
const pendingNodes = [];
let flushScheduled = false;

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

async function api(path, options) {
  const opts = options || {};
  opts.headers = Object.assign({ "Authorization": "Bearer " + TOKEN }, opts.headers || {});
  return fetch(path, opts);
}

function showURLErr(text) {
  if (!text) {
    urlErr.hidden = true;
    urlErr.textContent = "";
    return;
  }
  urlErr.textContent = text;
  urlErr.hidden = false;
}

function setRunning(running) {
  state.running = running;
  urlInput.readOnly = running;
  goBtn.textContent = running ? "取消" : "转换";
  goBtn.classList.toggle("running", running);
  document.body.classList.toggle("running", running);
  if (!running) showURLErr("");
}

function scheduleFlush() {
  if (flushScheduled) return;
  flushScheduled = true;
  requestAnimationFrame(flushLogs);
}

function enqueueNode(node) {
  pendingNodes.push(node);
  scheduleFlush();
}

function flushLogs() {
  flushScheduled = false;
  if (pendingNodes.length === 0) return;
  const stick = state.autoStick;
  const fragment = document.createDocumentFragment();
  let added = 0;
  while (pendingNodes.length > 0) {
    fragment.appendChild(pendingNodes.shift());
    added++;
  }
  logsBody.appendChild(fragment);
  state.logCount += added;
  if (state.logCount > MAX_LOG_LINES) {
    let excess = state.logCount - MAX_LOG_LINES;
    while (excess-- > 0 && logsBody.firstChild) {
      logsBody.removeChild(logsBody.firstChild);
    }
    state.logCount = MAX_LOG_LINES;
  }
  if (stick) logsBox.scrollTop = logsBox.scrollHeight;
}

function clearLogs() {
  pendingNodes.length = 0;
  flushScheduled = false;
  logsBody.replaceChildren();
  state.logCount = 0;
  stepEls.clear();
  state.autoStick = true;
  logsBox.scrollTop = 0;
}

function appendDropped(n) {
  enqueueNode(el("div", "log-dropped", "日志有间断，已丢弃 " + n + " 条"));
}

function appendLog(ev) {
  const line = el("div", "log-line" + (ev.level ? " " + ev.level : ""));
  if (ev.time) line.appendChild(el("span", "log-time", "[" + ev.time + "] "));
  line.appendChild(document.createTextNode(ev.text || ""));
  enqueueNode(line);
}

function setReaderStatus(mode, text) {
  if (mode === "hidden") {
    readerStatus.hidden = true;
    readerStatus.replaceChildren();
    return;
  }
  readerStatus.hidden = false;
  readerStatus.className = mode === "run" ? "" : mode;
  readerStatus.replaceChildren();
  if (mode === "run") {
    readerStatus.appendChild(el("span", "spinner"));
  } else {
    readerStatus.appendChild(el("span", "", mode === "done" ? "✓ " : mode === "fail" ? "✗ " : mode === "canceled" ? "○ " : ""));
  }
  readerStatus.appendChild(el("span", "status-title", text));
}

function handlePhase(ev) {
  const name = STEP_NAMES[ev.step] || ev.step || "";
  let line = stepEls.get(ev.step);
  if (ev.status === "start") {
    if (line && line.isConnected) {
      line.remove();
      state.logCount = Math.max(0, state.logCount - 1);
    }
    line = el("div", "step-line");
    const mark = el("span", "step-mark");
    mark.appendChild(el("span", "spinner"));
    line.appendChild(mark);
    line.appendChild(el("span", "step-text", name));
    stepEls.set(ev.step, line);
    enqueueNode(line);
    setReaderStatus("run", name);
    return;
  }
  if (!line || !line.isConnected) {
    line = el("div", "step-line");
    line.appendChild(el("span", "step-mark"));
    line.appendChild(el("span", "step-text", name));
    stepEls.set(ev.step, line);
    enqueueNode(line);
  }
  const mark = line.querySelector(".step-mark");
  mark.replaceChildren();
  if (ev.status === "done") {
    mark.textContent = "✓";
    line.classList.add("done");
    setReaderStatus("done", name);
  } else if (ev.status === "fail") {
    mark.textContent = "✗";
    line.classList.add("fail");
    setReaderStatus("fail", name);
  }
}

function setEmptyContent() {
  readerContent.className = "empty";
  readerContent.textContent = "粘贴链接开始转换，完成后的字幕显示在这里";
  readerStatus.hidden = true;
}

function showContentError(message) {
  readerContent.className = "error-state";
  readerContent.replaceChildren();
  readerContent.appendChild(el("div", "error-message", message || "加载失败"));
}

async function showTranscript(bvid, name) {
  const resp = await api("/api/transcript?bvid=" + encodeURIComponent(bvid));
  if (!resp.ok) {
    let msg = "文稿加载失败（" + resp.status + "）";
    try {
      const body = await resp.json();
      if (body && body.error) msg = body.error;
    } catch (_) { /* keep generic message */ }
    throw new Error(msg);
  }
  const text = await resp.text();
  readerContent.className = "";
  readerContent.textContent = text;
  readerContent.scrollTop = 0;
  if (name) {
    setReaderStatus("done", name);
    document.title = name + " - bilibili-txt";
  }
}

async function handleDone(data) {
  setRunning(false);
  const name = data && data.name ? data.name : "转换完成";
  if (data && data.bvid) {
    setReaderStatus("run", name);
    try {
      await showTranscript(data.bvid, name);
    } catch (err) {
      setReaderStatus("fail", "文稿加载失败");
      showContentError(err.message);
    }
  } else {
    setReaderStatus("done", name + " 转换完成");
  }
}

function handleError(data) {
  setRunning(false);
  setReaderStatus("fail", "转换失败");
  readerContent.className = "error-state";
  readerContent.replaceChildren();
  readerContent.appendChild(el("div", "error-message", (data && data.message) || "转换失败"));
  if (state.lastURL) {
    const retry = el("button", "", "重试");
    retry.id = "retry";
    retry.type = "button";
    retry.addEventListener("click", () => submitURL(state.lastURL));
    readerContent.appendChild(retry);
  }
}

function handleCanceled() {
  setRunning(false);
  setReaderStatus("canceled", "已取消");
}

function handleReset() {
  clearLogs();
  setReaderStatus("hidden");
  showURLErr("");
  setEmptyContent();
  document.title = "bilibili-txt";
}

function formatTime(mtime) {
  const d = new Date(mtime);
  if (isNaN(d.getTime())) return mtime || "";
  const pad = (n) => String(n).padStart(2, "0");
  const hm = pad(d.getMonth() + 1) + "-" + pad(d.getDate()) + " " + pad(d.getHours()) + ":" + pad(d.getMinutes());
  if (d.getFullYear() === new Date().getFullYear()) return hm;
  return d.getFullYear() + "-" + hm;
}

function renderHistory(entries) {
  historyList.replaceChildren();
  if (!entries || entries.length === 0) {
    historyList.appendChild(el("div", "history-empty", "暂无转换记录"));
    return;
  }
  for (const entry of entries) {
    const card = el("div", "history-card");
    card.appendChild(el("div", "card-name", entry.name || entry.bvid || ""));
    const meta = el("div", "card-meta");
    meta.appendChild(el("span", "card-bvid", entry.bvid || ""));
    meta.appendChild(el("span", "card-time", formatTime(entry.mtime)));
    card.appendChild(meta);
    card.addEventListener("click", () => {
      if (state.running) return;
      showTranscript(entry.bvid, entry.name).catch((err) => showContentError(err.message));
    });
    historyList.appendChild(card);
  }
}

function dispatch(ev) {
  if (ev.dropped && ev.dropped > 0) appendDropped(ev.dropped);
  switch (ev.type) {
    case "reset": handleReset(); break;
    case "log": appendLog(ev); break;
    case "phase": handlePhase(ev); break;
    case "done": handleDone(ev.data); break;
    case "error": handleError(ev.data); break;
    case "canceled": handleCanceled(); break;
    case "history": renderHistory(ev.data); break;
    default: break;
  }
}

async function loadHealth() {
  try {
    const resp = await api("/api/health");
    if (!resp.ok) throw new Error("HTTP " + resp.status);
    const checks = await resp.json();
    const failed = (checks || []).filter((c) => c.OK === false)
      .map((c) => c.Name + "：" + (c.Message || c.Detail || "检查失败"));
    if (failed.length > 0) {
      healthBar.textContent = "环境检查未通过：" + failed.join("；");
      healthBar.hidden = false;
    }
  } catch (err) {
    healthBar.textContent = "环境检查请求失败：" + err.message;
    healthBar.hidden = false;
  }
}

async function loadHistory() {
  try {
    const resp = await api("/api/history");
    if (!resp.ok) throw new Error("HTTP " + resp.status);
    renderHistory(await resp.json());
  } catch (err) {
    historyList.replaceChildren();
    historyList.appendChild(el("div", "history-empty", "历史记录加载失败"));
  }
}

async function submitURL(rawURL) {
  const value = (rawURL || "").trim();
  if (!value) {
    showURLErr("请输入视频链接");
    return;
  }
  state.lastURL = value;
  let resp;
  try {
    resp = await api("/api/jobs", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ url: value }),
    });
  } catch (err) {
    showURLErr("网络错误，无法连接服务：" + err.message);
    return;
  }
  if (resp.status === 202) {
    showURLErr("");
    setRunning(true);
    return;
  }
  let msg = "请求失败（" + resp.status + "）";
  try {
    const body = await resp.json();
    if (body && body.error) msg = body.error;
  } catch (_) { /* keep status message */ }
  if (resp.status === 409) msg = "已有任务进行中";
  showURLErr(msg);
}

async function cancelJob() {
  try {
    await api("/api/cancel", { method: "POST" });
  } catch (err) {
    showURLErr("取消请求失败：" + err.message);
  }
}

function primaryAction() {
  if (state.running) {
    cancelJob();
  } else {
    submitURL(urlInput.value);
  }
}

goBtn.addEventListener("click", primaryAction);
urlInput.addEventListener("keydown", (e) => {
  if (e.key === "Enter") {
    e.preventDefault();
    primaryAction();
  }
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && state.running) {
    e.preventDefault();
    cancelJob();
  }
});
logsBox.addEventListener("scroll", () => {
  state.autoStick = logsBox.scrollHeight - logsBox.scrollTop - logsBox.clientHeight < 40;
});

const events = new EventSource("/api/events?token=" + encodeURIComponent(TOKEN));
events.onmessage = (e) => {
  try {
    dispatch(JSON.parse(e.data));
  } catch (err) {
    appendLog({ level: "warn", text: "事件解析失败: " + err.message });
  }
};
events.onopen = () => {
  clearLogs();
};
events.onerror = () => {
  // 浏览器原生自动重连；重连成功后 onopen 清空容器，服务端重放全量缓冲。
};

// 转换进行中关闭窗口会连带结束后台服务并中断任务，先让用户确认一次；
// 空闲时关闭则不打扰（后台会随最后一个连接断开自动退出）。
window.addEventListener("beforeunload", (e) => {
  if (state.running) {
    e.preventDefault();
    e.returnValue = "";
  }
});

loadHealth();
loadHistory();
