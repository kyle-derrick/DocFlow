import io, re
NL = chr(10)
p = 'frontend/public/oo-plugins/docflow-ai/plugin.js'
s = io.open(p, encoding='utf-8').read().replace(chr(13)+chr(10), NL)

dp_line = "    'function dp(d){var k=ek();try{',"
assert s.count(dp_line) == 1

new_tools = [
    "    'function wImg(a){try{var d=Api.GetDocument();var img=Api.CreateImage(a.url,a.width||200,a.height||150);var p=Api.CreateParagraph();p.AddDrawing(img);d.Push(p);return{ok:true}}catch(e){return{ok:false,error:\"img:\"+String(e)}}}',",
    "    'function wPN(){try{var d=Api.GetDocument();var sec=d.GetSection(0);if(sec){var f=sec.GetFooter();if(f){var p=f.GetElement(0);if(!p){p=Api.CreateParagraph();f.Push(p)}p.AddPageNumber();return{ok:true}}}return{ok:false,error:\"no footer\"}}catch(e){return{ok:false,error:String(e)}}}',",
    "    'function wPC(){try{return{ok:true,elements:Api.GetDocument().GetElementsCount()}}catch(e){return{ok:false,error:String(e)}}}',",
    "    'function wPO(a){try{var sec=Api.GetDocument().GetSection(0);if(sec){sec.SetPageSize(a.orientation==\"landscape\"?842:595,a.orientation==\"landscape\"?595:842);return{ok:true}}return{ok:false,error:\"no sec\"}}catch(e){return{ok:false,error:String(e)}}}',",
    "    'function wHR(){try{var d=Api.GetDocument();var p=Api.CreateParagraph();p.SetJc(\"center\");var r=p.AddText(\"\\u2014\\u2014\\u2014\\u2014\\u2014\\u2014\\u2014\\u2014\\u2014\\u2014\");r.SetColor(180,180,180,false);d.Push(p);return{ok:true}}catch(e){return{ok:false,error:String(e)}}}',",
    "    'function wHL(a){try{var d=Api.GetDocument();var p=Api.CreateParagraph();var r=p.AddText(a.text||a.url);if(r.SetColor)r.SetColor(79,124,255,false);if(r.SetUnderline)r.SetUnderline(true);d.Push(p);return{ok:true}}catch(e){return{ok:false,error:String(e)}}}',",
    "    'function wPM(a){try{var sec=Api.GetDocument().GetSection(0);if(sec&&sec.SetPageMargins){sec.SetPageMargins(a.top||72,a.right||72,a.bottom||72,a.left||72);return{ok:true}}return{ok:false,error:\"no margins api\"}}catch(e){return{ok:false,error:String(e)}}}',",
    "    'function wTOC(){try{var d=Api.GetDocument();var out=[para(\"## 目录\")];var sc=scan(null);for(var i=0;i<sc.hits.length;i++){var h=sc.hits[i];if(h.kind==\"para\"&&h.text&&h.text.indexOf(\"#\")==0)out.push(para(h.text))}if(out.length<2)out.push(para(\"（未找到标题）\"));for(var j=0;j<out.length;j++)d.Push(out[j]);return{ok:true}}catch(e){return{ok:false,error:String(e)}}}',",
    "    'function wHF(a){try{var sec=Api.GetDocument().GetSection(0);if(!sec)return{ok:false,error:\"no sec\"};var hf=a.position==\"header\"?sec.GetHeader():sec.GetFooter();if(!hf)return{ok:false,error:\"no hf\"};var p=hf.GetElement(0);if(!p){p=Api.CreateParagraph();hf.Push(p)}if(a.text!==undefined){p.AddText(a.text)}return{ok:true}}catch(e){return{ok:false,error:String(e)}}}',",
    "    'function wRA(a){var d=Api.GetDocument(),sc=scan(a.find),ix=[];for(var h=0;h<sc.hits.length;h++)if(sc.hits[h].kind==\"para\")ix.push(sc.hits[h].i);if(!ix.length)return{ok:false,error:\"not found\"};var es=mdE(a.replace||\"\"),dn=0;for(var t=ix.length-1;t>=0;t--){var i=ix[t];try{d.RemoveElement(i);try{for(var k=es.length-1;k>=0;k--)d.AddElement(i,es[k])}catch(x){for(var j=0;j<es.length;j++)d.Push(es[j])}dn++}catch(y){}}return dn?{ok:true,replaced:dn}:{ok:false,error:\"fail\"}}',",
    "    'function xM(a){try{var sh=Api.GetActiveSheet();var rg=xR(sh,a.r1,a.c1,a.r2,a.c2);if(a.merge===false)rg.UnMerge();else rg.Merge();return{ok:true}}catch(e){return{ok:false,error:String(e)}}}',",
    "    'function xFz(a){try{var sh=Api.GetActiveSheet();sh.GetRange(cN(a.c||0)+(a.r||0)).SetFrozen(true);return{ok:true}}catch(e){return{ok:false,error:\"freeze n/a\"}}}',",
    "    'function xS(a){try{var wb=Api.GetWorkbook?Api.GetWorkbook():null;if(!wb)return{ok:false,error:\"no wb\"};if(a.op==\"list\"){return{ok:true,count:wb.GetSheetsCount?wb.GetSheetsCount():0}}if(a.op==\"add\"&&wb.AddSheet){wb.AddSheet(a.name||\"\");return{ok:true}}return{ok:false,error:\"unsupported\"}}catch(e){return{ok:false,error:String(e)}}}',",
    "    'function xAf(a){try{var sh=Api.GetActiveSheet();for(var j=0;j<20;j++){sh.GetRange(cN(j)+\":\"+cN(j)).SetColumnWidth(\"auto\")}return{ok:true}}catch(e){return{ok:false,error:\"autofit n/a\"}}}',",
    "    'function xSort(a){try{var sh=Api.GetActiveSheet();var rg=xR(sh,a.r1,a.c1,a.r2,a.c2);if(rg.Sort)rg.Sort(a.by||0,a.order==\"desc\");return{ok:true}}catch(e){return{ok:false,error:\"sort n/a\"}}}',",
    "    'function xFl(a){try{var sh=Api.GetActiveSheet();var rg=xR(sh,a.r1,a.c1,a.r2,a.c2);if(rg.SetFilter)rg.SetFilter(true);return{ok:true}}catch(e){return{ok:false,error:\"filter n/a\"}}}',",
    "    'function xFm(a){return xW({cells:[[a.r,a.c,a.formula]]})}',",
    "    'function xChart(a){try{var sh=Api.GetActiveSheet();var rg=xR(sh,a.r1,a.c1,a.r2,a.c2);if(sh.AddChart){sh.AddChart(a.type||\"bar\",rg,a.r2+2,a.c1,400,300);return{ok:true}}return{ok:false,error:\"chart n/a\"}}catch(e){return{ok:false,error:String(e)}}}',",
    "    'function xCF(a){try{var sh=Api.GetActiveSheet();var rg=xR(sh,a.r1,a.c1,a.r2,a.c2);if(rg.SetConditionalFormat){rg.SetConditionalFormat(a.rule||\"cellIs\",a.operator||\"greaterThan\",a.value||0,a.format||{fillColor:\"FFCCCC\"});return{ok:true}}return{ok:false,error:\"cf n/a\"}}catch(e){return{ok:false,error:String(e)}}}',",
    "    'function pTB(a){try{var pr=Api.GetPresentation();var s=pr.GetSlideByIndex(a.slide||pr.GetSlidesCount()-1);if(!s)return{ok:false,error:\"no slide\"};if(s.AddText){s.AddText(a.text||\"\",a.x||10,a.y||50,a.w||280,a.h||30,a.fontSize||14);return{ok:true}}return{ok:false,error:\"tb n/a\"}}catch(e){return{ok:false,error:String(e)}}}',",
    "    'function pDel(a){try{var pr=Api.GetPresentation();if(pr.DeleteSlide)pr.DeleteSlide(a.slide);return{ok:true}}catch(e){return{ok:false,error:String(e)}}}',",
    "    'function pMove(a){try{var pr=Api.GetPresentation();if(pr.MoveSlide)pr.MoveSlide(a.from,a.to);return{ok:true}}catch(e){return{ok:false,error:String(e)}}}',",
    "    'function pTitle(a){try{var pr=Api.GetPresentation();var s=pr.GetSlideByIndex(a.slide);if(!s)return{ok:false,error:\"no slide\"};var tx=s.GetAllShapes?s.GetAllShapes():[];if(tx.length>0&&tx[0].SetText){tx[0].SetText(a.title);return{ok:true}}return{ok:false,error:\"title n/a\"}}catch(e){return{ok:false,error:String(e)}}}',",
    "    'function pNotes(a){try{var pr=Api.GetPresentation();var s=pr.GetSlideByIndex(a.slide);if(!s)return{ok:false,error:\"no slide\"};if(a.notes!==undefined&&s.SetNotes){s.SetNotes(a.notes);return{ok:true}}if(s.GetNotes)return{ok:true,notes:s.GetNotes()};return{ok:false,error:\"notes n/a\"}}catch(e){return{ok:false,error:String(e)}}}',",
    "    'function pShape(a){try{var pr=Api.GetPresentation();var s=pr.GetSlideByIndex(a.slide||pr.GetSlidesCount()-1);if(!s)return{ok:false,error:\"no slide\"};var sh=Api.CreateShape?Api.CreateShape(a.type||\"rect\",a.w||100,a.h||50):null;if(sh&&s.AddObject){s.AddObject(sh);return{ok:true}}return{ok:false,error:\"shape n/a\"}}catch(e){return{ok:false,error:String(e)}}}',",
    "    'function pBg(a){try{var pr=Api.GetPresentation();var s=pr.GetSlideByIndex(a.slide);if(!s)return{ok:false,error:\"no slide\"};if(s.SetBackground)s.SetBackground(a.color||\"F5F5F5\");return{ok:true}}catch(e){return{ok:false,error:String(e)}}}',",
    "    'function pLayout(a){try{var pr=Api.GetPresentation();var s=pr.GetSlideByIndex(a.slide);if(!s)return{ok:false,error:\"no slide\"};if(s.SetLayout)s.SetLayout(a.layout||\"title\");return{ok:true}}catch(e){return{ok:false,error:String(e)}}}',",
]

