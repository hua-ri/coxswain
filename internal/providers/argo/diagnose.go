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
		for k, pod := range pods {
			if k == 3 {
				d.add("info", fmt.Sprintf("%d more unhealthy Pods", len(pods)-3), "")
				break
			}
			d.add(sevOf(pod.Health), fmt.Sprintf("Pod %s is %s", pod.Name, pod.Health), strings.TrimSpace(pod.Message+" "+pod.Info))
			ev, _ := p.events(ctx, i, cdInput{App: in.App, AppNS: in.AppNS, Name: pod.Name, Namespace: pod.Namespace})
			var warn []string
			for _, e := range ev {
				if e.Type == "Warning" && len(warn) < 5 {
					warn = append(warn, e.Reason+": "+e.Message)
				}
			}
			ev2 := map[string]any{"events": warn}
			podIn := cdInput{App: in.App, AppNS: in.AppNS, Pod: pod.Name, Namespace: pod.Namespace}
			logs, err := p.podLogs(ctx, i, podIn, 50)
			if strings.Contains(pod.Info, "Restart") || strings.Contains(pod.Message, "CrashLoop") || strings.Contains(pod.Info, "CrashLoop") {
				podIn.Previous = true
				if prev, perr := p.podLogs(ctx, i, podIn, 50); perr == nil && strings.TrimSpace(prev) != "" {
					logs, err = prev, nil
					ev2["logs_of"] = "previous container"
				}
			}
			if err == nil && strings.TrimSpace(logs) != "" {
				clipped, _ := shaper.Clip(logs, podLogLimit)
				ev2["logs_tail"] = clipped
			}
			d.Evidence["pod/"+pod.Name] = ev2
		}
		if len(pods) > 0 {
			d.NextSteps = append(d.NextSteps, "argocd_logs (with previous=true for a crashed container) and argocd_events for more; "+
				"if the cluster is in Coxswain too, k8s_diagnose on the Pod")
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
