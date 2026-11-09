package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/hua-ri/coxswain/internal/action"
	kube "github.com/hua-ri/coxswain/internal/k8s"
	"github.com/hua-ri/coxswain/internal/shaper"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
)

// Finding severities.
const (
	critical = "critical"
	warning  = "warning"
	info     = "info"
)

// Finding is one problem diagnose found.
type Finding struct {
	Severity     string   `json:"severity"` // critical, warning or info
	Title        string   `json:"title"`
	Detail       string   `json:"detail,omitempty"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"` // e.g. pod/web-1, node/node-1, events
}

// PodEvidence is what diagnose collected about one pod.
type PodEvidence struct {
	Name         string                   `json:"name"`
	Phase        string                   `json:"phase"`
	Status       string                   `json:"status"`
	Node         string                   `json:"node,omitempty"`
	Containers   []ContainerInfo          `json:"containers"`
	PreviousLogs map[string]shaper.Shaped `json:"previous_logs,omitempty"` // by container: tail of the crashed instance
}

// NodeEvidence is the state of a node a diagnosed pod runs on.
type NodeEvidence struct {
	Name     string   `json:"name"`
	Status   string   `json:"status"`
	Problems []string `json:"problems,omitempty"` // conditions that are not as they should be
	// SSHHost is the node's saved SSH host: one tagged k8s-node=<context>/<node>,
	// or the only one whose address matches. SSHCandidates are all that match.
	SSHHost       string          `json:"ssh_host,omitempty"`
	SSHCandidates []HostCandidate `json:"ssh_candidates,omitempty"`
}

// Evidence is the data behind the findings.
type Evidence struct {
	Workload map[string]string `json:"workload,omitempty"` // the controller's summary
	Pods     []PodEvidence     `json:"pods,omitempty"`
	Nodes    []NodeEvidence    `json:"nodes,omitempty"`
	Events   []EventRow        `json:"events,omitempty"` // Warning events, newest first
}

// DiagnoseOutput is the result of k8s.diagnose.
type DiagnoseOutput struct {
	Context   string    `json:"context"`
	Kind      string    `json:"kind"`
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	Findings  []Finding `json:"findings"`
	Evidence  Evidence  `json:"evidence"`
	NextSteps []string  `json:"next_steps"`
}

// Limits of what diagnose collects.
const (
	maxDiagnosedPods = 3
	maxEvidenceEvent = 20
	previousLogLines = 50
)

// diagnosis accumulates findings, evidence and next steps.
type diagnosis struct {
	out   DiagnoseOutput
	steps map[string]bool
	nodes map[string]bool
}

func (d *diagnosis) find(sev, title, detail string, refs ...string) {
	d.out.Findings = append(d.out.Findings, Finding{Severity: sev, Title: title, Detail: detail, EvidenceRefs: refs})
}

func (d *diagnosis) next(step string) {
	if !d.steps[step] {
		d.steps[step] = true
		d.out.NextSteps = append(d.out.NextSteps, step)
	}
}

var diagnoseKinds = map[string]string{
	"pod": "Pod", "pods": "Pod", "po": "Pod",
	"deployment": "Deployment", "deployments": "Deployment", "deploy": "Deployment",
	"statefulset": "StatefulSet", "statefulsets": "StatefulSet", "sts": "StatefulSet",
	"daemonset": "DaemonSet", "daemonsets": "DaemonSet", "ds": "DaemonSet",
	"job": "Job", "jobs": "Job",
}

