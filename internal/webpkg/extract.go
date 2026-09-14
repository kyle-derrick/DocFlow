package webpkg

import (
	"archive/zip"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
)

// Stats 为一次成功解包的统计结果。
type Stats struct {
	// EntryCount 实际写入的文件条目数（目录条目不落盘）。
	EntryCount int
	// TotalSize 展开后的总字节数。
	TotalSize int64
}

// entry 是校验通过后的条目视图（clean 为清理后的相对路径）。
type entry struct {
	fh    *zip.File
	clean string
	isDir bool
}

// Extract 将 zip 流 r 解包到 storage 的 destPrefix 下（key 形如
// <destPrefix>/<sanitized relative path>），成功时在末尾写入内部清单
// （manifestName）记录全部条目路径。任何安全校验失败返回 ErrWebpkgInvalid
// 包装的错误，并尽力清理已写入的 key。
//
// 校验规则（全部先校验后写入，避免半写状态）：
//   - 非 zip 内容、归档/条目注释与文件名控制字符；
//   - Zip Slip（../、绝对路径、盘符、反斜杠、空段）与保留设备名；
//   - 符号链接与其它非常规文件类型条目（硬链接在 zip 中无对应类型，
//     非常规 ModeType 一并拒绝）；
//   - 条目数、目录深度、单文件大小（声明值 + 实际拷贝双保险）、展开总大小；
//   - 必须包含 index.html（根或唯一顶层目录内，后者自动剥去该目录）。
//
// 压缩比异常（compressed>0 且 uncompressed:compressed > 100:1）仅记日志
// 不拒绝——展开总量已被 MaxTotalSize/MaxFileSize 硬限制覆盖。
func Extract(storage Storage, r io.Reader, destPrefix string, limits Limits) (Stats, error) {
	limits = limits.withDefaults()
	entries, closeArchive, err := readArchive(r, limits)
	if err != nil {
		return Stats{}, err
	}
	defer closeArchive()
	files, strip, err := validateArchive(entries, limits)
	if err != nil {
		return Stats{}, err
	}

	var total int64
	written := make([]string, 0, len(files))
	for _, e := range files {
		rel := strings.TrimPrefix(e.clean, strip)
		key := destPrefix + "/" + rel
		if err := copyEntry(storage, e.fh, key, limits, &total); err != nil {
			// 失败条目可能已部分写入（storage.Put 中途出错）：先删当前 key，
			// 再清理此前已完整写出的 key。
			_ = storage.Delete(key)
			cleanupKeys(storage, destPrefix, written)
			return Stats{}, err
		}
		written = append(written, rel)
	}
	if err := writeManifest(storage, destPrefix, written); err != nil {
		cleanupKeys(storage, destPrefix, written)
		return Stats{}, fmt.Errorf("write manifest: %w", err)
	}
	return Stats{EntryCount: len(written), TotalSize: total}, nil
}

// Remove 删除 destPrefix 下曾写入的全部条目（清单驱动），用于幂等重建时
// 先删旧 key。无清单（从未成功解包）时为空操作；失败静默（best-effort）。
func Remove(storage Storage, destPrefix string) {
	r, err := storage.Read(destPrefix + "/" + manifestName)
	if err != nil {
		return
	}
	data, err := io.ReadAll(io.LimitReader(r, 16<<20))
	// Windows 下文件句柄未关闭前无法删除，先显式关闭清单 reader。
	r.Close()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		_ = storage.Delete(destPrefix + "/" + line)
	}
	_ = storage.Delete(destPrefix + "/" + manifestName)
}

