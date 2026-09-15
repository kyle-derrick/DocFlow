package search

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// fakeMeili 构造假 Meilisearch 服务：记录请求（method+path+body），按路由
// 返回可配置响应；ensureIndexStatus 控制 POST /indexes 的首次/后续状态码。
type fakeMeili struct {
	mu             sync.Mutex
	requests       []string // "METHOD path body"
	ensureStatuses []int    // 每次 POST /indexes 依序取用；耗尽后 202
	searchResponse string
	upsertBodies   []map[string]any
	deletePaths    []string
}

func newFakeMeili() *fakeMeili { return &fakeMeili{ensureStatuses: []int{202}} }

func (f *fakeMeili) record(method, path, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, method+" "+path+" "+body)
}

func (f *fakeMeili) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := ""
	if r.Body != nil {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		body = string(raw)
	}
	f.record(r.Method, r.URL.Path+"?"+r.URL.RawQuery, body)
	switch {
	case r.URL.Path == "/indexes" && r.Method == http.MethodPost:
		f.mu.Lock()
		status := 202
		if len(f.ensureStatuses) > 0 {
			status = f.ensureStatuses[0]
			f.ensureStatuses = f.ensureStatuses[1:]
		}
		f.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"taskUid":1}`))
	case r.URL.Path == "/indexes/docflow-files/documents" && r.Method == http.MethodPost:
		f.mu.Lock()
		var docs []map[string]any
		_ = json.Unmarshal([]byte(body), &docs)
		f.upsertBodies = append(f.upsertBodies, docs...)
		f.mu.Unlock()
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"taskUid":2}`))
	case strings.HasPrefix(r.URL.Path, "/indexes/docflow-files/documents/") && r.Method == http.MethodDelete:
		f.mu.Lock()
		f.deletePaths = append(f.deletePaths, r.URL.Path)
		f.mu.Unlock()
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"taskUid":3}`))
	case r.URL.Path == "/indexes/docflow-files/search" && r.Method == http.MethodPost:
		w.Header().Set("Content-Type", "application/json")
		resp := f.searchResponse
		if resp == "" {
			resp = `{"hits":[]}`
		}
		_, _ = w.Write([]byte(resp))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"not found"}`))
	}
}

func newMeiliTestRepo(srvURL string, teams map[uuid.UUID][]uuid.UUID) *MeiliRepo {
	return NewMeiliRepo(srvURL, "meili-key", func(user uuid.UUID) ([]uuid.UUID, error) {
		return teams[user], nil
	})
}

// TestMeiliEnsureIndexIdempotent 首次创建 202 成功；已存在 405 幂等忽略；
// 其他状态码报错。
func TestMeiliEnsureIndexIdempotent(t *testing.T) {
	fake := newFakeMeili()
	fake.ensureStatuses = []int{202, 405}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	repo := newMeiliTestRepo(srv.URL, nil)

	if err := repo.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if err := repo.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("already exists (405): %v", err)
	}
	fake.ensureStatuses = []int{500}
	if err := repo.EnsureIndex(context.Background()); err == nil {
		t.Fatal("status 500: want error")
	}
	// 请求体：uid + primaryKey=file_id；带 Bearer Key。
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) < 2 || !strings.Contains(fake.requests[0], "POST /indexes?") ||
		!strings.Contains(fake.requests[0], `"primaryKey":"file_id"`) {
		t.Fatalf("ensure request = %v", fake.requests)
	}
}

