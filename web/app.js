"use strict";
const $ = (id) => document.getElementById(id);
const API = "/v0/management/plugins/request-recorder";
let managementKey = "",
  keyFromPanel = false,
  authGeneration = 0,
  offset = 0,
  total = 0,
  detail = null,
  detailGeneration = 0,
  activeTab = "conversation",
  generation = 0,
  refreshRequests = 0,
  refreshTimer = 0,
  refreshSeconds = 1,
  lastRowsSignature = "",
  promptTargets = [],
  promptCursor = -1,
  promptScrollTop = -1,
  promptScrollFrame = 0;
const outcomeNames = {
  succeeded: "成功",
  failed: "失败",
  canceled: "已中断",
  rejected: "已拒绝",
  incomplete: "未完整记录",
};
const fmt = (n) => (n == null ? "—" : Number(n).toLocaleString("zh-CN"));
const duration = (n) =>
  n == null
    ? "—"
    : n >= 1000
      ? (n / 1000).toFixed(2) + " s"
      : n.toFixed(0) + " ms";
const fileSize = (n) =>
  n == null
    ? "—"
    : n >= 1073741824
      ? (n / 1073741824).toFixed(2) + " GiB"
      : n >= 1048576
        ? (n / 1048576).toFixed(2) + " MiB"
        : n >= 1024
          ? (n / 1024).toFixed(1) + " KiB"
          : n + " B";
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
let callerKeysById = new Map(),
  callerKeysFor = "",
  keyLoadPromise = null;
