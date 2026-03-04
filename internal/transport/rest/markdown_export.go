package rest

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// writeVaultMarkdownExport streams a .tgz archive to w containing:
//   - index.md  — vault summary
//   - tags.md   — all tags with engram counts
//   - notes/<slug>-<id>.md — one file per engram (soft-deleted excluded)
func writeVaultMarkdownExport(ctx context.Context, eng EngineAPI, vault string, w io.Writer) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	notes, err := collectMarkdownNotes(ctx, eng, vault)
	if err != nil {
		return fmt.Errorf("markdown export: collect notes: %w", err)
	}

	// Build tag → engram-count index.
	tagCounts := make(map[string]int)
	for _, n := range notes {
		for _, tag := range n.tags {
			tagCounts[tag]++
		}
	}

	// Write index.md
	indexMD := renderIndexMD(vault, notes)
	if err := writeTarEntry(tw, "index.md", indexMD); err != nil {
		return err
	}

	// Write tags.md
	tagsMD := renderTagsMD(tagCounts)
	if err := writeTarEntry(tw, "tags.md", tagsMD); err != nil {
		return err
	}

	// Write one file per engram.
	for _, n := range notes {
		filename := "notes/" + noteFileName(n)
		body := renderNoteMD(n)
		if err := writeTarEntry(tw, filename, body); err != nil {
			return err
		}
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("markdown export: tar close: %w", err)
	}
	return gz.Close()
}

// markdownNote holds all data needed to render a single engram as markdown.
type markdownNote struct {
	id         string
	concept    string
	content    string
	summary    string
	tags       []string
	memType    uint8
	state      uint8
	confidence float32
	createdAt  int64
	updatedAt  int64
	links      []AssociationItem
}

// collectMarkdownNotes pages through all engrams in vault and returns non-deleted notes.
func collectMarkdownNotes(ctx context.Context, eng EngineAPI, vault string) ([]markdownNote, error) {
	const pageSize = 100
	var notes []markdownNote
	offset := 0

	for {
		resp, err := eng.ListEngrams(ctx, &ListEngramsRequest{
			Vault:  vault,
			Limit:  pageSize,
			Offset: offset,
			Sort:   "created",
		})
		if err != nil {
			return nil, fmt.Errorf("markdown export: list engrams: %w", err)
		}
		if len(resp.Engrams) == 0 {
			break
		}

		for _, item := range resp.Engrams {
			read, err := eng.Read(ctx, &ReadRequest{ID: item.ID, Vault: vault})
			if err != nil {
				return nil, fmt.Errorf("markdown export: read engram %s: %w", item.ID, err)
			}
			// Skip soft-deleted engrams — state 127 = StateSoftDeleted.
			if read.State == 127 {
				continue
			}

			linksResp, _ := eng.GetEngramLinks(ctx, &GetEngramLinksRequest{ID: item.ID, Vault: vault})
			var links []AssociationItem
			if linksResp != nil {
				links = linksResp.Links
			}

			notes = append(notes, markdownNote{
				id:         read.ID,
				concept:    read.Concept,
				content:    read.Content,
				summary:    read.Summary,
				tags:       read.Tags,
				memType:    read.MemoryType,
				state:      read.State,
				confidence: read.Confidence,
				createdAt:  read.CreatedAt,
				updatedAt:  read.UpdatedAt,
				links:      links,
			})
		}

		offset += len(resp.Engrams)
		if offset >= resp.Total {
			break
		}
	}

	return notes, nil
}

