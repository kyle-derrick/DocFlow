package files

import "testing"

func TestNormalizeName(t *testing.T) {
	tests := []struct {
		input, want string
		valid       bool
	}{
		{"  cafe\u0301  ", "café", true}, {"", "", false}, {"a/b", "", false}, {"a\\b", "", false}, {"a\n", "", false}, {"report. ", "", false}, {"CON.txt", "", false}, {"normal.txt", "normal.txt", true},
	}
	for _, test := range tests {
		got, err := NormalizeName(test.input)
		if (err == nil) != test.valid || got != test.want {
			t.Errorf("NormalizeName(%q) = %q, %v", test.input, got, err)
		}
	}
}
func TestNormalizeNameRuneLimit(t *testing.T) {
	name := ""
	for i := 0; i < 256; i++ {
		name += "界"
	}
	if _, err := NormalizeName(name); err == nil {
		t.Fatal("expected 256 rune name to be rejected")
	}
}
