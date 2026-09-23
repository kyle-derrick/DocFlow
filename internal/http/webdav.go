package http

import (
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/space"
	"github.com/google/uuid"
)

type davResource struct {
	File files.File
	Root bool
}

type davCollection struct{}

type davProp struct {
	ResourceType *struct {
		Collection davCollection `xml:"D:collection"`
	} `xml:"D:resourcetype,omitempty"`
	DisplayName string `xml:"D:displayname"`
}

type davPropstat struct {
	Prop   davProp `xml:"D:prop"`
	Status string  `xml:"D:status"`
}

type davResponse struct {
	Href     string      `xml:"D:href"`
	Propstat davPropstat `xml:"D:propstat"`
}

type davMultistatus struct {
	XMLName   xml.Name
	XMLNS     string        `xml:"xmlns:D,attr"`
	Responses []davResponse `xml:"D:response"`
}

type davHandler struct {
	h         *Handler
	tokens    *auth.WebDAVStore
	base      string
	enabledFn func() bool
	// WebDAV Basic Auth 失败锁定（防爆破）：per「用户名+IP」内存计数，
	// 阈值/时长复用登录锁定策略（h.loginMaxRetries / h.loginLockDuration，
	// 即 LOGIN_MAX_RETRIES / LOGIN_LOCK_MINUTES）。WebDAV 校验的是一次性
	// 令牌而非账号密码，登录级 users 表锁定字段不适用，故独立内存态
	//（重启清零可接受；成功验证即清零该键计数）。
	mu    sync.Mutex
	fails map[string]*davFailState
}

type davFailState struct {
	count int
	until time.Time
}

// davClientIP 提取 RemoteAddr 主机部分（兼容无端口形式）。
func davClientIP(remote string) string {
	if host, _, err := net.SplitHostPort(remote); err == nil {
		return host
	}
	return remote
}

// davAuthLocked 判定该「用户名+IP」是否处于锁定窗口。
func (d *davHandler) davAuthLocked(key string) (bool, time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.fails[key]
	if !ok || st.until.IsZero() || time.Now().After(st.until) {
		return false, 0
	}
	return true, time.Until(st.until)
}

// davAuthFail 记录一次失败；达到阈值（>=1 生效）进入锁定窗口。
func (d *davHandler) davAuthFail(key string) {
	maxRetries := d.h.loginMaxRetries
	lockFor := d.h.loginLockDuration
	if maxRetries < 1 || lockFor <= 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fails == nil {
		d.fails = map[string]*davFailState{}
	}
	st := d.fails[key]
	if st == nil {
		st = &davFailState{}
		d.fails[key] = st
	}
	if !st.until.IsZero() && time.Now().After(st.until) {
		st.count = 0 // 上一锁定窗口已过，重新计。
	}
	st.count++
	if st.count >= maxRetries {
		st.until = time.Now().Add(lockFor)
		st.count = 0
	}
}

// davAuthOK 验证成功清零该键。
func (d *davHandler) davAuthOK(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.fails, key)
}

