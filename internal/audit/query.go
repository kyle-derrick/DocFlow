package audit

import (
	"encoding/csv"
	"io"
	"strconv"
	"time"

	"github.com/google/uuid"
)

type Query struct {
	Limit  int
	Cursor int64
	Action string
	UserID *uuid.UUID
}
type Result struct {
	Items      []Entry `json:"items"`
	NextCursor string  `json:"next_cursor"`
	Total      int64   `json:"total"`
}

func (s *Store) Query(q Query) (Result, error) {
	if q.Limit < 1 {
		q.Limit = 50
	}
	if q.Limit > 1000 {
		q.Limit = 1000
	}
	db := s.db.Model(&Entry{})
	if q.Cursor > 0 {
		db = db.Where("id < ?", q.Cursor)
	}
	if q.Action != "" {
		db = db.Where("action = ?", q.Action)
	}
	if q.UserID != nil {
		db = db.Where("user_id = ?", q.UserID)
	}
	var items []Entry
	if err := db.Order("id DESC").Limit(q.Limit).Find(&items).Error; err != nil {
		return Result{}, err
	}
	var total int64
	count := s.db.Model(&Entry{})
	if q.Action != "" {
		count = count.Where("action = ?", q.Action)
	}
	if q.UserID != nil {
		count = count.Where("user_id = ?", q.UserID)
	}
	if err := count.Count(&total).Error; err != nil {
		return Result{}, err
	}
	next := ""
	if len(items) == q.Limit {
		next = strconv.FormatInt(items[len(items)-1].ID, 10)
	}
	return Result{Items: items, NextCursor: next, Total: total}, nil
}
func (s *Store) Export(w io.Writer, q Query) error {
	result, err := s.Query(Query{Limit: 1000, Action: q.Action, UserID: q.UserID})
	if err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	if err = cw.Write([]string{"id", "user_id", "action", "resource_type", "resource_id", "status", "created_at"}); err != nil {
		return err
	}
	for _, e := range result.Items {
		uid := ""
		if e.UserID != nil {
			uid = e.UserID.String()
		}
		if err = cw.Write([]string{strconv.FormatInt(e.ID, 10), uid, e.Action, e.ResourceType, e.ResourceID, e.Status, e.CreatedAt.UTC().Format(time.RFC3339)}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
