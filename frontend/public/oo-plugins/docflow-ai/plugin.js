/* DocFlow AI OnlyOffice 插件 v7.0 — 按编辑器类型的工具套装
 *
 * v7 相对 v6 的关键变化（见 CHANGELOG）：
 * - 运行时函数改为真实函数声明，经 .toString() 序列化注入 DS 沙箱
 *   （v6 为字符串数组，不可读不可语法校验；现可 node --check）。
 * - 坐标/序号全编辑器统一 1 基：excel [行,列]（[1,1]=A1）、word 元素序号
 *   从 1 起、slide 页码从 1 起（v6 word/slide 为 0 基，跨编辑器易混）。
 * - excel 新增 write_table 组合工具（二维数组整表写入 + 表头自动样式），
 *   根除逐格坐标错位类问题；write_cells/read_range 返回 A1 地址便于校验。
 * - 修复 v6 format_range SetFillColor 传数组导致样式静默失效的 bug。
 * - word/slide 工具扩充（read_table / col_op / find_text / insert_image /
 *   add_table / set_text 等）；提示词按编辑器类型分节细化。
 * - 缓存版本号 v=7（index.html）。 */
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
  var history = [], busy = false, token = '', controller = null;
  var webOn = false, thinkOn = true, editMode = true;
  var editorType = null, docInfo = null, lastReply = '';
  try { if (/theme-type=(dark)/.exec(window.location.search)) document.documentElement.dataset.themeType = 'dark'; } catch (e) {}
  function readEditorKind() {
    try { var k = window.localStorage.getItem('docflow.ai.editor.kind');
      if (k === 'word' || k === 'cell' || k === 'slide' || k === 'pdf') return k;
    } catch (e) {}
    return null;
  }
  function csrfToken() { var m = document.cookie.match(/(?:^|; )docflow_csrf=([^;]*)/); return m ? decodeURIComponent(m[1]) : null; }
  function refresh() {
    var h = { 'Content-Type': 'application/json' };
    var c = csrfToken(); if (c) h['X-CSRF-Token'] = c;
    return fetch('/api/v1/auth/refresh', { method: 'POST', credentials: 'same-origin', headers: h })
      .then(function (r) { if (!r.ok) throw { status: r.status }; return r.json(); })
      .then(function (d) { token = d.access_token || ''; });
  }
  function loadModels() {
    return fetch('/api/v1/ai/models', { credentials: 'same-origin', headers: { Authorization: 'Bearer ' + token } })
      .then(function (r) { return r.json(); })
      .then(function (d) {
        var items = [];
        var provs = Array.isArray(d && d.providers) ? d.providers : [];
        for (var pi = 0; pi < provs.length; pi++) {
          var pv = provs[pi] || {}; var raw = Array.isArray(pv.models) ? pv.models : [];
          for (var i = 0; i < raw.length; i++) {
            var m = raw[i] || {}, caps = m.capabilities || {};
            if (caps && typeof caps === 'object' && caps.kind !== 'chat') continue;
            items.push({ id: (pv.id || '') + '/' + (m.id || ''), label: (pv.name || pv.id || '') + ' / ' + (m.id || ''), providerId: pv.id || '', modelId: m.id || '' });
          }
        }
        return items;
      }).catch(function () { return []; });
  }
  function renderModels(items) {
    var saved = ''; try { saved = window.localStorage.getItem('docflow.ai.model') || ''; } catch (e) {}
    modelSel.innerHTML = '';
    var o = document.createElement('option'); o.value = ''; o.textContent = '默认模型'; modelSel.appendChild(o);
    for (var i = 0; i < items.length; i++) {
      var opt = document.createElement('option');
      opt.value = items[i].id; opt.textContent = items[i].label;
      if (saved === items[i].id) opt.selected = true;
      modelSel.appendChild(opt);
    }
  }
  function currentModel() {
    var v = modelSel.value; if (!v) return null;
    var i = v.lastIndexOf('/');
    return { providerId: v.slice(0, i), modelId: v.slice(i + 1) };
  }
  function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); }
  function inlineMd(s) { return esc(s).replace(/`([^`]+)`/g, '<code>$1</code>').replace(/\*\*([^*]+)\*\*/g, '<b>$1</b>').replace(/\[([^\]]+)\]\(([^)]+)\)/g, '<a href="$2" target="_blank" rel="noopener">$1</a>'); }
  function mdToHtml(md) {
    var L = String(md).replace(/\r/g, '').split('\n');
    var out = [], lo = false, co = false, cb = [], tb = [];
    var fl = function () { if (lo) { out.push('</ul>'); lo = false; } };
    var ft = function () {
      if (!tb.length) return;
      var rows = tb.filter(function (r) { return !/^\|[\s:|-]+\|?$/.test(r); });
      if (rows.length) {
        var h = ['<table>'];
        for (var i = 0; i < rows.length; i++) {
          var cs = rows[i].replace(/^\||\|$/g, '').split('|');
          h.push('<tr>' + cs.map(function (c, j) { return (i === 0 ? '<th>' : '<td>') + inlineMd(c.trim()) + (i === 0 ? '</th>' : '</td>'); }).join('') + '</tr>');
        }
        h.push('</table>'); out.push(h.join(''));
      }
      tb = [];
    };
    for (var i = 0; i < L.length; i++) {
      var ln = L[i];
      if (/^```/.test(ln)) {
        if (co) { out.push('<pre><code>' + esc(cb.join('\n')) + '</code></pre>'); cb = []; co = false; }
        else { fl(); ft(); co = true; }
        continue;
      }
      if (co) { cb.push(ln); continue; }
      if (/^\|.*\|/.test(ln)) { fl(); tb.push(ln.trim()); continue; }
      ft();
      var h = /^(#{1,3})\s+(.*)$/.exec(ln);
      if (h) { fl(); out.push('<h' + h[1].length + '>' + inlineMd(h[2]) + '</h' + h[1].length + '>'); continue; }
      if (/^\s*[-*]\s+/.test(ln)) {
        if (!lo) { out.push('<ul>'); lo = true; }
        out.push('<li>' + inlineMd(ln.replace(/^\s*[-*]\s+/, '')) + '</li>');
        continue;
      }
      fl();
      if (ln.trim() === '') continue;
      out.push('<p>' + inlineMd(ln) + '</p>');
    }
    fl(); ft();
    if (co) out.push('<pre><code>' + esc(cb.join('\n')) + '</code></pre>');
    return out.join('');
  }
  function el(tag, cls, html) {
    var d = document.createElement(tag);
    if (cls) d.className = cls;
    if (html !== undefined) d.innerHTML = html;
    log.appendChild(d); log.scrollTop = log.scrollHeight;
    return d;
  }
  function setHint(t) { hintEl.textContent = t; }
  function userBubble(text) { el('div', 'm u', esc(text)); }
  var thinkEl = null;
  function appendThink(chunk) {
    if (!thinkEl) {
      thinkEl = el('details', 'think', '<summary>思考中…</summary>');
      thinkEl.open = true;
      var b = document.createElement('div'); b.className = 't'; thinkEl.appendChild(b); thinkEl._b = b;
    }
    thinkEl._b.textContent += chunk;
    if (thinkEl._b.textContent.length > 12000) thinkEl._b.textContent = '…' + thinkEl._b.textContent.slice(-12000);
    log.scrollTop = log.scrollHeight;
  }
  function settleThink(ms) {
    if (!thinkEl) return;
    thinkEl.open = false;
    thinkEl.firstChild.textContent = '已深度思考' + (ms > 0 ? '（用时 ' + (ms / 1000).toFixed(1) + ' s）' : '');
  }
  function toolCard(tool, args) {
    return el('div', 'tcall',
      '<div class="t-head"><span class="spin"></span><span>🔧 ' + esc(tool) + '</span></div>' +
      '<div class="t-args">' + esc(JSON.stringify(args)).slice(0, 250) + '</div>');
  }
  function toolDone(card, ok, payload) {
    var sp = card.querySelector('.t-head span:last-child');
    var name = sp ? sp.textContent : '?';
    card.classList.add(ok ? 'ok' : 'bad');
    card.querySelector('.t-head').innerHTML = (ok ? '✅ ' : '⚠️ ') + esc(name);
    var r = document.createElement('div'); r.className = 't-res';
    r.textContent = (typeof payload === 'string' ? payload : JSON.stringify(payload)).slice(0, 400);
    card.appendChild(r); log.scrollTop = log.scrollHeight;
  }

  /* ====================================================================
   * 编辑器工具运行时（在 OnlyOffice 文档沙箱内执行）
   *
   * 约束（勿破坏）：
   * - 全部 ES5（无箭头函数/let/const/模板串/for-of）——DS 沙箱按 ES5 求值；
   * - 自包含：不得引用本 IIFE 的任何外部变量（callCommand 序列化注入，
   *   闭包不可用）——函数间只经彼此名字互调（声明提升，注入后同域可见）；
   * - 坐标/序号一律 1 基（对 AI 的契约），实现内部负责换算 0 基。
   * ==================================================================== */

  /** 安全调用沙箱对象方法：不存在或抛错时静默跳过（定制 CJK 构建部分 API
   * 受限），返回是否成功——样式类操作用，失败不阻断主流程。 */
  function rt_try(o, m) {
    try {
      if (o && typeof o[m] === 'function') { o[m].apply(o, Array.prototype.slice.call(arguments, 2)); return true; }
    } catch (e) {}
    return false;
  }
  function rt_clip(s, n) { s = String(s); return s.length > n ? s.slice(0, n) + '…' : s; }
  /** 编辑器类型探测（pdf = 三类文档 API 皆无，仅对话）。 */
  function rt_kind() {
    if (typeof Api.GetActiveSheet === 'function') return 'cell';
    if (typeof Api.GetPresentation === 'function') return 'slide';
    if (typeof Api.GetDocument === 'function') return 'word';
    return 'pdf';
  }
  /** '#RRGGBB' → [r,g,b]（非法回退黑）。 */
  function rt_hx(s) {
    s = String(s).replace('#', '');
    return [parseInt(s.slice(0, 2), 16) || 0, parseInt(s.slice(2, 4), 16) || 0, parseInt(s.slice(4, 6), 16) || 0];
  }
  /** 1 基列号 → Excel 列字母（1→A, 26→Z, 27→AA）。 */
  function rt_colName(n) {
    n = Math.floor(Number(n)); if (!(n >= 1)) n = 1;
    var s = '';
    while (n > 0) { var m = (n - 1) % 26; s = String.fromCharCode(65 + m) + s; n = Math.floor((n - 1) / 26); }
    return s;
  }
  /** 1 基 [行,列] → 'A1' 地址。 */
  function rt_a1(r, c) { return rt_colName(c) + (Math.floor(Number(r)) >= 1 ? Math.floor(Number(r)) : 1); }
  /** 正整数参数（<1 或非法回退 def）。 */
  function rt_pos(v, def) { var n = Math.floor(Number(v)); return (isFinite(n) && n >= 1) ? n : def; }
  /** word 元素序号（1 基）→ 文档 0 基索引；非法返回 -1。 */
  function rt_ix0(v) { var n = Math.floor(Number(v)); return (isFinite(n) && n >= 1) ? n - 1 : -1; }

  /* ---- word：Markdown → 文档元素（轻量子集） ---- */
  function rt_runs(p, t) {
    var s = String(t);
    while (true) {
      var a = s.indexOf('**'), b = s.indexOf('**', a + 2), c = s.indexOf('`'), d = s.indexOf('`', c + 1), pk = null;
      if (a >= 0 && b > a) pk = ['b', a, b + 2, s.slice(a + 2, b)];
      if (c >= 0 && d > c && (pk === null || c < pk[1])) pk = ['m', c, d + 1, s.slice(c + 1, d)];
      if (!pk) break;
      if (pk[1] > 0) p.AddText(s.slice(0, pk[1]));
      var r = p.AddText(pk[3]);
      if (pk[0] === 'b') rt_try(r, 'SetBold', true);
      else rt_try(r, 'SetFontName', 'Courier New');
      s = s.slice(pk[2]);
    }
    if (s) p.AddText(s);
  }
  /** 代码块段落：等宽字体 + 浅灰底（尽力而为，API 受限时退化为普通段落）。 */
  function rt_codePara(lines) {
    var p = Api.CreateParagraph(), nl = String.fromCharCode(10);
    for (var i = 0; i < lines.length; i++) {
      if (i) p.AddText(nl);
      var r = p.AddText(lines[i]);
      rt_try(r, 'SetFontName', 'Courier New');
      rt_try(r, 'SetFontSize', 10);
      rt_try(r, 'SetShd', 'clear', 245, 245, 245);
    }
    return p;
  }
  function rt_para(ln) {
    var p = Api.CreateParagraph(), t = ln, h = 0;
    while (t.charAt(0) === '#') { h++; t = t.slice(1); }
    if (h > 0 && t.charAt(0) === ' ') {
      var r = p.AddText(t);
      rt_try(r, 'SetBold', true);
      rt_try(r, 'SetFontSize', h === 1 ? 18 : h === 2 ? 15 : 13.5);
      return p;
    }
    var tr = ln.replace(/^\s+/, '');
    if (tr.charAt(0) === '-' || tr.charAt(0) === '*') { p.AddText('• '); rt_runs(p, tr.replace(/^[-*]\s+/, '')); return p; }
    if (/^\d+\.\s/.test(tr)) { rt_runs(p, tr); return p; }
    rt_runs(p, ln);
    return p;
  }
  function rt_mkTable(rs) { var t = Api.CreateTable(rs[0].length, rs.length); rt_fillTable(t, rs); return t; }
  function rt_fillTable(t, rs) {
    for (var ri = 0; ri < rs.length; ri++)
      for (var ci = 0; ci < rs[ri].length && ci < rs[0].length; ci++) {
        try { rt_fillCell(t.GetRow(ri).GetCell(ci), String(rs[ri][ci]), ri === 0 && rs.length > 1); } catch (e) {}
      }
  }
  /** 填充单元格文本；表头（b=true）加粗 + 浅蓝底纹 + 居中（尽力而为）。 */
  function rt_fillCell(c, tx, b) {
    var cc = c.GetContent(), p = null;
    if (cc.GetElementsCount() > 0) { var e0 = cc.GetElement(0); if (e0 && typeof e0.AddText === 'function') p = e0; }
    if (!p) { p = Api.CreateParagraph(); cc.Push(p); }
    var r = p.AddText(tx);
    if (b) {
      rt_try(r, 'SetBold', true);
      rt_try(p, 'SetJc', 'center');
      rt_try(c, 'SetShd', 'clear', 217, 226, 243);
    }
  }
  function rt_isTable(e) { return !!(e && typeof e.GetRow === 'function'); }
  function rt_pText(e) { try { return e.GetText(); } catch (x) { return ''; } }
  /** 全文扫描：hits[].n 为 1 基元素序号（可直接用于其它工具的 index）。 */
  function rt_scan(query) {
    var d = Api.GetDocument(), n = d.GetElementsCount(), h = [];
    for (var i = 0; i < n; i++) {
      var e = d.GetElement(i), k = rt_isTable(e) ? 'table' : 'para';
      var t = k === 'table' ? '[' + (i + 1) + '] 表格' : rt_pText(e);
      if (!query || t.indexOf(query) >= 0) h.push({ n: i + 1, kind: k, text: rt_clip(t, 400) });
    }
    return { count: n, hits: h };
  }
  /** Markdown 文本 → 元素数组（标题/列表/表格/代码块/粗体/行内代码）。 */
  function rt_mdE(md) {
    var out = [], ls = String(md).split('\n'), ic = false, bf = [], tb = [];
    function ft() {
      if (!tb.length) return;
      var rs = [];
      for (var k = 0; k < tb.length; k++) {
        var t = tb[k].trim();
        if (/^\|[\s:|-]+\|?$/.test(t)) continue;
        rs.push(t.replace(/^\|/, '').replace(/\|$/, '').split('|'));
      }
      if (rs.length) out.push(rt_mkTable(rs));
      tb = [];
    }
    for (var i = 0; i < ls.length; i++) {
      var ln = ls[i];
      if (ln.indexOf('```') === 0) {
        if (ic) { out.push(rt_codePara(bf)); bf = []; ic = false; } else { ft(); ic = true; }
        continue;
      }
      if (ic) { bf.push(ln); continue; }
      if (ln.trim() === '') { ft(); continue; }
      if (ln.charAt(0) === '|') { tb.push(ln); continue; }
      ft();
      out.push(rt_para(ln));
    }
    ft();
    if (ic && bf.length) out.push(rt_codePara(bf));
    return out;
  }
  /** 在指定 0 基索引处插入元素数组（越界回退文末 Push，返回 note）。 */
  function rt_insertAt(d, i0, es) {
    try { for (var k = es.length - 1; k >= 0; k--) d.AddElement(i0, es[k]); return ''; } catch (e) {}
    for (var j = 0; j < es.length; j++) d.Push(es[j]);
    return '（目标位置不可用，已追加到文末）';
  }

  /* ---- 工具实现：word ---- */
  function t_get_doc_info() {
    var k = rt_kind();
    if (k === 'word') {
      var sc = rt_scan(''), sel = '';
      try { if (Api.GetDocument().GetSelectedText) sel = Api.GetDocument().GetSelectedText() || ''; } catch (e) {}
      return { ok: true, kind: k, elements: sc.count, overview: sc.hits.slice(0, 80), selection: rt_clip(sel, 500) };
    }
    if (k === 'cell') return t_x_info();
    if (k === 'slide') return { ok: true, kind: k, slides: Api.GetPresentation().GetSlidesCount() };
    return { ok: true, kind: k };
  }
  function t_read_document(a) {
    var sc = rt_scan(''), from = rt_pos(a.from, 1), to = a.to !== undefined ? rt_pos(a.to, sc.count) : sc.count;
    var maxChars = rt_pos(a.maxChars, 400), d = Api.GetDocument(), out = [];
    for (var n = from; n <= sc.count && n <= to && out.length < 60; n++) {
      var e = d.GetElement(n - 1), t = rt_isTable(e) ? '' : rt_pText(e);
      out.push({ n: n, kind: rt_isTable(e) ? 'table' : 'para', text: rt_clip(t, maxChars) });
    }
    return { ok: true, total: sc.count, elements: out };
  }
  function t_search_text(a) {
    if (!a.query) return { ok: false, error: 'query 必填' };
    var sc = rt_scan(String(a.query));
    return { ok: true, total: sc.count, hits: sc.hits.slice(0, 30) };
  }
  function t_get_selection() {
    var sel = '';
    try { if (Api.GetDocument().GetSelectedText) sel = Api.GetDocument().GetSelectedText() || ''; } catch (e) {}
    return { ok: true, selection: rt_clip(sel, 3000), empty: !sel };
  }
  function t_insert_content(a) {
    var d = Api.GetDocument(), es = rt_mdE(a.md || '');
    if (!es.length) return { ok: false, error: '空内容' };
    var at = a.at || 'end';
    if (at === 'cursor') { try { d.InsertContent(es); return { ok: true }; } catch (e) { return { ok: false, error: '光标位置不可用' }; } }
    if (at === 'after' && a.index !== undefined) {
      var i0 = rt_ix0(a.index);
      if (i0 < 0) return { ok: false, error: 'index 须为 1 起的元素序号' };
      var note = rt_insertAt(d, i0 + 1, es);
      return { ok: true, note: note || undefined };
    }
    for (var i = 0; i < es.length; i++) d.Push(es[i]);
    return { ok: true };
  }
  /** find（纯文本）命中的段落（1 基序号集合）。 */
  function rt_paraHits(find) {
    var sc = rt_scan(String(find)), ix = [];
    for (var h = 0; h < sc.hits.length; h++) if (sc.hits[h].kind === 'para') ix.push(sc.hits[h].n - 1);
    return ix;
  }
  /** 把元素数组替换到 0 基索引 i0 处（删旧插新）。 */
  function rt_swapAt(d, i0, es) {
    try { d.RemoveElement(i0); } catch (y) { return false; }
    rt_insertAt(d, i0, es);
    return true;
  }
  function t_replace_text(a) {
    var d = Api.GetDocument(), ix = rt_paraHits(a.find || '');
    if (!ix.length) return { ok: false, error: '未找到：' + rt_clip(String(a.find || ''), 60) };
    if ((a.scope || 'first') === 'first') ix = [ix[0]];
    var es = rt_mdE(a.md || ''), dn = 0;
    for (var t = ix.length - 1; t >= 0; t--) { if (rt_swapAt(d, ix[t], es)) dn++; }
    return dn ? { ok: true, replaced: dn } : { ok: false, error: '替换失败' };
  }
  function t_replace_all(a) {
    var d = Api.GetDocument(), ix = rt_paraHits(a.find || '');
    if (!ix.length) return { ok: false, error: '未找到：' + rt_clip(String(a.find || ''), 60) };
    var es = rt_mdE(String(a.replace || '')), dn = 0;
    for (var t = ix.length - 1; t >= 0; t--) { if (rt_swapAt(d, ix[t], es)) dn++; }
    return dn ? { ok: true, replaced: dn } : { ok: false, error: '替换失败' };
  }
  function t_replace_element(a) {
    var d = Api.GetDocument(), i0 = rt_ix0(a.index);
    if (i0 < 0) return { ok: false, error: 'index 须为 1 起的元素序号' };
    if (!rt_swapAt(d, i0, rt_mdE(a.md || ''))) return { ok: false, error: '元素不存在' };
    return { ok: true };
  }
  function t_delete_element(a) {
    var i0 = rt_ix0(a.index);
    if (i0 < 0) return { ok: false, error: 'index 须为 1 起的元素序号' };
    try { Api.GetDocument().RemoveElement(i0); return { ok: true }; } catch (x) { return { ok: false, error: '元素不存在（序号越界）' }; }
  }
  function t_format_element(a) {
    var p = Api.GetDocument().GetElement(rt_ix0(a.index));
    if (!p) return { ok: false, error: '元素不存在（index 须为 1 起，见 read_document 返回的 n）' };
    var n = 0;
    function F(r) {
      var did = false;
      if (a.bold !== undefined) did = rt_try(r, 'SetBold', a.bold) || did;
      if (a.italic === true) did = rt_try(r, 'SetItalic', true) || did;
      if (a.underline === true) did = rt_try(r, 'SetUnderline', true) || did;
      if (a.fontSize) did = rt_try(r, 'SetFontSize', a.fontSize) || did;
      if (a.fontColor) { var c = rt_hx(a.fontColor); did = rt_try(r, 'SetColor', c[0], c[1], c[2], false) || did; }
      if (a.highlight) did = rt_try(r, 'SetHighlight', a.highlight) || did;
      if (did) n++;
    }
    F(p);
    try { if (p.GetElementsCount) for (var i = 0; i < p.GetElementsCount(); i++) F(p.GetElement(i)); } catch (x) {}
    return n ? { ok: true, applied: n } : { ok: false, error: '该元素无文本可格式化' };
  }
  function t_set_paragraph_style(a) {
    var p = Api.GetDocument().GetElement(rt_ix0(a.index));
    if (!p || typeof p.AddText !== 'function') return { ok: false, error: '非段落元素' };
    if (a.style && a.style !== 'normal') {
      var m = { heading_1: 'Heading 1', heading_2: 'Heading 2', heading_3: 'Heading 3', heading1: 'Heading 1', heading2: 'Heading 2', heading3: 'Heading 3', title: 'Title', quote: 'Quote' };
      var name = m[String(a.style).toLowerCase()] || a.style;
      try { if (!rt_try(p, 'SetStyle', Api.GetStyle(name))) return { ok: false, error: '样式不可用：' + name }; } catch (e) { return { ok: false, error: '样式不可用：' + name }; }
    }
    if (a.align) rt_try(p, 'SetJc', a.align);
    return { ok: true };
  }
  function t_insert_table(a) {
    var rows = a.rows;
    if (!rows || !rows.length || !rows[0].length) return { ok: false, error: 'rows 必填（二维数组，首行为表头）' };
    var t = rt_mkTable(rows), d = Api.GetDocument();
    if ((a.at || 'end') === 'cursor') { try { d.InsertContent([t]); return { ok: true }; } catch (e) {} }
    d.Push(t);
    return { ok: true };
  }
  function t_read_table(a) {
    var t = Api.GetDocument().GetElement(rt_ix0(a.index));
    if (!rt_isTable(t)) return { ok: false, error: '该元素不是表格（index 须为 read_document 中 kind=table 的序号）' };
    var rows = [];
    try {
      var rc = t.GetRowsCount();
      for (var r = 0; r < rc && r < 60; r++) {
        var cells = [];
        for (var c = 0; c < 50; c++) {
          var txt = '';
          try { txt = t.GetRow(r).GetCell(c).GetContent().GetElement(0).GetText(); } catch (x) { break; }
          cells.push(rt_clip(txt, 200));
        }
        rows.push(cells);
      }
    } catch (x) { return { ok: false, error: '读取表格失败：' + String(x) }; }
    return { ok: true, rows: rows };
  }
  function t_table_op(a) {
    var t = Api.GetDocument().GetElement(rt_ix0(a.index));
    if (!rt_isTable(t)) return { ok: false, error: '该元素不是表格' };
    if (a.op === 'add_row') {
      try {
        t.AddRow(t.GetRow(t.GetRowsCount() - 1));
        if (a.values && a.values.length) {
          var r = t.GetRowsCount() - 1;
          for (var c = 0; c < a.values.length; c++) { try { rt_fillCell(t.GetRow(r).GetCell(c), String(a.values[c]), false); } catch (x) { break; } }
        }
        return { ok: true, rows: t.GetRowsCount() };
      } catch (e) { return { ok: false, error: '不可用：' + String(e) }; }
    }
    if (a.op === 'set_cell') {
      var ri = rt_ix0(a.r), ci = rt_ix0(a.c);
      if (ri < 0 || ci < 0) return { ok: false, error: 'r/c 须为 1 起' };
      try { rt_fillCell(t.GetRow(ri).GetCell(ci), String(a.text || ''), false); return { ok: true }; }
      catch (x) { return { ok: false, error: '单元格不可达（越界）' }; }
    }
    return { ok: false, error: 'op 须为 add_row | set_cell' };
  }
  function t_insert_page_break() {
    var d = Api.GetDocument(), p = Api.CreateParagraph();
    try { p.AddPageBreak(); d.Push(p); return { ok: true }; } catch (e) { return { ok: false, error: '不可用' }; }
  }
  function t_add_comment(a) {
    try {
      var rs = Api.GetDocument().Search(String(a.find || ''));
      if (!rs || !rs.length) return { ok: false, error: '未找到' };
      rs[0].AddComment(a.text || '');
      return { ok: true };
    } catch (e) { return { ok: false, error: String(e) }; }
  }
  function t_insert_image(a) {
    try {
      var d = Api.GetDocument(), img = Api.CreateImage(String(a.url || ''), a.width || 400, a.height || 300);
      var p = Api.CreateParagraph(); p.AddDrawing(img); d.Push(p);
      return { ok: true };
    } catch (e) { return { ok: false, error: '图片不可用：' + String(e) }; }
  }
  function t_insert_page_number() {
    try {
      var sec = Api.GetDocument().GetSection(0);
      if (!sec) return { ok: false, error: '无节信息' };
      var f = sec.GetFooter();
      if (!f) return { ok: false, error: '无页脚' };
      var p = f.GetElement(0);
      if (!p) { p = Api.CreateParagraph(); f.Push(p); }
      p.AddPageNumber();
      return { ok: true };
    } catch (e) { return { ok: false, error: String(e) }; }
  }
  function t_get_page_count() { return { ok: true, elements: Api.GetDocument().GetElementsCount() }; }
  function t_set_page_orientation(a) {
    try {
      var sec = Api.GetDocument().GetSection(0);
      if (!sec) return { ok: false, error: '无节信息' };
      var land = a.orientation === 'landscape';
      sec.SetPageSize(land ? 842 : 595, land ? 595 : 842);
      return { ok: true };
    } catch (e) { return { ok: false, error: String(e) }; }
  }
  function t_insert_horizontal_rule() {
    try {
      var d = Api.GetDocument(), p = Api.CreateParagraph();
      p.SetJc('center');
      var r = p.AddText('——————————');
      rt_try(r, 'SetColor', 180, 180, 180, false);
      d.Push(p);
      return { ok: true };
    } catch (e) { return { ok: false, error: String(e) }; }
  }
  function t_insert_hyperlink(a) {
    var d = Api.GetDocument(), p = Api.CreateParagraph();
    var r = p.AddText(a.text || String(a.url || ''));
    rt_try(r, 'SetColor', 79, 124, 255, false);
    rt_try(r, 'SetUnderline', true);
    d.Push(p);
    return { ok: true, note: '以链接样式文本呈现（DS 插件 API 无原生超链接锚点）' };
  }
  function t_set_page_margins(a) {
    try {
      var sec = Api.GetDocument().GetSection(0);
      if (!sec) return { ok: false, error: '无节信息' };
      rt_try(sec, 'SetPageMargins', a.top || 72, a.right || 72, a.bottom || 72, a.left || 72);
      return { ok: true };
    } catch (e) { return { ok: false, error: String(e) }; }
  }
  function t_insert_toc() {
    try {
      var d = Api.GetDocument(), out = [rt_para('## 目录')], sc = rt_scan('');
      for (var i = 0; i < sc.hits.length; i++) {
        var h = sc.hits[i];
        if (h.kind === 'para' && h.text && h.text.indexOf('#') === 0) out.push(rt_para(h.text));
      }
      if (out.length < 2) out.push(rt_para('（未找到 Markdown 标题段落）'));
      for (var j = 0; j < out.length; j++) d.Push(out[j]);
      return { ok: true };
    } catch (e) { return { ok: false, error: String(e) }; }
  }
  function t_set_header_footer(a) {
    try {
      var sec = Api.GetDocument().GetSection(0);
      if (!sec) return { ok: false, error: '无节信息' };
      var hf = a.position === 'header' ? sec.GetHeader() : sec.GetFooter();
      if (!hf) return { ok: false, error: '无页眉/页脚' };
      var p = hf.GetElement(0);
      if (!p) { p = Api.CreateParagraph(); hf.Push(p); }
      if (a.text !== undefined) p.AddText(String(a.text));
      return { ok: true };
    } catch (e) { return { ok: false, error: String(e) }; }
  }
  function t_insert_code_block(a) {
    var d = Api.GetDocument();
    d.Push(rt_codePara(String(a.code || '').split('\n')));
    return { ok: true };
  }

  /* ---- 工具实现：cell（excel）---- */
  function t_x_info() {
    var sh = Api.GetActiveSheet(), v = [];
    try { v = sh.GetRange('A1:Z300').GetValues() || []; } catch (e) { v = []; }
    var mr = 0, mc = 0;
    for (var i = 0; i < v.length; i++) {
      var r = v[i];
      for (var j = 0; j < r.length; j++) {
        if (r[j] !== '' && r[j] !== null && r[j] !== undefined) { if (i + 1 > mr) mr = i + 1; if (j + 1 > mc) mc = j + 1; }
      }
    }
    var name = ''; rt_try0(function () { name = sh.GetName(); });
    return { ok: true, kind: 'cell', sheet: name, rows: mr, cols: mc, note: mr ? undefined : '空表（探测范围 A1:Z300）' };
  }
  /** 无异常执行 f（沙箱内 try 包裹 helper）。 */
  function rt_try0(f) { try { f(); } catch (e) {} }
  function t_read_range(a) {
    var sh = Api.GetActiveSheet();
    var r1 = rt_pos(a.r1, 1), c1 = rt_pos(a.c1, 1);
    var r2 = a.r2 !== undefined ? rt_pos(a.r2, r1) : r1 + 19;
    var c2 = a.c2 !== undefined ? rt_pos(a.c2, c1) : c1 + 9;
    if (r2 < r1) r2 = r1; if (c2 < c1) c2 = c1;
    r2 = Math.min(r2, r1 + 59); c2 = Math.min(c2, c1 + 19);
    var v = [];
    try { v = sh.GetRange(rt_a1(r1, c1) + ':' + rt_a1(r2, c2)).GetValues() || []; } catch (e) { v = []; }
    return { ok: true, address: rt_a1(r1, c1) + ':' + rt_a1(r2, c2), from: [r1, c1], rows: v.length, values: v };
  }
  function t_write_cells(a) {
    var cells = a.cells;
    if (!cells || !cells.length) return { ok: false, error: 'cells 必填：[[行,列,值],…]（1 基，[1,1]=A1）' };
    var sh = Api.GetActiveSheet(), n = 0, err = '', minR = 1e9, minC = 1e9, maxR = 0, maxC = 0;
    for (var i = 0; i < cells.length; i++) {
      var c = cells[i];
      var r = Math.floor(Number(c && c[0])), col = Math.floor(Number(c && c[1]));
      if (!(r >= 1) || !(col >= 1)) { err = '坐标须 1 起（收到 [' + String(c && c[0]) + ',' + String(c && c[1]) + ']）'; continue; }
      try { sh.GetRange(rt_a1(r, col)).SetValue(String(c[2])); n++; if (r < minR) minR = r; if (col < minC) minC = col; if (r > maxR) maxR = r; if (col > maxC) maxC = col; }
      catch (e) { err = String(e); }
    }
    if (!n) return { ok: false, error: '未写入' + (err ? '：' + err : '') };
    return { ok: true, written: n, range: rt_a1(minR, minC) + ':' + rt_a1(maxR, maxC), error: err ? rt_clip(err, 120) : undefined };
  }
  function t_write_table(a) {
    var rows = a.rows;
    if (!rows || !rows.length || !rows[0].length) return { ok: false, error: 'rows 必填：二维数组（含表头行），如 [["月份","销售额"],["1月",100]]' };
    var sh = Api.GetActiveSheet();
    var r0 = rt_pos(a.startRow, 1), c0 = rt_pos(a.startCol, 1);
    var n = 0, mc = c0;
    for (var ri = 0; ri < rows.length; ri++) {
      var row = rows[ri];
      for (var ci = 0; ci < row.length; ci++) {
        var v = row[ci];
        if (v === undefined || v === null || v === '') continue;
        try { sh.GetRange(rt_a1(r0 + ri, c0 + ci)).SetValue(String(v)); n++; } catch (e) {}
      }
      if (c0 + row.length - 1 > mc) mc = c0 + row.length - 1;
    }
    var rEnd = r0 + rows.length - 1;
    var header = a.header !== false && rows.length > 1;
    var styled = false;
    if (header && n) {
      var hr = sh.GetRange(rt_a1(r0, c0) + ':' + rt_a1(r0, mc));
      var s1 = rt_try(hr, 'SetBold', true);
      var s2 = rt_try(hr, 'SetFillColor', 217, 226, 243);
      var s3 = rt_try(hr, 'SetHorizontalAlignment', 'center');
      styled = s1 || s2 || s3;
    }
    if (a.autofit !== false) {
      for (var c = c0; c <= mc && c < c0 + 30; c++) rt_try(sh.GetRange(rt_colName(c) + ':' + rt_colName(c)), 'SetColumnWidth', 'auto');
    }
    if (a.freeze) rt_try(sh.GetRange(rt_a1(r0 + 1, 1)), 'SetFrozen', true);
    if (!n) return { ok: false, error: '未写入任何单元格（rows 全空？）' };
    return { ok: true, written: n, range: rt_a1(r0, c0) + ':' + rt_a1(rEnd, mc), headerRow: header ? r0 : 0, headerStyled: styled };
  }
  function t_format_range(a) {
    var sh = Api.GetActiveSheet();
    var r1 = rt_pos(a.r1, 1), c1 = rt_pos(a.c1, 1);
    var r2 = a.r2 !== undefined ? rt_pos(a.r2, r1) : r1, c2 = a.c2 !== undefined ? rt_pos(a.c2, c1) : c1;
    if (r2 < r1) r2 = r1; if (c2 < c1) c2 = c1;
    var rg = sh.GetRange(rt_a1(r1, c1) + ':' + rt_a1(r2, c2));
    var ops = [];
    if (a.bold !== undefined) { if (rt_try(rg, 'SetBold', a.bold)) ops.push('bold'); }
    if (a.italic !== undefined) { if (rt_try(rg, 'SetItalic', a.italic)) ops.push('italic'); }
    if (a.fontSize) { if (rt_try(rg, 'SetFontSize', a.fontSize)) ops.push('fontSize'); }
    if (a.fillColor) { var f = rt_hx(a.fillColor); if (rt_try(rg, 'SetFillColor', f[0], f[1], f[2])) ops.push('fill'); }
    if (a.fontColor) { var t = rt_hx(a.fontColor); if (rt_try(rg, 'SetFontColor', t[0], t[1], t[2])) ops.push('fontColor'); }
    if (a.numberFormat) { if (rt_try(rg, 'SetNumberFormat', a.numberFormat)) ops.push('numberFormat'); }
    if (a.hAlign) { if (rt_try(rg, 'SetHorizontalAlignment', a.hAlign)) ops.push('hAlign'); }
    if (a.vAlign) { if (rt_try(rg, 'SetVerticalAlignment', a.vAlign)) ops.push('vAlign'); }
    if (a.wrap !== undefined) { if (rt_try(rg, 'SetWrapText', a.wrap)) ops.push('wrap'); }
    return ops.length ? { ok: true, applied: ops } : { ok: false, error: '无可用样式 API（或参数缺失）' };
  }
  function t_row_op(a) {
    var sh = Api.GetActiveSheet(), at = rt_pos(a.at, 1), count = rt_pos(a.count, 1);
    try {
      var rg = sh.GetRange(at + ':' + (at + count - 1));
      if (a.op === 'insert') rg.Insert();
      else if (a.op === 'delete') rg.Delete();
      else return { ok: false, error: 'op 须 insert | delete' };
      return { ok: true };
    } catch (e) { return { ok: false, error: String(e) }; }
  }
  function t_col_op(a) {
    var sh = Api.GetActiveSheet(), at = rt_pos(a.at, 1), count = rt_pos(a.count, 1);
    try {
      var rg = sh.GetRange(rt_colName(at) + ':' + rt_colName(at + count - 1));
      if (a.op === 'insert') rg.Insert();
      else if (a.op === 'delete') rg.Delete();
      else return { ok: false, error: 'op 须 insert | delete' };
      return { ok: true };
    } catch (e) { return { ok: false, error: String(e) }; }
  }
  function t_set_col_width(a) {
    var sh = Api.GetActiveSheet(), col = rt_pos(a.col, 1);
    if (rt_try(sh.GetRange(rt_colName(col) + ':' + rt_colName(col)), 'SetColumnWidth', a.width === 'auto' ? 'auto' : Number(a.width) || 100)) return { ok: true };
    return { ok: false, error: '不可用' };
  }
  function t_clear_range(a) {
    var sh = Api.GetActiveSheet();
    var r1 = rt_pos(a.r1, 1), c1 = rt_pos(a.c1, 1);
    var r2 = a.r2 !== undefined ? rt_pos(a.r2, r1) : r1, c2 = a.c2 !== undefined ? rt_pos(a.c2, c1) : c1;
    var rg = sh.GetRange(rt_a1(r1, c1) + ':' + rt_a1(r2, c2));
    if (!rt_try(rg, 'Clear')) { if (!rt_try(rg, 'ClearContent')) rt_try(rg, 'SetValue', ''); }
    return { ok: true };
  }
  function t_merge_cells(a) {
    var sh = Api.GetActiveSheet();
    var r1 = rt_pos(a.r1, 1), c1 = rt_pos(a.c1, 1), r2 = rt_pos(a.r2, r1), c2 = rt_pos(a.c2, c1);
    var rg = sh.GetRange(rt_a1(r1, c1) + ':' + rt_a1(r2, c2));
    if (a.merge === false) { if (rt_try(rg, 'UnMerge')) return { ok: true }; }
    else if (rt_try(rg, 'Merge')) return { ok: true };
    return { ok: false, error: '不可用' };
  }
  function t_freeze_panes(a) {
    var sh = Api.GetActiveSheet();
    if (rt_try(sh.GetRange(rt_a1(rt_pos(a.r, 1), rt_pos(a.c, 1))), 'SetFrozen', true)) return { ok: true };
    return { ok: false, error: '不可用' };
  }
  function t_sheet_op(a) {
    var wb = null;
    try { wb = Api.GetWorkbook ? Api.GetWorkbook() : null; } catch (e) {}
    if (!wb) return { ok: false, error: '无工作簿 API' };
    if (a.op === 'list') {
      var names = [], n = 0;
      rt_try0(function () { n = wb.GetSheetsCount(); });
      for (var i = 0; i < n && i < 30; i++) { var idx = i; rt_try0(function () { names.push(wb.GetSheet(idx).GetName()); }); }
      return { ok: true, count: n, names: names, active: (function () { var s = ''; rt_try0(function () { s = Api.GetActiveSheet().GetName(); }); return s; })() };
    }
    if (a.op === 'add') { if (rt_try(wb, 'AddSheet', a.name || '')) return { ok: true }; return { ok: false, error: '不可用' }; }
    if (a.op === 'rename') {
      var i2 = rt_ix0(a.index);
      if (i2 < 0) return { ok: false, error: 'index 必填（1 起工作表序号）' };
      if (rt_try0r(function () { wb.GetSheet(i2).SetName(a.name || ''); })) return { ok: true };
      return { ok: false, error: '不可用' };
    }
    if (a.op === 'activate') {
      var i3 = rt_ix0(a.index);
      if (i3 < 0) return { ok: false, error: 'index 必填（1 起工作表序号）' };
      if (rt_try0r(function () { wb.SetActive(wb.GetSheet(i3)); })) return { ok: true };
      return { ok: false, error: '不可用' };
    }
    return { ok: false, error: 'op 须 list | add | rename | activate' };
  }
  /** 执行 f，返回是否未抛异常。 */
  function rt_try0r(f) { try { f(); return true; } catch (e) { return false; } }
  function t_autofit(a) {
    var sh = Api.GetActiveSheet();
    var from = rt_pos(a.from, 1), to = rt_pos(a.to, Math.max(from, 20));
    var n = 0;
    for (var c = from; c <= to && c < from + 60; c++) { if (rt_try(sh.GetRange(rt_colName(c) + ':' + rt_colName(c)), 'SetColumnWidth', 'auto')) n++; }
    return n ? { ok: true, cols: n } : { ok: false, error: '不可用' };
  }
  function t_sort_range(a) {
    var sh = Api.GetActiveSheet();
    var r1 = rt_pos(a.r1, 1), c1 = rt_pos(a.c1, 1), r2 = rt_pos(a.r2, r1), c2 = rt_pos(a.c2, c1);
    var rg = sh.GetRange(rt_a1(r1, c1) + ':' + rt_a1(r2, c2));
    if (rt_try(rg, 'Sort', rt_pos(a.by, 1) - 1, a.order === 'desc')) return { ok: true };
    return { ok: false, error: '不可用' };
  }
  function t_apply_filter(a) {
    var sh = Api.GetActiveSheet();
    var r1 = rt_pos(a.r1, 1), c1 = rt_pos(a.c1, 1), r2 = rt_pos(a.r2, r1), c2 = rt_pos(a.c2, c1);
    var rg = sh.GetRange(rt_a1(r1, c1) + ':' + rt_a1(r2, c2));
    if (rt_try(rg, 'SetFilter', true)) return { ok: true };
    return { ok: false, error: '不可用' };
  }
  function t_insert_formula(a) {
    var r = Math.floor(Number(a.r)), c = Math.floor(Number(a.c)), f = String(a.formula || '');
    if (!(r >= 1) || !(c >= 1)) return { ok: false, error: 'r/c 须 1 起' };
    if (f.charAt(0) !== '=') f = '=' + f;
    var sh = Api.GetActiveSheet();
    try { sh.GetRange(rt_a1(r, c)).SetValue(f); return { ok: true, cell: rt_a1(r, c) }; } catch (e) { return { ok: false, error: String(e) }; }
  }
  function t_insert_chart(a) {
    try {
      var sh = Api.GetActiveSheet();
      var r1 = rt_pos(a.r1, 1), c1 = rt_pos(a.c1, 1), r2 = rt_pos(a.r2, r1), c2 = rt_pos(a.c2, c1);
      var rg = sh.GetRange(rt_a1(r1, c1) + ':' + rt_a1(r2, c2));
      var ar = rt_pos(a.atRow, r2 + 2), ac = rt_pos(a.atCol, c1);
      if (sh.AddChart) { sh.AddChart(a.type || 'bar', rg, ar, ac, 400, 300); return { ok: true, at: rt_a1(ar, ac) }; }
      return { ok: false, error: '图表 API 不可用' };
    } catch (e) { return { ok: false, error: String(e) }; }
  }
  function t_set_conditional_format(a) {
    var sh = Api.GetActiveSheet();
    var r1 = rt_pos(a.r1, 1), c1 = rt_pos(a.c1, 1), r2 = rt_pos(a.r2, r1), c2 = rt_pos(a.c2, c1);
    var rg = sh.GetRange(rt_a1(r1, c1) + ':' + rt_a1(r2, c2));
    if (rt_try(rg, 'SetConditionalFormat', a.rule || 'cellIs', a.operator || 'greaterThan', a.value || 0, a.format || { fillColor: 'FFCCCC' })) return { ok: true };
    return { ok: false, error: '条件格式 API 不可用' };
  }
  function t_find_text(a) {
    var query = String(a.query || '');
    if (!query) return { ok: false, error: 'query 必填' };
    var sh = Api.GetActiveSheet(), v = [];
    try { v = sh.GetRange('A1:Z300').GetValues() || []; } catch (e) { v = []; }
    var hits = [];
    for (var i = 0; i < v.length && hits.length < 30; i++) {
      var row = v[i];
      for (var j = 0; j < row.length && hits.length < 30; j++) {
        var s = String(row[j] === null || row[j] === undefined ? '' : row[j]);
        if (s && s.indexOf(query) >= 0) hits.push({ cell: rt_a1(i + 1, j + 1), value: rt_clip(s, 80) });
      }
    }
    return { ok: true, found: hits.length, hits: hits };
  }

  /* ---- 工具实现：slide（ppt）---- */
  function rt_slideIdx(a, pr) {
    var total = pr.GetSlidesCount();
    var n = a.slide !== undefined ? rt_pos(a.slide, total) : total;
    return Math.min(n, total) - 1;
  }
  function t_read_slide(a) {
    var pr = Api.GetPresentation(), i = rt_slideIdx(a, pr), s = pr.GetSlideByIndex(i);
    if (!s) return { ok: false, error: '页不存在' };
    var tx = [], n = 0;
    rt_try0(function () { n = s.GetAllShapes().length; });
    rt_try0(function () {
      s.ForEachShape(function (sh) { var t = ''; rt_try0(function () { t = sh.GetText(); }); if (t) tx.push(rt_clip(t, 200)); });
    });
    var notes = '';
    rt_try0(function () { notes = s.GetNotes(); });
    return { ok: true, slide: i + 1, shapes: n, texts: tx, notes: rt_clip(notes, 300) };
  }
  function t_add_slide(a) {
    try {
      var pr = Api.GetPresentation(), s = pr.CreateSlide();
      pr.AddSlide(s);
      var y = 30;
      if (a.title) { rt_try(s, 'AddText', String(a.title), 40, y, 600, 44, 28); y += 70; }
      var bl = a.bullets || [];
      for (var i = 0; i < bl.length && i < 8; i++) { rt_try(s, 'AddText', String(bl[i]), 60, y, 560, 30, 18); y += 40; }
      return { ok: true, slide: pr.GetSlidesCount() };
    } catch (e) { return { ok: false, error: String(e) }; }
  }
  function t_add_text_box(a) {
    var pr = Api.GetPresentation(), s = pr.GetSlideByIndex(rt_slideIdx(a, pr));
    if (!s) return { ok: false, error: '页不存在' };
    if (rt_try(s, 'AddText', String(a.text || ''), a.x || 40, a.y || 100, a.w || 400, a.h || 32, a.fontSize || 18)) return { ok: true };
    return { ok: false, error: '文本框 API 不可用' };
  }
  function t_set_text(a) {
    var pr = Api.GetPresentation(), s = pr.GetSlideByIndex(rt_slideIdx(a, pr));
    if (!s) return { ok: false, error: '页不存在' };
    var shapes = [];
    rt_try0(function () { shapes = s.GetAllShapes(); });
    var idx = rt_ix0(a.shape);
    if (idx < 0 || idx >= shapes.length) return { ok: false, error: 'shape 序号须 1 起（见 read_slide 的 shapes 数量）' };
    if (rt_try(shapes[idx], 'SetText', String(a.text || ''))) return { ok: true };
    return { ok: false, error: '该形状不支持设置文本' };
  }
  function t_add_shape(a) {
    var pr = Api.GetPresentation(), s = pr.GetSlideByIndex(rt_slideIdx(a, pr));
    if (!s) return { ok: false, error: '页不存在' };
    var sh = null;
    try { sh = Api.CreateShape ? Api.CreateShape(a.type || 'rect', a.w || 120, a.h || 60) : null; } catch (e) { sh = null; }
    if (!sh) return { ok: false, error: '形状 API 不可用' };
    rt_try(sh, 'SetPosition', a.x || 60, a.y || 120);
    if (a.fill) { var f = rt_hx(a.fill); rt_try0(function () { sh.SetFill(Api.CreateSolidFill(Api.CreateRGB(f[0], f[1], f[2]))); }); }
    if (a.text) rt_try(sh, 'SetText', String(a.text));
    if (rt_try(s, 'AddObject', sh)) return { ok: true };
    return { ok: false, error: 'AddObject 不可用' };
  }
  function t_slide_insert_image(a) {
    var pr = Api.GetPresentation(), s = pr.GetSlideByIndex(rt_slideIdx(a, pr));
    if (!s) return { ok: false, error: '页不存在' };
    var img = null;
    try { img = Api.CreateImage(String(a.url || ''), a.w || 320, a.h || 240); } catch (e) { img = null; }
    if (!img) return { ok: false, error: '图片 API 不可用' };
    rt_try(img, 'SetPosition', a.x || 60, a.y || 120);
    if (rt_try(s, 'AddObject', img)) return { ok: true };
    return { ok: false, error: 'AddObject 不可用' };
  }
  function t_add_table(a) {
    var pr = Api.GetPresentation(), s = pr.GetSlideByIndex(rt_slideIdx(a, pr));
    if (!s) return { ok: false, error: '页不存在' };
    var rows = a.rows;
    if (!rows || !rows.length) return { ok: false, error: 'rows 必填（二维数组，首行为表头）' };
    var t = null;
    try { t = Api.CreateTable(rows[0].length, rows.length); } catch (e) { t = null; }
    if (!t) return { ok: false, error: '表格 API 不可用' };
    for (var ri = 0; ri < rows.length; ri++) {
      for (var ci = 0; ci < rows[ri].length; ci++) {
        rt_try0(function () { t.GetRow(ri).GetCell(ci).GetContent().GetElement(0).AddText(String(rows[ri][ci])); });
      }
    }
    if (rt_try(s, 'AddObject', t)) return { ok: true };
    return { ok: false, error: 'AddObject 不可用' };
  }
  function t_delete_slide(a) {
    var pr = Api.GetPresentation(), i = rt_slideIdx(a, pr);
    if (rt_try(pr, 'DeleteSlide', i)) return { ok: true };
    return { ok: false, error: '不可用' };
  }
  function t_move_slide(a) {
    var pr = Api.GetPresentation();
    var from = rt_pos(a.from, 1) - 1, to = rt_pos(a.to, 1) - 1;
    if (rt_try(pr, 'MoveSlide', from, to)) return { ok: true };
    return { ok: false, error: '不可用' };
  }
  function t_set_slide_title(a) {
    var pr = Api.GetPresentation(), s = pr.GetSlideByIndex(rt_slideIdx(a, pr));
    if (!s) return { ok: false, error: '页不存在' };
    var shapes = [];
    rt_try0(function () { shapes = s.GetAllShapes(); });
    for (var i = 0; i < shapes.length; i++) {
      var txt = ''; rt_try0(function () { txt = shapes[i].GetText(); });
      if (txt !== '' || i === 0) { if (rt_try(shapes[i], 'SetText', String(a.title || ''))) return { ok: true }; }
    }
    return { ok: false, error: '未找到可设置文本的形状' };
  }
  function t_slide_notes(a) {
    var pr = Api.GetPresentation(), s = pr.GetSlideByIndex(rt_slideIdx(a, pr));
    if (!s) return { ok: false, error: '页不存在' };
    if (a.notes !== undefined) { if (rt_try(s, 'SetNotes', String(a.notes))) return { ok: true }; return { ok: false, error: '备注 API 不可用' }; }
    var notes = '';
    rt_try0(function () { notes = s.GetNotes(); });
    return { ok: true, notes: rt_clip(notes, 1000) };
  }
  function t_set_slide_background(a) {
    var pr = Api.GetPresentation(), s = pr.GetSlideByIndex(rt_slideIdx(a, pr));
    if (!s) return { ok: false, error: '页不存在' };
    if (rt_try(s, 'SetBackground', a.color || 'F5F5F5')) return { ok: true };
    return { ok: false, error: '不可用' };
  }
  function t_set_slide_layout(a) {
    var pr = Api.GetPresentation(), s = pr.GetSlideByIndex(rt_slideIdx(a, pr));
    if (!s) return { ok: false, error: '页不存在' };
    if (rt_try(s, 'SetLayout', a.layout || 'title')) return { ok: true };
    return { ok: false, error: '不可用' };
  }

  /* ---- 工具注册表与分发（沙箱内） ---- */
  function rt_tools() {
    return {
      // 通用
      get_doc_info: { kinds: ['word', 'cell', 'slide'], fn: t_get_doc_info },
      // word
      read_document: { kinds: ['word'], fn: t_read_document },
      search_text: { kinds: ['word'], fn: t_search_text },
      get_selection: { kinds: ['word'], fn: t_get_selection },
      insert_content: { kinds: ['word'], fn: t_insert_content },
      replace_text: { kinds: ['word'], fn: t_replace_text },
      replace_all: { kinds: ['word'], fn: t_replace_all },
      replace_element: { kinds: ['word'], fn: t_replace_element },
      delete_element: { kinds: ['word'], fn: t_delete_element },
      format_element: { kinds: ['word'], fn: t_format_element },
      set_paragraph_style: { kinds: ['word'], fn: t_set_paragraph_style },
      insert_table: { kinds: ['word'], fn: t_insert_table },
      read_table: { kinds: ['word'], fn: t_read_table },
      table_op: { kinds: ['word'], fn: t_table_op },
      insert_page_break: { kinds: ['word'], fn: t_insert_page_break },
      add_comment: { kinds: ['word'], fn: t_add_comment },
      insert_image: { kinds: ['word'], fn: t_insert_image },
      insert_page_number: { kinds: ['word'], fn: t_insert_page_number },
      get_page_count: { kinds: ['word'], fn: t_get_page_count },
      set_page_orientation: { kinds: ['word'], fn: t_set_page_orientation },
      insert_horizontal_rule: { kinds: ['word'], fn: t_insert_horizontal_rule },
      insert_hyperlink: { kinds: ['word'], fn: t_insert_hyperlink },
      set_page_margins: { kinds: ['word'], fn: t_set_page_margins },
      insert_toc: { kinds: ['word'], fn: t_insert_toc },
      set_header_footer: { kinds: ['word'], fn: t_set_header_footer },
      insert_code_block: { kinds: ['word'], fn: t_insert_code_block },
      // cell
      read_range: { kinds: ['cell'], fn: t_read_range },
      write_cells: { kinds: ['cell'], fn: t_write_cells },
      write_table: { kinds: ['cell'], fn: t_write_table },
      format_range: { kinds: ['cell'], fn: t_format_range },
      row_op: { kinds: ['cell'], fn: t_row_op },
      col_op: { kinds: ['cell'], fn: t_col_op },
      set_col_width: { kinds: ['cell'], fn: t_set_col_width },
      clear_range: { kinds: ['cell'], fn: t_clear_range },
      merge_cells: { kinds: ['cell'], fn: t_merge_cells },
      freeze_panes: { kinds: ['cell'], fn: t_freeze_panes },
      sheet_op: { kinds: ['cell'], fn: t_sheet_op },
      autofit: { kinds: ['cell'], fn: t_autofit },
      sort_range: { kinds: ['cell'], fn: t_sort_range },
      apply_filter: { kinds: ['cell'], fn: t_apply_filter },
      insert_formula: { kinds: ['cell'], fn: t_insert_formula },
      insert_chart: { kinds: ['cell'], fn: t_insert_chart },
      set_conditional_format: { kinds: ['cell'], fn: t_set_conditional_format },
      find_text: { kinds: ['cell'], fn: t_find_text },
      // slide
      read_slide: { kinds: ['slide'], fn: t_read_slide },
      add_slide: { kinds: ['slide'], fn: t_add_slide },
      add_text_box: { kinds: ['slide'], fn: t_add_text_box },
      set_text: { kinds: ['slide'], fn: t_set_text },
      add_shape: { kinds: ['slide'], fn: t_add_shape },
      slide_insert_image: { kinds: ['slide'], fn: t_slide_insert_image },
      add_table: { kinds: ['slide'], fn: t_add_table },
      delete_slide: { kinds: ['slide'], fn: t_delete_slide },
      move_slide: { kinds: ['slide'], fn: t_move_slide },
      set_slide_title: { kinds: ['slide'], fn: t_set_slide_title },
      slide_notes: { kinds: ['slide'], fn: t_slide_notes },
      set_slide_background: { kinds: ['slide'], fn: t_set_slide_background },
      set_slide_layout: { kinds: ['slide'], fn: t_set_slide_layout }
    };
  }
  function rt_dispatch(d) {
    var k = rt_kind();
    var table = rt_tools();
    var t = table[d.tool];
    if (!t) return { ok: false, error: '未知工具 ' + d.tool };
    if (t.kinds && t.kinds.indexOf(k) < 0) return { ok: false, error: '工具 ' + d.tool + ' 在 ' + k + ' 编辑器不可用' };
    try { return t.fn(d.args || {}); }
    catch (e) { return { ok: false, error: String(e && e.message || e) }; }
  }

  /* 注入沙箱的运行时源码（真实函数 toString，node --check 可校验本文件）。 */
  var RUNTIME_SRC = [
    rt_try, rt_try0, rt_try0r, rt_clip, rt_kind, rt_hx, rt_colName, rt_a1, rt_pos, rt_ix0,
    rt_runs, rt_codePara, rt_para, rt_mkTable, rt_fillTable, rt_fillCell, rt_isTable, rt_pText, rt_scan, rt_mdE, rt_insertAt,
    t_get_doc_info, t_read_document, t_search_text, t_get_selection, t_insert_content,
    rt_paraHits, rt_swapAt, t_replace_text, t_replace_all, t_replace_element, t_delete_element,
    t_format_element, t_set_paragraph_style, t_insert_table, t_read_table, t_table_op,
    t_insert_page_break, t_add_comment, t_insert_image, t_insert_page_number, t_get_page_count,
    t_set_page_orientation, t_insert_horizontal_rule, t_insert_hyperlink, t_set_page_margins,
    t_insert_toc, t_set_header_footer, t_insert_code_block,
    t_x_info, t_read_range, t_write_cells, t_write_table, t_format_range,
    t_row_op, t_col_op, t_set_col_width, t_clear_range, t_merge_cells, t_freeze_panes, t_sheet_op,
    t_autofit, t_sort_range, t_apply_filter, t_insert_formula, t_insert_chart, t_set_conditional_format, t_find_text,
    rt_slideIdx, t_read_slide, t_add_slide, t_add_text_box, t_set_text, t_add_shape,
    t_slide_insert_image, t_add_table, t_delete_slide, t_move_slide, t_set_slide_title,
    t_slide_notes, t_set_slide_background, t_set_slide_layout,
    rt_tools, rt_dispatch
  ].map(function (f) { return f.toString(); }).join('\n');

  function runEditorTool(tool, args) {
    return new Promise(function (resolve) {
      if (!window.Asc || !window.Asc.plugin) { resolve({ ok: false, error: '环境未就绪' }); return; }
      try {
        var src = 'var data = ' + JSON.stringify({ tool: tool, args: args || {} }) + ';\n' + RUNTIME_SRC + '\nreturn rt_dispatch(data);';
        var fn = new Function('return (function () {\n' + src + '\n})')();
        var done = false;
        var to = setTimeout(function () { if (!done) { done = true; resolve({ ok: false, error: '超时' }); } }, 20000);
        window.Asc.plugin.callCommand(fn, false, false, function (r) {
          if (done) return; done = true; clearTimeout(to);
          resolve(r || { ok: false, error: '无返回' });
        });
      } catch (e) { resolve({ ok: false, error: String(e) }); }
    });
  }
  function httpTool(tool, args) {
    var name = String((args || {}).name || '').trim();
    if (tool === 'list_files') {
      var url = name ? '/api/v1/search?q=' + encodeURIComponent(name) + '&limit=20' : '/api/v1/files?limit=20';
      return fetch(url, { credentials: 'same-origin', headers: { Authorization: 'Bearer ' + token } })
        .then(function (r) { return r.json(); })
        .then(function (d) {
          var list = d.results || d.files || (Array.isArray(d) ? d : []) || [];
          return { ok: true, files: list.filter(function (x) { return x.type !== 'folder'; }).slice(0, 20).map(function (x) { return x.name; }) };
        }).catch(function (e) { return { ok: false, error: String(e) }; });
    }
    if (tool === 'read_file') {
      if (!name) return Promise.resolve({ ok: false, error: 'name 必填' });
      return fetch('/api/v1/search?q=' + encodeURIComponent(name) + '&limit=5', { credentials: 'same-origin', headers: { Authorization: 'Bearer ' + token } })
        .then(function (r) { return r.json(); })
        .then(function (d) {
          var list = d.results || d.files || [];
          var hit = list.filter(function (x) { return x.type !== 'folder' && x.name === name; })[0] || list.filter(function (x) { return x.type !== 'folder'; })[0];
          if (!hit) return { ok: false, error: '未找到：' + name };
          return fetch('/api/v1/files/' + hit.id + '/download', { credentials: 'same-origin', headers: { Authorization: 'Bearer ' + token } })
            .then(function (r) { return r.text(); })
            .then(function (t) { return { ok: true, name: hit.name, text: t.slice(0, 15000) }; });
        }).catch(function (e) { return { ok: false, error: String(e) }; });
    }
    return Promise.resolve(null);
  }
  function runTool(tool, args) { return httpTool(tool, args).then(function (r) { return r !== null ? r : runEditorTool(tool, args); }); }
  function probeEditor() { return runEditorTool('get_doc_info', {}).then(function (r) { docInfo = r; return r; }); }

  /* ---- 工具清单（system 提示词用；与 rt_tools 注册表一一对应） ---- */
  var TOOL_DOCS = {
    word: [
      'get_doc_info {} → 结构总览（元素数 + 前 80 元素概览 + 选区）。',
      'read_document {from?,to?,maxChars?} → 读段落文本（返回 n=1 基元素序号，可直接用于其它工具的 index）。',
      'search_text {query} → 定位段落（返回 n 序号）。',
      'get_selection {} → 读当前选中文本。',
      'insert_content {md,at?,index?} → 插入 Markdown（at=end 文末默认 / cursor 光标 / after 指定序号后）。',
      'replace_text {find,md,scope?} → 段落替换（find 为段落内原文片段；scope=first|all）。',
      'replace_all {find,replace} → 全文段落替换。',
      'replace_element {index,md} → 整元素替换。',
      'delete_element {index} → 删除元素。',
      'format_element {index,bold?,italic?,underline?,fontSize?,fontColor?,highlight?} → 段内文本格式（色值 #RRGGBB；highlight 如 yellow）。',
      'set_paragraph_style {index,style?,align?} → 样式（heading_1|heading_2|heading_3|title|quote|normal；align=left|center|right|both）。',
      'insert_table {rows,at?,header?} → 插入表格（rows 二维数组首行为表头，header=true 自动加粗+底纹+居中）。',
      'read_table {index} → 读表格内容（二维数组）。',
      'table_op {index,op,r?,c?,text?,values?} → add_row（values 可带整行值）/ set_cell（r,c 1 起）。',
      'insert_page_break {} → 分页符。',
      'add_comment {find,text} → 批注。',
      'insert_image {url,width?,height?} → 插入图片（url 须编辑器可访问）。',
      'insert_code_block {code} → 等宽字体代码块。',
      'insert_page_number {} → 页脚页码。',
      'get_page_count {} → 元素总数。',
      'set_page_orientation {orientation} → portrait|landscape。',
      'insert_horizontal_rule {} → 分隔线。',
      'insert_hyperlink {text,url} → 链接样式文本。',
      'set_page_margins {top,right,bottom,left} → 页边距（pt）。',
      'insert_toc {} → 目录大纲（按 Markdown 标题段落）。',
      'set_header_footer {position:"header"|"footer",text} → 页眉/页脚。'
    ].join('\n'),
    cell: [
      'get_doc_info {} → 已用区域（rows/cols 为 1 基计数）与工作表名。',
      'read_range {r1,c1,r2?,c2?} → 读区域值（1 基：r1=1,c1=1 即 A1；返回 address 如 "A1:D9" 供校验）。',
      'write_table {rows,startRow?,startCol?,header?,autofit?,freeze?} → ★写表格首选：rows 二维数组（含表头行）整表写入；header=true（默认）表头自动加粗+浅蓝底纹+居中，autofit 默认自适应列宽，freeze=true 冻结表头行。',
      'write_cells {cells:[[r,c,v],…]} → 逐格写入（1 基 [1,1]=A1；值以 = 开头即公式；返回实际写入 range 地址）。',
      'format_range {r1,c1,r2?,c2?,bold?,italic?,fontSize?,fillColor?,fontColor?,numberFormat?,hAlign?,vAlign?,wrap?} → 区域样式（色值 #RRGGBB；numberFormat 如 0.00% / #,##0 / yyyy-mm-dd；hAlign=left|center|right）。',
      'insert_formula {r,c,formula} → 写公式（= 可省略，自动补）。',
      'row_op {op:"insert"|"delete",at,count?} → 行操作（at 为 1 基行号）。',
      'col_op {op:"insert"|"delete",at,count?} → 列操作（at 为 1 基列号）。',
      'set_col_width {col,width} → 列宽（col 1 基；width 数字或 "auto"）。',
      'clear_range {r1,c1,r2?,c2?} → 清空区域。',
      'merge_cells {r1,c1,r2,c2,merge?} → 合并（merge=false 取消）。',
      'freeze_panes {r,c} → 冻结至该单元格（其上各行+其左各列冻结；冻结表头行用 {r:2,c:1}）。',
      'sheet_op {op:"list"|"add"|"rename"|"activate",name?,index?} → 工作表操作。',
      'autofit {from?,to?} → 列宽自适应（默认前 20 列）。',
      'sort_range {r1,c1,r2,c2,by?,order?} → 排序（by 为区域内 1 基列号；order=desc）。',
      'apply_filter {r1,c1,r2,c2} → 开启筛选。',
      'insert_chart {type,r1,c1,r2,c2,atRow?,atCol?} → 图表（bar|line|pie；数据区域；锚点默认数据下方）。',
      'set_conditional_format {r1,c1,r2,c2,rule?,operator?,value?} → 条件格式。',
      'find_text {query} → 全表搜值（返回 cell 地址与内容）。'
    ].join('\n'),
    slide: [
      'get_doc_info {} → 页数。',
      'read_slide {slide} → 读页内容（texts 文本列表 + 形状数 + 备注）。',
      'add_slide {title,bullets?} → 文末加页（标题+要点一次成型）。',
      'add_text_box {slide,text,x?,y?,w?,h?,fontSize?} → 文本框（pt 坐标，页约 960×540，标题区 y<100）。',
      'set_text {slide,shape,text} → 改形状文本（shape 为页内 1 基形状序号）。',
      'add_shape {slide,type?,x?,y?,w?,h?,fill?,text?} → 形状（rect|ellipse|triangle…）。',
      'slide_insert_image {slide,url,x?,y?,w?,h?} → 图片。',
      'add_table {slide,rows} → 表格（rows 二维数组首行表头）。',
      'delete_slide {slide} / move_slide {from,to} → 删页/移页（页码 1 起）。',
      'set_slide_title {slide,title} → 改标题。',
      'slide_notes {slide,notes?} → 读/写备注。',
      'set_slide_background {slide,color} / set_slide_layout {slide,layout} → 背景/布局。'
    ].join('\n'),
    pdf: '（当前为 PDF：OnlyOffice 无 PDF 文档 API，无法直接编辑——请在 FINAL 中说明并建议转 Word 编辑或到查看页用 AI 摘要。）'
  };
  /** 分编辑器类型的使用规范（system 提示词正文）。 */
  var KIND_GUIDES = {
    cell: [
      'Excel 规范：',
      '- 坐标一律 1 基数字 [行,列]（[1,1]=A1，A 列=1）——与 Excel A1 语义一致，绝不做 +1 偏移。',
      '- 写整表/批量数据首选 write_table（rows 含表头行的二维数组，自动表头样式），不要为整表逐格 write_cells。',
      '- 公式以 = 开头、英文函数名（=SUM(B2:B10)、=(B5-B6)/B5）；引用的行列号与写入位置按同一张表推算。',
      '- 数字/百分比/日期列用 format_range 的 numberFormat（0.00% / #,##0 / yyyy-mm-dd）。',
      '- 写完表格后用 read_range 校验（对照返回的 address 与预期一致再继续）。'
    ].join('\n'),
    word: [
      'Word 规范：',
      '- 元素序号全 1 基：read_document/search_text 返回的 n 直接用于 format_element/delete_element/replace_element 的 index。',
      '- insert_content 的 md 支持 Markdown：#/##/### 标题、- 与 1. 列表、**粗体**、`行内代码`、|表格|、```代码块```。',
      '- 表格用 insert_table {rows,header:true}；读取既有表格用 read_table；改单元格 table_op {index,op:"set_cell"}。'
    ].join('\n'),
    slide: [
      'PPT 规范：页码 1 起；一页内容用一次 add_slide {title,bullets} 成型；补充文字用 add_text_box（pt 坐标，页面约 960×540）。'
    ].join('\n'),
    pdf: ''
  };
  function buildSystem() {
    var k = editorType || 'pdf';
    var L = [
      '你是 DocFlow 内置文档编辑 Agent，运行在 OnlyOffice 编辑器侧栏中。',
      '当前编辑器类型：' + k + '。你操作的是当前打开的编辑器文档——不是平台文件系统。',
      '重要：写入内容必须用编辑器工具（insert_content / write_table / add_slide 等）直接修改当前文档，不要用 list_files/read_file 生成新文件。',
      '',
      '坐标与序号约定（硬性，全编辑器统一 1 基）：excel 单元格 [行,列]（[1,1]=A1，A 列=1）；word 元素序号从 1 起；slide 页码从 1 起。'
    ];
    var guide = KIND_GUIDES[k];
    if (guide) L.push('', guide);
    L.push('', '工具：', TOOL_DOCS[k] || TOOL_DOCS.pdf,
      '跨工具：list_files {name?} / read_file {name} → 平台文件检索/读取（作上下文参考，写入仍用编辑器工具）。',
      '',
      '工作方式：先读（get_doc_info / read_*）了解现状再改；一次恰好一行 TOOL_CALL 调一个工具，根据结果决定下一步；失败时读错误信息调整参数或换工具；关键写入后用读工具校验；完成后以 FINAL 开头总结（改了什么、在哪个位置）。',
      '输出格式（硬性）：每轮恰好一行 TOOL_CALL {"tool":…,"args":{…}}，或以 FINAL 开头的最终答复。');
    if (docInfo && docInfo.ok) L.push('', '【初始文档结构】' + JSON.stringify(docInfo).slice(0, 2000));
    return L.join('\n');
  }

  function extractCallJson(t) {
    var k = 0, found = [];
    while (found.length < 8) {
      var at = t.indexOf('TOOL_CALL', k); if (at < 0) break;
      var b = t.indexOf('{', at); if (b < 0) break;
      var d = 0, inS = false, esc = false, end = -1;
      for (var i = b; i < t.length; i++) {
        var ch = t.charAt(i);
        if (esc) { esc = false; continue; }
        if (ch === '\\') { if (inS) esc = true; continue; }
        if (ch === '"') { inS = !inS; continue; }
        if (inS) continue;
        if (ch === '{') d++; else if (ch === '}') { d--; if (d === 0) { end = i; break; } }
      }
      if (end < 0) break;
      found.push(t.slice(b, end + 1)); k = end + 1;
    }
    for (var j = found.length - 1; j >= 0; j--) {
      try { var c = JSON.parse(found[j]);
        var tn = c.tool || c.name;
        var ta = c.args || c.arguments || {};
        if (tn && typeof tn === 'string') return { type: 'tool', call: { tool: tn, args: ta } };
      } catch (e) {}
    }
    return null;
  }
  function parseReply(t) {
    t = t.trim();
    if (t.indexOf('FINAL') === 0) return { type: 'final', text: t.slice(5).trim() || t };
    var c = extractCallJson(t);
    return c || { type: 'final', text: t };
  }

  var MAX_ROUNDS = 50;
  function setBusyUI(b) { busy = b; sendBtn.style.display = b ? 'none' : 'inline-flex'; stopBtn.style.display = b ? 'inline-flex' : 'none'; }

  function chatOnce(messages, ev) {
    var body = { messages: messages, stream: true };
    body.use_files = false; // 关键：关闭后端 df_* 平台文件工具注入（后端默认 true），插件自身经 TOOL_CALL 协议调度编辑器工具
    if (webOn) body.web_search = true;
    if (thinkOn) body.think = true;
    var m = currentModel();
    if (m && m.providerId) body.model = { providerId: m.providerId, modelId: m.modelId };
    return fetch('/api/v1/ai/chat', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + token }, body: JSON.stringify(body), signal: controller.signal })
      .then(function (res) {
        if (!res.ok) { if (res.status === 401) throw { status: 401 }; throw new Error('HTTP ' + res.status); }
        var rd = res.body.getReader(), dec = new TextDecoder(), buf = '';
        function pump() {
          return rd.read().then(function (r) {
            if (r.done) return;
            buf += dec.decode(r.value, { stream: true });
            var idx;
            while ((idx = buf.indexOf('\n\n')) >= 0) {
              var blk = buf.slice(0, idx); buf = buf.slice(idx + 2);
              var n = '', d = '';
              blk.split('\n').forEach(function (ln) { if (ln.indexOf('event: ') === 0) n = ln.slice(7).trim(); else if (ln.indexOf('data: ') === 0) d = ln.slice(6); });
              if (!n) continue;
              var p = {}; try { p = JSON.parse(d); } catch (e) { continue; }
              if (n === 'delta' && p.text) ev.onDelta(p.text);
              else if (n === 'thinking' && p.text) ev.onThinking(p.text);
              else if (n === 'error') throw new Error(p.error || 'AI 请求失败');
              else if (n === 'done') { ev.onDone(); return; }
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

  function send() {
    var text = q.value.trim();
    if (!text || busy) return;
    q.value = '';
    setBusyUI(true);
    userBubble(text);
    thinkEl = null;
    controller = new AbortController();
    editorType = readEditorKind() || 'word';

    var convo = [{ role: 'system', content: buildSystem() }, { role: 'user', content: text }];
    var thinkStart = 0, rounds = 0;
    function finishOk(s) {
      settleThink(thinkStart ? Date.now() - thinkStart : 0);
      if (s) el('div', 'm a', mdToHtml(s));
      history.push({ role: 'user', content: text }, { role: 'assistant', content: ('完成：' + s).slice(0, 2000) });
      if (history.length > 12) history = history.slice(-12);
      probeEditor().then(function () {});
      setBusyUI(false); controller = null;
      setHint('已完成（DS 自动保存）');
    }
    function finishErr(m) { settleThink(0); el('div', 'm a err', esc(m)); setBusyUI(false); controller = null; }

    if (!editMode) {
      var acc = '', bub = null;
      chat(convo, {
        onDelta: function (d) { acc += d; if (!bub) bub = el('div', 'm a'); bub.innerHTML = mdToHtml(acc) + '<span class="caret"></span>'; log.scrollTop = log.scrollHeight; },
        onThinking: function (t) { if (!thinkStart) thinkStart = Date.now(); appendThink(t); },
        onDone: function () { settleThink(thinkStart ? Date.now() - thinkStart : 0); if (bub) bub.innerHTML = mdToHtml(acc); }
      }).then(function () { lastReply = acc; setBusyUI(false); controller = null; })
        .catch(function (e) { finishErr(String(e && e.message || e)); });
      return;
    }
    (function loop() {
      if (rounds >= MAX_ROUNDS) { finishErr('轮次上限'); return; }
      rounds++;
      var acc2 = '', raw = null;
      chat(convo, {
        onDelta: function (d) { acc2 += d; if (!raw) raw = el('div', 'raw'); raw.textContent = acc2.slice(-300) + ' ▌'; log.scrollTop = log.scrollHeight; },
        onThinking: function (t) { if (!thinkStart) thinkStart = Date.now(); appendThink(t); },
        onDone: function () {}
      }).then(function () {
        if (raw) raw.remove();
        var parsed = parseReply(acc2);
        if (parsed.type === 'final') { finishOk(parsed.text); return; }
        var card = toolCard(parsed.call.tool, parsed.call.args);
        setHint('执行（' + rounds + '）：' + parsed.call.tool);
        return runTool(parsed.call.tool, parsed.call.args).then(function (r) {
          toolDone(card, !!(r && r.ok), r);
          convo.push({ role: 'assistant', content: 'TOOL_CALL ' + JSON.stringify(parsed.call) });
          convo.push({ role: 'user', content: 'TOOL_RESULT ' + JSON.stringify(r).slice(0, 3000) });
          loop();
        });
      }).catch(function (e) {
        if (raw) raw.remove();
        finishErr(e && e.name === 'AbortError' ? '已停止' : String(e && e.message || e));
      });
    })();
  }

  sendBtn.onclick = send;
  stopBtn.onclick = function () { if (controller) controller.abort(); };
  q.addEventListener('keydown', function (e) { if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); send(); } });
  webBtn.onclick = function () { webOn = !webOn; webBtn.classList.toggle('on', webOn); };
  thinkBtn.onclick = function () { thinkOn = !thinkOn; thinkBtn.classList.toggle('on', thinkOn); };
  modeSeg.addEventListener('click', function (e) {
    var b = e.target.closest('button[data-v]'); if (!b) return;
    editMode = b.dataset.v === 'edit';
    var bs = modeSeg.querySelectorAll('button');
    for (var i = 0; i < bs.length; i++) bs[i].classList.toggle('on', bs[i] === b);
    manualBar.classList.toggle('hidden', editMode);
    q.placeholder = editMode ? '描述要做的修改…' : '仅对话模式…';
  });
  document.getElementById('clear').onclick = function () { history = []; lastReply = ''; log.innerHTML = ''; setHint('已清空'); };
  document.getElementById('insert').onclick = function () { if (lastReply && window.Asc && window.Asc.plugin) window.Asc.plugin.executeMethod('PasteHtml', [mdToHtml(lastReply)]); };
  document.getElementById('replace').onclick = function () { if (lastReply && window.Asc && window.Asc.plugin) window.Asc.plugin.executeMethod('PasteHtml', [mdToHtml(lastReply)]); };

  var reg = false;
  function register() {
    if (reg || !window.Asc || !window.Asc.plugin) return;
    reg = true;
    window.Asc.plugin.init = function () {
      editorType = readEditorKind();
      setHint((editorType ? '就绪 · ' + editorType : '就绪') + ' · v7');
      void refresh().then(function () { return loadModels().then(renderModels); }).catch(function () { setHint('初始化失败'); });
    };
    window.Asc.plugin.onMethodReturn = function () {};
    window.Asc.plugin.button = function () {};
  }
  if (window.Asc && window.Asc.plugin) { register(); }
  else {
    var n = 0, p = setInterval(function () {
      n++;
      if (window.Asc && window.Asc.plugin) { clearInterval(p); register(); if (window.Asc.plugin.init) window.Asc.plugin.init(); return; }
      if (n >= 20) { clearInterval(p); setHint('调试模式 · v7'); }
    }, 500);
    void refresh().then(function () { return loadModels().then(renderModels); }).catch(function () {});
  }
})();