func (p *Provider) diagnose() *action.Action {
	return &action.Action{
		ID:      "k8s.diagnose",
		Summary: "Find out why a pod or workload is unhealthy",
		Description: "Check a pod, deployment, statefulset, daemonset or job for known problems (crash loops, OOM kills, " +
			"image pull errors, missing config, scheduling failures from resources, taints, selectors or unbound PVCs, failing " +
			"probes, node pressure, failed jobs, quota) and return findings with evidence: warning events, container states, " +
			"the previous logs of crashed containers, the nodes and their SSH hosts, and next steps. For a controller the " +
			"unhealthy pods (up to 3) are diagnosed too. Start here before reading logs.",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"context":   contextProperty,
				"kind":      map[string]any{"type": "string", "enum": []string{"pod", "deployment", "statefulset", "daemonset", "job"}},
				"name":      map[string]any{"type": "string"},
				"namespace": namespaceProperty,
			},
			"required": []string{"context", "kind", "name"},
		},
		Positional:  []string{"context", "kind", "name"},
		Annotations: readOnlyRemote,
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			var in scope
			if err := action.Decode(raw, &in); err != nil {
				return action.Assessment{}, err
			}
			kind, ok := diagnoseKinds[strings.ToLower(in.Kind)]
			if !ok {
				return action.Assessment{}, fmt.Errorf("kind must be pod, deployment, statefulset, daemonset or job")
			}
			in.Kind, in.AllNamespaces = "", false
			r, err := p.resolve(in)
			if err != nil {
				return action.Assessment{}, err
			}
			return action.Assessment{Risk: action.Read, Targets: []action.Target{p.Clusters.Target(in.Context, r.ns)},
				Preview: fmt.Sprintf("diagnose %s/%s in %s/%s", kind, in.Name, in.Context, r.ns)}, nil
		},
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in scope
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			kind, ok := diagnoseKinds[strings.ToLower(in.Kind)]
			if !ok {
				return nil, fmt.Errorf("kind must be pod, deployment, statefulset, daemonset or job")
			}
			if in.Name == "" {
				return nil, fmt.Errorf("name is required")
			}
			in.Kind, in.AllNamespaces = "", false
			r, err := p.resolve(in)
			if err != nil {
				return nil, err
			}
			d := &diagnosis{out: DiagnoseOutput{Context: in.Context, Kind: kind, Namespace: r.ns, Name: in.Name},
				steps: map[string]bool{}, nodes: map[string]bool{}}
			if err := p.diagnoseTarget(ctx, r, d, kind, in.Name); err != nil {
				return nil, err
			}
			if len(d.out.Findings) == 0 {
				d.find(info, "No problems found", fmt.Sprintf("%s %s looks healthy", kind, in.Name))
			}
			sort.SliceStable(d.out.Findings, func(i, j int) bool {
				return severityRank(d.out.Findings[i].Severity) < severityRank(d.out.Findings[j].Severity)
			})
			if d.out.NextSteps == nil {
				d.out.NextSteps = []string{}
			}
			return d.out, nil
		},
	}
}

func severityRank(s string) int {
	switch s {
	case critical:
		return 0
	case warning:
		return 1
	}
	return 2
}

func (p *Provider) diagnoseTarget(ctx context.Context, r resolved, d *diagnosis, kind, name string) error {
	c, ns := r.client, r.ns
	if kind == "Pod" {
		pod, err := c.Typed.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return kube.Explain(err)
		}
		events, err := p.warnings(ctx, c, ns)
		if err != nil {
			return err
		}
		p.diagnosePod(ctx, r, d, pod, "", events)
		d.out.Evidence.Events = p.pick(events, map[string]bool{"Pod/" + name: true})
		return nil
	}
	obj, selector, err := p.controller(ctx, c, ns, kind, name)
	if err != nil {
		return err
	}
	d.out.Evidence.Workload = summaryMap(kind, obj, p.now())
	about := map[string]bool{kind + "/" + name: true}
	p.controllerRules(d, kind, obj)

	events, err := p.warnings(ctx, c, ns)
	if err != nil {
		return err
	}
	if kind == "Deployment" {
		rsList, err := c.Typed.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return kube.Explain(err)
		}
		for _, rs := range rsList.Items {
			for _, o := range rs.OwnerReferences {
				if o.Kind == "Deployment" && o.Name == name {
					about["ReplicaSet/"+rs.Name] = true
				}
			}
		}
	}
	for _, e := range events {
		if about[e.InvolvedObject.Kind+"/"+e.InvolvedObject.Name] && e.Reason == "FailedCreate" {
			d.find(critical, "Cannot create pods", clipLine(e.Message, 400), "events")
			d.next("Fix what blocks pod creation (quota, admission policy, service account) named in the event")
			break
		}
	}

	if selector != nil {
		pods, err := c.Typed.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
		if err != nil {
			return kube.Explain(err)
		}
		items := pods.Items
		sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
		n := 0
		for i := range items {
			if !unhealthy(&items[i]) {
				continue
			}
			if n == maxDiagnosedPods {
				d.find(info, "More unhealthy pods", "only the first 3 unhealthy pods were diagnosed")
				break
			}
			n++
			about["Pod/"+items[i].Name] = true
			p.diagnosePod(ctx, r, d, &items[i], "Pod "+items[i].Name+": ", events)
		}
	}
	d.out.Evidence.Events = p.pick(events, about)
	return nil
}

