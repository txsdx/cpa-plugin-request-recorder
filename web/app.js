"use strict";
const $ = (id) => document.getElementById(id);
const API = "/v0/management/plugins/request-recorder";
let managementKey = "",
  keyFromPanel = false,
  authGeneration = 0,
  offset = 0,
  total = 0,
  detail = null,
  activeTab = "conversation",
  generation = 0;
const outcomeNames = {
  succeeded: "成功",
  failed: "失败",
  canceled: "已中断",
  rejected: "已拒绝",
  incomplete: "未完整记录",
};
const sourceNames = {
  log_captured: "已采集日志",
  pending: "补全中",
  unavailable: "不可用",
  truncated: "有截断",
};
const fmt = (n) => (n == null ? "—" : Number(n).toLocaleString("zh-CN"));
const duration = (n) =>
  n == null
    ? "—"
    : n >= 1000
      ? (n / 1000).toFixed(2) + " s"
      : n.toFixed(0) + " ms";
const date = (s) =>
  s && !s.startsWith("0001-")
    ? new Date(s).toLocaleString("zh-CN", { hour12: false })
    : "—";
const pct = (n) => (n == null ? "—" : (n * 100).toFixed(1) + "%");
const el = (tag, cls, text) => {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text != null) n.textContent = text;
  return n;
};
function notice(s) {
  $("notice").hidden = !s;
  $("notice").textContent = s;
}
async function api(path, options = {}) {
  const r = await fetch(API + path, {
    ...options,
    credentials: "omit",
    cache: "no-store",
    headers: {
      Authorization: "Bearer " + managementKey,
      "Content-Type": "application/json",
      ...options.headers,
    },
  });
  if (!r.ok) {
    let error = "HTTP " + r.status;
    try {
      error = (await r.json()).error || error;
    } catch {}
    if (r.status === 401 || r.status === 403)
      error = "管理鉴权失败，请检查 CPA 管理密钥和 remote-management 设置。";
    throw new Error(error);
  }
  return r.json();
}
// CPAMC stores its remembered login and per-server key names in same-origin
// localStorage. Read only these known keys; never persist another credential copy.
function panelStoredValue(name) {
  let raw = localStorage.getItem(name);
  if (raw === null) return null;
  if (raw.length > 4 * 1024 * 1024) throw new Error("CPAMC 本地数据过大");
  const prefix = /^enc::v([12])::/.exec(raw);
  if (prefix) {
    const scope =
      prefix[1] === "2"
        ? "v2|" + location.host
        : location.host + "|" + navigator.userAgent;
    const secret = new TextEncoder().encode(
      "cli-proxy-api-webui::secure-storage|" + scope,
    );
    const binary = atob(raw.slice(prefix[0].length));
    const bytes = new Uint8Array(binary.length);
    for (let i = 0; i < binary.length; i++)
      bytes[i] = binary.charCodeAt(i) ^ secret[i % secret.length];
    raw = new TextDecoder().decode(bytes);
  }
  try {
    return JSON.parse(raw);
  } catch (error) {
    if (name === "managementKey" || name === "apiBase" || name === "apiUrl")
      return raw;
    throw error;
  }
}
function panelConnection() {
  try {
    const state = panelStoredValue("cli-proxy-auth")?.state;
    const apiBase =
      state?.apiBase ||
      panelStoredValue("apiBase") ||
      panelStoredValue("apiUrl");
    if (typeof apiBase !== "string") return null;
    const url = new URL(apiBase);
    if (url.origin !== location.origin || url.username || url.password)
      return null;
    const key = state?.managementKey || panelStoredValue("managementKey");
    return { apiBase, key: typeof key === "string" ? key.trim() : "" };
  } catch {
    return null;
  }
}
async function sha256Hex(text) {
  if (!crypto.subtle) throw new Error("备注同步需要 HTTPS 安全上下文");
  const bytes = await crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(text),
  );
  return Array.from(new Uint8Array(bytes), (b) =>
    b.toString(16).padStart(2, "0"),
  ).join("");
}
let panelMessage = "",
  panelSyncPromise = null,
  panelSyncAt = 0,
  panelSnapshot = "";
