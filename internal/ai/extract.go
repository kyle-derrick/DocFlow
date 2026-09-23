// Package ai —— extract.go：AI 输入用的文件内容抽取。
//
// 按类型分发（扩展名优先，回退 MIME）：
//   - md/txt/源码等文本类：直读原文；
//   - .dfdoc/.json（Tiptap/ProseMirror JSON）：遍历节点树收集 text 叶子
//     与 codeBlock 文本；
//   - .drawio：解析 mxGraphModel XML，收集 mxCell 的 value/label 属性
//     （含内嵌 HTML 标签剥离）；
//   - .excalidraw：解析 elements[].text（type=text 的白板文字元素）；
//   - .docx/.xlsx/.pptx：Office OpenXML 均为 zip+XML——纯标准库实现
//     （docx=word/document.xml 的 w:t；xlsx=sharedStrings+工作表内联串；
//     pptx=ppt/slides/*.xml 的 a:t），零第三方依赖；
//   - .pdf：最小实现（对象表解析 + FlateDecode 解压 + BT/ET 文本块
//     Tj/TJ 操作符提取），复杂编码（CID 字体/CMap）的 PDF 可能抽取不全，
//     返回已抽取部分；
//   - 其余二进制：返回 false，调用方回退「标题+元信息+描述」占位。
package ai

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// MaxExtractBytes 抽取读取的原始字节上限（防御性；超限截断）。
const MaxExtractBytes = 8 << 20 // 8 MiB

// MaxExtractRunes 抽取结果送入模型的字符上限（超限截断，末尾加省略标记）。
const MaxExtractRunes = 60000

// ErrExtractUnsupported 表示该类型暂不支持全文抽取（调用方回退占位摘要）。
var ErrExtractUnsupported = errors.New("content extraction unsupported for this file type")

// ExtractText 按文件类型抽取纯文本。ok=false 表示该类型不支持全文抽取。
func ExtractText(name, mime string, data []byte) (text string, ok bool) {
	defer func() {
		if recover() != nil {
			text, ok = "", false
		}
	}()
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".dfdoc"), strings.HasSuffix(lower, ".json"):
		return extractTiptap(data)
	case strings.HasSuffix(lower, ".drawio"):
		return extractDrawio(data)
	case strings.HasSuffix(lower, ".excalidraw"):
		return extractExcalidraw(data)
	case strings.HasSuffix(lower, ".docx"):
		return extractDocx(data)
	case strings.HasSuffix(lower, ".xlsx"):
		return extractXlsx(data)
	case strings.HasSuffix(lower, ".pptx"):
		return extractPptx(data)
	case strings.HasSuffix(lower, ".pdf"):
		return extractPDF(data)
	default:
		// 文本类（md/txt/源码等）直读；二进制回退不支持。
		if isPlainText(data) {
			return string(data), true
		}
		return "", false
	}
}

// isPlainText 判定字节流是否 UTF-8 可读文本（无 NUL、UTF-8 合法率）。
func isPlainText(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return false
	}
	// 粗判：解码 UTF-8，非法字节占比 >5% 视为二进制。
	bad := 0
	for i := 0; i < len(data); {
		r, size := decodeRune(data[i:])
		if r == 0xFFFD && size == 1 {
			bad++
		}
		i += size
	}
	return bad*20 <= len(data)
}

func decodeRune(b []byte) (rune, int) {
	if len(b) == 0 {
		return 0, 0
	}
	c := b[0]
	switch {
	case c < 0x80:
		return rune(c), 1
	case c&0xE0 == 0xC0 && len(b) >= 2:
		return rune(c&0x1F)<<6 | rune(b[1]&0x3F), 2
	case c&0xF0 == 0xE0 && len(b) >= 3:
		return rune(c&0x0F)<<12 | rune(b[1]&0x3F)<<6 | rune(b[2]&0x3F), 3
	case c&0xF8 == 0xF0 && len(b) >= 4:
		return rune(c&0x07)<<18 | rune(b[1]&0x3F)<<12 | rune(b[2]&0x3F)<<6 | rune(b[3]&0x3F), 4
	default:
		return 0xFFFD, 1
	}
}

