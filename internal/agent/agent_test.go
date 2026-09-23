package agent

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestSafeJoin(t *testing.T) {
	root := t.TempDir()
	if _, e := SafeJoin(root, "a/b.txt"); e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{"../x", "/etc/passwd", "a/../../x"} {
		if _, e := SafeJoin(root, p); !errors.Is(e, ErrPathTraversal) {
			t.Errorf("%s accepted", p)
		}
	}
	_ = filepath.Separator
}
func TestImageAndTransition(t *testing.T) {
	c := DefaultConfig()
	c.AllowedImages = []string{"x:1"}
	if c.ValidateImage("x:2") == nil {
		t.Fatal("image accepted")
	}
	if Transition(StatusSucceeded, StatusRunning) == nil {
		t.Fatal("transition accepted")
	}
}

// TestDefaultConfigImage 默认镜像：DefaultConfig 预填一个默认允许镜像
// （仓库不构建专用 agent 镜像，用 alpine:3.20 占位），未指定镜像的任务
// 可直接通过校验；空镜像给明确错误（不 panic、errors.Is 可判）。
func TestDefaultConfigImage(t *testing.T) {
	c := DefaultConfig()
	if len(c.AllowedImages) == 0 || c.AllowedImages[0] == "" {
		t.Fatalf("default config should prefill a default allowed image: %+v", c.AllowedImages)
	}
	if err := c.ValidateImage(c.AllowedImages[0]); err != nil {
		t.Fatalf("default image should pass validation: %v", err)
	}
	// 空镜像：无任何允许镜像时给明确错误（不 panic）。
	if err := (Config{}).ValidateImage(""); err == nil || !errors.Is(err, ErrImageNotAllowed) {
		t.Fatalf("empty image with no allowed images: got %v, want ErrImageNotAllowed", err)
	}
	// 空镜像：即便有默认镜像也拒绝（调用方须先取默认或显式指定）。
	if err := c.ValidateImage(""); err == nil || !errors.Is(err, ErrImageNotAllowed) {
		t.Fatalf("empty image should be rejected: got %v", err)
	}
}
func TestTokenDigest(t *testing.T) {
	p, d, e := NewCallbackToken()
	if e != nil || d != TokenDigest(p) {
		t.Fatal("token digest mismatch")
	}
	if len(p) == 0 {
		t.Fatal("empty token")
	}
}