async function syncPanelAliases(force = false) {
  if (
    !managementKey ||
    panelSyncPromise ||
    (!force && Date.now() - panelSyncAt < 30000)
  )
    return panelSyncPromise;
  panelSyncAt = Date.now();
  panelSyncPromise = (async () => {
    const panel = panelConnection();
    if (!panel) return;
    const stored = panelStoredValue("api-key-names:v1:" + panel.apiBase);
    if (
      stored !== null &&
      (typeof stored !== "object" || Array.isArray(stored))
    )
      throw new Error("CPAMC 备注格式无法识别");
    const names = Object.fromEntries(
      Object.entries(stored || {}).filter(
        ([id, label]) =>
          /^[a-f0-9]{64}$/.test(id) &&
          typeof label === "string" &&
          label.trim(),
      ),
    );
    const snapshot = JSON.stringify([panel.apiBase, names]);
    if (snapshot === panelSnapshot) return;
    const credential = managementKey;
    // Fetch the actual keys only in memory to translate CPAMC's scoped hash to
    // the recorder's SHA-256 key ID. Only hashes and labels reach /aliases/sync.
    let response = await fetch("/v8/management/config/access/api-keys", {
      credentials: "omit",
      cache: "no-store",
      headers: { Authorization: "Bearer " + credential },
    });
    if (response.status === 404 || response.status === 405)
      response = await fetch("/v0/management/api-keys", {
        credentials: "omit",
        cache: "no-store",
        headers: { Authorization: "Bearer " + credential },
      });
    if (!response.ok)
      throw new Error("无法读取 CPA key 列表（HTTP " + response.status + "）");
    const data = await response.json();
    const keys = Array.isArray(data) ? data : data["api-keys"];
    if (
      !Array.isArray(keys) ||
      keys.length > 10000 ||
      keys.some((key) => typeof key !== "string")
    )
      throw new Error("CPA key 列表格式无法识别或超过同步上限");
    const aliases = {};
    let matched = 0;
    for (const key of keys) {
      const id = await sha256Hex(
        JSON.stringify(["api-key-name", panel.apiBase, key]),
      );
      if (names[id]) {
        aliases[await sha256Hex(key)] = names[id].trim();
        matched++;
      }
    }
    if (
      credential !== managementKey ||
      panelConnection()?.apiBase !== panel.apiBase
    )
      return;
    await api("/aliases/sync", {
      method: "POST",
      body: JSON.stringify({ aliases }),
    });
    panelSnapshot = snapshot;
    panelMessage = `CPAMC 备注已同步 ${matched} 条（手动别名优先）`;
    const unmatched = Object.keys(names).length - matched;
    if (unmatched) panelMessage += ` · ${unmatched} 条未匹配当前 CPA key`;
  })()
    .catch((error) => {
      panelMessage = "CPAMC 备注同步失败：" + error.message;
    })
    .finally(() => {
      panelSyncPromise = null;
    });
  return panelSyncPromise;
}
async function connectFromPanel() {
  const panel = panelConnection();
  if (!panel?.key) {
    if (keyFromPanel) {
      managementKey = "";
      keyFromPanel = false;
      clearAuthenticatedView();
      notice("CPAMC 登录凭据已移除。后台仍在记录；查看记录需要重新登录。");
    }
    return false;
  }
  if (managementKey !== panel.key) clearAuthenticatedView();
  managementKey = panel.key;
  keyFromPanel = true;
  try {
    await api("/status"); // The host validates the borrowed key before any data access.
    if (managementKey !== panel.key) return false;
    await syncPanelAliases(true);
    await refresh();
    if ($("login").open) $("login").close();
    return true;
  } catch {
    if (managementKey !== panel.key) return false;
    managementKey = "";
    keyFromPanel = false;
    clearAuthenticatedView();
    notice("无法复用 CPAMC 登录，请使用管理密钥连接。后台记录不受影响。");
    return false;
  }
}
function clearAuthenticatedView() {
  generation++;
  authGeneration++;
  clearDetailImages();
  detail = null;
  if ($("detail").open) $("detail").close();
  $("rows").replaceChildren();
  $("detail-body").replaceChildren();
  $("detail-metrics").replaceChildren();
  $("empty").hidden = false;
  $("connection").textContent = "未连接";
  $("health").textContent = "等待连接";
  $("total").textContent = "—";
  document
    .querySelectorAll("#stats strong")
    .forEach((n) => (n.textContent = "—"));
}
function filterQuery() {
  const q = new URLSearchParams();
  for (const [k, v] of new FormData($("filters")))
    if (v) {
      q.set(k, k === "from" || k === "to" ? new Date(v).toISOString() : v);
    }
  q.set("offset", offset);
  q.set("limit", "50");
  return q;
}
async function refresh() {
  if (!managementKey) {
    $("login").showModal();
    return;
  }
  const seq = ++generation;
  try {
    await syncPanelAliases();
    const [page, status] = await Promise.all([
      api("/records?" + filterQuery()),
      api("/status"),
    ]);
    if (seq !== generation) return;
    total = page.total;
    renderRows(page.items);
    $("total").textContent = fmt(total);
    $("connection").textContent = "● 已连接";
    $("connection").className = "";
    $("health").replaceChildren(
      el(
        "div",
        "",
        status.logs_enabled
          ? "上游日志补全已启用"
          : "上游原文：未配置 logs_dir",
      ),
      el(
        "div",
        "",
        `采集中 ${status.active} · 补全中 ${status.pending_logs} · 丢弃 ${status.dropped_requests}`,
      ),
    );
    if (panelMessage) $("health").append(el("div", "", panelMessage));
    const warnings = [];
    if (status.last_error) warnings.push("采集错误：" + status.last_error);
    if (status.dropped_requests)
      warnings.push("部分请求因采集并发超限未被记录，请调整 max_inflight。");
    if (!status.logs_enabled)
      warnings.push(
        "当前只有下游响应。开启 CPA request-log 并配置插件 logs_dir 后才能补全上游原文。",
      );
    notice(warnings.join(" "));
    const sums = page.items;
    const success = sums.filter((r) => r.outcome === "succeeded").length;
    const values = [
      fmt(total),
      sums.length ? pct(success / sums.length) : "—",
      sums.length
        ? duration(sums.reduce((s, r) => s + r.duration_ms, 0) / sums.length)
        : "—",
      fmt(sums.reduce((s, r) => s + (r.tokens?.TotalTokens || 0), 0)),
    ];
    document
      .querySelectorAll("#stats strong")
      .forEach((n, i) => (n.textContent = values[i]));
    $("page-info").textContent = total
      ? `${offset + 1}–${Math.min(offset + 50, total)} / ${fmt(total)} 条 · 每页 50 条`
      : "0 条记录";
    $("prev").disabled = offset === 0;
    $("next").disabled = offset + 50 >= total;
    $("empty").hidden = sums.length > 0;
    if (!sums.length) {
      $("empty").querySelector("h3").textContent = "暂无匹配的请求";
      $("empty").querySelector("p").textContent =
        "请求结束后自动记录；也可以调整筛选条件。";
    }
  } catch (e) {
    if (seq === generation) {
      notice(e.message);
      $("connection").textContent = "连接失败";
      throw e;
    }
  }
}
function renderRows(items) {
  $("rows").replaceChildren();
  for (const r of items) {
    const tr = el("tr");
    tr.tabIndex = 0;
    tr.setAttribute("aria-label", "查看请求 " + r.id);
    tr.onclick = () => openDetail(r.id);
    tr.onkeydown = (e) => {
      if (e.key === "Enter") openDetail(r.id);
    };
    const time = el("td", "", date(r.started_at));
    time.append(
      el("small", "", r.id.slice(0, 12) + (r.stream ? " · STREAM" : " · JSON")),
    );
    const who = el(
      "td",
      "",
      r.key_alias || r.key_hint || r.caller_scope?.slice(0, 14) || "未知请求者",
    );
    who.append(el("small", "", r.client_ip || "IP 未提供"));
    const model = el(
      "td",
      "model",
      r.response_model || r.model || r.requested_model || "—",
    );
    model.append(
      el(
        "small",
        "",
        `${r.provider || "—"}${r.reasoning_effort ? " / " + r.reasoning_effort : ""}`,
      ),
    );
    const state = el("td");
    state.append(
      el("span", "badge " + r.outcome, outcomeNames[r.outcome] || r.outcome),
      el("small", "", r.status ? "HTTP " + r.status : "状态码未知"),
    );
    const latency = el(
      "td",
      "mono",
      `${duration(r.ttft_ms)} / ${duration(r.duration_ms)}`,
    );
    if (r.ttft_ms == null && r.first_chunk_ms != null)
      latency.append(el("small", "", `首分片 ${duration(r.first_chunk_ms)}`));
    const tokens = el("td", "mono", fmt(r.tokens?.TotalTokens));
    tokens.append(el("small", "", `缓存 ${pct(r.cache_ratio)}`));
    const upstream = el("td");
    upstream.append(
      el(
        "span",
        "badge " +
          (r.upstream_state === "pending"
            ? "pending"
            : r.upstream_state === "log_captured"
              ? ""
              : "incomplete"),
        sourceNames[r.upstream_state] || "未知",
      ),
    );
    if (r.warnings?.length)
      upstream.append(el("small", "", `△ ${r.warnings.length} 条采集提示`));
    tr.append(time, who, model, state, latency, tokens, upstream);
    $("rows").append(tr);
  }
}
async function openDetail(id) {
  const authRevision = authGeneration;
  try {
    const data = await api("/record?id=" + encodeURIComponent(id));
    if (authRevision !== authGeneration || !managementKey) return;
    if (!detail || detail.record.id !== data.record.id) clearDetailImages();
    detail = data;
    activeTab = "conversation";
    $("detail-title").textContent =
      data.record.response_model || data.record.model || "请求详情";
    $("detail-sub").textContent =
      data.record.id + " · " + date(data.record.started_at);
    const r = data.record;
    const vals = [
      ["请求者", r.key_alias || r.key_hint || "—"],
      ["首 token", duration(r.ttft_ms)],
      ["完整耗时", duration(r.duration_ms)],
      ["Token", fmt(r.tokens?.TotalTokens)],
      ["缓存命中", pct(r.cache_ratio)],
    ];
    $("detail-metrics").replaceChildren(
      ...vals.map(([k, v]) => {
        const d = el("div");
        d.append(el("small", "", k), document.createTextNode(v));
        return d;
      }),
    );
    renderDetail();
    if (!$("detail").open) $("detail").showModal();
  } catch (e) {
    notice(e.message);
  }
}
function raw(s) {
  const p = el("pre", "raw", s || "未采集 / 不可用");
  return p;
}
function pretty(s) {
  try {
    return JSON.stringify(JSON.parse(s), null, 2);
  } catch {
    return s;
  }
}
function section(title, body) {
  const box = el("section");
  box.append(el("h3", "", title), raw(body));
  return box;
}
// Images are fetched only from the authenticated CPA API. The original remote
// URL is never assigned to img.src, linked, or fetched by this page.
let imageSession = 0,
  imageController = new AbortController(),
  imagePollTimer = 0;
