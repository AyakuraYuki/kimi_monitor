const $ = selector => document.querySelector(selector);
const svgNS = "http://www.w3.org/2000/svg";
const pages = {
  overview: ["监控概览", "查看最近一次余额采集、今日消耗和企业微信通知。"],
  tokens: ["令牌管理", "管理参与余额采集的 API Key、备注与额度查询状态。"],
  consumption: ["每日消耗", "按北京时间查询每个令牌的每日消耗。"],
  settings: ["通知设置", "配置余额监控报告的企业微信接收地址。"],
};
const states = {
  normal: "正常", low: "余额不足", exhausted: "已用尽", error: "采集异常", pending: "待采集", paused: "已暂停",
};
const dayFormat = new Intl.DateTimeFormat("en-CA", {
  timeZone: "Asia/Shanghai", year: "numeric", month: "2-digit", day: "2-digit",
});
const timestampFormat = new Intl.DateTimeFormat("zh-CN", {
  timeZone: "Asia/Shanghai", year: "numeric", month: "2-digit", day: "2-digit",
  hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false,
});
const collator = new Intl.Collator("zh-CN", {numeric: true, sensitivity: "base"});
const dialog = $("#dialog");
let session = null;
let routeVersion = 0;
let active = "overview";
let toastTimer, pollTimer;
let tableSequence = 0;
let dialogBusy = false;

