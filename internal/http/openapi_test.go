package http

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/docflow/docflow/internal/onlyoffice"
	"github.com/docflow/docflow/internal/webpkg"
	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

// 本文件是 OpenAPI 契约测试：docs/openapi.yaml 与 Handler.Register 注册的 gin 路由
// 双向一致，防止契约与实现漂移。
//   - 正向：契约中每个 path+method 必须在 gin 路由树中存在（{id} 与 :id 归一化对比）；
//   - 反向：gin 注册的业务路由必须全部出现在契约中（豁免 /health、/ready 与 prometheus）。
// 全部走内存依赖（Register 只注册路由、不触发依赖调用），不依赖 PostgreSQL。

// contractMethodKeys 是 OpenAPI path item 中合法的 HTTP method 键；
// 出现其他键（如误把 parameters 写在 path 级别）直接判失败，保证契约结构可被本测试消费。
var contractMethodKeys = map[string]bool{
	http.MethodGet:     true,
	http.MethodPut:     true,
	http.MethodPost:    true,
	http.MethodDelete:  true,
	http.MethodOptions: true,
	http.MethodHead:    true,
	http.MethodPatch:   true,
	http.MethodTrace:   true,
}

type contractSpec struct {
	Servers []struct {
		URL string `yaml:"url"`
	} `yaml:"servers"`
	Paths map[string]map[string]yaml.Node `yaml:"paths"`
}

func loadContractSpec(t *testing.T) (base string, spec contractSpec) {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "docs", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read docs/openapi.yaml: %v", err)
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse docs/openapi.yaml: %v", err)
	}
	if len(spec.Servers) == 0 || spec.Servers[0].URL == "" {
		t.Fatal("spec must declare servers[0].url as the route base path")
	}
	if len(spec.Paths) == 0 {
		t.Fatal("spec contains no paths")
	}
	return strings.TrimSuffix(spec.Servers[0].URL, "/"), spec
}

// contractRouter 以与生产一致的方式构建完整路由树；依赖传 nil 即可，
// Register 仅完成路由注册与中间件装配，不会调用任何存储。
// ONLYOFFICE 集成在契约测试中恒启用（SetOnlyOffice 注入），保证契约中的
// onlyoffice 端点参与双向校验；生产未启用时不注册该组路由（404）。
// 网页包手动解包端点（POST /files/{id}/webpkg/extract）同理恒启用；
// 内容端点 /content/:pid/*filepath 为契约外内容域路由（isExcludedRoute 豁免）。
func contractRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", 0)
	h.SetOnlyOffice(onlyoffice.New(onlyoffice.Config{
		ServerURL:    "http://onlyoffice:80",
		DownloadBase: "http://backend:8080",
		JWTSecret:    "openapi-contract-onlyoffice-secret-0123456789",
	}, nil, nil, nil, nil), 60)
	h.SetWebpkg(webpkg.NewService(webpkg.NewMemoryRepo(), nil, nil, webpkg.DefaultLimits()), 60)
	r := gin.New()
	h.Register(r, "openapi-contract-test-secret", 1_000_000, 1_000_000, 1_000_000)
	return r
}

// openapiPathToGin 把契约路径归一化为 gin 路由形式：{id} → :id，并拼接 server 基础路径。
func openapiPathToGin(base, path string) string {
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		switch path[i] {
		case '{':
			b.WriteByte(':')
		case '}':
			// 跳过右花括号
		default:
			b.WriteByte(path[i])
		}
	}
	return base + b.String()
}

// ginPathToOpenapi 把 gin 路由归一化为契约路径形式：:id → {id}，并去除 server 基础路径。
// 路由不在基础路径下时返回 ok=false（如 /health、/ready）。
func ginPathToOpenapi(base, path string) (string, bool) {
	if !strings.HasPrefix(path, base+"/") {
		return "", false
	}
	segments := strings.Split(strings.TrimPrefix(path, base), "/")
	for i, seg := range segments {
		if strings.HasPrefix(seg, ":") {
			segments[i] = "{" + seg[1:] + "}"
		}
	}
	return strings.Join(segments, "/"), true
}

// isExcludedRoute 判断是否为契约外路由：
//   - 健康探针（/health、/ready）与 Prometheus 指标端点（/metrics，
//     METRICS_ENABLED 默认启用；设计文档 13.2 将其归为基础设施探针）；
//   - 网页包内容域端点 /content/:pid/*filepath：无认证的内容子资源路径，
//     面向 sandbox iframe 而非 API 客户端（独立按 IP 轻限流、严格 CSP），
//     与 /metrics 同属契约外基础设施路由，不入 docs/openapi.yaml。
func isExcludedRoute(path string) bool {
	if path == "/health" || path == "/ready" || path == "/metrics" {
		return true
	}
	if strings.HasPrefix(path, "/content/") {
		return true
	}
	return strings.Contains(path, "prometheus") || strings.Contains(path, "metrics")
}

// contractOperations 汇总契约中的全部 "METHOD /path" 操作。
func contractOperations(t *testing.T, spec contractSpec) map[string]bool {
	t.Helper()
	ops := make(map[string]bool)
	for path, item := range spec.Paths {
		for key := range item {
			if !contractMethodKeys[strings.ToUpper(key)] {
				t.Fatalf("docs/openapi.yaml: path %q 含非法的 path-item 键 %q（本测试仅支持 HTTP method 键）", path, key)
			}
			ops[strings.ToUpper(key)+" "+path] = true
		}
	}
	return ops
}

// TestOpenAPISpecRoutesRegistered 正向校验：契约中的每个 path+method 都真实注册在 gin 路由树。
func TestOpenAPISpecRoutesRegistered(t *testing.T) {
	base, spec := loadContractSpec(t)
	router := contractRouter(t)
	registered := make(map[string]bool)
	for _, route := range router.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	ops := contractOperations(t, spec)
	for op := range ops {
		method, path, _ := strings.Cut(op, " ")
		ginPath := openapiPathToGin(base, path)
		if !registered[method+" "+ginPath] {
			t.Errorf("契约 %s %s 未在 gin 路由树注册（归一化后 %s）", method, path, ginPath)
		}
	}
	if len(ops) == 0 {
		t.Fatal("契约中没有任何操作")
	}
	t.Logf("正向校验通过：%d 个契约操作（%d 个 path）均已在 gin 注册", len(ops), len(spec.Paths))
}

// TestGinRoutesInOpenAPISpec 反向校验：gin 注册的业务路由必须全部出现在契约中。
func TestGinRoutesInOpenAPISpec(t *testing.T) {
	base, spec := loadContractSpec(t)
	router := contractRouter(t)
	ops := contractOperations(t, spec)
	checked := 0
	for _, route := range router.Routes() {
		if isExcludedRoute(route.Path) {
			continue
		}
		path, ok := ginPathToOpenapi(base, route.Path)
		if !ok {
			t.Errorf("路由 %s %s 不在 server 基础路径 %s 下，也不属于豁免清单（/health、/ready、/metrics）", route.Method, route.Path, base)
			continue
		}
		if !ops[route.Method+" "+path] {
			t.Errorf("路由 %s %s 未收录在 docs/openapi.yaml，请补充契约", route.Method, path)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("未校验到任何 gin 业务路由")
	}
	t.Logf("反向校验通过：%d 条 gin 业务路由均已在契约中", checked)
}