function renderKeyChoices() {
  $("caller-keys").replaceChildren(
    ...Array.from(callerKeysById.values(), (entry) => {
      const option = el("option");
      option.value = entry.key;
      option.label = entry.alias || "";
      return option;
    }),
  );
}
async function fetchCallerKeys(credential) {
  const options = {
    credentials: "omit",
    cache: "no-store",
    headers: { Authorization: "Bearer " + credential },
  };
  let response = await fetch("/v8/management/config/access/api-keys", options);
  if (response.status === 404 || response.status === 405)
    response = await fetch("/v0/management/api-keys", options);
  if (!response.ok)
    throw new Error("无法读取 CPA key 列表（HTTP " + response.status + "）");
  const data = await response.json(),
    keys = Array.isArray(data) ? data : data["api-keys"];
  if (
    !Array.isArray(keys) ||
    keys.length > 10000 ||
    keys.some((key) => typeof key !== "string")
  )
    throw new Error("CPA key 列表格式无法识别");
  const entries = new Map();
  for (const key of keys) entries.set(await sha256Hex(key), { key, alias: "" });
  if (credential === managementKey) {
    callerKeysById = entries;
    callerKeysFor = credential;
    lastRowsSignature = "";
    renderKeyChoices();
  }
  return keys;
}
async function loadCallerKeys(force = false) {
  if (!managementKey || (!force && callerKeysFor === managementKey)) return;
  if (keyLoadPromise) return keyLoadPromise;
  const credential = managementKey;
  keyLoadPromise = fetchCallerKeys(credential)
    .catch(() => {
      if (credential === managementKey) callerKeysFor = credential;
    })
    .finally(() => {
      keyLoadPromise = null;
    });
  return keyLoadPromise;
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
    const keys = await fetchCallerKeys(credential);
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
  callerKeysById.clear();
  callerKeysFor = "";
  $("caller-keys").replaceChildren();
  if ($("detail").open) $("detail").close();
  lastRowsSignature = "";
  $("rows").replaceChildren();
  $("detail-body").replaceChildren();
  $("detail-sub").replaceChildren();
  promptTargets = [];
  $("prompt-nav").hidden = true;
  $("empty").hidden = false;
  $("connection").textContent = "未连接";
  $("health").textContent = "等待连接";
  if ($("storage-view").open) $("storage-view").close();
  $("storage-body").replaceChildren();
  $("total").textContent = "—";
  document
    .querySelectorAll("#stats strong")
    .forEach((n) => (n.textContent = "—"));
}
async function filterQuery() {
  const q = new URLSearchParams();
  for (const [k, v] of new FormData($("filters")))
    if (v) {
      if (k === "key") q.set("key_id", await sha256Hex(v.trim()));
      else q.set(k, k === "from" || k === "to" ? new Date(v).toISOString() : v);
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
  refreshRequests++;
  try {
    await syncPanelAliases();
    await loadCallerKeys();
    const query = await filterQuery();
    const [page, status] = await Promise.all([
      api("/records?" + query),
      api("/status"),
    ]);
    if (seq !== generation) return;
    total = page.total;
    refreshSeconds = status.refresh_interval_seconds || 1;
    if (document.activeElement !== $("refresh-interval"))
      $("refresh-interval").value = refreshSeconds;
    if (!$("ignore-rejected").disabled)
      $("ignore-rejected").checked = Boolean(status.ignore_rejected);
    renderRows(page.items);
    const currentHead =
      detail && page.items.find((r) => r.id === detail.record.id);
    if (
      $("detail").open &&
      currentHead &&
      currentHead.request_id !== detail.record.request_id
    )
      await openDetail(currentHead.id, true);
    else if (currentHead) {
      Object.assign(detail.record, currentHead);
      renderDetailHeader();
    }
    $("total").textContent = fmt(total);
    $("connection").textContent = "● 已连接";
    $("connection").className = "";
    $("health").replaceChildren(
      el("div", "", "依据用户请求与 CPA 响应还原 · 优先保存完整结束快照"),
      el(
        "div",
        "",
        `采集中 ${status.active} · 丢弃 ${status.dropped_requests}` +
          (status.unmatched_usage ? ` · 用量待关联 ${status.unmatched_usage}` : ""),
      ),
    );
    if (status.database_file_bytes != null)
      $("health").append(
        el(
          "div",
          "",
          `数据库 ${fileSize(status.database_file_bytes)} · 空闲 / 待复用 ${fileSize(status.database_free_page_bytes)}`,
        ),
      );
    if (panelMessage) $("health").append(el("div", "", panelMessage));
    const warnings = [];
    if (status.database_maintenance_error)
      warnings.push("数据库整理：" + status.database_maintenance_error);
    if (status.capacity_evicted_records)
      warnings.push(
        `已因容量淘汰 ${fmt(status.capacity_evicted_records)} 条最旧记录。`,
      );
    if (status.last_error) warnings.push("采集错误：" + status.last_error);
    if (status.usage_warning) warnings.push("用量统计提示：" + status.usage_warning);
    if (status.dropped_requests)
      warnings.push(
        "部分请求因采集或会话容量限制未更新，请查看采集错误和容量设置。",
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
      $("empty").querySelector("h3").textContent = "暂无匹配的会话";
      $("empty").querySelector("p").textContent =
        "请求结束后更新对应会话；也可以调整筛选条件。";
    }
  } catch (e) {
    if (seq === generation) {
      notice(e.message);
      $("connection").textContent = "连接失败";
      throw e;
    }
  } finally {
    refreshRequests--;
  }
}
function renderRows(items) {
  const signature = JSON.stringify(items);
  if (signature === lastRowsSignature) return;
  lastRowsSignature = signature;
  $("rows").replaceChildren();
  for (const r of items) {
    const tr = el("tr");
    tr.tabIndex = 0;
    tr.setAttribute("aria-label", "查看会话 " + r.id);
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
      r.key_alias ||
        callerKeysById.get(r.key_id)?.key ||
        r.key_hint ||
        "未知请求者",
    );
    const knownKey = callerKeysById.get(r.key_id);
    if (knownKey) {
      knownKey.alias = r.key_alias || "";
      who.title = knownKey.key;
      if (r.key_alias) who.append(el("small", "mono", knownKey.key));
    }
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
    if (r.warnings?.length)
      state.append(el("small", "", `△ ${r.warnings.length} 条采集提示`));
    const prompt = el("td"),
      response = el("td");
    prompt.append(el("div", "row-preview", r.latest_prompt || "—"));
    response.append(
      el("div", "row-preview", r.latest_response || "点击条目查看模型最终结果"),
    );
    tr.append(time, who, model, state, latency, tokens, prompt, response);
    $("rows").append(tr);
  }
  renderKeyChoices();
}
function renderDetailHeader() {
  if (!detail) return;
  const r = detail.record,
    totals = r.conversation_totals || {
      duration_ms: r.duration_ms,
      total_tokens: r.tokens?.TotalTokens,
      duration_requests:
        r.duration_ms > 0 || (r.completed_at && !r.completed_at.startsWith("0001-"))
          ? 1
          : 0,
      token_requests: r.tokens ? 1 : 0,
    },
    turns = r.conversation_turns || 1;
  const metric = (value, count) =>
    count ? value + (count < turns ? "（部分）" : "") : "—";
  $("detail-title").textContent = r.response_model || r.model || "请求详情";
  $("detail-sub").textContent =
    `会话总耗时 ${metric(duration(totals.duration_ms), totals.duration_requests)} · 总消耗 token ${metric(fmt(totals.total_tokens), totals.token_requests)} · 最新 ${date(r.started_at)}`;
  $("detail-sub").title = "模型请求耗时累计，不含用户停顿。";
  if (totals.duration_requests < turns || totals.token_requests < turns)
    $("detail-sub").title +=
      `耗时已统计 ${totals.duration_requests}/${turns} 轮，token 已统计 ${totals.token_requests}/${turns} 轮；未记录的历史统计无法补回。`;
}
async function openDetail(id, preserveTab = false) {
  const authRevision = authGeneration,
    sequence = ++detailGeneration;
  try {
    const needsRaw =
      preserveTab && !["conversation", "metadata"].includes(activeTab);
    const data = await api(
      "/record?id=" + encodeURIComponent(id) + (needsRaw ? "" : "&view=chat"),
    );
    data.rawLoaded = needsRaw;
    if (
      authRevision !== authGeneration ||
      sequence !== detailGeneration ||
      !managementKey
    )
      return;
    const sameRecord = detail?.record.id === data.record.id;
    if (!sameRecord) clearDetailImages();
    detail = data;
    if (!preserveTab) activeTab = "conversation";
    renderDetailHeader();
    renderDetail(preserveTab && sameRecord);
    if (!$("detail").open) $("detail").showModal();
    promptScrollTop = -1;
    trackPromptPosition();
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
  $("image-download").onclick = null;
  $("image-download").disabled = false;
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
function archivedImage(image, recordID, variant = "preview") {
  const key = recordID + ":" + image.id + ":" + variant;
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
          encodeURIComponent(image.id) +
          "&variant=" +
          variant;
        let response;
        for (let attempt = 0; attempt < 10; attempt++) {
          response = await fetch(path, {
            signal,
            credentials: "omit",
            cache: "no-store",
            headers: { Authorization: "Bearer " + managementKey },
          });
          if (response.status !== 429 || attempt === 9) break;
          await response.body?.cancel();
          await new Promise((resolve) => setTimeout(resolve, 400));
          if (signal.aborted) throw new DOMException("Canceled", "AbortError");
        }
        if (!response.ok) {
          const error = await response.json().catch(() => ({}));
          throw new Error(
            error.error || "CPA 图片读取失败（HTTP " + response.status + "）",
          );
        }
        const blob = await response.blob();
        if (
          variant === "preview"
            ? blob.type !== "image/webp"
            : !/^image\/(png|jpeg|gif|webp|bmp|avif)$/.test(blob.type)
        )
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
      for (const m of detail.conversation || []) {
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
      if (changed) renderDetail(true);
      if (pending || state.pending) scheduleImagePoll();
    } catch (error) {
      notice("无法查询 CPA 图片归档状态：" + error.message);
    }
  }, 1000);
}
async function downloadOriginal(image, recordID, button) {
  const label = button.textContent,
    session = imageSession;
  button.disabled = true;
  button.textContent = "正在读取原图…";
  try {
    const original = await archivedImage(image, recordID, "original");
    if (session !== imageSession) return;
    const link = el("a");
    link.href = original.url;
    link.download =
      image.id +
      "." +
      (original.mime === "image/jpeg" ? "jpg" : original.mime.split("/")[1]);
    link.click();
  } catch (error) {
    if (error.name !== "AbortError") notice(error.message);
  } finally {
    if (session === imageSession) {
      button.disabled = false;
      button.textContent = label;
    }
  }
}
function imageView(image) {
  const box = el("div", "image-card"),
    recordID = detail.record.id;
  box.dataset.imageId = image.id || "";
  box.append(el("div", "image-caption", "WebP 压缩预览 · 原图保存在 CPA"));
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
          await api("/images/retry?record_id=" + encodeURIComponent(recordID), {
            method: "POST",
          });
          image.status = "pending";
          image.error = "";
          renderDetail(true);
          scheduleImagePoll();
        } catch (error) {
          notice(error.message);
        }
      };
      box.append(retry);
    }
    return box;
  }
  const img = el("img", "original-image"),
    status = el("p", "muted", "从 CPA 加载 WebP 预览…");
  img.alt = "WebP 图片预览";
  img.loading = "lazy";
  const download = el("button", "link", "下载原图");
  download.type = "button";
  download.onclick = () => downloadOriginal(image, recordID, download);
  box.append(img, status, download);
  const load = async () => {
    try {
      const saved = await archivedImage(image, recordID);
      if (!box.isConnected) return;
      img.onerror = () => {
        status.className = "error";
        status.textContent = "WebP 预览无法解码；仍可下载原图。";
      };
      img.onload = () => status.remove();
      img.src = saved.url;
      const open = () => {
        $("full-image").src = saved.url;
        $("image-download").hidden = false;
        $("image-download").onclick = () =>
          downloadOriginal(image, recordID, $("image-download"));
        $("image-view").showModal();
      };
      img.onclick = open;
      const zoom = el("button", "link", "放大 WebP 预览 ↗");
      zoom.type = "button";
      zoom.onclick = open;
      box.append(zoom);
    } catch (error) {
      if (error.name !== "AbortError" && box.isConnected) {
        status.className = "error";
        status.textContent = error.message;
        const retry = el("button", "link", "重试加载预览");
        retry.type = "button";
        retry.onclick = () => {
          retry.remove();
          status.textContent = "正在加载 WebP 预览…";
          load();
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
function updatePromptControls() {
  $("prompt-prev").disabled = promptCursor <= 0;
  $("prompt-next").disabled = promptCursor + 1 >= promptTargets.length;
  $("prompt-position").textContent = promptTargets.length
    ? `${promptCursor < 0 ? "—" : promptCursor + 1} / ${promptTargets.length}`
    : "无用户提示词";
  promptTargets.forEach((target, i) =>
    target.classList.toggle("prompt-current", i === promptCursor),
  );
}
function trackPromptPosition() {
  if ($("prompt-nav").hidden || !$("detail").open) return;
  const body = $("detail-body");
  if (Math.abs(body.scrollTop - promptScrollTop) < 1) return;
  promptScrollTop = body.scrollTop;
  const top = body.getBoundingClientRect().top + 24;
  promptCursor = promptTargets.findLastIndex(
    (target) => target.getBoundingClientRect().top <= top,
  );
  if (
    body.scrollHeight > body.clientHeight &&
    body.scrollTop + body.clientHeight >= body.scrollHeight - 1
  )
    promptCursor = promptTargets.length - 1;
  updatePromptControls();
}
function jumpPrompt(direction) {
  const index = promptCursor + direction,
    target = promptTargets[index];
  if (!target) return;
  const body = $("detail-body"),
    top =
      body.scrollTop + target.getBoundingClientRect().top -
      body.getBoundingClientRect().top - 16;
  body.scrollTo({ top, behavior: "auto" });
  target.focus({ preventScroll: true });
  promptCursor = index;
  promptScrollTop = body.scrollTop;
  updatePromptControls();
}
$("prompt-prev").onclick = () => jumpPrompt(-1);
$("prompt-next").onclick = () => jumpPrompt(1);
$("detail-body").onscroll = () => {
  if (promptScrollFrame) return;
  promptScrollFrame = requestAnimationFrame(() => {
    promptScrollFrame = 0;
    trackPromptPosition();
  });
};
function renderDetail(preserveScroll = false) {
  if (!detail) return;
  const r = detail.record,
    body = $("detail-body"),
    scrollTop = preserveScroll ? body.scrollTop : 0;
  promptTargets = [];
  $("prompt-nav").hidden = activeTab !== "conversation";
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
        "每个会话保留最新完整历史。原图通过 CPA 鉴权接口提供，浏览器不访问源站。",
      ),
    );
    if (!detail.conversation?.length)
      body.append(el("p", "muted", "无可还原的对话内容，请查看原文。"));
    let previousUser = false;
    for (const m of detail.conversation || []) {
      const node = bubble(m),
        user = m.role === "user" && !["tool_result", "notice"].includes(m.kind);
      if (user && !previousUser) {
        node.dataset.userPrompt = promptTargets.length;
        node.tabIndex = -1;
        promptTargets.push(node);
      }
      previousUser = user;
      body.append(node);
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
  } else if (activeTab === "response") {
    body.append(
      el(
        "div",
        "view-note",
        "来自 CPA 响应回调的模型最终结果；优先保存完整结束快照；缺少快照时使用完整输出项或分片兜底，无需开启请求日志。",
      ),
      section("模型最终结果（CPA 响应回调）", pretty(r.response_body)),
      section("响应头", JSON.stringify(r.response_headers, null, 2)),
    );
  } else {
    const meta = { ...r };
    for (const k of [
      "key_id",
      "caller_scope",
      "request_body",
      "conversation_history",
      "request_headers",
      "response_headers",
      "response_body",
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
      $("alias-key").value = callerKeysById.get(r.key_id)?.key || "";
      $("alias-dialog").showModal();
    };
    body.prepend(alias);
  }
  body.scrollTop = scrollTop;
  promptCursor = -1;
  promptScrollTop = -1;
  trackPromptPosition();
}
$("storage-open").onclick = async () => {
  if (!managementKey) {
    $("login").showModal();
    return;
  }
  const revision = authGeneration;
  const body = $("storage-body");
  body.replaceChildren(el("p", "muted", "正在统计存储占用…"));
  $("storage-view").showModal();
  try {
    const stats = await api("/storage");
    if (revision !== authGeneration) return;
    const list = el("dl", "meta-grid");
    for (const [label, value] of [
      ["记录数量", fmt(stats.records)],
      ["数据库文件", fileSize(stats.database_file_bytes)],
      ["空闲 / 待复用空间", fileSize(stats.database_free_page_bytes)],
      ["正文原始合计", fileSize(stats.body_logical_bytes)],
      ["去重压缩后正文", fileSize(stats.body_stored_bytes)],
      ["数据库上限", fileSize(stats.max_database_bytes)],
      ["因容量淘汰记录", fmt(stats.capacity_evicted_records)],
      ["自动整理", stats.database_maintenance_running ? "进行中" : "待命"],
    ])
      list.append(el("dt", "", label), el("dd", "mono", value));
    body.replaceChildren(
      list,
      el(
        "p",
        "",
        "只保存用户请求、模型最终结果和最新会话，保留文字、可见思考、工具调用和原图；不保存 SSE 中间事件。正文进一步去重压缩。",
      ),
      el(
        "p",
        "",
        "容量接近上限时自动淘汰最旧记录并整理空闲页。整理期间正常写入会在切换前重放；原图目录、CPA 源日志及整理临时文件不计入此上限。",
      ),
      el(
        "p",
        "",
        "文件增长与后台整理之间可能短暂超过上限；单条记录无法容纳时也会被淘汰。旧版本数据库不兼容，请停止 CPA 并移走旧库后启用新版。",
      ),
    );
  } catch (error) {
    if (revision === authGeneration)
      body.replaceChildren(el("p", "error", error.message));
  }
};
$("login-open").onclick = () => $("login").showModal();
$("refresh").onclick = async () => {
  await loadCallerKeys(true);
  await refresh().catch(() => {});
};
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
  const key = $("alias-key").value.trim();
  $("alias-key").value = "";
  if (!key && !$("alias-id").value) {
    $("alias-error").textContent = "请选择或填写 API key";
    return;
  }
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
  b.onclick = () => {
    if (b.dataset.close === "detail") detailGeneration++;
    $(b.dataset.close).close();
  };
async function ensureRawDetail() {
  const current = detail;
  if (!current || current.rawLoaded) return Boolean(current);
  const data = await api("/record?id=" + encodeURIComponent(current.record.id));
  if (detail !== current || !$("detail").open) return false;
  data.rawLoaded = true;
  detail = data;
  return true;
}
for (const b of document.querySelectorAll("[data-tab]"))
  b.onclick = async () => {
    activeTab = b.dataset.tab;
    try {
      if (
        !["conversation", "metadata"].includes(activeTab) &&
        !detail?.rawLoaded
      ) {
        $("detail-body").replaceChildren(
          el("p", "muted", "正在读取原文（包含原始图片字段）…"),
        );
        if (!(await ensureRawDetail())) return;
      }
      renderDetail();
    } catch (error) {
      notice(error.message);
    }
  };
function downloadFile(blob, filename) {
  const url = URL.createObjectURL(blob),
    a = el("a");
  a.href = url;
  a.download = filename;
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
$("export-markdown").onclick = async () => {
  if (!detail) return;
  const id = detail.record.id,
    revision = authGeneration,
    button = $("export-markdown");
  button.disabled = true;
  button.textContent = "正在导出…";
  try {
    const response = await fetch(
      API + "/export/markdown?id=" + encodeURIComponent(id),
      {
        headers: { Authorization: "Bearer " + managementKey },
        cache: "no-store",
        credentials: "omit",
      },
    );
    if (!response.ok) {
      const error = await response.json().catch(() => ({}));
      throw new Error(error.error || "Markdown 导出失败（" + response.status + "）");
    }
    const blob = await response.blob();
    if (revision !== authGeneration) return;
    downloadFile(blob, "conversation-" + id + ".md");
  } catch (error) {
    if (revision === authGeneration) notice(error.message);
  } finally {
    button.disabled = false;
    button.textContent = "↓ 导出 Markdown";
  }
};
$("export").onclick = async () => {
  try {
    if (!(await ensureRawDetail())) return;
    const { rawLoaded, ...content } = detail;
    const blob = new Blob([JSON.stringify(content, null, 2)], {
      type: "application/json",
    });
    downloadFile(blob, "conversation-" + detail.record.id + ".json");
  } catch (error) {
    notice(error.message);
  }
};
function scheduleRefresh() {
  clearTimeout(refreshTimer);
  if (!$("auto").checked) return;
  refreshTimer = setTimeout(async () => {
    if (managementKey && !document.hidden && refreshRequests === 0)
      await refresh().catch(() => {});
    scheduleRefresh();
  }, refreshSeconds * 1000);
}
$("auto").onchange = scheduleRefresh;
$("ignore-rejected").onchange = async () => {
  const checkbox = $("ignore-rejected"),
    checked = checkbox.checked,
    revision = authGeneration;
  if (!managementKey) {
    checkbox.checked = !checked;
    $("login").showModal();
    return;
  }
  checkbox.disabled = true;
  generation++;
  try {
    await api("/settings", {
      method: "POST",
      body: JSON.stringify({ ignore_rejected: checked }),
    });
    if (revision !== authGeneration) return;
    offset = 0;
    await refresh().catch(() => {});
  } catch (error) {
    if (revision === authGeneration) {
      checkbox.checked = !checked;
      notice(error.message);
    }
  } finally {
    checkbox.disabled = false;
  }
};
$("refresh-interval").onchange = async () => {
  const seconds = Number($("refresh-interval").value);
  if (!Number.isInteger(seconds) || seconds < 1 || seconds > 3600) {
    notice("刷新间隔需为 1 至 3600 秒");
    return;
  }
  if (!managementKey) {
    $("login").showModal();
    return;
  }
  try {
    await api("/settings", {
      method: "POST",
      body: JSON.stringify({ refresh_interval_seconds: seconds }),
    });
    refreshSeconds = seconds;
    scheduleRefresh();
  } catch (error) {
    notice(error.message);
  }
};
scheduleRefresh();

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
$("detail").addEventListener("close", () => {
  detailGeneration++;
  clearDetailImages();
});
window.addEventListener("pagehide", clearDetailImages);
