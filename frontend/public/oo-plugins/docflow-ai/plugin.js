/* DocFlow AI OnlyOffice 插件 v5.0 —— 工具化 + 内置 Agent 自主调度。
 *
 * 架构：
 * - 工具运行时（RUNTIME_SRC）：在 OnlyOffice 编辑器上下文（callCommand）内
 *   执行的工具集，按编辑器类型（word/excel/ppt/pdf）能力分层；数据经 JSON
 *   内联注入（该 DS 版本 callCommand 不透传 data 参数，实测）。
 * - Agent 循环（插件侧）：AI 每轮输出 TOOL_CALL（一次一个工具）或 FINAL
 *   （完成总结）；插件执行工具→TOOL_RESULT 回传→下一轮，直至 FINAL 或达
 *   轮次上限。读哪些内容、局部还是全文修改、改后如何验证，全部由 AI 自行
 *   决策——用户只描述意图。
 * - 编辑范围自主判断：AI 通过 get_doc_info/search_text/read_document 建立
 *   文档结构认知，再用 insert_content / replace_text / replace_element /
 *   format_element 等组合完成精确编辑。
 *
 * 工具集（word，14 个）：get_doc_info / read_document / search_text /
 *   insert_content / replace_text / replace_element / delete_element /
 *   format_element / set_paragraph_style / insert_table / table_set_cell /
 *   table_add_row / insert_page_break / add_comment
 * 工具集（excel，7 个）：get_sheet_info / read_range / write_cells /
 *   format_range / insert_rows / delete_rows / set_col_width
 * 工具集（ppt，3 个）：list_slides / read_slide / add_slide
 * 工具集（pdf）：get_doc_info（只读说明——DS pdf 编辑器无文档模型 API）
 *
 * 认证：同站 cookie 换一次性 access token（POST /auth/refresh 带
 *   X-CSRF-Token，与主应用 authFetch 同协议），401 自动重试一次。
 * 模型：GET /ai/models（chat 能力过滤），与主应用共用 docflow.ai.model。
 */