// warnings returns the namespace's Warning events, newest first.
func (p *Provider) warnings(ctx context.Context, c *kube.Client, ns string) ([]corev1.Event, error) {
	list, err := c.Typed.CoreV1().Events(ns).List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("type", corev1.EventTypeWarning).String()})
	if err != nil {
		return nil, kube.Explain(err)
	}
	var out []corev1.Event
	for _, e := range list.Items {
		if e.Type == corev1.EventTypeWarning {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return eventTime(&out[i]).After(eventTime(&out[j])) })
	return out, nil
}

// pick returns up to maxEvidenceEvent events about the given Kind/name objects.
func (p *Provider) pick(events []corev1.Event, about map[string]bool) []EventRow {
	var out []corev1.Event
	for _, e := range events {
		if about[e.InvolvedObject.Kind+"/"+e.InvolvedObject.Name] && len(out) < maxEvidenceEvent {
			out = append(out, e)
		}
	}
	return p.eventRows(out, false)
}

// controller fetches a controller and the selector of its pods.
func (p *Provider) controller(ctx context.Context, c *kube.Client, ns, kind, name string) (*unstructured.Unstructured, labels.Selector, error) {
	var obj runtime.Object
	var sel *metav1.LabelSelector
	var err error
	apps := c.Typed.AppsV1()
	switch kind {
	case "Deployment":
		obj, err = apps.Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	case "StatefulSet":
		obj, err = apps.StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
	case "DaemonSet":
		obj, err = apps.DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
	case "Job":
		obj, err = c.Typed.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
	}
	if err != nil {
		return nil, nil, kube.Explain(err)
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, nil, err
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetKind(kind)
	if raw, ok, _ := unstructured.NestedMap(m, "spec", "selector"); ok {
		sel = &metav1.LabelSelector{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, sel); err != nil {
			return nil, nil, err
		}
	}
	if sel == nil {
		return u, nil, nil
	}
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return nil, nil, err
	}
	return u, s, nil
}

// controllerRules checks a controller's own status.
func (p *Provider) controllerRules(d *diagnosis, kind string, u *unstructured.Unstructured) {
	ref := strings.ToLower(kind) + "/" + u.GetName()
	if kind == "Job" {
		job := convert[batchv1.Job](u)
		for _, c := range job.Status.Conditions {
			if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
				d.find(critical, "Job failed", strings.TrimSpace(c.Reason+": "+c.Message)+fmt.Sprintf(" (%d failed pods)", job.Status.Failed), ref)
				d.next("Read the failed pods' logs (k8s_logs with previous=true) to see why they exited")
			}
		}
		if job.Status.Active > 0 && job.Status.Failed > 0 {
			d.find(warning, fmt.Sprintf("%d pods of the job failed", job.Status.Failed), "the job is retrying", ref)
		}
		return
	}
	var want, ready int64
	switch kind {
	case "Deployment", "StatefulSet":
		var ok bool
		if want, ok, _ = unstructured.NestedInt64(u.Object, "spec", "replicas"); !ok {
			want = 1
		}
		ready, _, _ = unstructured.NestedInt64(u.Object, "status", "readyReplicas")
	case "DaemonSet":
		want, _, _ = unstructured.NestedInt64(u.Object, "status", "desiredNumberScheduled")
		ready, _, _ = unstructured.NestedInt64(u.Object, "status", "numberReady")
	}
	if ready < want {
		sev := warning
		if ready == 0 {
			sev = critical
		}
		d.find(sev, fmt.Sprintf("%d of %d replicas ready", ready, want), "", ref)
	}
	for _, c := range conditions(u) {
		switch {
		case c.Reason == "ProgressDeadlineExceeded":
			d.find(critical, "Rollout stuck: progress deadline exceeded", c.Message, ref)
			d.next("Fix the new pods' problem, or roll back with k8s_rollout action=undo")
		case c.Type == "Available" && c.Status == "False":
			d.find(warning, kind+" not available", strings.TrimSpace(c.Reason+": "+c.Message), ref)
		case c.Type == "ReplicaFailure" && c.Status == "True":
			d.find(critical, "Cannot create pods", c.Message, ref)
		}
	}
	if done, msg := rolloutStatus(u); !done && ready >= want {
		d.find(info, "Rollout in progress", msg, ref)
	}
}