function el(tag, cls = "", ...children) {
  const node = document.createElement(tag);
  if (cls) node.className = cls;
  node.append(...children.filter(child => child != null));
  return node;
}
function icon(name, extra = "") {
  const svg = document.createElementNS(svgNS, "svg");
  svg.setAttribute("class", ("icon " + extra).trim());
  svg.setAttribute("aria-hidden", "true");
  const use = document.createElementNS(svgNS, "use");
  use.setAttribute("href", "/assets/icons.svg#" + name);
  svg.append(use);
  return svg;
}
function button(label, symbol, action, cls = "btn") {
  const btn = el("button", cls, symbol ? icon(symbol) : null, label);
  btn.type = "button";
  if (action) btn.addEventListener("click", () => action(btn));
  return btn;
}
function link(label, symbol, route, cls = "btn") {
  const anchor = el("a", cls, symbol ? icon(symbol) : null, label);
  anchor.href = "#" + route;
  return anchor;
}
function iconButton(label, symbol, action) {
  const btn = button("", symbol, action, "icon-button");
  btn.setAttribute("aria-label", label);
  btn.title = label;
  return btn;
}
function notice(message) {
  clearTimeout(toastTimer);
  $("#notice").textContent = message;
  toastTimer = setTimeout(() => { $("#notice").textContent = ""; }, 4500);
}
function problem(region, message) {
  region.hidden = !message;
  region.textContent = message || "";
}
function errorBox() {
  const box = el("p", "error");
  box.setAttribute("role", "alert");
  box.hidden = true;
  box.tabIndex = -1;
  return box;
}
function timeText(value) {
  return value ? timestampFormat.format(new Date(value)) : "尚未采集";
}
function timeNode(value) {
  const node = el("time", "", timeText(value));
  if (value) node.dateTime = value;
  return node;
}
function today() { return dayFormat.format(new Date()); }
function previousDays(date, count) {
  const value = new Date(date + "T00:00:00Z");
  value.setUTCDate(value.getUTCDate() - count);
  return value.toISOString().slice(0, 10);
}
function micros(value) {
  if (value == null) return null;
  const [whole, fraction = ""] = String(value).replace(/^-/, "").split(".");
  const units = BigInt(whole) * 1000000n + BigInt(fraction.padEnd(6, "0"));
  return String(value).startsWith("-") ? -units : units;
}
function money(value) {
  if (value == null) return "—";
  const units = typeof value === "bigint" ? value : micros(value);
  const abs = units < 0n ? -units : units;
  const decimal = (abs % 1000000n).toString().padStart(6, "0").replace(/0+$/, "").padEnd(2, "0");
  return (units < 0n ? "−" : "") + "¥" + (abs / 1000000n).toLocaleString("zh-CN") + "." + decimal;
}
function sumKnown(items, key) {
  const known = items.filter(item => item[key] != null);
  return known.length ? known.reduce((sum, item) => sum + micros(item[key]), 0n) : null;
}
function badge(status, title = states[status]) {
  return el("span", "status " + status, title || "未知");
}
function intervalLabel(seconds) {
  if (seconds < 60) return seconds + " 秒";
  if (seconds % 3600 === 0) return seconds / 3600 + " 小时";
  return Number((seconds / 60).toFixed(1)) + " 分钟";
}
async function api(url, {method = "GET", data, csv = false} = {}) {
  const headers = {Accept: csv ? "text/csv" : "application/json"};
  if (data !== undefined) headers["Content-Type"] = "application/json";
  if (session?.csrf_token && method !== "GET") headers["X-CSRF-Token"] = session.csrf_token;
  let result;
  try {
    result = await fetch(url, {method, headers, credentials: "same-origin", cache: "no-store",
      body: data === undefined ? undefined : JSON.stringify(data), signal: AbortSignal.timeout(csv ? 90000 : 30000)});
  } catch (error) {
    throw new Error(error.name === "TimeoutError" ? "请求超时，请重试。" : "连接失败，请检查网络后重试。");
  }
  if (!result.ok) {
    if (result.status === 401 && session?.authenticated) {
      showLogin({...session, authenticated: false, csrf_token: ""}, "登录已过期，请重新登录。");
    }
    const body = await result.json().catch(() => ({}));
    throw new Error(body.error || "请求失败，请重试。");
  }
  if (csv) return result.blob();
  if (result.status === 204) return null;
  const text = await result.text();
  return text ? JSON.parse(text) : null;
}
function setBusy(control, state) {
  control.disabled = state;
  if (state) control.setAttribute("aria-busy","true");
  else control.removeAttribute("aria-busy");
}
function showLogin(data, message = "") {
  routeVersion++;
  clearTimeout(pollTimer);
  session = data;
  if (dialog.open) dialog.close();
  $("#shell").hidden = true;
  $("#content #view").replaceChildren();
  $("#who").textContent = "";
  $("#notice").textContent = "";
  $("#boot").hidden = true;
  $("#login").hidden = false;
  $("#local-form").hidden = !data.local_enabled;
  $("#sso").hidden = !data.oidc_enabled;
  $("#login-divider").hidden = !(data.local_enabled && data.oidc_enabled);
  $("#sso").classList.toggle("primary", !data.local_enabled);
  $("#local-form").reset();
  $("#password").type = "password";
  $("#show-password").replaceChildren(icon("eye"));
  $("#show-password").setAttribute("aria-label", "显示密码");
  $("#show-password").setAttribute("aria-pressed", "false");
  document.title = "登录控制台 · Kimi Monitor";
  problem($("#login-error"), message || ((!data.oidc_enabled && !data.local_enabled) ? "当前没有可用的登录方式，请联系管理员检查配置。" : ""));
  $("#login h1").focus();
}
function enter(data) {
  session = data;
  $("#boot").hidden = true;
  $("#login").hidden = true;
  $("#local-form").reset();
  $("#shell").hidden = false;
  $("#who").textContent = data.username;
  loadPage(true);
}
async function bootstrap() {
  try {
    const data = await api("/api/session");
    const url = new URL(location.href);
    const failure = url.searchParams.get("login_error") === "oidc";
    if (failure) { url.searchParams.delete("login_error"); history.replaceState(null, "", url); }
    if (data.authenticated) enter(data);
    else showLogin(data, failure ? "单点登录未完成，请确认账号已获授权后重试。" : "");
  } catch (error) {
    $("#boot").replaceChildren(el("p", "error", error.message), button("重新连接", "refresh", bootstrap));
  }
}
$("#show-password").addEventListener("click", event => {
  const show = $("#password").type === "password";
  $("#password").type = show ? "text" : "password";
  event.currentTarget.replaceChildren(icon(show ? "eye-off" : "eye"));
  event.currentTarget.setAttribute("aria-label", show ? "隐藏密码" : "显示密码");
  event.currentTarget.setAttribute("aria-pressed", String(show));
});
$("#local-form").addEventListener("submit", async event => {
  event.preventDefault();
  const submit = event.submitter;
  setBusy(submit, true);
  problem($("#login-error"), "");
  try {
    enter(await api("/api/login", {method:"POST", data:{username:$("#username").value, password:$("#password").value}}));
  } catch (error) {
    problem($("#login-error"), error.message);
    $("#password").focus();
  } finally { setBusy(submit, false); }
});
$("#logout").addEventListener("click", async event => {
  const btn = event.currentTarget;
  setBusy(btn, true);
  try {
    await api("/api/logout", {method:"POST"});
    showLogin({...session, authenticated:false, csrf_token:""});
  } catch (error) { notice(error.message); }
  finally { setBusy(btn, false); }
});
$("#skip").addEventListener("click", event => { event.preventDefault(); $("#content").focus(); });
dialog.querySelector(".close-dialog").addEventListener("click", () => { if (!dialogBusy) dialog.close(); });
dialog.addEventListener("cancel", event => { if (dialogBusy) event.preventDefault(); });
dialog.addEventListener("close", () => { $("#dialog-body").replaceChildren(); dialogBusy = false; });
function openDialog(title, ...children) {
  $("#dialog-title").textContent = title;
  $("#dialog-body").replaceChildren(...children);
  dialog.classList.toggle("preview", title === "通知原文");
  dialog.showModal();
}
function formField(id, title, type, hint) {
  const label = el("label", "", title);
  label.htmlFor = id;
  const input = type === "textarea" ? el("textarea") : el("input");
  input.id = id;
  if (type !== "textarea") input.type = type;
  const helper = hint ? el("p", "hint", hint) : null;
  if (helper) {
    helper.id = id + "-hint";
    input.setAttribute("aria-describedby", helper.id);
  }
  return {input, group: el("div", "field", label, input, helper)};
}
function confirmForm(label, action, {danger = false, fields = []} = {}) {
  const form = el("form");
  form.method = "post";
  const error = errorBox();
  const submit = button(label, null, null, "btn " + (danger ? "danger" : "primary"));
  submit.type = "submit";
  const cancel = button("取消", null, () => dialog.close());
  if (danger) cancel.autofocus = true;
  form.append(...fields, error, el("div", "form-footer", cancel, submit));
  form.addEventListener("submit", async event => {
    event.preventDefault();
    dialogBusy = true;
    cancel.disabled = true;
    setBusy(submit, true);
    problem(error, "");
    try { await action(); }
    catch (failure) { problem(error, failure.message); error.focus(); }
    finally { dialogBusy = false; cancel.disabled = false; setBusy(submit, false); }
  });
  return form;
}
function showTokenForm(token) {
  const editing = !!token;
  const key = formField("token-key", editing ? "令牌名称" : "API Key", editing ? "text" : "password",
    editing ? "API Key 在添加后保持固定。" : "填写以 sk- 开头的普通 Kimi API Key。保存后仅显示打码名称。");
  key.input.readOnly = editing;
  key.input.value = token ? "#" + token.id + " · " + token.name : "";
  key.input.required = !editing;
  key.input.autocomplete = "off";
  key.input.spellcheck = false;
  key.input.maxLength = 4096;
  if (!editing) key.input.minLength = 15;
  const remark = formField("token-remark", "令牌备注（选填）", "textarea", "最多 256 个字符，可留空。");
  remark.input.value = token?.remark || "";
  const form = confirmForm(editing ? "保存备注" : "添加令牌", async () => {
    const note = remark.input.value.trim();
    if ([...note].length > 256) throw new Error("备注最多可包含 256 个字符。");
    const path = editing ? "/api/tokens/" + token.id : "/api/tokens";
    await api(path, {method:editing ? "PATCH" : "POST", data:editing ? {remark:note} : {api_key:key.input.value.trim(), remark:note}});
    dialog.close();
    notice(editing ? "令牌备注已更新。" : "令牌已添加，将在下一轮采集时查询余额。");
    await loadPage(false);
  }, {fields:[key.group, remark.group]});
  openDialog(editing ? "编辑令牌备注" : "添加监控令牌", form);
  (editing ? remark.input : key.input).focus();
}
function monitoringBadge(token) {
  const state = badge(token.monitoring_enabled ? "normal" : "paused", token.monitoring_enabled ? "已开启" : "已暂停");
  state.classList.add("monitoring-status");
  return state;
}
function updateMonitoringButton(btn, token) {
  const label = (token.monitoring_enabled ? "暂停" : "开启") + " #" + token.id + " " + token.name + " 的 API 额度查询";
  btn.replaceChildren(icon(token.monitoring_enabled ? "pause" : "play"));
  btn.setAttribute("aria-label", label);
  btn.title = label;
}
function monitoringButton(token) {
  const btn = iconButton("", null, async current => {
    const enabled = !token.monitoring_enabled;
    setBusy(current, true);
    try {
      await api("/api/tokens/" + token.id + "/monitoring", {method:"PUT", data:{enabled}});
      token.monitoring_enabled = enabled;
      current.closest("tr")?.querySelector(".monitoring-status")?.replaceWith(monitoringBadge(token));
      updateMonitoringButton(current, token);
      notice(enabled ? "已开启 API 额度查询，将从下一轮开始采集。" : "已暂停 API 额度查询，历史余额和消耗记录仍会保留。");
    } catch (error) { notice(error.message); }
    finally { setBusy(current, false); }
  });
  updateMonitoringButton(btn, token);
  return btn;
}
function deleteToken(token) {
  const selected = el("div", "selected-token", tokenLabel(token, true));
  const help = el("p", "dialog-help", "删除将一并清除该令牌的余额和消耗历史。此操作无法撤销。");
  const form = confirmForm("删除令牌", async () => {
    await api("/api/tokens/" + token.id, {method:"DELETE"});
    dialog.close();
    notice("令牌已删除。");
    await loadPage(false);
  }, {danger:true});
  openDialog("删除监控令牌？", selected, help, form);
}

