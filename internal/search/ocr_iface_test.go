// Package search_test —— 外部测试包：仅放跨包编译期断言（ai 依赖 search，
// 反向断言须放 _test 外部包避免测试期循环导入）。
package search_test

import (
	"github.com/docflow/docflow/internal/ai"
	"github.com/docflow/docflow/internal/search"
)

// *ai.Service 实现 search.OCRExtractor（main.go 经 Indexer.SetOCR 接线，
// 此断言保证后续签名演化不破坏装配）。
var _ search.OCRExtractor = (*ai.Service)(nil)
