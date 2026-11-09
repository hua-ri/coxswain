package k8s

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/duration"
	"sigs.k8s.io/yaml"
)

// lastApplied holds the whole object as last applied, Secret values
// included.
const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"

// age is how long ago t was, kubectl style (5m, 3h, 2d).
func age(now time.Time, t time.Time) string {
	if t.IsZero() {
		return "<unknown>"
	}
	return duration.ShortHumanDuration(now.Sub(t))
}

// summary renders objects of one kind as a compact table.
type summary struct {
	columns []string
	row     func(u *unstructured.Unstructured, now time.Time) []string
}

var summaries = map[string]summary{
	"Pod":         {[]string{"name", "ready", "status", "restarts", "age", "node"}, podRow},
	"Deployment":  {[]string{"name", "ready", "up-to-date", "available", "age"}, deploymentRow},
	"StatefulSet": {[]string{"name", "ready", "age"}, statefulSetRow},
	"ReplicaSet":  {[]string{"name", "desired", "current", "ready", "age"}, replicaSetRow},
	"DaemonSet":   {[]string{"name", "desired", "current", "ready", "up-to-date", "available", "age"}, daemonSetRow},
	"Job":         {[]string{"name", "status", "completions", "age"}, jobRow},
	"Node":        {[]string{"name", "status", "roles", "age", "version", "internal-ip"}, nodeRow},
	"Service":     {[]string{"name", "type", "cluster-ip", "ports", "age"}, serviceRow},
	"Secret":      {[]string{"name", "type", "keys", "age"}, secretRow},
}

var genericSummary = summary{[]string{"name", "status", "age"}, genericRow}

func summaryFor(kind string) summary {
	if s, ok := summaries[kind]; ok {
		return s
	}
	return genericSummary
}

// table summarizes items (all of one kind); with namespaces, the first
// column is the namespace.
func table(kind string, items []unstructured.Unstructured, namespaces bool, now time.Time) ([]string, [][]string) {
	s := summaryFor(kind)
	cols := s.columns
	if namespaces {
		cols = append([]string{"namespace"}, cols...)
	}
	rows := make([][]string, 0, len(items))
	for i := range items {
		row := s.row(&items[i], now)
		if namespaces {
			row = append([]string{items[i].GetNamespace()}, row...)
		}
		rows = append(rows, row)
	}
	return cols, rows
}

// summaryMap is one object's summary as column → value.
func summaryMap(kind string, u *unstructured.Unstructured, now time.Time) map[string]string {
	s := summaryFor(kind)
	row := s.row(u, now)
	m := make(map[string]string, len(row))
	for i, c := range s.columns {
		m[c] = row[i]
	}
	return m
}

func convert[T any](u *unstructured.Unstructured) *T {
	var v T
	runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &v)
	return &v
}

func created(u *unstructured.Unstructured) time.Time { return u.GetCreationTimestamp().Time }

func podRow(u *unstructured.Unstructured, now time.Time) []string {
	p := convert[corev1.Pod](u)
	ready, restarts := 0, int32(0)
	for _, c := range p.Status.ContainerStatuses {
		if c.Ready {
			ready++
		}
		restarts += c.RestartCount
	}
	return []string{p.Name, fmt.Sprintf("%d/%d", ready, len(p.Spec.Containers)), podStatus(p),
		strconv.Itoa(int(restarts)), age(now, p.CreationTimestamp.Time), dash(p.Spec.NodeName)}
}

// podStatus is the pod's status as kubectl shows it (Running,
// CrashLoopBackOff, Init:1/2, Terminating, …).
func podStatus(p *corev1.Pod) string {
	reason := string(p.Status.Phase)
	if p.Status.Reason != "" {
		reason = p.Status.Reason
	}
	initializing := false
	for i, c := range p.Status.InitContainerStatuses {
		switch {
		case c.State.Terminated != nil && c.State.Terminated.ExitCode == 0:
			continue
		case c.State.Terminated != nil:
			reason = "Init:" + terminatedReason(c.State.Terminated)
		case c.State.Waiting != nil && c.State.Waiting.Reason != "" && c.State.Waiting.Reason != "PodInitializing":
			reason = "Init:" + c.State.Waiting.Reason
		default:
			reason = fmt.Sprintf("Init:%d/%d", i, len(p.Spec.InitContainers))
		}
		initializing = true
		break
	}
	if !initializing {
		for i := len(p.Status.ContainerStatuses) - 1; i >= 0; i-- {
			c := p.Status.ContainerStatuses[i]
			switch {
			case c.State.Waiting != nil && c.State.Waiting.Reason != "":
				reason = c.State.Waiting.Reason
			case c.State.Terminated != nil:
				reason = terminatedReason(c.State.Terminated)
			}
		}
	}
	if p.DeletionTimestamp != nil {
		reason = "Terminating"
	}
	return dash(reason)
}

