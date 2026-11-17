package argo

import (
	"context"
	"fmt"
	"strings"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/argo"
	"github.com/hua-ri/coxswain/internal/shaper"
)

// Finding is one thing a diagnosis found.
type Finding struct {
	Severity string `json:"severity"` // error, warning, info
	Title    string `json:"title"`
	Detail   string `json:"detail,omitempty"`
}

// Diagnosis is the result of argocd.diagnose and argowf.diagnose.
type Diagnosis struct {
	Target       string         `json:"target"`
	InstanceNote string         `json:"instance_note,omitempty"`
	Summary      string         `json:"summary"`
	Findings     []Finding      `json:"findings"`
	Evidence     map[string]any `json:"evidence,omitempty"`
	NextSteps    []string       `json:"next_steps,omitempty"`
}

func (d *Diagnosis) add(sev, title, detail string) {
	d.Findings = append(d.Findings, Finding{Severity: sev, Title: title, Detail: detail})
}

// podLogLimit keeps evidence logs short.
var podLogLimit = shaper.Limits{MaxBytes: 3 * 1024, MaxLines: 50}

func (p *Provider) cdDiagnose() *action.Action {
	return p.cdAction(&action.Action{
		ID:      "argocd.diagnose",
		Summary: "Find out why an Argo CD application is unhealthy or out of sync",
		Description: "Checks the app's conditions (e.g. ComparisonError), its last sync's failures, out-of-sync resources, " +
			"and unhealthy resources; for unhealthy Pods it adds their events and the tail of their (previous) logs. " +
			"Returns findings, evidence and next steps. Run it before syncing or rolling back.",
		Input:       cdSchema(appProps(nil), "app"),
		Positional:  []string{"app"},
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true, OpenWorld: true},
	}, read("diagnose"), func(ctx context.Context, i argo.Instance, in cdInput) (any, error) {
		a, err := p.getApp(ctx, i, in, "")
		if err != nil {
			return nil, err
		}
		return p.diagnoseApp(ctx, i, in, a), nil
	})
}

func (p *Provider) diagnoseApp(ctx context.Context, i argo.Instance, in cdInput, a argo.Application) Diagnosis {
	det := detail(i, a)
	d := Diagnosis{Target: i.Name + "/" + in.App, InstanceNote: i.Note, Evidence: map[string]any{},
		Summary: fmt.Sprintf("sync %s, health %s", det.Sync, det.Health)}
	for _, c := range a.Status.Conditions {
		sev := "warning"
		if strings.HasSuffix(c.Type, "Error") {
			sev = "error"
		}
		d.add(sev, c.Type, firstLine(c.Message))
		if c.Type == "ComparisonError" || c.Type == "InvalidSpecError" {
			d.NextSteps = append(d.NextSteps, "the manifests cannot be generated or compared: fix the source in Git "+
				"(the message above names the file or chart), then argocd_refresh with hard=true")
		}
	}
	if op := det.Operation; op != nil && (op.Phase == "Failed" || op.Phase == "Error") {
		d.add("error", "Last sync "+strings.ToLower(op.Phase), op.Message)
		for _, f := range op.Failed {
			d.add("error", "Sync failed for a resource", f)
		}
		d.NextSteps = append(d.NextSteps, "fix what the failed resources report, then argocd_sync (dry_run first)")
	}
	if det.Running {
		d.add("info", "An operation is running", "a sync is in progress; argocd_app shows it")
	}
	var outOfSync, prune []string
	for _, r := range det.Unhealthy {
		obj := r.Kind + "/" + r.Name
		if r.Prune {
			prune = append(prune, obj)
		} else if r.Sync == "OutOfSync" {
			outOfSync = append(outOfSync, obj)
		}
	}
	if len(outOfSync) > 0 {
		d.add("warning", fmt.Sprintf("%d resources differ from Git", len(outOfSync)), strings.Join(limit(outOfSync, 10), ", "))
		why := "argocd_diff shows the differences: a manual change in the cluster, or a new commit not yet synced"
		if det.AutoSync && det.SelfHeal {
			why = "auto-sync with self-heal is on, yet resources differ: Argo CD may fail to apply them (see the last sync) or ignore them; argocd_diff shows the differences"
		}
		d.NextSteps = append(d.NextSteps, why)
	}
	if len(prune) > 0 {
		d.add("warning", fmt.Sprintf("%d resources are no longer in Git", len(prune)),
			strings.Join(limit(prune, 10), ", ")+" — a sync with prune=true would delete them")
	}

	if det.Health != "Healthy" && det.Health != "" {
		nodes, err := p.tree(ctx, i, in)
		if err != nil {
			d.add("info", "Could not read the resource tree", err.Error())
		}
		var pods []TreeNode
		for _, n := range nodes {
			if n.Health == "" || n.Health == "Healthy" {
				continue
			}
			if n.Kind == "Pod" {
				pods = append(pods, n)
				continue
			}
			if n.Kind != "ReplicaSet" {
				d.add(sevOf(n.Health), fmt.Sprintf("%s/%s is %s", n.Kind, n.Name, n.Health), n.Message)
			}
		}
		kctx, why := p.appCluster(i, a)
		for k, pod := range pods {
			if k == 3 {
				d.add("info", fmt.Sprintf("%d more unhealthy Pods", len(pods)-3), "")
				break
			}
			if kctx != "" && p.followPod(ctx, &d, kctx, pod.Namespace, pod.Name) {
				continue
			}
			d.add(sevOf(pod.Health), fmt.Sprintf("Pod %s is %s", pod.Name, pod.Health), strings.TrimSpace(pod.Message+" "+pod.Info))
			d.Evidence["pod/"+pod.Name] = p.podEvidence(ctx, i, in, pod)
		}
		if len(pods) > 0 && kctx == "" && why != "" {
			d.NextSteps = append(d.NextSteps, why)
		}
		if len(pods) > 0 {
			d.NextSteps = append(d.NextSteps, "argocd_logs (with previous=true for a crashed container) and argocd_events for more")
		}
	}
	if len(d.Findings) == 0 {
		d.add("info", "Nothing wrong found", "the app is "+det.Sync+" and "+det.Health)
	}
	if det.LastSync != nil {
		d.Evidence["last_sync"] = map[string]any{"at": det.LastSync, "result": det.LastResult, "revision": det.Revision}
	}
	if len(det.History) > 0 {
		d.Evidence["last_deployment"] = det.History[0]
	}
	return d
}

