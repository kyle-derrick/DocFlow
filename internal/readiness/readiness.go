package readiness

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/docflow/docflow/internal/config"
	"github.com/docflow/docflow/internal/tasks"
	"github.com/docflow/docflow/internal/upload"
)

type Checker struct {
	db      *sql.DB
	cfg     config.Config
	storage upload.Storage
}

func New(db *sql.DB, cfg config.Config, storage upload.Storage) *Checker {
	return &Checker{db: db, cfg: cfg, storage: storage}
}

func (c *Checker) Check(ctx context.Context) (map[string]string, bool) {
	checks := map[string]string{}
	ok := true
	check := func(name string, err error) {
		if err != nil {
			checks[name] = "failed"
			ok = false
		} else {
			checks[name] = "ok"
		}
	}
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	check("db", c.db.PingContext(pingCtx))
	if c.cfg.QueueDriver == "redis" {
		check("queue", tasks.PingRedis(pingCtx, c.cfg.RedisAddr, c.cfg.RedisPassword))
	} else {
		checks["queue"] = "ok"
	}
	if c.cfg.StorageDriver == "local" {
		f, err := os.OpenFile(c.cfg.StorageRoot+string(os.PathSeparator)+".readiness", os.O_CREATE|os.O_WRONLY, 0600)
		if err == nil {
			err = f.Close()
			_ = os.Remove(f.Name())
		}
		check("storage", err)
	} else if h, okh := c.storage.(interface{ HeadBucket(context.Context) error }); okh {
		check("storage", h.HeadBucket(pingCtx))
	} else {
		check("storage", fmt.Errorf("storage health check unavailable"))
	}
	if c.cfg.ScanEnabled {
		check("clamav", tcpPing(pingCtx, c.cfg.ClamAVAddr))
	}
	if c.cfg.OnlyOfficeEnabled {
		check("onlyoffice", httpHealth(pingCtx, c.cfg.OnlyOfficeServerURL))
	}
	// draw.io 静态层由 caddy 镜像 /drawio/* 直接服务，不属于 backend
	// 就绪依赖，因此不在此处伪造独立服务检查项。
	return checks, ok
}

func tcpPing(ctx context.Context, addr string) error {
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	// zPING 为 clamd n-command 规范形态（0.95+ 全支持，响应以 \x00 结尾）；
	// 旧式 PING\x00 在 clamd 1.5 返回 "PONG\n"（真实实例暴露），故响应
	// 白名单兼容 \x00 / 裸 PONG / \n 三种。
	if _, err := conn.Write([]byte("zPING\x00")); err != nil {
		return err
	}
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if err != nil {
		return err
	}
	switch string(buf[:n]) {
	case "PONG\x00", "PONG", "PONG\n":
		return nil
	}
	return fmt.Errorf("unexpected clamav response")
}
func httpHealth(ctx context.Context, base string) error {
	if !strings.HasSuffix(base, "/healthcheck") {
		base = strings.TrimSuffix(base, "/") + "/healthcheck"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
