package k8s

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/hua-ri/coxswain/internal/inventory"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func waiting(p *corev1.Pod, reason, msg string) *corev1.Pod {
	p.Status.ContainerStatuses[0].Ready = false
	p.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg}}
	return p
}

func pending(name, msg string) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: meta("default", name, time.Minute),
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "web:1"}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: msg}}}}
	return p
}

func oomPod() *corev1.Pod {
	p := runningPod("default", "web-1", "node-1")
	p.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")}
	p.Status.ContainerStatuses[0].RestartCount = 4
	p.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		Reason: "OOMKilled", ExitCode: 137, FinishedAt: metav1.NewTime(now.Add(-2 * time.Minute))}}
	return p
}

func pressured(n *corev1.Node) *corev1.Node {
	n.Status.Conditions = append(n.Status.Conditions,
		corev1.NodeCondition{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue, Message: "kubelet has insufficient memory available"},
		corev1.NodeCondition{Type: corev1.NodeDiskPressure, Status: corev1.ConditionFalse})
	return n
}

// findings renders findings as "severity: title — detail" lines.
func findings(d DiagnoseOutput) string {
	var lines []string
	for _, f := range d.Findings {
		lines = append(lines, f.Severity+": "+f.Title+" — "+f.Detail)
	}
	return strings.Join(lines, "\n")
}

func TestDiagnosePod(t *testing.T) {
	pvcPod := pending("db-0", "0/3 nodes are available: pod has unbound immediate PersistentVolumeClaims.")
	pvcPod.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-db-0"}}}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: meta("default", "data-db-0", time.Minute),
		Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: new("fast")}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending}}
	notReady := runningPod("default", "web-1", "node-1")
	notReady.Status.ContainerStatuses[0].Ready = false

	for _, c := range []struct {
		name string
		objs []runtime.Object
		want []string // substrings of the findings
		logs bool     // previous logs collected
		not  string   // must not appear
	}{
		{"crashloop", []runtime.Object{crashingPod("default", "web-1")},
			[]string{"critical: Container app is crash-looping", "exit code 1", "7 restarts"}, true, ""},
		{"oomkilled", []runtime.Object{oomPod()},
			[]string{"Container app was OOM-killed", "exit code 137", "memory limit 128Mi"}, true, ""},
		{"image pull", []runtime.Object{waiting(runningPod("default", "web-1", "node-1"), "ImagePullBackOff", `Back-off pulling image "web:nope"`)},
			[]string{"critical: Cannot pull image nginx:1", `Back-off pulling image \"web:nope\"`}, false, ""},
		{"config error", []runtime.Object{waiting(runningPod("default", "web-1", "node-1"), "CreateContainerConfigError", `secret "db" not found`)},
			[]string{"critical: Container app cannot be created", `secret \"db\" not found`}, false, ""},
		{"insufficient", []runtime.Object{pending("web-1", "0/3 nodes are available: 3 Insufficient memory.")},
			[]string{"critical: Pod cannot be scheduled: not enough cpu or memory", "Insufficient memory"}, false, ""},
		{"taint", []runtime.Object{pending("web-1", "0/1 nodes are available: 1 node(s) had untolerated taint {node-role.kubernetes.io/control-plane: }.")},
			[]string{"Pod cannot be scheduled: node taints", "untolerated taint"}, false, ""},
		{"selector", []runtime.Object{pending("web-1", "0/2 nodes are available: 2 node(s) didn't match Pod's node affinity/selector.")},
			[]string{"Pod cannot be scheduled: node selector or affinity"}, false, ""},
		{"pvc", []runtime.Object{pvcPod, pvc},
			[]string{"Pod cannot be scheduled: unbound PersistentVolumeClaim", "PersistentVolumeClaim data-db-0 is Pending (storage class fast)"}, false, ""},
		{"probe", []runtime.Object{notReady, event("default", "web-1", "Pod", "Warning", "Unhealthy",
			"Readiness probe failed: HTTP probe failed with statuscode: 500", time.Minute)},
			[]string{"warning: Readiness probe failing", "statuscode: 500"}, false, ""},
		{"healthy", []runtime.Object{runningPod("default", "web-1", "node-1")},
			[]string{"info: No problems found"}, false, "critical"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t, append(c.objs, node("node-1", "10.1.0.5", true))...)
			name := "web-1"
			if c.name == "pvc" {
				name = "db-0"
			}
			d := f.invoke(t, codex, "k8s.diagnose", args("context", "dev", "kind", "pod", "name", name)).(DiagnoseOutput)
			got := findings(d) + "\n" + jsonOf(t, d.Findings)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("findings lack %q:\n%s", w, findings(d))
				}
			}
			if c.not != "" && strings.Contains(got, c.not) {
				t.Errorf("findings have %q:\n%s", c.not, findings(d))
			}
			if c.name != "healthy" && len(d.NextSteps) == 0 {
				t.Error("no next steps")
			}
			hasLogs := len(d.Evidence.Pods) > 0 && len(d.Evidence.Pods[0].PreviousLogs) > 0
			if hasLogs != c.logs {
				t.Errorf("previous logs collected: %v, want %v", hasLogs, c.logs)
			}
			if hasLogs && d.Evidence.Pods[0].PreviousLogs["app"].Text != "fake logs" {
				t.Errorf("logs %+v", d.Evidence.Pods[0].PreviousLogs)
			}
			if ev := f.requested(t, "k8s.diagnose"); ev.Risk != "read" || ev.Targets[0].ID != "k8s:dev/default" {
				t.Errorf("audit %+v", ev)
			}
		})
	}
}

