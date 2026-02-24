package main

import (
	"encoding/json"
	"fmt"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// kjvRecord is one verse from the scrollmapper KJV JSON format.
type kjvRecord struct {
	B int    `json:"b"` // book number 1-66
	C int    `json:"c"` // chapter
	V int    `json:"v"` // verse
	T string `json:"t"` // text
}

// bookNames maps book number (1-66) to canonical name. Index 0 is unused.
var bookNames = [67]string{
	0:  "",
	1:  "Genesis",
	2:  "Exodus",
	3:  "Leviticus",
	4:  "Numbers",
	5:  "Deuteronomy",
	6:  "Joshua",
	7:  "Judges",
	8:  "Ruth",
	9:  "1 Samuel",
	10: "2 Samuel",
	11: "1 Kings",
	12: "2 Kings",
	13: "1 Chronicles",
	14: "2 Chronicles",
	15: "Ezra",
	16: "Nehemiah",
	17: "Esther",
	18: "Job",
	19: "Psalms",
	20: "Proverbs",
	21: "Ecclesiastes",
	22: "Song of Solomon",
	23: "Isaiah",
	24: "Jeremiah",
	25: "Lamentations",
	26: "Ezekiel",
	27: "Daniel",
	28: "Hosea",
	29: "Joel",
	30: "Amos",
	31: "Obadiah",
	32: "Jonah",
	33: "Micah",
	34: "Nahum",
	35: "Habakkuk",
	36: "Zephaniah",
	37: "Haggai",
	38: "Zechariah",
	39: "Malachi",
	40: "Matthew",
	41: "Mark",
	42: "Luke",
	43: "John",
	44: "Acts",
	45: "Romans",
	46: "1 Corinthians",
	47: "2 Corinthians",
	48: "Galatians",
	49: "Ephesians",
	50: "Philippians",
	51: "Colossians",
	52: "1 Thessalonians",
	53: "2 Thessalonians",
	54: "1 Timothy",
	55: "2 Timothy",
	56: "Titus",
	57: "Philemon",
	58: "Hebrews",
	59: "James",
	60: "1 Peter",
	61: "2 Peter",
	62: "1 John",
	63: "2 John",
	64: "3 John",
	65: "Jude",
	66: "Revelation",
}

// genreTags maps book number (1-66) to genre tag. Index 0 is unused.
var genreTags = [67]string{
	0:  "",
	1:  "law",
	2:  "law",
	3:  "law",
	4:  "law",
	5:  "law",
	6:  "history",
	7:  "history",
	8:  "history",
	9:  "history",
	10: "history",
	11: "history",
	12: "history",
	13: "history",
	14: "history",
	15: "history",
	16: "history",
	17: "history",
	18: "poetry",
	19: "poetry",
	20: "poetry",
	21: "poetry",
	22: "poetry",
	23: "prophecy",
	24: "prophecy",
	25: "prophecy",
	26: "prophecy",
	27: "prophecy",
	28: "prophecy",
	29: "prophecy",
	30: "prophecy",
	31: "prophecy",
	32: "prophecy",
	33: "prophecy",
	34: "prophecy",
	35: "prophecy",
	36: "prophecy",
	37: "prophecy",
	38: "prophecy",
	39: "prophecy",
	40: "gospel",
	41: "gospel",
	42: "gospel",
	43: "gospel",
	44: "acts",
	45: "epistle",
	46: "epistle",
	47: "epistle",
	48: "epistle",
	49: "epistle",
	50: "epistle",
	51: "epistle",
	52: "epistle",
	53: "epistle",
	54: "epistle",
	55: "epistle",
	56: "epistle",
	57: "epistle",
	58: "epistle",
	59: "epistle",
	60: "epistle",
	61: "epistle",
	62: "epistle",
	63: "epistle",
	64: "epistle",
	65: "epistle",
	66: "prophecy",
}

// verseRef converts a book number, chapter, and verse to a canonical reference string.
func verseRef(bookNum, chapter, verse int) string {
	if bookNum < 1 || bookNum > 66 {
		return fmt.Sprintf("Unknown %d:%d", chapter, verse)
	}
	return fmt.Sprintf("%s %d:%d", bookNames[bookNum], chapter, verse)
}

// parseKJV parses KJV JSON data into WriteRequest slice.
// If ntOnly is true, only New Testament verses (books 40-66) are included.
func parseKJV(data []byte, ntOnly bool) ([]mbp.WriteRequest, error) {
	var records []kjvRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("parse KJV JSON: %w", err)
	}

	reqs := make([]mbp.WriteRequest, 0, len(records))
	for _, r := range records {
		if r.B < 1 || r.B > 66 {
			continue
		}
		isNT := r.B >= 40
		if ntOnly && !isNT {
			continue
		}

		tags := []string{}
		if isNT {
			tags = append(tags, "New Testament")
		} else {
			tags = append(tags, "Old Testament")
		}
		tags = append(tags, bookNames[r.B])
		if g := genreTags[r.B]; g != "" {
			tags = append(tags, g)
		}

		reqs = append(reqs, mbp.WriteRequest{
			Concept: verseRef(r.B, r.C, r.V),
			Content: r.T,
			Tags:    tags,
		})
	}
	return reqs, nil
}
