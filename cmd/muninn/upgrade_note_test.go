package main

import (
	"strings"
	"testing"
)

// The shape v0.9.0 actually shipped: a blockquote introduced by a bolded
// "Upgrade note", carrying the one manual step the release requires.
const v090ReleaseBody = `Adds an agent-oriented credential and multi-agent workflow layer (capability
tokens and self-provisioned workflow vaults), a ` + "`working`" + ` plasticity preset.

> **Upgrade note — on-disk migration.** This release relocates the auth
> subsystem's internal Pebble key prefixes to resolve a latent collision with
> storage keys (#611). A one-time v3 migration rewrites existing admin-user,
> API-key, and vault-config records on the first start after upgrade. It is
> idempotent and crash-safe, but **back up your data directory before
> upgrading**, as with any storage-format change.

### Added

- Capability tokens and agent-provisioned workflow vaults.
`

func TestUpgradeNote_ExtractsReleaseUpgradeNote(t *testing.T) {
	note := upgradeNote(v090ReleaseBody)
	if note == "" {
		t.Fatal("upgrade note not found in a release body that carries one")
	}
	// The operator-actionable sentence is the whole point of surfacing this.
	if !strings.Contains(note, "back up your data directory before upgrading") {
		t.Errorf("note lost the actionable instruction: %q", note)
	}
	if !strings.Contains(note, "on-disk migration") {
		t.Errorf("note lost its subject: %q", note)
	}
	// Markdown emphasis must not survive into terminal output.
	if strings.Contains(note, "**") || strings.Contains(note, "`") {
		t.Errorf("markdown markers leaked into the rendered note: %q", note)
	}
	// The blockquote ends before the "### Added" section.
	if strings.Contains(note, "Capability tokens") || strings.Contains(note, "Added") {
		t.Errorf("note ran past the end of the blockquote: %q", note)
	}
	// Prose above the blockquote is not part of the note.
	if strings.Contains(note, "plasticity preset") {
		t.Errorf("note picked up body text before the blockquote: %q", note)
	}
}

func TestUpgradeNote_AbsentWhenReleaseHasNone(t *testing.T) {
	body := `### Added

- A thing.

### Fixed

- Another thing.
`
	if note := upgradeNote(body); note != "" {
		t.Errorf("upgradeNote = %q, want empty for a release with no note", note)
	}
}

func TestUpgradeNote_IgnoresThePhraseOutsideABlockquote(t *testing.T) {
	// "upgrade note" appearing in ordinary prose must not be mistaken for one:
	// the convention is a blockquote, and firing on loose prose would print
	// arbitrary release text as though it were an operator instruction.
	body := `This release has no upgrade note, so just install it.

### Added
- A thing.
`
	if note := upgradeNote(body); note != "" {
		t.Errorf("upgradeNote = %q, want empty when the phrase is not in a blockquote", note)
	}
}

// Prose that mentions the phrase before the real blockquote must not become the
// match: anchoring on the first textual hit instead of the first blockquote hit
// silently loses the actual note, which is the failure that matters here.
func TestUpgradeNote_FindsTheBlockquoteWhenProseMentionsItFirst(t *testing.T) {
	body := `Please read the upgrade note before installing this release.

> **Upgrade note — on-disk migration.** Back up your data directory first.

### Added
- A thing.
`
	note := upgradeNote(body)
	if !strings.Contains(note, "Back up your data directory first.") {
		t.Errorf("the real note was missed because prose matched first: %q", note)
	}
	if strings.Contains(note, "Please read") {
		t.Errorf("note anchored on the prose line: %q", note)
	}
}

func TestUpgradeNote_IgnoresUnrelatedBlockquote(t *testing.T) {
	body := `> **Note.** Nothing special about this release.

### Added
- A thing.
`
	if note := upgradeNote(body); note != "" {
		t.Errorf("upgradeNote = %q, want empty for a blockquote that is not an upgrade note", note)
	}
}

func TestUpgradeNote_JoinsMultipleBlockquoteParagraphs(t *testing.T) {
	body := `> **Upgrade note.** First paragraph.
>
> Second paragraph.

Not part of it.
`
	note := upgradeNote(body)
	if !strings.Contains(note, "First paragraph.") || !strings.Contains(note, "Second paragraph.") {
		t.Errorf("note dropped a paragraph: %q", note)
	}
	if strings.Contains(note, "Not part of it") {
		t.Errorf("note ran past the blockquote: %q", note)
	}
}

