// studio_test.go：Studio 项目载荷校验测试（纯函数分支；DB 交互依赖真实
// gorm 存储，由 E2E 覆盖——同 auth.aimemory_test 的取舍）。
package studio

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func validProject() Project {
	return Project{
		Name:       "产品官网",
		SpaceID:    uuid.Must(uuid.NewV7()),
		RootFolder: uuid.Must(uuid.NewV7()),
		SpaceName:  "默认空间",
		FolderPath: "默认空间",
	}
}

// TestValidateProject 合法分支：engine 空/docker 归一、快照字段去空白。
func TestValidateProject(t *testing.T) {
	p := validProject()
	p.Engine = ""
	if err := ValidateProject(&p); err != nil {
		t.Fatalf("合法载荷: %v", err)
	}
	if p.Engine != EnginePlatform {
		t.Fatalf("空 engine 应归一 platform，得 %q", p.Engine)
	}
	p = validProject()
	p.Engine = EngineDocker
	p.Harness = "claude-code"
	p.Model = "prov/model"
	if err := ValidateProject(&p); err != nil {
		t.Fatalf("docker 引擎载荷: %v", err)
	}
	p = validProject()
	p.SpaceName, p.FolderPath = "  默认空间  ", " 空间/目录 "
	if err := ValidateProject(&p); err != nil {
		t.Fatalf("快照字段: %v", err)
	}
	if p.SpaceName != "默认空间" || p.FolderPath != "空间/目录" {
		t.Fatalf("快照字段应去空白，得 %q / %q", p.SpaceName, p.FolderPath)
	}
}

// TestValidateProjectInvalid 非法分支：name 空/超长、缺 space/folder、
// engine 非法值归一（不报错）、harness/model 超长。
func TestValidateProjectInvalid(t *testing.T) {
	p := validProject()
	p.Name = "  "
	if err := ValidateProject(&p); err == nil || !errors.Is(err, ErrInvalidProject) {
		t.Fatalf("空白 name 应报错: %v", err)
	}
	p = validProject()
	p.Name = strings.Repeat("名", MaxNameRunes+1)
	if err := ValidateProject(&p); err == nil || !errors.Is(err, ErrInvalidProject) {
		t.Fatalf("超长 name 应报错: %v", err)
	}
	p = validProject()
	p.SpaceID = uuid.Nil
	if err := ValidateProject(&p); err == nil || !errors.Is(err, ErrInvalidProject) {
		t.Fatalf("缺 space_id 应报错: %v", err)
	}
	p = validProject()
	p.RootFolder = uuid.Nil
	if err := ValidateProject(&p); err == nil || !errors.Is(err, ErrInvalidProject) {
		t.Fatalf("缺 root_folder_id 应报错: %v", err)
	}
	// 非法 engine 归一为 platform（宽松：旧数据/前端未传不致 400）。
	p = validProject()
	p.Engine = "k8s"
	if err := ValidateProject(&p); err != nil {
		t.Fatalf("非法 engine 应归一不报错: %v", err)
	}
	if p.Engine != EnginePlatform {
		t.Fatalf("非法 engine 应归一 platform，得 %q", p.Engine)
	}
	p = validProject()
	p.Harness = strings.Repeat("h", MaxHarnessRunes+1)
	if err := ValidateProject(&p); err == nil || !errors.Is(err, ErrInvalidProject) {
		t.Fatalf("超长 harness 应报错: %v", err)
	}
	p = validProject()
	p.Model = strings.Repeat("m", MaxModelRunes+1)
	if err := ValidateProject(&p); err == nil || !errors.Is(err, ErrInvalidProject) {
		t.Fatalf("超长 model 应报错: %v", err)
	}
}
