/* DocFlow AI OnlyOffice 插件 v6.0 — localStorage 类型判定 + 时间序消息 */
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
  var NL = String.fromCharCode(10);
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
  function inlineMd(s) { return esc(s).replace(/`([^`]+)`/g, '<code>$1</code>').replace(/\*\*([^*]+)\*\*/g, '<b>$1</b>'); }
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

  var RUNTIME = [
    'function clip(s,n){s=String(s);return s.length>n?s.slice(0,n)+"…":s}',
    'function ek(){if(typeof Api.GetActiveSheet==="function")return "cell";if(typeof Api.GetPresentation==="function")return "slide";if(typeof Api.GetDocument==="function")return "word";return "pdf"}',
    'function runs(p,t){var s=String(t);while(true){var a=s.indexOf("**"),b=s.indexOf("**",a+2),c=s.indexOf("`"),d=s.indexOf("`",c+1),pk=null;if(a>=0&&b>a)pk=["b",a,b+2,s.slice(a+2,b)];if(c>=0&&d>c&&(pk===null||c<pk[1]))pk=["m",c,d+1,s.slice(c+1,d)];if(!pk)break;if(pk[1]>0)p.AddText(s.slice(0,pk[1]));var r=p.AddText(pk[3]);if(pk[0]==="b")r.SetBold(true);else r.SetFontName("Courier New");s=s.slice(pk[2])}if(s)p.AddText(s)}',
    'function para(ln){var p=Api.CreateParagraph();var t=ln,h=0;while(t.charAt(0)==="#"){h++;t=t.slice(1)}if(h>0&&t.charAt(0)===" "){var r=p.AddText(t);r.SetBold(true);r.SetFontSize(h===1?18:h===2?15:13.5);return p}var tr=ln.replace(/^\\s+/,"");if(tr.charAt(0)==="-"||tr.charAt(0)==="*"){p.AddText("\\u2022 ");runs(p,tr.replace(/^[-*]\\s+/,""));return p}runs(p,ln);return p}',
    'function mkT(rs){var t=Api.CreateTable(rs[0].length,rs.length);fillT(t,rs);return t}',
    'function fillT(t,rs){for(var ri=0;ri<rs.length;ri++)for(var ci=0;ci<rs[ri].length&&ci<rs[0].length;ci++){try{fillC(t.GetRow(ri).GetCell(ci),String(rs[ri][ci]),ri===0)}catch(e){}}}',
    'function fillC(c,tx,b){var cc=c.GetContent(),p=null;if(cc.GetElementsCount()>0){var e0=cc.GetElement(0);if(e0&&typeof e0.AddText==="function")p=e0}if(!p){p=Api.CreateParagraph();cc.Push(p)}var r=p.AddText(tx);if(b&&r.SetBold)r.SetBold(true)}',
    'function isT(e){return e&&typeof e.GetRow==="function"}',
    'function pT(e){try{return e.GetText()}catch(x){return""}}',
    'function scan(q){var d=Api.GetDocument(),n=d.GetElementsCount(),h=[];for(var i=0;i<n;i++){var e=d.GetElement(i),k=isT(e)?"table":"para",t=k==="table"?"[表格]":pT(e);if(!q||t.indexOf(q)>=0)h.push({i:i,kind:k,text:clip(t,400)})}return{count:n,hits:h}}',
    'function mdE(md){var out=[],ls=String(md).split(NL),ic=false,bf=[],tb=[];function ft(){if(!tb.length)return;var rs=[];for(var k=0;k<tb.length;k++){var t=tb[k].trim();if(/^\\|[\\s:|-]+\\|?$/.test(t))continue;rs.push(t.replace(/^\\|/,"").replace(/\\|$/,"").split("|"))}if(rs.length)out.push(mkT(rs));tb=[]}for(var i=0;i<ls.length;i++){var ln=ls[i];if(ln.indexOf("```")===0){if(ic){out.push(para(bf.join(NL)));bf=[];ic=false}else{ft();ic=true}continue}if(ic){bf.push(ln);continue}if(ln.trim()===""){ft();continue}if(ln.charAt(0)==="|"){tb.push(ln);continue}ft();out.push(para(ln))}ft();if(ic&&bf.length)out.push(para(bf.join(NL)));return out}',
    'function wIns(a){var d=Api.GetDocument(),es=mdE(a.md||"");if(!es.length)return{ok:false,error:"空内容"};if((a.at||"end")==="cursor"){d.InsertContent(es);return{ok:true}}if(a.at==="after"&&typeof a.index==="number"){try{for(var k=es.length-1;k>=0;k--)d.AddElement(a.index+1,es[k]);return{ok:true}}catch(e){for(var j=0;j<es.length;j++)d.Push(es[j]);return{ok:true,note:"追加到文末"}}}for(var i=0;i<es.length;i++)d.Push(es[i]);return{ok:true}}',
    'function wRep(a){var d=Api.GetDocument(),sc=scan(a.find),ix=[];for(var h=0;h<sc.hits.length;h++)if(sc.hits[h].kind==="para")ix.push(sc.hits[h].i);if(!ix.length)return{ok:false,error:"未找到"};if((a.scope||"first")==="first")ix=[ix[0]];var es=mdE(a.md||""),dn=0;for(var t=ix.length-1;t>=0;t--){var i=ix[t];try{d.RemoveElement(i);try{for(var k=es.length-1;k>=0;k--)d.AddElement(i,es[k])}catch(e2){for(var j=0;j<es.length;j++)d.Push(es[j])}dn++}catch(x){}}return dn?{ok:true,replaced:dn}:{ok:false,error:"替换失败"}}',
    'function hx(s){s=String(s).replace("#","");return[parseInt(s.slice(0,2),16)||0,parseInt(s.slice(2,4),16)||0,parseInt(s.slice(4,6),16)||0]}',
    'function wFmt(a){var d=Api.GetDocument(),p=d.GetElement(a.index);if(!p)return{ok:false,error:"非段落"};var n=0;function F(r){try{if(a.bold!==undefined&&r.SetBold)r.SetBold(a.bold);if(a.italic===true&&r.SetItalic)r.SetItalic(true);if(a.underline===true&&r.SetUnderline)r.SetUnderline(true);if(a.fontSize&&r.SetFontSize)r.SetFontSize(a.fontSize);if(a.color&&r.SetColor){var c=hx(a.color);r.SetColor(c[0],c[1],c[2],false)}n++}catch(x){}}F(p);try{if(p.GetElementsCount)for(var i=0;i<p.GetElementsCount();i++){F(p.GetElement(i))}}catch(x){}return n?{ok:true,applied:n}:{ok:false,error:"无格式化目标"}}',
    'function wSty(a){var d=Api.GetDocument(),p=d.GetElement(a.index);if(!p||typeof p.AddText!=="function")return{ok:false,error:"非段落"};if(a.style&&a.style!=="normal"){var n=a.style==="heading_1"?"Heading 1":a.style==="heading_2"?"Heading 2":a.style==="heading_3"?"Heading 3":a.style;try{p.SetStyle(Api.GetStyle(n))}catch(e){return{ok:false,error:"样式不可用"}}}if(a.align){try{p.SetJc(a.align)}catch(x){}}return{ok:true}}',
    'function wPB(){var d=Api.GetDocument(),p=Api.CreateParagraph();try{p.AddPageBreak();d.Push(p);return{ok:true}}catch(e){return{ok:false,error:"不可用"}}}',
    'function wCm(a){try{var d=Api.GetDocument(),rs=d.Search(a.find);if(!rs||!rs.length)return{ok:false,error:"未找到"};rs[0].AddComment(a.text||"");return{ok:true}}catch(e){return{ok:false,error:String(e)}}}',
    'function wTO(a){var d=Api.GetDocument(),t=d.GetElement(a.index);if(!isT(t))return{ok:false,error:"非表格"};if(a.op==="add_row"){try{t.AddRow(t.GetRow(t.GetRowsCount()-1));return{ok:true}}catch(e){return{ok:false,error:"不可用"}}}if(a.op==="set_cell"){try{fillC(t.GetRow(a.r).GetCell(a.c),String(a.text),false);return{ok:true}}catch(x){return{ok:false,error:"不可达"}}}return{ok:false,error:"未知"}}',
    'function cN(n){var s="";n=n+1;while(n>0){var m=(n-1)%26;s=String.fromCharCode(65+m)+s;n=Math.floor((n-1)/26)}return s}',
    'function xI(){var sh=Api.GetActiveSheet(),v=[];try{v=sh.GetRange("A1:T200").GetValues()||[]}catch(e){v=[]}var mr=-1,mc=-1;for(var i=0;i<v.length;i++){var r=v[i];for(var j=0;j<r.length;j++){if(r[j]!==""&&r[j]!==null&&r[j]!==undefined){if(i>mr)mr=i;if(j>mc)mc=j}}}return{ok:true,kind:"cell",rows:mr+1,cols:mc+1}}',
    'function xR(sh,r1,c1,r2,c2){return sh.GetRange(cN(c1)+(r1+1)+":"+cN(c2)+(r2+1))}',
    'function xRd(a){var sh=Api.GetActiveSheet();var r1=a.r1||0,c1=a.c1||0,r2=a.r2!==undefined?a.r2:r1+19,c2=a.c2!==undefined?a.c2:c1+9;var v=xR(sh,r1,c1,r2,c2).GetValues();return{ok:true,from:[r1,c1],values:v.slice(0,60).map(function(r){return r.slice(0,20)})}}',
    'function xW(a){var sh=Api.GetActiveSheet(),n=0;for(var i=0;i<a.cells.length;i++){var c=a.cells[i];try{sh.GetRange(cN(c[1])+(c[0]+1)).SetValue(String(c[2]));n++}catch(e){}}return n?{ok:true,written:n}:{ok:false,error:"未写入"}}',
    'function xF(a){var sh=Api.GetActiveSheet();var rg=xR(sh,a.r1||0,a.c1||0,a.r2||(a.r1||0),a.c2||(a.c1||0));var ops=[];function T(n,f){try{f();ops.push(n)}catch(e){}}if(a.bold!==undefined)T("bold",function(){rg.SetBold(a.bold)});if(a.fontSize)T("size",function(){rg.SetFontSize(a.fontSize)});if(a.fillColor)T("fill",function(){rg.SetFillColor(hx(a.fillColor))});if(a.numberFormat)T("fmt",function(){rg.SetNumberFormat(a.numberFormat)});if(a.hAlign)T("al",function(){rg.SetHorizontalAlignment(a.hAlign)});return ops.length?{ok:true,applied:ops}:{ok:false,error:"无可用API"}}',
    'function xRo(a){var sh=Api.GetActiveSheet();try{var rg=sh.GetRange((a.at+1)+":"+(a.at+(a.count||1)));if(a.op==="insert")rg.Insert();else if(a.op==="delete")rg.Delete();else return{ok:false,error:"op须insert|delete"};return{ok:true}}catch(e){return{ok:false,error:String(e)}}}',
    'function xCw(a){var sh=Api.GetActiveSheet();try{sh.GetRange(cN(a.col)+":"+cN(a.col)).SetColumnWidth(a.width);return{ok:true}}catch(e){return{ok:false,error:"不可用"}}}',
    'function pL(){var pr=Api.GetPresentation();return{ok:true,kind:"slide",slides:pr.GetSlidesCount()}}',
    'function pR(a){var pr=Api.GetPresentation(),s=pr.GetSlideByIndex(a.index);if(!s)return{ok:false,error:"不存在"};var tx=[];try{s.ForEachShape(function(sh){try{if(sh.GetText)tx.push(clip(sh.GetText(),200))}catch(e){}})}catch(x){}return{ok:true,texts:tx}}',
    'function pA(a){try{var pr=Api.GetPresentation(),s=pr.CreateSlide();pr.AddSlide(s);var ls=[a.title||""].concat(a.bullets||[]);var y=10;for(var i=0;i<ls.length;i++){if(!ls[i])continue;try{if(s.AddText)s.AddText(String(ls[i]),10,y,280,20,20)}catch(x){}y+=24}return{ok:true}}catch(e){return{ok:false,error:String(e)}}}',
    'function wImg(a){try{var d=Api.GetDocument();var img=Api.CreateImage(a.url,a.width||200,a.height||150);var p=Api.CreateParagraph();p.AddDrawing(img);d.Push(p);return{ok:true}}catch(e){return{ok:false,error:"img:"+String(e)}}}',
    'function wPN(){try{var sec=Api.GetDocument().GetSection(0);if(sec){var f=sec.GetFooter();if(f){var p=f.GetElement(0);if(!p){p=Api.CreateParagraph();f.Push(p)}p.AddPageNumber();return{ok:true}}}return{ok:false,error:"no footer"}}catch(e){return{ok:false,error:String(e)}}}',
    'function wPC(){try{return{ok:true,elements:Api.GetDocument().GetElementsCount()}}catch(e){return{ok:false,error:String(e)}}}',
    'function wPO(a){try{var sec=Api.GetDocument().GetSection(0);if(sec){sec.SetPageSize(a.orientation==="landscape"?842:595,a.orientation==="landscape"?595:842);return{ok:true}}return{ok:false,error:"no sec"}}catch(e){return{ok:false,error:String(e)}}}',
    'function wHR(){try{var d=Api.GetDocument();var p=Api.CreateParagraph();p.SetJc("center");var r=p.AddText("\\u2014\\u2014\\u2014\\u2014\\u2014\\u2014\\u2014\\u2014\\u2014\\u2014");r.SetColor(180,180,180,false);d.Push(p);return{ok:true}}catch(e){return{ok:false,error:String(e)}}}',
    'function wHL(a){try{var d=Api.GetDocument();var p=Api.CreateParagraph();var r=p.AddText(a.text||a.url);if(r.SetColor)r.SetColor(79,124,255,false);if(r.SetUnderline)r.SetUnderline(true);d.Push(p);return{ok:true}}catch(e){return{ok:false,error:String(e)}}}',
    'function wPM(a){try{var sec=Api.GetDocument().GetSection(0);if(sec&&sec.SetPageMargins){sec.SetPageMargins(a.top||72,a.right||72,a.bottom||72,a.left||72);return{ok:true}}return{ok:false,error:"n/a"}}catch(e){return{ok:false,error:String(e)}}}',
    'function wTOC(){try{var d=Api.GetDocument();var out=[para("## 目录")];var sc=scan(null);for(var i=0;i<sc.hits.length;i++){var h=sc.hits[i];if(h.kind==="para"&&h.text&&h.text.indexOf("#")==0)out.push(para(h.text))}if(out.length<2)out.push(para("(no headings)"));for(var j=0;j<out.length;j++)d.Push(out[j]);return{ok:true}}catch(e){return{ok:false,error:String(e)}}}',
    'function wHF(a){try{var sec=Api.GetDocument().GetSection(0);if(!sec)return{ok:false,error:"no sec"};var hf=a.position==="header"?sec.GetHeader():sec.GetFooter();if(!hf)return{ok:false,error:"no hf"};var p=hf.GetElement(0);if(!p){p=Api.CreateParagraph();hf.Push(p)}if(a.text!==undefined){p.AddText(a.text)}return{ok:true}}catch(e){return{ok:false,error:String(e)}}}',
    'function wRA(a){var d=Api.GetDocument(),sc=scan(a.find),ix=[];for(var h=0;h<sc.hits.length;h++)if(sc.hits[h].kind==="para")ix.push(sc.hits[h].i);if(!ix.length)return{ok:false,error:"not found"};var es=mdE(a.replace||""),dn=0;for(var t=ix.length-1;t>=0;t--){var i=ix[t];try{d.RemoveElement(i);try{for(var k=es.length-1;k>=0;k--)d.AddElement(i,es[k])}catch(x){for(var j=0;j<es.length;j++)d.Push(es[j])}dn++}catch(y){}}return dn?{ok:true,replaced:dn}:{ok:false,error:"fail"}}',
    'function xM(a){try{var sh=Api.GetActiveSheet();var rg=xR(sh,a.r1,a.c1,a.r2,a.c2);if(a.merge===false)rg.UnMerge();else rg.Merge();return{ok:true}}catch(e){return{ok:false,error:String(e)}}}',
    'function xFz(a){try{var sh=Api.GetActiveSheet();sh.GetRange(cN(a.c||0)+(a.r||0)).SetFrozen(true);return{ok:true}}catch(e){return{ok:false,error:"n/a"}}}',
    'function xS(a){try{var wb=Api.GetWorkbook?Api.GetWorkbook():null;if(!wb)return{ok:false,error:"no wb"};if(a.op==="list"){return{ok:true,count:wb.GetSheetsCount?wb.GetSheetsCount():0}}if(a.op==="add"&&wb.AddSheet){wb.AddSheet(a.name||"");return{ok:true}}return{ok:false,error:"n/a"}}catch(e){return{ok:false,error:String(e)}}}',
    'function xAf(a){try{var sh=Api.GetActiveSheet();for(var j=0;j<20;j++){sh.GetRange(cN(j)+":"+cN(j)).SetColumnWidth("auto")}return{ok:true}}catch(e){return{ok:false,error:"n/a"}}}',
    'function xSort(a){try{var sh=Api.GetActiveSheet();var rg=xR(sh,a.r1,a.c1,a.r2,a.c2);if(rg.Sort)rg.Sort(a.by||0,a.order==="desc");return{ok:true}}catch(e){return{ok:false,error:"n/a"}}}',
    'function xFl(a){try{var sh=Api.GetActiveSheet();var rg=xR(sh,a.r1,a.c1,a.r2,a.c2);if(rg.SetFilter)rg.SetFilter(true);return{ok:true}}catch(e){return{ok:false,error:"n/a"}}}',
    'function xFm(a){return xW({cells:[[a.r,a.c,a.formula]]})}',
    'function xChart(a){try{var sh=Api.GetActiveSheet();var rg=xR(sh,a.r1,a.c1,a.r2,a.c2);if(sh.AddChart){sh.AddChart(a.type||"bar",rg,a.r2+2,a.c1,400,300);return{ok:true}}return{ok:false,error:"chart n/a"}}catch(e){return{ok:false,error:String(e)}}}',
    'function xCF(a){try{var sh=Api.GetActiveSheet();var rg=xR(sh,a.r1,a.c1,a.r2,a.c2);if(rg.SetConditionalFormat){rg.SetConditionalFormat(a.rule||"cellIs",a.operator||"greaterThan",a.value||0,a.format||{fillColor:"FFCCCC"});return{ok:true}}return{ok:false,error:"cf n/a"}}catch(e){return{ok:false,error:String(e)}}}',
    'function pTB(a){try{var pr=Api.GetPresentation();var s=pr.GetSlideByIndex(a.slide||pr.GetSlidesCount()-1);if(!s)return{ok:false,error:"no slide"};if(s.AddText){s.AddText(a.text||"",a.x||10,a.y||50,a.w||280,a.h||30,a.fontSize||14);return{ok:true}}return{ok:false,error:"n/a"}}catch(e){return{ok:false,error:String(e)}}}',
    'function pDel(a){try{var pr=Api.GetPresentation();if(pr.DeleteSlide)pr.DeleteSlide(a.slide);return{ok:true}}catch(e){return{ok:false,error:String(e)}}}',
    'function pMove(a){try{var pr=Api.GetPresentation();if(pr.MoveSlide)pr.MoveSlide(a.from,a.to);return{ok:true}}catch(e){return{ok:false,error:String(e)}}}',
    'function pTitle(a){try{var pr=Api.GetPresentation();var s=pr.GetSlideByIndex(a.slide);if(!s)return{ok:false,error:"no slide"};var tx=s.GetAllShapes?s.GetAllShapes():[];if(tx.length>0&&tx[0].SetText){tx[0].SetText(a.title);return{ok:true}}return{ok:false,error:"n/a"}}catch(e){return{ok:false,error:String(e)}}}',
    'function pNotes(a){try{var pr=Api.GetPresentation();var s=pr.GetSlideByIndex(a.slide);if(!s)return{ok:false,error:"no slide"};if(a.notes!==undefined&&s.SetNotes){s.SetNotes(a.notes);return{ok:true}}if(s.GetNotes)return{ok:true,notes:s.GetNotes()};return{ok:false,error:"n/a"}}catch(e){return{ok:false,error:String(e)}}}',
    'function pShape(a){try{var pr=Api.GetPresentation();var s=pr.GetSlideByIndex(a.slide||pr.GetSlidesCount()-1);if(!s)return{ok:false,error:"no slide"};var sh=Api.CreateShape?Api.CreateShape(a.type||"rect",a.w||100,a.h||50):null;if(sh&&s.AddObject){s.AddObject(sh);return{ok:true}}return{ok:false,error:"n/a"}}catch(e){return{ok:false,error:String(e)}}}',
    'function pBg(a){try{var pr=Api.GetPresentation();var s=pr.GetSlideByIndex(a.slide);if(!s)return{ok:false,error:"no slide"};if(s.SetBackground)s.SetBackground(a.color||"F5F5F5");return{ok:true}}catch(e){return{ok:false,error:String(e)}}}',
    'function pLayout(a){try{var pr=Api.GetPresentation();var s=pr.GetSlideByIndex(a.slide);if(!s)return{ok:false,error:"no slide"};if(s.SetLayout)s.SetLayout(a.layout||"title");return{ok:true}}catch(e){return{ok:false,error:String(e)}}}',
    'function dp(d){var k=ek();try{',
    'if(d.tool==="get_doc_info"){if(k==="word"){var sc=scan(null),sel="";try{sel=Api.GetDocument().GetSelectedText?Api.GetDocument().GetSelectedText():""}catch(s0){}return{ok:true,kind:k,elements:sc.count,overview:sc.hits.slice(0,80),selection:clip(sel,500)}}if(k==="cell")return xI();if(k==="slide")return pL();return{ok:true,kind:k}}',
    'if(k==="word"){',
    'if(d.tool==="read_document"){var f=d.args.from||0,t2=d.args.to,sc2=scan(null),l=[];for(var i=f;i<sc2.count&&(t2===undefined||i<=t2)&&l.length<60;i++)l.push({i:i,text:clip(sc2.hits[i]?sc2.hits[i].text:"",d.args.maxChars||500)});return{ok:true,total:sc2.count,elements:l}}',
    'if(d.tool==="search_text")return{ok:true,total:scan(null).count,hits:scan(d.args.query).hits.slice(0,30)}',
    'if(d.tool==="insert_content")return wIns(d.args)',
    'if(d.tool==="replace_text")return wRep(d.args)',
    'if(d.tool==="replace_element"){var d2=Api.GetDocument(),es2=mdE(d.args.md||"");d2.RemoveElement(d.args.index);try{for(var k3=es2.length-1;k3>=0;k3--)d2.AddElement(d.args.index,es2[k3])}catch(x2){for(var j2=0;j2<es2.length;j2++)d2.Push(es2[j2])}return{ok:true}}',
    'if(d.tool==="delete_element"){try{Api.GetDocument().RemoveElement(d.args.index);return{ok:true}}catch(x3){return{ok:false,error:"越界"}}}',
    'if(d.tool==="format_element")return wFmt(d.args)',
    'if(d.tool==="set_paragraph_style")return wSty(d.args)',
    'if(d.tool==="insert_table"){var t3=mkT(d.args.rows||[["1","2"]]);if((d.args.at||"end")==="cursor")Api.GetDocument().InsertContent([t3]);else Api.GetDocument().Push(t3);return{ok:true}}',
    'if(d.tool==="table_op")return wTO(d.args)',
    'if(d.tool==="insert_page_break")return wPB()',
    'if(d.tool==="add_comment")return wCm(d.args)',
    'if(d.tool==="insert_image")return wImg(d.args)',
    'if(d.tool==="insert_page_number")return wPN()',
    'if(d.tool==="get_page_count")return wPC()',
    'if(d.tool==="set_page_orientation")return wPO(d.args)',
    'if(d.tool==="insert_horizontal_rule")return wHR()',
    'if(d.tool==="insert_hyperlink")return wHL(d.args)',
    'if(d.tool==="set_page_margins")return wPM(d.args)',
    'if(d.tool==="insert_toc")return wTOC()',
    'if(d.tool==="set_header_footer")return wHF(d.args)',
    'if(d.tool==="replace_all")return wRA(d.args)}',
    'if(k==="cell"){var aA=d.args||{};',
    'if(d.tool==="read_range"||d.tool==="read_document"||d.tool==="read_cells")return xRd(aA)',
    'if(d.tool==="write_cells")return xW(d.args)',
    'if(d.tool==="format_range")return xF(d.args)',
    'if(d.tool==="row_op")return xRo(d.args)',
    'if(d.tool==="set_col_width")return xCw(d.args)',
    'if(d.tool==="merge_cells")return xM(d.args)',
    'if(d.tool==="freeze_panes")return xFz(d.args)',
    'if(d.tool==="sheet_op")return xS(d.args)',
    'if(d.tool==="autofit")return xAf(d.args)',
    'if(d.tool==="sort_range")return xSort(d.args)',
    'if(d.tool==="apply_filter")return xFl(d.args)',
    'if(d.tool==="insert_formula")return xFm(d.args)',
    'if(d.tool==="insert_chart")return xChart(d.args)',
    'if(d.tool==="set_conditional_format")return xCF(d.args)}',
    'if(k==="slide"){if(d.tool==="read_slide")return pR(d.args);if(d.tool==="add_slide")return pA(d.args)',
    'if(d.tool==="add_text_box")return pTB(d.args)',
    'if(d.tool==="delete_slide")return pDel(d.args)',
    'if(d.tool==="move_slide")return pMove(d.args)',
    'if(d.tool==="set_slide_title")return pTitle(d.args)',
    'if(d.tool==="slide_notes")return pNotes(d.args)',
    'if(d.tool==="add_shape")return pShape(d.args)',
    'if(d.tool==="set_slide_background")return pBg(d.args)',
    'if(d.tool==="set_slide_layout")return pLayout(d.args)}',
    'return{ok:false,error:"工具"+d.tool+"在"+k+"不可用"}}catch(x){return{ok:false,error:String(x&&x.message||x)}}}',
    'return dp(data);'
  ].join(NL);

  function runEditorTool(tool, args) {
    return new Promise(function (resolve) {
      if (!window.Asc || !window.Asc.plugin) { resolve({ ok: false, error: '环境未就绪' }); return; }
      try {
        var src = 'var data=' + JSON.stringify({ tool: tool, args: args || {} }) + ';\nvar NL=String.fromCharCode(10);\n' + RUNTIME;
        var fn = new Function('return (function(){\n' + src + '\n})')();
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

  var TOOLS = {
    word: ['get_doc_info {} → 结构总览。', 'read_document {from?,to?} → 读全文。', 'search_text {query} → 定位。',
      'insert_content {md,at?,index?} → 插入。', 'replace_text {find,md,scope?} → 段落替换。', 'replace_all {find,replace} → 全文替换。',
      'replace_element {index,md}。', 'delete_element {index}。',
      'format_element {index,bold?,italic?,underline?,fontSize?,color?}。', 'set_paragraph_style {index,style?,align?}。',
      'insert_table {rows,at?}。', 'table_op {index,op,r?,c?,text?}。', 'insert_page_break {}。', 'add_comment {find,text}。',
      'insert_image {url,width?,height?} → 插入图片。', 'insert_page_number {} → 页脚页码。', 'get_page_count {} → 元素数。',
      'set_page_orientation {orientation} → portrait/landscape。', 'insert_horizontal_rule {} → 分隔线。',
      'insert_hyperlink {text,url}。', 'set_page_margins {top,right,bottom,left} → pt。', 'insert_toc {} → 目录大纲。',
      'set_header_footer {position:"header"|"footer",text} → 页眉/页脚。'].join('\n'),
    cell: ['get_doc_info {} → 区域。', 'read_range {r1,c1,r2?,c2?}。', 'write_cells {cells:[[r,c,v],…]} → 写（支持公式）。',
      'format_range {…,bold?,fontSize?,fillColor?,numberFormat?,hAlign?}。', 'row_op {op,at,count?}。', 'set_col_width {col,width}。',
      'merge_cells {r1,c1,r2,c2,merge?} → 合并/取消。', 'freeze_panes {r,c}。', 'sheet_op {op:"list"|"add",name?}。',
      'autofit {} → 自动列宽。', 'sort_range {r1,c1,r2,c2,by?,order?}。', 'apply_filter {r1,c1,r2,c2}。',
      'insert_formula {r,c,formula}。', 'insert_chart {type,r1,c1,r2,c2}。', 'set_conditional_format {r1,c1,r2,c2,rule?,operator?,value?}。'].join('\n'),
    slide: ['get_doc_info {} → 页数。', 'read_slide {index}。', 'add_slide {title,bullets?}。',
      'add_text_box {slide,text,x?,y?,w?,h?}。', 'delete_slide {slide}。', 'move_slide {from,to}。',
      'set_slide_title {slide,title}。', 'slide_notes {slide,notes?} → 读/写备注。', 'add_shape {slide,type?,w?,h?}。',
      'set_slide_background {slide,color}。', 'set_slide_layout {slide,layout}。'].join('\n'),
    pdf: '（PDF 无文档 API，仅对话。）'
  };
  function buildSystem() {
    var k = editorType || 'pdf';
    var L = [
      '你是 DocFlow 内置文档编辑 Agent，运行在 OnlyOffice 编辑器侧栏中。',
      '当前编辑器类型：' + k + '。你操作的是当前打开的编辑器文档——不是平台文件系统。',
      '重要：写入内容必须用编辑器工具（insert_content / write_cells 等）直接修改当前文档，不要用 list_files/read_file 生成新文件。',
      '', '工具：', TOOLS[k] || TOOLS.pdf,
      '跨工具：list_files {name?} / read_file {name} → 平台文件检索/读取（作上下文参考，写入仍用编辑器工具）。',
      '', '输出格式（硬性）：每轮恰好一行 TOOL_CALL {...}，或 FINAL 开头。'
    ];
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

  var MAX_ROUNDS = 20;
  function setBusyUI(b) { busy = b; sendBtn.style.display = b ? 'none' : ''; stopBtn.style.display = b ? '' : 'none'; }

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
      setHint(editorType ? '就绪 · ' + editorType : '就绪');
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
      if (n >= 20) { clearInterval(p); setHint('调试模式'); }
    }, 500);
    void refresh().then(function () { return loadModels().then(renderModels); }).catch(function () {});
  }
})();
