package http

import (
	"encoding/xml"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDavPathRejectsTraversalAndOutsideRoot(t *testing.T) {
	for _, raw := range []string{"", "/dav", "/webdavx/file", "/webdav/../secret", "/webdav/a%5Cb"} {
		if got, ok := davPath(raw, "/webdav"); ok || got != "" {
			t.Fatalf("davPath(%q) = %q, %v; want rejection", raw, got, ok)
		}
	}
	if got, ok := davPath("/webdav/a%20b", "/webdav"); !ok || got != "a b" {
		t.Fatalf("davPath escaped segment = %q, %v", got, ok)
	}
}

func TestDestinationPathAcceptsAbsoluteURIOnlyOnRequestHost(t *testing.T) {
	r := httptest.NewRequest("MOVE", "https://example.test/webdav/src", nil)
	for _, raw := range []string{"", "https://evil.test/webdav/dst", "/outside/dst", "/webdav/../dst"} {
		if got, ok := destinationPath(raw, "/webdav", r); ok || got != "" {
			t.Fatalf("destinationPath(%q) = %q, %v; want rejection", raw, got, ok)
		}
	}
	for _, raw := range []string{"/webdav/dir/file", "https://example.test/webdav/dir/file"} {
		if got, ok := destinationPath(raw, "/webdav", r); !ok || got != "dir/file" {
			t.Fatalf("destinationPath(%q) = %q, %v", raw, got, ok)
		}
	}
}

func TestWebDAVTokenDTODoesNotExposeSecrets(t *testing.T) {
	body := `{"id":"x","name":"n","created_at":"2026-01-01T00:00:00Z"}`
	for _, forbidden := range []string{"token_hash", "user_id", "revoked_at"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("DTO contains forbidden field %q", forbidden)
		}
	}
}

func TestDAVMultistatusXMLNamespaceAndCollection(t *testing.T) {
	m := davMultistatus{XMLName: xml.Name{Local: "D:multistatus"}, XMLNS: "DAV:", Responses: []davResponse{{
		Href: "/webdav/folder/",
		Propstat: davPropstat{
			Prop: davProp{
				ResourceType: &struct {
					Collection davCollection `xml:"D:collection"`
				}{},
				DisplayName: "folder",
			},
			Status: "HTTP/1.1 200 OK",
		},
	}}}
	body, err := xml.Marshal(m)
	if err != nil {
		t.Fatalf("marshal PROPFIND response: %v", err)
	}

	var decoded struct {
		XMLName xml.Name
	}
	if err := xml.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal PROPFIND response: %v\n%s", err, body)
	}
	if decoded.XMLName.Space != "DAV:" || decoded.XMLName.Local != "multistatus" {
		t.Fatalf("decoded root XML name = %#v, want DAV: multistatus", decoded.XMLName)
	}

	decoder := xml.NewDecoder(strings.NewReader(string(body)))
	seenResponse, seenCollection, seenDisplayName := false, false, false
	for {
		token, err := decoder.Token()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("parse PROPFIND response: %v", err)
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Space != "DAV:" {
			t.Fatalf("element %q has namespace %q, want DAV:", start.Name.Local, start.Name.Space)
		}
		switch start.Name.Local {
		case "response":
			seenResponse = true
		case "collection":
			seenCollection = true
		case "displayname":
			seenDisplayName = true
		}
	}
	if !seenResponse || !seenCollection || !seenDisplayName {
		t.Fatalf("parsed response structure = response:%v collection:%v displayname:%v", seenResponse, seenCollection, seenDisplayName)
	}
}
