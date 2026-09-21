"use strict";

(() => {
  const API = "/fleet-admin/api/";
  const $ = (id) => document.getElementById(id);
  const ROOM_TERMINAL = new Set(["completed", "cancelled", "expired", "failed"]);
  const WORKER_TERMINAL = new Set(["stopped", "failed", "lost"]);
  const labels = {
    active: "进行中", assigned: "等待入场", pending: "等待分配", reserved: "已预留",
    requested: "已请求", allocating: "分配中", completed: "已完成", cancelled: "已取消",
    expired: "已过期", failed: "失败", stopped: "已退出", lost: "已失联",
    launching: "启动中", starting: "启动中", running: "运行中", ready: "已就绪",
    draining: "排空中", stopping: "退出中", closed: "已关闭", healthy: "正常",
    connected: "已连接", disconnected: "已断开", Running: "运行中", Pending: "等待调度",
    waiting_capacity: "等待容量", preparing: "准备中", prepared: "已准备", cancelling: "取消中",
    bootstrapping: "初始化中", suspect: "状态待确认", unknown: "未知",
    Succeeded: "已完成", Failed: "失败", Unknown: "未知"
  };
  const viewInfo = {
    overview: ["集群概览", "集群概览", "查看玩家承载、区域健康与实例容量。"],
    rooms: ["房间与玩家", "房间与玩家", "追踪活动房间、原席位恢复与历史对局。"],
    workers: ["游戏服实例", "游戏服实例", "按实例查看负载、容量和日志，按需停止接纳新房间。"],
    nodes: ["节点资源", "节点资源", "查看各区域 VPS 的可调度状态与资源使用。"],
    logs: ["日志检索", "日志检索", "查询实时容器或最近 7 天的已采集日志，退出实例仍可检索。"]
  };
  const DEFAULT_INTERVALS = { overview: 15, rooms: 5, workers: 15, nodes: 30, logs: 0, details: 15 };
  const ALLOWED_INTERVALS = new Set([0, 5, 15, 30, 60]);
  function loadIntervals() {
    const values = { ...DEFAULT_INTERVALS };
    try {
      const saved = JSON.parse(localStorage.getItem("fleet-console.refresh.v1") || "{}");
      for (const key of Object.keys(values)) if (ALLOWED_INTERVALS.has(saved[key])) values[key] = saved[key];
    } catch (_) { /* Refresh preferences are optional; sessions never use localStorage. */ }
    return values;
  }
  const state = {
    session: null, snapshot: null, goodFleet: null, goodFleetAt: null, goodRegions: new Map(),
    view: "overview", snapshotPromise: null, snapshotTargets: new Set(),
    logBusy: false, logController: null, logs: "", logQuery: "", detail: null,
    toastTimer: null, mutationBusy: false, sessionGeneration: 0,
    intervals: loadIntervals(), requestedAt: {}, renderedAt: {}, scroll: {},
    deferred: new Set(), pendingOptions: new Map(), renderArea: null,
    logDraftDirty: false, pendingLogs: null, snapshotError: null, logObservedAt: null
  };
  const buttonActions = new WeakMap();

  function node(tag, className, text) {
    const item = document.createElement(tag);
    if (className) item.className = className;
    if (text !== undefined && text !== null) item.textContent = String(text);
    return item;
  }
  function setText(element, value) { const next = String(value ?? ""); if (element.textContent !== next) element.textContent = next; }
  const list = (value) => Array.isArray(value) ? value : [];
  const number = (value) => typeof value === "number" && Number.isFinite(value);
  const count = (value) => number(value) ? value.toLocaleString("zh-CN", { maximumFractionDigits: 0 }) : "—";
  const decimal = (value, digits = 1) => number(value) ? value.toLocaleString("zh-CN", { maximumFractionDigits: digits }) : "—";
  const text = (value) => value === undefined || value === null || value === "" ? "—" : String(value);
  const shortID = (value) => value ? String(value).slice(0, 14) + (String(value).length > 14 ? "…" : "") : "—";
  const ms = (value) => number(value) ? decimal(value) + " ms" : "—";
  const cpu = (value) => number(value) ? decimal(value / 1000, 2) + " 核" : "—";
  const seconds = (value) => number(value) ? decimal(value) + " 秒" : "—";
  function bytes(value) {
    if (!number(value) || value < 0) return "—";
    if (value >= 1073741824) return decimal(value / 1073741824, 2) + " GiB";
    if (value >= 1048576) return decimal(value / 1048576) + " MiB";
    if (value >= 1024) return decimal(value / 1024) + " KiB";
    return count(value) + " B";
  }
  function time(value, compact = false) {
    if (value === null || value === undefined || value === "" || value === 0) return "—";
    const date = new Date(typeof value === "number" ? value * 1000 : value);
    if (!Number.isFinite(date.getTime())) return "—";
    return compact ? date.toLocaleTimeString("zh-CN", { hour12: false }) : date.toLocaleString("zh-CN", { hour12: false });
  }
  function remaining(value) {
    if (!number(value) || value <= 0) return "—";
    const left = Math.ceil(value - Date.now() / 1000);
    return left > 0 ? "剩余 " + count(left) + " 秒" : "已截止";
  }
  function sum(items, key) {
    if (items.some((item) => !number(item[key]))) return undefined;
    return items.reduce((total, item) => total + item[key], 0);
  }
  function badge(value, tone) {
    const good = new Set(["active", "running", "ready", "healthy", "Running", "connected"]);
    const bad = new Set(["failed", "lost", "Failed", "disconnected"]);
    const warning = new Set(["draining", "stopping", "launching", "starting", "pending", "Pending", "assigned", "allocating", "waiting_capacity", "preparing", "cancelling", "bootstrapping", "suspect"]);
    return node("span", "badge " + (tone || (good.has(value) ? "good" : bad.has(value) ? "danger" : warning.has(value) ? "warning" : "")), labels[value] || text(value));
  }
  function button(label, callback, className = "button small") {
    const item = node("button", className, label);
    item.type = "button";
    buttonActions.set(item, callback);
    item.addEventListener("click", dispatchButton);
    return item;
  }
  function dispatchButton(event) { buttonActions.get(event.currentTarget)?.(event); }
  function managementButton(label, callback) {
    const item = button(label, callback); item.dataset.managementAction = "true"; item.hidden = !canManage(); return item;
  }
  function keyed(item, value) { item.dataset.key = String(value); return item; }
  function hasSelection(container) {
    const selection = window.getSelection();
    return selection && !selection.isCollapsed && (container.contains(selection.anchorNode) || container.contains(selection.focusNode));
  }
  // Patch only generated data nodes. Forms, native selects and dialog shells are
  // permanent DOM nodes; editable focus and selected text wait until interaction ends.
  function patchNode(current, next) {
    if (current.nodeType !== next.nodeType || current.nodeName !== next.nodeName) { current.replaceWith(next); return next; }
    if (current.nodeType === Node.TEXT_NODE) { if (current.data !== next.data) current.data = next.data; return current; }
    for (const attr of [...current.attributes]) if (!next.hasAttribute(attr.name)) current.removeAttribute(attr.name);
    for (const attr of [...next.attributes]) if (current.getAttribute(attr.name) !== attr.value) current.setAttribute(attr.name, attr.value);
    if (buttonActions.has(next)) {
      if (!buttonActions.has(current)) current.addEventListener("click", dispatchButton);
      buttonActions.set(current, buttonActions.get(next));
    }
    patchChildren(current, [...next.childNodes]);
    return current;
  }
  function patchChildren(parent, children) {
    const available = [...parent.childNodes], used = new Set();
    let cursor = parent.firstChild;
    for (const next of children) {
      const key = next.nodeType === Node.ELEMENT_NODE ? next.dataset.key : undefined;
      const current = available.find((candidate) => !used.has(candidate) && (key !== undefined
        ? candidate.nodeType === Node.ELEMENT_NODE && candidate.dataset.key === key
        : candidate.nodeName === next.nodeName && !(candidate.nodeType === Node.ELEMENT_NODE && candidate.dataset.key !== undefined)));
      const atCursor = current === cursor;
      const target = current ? patchNode(current, next) : next;
      if (atCursor) cursor = target;
      if (current) used.add(current);
      if (target !== cursor) parent.insertBefore(target, cursor);
      cursor = target.nextSibling;
    }
    for (const old of available) if (!used.has(old) && old.parentNode === parent) old.remove();
  }
  function patchRegion(container, children) {
    const focused = document.activeElement;
    const editing = focused && container.contains(focused) && (focused.matches("input, textarea, select") || focused.isContentEditable);
    if (editing || hasSelection(container)) {
      if (state.renderArea) state.deferred.add(state.renderArea);
      return false;
    }
    const left = container.scrollLeft, top = container.scrollTop;
    patchChildren(container, children);
    container.scrollLeft = left; container.scrollTop = top;
    return true;
  }
  function empty(container, message, action) {
    const item = node("div", "empty-state"); item.append(node("p", "", message));
    if (action) item.append(action);
    patchRegion(container, [item]);
  }
  function options(id, values, placeholder) {
    const select = $(id);
    if (select === document.activeElement || id === "pod-options" && $("log-pod") === document.activeElement) {
      state.pendingOptions.set(id, [values, placeholder]); return;
    }
    state.pendingOptions.delete(id);
    const distinct = [...new Set(values.filter((value) => typeof value === "string" && value))].sort();
    const existing = new Set([...select.options].map((item) => item.value));
    if (placeholder && !existing.has("")) select.prepend(new Option(placeholder, ""));
    // Keep previously observed choices, even if the current snapshot no longer
    // contains them. A selected historical region/state must never silently reset.
    for (const value of distinct) if (!existing.has(value)) select.append(new Option(id === "room-state" ? labels[value] || value : value, value));
  }
  function cell(primary, secondary, className = "") {
    const item = node("div", className);
    item.append(primary instanceof Node ? primary : node("span", "", text(primary)));
    if (secondary) item.append(node("span", "secondary", secondary));
    return item;
  }
  function table(container, headers, rows, message, emptyAction) {
    if (!rows.length) { empty(container, message, emptyAction); return; }
    const tableNode = node("table");
    const head = node("thead"), heading = node("tr");
    for (const title of headers) { const th = node("th", "", title); th.scope = "col"; heading.append(th); }
    head.append(heading); tableNode.append(head);
    const body = node("tbody");
    for (const row of rows) {
      const tr = keyed(node("tr"), row.key);
      for (const value of row.cells) { const td = node("td"); td.append(value instanceof Node ? value : node("span", "nowrap", text(value))); tr.append(td); }
      body.append(tr);
    }
    tableNode.append(body); patchRegion(container, [tableNode]);
  }
  function toast(message, error = false) {
    clearTimeout(state.toastTimer);
    setText($("toast"), message);
    $("toast").classList.toggle("error", error);
    $("toast").hidden = false;
    state.toastTimer = setTimeout(() => { $("toast").hidden = true; }, error ? 8000 : 4500);
  }
  const errorMessages = {
    invalid_credentials: "账号或密码不正确。", invalid_login: "账号或密码不正确。",
    rate_limited: "请求过于频繁，请稍后重试。", login_rate_limited: "登录尝试过于频繁，请稍后重试。",
    unauthorized: "登录状态已失效，请重新登录。", unauthenticated: "控制台登录已失效，请重新登录。", session_expired: "登录已过期，请重新登录。",
    forbidden: "此操作被拒绝，当前登录仍有效。请检查此部署的操作权限。", invalid_csrf: "操作校验未通过，请重新登录以更新操作凭据。",
    management_disabled: "当前控制台为只读模式，未启用实例管理操作。",
    invalid_origin: "请求来源校验未通过。请从配置的 SSH 隧道地址访问控制台。",
    upstream_access_denied: "上游服务拒绝访问（RBAC 或服务凭据权限不足），控制台登录仍有效。",
    upstream_credential_unavailable: "服务端无法读取上游凭据。请管理员检查部署配置。",
    control_unavailable: "实例管理服务暂时不可用，状态与日志查询不受此权限影响。",
    control_access_denied: "实例管理服务拒绝此操作；请管理员检查受限操作授权。",
    control_invalid_response: "实例管理服务响应异常，请稍后重试。",
    control_credential_unavailable: "实例管理服务无法读取操作凭据，请管理员检查部署配置。",
    control_busy: "实例管理服务正忙，请稍后重试。",
    upstream_unavailable: "上游服务暂时无法连接，请稍后重试。",
    upstream_invalid_response: "上游数据格式异常，请管理员检查服务状态。",
    upstream_response_limit: "上游响应超过读取上限，请缩小查询范围。",
    resource_not_found: "未找到对应容器或记录。实例退出后请使用历史日志。",
    fleet_snapshot_unavailable: "Fleet 快照暂时不可用，保留上次成功数据。",
    fleet_snapshot_stale: "Fleet 快照已过期，当前显示为上次成功记录。",
    fleet_snapshot_invalid: "Fleet 快照格式异常，无法确认最新状态。",
    invalid_request: "请求参数不正确，请检查填写内容。", invalid_parameters: "请求参数不正确，请检查填写内容。",
    invalid_query: "查询参数不正确，请检查地区、Pod、容器及时间范围。",
    not_found: "未找到对应记录。实例退出后请尝试历史日志。",
    logs_unavailable: "日志源暂时不可用，请稍后重试。", metrics_unavailable: "指标采集暂时不可用。",
    loki_unavailable: "历史日志服务暂时不可用。", region_unavailable: "该地区暂时不可用。",
    timeout: "请求超时，请确认 SSH 隧道保持连接。", request_timeout: "请求超时，请稍后重试。",
    network_error: "连接失败，请确认 SSH 隧道保持连接。"
  };
  function errorText(error) {
    if (errorMessages[error.code]) return errorMessages[error.code];
    if (error.status === 401) return "登录状态已失效，请重新登录。";
    if (error.status === 403) return "此请求被权限策略拒绝，当前登录仍有效。请检查服务端操作授权。";
    if (error.status === 429) return "请求过于频繁，请稍后重试。";
    return error.status ? "请求失败（HTTP " + error.status + "），请稍后重试。" : "连接失败，请确认 SSH 隧道保持连接。";
  }
  async function api(path, { method = "GET", body, signal, allowUnauthorized = false } = {}) {
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 20000);
    const onAbort = () => controller.abort();
    if (signal) { if (signal.aborted) controller.abort(); else signal.addEventListener("abort", onAbort, { once: true }); }
    try {
      const headers = { Accept: "application/json" };
      if (body !== undefined) headers["Content-Type"] = "application/json";
      if (method !== "GET" && state.session && state.session.csrf_token) headers["X-CSRF-Token"] = state.session.csrf_token;
      const response = await fetch(API + path, { method, credentials: "same-origin", cache: "no-store", redirect: "error", headers, body: body === undefined ? undefined : JSON.stringify(body), signal: controller.signal });
      let data = null;
      if (response.status !== 204) { try { data = await response.json(); } catch (_) { /* A proxy may return a non-JSON error. */ } }
      if (!response.ok) {
        const error = new Error("request_failed");
        error.status = response.status;
        error.code = typeof data?.error === "string" ? data.error : typeof data?.error?.code === "string" ? data.error.code : data?.code;
        if (response.status === 401 && !allowUnauthorized && state.session) showLogin("登录已过期，请重新登录。");
        throw error;
      }
      return data;
    } catch (error) {
      if (controller.signal.aborted) {
        const timeoutError = new Error("request_timeout");
        timeoutError.code = "timeout";
        throw timeoutError;
      }
      throw error;
    } finally {
      clearTimeout(timeout);
      if (signal) signal.removeEventListener("abort", onAbort);
    }
  }
  function showLogin(message = "通过 SSH 隧道访问。请使用独立管理员账号登录。") {
    state.session = null; state.snapshot = null; state.goodFleet = null; state.goodFleetAt = null;
    state.goodRegions.clear(); state.logs = ""; state.logQuery = ""; state.logObservedAt = null;
    state.sessionGeneration += 1;
    state.logController?.abort(); state.snapshotController?.abort(); state.snapshotTargets.clear();
    state.snapshotPromise = null; state.requestedAt = {}; state.renderedAt = {}; state.deferred.clear();
    state.snapshotError = null; state.pendingLogs = null; state.logDraftDirty = false;
    $("app").hidden = true; $("login-view").hidden = false; setText($("login-state"), message);
    setText($("log-output"), "尚未查询日志。"); $("password").value = "";
    for (const id of ["detail-dialog", "confirm-dialog"]) if ($(id).open) $(id).close("cancel");
    state.detail = null; renderManagementAccess();
  }
  function showSession(session) {
    if (!session || typeof session.username !== "string" || typeof session.csrf_token !== "string" || !session.csrf_token) throw new Error("invalid_session_response");
    state.session = session; state.sessionGeneration += 1;
    setText($("session-user"), session.username);
    $("login-view").hidden = true; $("app").hidden = false; $("password").value = ""; $("login-error").hidden = true;
    const hash = location.hash.slice(1) === "infrastructure" ? "workers" : location.hash.slice(1);
    setView(viewInfo[hash] ? hash : "overview", false);
  }
  function saveIntervals() {
    try { localStorage.setItem("fleet-console.refresh.v1", JSON.stringify(state.intervals)); } catch (_) { /* Optional browser preference. */ }
  }
  function preserveScroll(action) {
    const x = window.scrollX, y = window.scrollY;
    const positions = [...document.querySelectorAll(".table-wrap, .detail-dialog, .dialog-body")].map((element) => [element, element.scrollLeft, element.scrollTop]);
    action();
    for (const [element, left, top] of positions) if (element.isConnected) { element.scrollLeft = left; element.scrollTop = top; }
    if (window.scrollX !== x || window.scrollY !== y) window.scrollTo(x, y);
  }
  function setView(view, updateHash = true) {
    if (view === "infrastructure") view = "workers";
    if (!viewInfo[view]) return;
    const previous = state.view, changed = previous !== view;
    if (changed) state.scroll[previous] = [window.scrollX, window.scrollY];
    state.view = view;
    for (const name of Object.keys(viewInfo)) $("view-" + name).hidden = name !== view;
    for (const item of document.querySelectorAll(".nav-item")) {
      const selected = item.dataset.view === view;
      item.classList.toggle("active", selected);
      if (selected) item.setAttribute("aria-current", "page"); else item.removeAttribute("aria-current");
    }
    setText($("page-name"), viewInfo[view][0]); setText($("page-title"), viewInfo[view][1]); setText($("page-description"), viewInfo[view][2]);
    $("page-refresh").value = String(state.intervals[view]);
    setText($("refresh-button"), view === "logs" ? "刷新查询结果" : "刷新当前页");
    if (updateHash) history.replaceState(null, "", "#" + view);
    renderCurrentView();
    if (changed) { const position = state.scroll[view] || [0, 0]; window.scrollTo(position[0], position[1]); }
    if (state.session) {
      if (!state.snapshot) requestSnapshot([view]);
      else if (view !== "logs" && !state.renderedAt[view]) requestSnapshot([view]);
    }
  }
  const fleet = () => state.snapshot?.fleet;
  const canManage = () => !state.snapshotError && fleet()?.ok === true && fleet()?.can_manage === true;
  const workers = () => list(state.goodFleet?.workers);
  const rooms = () => list(state.goodFleet?.rooms);
  const roomKey = (room) => room.allocation_id || room.id;
  const regions = () => list(state.snapshot?.regions).map((region) => region.ok === true || !state.goodRegions.has(region.name)
    ? region : { ...state.goodRegions.get(region.name), ...region, nodes: state.goodRegions.get(region.name).nodes, pods: state.goodRegions.get(region.name).pods, stale: true });
  function podFor(worker) {
    const region = regions().find((item) => item.name === worker.region);
    return list(region?.pods).find((item) => item.name === worker.pod || item.worker_id === worker.id);
  }
  const canDrainWorker = (worker) => canManage() && !!worker && !worker.draining && ["ready", "suspect"].includes(worker.state);
  function workerStatus(worker) { return worker.draining && !WORKER_TERMINAL.has(worker.state) ? "draining" : worker.state; }
  function identifierButton(value, callback) {
    const item = button(shortID(value), callback, "id-button identifier"); item.title = text(value); return item;
  }
  function updateViewOptions(view) {
    const names = [...regions().map((item) => item.name), ...workers().map((item) => item.region)];
    if (view === "rooms") { options("room-region", names, "全部地区"); options("room-state", rooms().map((item) => item.state), "全部状态"); }
    if (view === "workers") options("worker-region", names, "全部地区");
    if (view === "nodes") options("node-region", names, "全部地区");
    if (view === "logs") {
      options("log-region", names.length ? names : ["us-west"]);
      const podNames = [...workers().map((worker) => worker.pod), ...regions().flatMap((region) => list(region.pods).map((pod) => pod.name))];
      options("pod-options", podNames);
    }
  }
  function renderCurrentView() {
    const view = state.view;
    state.renderArea = view; state.deferred.delete(view);
    preserveScroll(() => {
      updateViewOptions(view);
      if (view === "overview") renderOverview();
      else if (view === "rooms") renderRooms();
      else if (view === "workers") renderWorkers();
      else if (view === "nodes") renderNodes();
    });
    state.renderArea = null;
    if (view !== "logs" && state.snapshot && !state.deferred.has(view)) state.renderedAt[view] = state.snapshot.observed_at;
    renderStatus();
  }
  async function requestSnapshot(targets = [state.view]) {
    if (!state.session) return;
    for (const target of targets) { state.snapshotTargets.add(target); state.requestedAt[target] = Date.now(); }
    if (state.snapshotPromise) return state.snapshotPromise;
    const generation = state.sessionGeneration, controller = new AbortController();
    state.snapshotController = controller;
    const task = (async () => {
      try {
        const snapshot = await api("snapshot", { signal: controller.signal });
        if (!state.session || generation !== state.sessionGeneration) return;
        if (!snapshot || typeof snapshot !== "object" || !snapshot.fleet) { const error = new Error("invalid_snapshot_response"); error.code = "upstream_invalid_response"; throw error; }
        state.snapshot = snapshot; state.snapshotError = null;
        if (snapshot.fleet.ok === true) { state.goodFleet = snapshot.fleet; state.goodFleetAt = snapshot.observed_at; }
        for (const region of list(snapshot.regions)) if (region.ok === true) state.goodRegions.set(region.name, region);
        const requested = new Set(state.snapshotTargets); state.snapshotTargets.clear();
        renderManagementAccess();
        if (requested.has(state.view)) renderCurrentView();
        if (requested.has("details") && state.detail && $("detail-dialog").open) renderDetail();
        renderStatus();
      } catch (error) {
        if (state.session && generation === state.sessionGeneration && !controller.signal.aborted) { state.snapshotError = error; renderManagementAccess(); renderStatus(); }
      } finally {
        if (state.snapshotController === controller) { state.snapshotPromise = null; state.snapshotTargets.clear(); }
      }
    })();
    state.snapshotPromise = task;
    return task;
  }
  function renderStatus() {
    const view = state.view;
    $("connection-banner").hidden = !state.snapshotError;
    if (state.snapshotError) setText($("connection-banner"), errorText(state.snapshotError) + (state.goodFleet || state.goodRegions.size ? " 已保留上次成功数据，请勿视为最新状态。" : " 尚未取得运行数据。"));
    const problems = [];
    if (["overview", "rooms", "workers"].includes(view) && fleet()?.ok === false) problems.push("Fleet 状态：" + errorText({ code: fleet().error }) + (state.goodFleet ? " 当前显示 " + time(state.goodFleetAt) + " 的成功记录。" : " 尚无可用记录，未将错误显示为零。"));
    if (["overview", "workers", "nodes"].includes(view)) for (const region of regions()) {
      if (!region.ok) problems.push(text(region.name) + "：" + errorText({ code: region.error }) + (region.stale ? " 节点 / Pod 保留上次成功记录。" : ""));
      else if (!region.metrics_ok) problems.push(text(region.name) + "：资源指标暂不可用；状态数据仍可查看，缺失数值显示为 —。");
    }
    $("source-banner").hidden = problems.length === 0; setText($("source-banner"), problems.slice(0, 3).join(" "));
    const stale = !!state.snapshotError || problems.length > 0;
    $("refresh-indicator").className = "status-dot" + (stale ? " stale" : "");
    const at = view === "logs" ? state.logObservedAt : state.renderedAt[view];
    setText($("snapshot-time"), at ? "显示于 " + time(at, true) + (stale && view !== "logs" ? " · 含过时 / 缺失数据" : "") : "尚未取得本页数据");
    const interval = state.intervals[view];
    setText($("refresh-policy"), state.deferred.has(view) ? "正在操作或选择列表内容，局部更新暂缓" : view === "logs" && state.logDraftDirty ? "查询条件尚未应用，日志自动刷新暂停" : interval === 0 ? "自动刷新已关闭 · 可手动刷新当前页" : "本页每 " + interval + " 秒更新 · 筛选与滚动位置保留");
  }
  function renderManagementAccess() {
    const enabled = canManage(), managementError = fleet()?.management_error;
    setText($("access-status"), enabled ? "受限管理" : managementError ? "管理不可用" : "只读观测");
    $("access-status").className = "badge " + (enabled ? "good" : managementError ? "warning" : "");
    $("read-only-banner").hidden = enabled;
    setText($("read-only-banner"), managementError ? "实例管理暂不可用：" + errorText({ code: managementError }) + " 房间、玩家与日志仍可查看。" : fleet()?.ok === false || state.snapshotError ? "Fleet 最新状态不可用，管理入口暂时关闭。页面保留上次成功记录，仍可查询日志；此问题不代表控制台登录失效。" : "只读观测：可以查看房间、玩家、资源指标与日志。此部署尚未启用实例排空或创建重试；不影响匹配与对局。");
    $("retry-creation-button").hidden = !enabled;
    setText($("worker-management-note"), "CPU 按实例统计，无法单独测量每个房间。" + (enabled ? "排空仅停止接纳新房间，现有对局正常结束后退出；不支持强退单个房间。" : "当前可查看实例状态与日志，实例管理尚不可用。"));
    if (!enabled && $("confirm-dialog").open) $("confirm-dialog").close("cancel");
    // Capability changes apply immediately, including a hidden page's old buttons.
    for (const item of document.querySelectorAll("[data-management-action]")) item.hidden = !enabled;
  }
  function metric(label, value, unit, note) {
    const item = keyed(node("article", "metric-card"), label), title = node("div", "metric-label", label), amount = node("div", "metric-value", count(value));
    if (unit) amount.append(node("span", "metric-unit", unit));
    item.append(title, amount, node("p", "metric-note", note)); return item;
  }
  function healthItem(title, description, tone = "") {
    const item = keyed(node("div", "health-item"), title), dot = node("span", "status-dot " + tone), content = node("div");
    dot.setAttribute("aria-hidden", "true"); content.append(node("strong", "", title), node("p", "", description)); item.append(dot, content); return item;
  }
  function renderOverview() {
    const ok = !!state.goodFleet, live = workers().filter((worker) => !WORKER_TERMINAL.has(worker.state)), liveRooms = rooms().filter((room) => !ROOM_TERMINAL.has(room.state));
    patchRegion($("overview-metrics"), [
      metric("在线玩家", ok ? sum(live, "player_count") : undefined, "人", "来自游戏服最近一次心跳"),
      metric("活跃房间", ok ? liveRooms.length : undefined, "间", "包含等待分配与等待入场"),
      metric("运行实例", ok ? live.length : undefined, "个", "已就绪 " + (ok ? count(live.filter((worker) => worker.ready && !worker.draining).length) : "—") + " · 上限 " + count(fleet()?.max_processes)),
      metric("实例已占用房间", ok ? sum(live, "occupied_rooms") : undefined, "/ " + (ok ? count(sum(live, "max_rooms")) : "—"), "占用包含已预留房间，受心跳刷新影响")
    ]);
    const blocked = typeof fleet()?.creation_blocked_reason === "string" && fleet().creation_blocked_reason !== "";
    $("creation-banner").hidden = !blocked; setText($("creation-reason"), blocked ? fleet().creation_blocked_reason : "");
    const regionList = $("region-overview");
    if (!regions().length) empty(regionList, "尚无区域数据。");
    else patchRegion(regionList, regions().map((region) => {
      const items = live.filter((worker) => worker.region === region.name), localRooms = liveRooms.filter((room) => room.region === region.name);
      const row = keyed(node("div", "region-row"), region.name), name = node("div", "region-name"), dot = node("span", "status-dot " + (region.ok ? "" : "failed")), numbers = node("div", "region-numbers");
      dot.setAttribute("aria-hidden", "true"); name.append(dot, node("strong", "", text(region.name)));
      for (const [label, value] of [["玩家", ok ? sum(items, "player_count") : undefined], ["房间", ok ? localRooms.length : undefined], ["实例", ok ? items.length : undefined]]) { const item = node("span", "", label); item.prepend(node("strong", "", count(value))); numbers.append(item); }
      row.append(name, numbers); return row;
    }));
    const fresh = fleet()?.ok === true;
    const notes = [healthItem(fresh ? "Fleet 状态读取正常" : "Fleet 状态未就绪", fresh ? "快照版本 " + text(fleet().revision) + "。单实例配置上限 " + count(fleet().rooms_per_process) + " 间房。" : state.snapshot ? errorText({code:fleet()?.error}) : "正在读取首个运行快照…", fresh || !state.snapshot ? "" : "failed")];
    for (const region of regions()) {
      if (!region.ok) notes.push(healthItem(region.name + "：集群读取失败", errorText({code:region.error}), "failed"));
      else if (!region.metrics_ok) notes.push(healthItem(region.name + "：资源指标未就绪", "节点与 Pod 可见，CPU / 内存采集暂时不可用。", "stale"));
      else notes.push(healthItem(region.name + "：资源指标可用", "节点 " + count(list(region.nodes).length) + " 个，Pod " + count(list(region.pods).length) + " 个。"));
      const warningEvents = list(region.events).filter((event) => event.type === "Warning").slice(0, 3);
      for (const event of warningEvents) notes.push(healthItem(text(event.reason) + " · " + text(event.object_name), text(event.message) + " · " + time(event.time), "stale"));
    }
    notes.push(healthItem("历史日志保留 " + count(state.snapshot?.log_retention_days) + " 天", "仅包含启用采集后的记录。房间状态列表并非同期限的历史档案。"));
    patchRegion($("health-notes"), notes);
    if (!ok) empty($("active-workers"), "无法读取 Fleet 实例状态。");
    else if (!live.length) empty($("active-workers"), "当前没有运行实例。有玩家匹配后将按容量策略创建。");
    else patchRegion($("active-workers"), live.slice(0, 8).map(workerCard));
  }
  function workerCard(worker) {
    const pod = podFor(worker), metrics = worker.metrics || {}, card = keyed(node("article", "worker-card"), worker.id), top = node("div", "worker-card-top"), title = node("h3", "", text(worker.pod || worker.id));
    title.title = text(worker.id); top.append(title, badge(workerStatus(worker)));
    card.append(top, node("p", "worker-card-region", text(worker.region) + " · " + text(pod?.node)));
    const details = node("dl", "worker-card-metrics");
    for (const [label, value] of [["房间 / 容量", count(worker.occupied_rooms) + " / " + count(worker.max_rooms)], ["CPU", cpu(pod?.cpu_millicores)], ["帧间隔 P99", ms(metrics.frame_p99_ms)]]) { const pair = node("div"); pair.append(node("dt", "", label), node("dd", "", value)); details.append(pair); }
    const actions = node("div", "worker-card-actions"); actions.append(button("查看实例", () => openDetail("worker", worker.id)), button("运行日志", () => openLogs(worker), "button small quiet"));
    card.append(details, actions); return card;
  }
  function renderRooms() {
    const query = $("room-search").value.trim().toLowerCase(), region = $("room-region").value, status = $("room-state").value, scope = $("room-scope").value;
    const filtered = rooms().filter((room) => (scope === "all" || ROOM_TERMINAL.has(room.state) === (scope === "history")) && (!region || room.region === region) && (!status || room.state === status) && (!query || [room.id, room.allocation_id, room.worker_id, ...list(room.players).map((player) => player.user_id)].some((value) => String(value || "").toLowerCase().includes(query))));
    setText($("room-count"), state.goodFleet ? count(filtered.length) + " 间" + (fleet()?.ok === true ? "" : " · 上次记录") : "尚无可用数据");
    const historyAction = scope === "active" && rooms().some((room) => ROOM_TERMINAL.has(room.state)) ? button("查看历史房间", () => { $("room-scope").value = "history"; $("room-scope").focus(); renderCurrentView(); }, "button small quiet") : null;
    table($("rooms-table"), ["房间 / 分配", "状态", "地区", "玩家 / 席位", "所属实例", "创建时间", "操作"], filtered.map((room) => {
      const players = list(room.players), historical = ROOM_TERMINAL.has(room.state), connected = players.every((player) => typeof player.connected === "boolean") ? players.filter((player) => player.connected).length : undefined;
      return { key: roomKey(room), cells: [cell(identifierButton(room.id || room.allocation_id, () => openDetail("room", roomKey(room))), "分配 " + shortID(room.allocation_id)), badge(room.state), text(room.region), historical ? cell(count(players.length) + " 个席位", "历史快照 · 非当前在线") : cell(count(connected) + " / " + count(players.length), "当前连接 / 预留席位"), identifierButton(room.worker_id, () => openDetail("worker", room.worker_id)), time(room.created_at), button("房间详情", () => openDetail("room", roomKey(room)), "button small quiet")] };
    }), state.goodFleet ? scope === "active" && !query && !region && !status ? "当前没有活动房间。可切换到历史房间查看已结束对局及日志。" : "没有符合当前范围和筛选条件的房间。" : "房间数据暂不可用。请查看上方数据来源提示；不会将权限错误显示为空房间。", historyAction);
  }
  function renderWorkers() {
    const query = $("worker-search").value.trim().toLowerCase(), region = $("worker-region").value, include = $("include-stopped").checked;
    const filtered = workers().filter((worker) => (include || !WORKER_TERMINAL.has(worker.state)) && (!region || worker.region === region) && (!query || [worker.id, worker.pod, podFor(worker)?.node].some((value) => String(value || "").toLowerCase().includes(query))));
    setText($("worker-count"), state.goodFleet ? count(filtered.length) + " 个" + (fleet()?.ok === true ? "" : " · 上次记录") : "尚无可用数据");
    table($("workers-table"), ["实例 / 节点", "状态", "地区", "房间 / 容量", "玩家", "Pod CPU / 内存", "帧间隔 P99", "模拟运行 / 排队", "操作"], filtered.map((worker) => {
      const pod = podFor(worker), metrics = worker.metrics || {}, actions = node("div", "cell-actions"), terminal = WORKER_TERMINAL.has(worker.state);
      actions.append(button("日志", () => openLogs(worker), "button small quiet"));
      if (canDrainWorker(worker)) actions.append(managementButton("排空", () => drainWorker(worker)));
      return { key: worker.id, cells: [cell(identifierButton(worker.pod || worker.id, () => openDetail("worker", worker.id)), text(pod?.node)), badge(workerStatus(worker)), text(worker.region), count(worker.occupied_rooms) + " / " + count(worker.max_rooms), terminal ? cell("已退出", "末次 " + count(worker.player_count) + " 人") : count(worker.player_count), cell(cpu(pod?.cpu_millicores), bytes(pod?.memory_bytes)), ms(metrics.frame_p99_ms), count(metrics.simulation_active) + " / " + count(metrics.simulation_pending), actions] };
    }), state.goodFleet ? "没有符合筛选条件的实例。勾选“包括已退出实例”可查询历史日志。" : "Fleet 实例数据暂时不可用，未将缺失数据视为零。");
  }
  function renderNodes() {
    const query = $("node-search").value.trim().toLowerCase(), filterRegion = $("node-region").value, status = $("node-state").value;
    const nodes = regions().flatMap((region) => list(region.nodes).map((item) => ({ ...item, region: region.name, stale: region.stale })));
    const filtered = nodes.filter((item) => (!filterRegion || item.region === filterRegion) && (!query || [item.name, item.role].some((value) => String(value || "").toLowerCase().includes(query))) && (!status || (status === "unready" ? !item.ready : status === "unschedulable" ? item.ready && item.unschedulable : item.ready && !item.unschedulable)));
    setText($("node-count"), regions().some((region) => region.ok || region.stale) ? count(filtered.length) + " 个" : "尚无可用数据");
    table($("nodes-table"), ["节点 / 地区", "调度状态", "角色", "CPU 使用 / 可分配", "内存使用 / 可分配", "快照中的 Pod"], filtered.map((item) => {
      const region = regions().find((region) => region.name === item.region);
      const status = item.ready !== true ? badge("未就绪", "danger") : item.unschedulable ? badge("已暂停调度", "warning") : badge("可调度", "good");
      const podCount = region ? list(region.pods).filter((pod) => pod.node === item.name).length : undefined;
      return { key: item.region + "/" + item.name, cells: [cell(text(item.name), text(item.region) + (item.stale ? " · 上次记录" : "")), status, text(item.role), cell(cpu(item.cpu_millicores), "可分配 " + cpu(item.cpu_allocatable_millicores)), cell(bytes(item.memory_bytes), "可分配 " + bytes(item.memory_allocatable_bytes)), count(podCount)] };
    }), "暂无符合筛选的节点记录；集群读取失败时会保留上次成功快照。");
  }
  function detailGrid(pairs) {
    const grid = keyed(node("dl", "detail-grid"), "fields");
    for (const [label, value] of pairs) { const item = keyed(node("div"), label); item.append(node("dt", "", label), node("dd", "", text(value))); grid.append(item); }
    return grid;
  }
  function openDetail(kind, id) {
    if (!id) { toast("此记录尚未关联实例。", true); return; }
    const changed = state.detail?.kind !== kind || state.detail?.id !== id;
    if (changed) {
      // Explicit navigation replaces the selected record immediately. Polling never moves focus.
      if (hasSelection($("detail-body"))) window.getSelection().removeAllRanges();
      if ($("detail-dialog").open) $("detail-close").focus({ preventScroll: true });
    }
    state.detail = { kind, id }; state.requestedAt.details = Date.now();
    $("detail-refresh").value = String(state.intervals.details);
    renderDetail();
    if (!$("detail-dialog").open) $("detail-dialog").showModal();
    if (changed) $("detail-dialog").scrollTop = 0;
  }
  function renderDetail() {
    const detail = state.detail, body = $("detail-body");
    if (!detail) return;
    state.renderArea = "details"; state.deferred.delete("details");
    preserveScroll(() => {
      const content = [];
      if (detail.kind === "room") {
        const room = rooms().find((item) => roomKey(item) === detail.id || item.id === detail.id);
        setText($("detail-eyebrow"), "房间详情"); setText($("detail-title"), text(room?.id || detail.id));
        if (!room) { empty(body, "该房间已不在当前 Fleet 状态保留窗口内，所属实例的历史日志仍可按 Pod 名查询。"); return; }
        const historical = ROOM_TERMINAL.has(room.state);
        content.push(detailGrid([["状态", labels[room.state] || room.state], ["地区", room.region], ["分配 ID", room.allocation_id], ["所属实例", room.worker_id], ["房间 Epoch", room.epoch], ["创建时间", time(room.created_at)], ["入场预约截止", time(room.expires_at)], ["终态时间", time(room.terminal_at)], ["错误代码", room.error]]));
        content.push(keyed(node("h3", "detail-section", historical ? "历史席位快照 · 非当前在线" : "玩家 · Nakama 用户 ID"), "players-heading"));
        const tableWrap = keyed(node("div", "table-wrap"), "players");
        table(tableWrap, ["玩家 ID", "座位", historical ? "最后连接记录" : "当前连接", "曾经入场", historical ? "历史恢复截止" : "恢复截止"], list(room.players).map((player) => {
          const connected = historical ? player.connected === true ? "最后已连接" : player.connected === false ? "最后未连接" : "—" : player.connected === true ? badge("connected") : player.connected === false ? badge(player.ever_connected ? "等待恢复" : "等待入场", "warning") : "—";
          return { key: text(player.user_id) + "/" + text(player.seat), cells: [node("span", "identifier detail-player-id", text(player.user_id)), count(player.seat), connected, player.ever_connected === true ? "是" : player.ever_connected === false ? "否" : "—", cell(time(player.reconnect_until), historical ? "历史记录" : player.connected ? "当前已连接" : remaining(player.reconnect_until))] };
        }), "此房间暂无玩家记录。");
        content.push(tableWrap, keyed(node("p", "table-note", historical ? "此对局已结束；以上为席位末次记录，不代表当前仍有连接。座位按协议从 0 编号。" : "连接状态来自游戏服心跳；座位按协议从 0 编号。进入对局后，入场预约截止不表示对局结束时间。房间不提供独立 CPU 指标。"), "player-note"));
        const worker = workers().find((item) => item.id === room.worker_id);
        if (worker) { const actions = keyed(node("div", "detail-actions"), "actions"); actions.append(button("查看所属实例", () => openDetail("worker", worker.id)), button("查看实例日志", () => openLogs(worker))); content.push(actions); }
      } else {
        const worker = workers().find((item) => item.id === detail.id);
        setText($("detail-eyebrow"), "游戏服实例"); setText($("detail-title"), text(worker?.pod || detail.id));
        if (!worker) { empty(body, "当前 Fleet 状态中没有此实例。若已退出，可在日志页手填 Pod 名查询归档。"); return; }
        const pod = podFor(worker), metrics = worker.metrics || {}, terminal = WORKER_TERMINAL.has(worker.state);
        content.push(detailGrid([["实例 ID", worker.id], ["状态", labels[workerStatus(worker)] || workerStatus(worker)], ["地区", worker.region], ["Pod / 命名空间", text(worker.pod) + " / " + text(pod?.namespace)], ["节点", pod?.node], ["联机版本", worker.build_hash], ["地址", worker.host ? text(worker.host) + ":" + text(worker.port) : "—"], ["房间 / 容量", count(worker.occupied_rooms) + " / " + count(worker.max_rooms)], [terminal ? "末次在线玩家" : "在线玩家", count(worker.player_count)], ["Pod CPU", cpu(pod?.cpu_millicores)], ["Pod 内存", bytes(pod?.memory_bytes)], ["游戏进程内存", bytes(metrics.memory_bytes)], ["帧间隔 P99", ms(metrics.frame_p99_ms)], ["模拟运行 / 排队", count(metrics.simulation_active) + " / " + count(metrics.simulation_pending)], ["最老模拟排队", seconds(metrics.simulation_oldest_seconds)], ["审计运行 / 排队", count(metrics.audit_active) + " / " + count(metrics.audit_pending)], ["待写回结果", count(metrics.pending_results)], ["容器重启次数", count(pod?.restarts)], ["创建时间", time(worker.created_at)], ["最近心跳", time(worker.last_heartbeat)], ["错误 / Pod 原因", worker.error || pod?.reason]]));
        const actions = keyed(node("div", "detail-actions"), "actions");
        actions.append(button("游戏服日志", () => openLogs(worker, "game")), button("Agones sidecar 日志", () => openLogs(worker, "agones-gameserver-sidecar")));
        if (canDrainWorker(worker)) actions.append(managementButton("排空实例", () => drainWorker(worker)));
        content.push(actions);
      }
      patchRegion(body, content);
    });
    state.renderArea = null;
    const stale = fleet()?.ok !== true || !!state.snapshotError;
    setText($("detail-status"), state.deferred.has("details") ? "正在选择或操作详情，局部更新暂缓" : (stale ? "上次成功记录 · " : "快照 · ") + time(state.goodFleetAt, true));
    if (!state.deferred.has("details")) state.renderedAt.details = state.goodFleetAt;
  }
  function confirmAction(title, description, target, label) {
    const dialog = $("confirm-dialog");
    if (dialog.open) return Promise.resolve(false);
    setText($("confirm-title"), title); setText($("confirm-description"), description); setText($("confirm-target"), target || ""); $("confirm-target").hidden = !target; setText($("confirm-submit"), label);
    dialog.returnValue = "cancel";
    return new Promise((resolve) => { dialog.addEventListener("close", () => resolve(dialog.returnValue === "confirm"), { once: true }); dialog.showModal(); });
  }
  async function drainWorker(worker) {
    if (state.mutationBusy || !state.session) return;
    if (!canManage()) { toast(errorMessages.management_disabled, true); return; }
    if (!canDrainWorker(workers().find((item) => item.id === worker.id))) { toast("当前实例状态不可排空，请等待就绪或检查实例状态。", true); return; }
    const accepted = await confirmAction("排空这个实例？", "确认后停止向该实例分配新房间。已有对局、有效重连预留和结果回写会继续完成；不会强制删除正在进行的对局。", worker.pod || worker.id, "确认排空");
    if (!accepted || !state.session || state.mutationBusy) return;
    if (!canManage()) { toast(errorMessages.management_disabled, true); return; }
    if (!canDrainWorker(workers().find((item) => item.id === worker.id))) { toast("实例状态已变化，未提交排空请求。请查看最新状态。", true); return; }
    state.mutationBusy = true;
    try { await api("drain", { method: "POST", body: { worker_id: worker.id } }); toast("已请求排空，等待实例完成现有对局后退出。"); await requestSnapshot([state.view, ...(state.detail ? ["details"] : [])]); }
    catch (error) { toast(errorText(error), true); requestSnapshot([state.view]); }
    finally { state.mutationBusy = false; }
  }
  function logParameters() {
    return new URLSearchParams({ region: $("log-region").value, namespace: $("log-namespace").value.trim(), pod: $("log-pod").value.trim(), container: $("log-container").value, mode: $("log-mode").value, minutes: $("log-minutes").value, limit: $("log-limit").value, search: $("log-search").value.trim() });
  }
  function constrainLogWindow() {
    const live = $("log-mode").value === "live";
    for (const item of $("log-minutes").options) item.disabled = live && Number(item.value) > 1440;
    if (live && Number($("log-minutes").value) > 1440) $("log-minutes").value = "1440";
  }
  function markLogDraft() {
    state.logDraftDirty = !state.logQuery || state.logQuery !== logParameters().toString();
    setText($("log-draft-state"), state.logDraftDirty ? "条件尚未应用 · 点击查询后生效" : "已应用查询条件 · 自动刷新不会改动表单");
    $("log-search").setCustomValidity(new TextEncoder().encode($("log-search").value).length > 200 ? "关键词最长为 200 字节；中文字符通常占 3 字节。" : "");
    renderStatus();
  }
  async function openLogs(worker, container = "game") {
    if (!worker.pod) { toast("此实例尚无 Pod 名称。", true); return; }
    if ($("detail-dialog").open) $("detail-dialog").close();
    options("log-region", [worker.region]);
    $("log-region").value = worker.region; $("log-pod").value = worker.pod; $("log-namespace").value = podFor(worker)?.namespace || "agones-games";
    $("log-container").value = container; $("log-mode").value = WORKER_TERMINAL.has(worker.state) ? "history" : "live";
    $("log-search").value = ""; constrainLogWindow(); markLogDraft(); setView("logs");
    await fetchLogs({ apply: true, manual: true });
  }
  function showLogResult(result) {
    if (result.query !== state.logQuery || !state.session) return;
    if (hasSelection($("log-output"))) {
      state.pendingLogs = result; setText($("log-status"), "已收到新日志；选中文字保持不动，取消选择后更新。"); return;
    }
    state.pendingLogs = null;
    const entries = list(result.response?.entries);
    const content = entries.map((entry) => (typeof entry.timestamp === "string" ? entry.timestamp + "  " : "") + (typeof entry.text === "string" ? entry.text : "")).join("\n");
    const output = $("log-output"), top = output.scrollTop, left = output.scrollLeft;
    state.logs = content;
    if (output.textContent !== (content || "此时间范围没有匹配日志。历史记录仅从启用采集开始。")) output.textContent = content || "此时间范围没有匹配日志。历史记录仅从启用采集开始。";
    output.scrollTop = $("log-follow").checked ? output.scrollHeight : top; output.scrollLeft = left;
    $("copy-logs-button").disabled = !content;
    const source = result.response?.source === "loki" ? "历史归档" : result.response?.source === "kubernetes" ? "实时容器" : "日志";
    setText($("log-status"), source + " · " + count(entries.length) + " 行 · " + time(result.response?.observed_at, true) + (result.response?.truncated ? " · 达到读取上限" : "") + (state.logDraftDirty ? " · 显示上次应用的查询" : ""));
    state.logObservedAt = result.response?.observed_at; renderStatus();
  }
  async function fetchLogs({ apply = false, manual = false } = {}) {
    if (!state.session) return;
    if (apply) {
      markLogDraft();
      if (!$("log-form").checkValidity()) { if (manual) $("log-form").reportValidity(); return; }
    } else if (!state.logQuery || state.logDraftDirty) {
      if (manual) toast("请先点击“查询日志”应用完整查询条件。");
      return;
    }
    const query = apply ? logParameters().toString() : state.logQuery;
    if (state.logBusy) {
      if (!apply || state.logQuery === query) return;
      state.logController?.abort();
    }
    state.logQuery = query; state.logDraftDirty = false; state.pendingLogs = null; state.logBusy = true;
    state.requestedAt.logs = Date.now();
    setText($("log-draft-state"), "已应用查询条件 · 自动刷新不会改动表单");
    const controller = new AbortController(), generation = state.sessionGeneration;
    state.logController = controller;
    setText($("log-status"), state.logs ? "正在读取… 以下暂为上次查询结果。" : "正在读取日志…");
    try {
      const response = await api("logs?" + query, { signal: controller.signal });
      if (!state.session || generation !== state.sessionGeneration || query !== state.logQuery || controller.signal.aborted) return;
      showLogResult({ response, query });
    } catch (error) {
      if (!controller.signal.aborted && state.session && generation === state.sessionGeneration && query === state.logQuery) {
        setText($("log-status"), errorText(error) + (state.logs ? " 当前保留上次结果。" : ""));
        if (manual) toast(errorText(error), true);
      }
    } finally { if (state.logController === controller) state.logBusy = false; }
  }
  let interactionFlushScheduled = false;
  function flushInteractions() {
    if (interactionFlushScheduled) return;
    interactionFlushScheduled = true;
    requestAnimationFrame(() => {
      interactionFlushScheduled = false;
      for (const [id, args] of [...state.pendingOptions]) options(id, args[0], args[1]);
      if (state.session && state.deferred.has(state.view)) renderCurrentView();
      if (state.session && state.detail && state.deferred.has("details")) renderDetail();
      if (state.pendingLogs && !hasSelection($("log-output"))) showLogResult(state.pendingLogs);
    });
  }
  function refreshCurrent() {
    if (state.view === "logs") fetchLogs({ manual: true }); else requestSnapshot([state.view]);
  }
  $("login-form").addEventListener("submit", async (event) => {
    event.preventDefault(); $("login-button").disabled = true; $("login-error").hidden = true;
    try { showSession(await api("login", { method: "POST", body: { username: $("username").value.trim(), password: $("password").value }, allowUnauthorized: true })); }
    catch (error) { setText($("login-error"), error.status === 401 ? "账号或密码不正确。" : errorText(error)); $("login-error").hidden = false; }
    finally { $("login-button").disabled = false; }
  });
  $("logout-button").addEventListener("click", async () => {
    $("logout-button").disabled = true;
    try { await api("logout", { method: "POST", body: {} }); showLogin("已退出。通过 SSH 隧道重新登录即可继续管理。"); }
    catch (error) { toast(errorText(error), true); }
    finally { $("logout-button").disabled = false; }
  });
  $("refresh-button").addEventListener("click", refreshCurrent);
  $("page-refresh").addEventListener("change", () => {
    const value = Number($("page-refresh").value); if (!ALLOWED_INTERVALS.has(value)) return;
    state.intervals[state.view] = value; state.requestedAt[state.view] = Date.now(); saveIntervals(); renderStatus();
  });
  $("detail-refresh").value = String(state.intervals.details);
  $("detail-refresh").addEventListener("change", () => {
    const value = Number($("detail-refresh").value); if (!ALLOWED_INTERVALS.has(value)) return;
    state.intervals.details = value; state.requestedAt.details = Date.now(); saveIntervals();
  });
  $("detail-refresh-button").addEventListener("click", () => requestSnapshot(["details"]));
  for (const item of document.querySelectorAll("[data-view]")) item.addEventListener("click", () => setView(item.dataset.view));
  window.addEventListener("hashchange", () => { if (state.session) setView(location.hash.slice(1), false); });
  for (const id of ["room-search", "room-region", "room-state", "room-scope", "worker-search", "worker-region", "include-stopped", "node-search", "node-region", "node-state"]) $(id).addEventListener(id.endsWith("search") ? "input" : "change", renderCurrentView);
  $("detail-close").addEventListener("click", () => $("detail-dialog").close());
  $("detail-dialog").addEventListener("close", () => { state.detail = null; state.deferred.delete("details"); });
  $("retry-creation-button").addEventListener("click", async () => {
    if (state.mutationBusy || !state.session) return;
    if (!canManage()) { toast(errorMessages.management_disabled, true); return; }
    if (!await confirmAction("允许重试创建实例？", "清除创建暂停状态后，Fleet 会根据当前匹配需求和容量策略重新尝试创建。请先确认导致暂停的问题已处理。", "", "确认重试") || !state.session || state.mutationBusy) return;
    if (!canManage()) { toast(errorMessages.management_disabled, true); return; }
    state.mutationBusy = true; $("retry-creation-button").disabled = true;
    try { await api("retry-creation", { method: "POST", body: {} }); toast("已请求重试创建。后续状态将自动刷新。"); await requestSnapshot([state.view]); }
    catch (error) { toast(errorText(error), true); requestSnapshot([state.view]); }
    finally { state.mutationBusy = false; $("retry-creation-button").disabled = false; }
  });
  $("log-form").addEventListener("submit", (event) => { event.preventDefault(); fetchLogs({ apply: true, manual: true }); });
  for (const id of ["log-region", "log-pod", "log-namespace", "log-container", "log-mode", "log-minutes", "log-limit", "log-search"]) {
    $(id).addEventListener($(id).tagName === "INPUT" ? "input" : "change", () => { if (id === "log-mode") constrainLogWindow(); markLogDraft(); });
  }
  $("log-follow").addEventListener("change", () => { if ($("log-follow").checked) $("log-output").scrollTop = $("log-output").scrollHeight; });
  $("log-output").addEventListener("scroll", () => { const output = $("log-output"); if (output.scrollTop + output.clientHeight < output.scrollHeight - 40) $("log-follow").checked = false; }, { passive: true });
  $("copy-logs-button").addEventListener("click", async () => {
    try { await navigator.clipboard.writeText(state.logs); toast("日志已复制。"); }
    catch (_) { toast("浏览器未允许自动复制，请在日志区手动选择文本。", true); }
  });
  document.addEventListener("focusout", flushInteractions);
  document.addEventListener("selectionchange", flushInteractions);
  // A single cheap scheduler chooses due areas. No page reloads, no global DOM
  // rebuild, no polling hidden tabs, and at most one snapshot request in flight.
  function scheduleRefresh() {
    if (document.hidden || !state.session) return;
    const now = Date.now(), targets = [], interval = state.intervals[state.view];
    if (interval > 0 && now - (state.requestedAt[state.view] || 0) >= interval * 1000) {
      if (state.view === "logs") { if (state.logQuery && !state.logDraftDirty) fetchLogs(); }
      else targets.push(state.view);
    }
    if (state.detail && $("detail-dialog").open && state.intervals.details > 0 && now - (state.requestedAt.details || 0) >= state.intervals.details * 1000) targets.push("details");
    if (targets.length) requestSnapshot(targets);
  }
  document.addEventListener("visibilitychange", () => { if (!document.hidden) scheduleRefresh(); });
  setInterval(scheduleRefresh, 1000);
  constrainLogWindow();
  $("login-button").disabled = true;
  api("session", { allowUnauthorized: true }).then(showSession).catch((error) => {
    showLogin(error.status === 401 ? undefined : errorText(error));
  }).finally(() => { $("login-button").disabled = false; });
})();
