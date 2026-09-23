package files

import (
	"errors"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

var (
	ErrSnapshotNotFound       = errors.New("snapshot not found")
	ErrSnapshotVersionMissing = errors.New("snapshot version no longer exists")
	ErrSnapshotConflict       = errors.New("snapshot restore conflict")
)

type DirectorySnapshot struct {
	ID        uuid.UUID                `json:"id"`
	RootID    uuid.UUID                `json:"root_id"`
	SpaceID   uuid.UUID                `json:"space_id"`
	Name      string                   `json:"name"`
	CreatorID uuid.UUID                `json:"creator_id"`
	CreatedAt time.Time                `json:"created_at"`
	Entries   []DirectorySnapshotEntry `json:"entries,omitempty" gorm:"-"`
}
type DirectorySnapshotEntry struct {
	ID              uuid.UUID  `json:"id"`
	SnapshotID      uuid.UUID  `json:"snapshot_id"`
	RelativePath    string     `json:"relative_path"`
	NodeType        string     `json:"node_type"`
	Name            string     `json:"name"`
	SourceFileID    *uuid.UUID `json:"source_file_id,omitempty"`
	SourceVersionID *uuid.UUID `json:"source_version_id,omitempty"`
	ContentSHA256   string     `json:"content_sha256,omitempty"`
	Size            int64      `json:"size"`
	MimeType        string     `json:"mime_type"`
}
type SnapshotDiff struct {
	Added     []string `json:"added"`
	Removed   []string `json:"removed"`
	Changed   []string `json:"changed"`
	Unchanged []string `json:"unchanged"`
}
type SnapshotRestoreResult struct {
	Restored  []string `json:"restored"`
	Conflicts []string `json:"conflicts"`
	Skipped   []string `json:"skipped"`
}

func snapshotEntries(db *gorm.DB, root uuid.UUID) ([]DirectorySnapshotEntry, error) {
	type node struct {
		F File
		P string
	}
	queue := []node{{P: "", F: File{ID: root}}}
	var out []DirectorySnapshotEntry
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		var children []File
		if err := db.Where("parent_id = ? AND deleted_at IS NULL", cur.F.ID).Order("lower(name), id").Find(&children).Error; err != nil {
			return nil, err
		}
		for _, f := range children {
			rel := f.Name
			if cur.P != "" {
				rel = path.Join(cur.P, f.Name)
			}
			e := DirectorySnapshotEntry{RelativePath: rel, NodeType: f.Type, Name: f.Name, SourceFileID: &f.ID}
			if f.Type == "file" && f.CurrentVersionID != nil {
				var v FileVersion
				if err := db.Where("id = ? AND file_id = ?", *f.CurrentVersionID, f.ID).First(&v).Error; err != nil {
					return nil, err
				}
				var b ObjectBlob
				if err := db.Where("id = ?", v.ObjectBlobID).First(&b).Error; err != nil {
					return nil, err
				}
				e.SourceVersionID, e.ContentSHA256, e.Size, e.MimeType = &v.ID, v.ContentSHA256, v.Size, b.MimeType
			}
			out = append(out, e)
			if f.Type == "folder" {
				queue = append(queue, node{F: f, P: rel})
			}
		}
	}
	return out, nil
}

func (s *Store) authorizeSnapshot(root File, user uuid.UUID, write bool) error {
	if write {
		return authorizeFileWrite(root, user, s.spaceWriter, s.acl)
	}
	return authorizeFileAccess(root, user, s.spaceReader, s.acl)
}