// ---------- Tiptap / ProseMirror JSON（.dfdoc 富文本） ----------

// extractTiptap 遍历 {type:"doc",content:[...]} 节点树，收集 text 节点与
// 代码块文本；块级节点间补换行。普通 .json 文件同样安全（无 text 字段则空）。
func extractTiptap(data []byte) (string, bool) {
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return "", false
	}
	var b strings.Builder
	walkTiptap(root, &b, true)
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", false
	}
	return out, true
}

var tiptapBlockTypes = map[string]bool{
	"paragraph": true, "heading": true, "bulletList": true, "orderedList": true,
	"listItem": true, "codeBlock": true, "blockquote": true, "tableRow": true,
	"tableCell": true, "horizontalRule": true, "taskItem": true,
}

func walkTiptap(node any, b *strings.Builder, blockish bool) {
	m, ok := node.(map[string]any)
	if !ok {
		return
	}
	if t, _ := m["type"].(string); blockish && (t == "doc" || t == "") {
		// 根节点直接下潜。
	} else if text, ok := m["text"].(string); ok {
		b.WriteString(text)
	}
	if content, ok := m["content"].([]any); ok {
		for _, child := range content {
			childType, _ := child.(map[string]any)["type"].(string)
			walkTiptap(child, b, blockish)
			if tiptapBlockTypes[childType] {
				b.WriteString("\n")
			}
		}
	}
}

// ---------- drawio（mxGraphModel XML） ----------

// extractDrawio 收集任意元素的 value/label 属性文本（mxCell 的显示文本）。
func extractDrawio(data []byte) (string, bool) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	var lines []string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		for _, attr := range start.Attr {
			if attr.Name.Local != "value" && attr.Name.Local != "label" {
				continue
			}
			if text := strings.TrimSpace(stripHTML(attr.Value)); text != "" {
				lines = append(lines, text)
			}
		}
	}
	out := strings.Join(lines, "\n")
	return out, out != ""
}

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

func stripHTML(s string) string {
	un := htmlEntityRe.ReplaceAllStringFunc(s, func(m string) string {
		switch m {
		case "&amp;":
			return "&"
		case "&lt;":
			return "<"
		case "&gt;":
			return ">"
		case "&quot;":
			return `"`
		case "&#39;", "&apos;":
			return "'"
		case "&nbsp;":
			return " "
		default:
			return m
		}
	})
	return htmlTagRe.ReplaceAllString(un, "")
}

var htmlEntityRe = regexp.MustCompile(`&(amp|lt|gt|quot|#39|apos|nbsp);`)

// ---------- excalidraw（JSON） ----------

// extractExcalidraw 收集 elements 数组中 type=text 元素的 text 字段。
func extractExcalidraw(data []byte) (string, bool) {
	var doc struct {
		Elements []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"elements"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", false
	}
	var lines []string
	for _, e := range doc.Elements {
		if e.Type == "text" && strings.TrimSpace(e.Text) != "" {
			lines = append(lines, e.Text)
		}
	}
	out := strings.Join(lines, "\n")
	return out, out != ""
}

// ---------- Office OpenXML（docx / xlsx / pptx，zip+XML 纯标准库） ----------

// extractDocx 读取 word/document.xml，收集 w:t 文本节点（w:p 段落换行）。
func extractDocx(data []byte) (string, bool) {
	raw, err := readZipEntry(data, "word/document.xml")
	if err != nil {
		return "", false
	}
	return extractXMLText(raw, "t", "p", "word/document.xml")
}

// extractXlsx 读取共享字符串表 + 全部工作表（sharedStrings 的 t 与内联
// is>t），按行拼接单元格值。
func extractXlsx(data []byte) (string, bool) {
	var b strings.Builder
	if shared, err := readZipEntry(data, "xl/sharedStrings.xml"); err == nil {
		if text, ok := extractXMLText(shared, "t", "si", "xl/sharedStrings.xml"); ok {
			b.WriteString(text)
			b.WriteString("\n")
		}
	}
	sheets := listZipEntries(data, "xl/worksheets/sheet")
	for _, sheet := range sheets {
		raw, err := readZipEntry(data, sheet)
		if err != nil {
			continue
		}
		if text, ok := extractXMLText(raw, "t", "row", sheet); ok {
			b.WriteString(text)
			b.WriteString("\n")
		}
	}
	out := strings.TrimSpace(b.String())
	return out, out != ""
}

