package upload

import (
	"archive/zip"
	"errors"
	"io"
	"path/filepath"
	"strings"
)

var ErrInvalidOffice = errors.New("file content does not match Office extension")

// storageReaderAt allows ZIP directory inspection without buffering the upload.
type storageReaderAt struct {
	storage Storage
	key     string
	size    int64
}

func (r storageReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= r.size {
		return 0, io.EOF
	}
	length := int64(len(p))
	if length > r.size-off {
		length = r.size - off
	}
	stream, err := ReadSection(r.storage, r.key, off, length)
	if err != nil {
		return 0, err
	}
	defer stream.Close()
	n, err := io.ReadFull(stream, p)
	return n, err
}

func (s *Service) validateOffice(v UploadSession) error {
	part := map[string]string{".docx": "word/document.xml", ".xlsx": "xl/workbook.xml", ".pptx": "ppt/presentation.xml"}[strings.ToLower(filepath.Ext(v.Name))]
	if part == "" {
		return nil
	}
	reader := storageReaderAt{s.storage, v.StorageKey, v.Size}
	var signature [4]byte
	if _, err := reader.ReadAt(signature[:], 0); err != nil || string(signature[:]) != "PK\x03\x04" {
		return ErrInvalidOffice
	}
	archive, err := zip.NewReader(reader, v.Size)
	if err != nil {
		return ErrInvalidOffice
	}
	found := map[string]bool{}
	for _, entry := range archive.File {
		if entry.Name != "[Content_Types].xml" && entry.Name != part {
			continue
		}
		if found[entry.Name] || entry.UncompressedSize64 == 0 || entry.UncompressedSize64 > 16<<20 {
			return ErrInvalidOffice
		}
		stream, err := entry.Open()
		if err != nil {
			return ErrInvalidOffice
		}
		_, err = io.Copy(io.Discard, io.LimitReader(stream, 16<<20+1))
		stream.Close()
		if err != nil {
			return ErrInvalidOffice
		}
		found[entry.Name] = true
	}
	if !found["[Content_Types].xml"] || !found[part] {
		return ErrInvalidOffice
	}
	return nil
}