insert_block = NL.join(new_tools) + NL + dp_line
s = s.replace(dp_line, insert_block)

# Add dispatch branches
old_w = "    'if(d.tool===\"add_comment\")return wCm(d.args)}',"
new_w = ("    'if(d.tool==\"add_comment\")return wCm(d.args)',\n"
         "    'if(d.tool==\"insert_image\")return wImg(d.args)',\n"
         "    'if(d.tool==\"insert_page_number\")return wPN()',\n"
         "    'if(d.tool==\"get_page_count\")return wPC()',\n"
         "    'if(d.tool==\"set_page_orientation\")return wPO(d.args)',\n"
         "    'if(d.tool==\"insert_horizontal_rule\")return wHR()',\n"
         "    'if(d.tool==\"insert_hyperlink\")return wHL(d.args)',\n"
         "    'if(d.tool==\"set_page_margins\")return wPM(d.args)',\n"
         "    'if(d.tool==\"insert_toc\")return wTOC()',\n"
         "    'if(d.tool==\"set_header_footer\")return wHF(d.args)',\n"
         "    'if(d.tool==\"replace_all\")return wRA(d.args)}',")
assert s.count(old_w) == 1
s = s.replace(old_w, new_w)

old_c = "    'if(d.tool==\"set_col_width\")return xCw(d.args)}',"
new_c = ("    'if(d.tool==\"set_col_width\")return xCw(d.args)',\n"
         "    'if(d.tool==\"merge_cells\")return xM(d.args)',\n"
         "    'if(d.tool==\"freeze_panes\")return xFz(d.args)',\n"
         "    'if(d.tool==\"sheet_op\")return xS(d.args)',\n"
         "    'if(d.tool==\"autofit\")return xAf(d.args)',\n"
         "    'if(d.tool==\"sort_range\")return xSort(d.args)',\n"
         "    'if(d.tool==\"apply_filter\")return xFl(d.args)',\n"
         "    'if(d.tool==\"insert_formula\")return xFm(d.args)',\n"
         "    'if(d.tool==\"insert_chart\")return xChart(d.args)',\n"
         "    'if(d.tool==\"set_conditional_format\")return xCF(d.args)}',")