func (s *Store) CreateSnapshot(user, rootID uuid.UUID, name string) (DirectorySnapshot, error) {
	root, err := s.getFolder(rootID)
	if err != nil {
		return DirectorySnapshot{}, err
	}
	if err = s.authorizeSnapshot(root, user, true); err != nil {
		return DirectorySnapshot{}, err
	}
	entries, err := snapshotEntries(s.db, rootID)
	if err != nil {
		return DirectorySnapshot{}, err
	}
	snap := DirectorySnapshot{ID: uuid.New(), RootID: rootID, SpaceID: root.SpaceID, Name: strings.TrimSpace(name), CreatorID: user, CreatedAt: time.Now().UTC()}
	if snap.Name == "" {
		snap.Name = "快照 " + snap.CreatedAt.Format("2006-01-02 15:04:05")
	}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&snap).Error; err != nil {
			return err
		}
		for i := range entries {
			entries[i].ID = uuid.New()
			entries[i].SnapshotID = snap.ID
			if err := tx.Create(&entries[i]).Error; err != nil {
				return err
			}
		}
		return nil
	})
	snap.Entries = entries
	return snap, err
}
func (s *Store) ListSnapshots(user, rootID uuid.UUID) ([]DirectorySnapshot, error) {
	root, err := s.getFolder(rootID)
	if err != nil {
		return nil, err
	}
	if err = s.authorizeSnapshot(root, user, false); err != nil {
		return nil, err
	}
	var out []DirectorySnapshot
	err = s.db.Where("root_id = ?", rootID).Order("created_at DESC").Find(&out).Error
	return out, err
}
func (s *Store) GetSnapshot(user, id uuid.UUID) (DirectorySnapshot, error) {
	var snap DirectorySnapshot
	if err := s.db.Where("id = ?", id).First(&snap).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return snap, ErrSnapshotNotFound
		}
		return snap, err
	}
	root, err := s.getFolder(snap.RootID)
	if err != nil {
		return snap, err
	}
	if err = s.authorizeSnapshot(root, user, false); err != nil {
		return snap, err
	}
	if err = s.db.Where("snapshot_id = ?", id).Order("relative_path").Find(&snap.Entries).Error; err != nil {
		return snap, err
	}
	return snap, nil
}
func snapshotMap(es []DirectorySnapshotEntry) map[string]DirectorySnapshotEntry {
	m := make(map[string]DirectorySnapshotEntry, len(es))
	for _, e := range es {
		m[e.RelativePath] = e
	}
	return m
}
func (s *Store) DiffSnapshot(user, id uuid.UUID) (SnapshotDiff, error) {
	snap, err := s.GetSnapshot(user, id)
	if err != nil {
		return SnapshotDiff{}, err
	}
	cur, err := snapshotEntries(s.db, snap.RootID)
	if err != nil {
		return SnapshotDiff{}, err
	}
	a, b := snapshotMap(snap.Entries), snapshotMap(cur)
	d := SnapshotDiff{}
	for p := range a {
		if _, ok := b[p]; !ok {
			d.Removed = append(d.Removed, p)
		}
	}
	for p, e := range b {
		old, ok := a[p]
		if !ok {
			d.Added = append(d.Added, p)
		} else if old.NodeType != e.NodeType || old.ContentSHA256 != e.ContentSHA256 || old.Size != e.Size {
			d.Changed = append(d.Changed, p)
		} else {
			d.Unchanged = append(d.Unchanged, p)
		}
	}
	sort.Strings(d.Added)
	sort.Strings(d.Removed)
	sort.Strings(d.Changed)
	sort.Strings(d.Unchanged)
	return d, nil
}

func (s *Store) RestoreSnapshotVersionsOnly(user, id uuid.UUID) (SnapshotRestoreResult, error) {
	snap, err := s.GetSnapshot(user, id)
	if err != nil {
		return SnapshotRestoreResult{}, err
	}
	root, err := s.getFolder(snap.RootID)
	if err != nil {
		return SnapshotRestoreResult{}, err
	}
	if err = s.authorizeSnapshot(root, user, true); err != nil {
		return SnapshotRestoreResult{}, err
	}
	cur, err := snapshotEntries(s.db, snap.RootID)
	if err != nil {
		return SnapshotRestoreResult{}, err
	}
	cm := snapshotMap(cur)
	result := SnapshotRestoreResult{}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		for _, e := range snap.Entries {
			if e.NodeType != "file" || e.SourceVersionID == nil {
				continue
			}
			c, ok := cm[e.RelativePath]
			if !ok || c.NodeType != "file" {
				result.Skipped = append(result.Skipped, e.RelativePath)
				continue
			}
			if c.SourceFileID == nil || e.SourceFileID == nil || *c.SourceFileID != *e.SourceFileID {
				result.Conflicts = append(result.Conflicts, e.RelativePath)
				continue
			}
			var target File
			if err := tx.Where("id = ? AND deleted_at IS NULL", *c.SourceFileID).First(&target).Error; err != nil {
				return err
			}
			if err := authorizeFileWrite(target, user, s.spaceWriter, s.acl); err != nil {
				return err
			}
			var v FileVersion
			if err := tx.Where("id = ? AND file_id = ?", *e.SourceVersionID, *e.SourceFileID).First(&v).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrSnapshotVersionMissing
				}
				return err
			}
			update := tx.Model(&File{}).Where("id = ? AND deleted_at IS NULL", *c.SourceFileID).Update("current_version_id", v.ID)
			if update.Error != nil {
				return update.Error
			}
			if update.RowsAffected == 0 {
				return ErrSnapshotConflict
			}
			result.Restored = append(result.Restored, e.RelativePath)
		}
		return nil
	})
	return result, err
}