// readArchive 将 zip 流落盘暂存（zip.NewReader 需要 ReaderAt；暂存上限为
// MaxTotalSize+MaxFileSize，超出直接拒绝）并完成逐条目校验。
// 返回的 closeArchive 释放暂存文件——zip 条目内容为懒加载，调用方必须在
// 全部条目写出完毕后才可调用。
func readArchive(r io.Reader, limits Limits) ([]entry, func(), error) {
	tmp, err := os.CreateTemp("", "webpkg-*.zip")
	if err != nil {
		return nil, nil, fmt.Errorf("spool archive: %w", err)
	}
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}

	spoolLimit := limits.MaxTotalSize + limits.MaxFileSize
	n, err := io.Copy(tmp, io.LimitReader(r, spoolLimit+1))
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("read archive: %w", err)
	}
	if n > spoolLimit {
		cleanup()
		return nil, nil, fmt.Errorf("%w: archive exceeds size limit", ErrWebpkgInvalid)
	}
	zr, err := zip.NewReader(tmp, n)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("%w: not a valid zip archive", ErrWebpkgInvalid)
	}
	if hasControlCharacter(zr.Comment) {
		cleanup()
		return nil, nil, fmt.Errorf("%w: control character in archive comment", ErrWebpkgInvalid)
	}
	if len(zr.File) > limits.MaxEntries {
		cleanup()
		return nil, nil, fmt.Errorf("%w: %d entries exceed limit %d", ErrWebpkgInvalid, len(zr.File), limits.MaxEntries)
	}

	entries := make([]entry, 0, len(zr.File))
	for _, fh := range zr.File {
		e, err := validateEntry(fh, limits)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		entries = append(entries, e)
	}
	return entries, cleanup, nil
}

// validateEntry 校验单个条目（文件名、类型、元数据与声明大小）。
func validateEntry(fh *zip.File, limits Limits) (entry, error) {
	if hasControlCharacter(fh.Comment) {
		return entry{}, fmt.Errorf("%w: control character in entry comment %q", ErrWebpkgInvalid, fh.Name)
	}
	isDir := fh.FileInfo().IsDir() || strings.HasSuffix(fh.Name, "/")
	mode := fh.Mode()
	if !isDir && mode&os.ModeType != 0 {
		// 符号链接、设备、命名管道、套接字等一律拒绝（zip 无硬链接类型）。
		return entry{}, fmt.Errorf("%w: non-regular entry %q (mode %s)", ErrWebpkgInvalid, fh.Name, mode)
	}
	clean, sanitizedDir, err := SanitizePath(fh.Name, limits.MaxDepth)
	if err != nil {
		return entry{}, err
	}
	if sanitizedDir && clean == "" {
		return entry{fh: fh, clean: "", isDir: true}, nil // 根目录条目
	}
	if !sanitizedDir && fh.UncompressedSize64 > uint64(limits.MaxFileSize) {
		return entry{}, fmt.Errorf("%w: entry %q declared size %d exceeds limit %d", ErrWebpkgInvalid, fh.Name, fh.UncompressedSize64, limits.MaxFileSize)
	}
	return entry{fh: fh, clean: clean, isDir: sanitizedDir}, nil
}

// validateArchive 完成跨条目校验：声明总量、入口识别与唯一顶层目录剥除。
// 返回待写入的文件条目与剥除前缀 strip（"" 表示不剥除）。
func validateArchive(entries []entry, limits Limits) (files []entry, strip string, err error) {
	var declaredTotal uint64
	fileSet := make(map[string]bool, len(entries))
	dirSet := make(map[string]bool)
	for _, e := range entries {
		if e.isDir {
			if e.clean != "" {
				dirSet[e.clean] = true
			}
			continue
		}
		if fileSet[e.clean] {
			return nil, "", fmt.Errorf("%w: duplicate entry %q", ErrWebpkgInvalid, e.clean)
		}
		fileSet[e.clean] = true
		files = append(files, e)
		declaredTotal += e.fh.UncompressedSize64
		if declaredTotal > uint64(limits.MaxTotalSize) {
			return nil, "", fmt.Errorf("%w: declared total size exceeds limit %d", ErrWebpkgInvalid, limits.MaxTotalSize)
		}
	}
	if len(files) == 0 {
		return nil, "", fmt.Errorf("%w: archive contains no files", ErrWebpkgInvalid)
	}

	// 入口识别：根 index.html 优先；否则全部条目位于唯一顶层目录内时剥除该目录。
	if !fileSet["index.html"] {
		top := make(map[string]bool)
		for _, e := range entries {
			if e.clean == "" {
				continue
			}
			top[firstSegment(e.clean)] = true
		}
		if len(top) == 1 {
			sole := ""
			for name := range top {
				sole = name
			}
			if sole != "" && (dirSet[sole] || containsNested(entries, sole+"/")) {
				strip = sole + "/"
			}
		}
	}
	if !fileSet[strip+"index.html"] {
		return nil, "", fmt.Errorf("%w: missing index.html at package root", ErrWebpkgInvalid)
	}
	return files, strip, nil
}