// unhealthy reports whether a pod is worth diagnosing.
func unhealthy(pod *corev1.Pod) bool {
	if pod.Status.Phase == corev1.PodSucceeded {
		return false
	}
	if pod.Status.Phase != corev1.PodRunning {
		return true
	}
	for _, c := range pod.Status.ContainerStatuses {
		if !c.Ready || c.State.Waiting != nil {
			return true
		}
	}
	return false
}

// diagnosePod applies the pod rules; prefix names the pod in titles of a
// controller's diagnosis.
func (p *Provider) diagnosePod(ctx context.Context, r resolved, d *diagnosis, pod *corev1.Pod, prefix string, events []corev1.Event) {
	now := p.now()
	ref := "pod/" + pod.Name
	ev := PodEvidence{Name: pod.Name, Phase: string(pod.Status.Phase), Status: podStatus(pod), Node: pod.Spec.NodeName,
		Containers: containers(pod, now)}
	limits := map[string]string{}
	for _, c := range append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...) {
		if m, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			limits[c.Name] = m.String()
		}
	}
	images := map[string]string{}
	for _, c := range append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...) {
		images[c.Name] = c.Image
	}
	crashed := map[string]bool{}
	for _, s := range append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
		last := s.LastTerminationState.Terminated
		if w := s.State.Waiting; w != nil {
			switch w.Reason {
			case "CrashLoopBackOff":
				detail := fmt.Sprintf("%d restarts", s.RestartCount)
				if last != nil {
					detail = fmt.Sprintf("last exit: %s, exit code %d; %s", dash(last.Reason), last.ExitCode, detail)
				}
				if last != nil && last.Reason == "OOMKilled" {
					d.find(critical, prefix+"Container "+s.Name+" was OOM-killed", oomDetail(last, limits[s.Name], s.RestartCount), ref)
					d.next("Raise the container's memory limit or reduce its memory use")
				} else {
					d.find(critical, prefix+"Container "+s.Name+" is crash-looping", detail, ref, "logs:"+pod.Name+"/"+s.Name)
					d.next("Read the crashed container's last log lines (evidence previous_logs) and fix what makes it exit")
				}
				crashed[s.Name] = true
			case "ImagePullBackOff", "ErrImagePull", "InvalidImageName", "ErrImageNeverPull":
				d.find(critical, prefix+"Cannot pull image "+images[s.Name], clipLine(w.Reason+": "+w.Message, 400), ref)
				d.next("Check the image name and tag, that it exists in the registry, and the pod's imagePullSecrets")
			case "CreateContainerConfigError", "CreateContainerError", "RunContainerError":
				d.find(critical, prefix+"Container "+s.Name+" cannot be created", clipLine(w.Reason+": "+w.Message, 400), ref)
				d.next("Create the Secret or ConfigMap (or key) the container refers to, or fix the reference")
			}
		} else if last != nil && last.Reason == "OOMKilled" {
			d.find(warning, prefix+"Container "+s.Name+" was OOM-killed", oomDetail(last, limits[s.Name], s.RestartCount), ref)
			d.next("Raise the container's memory limit or reduce its memory use")
			crashed[s.Name] = true
		} else if t := s.State.Terminated; t != nil && t.ExitCode != 0 && pod.Status.Phase != corev1.PodRunning {
			d.find(warning, prefix+"Container "+s.Name+" exited with code "+fmt.Sprint(t.ExitCode), dash(t.Reason), ref)
		} else if last != nil && s.RestartCount >= 3 {
			d.find(warning, prefix+fmt.Sprintf("Container %s restarted %d times", s.Name, s.RestartCount),
				fmt.Sprintf("last exit: %s, exit code %d", dash(last.Reason), last.ExitCode), ref)
			crashed[s.Name] = true
		}
	}

	// Probe failures.
	probes := map[string]string{}
	for _, e := range events {
		if e.InvolvedObject.Kind == "Pod" && e.InvolvedObject.Name == pod.Name && e.Reason == "Unhealthy" {
			for _, kind := range []string{"Readiness", "Liveness", "Startup"} {
				if strings.HasPrefix(e.Message, kind+" probe failed") && probes[kind] == "" {
					probes[kind] = e.Message
				}
			}
		}
	}
	for _, kind := range []string{"Liveness", "Readiness", "Startup"} {
		if msg := probes[kind]; msg != "" {
			sev := warning
			if kind == "Liveness" {
				sev = critical // the kubelet restarts the container
			}
			d.find(sev, prefix+kind+" probe failing", clipLine(msg, 400), ref, "events")
			d.next("Check the probe's path, port and timing against the app (does it start in time? does the endpoint work?)")
		}
	}

	// Scheduling.
	if pod.Status.Phase == corev1.PodPending && pod.Spec.NodeName == "" {
		msg := ""
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
				msg = c.Message
			}
		}
		for _, e := range events {
			if msg == "" && e.InvolvedObject.Name == pod.Name && e.Reason == "FailedScheduling" {
				msg = e.Message
			}
		}
		p.schedulingRules(ctx, r, d, pod, prefix, msg)
	}

	// The node.
	if pod.Spec.NodeName != "" && !d.nodes[pod.Spec.NodeName] {
		d.nodes[pod.Spec.NodeName] = true
		p.nodeRules(ctx, r, d, pod.Spec.NodeName)
	}

	// Previous logs of crashed containers.
	for name := range crashed {
		tail := int64(previousLogLines)
		text, err := p.readLogs(ctx, r.client, pod, &corev1.PodLogOptions{Container: name, Previous: true, TailLines: &tail})
		if err != nil {
			text = "(previous logs unavailable: " + err.Error() + ")"
		}
		shaped, err := p.Artifacts.Shape(text, shaper.Limits{MaxBytes: 4 << 10, MaxLines: previousLogLines})
		if err != nil {
			continue
		}
		if ev.PreviousLogs == nil {
			ev.PreviousLogs = map[string]shaper.Shaped{}
		}
		ev.PreviousLogs[name] = shaped
	}
	d.out.Evidence.Pods = append(d.out.Evidence.Pods, ev)
}

