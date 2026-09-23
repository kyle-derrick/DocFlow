// Package ai —— platformtools.go：内置平台文件工具集（df_*）声明。
//
// 用户架构决策：对齐 MCP 官方 filesystem server 范式，为平台内 AI 对话
// 提供直调 service 层的文件工具（不经 MCP HTTP）——/ai/chat 开启 use_files
// 后由 HTTP 层注入本声明集与执行器回调（见 internal/http/ai_platform_tools.go），
// chat.go 把 df_* 与外部 MCP 工具合并进同一条工具循环。
//
// 安全边界（刻意设计，不提供 delete/move）：
//   - 工具仅能读写当前用户有权限的空间文件（service 层既有授权链复用）；
//   - df_write_file 限文本类扩展名白名单（拒绝 exe/bin 等二进制）且 ≤2MB，
//     覆盖既有文件走「上传 file_id 覆盖新版本」链路（版本链自动保护，
//     旧版本可随时回滚）；
//   - 不提供删除/移动工具——避免模型误删用户数据（不可逆操作交还用户）。
package ai

import (
	"strconv"
	"strings"
)

// 内置工具名常量（df_ 前缀与 MCP 工具的 mcp_ 前缀天然区分）。
const (
	PlatformToolListDir   = "df_list_dir"   // 列目录
	PlatformToolReadFile  = "df_read_file"  // 读文件文本
	PlatformToolWriteFile = "df_write_file" // 创建/覆盖文件
	PlatformToolMkdir     = "df_mkdir"      // 建目录（多级）
	PlatformToolSearch    = "df_search"     // 平台全文搜索
)

// 工具行为边界常量（执行器与声明共同引用）。
const (
	// PlatformToolListLimit df_list_dir 返回条目上限（超出截断并标注）。
	PlatformToolListLimit = 50
	// PlatformToolReadMaxRunes df_read_file 返回内容的字符上限（rune 计）。
	PlatformToolReadMaxRunes = 24000
	// PlatformToolWriteMaxBytes df_write_file 单文件内容上限（2MB）。
	PlatformToolWriteMaxBytes = 2 << 20
	// PlatformToolSearchLimit df_search 返回条目上限。
	PlatformToolSearchLimit = 8
)

// PlatformToolWriteExts df_write_file 允许的扩展名白名单（小写，不含点；
// 维护点：调整文本类支持范围只改这里）。仅文本/结构化文本/图表源码格式；
// 二进制扩展（exe/bin/png/docx 等）一律拒绝——AI 生成的二进制无意义且
// 存在注入风险，office/图片编辑走平台既有编辑器。
var PlatformToolWriteExts = []string{
	// 文档与纯文本
	"md", "markdown", "txt", "text", "dfdoc", "rst", "adoc",
	// 结构化数据
	"json", "xml", "yaml", "yml", "toml", "ini", "cfg", "conf", "env", "properties", "csv", "tsv",
	// Web
	"html", "htm", "css", "scss", "less", "js", "mjs", "cjs", "jsx", "ts", "tsx",
	// 源码
	"py", "go", "java", "kt", "kts", "rs", "c", "h", "cpp", "hpp", "cs", "php", "rb", "swift", "scala", "dart", "lua", "perl", "r", "sql", "graphql", "proto",
	// 脚本
	"sh", "bash", "zsh", "fish", "ps1", "bat",
	// 图表源码（文本格式）
	"svg", "mermaid", "drawio", "excalidraw", "plantuml",
}

// PlatformToolWriteExtAllowed 判定文件名扩展是否在 df_write_file 白名单内
// （大小写不敏感；无扩展名/空扩展名返回 false——强制显式文本类型）。
func PlatformToolWriteExtAllowed(name string) bool {
	dot := strings.LastIndex(name, ".")
	if dot < 0 || dot == len(name)-1 {
		return false
	}
	ext := strings.ToLower(name[dot+1:])
	for _, e := range PlatformToolWriteExts {
		if e == ext {
			return true
		}
	}
	return false
}

