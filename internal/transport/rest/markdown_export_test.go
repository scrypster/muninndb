package rest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"strings"
	"testing"
)

type markdownEngine struct {
	MockEngine
}

func (m *markdownEngine) ListEngrams(_ context.Context, req *ListEngramsRequest) (*ListEngramsResponse, error) {
	return &ListEngramsResponse{
		Engrams: []EngramItem{
			{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Concept: "Alpha Note", Vault: req.Vault},
			{ID: "01ARZ3NDEKTSV4RRFFQ69G5FB0", Concept: "Beta Note", Vault: req.Vault},
		},
		Total:  2,
		Limit:  req.Limit,
		Offset: req.Offset,
	}, nil
}

func (m *markdownEngine) Read(_ context.Context, req *ReadRequest) (*ReadResponse, error) {
	switch req.ID {
	case "01ARZ3NDEKTSV4RRFFQ69G5FAV":
		return &ReadResponse{
			ID:         req.ID,
			Concept:    "Alpha Note",
			Content:    "Alpha content",
			Confidence: 0.8,
			Tags:       []string{"project", "alpha"},
			State:      1,
			CreatedAt:  1700000000,
			UpdatedAt:  1700000100,
			LastAccess: 1700000200,
			MemoryType: 1,
		}, nil
	case "01ARZ3NDEKTSV4RRFFQ69G5FB0":
		return &ReadResponse{
			ID:         req.ID,
			Concept:    "Beta Note",
			Content:    "Beta content",
			Confidence: 0.7,
			Tags:       []string{"project"},
			State:      1,
			CreatedAt:  1700000300,
			UpdatedAt:  1700000400,
			LastAccess: 1700000500,
			MemoryType: 0,
		}, nil
	}
	return &ReadResponse{ID: req.ID, Concept: req.ID, Content: ""}, nil
}

func (m *markdownEngine) GetEngramLinks(_ context.Context, req *GetEngramLinksRequest) (*GetEngramLinksResponse, error) {
	if req.ID == "01ARZ3NDEKTSV4RRFFQ69G5FAV" {
		return &GetEngramLinksResponse{
			Links: []AssociationItem{
				{
					TargetID: "01ARZ3NDEKTSV4RRFFQ69G5FB0",
					RelType:  5,
					Weight:   0.9,
				},
			},
		}, nil
	}
	return &GetEngramLinksResponse{Links: []AssociationItem{}}, nil
}

func TestWriteVaultMarkdownExport(t *testing.T) {
	var out bytes.Buffer
	n, err := writeVaultMarkdownExport(context.Background(), &markdownEngine{}, "default", &out)
	if err != nil {
		t.Fatalf("writeVaultMarkdownExport: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 exported notes, got %d", n)
	}

	gz, err := gzip.NewReader(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	files := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read tar entry %s: %v", hdr.Name, err)
		}
		files[hdr.Name] = string(body)
	}

	if _, ok := files["index.md"]; !ok {
		t.Fatal("missing index.md")
	}
	if _, ok := files["tags.md"]; !ok {
		t.Fatal("missing tags.md")
	}

	alphaFile := "notes/alpha-note--01ARZ3NDEKTSV4RRFFQ69G5FAV.md"
	alphaBody, ok := files[alphaFile]
	if !ok {
		t.Fatalf("missing %s", alphaFile)
	}
	if !strings.Contains(alphaBody, "tags:") || !strings.Contains(alphaBody, "project") {
		t.Fatalf("alpha markdown missing tags front matter: %s", alphaBody)
	}
	if !strings.Contains(alphaBody, "`relates_to`") || !strings.Contains(alphaBody, "beta-note--01ARZ3NDEKTSV4RRFFQ69G5FB0.md") {
		t.Fatalf("alpha markdown missing link to beta: %s", alphaBody)
	}
}