func oomDetail(t *corev1.ContainerStateTerminated, limit string, restarts int32) string {
	s := fmt.Sprintf("exit code %d, %d restarts", t.ExitCode, restarts)
	if limit != "" {
		s += ", memory limit " + limit
	} else {
		s += ", no memory limit (the node ran out of memory)"
	}
	return s
}

// schedulingRules explains why a pod is not scheduled.
func (p *Provider) schedulingRules(ctx context.Context, r resolved, d *diagnosis, pod *corev1.Pod, prefix, msg string) {
	ref := "pod/" + pod.Name
	lower := strings.ToLower(msg)
	var causes []string
	if strings.Contains(lower, "insufficient") {
		causes = append(causes, "not enough cpu or memory")
		d.next("Free capacity (or add nodes), or lower the pod's resource requests")
	}
	if strings.Contains(lower, "taint") {
		causes = append(causes, "node taints")
		d.next("Add a toleration for the taint to the pod, or remove the taint from the nodes")
	}
	if strings.Contains(lower, "affinity") || strings.Contains(lower, "selector") {
		causes = append(causes, "node selector or affinity")
		d.next("Check the pod's nodeSelector / affinity against the nodes' labels")
	}
	if strings.Contains(lower, "persistentvolumeclaim") {
		causes = append(causes, "unbound PersistentVolumeClaim")
	}
	// Claims that are not bound, whatever the message says.
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		name := v.PersistentVolumeClaim.ClaimName
		pvc, err := r.client.Typed.CoreV1().PersistentVolumeClaims(pod.Namespace).Get(ctx, name, metav1.GetOptions{})
		var detail string
		switch {
		case err != nil:
			detail = fmt.Sprintf("PersistentVolumeClaim %s: %v", name, kube.Explain(err))
		case pvc.Status.Phase != corev1.ClaimBound:
			sc := "default storage class"
			if pvc.Spec.StorageClassName != nil {
				sc = "storage class " + *pvc.Spec.StorageClassName
			}
			detail = fmt.Sprintf("PersistentVolumeClaim %s is %s (%s)", name, pvc.Status.Phase, sc)
		default:
			continue
		}
		if !contains(causes, "unbound PersistentVolumeClaim") {
			causes = append(causes, "unbound PersistentVolumeClaim")
		}
		d.find(warning, prefix+"Volume claim not bound", detail, "pvc/"+name)
		d.next("Check the PersistentVolumeClaim's events and its StorageClass (provisioner running? capacity?)")
	}
	if contains(causes, "unbound PersistentVolumeClaim") {
		d.next("Check the PersistentVolumeClaim's events and its StorageClass (provisioner running? capacity?)")
	}
	title := prefix + "Pod cannot be scheduled"
	if len(causes) > 0 {
		title += ": " + strings.Join(causes, ", ")
	}
	if msg == "" {
		msg = "the pod is Pending and not assigned to a node"
	}
	d.find(critical, title, clipLine(msg, 400), ref, "events")
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// nodeRules checks the node a pod runs on and maps it to an SSH host.
func (p *Provider) nodeRules(ctx context.Context, r resolved, d *diagnosis, name string) {
	node, err := r.client.Typed.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		d.find(info, "Node "+name+" could not be read", kube.Explain(err).Error())
		return
	}
	ev := NodeEvidence{Name: name, Status: nodeStatus(node)}
	ref := "node/" + name
	for _, c := range node.Status.Conditions {
		switch {
		case c.Type == corev1.NodeReady && c.Status != corev1.ConditionTrue:
			ev.Problems = append(ev.Problems, "NotReady")
			d.find(critical, "Node "+name+" is not ready", strings.TrimSpace(c.Reason+" "+c.Message), ref)
		case c.Type != corev1.NodeReady && c.Status == corev1.ConditionTrue:
			ev.Problems = append(ev.Problems, string(c.Type))
			d.find(warning, "Node "+name+" has "+string(c.Type), strings.TrimSpace(c.Message), ref)
		}
	}
	m, _ := p.nodeHosts(r.client.Context, node)
	ev.SSHHost, ev.SSHCandidates = m.Host, m.Candidates
	switch {
	case len(ev.Problems) == 0:
	case m.Host != "":
		d.next(fmt.Sprintf("Look at the node itself with ssh_exec on %s (dmesg -T | tail -50, df -h, free -m, journalctl -u kubelet -n 100)", m.Host))
	case len(m.Candidates) > 1:
		d.next(fmt.Sprintf("Several saved SSH hosts match node %s (%s); ask the user which one it is and to tag it "+
			"k8s-node=%s/%s before running anything on it", name, candidateNames(m.Candidates), r.client.Context, name))
	default:
		d.next("Look at node " + name + " (k8s_describe kind=node); k8s_node_host finds its SSH host if it is saved")
	}
	d.out.Evidence.Nodes = append(d.out.Evidence.Nodes, ev)
}