func TestDiagnoseNode(t *testing.T) {
	f := setup(t, crashingPod("default", "web-1"), pressured(node("node-1", "10.1.0.5", false)),
		event("default", "web-1", "Pod", "Warning", "BackOff", "Back-off restarting failed container", time.Minute))
	f.p.Hosts.Merge([]inventory.Host{{Name: "k8s-node-1", HostName: "10.1.0.5"}})
	d := f.invoke(t, codex, "k8s.diagnose", args("context", "dev", "kind", "pod", "name", "web-1")).(DiagnoseOutput)
	got := findings(d)
	for _, w := range []string{"critical: Node node-1 is not ready", "warning: Node node-1 has MemoryPressure", "insufficient memory"} {
		if !strings.Contains(got, w) {
			t.Errorf("lack %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "DiskPressure") {
		t.Errorf("a false condition was reported:\n%s", got)
	}
	if len(d.Evidence.Nodes) != 1 || d.Evidence.Nodes[0].SSHHost != "k8s-node-1" {
		t.Fatalf("nodes %+v", d.Evidence.Nodes)
	}
	if !strings.Contains(strings.Join(d.NextSteps, "\n"), "ssh_exec on k8s-node-1") {
		t.Fatalf("next steps %v", d.NextSteps)
	}
	if len(d.Evidence.Events) != 1 || d.Evidence.Events[0].Reason != "BackOff" {
		t.Fatalf("events %+v", d.Evidence.Events)
	}
}

func TestDiagnoseControllers(t *testing.T) {
	d := deployment("default", "web", 3, 1)
	d.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse,
		Reason: "MinimumReplicasUnavailable", Message: "Deployment does not have minimum availability."}}
	healthy := runningPod("default", "web-a", "node-1")
	healthy.Labels["app"] = "web"
	var crashing []runtime.Object
	for _, n := range []string{"web-b", "web-c", "web-d", "web-e"} {
		p := crashingPod("default", n)
		p.Labels["app"] = "web"
		crashing = append(crashing, p)
	}
	other := crashingPod("default", "db-1") // not selected
	rsEvent := event("default", "web-7d9", "ReplicaSet", "Warning", "FailedCreate",
		`pods "web-7d9-x" is forbidden: exceeded quota: compute, requested: cpu=1, used: cpu=4, limited: cpu=4`, time.Minute)
	f := setup(t, append(crashing, d, healthy, other, node("node-1", "10.1.0.5", true), rsEvent,
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web-7d9",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web", UID: d.UID}}}})...)
	out := f.invoke(t, codex, "k8s.diagnose", args("context", "dev", "kind", "deployment", "name", "web")).(DiagnoseOutput)
	got := findings(out)
	for _, w := range []string{"1 of 3 replicas ready", "Pod web-b: Container app is crash-looping", "Cannot create pods", "exceeded quota"} {
		if !strings.Contains(got, w) {
			t.Errorf("lack %q:\n%s", w, got)
		}
	}
	if len(out.Evidence.Pods) != 3 || strings.Contains(got, "db-1") || strings.Contains(got, "web-a:") {
		t.Fatalf("pods: %d\n%s", len(out.Evidence.Pods), got)
	}
	if out.Evidence.Workload["ready"] != "1/3" {
		t.Fatalf("workload %+v", out.Evidence.Workload)
	}

	job := &batchv1.Job{ObjectMeta: meta("default", "migrate", time.Hour), Spec: batchv1.JobSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"job": "migrate"}}},
		Status: batchv1.JobStatus{Failed: 6, Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit"}}}}
	f = setup(t, job)
	out = f.invoke(t, codex, "k8s.diagnose", args("context", "dev", "kind", "job", "name", "migrate")).(DiagnoseOutput)
	if got := findings(out); !strings.Contains(got, "critical: Job failed") || !strings.Contains(got, "BackoffLimitExceeded") {
		t.Fatalf("job:\n%s", got)
	}
	if res := f.e.Invoke(context.Background(), codex, "k8s.diagnose", args("context", "dev", "kind", "service", "name", "x")); res.Status != engine.StatusError {
		t.Fatalf("service: %+v", res)
	}
	if res := f.e.Invoke(context.Background(), codex, "k8s.diagnose", args("context", "dev", "kind", "pod", "name", "nope")); res.Status != engine.StatusError ||
		!strings.Contains(res.Error, "not found") {
		t.Fatalf("missing pod: %+v", res)
	}
}

