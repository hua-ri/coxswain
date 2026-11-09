package k8s

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hua-ri/coxswain/internal/action"
	kube "github.com/hua-ri/coxswain/internal/k8s"
	"github.com/hua-ri/coxswain/internal/term"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/remotecommand"
)

// Terminals starts shells in pods for shared terminals (term.PodOpener).
type Terminals struct{ Clusters *kube.Clusters }

// Target is a namespace's audit target; "" is the context's namespace.
func (ts *Terminals) Target(context, namespace string) action.Target {
	if namespace == "" {
		if ci, err := ts.Clusters.Context(context); err == nil {
			namespace = ci.Namespace
		}
	}
	return ts.Clusters.Target(context, namespace)
}

func (ts *Terminals) OpenPod(ctx context.Context, spec term.PodSpec, cols, rows int, out io.Writer) (term.Backend, term.PodSpec, error) {
	ci, err := ts.Clusters.Context(spec.Context)
	if err != nil {
		return nil, spec, err
	}
	c, err := ts.Clusters.Client(spec.Context)
	if err != nil {
		return nil, spec, err
	}
	if spec.Namespace == "" {
		spec.Namespace = ci.Namespace
	}
	pod, err := c.Typed.CoreV1().Pods(spec.Namespace).Get(ctx, spec.Pod, metav1.GetOptions{})
	if err != nil {
		return nil, spec, kube.Explain(err)
	}
	if pod.Status.Phase != corev1.PodRunning {
		return nil, spec, fmt.Errorf("pod %s is %s, not running", pod.Name, pod.Status.Phase)
	}
	if spec.Container, err = pickContainer(pod, spec.Container); err != nil {
		return nil, spec, err
	}
	shells := []string{"sh"}
	if spec.Shell != "" && spec.Shell != "sh" {
		shells = []string{spec.Shell, "sh"}
	}
	b := &podShell{exec: c.Exec, size: remotecommand.TerminalSize{Width: uint16(cols), Height: uint16(rows)},
		done: make(chan struct{})}
	b.stdinR, b.stdinW = io.Pipe()
	b.ctx, b.cancel = context.WithCancel(context.Background())
	go b.run(kube.ExecRequest{Namespace: spec.Namespace, Pod: pod.Name, Container: spec.Container, TTY: true}, shells,
		&seenWriter{w: out, seen: &b.output})
	return b, spec, nil
}

// podShell is an interactive shell in a container (exec with a TTY).
type podShell struct {
	exec   kube.Executor
	stdinR *io.PipeReader
	stdinW *io.PipeWriter
	ctx    context.Context
	cancel context.CancelFunc
	output atomic.Bool // the shell wrote something
	done   chan struct{}
	code   *int
	mu     sync.Mutex
	size   remotecommand.TerminalSize
	queue  *sizeQueue
}

// run starts the first shell that exists: a shell that is not found
// fails at once with exit code 126 or 127 (or "not found") and no output.
func (b *podShell) run(req kube.ExecRequest, shells []string, out io.Writer) {
	defer close(b.done)
	defer b.stdinR.Close()
	var err error
	for i, sh := range shells {
		b.mu.Lock()
		q := newSizeQueue(b.size)
		b.queue = q
		b.mu.Unlock()
		req.Command, req.Stdin, req.Stdout, req.Resize = []string{sh}, b.stdinR, out, q
		err = b.exec.Exec(b.ctx, req)
		q.close()
		if i+1 < len(shells) && err != nil && b.ctx.Err() == nil && !b.output.Load() && notFound(err) {
			continue
		}
		break
	}
	switch code, ok := kube.ExitCode(err); {
	case err == nil:
		b.code = new(0)
	case ok:
		b.code = &code
	}
	if err != nil && b.ctx.Err() == nil && (b.code == nil || !b.output.Load()) {
		fmt.Fprintf(out, "\r\n[%v]\r\n", err) // why the terminal ended at once
	}
}

func notFound(err error) bool {
	if code, ok := kube.ExitCode(err); ok && (code == 126 || code == 127) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "no such file")
}

func (b *podShell) Write(p []byte) (int, error) { return b.stdinW.Write(p) }

func (b *podShell) Resize(cols, rows int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.size = remotecommand.TerminalSize{Width: uint16(cols), Height: uint16(rows)}
	if b.queue != nil {
		b.queue.push(b.size)
	}
	return nil
}

func (b *podShell) Wait() *int {
	<-b.done
	return b.code
}

func (b *podShell) Close() error {
	b.cancel()
	b.stdinW.Close()
	return nil
}

// seenWriter notes that something was written.
type seenWriter struct {
	w    io.Writer
	seen *atomic.Bool
}

func (s *seenWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		s.seen.Store(true)
	}
	return s.w.Write(p)
}

// sizeQueue hands terminal sizes to the exec stream: the latest one wins.
type sizeQueue struct {
	mu     sync.Mutex
	ch     chan remotecommand.TerminalSize
	closed bool
}

func newSizeQueue(initial remotecommand.TerminalSize) *sizeQueue {
	q := &sizeQueue{ch: make(chan remotecommand.TerminalSize, 1)}
	q.ch <- initial
	return q
}

func (q *sizeQueue) push(s remotecommand.TerminalSize) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	select {
	case <-q.ch:
	default:
	}
	q.ch <- s
}

func (q *sizeQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closed {
		q.closed = true
		close(q.ch)
	}
}

// Next blocks for the next size; nil once the stream ended.
func (q *sizeQueue) Next() *remotecommand.TerminalSize {
	s, ok := <-q.ch
	if !ok {
		return nil
	}
	return &s
}
