package activation

import (
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
)

// Regression/feature test for #479: server-side tag filters on recall.
// passesMetaFilter must support tags_all (AND), tags_any (OR), and a
// tag-prefix value range (e.g. "due:" <= "2026-06-17", lexical compare).
func TestPassesMetaFilter_Tags(t *testing.T) {
	eng := &storage.Engram{Tags: []string{"truth:current", "owner:alice", "due:2026-06-15", "status:open"}}

	cases := []struct {
		name    string
		filters []Filter
		want    bool
	}{
		{"tags_all present", []Filter{{Field: "tags_all", Value: []string{"truth:current", "status:open"}}}, true},
		{"tags_all one missing", []Filter{{Field: "tags_all", Value: []string{"truth:current", "nope"}}}, false},
		{"tags_any one present", []Filter{{Field: "tags_any", Value: []string{"nope", "status:open"}}}, true},
		{"tags_any none present", []Filter{{Field: "tags_any", Value: []string{"a", "b"}}}, false},
		{"tag_prefix lte in range", []Filter{{Field: "tag_prefix", Op: "lte", Value: [2]string{"due:", "2026-06-17"}}}, true},
		{"tag_prefix lte out of range", []Filter{{Field: "tag_prefix", Op: "lte", Value: [2]string{"due:", "2026-06-10"}}}, false},
		{"tag_prefix gte in range", []Filter{{Field: "tag_prefix", Op: "gte", Value: [2]string{"due:", "2026-06-01"}}}, true},
		{"tag_prefix eq", []Filter{{Field: "tag_prefix", Op: "eq", Value: [2]string{"status:", "open"}}}, true},
		{"tag_prefix no matching prefix", []Filter{{Field: "tag_prefix", Op: "lte", Value: [2]string{"missing:", "z"}}}, false},
		{"compose tags_all + prefix", []Filter{
			{Field: "tags_all", Value: []string{"truth:current"}},
			{Field: "tag_prefix", Op: "lte", Value: [2]string{"due:", "2026-06-20"}},
		}, true},
	}
	for _, c := range cases {
		if got := passesMetaFilter(eng, c.filters); got != c.want {
			t.Errorf("%s: passesMetaFilter = %v, want %v", c.name, got, c.want)
		}
	}
}
