package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
)

// ExecRequest is one command in a pod's container.
type ExecRequest struct {
	Namespace string
	Pod       string
	Container string
	Command   []string
	Stdin     io.Reader // nil: no stdin
	Stdout    io.Writer
	Stderr    io.Writer // ignored with TTY (merged into Stdout)
	TTY       bool
	// Resize delivers terminal sizes (TTY only); nil keeps the default.
	Resize remotecommand.TerminalSizeQueue
}

// Executor runs commands in containers. Tests use a fake.
type Executor interface {
	// Exec runs req until the command exits or ctx ends. A command that
	// exits non-zero returns an error for which ExitCode reports the code.
	Exec(ctx context.Context, req ExecRequest) error
}

// ExitCode returns the command's exit code if err says it exited non-zero.
func ExitCode(err error) (int, bool) {
	var exit utilexec.ExitError
	if errors.As(err, &exit) && exit.Exited() {
		return exit.ExitStatus(), true
	}
	return 0, false
}

// RemoteExecutor runs commands over the API server's exec subresource:
// WebSocket, falling back to SPDY for servers that do not upgrade.
type RemoteExecutor struct {
	Config *rest.Config // without a request timeout
}

func (e *RemoteExecutor) Exec(ctx context.Context, req ExecRequest) error {
	if len(req.Command) == 0 {
		return fmt.Errorf("command is required")
	}
	cs, err := kubernetes.NewForConfig(e.Config)
	if err != nil {
		return err
	}
	u := cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(req.Namespace).Name(req.Pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: req.Container, Command: req.Command,
			Stdin: req.Stdin != nil, Stdout: req.Stdout != nil, Stderr: req.Stderr != nil && !req.TTY, TTY: req.TTY,
		}, scheme.ParameterCodec).URL()
	ws, err := remotecommand.NewWebSocketExecutor(e.Config, "GET", u.String())
	if err != nil {
		return err
	}
	spdy, err := remotecommand.NewSPDYExecutor(e.Config, "POST", u)
	if err != nil {
		return err
	}
	exec, err := remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return err
	}
	opts := remotecommand.StreamOptions{Stdin: req.Stdin, Stdout: req.Stdout, Tty: req.TTY, TerminalSizeQueue: req.Resize}
	if !req.TTY {
		opts.Stderr = req.Stderr
	}
	return Explain(exec.StreamWithContext(ctx, opts))
}