func TestNodeHost(t *testing.T) {
	f := setup(t, node("node-1", "10.1.0.5", true), node("node-2", "10.1.0.6", true), node("node-3", "10.1.0.7", true),
		node("node-4", "10.1.0.8", true))
	f.p.Hosts.Merge([]inventory.Host{
		{Name: "k8s-node-1", HostName: "10.1.0.5"}, {Name: "staging-box", HostName: "10.1.0.5"}, // same private IP elsewhere
		{Name: "n2", HostName: "NODE-2"},
		{Name: "n4-tagged", HostName: "bastion.example"}, {Name: "n4-ip", HostName: "10.1.0.8"},
	})
	f.p.Hosts.Update(func(m map[string]inventory.Host) error {
		h := m["n4-tagged"]
		h.Tags = map[string]string{"k8s-node": "dev/node-4"}
		m["n4-tagged"] = h
		h = m["staging-box"]
		h.Tags = map[string]string{"env": "staging"}
		m["staging-box"] = h
		return nil
	})
	// Two hosts share the address: no guess, both shown, the user is asked to tag one.
	res := f.invoke(t, codex, "k8s.node_host", args("context", "dev", "node", "node-1")).(NodeHostOutput)
	if res.Host != "" || len(res.Candidates) != 2 || res.Candidates[1].Name != "staging-box" || res.Candidates[1].Tags["env"] != "staging" ||
		res.Candidates[0].MatchedBy != "address" || !strings.Contains(res.Note, "k8s-node=dev/node-1") {
		t.Fatalf("ambiguous: %+v", res)
	}
	res = f.invoke(t, codex, "k8s.node_host", args("context", "dev", "node", "node-2")).(NodeHostOutput)
	if res.Host != "n2" || res.MatchedBy != "hostname" || len(res.Candidates) != 1 {
		t.Fatalf("single candidate: %+v", res)
	}
	res = f.invoke(t, codex, "k8s.node_host", args("context", "dev", "node", "node-4")).(NodeHostOutput)
	if res.Host != "n4-tagged" || res.MatchedBy != "tag" || len(res.Candidates) != 2 {
		t.Fatalf("explicit tag wins: %+v", res)
	}
	res = f.invoke(t, codex, "k8s.node_host", args("context", "dev", "node", "node-3")).(NodeHostOutput)
	if res.Host != "" || len(res.Candidates) != 0 || !strings.Contains(res.Note, "10.1.0.7") {
		t.Fatalf("no match: %+v", res)
	}
	if ev := f.requested(t, "k8s.node_host"); ev.Risk != "read" || ev.Targets[0].ID != "k8s:dev" {
		t.Fatalf("audit %+v", ev)
	}
	if out := f.e.Invoke(context.Background(), codex, "k8s.node_host", args("context", "dev", "node", "nope")); out.Status != engine.StatusError {
		t.Fatalf("%+v", out)
	}
}

// TestDiagnoseAmbiguousNodeHost: with two candidate hosts, diagnose
// shows both and gives no SSH step.
func TestDiagnoseAmbiguousNodeHost(t *testing.T) {
	f := setup(t, crashingPod("default", "web-1"), pressured(node("node-1", "10.1.0.5", false)))
	f.p.Hosts.Merge([]inventory.Host{{Name: "k8s-node-1", HostName: "10.1.0.5"}, {Name: "staging-box", HostName: "10.1.0.5"}})
	d := f.invoke(t, codex, "k8s.diagnose", args("context", "dev", "kind", "pod", "name", "web-1")).(DiagnoseOutput)
	steps := strings.Join(d.NextSteps, "\n")
	if strings.Contains(steps, "ssh_exec") || !strings.Contains(steps, "k8s-node=dev/node-1") {
		t.Fatalf("next steps %v", d.NextSteps)
	}
	if n := d.Evidence.Nodes[0]; n.SSHHost != "" || len(n.SSHCandidates) != 2 {
		t.Fatalf("node %+v", n)
	}
}
