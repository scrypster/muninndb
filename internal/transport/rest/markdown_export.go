package rest

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

type markdownNote struct {
	Read  *ReadResponse
	Links []AssociationItem
}

// writeVaultMarkdownExport streams a tar.gz archive containing markdown notes
// for one vault.
func writeVaultMarkdownExport(ctx context.Context, eng EngineAPI, vault string, w io.Writer) (int, error) {
	notes, err := collectMarkdownNotes(ctx, eng, vault)
	if err != nil {
		return 0, err
	}

	sort.Slice(notes, func(i, j int) bool {
		if notes[i].Read.CreatedAt != notes[j].Read.CreatedAt {
			return notes[i].Read.CreatedAt < notes[j].Read.CreatedAt
		}
		return notes[i].Read.ID < notes[j].Read.ID
	})

	filenames := make(map[string]string, len(notes))
	for _, note := range notes {
		id := note.Read.ID
		filenames[id] = noteFileName(note.Read.Concept, id)
	}

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	now := time.Now().UTC()

	// index.md
	indexBody := renderMarkdownIndex(vault, notes, filenames)
	if err := writeTarTextFile(tw, "index.md", indexBody, now); err != nil {
		return 0, err
	}

	// tags.md
	tagsBody := renderMarkdownTags(vault, notes, filenames)
	if err := writeTarTextFile(tw, "tags.md", tagsBody, now); err != nil {
		return 0, err
	}

	// per-note files
	for _, note := range notes {
		name := "notes/" + filenames[note.Read.ID]
		body := renderMarkdownNote(vault, note, filenames, notes)
		if err := writeTarTextFile(tw, name, body, now); err != nil {
			return 0, err
		}
	}

	if err := tw.Close(); err != nil {
		return 0, fmt.Errorf("markdown export: tar close: %w", err)
	}
	if err := gz.Close(); err != nil {
		return 0, fmt.Errorf("markdown export: gzip close: %w", err)
	}
	return len(notes), nil
}

func collectMarkdownNotes(ctx context.Context, eng EngineAPI, vault string) ([]*markdownNote, error) {
	const pageSize = 100
	offset := 0
	total := -1
	var out []*markdownNote

	for total == -1 || offset < total {
		page, err := eng.ListEngrams(ctx, &ListEngramsRequest{
			Vault:  vault,
			Limit:  pageSize,
			Offset: offset,
			Sort:   "created",
		})
		if err != nil {
			return nil, fmt.Errorf("markdown export: list engrams: %w", err)
		}
		total = page.Total
		if len(page.Engrams) == 0 {
			break
		}

		for _, item := range page.Engrams {
			read, err := eng.Read(ctx, &ReadRequest{ID: item.ID, Vault: vault})
			if err != nil {
				return nil, fmt.Errorf("markdown export: read engram %s: %w", item.ID, err)
			}
			links, err := eng.GetEngramLinks(ctx, &GetEngramLinksRequest{ID: item.ID, Vault: vault})
			if err != nil {
				return nil, fmt.Errorf("markdown export: get links for %s: %w", item.ID, err)
			}
			out = append(out, &markdownNote{Read: read, Links: links.Links})
		}
		offset += len(page.Engrams)
	}

	return out, nil
}

func writeTarTextFile(tw *tar.Writer, name, body string, modTime time.Time) error {
	b := []byte(body)
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0644,
		Size:     int64(len(b)),
		ModTime:  modTime,
		Typeflag: tar.TypeReg,
	}); err != nil {
		return fmt.Errorf("markdown export: tar header %s: %w", name, err)
	}
	if _, err := tw.Write(b); err != nil {
		return fmt.Errorf("markdown export: tar write %s: %w", name, err)
	}
	return nil
}

func renderMarkdownIndex(vault string, notes []*markdownNote, filenames map[string]string) string {
	var b strings.Builder
	b.WriteString("# Vault Export: " + vault + "\n\n")
	b.WriteString(fmt.Sprintf("Total notes: %d\n\n", len(notes)))
	b.WriteString("| Concept | ID | File |\n")
	b.WriteString("|---|---|---|\n")
	for _, note := range notes {
		id := note.Read.ID
		concept := markdownInline(note.Read.Concept)
		file := "notes/" + filenames[id]
		b.WriteString(fmt.Sprintf("| %s | `%s` | [%s](%s) |\n", concept, id, filenames[id], file))
	}
	return b.String()
}

