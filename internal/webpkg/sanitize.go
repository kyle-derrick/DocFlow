package webpkg

import (
	"fmt"
	"strings"
)

// reservedNames 与 files.NormalizeName 的 Windows 保留设备名一致，
// 防止解包条目在 Windows 存储后端落盘异常。
var reservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

func isASCIILetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// hasControlCharacter 报告 s 是否含 C0 控制字符或 DEL。
func hasControlCharacter(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

// SanitizePath 将 zip 条目名 / /content 相对路径清理为安全的相对路径。
// 纯函数，解包（Extract）与取内容（Resolve）共用同一套规则。
//
// 拒绝（返回 ErrWebpkgInvalid 包装的原因）：
//   - 空路径、控制字符、反斜杠（跨平台分隔符混淆）；
//   - 绝对路径（前导 '/'）与盘符（"C:..."）；
//   - 空路径段（"a//b"）、"." 与 ".." 段（Zip Slip）；
//   - Windows 保留设备名段、以点/空格结尾的段；
//   - 目录深度超过 maxDepth。
//
// 目录条目（尾随 '/'）返回去掉尾斜杠的路径与 isDir=true；
// 根目录条目 "/" 返回空路径与 isDir=true。
func SanitizePath(name string, maxDepth int) (path string, isDir bool, err error) {
	invalid := func(format string, args ...any) (string, bool, error) {
		return "", false, fmt.Errorf("%w: %s", ErrWebpkgInvalid, fmt.Sprintf(format, args...))
	}
	if name == "" {
		return invalid("empty path")
	}
	if hasControlCharacter(name) {
		return invalid("control character in path %q", name)
	}
	if strings.ContainsRune(name, '\\') {
		return invalid("backslash in path %q", name)
	}
	if strings.HasPrefix(name, "/") {
		if name == "/" {
			// 根目录条目 "/"。
			return "", true, nil
		}
		return invalid("absolute path %q", name)
	}
	isDir = strings.HasSuffix(name, "/")
	trimmed := strings.TrimSuffix(name, "/")
	if trimmed == "" {
		return invalid("empty path %q", name)
	}
	if len(trimmed) >= 2 && trimmed[1] == ':' && isASCIILetter(trimmed[0]) {
		return invalid("drive-letter path %q", name)
	}
	segments := strings.Split(trimmed, "/")
	for _, seg := range segments {
		switch {
		case seg == "":
			return invalid("empty path segment in %q", name)
		case seg == "." || seg == "..":
			return invalid("traversal segment in %q", name)
		}
		base := strings.ToUpper(strings.SplitN(seg, ".", 2)[0])
		if reservedNames[base] {
			return invalid("reserved device name segment %q", seg)
		}
		if strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") {
			return invalid("segment %q ends with dot or space", seg)
		}
	}
	if len(segments)-1 > maxDepth {
		return invalid("directory depth %d exceeds limit %d", len(segments)-1, maxDepth)
	}
	return trimmed, isDir, nil
}

// ResolvePath 对 /content 相对路径做清理 + 扩展名白名单判定，
// 返回清理后的路径与推断的 Content-Type；任何不合规（含目录路径）均 ok=false。
// 与 SanitizePath 复用同一套穿越/控制字符/深度防护。
func ResolvePath(rel string, maxDepth int) (path, contentType string, ok bool) {
	rel = strings.TrimPrefix(rel, "/")
	clean, isDir, err := SanitizePath(rel, maxDepth)
	if err != nil || isDir || clean == "" || clean == manifestName {
		return "", "", false
	}
	dot := strings.LastIndexByte(clean, '.')
	if dot < 0 {
		return "", "", false
	}
	ct, allowed := contentTypes[strings.ToLower(clean[dot:])]
	if !allowed {
		return "", "", false
	}
	return clean, ct, true
}
