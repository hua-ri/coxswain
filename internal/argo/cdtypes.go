package argo

import "time"

// The parts of Argo CD's API objects Coxswain reads (v1alpha1).

type Application struct {
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels,omitempty"`
	} `json:"metadata"`
	Spec struct {
		Project     string         `json:"project"`
		Source      *AppSource     `json:"source,omitempty"`
		Sources     []AppSource    `json:"sources,omitempty"`
		Destination AppDest        `json:"destination"`
		SyncPolicy  *AppSyncPolicy `json:"syncPolicy,omitempty"`
	} `json:"spec"`
	Status struct {
		Sync struct {
			Status    string   `json:"status"`
			Revision  string   `json:"revision,omitempty"`
			Revisions []string `json:"revisions,omitempty"`
		} `json:"sync"`
		Health struct {
			Status  string `json:"status"`
			Message string `json:"message,omitempty"`
		} `json:"health"`
		Conditions     []AppCondition   `json:"conditions,omitempty"`
		OperationState *OperationState  `json:"operationState,omitempty"`
		History        []HistoryEntry   `json:"history,omitempty"`
		Resources      []ResourceStatus `json:"resources,omitempty"`
		ReconciledAt   *time.Time       `json:"reconciledAt,omitempty"`
	} `json:"status"`
	Operation *struct{} `json:"operation,omitempty"`
}

type AppSource struct {
	RepoURL        string `json:"repoURL"`
	Path           string `json:"path,omitempty"`
	TargetRevision string `json:"targetRevision,omitempty"`
	Chart          string `json:"chart,omitempty"`
}

type AppDest struct {
	Server    string `json:"server,omitempty"`
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

type AppSyncPolicy struct {
	Automated *struct {
		Prune    bool `json:"prune,omitempty"`
		SelfHeal bool `json:"selfHeal,omitempty"`
	} `json:"automated,omitempty"`
}

type AppCondition struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type OperationState struct {
	Phase      string     `json:"phase"`
	Message    string     `json:"message,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	SyncResult *struct {
		Revision  string `json:"revision,omitempty"`
		Resources []struct {
			Group     string `json:"group,omitempty"`
			Kind      string `json:"kind"`
			Namespace string `json:"namespace,omitempty"`
			Name      string `json:"name"`
			Status    string `json:"status,omitempty"`
			Message   string `json:"message,omitempty"`
			HookPhase string `json:"hookPhase,omitempty"`
			HookType  string `json:"hookType,omitempty"`
			SyncPhase string `json:"syncPhase,omitempty"`
		} `json:"resources,omitempty"`
	} `json:"syncResult,omitempty"`
}

type HistoryEntry struct {
	ID          int64      `json:"id"`
	Revision    string     `json:"revision,omitempty"`
	Revisions   []string   `json:"revisions,omitempty"`
	DeployedAt  *time.Time `json:"deployedAt,omitempty"`
	InitiatedBy *struct {
		Username  string `json:"username,omitempty"`
		Automated bool   `json:"automated,omitempty"`
	} `json:"initiatedBy,omitempty"`
}

type ResourceStatus struct {
	Group     string `json:"group,omitempty"`
	Version   string `json:"version,omitempty"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Status    string `json:"status,omitempty"`
	Health    *struct {
		Status  string `json:"status,omitempty"`
		Message string `json:"message,omitempty"`
	} `json:"health,omitempty"`
	Hook            bool `json:"hook,omitempty"`
	RequiresPruning bool `json:"requiresPruning,omitempty"`
}

type ResourceTree struct {
	Nodes []struct {
		Group      string `json:"group,omitempty"`
		Version    string `json:"version,omitempty"`
		Kind       string `json:"kind"`
		Namespace  string `json:"namespace,omitempty"`
		Name       string `json:"name"`
		UID        string `json:"uid,omitempty"`
		ParentRefs []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
			UID  string `json:"uid,omitempty"`
		} `json:"parentRefs,omitempty"`
		Info []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"info,omitempty"`
		Health *struct {
			Status  string `json:"status,omitempty"`
			Message string `json:"message,omitempty"`
		} `json:"health,omitempty"`
		CreatedAt *time.Time `json:"createdAt,omitempty"`
	} `json:"nodes"`
}

type ResourceDiff struct {
	Group               string `json:"group,omitempty"`
	Kind                string `json:"kind"`
	Namespace           string `json:"namespace,omitempty"`
	Name                string `json:"name"`
	TargetState         string `json:"targetState,omitempty"`
	LiveState           string `json:"liveState,omitempty"`
	NormalizedLiveState string `json:"normalizedLiveState,omitempty"`
	PredictedLiveState  string `json:"predictedLiveState,omitempty"`
	Hook                bool   `json:"hook,omitempty"`
	Modified            bool   `json:"modified,omitempty"`
}

type Event struct {
	Type           string `json:"type"`
	Reason         string `json:"reason"`
	Message        string `json:"message"`
	Count          int    `json:"count,omitempty"`
	InvolvedObject struct {
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		Namespace string `json:"namespace,omitempty"`
	} `json:"involvedObject"`
	FirstTimestamp *time.Time `json:"firstTimestamp,omitempty"`
	LastTimestamp  *time.Time `json:"lastTimestamp,omitempty"`
	EventTime      *time.Time `json:"eventTime,omitempty"`
}

// When is the latest time the event happened.
func (e Event) When() time.Time {
	for _, t := range []*time.Time{e.LastTimestamp, e.EventTime, e.FirstTimestamp} {
		if t != nil && !t.IsZero() {
			return *t
		}
	}
	return time.Time{}
}
