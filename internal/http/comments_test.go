package http

import (
	"strings"
	"testing"
)

func TestCommentValidation(t *testing.T) {
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{"hello", true}, {"  \n", false}, {strings.Repeat("界", 4000), true}, {strings.Repeat("界", 4001), false},
	} {
		if got := validCommentBody(tc.body); got != tc.valid {
			t.Errorf("body length %d: got %v", len(tc.body), got)
		}
	}
	for _, tc := range []struct {
		from, to int
		quote    string
		valid    bool
	}{
		{1, 3, "hi", true}, {-1, 2, "", false}, {2, 2, "", false}, {3, 2, "", false}, {0, 1000001, "", false}, {0, 1, strings.Repeat("x", 1001), false},
	} {
		if got := validAnchor(tc.from, tc.to, tc.quote); got != tc.valid {
			t.Errorf("anchor %+v: got %v", tc, got)
		}
	}
}