(function () {
  'use strict';
  var log = document.getElementById('log');
  var q = document.getElementById('q');
  var hintEl = document.getElementById('hint');
  var sendBtn = document.getElementById('send');
  var stopBtn = document.getElementById('stop');
  var webBtn = document.getElementById('web');
  var thinkBtn = document.getElementById('think');
  var modelSel = document.getElementById('model');
  var modeSeg = document.getElementById('mode');
  var manualBar = document.getElementById('manualBar');

  var history = [];        // 对话历史（跨轮保留；工具中间消息仅本轮使用）
  var lastReply = '';      // 仅对话模式手动插入用
  var selection = '';      // 「引用选中」圈定的选区文本
  var busy = false;
  var token = '';
  var controller = null;
  var webOn = false;
  var thinkOn = true;
  var editMode = true;
  var editorType = null;   // word | cell | slide | pdf | unknown（打开时探测）
  var docInfo = null;      // 初始 get_doc_info 结果（system 上下文）
  var THINK_BUDGET = 12000;

  // 跟随 DS 主题参数（?theme-type=dark）
  try {
    var tt = /theme-type=(dark)/.exec(window.location.search);
    if (tt) document.documentElement.dataset.themeType = 'dark';
  } catch (e) {}

  // ================= 认证 =================
  function csrfToken() {
    var m = document.cookie.match(/(?:^|; )docflow_csrf=([^;]*)/);
    return m ? decodeURIComponent(m[1]) : null;
  }
  function refresh() {
    var headers = { 'Content-Type': 'application/json' };
    var csrf = csrfToken();
    if (csrf) headers['X-CSRF-Token'] = csrf;
    return fetch('/api/v1/auth/refresh', { method: 'POST', credentials: 'same-origin', headers: headers })
      .then(function (r) { if (!r.ok) throw { status: r.status }; return r.json(); })
      .then(function (d) { token = d.access_token || ''; });
  }

  // ================= 模型 =================
  function loadModels() {
    return fetch('/api/v1/ai/models', { credentials: 'same-origin', headers: { Authorization: 'Bearer ' + token } })
      .then(function (r) { if (!r.ok) throw new Error('models ' + r.status); return r.json(); })
      .then(function (d) {
        var raw = Array.isArray(d && d.models) ? d.models : [];
        var items = [];
        for (var i = 0; i < raw.length; i++) {
          var m = raw[i] || {};
          var caps = m.capabilities || {};
          if (caps && typeof caps === 'object' && caps.kind !== 'chat') continue;
          items.push({ id: (m.provider_id || '') + '/' + (m.id || ''), label: (m.provider_name || m.provider_id || '') + ' / ' + (m.id || ''), providerId: m.provider_id || '', modelId: m.id || '' });
        }
        return items;
      })
      .catch(function () { return []; });
  }
  function renderModels(items) {
    var saved = '';
    try { saved = window.localStorage.getItem('docflow.ai.model') || ''; } catch (e) {}
    modelSel.innerHTML = '';
    var opt = document.createElement('option');
    opt.value = ''; opt.textContent = '默认模型';
    modelSel.appendChild(opt);
    for (var i = 0; i < items.length; i++) {
      var o = document.createElement('option');
      o.value = items[i].id; o.textContent = items[i].label;
      if (saved === items[i].id) o.selected = true;
      modelSel.appendChild(o);
    }
    modelSel.onchange = function () {
      try { window.localStorage.setItem('docflow.ai.model', modelSel.value); } catch (e) {}
    };
  }
  function currentModel() {
    var v = modelSel.value;
    if (!v) return null;
    var i = v.lastIndexOf('/');
    return { providerId: v.slice(0, i), modelId: v.slice(i + 1) };
  }

  // ================= Markdown → HTML（气泡渲染） =================
  function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); }
  function inlineMd(s) {
    return esc(s).replace(/`([^`]+)`/g, '<code>$1</code>').replace(/\*\*([^*]+)\*\*/g, '<b>$1</b>');
  }
  function mdToHtml(md) {
    var lines = String(md).replace(/\r/g, '').split('\n');
    var out = [], listOpen = false, codeOpen = false, codeBuf = [], tableBuf = [];
    var flushList = function () { if (listOpen) { out.push('</ul>'); listOpen = false; } };
    var flushTable = function () {
      if (!tableBuf.length) return;
      var rows = tableBuf.filter(function (r) { return !/^\|[\s:|-]+\|?$/.test(r); });
      if (rows.length) {
        var html = ['<table>'];
        for (var i = 0; i < rows.length; i++) {
          var cells = rows[i].replace(/^\||\|$/g, '').split('|');
          html.push('<tr>' + cells.map(function (c, j) { return (i === 0 ? '<th>' : '<td>') + inlineMd(c.trim()) + (i === 0 ? '</th>' : '</td>'); }).join('') + '</tr>');
        }
        html.push('</table>'); out.push(html.join(''));
      }
      tableBuf = [];
    };
    for (var i = 0; i < lines.length; i++) {
      var line = lines[i];
      if (/^```/.test(line)) {
        if (codeOpen) { out.push('<pre><code>' + esc(codeBuf.join('\n')) + '</code></pre>'); codeBuf = []; codeOpen = false; }
        else { flushList(); flushTable(); codeOpen = true; }
        continue;
      }
      if (codeOpen) { codeBuf.push(line); continue; }
      if (/^\|.*\|/.test(line)) { flushList(); tableBuf.push(line.trim()); continue; }
      flushTable();
      var h = /^(#{1,3})\s+(.*)$/.exec(line);
      if (h) { flushList(); out.push('<h' + h[1].length + '>' + inlineMd(h[2]) + '</h' + h[1].length + '>'); continue; }
      if (/^\s*[-*]\s+/.test(line)) {
        if (!listOpen) { out.push('<ul>'); listOpen = true; }
        out.push('<li>' + inlineMd(line.replace(/^\s*[-*]\s+/, '')) + '</li>');
        continue;
      }
      flushList();
      if (line.trim() === '') continue;
      out.push('<p>' + inlineMd(line) + '</p>');
    }
    flushList(); flushTable();
    if (codeOpen) out.push('<pre><code>' + esc(codeBuf.join('\n')) + '</code></pre>');
    return out.join('');
  }

  // ================= 消息渲染 =================
  function el(tag, cls, html) {
    var d = document.createElement(tag);
    if (cls) d.className = cls;
    if (html !== undefined) d.innerHTML = html;
    log.appendChild(d); log.scrollTop = log.scrollHeight;
    return d;
  }
  function userBubble(text, refNote) {
    el('div', 'm u', esc(text) + (refNote ? '<span class="ref">📎 ' + esc(refNote) + '</span>' : ''));
  }
  function setHint(t) { hintEl.textContent = t; }

  var thinkEl = null;
  function appendThink(chunk) {
    if (!thinkEl) {
      thinkEl = el('details', 'think', '<summary>思考中…</summary>');
      thinkEl.open = true;
      var body = document.createElement('div'); body.className = 't';
      thinkEl.appendChild(body); thinkEl._body = body;
    }
    thinkEl._body.textContent += chunk;
    if (thinkEl._body.textContent.length > THINK_BUDGET) thinkEl._body.textContent = '…' + thinkEl._body.textContent.slice(-THINK_BUDGET);
    log.scrollTop = log.scrollHeight;
  }
  function settleThink(ms) {
    if (!thinkEl) return;
    thinkEl.open = false;
    thinkEl.firstChild.textContent = '已深度思考' + (ms > 0 ? '（用时 ' + (ms / 1000).toFixed(1) + ' s）' : '');
  }

  /** 工具调用过程卡：head（工具名）+ args 摘要 + 执行态/结果。 */
  function toolCard(tool, args) {
    var card = el('div', 'tcall',
      '<div class="t-head"><span class="spin"></span><span>🔧 ' + esc(tool) + '</span></div>' +
      '<div class="t-args">' + esc(JSON.stringify(args)).slice(0, 300) + '</div>');
    card._head = card.firstChild;
    card._res = null;
    return card;
  }
  function toolDone(card, ok, payload) {
    card.classList.remove('ok'); card.classList.add(ok ? 'ok' : 'bad');
    card._head.innerHTML = (ok ? '✅ ' : '⚠️ ') + esc(card._head.textContent.replace(/^[✅⚠🔧]\s*/, '').replace('🔧 ', '')) ;
    card._head.querySelector ? null : null;
    // 重建 head（去 spinner）
    var name = card._head.textContent.replace(/^[✅⚠️]\s*/, '').trim();
    card._head.innerHTML = (ok ? '✅ ' : '⚠️ ') + esc(name);
    var res = document.createElement('div');
    res.className = 't-res';
    res.textContent = typeof payload === 'string' ? payload.slice(0, 500) : JSON.stringify(payload).slice(0, 500);
    card.appendChild(res);
    log.scrollTop = log.scrollHeight;
  }

  // ================= 工具运行时（编辑器上下文执行） =================
  /* 仅可用 data / Api；防御式 API 探测（各编辑器/版本方法名差异以 try 链
     兜底，失败返回明确错误供 AI 调整策略——工具失败本身是 agent 信号）。 */
  var RUNTIME_SRC = [
    'function clip(s, n) { s = String(s); return s.length > n ? s.slice(0, n) + "…" : s; }',
    'function editorKind() {',
    '  if (typeof Api.GetDocument === "function") return "word";',
    '  if (typeof Api.GetActiveSheet === "function") return "cell";',
    '  if (typeof Api.GetPresentation === "function") return "slide";',
    '  return "pdf";',
    '}',
    '// ---- md → 文档元素（word）----',
    'function mdRuns(par, text) {',
    '  var seg = String(text);',
    '  while (true) {',
    '    var a = seg.indexOf("**"), b = seg.indexOf("**", a + 2);',
    '    var c = seg.indexOf("`"), d = seg.indexOf("`", c + 1);',
    '    var pick = null;',
    '    if (a >= 0 && b > a) pick = ["b", a, b + 2, seg.slice(a + 2, b)];',
    '    if (c >= 0 && d > c && (pick === null || c < pick[1])) pick = ["m", c, d + 1, seg.slice(c + 1, d)];',
    '    if (pick === null) break;',
    '    if (pick[1] > 0) par.AddText(seg.slice(0, pick[1]));',
    '    var r = par.AddText(pick[3]);',
    '    if (pick[0] === "b") r.SetBold(true); else r.SetFontName("Courier New");',
    '    seg = seg.slice(pick[2]);',
    '  }',
    '  if (seg) par.AddText(seg);',
    '}',
    'function mdPara(line) {',
    '  var p = Api.CreateParagraph();',
    '  var t = line; var h = 0;',
    '  while (t.charAt(0) === "#") { h++; t = t.slice(1); }',
    '  if (h > 0 && t.charAt(0) === " ") {',
    '    var r = p.AddText(t.replace(/^\\s+/, ""));',
    '    r.SetBold(true); r.SetFontSize(h === 1 ? 18 : h === 2 ? 15 : 13.5);',
    '    return p;',
    '  }',
    '  var tr = line.replace(/^\\s+/, "");',
    '  if (tr.charAt(0) === "-" || tr.charAt(0) === "*") { p.AddText("\\u2022 "); mdRuns(p, tr.replace(/^[-*]\\s+/, "")); return p; }',
    '  mdRuns(p, line); return p;',
    '}',
    'function mdElems(md) {',
    '  var out = [], lines = String(md).split("\\n"), inCode = false, buf = [], tbl = [];',
    '  function flushTbl() {',
    '    if (!tbl.length) return;',
    '    var rows = [];',
    '    for (var k = 0; k < tbl.length; k++) { var tt = tbl[k].trim(); if (/^\\|[\\s:|-]+\\|?$/.test(tt)) continue; rows.push(tt.replace(/^\\|/, "").replace(/\\|$/, "").split("|")); }',
    '    if (rows.length) out.push(makeTable(rows));',
    '    tbl = [];',
    '  }',
    '  for (var i = 0; i < lines.length; i++) {',
    '    var ln = lines[i];',
    '    if (ln.indexOf("```") === 0) {',
    '      if (inCode) { var cp = Api.CreateParagraph(); mdRuns(cp, buf.join("\\n")); cp.SetFontSize ? null : null; out.push(cp); buf = []; inCode = false; }',
    '      else { flushTbl(); inCode = true; }',
    '      continue;',
    '    }',
    '    if (inCode) { buf.push(ln); continue; }',
    '    if (ln.trim() === "") { flushTbl(); continue; }',
    '    if (ln.charAt(0) === "|") { tbl.push(ln); continue; }',
    '    flushTbl();',
    '    out.push(mdPara(ln));',
    '  }',
    '  flushTbl();',
    '  if (inCode && buf.length) { var cp2 = Api.CreateParagraph(); mdRuns(cp2, buf.join("\\n")); out.push(cp2); }',
    '  return out;',
    '}',
    'function makeTable(rows) {',
    '  var t = Api.CreateTable(rows[0].length, rows.length);',
    '  fillTable(t, rows); return t;',
    '}',
    'function fillTable(t, rows) {',
    '  for (var ri = 0; ri < rows.length; ri++) {',
    '    for (var ci = 0; ci < rows[ri].length && ci < rows[0].length; ci++) {',
    '      try { fillCell(t.GetRow(ri).GetCell(ci), String(rows[ri][ci]), ri === 0); } catch (e) {}',
    '    }',
    '  }',
    '}',
    'function fillCell(cell, text, bold) {',
    '  var cc = cell.GetContent();',
    '  var p = null;',
    '  if (cc.GetElementsCount() > 0) { var e0 = cc.GetElement(0); if (e0 && typeof e0.AddText === "function") p = e0; }',
    '  if (!p) { p = Api.CreateParagraph(); cc.Push(p); }',
    '  var r = p.AddText(text);',
    '  if (bold && r.SetBold) r.SetBold(true);',
    '}',
    'function isTable(e) { return e && typeof e.GetRow === "function"; }',
    'function paraText(e) { try { return e.GetText(); } catch (err) { return ""; } }',
    'function docScan(query) {', // 遍历元素：返回 {count, hits:[{i, kind, text}]}（query 空则不筛）
    '  var doc = Api.GetDocument();',
    '  var n = doc.GetElementsCount(), hits = [];',
    '  for (var i = 0; i < n; i++) {',
    '    var e = doc.GetElement(i);',
    '    var kind = isTable(e) ? "table" : "para";',
    '    var text = kind === "table" ? "[表格]" : paraText(e);',
    '    if (!query || text.indexOf(query) >= 0) hits.push({ i: i, kind: kind, text: clip(text, 400) });',
    '  }',
    '  return { count: n, hits: hits };',
    '}',
    '// ---- word 工具 ----',
    'function wInsertContent(a) {',
    '  var doc = Api.GetDocument(); var elems = mdElems(a.md || "");',
    '  if (!elems.length) return { ok: false, error: "空内容" };',
    '  var at = a.at || "end";',
    '  if (at === "cursor") { doc.InsertContent(elems); return { ok: true }; }',
    '  if (at === "after" && typeof a.index === "number") {',
    '    try { for (var k = elems.length - 1; k >= 0; k--) doc.AddElement(a.index + 1, elems[k]); return { ok: true }; }',
    '    catch (e) { for (var k2 = 0; k2 < elems.length; k2++) doc.Push(elems[k2]); return { ok: true, note: "AddElement 不可用，已追加到文末" }; }',
    '  }',
    '  for (var j = 0; j < elems.length; j++) doc.Push(elems[j]);',
    '  return { ok: true };',
    '}',
    'function wReplaceText(a) { // 段落粒度：找到含 find 的段落，整段替换为 md',
    '  var doc = Api.GetDocument();',
    '  var scan = docScan(a.find);',
    '  var done = 0, idxs = [];',
    '  for (var h = 0; h < scan.hits.length; h++) if (scan.hits[h].kind === "para") idxs.push(scan.hits[h].i);',
    '  if (!idxs.length) return { ok: false, error: "未找到: " + clip(a.find, 60) };',
    '  if ((a.scope || "first") === "first") idxs = [idxs[0]];',
    '  var elems = mdElems(a.md || "");',
    '  for (var t = idxs.length - 1; t >= 0; t--) {',
    '    var i = idxs[t];',
    '    try {',
    '      var newElems = [];',
    '      for (var c = 0; c < elems.length; c++) newElems.push(elems[c]);',
    '      var pos = i;',
    '      doc.RemoveElement(i);',
    '      try { for (var k = newElems.length - 1; k >= 0; k--) doc.AddElement(pos, newElems[k]); }',
    '      catch (e2) { for (var k2 = 0; k2 < newElems.length; k2++) doc.Push(newElems[k2]); }',
    '      done++;',
    '    } catch (err) {}',
    '  }',
    '  return done ? { ok: true, replaced: done } : { ok: false, error: "替换失败" };',
    '}',
    'function wReplaceElement(a) { var a2 = {}; a2.find = null; return wDelThenIns(a.index, a.md); }',
    'function wDelThenIns(index, md) {',
    '  var doc = Api.GetDocument();',
    '  var elems = mdElems(md || "");',
    '  doc.RemoveElement(index);',
    '  try { for (var k = elems.length - 1; k >= 0; k--) doc.AddElement(index, elems[k]); }',
    '  catch (e) { for (var j = 0; j < elems.length; j++) doc.Push(elems[j]); }',
    '  return { ok: true };',
    '}',
    'function wFormatElement(a) { // 段落内全部 run 应用格式',
    '  var doc = Api.GetDocument();',
    '  var p = doc.GetElement(a.index);',
    '  if (!p || typeof p.GetElementsCount !== "function") return { ok: false, error: "元素不是段落" };',
    '  var n = 0;',
    '  for (var i = 0; i < p.GetElementsCount(); i++) {',
    '    var r = p.GetElement(i);',
    '    if (!r || typeof r.AddText === "function") continue;',
    '    try {',
    '      if (a.bold === true && r.SetBold) r.SetBold(true);',
    '      if (a.bold === false && r.SetBold) r.SetBold(false);',
    '      if (a.italic === true && r.SetItalic) r.SetItalic(true);',
    '      if (a.underline === true && r.SetUnderline) r.SetUnderline(true);',
    '      if (a.fontSize && r.SetFontSize) r.SetFontSize(a.fontSize);',
    '      if (a.color && r.SetColor) { var c = hex(a.color); r.SetColor(c[0], c[1], c[2], false); }',
    '      if (a.highlight && r.SetHighlight) r.SetHighlight(a.highlight);',
    '      n++;',
    '    } catch (e) {}',
    '  }',
    '  return n ? { ok: true, runs: n } : { ok: false, error: "段落无文本 run" };',
    '}',
    'function hex(s) {',
    '  s = String(s).replace("#", "");',
    '  return [parseInt(s.slice(0, 2), 16) || 0, parseInt(s.slice(2, 4), 16) || 0, parseInt(s.slice(4, 6), 16) || 0];',
    '}',
    'function wSetStyle(a) {',
    '  var doc = Api.GetDocument();',
    '  var p = doc.GetElement(a.index);',
    '  if (!p || typeof p.AddText !== "function") return { ok: false, error: "元素不是段落" };',
    '  if (a.style && a.style !== "normal") {',
    '    var name = a.style === "heading_1" ? "Heading 1" : a.style === "heading_2" ? "Heading 2" : a.style === "heading_3" ? "Heading 3" : a.style;',
    '    try { p.SetStyle(Api.GetStyle(name)); } catch (e) { return { ok: false, error: "样式不可用: " + name }; }',
    '  }',
    '  if (a.align) { try { p.SetJc(a.align); } catch (e2) { try { p.SetAlign(a.align); } catch (e3) {} } }',
    '  return { ok: true };',
    '}',
    'function wInsertPageBreak() {',
    '  var doc = Api.GetDocument();',
    '  var p = Api.CreateParagraph();',
    '  try { p.AddPageBreak(); doc.Push(p); return { ok: true }; }',
    '  catch (e) { return { ok: false, error: "AddPageBreak 不可用" }; }',
    '}',
    'function wAddComment(a) {',
    '  try {',
    '    var doc = Api.GetDocument();',
    '    var rs = doc.Search(a.find);',
    '    if (!rs || !rs.length) return { ok: false, error: "未找到注释目标" };',
    '    rs[0].AddComment(a.text || "");',
    '    return { ok: true };',
    '  } catch (e) { return { ok: false, error: "注释 API 不可用: " + e }; }',
    '}',
    'function wTableOp(a) {',
    '  var doc = Api.GetDocument();',
    '  var t = doc.GetElement(a.index);',
    '  if (!isTable(t)) return { ok: false, error: "元素不是表格" };',
    '  if (a.op === "add_row") { try { t.AddRow(t.GetRow(t.GetRowsCount() - 1)); return { ok: true }; } catch (e) { return { ok: false, error: "AddRow 不可用" }; } }',
    '  if (a.op === "set_cell") {',
    '    try { fillCell(t.GetRow(a.r).GetCell(a.c), String(a.text), false); return { ok: true }; }',
    '    catch (e2) { return { ok: false, error: "单元格不可达" }; }',
    '  }',
    '  return { ok: false, error: "未知表格操作" };',
    '}',
    '// ---- excel 工具 ----',
    'function colName(n) { var s = ""; n = n + 1; while (n > 0) { var m = (n - 1) % 26; s = String.fromCharCode(65 + m) + s; n = Math.floor((n - 1) / 26); } return s; }',
    'function xRange(sh, r1, c1, r2, c2) { return sh.GetRange(colName(c1) + (r1 + 1) + ":" + colName(c2) + (r2 + 1)); }',
    'function xInfo() {',
    '  var sh = Api.GetActiveSheet();',
    '  var vals = [];',
    '  try { vals = sh.GetRange("A1:T200").GetValues(); } catch (e) { vals = []; }',
    '  var maxR = -1, maxC = -1;',
    '  for (var i = 0; i < vals.length; i++) {',
    '    var row = vals[i];',
    '    for (var j = 0; j < row.length; j++) {',
    '      var v = row[j];',
    '      if (v !== "" && v !== null && v !== undefined) { if (i > maxR) maxR = i; if (j > maxC) maxC = j; }',
    '    }',
    '  }',
    '  return { ok: true, rows: maxR + 1, cols: maxC + 1 };',
    '}',
    'function xRead(a) {',
    '  var sh = Api.GetActiveSheet();',
    '  var r1 = a.r1 || 0, c1 = a.c1 || 0, r2 = a.r2 !== undefined ? a.r2 : r1 + 19, c2 = a.c2 !== undefined ? a.c2 : c1 + 9;',
    '  var vals = xRange(sh, r1, c1, r2, c2).GetValues();',
    '  return { ok: true, from: [r1, c1], values: vals.slice(0, 60).map(function (row) { return row.slice(0, 20); }) };',
    '}',
    'function xWrite(a) {',
    '  var sh = Api.GetActiveSheet();',
    '  var n = 0;',
    '  for (var i = 0; i < a.cells.length; i++) {',
    '    var c = a.cells[i];',
    '    try { sh.GetRange(colName(c[1]) + (c[0] + 1)).SetValue(String(c[2])); n++; } catch (e) {}',
    '  }',
    '  return n ? { ok: true, written: n } : { ok: false, error: "未写入任何单元格" };',
    '}',
    'function xFormat(a) {',
    '  var sh = Api.GetActiveSheet();',
    '  var rg = xRange(sh, a.r1 || 0, a.c1 || 0, a.r2 || (a.r1 || 0), a.c2 || (a.c1 || 0));',
    '  var ops = [];',
    '  function T(name, fn) { try { fn(); ops.push(name); } catch (e) {} }',
    '  if (a.bold !== undefined) T("bold", function () { rg.SetBold(a.bold); });',
    '  if (a.fontSize) T("size", function () { rg.SetFontSize(a.fontSize); });',
    '  if (a.fillColor) T("fill", function () { rg.SetFillColor(hex(a.fillColor)); });',
    '  if (a.numberFormat) T("numfmt", function () { rg.SetNumberFormat(a.numberFormat); });',
    '  if (a.hAlign) T("align", function () { rg.SetHorizontalAlignment(a.hAlign); });',
    '  return ops.length ? { ok: true, applied: ops } : { ok: false, error: "无可用格式 API" };',
    '}',
    'function xRows(a) {',
    '  var sh = Api.GetActiveSheet();',
    '  try {',
    '    var rg = sh.GetRange((a.at + 1) + ":" + (a.at + (a.count || 1)));',
    '    if (a.op === "insert") rg.Insert();',
    '    else if (a.op === "delete") rg.Delete();',
    '    else return { ok: false, error: "op 须为 insert|delete" };',
    '    return { ok: true };',
    '  } catch (e) { return { ok: false, error: "行操作 API 不可用: " + e }; }',
    '}',
    'function xColWidth(a) {',
    '  var sh = Api.GetActiveSheet();',
    '  try { sh.GetRange(colName(a.col) + ":" + colName(a.col)).SetColumnWidth(a.width); return { ok: true }; }',
    '  catch (e) { return { ok: false, error: "SetColumnWidth 不可用" }; }',
    '}',
    '// ---- ppt 工具（防御式：方法名随版本差异较大） ----',
    'function pList() {',
    '  var pr = Api.GetPresentation();',
    '  return { ok: true, slides: pr.GetSlidesCount() };',
    '}',
    'function pRead(a) {',
    '  var pr = Api.GetPresentation();',
    '  var s = pr.GetSlideByIndex(a.index);',
    '  if (!s) return { ok: false, error: "slide 不存在" };',
    '  var texts = [];',
    '  try { s.ForEachShape(function (sh) { try { if (sh.GetText) texts.push(clip(sh.GetText(), 200)); } catch (e) {} }); }',
    '  catch (e1) { try { var arr = s.GetAllShapes(); for (var i = 0; i < arr.length; i++) { try { if (arr[i].GetText) texts.push(clip(arr[i].GetText(), 200)); } catch (e2) {} } } catch (e3) {} }',
    '  return { ok: true, texts: texts };',
    '}',
    'function pAdd(a) {',
    '  try {',
    '    var pr = Api.GetPresentation();',
    '    var s = pr.CreateSlide();',
    '    pr.AddSlide(s);',
    '    var lines = [a.title || ""].concat(a.bullets || []);',
    '    var y = 10;',
    '    for (var i = 0; i < lines.length; i++) {',
    '      if (!lines[i]) continue;',
    '      try {',
    '        var tb = Api.CreateTextBlock ? null : null;',
    '        s.AddText ? s.AddText(String(lines[i]), 10, y, 280, 20, 20) : null;',
    '      } catch (e) {}',
    '      y += 24;',
    '    }',
    '    return { ok: true, note: "slide 已添加；文本写入能力取决于 DS 版本" };',
    '  } catch (err) { return { ok: false, error: "PPT 写入 API 不可用: " + err }; }',
    '}',
    '// ---- 分发 ----',
    'function dispatch(d) {',
    '  var kind = editorKind();',
    '  try {',
    '    if (d.tool === "get_doc_info") {',
    '      if (kind === "word") { var sc = docScan(null); var sel = ""; try { sel = Api.GetDocument().GetSelectedText ? Api.GetDocument().GetSelectedText() : ""; } catch (e0) {} return { ok: true, kind: kind, elements: sc.count, overview: sc.hits.slice(0, 80), selection: clip(sel, 500) }; }',
    '      if (kind === "cell") return xInfo();',
    '      if (kind === "slide") return pList();',
    '      return { ok: true, kind: "pdf", note: "DS pdf 编辑器无文档模型 API，仅支持对话" };',
    '    }',
    '    if (kind === "word") {',
    '      if (d.tool === "read_document") { var from = d.args.from || 0, to = d.args.to; var sc2 = docScan(null); var list = []; for (var i = from; i < sc2.count && (to === undefined || i <= to) && list.length < 60; i++) list.push(sc2.hits[i] ? { i: i, kind: sc2.hits[i].kind, text: clip(sc2.hits[i].text, d.args.maxChars || 500) } : null); return { ok: true, total: sc2.count, elements: list }; }',
    '      if (d.tool === "search_text") { var sc3 = docScan(d.args.query); return { ok: true, total: sc3.count, hits: sc3.hits.slice(0, 30) }; }',
    '      if (d.tool === "insert_content") return wInsertContent(d.args);',
    '      if (d.tool === "replace_text") return wReplaceText(d.args);',
    '      if (d.tool === "replace_element") return wDelThenIns(d.args.index, d.args.md);',
    '      if (d.tool === "delete_element") { try { Api.GetDocument().RemoveElement(d.args.index); return { ok: true }; } catch (e1) { return { ok: false, error: "索引越界" }; } }',
    '      if (d.tool === "format_element") return wFormatElement(d.args);',
    '      if (d.tool === "set_paragraph_style") return wSetStyle(d.args);',
    '      if (d.tool === "insert_table") { var t = makeTable(d.args.rows || [["1", "2"]]); var doc = Api.GetDocument(); if ((d.args.at || "end") === "cursor") doc.InsertContent([t]); else doc.Push(t); return { ok: true }; }',
    '      if (d.tool === "table_op") return wTableOp(d.args);',
    '      if (d.tool === "insert_page_break") return wInsertPageBreak();',
    '      if (d.tool === "add_comment") return wAddComment(d.args);',
    '    }',
    '    if (kind === "cell") {',
    '      if (d.tool === "read_range") return xRead(d.args);',
    '      if (d.tool === "write_cells") return xWrite(d.args);',
    '      if (d.tool === "format_range") return xFormat(d.args);',
    '      if (d.tool === "row_op") return xRows(d.args);',
    '      if (d.tool === "set_col_width") return xColWidth(d.args);',
    '    }',
    '    if (kind === "slide") {',
    '      if (d.tool === "read_slide") return pRead(d.args);',
    '      if (d.tool === "add_slide") return pAdd(d.args);',
    '    }',
    '    return { ok: false, error: "工具 " + d.tool + " 在 " + kind + " 编辑器中不可用" };',
    '  } catch (err) { return { ok: false, error: String(err && err.message || err) }; }',
    '}',
    'return dispatch(data);'
  ].join('\n');

  /** 单次工具执行：JSON 内联 + new Function + callCommand。 */
  function runTool(tool, args) {
    return new Promise(function (resolve) {
      if (!window.Asc || !window.Asc.plugin) { resolve({ ok: false, error: '插件环境未就绪' }); return; }
      try {
        var src = 'var data = ' + JSON.stringify({ tool: tool, args: args || {} }) + ';\n' + RUNTIME_SRC;
        var fn = new Function('return (function () {\n' + src + '\n})')();
        var settled = false;
        var to = setTimeout(function () { if (!settled) { settled = true; resolve({ ok: false, error: '工具执行超时' }); } }, 20000);
        window.Asc.plugin.callCommand(fn, false, false, function (r) {
          if (settled) return; settled = true; clearTimeout(to);
          resolve(r || { ok: false, error: 'callCommand 无返回' });
        });
      } catch (e) { resolve({ ok: false, error: String(e) }); }
    });
  }

  /** 打开面板时探测编辑器类型 + 初始 doc_info。 */
  function probeEditor() {
    return runTool('get_doc_info', {}).then(function (r) {
      editorType = (r && r.kind) || 'unknown';
      docInfo = r;
      return r;
    });
  }

  // ================= OnlyOffice 桥（仅对话模式手动插入） =================
  function insertReply(text) {
    if (!window.Asc || !window.Asc.plugin) return;
    window.Asc.plugin.executeMethod('PasteHtml', [mdToHtml(text)]);
  }
  document.getElementById('insert').onclick = function () { if (lastReply) insertReply(lastReply); };
  document.getElementById('replace').onclick = function () { if (lastReply) insertReply(lastReply); };
  function getSelection(cb) {
    if (!window.Asc || !window.Asc.plugin) { cb(''); return; }
    try { window.Asc.plugin.executeMethod('GetSelectedText', null, function (r) { cb(r && typeof r === 'object' ? r.Text : r); }); }
    catch (e) { cb(''); }
  }
  document.getElementById('useSel').onclick = function () {
    getSelection(function (sel) {
      selection = sel || '';
      setHint(selection ? ('已引用选中（' + selection.length + ' 字），本次指令将重点关注该选区') : '选区为空（可在文档中选中后重试）');
    });
  };
  document.getElementById('useAll').onclick = function () {
    setHint('正在读取文档结构…');
    probeEditor().then(function (r) {
      setHint(r && r.ok ? ('文档结构已刷新（' + (r.elements || r.rows || r.slides || '?') + ' 项）') : '文档结构读取失败');
    });
  };
  document.getElementById('clear').onclick = function () {
    history = []; selection = ''; lastReply = '';
    log.innerHTML = '';
    setHint('会话已清空');
  };

  // ================= 模式 / 开关 =================
  modeSeg.addEventListener('click', function (e) {
    var b = e.target.closest('button[data-v]');
    if (!b) return;
    editMode = b.dataset.v === 'edit';
    var btns = modeSeg.querySelectorAll('button');
    for (var i = 0; i < btns.length; i++) btns[i].classList.toggle('on', btns[i] === b);
    manualBar.classList.toggle('hidden', editMode);
    q.placeholder = editMode
      ? '描述目标，AI 自主完成（读文档→规划→修改→验证），例如「把第二段改简洁并加粗关键词」「按 A 列数据生成汇总表」'
      : '仅对话模式：围绕当前文档提问，回复可用下方按钮手动插入';
    setHint(editMode ? '自主编辑：AI 通过内置工具直接操作文档（Ctrl+Z 可撤销）' : '仅对话：AI 不改动文档');
  });
  webBtn.onclick = function () { webOn = !webOn; webBtn.classList.toggle('on', webOn); };
  thinkBtn.onclick = function () { thinkOn = !thinkOn; thinkBtn.classList.toggle('on', thinkOn); };

  // ================= /ai/chat SSE =================
  function chatOnce(messages, ev) {
    var body = { messages: messages, stream: true };
    if (webOn) body.web_search = true;
    if (thinkOn) body.think = true;
    var m = currentModel();
    if (m && m.providerId) body.model = { providerId: m.providerId, modelId: m.modelId };
    return fetch('/api/v1/ai/chat', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + token },
      body: JSON.stringify(body),
      signal: controller.signal,
    }).then(function (res) {
      if (!res.ok) {
        if (res.status === 401) throw { status: 401 };
        return res.text().then(function (t) { throw new Error('HTTP ' + res.status + ' ' + t.slice(0, 120)); });
      }
      var reader = res.body.getReader();
      var dec = new TextDecoder();
      var buf = '';
      function pump() {
        return reader.read().then(function (r) {
          if (r.done) return;
          buf += dec.decode(r.value, { stream: true });
          var idx;
          while ((idx = buf.indexOf('\n\n')) >= 0) {
            var block = buf.slice(0, idx); buf = buf.slice(idx + 2);
            var name = '', data = '';
            block.split('\n').forEach(function (line) {
              if (line.indexOf('event: ') === 0) name = line.slice(7).trim();
              else if (line.indexOf('data: ') === 0) data = line.slice(6);
            });
            if (!name) continue;
            var payload = {};
            try { payload = JSON.parse(data); } catch (e) { continue; }
            if (name === 'delta') { var t = payload.text || ''; if (t) ev.onDelta(t); }
            else if (name === 'thinking') { var th = payload.text || ''; if (th) ev.onThinking(th); }
            else if (name === 'error') { throw new Error(payload.error || 'AI 请求失败'); }
            else if (name === 'done') { ev.onDone(); return; }
          }
          return pump();
        });
      }
      return pump();
    });
  }
  function chat(messages, ev) {
    return chatOnce(messages, ev).catch(function (err) {
      if (err && err.status === 401) return refresh().then(function () { return chatOnce(messages, ev); });
      throw err;
    });
  }

  // ================= Agent 协议与系统提示 =================
  // 协议解析（v5.2）：括号深度扫描提取 TOOL_CALL {...} 候选（字符串
  // 感知——引号内的 {}/BS 转义不计深度，支持嵌套对象与跨行），从最后一
  // 个候选向前取首个可 JSON 解析的；FINAL 标记优先；无候选才视为答复文本。
  function extractCallJson(t) {
    var k = 0;
    var found = [];
    while (found.length < 8) {
      var at = t.indexOf('TOOL_CALL', k);
      if (at < 0) break;
      var brace = t.indexOf('{', at);
      if (brace < 0) break;
      var depth = 0, inStr = false, escp = false, end = -1;
      for (var i = brace; i < t.length; i++) {
        var ch = t.charAt(i);
        if (escp) { escp = false; continue; }
        if (ch === '\\') { if (inStr) escp = true; continue; }
        if (ch === '"') { inStr = !inStr; continue; }
        if (inStr) continue;
        if (ch === '{') depth++;
        else if (ch === '}') { depth--; if (depth === 0) { end = i; break; } }
      }
      if (end < 0) break;
      found.push(t.slice(brace, end + 1));
      k = end + 1;
    }
    for (var j = found.length - 1; j >= 0; j--) {
      try {
        var c = JSON.parse(found[j]);
        if (c && typeof c.tool === 'string') return { type: 'tool', call: { tool: c.tool, args: c.args || {} } };
      } catch (e) {}
    }
    return null;
  }
  function parseAgentReply(text) {
    var t = text.trim();
    if (t.indexOf('FINAL') === 0) return { type: 'final', text: t.slice(5).trim() || t };
    var call = extractCallJson(t);
    if (call) return call;
    return { type: 'final', text: t };
  }

  var TOOLS_DOC = {
    word: [
      'get_doc_info {} → 文档结构总览：元素总数 + 每元素（段落/表格）索引与文本摘要 + 当前选区。任何任务先调它。',
      'read_document {from?, to?, maxChars?} → 按元素索引读全文（每元素截断，默认500字）。',
      'search_text {query} → 返回所有含 query 的元素索引与片段（定位修改点）。',
      'insert_content {md, at:"end"|"cursor"|"after", index?} → 插入内容（at=after 时 index 为目标元素索引）。md 支持 #/##/### 标题、**加粗**、`代码`、- 列表、|表格行|。',
      'replace_text {find, md, scope?:"first"|"all"} → 把含 find 的段落整段替换为 md（段落粒度）。多个小修改分别调用，勿整篇重写。',
      'replace_element {index, md} → 按索引整段替换。',
      'delete_element {index} → 删除指定元素。',
      'format_element {index, bold?, italic?, underline?, fontSize?, color?:"RRGGBB", highlight?:"yellow"} → 段落内全部文本应用格式。',
      'set_paragraph_style {index, style?:"heading_1"|"heading_2"|"heading_3"|"normal", align?:"left"|"center"|"right"|"justify"} → 段落样式与对齐。',
      'insert_table {rows:[["表头",…],…], at?:"end"|"cursor"} → 插入表格（首行自动加粗）。',
      'table_op {index, op:"add_row"|"set_cell", r?, c?, text?} → 表格加行 / 填充单元格。',
      'insert_page_break {} → 文末插入分页符。',
      'add_comment {find, text} → 在首个匹配处添加批注。'
    ].join('\n'),
    cell: [
      'get_doc_info {} → 已用区域行列数。',
      'read_range {r1,c1,r2?,c2?} → 读区域值（行列 0 起，默认 20x10 窗口）。',
      'write_cells {cells:[[行,列,值],…]} → 写单元格（值可为公式如 =SUM(A1:A5)）。',
      'format_range {r1,c1,r2,c2, bold?, fontSize?, fillColor?:"RRGGBB", numberFormat?, hAlign?:"left"|"center"|"right"} → 区域格式。',
      'row_op {op:"insert"|"delete", at, count?} → 插入/删除行。',
      'set_col_width {col, width} → 列宽。'
    ].join('\n'),
    slide: [
      'get_doc_info {} → 幻灯片数量。',
      'read_slide {index} → 读指定页全部文本。',
      'add_slide {title, bullets?} → 新建幻灯片（文本写入能力取决于 DS 版本，失败会如实返回）。'
    ].join('\n'),
    pdf: '（DS pdf 编辑器暂无文档模型 API——请基于用户描述对话，无法直接编辑 pdf）'
  };

  function buildSystem() {
    var kind = editorType || 'word';
    var sys = [
      '你是 DocFlow 内置文档编辑 Agent，运行在 OnlyOffice 编辑器侧栏中，通过调用工具直接操作当前文档。当前编辑器类型：' + kind + '（word=文档 / cell=表格 / slide=演示 / pdf）。',
      '',
      '可用工具（每次回复恰好一行 TOOL_CALL {...} 调用一个工具，或以 FINAL 开头给出最终答复）：',
      TOOLS_DOC[kind] || TOOLS_DOC.pdf,
      '',
      '输出格式（硬性要求）：每一轮回复的完整内容必须恰好是一行 TOOL_CALL {...}（单个工具、合法 JSON、前后不得有任何解释/思考/多余文字），或以 FINAL 开头的最终答复。同一轮绝不输出两个 TOOL_CALL。',
      '',
      '工作方式（严格遵循）：',
      '1. 接到任务先 get_doc_info（及必要的 search_text/read_document）了解文档结构，再动手；',
      '2. 编辑粒度自主判断：局部修改用 replace_text/replace_element/format_element（段落级），新增用 insert_content，大范围重构才逐段处理——不要一上来整篇重写；',
      '3. 小步执行：一次一个工具调用，拿到结果再决定下一步；工具失败时读取错误调整策略或换路径，不要重复同一失败调用；',
      '4. 关键修改后可用 read_document/search_text 验证；',
      '5. 完成后输出 FINAL + 简明中文总结（改了什么、各在哪个位置），不要在 FINAL 里再调用工具；',
      '6. 用户以「针对选区」开头时，其选区文本会在消息中给出（段落粒度替换即等价于改写选区）。',
      '',
      'TOOL_CALL 格式示例（单行 JSON）：',
      'TOOL_CALL {"tool":"replace_text","args":{"find":"旧文本","md":"**新文本**"}}'
    ];
    if (docInfo && docInfo.ok) sys.push('', '【初始文档结构】\n' + JSON.stringify(docInfo).slice(0, 2500));
    if (selection) sys.push('', '【用户圈定选区】\n' + selection.slice(0, 2000));
    return sys.join('\n');
  }

  // ================= 发送（agent 循环 / 单轮对话） =================
  var MAX_TOOL_ROUNDS = 14;

  function setBusyUI(b) {
    busy = b;
    sendBtn.style.display = b ? 'none' : '';
    stopBtn.style.display = b ? '' : 'none';
  }

  function send() {
    var text = q.value.trim();
    if (!text || busy) return;
    q.value = '';
    setBusyUI(true);
    userBubble(text, selection ? ('选区 ' + selection.length + ' 字') : null);
    thinkEl = null;
    controller = new AbortController();

    var prompt = selection ? '【针对选区】' + text : text;

    if (!editMode) {
      // ---- 仅对话：单轮问答 ----
      var acc = '';
      var bubble = null;
      var thinkStart = 0;
      var msgs = [{ role: 'system', content: '你是 OnlyOffice 文档助手（DocFlow AI，「仅对话」模式）。围绕当前文档回答，输出简洁 Markdown；不改动文档。' + (docInfo ? '\n文档结构：' + JSON.stringify(docInfo).slice(0, 1200) : '') }].concat(history, [{ role: 'user', content: prompt }]);
      chat(msgs, {
        onDelta: function (d) {
          acc += d;
          if (!bubble) bubble = el('div', 'm a');
          bubble.innerHTML = mdToHtml(acc) + '<span class="caret"></span>';
          log.scrollTop = log.scrollHeight;
        },
        onThinking: function (th) { if (!thinkStart) thinkStart = Date.now(); appendThink(th); },
        onDone: function () { settleThink(thinkStart ? Date.now() - thinkStart : 0); if (bubble) bubble.innerHTML = mdToHtml(acc); }
      })
        .then(function () {
          lastReply = acc;
          if (acc) { history.push({ role: 'user', content: prompt }, { role: 'assistant', content: acc.slice(0, 4000) }); if (history.length > 12) history = history.slice(-12); }
          else el('div', 'm a', '（模型未返回内容）');
        })
        .catch(function (err) {
          if (err && err.name === 'AbortError') { if (bubble) bubble.innerHTML = mdToHtml(acc) + '<br>（已停止）'; }
          else el('div', 'm a err', esc(err && err.status === 401 ? '登录态失效（请刷新文档页后重试）' : String(err && err.message || err)));
        })
        .then(function () { setBusyUI(false); controller = null; });
      return;
    }

    // ---- 自主编辑：agent 工具循环 ----
    var rounds = 0;
    var convo = [{ role: 'system', content: buildSystem() }, { role: 'user', content: prompt }];
    var finalText = '';
    var thinkStart = 0;

    function finishOk(summary) {
      settleThink(thinkStart ? Date.now() - thinkStart : 0);
      if (summary) el('div', 'm a', mdToHtml(summary));
      history.push({ role: 'user', content: prompt }, { role: 'assistant', content: ('已完成：' + summary).slice(0, 2000) });
      if (history.length > 12) history = history.slice(-12);
      // 编辑后刷新结构缓存，供下一轮 system 上下文
      probeEditor().then(function () {} );
      setBusyUI(false); controller = null;
      setHint('已完成（文档由 DS 自动保存落版本链）');
    }
    function finishErr(msg) {
      settleThink(0);
      el('div', 'm a err', esc(msg));
      setBusyUI(false); controller = null;
    }

    (function loop() {
      if (rounds >= MAX_TOOL_ROUNDS) { finishErr('已达工具调用轮次上限（' + MAX_TOOL_ROUNDS + '），已执行的操作保留，可继续发送指令接力'); return; }
      rounds++;
      var acc2 = '';
      var raw = null;
      var curCard = null;
      chat(convo, {
        onDelta: function (d) {
          acc2 += d;
          if (!raw) raw = el('div', 'raw', '');
          raw.textContent = acc2.slice(-400) + ' ▌';
          log.scrollTop = log.scrollHeight;
        },
        onThinking: function (th) { if (!thinkStart) thinkStart = Date.now(); appendThink(th); },
        onDone: function () {}
      })
        .then(function () {
          if (raw) raw.remove();
          var parsed = parseAgentReply(acc2);
          if (parsed.type === 'final') {
            finalText = parsed.text || acc2;
            finishOk(finalText);
            return;
          }
          var call = parsed.call || {};
          var tool = call.tool || '';
          var args = call.args || {};
          curCard = toolCard(tool, args);
          setHint('Agent 执行中（第 ' + rounds + ' 轮）：' + tool);
          return runTool(tool, args).then(function (r) {
            var ok = !!(r && r.ok);
            toolDone(curCard, ok, r);
            convo.push({ role: 'assistant', content: 'TOOL_CALL ' + JSON.stringify(call) });
            convo.push({ role: 'user', content: 'TOOL_RESULT ' + JSON.stringify(r).slice(0, 3000) });
            loop();
          });
        })
        .catch(function (err) {
          if (raw) raw.remove();
          if (err && err.name === 'AbortError') {
            finishErr('已停止（已执行的操作保留在文档中，Ctrl+Z 可撤销）');
          } else {
            finishErr(err && err.status === 401 ? '登录态失效（请刷新文档页后重试）' : String(err && err.message || err));
          }
        });
    })();
  }

  sendBtn.onclick = send;
  stopBtn.onclick = function () { if (controller) controller.abort(); };
  q.addEventListener('keydown', function (e) {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); send(); }
  });

  // ================= 插件生命周期 =================
  var apiRegistered = false;
  function registerPluginApi() {
    if (apiRegistered || !window.Asc || !window.Asc.plugin) return;
    apiRegistered = true;
    window.Asc.plugin.init = function () {
      setHint('就绪 · 正在读取文档结构…');
      void refresh()
        .then(function () { return loadModels().then(renderModels); })
        .then(probeEditor)
        .then(function (r) {
          if (r && r.ok) setHint('就绪 · ' + (editorType === 'word' ? ('文档 ' + (r.elements || 0) + ' 个元素') : editorType === 'cell' ? ('表格 ' + (r.rows || 0) + '×' + (r.cols || 0)) : editorType === 'slide' ? (r.slides + ' 页') : 'PDF（只读）') + ' · 描述目标即可，AI 自主完成');
          else setHint('就绪（文档结构读取失败，仍可对话）');
        })
        .catch(function () { setHint('未登录 DocFlow（请先在 DocFlow 页面登录后刷新文档）'); });
    };
    window.Asc.plugin.onMethodReturn = function (returnValue) {
      if (returnValue && typeof returnValue.Text === 'string') {
        selection = returnValue.Text;
        setHint(selection ? ('已引用选中（' + selection.length + ' 字）') : '选区为空');
      }
    };
    window.Asc.plugin.button = function () {};
  }
  if (window.Asc && window.Asc.plugin) {
    registerPluginApi();
  } else {
    var tries = 0;
    var poll = window.setInterval(function () {
      tries++;
      if (window.Asc && window.Asc.plugin) {
        window.clearInterval(poll);
        registerPluginApi();
        if (window.Asc.plugin.init) window.Asc.plugin.init();
        return;
      }
      if (tries >= 20) {
        window.clearInterval(poll);
        setHint('独立调试模式（文档操作需在 OnlyOffice 编辑器内）');
      }
    }, 500);
    void refresh().then(function () { return loadModels().then(renderModels); }).catch(function () {});
  }
})();
