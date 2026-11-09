package k8s

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/hua-ri/coxswain/internal/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// withTunnels gives the fixture a tunnel manager that port-forwards
// through the provider's Forwarder.
func withTunnels(t *testing.T, f fixture) *tunnel.Manager {
	m := &tunnel.Manager{Pods: &Forwarder{Clusters: f.p.Clusters}, Events: f.e.Events,
		Backoff: func(int) time.Duration { return time.Millisecond }}
	t.Cleanup(m.CloseAll)
	f.p.Tunnels = m
	return m
}

func echoThrough(t *testing.T, addr string) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(60 * time.Second))
	io.WriteString(c, "ping\n")
	if line, err := bufio.NewReader(c).ReadString('\n'); err != nil || line != "ping\n" {
		t.Fatalf("%q %v", line, err)
	}
}

func service(name string, port int32, target intstr.IntOrString) *corev1.Service {
	return &corev1.Service{ObjectMeta: meta("shop", name, time.Hour), Spec: corev1.ServiceSpec{
		Selector: map[string]string{"app": name}, Ports: []corev1.ServicePort{{Name: "web", Port: port, TargetPort: target}}}}
}

func readyPod(name, app string, ready bool, ports ...corev1.ContainerPort) *corev1.Pod {
	p := runningPod("shop", name, "node-1")
	p.Labels["app"] = app
	p.Spec.Containers[0].Ports = ports
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: st}}
	return p
}

func TestPortForwardPod(t *testing.T) {
	f := setup(t, readyPod("web-1", "web", true))
	m := withTunnels(t, f)
	res := f.invoke(t, codex, "k8s.port_forward", args("context", "prod", "pod", "web-1", "remote_port", 8080)).(PortForwardOutput)
	if res.Type != tunnel.K8s || res.Context != "prod" || res.Namespace != "shop" || res.Pod != "web-1" || res.Target != "web-1:8080" ||
		res.Owner.Name != "codex" || res.Owner.Session != "" || res.IdleMinutes != tunnel.DefaultAgentIdle ||
		res.ContextTags["env"] != "prod" || !strings.Contains(res.Hint, res.Listen) {
		t.Fatalf("%+v", res)
	}
	echoThrough(t, res.Listen)
	if calls := f.fakes["prod"].Forwards.Calls(); len(calls) != 1 || calls[0] != "shop/web-1" {
		t.Fatalf("forwards %v", calls)
	}
	ev := f.requested(t, "k8s.port_forward")
	if ev.Risk != "write" || ev.Targets[0].ID != "k8s:prod/shop" || ev.Targets[0].Tags["env"] != "prod" {
		t.Fatalf("audit %+v", ev)
	}
	if len(m.List()) != 1 {
		t.Fatal("not listed")
	}
}

func TestPortForwardService(t *testing.T) {
	http := corev1.ContainerPort{Name: "http", ContainerPort: 8080}
	f := setup(t, service("web", 80, intstr.FromString("http")), service("api", 80, intstr.FromInt32(0)),
		readyPod("web-a", "web", false, http), readyPod("web-b", "web", true, http), readyPod("web-c", "web", true, http),
		readyPod("api-1", "api", true))
	withTunnels(t, f)
	served := make(chan string, 4)
	f.fakes["prod"].Forwards.Serve = func(pod string, port int, c net.Conn) {
		served <- pod + ":" + itoa(int32(port))
		io.Copy(c, c)
		c.Close()
	}
	res := f.invoke(t, desktop, "k8s.port_forward", args("context", "prod", "service", "web", "remote_port", 80)).(PortForwardOutput)
	if res.Service != "web" || res.Pod != "web-b" || res.Target != "web-b:8080" {
		t.Fatalf("%+v", res)
	}
	echoThrough(t, res.Listen)
	if got := <-served; got != "web-b:8080" {
		t.Fatalf("served %s", got)
	}
	res = f.invoke(t, desktop, "k8s.port_forward", args("context", "prod", "service", "api", "remote_port", 80)).(PortForwardOutput)
	if res.Target != "api-1:80" {
		t.Fatalf("target port 0 means the service port: %+v", res)
	}
}

func TestPortForwardRefused(t *testing.T) {
	pend := readyPod("web-p", "web", false)
	pend.Status.Phase = corev1.PodPending
	f := setup(t, pend, service("web", 80, intstr.FromInt32(8080)), readyPod("web-x", "web", false), &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "ext"},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}}})
	withTunnels(t, f)
	for _, c := range []struct {
		args []any
		want string
	}{
		{[]any{"pod", "web-p", "remote_port", 80}, "Pending"},
		{[]any{"pod", "nope", "remote_port", 80}, "not found"},
		{[]any{"service", "web", "remote_port", 81}, "has no port 81"},
		{[]any{"service", "web", "remote_port", 80}, "no ready pod"},
		{[]any{"service", "ext", "remote_port", 80}, "no selector"},
		{[]any{"pod", "web-x", "service", "web", "remote_port", 80}, "either pod or service"},
		{[]any{"pod", "web-x"}, "remote_port"},
	} {
		out := f.e.Invoke(context.Background(), codex, "k8s.port_forward", args(append([]any{"context", "prod"}, c.args...)...))
		if out.Status != engine.StatusError || !strings.Contains(out.Error, c.want) {
			t.Errorf("%v: %+v", c.args, out)
		}
	}
	f.p.Tunnels = nil
	if out := f.e.Invoke(context.Background(), codex, "k8s.port_forward", args("context", "prod", "pod", "web-x", "remote_port", 80)); out.Status != engine.StatusError {
		t.Fatalf("%+v", out)
	}
}

func TestPortForwardReconnects(t *testing.T) {
	f := setup(t, service("web", 80, intstr.FromInt32(8080)), readyPod("web-b", "web", true))
	m := withTunnels(t, f)
	events, stop := f.e.Events.Subscribe()
	defer stop()
	res := f.invoke(t, desktop, "k8s.port_forward", args("context", "prod", "service", "web", "remote_port", 80)).(PortForwardOutput)
	// The pod is replaced; the reconnect finds the new one.
	podGVR := corev1.SchemeGroupVersion.WithResource("pods")
	f.fakes["prod"].Typed.Tracker().Delete(podGVR, "shop", "web-b")
	f.fakes["prod"].Typed.Tracker().Add(readyPod("web-c", "web", true))
	f.fakes["prod"].Forwards.Drop()
	deadline := time.After(60 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Type == "tunnel.reconnected" && ev.Tunnel == res.ID {
				tn, _ := m.Get(res.ID)
				if tn.Pod != "web-c" {
					t.Fatalf("%+v", tn)
				}
				echoThrough(t, res.Listen)
				return
			}
		case <-deadline:
			t.Fatal("no reconnect")
		}
	}
}