// writeTarEntry writes content as a tar file entry.
func writeTarEntry(tw *tar.Writer, name, content string) error {
	data := []byte(content)
	hdr := &tar.Header{
		Name:    name,
		Mode:    0644,
		Size:    int64(len(data)),
		ModTime: time.Now(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("markdown export: write tar header %s: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("markdown export: write tar data %s: %w", name, err)
	}
	return nil
}

// renderIndexMD builds the index.md summary document.
func renderIndexMD(vault string, notes []markdownNote) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Vault: %s\n\n", vault)
	fmt.Fprintf(&sb, "Exported: %s\n\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&sb, "Total engrams: %d\n\n", len(notes))
	sb.WriteString("## Engrams\n\n")
	for _, n := range notes {
		fname := noteFileName(n)
		fmt.Fprintf(&sb, "- [%s](notes/%s) — %s\n",
			n.concept, fname, lifecycleStateLabelFromCode(n.state))
	}
	return sb.String()
}

// renderTagsMD builds the tags.md index document.
func renderTagsMD(tagCounts map[string]int) string {
	var sb strings.Builder
	sb.WriteString("# Tags\n\n")
	if len(tagCounts) == 0 {
		sb.WriteString("No tags in this vault.\n")
		return sb.String()
	}
	for tag, count := range tagCounts {
		fmt.Fprintf(&sb, "- **%s** (%d)\n", tag, count)
	}
	return sb.String()
}

// renderNoteMD builds the full markdown document for a single engram.
func renderNoteMD(n markdownNote) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s\n\n", n.concept)

	// Front-matter block
	sb.WriteString("---\n")
	fmt.Fprintf(&sb, "id: %s\n", n.id)
	fmt.Fprintf(&sb, "type: %s\n", memoryTypeLabelFromCode(n.memType))
	fmt.Fprintf(&sb, "state: %s\n", lifecycleStateLabelFromCode(n.state))
	fmt.Fprintf(&sb, "confidence: %.2f\n", n.confidence)
	if len(n.tags) > 0 {
		fmt.Fprintf(&sb, "tags: [%s]\n", strings.Join(n.tags, ", "))
	}
	fmt.Fprintf(&sb, "created: %s\n", time.Unix(n.createdAt, 0).UTC().Format(time.RFC3339))
	if n.updatedAt > 0 && n.updatedAt != n.createdAt {
		fmt.Fprintf(&sb, "updated: %s\n", time.Unix(n.updatedAt, 0).UTC().Format(time.RFC3339))
	}
	sb.WriteString("---\n\n")

	// Content
	sb.WriteString(n.content)
	sb.WriteString("\n")

	// Summary (if present)
	if n.summary != "" {
		sb.WriteString("\n## Summary\n\n")
		sb.WriteString(n.summary)
		sb.WriteString("\n")
	}

	// Links/associations
	if len(n.links) > 0 {
		sb.WriteString("\n## Links\n\n")
		for _, lnk := range n.links {
			fmt.Fprintf(&sb, "- [%s](../%s) — %s (weight: %.2f)\n",
				lnk.TargetID, lnk.TargetID, relTypeLabelFromCode(lnk.RelType), lnk.Weight)
		}
	}

	return sb.String()
}

// slugify converts a string into a URL-safe filename slug using ASCII characters only.
func slugify(s string) string {
	// Lowercase and replace non-ASCII-alphanumeric with hyphens.
	var sb strings.Builder
	prev := '-'
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
			prev = r
		} else if prev != '-' {
			sb.WriteRune('-')
			prev = '-'
		}
	}
	result := strings.Trim(sb.String(), "-")
	if result == "" {
		return "engram"
	}
	return result
}

var reSlugTrim = regexp.MustCompile(`-+`)

// noteFileName builds the filename for an engram note file.
func noteFileName(n markdownNote) string {
	slug := reSlugTrim.ReplaceAllString(slugify(n.concept), "-")
	// Use last 8 chars of ID to keep filenames short but unique.
	suffix := n.id
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	return fmt.Sprintf("%s-%s.md", slug, suffix)
}

// lifecycleStateLabelFromCode returns a human-readable label for a lifecycle state code.
func lifecycleStateLabelFromCode(code uint8) string {
	switch code {
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
		return fmt.Sprintf("unknown(%d)", code)
	}
}

// memoryTypeLabelFromCode returns a human-readable label for a memory type code.
func memoryTypeLabelFromCode(code uint8) string {
	switch code {
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
		return fmt.Sprintf("unknown(%d)", code)
	}
}

// NOTE: mirrors storage.RelType constants; update both if new RelType values are added.
func relTypeLabelFromCode(code uint16) string {
	switch code {
	case 0x0001:
		return "supports"
	case 0x0002:
		return "contradicts"
	case 0x0003:
		return "depends_on"
	case 0x0004:
		return "supersedes"
	case 0x0005:
		return "relates_to"
	case 0x0006:
		return "is_part_of"
	case 0x0007:
		return "causes"
	case 0x0008:
		return "preceded_by"
	case 0x0009:
		return "followed_by"
	case 0x000A:
		return "created_by_person"
	case 0x000B:
		return "belongs_to_project"
	case 0x000C:
		return "references"
	case 0x000D:
		return "implements"
	case 0x000E:
		return "blocks"
	case 0x000F:
		return "resolves"
	case 0x0010:
		return "refines"
	case 0x8000:
		return "user_defined"
	default:
		return fmt.Sprintf("rel(%d)", code)
	}
}
