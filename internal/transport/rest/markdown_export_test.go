package rest

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// markdownExportEngine is an EngineAPI that returns a fixed set of engrams
// for markdown export tests. Soft-deleted engrams (state=127) are included
// in the list to verify they are filtered out.
type markdownExportEngine struct {
	MockEngine
}

func (e *markdownExportEngine) ListEngrams(_ context.Context, req *ListEngramsRequest) (*ListEngramsResponse, error) {
	if req.Offset > 0 {
		return &ListEngramsResponse{Engrams: nil, Total: 2}, nil
	}
	return &ListEngramsResponse{
		Engrams: []EngramItem{
			{ID: "01ABCDEF01234567", Concept: "Active Note", Vault: req.Vault},
			{ID: "01ABCDEF89ABCDEF", Concept: "Deleted Note", Vault: req.Vault},
		},
		Total: 2,
	}, nil
}

func (e *markdownExportEngine) Read(_ context.Context, req *ReadRequest) (*ReadResponse, error) {
	switch req.ID {
	case "01ABCDEF01234567":
		return &ReadResponse{
			ID:         "01ABCDEF01234567",
			Concept:    "Active Note",
			Content:    "This is active content.",
			Tags:       []string{"go", "test"},
			MemoryType: 0, // fact
			State:      1, // active
			Confidence: 0.9,
			CreatedAt:  1700000000,
		}, nil
	case "01ABCDEF89ABCDEF":
		return &ReadResponse{
			ID:      "01ABCDEF89ABCDEF",
			Concept: "Deleted Note",
			Content: "This should not appear.",
			State:   127, // soft-deleted
		}, nil
	}
	return &ReadResponse{}, nil
}

func (e *markdownExportEngine) GetEngramLinks(_ context.Context, req *GetEngramLinksRequest) (*GetEngramLinksResponse, error) {
	if req.ID == "01ABCDEF01234567" {
		return &GetEngramLinksResponse{
			Links: []AssociationItem{
				{TargetID: "01ABCDEF89ABCDEF", RelType: 0x0001, Weight: 0.7},
			},
		}, nil
	}
	return &GetEngramLinksResponse{}, nil
}

func (e *markdownExportEngine) GetBatchEngramLinks(_ context.Context, _ *BatchGetEngramLinksRequest) (*BatchGetEngramLinksResponse, error) {
	return &BatchGetEngramLinksResponse{Links: map[string][]AssociationItem{}}, nil
}

// ---------------------------------------------------------------------------
// writeVaultMarkdownExport
// ---------------------------------------------------------------------------

func TestWriteVaultMarkdownExport_ContainsExpectedFiles(t *testing.T) {
	eng := &markdownExportEngine{}
	var buf strings.Builder
	w := &nopWriteCloser{&buf}
	_ = w

	var rawBuf strings.Builder
	err := writeVaultMarkdownExport(context.Background(), eng, "default", &rawBuf)
	if err != nil {
		t.Fatalf("writeVaultMarkdownExport: %v", err)
	}

	// Decompress and list tar entries.
	gr, err := gzip.NewReader(strings.NewReader(rawBuf.String()))
	if err != nil {
		t.Fatalf("gzip open: %v", err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)

	var files []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		files = append(files, hdr.Name)
	}

	want := map[string]bool{
		"index.md": false,
		"tags.md":  false,
	}
	notesSeen := 0
	for _, f := range files {
		if f == "index.md" || f == "tags.md" {
			want[f] = true
		}
		if strings.HasPrefix(f, "notes/") && strings.HasSuffix(f, ".md") {
			notesSeen++
		}
	}
	for k, v := range want {
		if !v {
			t.Errorf("expected file %q in archive, not found; got: %v", k, files)
		}
	}
	// Only 1 active engram; the soft-deleted one must be excluded.
	if notesSeen != 1 {
		t.Errorf("expected 1 note file (soft-deleted excluded), got %d; files: %v", notesSeen, files)
	}
}

// nopWriteCloser is a strings.Builder + io.WriteCloser for use in tests.
type nopWriteCloser struct{ *strings.Builder }

func (n *nopWriteCloser) Close() error { return nil }

// ---------------------------------------------------------------------------
// slugify
// ---------------------------------------------------------------------------

