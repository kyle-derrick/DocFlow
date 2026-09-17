// Package xmind 解析 .xmind 思维导图文件（zip 容器，新格式 content.json）
// 并转换为 Markdown：每个 sheet 输出一个 `# 标题`，topic 树转嵌套 `- ` 列表，
// notes 转缩进 `> ` 引用。老版 content.xml 格式明确不支持。
package xmind

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

var (
	// ErrInvalid 表示内容不是合法的 .xmind 文件（非 zip、缺 content.json、JSON 解析失败）。
	ErrInvalid = errors.New("invalid xmind file")
	// ErrLegacyUnsupported 表示老版 XMind 格式（content.xml，XMind 8 及以前）。
	ErrLegacyUnsupported = errors.New("legacy xmind format (content.xml) is not supported")
)

// topic 为 content.json 的 topic 节点：标题、备注与子主题（attached 挂靠分支）。
type topic struct {
	Title    string `json:"title"`
	Notes    *notes `json:"notes"`
	Children *struct {
		Attached []topic `json:"attached"`
	} `json:"children"`
}

// notes 为 topic 备注：新格式为 notes.plain.content 纯文本。
type notes struct {
	Plain *struct {
		Content string `json:"content"`
	} `json:"plain"`
	Content string `json:"content"`
}

func (n *notes) text() string {
	if n == nil {
		return ""
	}
	if n.Plain != nil {
		return strings.TrimSpace(n.Plain.Content)
	}
	return strings.TrimSpace(n.Content)
}

// sheet 为 content.json 数组的元素：画布标题 + 根 topic。
type sheet struct {
	Title     string `json:"title"`
	RootTopic topic  `json:"rootTopic"`
}

// maxContentSize 为单次解析的 content.json 解压上限（防御异常超大条目）。
const maxContentSize = 64 << 20

// ToMarkdown 解析 .xmind 数据并生成 Markdown 文本：
//   - 每个 sheet 输出 `# <sheet标题>`（缺省回退根 topic 标题）；
//   - 根 topic 作为 sheet 标题下的首层 `- ` 列表项，子 topic 逐层缩进两级空格；
//   - topic 的 notes 以同层 +2 缩进的 `> ` 引用行输出（多行逐行引用）。
func ToMarkdown(data []byte) (string, error) {
	sheets, err := parseSheets(data)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for i, sh := range sheets {
		if i > 0 {
			b.WriteString("\n")
		}
		title := strings.TrimSpace(sh.Title)
		if title == "" {
			title = strings.TrimSpace(sh.RootTopic.Title)
		}
		if title == "" {
			title = fmt.Sprintf("Sheet %d", i+1)
		}
		b.WriteString("# ")
		b.WriteString(title)
		b.WriteString("\n\n")
		writeTopic(&b, sh.RootTopic, 0)
	}
	return strings.TrimRight(b.String(), "\n") + "\n", nil
}

// parseSheets 打开 zip 容器并解析 content.json（新格式）；
// 老版 content.xml 返回 ErrLegacyUnsupported，其余结构问题返回 ErrInvalid。
func parseSheets(data []byte) ([]sheet, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, ErrInvalid
	}
	var content []byte
	hasLegacy := false
	for _, fh := range zr.File {
		switch fh.Name {
		case "content.json":
			rc, oerr := fh.Open()
			if oerr != nil {
				return nil, ErrInvalid
			}
			content, err = io.ReadAll(io.LimitReader(rc, maxContentSize+1))
			rc.Close()
			if err != nil || int64(len(content)) > maxContentSize {
				return nil, ErrInvalid
			}
		case "content.xml":
			hasLegacy = true
		}
	}
	if content == nil {
		if hasLegacy {
			return nil, ErrLegacyUnsupported
		}
		return nil, ErrInvalid
	}
	var sheets []sheet
	if err := json.Unmarshal(content, &sheets); err != nil {
		return nil, ErrInvalid
	}
	if len(sheets) == 0 {
		return nil, ErrInvalid
	}
	return sheets, nil
}

// writeTopic 递归输出 topic：列表项（indent 级，每级 2 空格）、备注引用与子主题。
func writeTopic(b *strings.Builder, t topic, level int) {
	indent := strings.Repeat("  ", level)
	title := strings.TrimSpace(t.Title)
	noteText := t.Notes.text()
	// 标题非空（或无备注内容）时输出列表项；空标题仅有备注时只输出引用块。
	if title != "" || noteText == "" {
		b.WriteString(indent)
		b.WriteString("- ")
		b.WriteString(title)
		b.WriteString("\n")
	}
	if noteText != "" {
		for _, line := range strings.Split(noteText, "\n") {
			b.WriteString(indent + "  > ")
			b.WriteString(strings.TrimSpace(line))
			b.WriteString("\n")
		}
	}
	if t.Children != nil {
		for _, child := range t.Children.Attached {
			writeTopic(b, child, level+1)
		}
	}
}
