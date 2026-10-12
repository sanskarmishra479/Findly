package evalkit

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestScore(t *testing.T) {
	c := Case{Query: "q", Expect: []string{"a/lease.pdf#page=2"}}
	hits := []Hit{{"x.txt", 0}, {"a/lease.pdf", 1}, {"a/lease.pdf", 2}}
	r, rr := Score(c, hits)
	if r != 1 || rr != 1.0/3 {
		t.Fatalf("recall=%v rr=%v", r, rr)
	}
	if r, rr := Score(Case{Expect: []string{"a/b.txt"}}, []Hit{{"A/B.TXT", 0}}); r != 1 || rr != 1 {
		t.Fatalf("case-insensitive path: %v %v", r, rr)
	}
	many := make([]Hit, 12)
	many[11] = Hit{"a/b.txt", 0}
	if r, rr := Score(Case{Expect: []string{"a/b.txt"}}, many); r != 0 || rr != 0 {
		t.Fatalf("outside top 10 counted: %v %v", r, rr)
	}
}

func TestBuildTestbed(t *testing.T) {
	src := t.TempDir()
	out := t.TempDir()
	os.MkdirAll(filepath.Join(src, "d"), 0o755)
	os.WriteFile(filepath.Join(src, "d", "x.pdf.txt"), []byte("page one\n"+PageBreak+"\npage two"), 0o644)
	os.WriteFile(filepath.Join(src, "d", "y.docx.txt"), []byte("hello & <world>"), 0o644)
	os.WriteFile(filepath.Join(src, "z.odt.txt"), []byte("odt text"), 0o644)
	os.WriteFile(filepath.Join(src, "plain.md"), []byte("# md"), 0o644)
	n, err := BuildTestbed(src, out)
	if err != nil || n != 4 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	for _, p := range []string{"d/x.pdf", "d/y.docx", "z.odt", "plain.md"} {
		if _, err := os.Stat(filepath.Join(out, filepath.FromSlash(p))); err != nil {
			t.Errorf("%s missing", p)
		}
	}
}

func TestLoadCases(t *testing.T) {
	p := filepath.Join(t.TempDir(), "q.jsonl")
	os.WriteFile(p, []byte(`{"query":"a","expect":["x"]}`+"\n\n"+`{"query":"b","expect":["y","z"]}`), 0o644)
	cs, err := LoadCases(p)
	if err != nil || len(cs) != 2 || len(cs[1].Expect) != 2 {
		t.Fatalf("%v %v", cs, err)
	}
	os.WriteFile(p, []byte(`{"query":"","expect":[]}`), 0o644)
	if _, err := LoadCases(p); err == nil {
		t.Fatal("empty query accepted")
	}
	os.WriteFile(p, []byte(`{"query":"x"}`), 0o644)
	if _, err := LoadCases(p); err == nil {
		t.Fatal("missing expect accepted")
	}
	os.WriteFile(p, []byte(`{"query":"nothing here","expect":[]}`), 0o644)
	if cs, err := LoadCases(p); err != nil || len(cs) != 1 || len(cs[0].Expect) != 0 {
		t.Fatalf("negative case: %v %v", cs, err)
	}
}

// ODT rules: mimetype first and stored; manifest present.
func TestODTLayout(t *testing.T) {
	b, err := MakeODT([]string{"hello"})
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	if zr.File[0].Name != "mimetype" || zr.File[0].Method != zip.Store {
		t.Fatalf("first entry %q method %d", zr.File[0].Name, zr.File[0].Method)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["META-INF/manifest.xml"] || !names["content.xml"] {
		t.Fatalf("entries: %v", names)
	}
}