let imageQueue = [],
  imageActive = 0,
  imageObserver = null;
const imagePromises = new Map(),
  imageObjectURLs = new Set();
function clearDetailImages() {
  imageSession++;
  imageController.abort();
  imageController = new AbortController();
  for (const job of imageQueue)
    job.reject(new DOMException("Canceled", "AbortError"));
  imageQueue = [];
  imagePromises.clear();
  for (const url of imageObjectURLs) URL.revokeObjectURL(url);
  imageObjectURLs.clear();
  imageObserver?.disconnect();
  imageObserver = null;
  clearTimeout(imagePollTimer);
  imagePollTimer = 0;
  $("full-image").removeAttribute("src");
  $("image-download").removeAttribute("href");
  if ($("image-view").open) $("image-view").close();
}
function pumpImages() {
  while (imageActive < 2 && imageQueue.length) {
    const job = imageQueue.shift();
    imageActive++;
    job
      .run()
      .then(job.resolve, job.reject)
      .finally(() => {
        imageActive--;
        pumpImages();
      });
  }
}
function archivedImage(image, recordID) {
  const key = recordID + ":" + image.id;
  if (imagePromises.has(key)) return imagePromises.get(key);
  const session = imageSession,
    signal = imageController.signal;
  const promise = new Promise((resolve, reject) => {
    imageQueue.push({
      resolve,
      reject,
      run: async () => {
        if (session !== imageSession)
          throw new DOMException("Canceled", "AbortError");
        const path =
          API +
          "/image?record_id=" +
          encodeURIComponent(recordID) +
          "&id=" +
          encodeURIComponent(image.id);
        const response = await fetch(path, {
          signal,
          credentials: "omit",
          cache: "no-store",
          headers: { Authorization: "Bearer " + managementKey },
        });
        if (!response.ok)
          throw new Error("CPA 图片读取失败（HTTP " + response.status + "）");
        const blob = await response.blob();
        if (!/^image\/(png|jpeg|gif|webp|bmp|avif)$/.test(blob.type))
          throw new Error("CPA 返回的图片类型不支持");
        if (session !== imageSession || signal.aborted)
          throw new DOMException("Canceled", "AbortError");
        const url = URL.createObjectURL(blob);
        imageObjectURLs.add(url);
        return { url, mime: blob.type };
      },
    });
    pumpImages();
  });
  imagePromises.set(key, promise);
  promise.catch(() => imagePromises.delete(key));
  return promise;
}
function scheduleImagePoll() {
  if (imagePollTimer || !detail) return;
  const recordID = detail.record.id,
    session = imageSession;
  imagePollTimer = setTimeout(async () => {
    imagePollTimer = 0;
    if (
      session !== imageSession ||
      !detail ||
      detail.record.id !== recordID ||
      !$("detail").open
    )
      return;
    try {
      const state = await api(
        "/images?record_id=" + encodeURIComponent(recordID),
      );
      if (session !== imageSession || detail?.record.id !== recordID) return;
      const byID = new Map(state.images.map((im) => [im.id, im]));
      let changed = false,
        pending = false;
      detail.record.images = state.images;
      for (const m of [
        ...(detail.conversation || []),
        ...(detail.upstream_conversation || []),
      ]) {
        for (const im of m.images || []) {
          const asset = byID.get(im.id);
          let status =
            asset?.status ||
            (state.images.length >= state.limit ? "blocked" : "pending");
          const error =
            asset?.error ||
            (status === "blocked" ? "超过单条记录图片数量限制" : "");
          if (im.status !== status || im.error !== error) {
            im.status = status;
            im.error = error;
            im.mime = asset?.mime;
            changed = true;
          }
          if (status === "pending") pending = true;
        }
      }
      if (changed) renderDetail();
      if (pending || state.pending) scheduleImagePoll();
    } catch (error) {
      notice("无法查询 CPA 图片归档状态：" + error.message);
    }
  }, 1000);
}
function imageView(image) {
  const box = el("div", "image-card");
  box.dataset.imageId = image.id || "";
  box.append(
    el("div", "image-caption", image.alt || "原始图片 · CPA 本地归档"),
  );
  if (image.status !== "ready") {
    box.append(
      el(
        "p",
        image.status === "pending" ? "muted" : "error",
        image.status === "pending"
          ? "CPA 正在下载并保存原图…"
          : image.error || "图片未能保存到 CPA",
      ),
    );
    if (image.status === "pending") scheduleImagePoll();
    else {
      const retry = el("button", "", "重试服务端保存");
      retry.type = "button";
      retry.onclick = async () => {
        try {
          await api(
            "/images/retry?record_id=" + encodeURIComponent(detail.record.id),
            { method: "POST" },
          );
          image.status = "pending";
          image.error = "";
          renderDetail();
          scheduleImagePoll();
        } catch (error) {
          notice(error.message);
        }
      };
      box.append(retry);
    }
    return box;
  }
  const img = el("img", "original-image");
  img.alt = image.alt || "CPA 保存的原始图片";
  img.loading = "lazy";
  const status = el("p", "muted", "从 CPA 加载原图…");
  box.append(img, status);
  const recordID = detail.record.id;
  const load = async () => {
    try {
      const saved = await archivedImage(image, recordID);
      if (!box.isConnected) return;
      img.src = saved.url;
      status.remove();
      img.onerror = () =>
        box.append(
          el("p", "error", "本地图片无法解码；原始数据仍保留在 CPA。"),
        );
      const open = () => {
        $("full-image").src = saved.url;
        $("image-download").hidden = false;
        $("image-download").href = saved.url;
        $("image-download").download =
          image.id +
          "." +
          (saved.mime.split("/")[1] === "jpeg"
            ? "jpg"
            : saved.mime.split("/")[1]);
        $("image-view").showModal();
      };
      img.onclick = open;
      const zoom = el("button", "link", "查看已归档原图 ↗");
      zoom.type = "button";
      zoom.onclick = open;
      box.append(zoom);
    } catch (error) {
      if (error.name !== "AbortError" && box.isConnected) {
        status.className = "error";
        status.textContent = error.message;
        const retry = el("button", "", "重试服务端保存");
        retry.onclick = async () => {
          try {
            await api(
              "/images/retry?record_id=" + encodeURIComponent(recordID),
              { method: "POST" },
            );
            image.status = "pending";
            renderDetail();
            scheduleImagePoll();
          } catch (err) {
            notice(err.message);
          }
        };
        box.append(retry);
      }
    }
  };
  if ("IntersectionObserver" in window) {
    if (!imageObserver)
      imageObserver = new IntersectionObserver(
        (entries) => {
          for (const entry of entries)
            if (entry.isIntersecting) {
              imageObserver.unobserve(entry.target);
              entry.target.loadArchivedImage?.();
            }
        },
        { root: $("detail-body"), rootMargin: "250px" },
      );
    box.loadArchivedImage = load;
    imageObserver.observe(box);
  } else queueMicrotask(load);
  return box;
}

