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
    Succeeded: "已完成", Failed: "失败", Unknown: "未知"
  };
  const viewInfo = {
    overview: ["运行总览", "战斗服运行总览", "从匹配入场到实例回收，查看集群当前状态。"],
    rooms: ["对局与玩家", "对局与玩家", "追踪房间分配、玩家连接以及原席位恢复。"],
    infrastructure: ["实例与节点", "实例与节点", "查看进程负载、房间容量与 VPS 资源使用。"],
    logs: ["运行日志", "运行日志", "读取实时容器日志，或查询退出实例的历史归档。"]
  };
  const state = {
    session: null, snapshot: null, view: "overview", snapshotBusy: false,
    logBusy: false, logController: null, logs: "", logQuery: "", detail: null,
    toastTimer: null, mutationBusy: false, sessionGeneration: 0
  };

  function node(tag, className, text) {
    const item = document.createElement(tag);
    if (className) item.className = className;
    if (text !== undefined && text !== null) item.textContent = String(text);
    return item;
  }
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
    const warning = new Set(["draining", "stopping", "launching", "starting", "pending", "Pending", "assigned", "allocating"]);
    return node("span", "badge " + (tone || (good.has(value) ? "good" : bad.has(value) ? "danger" : warning.has(value) ? "warning" : "")), labels[value] || text(value));
  }
  function button(label, callback, className = "button small") {
    const item = node("button", className, label);
    item.type = "button";
    item.addEventListener("click", callback);
    return item;
  }
  function empty(container, message) { container.replaceChildren(node("p", "empty-state", message)); }
  function options(id, values, placeholder) {
    const select = $(id);
    const distinct = [...new Set(values.filter((value) => typeof value === "string" && value))].sort();
    const signature = JSON.stringify([distinct, placeholder]);
    if (select.dataset.options === signature) return;
    const previous = select.value;
    const children = [];
    if (placeholder) children.push(new Option(placeholder, ""));
    for (const value of distinct) children.push(new Option(id === "room-state" ? labels[value] || value : value, value));
    select.replaceChildren(...children);
    if (distinct.includes(previous) || previous === "" && placeholder) select.value = previous;
    select.dataset.options = signature;
  }
  function cell(primary, secondary, className = "") {
    const item = node("div", className);
    item.append(primary instanceof Node ? primary : node("span", "", text(primary)));
    if (secondary) item.append(node("span", "secondary", secondary));
    return item;
  }
  function table(container, headers, rows, message) {
    if (!rows.length) { empty(container, message); return; }
    const tableNode = node("table");
    const head = node("thead"), heading = node("tr");
    for (const title of headers) { const th = node("th", "", title); th.scope = "col"; heading.append(th); }
    head.append(heading); tableNode.append(head);
    const body = node("tbody");
    for (const row of rows) {
      const tr = node("tr");
      for (const value of row) { const td = node("td"); td.append(value instanceof Node ? value : node("span", "nowrap", text(value))); tr.append(td); }
      body.append(tr);
    }
    tableNode.append(body); container.replaceChildren(tableNode);
  }
  function toast(message, error = false) {
    clearTimeout(state.toastTimer);
    $("toast").textContent = message;
    $("toast").classList.toggle("error", error);
    $("toast").hidden = false;
    state.toastTimer = setTimeout(() => { $("toast").hidden = true; }, error ? 8000 : 4500);
  }
  const errorMessages = {
    invalid_credentials: "账号或密码不正确。", invalid_login: "账号或密码不正确。",
    rate_limited: "请求过于频繁，请稍后重试。", login_rate_limited: "登录尝试过于频繁，请稍后重试。",
    unauthorized: "登录状态已失效，请重新登录。", session_expired: "登录已过期，请重新登录。",
    forbidden: "当前请求未获授权。请重新登录后重试。", invalid_csrf: "操作校验已失效，请重新登录后重试。",
    management_disabled: "当前控制台为只读模式，未启用实例管理操作。",
    invalid_request: "请求参数不正确，请检查填写内容。", invalid_parameters: "请求参数不正确，请检查填写内容。",
    not_found: "未找到对应记录。实例退出后请尝试历史日志。",
    logs_unavailable: "日志源暂时不可用，请稍后重试。", metrics_unavailable: "指标采集暂时不可用。",
    loki_unavailable: "历史日志服务暂时不可用。", region_unavailable: "该地区暂时不可用。",
    timeout: "请求超时，请确认 SSH 隧道保持连接。", request_timeout: "请求超时，请稍后重试。",
    network_error: "连接失败，请确认 SSH 隧道保持连接。"
  };
  function errorText(error) {
    if (errorMessages[error.code]) return errorMessages[error.code];
    if (error.status === 401) return "登录状态已失效，请重新登录。";
    if (error.status === 403) return "操作未获授权，请重新登录后重试。";
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
    state.session = null; state.snapshot = null; state.logs = ""; state.logQuery = "";
    state.sessionGeneration += 1;
    state.logController?.abort();
    $("app").hidden = true; $("login-view").hidden = false; $("login-state").textContent = message;
    $("log-auto").checked = false; $("log-output").textContent = "尚未查询日志。";
    $("password").value = "";
    for (const id of ["detail-dialog", "confirm-dialog"]) if ($(id).open) $(id).close();
    state.detail = null;
    renderManagementAccess(); renderInfrastructure();
  }
  function showSession(session) {
    if (!session || typeof session.username !== "string" || typeof session.csrf_token !== "string" || !session.csrf_token) throw new Error("invalid_session_response");
    state.session = session; state.sessionGeneration += 1;
    $("session-user").textContent = session.username;
    $("login-view").hidden = true; $("app").hidden = false; $("password").value = "";
    $("login-error").hidden = true;
    setView(viewInfo[location.hash.slice(1)] ? location.hash.slice(1) : "overview", false);
    refreshSnapshot();
  }
  function setView(view, updateHash = true) {
    if (!viewInfo[view]) return;
    state.view = view;
    for (const name of Object.keys(viewInfo)) $("view-" + name).hidden = name !== view;
    for (const item of document.querySelectorAll(".nav-item")) {
      const selected = item.dataset.view === view;
      item.classList.toggle("active", selected);
      if (selected) item.setAttribute("aria-current", "page"); else item.removeAttribute("aria-current");
    }
    $("page-name").textContent = viewInfo[view][0]; $("page-title").textContent = viewInfo[view][1]; $("page-description").textContent = viewInfo[view][2];
    if (updateHash) history.replaceState(null, "", "#" + view);
  }
  const fleet = () => state.snapshot?.fleet;
  const canManage = () => fleet()?.can_manage === true;
  const workers = () => list(fleet()?.workers);
  const rooms = () => list(fleet()?.rooms);
  const regions = () => list(state.snapshot?.regions);
  function podFor(worker) {
    const region = regions().find((item) => item.name === worker.region);
    return list(region?.pods).find((item) => item.name === worker.pod || item.worker_id === worker.id);
  }
  function workerStatus(worker) { return worker.draining && !WORKER_TERMINAL.has(worker.state) ? "draining" : worker.state; }
  function identifierButton(value, callback) {
    const item = button(shortID(value), callback, "id-button identifier"); item.title = text(value); return item;
  }
  async function refreshSnapshot() {
    if (!state.session || state.snapshotBusy) return;
    state.snapshotBusy = true; $("refresh-button").disabled = true;
    const generation = state.sessionGeneration;
    try {
      const snapshot = await api("snapshot");
      if (!state.session || generation !== state.sessionGeneration) return;
      if (!snapshot || typeof snapshot !== "object" || !snapshot.fleet) throw new Error("invalid_snapshot_response");
      state.snapshot = snapshot; $("connection-banner").hidden = true;
      $("refresh-indicator").className = "status-dot";
      $("snapshot-time").textContent = "更新于 " + time(snapshot.observed_at, true);
      renderSnapshot();
    } catch (error) {
      if (state.session && generation === state.sessionGeneration) {
        $("connection-banner").textContent = errorText(error) + (state.snapshot ? " 页面保留上次快照，当前显示可能已过时。" : " 尚未取得集群数据。");
        $("connection-banner").hidden = false; $("refresh-indicator").className = "status-dot failed";
      }
    } finally { state.snapshotBusy = false; $("refresh-button").disabled = false; }
  }
  function renderSnapshot() {
    renderManagementAccess();
    const names = [...regions().map((item) => item.name), ...workers().map((item) => item.region)];
    options("room-region", names, "全部地区"); options("worker-region", names, "全部地区");
    options("log-region", names.length ? names : ["us-west"]);
    options("room-state", rooms().map((item) => item.state), "全部状态");
    const pods = new Map();
    for (const worker of workers()) if (worker.pod) pods.set(worker.pod, worker.region);
    for (const region of regions()) for (const pod of list(region.pods)) if (pod.name) pods.set(pod.name, region.name);
    $("pod-options").replaceChildren(...[...pods].map(([name, region]) => { const item = node("option"); item.value = name; item.label = text(region); return item; }));
    renderOverview(); renderRooms(); renderInfrastructure();
    if (state.detail && $("detail-dialog").open) renderDetail();
  }
  function renderManagementAccess() {
    const enabled = canManage();
    $("read-only-banner").hidden = enabled;
    $("retry-creation-button").hidden = !enabled;
    $("worker-management-note").textContent = "CPU 按实例统计，无法单独测量每个房间。" + (enabled ? "排空保留现有对局，待完成后退出实例。" : "当前控制台只读，可查看实例状态与日志。");
    if (!enabled && $("confirm-dialog").open) $("confirm-dialog").close("cancel");
  }
  function metric(label, value, unit, note) {
    const item = node("article", "metric-card"), title = node("div", "metric-label", label), amount = node("div", "metric-value", count(value));
    if (unit) amount.append(node("span", "metric-unit", unit));
    item.append(title, amount, node("p", "metric-note", note)); return item;
  }
  function healthItem(title, description, tone = "") {
    const item = node("div", "health-item"), dot = node("span", "status-dot " + tone), content = node("div");
    dot.setAttribute("aria-hidden", "true"); content.append(node("strong", "", title), node("p", "", description)); item.append(dot, content); return item;
  }
  function renderOverview() {
    const ok = fleet()?.ok === true, live = workers().filter((worker) => !WORKER_TERMINAL.has(worker.state)), liveRooms = rooms().filter((room) => !ROOM_TERMINAL.has(room.state));
    $("overview-metrics").replaceChildren(
      metric("在线玩家", ok ? sum(live, "player_count") : undefined, "人", "来自游戏服最近一次心跳"),
      metric("活跃房间", ok ? liveRooms.length : undefined, "间", "包含等待分配与等待入场"),
      metric("运行实例", ok ? live.length : undefined, "个", "已就绪 " + (ok ? count(live.filter((worker) => worker.ready && !worker.draining).length) : "—") + " · 上限 " + count(fleet()?.max_processes)),
      metric("实例已占用房间", ok ? sum(live, "occupied_rooms") : undefined, "/ " + (ok ? count(sum(live, "max_rooms")) : "—"), "占用包含已预留房间，受心跳刷新影响")
    );
    const blocked = typeof fleet()?.creation_blocked_reason === "string" && fleet().creation_blocked_reason !== "";
    $("creation-banner").hidden = !blocked; $("creation-reason").textContent = blocked ? fleet().creation_blocked_reason : "";
    const regionList = $("region-overview");
    if (!regions().length) empty(regionList, "尚无区域数据。");
    else regionList.replaceChildren(...regions().map((region) => {
      const items = live.filter((worker) => worker.region === region.name), localRooms = liveRooms.filter((room) => room.region === region.name);
      const row = node("div", "region-row"), name = node("div", "region-name"), dot = node("span", "status-dot " + (region.ok ? "" : "failed")), numbers = node("div", "region-numbers");
      dot.setAttribute("aria-hidden", "true"); name.append(dot, node("strong", "", text(region.name)));
      for (const [label, value] of [["玩家", ok ? sum(items, "player_count") : undefined], ["房间", ok ? localRooms.length : undefined], ["实例", ok ? items.length : undefined]]) { const item = node("span", "", label); item.prepend(node("strong", "", count(value))); numbers.append(item); }
      row.append(name, numbers); return row;
    }));
    const notes = [healthItem(ok ? "Fleet 控制通道可用" : "Fleet 控制通道不可用", ok ? "快照版本 " + text(fleet().revision) + "。单实例配置上限 " + count(fleet().rooms_per_process) + " 间房。" : "错误代码：" + text(fleet()?.error), ok ? "" : "failed")];
    for (const region of regions()) {
      if (!region.ok) notes.push(healthItem(region.name + "：集群读取失败", "错误代码：" + text(region.error), "failed"));
      else if (!region.metrics_ok) notes.push(healthItem(region.name + "：资源指标未就绪", "节点与 Pod 可见，CPU / 内存采集暂时不可用。", "stale"));
      else notes.push(healthItem(region.name + "：资源指标可用", "节点 " + count(list(region.nodes).length) + " 个，Pod " + count(list(region.pods).length) + " 个。"));
      const warningEvents = list(region.events).filter((event) => event.type === "Warning").slice(0, 3);
      for (const event of warningEvents) notes.push(healthItem(text(event.reason) + " · " + text(event.object_name), text(event.message) + " · " + time(event.time), "stale"));
    }
    notes.push(healthItem("历史日志保留 " + count(state.snapshot?.log_retention_days) + " 天", "仅包含启用采集后的记录。房间状态列表并非同期限的历史档案。"));
    $("health-notes").replaceChildren(...notes);
    if (!ok) empty($("active-workers"), "无法读取 Fleet 实例状态。");
    else if (!live.length) empty($("active-workers"), "当前没有运行实例。有玩家匹配后将按容量策略创建。");
    else $("active-workers").replaceChildren(...live.slice(0, 8).map(workerCard));
  }
  function workerCard(worker) {
    const pod = podFor(worker), metrics = worker.metrics || {}, card = node("article", "worker-card"), top = node("div", "worker-card-top"), title = node("h3", "", text(worker.pod || worker.id));
    title.title = text(worker.id); top.append(title, badge(workerStatus(worker)));
    card.append(top, node("p", "worker-card-region", text(worker.region) + " · " + text(pod?.node)));
    const details = node("dl", "worker-card-metrics");
    for (const [label, value] of [["房间 / 容量", count(worker.occupied_rooms) + " / " + count(worker.max_rooms)], ["CPU", cpu(pod?.cpu_millicores)], ["帧间隔 P99", ms(metrics.frame_p99_ms)]]) { const pair = node("div"); pair.append(node("dt", "", label), node("dd", "", value)); details.append(pair); }
    const actions = node("div", "worker-card-actions"); actions.append(button("查看实例", () => openDetail("worker", worker.id)), button("运行日志", () => openLogs(worker), "button small quiet"));
    card.append(details, actions); return card;
  }
  function renderRooms() {
    const query = $("room-search").value.trim().toLowerCase(), region = $("room-region").value, status = $("room-state").value;
    const filtered = rooms().filter((room) => (!region || room.region === region) && (!status || room.state === status) && (!query || [room.id, room.allocation_id, room.worker_id, ...list(room.players).map((player) => player.user_id)].some((value) => String(value || "").toLowerCase().includes(query))));
    $("room-count").textContent = fleet()?.ok ? count(filtered.length) + " / " + count(rooms().length) : "不可用";
    table($("rooms-table"), ["房间 / 分配", "状态", "地区", "玩家连接", "所属实例", "创建时间", "操作"], filtered.map((room) => {
      const players = list(room.players), connected = players.filter((player) => player.connected === true).length;
      return [cell(identifierButton(room.id, () => openDetail("room", room.id)), "分配 " + shortID(room.allocation_id)), badge(room.state), text(room.region), count(connected) + " / " + count(players.length), identifierButton(room.worker_id, () => openDetail("worker", room.worker_id)), time(room.created_at), button("房间详情", () => openDetail("room", room.id), "button small quiet")];
    }), fleet()?.ok ? "没有符合筛选条件的房间。" : "Fleet 房间数据暂时不可用。");
  }
  function renderInfrastructure() {
    const query = $("worker-search").value.trim().toLowerCase(), region = $("worker-region").value, include = $("include-stopped").checked;
    const filtered = workers().filter((worker) => (include || !WORKER_TERMINAL.has(worker.state)) && (!region || worker.region === region) && (!query || [worker.id, worker.pod, podFor(worker)?.node].some((value) => String(value || "").toLowerCase().includes(query))));
    $("worker-count").textContent = fleet()?.ok ? count(filtered.length) + " / " + count(workers().length) : "不可用";
    table($("workers-table"), ["实例 / 节点", "状态", "地区", "房间 / 容量", "玩家", "Pod CPU / 内存", "帧间隔 P99", "模拟运行 / 排队", "操作"], filtered.map((worker) => {
      const pod = podFor(worker), metrics = worker.metrics || {}, actions = node("div", "cell-actions");
      actions.append(button("日志", () => openLogs(worker), "button small quiet"));
      if (canManage() && !WORKER_TERMINAL.has(worker.state) && !worker.draining) actions.append(button("排空", () => drainWorker(worker), "button small"));
      return [cell(identifierButton(worker.pod || worker.id, () => openDetail("worker", worker.id)), text(pod?.node)), badge(workerStatus(worker)), text(worker.region), count(worker.occupied_rooms) + " / " + count(worker.max_rooms), count(worker.player_count), cell(cpu(pod?.cpu_millicores), bytes(pod?.memory_bytes)), ms(metrics.frame_p99_ms), count(metrics.simulation_active) + " / " + count(metrics.simulation_pending), actions];
    }), fleet()?.ok ? "没有符合筛选条件的实例。" : "Fleet 实例数据暂时不可用。");
    const nodes = regions().flatMap((region) => list(region.nodes).map((item) => ({ ...item, region: region.name })));
    table($("nodes-table"), ["节点 / 地区", "调度状态", "角色", "CPU 使用 / 可分配", "内存使用 / 可分配", "Pod 数量"], nodes.map((item) => {
      const region = regions().find((region) => region.name === item.region);
      const status = item.ready !== true ? badge("未就绪", "danger") : item.unschedulable ? badge("已暂停调度", "warning") : badge("可调度", "good");
      const podCount = region?.ok ? list(region.pods).filter((pod) => pod.node === item.name).length : undefined;
      return [cell(text(item.name), text(item.region)), status, text(item.role), cell(cpu(item.cpu_millicores), "可分配 " + cpu(item.cpu_allocatable_millicores)), cell(bytes(item.memory_bytes), "可分配 " + bytes(item.memory_allocatable_bytes)), count(podCount)];
    }), "暂无节点数据；集群读取失败时不会将缺失指标显示为 0。");
  }
  function detailGrid(pairs) {
    const grid = node("dl", "detail-grid");
    for (const [label, value] of pairs) { const item = node("div"); item.append(node("dt", "", label), node("dd", "", text(value))); grid.append(item); }
    return grid;
  }
  function openDetail(kind, id) {
    if (!id) { toast("此记录尚未关联实例。", true); return; }
    state.detail = { kind, id }; renderDetail();
    if (!$("detail-dialog").open) $("detail-dialog").showModal();
  }
  function renderDetail() {
    const detail = state.detail, body = $("detail-body");
    if (!detail) return;
    if (detail.kind === "room") {
      const room = rooms().find((item) => item.id === detail.id);
      $("detail-eyebrow").textContent = "房间详情"; $("detail-title").textContent = text(detail.id);
      if (!room) { empty(body, "该房间已不在当前 Fleet 状态保留窗口内。"); return; }
      body.replaceChildren(detailGrid([["状态", labels[room.state] || room.state], ["地区", room.region], ["分配 ID", room.allocation_id], ["所属实例", room.worker_id], ["房间 Epoch", room.epoch], ["创建时间", time(room.created_at)], ["分配过期时间", time(room.expires_at)], ["终态时间", time(room.terminal_at)], ["错误代码", room.error]]));
      body.append(node("h3", "detail-section", "玩家 · Nakama 用户 ID"));
      const tableWrap = node("div", "table-wrap");
      table(tableWrap, ["玩家 ID", "座位", "当前连接", "曾经入场", "恢复截止"], list(room.players).map((player) => [node("span", "identifier detail-player-id", text(player.user_id)), count(player.seat), player.connected === true ? badge("connected") : player.connected === false ? badge("disconnected", player.ever_connected ? "warning" : "") : "—", player.ever_connected === true ? "是" : player.ever_connected === false ? "否" : "—", cell(time(player.reconnect_until), player.connected ? "当前已连接" : remaining(player.reconnect_until))]), "此房间暂无玩家记录。");
      body.append(tableWrap, node("p", "table-note", "座位按协议从 0 编号。连接状态来自游戏服心跳；房间列表不提供单房 CPU 指标。"));
      const worker = workers().find((item) => item.id === room.worker_id);
      if (worker) { const actions = node("div", "detail-actions"); actions.append(button("查看所属实例", () => openDetail("worker", worker.id)), button("查看实例日志", () => openLogs(worker))); body.append(actions); }
    } else {
      const worker = workers().find((item) => item.id === detail.id);
      $("detail-eyebrow").textContent = "游戏服实例"; $("detail-title").textContent = text(worker?.pod || detail.id);
      if (!worker) { empty(body, "当前 Fleet 状态中没有此实例。若已退出，可在日志页手填 Pod 名查询归档。"); return; }
      const pod = podFor(worker), metrics = worker.metrics || {};
      body.replaceChildren(detailGrid([["实例 ID", worker.id], ["状态", labels[workerStatus(worker)] || workerStatus(worker)], ["地区", worker.region], ["Pod / 命名空间", text(worker.pod) + " / " + text(pod?.namespace)], ["节点", pod?.node], ["联机版本", worker.build_hash], ["地址", worker.host ? text(worker.host) + ":" + text(worker.port) : "—"], ["房间 / 容量", count(worker.occupied_rooms) + " / " + count(worker.max_rooms)], ["在线玩家", count(worker.player_count)], ["Pod CPU", cpu(pod?.cpu_millicores)], ["Pod 内存", bytes(pod?.memory_bytes)], ["游戏进程内存", bytes(metrics.memory_bytes)], ["帧间隔 P99", ms(metrics.frame_p99_ms)], ["模拟运行 / 排队", count(metrics.simulation_active) + " / " + count(metrics.simulation_pending)], ["最老模拟排队", seconds(metrics.simulation_oldest_seconds)], ["审计运行 / 排队", count(metrics.audit_active) + " / " + count(metrics.audit_pending)], ["待写回结果", count(metrics.pending_results)], ["容器重启次数", count(pod?.restarts)], ["创建时间", time(worker.created_at)], ["最近心跳", time(worker.last_heartbeat)], ["错误 / Pod 原因", worker.error || pod?.reason]]));
      const actions = node("div", "detail-actions");
      actions.append(button("游戏服日志", () => openLogs(worker, "game")), button("Agones sidecar 日志", () => openLogs(worker, "agones-gameserver-sidecar")));
      if (canManage() && !WORKER_TERMINAL.has(worker.state) && !worker.draining) actions.append(button("排空实例", () => drainWorker(worker)));
      body.append(actions);
    }
  }
  function confirmAction(title, description, target, label) {
    const dialog = $("confirm-dialog");
    if (dialog.open) return Promise.resolve(false);
    $("confirm-title").textContent = title; $("confirm-description").textContent = description; $("confirm-target").textContent = target || ""; $("confirm-target").hidden = !target; $("confirm-submit").textContent = label;
    dialog.returnValue = "cancel";
    return new Promise((resolve) => { dialog.addEventListener("close", () => resolve(dialog.returnValue === "confirm"), { once: true }); dialog.showModal(); });
  }
  async function drainWorker(worker) {
    if (state.mutationBusy || !state.session) return;
    if (!canManage()) { toast(errorMessages.management_disabled, true); return; }
    const accepted = await confirmAction("排空这个实例？", "确认后停止向该实例分配新房间。已有对局、有效重连预留和结果回写会继续完成；不会强制删除正在进行的对局。", worker.pod || worker.id, "确认排空");
    if (!accepted || !state.session || state.mutationBusy) return;
    if (!canManage()) { toast(errorMessages.management_disabled, true); return; }
    state.mutationBusy = true;
    try { await api("drain", { method: "POST", body: { worker_id: worker.id } }); toast("已请求排空，等待实例完成现有对局后退出。"); await refreshSnapshot(); }
    catch (error) { toast(errorText(error), true); }
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
  function invalidateLogs() {
    state.logQuery = ""; state.logController?.abort();
    state.logs = ""; $("log-output").textContent = "筛选已更改，请重新查询。"; $("log-status").textContent = "筛选已更改"; $("copy-logs-button").disabled = true;
  }
  async function openLogs(worker, container = "game") {
    if (!worker.pod) { toast("此实例尚无 Pod 名称。", true); return; }
    if ($("detail-dialog").open) $("detail-dialog").close();
    $("log-region").value = worker.region; $("log-pod").value = worker.pod; $("log-namespace").value = podFor(worker)?.namespace || "agones-games";
    $("log-container").value = container; $("log-mode").value = WORKER_TERMINAL.has(worker.state) ? "history" : "live";
    $("log-search").value = ""; constrainLogWindow(); invalidateLogs(); setView("logs");
    await fetchLogs(true);
  }
  async function fetchLogs(explicit = false) {
    if (!state.session) return;
    if (!$("log-form").checkValidity()) { if (explicit) $("log-form").reportValidity(); return; }
    const query = logParameters().toString();
    if (state.logBusy) { if (query !== state.logQuery) state.logController?.abort(); return; }
    state.logBusy = true; state.logQuery = query; state.logController = new AbortController();
    const controller = state.logController, generation = state.sessionGeneration;
    $("fetch-logs-button").disabled = true; $("log-status").textContent = "正在读取日志…";
    try {
      const response = await api("logs?" + query, { signal: controller.signal });
      if (!state.session || generation !== state.sessionGeneration || query !== logParameters().toString() || controller.signal.aborted) return;
      const entries = list(response?.entries);
      const lines = entries.map((entry) => (typeof entry.timestamp === "string" ? entry.timestamp + "  " : "") + (typeof entry.text === "string" ? entry.text : ""));
      state.logs = lines.join("\n");
      const scroll = $("log-output").scrollTop;
      $("log-output").textContent = state.logs || "此时间范围没有匹配日志。历史记录仅从启用采集开始。";
      $("log-output").scrollTop = $("log-follow").checked ? $("log-output").scrollHeight : scroll;
      $("copy-logs-button").disabled = !state.logs;
      const source = response?.source === "loki" ? "历史归档" : response?.source === "kubernetes" ? "实时容器" : "日志";
      $("log-status").textContent = source + " · " + count(entries.length) + " 行 · " + time(response?.observed_at, true) + (response?.truncated ? " · 达到查询上限，可缩小时间范围" : "");
    } catch (error) {
      if (!controller.signal.aborted && state.session && generation === state.sessionGeneration && query === logParameters().toString()) {
        $("log-status").textContent = errorText(error) + (state.logs ? " 当前保留上次结果。" : "");
        if (explicit) toast(errorText(error), true);
      }
    } finally {
      if (state.logController === controller) { state.logBusy = false; $("fetch-logs-button").disabled = false; }
    }
  }

  $("login-form").addEventListener("submit", async (event) => {
    event.preventDefault(); $("login-button").disabled = true; $("login-error").hidden = true;
    try { showSession(await api("login", { method: "POST", body: { username: $("username").value.trim(), password: $("password").value }, allowUnauthorized: true })); }
    catch (error) { $("login-error").textContent = error.status === 401 ? "账号或密码不正确。" : errorText(error); $("login-error").hidden = false; }
    finally { $("login-button").disabled = false; }
  });
  $("logout-button").addEventListener("click", async () => {
    $("logout-button").disabled = true;
    try { await api("logout", { method: "POST", body: {} }); showLogin("已退出。通过 SSH 隧道重新登录即可继续管理。"); }
    catch (error) { toast(errorText(error), true); }
    finally { $("logout-button").disabled = false; }
  });
  $("refresh-button").addEventListener("click", () => refreshSnapshot());
  for (const item of document.querySelectorAll("[data-view]")) item.addEventListener("click", () => setView(item.dataset.view));
  window.addEventListener("hashchange", () => { if (state.session) setView(location.hash.slice(1), false); });
  for (const id of ["room-search", "room-region", "room-state"]) $(id).addEventListener(id.endsWith("search") ? "input" : "change", renderRooms);
  for (const id of ["worker-search", "worker-region", "include-stopped"]) $(id).addEventListener(id.endsWith("search") ? "input" : "change", renderInfrastructure);
  $("detail-close").addEventListener("click", () => $("detail-dialog").close());
  $("detail-dialog").addEventListener("close", () => { state.detail = null; });
  $("retry-creation-button").addEventListener("click", async () => {
    if (state.mutationBusy || !state.session) return;
    if (!canManage()) { toast(errorMessages.management_disabled, true); return; }
    if (!await confirmAction("允许重试创建实例？", "清除创建暂停状态后，Fleet 会根据当前匹配需求和容量策略重新尝试创建。请先确认导致暂停的问题已处理。", "", "确认重试") || !state.session || state.mutationBusy) return;
    if (!canManage()) { toast(errorMessages.management_disabled, true); return; }
    state.mutationBusy = true; $("retry-creation-button").disabled = true;
    try { await api("retry-creation", { method: "POST", body: {} }); toast("已请求重试创建。后续状态将自动刷新。"); await refreshSnapshot(); }
    catch (error) { toast(errorText(error), true); }
    finally { state.mutationBusy = false; $("retry-creation-button").disabled = false; }
  });
  $("log-form").addEventListener("submit", (event) => { event.preventDefault(); fetchLogs(true); });
  for (const id of ["log-region", "log-pod", "log-namespace", "log-container", "log-mode", "log-minutes", "log-limit", "log-search"]) {
    $(id).addEventListener($(id).tagName === "INPUT" ? "input" : "change", () => { if (id === "log-mode") constrainLogWindow(); invalidateLogs(); });
  }
  $("log-auto").addEventListener("change", () => { if ($("log-auto").checked) fetchLogs(true); });
  $("copy-logs-button").addEventListener("click", async () => {
    try { await navigator.clipboard.writeText(state.logs); toast("日志已复制。"); }
    catch (_) { toast("浏览器未允许自动复制，请在日志区手动选择文本。", true); }
  });
  document.addEventListener("visibilitychange", () => { if (!document.hidden && state.session) refreshSnapshot(); });
  setInterval(() => {
    if (document.hidden || !state.session) return;
    refreshSnapshot();
    if (state.view === "logs" && $("log-auto").checked && $("log-pod").value.trim()) fetchLogs();
  }, 5000);
  constrainLogWindow();
  $("login-button").disabled = true;
  api("session", { allowUnauthorized: true }).then(showSession).catch((error) => {
    showLogin(error.status === 401 ? undefined : errorText(error));
  }).finally(() => { $("login-button").disabled = false; });
})();
