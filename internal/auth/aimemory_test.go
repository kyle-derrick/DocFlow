// aimemory_test.go：AI 记忆载荷校验测试（ai_memory 结构，migration 050）。
package auth

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestValidateAIMemory 校验分支：kind 白名单、content 非空与 ≤2000 字符。
func TestValidateAIMemory(t *testing.T) {
	if err := ValidateAIMemory(AIMemoryKindManual, "偏好简洁中文回答"); err != nil {
		t.Fatalf("manual 合法: %v", err)
	}
	if err := ValidateAIMemory(AIMemoryKindAuto, "偏好简洁中文回答"); err != nil {
		t.Fatalf("auto 为预留合法值（API 写入侧另行拒绝）: %v", err)
	}
	// kind 空串 / 非法值（缺省归一在 CreateAIMemory 层完成，此处直接报错）。
	if err := ValidateAIMemory("", "内容"); err == nil || !errors.Is(err, ErrInvalidAIMemory) {
		t.Fatalf("空 kind 应报错: %v", err)
	}
	if err := ValidateAIMemory("other", "内容"); err == nil || !errors.Is(err, ErrInvalidAIMemory) {
		t.Fatalf("非法 kind 应报错: %v", err)
	}
	// content 空 / 纯空白。
	if err := ValidateAIMemory(AIMemoryKindManual, ""); err == nil || !errors.Is(err, ErrInvalidAIMemory) {
		t.Fatalf("空 content 应报错: %v", err)
	}
	if err := ValidateAIMemory(AIMemoryKindManual, "   \n\t "); err == nil || !errors.Is(err, ErrInvalidAIMemory) {
		t.Fatalf("纯空白 content 应报错: %v", err)
	}
	// content 超长（rune 计，中文按字符）。
	if err := ValidateAIMemory(AIMemoryKindManual, strings.Repeat("记", AIMemoryMaxContentRunes)); err != nil {
		t.Fatalf("恰 2000 字符应通过: %v", err)
	}
	if err := ValidateAIMemory(AIMemoryKindManual, strings.Repeat("记", AIMemoryMaxContentRunes+1)); err == nil || !errors.Is(err, ErrInvalidAIMemory) {
		t.Fatalf("超 2000 字符应报错: %v", err)
	}
}

// TestUpdateAIMemoryValidation 编辑校验：content 空/超长直接以 400 语义
// 拒绝且 found=false、不触库（校验先于 DB 访问，UserStore 零值即可覆盖该
// 分支）。更新成功 / 不存在返回 false 依赖真实 gorm 存储——本包测试设施
// 全为纯逻辑与内存 fake（无 sqlite/测试库依赖），这两个 DB 分支经
// internal/http 的 fakeAIMemoryStore PUT 用例覆盖（对应 200/404 映射）。
func TestUpdateAIMemoryValidation(t *testing.T) {
	s := &UserStore{}
	if _, found, err := s.UpdateAIMemory(uuid.New(), uuid.New(), strings.Repeat("记", AIMemoryMaxContentRunes+1)); found || err == nil || !errors.Is(err, ErrInvalidAIMemory) {
		t.Fatalf("超长 content 应拒绝且 found=false: found=%v err=%v", found, err)
	}
	if _, found, err := s.UpdateAIMemory(uuid.New(), uuid.New(), "   \n\t "); found || err == nil || !errors.Is(err, ErrInvalidAIMemory) {
		t.Fatalf("纯空白 content 应拒绝且 found=%v err=%v", found, err)
	}
}

// TestCreateAutoAIMemoryValidation 自动写入的校验保留：content 空/纯空白/
// 超长在触库之前以 ErrInvalidAIMemory 拒绝（校验先于 DB 访问，UserStore
// 零值即可覆盖）。成功落库（kind=auto、去空白入库）与 TrimAutoAIMemory
// 的裁剪语义依赖真实 gorm 存储——本包无 DB 测试设施，经 internal/http 的
// fakeAutoMemoryStore 用例镜像覆盖（模式同 TestUpdateAIMemoryValidation）。
func TestCreateAutoAIMemoryValidation(t *testing.T) {
	s := &UserStore{}
	user := uuid.New()
	for name, content := range map[string]string{
		"空":   "",
		"纯空白": "   \n\t ",
		"超长":  strings.Repeat("记", AIMemoryMaxContentRunes+1),
	} {
		if _, err := s.CreateAutoAIMemory(user, content); err == nil || !errors.Is(err, ErrInvalidAIMemory) {
			t.Fatalf("%s content 应以校验错误拒绝: err=%v", name, err)
		}
	}
	// 恰上限长度通过校验层（ValidateAIMemory 边界，见 TestValidateAIMemory
	// 的 auto 分支）；AIMemoryAutoMax 上限常量存在且为正。
	if AIMemoryAutoMax <= 0 {
		t.Fatalf("AIMemoryAutoMax 应为正: %d", AIMemoryAutoMax)
	}
}
