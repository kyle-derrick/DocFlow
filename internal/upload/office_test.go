package upload

import (
	"archive/zip"
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func officeArchive(t *testing.T, parts ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for _, part := range parts {
		entry, err := w.Create(part)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = entry.Write([]byte("<root/>")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestOfficeUploadValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		data  []byte
		valid bool
	}{
		{"bad.docx", []byte("not zip"), false},
		{"bad.xlsx", officeArchive(t, "[Content_Types].xml", "word/document.xml"), false},
		{"bad.pptx", officeArchive(t, "ppt/presentation.xml"), false},
		{"bad.docx", officeArchive(t, "[Content_Types].xml"), false},
		{"ok.DOCX", officeArchive(t, "[Content_Types].xml", "word/document.xml"), true},
		{"ok.xlsx", officeArchive(t, "[Content_Types].xml", "xl/workbook.xml"), true},
		{"ok.pptx", officeArchive(t, "[Content_Types].xml", "ppt/presentation.xml"), true},
		{"ordinary.zip", officeArchive(t, "readme.txt"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storage, err := NewLocalStorage(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			created := false
			svc := NewService(NewMemoryStore(), storage, time.Hour, 1<<20, false, nil, func(user, parent uuid.UUID, name, key string, size int64, hash, mime string) (uuid.UUID, bool, error) {
				created = true
				return uuid.New(), true, nil
			})
			v, err := svc.Start(uuid.New(), uuid.New(), tc.name, int64(len(tc.data)), "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = svc.Append(v.ID, 0, bytes.NewReader(tc.data)); err != nil {
				t.Fatal(err)
			}
			result, err := svc.Complete(v.ID)
			if tc.valid {
				if err != nil || !created || result.Status != StatusAvailable {
					t.Fatalf("valid: %v %+v created=%v", err, result, created)
				}
			} else {
				if !errors.Is(err, ErrInvalidOffice) || created || result.Status != StatusFailed {
					t.Fatalf("invalid: %v %+v created=%v", err, result, created)
				}
				if stream, err := storage.Read(v.StorageKey); err == nil {
					stream.Close()
					t.Fatal("invalid temporary content retained")
				}
			}
		})
	}
}