function panel(title, symbol, trailing) {
  const heading = el("h2", "", icon(symbol, "panel-icon"), title);
  const header = el("div", "panel-head", heading, trailing);
  return el("section", "panel", header);
}
function empty(title, description, symbol, action) {
  return el("div", "empty", el("span", "icon-tile", icon(symbol)), el("h3", "", title),
    el("p", "", description), action);
}
function compare(a, b, direction) {
  if (a == null) return b == null ? 0 : 1;
  if (b == null) return -1;
  if (typeof a === "string") return direction * collator.compare(a, b);
  return direction * (a > b ? 1 : a < b ? -1 : 0);
}
// Build only the visible page and keep its existing rows when the sort order changes.
function dataTable(title, columns, rows, {kind = "", initialKey, descending = false} = {}) {
  const table = el("table", kind);
  table.id = "table-" + ++tableSequence;
  const caption = table.createCaption();
  caption.className = "sr-only";
  caption.id = table.id + "-caption";
  caption.textContent = title;
  const region = el("div", "table-region", table);
  region.tabIndex = 0;
  region.setAttribute("role", "region");
  region.setAttribute("aria-labelledby", caption.id);
  const header = table.createTHead().insertRow();
  const body = table.createTBody();
  const status = el("span", "small");
  let sortColumn = initialKey ? columns.find(col => col.key === initialKey) : null;
  let direction = descending ? -1 : 1;
  let ordered = rows;
  let page = 0;
  let visible = new Map();
  const pageSize = 25;
  const back = button("上一页", null, () => { page--; renderPage(); });
  const next = button("下一页", null, () => { page++; renderPage(); });
  const footer = el("div", "pager", status, el("div", "actions", back, next));
  footer.hidden = rows.length <= pageSize;
  function renderPage() {
    const pageRows = ordered.slice(page * pageSize, (page + 1) * pageSize);
    const keep = new Map();
    for (const item of pageRows) {
      let row = visible.get(item);
      if (!row) {
        row = el("tr");
        columns.forEach((col, index) => {
          const cell = el(index === 0 ? "th" : "td", col.cls || "");
          if (index === 0) cell.scope = "row";
          cell.append(col.render(item));
          row.append(cell);
        });
      }
      if (row.parentNode === body && typeof body.moveBefore === "function") body.moveBefore(row, null);
      else body.append(row);
      keep.set(item, row);
    }
    for (const [item, row] of visible) if (!keep.has(item)) row.remove();
    visible = keep;
    status.textContent = "第 " + (page + 1) + " / " + Math.ceil(rows.length / pageSize) + " 页 · 共 " + rows.length + " 条";
    back.disabled = page === 0;
    next.disabled = (page + 1) * pageSize >= rows.length;
  }
  function sort(announce = false) {
    if (sortColumn) {
      ordered = [...rows].sort((a, b) => compare(sortColumn.value(a), sortColumn.value(b), direction));
      for (const [i, th] of [...header.cells].entries()) {
        if (columns[i] === sortColumn) th.setAttribute("aria-sort", direction === 1 ? "ascending" : "descending");
        else th.removeAttribute("aria-sort");
      }
      if (announce) $("#sort-notice").textContent = "已按" + sortColumn.title + (direction === 1 ? "升序" : "降序") + "排列。";
    }
    page = 0;
    renderPage();
  }
  function sortBy(btn) {
    const col = columns[btn.closest("th").cellIndex];
    direction = col === sortColumn ? -direction : 1;
    sortColumn = col;
    sort(true);
  }
  for (const col of columns) {
    const th = el("th", col.cls || "");
    th.scope = "col";
    if (col.value) {
      const btn = button(col.title, null, null, "");
      btn.append(icon("sort"));
      btn.setAttribute("commandfor", table.id);
      btn.setAttribute("command", "--sort");
      if (!("commandForElement" in HTMLButtonElement.prototype)) btn.addEventListener("click", () => sortBy(btn));
      th.append(btn);
    } else th.textContent = col.title;
    header.append(th);
  }
  table.addEventListener("command", event => { if (event.command === "--sort") sortBy(event.source); });
  sort();
  return el("div", "", region, footer);
}
function tokenLabel(item, includeRemark = false) {
  const name = el("div", "token-identity", el("span", "token-name", item.name),
    el("span", "token-id", "#" + (item.token_id ?? item.id)));
  if (includeRemark) name.append(el("small", "small", item.remark || "无备注"));
  return name;
}
function amountColumn(key, title) {
  return {key, title, cls:"align-right", value:row => micros(row[key]), render:row => el("span", "num", money(row[key]))};
}
function tokenTable(tokens, managing) {
  if (!tokens.length) {
    return empty("还没有监控令牌", "添加第一个 API Key 后，即可在下一轮采集中查看余额。", "token",
      button("添加令牌", "plus", () => showTokenForm(null), "btn primary"));
  }
  let columns;
  if (managing) {
    columns = [
      {key:"name", title:"令牌名称", value:row => row.name, render:row => tokenLabel(row)},
      {key:"remark", title:"令牌备注", value:row => row.remark, render:row => el("span", "remark", row.remark || "—")},
      {key:"created", title:"添加时间", value:row => Date.parse(row.created_at), render:row => timeNode(row.created_at)},
      {key:"monitoring", title:"API 额度查询", value:row => row.monitoring_enabled, render:row => monitoringBadge(row)},
      {title:"操作", cls:"align-right", render:row => el("div", "row-actions",
        iconButton("编辑 #" + row.id + " " + row.name + " 的备注", "edit", () => showTokenForm(row)),
        monitoringButton(row),
        iconButton("删除 #" + row.id + " " + row.name, "trash", () => deleteToken(row)))},
    ];
  } else {
    columns = [
      {key:"name", title:"令牌 / 备注", value:row => row.name, render:row => tokenLabel(row, true)},
      {key:"status", title:"状态", value:row => states[row.status], render:row => badge(row.status)},
      amountColumn("available", "可用余额"), amountColumn("cash", "现金"), amountColumn("voucher", "代金券"),
      amountColumn("today_consumption", "今日消耗"),
      {key:"checked_at", title:"最近采集", value:row => row.checked_at ? Date.parse(row.checked_at) : null,
        render:row => el("div", "", row.checked_at ? timeNode(row.checked_at) : row.monitoring_enabled ? "等待首轮" : "尚未采集",
          row.monitoring_enabled && row.status === "error" && row.error ? el("small", "small", row.error) : null,
          row.monitoring_enabled && row.status === "error" && row.balance_at ? el("small", "small", "余额采集于 " + timeText(row.balance_at)) : null,
          !row.monitoring_enabled ? el("small", "small", "已暂停查询，历史数据不再更新") : null)},
    ];
  }
  return dataTable(managing ? "监控令牌列表" : "令牌余额明细", columns, tokens, {kind:managing ? "" : "balances"});
}
function metric(title, symbol, val, note, large = false) {
  const value = el("div", large ? "hero-value" : "metric-value", val);
  value.classList.toggle("compact-value", val.length > 15);
  return el("div", "metric" + (large ? " hero" : ""), el("div", "metric-label", icon(symbol), title),
    value, el("div", "metric-note", note));
}
function notificationCard(message) {
  const delivery = message ? "已记录送达 " + message.delivered + " / " + message.total + " 段" : "";
  const showFull = message ? button("查看全文", "expand", () => {
    openDialog("通知原文", el("p", "dialog-help", timeText(message.sent_at) + " · " + delivery),
      el("pre", "raw", message.text));
  }, "text-btn") : null;
  const card = panel("上一次通知", "message", showFull);
  if (!message) {
    card.append(empty("暂无已发送的通知", "企业微信首次接收报告后，这里将显示实际发送的文本。", "message",
      link("查看通知设置", "settings", "settings", "text-btn")));
  } else {
    card.append(el("div", "notification-body", el("pre", "raw excerpt", message.text)),
      el("div", "section-end", el("span", "", "企业微信 · " + delivery), timeNode(message.sent_at)));
  }
  return card;
}
function overview(data) {
  const tokens = data.tokens;
  const funded = tokens.filter(t => t.available != null).length;
  const observed = tokens.filter(t => t.today_consumption != null).length;
  const problematic = tokens.filter(t => ["low", "exhausted", "error"].includes(t.status)).length;
  const metrics = el("section", "metrics",
    metric("可用余额合计", "wallet", money(sumKnown(tokens, "available")), "汇总 " + funded + " 个令牌最近一次成功余额", true),
    metric("今日累计消耗", "report", money(sumKnown(tokens, "today_consumption")), data.today + " · " + observed + " 个已采集"),
    metric("监控令牌", "token", String(tokens.length), "需关注 " + problematic + " 个 · 待采集 " + tokens.filter(t => t.status === "pending").length + " 个"));
  const card = panel("令牌余额", "pulse", link("管理令牌", "token", "tokens", "text-btn"));
  card.append(tokenTable(tokens, false));
  const times = tokens.map(row => row.checked_at).filter(Boolean);
  const newest = times.reduce((max, value) => !max || Date.parse(value) > Date.parse(max) ? value : max, null);
  card.append(el("div", "section-end", el("span", "", "低余额预警线 " + money(data.threshold)),
    el("span", "", newest ? "最近采集 " + timeText(newest) : "采集间隔 " + intervalLabel(data.interval_seconds))));
  return [metrics, card, notificationCard(data.notification)];
}
function management(data) {
  const card = panel("监控令牌", "token", el("span", "small", "共 " + data.tokens.length + " 个"));
  card.append(tokenTable(data.tokens, true));
  return card;
}
function consumptionInputs(parameters) {
  const last = today();
  const start = formField("date-start", "开始日期", "date");
  const end = formField("date-end", "结束日期", "date");
  start.input.value = parameters.get("start") ?? previousDays(last, 6);
  end.input.value = parameters.get("end") ?? last;
  for (const input of [start.input, end.input]) { input.required = true; input.max = last; }
  const submit = button("查询", "search", null, "btn primary");
  submit.type = "submit";
  const form = el("form", "filters", start.group, end.group, submit);
  const box = errorBox();
  function range() {
    const from = start.input.value, to = end.input.value;
    if (!from || !to) throw new Error("请选择完整的开始和结束日期。");
    if (from > to) throw new Error("开始日期应早于或等于结束日期。");
    if (to > last) throw new Error("结束日期不能晚于北京时间今天。");
    if (Date.parse(to) - Date.parse(from) > 365 * 86400000) throw new Error("一次最多查询 366 个自然日，请缩短时间范围。");
    return new URLSearchParams({start:from, end:to}).toString();
  }
  const filterCard = panel("查询范围", "calendar", el("span","small","含首尾日期"));
  const body = el("div", "panel-body filter-body", form, box);
  filterCard.append(body);
  const result = panel("每日明细", "report");
  const guide = el("div", "note-line", icon("info"), el("p", "",
    "消耗按相邻余额快照的下降量估算。多个 Key 共用账户时可能重复，同一采集间隔内同时充值和消费也可能漏计。正式账单以平台对账数据为准。"));
  const exportBtn = button("导出 CSV", "download", async btn => {
    setBusy(btn, true);
    try {
      const query = range();
      const blob = await api("/api/consumption.csv?" + query, {csv:true});
      if (!btn.isConnected || query !== range()) return;
      const file = URL.createObjectURL(blob);
      const anchor = el("a");
      anchor.href = file;
      anchor.download = "每日消耗_" + start.input.value + "_" + end.input.value + ".csv";
      document.body.append(anchor);
      anchor.click(); anchor.remove();
      setTimeout(() => URL.revokeObjectURL(file), 10000);
      notice("已生成 CSV 文件。");
    } catch (error) { problem(box, error.message); box.focus(); }
    finally { setBusy(btn, false); }
  });
  exportBtn.disabled = true;
  $("#page-actions").append(exportBtn);
  form.addEventListener("input", () => { exportBtn.disabled = true; problem(box, ""); });
  form.addEventListener("submit", event => {
    event.preventDefault();
    try {
      const hash = "#consumption?" + range();
      if (location.hash === hash) loadPage(false);
      else location.hash = hash;
    } catch (error) { problem(box, error.message); box.focus(); }
  });
  return {filterCard, result, guide, box, range, submit, exportBtn};
}
async function consumption(parameters, version) {
  const ui = consumptionInputs(parameters);
  $("#view").replaceChildren(ui.filterCard, ui.result, ui.guide);
  const loading = el("div", "panel-body muted", "正在查询消耗…");
  ui.result.append(loading);
  let query;
  try {
    query = ui.range();
    setBusy(ui.submit, true);
    const data = await api("/api/consumption?" + query);
    if (version !== routeVersion) return;
    ui.result.firstElementChild.append(el("span", "small", "共 " + data.rows.length + " 条"));
    if (data.rows.length) {
      ui.result.append(dataTable("每日消耗明细", [
        {key:"date", title:"日期", value:row => row.date, render:row => el("span", "mono", row.date)},
        {key:"name", title:"令牌名称", value:row => row.name, render:row => tokenLabel(row)},
        {key:"remark", title:"令牌备注", value:row => row.remark, render:row => el("span", "remark", row.remark || "—")},
        {...amountColumn("consumption", "当天总计消耗"), render:row => row.consumption == null ? el("span","small","无成功采样") : el("span","num",money(row.consumption))},
      ], data.rows, {initialKey:"date", descending:true}));
    } else ui.result.append(empty("此范围暂无消耗记录", "可以调整日期范围，或等待监控产生采集记录。", "report"));
    try { ui.exportBtn.disabled = ui.range() !== query; } catch { ui.exportBtn.disabled = true; }
  } catch (error) {
    if (version === routeVersion) problem(ui.box, error.message);
  } finally { loading.remove(); setBusy(ui.submit, false); }
}