func TestSlugify(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"Hello World", "hello-world"},
		{"foo--bar", "foo-bar"},
		{"  spaces  ", "spaces"},
		{"", "engram"},
		{"123 facts!", "123-facts"},
		{"café au lait", "caf-au-lait"},
	}
	for _, tc := range cases {
		got := slugify(tc.input)
		if got != tc.want {
			t.Errorf("slugify(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// lifecycleStateLabelFromCode
// ---------------------------------------------------------------------------

func TestLifecycleStateLabelFromCode(t *testing.T) {
	cases := []struct {
		code uint8
		want string
	}{
		{0, "planning"},
		{1, "active"},
		{2, "paused"},
		{3, "blocked"},
		{4, "completed"},
		{5, "cancelled"},
		{6, "archived"},
		{127, "soft_deleted"},
		{99, "unknown(99)"},
	}
	for _, tc := range cases {
		got := lifecycleStateLabelFromCode(tc.code)
		if got != tc.want {
			t.Errorf("lifecycleStateLabelFromCode(%d) = %q, want %q", tc.code, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// memoryTypeLabelFromCode
// ---------------------------------------------------------------------------

func TestMemoryTypeLabelFromCode(t *testing.T) {
	cases := []struct {
		code uint8
		want string
	}{
		{0, "fact"},
		{1, "decision"},
		{2, "observation"},
		{3, "preference"},
		{4, "issue"},
		{5, "task"},
		{6, "procedure"},
		{7, "event"},
		{8, "goal"},
		{9, "constraint"},
		{10, "identity"},
		{11, "reference"},
		{99, "unknown(99)"},
	}
	for _, tc := range cases {
		got := memoryTypeLabelFromCode(tc.code)
		if got != tc.want {
			t.Errorf("memoryTypeLabelFromCode(%d) = %q, want %q", tc.code, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// relTypeLabelFromCode
// ---------------------------------------------------------------------------

func TestRelTypeLabelFromCode(t *testing.T) {
	cases := []struct {
		code uint16
		want string
	}{
		{0x0001, "supports"},
		{0x0002, "contradicts"},
		{0x0003, "depends_on"},
		{0x0004, "supersedes"},
		{0x0005, "relates_to"},
		{0x0006, "is_part_of"},
		{0x0007, "causes"},
		{0x0008, "preceded_by"},
		{0x0009, "followed_by"},
		{0x000A, "created_by_person"},
		{0x000B, "belongs_to_project"},
		{0x000C, "references"},
		{0x000D, "implements"},
		{0x000E, "blocks"},
		{0x000F, "resolves"},
		{0x0010, "refines"},
		{0x8000, "user_defined"},
		{0x9999, "rel(39321)"},
	}
	for _, tc := range cases {
		got := relTypeLabelFromCode(tc.code)
		if got != tc.want {
			t.Errorf("relTypeLabelFromCode(0x%04X) = %q, want %q", tc.code, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// noteFileName
// ---------------------------------------------------------------------------

func TestNoteFileName(t *testing.T) {
	n := markdownNote{
		id:      "01HZ0000000000ABCDEF",
		concept: "My Test Concept",
	}
	got := noteFileName(n)
	if !strings.HasPrefix(got, "my-test-concept-") {
		t.Errorf("expected prefix 'my-test-concept-', got %q", got)
	}
	if !strings.HasSuffix(got, ".md") {
		t.Errorf("expected .md suffix, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// renderIndexMD / renderTagsMD / renderNoteMD
// ---------------------------------------------------------------------------

func TestRenderIndexMD(t *testing.T) {
	notes := []markdownNote{
		{id: "abc", concept: "First Note", state: 1},
		{id: "def", concept: "Second Note", state: 4},
	}
	out := renderIndexMD("testvault", notes)
	if !strings.Contains(out, "# Vault: testvault") {
		t.Errorf("missing vault heading: %q", out)
	}
	if !strings.Contains(out, "First Note") {
		t.Errorf("missing first note: %q", out)
	}
	if !strings.Contains(out, "Total engrams: 2") {
		t.Errorf("missing engram count: %q", out)
	}
}

func TestRenderTagsMD_Empty(t *testing.T) {
	out := renderTagsMD(map[string]int{})
	if !strings.Contains(out, "No tags") {
		t.Errorf("expected no-tags message: %q", out)
	}
}

func TestRenderTagsMD_WithTags(t *testing.T) {
	out := renderTagsMD(map[string]int{"go": 3, "test": 1})
	if !strings.Contains(out, "go") {
		t.Errorf("expected tag 'go': %q", out)
	}
}

func TestRenderNoteMD(t *testing.T) {
	n := markdownNote{
		id:         "abc123",
		concept:    "Test Concept",
		content:    "Some content here.",
		summary:    "A brief summary.",
		tags:       []string{"alpha", "beta"},
		memType:    1,
		state:      1,
		confidence: 0.85,
		createdAt:  1700000000,
		links: []AssociationItem{
			{TargetID: "xyz999", RelType: 0x0001, Weight: 0.6},
		},
	}
	out := renderNoteMD(n)
	if !strings.Contains(out, "# Test Concept") {
		t.Errorf("missing heading: %q", out)
	}
	if !strings.Contains(out, "type: decision") {
		t.Errorf("missing type: %q", out)
	}
	if !strings.Contains(out, "state: active") {
		t.Errorf("missing state: %q", out)
	}
	if !strings.Contains(out, "Some content here.") {
		t.Errorf("missing content: %q", out)
	}
	if !strings.Contains(out, "A brief summary.") {
		t.Errorf("missing summary: %q", out)
	}
	if !strings.Contains(out, "supports") {
		t.Errorf("missing link rel type: %q", out)
	}
}