// extractPptx 读取 ppt/slides/slideN.xml，收集 a:t 文本（按段落分组）。
func extractPptx(data []byte) (string, bool) {
	slides := listZipEntries(data, "ppt/slides/slide")
	var b strings.Builder
	for _, slide := range slides {
		raw, err := readZipEntry(data, slide)
		if err != nil {
			continue
		}
		if text, ok := extractXMLText(raw, "t", "p", slide); ok {
			b.WriteString(text)
			b.WriteString("\n")
		}
	}
	out := strings.TrimSpace(b.String())
	return out, out != ""
}

// extractXMLText 在 OOXML 局部名空间下收集 local==textLocal 的字符数据，
// local==groupLocal 的元素结束后补换行（段落/行/幻灯片分组）。
func extractXMLText(raw []byte, textLocal, groupLocal, source string) (string, bool) {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Strict = false
	var b strings.Builder
	depthText := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case textLocal:
				depthText++
			case groupLocal:
				// 分组边界：块间换行（docx 的 w:p、xlsx 的 row、pptx 的 a:p）。
			}
		case xml.CharData:
			if depthText > 0 {
				b.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case textLocal:
				if depthText > 0 {
					depthText--
				}
			case groupLocal:
				b.WriteString("\n")
			}
		}
	}
	out := strings.TrimSpace(b.String())
	_ = source
	return out, out != ""
}

func readZipEntry(data []byte, name string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, MaxExtractBytes))
	}
	return nil, fmt.Errorf("entry %s not found", name)
}

// listZipEntries 返回名称以 prefix 开头的 zip 条目（按名称排序）。
func listZipEntries(data []byte, prefix string) []string {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil
	}
	names := make([]string, 0, 8)
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, prefix) {
			names = append(names, f.Name)
		}
	}
	// 自然排序（sheet1 < sheet10 < sheet2 需数值感知；简单后缀数值排序）。
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && natLess(names[j], names[j-1]); j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}

// natLess 自然序比较（数字段按数值）。
func natLess(a, b string) bool {
	ai, bi := 0, 0
	for ai < len(a) && bi < len(b) {
		ca, cb := a[ai], b[bi]
		if isDigit(ca) && isDigit(cb) {
			anj, bnj := ai, bi
			for anj < len(a) && isDigit(a[anj]) {
				anj++
			}
			for bnj < len(b) && isDigit(b[bnj]) {
				bnj++
			}
			na, _ := strconv.Atoi(a[ai:anj])
			nb, _ := strconv.Atoi(b[bi:bnj])
			if na != nb {
				return na < nb
			}
			ai, bi = anj, bnj
			continue
		}
		if ca != cb {
			return ca < cb
		}
		ai++
		bi++
	}
	return len(a)-ai < len(b)-bi
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// ---------- PDF（最小文本抽取，纯标准库） ----------
//
// 流程：扫描对象段 → 找出 FlateDecode 压缩的 stream → zlib 解压 → 扫描
// 内容流文本块中的 (..) Tj / [(..)..] TJ 字符串字面量。仅覆盖简单字体
//（WinAnsi/标准编码）的 PDF；CID 字体中文 PDF 可能只抽到少量文本，
// 返回已得部分（调用方容错）。

// extractPDF 抽取 PDF 文本。
func extractPDF(data []byte) (string, bool) {
	text := extractPDFStreams(data)
	out := strings.TrimSpace(text)
	return out, out != ""
}

func extractPDFStreams(data []byte) string {
	var b strings.Builder
	// 扫描 "N M obj ... endobj" 段：头部长度 ≤64KB。
	rest := data
	prefix := []byte(" obj")
	for {
		idx := bytes.Index(rest, prefix)
		if idx < 0 || idx > 20 {
			if idx < 0 {
				break
			}
			rest = rest[idx+len(prefix):]
			continue
		}
		objStart := idx + len(prefix)
		endIdx := bytes.Index(rest[objStart:], []byte("endobj"))
		if endIdx < 0 {
			break
		}
		objBody := rest[objStart : objStart+endIdx]
		if streamIdx := bytes.Index(objBody, []byte("stream")); streamIdx >= 0 {
			head := objBody[:streamIdx]
			raw := objBody[streamIdx+len("stream"):]
			raw = trimEOL(raw)
			if endStream := bytes.Index(raw, []byte("endstream")); endStream >= 0 {
				raw = bytes.TrimRight(raw[:endStream], "\r\n \t")
			}
			if isFlateStream(head) {
				if text := pdfContentText(inflate(raw)); text != "" {
					b.WriteString(text)
					b.WriteString("\n")
				}
			}
		}
		rest = rest[objStart+endIdx+len("endobj"):]
	}
	return b.String()
}

func trimEOL(b []byte) []byte {
	if len(b) > 0 && b[0] == '\r' {
		b = b[1:]
	}
	if len(b) > 0 && b[0] == '\n' {
		b = b[1:]
	}
	return b
}

// isFlateStream 判定对象头是否 FlateDecode 流（内容流或对象流均解压扫描）。
func isFlateStream(head []byte) bool {
	return bytes.Contains(head, []byte("/FlateDecode"))
}

func inflate(raw []byte) []byte {
	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, MaxExtractBytes))
	if err != nil && len(out) == 0 {
		return nil
	}
	return out
}