// HostCandidate is a saved SSH host that may be a node.
type HostCandidate struct {
	Name      string            `json:"name"`
	MatchedBy string            `json:"matched_by"` // tag, address or hostname
	Address   string            `json:"address,omitempty"`
	Tags      map[string]string `json:"tags,omitempty"`
	Note      string            `json:"note,omitempty"`
}

// hostMatch is what is known about a node's SSH host.
type hostMatch struct {
	Host       string // chosen: the tagged one, or the only candidate
	MatchedBy  string
	Candidates []HostCandidate
}

// nodeTag marks a saved SSH host as a node: k8s-node=<context>/<node>.
const nodeTag = "k8s-node"

func candidateNames(cs []HostCandidate) string {
	names := make([]string, len(cs))
	for i, c := range cs {
		names[i] = c.Name
	}
	return strings.Join(names, ", ")
}

// nodeHosts finds a node's saved SSH host. A host tagged
// k8s-node=<context>/<node> is the answer; otherwise hosts whose HostName
// is one of the node's addresses (or its name) are candidates, and one is
// chosen only if it is the only one: private addresses are reused across
// environments, so a guess could be the wrong machine.
func (p *Provider) nodeHosts(context string, node *corev1.Node) (hostMatch, error) {
	var m hostMatch
	if p.Hosts == nil {
		return m, nil
	}
	hosts, err := p.Hosts.List()
	if err != nil {
		return m, err
	}
	want := context + "/" + node.Name
	type addr struct{ by, value string }
	var addrs []addr
	for _, a := range node.Status.Addresses {
		by := "hostname"
		if a.Type == corev1.NodeInternalIP || a.Type == corev1.NodeExternalIP {
			by = "address"
		}
		if a.Address != "" {
			addrs = append(addrs, addr{by, a.Address})
		}
	}
	addrs = append(addrs, addr{"hostname", node.Name})
	var tagged, byAddress []HostCandidate
	for _, h := range hosts {
		if h.Tags[nodeTag] == want {
			tagged = append(tagged, HostCandidate{Name: h.Name, MatchedBy: "tag", Tags: h.Tags, Note: h.Note})
			continue
		}
		for _, a := range addrs {
			if strings.EqualFold(h.HostName, a.value) {
				byAddress = append(byAddress, HostCandidate{Name: h.Name, MatchedBy: a.by, Address: a.value, Tags: h.Tags, Note: h.Note})
				break
			}
		}
	}
	m.Candidates = append(tagged, byAddress...)
	switch {
	case len(tagged) == 1:
		m.Host, m.MatchedBy = tagged[0].Name, "tag"
	case len(tagged) == 0 && len(byAddress) == 1:
		m.Host, m.MatchedBy = byAddress[0].Name, byAddress[0].MatchedBy
	}
	return m, nil
}

