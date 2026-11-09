package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/hua-ri/coxswain/internal/action"
	kube "github.com/hua-ri/coxswain/internal/k8s"
	"github.com/hua-ri/coxswain/internal/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Forwarder connects k8s tunnels (tunnel.PodDialer) through the
// contexts' clients.
type Forwarder struct{ Clusters *kube.Clusters }

func (f *Forwarder) Target(context, namespace string) action.Target {
	return f.Clusters.Target(context, namespace)
}

func (f *Forwarder) Connect(ctx context.Context, spec tunnel.Spec, failed func(error)) (tunnel.PodConn, error) {
	c, err := f.Clusters.Client(spec.Context)
	if err != nil {
		return nil, err
	}
	pod, port, err := forwardTarget(ctx, c, spec)
	if err != nil {
		return nil, err
	}
	s, err := c.Forward.Forward(ctx, spec.Namespace, pod)
	if err != nil {
		return nil, kube.Explain(err)
	}
	return &podConn{s: s, pod: pod, port: port, failed: failed}, nil
}

// forwardTarget is the pod and port a k8s tunnel leads to: its pod, or
// the first ready pod (by name) of its service, with the service port's
// target port.
func forwardTarget(ctx context.Context, c *kube.Client, spec tunnel.Spec) (string, int, error) {
	core := c.Typed.CoreV1()
	if spec.Pod != "" {
		pod, err := core.Pods(spec.Namespace).Get(ctx, spec.Pod, metav1.GetOptions{})
		if err != nil {
			return "", 0, kube.Explain(err)
		}
		if pod.Status.Phase != corev1.PodRunning {
			return "", 0, fmt.Errorf("pod %s is %s, not running", pod.Name, pod.Status.Phase)
		}
		return pod.Name, spec.RemotePort, nil
	}
	svc, err := core.Services(spec.Namespace).Get(ctx, spec.Service, metav1.GetOptions{})
	if err != nil {
		return "", 0, kube.Explain(err)
	}
	var sp *corev1.ServicePort
	var ports []string
	for i := range svc.Spec.Ports {
		ports = append(ports, strconv.Itoa(int(svc.Spec.Ports[i].Port)))
		if int(svc.Spec.Ports[i].Port) == spec.RemotePort {
			sp = &svc.Spec.Ports[i]
		}
	}
	if sp == nil {
		return "", 0, fmt.Errorf("service %s has no port %d (ports: %s)", svc.Name, spec.RemotePort, dash(strings.Join(ports, ", ")))
	}
	if len(svc.Spec.Selector) == 0 {
		return "", 0, fmt.Errorf("service %s has no selector, so no pods to forward to; forward to a pod instead", svc.Name)
	}
	list, err := core.Pods(spec.Namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(svc.Spec.Selector).String()})
	if err != nil {
		return "", 0, kube.Explain(err)
	}
	pods := list.Items
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	for i := range pods {
		p := &pods[i]
		if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil || !podReady(p) {
			continue
		}
		if port, ok := containerPort(p, sp); ok {
			return p.Name, port, nil
		}
	}
	return "", 0, fmt.Errorf("service %s has no ready pod with port %s", svc.Name, sp.TargetPort.String())
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// containerPort is the pod's port for a service port's target port.
func containerPort(p *corev1.Pod, sp *corev1.ServicePort) (int, bool) {
	switch {
	case sp.TargetPort.Type == intstr.String && sp.TargetPort.StrVal != "":
		for _, c := range p.Spec.Containers {
			for _, cp := range c.Ports {
				if cp.Name == sp.TargetPort.StrVal {
					return int(cp.ContainerPort), true
				}
			}
		}
		return 0, false
	case sp.TargetPort.IntValue() == 0:
		return int(sp.Port), true
	}
	return sp.TargetPort.IntValue(), true
}

// podConn is a port-forward session as a tunnel's upstream.
type podConn struct {
	s      kube.ForwardSession
	pod    string
	port   int
	failed func(error)
}

func (c *podConn) Pod() string             { return c.pod }
func (c *podConn) Port() int               { return c.port }
func (c *podConn) Dial() (net.Conn, error) { return c.s.Dial(c.port, c.failed) }
func (c *podConn) Close() error            { return c.s.Close() }

func (c *podConn) Wait() error {
	<-c.s.Done()
	return errors.New("port-forward connection to pod " + c.pod + " closed")
}