function bubble(m) {
  const box = el("article", "bubble " + m.role + " " + m.kind);
  const label = el("div", "bubble-label");
  const roles = {
    user: "用户",
    assistant: "模型",
    system: "系统",
    developer: "开发者",
    tool: "工具",
  };
  const kinds = {
    reasoning: "思考 / 摘要",
    tool_call: "调用工具",
    tool_result: "工具结果",
    attachment: "附件",
    image: "原始图片",
    notice: "上下文提示",
    error: "错误",
  };
  label.append(
    document.createTextNode(
      (roles[m.role] || m.role) + (kinds[m.kind] ? " · " + kinds[m.kind] : ""),
    ),
  );
  if (m.name) label.append(el("span", "mono", m.name));
  if (m.tool_call_id) label.append(el("span", "mono", m.tool_call_id));
  if (m.choice) label.append(el("span", "", `候选 ${m.choice}`));
  const body = el("div", "bubble-content");
  if (m.images?.length) {
    m.images.forEach((im) => body.append(imageView(im)));
    if (m.content) {
      body.append(el("pre", "", m.content));
    }
  } else if (
    m.kind === "reasoning" ||
    m.kind === "attachment" ||
    m.kind === "structured"
  ) {
    const d = el("details");
    d.append(
      el(
        "summary",
        "",
        m.kind === "reasoning" ? "查看思考内容" : "查看结构化内容",
      ),
      el("pre", "", m.content),
    );
    body.append(d);
  } else body.textContent = m.content;
  box.append(label, body);
  return box;
}
function renderDetail() {
  if (!detail) return;
  const r = detail.record,
    body = $("detail-body");
  imageObserver?.disconnect();
  body.replaceChildren();
  document.querySelectorAll("[data-tab]").forEach((b) => {
    const yes = b.dataset.tab === activeTab;
    b.classList.toggle("selected", yes);
    b.setAttribute("aria-selected", yes);
  });
  if (r.warnings?.length)
    body.append(el("div", "warnings", r.warnings.join("\n")));
  if (activeTab === "conversation") {
    body.append(
      el(
        "div",
        "view-note",
        "根据这次请求携带的历史上下文还原。原图由 CPA 保存并通过管理鉴权接口提供，浏览器不会访问图片源站。",
      ),
    );
    if (!detail.conversation?.length)
      body.append(el("p", "muted", "无可还原的对话内容，请查看原文。"));
    let source = "";
    for (const m of detail.conversation || []) {
      if (m.source !== source) {
        source = m.source;
        body.append(
          el(
            "div",
            "split-marker",
            source === "request"
              ? "REQUEST · 原请求中的历史"
              : "RESPONSE · 本次模型输出（下游）",
          ),
        );
      }
      body.append(bubble(m));
    }
    if (detail.upstream_conversation?.length) {
      body.append(
        el("div", "split-marker", "UPSTREAM · 最后一次上游尝试的输出"),
      );
      for (const m of detail.upstream_conversation) body.append(bubble(m));
    }
    if (r.error) body.append(el("div", "warnings", r.error));
  } else if (activeTab === "request") {
    body.append(
      el(
        "div",
        "view-note",
        "采集来源：" +
          r.request_source +
          "。正文保留多模态原始字段；鉴权请求头已脱敏。",
      ),
      section("原请求正文", pretty(r.request_body)),
    );
    const h = el("details", "headers");
    h.append(
      el("summary", "", "查看拦截器请求头（不是网络抓包）"),
      raw(JSON.stringify(r.request_headers, null, 2)),
    );
    body.append(h);
    if (r.intercepted_body)
      body.append(
        section(
          "选取凭证后的请求正文（仍可能在协议转换前）",
          pretty(r.intercepted_body),
        ),
      );
  } else if (activeTab === "upstream") {
    body.append(
      el(
        "div",
        "view-note",
        "来源：CPA request-log。保留所有上游尝试的请求 / 响应区段，包含原始 SSE。日志格式可能做过脱敏，不能视为网络字节级抓包。",
      ),
    );
    if (r.upstream_error || !r.upstream_responses)
      body.append(
        el(
          "div",
          "warnings",
          r.upstream_error ||
            "上游原文尚不可用。请开启 CPA request-log 并配置 logs_dir；点击“重新读取”检查补全状态。",
        ),
      );
    const reload = el("button", "", "重新读取");
    reload.onclick = () => openDetail(r.id);
    body.append(
      reload,
      section("上游请求（所有尝试）", r.upstream_requests),
      section("上游响应（所有尝试）", r.upstream_responses),
    );
  } else if (activeTab === "response") {
    body.append(
      el(
        "div",
        "view-note",
        "这是 response interceptor 观察到的下游协议响应，流式内容按原顺序拼接；后续插件或宿主还可能修改。",
      ),
      section("完整响应正文 / SSE", pretty(r.response_body)),
      section("响应头", JSON.stringify(r.response_headers, null, 2)),
    );
  } else {
    const meta = { ...r };
    for (const k of [
      "request_body",
      "intercepted_body",
      "request_headers",
      "response_headers",
      "response_body",
      "upstream_requests",
      "upstream_responses",
    ])
      delete meta[k];
    const dl = el("dl", "meta-grid");
    for (const [k, v] of Object.entries(meta)) {
      dl.append(
        el("dt", "mono", k),
        el(
          "dd",
          "mono",
          typeof v === "object" ? JSON.stringify(v, null, 2) : String(v ?? "—"),
        ),
      );
    }
    body.append(dl);
    const alias = el("button", "", "设置此 key 的别名");
    alias.disabled = !r.key_id;
    alias.onclick = () => {
      $("alias-id").value = r.key_id;
      $("alias-name").value = r.key_alias || "";
      $("alias-key").value = "";
      $("alias-dialog").showModal();
    };
    body.prepend(alias);
  }
  body.scrollTop = 0;
}
$("login-open").onclick = () => $("login").showModal();
$("refresh").onclick = () => refresh().catch(() => {});
$("alias-open").onclick = () => {
  $("alias-form").reset();
  $("alias-dialog").showModal();
};
$("login-form").onsubmit = async (e) => {
  e.preventDefault();
  clearAuthenticatedView();
  keyFromPanel = false;
  managementKey = $("management-key").value.trim();
  $("management-key").value = "";
  $("login-error").textContent = "";
  try {
    await refresh();
    $("login").close();
  } catch (err) {
    $("login-error").textContent = err.message;
  }
};
$("alias-form").onsubmit = async (e) => {
  e.preventDefault();
  $("alias-error").textContent = "";
  const key = $("alias-key").value;
  $("alias-key").value = "";
  try {
    await api("/aliases", {
      method: "POST",
      body: JSON.stringify({
        key,
        key_id: $("alias-id").value.trim(),
        alias: $("alias-name").value,
      }),
    });
    $("alias-dialog").close();
    await refresh();
    if (detail) await openDetail(detail.record.id);
  } catch (err) {
    $("alias-error").textContent = err.message;
  }
};
$("filters").onsubmit = (e) => {
  e.preventDefault();
  offset = 0;
  refresh().catch(() => {});
};
$("filters").onreset = () => {
  offset = 0;
  setTimeout(() => refresh().catch(() => {}), 0);
};
$("prev").onclick = () => {
  offset = Math.max(0, offset - 50);
  refresh().catch(() => {});
};
$("next").onclick = () => {
  offset += 50;
  refresh().catch(() => {});
};
for (const b of document.querySelectorAll("[data-close]"))
  b.onclick = () => $(b.dataset.close).close();