// NodeHostOutput is the result of k8s.node_host.
type NodeHostOutput struct {
	Context   string   `json:"context"`
	Node      string   `json:"node"`
	Addresses []string `json:"addresses"` // Type=address
	// Host is the node's host: the one tagged k8s-node=<context>/<node>,
	// or the only candidate. MatchedBy: tag, address or hostname.
	Host       string            `json:"host,omitempty"`
	MatchedBy  string            `json:"matched_by,omitempty"`
	HostNote   string            `json:"host_note,omitempty"`
	HostTags   map[string]string `json:"host_tags,omitempty"`
	Candidates []HostCandidate   `json:"candidates"`
	Note       string            `json:"note,omitempty"`
}

type nodeHostInput struct {
	Context string `json:"context"`
	Node    string `json:"node"`
}

func (p *Provider) nodeHost() *action.Action {
	return &action.Action{
		ID:      "k8s.node_host",
		Summary: "Find the saved SSH host of a Kubernetes node",
		Description: "Find a node's saved SSH host, so you can inspect the machine with ssh_exec (dmesg, df -h, journalctl " +
			"-u kubelet). A host tagged k8s-node=<context>/<node> is the answer; otherwise hosts whose HostName is the node's " +
			"InternalIP, Hostname or ExternalIP are candidates, and host is set only if there is exactly one. With several " +
			"candidates, do not guess (private addresses repeat across environments): ask the user and have them tag the host.",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"context": contextProperty,
				"node":    map[string]any{"type": "string"},
			},
			"required": []string{"context", "node"},
		},
		Positional:  []string{"context", "node"},
		Annotations: readOnlyRemote,
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			var in nodeHostInput
			if err := action.Decode(raw, &in); err != nil {
				return action.Assessment{}, err
			}
			if _, err := p.Clusters.Context(in.Context); err != nil {
				return action.Assessment{}, err
			}
			return action.Assessment{Risk: action.Read, Targets: []action.Target{p.Clusters.Target(in.Context, "")},
				Preview: "SSH host of node " + in.Node + " in " + in.Context}, nil
		},
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in nodeHostInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			if in.Node == "" {
				return nil, fmt.Errorf("node is required")
			}
			r, err := p.resolve(scope{Context: in.Context})
			if err != nil {
				return nil, err
			}
			node, err := r.client.Typed.CoreV1().Nodes().Get(ctx, in.Node, metav1.GetOptions{})
			if err != nil {
				return nil, kube.Explain(err)
			}
			out := NodeHostOutput{Context: in.Context, Node: in.Node, Addresses: []string{}}
			var values []string
			for _, a := range node.Status.Addresses {
				out.Addresses = append(out.Addresses, string(a.Type)+"="+a.Address)
				values = append(values, a.Address)
			}
			m, err := p.nodeHosts(in.Context, node)
			if err != nil {
				return nil, err
			}
			out.Host, out.MatchedBy, out.Candidates = m.Host, m.MatchedBy, m.Candidates
			if out.Candidates == nil {
				out.Candidates = []HostCandidate{}
			}
			switch {
			case len(m.Candidates) == 0:
				out.Note = fmt.Sprintf("no saved SSH host has HostName %s or the tag k8s-node=%s/%s; add one to ~/.ssh/config and "+
					"import it (ssh_hosts_import), or ask the user how to reach the node", strings.Join(append(values, in.Node), ", "), in.Context, in.Node)
			case m.Host == "":
				out.Note = fmt.Sprintf("several saved SSH hosts match (%s); do not guess: ask the user which one is the node "+
					"and to tag it (cox ssh hosts tag <host> --set k8s-node=%s/%s)", candidateNames(m.Candidates), in.Context, in.Node)
			default:
				if h, err := p.Hosts.Get(m.Host); err == nil {
					out.HostNote, out.HostTags = h.Note, h.Tags
				}
			}
			return out, nil
		},
	}
}
