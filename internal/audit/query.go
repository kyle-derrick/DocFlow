package audit

import (
	"encoding/csv"
	"io"
	"strconv"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Query struct {
	Limit        int
	Cursor       int64
	Action       string
	UserID       *uuid.UUID
	Status       string
	ResourceType string
	ResourceID   string
	From         *time.Time
	To           *time.Time
}
type Result struct {
	Items      []Entry `json:"items"`
	NextCursor string  `json:"next_cursor"`
	Total      int64   `json:"total"`
}

func applyFilters(db *gorm.DB, q Query) *gorm.DB {
	out := db.Where("1 = 1")
	if q.Action != "" {
		out = out.Where("action = ?", q.Action)
	}
	if q.UserID != nil {
		out = out.Where("user_id = ?", q.UserID)
	}
	if q.Status != "" {
		out = out.Where("status = ?", q.Status)
	}
	if q.ResourceType != "" {
		out = out.Where("resource_type = ?", q.ResourceType)
	}
	if q.ResourceID != "" {
		out = out.Where("resource_id = ?", q.ResourceID)
	}
	if q.From != nil {
		out = out.Where("created_at >= ?", *q.From)
	}
	if q.To != nil {
		out = out.Where("created_at <= ?", *q.To)
	}
	return out
}

func (s *Store) Query(q Query) (Result, error) {
	if q.Limit < 1 {
		q.Limit = 50
	}
	if q.Limit > 1000 {
		q.Limit = 1000
	}
	db := applyFilters(s.db.Model(&Entry{}), q)
	if q.Cursor > 0 {
		db = db.Where("id < ?", q.Cursor)
	}
	var items []Entry
	if err := db.Order("id DESC").Limit(q.Limit).Find(&items).Error; err != nil {
		return Result{}, err
	}
	var total int64
	if err := applyFilters(s.db.Model(&Entry{}), q).Count(&total).Error; err != nil {
		return Result{}, err
	}
	next := ""
	if len(items) == q.Limit {
		next = strconv.FormatInt(items[len(items)-1].ID, 10)
	}
	return Result{Items: items, NextCursor: next, Total: total}, nil
}
func (s *Store) Export(w io.Writer, q Query) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"id", "user_id", "action", "resource_type", "resource_id", "ip", "status", "user_agent", "metadata", "created_at"}); err != nil {
		return err
	}
	q.Limit = 1000
	q.Cursor = 0
	for {
		result, err := s.Query(q)
		if err != nil {
			return err
		}
		for _, e := range result.Items {
			uid, ip := "", ""
			if e.UserID != nil {
				uid = e.UserID.String()
			}
			if e.IP != nil {
				ip = *e.IP
			}
			if err = cw.Write([]string{strconv.FormatInt(e.ID, 10), uid, e.Action, e.ResourceType, e.ResourceID, ip, e.Status, e.UserAgent, e.Metadata, e.CreatedAt.UTC().Format(time.RFC3339)}); err != nil {
				return err
			}
		}
		if result.NextCursor == "" {
			break
		}
		q.Cursor, _ = strconv.ParseInt(result.NextCursor, 10, 64)
	}
	cw.Flush()
	return cw.Error()
}