for (const b of document.querySelectorAll("[data-tab]"))
  b.onclick = () => {
    activeTab = b.dataset.tab;
    renderDetail();
  };
$("export").onclick = () => {
  if (!detail) return;
  const blob = new Blob([JSON.stringify(detail, null, 2)], {
    type: "application/json",
  });
  const url = URL.createObjectURL(blob);
  const a = el("a");
  a.href = url;
  a.download = "request-" + detail.record.id + ".json";
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
};
setInterval(() => {
  if ($("auto").checked && managementKey && !document.hidden)
    refresh().catch(() => {});
}, 5000);

// Storage events are emitted in this iframe when CPAMC edits its saved notes.
let panelStorageTimer;
window.addEventListener("storage", (event) => {
  if (
    event.key === "cli-proxy-auth" ||
    event.key?.startsWith("api-key-names:v1:")
  ) {
    clearTimeout(panelStorageTimer);
    panelStorageTimer = setTimeout(async () => {
      if (event.key === "cli-proxy-auth") {
        panelSnapshot = "";
        if (keyFromPanel || !managementKey) await connectFromPanel();
      } else if (managementKey) {
        await syncPanelAliases(true);
        await refresh().catch(() => {});
      }
    }, 200);
  }
});
connectFromPanel();
$("detail").addEventListener("close", clearDetailImages);
window.addEventListener("pagehide", clearDetailImages);
