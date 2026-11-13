package argo

import (
	"fmt"
	"hash/fnv"
	"time"
)

// The parts of Argo Workflows' API objects Coxswain reads (v1alpha1).

type Workflow struct {
	Metadata struct {
		Name              string            `json:"name"`
		Namespace         string            `json:"namespace"`
		Labels            map[string]string `json:"labels,omitempty"`
		Annotations       map[string]string `json:"annotations,omitempty"`
		CreationTimestamp *time.Time        `json:"creationTimestamp,omitempty"`
	} `json:"metadata"`
	Spec struct {
		Entrypoint          string `json:"entrypoint,omitempty"`
		Suspend             *bool  `json:"suspend,omitempty"`
		Shutdown            string `json:"shutdown,omitempty"`
		WorkflowTemplateRef *struct {
			Name         string `json:"name"`
			ClusterScope bool   `json:"clusterScope,omitempty"`
		} `json:"workflowTemplateRef,omitempty"`
		Arguments Arguments `json:"arguments,omitempty"`
	} `json:"spec"`
	Status struct {
		Phase      string            `json:"phase,omitempty"`
		Message    string            `json:"message,omitempty"`
		Progress   string            `json:"progress,omitempty"`
		StartedAt  *time.Time        `json:"startedAt,omitempty"`
		FinishedAt *time.Time        `json:"finishedAt,omitempty"`
		Nodes      map[string]WFNode `json:"nodes,omitempty"`
	} `json:"status"`
}

type Arguments struct {
	Parameters []Parameter `json:"parameters,omitempty"`
}

type Parameter struct {
	Name        string   `json:"name"`
	Value       *string  `json:"value,omitempty"`
	Default     *string  `json:"default,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Description string   `json:"description,omitempty"`
}

type WFNode struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	DisplayName  string `json:"displayName"`
	Type         string `json:"type"`
	TemplateName string `json:"templateName,omitempty"`
	TemplateRef  *struct {
		Template string `json:"template"`
	} `json:"templateRef,omitempty"`
	Phase        string     `json:"phase,omitempty"`
	Message      string     `json:"message,omitempty"`
	BoundaryID   string     `json:"boundaryID,omitempty"`
	Children     []string   `json:"children,omitempty"`
	StartedAt    *time.Time `json:"startedAt,omitempty"`
	FinishedAt   *time.Time `json:"finishedAt,omitempty"`
	HostNodeName string     `json:"hostNodeName,omitempty"`
	Outputs      *struct {
		ExitCode *string `json:"exitCode,omitempty"`
	} `json:"outputs,omitempty"`
}

// WorkflowTemplate is a WorkflowTemplate or ClusterWorkflowTemplate.
type WorkflowTemplate struct {
	Metadata struct {
		Name        string            `json:"name"`
		Namespace   string            `json:"namespace,omitempty"`
		Annotations map[string]string `json:"annotations,omitempty"`
	} `json:"metadata"`
	Spec struct {
		Entrypoint string    `json:"entrypoint,omitempty"`
		Arguments  Arguments `json:"arguments,omitempty"`
	} `json:"spec"`
}

type CronWorkflow struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		Schedule          string   `json:"schedule,omitempty"`
		Schedules         []string `json:"schedules,omitempty"`
		Timezone          string   `json:"timezone,omitempty"`
		Suspend           bool     `json:"suspend,omitempty"`
		ConcurrencyPolicy string   `json:"concurrencyPolicy,omitempty"`
	} `json:"spec"`
	Status struct {
		LastScheduledTime *time.Time `json:"lastScheduledTime,omitempty"`
		Active            []struct {
			Name string `json:"name"`
		} `json:"active,omitempty"`
		Failed    int `json:"failed,omitempty"`
		Succeeded int `json:"succeeded,omitempty"`
	} `json:"status"`
}

// PodName is the Pod a node ran in (argo-workflows workflow/util
// GeneratePodName), or "" for nodes without one.
func (w Workflow) PodName(n WFNode) string {
	if n.Type != "Pod" {
		return ""
	}
	if w.Metadata.Annotations["workflows.argoproj.io/pod-name-format"] == "v1" {
		return n.ID
	}
	if w.Metadata.Name == n.Name {
		return w.Metadata.Name
	}
	tmpl := n.TemplateName
	if tmpl == "" && n.TemplateRef != nil {
		tmpl = n.TemplateRef.Template
	}
	prefix := w.Metadata.Name
	if tmpl != "" {
		prefix += "-" + tmpl
	}
	if len(prefix) > 253-10-1 {
		prefix = prefix[:253-10-1]
	}
	h := fnv.New32a()
	h.Write([]byte(n.Name))
	return fmt.Sprintf("%s-%d", prefix, h.Sum32())
}