// podEvidence is what Argo CD itself tells about a pod: its warning events
// and the tail of its (previous, after a crash) logs.
func (p *Provider) podEvidence(ctx context.Context, i argo.Instance, in cdInput, pod TreeNode) map[string]any {
	ev, _ := p.events(ctx, i, cdInput{App: in.App, AppNS: in.AppNS, Name: pod.Name, Namespace: pod.Namespace})
	var warn []string
	for _, e := range ev {
		if e.Type == "Warning" && len(warn) < 5 {
			warn = append(warn, e.Reason+": "+e.Message)
		}
	}
	out := map[string]any{"events": warn}
	podIn := cdInput{App: in.App, AppNS: in.AppNS, Pod: pod.Name, Namespace: pod.Namespace}
	logs, err := p.podLogs(ctx, i, podIn, 50)
	if strings.Contains(pod.Info, "Restart") || strings.Contains(pod.Message, "CrashLoop") || strings.Contains(pod.Info, "CrashLoop") {
		podIn.Previous = true
		if prev, perr := p.podLogs(ctx, i, podIn, 50); perr == nil && strings.TrimSpace(prev) != "" {
			logs, err = prev, nil
			out["logs_of"] = "previous container"
		}
	}
	if err == nil && strings.TrimSpace(logs) != "" {
		clipped, _ := shaper.Clip(logs, podLogLimit)
		out["logs_tail"] = clipped
	}
	return out
}

// inCluster is Argo CD's destination for the cluster it runs in.
const inCluster = "https://kubernetes.default.svc"

// appCluster is the Coxswain context of an app's destination, or "" and
// what the user can do about it.
func (p *Provider) appCluster(i argo.Instance, a argo.Application) (string, string) {
	if p.K8s == nil {
		return "", ""
	}
	dest := a.Spec.Destination
	if dest.Server == "" || dest.Server == inCluster || dest.Name == "in-cluster" {
		if i.K8sContext != "" {
			return i.K8sContext, ""
		}
		return "", fmt.Sprintf("the app runs in the cluster of Argo CD itself; if that cluster is in Coxswain, have the user set it "+
			"(argo_update kind=argocd name=%s k8s_context=<context>) and diagnose again for node and SSH host details", i.Name)
	}
	list := p.K8s.ContextsForServer(dest.Server)
	switch len(list) {
	case 1:
		return list[0], ""
	case 0:
		return "", "the destination cluster " + dest.Server + " is not in Coxswain; adding it (Clusters → +) lets diagnoses follow pods to their nodes and SSH hosts"
	}
	return "", fmt.Sprintf("several Coxswain contexts reach %s (%s); diagnose a pod with k8s_diagnose on the right one", dest.Server, strings.Join(list, ", "))
}

