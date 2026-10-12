//go:build windows

package scan

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// tree creates files (paths with "/" separators) under a temp root.
func tree(t *testing.T, files ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// found runs a scan and returns the relative paths found, sorted.
func found(t *testing.T, root string, opts Options) ([]string, Stats) {
	t.Helper()
	var out []string
	stats, err := Root(context.Background(), root, opts, func(f File) error {
		rel, _ := filepath.Rel(root, f.Path)
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out, stats
}

func equal(t *testing.T, got []string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v\nwant %v", got, want)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := map[string]string{
		"a.TXT": KindText, "b.docx": KindDoc, "c.Pdf": KindPDF, "d.JPEG": KindImage,
		"e.mp3": KindAudio, "f.mkv": KindVideo, "g.exe": KindOther, "noext": KindOther,
	}
	for name, want := range cases {
		if got := Classify(name); got != want {
			t.Errorf("Classify(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestScanFindsNormalFilesAndSkipsJunk(t *testing.T) {
	root := tree(t,
		"notes.txt", "Report.PDF", "photos/beach.jpg", "music/song.mp3", "setup.exe",
		"node_modules/lib/readme.md", "__pycache__/x.txt", ".cache/y.txt",
		"$Recycle.Bin/old.txt", "AppData/Local/z.txt",
		"Thumbs.db", "desktop.ini",
	)
	got, stats := found(t, root, Options{})
	equal(t, got, "notes.txt", "Report.PDF", "photos/beach.jpg", "music/song.mp3", "setup.exe")
	if stats.Counts[KindImage] != 1 || stats.Counts[KindOther] != 1 || stats.Counts[KindPDF] != 1 {
		t.Errorf("counts = %v", stats.Counts)
	}
}

// P-6: secret-looking files and credential folders are never indexed, not even by name.
func TestScanSkipsPrivate(t *testing.T) {
	root := tree(t,
		"ok.txt",
		"server.pem", "private.key", "cert.pfx", "cert.p12", "vault.kdbx", "work.ovpn",
		".env", ".env.local", "prod.env", "id_rsa", "id_rsa.pub", "id_ed25519",
		"My Passwords.xlsx", "top-SECRET-plan.docx",
		".ssh/config", ".aws/credentials", ".gnupg/pubring.kbx", ".kube/config", ".azure/token.txt",
	)
	got, stats := found(t, root, Options{})
	equal(t, got, "ok.txt")
	if stats.Secret != 14 {
		t.Errorf("Secret = %d, want 14", stats.Secret)
	}
}

func TestScanCodeProjectsOnlyImages(t *testing.T) {
	root := tree(t,
		"myapp/package.json", "myapp/src/index.js", "myapp/README.md", "myapp/assets/logo.png",
		"goproj/go.mod", "goproj/main.go", "goproj/docs/diagram.jpg",
		"essay.txt",
	)
	got, _ := found(t, root, Options{})
	equal(t, got, "essay.txt", "myapp/assets/logo.png", "goproj/docs/diagram.jpg")
}

func TestScanHonorsExcludes(t *testing.T) {
	root := tree(t, "keep/a.txt", "private/b.txt", "private/deep/c.txt", "privateer/d.txt")
	got, _ := found(t, root, Options{Excludes: []string{filepath.Join(root, "PRIVATE")}})
	// "privateer" only shares a prefix and must not be excluded.
	equal(t, got, "keep/a.txt", "privateer/d.txt")
}

func TestScanSkipsHiddenAndCloudOnly(t *testing.T) {
	root := tree(t, "visible.txt", "hidden.txt", "cloud.txt", "hiddendir/x.txt")
	setAttrs(t, filepath.Join(root, "hidden.txt"), windows.FILE_ATTRIBUTE_HIDDEN)
	setAttrs(t, filepath.Join(root, "hiddendir"), windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_HIDDEN)
	setAttrs(t, filepath.Join(root, "cloud.txt"), windows.FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS|windows.FILE_ATTRIBUTE_OFFLINE)
	t.Cleanup(func() { setAttrs(t, filepath.Join(root, "cloud.txt"), windows.FILE_ATTRIBUTE_NORMAL) })

	got, stats := found(t, root, Options{})
	equal(t, got, "visible.txt")
	if stats.CloudOnly != 1 {
		t.Errorf("CloudOnly = %d, want 1", stats.CloudOnly)
	}
}

func TestScanDoesNotFollowJunctions(t *testing.T) {
	root := tree(t, "a.txt")
	outside := tree(t, "outside.txt")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(root, "jump"), outside).CombinedOutput(); err != nil {
		t.Fatalf("mklink: %v: %s", err, out)
	}
	got, stats := found(t, root, Options{})
	equal(t, got, "a.txt")
	if stats.Links != 1 {
		t.Errorf("Links = %d, want 1", stats.Links)
	}
}

func TestScanStopsOnCancelAndCallbackError(t *testing.T) {
	root := tree(t, "a/1.txt", "b/2.txt", "c/3.txt")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Root(ctx, root, Options{}, func(File) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled scan: got %v", err)
	}

	stop := errors.New("stop")
	n := 0
	_, err := Root(context.Background(), root, Options{}, func(File) error { n++; return stop })
	if !errors.Is(err, stop) || n != 1 {
		t.Errorf("callback error: err=%v calls=%d", err, n)
	}
}

func TestScanReportsMetadata(t *testing.T) {
	root := tree(t, "Doc.TXT")
	var f File
	Root(context.Background(), root, Options{}, func(x File) error { f = x; return nil })
	if f.Name != "Doc.TXT" || f.Ext != ".txt" || f.Kind != KindText || f.Size != 1 || f.ModTime == 0 {
		t.Fatalf("unexpected file: %+v", f)
	}
}

func setAttrs(t *testing.T, path string, attrs uint32) {
	t.Helper()
	p, _ := windows.UTF16PtrFromString(path)
	if err := windows.SetFileAttributes(p, attrs); err != nil {
		t.Fatal(err)
	}
}

// Review finding 1: a missing or unusable root is an error, never "empty folder".
func TestScanBadRootIsError(t *testing.T) {
	root := tree(t, "a.txt")
	outside := tree(t, "b.txt")
	link := filepath.Join(root, "jump")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
		t.Fatalf("mklink: %v: %s", err, out)
	}
	for _, bad := range []string{filepath.Join(root, "missing"), link, filepath.Join(root, "a.txt"), "relative"} {
		if _, err := Root(context.Background(), bad, Options{}, func(File) error { return nil }); !errors.Is(err, ErrBadRoot) {
			t.Errorf("Root(%q) = %v, want ErrBadRoot", bad, err)
		}
	}
}

// Review findings 2+3: secret-named folders are skipped; look-alike names are not.
func TestScanPrivateFoldersAndLookalikes(t *testing.T) {
	root := tree(t,
		"Passwords/bank-logins.txt", "Secrets/keys.txt", "credentials/aws.txt",
		"credentials.json", "service-credential.json", "accessKeys.csv", ".netrc", "putty.ppk", "release.jks",
		".envelope.txt", "Secretary meeting.docx", "secretariat.txt", "envoy.txt",
	)
	got, _ := found(t, root, Options{})
	equal(t, got, ".envelope.txt", "Secretary meeting.docx", "secretariat.txt", "envoy.txt")
}

func TestIsPrivateName(t *testing.T) {
	yes := []string{"top_secret.txt", "secretary_secrets.txt", "my.passwd", "x.GPG", ".env", ".env.production"}
	no := []string{"secretary.docx", "Secretariat notes.pdf", ".envrc.txt", "environment.md"}
	for _, n := range yes {
		if !IsSecretName(n) {
			t.Errorf("IsSecretName(%q) = false, want true", n)
		}
	}
	for _, n := range no {
		if IsSecretName(n) {
			t.Errorf("IsSecretName(%q) = true, want false", n)
		}
	}
}

// Review finding 4: excludes match regardless of trailing slash, case or 8.3 short name.
func TestScanExcludeForms(t *testing.T) {
	root := tree(t, "Long Folder Name/a.txt", "keep.txt")
	long := filepath.Join(root, "Long Folder Name")
	forms := []string{long + `\`, strings.ToUpper(long), filepath.ToSlash(long)}
	if short := shortName(long); short != "" && short != long {
		forms = append(forms, short)
	}
	for _, ex := range forms {
		got, _ := found(t, root, Options{Excludes: []string{ex}})
		if len(got) != 1 || got[0] != "keep.txt" {
			t.Errorf("exclude %q: got %v", ex, got)
		}
	}
}

// Review finding 6: counts only include files the callback accepted.
func TestScanCountsOnlyAccepted(t *testing.T) {
	root := tree(t, "a.txt")
	stats, _ := Root(context.Background(), root, Options{}, func(File) error { return errors.New("stop") })
	if stats.Counts[KindText] != 0 {
		t.Errorf("counts = %v, want none", stats.Counts)
	}
}

// Files over 2 GB are skipped, except videos. Sparse files avoid writing gigabytes.
func TestScanTooLarge(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"huge.txt", "movie.mp4"} {
		p := filepath.Join(root, name)
		os.WriteFile(p, nil, 0o644)
		if out, err := exec.Command("fsutil", "sparse", "setflag", p).CombinedOutput(); err != nil {
			t.Skipf("fsutil sparse: %v: %s", err, out)
		}
		if err := os.Truncate(p, MaxSize+1); err != nil {
			t.Skipf("truncate: %v", err)
		}
	}
	got, stats := found(t, root, Options{})
	equal(t, got, "movie.mp4")
	if stats.TooLarge != 1 {
		t.Errorf("TooLarge = %d, want 1", stats.TooLarge)
	}
}

func shortName(p string) string {
	p16, _ := windows.UTF16PtrFromString(p)
	buf := make([]uint16, 1024)
	n, err := windows.GetShortPathName(p16, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

// Full review #4: password-manager exports and credential stores are skipped (P-6).
// The name avoids "Password": Go puts the test name in its temp folder path, and a
// chosen folder inside a "password" folder is refused.
func TestScanSkipsManagerExports(t *testing.T) {
	root := tree(t,
		"ok.txt",
		"bitwarden_export_20250101.json", "lastpass_export.csv", "logins.csv", "1Password Export.1pux", "github-recovery-codes.txt", "Login Data", "rclone.conf", "AuthKey_ABC.p8", "wallet seed phrase.txt",
		"vault.kdb", "infra.tfstate", "wallet.dat", ".npmrc",
		".docker/config.json", ".password-store/bank.gpg",
	)
	got, _ := found(t, root, Options{})
	equal(t, got, "ok.txt")
}

// Verification follow-up: study notes about mnemonics are not secrets; codes with spaces are.
func TestCodesAndSeedNames(t *testing.T) {
	for _, n := range []string{"Backup Codes.txt", "recovery codes.txt", "github-recovery-codes.txt",
		"wallet mnemonic.txt", "seed_phrase.txt", "Bitcoin mnemonic backup.txt"} {
		if !IsSecretName(n) {
			t.Errorf("%q should be secret", n)
		}
	}
	for _, n := range []string{"Mnemonics for Biology.pdf", "Medical_Mnemonics.pdf", "Mnemonics", "seed catalog.pdf"} {
		if IsSecretName(n) {
			t.Errorf("%q should not be secret", n)
		}
	}
}

// Medium #16: a credentials folder chosen directly (or a folder inside one) is refused.
func TestPrivateRootRefused(t *testing.T) {
	base := t.TempDir()
	for _, rel := range []string{".aws", `.ssh\keys`, "My Passwords", `Work\credentials\old`} {
		p := filepath.Join(base, rel)
		os.MkdirAll(p, 0o755)
		os.WriteFile(filepath.Join(p, "config.txt"), []byte("x"), 0o644)
		if _, err := Root(context.Background(), p, Options{}, func(File) error { return nil }); !errors.Is(err, ErrSecretRoot) {
			t.Errorf("%s: got %v, want ErrSecretRoot", rel, err)
		}
	}
	ok := filepath.Join(base, "AppData", "Local", "MyNotes")
	os.MkdirAll(ok, 0o755)
	if _, err := Root(context.Background(), ok, Options{}, func(File) error { return nil }); err != nil {
		t.Errorf("a normal folder inside AppData refused: %v", err)
	}
}

// Review of the Medium fixes: a chosen folder gets the same name rules as a subfolder.
func TestPrivateFolderUsesChildRulesForTheFolderItself(t *testing.T) {
	for _, p := range []string{`D:\MyPasswords`, `D:\passwd`, `D:\KeePass`, `D:\Passwords2024`, `D:\Work\.ssh`} {
		if !SecretFolder(p) {
			t.Errorf("%s allowed", p)
		}
	}
	// Lookalikes stay allowed, as folders and as parents.
	for _, p := range []string{`C:\Users\Secretary\Docs`, `D:\Keys to success`, `D:\Secretary notes`, `D:\`} {
		if SecretFolder(p) {
			t.Errorf("%s refused", p)
		}
	}
}

// Review round 2: parents of the chosen folder, share names and folder "extensions".
func TestPrivateFolderParentsSharesAndExtensions(t *testing.T) {
	for _, p := range []string{`D:\KeePass\exports`, `D:\MyPasswords\docs`, `D:\Bitwarden\exports`,
		`\server\Passwords`, `\server\Passwords\`, `\?\UNC\srv\KeePass`, `\srv\share\.aws\x`} {
		if !SecretFolder(p) {
			t.Errorf("%s allowed", p)
		}
	}
	for _, p := range []string{`D:\Website.asc`, `D:\Projects.Key`, `D:\photos.gpg`, `D:\docs.ppk`,
		`D:\MySecretsArchive2\..\Notes`, `\server\share`, `\server\share\Docs`} {
		if SecretFolder(p) {
			t.Errorf("%s refused", p)
		}
	}
	if IsSecretFolderName("Website.asc") || !IsSecretName("website.asc") {
		t.Error("extension rule must apply to files only")
	}
}

// Round 3 review: folders inside a "MySecrets"-style folder, and names with trailing
// dots or spaces.
func TestPrivateFolderInsideAndTrailingDots(t *testing.T) {
	for _, p := range []string{`C:\Users\bob\MySecrets\Bank`, `\srv\MySecrets\x`, `D:\KeePass. \x`} {
		if !SecretFolder(p) {
			t.Errorf("%s allowed", p)
		}
	}
	for _, n := range []string{"key.pem.", "id_rsa ", "wallet.dat. ", ".env."} {
		if !IsSecretName(n) {
			t.Errorf("%q not treated as secret", n)
		}
	}
}

// Final review: as parents, "secret" must be a word of its own.
func TestPrivateParentsNeedWholeWord(t *testing.T) {
	for _, p := range []string{`C:\Users\secretagent\Documents`, `D:\Biology\Secretion studies\x`, `D:\secretsauce\x`} {
		if SecretFolder(p) {
			t.Errorf("%s refused", p)
		}
	}
	for _, p := range []string{`C:\Users\bob\MySecrets\Bank`, `D:\My Secrets\x`, `D:\SecretsArchive\x`, `D:\old_secret\x`, `D:\Top SECRET\x`} {
		if !SecretFolder(p) {
			t.Errorf("%s allowed", p)
		}
	}
}