func firstSegment(p string) string {
	if idx := strings.IndexByte(p, '/'); idx >= 0 {
		return p[:idx]
	}
	return p
}

// containsNested 报告是否存在以 prefix 为前缀的更深条目。
func containsNested(entries []entry, prefix string) bool {
	for _, e := range entries {
		if e.clean != "" && strings.HasPrefix(e.clean, prefix) {
			return true
		}
	}
	return false
}

// copyEntry 将单个条目流式写入 storage；声明大小可能撒谎，实际拷贝经
// limitCountReader 双保险（单文件与累计总量）。高压缩比仅记日志。
func copyEntry(storage Storage, fh *zip.File, key string, limits Limits, total *int64) error {
	if fh.CompressedSize64 > 0 && fh.UncompressedSize64 > 100*fh.CompressedSize64 {
		log.Printf("[webpkg] high compression ratio entry %q: %d -> %d bytes", fh.Name, fh.CompressedSize64, fh.UncompressedSize64)
	}
	rc, err := fh.Open()
	if err != nil {
		return fmt.Errorf("open entry %q: %w", fh.Name, err)
	}
	defer rc.Close()
	lr := &limitCountReader{r: rc, fileLimit: limits.MaxFileSize, total: total, totalLimit: limits.MaxTotalSize}
	return storage.Put(key, lr)
}

// limitCountReader 在读取时强制单文件与累计总量上限（超限返回 ErrWebpkgInvalid，
// 由 storage.Put 的 io.Copy 向上传播）；截断读取窗口保证恰好在越界处终止。
type limitCountReader struct {
	r          io.Reader
	fileLimit  int64
	fileRead   int64
	total      *int64
	totalLimit int64
}

func (l *limitCountReader) Read(p []byte) (int, error) {
	if l.fileRead >= l.fileLimit {
		return 0, fmt.Errorf("%w: extracted file exceeds max size %d", ErrWebpkgInvalid, l.fileLimit)
	}
	if *l.total >= l.totalLimit {
		return 0, fmt.Errorf("%w: total extracted size exceeds limit %d", ErrWebpkgInvalid, l.totalLimit)
	}
	if int64(len(p)) > l.fileLimit-l.fileRead {
		p = p[:l.fileLimit-l.fileRead]
	}
	if int64(len(p)) > l.totalLimit-*l.total {
		p = p[:l.totalLimit-*l.total]
	}
	n, err := l.r.Read(p)
	l.fileRead += int64(n)
	*l.total += int64(n)
	return n, err
}

// writeManifest 写入内部清单（条目相对路径逐行）。
func writeManifest(storage Storage, destPrefix string, written []string) error {
	var b strings.Builder
	for _, rel := range written {
		b.WriteString(rel)
		b.WriteByte('\n')
	}
	return storage.Put(destPrefix+"/"+manifestName, strings.NewReader(b.String()))
}

// cleanupKeys 尽力删除已写入的 key（失败静默，重建路径的 Remove 兜底）。
func cleanupKeys(storage Storage, destPrefix string, written []string) {
	for _, rel := range written {
		_ = storage.Delete(destPrefix + "/" + rel)
	}
}