// followPod adds k8s.diagnose's findings for a pod (with its node and that
// node's SSH host). It reports false if the pod could not be diagnosed there.
func (p *Provider) followPod(ctx context.Context, d *Diagnosis, kctx, ns, pod string) bool {
	k, err := p.K8s.Diagnose(ctx, kctx, ns, "pod", pod)
	if err != nil {
		d.add("info", "Could not diagnose pod "+pod+" in "+kctx, err.Error())
		return false
	}
	for _, f := range k.Findings {
		d.add(f.Severity, "Pod "+pod+": "+f.Title, f.Detail)
	}
	d.Evidence["k8s/"+kctx+"/"+ns+"/"+pod] = k.Evidence
	for _, n := range k.Evidence.Nodes {
		if n.SSHHost != "" {
			d.add("info", fmt.Sprintf("Node %s is SSH host %s", n.Name, n.SSHHost), "inspect it with ssh_exec (dmesg, df -h, journalctl -u kubelet) if the node is suspect")
		}
	}
	d.NextSteps = append(d.NextSteps, k.NextSteps...)
	return true
}

func sevOf(health string) string {
	if health == "Degraded" || health == "Missing" {
		return "error"
	}
	return "warning"
}

func limit(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(s[:n:n], fmt.Sprintf("… %d more", len(s)-n))
}

func (p *Provider) wfDiagnose() *action.Action {
	return p.wfAction(&action.Action{
		ID:      "argowf.diagnose",
		Summary: "Find out why an Argo workflow failed, and whether to retry",
		Description: "Finds the failed steps, classifies each (OOM, exit code, image pull, deadline, pod deleted…), adds the tail " +
			"of the first failed step's logs, and says whether a retry is likely to help. Run it before argowf_retry.",
		Input:       wfSchema(map[string]any{"name": wfNameProp}, "name"),
		Positional:  []string{"name"},
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true, OpenWorld: true},
	}, wfRead("diagnose workflow"), func(ctx context.Context, i argo.Instance, in wfInput) (any, error) {
		w, err := p.getWF(ctx, i, in)
		if err != nil {
			return nil, err
		}
		return p.diagnoseWF(ctx, i, w), nil
	})
}

// failureClass says what kind of failure a node's message and exit code
// show, and whether retrying it as it is may help.
func failureClass(n NodeView) (class string, transient bool) {
	m := strings.ToLower(n.Message)
	switch {
	case strings.Contains(m, "oomkilled") || n.ExitCode == "137" && strings.Contains(m, "oom"):
		return "out of memory (OOMKilled): raise the step's memory limit; a plain retry will likely fail again", false
	case strings.Contains(m, "imagepullbackoff") || strings.Contains(m, "errimagepull") || strings.Contains(m, "pull access denied"):
		return "image cannot be pulled: check the image name/tag and registry credentials", false
	case strings.Contains(m, "deadline") || strings.Contains(m, "activedeadlineseconds") || strings.Contains(m, "timeout"):
		return "deadline exceeded: the step or workflow ran past its time limit", true
	case strings.Contains(m, "pod deleted") || strings.Contains(m, "evicted") || strings.Contains(m, "node") && strings.Contains(m, "shutdown") || strings.Contains(m, "preempt"):
		return "the pod was removed (deleted, evicted or preempted): infrastructure, not the step itself", true
	case strings.Contains(m, "createcontainerconfigerror") || strings.Contains(m, "secret") && strings.Contains(m, "not found") || strings.Contains(m, "configmap") && strings.Contains(m, "not found"):
		return "container cannot be configured (missing Secret/ConfigMap?)", false
	case n.ExitCode == "137" || n.ExitCode == "143":
		return "killed by a signal (exit " + n.ExitCode + "): out of memory, a deadline, or the node", true
	case n.ExitCode != "" && n.ExitCode != "0":
		return "the step's command failed with exit code " + n.ExitCode + ": read its logs", false
	}
	return "", false
}