assert s.count(old_c) == 1
s = s.replace(old_c, new_c)

old_p = "    'if(k==\"slide\"){if(d.tool==\"read_slide\")return pR(d.args);if(d.tool==\"add_slide\")return pA(d.args)}',"
new_p = ("    'if(k==\"slide\"){if(d.tool==\"read_slide\")return pR(d.args);if(d.tool==\"add_slide\")return pA(d.args)',\n"
         "    'if(d.tool==\"add_text_box\")return pTB(d.args)',\n"
         "    'if(d.tool==\"delete_slide\")return pDel(d.args)',\n"
         "    'if(d.tool==\"move_slide\")return pMove(d.args)',\n"
         "    'if(d.tool==\"set_slide_title\")return pTitle(d.args)',\n"
         "    'if(d.tool==\"slide_notes\")return pNotes(d.args)',\n"
         "    'if(d.tool==\"add_shape\")return pShape(d.args)',\n"
         "    'if(d.tool==\"set_slide_background\")return pBg(d.args)',\n"
         "    'if(d.tool==\"set_slide_layout\")return pLayout(d.args)}',")
assert s.count(old_p) == 1
s = s.replace(old_p, new_p)

# Update TOOLS docs - append new entries via regex
for key, additions in [
    ('word', ", 'insert_image {url,w?,h?}', 'insert_page_number {}', 'get_page_count {}', 'set_page_orientation {o}', 'insert_horizontal_rule {}', 'insert_hyperlink {text,url}', 'set_page_margins {t,r,b,l}', 'insert_toc {}', 'set_header_footer {position,text}', 'replace_all {find,replace}'"),
    ('cell', ", 'merge_cells {r1,c1,r2,c2,merge?}', 'freeze_panes {r,c}', 'sheet_op {op,name?}', 'autofit {}', 'sort_range {…,by?,order?}', 'apply_filter {r1,c1,r2,c2}', 'insert_formula {r,c,f}', 'insert_chart {type,r1,c1,r2,c2}', 'set_conditional_format {…,rule?,op?,val?}'"),
    ('slide', ", 'add_text_box {slide,text,x?,y?,w?,h?}', 'delete_slide {slide}', 'move_slide {from,to}', 'set_slide_title {slide,title}', 'slide_notes {slide,notes?}', 'add_shape {slide,type?,w?,h?}', 'set_slide_background {slide,color}', 'set_slide_layout {slide,layout}'"),
]:
    # Find the last entry before .join and append
    pattern = f"({key}: \\[)(.*?)(\\]\\.join)"
    m = re.search(pattern, s, re.DOTALL)
    if m and additions not in m.group(2):
        s = s[:m.end(2)] + additions + s[m.end(2):]

io.open(p, 'w', encoding='utf-8', newline='').write(s)
print('ALL TOOLS ADDED')