func renderMarkdownTags(vault string, notes []*markdownNote, filenames map[string]string) string {
	tagToIDs := make(map[string][]string)
	for _, note := range notes {
		for _, tag := range note.Read.Tags {
			tagToIDs[tag] = append(tagToIDs[tag], note.Read.ID)
		}
	}

	tags := make([]string, 0, len(tagToIDs))
	for tag := range tagToIDs {
		tags = append(tags, tag)
	}
	sort.Strings(tags)

	var b strings.Builder
	b.WriteString("# Tags: " + vault + "\n\n")
	if len(tags) == 0 {
		b.WriteString("No tags found.\n")
		return b.String()
	}

	concepts := noteConceptMap(notes)
	for _, tag := range tags {
		ids := tagToIDs[tag]
		sort.Slice(ids, func(i, j int) bool {
			ci := concepts[ids[i]]
			cj := concepts[ids[j]]
			if ci != cj {
				return ci < cj
			}
			return ids[i] < ids[j]
		})
		b.WriteString("## #" + markdownInline(tag) + "\n\n")
		for _, id := range ids {
			name := filenames[id]
			b.WriteString(fmt.Sprintf("- [%s](notes/%s) (`%s`)\n", markdownInline(concepts[id]), name, id))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func renderMarkdownNote(vault string, note *markdownNote, filenames map[string]string, all []*markdownNote) string {
	r := note.Read
	concepts := noteConceptMap(all)
	var b strings.Builder

	b.WriteString("---\n")
	writeYAMLKV(&b, "id", r.ID)
	writeYAMLKV(&b, "vault", vault)
	writeYAMLKV(&b, "concept", r.Concept)
	writeYAMLKV(&b, "created_at", unixToRFC3339(r.CreatedAt))
	writeYAMLKV(&b, "updated_at", unixToRFC3339(r.UpdatedAt))
	writeYAMLKV(&b, "last_access", unixToRFC3339(r.LastAccess))
	writeYAMLKV(&b, "state", lifecycleStateLabelFromCode(r.State))
	writeYAMLKV(&b, "confidence", fmt.Sprintf("%.6f", r.Confidence))
	writeYAMLKV(&b, "relevance", fmt.Sprintf("%.6f", r.Relevance))
	writeYAMLKV(&b, "stability", fmt.Sprintf("%.6f", r.Stability))
	writeYAMLKV(&b, "access_count", fmt.Sprintf("%d", r.AccessCount))
	writeYAMLKV(&b, "memory_type", memoryTypeLabelFromCode(r.MemoryType))
	writeYAMLKV(&b, "type_label", r.TypeLabel)
	writeYAMLKV(&b, "classification", fmt.Sprintf("%d", r.Classification))

	b.WriteString("tags:\n")
	if len(r.Tags) == 0 {
		b.WriteString("  []\n")
	} else {
		for _, tag := range r.Tags {
			b.WriteString("  - " + yamlQuote(tag) + "\n")
		}
	}
	b.WriteString("---\n\n")

	b.WriteString("# " + markdownInline(r.Concept) + "\n\n")

	if r.Summary != "" {
		b.WriteString("## Summary\n\n")
		b.WriteString(r.Summary + "\n\n")
	}

	b.WriteString("## Content\n\n")
	b.WriteString(r.Content + "\n\n")

	b.WriteString("## Links\n\n")
	if len(note.Links) == 0 {
		b.WriteString("- None\n")
		return b.String()
	}

	sort.Slice(note.Links, func(i, j int) bool {
		if note.Links[i].RelType != note.Links[j].RelType {
			return note.Links[i].RelType < note.Links[j].RelType
		}
		return note.Links[i].TargetID < note.Links[j].TargetID
	})

	for _, link := range note.Links {
		targetID := link.TargetID
		rel := relTypeLabelFromCode(link.RelType)
		targetName := concepts[targetID]
		targetFile, ok := filenames[targetID]
		if !ok {
			b.WriteString(fmt.Sprintf("- `%s` (weight=%.3f) -> `%s`\n", rel, link.Weight, targetID))
			continue
		}
		label := targetName
		if label == "" {
			label = targetID
		}
		b.WriteString(fmt.Sprintf("- `%s` (weight=%.3f) -> [%s](%s)\n", rel, link.Weight, markdownInline(label), targetFile))
	}
	return b.String()
}

func noteConceptMap(notes []*markdownNote) map[string]string {
	out := make(map[string]string, len(notes))
	for _, n := range notes {
		out[n.Read.ID] = n.Read.Concept
	}
	return out
}

func noteFileName(concept, id string) string {
	slug := slugify(concept)
	return slug + "--" + id + ".md"
}

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "note"
	}
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "note"
	}
	return out
}

func markdownInline(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

func writeYAMLKV(b *strings.Builder, key, value string) {
	b.WriteString(key)
	b.WriteString(": ")
	b.WriteString(yamlQuote(value))
	b.WriteByte('\n')
}

func yamlQuote(s string) string {
	return strconv.Quote(s)
}

func unixToRFC3339(ts int64) string {
	if ts <= 0 {
		return ""
	}
	return time.Unix(ts, 0).UTC().Format(time.RFC3339)
}

func lifecycleStateLabelFromCode(state uint8) string {
	switch state {
	case 0:
		return "planning"
	case 1:
		return "active"
	case 2:
		return "paused"
	case 3:
		return "blocked"
	case 4:
		return "completed"
	case 5:
		return "cancelled"
	case 6:
		return "archived"
	case 127:
		return "soft_deleted"
	default:
		return fmt.Sprintf("unknown(%d)", state)
	}
}

func memoryTypeLabelFromCode(t uint8) string {
	switch t {
	case 0:
		return "fact"
	case 1:
		return "decision"
	case 2:
		return "observation"
	case 3:
		return "preference"
	case 4:
		return "issue"
	case 5:
		return "task"
	case 6:
		return "procedure"
	case 7:
		return "event"
	case 8:
		return "goal"
	case 9:
		return "constraint"
	case 10:
		return "identity"
	case 11:
		return "reference"
	default:
		return "fact"
	}
}

func relTypeLabelFromCode(t uint16) string {
	switch t {
	case 1:
		return "supports"
	case 2:
		return "contradicts"
	case 3:
		return "depends_on"
	case 4:
		return "supersedes"
	case 5:
		return "relates_to"
	case 6:
		return "is_part_of"
	case 7:
		return "causes"
	case 8:
		return "preceded_by"
	case 9:
		return "followed_by"
	case 10:
		return "created_by_person"
	case 11:
		return "belongs_to_project"
	case 12:
		return "references"
	case 13:
		return "implements"
	case 14:
		return "blocks"
	case 15:
		return "resolves"
	case 16:
		return "refines"
	default:
		if t >= 0x8000 {
			return "user_defined"
		}
		return fmt.Sprintf("rel_%d", t)
	}
}
