package chunk

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func sentence(i int) string {
	return fmt.Sprintf("Sentence number %d talks about the lease and the rules for pets in the flat.", i)
}

func TestShortTextIsOneChunk(t *testing.T) {
	got := Split([]Section{{Text: "Hello world. This is short."}})
	if len(got) != 1 || got[0].Text != "Hello world. This is short." || got[0].Index != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestEmptyInput(t *testing.T) {
	if got := Split([]Section{{Text: ""}, {Text: "\n\n  \n"}}); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestChunksRespectSizeAndOverlap(t *testing.T) {
	var parts []string
	for i := range 100 {
		parts = append(parts, sentence(i))
	}
	got := Split([]Section{{Text: strings.Join(parts, " ")}})
	if len(got) < 5 {
		t.Fatalf("only %d chunks", len(got))
	}
	for i, c := range got {
		n := utf8.RuneCountInString(c.Text)
		if n > Max {
			t.Errorf("chunk %d has %d runes > Max", i, n)
		}
		if i < len(got)-1 && n < Target/2 {
			t.Errorf("chunk %d is only %d runes", i, n)
		}
		if !strings.HasPrefix(c.Text, "Sentence") || !strings.HasSuffix(c.Text, ".") {
			t.Errorf("chunk %d does not start/end at a sentence: %q…%q", i, c.Text[:20], c.Text[len(c.Text)-20:])
		}
		if c.Index != i {
			t.Errorf("chunk %d has Index %d", i, c.Index)
		}
	}
	// Consecutive chunks overlap: the next one starts with a sentence the previous ended with.
	for i := 0; i+1 < len(got); i++ {
		first := strings.SplitAfter(got[i+1].Text, ". ")[0]
		if !strings.Contains(got[i].Text, strings.TrimSpace(first)) {
			t.Errorf("no overlap between chunk %d and %d", i, i+1)
		}
	}
	// Every sentence appears somewhere.
	all := ""
	for _, c := range got {
		all += c.Text + " "
	}
	for i := range 100 {
		if !strings.Contains(all, sentence(i)) {
			t.Fatalf("sentence %d lost", i)
		}
	}
}

func TestPagesAreKeptAndNotCrossed(t *testing.T) {
	got := Split([]Section{{Page: 1, Text: "Page one text."}, {Page: 2, Text: "Page two text."}})
	if len(got) != 2 || got[0].Page != 1 || got[1].Page != 2 || got[1].Index != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestParagraphBreaksKept(t *testing.T) {
	got := Split([]Section{{Text: "First para.\n\nSecond para."}})
	if len(got) != 1 || got[0].Text != "First para.\n\nSecond para." {
		t.Fatalf("got %q", got[0].Text)
	}
}

// Text with no spaces or sentence ends (CJK, base64, long URLs) is still cut.
func TestLongUnbrokenText(t *testing.T) {
	for _, s := range []string{strings.Repeat("字", 5000), strings.Repeat("x", 5000)} {
		got := Split([]Section{{Text: s}})
		total := 0
		for _, c := range got {
			n := utf8.RuneCountInString(c.Text)
			if n > Max {
				t.Fatalf("chunk of %d runes", n)
			}
			total += n
		}
		if total < 5000 {
			t.Fatalf("lost text: %d of 5000 runes", total)
		}
	}
}

func TestCJKSentences(t *testing.T) {
	text := strings.Repeat("这是一个关于租约的句子。", 300)
	got := Split([]Section{{Text: text}})
	for _, c := range got {
		if !strings.HasSuffix(c.Text, "。") {
			t.Fatalf("CJK chunk not cut at sentence end: …%q", string([]rune(c.Text)[len([]rune(c.Text))-5:]))
		}
	}
}

func TestNoSplitBeforeLowercase(t *testing.T) {
	s := sentences("Pay approx. ten dollars. Then leave.")
	if len(s) != 2 || s[0] != "Pay approx. ten dollars." {
		t.Fatalf("got %q", s)
	}
}

func TestLargeInputIsFast(t *testing.T) {
	text := strings.Repeat(sentence(1)+" ", 130_000) // ~10 MB, the extract output cap
	start := time.Now()
	got := Split([]Section{{Text: text}})
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %v for %d chunks", d, len(got))
	}
}

// Review: tiny units and punctuation used to produce passages up to 4,198 characters,
// because the separators between units were not counted.
func TestMaxIsEnforcedInBytes(t *testing.T) {
	inputs := map[string]string{
		"tiny paragraphs": strings.Repeat("x\n\n", 5000),
		"exclamations":    strings.Repeat("! ", 5000),
		"initials":        strings.Repeat("A. ", 5000),
		"emoji":           strings.Repeat("😀", 5000),
		"rare script":     strings.Repeat("ꙮ𐍈ᚠ ", 3000),
		"cjk":             strings.Repeat("租约规定不允许养宠物。", 500),
	}
	for name, in := range inputs {
		got := Split([]Section{{Text: in}})
		if len(got) == 0 {
			t.Errorf("%s: no chunks", name)
		}
		for i, c := range got {
			if len(c.Text) > Max {
				t.Errorf("%s: chunk %d is %d bytes > Max %d", name, i, len(c.Text), Max)
			}
			if !utf8.ValidString(c.Text) {
				t.Errorf("%s: chunk %d is not valid UTF-8", name, i)
			}
		}
	}
}