// PlatformTool 为一个内置工具的声明（openai/anthropic 双协议共用：
// openai 取 function.name/description/parameters，anthropic 取
// name/description/input_schema——Params 即 JSON Schema 的 parameters 对象）。
type PlatformTool struct {
	Name        string
	Description string
	// Params 为 openai 兼容 JSONSchema 对象（{"type":"object",...}）。
	Params map[string]any
}

// platformToolsAll 内置工具声明全集（声明即文档：描述面向模型，说明
// 相对路径基于工作目录、返回形态与截断边界）。
var platformToolsAll = []PlatformTool{
	{
		Name:        PlatformToolListDir,
		Description: "列出平台空间中指定目录的直属子项（文件与子目录）。path 为相对当前工作目录的路径（如 \"docs/笔记\"），留空表示工作目录本身。返回条目数组（名称/类型/大小/更新时间，目录 size 为 0），最多 " + itoa(PlatformToolListLimit) + " 条，超出截断并标注 truncated。",
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "相对工作目录的目录路径，可省略（默认工作目录）",
				},
			},
		},
	},
	{
		Name:        PlatformToolReadFile,
		Description: "读取平台空间中一个文件的文本内容（PDF/docx/xlsx/pptx/drawio/excalidraw/dfdoc 等自动抽取为纯文本；纯二进制不支持）。path 为相对当前工作目录的文件路径。内容截断至 " + itoa(PlatformToolReadMaxRunes) + " 字符并标注 truncated，需要更多内容时请分段请求。",
		Params: map[string]any{
			"type":     "object",
			"required": []string{"path"},
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "相对工作目录的文件路径（必填）",
				},
			},
		},
	},
	{
		Name:        PlatformToolWriteFile,
		Description: "在平台空间创建文件或用新内容覆盖既有文本文件（覆盖自动保留历史版本，可在版本历史回滚）。path 为相对工作目录的目标路径（父目录须已存在，可用 df_mkdir 先建目录），content 为完整文件内容（UTF-8 文本）。限制：仅允许文本类扩展名（md/txt/代码/json/yaml/html/svg/drawio 等），内容 ≤2MB。返回创建的文件 ID 与是否为覆盖。",
		Params: map[string]any{
			"type":     "object",
			"required": []string{"path", "content"},
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "相对工作目录的目标文件路径（必填，含扩展名）",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "完整文件内容（必填，UTF-8 文本）",
				},
			},
		},
	},
	{
		Name:        PlatformToolMkdir,
		Description: "在平台空间创建目录（支持多级路径，如 \"项目/文档/草稿\"；已存在的中间目录自动跳过，同名文件存在时报错）。",
		Params: map[string]any{
			"type":     "object",
			"required": []string{"path"},
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "相对工作目录的目录路径（必填，多级以 / 分隔）",
				},
			},
		},
	},
	{
		Name:        PlatformToolSearch,
		Description: "在平台全文检索（覆盖当前用户可见空间的文件名与已建索引的文本内容）。返回最相关的 " + itoa(PlatformToolSearchLimit) + " 条结果（名称/类型/命中片段 snippet）。用于不知道文件路径时定位文件，找到后可用 df_read_file 读取。",
		Params: map[string]any{
			"type":     "object",
			"required": []string{"query"},
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "检索关键词（必填）",
				},
			},
		},
	},
}

// itoa 拼接工具描述中的数值边界（strconv.Itoa 的短名别名）。
func itoa(n int) string { return strconv.Itoa(n) }

// PlatformTools 返回内置平台文件工具声明（openai 格式 tools 数组的数据源；
// HTTP 层 use_files=true 时注入 ChatRequest.PlatformTools）。
func PlatformTools() []PlatformTool {
	out := make([]PlatformTool, len(platformToolsAll))
	copy(out, platformToolsAll)
	return out
}

// PlatformToolNames 返回内置工具名集合（工具执行分发时先查此表命中内置
// 工具，未命中回落 MCP 工具路径）。
func PlatformToolNames() map[string]bool {
	names := make(map[string]bool, len(platformToolsAll))
	for _, t := range platformToolsAll {
		names[t.Name] = true
	}
	return names
}