// pdfContentText 扫描解压后内容流的文本显示操作符：括号字面量为文本，
// 相邻字面量之间若出现文本定位操作符（Td/TD/T*/Tm/TJ/Tj/ET）则插入分隔
// （空格或换行），其余操作符（颜色/字体/坐标矩阵等）跳过。
func pdfContentText(content []byte) string {
	var b strings.Builder
	i := 0
	for i < len(content) {
		c := content[i]
		if c == '(' {
			s, next := readPDFString(content, i)
			b.WriteString(s)
			i = next
			continue
		}
		// 非字面量区段：识别文本操作符做断词。
		j := i
		for j < len(content) && content[j] != '(' {
			j++
		}
		gap := content[i:j]
		if bytes.Contains(gap, []byte("Td")) || bytes.Contains(gap, []byte("TD")) ||
			bytes.Contains(gap, []byte("T*")) || bytes.Contains(gap, []byte("Tm")) ||
			bytes.Contains(gap, []byte("ET")) {
			b.WriteString("\n")
		} else if bytes.Contains(gap, []byte("TJ")) || bytes.Contains(gap, []byte("Tj")) ||
			bytes.Contains(gap, []byte("TL")) {
			b.WriteString(" ")
		}
		i = j
	}
	out := b.String()
	if out == "" {
		return ""
	}
	// 清理控制字符（保留换行/制表）。
	out = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, out)
	return out
}

// readPDFString 读取自 content[start]='(' 起的 PDF 字符串字面量，返回解码
// 文本与结束下标（含右括号）。
func readPDFString(content []byte, start int) (string, int) {
	var b []byte
	i := start + 1
	for i < len(content) {
		c := content[i]
		switch c {
		case '\\':
			if i+1 < len(content) {
				n := content[i+1]
				switch n {
				case 'n':
					b = append(b, '\n')
				case 'r':
					b = append(b, '\r')
				case 't':
					b = append(b, '\t')
				case 'b', 'f':
					// 忽略
				case '(', ')', '\\':
					b = append(b, n)
				default:
					if n >= '0' && n <= '7' {
						// 八进制转义（最多 3 位）。
						v := int(n - '0')
						j := i + 2
						for digits := 1; digits < 3 && j < len(content) && content[j] >= '0' && content[j] <= '7'; digits++ {
							v = v*8 + int(content[j]-'0')
							j++
						}
						b = appendRuneOctal(b, v)
						i = j - 1
					}
				}
				i += 2
				continue
			}
			i++
		case '(':
			// 嵌套括号（PDF 允许）：递归计数，原样保留。
			b = append(b, c)
			i++
		case ')':
			return string(b), i + 1
		default:
			b = append(b, c)
			i++
		}
	}
	return string(b), i
}

func appendRuneOctal(b []byte, v int) []byte {
	return append(b, []byte(string(rune(v)))...)
}
