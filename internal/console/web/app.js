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
    Succeeded: "已完成", Failed: "失败", Unknown: "未知", queued: "等待执行", installing: "安装中", verifying: "验证中", firing: "告警中", recovering: "恢复确认中", recovered: "已恢复"
  };
  const viewInfo = {
    overview: ["集群概览", "集群概览", "查看玩家承载、区域健康与实例容量。"],
    rooms: ["房间与玩家", "房间与玩家", "追踪活动房间、原席位恢复与历史对局。"],
    workers: ["游戏服实例", "游戏服实例", "按实例查看负载、容量和日志，按需停止接纳新房间。"],
    nodes: ["节点资源", "节点资源", "查看各区域 VPS 的可调度状态与资源使用。"],
    policy: ["容量与告警", "容量与告警", "区分目标策略与实例有效配置，观察等待时间并配置飞书通知。"],
    onboarding: ["添加战斗节点", "添加战斗节点", "核验已有 VPS 身份、审阅安装计划，再加入指定的 K3s 集群。"],
    logs: ["日志检索", "日志检索", "查询实时容器或最近 7 天的已采集日志，退出实例仍可检索。"]
  };
  const DEFAULT_INTERVALS = { overview: 15, rooms: 5, workers: 15, nodes: 30, policy: 15, onboarding: 30, logs: 0, details: 15 };
  const ALLOWED_INTERVALS = new Set([0, 5, 15, 30, 60]);
  function loadIntervals() {
    const values = { ...DEFAULT_INTERVALS };
    try {
      const saved = JSON.parse(localStorage.getItem("fleet-console.refresh.v1") || "{}");
      for (const key of Object.keys(values)) if (ALLOWED_INTERVALS.has(saved[key])) values[key] = saved[key];
    } catch (_) { /* Refresh preferences are optional; sessions never use localStorage. */ }
    return values;
  }
  function freshResources() { return Object.fromEntries(["policy", "alerts", "onboarding"].map((name) => [name, { data: null, error: null, promise: null, controller: null, observedAt: null }])); }
  const state = {
    session: null, snapshot: null, goodFleet: null, goodFleetAt: null, goodRegions: new Map(),
    view: "overview", snapshotPromise: null, snapshotTargets: new Set(),
    logBusy: false, logController: null, logs: "", logQuery: "", detail: null,
    toastTimer: null, mutationBusy: false, sessionGeneration: 0,
    intervals: loadIntervals(), requestedAt: {}, renderedAt: {}, scroll: {},
    deferred: new Set(), pendingOptions: new Map(), renderArea: null,
    logDraftDirty: false, pendingLogs: null, snapshotError: null, logObservedAt: null,
    resources: freshResources(), policyDirty: false, policySaving: false, policyConflict: false,
    alertDirty: false, alertSaving: false, alertConflict: false, confirmScope: null,
    nodeScan: null, nodePreflight: null, nodeScanTarget: "", nodeBusy: false, nodeAction: "", nodeError: null,
    currentJob: null, jobBusy: false, jobRequestedAt: 0, pendingEnrollment: null, retirement: freshRetirement()
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
    network_error: "连接失败，请确认 SSH 隧道保持连接。",
    feature_unavailable: "当前服务版本尚未启用此功能接口；房间、实例和日志查询不受影响。",
    policy_invalid: "容量策略不合法。房间为 1–512，CPU 为 0.1–64 核，request 与 limit 必须一致。",
    policy_conflict: "策略已被其他操作更新。草稿已保留，请重新载入最新修订后核对。",
    alerts_invalid: "告警参数或飞书地址不合法。请检查范围；Webhook 仅支持 open.feishu.cn 的机器人地址。",
    alerts_conflict: "告警配置已变化。草稿已保留，请重新载入最新修订。",
    alerts_disabled: "此部署未开放告警配置操作。", alerts_storage_unavailable: "告警配置存储暂不可用，请稍后重试。",
    alerts_file_changed: "告警私有配置已由其他操作修改，请重新读取后核对。",
    feishu_not_configured: "启用飞书通知前请配置机器人 Webhook。控制台告警监测无需机器人。",
    feishu_delivery_failed: "最近一次飞书通知未投递成功，请检查机器人配置和服务端网络。",
    node_onboarding_disabled: "此部署尚未启用节点接入服务。", onboarding_disabled: "此部署尚未启用节点接入服务。",
    scan_expired: "主机指纹扫描已过期，请重新扫描并核对。", preflight_expired: "本次预检授权已过期，请重新扫描和认证。",
    fingerprint_mismatch: "SSH 主机指纹与已核对记录不一致，未继续认证或安装。",
    ssh_authentication_failed: "SSH 认证失败。密码已清空，请核对 root 登录方式。",
    ssh_unavailable: "无法建立 SSH 连接，请检查地址、端口与防火墙。",
    preflight_failed: "主机预检未通过，请核对检查结果。", onboarding_busy: "节点接入服务已有任务执行中，请稍后重试。",
    node_onboarding_not_configured: "此部署尚未启用节点接入服务。",
    node_onboarding_unavailable: "地域节点控制器或 Kubernetes API 暂不可用，请稍后重新读取状态。",
    node_onboarding_access_denied: "地域节点任务被 Kubernetes 权限策略拒绝，请检查控制台受限任务授权。",
    node_onboarding_request_failed: "地域节点任务请求未完成，请检查任务状态，不要重复提交安装或退役。",
    onboarding_state_changed: "Kubernetes 任务状态已变化，请重新读取并核对后操作。",
    invalid_retirement_confirmation: "永久退役确认不完整，请重新核对节点名与 Node 上报 IP。",
    node_identity_changed: "节点 UID 或资源版本已变化，请重新检查退役条件。",
    node_onboarding_invalid_response: "节点接入服务返回了无法读取的状态，请检查服务部署。",
    node_onboarding_failed: "节点接入未完成，请检查服务端状态后再操作。",
    onboarding_unavailable: "节点接入服务暂不可用，请稍后重试。",
    literal_ipv4_required: "请输入目标 VPS 的公网 IPv4 地址，当前不支持主机名或 IPv6。",
    invalid_target_address: "该地址不能用于接入，请填写目标 VPS 的公网 IPv4。",
    protected_control_host: "该主机是受保护的管理节点，不能作为新战斗节点接入。",
    unknown_region: "目标地区不存在，请刷新地区列表。",
    ssh_scan_failed: "SSH 指纹扫描失败，请检查公网 IPv4、端口和防火墙。",
    ssh_host_identity_unavailable: "未读取到受支持的 SSH 主机指纹，未发送密码。",
    host_fingerprint_mismatch: "SSH 指纹与已核对记录不一致，未继续认证或安装。",
    root_user_required: "当前节点接入仅支持 root 用户。",
    invalid_ssh_password: "SSH 密码格式不正确：最多 1024 字节且不能包含换行。",
    ssh_operation_failed: "SSH 认证或预检执行失败，请检查 root 登录方式；需重新扫描并认证。",
    ssh_operation_timeout_check_node: "SSH 操作超时，请先核对主机状态；不要直接重复安装。",
    ssh_invalid_response: "主机返回的预检结果无法读取，请检查系统环境。",
    cluster_read_access_unavailable: "节点接入服务无法读取集群状态，请检查受限集群凭据。",
    cluster_unreachable: "节点接入服务无法连接目标集群，请检查管理节点与私网连接。",
    too_many_pending_requests: "待处理的节点扫描或预检过多，请稍后重试。",
    another_node_join_in_progress: "已有节点安装任务执行中，请等待该任务结束。",
    node_name_already_registered: "这个节点名已在集群中注册，未重复安装。",
    host_changed_repeat_preflight: "主机状态在预检后发生变化，请重新扫描和预检。",
    agent_only_join_token_required: "节点接入服务未配置仅限 worker 的入群凭据，请检查部署。",
    unsafe_node_installer: "节点安装程序未通过完整性检查，未继续安装。",
    installation_failed_check_host: "节点安装未完成，请检查该 VPS 的安装状态后再处理。",
    node_ready_timeout_check_cluster: "等待节点 Ready 超时，请检查集群和目标 VPS，避免重复安装。",
    invalid_job_id: "接入任务标识不正确，请从任务记录重新选择。",
    job_not_found: "未找到该接入任务，请刷新任务列表。"
  };
  function errorText(error) {
    if (errorMessages[error.code]) return errorMessages[error.code];
    if (error.status === 401) return "登录状态已失效，请重新登录。";
    if (error.status === 403) return "此请求被权限策略拒绝，当前登录仍有效。请检查服务端操作授权。";
    if (error.status === 429) return "请求过于频繁，请稍后重试。";
    return error.status ? "请求失败（HTTP " + error.status + "），请稍后重试。" : "连接失败，请确认 SSH 隧道保持连接。";
  }
  async function api(path, { method = "GET", body, signal, allowUnauthorized = false, timeoutMs = 20000 } = {}) {
    const controller = new AbortController(), requestGeneration = state.sessionGeneration;
    const timeout = setTimeout(() => controller.abort(), timeoutMs);
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
        if (response.status === 401 && !allowUnauthorized && state.session && requestGeneration === state.sessionGeneration) showLogin("登录已过期，请重新登录。");
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
    for (const id of ["detail-dialog", "confirm-dialog", "node-retire-dialog"]) if ($(id).open) $(id).close("cancel");
    for (const entry of Object.values(state.resources)) entry.controller?.abort();
    state.resources = freshResources(); state.policyDirty = false; state.alertDirty = false; state.policySaving = false; state.alertSaving = false;
    state.policyConflict = false; state.alertConflict = false; state.policyBaseRevision = undefined; state.alertBaseRevision = undefined;
    state.nodeBusy = false; state.nodeAction = ""; state.nodeError = null; state.currentJob = null; state.pendingEnrollment = null; state.jobBusy = false; state.retirement = freshRetirement();
    for (const id of ["policy-form", "alert-form", "node-scan-form", "node-preflight-form"]) $(id).reset();
    clearFeishuInputs(); clearNodeAuthorization();
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
      if (view === "policy") refreshPolicy();
      else if (view === "onboarding") refreshOnboarding();
      else if (!state.snapshot || view !== "logs" && !state.renderedAt[view]) requestSnapshot([view]);
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
  function nodeRole(role) { return role === "control" ? "管理节点" : role === "game" ? "战斗节点" : role ? String(role) : "未标注"; }
  const nodeKey = (item) => item.region + "/" + item.name;
  const allNodes = () => regions().flatMap((region) => list(region.nodes).map((item) => ({ ...item, region: region.name, stale: region.stale === true || region.ok !== true })));
  const nodeIPs = (item, key) => list(item?.[key]).filter((value) => typeof value === "string" && value);
  function nodeReadyStatus(item) { return ["True", "False", "Unknown"].includes(item?.ready_status) ? item.ready_status : item?.ready === true ? "True" : "Unknown"; }
  function nodeReadyLabel(item) { return ({ True: "已就绪", False: "未就绪", Unknown: "状态未知" })[nodeReadyStatus(item)]; }
  function nodePublicIP(item) {
    const values = nodeIPs(item, "external_ips");
    return values.length ? { label: "公网 IP · ExternalIP", value: values.join("、"), note: "Node.status.addresses" } : item.operator_public_ip ? { label: "公网 IP（管理备注）", value: item.operator_public_ip, note: "人工显示备注，不是节点网络配置" } : { label: "公网 IP · ExternalIP", value: "未上报", note: "不从节点名称推断" };
  }
  function nodePrivateIP(item) { return nodeIPs(item, "internal_ips").join("、") || "未上报"; }
  function nodeMatches(item, query) { return !query || [item.name, item.role, nodeRole(item.role), ...nodeIPs(item, "external_ips"), ...nodeIPs(item, "internal_ips"), item.operator_public_ip].some((value) => String(value || "").toLowerCase().includes(query)); }

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
      else if (view === "nodes") { renderNodes(); renderRetirementJob(); }
      else if (view === "policy") renderPolicy();
      else if (view === "onboarding") renderOnboarding();
    });
    state.renderArea = null;
    if (!["logs", "policy", "onboarding"].includes(view) && state.snapshot && !state.deferred.has(view)) state.renderedAt[view] = state.snapshot.observed_at;
    renderStatus(); renderRetirementConfirmation();
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
    const usesSnapshot = view !== "onboarding";
    $("connection-banner").hidden = !state.snapshotError || !usesSnapshot;
    if (state.snapshotError && usesSnapshot) setText($("connection-banner"), errorText(state.snapshotError) + (state.goodFleet || state.goodRegions.size ? " 已保留上次成功数据，请勿视为最新状态。" : " 尚未取得运行数据。"));
    const problems = [];
    if (["overview", "rooms", "workers", "policy"].includes(view) && fleet()?.ok === false) problems.push("Fleet 状态：" + errorText({ code: fleet().error }) + (state.goodFleet ? " 当前显示 " + time(state.goodFleetAt) + " 的成功记录。" : " 尚无可用记录，未将错误显示为零。"));
    if (["overview", "workers", "nodes", "policy"].includes(view)) for (const region of regions()) {
      if (!region.ok) problems.push(text(region.name) + "：" + errorText({ code: region.error }) + (region.stale ? " 节点 / Pod 保留上次成功记录。" : ""));
      else if (!region.metrics_ok) problems.push(text(region.name) + "：资源指标暂不可用；状态数据仍可查看，缺失数值显示为 —。");
    }
    $("source-banner").hidden = problems.length === 0; setText($("source-banner"), problems.slice(0, 3).join(" "));
    const stale = !!state.snapshotError && usesSnapshot || problems.length > 0 || view === "policy" && (!!resource("policy").error || !!resource("alerts").error) || view === "onboarding" && (!!resource("onboarding").error || !!state.nodeError);
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
    if (!enabled && state.confirmScope === "fleet" && $("confirm-dialog").open) $("confirm-dialog").close("cancel");
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
    const sources = regions(), observed = sources.filter((region) => region.ok || region.stale);
    const complete = sources.length > 0 && observed.length === sources.length;
    const visibleNodes = observed.flatMap((region) => list(region.nodes));
    const visiblePods = observed.flatMap((region) => list(region.pods));
    const namespaces = [...new Set(visiblePods.map((pod) => pod.namespace).filter(Boolean))].sort();
    const topology = [
      ["已注册节点", complete ? visibleNodes.length : undefined, "台 VPS", "管理节点 " + count(visibleNodes.filter((item) => item.role === "control").length) + " · 战斗节点 " + count(visibleNodes.filter((item) => item.role === "game").length) + "；角色按已读取标签统计。"],
      ["可见 Pod", complete ? visiblePods.length : undefined, "个", namespaces.length ? "本次可见命名空间：" + namespaces.join("、") : "等待观察范围内的 Pod 数据。"],
      ["游戏服实例", ok ? live.length : undefined, "个进程", "Fleet 管理的非终态实例；一个实例可承载多个对局房间。"]
    ];
    patchRegion($("topology-overview"), topology.map(([title, value, unit, note]) => {
      const item = keyed(node("article", "topology-card"), title), amount = node("div", "topology-value", count(value));
      amount.append(node("span", "", unit)); item.append(node("h3", "", title), amount, node("p", "", note)); return item;
    }));
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
      else notes.push(healthItem(region.name + "：资源指标可用", "已注册节点 " + count(list(region.nodes).length) + " 个；观察范围内 Pod " + count(list(region.pods).length) + " 个（非全群总数）。"));
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
    for (const [label, value] of [["房间 / 容量", count(worker.occupied_rooms) + " / " + count(worker.max_rooms)], ["Pod CPU", cpu(pod?.cpu_millicores)], ["Pod 内存", bytes(pod?.memory_bytes)], ["帧间隔 P99", ms(metrics.frame_p99_ms)]]) { const pair = node("div"); pair.append(node("dt", "", label), node("dd", "", value)); details.append(pair); }
    const actions = node("div", "worker-card-actions"); actions.append(button("查看实例", () => openDetail("worker", worker.id)), button("运行日志", () => openLogs(worker), "button small quiet"));
    card.append(details, node("p", "worker-card-footnote", "CPU / 内存为 Pod 内容器合计，含 sidecar。"), actions); return card;
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
    const nodes = allNodes();
    const filtered = nodes.filter((item) => (!filterRegion || item.region === filterRegion) && nodeMatches(item, query) && (!status || (status === "unready" ? nodeReadyStatus(item) === "False" : status === "unknown" ? nodeReadyStatus(item) === "Unknown" : status === "unschedulable" ? item.ready && item.unschedulable : item.ready && !item.unschedulable)));
    setText($("node-count"), regions().some((region) => region.ok || region.stale) ? count(filtered.length) + " 个" : "尚无可用数据");
    table($("nodes-table"), ["节点 / 地区", "Ready / 调度", "公网 IP", "内网 IP", "角色", "CPU 使用 / 可分配", "内存使用 / 可分配", "可见 Pod", "操作"], filtered.map((item) => {
      const region = regions().find((region) => region.name === item.region), address = nodePublicIP(item);
      const statusCell = cell(badge(nodeReadyLabel(item), nodeReadyStatus(item) === "True" ? "good" : nodeReadyStatus(item) === "False" ? "danger" : "warning"), item.unschedulable ? "已暂停调度" : nodeReadyStatus(item) === "True" ? "可调度" : "不据此推断 VPS 已关机 / 到期");
      const podCount = region ? list(region.pods).filter((pod) => pod.node === item.name).length : undefined;
      return { key: nodeKey(item), cells: [cell(identifierButton(item.name, () => openDetail("node", nodeKey(item))), text(item.region) + (item.stale ? " · 上次记录" : "")), statusCell, cell(node("span", "identifier", address.value), address.label.includes("管理备注") ? "管理备注" : "ExternalIP"), node("span", "identifier", nodePrivateIP(item)), cell(nodeRole(item.role), item.role ? "标签：" + item.role : "尚无角色标签"), cell(cpu(item.cpu_millicores), "可分配 " + cpu(item.cpu_allocatable_millicores)), cell(bytes(item.memory_bytes), "可分配 " + bytes(item.memory_allocatable_bytes)), count(podCount), button("节点详情", () => openDetail("node", nodeKey(item)), "button small quiet")] };
    }), "暂无符合筛选的节点记录；集群读取失败时会保留上次成功快照。");
    const nodeIDs = new Set(filtered.map(nodeKey));
    const visiblePods = regions().flatMap((region) => list(region.pods).map((pod) => ({ ...pod, region: region.name, stale: region.stale }))).filter((pod) => (!filterRegion || pod.region === filterRegion) && (!status || nodeIDs.has(pod.region + "/" + pod.node)) && (!query || nodeIDs.has(pod.region + "/" + pod.node) || [pod.name, pod.node, pod.namespace].some((value) => String(value || "").toLowerCase().includes(query))));
    setText($("pod-count"), regions().some((region) => region.ok || region.stale) ? count(visiblePods.length) + " 个 · 当前观察范围" : "尚无可用数据");
    table($("pods-table"), ["Pod / 命名空间", "节点", "状态", "关联游戏服", "容器", "Pod CPU / 内存"], visiblePods.map((pod) => {
      const worker = workers().find((item) => item.id === pod.worker_id || item.pod === pod.name && item.region === pod.region), host = nodes.find((item) => item.region === pod.region && item.name === pod.node);
      return { key: pod.region + "/" + pod.namespace + "/" + pod.name, cells: [cell(text(pod.name), text(pod.namespace) + (pod.stale ? " · 上次记录" : "")), cell(host ? identifierButton(host.name, () => openDetail("node", nodeKey(host))) : text(pod.node), pod.region), badge(pod.phase), worker ? identifierButton(worker.id, () => openDetail("worker", worker.id)) : cell("未关联 Fleet 实例", pod.worker_id ? "快照暂无对应实例" : "不按 Pod 名推断用途"), list(pod.containers).join("、") || "—", cell(cpu(pod.cpu_millicores), bytes(pod.memory_bytes))] };
    }), "当前观察范围没有符合筛选的 Pod；这不代表整个集群没有 Pod。");
  }
  function freshRetirement() { return { key: "", check: null, error: null, busy: false, job: null, jobBusy: false, requestedAt: 0, confirmation: null }; }
  function retirementCandidate(host) {
    if (!host) return "当前快照没有这个节点记录。";
    if (state.snapshotError || host.stale) return "节点状态暂不可确认，请先恢复集群读取。";
    if (host.role === "control" || host.game_node !== true) return "管理节点与未标注为受管战斗节点的记录不能从此入口移除。";
    if (nodeReadyStatus(host) === "True") return "节点仍为 Ready=True，不能移除。";
    return "";
  }
  function retirementReason(code) {
    return ({ node_identity_unavailable: "未能取得完整 Node 身份，不允许移除。", node_not_owned_worker: "该节点不是此系统管理的战斗节点。", node_still_ready: "节点仍为 Ready=True，不允许移除。", node_readiness_unknown: "缺少可确认的 Ready 条件，不能据此判断永久离线。", node_has_pods: "节点仍有关联的非允许 Pod，不能移除。", node_inventory_incomplete: "节点工作负载清单不完整，不能安全移除。", fleet_snapshot_required: "需要可用的 Fleet 快照来检查实例与对局。", fleet_snapshot_invalid: "Fleet 快照无效，无法确认实例与对局已结束。", node_has_processing_worker: "节点仍有关联处理中的游戏服进程。", worker_location_unavailable: "无法确认游戏服实例的所在节点，禁止移除。", node_has_active_allocations: "节点仍有关联活动分配或对局，不能移除。", node_ready: "节点仍处于 Ready=True。", node_not_offline: "服务端尚未确认节点离线。", control_node_protected: "管理节点受保护，不能移除。", protected_control_node: "管理节点受保护，不能移除。", game_node_required: "该记录不是受管战斗节点。", node_has_workloads: "节点仍有关联工作负载，不能移除。", node_has_gameservers: "节点仍有关联游戏服，不能移除。", node_identity_changed: "节点身份或资源版本已变化，需重新检查。", node_retirement_disabled: "此部署未开放离线节点移除能力。", node_onboarding_disabled: "此部署未开放离线节点移除能力。", node_retirement_not_configured: "此部署未配置离线节点移除控制器。", node_not_found: "节点记录已不存在，请刷新列表。" })[code] || "服务端尚未允许移除，请核对节点状态和阻断项。";
  }
  function retirementIdentityIPs(identity) { return [...nodeIPs(identity, "external_ips"), ...nodeIPs(identity, "internal_ips")]; }
  function retirementEligible(host) {
    const item = state.retirement, value = item.check;
    return !retirementCandidate(host) && !retirementJobRunning(item.job) && item.key === nodeKey(host) && !item.error && value?.enabled === true && value.eligible === true && value.identity?.name === host.name && typeof value.identity.uid === "string" && value.identity.uid !== "" && typeof value.identity.resource_version === "string" && value.identity.resource_version !== "" && retirementIdentityIPs(value.identity).length > 0;
  }
  function retirementControl(host) {
    const section = keyed(node("section", "node-retirement-section"), "node-retirement"), actions = node("div", "detail-actions"), item = state.retirement;
    section.append(node("h3", "detail-section", "永久失效节点的退役移除"), node("p", "table-note", "只移除 Kubernetes Node 注册记录，不调用腾讯云关机或销毁 API。普通离线不能证明永久失效；已有游戏服和非允许的工作负载会阻止移除。"));
    const localReason = retirementCandidate(host), check = item.key === nodeKey(host) ? item.check : null;
    const checkButton = button(item.busy && item.key === nodeKey(host) ? "正在检查…" : "检查退役条件", () => loadRetirement(host));
    checkButton.disabled = !!localReason || item.busy || retirementJobRunning(item.job); actions.append(checkButton);
    let message = localReason || (retirementJobRunning(item.job) ? "已有退役任务执行中，不能重复提交。" : !check ? "尚未读取服务端退役条件；没有移除授权。" : !check.enabled ? "此部署未开放离线节点移除能力。" : check.eligible ? "服务端初步检查允许移除；仍需确认该 VPS 已永久退役并输入节点名、IP。提交时会再次检查。" : retirementReason(check.reason));
    if (item.key === nodeKey(host) && item.error) message = errorText(item.error);
    section.append(node("p", "inline-note", message));
    if (retirementEligible(host)) actions.append(button("移除已退役 Node 记录", () => openRetirement(host), "button danger"));
    section.append(actions);
    if (check?.blockers?.length) section.append(retirementItems(check.blockers, "阻断项"));
    return section;
  }
  function retirementItems(items, title) {
    const section = node("div"); section.append(node("h3", "detail-section", title));
    const wrapper = node("div", "table-wrap");
    table(wrapper, ["类型", "命名空间 / 名称", "说明"], list(items).map((item, index) => ({ key: text(item.kind) + "/" + text(item.namespace) + "/" + text(item.name) + "/" + index, cells: [text(item.kind), cell(text(item.name), item.namespace), text(item.reason)] })), "无已报告项目。");
    section.append(wrapper); return section;
  }
  async function loadRetirement(host, { render = true } = {}) {
    if (!state.session || retirementCandidate(host) || state.retirement.busy) return null;
    const item = state.retirement, generation = state.sessionGeneration, key = nodeKey(host);
    item.key = key; item.busy = true; item.error = null;
    if (render && state.detail?.kind === "node") renderDetail();
    try {
      const value = await api("node-retirement?" + new URLSearchParams({ region: host.region, node: host.name }));
      if (!state.session || generation !== state.sessionGeneration) return null;
      if (!value || typeof value.enabled !== "boolean" || value.enabled && typeof value.eligible !== "boolean") { const error = new Error("invalid_retirement"); error.code = "upstream_invalid_response"; throw error; }
      item.check = value; return value;
    } catch (error) { if (generation === state.sessionGeneration && state.session) { item.check = null; item.error = error; } return null; }
    finally { if (generation === state.sessionGeneration) { item.busy = false; if (render && state.detail?.kind === "node") renderDetail(); } }
  }
  function openRetirement(host) {
    if (!retirementEligible(host) || state.retirement.busy) return;
    state.retirement.confirmation = { key: nodeKey(host), host: { name: host.name, region: host.region }, identity: state.retirement.check.identity };
    $("node-retire-form").reset();
    setText($("node-retire-target"), "地区 " + host.region + " · 节点 " + host.name + "\n可用于确认的 Node 上报 IP：" + retirementIdentityIPs(state.retirement.check.identity).join("、"));
    setText($("node-retire-error"), ""); $("node-retire-error").hidden = true;
    $("node-retire-dialog").showModal(); renderRetirementConfirmation();
  }
  function renderRetirementConfirmation() {
    if (!$("node-retire-dialog").open) return;
    const confirmation = state.retirement.confirmation, host = allNodes().find((item) => nodeKey(item) === confirmation?.key), identity = confirmation?.identity;
    const allowed = !!host && retirementEligible(host) && !state.retirement.busy && identity && $("node-retire-name").value === identity.name && retirementIdentityIPs(identity).includes($("node-retire-ip").value.trim()) && $("node-retire-permanent").checked;
    $("node-retire-submit").disabled = !allowed;
    $("node-retire-cancel").disabled = state.retirement.busy;
    setText($("node-retire-submit"), state.retirement.busy ? "正在复查并提交…" : "仅移除 Node 注册记录");
  }
  async function submitRetirement(event) {
    event.preventDefault();
    const current = state.retirement.confirmation, item = state.retirement;
    if (!current || item.busy || !$("node-retire-form").reportValidity()) return;
    const host = allNodes().find((value) => nodeKey(value) === current.key), confirmedName = $("node-retire-name").value, confirmedIP = $("node-retire-ip").value.trim();
    if (!host || !retirementEligible(host) || confirmedName !== current.identity.name || !retirementIdentityIPs(current.identity).includes(confirmedIP) || !$("node-retire-permanent").checked) return;
    const generation = state.sessionGeneration;
    $("node-retire-submit").disabled = true; $("node-retire-error").hidden = true;
    const checked = await loadRetirement(host, { render: false });
    if (!state.session || generation !== state.sessionGeneration || item.confirmation !== current) return;
    if (!checked || !retirementEligible(allNodes().find((value) => nodeKey(value) === current.key))) {
      setText($("node-retire-error"), item.error ? errorText(item.error) : retirementReason(checked?.reason)); $("node-retire-error").hidden = false; renderRetirementConfirmation(); return;
    }
    const identity = checked.identity;
    if (identity.uid !== current.identity.uid || identity.resource_version !== current.identity.resource_version || identity.name !== confirmedName || !retirementIdentityIPs(identity).includes(confirmedIP)) {
      item.confirmation = { ...current, identity }; $("node-retire-form").reset();
      setText($("node-retire-target"), "地区 " + host.region + " · 节点 " + identity.name + "\n可用于确认的 Node 上报 IP：" + retirementIdentityIPs(identity).join("、"));
      setText($("node-retire-error"), "节点身份、地址或资源版本已经变化，请重新核对并输入确认。未提交删除。"); $("node-retire-error").hidden = false; renderRetirementConfirmation(); return;
    }
    item.busy = true; renderRetirementConfirmation();
    try {
      const job = await api("node-retirement", { method: "POST", body: { region: host.region, node_name: identity.name, node_uid: identity.uid, resource_version: identity.resource_version, confirmed_node_name: confirmedName, confirmed_ip: confirmedIP, permanent_retirement: true } });
      if (!state.session || generation !== state.sessionGeneration) return;
      if (!job || typeof job.id !== "string" || !/^[a-f0-9]{32}$/.test(job.id) || typeof job.state !== "string") { const error = new Error("invalid_retirement_job"); error.code = "upstream_invalid_response"; throw error; }
      item.job = { region: host.region, node_name: identity.name, ...job }; item.requestedAt = 0; item.check = null;
      $("node-retire-dialog").close();
      if ($("detail-dialog").open) $("detail-dialog").close();
      setView("nodes"); renderRetirementJob();
      $("node-retirement-panel").scrollIntoView({ block: "nearest" });
      toast("退役移除任务已提交，尚未确认删除；请查看任务结果。");
      requestSnapshot(["nodes"]);
    } catch (error) { if (generation === state.sessionGeneration && state.session) { setText($("node-retire-error"), errorText(error) + " 不会自动重试删除。"); $("node-retire-error").hidden = false; } }
    finally { if (generation === state.sessionGeneration) { item.busy = false; renderRetirementConfirmation(); } }
  }
  function retirementJobRunning(job) { return ["queued", "checking", "cordoned"].includes(job?.state); }
  function renderRetirementJob() {
    const job = state.retirement.job; $("node-retirement-panel").hidden = !job;
    if (!job) return;
    const stateLabel = ({ queued: "等待执行", checking: "复查中", cordoned: "待继续处理", deleted: "Node 记录已移除", blocked: "已阻止移除", needs_review: "需人工复核" })[job.state] || text(job.state);
    const body = [detailGrid([["任务 ID", job.id], ["地区 / 节点", text(job.region) + " / " + text(job.node_name)], ["状态", stateLabel], ["最近更新", time(job.updated_at)]])];
    body.push(node("p", "table-note", job.state === "deleted" ? "仅 Kubernetes Node 注册记录已移除。腾讯云 VPS 未被此操作关闭、销毁或释放。" : retirementJobRunning(job) ? "地域 controller 正在复查并执行受限任务，每 5 秒读取结果；不会自动提交第二次删除。" : "任务未确认完成删除。请查看阻断项并人工核对，普通离线不等于永久失效。"));
    if (state.retirement.error) body.push(node("p", "form-error", errorText(state.retirement.error) + " 当前任务结果尚未确认；继续读取状态，不会重新提交删除。"));
    if (job.reason || job.error) body.push(node("p", "inline-note", retirementReason(job.reason || job.error)));
    if (list(job.blockers).length) body.push(retirementItems(job.blockers, "阻断项"));
    if (list(job.allowed_system_pods).length) body.push(retirementItems(job.allowed_system_pods, "控制器允许的系统 DaemonSet Pod"));
    patchRegion($("node-retirement-result"), body);
  }
  async function fetchRetirementJob() {
    const item = state.retirement, job = item.job;
    if (!state.session || !retirementJobRunning(job) || item.jobBusy) return;
    item.jobBusy = true; item.requestedAt = Date.now(); const generation = state.sessionGeneration;
    try {
      const value = await api("node-retirement/jobs?" + new URLSearchParams({ id: job.id }));
      if (!state.session || generation !== state.sessionGeneration || item.job?.id !== job.id) return;
      if (!value || value.id !== job.id || typeof value.state !== "string") { const error = new Error("invalid_retirement_job"); error.code = "upstream_invalid_response"; throw error; }
      item.job = { ...job, ...value }; item.error = null;
      if (value.state === "deleted") requestSnapshot(["nodes", ...(state.detail ? ["details"] : [])]);
    } catch (error) { if (generation === state.sessionGeneration && state.session) { item.error = error; } }
    finally { if (generation === state.sessionGeneration) { item.jobBusy = false; if (state.session && state.view === "nodes") renderRetirementJob(); } }
  }

  function resource(name) { return state.resources[name]; }
  function featureData(name) { return resource(name).data; }
  function featureAvailable(name, capability) { const item = resource(name); return !!state.session && !item.error && item.data?.[capability] === true; }
  function setDisabled(ids, disabled) { for (const id of ids) $(id).disabled = disabled; }
  function cpuBudget(value) { return number(value) ? decimal(value / 1000, 3) + " 核" : "—"; }
  function cpuLimit(value) { return value === 0 ? "未设置上限" : cpuBudget(value); }
  function expired(value) {
    const until = typeof value === "number" ? value * 1000 : Date.parse(value);
    return !Number.isFinite(until) || Date.now() >= until;
  }
  function errorBanner(id, error) { $(id).hidden = !error; if (error) setText($(id), errorText(error) + " 已有记录保留，修改不会自动重试。"); }
  async function loadFeature(name, path, { fresh = false } = {}) {
    if (!state.session) return;
    const entry = resource(name);
    if (entry.promise && !fresh) return entry.promise;
    if (fresh) entry.controller?.abort();
    const generation = state.sessionGeneration, controller = new AbortController();
    entry.controller = controller;
    const task = (async () => {
      try {
        const data = await api(path, { signal: controller.signal });
        if (!state.session || generation !== state.sessionGeneration || controller.signal.aborted) return;
        if (!data || typeof data !== "object" || (name === "policy" && (!data.desired || data.revision === undefined)) || (name === "alerts" && (!data.config || data.revision === undefined)) || (name === "onboarding" && typeof data.enabled !== "boolean")) {
          const failure = new Error("invalid_feature_response"); failure.code = "upstream_invalid_response"; throw failure;
        }
        entry.data = data; entry.error = null; entry.observedAt = data.observed_at || new Date().toISOString();
        state.renderedAt[name === "onboarding" ? "onboarding" : "policy"] = entry.observedAt;
        if (name === "onboarding" && state.currentJob) {
          const updated = list(data.jobs).find((job) => job.id === state.currentJob.id);
          if (updated) state.currentJob = { ...state.currentJob, ...updated };
        }
      } catch (error) {
        if (state.session && generation === state.sessionGeneration && !controller.signal.aborted) {
          if (error.status === 404) error.code = "feature_unavailable";
          entry.error = error;
        }
      } finally {
        if (entry.controller === controller) { entry.promise = null; entry.controller = null; }
        if (state.session && generation === state.sessionGeneration && ["policy", "onboarding"].includes(state.view)) renderCurrentView();
      }
    })();
    entry.promise = task;
    return task;
  }
  function refreshPolicy() {
    state.requestedAt.policy = Date.now();
    return Promise.all([loadFeature("policy", "v1/policy"), loadFeature("alerts", "v1/alerts"), requestSnapshot(["policy"])]);
  }
  function refreshOnboarding() { state.requestedAt.onboarding = Date.now(); return loadFeature("onboarding", "node-onboarding"); }
  function hydratePolicy(force = false) {
    const value = featureData("policy"), form = $("policy-form");
    if (!value || state.policySaving || (!force && (state.policyDirty || form.contains(document.activeElement)))) return;
    const desired = value.desired || {};
    $("policy-rooms").value = number(desired.rooms_per_instance) ? String(desired.rooms_per_instance) : "";
    $("policy-cpu").value = number(desired.cpu_request_millicores) && desired.cpu_request_millicores === desired.cpu_limit_millicores ? String(desired.cpu_request_millicores / 1000) : "";
    state.policyBaseRevision = value.revision; state.policyDirty = false; state.policyConflict = false;
  }
  function hydrateAlerts(force = false) {
    const value = featureData("alerts"), form = $("alert-form");
    if (!value || state.alertSaving || (!force && (state.alertDirty || form.contains(document.activeElement)))) return;
    const config = value.config || {};
    $("alerts-enabled").checked = config.enabled === true;
    $("feishu-enabled").checked = config.feishu_enabled === true;
    for (const [id, key] of [["alert-wait", "wait_p95_ms"], ["alert-frame", "frame_p99_ms"], ["alert-hold", "hold_seconds"], ["alert-cooldown", "cooldown_seconds"], ["alert-samples", "min_samples"]]) $(id).value = number(config[key]) ? String(config[key]) : "";
    // Secret fields are write-only. A response can never populate these inputs.
    state.alertBaseRevision = value.revision; state.alertDirty = false; state.alertConflict = false;
  }
  function sampleCell(window) {
    if (!window) return cell("—", "尚无客户端上报");
    const available = number(window.count) && window.count > 0;
    return cell(available ? ms(window.p95) : "—", count(window.count) + " 样本 · " + count(window.window_seconds) + " 秒窗口 · " + (number(window.last_sample_age_seconds) ? "最新样本 " + seconds(window.last_sample_age_seconds) + "前" : "暂无新样本"));
  }
  function alertKind(kind) { return ({ client_wait_high: "停球后等待 p95", client_settlement_wait_high: "停球后结算等待 p95", frame_p99_high: "帧间隔 p99", physical_capacity_shortage: "节点物理容量不足" })[kind] || text(kind); }
  function alertValue(item, key) { return item.kind === "physical_capacity_shortage" ? count(item[key]) + " 个 Pending Pod" : ms(item[key]); }
  function renderPolicy() {
    const policy = featureData("policy"), alerts = featureData("alerts");
    errorBanner("policy-error", resource("policy").error); errorBanner("alerts-error", resource("alerts").error);
    hydratePolicy(); hydrateAlerts();
    const manage = featureAvailable("policy", "can_manage"), configure = featureAvailable("alerts", "can_configure");
    const desired = policy?.desired || {};
    setText($("policy-revision"), policy ? "目标修订 " + text(policy.revision) : "尚未取得策略");
    if (policy) patchRegion($("policy-summary"), [detailGrid([["目标房间上限", count(desired.rooms_per_instance) + " 间 / 实例"], ["目标 CPU request", cpuBudget(desired.cpu_request_millicores)], ["目标 CPU limit", cpuLimit(desired.cpu_limit_millicores)], ["需重建实例", count(policy.requires_replacement_count)]])]);
    else empty($("policy-summary"), "尚无策略数据；不会用预设值冒充正在生效的配置。");
    $("policy-legacy-note").hidden = !policy || desired.cpu_request_millicores === desired.cpu_limit_millicores;
    setText($("policy-draft-state"), !policy ? "等待服务器配置。" : !manage ? "当前可查看策略，服务端未开放策略修改。" : state.policyConflict ? "服务器修订已变化；草稿保留，请重新载入并核对后再保存。" : state.policyDirty ? "有未保存修改 · 自动刷新只更新状态，不覆盖输入。" : "保存只影响后续创建的实例，现有实例不会自动重建。");
    setDisabled(["policy-rooms", "policy-cpu", "policy-cpu-preset"], !manage || state.policySaving);
    $("policy-save-button").disabled = !manage || state.policySaving || state.policyConflict || state.policyBaseRevision === undefined;
    $("policy-reload-button").disabled = !policy || state.policySaving;
    setText($("policy-save-button"), state.policySaving ? "正在保存…" : "保存目标策略");
    const effective = list(policy?.effective_instances);
    setText($("policy-effective-count"), policy ? count(effective.length) + " 个" : "等待数据");
    table($("policy-effective-table"), ["实例", "状态", "有效房间上限", "有效 CPU request / limit", "实例策略修订", "与目标比较"], effective.map((item) => ({ key: item.worker_id, cells: [identifierButton(item.worker_id, () => openDetail("worker", item.worker_id)), badge(item.state), count(item.rooms_per_instance), cell(cpuBudget(item.cpu_request_millicores), "上限 " + cpuLimit(item.cpu_limit_millicores)), text(item.policy_revision), item.requires_replacement === true ? badge("需排空重建", "warning") : item.requires_replacement === false ? badge("与目标一致", "good") : "—"] })), policy ? "暂无运行实例；新实例将使用上方目标策略。" : "尚未读取实例有效配置。");
    const active = workers().filter((item) => !WORKER_TERMINAL.has(item.state));
    table($("alert-metric-table"), ["实例 / 地区", "停球 → 可继续操作 p95", "停球 → 结算 p95", "帧间隔 p99", "来源"], active.map((worker) => ({ key: worker.id, cells: [cell(identifierButton(worker.pod || worker.id, () => openDetail("worker", worker.id)), worker.region), sampleCell(worker.metrics?.client_presentation_to_ready_ms), sampleCell(worker.metrics?.client_presentation_to_settlement_ms), ms(worker.metrics?.frame_p99_ms), cell("客户端自报等待", "帧间隔来自服务端心跳")] })), state.goodFleet ? "暂无运行实例的样本；没有样本不表示延迟为 0。" : "实例指标暂不可用，未将缺失数据显示为零。");
    setText($("alerts-status"), !alerts ? "等待告警配置" : alerts.config?.enabled ? "监测已开启" : "监测已关闭");
    $("alerts-status").className = "badge " + (alerts?.config?.enabled ? "good" : "");
    setText($("feishu-config-state"), !alerts ? "等待配置状态；Webhook 和签名密钥不会回显。" : "通知：" + (alerts.config?.feishu_enabled ? alerts.config?.enabled ? "已启用" : "已配置开启，监测关闭时不发送" : "未启用") + " · Webhook：" + (alerts.feishu_configured ? "已配置" : "未配置，不影响控制台监测") + " · 签名：" + (alerts.feishu_signing_configured ? "已配置" : "未配置") + "。留空保留，私密值不回显、不持久化到浏览器。");
    setText($("alert-draft-state"), !alerts ? "等待服务器配置。" : !configure ? "此部署未开放告警配置修改。" : state.alertConflict ? "服务器修订已变化；草稿保留，请重新载入后核对。" : state.alertDirty ? "有未保存修改 · 私密输入提交后即清空。" : "等待指标需达到最少样本数并持续超阈值才告警。");
    const alertFields = ["alerts-enabled", "feishu-enabled", "alert-wait", "alert-frame", "alert-hold", "alert-cooldown", "alert-samples", "feishu-clear-webhook", "feishu-clear-secret"];
    setDisabled(alertFields, !configure || state.alertSaving);
    $("feishu-webhook").disabled = !configure || state.alertSaving || $("feishu-clear-webhook").checked;
    $("feishu-secret").disabled = !configure || state.alertSaving || $("feishu-clear-secret").checked;
    $("alert-save-button").disabled = !configure || state.alertSaving || state.alertConflict || state.alertBaseRevision === undefined;
    $("alert-reload-button").disabled = !alerts || state.alertSaving;
    setText($("alert-save-button"), state.alertSaving ? "正在保存…" : "保存告警设置");
    setText($("alert-delivery-state"), alerts ? "最近通知成功：" + time(alerts.delivery?.last_success_at) + (alerts.delivery?.last_error_code ? " · 最近错误：" + errorText({ code: alerts.delivery.last_error_code }) : " · 无已报告的投递错误") : "尚无通知结果；不会发送测试消息。");
    const current = list(alerts?.active);
    if (!current.length) empty($("alerts-active"), alerts ? "当前没有活动告警；请检查监测是否开启、样本是否充足。飞书通知关闭不影响这里显示。" : "等待告警状态。");
    else patchRegion($("alerts-active"), current.map((item) => {
      const row = keyed(node("article", "alert-entry"), item.key), body = node("div");
      body.append(node("strong", "", alertKind(item.kind) + (item.worker_id ? " · " + shortID(item.worker_id) : "")), node("p", "", text(item.region) + " · 当前 " + alertValue(item, "value") + " / 阈值 " + alertValue(item, "threshold") + " · 自 " + time(item.since)), node("p", "", "来源 " + text(item.source) + " · 最近通知 " + time(item.last_notified_at)));
      row.append(body, badge(item.state === "pending" ? "持续观察" : item.state, item.state === "firing" ? "danger" : item.state === "unknown" ? "" : "warning")); return row;
    }));
    table($("alerts-history-table"), ["时间", "指标", "事件", "实例", "观测值"], list(alerts?.history).map((item, index) => ({ key: text(item.at) + "/" + text(item.key) + "/" + index, cells: [time(item.at), alertKind(item.kind), badge(item.event, item.event === "recovered" ? "good" : "danger"), text(item.worker_id), alertValue(item, "value")] })), "尚无告警变更记录。");
    table($("policy-audit-table"), ["时间", "修订", "操作者"], list(policy?.audit).map((item, index) => ({ key: text(item.after?.revision) + "/" + index, cells: [time(item.at), text(item.after?.revision), text(item.actor)] })), "尚无策略变更记录。");
  }
  async function reloadSettings(name) {
    const dirty = name === "policy" ? state.policyDirty : state.alertDirty;
    if (dirty && !await confirmAction("放弃本页未保存修改？", "重新载入会用服务器当前配置替换这份草稿。私密输入会清空，不会提交。", "", "重新载入", name)) return;
    const generation = state.sessionGeneration;
    await loadFeature(name, "v1/" + (name === "policy" ? "policy" : "alerts"), { fresh: true });
    if (!state.session || generation !== state.sessionGeneration || resource(name).error) return;
    if (name === "policy") hydratePolicy(true);
    else { clearFeishuInputs(); hydrateAlerts(true); }
    renderCurrentView();
  }
  async function savePolicy(event) {
    event.preventDefault();
    if (state.policySaving || !featureAvailable("policy", "can_manage") || state.policyConflict) return;
    if (!$("policy-form").reportValidity()) return;
    const cores = Number($("policy-cpu").value), millis = Math.round(cores * 1000), roomLimit = Number($("policy-rooms").value);
    if (!Number.isInteger(roomLimit) || !Number.isFinite(cores) || Math.abs(cores * 1000 - millis) > 0.000001) { toast("请按整数房间数和最多三位小数的 CPU 核数填写。", true); return; }
    const payload = { expected_revision: state.policyBaseRevision, rooms_per_instance: roomLimit, cpu_request_millicores: millis, cpu_limit_millicores: millis };
    if (!await confirmAction("保存新实例的目标策略？", "新实例将采用 " + roomLimit + " 间房、CPU request / limit 均为 " + decimal(cores, 3) + " 核。现有实例不会自动排空；需要更新时请等待当前对局结束并排空重建。", "", "保存目标策略", "policy")) return;
    if (!featureAvailable("policy", "can_manage") || state.policySaving) return;
    const generation = state.sessionGeneration; state.policySaving = true; renderCurrentView();
    try {
      await api("policy", { method: "POST", body: payload });
      if (!state.session || generation !== state.sessionGeneration) return;
      await loadFeature("policy", "v1/policy", { fresh: true });
      if (!state.session || generation !== state.sessionGeneration) return;
      state.policySaving = false;
      if (!resource("policy").error) hydratePolicy(true);
      toast("目标策略已保存，仅新实例采用；现有实例未自动排空。");
    } catch (error) {
      if (state.session && generation === state.sessionGeneration) { if (error.code === "policy_conflict" || error.status === 409) state.policyConflict = true; toast(errorText(error), true); }
    } finally { if (generation === state.sessionGeneration) { state.policySaving = false; if (state.session) renderCurrentView(); } }
  }
  function clearFeishuInputs() {
    $("feishu-webhook").value = ""; $("feishu-secret").value = "";
    $("feishu-clear-webhook").checked = false; $("feishu-clear-secret").checked = false;
  }
  async function saveAlerts(event) {
    event.preventDefault();
    if (state.alertSaving || !featureAvailable("alerts", "can_configure") || state.alertConflict || !$("alert-form").reportValidity()) return;
    const payload = { expected_revision: state.alertBaseRevision, enabled: $("alerts-enabled").checked, feishu_enabled: $("feishu-enabled").checked };
    const hasWebhook = !$("feishu-clear-webhook").checked && (featureData("alerts")?.feishu_configured === true || !!$("feishu-webhook").value.trim());
    if (payload.feishu_enabled && !hasWebhook) { toast(errorMessages.feishu_not_configured, true); $("feishu-webhook").focus(); return; }
    for (const [id, key] of [["alert-wait", "wait_p95_ms"], ["alert-frame", "frame_p99_ms"], ["alert-hold", "hold_seconds"], ["alert-cooldown", "cooldown_seconds"], ["alert-samples", "min_samples"]]) payload[key] = Number($(id).value);
    if (!await confirmAction("保存告警设置？", "控制台监测" + (payload.enabled ? "开启" : "关闭") + "，飞书通知" + (payload.feishu_enabled ? "开启" : "关闭") + "。Webhook / 签名留空时保留；勾选清除时才删除。保存不发送测试通知，也不会自动扩容或排空实例。", "", "保存告警设置", "alerts")) return;
    if (!featureAvailable("alerts", "can_configure") || state.alertSaving) return;
    if ($("feishu-clear-webhook").checked) payload.feishu_webhook = ""; else if ($("feishu-webhook").value.trim()) payload.feishu_webhook = $("feishu-webhook").value.trim();
    if ($("feishu-clear-secret").checked) payload.feishu_signing_secret = ""; else if ($("feishu-secret").value.trim()) payload.feishu_signing_secret = $("feishu-secret").value.trim();
    clearFeishuInputs();
    const generation = state.sessionGeneration; state.alertSaving = true; renderCurrentView();
    try {
      await api("alerts", { method: "POST", body: payload });
      if (!state.session || generation !== state.sessionGeneration) return;
      await loadFeature("alerts", "v1/alerts", { fresh: true });
      if (!state.session || generation !== state.sessionGeneration) return;
      state.alertSaving = false;
      if (!resource("alerts").error) hydrateAlerts(true);
      toast("告警设置已保存；未发送测试通知。");
    } catch (error) {
      if (state.session && generation === state.sessionGeneration) { if (error.code === "alerts_conflict" || error.status === 409) state.alertConflict = true; toast(errorText(error) + " 私密输入已清空，需要时请重新填写。", true); }
    } finally { delete payload.feishu_webhook; delete payload.feishu_signing_secret; if (generation === state.sessionGeneration) { state.alertSaving = false; if (state.session) renderCurrentView(); } }
  }
  function nodeTargetKey() { return [$("node-join-region").value, $("node-join-host").value.trim(), $("node-join-port").value].join("\n"); }
  function clearNodeAuthorization() {
    state.nodeScan = null; state.nodePreflight = null; state.nodeScanTarget = "";
    $("node-join-password").value = ""; $("node-fingerprint-confirmed").checked = false;
    $("node-fingerprint").replaceChildren();
  }
  function nodeJobs() {
    const data = list(featureData("onboarding")?.jobs);
    if (!state.currentJob) return data;
    return [state.currentJob, ...data.filter((job) => job.id !== state.currentJob.id)];
  }
  function jobRunning(job) {
    if (!job || ["AwaitingPreflight", "AwaitingApproval", "Ready", "Failed", "NeedsReview", "Expired"].includes(job.phase)) return false;
    return ["queued", "scanning", "preflighting", "installing", "verifying"].includes(job.state) || ["Scanning", "Preflighting", "Installing", "Verifying"].includes(job.phase);
  }
  function enrollmentLabel(job) { return ({ Scanning: "扫描主机指纹", AwaitingPreflight: "待核验指纹", Preflighting: "环境预检中", AwaitingApproval: "待确认安装", Installing: "安装中", Verifying: "验证节点", Ready: "节点已就绪", Failed: "任务失败", NeedsReview: "需人工复核", Expired: "任务已过期" })[job?.phase] || labels[job?.state] || text(job?.state); }
  function enrollmentFailed(job) { return ["failed", "needs_review", "expired"].includes(job?.state) || ["Failed", "NeedsReview", "Expired"].includes(job?.phase); }
  function acceptNodeScan(result, target) {
    if (!result || typeof result.scan_id !== "string" || !list(result.fingerprints).some((item) => typeof item.fingerprint === "string")) { const failure = new Error("invalid_scan"); failure.code = "upstream_invalid_response"; throw failure; }
    state.nodeScan = result; state.nodeScanTarget = target;
    $("node-fingerprint").replaceChildren(...list(result.fingerprints).filter((item) => typeof item.fingerprint === "string").map((item) => new Option(text(item.algorithm) + " · " + item.fingerprint, item.fingerprint)));
  }
  function acceptNodePreflight(result) {
    if (!result || typeof result.preflight_id !== "string" || typeof result.can_join !== "boolean") { const failure = new Error("invalid_preflight"); failure.code = "upstream_invalid_response"; throw failure; }
    state.nodePreflight = result;
  }
  function beginPendingEnrollment(result, kind, target, info) {
    if (typeof result.id !== "string" || !/^[a-f0-9]{32}$/.test(result.id)) { const failure = new Error("invalid_job"); failure.code = "upstream_invalid_response"; throw failure; }
    state.currentJob = { ...info, ...result }; state.pendingEnrollment = { id: result.id, kind, target };
    state.jobRequestedAt = 0;
  }
  function acceptEnrollmentJob(job) {
    const pending = state.pendingEnrollment;
    if (!pending || job.id !== pending.id) return;
    if (enrollmentFailed(job)) {
      // A completed read-only preflight may fail with useful checks; it never authorizes join.
      if (pending.kind === "preflight" && job.preflight?.can_join === false) acceptNodePreflight(job.preflight);
      state.pendingEnrollment = null; state.nodeAction = ""; return;
    }
    if (pending.kind === "scan" && job.scan) { acceptNodeScan(job.scan, pending.target); state.pendingEnrollment = null; state.nodeAction = ""; }
    else if (pending.kind === "preflight" && job.preflight) { acceptNodePreflight(job.preflight); state.pendingEnrollment = null; state.nodeAction = ""; }
  }
  function renderChecks(id, checks) {
    patchRegion($(id), list(checks).map((item, index) => {
      const row = keyed(node("div", "check-row"), text(item.name) + "/" + index), body = node("div");
      body.append(node("strong", "", text(item.name)), node("p", "", text(item.detail)));
      row.append(badge(item.ok === true ? "通过" : item.ok === false ? "未通过" : "未知", item.ok === true ? "good" : item.ok === false ? "danger" : ""), body); return row;
    }));
  }
  function renderOnboarding() {
    const capability = featureData("onboarding"), available = featureAvailable("onboarding", "enabled"), busy = state.nodeBusy || !!state.pendingEnrollment;
    errorBanner("onboarding-error", state.nodeError || resource("onboarding").error);
    $("node-action-status").hidden = !busy;
    setText($("node-action-status"), state.nodeAction === "preflight" ? "正在认证并执行只读预检；密码已从输入框清空，请稍候。" : state.nodeAction === "join" ? "正在提交安装任务，请稍候；Kubernetes 任务接受后由所选地域 controller 执行。" : "正在读取 SSH 主机指纹；此步骤未提交密码，请稍候。");
    setText($("onboarding-capability"), !capability ? "正在读取节点接入能力…" : !available ? "此部署未启用节点接入，仍可查看节点与实例。请由运维通过现有安装流程接入。" : "控制台通过 Kubernetes API 提交受限任务；所选地域管理节点的 controller 负责连接目标 VPS，执行预检与安装。当前仅支持 root；先核对主机指纹，再预检和确认安装。云防火墙与公网 UDP 连通性需另行验收。");
    const choices = list(capability?.regions);
    options("node-join-region", choices.map((item) => item.name), "选择地区");
    if (choices.length === 1 && !$("node-join-region").value && !state.nodeScan && $("node-join-region") !== document.activeElement) $("node-join-region").value = choices[0].name;
    const selected = choices.find((item) => item.name === $("node-join-region").value);
    setText($("node-region-plan"), selected ? "目标集群 " + text(selected.cluster_id) + " · API " + text(selected.server) + " · 游戏 UDP 端口 " + text(selected.game_port_min) + "–" + text(selected.game_port_max) : "选择地区后显示接入目标和游戏端口范围。");
    setDisabled(["node-join-region", "node-join-host", "node-join-port"], !available || busy);
    $("node-scan-button").disabled = !available || busy;
    setText($("node-scan-button"), state.nodeAction === "scan" ? "正在扫描…" : "扫描 SSH 主机指纹");
    const scan = state.nodeScan, preflight = state.nodePreflight, job = state.currentJob;
    $("node-identity-panel").hidden = !scan;
    if (scan) {
      setText($("node-scan-expiry"), text(scan.host) + ":" + text(scan.port) + " · 指纹记录有效至 " + time(scan.expires_at) + (expired(scan.expires_at) ? "（已过期，请重新扫描）" : ""));
      const option = $("node-fingerprint").selectedOptions[0]; setText($("node-fingerprint-preview"), option ? option.textContent : "尚无可选指纹。");
    }
    setDisabled(["node-fingerprint", "node-fingerprint-confirmed", "node-join-password"], !available || busy || !scan || expired(scan.expires_at));
    $("node-preflight-button").disabled = !available || busy || !scan || expired(scan.expires_at) || !$("node-fingerprint-confirmed").checked;
    setText($("node-preflight-button"), state.nodeAction === "preflight" ? "正在预检…" : "认证并执行只读预检");
    $("node-plan-panel").hidden = !preflight;
    if (preflight) {
      setText($("node-preflight-expiry"), "本次认证与计划有效至 " + time(preflight.expires_at) + (expired(preflight.expires_at) ? "（已过期，需重新认证）" : ""));
      setText($("node-preflight-status"), preflight.can_join ? "预检通过" : "预检未通过"); $("node-preflight-status").className = "badge " + (preflight.can_join ? "good" : "danger");
      patchRegion($("node-preflight-summary"), [detailGrid([["SSH 主机", preflight.host], ["地区 / 节点名", text(preflight.region) + " / " + text(preflight.node_name)], ["私网 IP / 网卡", text(preflight.private_ip) + " / " + text(preflight.interface)], ["外部 IP", preflight.external_ip], ["CPU", number(preflight.cpu_cores) ? decimal(preflight.cpu_cores, 2) + " 核" : "—"], ["内存", number(preflight.memory_mib) ? bytes(preflight.memory_mib * 1048576) : "—"], ["剩余磁盘", number(preflight.disk_free_gib) ? decimal(preflight.disk_free_gib) + " GiB" : "—"], ["已有安装", typeof preflight.existing_installation === "boolean" ? preflight.existing_installation ? "存在" : "无" : text(preflight.existing_installation)]])]);
      renderChecks("node-preflight-checks", preflight.checks);
      patchRegion($("node-install-plan"), list(preflight.plan).map((step, index) => keyed(node("li", "", text(step)), index)));
    }
    $("node-join-button").disabled = !available || busy || !preflight || preflight.can_join !== true || expired(preflight.expires_at) || nodeJobs().some(jobRunning);
    setText($("node-join-button"), state.nodeAction === "join" ? "正在提交…" : nodeJobs().some(jobRunning) ? "已有任务执行中" : "确认计划并加入集群");
    $("node-job-panel").hidden = !job;
    if (job) {
      setText($("node-job-state"), enrollmentLabel(job)); $("node-job-state").className = "badge " + (job.state === "ready" || job.phase === "Ready" ? "good" : enrollmentFailed(job) ? "danger" : "warning");
      patchRegion($("node-job-summary"), [detailGrid([["任务 ID", job.id], ["主机", job.host], ["地区 / 节点", text(job.region) + " / " + text(job.node_name)], ["当前步骤", job.stage || enrollmentLabel(job)], ["提交时间", time(job.created_at)], ["最近更新", time(job.updated_at)]])]);
      renderChecks("node-job-checks", job.checks);
      setText($("node-job-note"), job.state === "ready" ? "K3s 节点已就绪。公网 UDP 与真实游戏连接仍需实测，Ready 不代表玩家网络已验收。" : enrollmentFailed(job) ? "任务未完成；请先核对状态，不会自动重复安装。" + (job.error ? errorText({ code: job.error }) : "请核对上方检查结果后处理；不会自动重复安装。") : job.phase === "AwaitingPreflight" ? "指纹扫描完成；需核验主机身份后才提交密码，尚未安装。" : job.phase === "AwaitingApproval" ? "只读预检已完成，等待你确认安装计划，尚未加入集群。" : "Kubernetes 接入任务由所选地域 controller 继续执行；页面每 5 秒读取状态，关闭页面不会取消正在执行的任务。");
    }
    const jobs = nodeJobs(); setText($("node-job-count"), capability ? count(jobs.length) + " 个" : "等待数据");
    table($("node-jobs-table"), ["主机 / 地区", "节点", "状态", "步骤", "更新时间", "操作"], jobs.map((item) => ({ key: item.id, cells: [cell(text(item.host), text(item.region)), text(item.node_name), badge(enrollmentLabel(item)), text(item.stage || enrollmentLabel(item)), time(item.updated_at), enrollmentViewButton(item, busy)] })), capability ? "暂无接入任务。完成预检并确认安装后会创建任务。" : "尚未读取接入任务。");
    const phase = state.pendingEnrollment?.kind === "scan" || state.nodeAction === "scan" ? "scan" : state.pendingEnrollment?.kind === "preflight" || state.nodeAction === "preflight" ? "preflight" : preflight ? "plan" : scan ? "preflight" : jobRunning(job) || job?.state === "ready" ? "job" : "scan";
    for (const key of ["scan", "preflight", "plan", "job"]) $("join-step-" + key).classList.toggle("active", key === phase);
  }
  function enrollmentViewButton(item, disabled) {
    const action = button("查看任务", () => { state.currentJob = item; renderCurrentView(); fetchNodeJob(true); }, "button small quiet"); action.disabled = disabled; return action;
  }
  async function scanNode(event) {
    event.preventDefault();
    if (!featureAvailable("onboarding", "enabled") || state.nodeBusy || state.pendingEnrollment || !$("node-scan-form").reportValidity()) return;
    const payload = { region: $("node-join-region").value, host: $("node-join-host").value.trim(), port: Number($("node-join-port").value) }, target = nodeTargetKey();
    clearNodeAuthorization(); state.currentJob = null; state.nodeError = null; state.nodeBusy = true; state.nodeAction = "scan"; renderCurrentView();
    const generation = state.sessionGeneration;
    try {
      const result = await api("node-onboarding/scan", { method: "POST", body: payload });
      if (!state.session || generation !== state.sessionGeneration) return;
      if (result?.pending === true) beginPendingEnrollment(result, "scan", target, payload);
      else acceptNodeScan(result, target);
    } catch (error) { if (state.session && generation === state.sessionGeneration) state.nodeError = error; }
    finally { if (generation === state.sessionGeneration) { state.nodeBusy = false; if (!state.pendingEnrollment) state.nodeAction = ""; if (state.session) renderCurrentView(); } }
  }
  async function preflightNode(event) {
    event.preventDefault();
    if (!featureAvailable("onboarding", "enabled") || state.nodeBusy || state.pendingEnrollment || !state.nodeScan || expired(state.nodeScan.expires_at) || !$("node-preflight-form").reportValidity()) return;
    const scan = state.nodeScan, target = state.nodeScanTarget;
    const payload = { scan_id: scan.scan_id, username: "root", password: $("node-join-password").value, fingerprint: $("node-fingerprint").value };
    // A scan is single-use server-side, including failed authentication.
    clearNodeAuthorization(); state.nodeError = null; state.nodeBusy = true; state.nodeAction = "preflight"; renderCurrentView();
    const generation = state.sessionGeneration;
    try {
      const result = await api("node-onboarding/preflight", { method: "POST", body: payload, timeoutMs: 25000 });
      if (!state.session || generation !== state.sessionGeneration) return;
      if (result?.pending === true) beginPendingEnrollment(result, "preflight", target, { host: scan.host, region: $("node-join-region").value });
      else acceptNodePreflight(result);
    } catch (error) { if (state.session && generation === state.sessionGeneration) state.nodeError = error; }
    finally { delete payload.password; if (generation === state.sessionGeneration) { $("node-join-password").value = ""; state.nodeBusy = false; if (!state.pendingEnrollment) state.nodeAction = ""; if (state.session) renderCurrentView(); } }
  }
  async function joinNode() {
    const preflight = state.nodePreflight;
    if (!featureAvailable("onboarding", "enabled") || state.nodeBusy || !preflight || !preflight.can_join || expired(preflight.expires_at) || nodeJobs().some(jobRunning)) return;
    if (!await confirmAction("按已展示计划加入这个 VPS？", "将提交受限 Kubernetes 任务，由所选地域管理节点的 controller 使用刚才核验的主机身份和临时认证，在目标 VPS 执行上方安装计划并加入 K3s。不会购买服务器或修改云防火墙；节点 Ready 后仍需验证公网 UDP 游戏连接。", text(preflight.host) + " · " + text(preflight.region) + " · " + text(preflight.node_name), "确认安装并加入", "onboarding")) return;
    if (!featureAvailable("onboarding", "enabled") || state.nodeBusy || state.nodePreflight !== preflight || expired(preflight.expires_at)) return;
    const generation = state.sessionGeneration; state.nodeBusy = true; state.nodeAction = "join"; state.nodeError = null; renderCurrentView();
    try {
      const job = await api("node-onboarding/join", { method: "POST", body: { preflight_id: preflight.preflight_id } });
      if (!state.session || generation !== state.sessionGeneration) return;
      if (!job || typeof job.id !== "string" || typeof job.state !== "string") { const failure = new Error("invalid_job"); failure.code = "upstream_invalid_response"; throw failure; }
      state.currentJob = job; state.jobRequestedAt = Date.now(); clearNodeAuthorization();
      toast("接入任务已提交，安装与验证状态将每 5 秒更新。");
    } catch (error) { if (state.session && generation === state.sessionGeneration) state.nodeError = error; }
    finally { if (generation === state.sessionGeneration) { state.nodeBusy = false; if (!state.pendingEnrollment) state.nodeAction = ""; if (state.session) renderCurrentView(); } }
  }
  async function fetchNodeJob(manual = false) {
    const job = state.currentJob;
    if (!state.session || !job || state.jobBusy || (!manual && !state.pendingEnrollment && !jobRunning(job))) return;
    state.jobBusy = true; state.jobRequestedAt = Date.now(); const generation = state.sessionGeneration;
    try {
      const result = await api("node-onboarding/jobs?" + new URLSearchParams({ id: job.id }));
      if (!state.session || generation !== state.sessionGeneration || state.currentJob?.id !== job.id) return;
      if (!result || result.id !== job.id || typeof result.state !== "string") { const failure = new Error("invalid_job"); failure.code = "upstream_invalid_response"; throw failure; }
      state.currentJob = result; acceptEnrollmentJob(result); state.nodeError = null;
    } catch (error) { if (state.session && generation === state.sessionGeneration && state.currentJob?.id === job.id) state.nodeError = error; }
    finally { if (generation === state.sessionGeneration) { state.jobBusy = false; if (state.session && state.view === "onboarding") renderCurrentView(); } }
  }

  function detailGrid(pairs) {
    const grid = keyed(node("dl", "detail-grid"), "fields");
    for (const [label, value] of pairs) { const item = keyed(node("div"), label); item.append(node("dt", "", label), node("dd", "", text(value))); grid.append(item); }
    return grid;
  }
  function workerDetailSection(key, title, description) {
    const section = keyed(node("section"), key);
    section.append(node("h3", "detail-section", title));
    if (description) section.append(node("p", "inline-note", description));
    return section;
  }
  function workerTimingTable(metrics, key, title, description, definitions) {
    const section = workerDetailSection(key, title, description), wrapper = node("div", "table-wrap");
    wrapper.tabIndex = 0; wrapper.setAttribute("aria-label", title + "，可横向滚动查看各列");
    table(wrapper, ["阶段", "P50", "P95", "P99", "样本 / 窗口", "最近样本年龄"], definitions.map(([field, label, note]) => {
      const value = metrics[field], windowKnown = value?.window_seconds === 60, countKnown = Number.isInteger(value?.count) && value.count >= 0;
      const available = windowKnown && countKnown && value.count > 0 && [value.p50, value.p95, value.p99, value.last_sample_age_seconds].every((item) => number(item) && item >= 0) && value.last_sample_age_seconds <= value.window_seconds;
      const missing = !value ? "未上报" : countKnown && value.count === 0 ? "暂无样本" : "数据未知";
      return { key: field, cells: [cell(label, note), ...["p50", "p95", "p99"].map((quantile) => node("span", "nowrap", available ? ms(value[quantile]) : "—")), cell(countKnown ? count(value.count) + " 个" : "未知", windowKnown ? "60 秒窗口" : "窗口未知"), cell(available ? seconds(value.last_sample_age_seconds) : "—", available ? "按该次心跳上报" : missing)] };
    }), "尚无阶段采样。");
    section.append(wrapper); return section;
  }
  function workerDiagnostics(worker, pod) {
    const metrics = worker.metrics || {}, terminal = WORKER_TERMINAL.has(worker.state), parts = [];
    const resources = workerDetailSection("worker-resources", "进程与容器", "Pod 指标来自 Kubernetes Metrics API，汇总其返回的各容器用量；游戏进程内存由宿主心跳上报。两者采样时间与统计口径不同，不能直接相减计算 sidecar 内存。");
    resources.append(detailGrid([["Pod CPU", cpu(pod?.cpu_millicores)], ["Pod 内存 · 容器合计", bytes(pod?.memory_bytes)], ["游戏进程内存 · 心跳", number(metrics.memory_bytes) && metrics.memory_bytes > 0 ? bytes(metrics.memory_bytes) : "未知"], ["帧间隔 P99", ms(metrics.frame_p99_ms)], ["待写回结果", count(metrics.pending_results)], ["容器重启次数", count(pod?.restarts)]]));
    parts.push(resources);
    const concurrency = workerDetailSection("worker-concurrency", "工作线程与队列", "实际工作线程数来自游戏宿主上报，不从 CPU 核数或预算推断。未上报显示未知。");
    const concurrencyTable = node("div", "table-wrap"), threads = (value) => Number.isInteger(value) && value > 0 ? count(value) : "未知";
    table(concurrencyTable, ["任务", "实际工作线程", "正在运行", "等待队列"], [
      { key: "simulation", cells: ["模拟", threads(metrics.simulation_workers), count(metrics.simulation_active), count(metrics.simulation_pending)] },
      { key: "audit", cells: ["审计", threads(metrics.audit_workers), count(metrics.audit_active), count(metrics.audit_pending)] }
    ], "尚无线程数据。");
    concurrency.append(concurrencyTable, node("p", "table-note", "最老模拟排队：" + seconds(metrics.simulation_oldest_seconds))); parts.push(concurrency);
    const reference = workerDetailSection("worker-timing-reference", "采样与告警参考 · 只读", (terminal ? "实例已退出，以下仅为末次心跳记录。" : "以下是最近心跳中报告的采样窗口，样本年龄不是页面实时计时。") + "窗口为最近 60 秒；缺失或零样本时分位数未知，不能解释为 0 ms。页面不会据此自动扩容或排空实例。");
    const alerts = state.snapshotError ? null : state.snapshot?.alerts, config = alerts?.config;
    if (!config) reference.append(node("p", "inline-note", "告警阈值未知：本次快照未返回可用的只读告警配置。没有用预设值替代线上设置。"));
    else {
      const configured = (value, format) => number(value) ? format(value) : "未知";
      reference.append(node("p", "inline-note", "来源：本次快照的 alerts.config · 修订 " + text(alerts.revision) + " · 监测" + (config.enabled === true ? "已启用" : config.enabled === false ? "已关闭（以下为已保存配置）" : "状态未知") + "。客户端两类等待的 P95 阈值：" + configured(config.wait_p95_ms, ms) + "；帧间隔 P99 阈值：" + configured(config.frame_p99_ms, ms) + "；最少 " + configured(config.min_samples, count) + " 个样本，持续 " + configured(config.hold_seconds, seconds) + "。服务器 ACK 与模拟阶段没有对应的独立告警阈值。"));
    }
    parts.push(reference);
    parts.push(workerTimingTable(metrics, "timing-client", "客户端 · 停球后等待", "客户端自报：从视觉播放结束到可以继续操作或收到结算。包含客户端侧观察，不能当作服务器执行时间；告警状态仍以服务端监测结果为准。", [
      ["client_presentation_to_ready_ms", "停球 → Ready", "可再次操作"],
      ["client_presentation_to_settlement_ms", "停球 → 结算", "收到最终结算"]
    ]));
    parts.push(workerTimingTable(metrics, "timing-server", "服务器 · 播放 ACK 后处理", "首个 ACK 阶段包含等待另一名玩家确认播放结束；末个 ACK 阶段从双方有效确认后开始。这里只反映服务端阶段，不等于完整玩家体验或网络 RTT。", [
      ["server_first_ack_to_ready_ms", "首个 ACK → Ready", "包含等待另一名玩家"],
      ["server_first_ack_to_settlement_ms", "首个 ACK → 结算", "包含等待另一名玩家"],
      ["server_last_ack_to_ready_ms", "末个 ACK → Ready", "双方有效 ACK 后"],
      ["server_last_ack_to_settlement_ms", "末个 ACK → 结算", "双方有效 ACK 后"]
    ]));
    parts.push(workerTimingTable(metrics, "timing-simulation", "模拟 · 排队与执行", "工作队列与计算阶段的独立采样，用于定位服务器处理瓶颈；不包含完整网络传输和客户端渲染。", [
      ["simulation_queue_ms", "模拟排队", "等待工作线程"],
      ["simulation_work_ms", "模拟执行", "工作线程计算"]
    ]));
    return parts;
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
      } else if (detail.kind === "node") {
        const host = allNodes().find((item) => nodeKey(item) === detail.id);
        setText($("detail-eyebrow"), "VPS / Kubernetes 节点"); setText($("detail-title"), text(host?.name || detail.id));
        if (!host) { empty(body, "当前快照中没有这个 Node 记录。记录消失不代表云 VPS 已被关闭或删除。"); return; }
        const address = nodePublicIP(host), region = regions().find((item) => item.name === host.region), pods = list(region?.pods).filter((item) => item.node === host.name);
        content.push(detailGrid([["节点名", host.name], ["地区", host.region], ["角色标签", nodeRole(host.role)], ["Ready 状态", nodeReadyStatus(host) + " · " + nodeReadyLabel(host)], ["Ready 最近变更", time(host.ready_last_transition_at)], ["调度暂停", host.unschedulable ? "是" : "否"], [address.label, address.value], ["内网 IP · InternalIP", nodePrivateIP(host)], ["CPU 使用 / 可分配", cpu(host.cpu_millicores) + " / " + cpu(host.cpu_allocatable_millicores)], ["内存使用 / 可分配", bytes(host.memory_bytes) + " / " + bytes(host.memory_allocatable_bytes)], ["观察范围内 Pod", count(pods.length)]]));
        content.push(keyed(node("p", "table-note", "地址来自 Node.status.addresses；ExternalIP 未上报时才显示明确标注的管理备注。备注不修改节点网络，也不能用于退役确认。Ready=False / Unknown 不等于云 VPS 已关机、到期或永久退役。"), "node-address-note"));
        const podTable = keyed(node("div", "table-wrap"), "node-pods");
        table(podTable, ["可见 Pod", "命名空间", "状态", "关联游戏服"], pods.map((pod) => { const worker = workers().find((item) => item.pod === pod.name && item.region === host.region); return { key: pod.namespace + "/" + pod.name, cells: [pod.name, pod.namespace, badge(pod.phase), worker ? identifierButton(worker.id, () => openDetail("worker", worker.id)) : "未关联 Fleet 实例"] }; }), "当前观察范围没有此节点的 Pod；不代表节点没有其他系统工作负载。");
        content.push(podTable, retirementControl(host));
      } else {
        const worker = workers().find((item) => item.id === detail.id);
        setText($("detail-eyebrow"), "游戏服实例"); setText($("detail-title"), text(worker?.pod || detail.id));
        if (!worker) { empty(body, "当前 Fleet 状态中没有此实例。若已退出，可在日志页手填 Pod 名查询归档。"); return; }
        const pod = podFor(worker), terminal = WORKER_TERMINAL.has(worker.state);
        content.push(detailGrid([["实例 ID", worker.id], ["状态", labels[workerStatus(worker)] || workerStatus(worker)], ["地区", worker.region], ["Pod / 命名空间", text(worker.pod) + " / " + text(pod?.namespace)], ["节点", pod?.node], ["联机版本", worker.build_hash], ["地址", worker.host ? text(worker.host) + ":" + text(worker.port) : "—"], ["房间 / 容量", count(worker.occupied_rooms) + " / " + count(worker.max_rooms)], [terminal ? "末次在线玩家" : "在线玩家", count(worker.player_count)], ["创建时间", time(worker.created_at)], ["最近心跳", time(worker.last_heartbeat)], ["错误 / Pod 原因", worker.error || pod?.reason]]));
        content.push(...workerDiagnostics(worker, pod));
        const actions = keyed(node("div", "detail-actions"), "actions");
        actions.append(button("游戏服日志", () => openLogs(worker, "game")), button("Agones sidecar 日志", () => openLogs(worker, "agones-gameserver-sidecar")));
        if (canDrainWorker(worker)) actions.append(managementButton("排空实例", () => drainWorker(worker)));
        content.push(actions);
      }
      patchRegion(body, content);
    });
    state.renderArea = null;
    const selectedNode = detail.kind === "node" ? allNodes().find((item) => nodeKey(item) === detail.id) : null;
    const stale = !!state.snapshotError || (detail.kind === "node" ? !selectedNode || selectedNode.stale : fleet()?.ok !== true), observed = detail.kind === "node" ? state.snapshot?.observed_at : state.goodFleetAt;
    setText($("detail-status"), state.deferred.has("details") ? "正在选择或操作详情，局部更新暂缓" : (detail.kind === "node" && stale ? "节点保留上次成功记录；当前读取未确认" : (stale ? "上次成功记录 · " : "快照 · ") + time(observed, true)));
    if (!state.deferred.has("details")) state.renderedAt.details = observed;
  }
  function confirmAction(title, description, target, label, scope = "fleet") {
    const dialog = $("confirm-dialog");
    if (dialog.open) return Promise.resolve(false);
    setText($("confirm-title"), title); setText($("confirm-description"), description); setText($("confirm-target"), target || ""); $("confirm-target").hidden = !target; setText($("confirm-submit"), label);
    dialog.returnValue = "cancel"; state.confirmScope = scope;
    return new Promise((resolve) => { dialog.addEventListener("close", () => { state.confirmScope = null; resolve(dialog.returnValue === "confirm"); }, { once: true }); dialog.showModal(); });
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
    if (state.view === "logs") fetchLogs({ manual: true }); else if (state.view === "policy") refreshPolicy(); else if (state.view === "onboarding") { refreshOnboarding(); fetchNodeJob(true); } else requestSnapshot([state.view]);
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
  $("policy-form").addEventListener("input", () => { state.policyDirty = true; renderPolicy(); });
  $("policy-cpu-preset").addEventListener("click", () => { $("policy-cpu").value = "1.5"; state.policyDirty = true; renderPolicy(); $("policy-cpu").focus(); });
  $("policy-form").addEventListener("submit", savePolicy);
  $("policy-reload-button").addEventListener("click", () => reloadSettings("policy"));
  $("alert-form").addEventListener("input", () => { state.alertDirty = true; renderPolicy(); });
  $("alert-form").addEventListener("submit", saveAlerts);
  $("alert-reload-button").addEventListener("click", () => reloadSettings("alerts"));
  for (const [toggle, input] of [["feishu-clear-webhook", "feishu-webhook"], ["feishu-clear-secret", "feishu-secret"]]) $(toggle).addEventListener("change", () => { if ($(toggle).checked) $(input).value = ""; state.alertDirty = true; renderPolicy(); });
  $("node-retire-form").addEventListener("submit", submitRetirement);
  $("node-retire-form").addEventListener("input", renderRetirementConfirmation);
  $("node-retire-cancel").addEventListener("click", () => { if (!state.retirement.busy) $("node-retire-dialog").close(); });
  $("node-retire-dialog").addEventListener("cancel", (event) => { if (state.retirement.busy) event.preventDefault(); });
  $("node-retire-dialog").addEventListener("close", () => { state.retirement.confirmation = null; $("node-retire-form").reset(); });
  $("node-scan-form").addEventListener("submit", scanNode);
  $("node-preflight-form").addEventListener("submit", preflightNode);
  $("node-join-button").addEventListener("click", joinNode);
  for (const id of ["node-join-region", "node-join-host", "node-join-port"]) $(id).addEventListener($(id).tagName === "SELECT" ? "change" : "input", () => {
    if (state.nodeScanTarget && nodeTargetKey() !== state.nodeScanTarget) { clearNodeAuthorization(); state.nodeError = null; }
    renderOnboarding();
  });
  $("node-fingerprint").addEventListener("change", () => { $("node-fingerprint-confirmed").checked = false; state.nodePreflight = null; renderOnboarding(); });
  $("node-fingerprint-confirmed").addEventListener("change", renderOnboarding);
  window.addEventListener("pagehide", () => { clearFeishuInputs(); $("node-join-password").value = ""; });
  document.addEventListener("focusout", flushInteractions);
  document.addEventListener("selectionchange", flushInteractions);
  // A single cheap scheduler chooses due areas. No page reloads, no global DOM
  // rebuild, no polling hidden tabs, and at most one snapshot request in flight.
  function scheduleRefresh() {
    if (document.hidden || !state.session) return;
    const now = Date.now(), targets = [], interval = state.intervals[state.view];
    if (interval > 0 && now - (state.requestedAt[state.view] || 0) >= interval * 1000) {
      if (state.view === "logs") { if (state.logQuery && !state.logDraftDirty) fetchLogs(); }
      else if (state.view === "policy") refreshPolicy();
      else if (state.view === "onboarding") refreshOnboarding();
      else targets.push(state.view);
    }
    if (state.detail && $("detail-dialog").open && state.intervals.details > 0 && now - (state.requestedAt.details || 0) >= state.intervals.details * 1000) targets.push("details");
    if (targets.length) requestSnapshot(targets);
    if ((state.pendingEnrollment || jobRunning(state.currentJob)) && now - state.jobRequestedAt >= 5000) fetchNodeJob();
    if (retirementJobRunning(state.retirement.job) && now - state.retirement.requestedAt >= 5000) fetchRetirementJob();
  }
  document.addEventListener("visibilitychange", () => { if (!document.hidden) scheduleRefresh(); });
  setInterval(scheduleRefresh, 1000);
  constrainLogWindow();
  $("login-button").disabled = true;
  api("session", { allowUnauthorized: true }).then(showSession).catch((error) => {
    showLogin(error.status === 401 ? undefined : errorText(error));
  }).finally(() => { $("login-button").disabled = false; });
})();