func terminatedReason(t *corev1.ContainerStateTerminated) string {
	switch {
	case t.Reason != "":
		return t.Reason
	case t.Signal != 0:
		return fmt.Sprintf("Signal:%d", t.Signal)
	}
	return fmt.Sprintf("ExitCode:%d", t.ExitCode)
}

func deploymentRow(u *unstructured.Unstructured, now time.Time) []string {
	d := convert[appsv1.Deployment](u)
	return []string{d.Name, fmt.Sprintf("%d/%d", d.Status.ReadyReplicas, replicas(d.Spec.Replicas)),
		itoa(d.Status.UpdatedReplicas), itoa(d.Status.AvailableReplicas), age(now, d.CreationTimestamp.Time)}
}

func statefulSetRow(u *unstructured.Unstructured, now time.Time) []string {
	s := convert[appsv1.StatefulSet](u)
	return []string{s.Name, fmt.Sprintf("%d/%d", s.Status.ReadyReplicas, replicas(s.Spec.Replicas)), age(now, s.CreationTimestamp.Time)}
}

func replicaSetRow(u *unstructured.Unstructured, now time.Time) []string {
	r := convert[appsv1.ReplicaSet](u)
	return []string{r.Name, itoa(replicas(r.Spec.Replicas)), itoa(r.Status.Replicas), itoa(r.Status.ReadyReplicas), age(now, r.CreationTimestamp.Time)}
}

func daemonSetRow(u *unstructured.Unstructured, now time.Time) []string {
	d := convert[appsv1.DaemonSet](u)
	s := d.Status
	return []string{d.Name, itoa(s.DesiredNumberScheduled), itoa(s.CurrentNumberScheduled), itoa(s.NumberReady),
		itoa(s.UpdatedNumberScheduled), itoa(s.NumberAvailable), age(now, d.CreationTimestamp.Time)}
}

func jobRow(u *unstructured.Unstructured, now time.Time) []string {
	succeeded, _, _ := unstructured.NestedInt64(u.Object, "status", "succeeded")
	completions, ok, _ := unstructured.NestedInt64(u.Object, "spec", "completions")
	if !ok {
		completions = 1
	}
	status := "Running"
	for _, c := range conditions(u) {
		if c.Status == "True" && (c.Type == "Complete" || c.Type == "Failed" || c.Type == "Suspended") {
			status = c.Type
		}
	}
	return []string{u.GetName(), status, fmt.Sprintf("%d/%d", succeeded, completions), age(now, created(u))}
}

func nodeRow(u *unstructured.Unstructured, now time.Time) []string {
	n := convert[corev1.Node](u)
	return []string{n.Name, nodeStatus(n), nodeRoles(n), age(now, n.CreationTimestamp.Time), dash(n.Status.NodeInfo.KubeletVersion),
		dash(nodeAddress(n, corev1.NodeInternalIP))}
}

func nodeStatus(n *corev1.Node) string {
	status := "Unknown"
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			switch c.Status {
			case corev1.ConditionTrue:
				status = "Ready"
			case corev1.ConditionFalse:
				status = "NotReady"
			}
		}
	}
	if n.Spec.Unschedulable {
		status += ",SchedulingDisabled"
	}
	return status
}

func nodeRoles(n *corev1.Node) string {
	var roles []string
	for k, v := range n.Labels {
		if r, ok := strings.CutPrefix(k, "node-role.kubernetes.io/"); ok && r != "" {
			roles = append(roles, r)
		} else if k == "kubernetes.io/role" && v != "" {
			roles = append(roles, v)
		}
	}
	if len(roles) == 0 {
		return "<none>"
	}
	sort.Strings(roles)
	return strings.Join(roles, ",")
}

func nodeAddress(n *corev1.Node, t corev1.NodeAddressType) string {
	for _, a := range n.Status.Addresses {
		if a.Type == t {
			return a.Address
		}
	}
	return ""
}

func serviceRow(u *unstructured.Unstructured, now time.Time) []string {
	s := convert[corev1.Service](u)
	var ports []string
	for _, p := range s.Spec.Ports {
		port := strconv.Itoa(int(p.Port))
		if p.NodePort != 0 {
			port += ":" + strconv.Itoa(int(p.NodePort))
		}
		ports = append(ports, port+"/"+string(p.Protocol))
	}
	return []string{s.Name, string(s.Spec.Type), dash(s.Spec.ClusterIP), dash(strings.Join(ports, " ")), age(now, s.CreationTimestamp.Time)}
}