func (d *davHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !d.enabled() {
		http.NotFound(w, r)
		return
	}
	user, token, ok := r.BasicAuth()
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="DocFlow WebDAV"`)
		http.Error(w, "authentication required", 401)
		return
	}
	// 防爆破：锁定窗口内直接拒绝（带 Retry-After）；失败计数/成功清零。
	authKey := user + "|" + davClientIP(r.RemoteAddr)
	if locked, wait := d.davAuthLocked(authKey); locked {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		w.Header().Set("WWW-Authenticate", `Basic realm="DocFlow WebDAV"`)
		http.Error(w, "too many failed attempts, try later", 401)
		return
	}
	uid, err := d.tokens.Verify(user, token)
	if err != nil {
		d.davAuthFail(authKey)
		w.Header().Set("WWW-Authenticate", `Basic realm="DocFlow WebDAV"`)
		http.Error(w, "invalid credentials", 401)
		return
	}
	d.davAuthOK(authKey)
	rel, ok := davPath(r.URL.Path, d.base)
	if !ok {
		http.Error(w, "invalid path", 400)
		return
	}
	res, err := d.resolve(uid, rel)
	if err != nil && r.Method != "PUT" && r.Method != "MKCOL" {
		http.Error(w, "not found", 404)
		return
	}
	switch r.Method {
	case "PROPFIND":
		d.propfind(w, r, res, uid, rel)
	case "GET", "HEAD":
		d.get(w, r, res, uid)
	case "PUT":
		d.put(w, r, res, uid, rel)
	case "MKCOL":
		d.mkcol(w, r, res, uid, rel)
	case "DELETE":
		d.del(w, res, uid)
	case "MOVE", "COPY":
		d.moveCopy(w, r, res, uid, rel)
	case "LOCK", "UNLOCK":
		w.WriteHeader(http.StatusNotImplemented)
	default:
		w.Header().Set("Allow", "PROPFIND,GET,HEAD,PUT,MKCOL,MOVE,COPY,DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
func (d *davHandler) enabled() bool { return d.enabledFn != nil && d.enabledFn() }

func davPath(raw, base string) (string, bool) {
	if raw == "" || !strings.HasPrefix(raw, "/") || (raw != base && !strings.HasPrefix(raw, strings.TrimSuffix(base, "/")+"/")) {
		return "", false
	}
	p := strings.TrimPrefix(raw, strings.TrimSuffix(base, "/"))
	p = strings.TrimPrefix(p, "/")
	if strings.Contains(p, "\\") {
		return "", false
	}
	parts := strings.Split(p, "/")
	out := make([]string, 0, len(parts))
	for _, x := range parts {
		if x == "" || x == "." {
			continue
		}
		if x == ".." {
			return "", false
		}
		un, err := url.PathUnescape(x)
		if err != nil || un == ".." || strings.Contains(un, "\\") {
			return "", false
		}
		out = append(out, un)
	}
	return strings.Join(out, "/"), true
}

func destinationPath(raw, base string, req *http.Request) (string, bool) {
	if strings.TrimSpace(raw) == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Path == "" {
		return "", false
	}
	if u.IsAbs() && (u.Host != "" && !strings.EqualFold(u.Host, req.Host)) {
		return "", false
	}
	return davPath(u.EscapedPath(), base)
}
func (d *davHandler) resolve(uid uuid.UUID, rel string) (davResource, error) {
	if rel == "" {
		return davResource{Root: true}, nil
	}
	spaces, err := d.h.spaces.ListSpaces(uid)
	if err != nil {
		return davResource{}, err
	}
	parts := strings.Split(rel, "/")
	for _, sp := range spaces {
		root, e := d.h.files.SpaceRoot(sp.ID)
		if e != nil || parts[0] != sp.Name {
			continue
		}
		if len(parts) == 1 {
			return davResource{File: root}, nil
		}
		cur := root
		for _, name := range parts[1:] {
			items, e := d.h.files.ListSpace(sp.ID, cur.ID, 1000, files.SpaceListFilter{})
			if e != nil {
				return davResource{}, e
			}
			found := false
			for _, f := range items {
				if f.Name == name {
					cur, found = f, true
					break
				}
			}
			if !found {
				return davResource{}, files.ErrNotFound
			}
		}
		if _, e = d.h.files.Get(uid, cur.ID); e != nil {
			return davResource{}, e
		}
		return davResource{File: cur}, nil
	}
	return davResource{}, files.ErrNotFound
}

func (d *davHandler) propfind(w http.ResponseWriter, r *http.Request, res davResource, uid uuid.UUID, rel string) {
	depth := r.Header.Get("Depth")
	if depth == "" {
		depth = "infinity"
	}
	if depth != "0" && depth != "1" && depth != "infinity" {
		http.Error(w, "invalid depth", 400)
		return
	}
	if depth == "infinity" {
		http.Error(w, "infinity depth is not supported", 403)
		return
	}
	m := davMultistatus{XMLName: xml.Name{Local: "D:multistatus"}, XMLNS: "DAV:"}
	add := func(href, name string, dir bool) {
		p := davProp{DisplayName: name}
		if dir {
			p.ResourceType = &struct {
				Collection davCollection `xml:"D:collection"`
			}{}
		}
		m.Responses = append(m.Responses, davResponse{Href: href, Propstat: davPropstat{Prop: p, Status: "HTTP/1.1 200 OK"}})
	}
	base := strings.TrimSuffix(r.URL.Path, "/")
	if base == "" {
		base = "/"
	}
	if res.Root || res.File.Type == "folder" {
		base += "/"
	}
	add(escapedHref(base), path.Base(strings.TrimSuffix(base, "/")), res.Root || res.File.Type == "folder")
	if depth == "1" && (res.Root || res.File.Type == "folder") {
		if res.Root {
			for _, sp := range mustSpaces(d.h.spaces, uid) {
				add(escapedHref(strings.TrimSuffix(base, "/")+"/"+sp.Name+"/"), sp.Name, true)
			}
		} else if items, e := d.h.files.ListSpace(res.File.SpaceID, res.File.ID, 1000, files.SpaceListFilter{}); e == nil {
			for _, f := range items {
				suffix := ""
				if f.Type == "folder" {
					suffix = "/"
				}
				add(escapedHref(strings.TrimSuffix(base, "/")+"/"+f.Name+suffix), f.Name, f.Type == "folder")
			}
		}
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(207)
	_ = xml.NewEncoder(w).Encode(m)
}
func escapedHref(p string) string { u := url.URL{Path: p}; return u.EscapedPath() }
func mustSpaces(s *space.Service, u uuid.UUID) []space.Space {
	x, e := s.ListSpaces(u)
	if e != nil {
		return nil
	}
	return x
}
func (d *davHandler) get(w http.ResponseWriter, r *http.Request, res davResource, uid uuid.UUID) {
	if res.Root || res.File.Type == "folder" {
		http.Error(w, "directory", 405)
		return
	}
	_, b, e := d.h.files.CurrentVersion(uid, res.File.ID)
	if e != nil || b.Status != files.BlobStatusAvailable {
		http.NotFound(w, r)
		return
	}
	rd, e := d.h.storage.Read(b.StorageKey)
	if e != nil {
		http.NotFound(w, r)
		return
	}
	defer rd.Close()
	w.Header().Set("Content-Length", fmt.Sprint(b.Size))
	w.Header().Set("Content-Type", b.MimeType)
	if r.Method != "HEAD" {
		_, _ = io.Copy(w, rd)
	}
}
func (d *davHandler) put(w http.ResponseWriter, r *http.Request, res davResource, uid uuid.UUID, rel string) {
	if res.File.ID != uuid.Nil && res.File.Type == "folder" || res.Root {
		http.Error(w, "invalid target", 405)
		return
	}
	data, e := io.ReadAll(io.LimitReader(r.Body, d.h.uploads.MaxSize()+1))
	if e != nil || int64(len(data)) > d.h.uploads.MaxSize() {
		http.Error(w, "too large", 413)
		return
	}
	if res.File.ID != uuid.Nil {
		s, e := d.h.uploads.StartReplace(uid, res.File.ID, int64(len(data)), "")
		if e == nil {
			_, e = d.h.uploads.Append(s.ID, 0, strings.NewReader(string(data)))
		}
		if e == nil {
			_, e = d.h.uploads.Complete(s.ID)
		}
		if e != nil {
			http.Error(w, "upload failed", 500)
			return
		}
		w.WriteHeader(204)
		return
	}
	parts := strings.Split(rel, "/")
	name := parts[len(parts)-1]
	parent, e := d.resolve(uid, strings.Join(parts[:len(parts)-1], "/"))
	if e != nil || parent.Root {
		http.Error(w, "invalid parent", 409)
		return
	}
	if _, e = d.h.uploads.UploadBytes(uid, parent.File.ID, name, data); e != nil {
		http.Error(w, "upload failed", 500)
		return
	}
	w.WriteHeader(201)
}
func (d *davHandler) mkcol(w http.ResponseWriter, r *http.Request, res davResource, uid uuid.UUID, rel string) {
	if res.File.ID != uuid.Nil || res.Root {
		http.Error(w, "exists", 405)
		return
	}
	parts := strings.Split(rel, "/")
	name := parts[len(parts)-1]
	parent, e := d.resolve(uid, strings.Join(parts[:len(parts)-1], "/"))
	if e != nil || parent.Root {
		http.Error(w, "invalid parent", 409)
		return
	}
	if _, e = d.h.files.CreateFolderIn(uid, parent.File.ID, name); e != nil {
		http.Error(w, "cannot create", 403)
		return
	}
	w.WriteHeader(201)
}
func (d *davHandler) del(w http.ResponseWriter, res davResource, uid uuid.UUID) {
	if res.Root {
		http.Error(w, "forbidden", 403)
		return
	}
	if e := d.h.files.Delete(uid, res.File.ID); e != nil {
		http.Error(w, "forbidden", 403)
		return
	}
	w.WriteHeader(204)
}
func (d *davHandler) moveCopy(w http.ResponseWriter, r *http.Request, res davResource, uid uuid.UUID, rel string) {
	if res.Root {
		http.Error(w, "invalid source", 405)
		return
	}
	dr, ok := destinationPath(r.Header.Get("Destination"), d.base, r)
	if !ok {
		http.Error(w, "invalid destination", 400)
		return
	}
	parts := strings.Split(dr, "/")
	name := parts[len(parts)-1]
	parent, e := d.resolve(uid, strings.Join(parts[:len(parts)-1], "/"))
	if e != nil || parent.Root {
		http.Error(w, "invalid destination", 409)
		return
	}
	if r.Method == "COPY" {
		_, e = d.h.files.Copy(uid, res.File.ID, parent.File.ID, name)
	} else {
		_, e = d.h.files.MoveRename(uid, res.File.ID, parent.File.ID, name)
	}
	if e != nil {
		http.Error(w, "operation failed", 403)
		return
	}
	w.WriteHeader(201)
}
