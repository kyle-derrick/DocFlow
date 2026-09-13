package http

import "testing"

func TestParseRange(t *testing.T) {
	tests := []struct {
		header string
		size   int64
		want   byteRange
		ok     bool
		err    bool
	}{
		{header: "", size: 100, want: byteRange{}, ok: false, err: false},
		{header: "bytes=0-49", size: 100, want: byteRange{Start: 0, End: 49}, ok: true},
		{header: "bytes=50-", size: 100, want: byteRange{Start: 50, End: 99}, ok: true},
		{header: "bytes=-30", size: 100, want: byteRange{Start: 70, End: 99}, ok: true},
		{header: "bytes=-500", size: 100, want: byteRange{Start: 0, End: 99}, ok: true},
		{header: "bytes=0-0", size: 100, want: byteRange{Start: 0, End: 0}, ok: true},
		{header: "bytes=10-9", size: 100, err: true},
		{header: "bytes=100-", size: 100, err: true},
		{header: "bytes=0-99", size: 100, want: byteRange{Start: 0, End: 99}, ok: true},
		{header: "bytes=0-200", size: 100, want: byteRange{Start: 0, End: 99}, ok: true},
		{header: "bytes=-1", size: 1, want: byteRange{Start: 0, End: 0}, ok: true},
		{header: "bytes=-1", size: 0, err: true},
		{header: "bytes=0-4", size: 0, err: true},
		{header: "bytes=", size: 100, err: true},
		{header: "bytes=5", size: 100, err: true},
		{header: "bytes=a-b", size: 100, err: true},
		{header: "bytes=0-4,10-14", size: 100, err: true},
		{header: "chars=0-4", size: 100, err: true},
		{header: "bytes=--5", size: 100, err: true},
		{header: "bytes=-0", size: 100, err: true},
		{header: "bytes=-2--1", size: 100, err: true},
		{header: "bytes= 20-40 ", size: 100, want: byteRange{Start: 20, End: 40}, ok: true},
	}
	for _, test := range tests {
		got, ok, err := parseRange(test.header, test.size)
		if test.err {
			if err == nil {
				t.Errorf("parseRange(%q, %d) expected error, got %+v", test.header, test.size, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseRange(%q, %d) unexpected error: %v", test.header, test.size, err)
			continue
		}
		if ok != test.ok || got != test.want {
			t.Errorf("parseRange(%q, %d) = %+v, ok=%v, want %+v, ok=%v", test.header, test.size, got, ok, test.want, test.ok)
		}
	}
}

func TestParseRangeSuffixClampsToZero(t *testing.T) {
	got, ok, err := parseRange("bytes=-0", 50)
	if err == nil || ok {
		t.Fatalf("bytes=-0 should be rejected, got %+v ok=%v err=%v", got, ok, err)
	}
	got, ok, err = parseRange("bytes=-100", 50)
	if err != nil || !ok || got.Start != 0 || got.End != 49 {
		t.Fatalf("bytes=-100 of 50 should clamp to 0-49, got %+v ok=%v err=%v", got, ok, err)
	}
}

func TestByteRangeLength(t *testing.T) {
	if (byteRange{Start: 0, End: 0}).Length() != 1 {
		t.Fatal("single byte range length must be 1")
	}
	if (byteRange{Start: 10, End: 49}).Length() != 40 {
		t.Fatal("range length mismatch")
	}
}

func TestContentDisposition(t *testing.T) {
	tests := []struct {
		filename string
		want     string
	}{
		{"report.pdf", `attachment; filename="report.pdf"`},
		{"my report.pdf", `attachment; filename="my report.pdf"`},
		{"年度报告.pdf", `attachment; filename="____.pdf"; filename*=UTF-8''%E5%B9%B4%E5%BA%A6%E6%8A%A5%E5%91%8A.pdf`},
		{`weird"name.txt`, `attachment; filename="weird\"name.txt"; filename*=UTF-8''weird%22name.txt`},
		{`back\slash.txt`, `attachment; filename="back\\slash.txt"; filename*=UTF-8''back%5Cslash.txt`},
		{"emoji-🚀.zip", `attachment; filename="emoji-_.zip"; filename*=UTF-8''emoji-%F0%9F%9A%80.zip`},
	}
	for _, test := range tests {
		if got := contentDisposition(test.filename); got != test.want {
			t.Errorf("contentDisposition(%q) = %q, want %q", test.filename, got, test.want)
		}
	}
}

func TestContentDispositionHeaderSafe(t *testing.T) {
	for _, name := range []string{"正常文件.txt", "quote\".bin", "semi;colon.doc", "nl\nx", "cr\rx"} {
		value := contentDisposition(name)
		for i := 0; i < len(value); i++ {
			if value[i] < 0x20 || value[i] == 0x7f {
				t.Fatalf("contentDisposition(%q) contains control byte: %q", name, value)
			}
		}
	}
}
