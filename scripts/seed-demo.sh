#!/usr/bin/env bash
set -euo pipefail

if [[ "${DEMO_SEED:-false}" != "true" ]]; then
  echo "DEMO_SEED=true is required; no demo data was created" >&2
  exit 2
fi

python3 - <<'PY'
import base64, io, json, os, urllib.error, urllib.request, zipfile

base = os.getenv('DOCFLOW_URL', 'http://127.0.0.1').rstrip('/')
email = os.environ['SEED_ADMIN_EMAIL']
password = os.environ['SEED_ADMIN_PASSWORD']

def request(method, path, token='', body=None, content_type='application/json', headers=None):
    data = json.dumps(body).encode() if content_type == 'application/json' and body is not None else body
    req_headers = {'Content-Type': content_type, **(headers or {})}
    if token:
        req_headers['Authorization'] = f'Bearer {token}'
    req = urllib.request.Request(base + path, data=data, method=method, headers=req_headers)
    with urllib.request.urlopen(req) as response:
        raw = response.read()
        return json.loads(raw) if raw and response.headers.get_content_type() == 'application/json' else raw

def archive(entries):
    out = io.BytesIO()
    with zipfile.ZipFile(out, 'w', zipfile.ZIP_DEFLATED) as z:
        for name, content in entries.items():
            z.writestr(name, content)
    return out.getvalue()

def docx():
    return archive({
        '[Content_Types].xml': '<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/><Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/><Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/><Override PartName="/docProps/app.xml" ContentType="application/vnd.openxmlformats-officedocument.extended-properties+xml"/></Types>',
        '_rels/.rels': '<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/><Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/extended-properties" Target="docProps/app.xml"/></Relationships>',
        'docProps/core.xml': '<?xml version="1.0"?><cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>DocFlow demo</dc:title></cp:coreProperties>',
        'docProps/app.xml': '<?xml version="1.0"?><Properties xmlns="http://schemas.openxmlformats.org/officeDocument/2006/extended-properties"><Application>DocFlow</Application></Properties>',
        'word/document.xml': '<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>DocFlow demo document</w:t></w:r></w:p><w:sectPr/></w:body></w:document>',
        'word/_rels/document.xml.rels': '<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/></Relationships>',
        'word/styles.xml': '<?xml version="1.0"?><w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style></w:styles>',
    })

def pptx():
    return archive({
        '[Content_Types].xml': '<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/><Override PartName="/ppt/slides/slide1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slide+xml"/></Types>',
        '_rels/.rels': '<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="ppt/presentation.xml"/></Relationships>',
        'ppt/presentation.xml': '<?xml version="1.0"?><p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:sldIdLst><p:sldId id="256" r:id="rId1"/></p:sldIdLst><p:sldMasterIdLst/><p:sldSz cx="12192000" cy="6858000"/><p:notesSz cx="6858000" cy="9144000"/></p:presentation>',
        'ppt/_rels/presentation.xml.rels': '<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide1.xml"/></Relationships>',
        'ppt/slides/slide1.xml': '<?xml version="1.0"?><p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"><p:cSld><p:spTree/></p:cSld></p:sld>',
    })

def xlsx():
    return archive({
        '[Content_Types].xml': '<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/><Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/><Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/><Override PartName="/docProps/app.xml" ContentType="application/vnd.openxmlformats-officedocument.extended-properties+xml"/></Types>',
        '_rels/.rels': '<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/><Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/extended-properties" Target="docProps/app.xml"/></Relationships>',
        'docProps/core.xml': '<?xml version="1.0"?><cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>DocFlow demo</dc:title></cp:coreProperties>',
        'docProps/app.xml': '<?xml version="1.0"?><Properties xmlns="http://schemas.openxmlformats.org/officeDocument/2006/extended-properties"><Application>DocFlow</Application></Properties>',
        'xl/workbook.xml': '<?xml version="1.0"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Demo" sheetId="1" r:id="rId1"/></sheets></workbook>',
        'xl/_rels/workbook.xml.rels': '<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/></Relationships>',
        'xl/worksheets/sheet1.xml': '<?xml version="1.0"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>DocFlow demo</t></is></c></row></sheetData></worksheet>',
        'xl/styles.xml': '<?xml version="1.0"?><styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><fonts count="1"><font/></fonts><fills count="1"><fill/></fills><borders count="1"><border/></borders><cellStyleXfs count="1"><xf/></cellStyleXfs><cellXfs count="1"><xf xfId="0"/></cellXfs></styleSheet>',
    })

login = request('POST', '/api/v1/auth/login', body={'email': email, 'password': password})
token = login['access_token']
listing = request('GET', '/api/v1/files?limit=100', token)
if isinstance(listing, list):
    items = listing
elif isinstance(listing, dict):
    items = listing.get('files') or listing.get('items') or listing.get('data') or []
    if isinstance(items, dict):
        items = items.get('files') or items.get('items') or []
else:
    items = []
existing = {item['name']: item for item in items}
files = {
    'demo-document.docx': docx(),
    'demo-sheet.xlsx': xlsx(),
    'demo-presentation.pptx': pptx(),
    'demo-config.json': b'{"name": "DocFlow demo", "version": 1}\n',
    'demo-text.txt': b'DocFlow demo text\n',
    'demo-note.md': b'# DocFlow demo\n\nSafe **Markdown** preview.\n',
    'demo-page.html': b'<!doctype html><meta charset="utf-8"><title>DocFlow demo</title><h1>Demo page</h1>',
    'demo-web-package.zip': archive({'index.html': '<!doctype html><meta charset="utf-8"><h1>DocFlow web package</h1>'}),
    'demo-diagram.drawio': b'<mxfile><diagram name="Page-1"><mxGraphModel><root><mxCell id="0"/><mxCell id="1" parent="0"/></root></mxGraphModel></diagram></mxfile>',
    'demo-whiteboard.excalidraw': json.dumps({'type': 'excalidraw', 'version': 2, 'source': 'docflow', 'elements': [], 'appState': {}, 'files': {}}).encode(),
}
for name, content in files.items():
    if name.endswith(('.docx', '.xlsx', '.pptx')):
        with zipfile.ZipFile(io.BytesIO(content)) as package:
            required = {'[Content_Types].xml', 'word/document.xml'} if name.endswith('.docx') else {'[Content_Types].xml', 'xl/workbook.xml', 'xl/_rels/workbook.xml.rels'} if name.endswith('.xlsx') else {'[Content_Types].xml', 'ppt/presentation.xml'}
            if not required.issubset(package.namelist()) or package.testzip() is not None:
                raise RuntimeError(f'{name} is not a valid OOXML package')
    if name in existing:
        print('SKIP', name)
        continue
    session = request('POST', '/api/v1/uploads', token, {'name': name, 'size': len(content)})
    request('PATCH', f"/api/v1/uploads/{session['id']}", token, content, 'application/octet-stream', {'Upload-Offset': '0'})
    request('POST', f"/api/v1/uploads/{session['id']}/complete", token, {})
    print('CREATED', name)
PY