func (p *Provider) diagnoseWF(ctx context.Context, i argo.Instance, w argo.Workflow) Diagnosis {
	s := p.wfSummary(w)
	d := Diagnosis{Target: i.Name + "/" + w.Metadata.Namespace + "/" + w.Metadata.Name, InstanceNote: i.Note, Evidence: map[string]any{},
		Summary: fmt.Sprintf("%s, %s, ran %s", s.Phase, s.Progress, s.Duration)}
	if s.Message != "" {
		d.add(map[bool]string{true: "error", false: "info"}[failed(s.Phase)], "Workflow message", s.Message)
	}
	nodes := p.nodes(w)
	var bad []NodeView
	for _, n := range nodes {
		if failed(n.Phase) && n.Type == "Pod" {
			bad = append(bad, n)
		}
	}
	if len(bad) == 0 {
		for _, n := range nodes {
			if failed(n.Phase) && n.Type != "Pod" && n.Message != "" {
				d.add("error", fmt.Sprintf("%s %s %s", n.Type, n.Name, n.Phase), n.Message)
			}
		}
		if s.Phase == "Running" || s.Phase == "Pending" {
			var running []string
			for _, n := range nodes {
				if n.Type == "Pod" && (n.Phase == "Running" || n.Phase == "Pending") {
					running = append(running, n.Name+" ("+n.Phase+")")
				}
			}
			d.add("info", "Still running", strings.Join(limit(running, 10), ", "))
		}
		if len(d.Findings) == 0 {
			d.add("info", "No failed steps", "the workflow is "+s.Phase)
		}
		return d
	}
	allTransient := true
	for k, n := range bad {
		if k == 5 {
			d.add("info", fmt.Sprintf("%d more failed steps", len(bad)-5), "see argowf_get")
			break
		}
		class, transient := failureClass(n)
		allTransient = allTransient && transient && class != ""
		detail := n.Message
		if n.ExitCode != "" {
			detail = strings.TrimSpace(fmt.Sprintf("exit code %s; %s", n.ExitCode, n.Message))
		}
		if class != "" {
			detail += " — " + class
		}
		d.add("error", fmt.Sprintf("Step %s (%s) %s", n.Name, n.Template, strings.ToLower(n.Phase)), detail)
	}
	first := bad[0]
	ev := map[string]any{"node": first.ID, "pod": first.Pod}
	if wn, err := p.node(w, first.ID); err == nil {
		logs, err := p.nodeLogs(ctx, i, w, wn, "main", 50, "")
		switch {
		case err != nil:
			ev["logs_error"] = err.Error()
		case strings.TrimSpace(logs) == "":
			ev["logs"] = "(empty: the pod may be gone; logs then live only in the artifact repository, if archived)"
		default:
			clipped, _ := shaper.Clip(logs, podLogLimit)
			ev["logs_tail"] = clipped
		}
	}
	d.Evidence["first_failed_step"] = ev
	if p.K8s != nil && i.K8sContext != "" {
		followed := first.Pod != "" && p.followPod(ctx, &d, i.K8sContext, w.Metadata.Namespace, first.Pod)
		// The pod is often gone already; the node it ran on is in the workflow.
		if wn, err := p.node(w, first.ID); !followed && err == nil && wn.HostNodeName != "" {
			if h, err := p.K8s.NodeHost(ctx, i.K8sContext, wn.HostNodeName); err == nil {
				ev["node"] = wn.HostNodeName
				if h.Host != "" {
					ev["ssh_host"] = h.Host
					d.add("info", fmt.Sprintf("The step ran on node %s, SSH host %s", wn.HostNodeName, h.Host),
						"inspect it with ssh_exec (dmesg, df -h, journalctl -u kubelet) if the node is suspect")
				} else if h.Note != "" {
					ev["ssh_host_note"] = h.Note
				}
			}
		}
	} else if p.K8s != nil {
		d.NextSteps = append(d.NextSteps, fmt.Sprintf("if the cluster this Argo Workflows runs in is in Coxswain, have the user set it "+
			"(argo_update kind=argowf name=%s k8s_context=<context>): diagnoses then cover the pods, nodes and their SSH hosts", i.Name))
	}
	if allTransient {
		d.NextSteps = append(d.NextSteps, "the failures look transient: argowf_retry is likely to help")
	} else {
		d.NextSteps = append(d.NextSteps, "fix the cause first (see findings and logs); a plain argowf_retry would likely fail the same way",
			"after a fix in the template or image, argowf_resubmit or argowf_submit runs it fresh")
	}
	return d
}
