//go:build windows

package sidecar

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func cmdExe(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("cmd.exe")
	if err != nil {
		t.Skip("cmd.exe not found")
	}
	p, _ = filepath.Abs(p)
	return p
}

func TestStartOutputAndExit(t *testing.T) {
	var out Tail
	out.Size = 1024
	p, err := Start(cmdExe(t), []string{"/c", "echo hello-from-child"}, Options{Output: &out})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("child did not exit")
	}
	if !strings.Contains(out.String(), "hello-from-child") {
		t.Fatalf("output = %q", out.String())
	}
	p.Stop() // after exit: must not hang or panic
}

// The child must not inherit Findly's environment (e.g. LLAMA_ARG_* or tokens).
func TestEnvironmentIsNotInherited(t *testing.T) {
	t.Setenv("FINDLY_SECRET_TEST", "leaked")
	var out Tail
	out.Size = 4096
	p, err := Start(cmdExe(t), []string{"/c", "set"}, Options{Env: []string{"ONLY_THIS=1"}, Output: &out})
	if err != nil {
		t.Fatal(err)
	}
	<-p.Done()
	s := out.String()
	if strings.Contains(s, "FINDLY_SECRET_TEST") {
		t.Fatal("parent environment leaked into child")
	}
	if !strings.Contains(s, "ONLY_THIS=1") {
		t.Fatalf("given env missing: %q", s)
	}
}

func TestStopKillsRunningProcess(t *testing.T) {
	p, err := Start(cmdExe(t), []string{"/c", "ping -n 30 127.0.0.1 >nul"}, Options{LowPriority: true})
	if err != nil {
		t.Fatal(err)
	}
	if exited, _ := p.Exited(); exited {
		t.Fatal("exited too early")
	}
	start := time.Now()
	p.Stop()
	if time.Since(start) > 5*time.Second {
		t.Fatal("Stop took too long")
	}
	if exited, _ := p.Exited(); !exited {
		t.Fatal("still running after Stop")
	}
	p.Stop()
}

func TestRequiresAbsolutePath(t *testing.T) {
	if _, err := Start("cmd.exe", nil, Options{}); err == nil {
		t.Fatal("expected error for relative path")
	}
}

func TestTailKeepsEnd(t *testing.T) {
	tail := Tail{Size: 5}
	tail.Write([]byte("abc"))
	tail.Write([]byte("defgh"))
	if got := tail.String(); got != "defgh" {
		t.Fatalf("got %q", got)
	}
}

// When the job handle closes (as when Findly dies), Windows kills the child.
func TestJobKillsChildWhenParentDies(t *testing.T) {
	if os.Getenv("FINDLY_SIDECAR_CHILD") == "1" {
		// Running as the "parent": start a long child, print its pid, then exit
		// without stopping it.
		p, err := Start(cmdExe(t), []string{"/c", "ping -n 60 127.0.0.1 >nul"}, Options{})
		if err != nil {
			os.Exit(2)
		}
		os.Stdout.WriteString("PID=" + strconv.Itoa(p.Pid()) + "\n")
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestJobKillsChildWhenParentDies", "-test.v")
	cmd.Env = append(os.Environ(), "FINDLY_SIDECAR_CHILD=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper parent failed: %v: %s", err, out)
	}
	i := strings.Index(string(out), "PID=")
	if i < 0 {
		t.Fatalf("no pid in %q", out)
	}
	pid := strings.TrimSpace(strings.SplitN(string(out)[i+4:], "\n", 2)[0])
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		list, _ := exec.Command("tasklist", "/FI", "PID eq "+pid, "/NH").Output()
		if !strings.Contains(string(list), pid) {
			return // child is gone
		}
		time.Sleep(200 * time.Millisecond)
	}
	exec.Command("taskkill", "/F", "/PID", pid).Run()
	t.Fatal("child outlived its parent")
}