// PortForwardOutput is the result of k8s.port_forward.
type PortForwardOutput struct {
	tunnel.Tunnel
	ContextNote string            `json:"context_note,omitempty"`
	ContextTags map[string]string `json:"context_tags,omitempty"`
	Hint        string            `json:"hint"`
}

type portForwardInput struct {
	Context     string `json:"context"`
	Namespace   string `json:"namespace"`
	Pod         string `json:"pod"`
	Service     string `json:"service"`
	RemotePort  int    `json:"remote_port"`
	LocalPort   int    `json:"local_port"`
	IdleMinutes *int   `json:"idle_minutes"`
}

func (in portForwardInput) check() error {
	switch {
	case (in.Pod == "") == (in.Service == ""):
		return fmt.Errorf("give either pod or service")
	case in.RemotePort < 1 || in.RemotePort > 65535:
		return fmt.Errorf("remote_port (1..65535) is required")
	}
	return nil
}

func (p *Provider) portForward() *action.Action {
	return &action.Action{
		ID:      "k8s.port_forward",
		Summary: "Forward a local port to a pod or service (like kubectl port-forward)",
		Description: "Open a tunnel: 127.0.0.1:local_port on this machine leads to remote_port of a pod, or of a ready pod " +
			"of a service (remote_port is then the service port; it is resolved again if the connection breaks). It " +
			"appears in tunnel_list (type k8s); close it with tunnel_close when done. Agents' tunnels close after " +
			"idle_minutes without connections (default 30). " + prodHint,
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"context":      contextProperty,
				"namespace":    namespaceProperty,
				"pod":          map[string]any{"type": "string", "description": "a pod, or"},
				"service":      map[string]any{"type": "string", "description": "a service (one of its ready pods)"},
				"remote_port":  map[string]any{"type": "integer", "minimum": 1, "maximum": 65535, "description": "pod port, or service port"},
				"local_port":   map[string]any{"type": "integer", "minimum": 0, "maximum": 65535, "description": "port on 127.0.0.1, default free"},
				"idle_minutes": map[string]any{"type": "integer", "minimum": 0, "maximum": 240, "description": "close after this many minutes unused"},
			},
			"required": []string{"context", "remote_port"},
		},
		Positional:  []string{"context"},
		Annotations: action.Annotations{Destructive: true, OpenWorld: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			var in portForwardInput
			if err := action.Decode(raw, &in); err != nil {
				return action.Assessment{}, err
			}
			if err := in.check(); err != nil {
				return action.Assessment{}, err
			}
			r, err := p.resolve(scope{Context: in.Context, Namespace: in.Namespace})
			if err != nil {
				return action.Assessment{}, err
			}
			to := "pod " + in.Pod
			if in.Service != "" {
				to = "service " + in.Service
			}
			local := "a free port"
			if in.LocalPort != 0 {
				local = strconv.Itoa(in.LocalPort)
			}
			return action.Assessment{Risk: action.Write, Targets: []action.Target{p.Clusters.Target(in.Context, r.ns)},
				Preview: fmt.Sprintf("forward 127.0.0.1:%s → %s/%s %s:%d", local, in.Context, r.ns, to, in.RemotePort)}, nil
		},
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in portForwardInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			if err := in.check(); err != nil {
				return nil, err
			}
			if p.Tunnels == nil {
				return nil, fmt.Errorf("port forwards are only available in cox server")
			}
			r, err := p.resolve(scope{Context: in.Context, Namespace: in.Namespace})
			if err != nil {
				return nil, err
			}
			who, _ := action.ActorFrom(ctx)
			who.Session = "" // the owner is the client, not one of its sessions
			tn, err := p.Tunnels.Open(ctx, tunnel.Spec{Type: tunnel.K8s, Context: in.Context, Namespace: r.ns, Pod: in.Pod,
				Service: in.Service, RemotePort: in.RemotePort, LocalPort: in.LocalPort, IdleMinutes: in.IdleMinutes}, who)
			if err != nil {
				return nil, err
			}
			return PortForwardOutput{Tunnel: tn, ContextNote: r.info.Note, ContextTags: r.info.Tags,
				Hint: fmt.Sprintf("connect to %s to reach %s; if a connection closes at once, tunnel_list shows why in "+
					"last_error; close it with tunnel_close %s when done", tn.Listen, tn.Target, tn.ID)}, nil
		},
	}
}