func TestUpgradeNote_HandlesCRLF(t *testing.T) {
	body := strings.ReplaceAll("> **Upgrade note.** Back up first.\r\n\r\n### Added\r\n", "\n", "\n")
	note := upgradeNote(body)
	if !strings.Contains(note, "Back up first.") {
		t.Errorf("CRLF body yielded %q", note)
	}
	if strings.Contains(note, "\r") {
		t.Errorf("carriage return survived into the note: %q", note)
	}
}

func TestUpgradeNote_TruncatesRunawayNote(t *testing.T) {
	body := "> **Upgrade note.** " + strings.Repeat("word ", 2000)
	note := upgradeNote(body)
	if len(note) > 1210 {
		t.Errorf("note length = %d, want it capped near 1200", len(note))
	}
	if !strings.HasSuffix(note, "...") {
		t.Errorf("truncated note should be marked with an ellipsis: %q", note[max(0, len(note)-20):])
	}
}

func TestUpgradeNote_EmptyBody(t *testing.T) {
	if note := upgradeNote(""); note != "" {
		t.Errorf("upgradeNote(\"\") = %q, want empty", note)
	}
}

func TestWrapText_RespectsWidth(t *testing.T) {
	lines := wrapText("the quick brown fox jumps over the lazy dog", 12)
	if len(lines) < 2 {
		t.Fatalf("expected the text to wrap, got %d line(s)", len(lines))
	}
	for _, l := range lines {
		if len(l) > 12 {
			t.Errorf("line exceeds width: %q (%d)", l, len(l))
		}
	}
	if got := strings.Join(lines, " "); got != "the quick brown fox jumps over the lazy dog" {
		t.Errorf("wrapping changed the text: %q", got)
	}
}

func TestWrapText_DoesNotSplitAnOverlongWord(t *testing.T) {
	long := strings.Repeat("x", 40)
	lines := wrapText(long, 10)
	if len(lines) != 1 || lines[0] != long {
		t.Errorf("an over-long word must be emitted whole, got %v", lines)
	}
}

func TestWrapText_Empty(t *testing.T) {
	if lines := wrapText("   ", 10); lines != nil {
		t.Errorf("wrapText(blank) = %v, want nil", lines)
	}
}

// End-to-end: an operator running `muninn upgrade --check` against a release that
// carries an upgrade note must see the note itself, not just a link to it.
func TestRunUpgrade_PrintsUpgradeNote(t *testing.T) {
	oldVersion := version
	version = "v0.8.0"
	defer func() { version = oldVersion }()

	oldFn := latestReleaseFn
	latestReleaseFn = func() (releaseInfo, error) {
		return releaseInfo{TagName: "v0.9.0", Body: v090ReleaseBody}, nil
	}
	defer func() { latestReleaseFn = oldFn }()

	oldExit := osExit
	osExit = func(int) {}
	defer func() { osExit = oldExit }()

	out := captureStdout(func() { runUpgrade([]string{"--check"}) })

	if !strings.Contains(out, "Upgrade note for this release") {
		t.Errorf("upgrade note header missing from output:\n%s", out)
	}
	if !strings.Contains(out, "back up your data directory") {
		t.Errorf("the actionable instruction never reached the operator:\n%s", out)
	}
}

func TestRunUpgrade_NoNoteHeaderWhenReleaseHasNone(t *testing.T) {
	oldVersion := version
	version = "v0.8.0"
	defer func() { version = oldVersion }()

	oldFn := latestReleaseFn
	latestReleaseFn = func() (releaseInfo, error) {
		return releaseInfo{TagName: "v0.9.0", Body: "### Added\n\n- A thing.\n"}, nil
	}
	defer func() { latestReleaseFn = oldFn }()

	oldExit := osExit
	osExit = func(int) {}
	defer func() { osExit = oldExit }()

	out := captureStdout(func() { runUpgrade([]string{"--check"}) })

	if strings.Contains(out, "Upgrade note") {
		t.Errorf("printed an upgrade-note header for a release with no note:\n%s", out)
	}
}