function webhookSettings(settings) {
  const sourceName = {override:"界面自定义", config:"配置文件", none:"尚未配置"};
  const card = panel("企业微信机器人", "message", badge(settings.source, sourceName[settings.source]));
  card.classList.add("settings");
  const description = settings.has_fallback ?
    (settings.source === "override" ? "配置文件中已有默认 Webhook。清除自定义地址后，监控将自动使用默认地址。" : "当前使用配置文件中的默认 Webhook。保存自定义地址后将优先使用新地址。") :
    "配置文件中尚无默认 Webhook。未设置自定义地址时，监控将继续采集余额并暂停推送。";
  const note = el("div", "note-line", icon("info"), el("p", "", description));
  const field = formField("webhook", "自定义 Webhook 地址", "password",
    "粘贴企业微信群机器人的完整 HTTPS 地址。已保存的密钥不会回显。");
  field.input.required = true;
  field.input.autocomplete = "off";
  field.input.spellcheck = false;
  field.input.maxLength = 4096;
  field.input.placeholder = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=…";
  const error = errorBox();
  const save = button("保存地址", null, null, "btn primary");
  save.type = "submit";
  const reset = button(settings.has_fallback ? "恢复配置文件默认值" : "清除自定义地址", null, () => {
    const help = el("p", "dialog-help", settings.has_fallback ?
      "清除后，监控将使用配置文件中的默认企业微信机器人推送。" : "清除后将暂停企业微信推送，余额采集将继续运行。");
    openDialog("清除自定义地址？", help, confirmForm("确认清除", async () => {
      await api("/api/settings/webhook", {method:"DELETE"});
      dialog.close();
      notice("已清除自定义 Webhook。");
      await loadPage(false);
    }, {danger:!settings.has_fallback}));
  });
  reset.hidden = settings.source !== "override";
  const form = el("form", "", field.group, error, el("div", "form-footer", save, reset));
  form.method = "post";
  form.addEventListener("submit", async event => {
    event.preventDefault();
    setBusy(save, true); reset.disabled = true; problem(error, "");
    try {
      await api("/api/settings/webhook", {method:"PUT", data:{url:field.input.value.trim()}});
      field.input.value = "";
      notice("自定义 Webhook 已保存。");
      await loadPage(false);
    } catch (failure) { problem(error, failure.message); error.focus(); }
    finally { setBusy(save, false); reset.disabled = false; }
  });
  card.append(el("div","panel-body", el("h3","","当前通知来源"), note, form));
  return card;
}
function queueRefresh(intervalSeconds) {
  clearTimeout(pollTimer);
  const delay = Math.max(30000, Math.min(600000, intervalSeconds * 1000));
  pollTimer = setTimeout(() => {
    if (!session?.authenticated || active !== "overview") return;
    if (document.hidden || dialog.open || $("#view").contains(document.activeElement)) {
      queueRefresh(intervalSeconds);
    } else loadPage(false, true);
  }, delay);
}
async function loadPage(focus, background = false) {
  if (!session?.authenticated) return;
  const [raw, params] = location.hash.slice(1).split("?");
  active = Object.hasOwn(pages, raw) ? raw : "overview";
  const version = ++routeVersion;
  clearTimeout(pollTimer);
  if (!background) {
    if (dialog.open) dialog.close();
    $("#title").textContent = pages[active][0];
    $("#subtitle").textContent = pages[active][1];
    $("#crumb").textContent = pages[active][0];
    document.title = pages[active][0] + " · Kimi Monitor";
    for (const anchor of $("#nav").children) {
      if (anchor.hash === "#" + active) anchor.setAttribute("aria-current","page");
      else anchor.removeAttribute("aria-current");
    }
    $("#page-actions").replaceChildren();
    if (["tokens","overview"].includes(active)) {
      if (active === "tokens") $("#page-actions").append(button("添加令牌", "plus", () => showTokenForm(null), "btn primary"));
      else $("#page-actions").append(button("刷新数据", "refresh", () => loadPage(false)));
    }
    $("#view").replaceChildren(el("p", "muted", "正在加载…"));
    if (focus) $("#title").focus();
  }
  try {
    if (active === "consumption") { await consumption(new URLSearchParams(params), version); return; }
    if (active === "settings") {
      const settings = await api("/api/settings/webhook");
      if (version === routeVersion) $("#view").replaceChildren(webhookSettings(settings));
      return;
    }
    const data = await api("/api/overview");
    if (version !== routeVersion) return;
    if (active === "overview") {
      // A dialog may have opened while an automatic refresh was in flight.
      if (!background || (!dialog.open && !$("#view").contains(document.activeElement))) {
        $("#view").replaceChildren(...overview(data));
      }
      queueRefresh(data.interval_seconds);
    } else $("#view").replaceChildren(management(data));
  } catch (error) {
    if (version !== routeVersion) return;
    if (background) {
      notice("自动刷新失败，当前显示上次读取的数据。");
      queueRefresh(60);
    } else {
      const card = el("section", "panel", empty("数据加载失败", error.message, "info", button("重新加载", "refresh", () => loadPage(false))));
      $("#view").replaceChildren(card);
    }
  }
}
window.addEventListener("hashchange", () => loadPage(true));
window.addEventListener("pageshow", event => { if (event.persisted) location.reload(); });
bootstrap();