// TestMeiliUpsertDocBody 验证 upsert 请求：路径带 primaryKey=file_id、
// 文档字段映射（team_id null / 字符串 UUID）、Content 为空串时原样提交。
func TestMeiliUpsertDocBody(t *testing.T) {
	fake := newFakeMeili()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	repo := newMeiliTestRepo(srv.URL, nil)

	fileID, versionID, ownerID, teamID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if err := repo.UpsertDoc(Doc{FileID: fileID, VersionID: versionID, OwnerID: ownerID, TeamID: &teamID, Name: "notes.md", Content: "hello"}); err != nil {
		t.Fatalf("upsert team doc: %v", err)
	}
	if err := repo.UpsertDoc(Doc{FileID: uuid.New(), VersionID: versionID, OwnerID: ownerID, Name: "bin.png", Content: ""}); err != nil {
		t.Fatalf("upsert personal doc: %v", err)
	}

	fake.mu.Lock()
	if len(fake.upsertBodies) != 2 {
		fake.mu.Unlock()
		t.Fatalf("upserts = %d, want 2", len(fake.upsertBodies))
	}
	first := fake.upsertBodies[0]
	secondTeamID := fake.upsertBodies[1]["team_id"]
	firstRequest := ""
	if len(fake.requests) > 0 {
		firstRequest = fake.requests[0]
	}
	fake.mu.Unlock()
	if first["file_id"] != fileID.String() || first["version_id"] != versionID.String() ||
		first["owner_id"] != ownerID.String() || first["team_id"] != teamID.String() ||
		first["name"] != "notes.md" || first["content"] != "hello" {
		t.Fatalf("team doc = %v", first)
	}
	if secondTeamID != nil {
		t.Fatalf("personal doc team_id = %v, want nil", secondTeamID)
	}
	if !strings.Contains(firstRequest, "/documents?primaryKey=file_id") {
		t.Fatalf("upsert path = %s", firstRequest)
	}

	// RemoveDoc：DELETE /indexes/{uid}/documents/{fileID}。
	if err := repo.RemoveDoc(fileID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	fake.mu.Lock()
	paths := append([]string(nil), fake.deletePaths...)
	fake.mu.Unlock()
	if len(paths) != 1 || paths[0] != "/indexes/docflow-files/documents/"+fileID.String() {
		t.Fatalf("delete paths = %v", paths)
	}
}

// TestMeiliQueryDocs 覆盖查询：过滤表达式（owner OR team IN）、结果映射
// （file_id/name/type= file/snippet 高亮）、limit 透传。
func TestMeiliQueryDocs(t *testing.T) {
	user, other := uuid.New(), uuid.New()
	teamA, teamB := uuid.New(), uuid.New()
	fileID := uuid.New()

	fake := newFakeMeili()
	fake.searchResponse = `{"hits":[
		{"file_id":"` + fileID.String() + `","name":"report.md","content":"quarterly [[report]] body","_formatted":{"content":"quarterly [[report]] body"}},
		{"file_id":"not-a-uuid","name":"bad","_formatted":{"content":""}}
	]}`
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	repo := newMeiliTestRepo(srv.URL, map[uuid.UUID][]uuid.UUID{user: {teamA, teamB}})

	results, err := repo.QueryDocs(user, QueryOptions{Q: "report", Limit: 5})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1（非法主键跳过）", len(results))
	}
	r := results[0]
	if r.ID != fileID || r.Name != "report.md" || r.Type != "file" || r.Snippet != "quarterly [[report]] body" {
		t.Fatalf("result = %+v", r)
	}

	fake.mu.Lock()
	last := ""
	for _, req := range fake.requests {
		if strings.HasPrefix(req, "POST /indexes/docflow-files/search") {
			last = req
		}
	}
	fake.mu.Unlock()
	if last == "" {
		t.Fatal("search request not captured")
	}
	wantFilter := "owner_id = \"" + user.String() + "\" OR team_id IN [\"" + teamA.String() + "\", \"" + teamB.String() + "\"]"
	var payload struct {
		Q         string   `json:"q"`
		Limit     int      `json:"limit"`
		Filter    string   `json:"filter"`
		Highlight []string `json:"attributesToHighlight"`
	}
	if err := json.Unmarshal([]byte(last[strings.Index(last, " {"):]), &payload); err != nil {
		t.Fatalf("decode search body %q: %v", last, err)
	}
	if payload.Q != "report" || payload.Limit != 5 || payload.Filter != wantFilter {
		t.Fatalf("search payload = %+v, want filter %s", payload, wantFilter)
	}
	if len(payload.Highlight) != 1 || payload.Highlight[0] != "content" {
		t.Fatalf("highlight = %v", payload.Highlight)
	}

	// 无团队用户：仅 owner 过滤（不含 OR team_id）。
	if _, err := repo.QueryDocs(other, QueryOptions{Q: "x", Limit: 1}); err != nil {
		t.Fatalf("query other: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, req := range fake.requests {
		if strings.HasPrefix(req, "POST /indexes/docflow-files/search") && strings.Contains(req, other.String()) {
			if strings.Contains(req, "team_id") {
				t.Fatalf("user without teams should not filter by team_id: %s", req)
			}
		}
	}
}

// TestMeiliQueryDocsUnsupportedFilter tag/starred 过滤显式报错（不静默放宽）。
func TestMeiliQueryDocsUnsupportedFilter(t *testing.T) {
	srv := httptest.NewServer(newFakeMeili())
	t.Cleanup(srv.Close)
	repo := newMeiliTestRepo(srv.URL, nil)
	tag := uuid.New()
	if _, err := repo.QueryDocs(uuid.New(), QueryOptions{Q: "x", TagID: &tag}); err != ErrUnsupportedFilter {
		t.Fatalf("tag filter: err = %v, want ErrUnsupportedFilter", err)
	}
	starred := true
	if _, err := repo.QueryDocs(uuid.New(), QueryOptions{Q: "x", Starred: &starred}); err != ErrUnsupportedFilter {
		t.Fatalf("starred filter: err = %v, want ErrUnsupportedFilter", err)
	}
}

// TestMeiliNameMatchSnippetFallback 名称命中（内容未高亮）时 snippet 回退名称。
func TestMeiliNameMatchSnippetFallback(t *testing.T) {
	fake := newFakeMeili()
	fake.searchResponse = `{"hits":[{"file_id":"` + uuid.New().String() + `","name":"Report.txt","content":"","_formatted":{"content":""}}]}`
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	repo := newMeiliTestRepo(srv.URL, nil)
	results, err := repo.QueryDocs(uuid.New(), QueryOptions{Q: "report", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Snippet != "Report.txt" {
		t.Fatalf("results = %+v, want name snippet fallback", results)
	}
}