func secretRow(u *unstructured.Unstructured, now time.Time) []string {
	t, _, _ := unstructured.NestedString(u.Object, "type")
	return []string{u.GetName(), dash(t), dash(strings.Join(secretKeys(u), " ")), age(now, created(u))}
}

// secretKeys lists a Secret's keys with their value lengths in bytes,
// e.g. "password(19)". Values never leave the server.
func secretKeys(u *unstructured.Unstructured) []string {
	var out []string
	for _, field := range []string{"data", "stringData"} {
		m, _, _ := unstructured.NestedMap(u.Object, field)
		for k, v := range m {
			out = append(out, fmt.Sprintf("%s(%d)", k, valueLen(field, v)))
		}
	}
	sort.Strings(out)
	return out
}

// valueLen is the decoded length of a Secret value (data is base64).
func valueLen(field string, v any) int {
	s, _ := v.(string)
	if field == "data" {
		return base64Len(s)
	}
	return len(s)
}

func base64Len(s string) int {
	n := len(s) / 4 * 3
	if len(s) >= 1 && s[len(s)-1] == '=' {
		n--
	}
	if len(s) >= 2 && s[len(s)-2] == '=' {
		n--
	}
	return max(n, 0)
}

func genericRow(u *unstructured.Unstructured, now time.Time) []string {
	status, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	if status == "" {
		var parts []string
		for _, c := range conditions(u) {
			parts = append(parts, c.Type+"="+c.Status)
		}
		status = strings.Join(parts, " ")
	}
	return []string{u.GetName(), dash(status), age(now, created(u))}
}

// Condition is one status condition, for people and agents.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

func conditions(u *unstructured.Unstructured) []Condition {
	list, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	var out []Condition
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		str := func(k string) string { s, _ := m[k].(string); return s }
		out = append(out, Condition{Type: str("type"), Status: str("status"), Reason: str("reason"), Message: str("message")})
	}
	return out
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func itoa(n int32) string { return strconv.Itoa(int(n)) }

func replicas(r *int32) int32 {
	if r == nil {
		return 1
	}
	return *r
}

// clean returns a copy of u as shown to people and agents: without
// managedFields, and for Secrets without values.
func clean(u *unstructured.Unstructured) *unstructured.Unstructured {
	c := u.DeepCopy()
	unstructured.RemoveNestedField(c.Object, "metadata", "managedFields")
	if isSecret(c) {
		redactSecret(c, nil)
	}
	return c
}

func isSecret(u *unstructured.Unstructured) bool {
	return u.GetKind() == "Secret" && (u.GetAPIVersion() == "v1" || u.GetAPIVersion() == "")
}

// redactSecret replaces a Secret's values with their lengths and drops
// the last-applied annotation (which holds them too). A value that
// differs from the one in prev (if given) is marked as changed, so a
// diff still shows it.
func redactSecret(u, prev *unstructured.Unstructured) {
	for _, field := range []string{"data", "stringData"} {
		m, ok, _ := unstructured.NestedMap(u.Object, field)
		if !ok {
			continue
		}
		var old map[string]any
		if prev != nil {
			old, _, _ = unstructured.NestedMap(prev.Object, field)
		}
		for k, v := range m {
			s := fmt.Sprintf("<redacted %d bytes>", valueLen(field, v))
			if prev != nil {
				if ov, ok := old[k]; !ok || ov != v {
					s = fmt.Sprintf("<redacted %d bytes, changed>", valueLen(field, v))
				}
			}
			m[k] = s
		}
		unstructured.SetNestedMap(u.Object, m, field)
	}
	if ann := u.GetAnnotations(); ann[lastApplied] != "" {
		delete(ann, lastApplied)
		if len(ann) == 0 {
			ann = nil
		}
		u.SetAnnotations(ann)
	}
}

// toYAML renders an object as YAML.
func toYAML(u *unstructured.Unstructured) (string, error) {
	b, err := yaml.Marshal(u.Object)
	return string(b), err
}

// eventTime is when an event last happened.
func eventTime(e *corev1.Event) time.Time {
	for _, t := range []metav1.Time{e.LastTimestamp, {Time: e.EventTime.Time}, e.FirstTimestamp, e.CreationTimestamp} {
		if !t.IsZero() {
			return t.Time
		}
	}
	return time.Time{}
}
