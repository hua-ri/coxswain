package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	kube "github.com/hua-ri/coxswain/internal/k8s"
	"github.com/hua-ri/coxswain/internal/term"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
)

// syncBuf collects a terminal's output.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		s.mu.Lock()
		got := s.b.String()
		s.mu.Unlock()
		if strings.Contains(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("output lacks %q: %q", want, got)
		}
		time.Sleep(time.Millisecond)
	}
}

// shellExec is a fake container with only the shells in have: it echoes
// input, reports terminal sizes, and exits on "exit N".
func shellExec(have ...string) func(context.Context, kube.ExecRequest) error {
	return func(ctx context.Context, req kube.ExecRequest) error {
		found := false
		for _, h := range have {
			found = found || req.Command[0] == h
		}
		if !found {
			return fmt.Errorf("command terminated: %w", utilexec.CodeExitError{
				Err: errors.New(`exec: "` + req.Command[0] + `": executable file not found in $PATH`), Code: 127})
		}
		if !req.TTY || req.Stdin == nil || req.Resize == nil || req.Stderr != nil {
			return errors.New("not a terminal request")
		}
		sizes := make(chan string, 8)
		go func() {
			for {
				sz := req.Resize.Next()
				if sz == nil {
					return
				}
				sizes <- fmt.Sprintf("[%dx%d]", sz.Width, sz.Height)
			}
		}()
		fmt.Fprintf(req.Stdout, "%s$ ", req.Command[0])
		lines := make(chan string)
		go func() {
			buf := make([]byte, 256)
			for {
				n, err := req.Stdin.Read(buf)
				if err != nil {
					close(lines)
					return
				}
				lines <- string(buf[:n])
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case sz := <-sizes:
				io.WriteString(req.Stdout, sz)
			case in, ok := <-lines:
				if !ok {
					return nil
				}
				var code int
				if _, err := fmt.Sscanf(in, "exit %d", &code); err == nil {
					if code == 0 {
						return nil
					}
					return utilexec.CodeExitError{Err: errors.New("exit"), Code: code}
				}
				io.WriteString(req.Stdout, in)
			}
		}
	}
}

func TestPodShell(t *testing.T) {
	f := setup(t, runningPod("shop", "web-1", "node-1"), twoContainers("shop", "api-1"))
	f.fakes["prod"].ExecCalls.Run = shellExec("sh")
	ts := &Terminals{Clusters: f.p.Clusters}
	out := &syncBuf{}
	b, spec, err := ts.OpenPod(context.Background(), term.PodSpec{Context: "prod", Pod: "web-1", Shell: "bash"}, 80, 24, out)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Namespace != "shop" || spec.Container != "app" {
		t.Fatalf("%+v", spec)
	}
	// bash is missing: sh is used instead, with the initial size.
	out.waitFor(t, "sh$ [80x24]")
	calls := f.fakes["prod"].ExecCalls.Calls()
	if len(calls) != 2 || calls[0].Command[0] != "bash" || calls[1].Command[0] != "sh" || calls[1].Container != "app" || calls[1].Namespace != "shop" {
		t.Fatalf("calls %+v", calls)
	}
	b.Write([]byte("ls\r"))
	out.waitFor(t, "ls\r")
	if err := b.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	out.waitFor(t, "[120x40]")
	b.Write([]byte("exit 3"))
	if code := b.Wait(); code == nil || *code != 3 {
		t.Fatalf("exit code %v", code)
	}
	if _, err := b.Write([]byte("x")); err == nil {
		t.Fatal("wrote to an ended shell")
	}
	if err := ts.Target("prod", "").ID; err != "k8s:prod/shop" {
		t.Fatalf("target %s", err)
	}
}

func TestPodShellClose(t *testing.T) {
	f := setup(t, runningPod("default", "web-1", "node-1"))
	f.fakes["dev"].ExecCalls.Run = shellExec("bash", "sh")
	ts := &Terminals{Clusters: f.p.Clusters}
	out := &syncBuf{}
	b, _, err := ts.OpenPod(context.Background(), term.PodSpec{Context: "dev", Pod: "web-1", Shell: "bash"}, 80, 24, out)
	if err != nil {
		t.Fatal(err)
	}
	out.waitFor(t, "bash$ ")
	b.Close()
	if code := b.Wait(); code != nil {
		t.Fatalf("closed shell has exit code %d", *code)
	}
	if len(f.fakes["dev"].ExecCalls.Calls()) != 1 {
		t.Fatal("fell back although bash exists")
	}
}

func TestPodShellErrors(t *testing.T) {
	pend := runningPod("default", "p-1", "")
	pend.Status.Phase = "Pending"
	f := setup(t, twoContainers("default", "api-1"), pend)
	ts := &Terminals{Clusters: f.p.Clusters}
	for _, c := range []struct {
		spec term.PodSpec
		want string
	}{
		{term.PodSpec{Context: "dev", Pod: "api-1"}, "app, sidecar"},
		{term.PodSpec{Context: "dev", Pod: "nope"}, "not found"},
		{term.PodSpec{Context: "dev", Pod: "p-1"}, "Pending"},
		{term.PodSpec{Context: "staging", Pod: "api-1"}, "staging"},
	} {
		if _, _, err := ts.OpenPod(context.Background(), c.spec, 80, 24, io.Discard); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v", c.spec, err)
		}
	}
	// Neither shell exists: the error is the last one's.
	f.fakes["dev"].ExecCalls.Run = shellExec()
	b, _, err := ts.OpenPod(context.Background(), term.PodSpec{Context: "dev", Pod: "api-1", Container: "app", Shell: "zsh"}, 80, 24, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if code := b.Wait(); code == nil || *code != 127 {
		t.Fatalf("code %v", code)
	}
	if n := len(f.fakes["dev"].ExecCalls.Calls()); n != 2 {
		t.Fatalf("calls %d", n)
	}
}

var _ remotecommand.TerminalSizeQueue = (*sizeQueue)(nil)
