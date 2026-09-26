package platformstate

import "time"

// The Kubernetes objects platformstate reads, cut down to the fields it
// uses. The JSON names are the API's own (argoproj.io/v1alpha1
// Application, apps/v1 Deployment, v1 Pod, batch/v1 Job and CronJob), so
// the same structs decode a list response from the API server or an
// unstructured object's content.

// ObjectMeta is the part of metadata used here.
type ObjectMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	Labels            map[string]string `json:"labels"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
}

// ArgoCDApplication is an ArgoCD Application.
type ArgoCDApplication struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		Destination struct {
			Namespace string `json:"namespace"`
		} `json:"destination"`
	} `json:"spec"`
	Status struct {
		Sync struct {
			Status string `json:"status"`
		} `json:"sync"`
		Health struct {
			Status string `json:"status"`
		} `json:"health"`
		OperationState *struct {
			Phase      string     `json:"phase"`
			Message    string     `json:"message"`
			StartedAt  *time.Time `json:"startedAt"`
			FinishedAt *time.Time `json:"finishedAt"`
		} `json:"operationState"`
		Summary struct {
			ExternalURLs []string `json:"externalURLs"`
			Images       []string `json:"images"`
		} `json:"summary"`
	} `json:"status"`
}

// Deployment is an apps/v1 Deployment.
type Deployment struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		Selector struct {
			MatchLabels map[string]string `json:"matchLabels"`
		} `json:"selector"`
		Template struct {
			Spec struct {
				Containers []struct {
					Name  string `json:"name"`
					Image string `json:"image"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

// Pod is a v1 Pod.
type Pod struct {
	Metadata ObjectMeta `json:"metadata"`
	Status   struct {
		Phase      string `json:"phase"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
		ContainerStatuses []struct {
			Name         string `json:"name"`
			Ready        bool   `json:"ready"`
			RestartCount int    `json:"restartCount"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

// Job is a batch/v1 Job.
type Job struct {
	Metadata ObjectMeta `json:"metadata"`
	Status   struct {
		StartTime      *time.Time `json:"startTime"`
		CompletionTime *time.Time `json:"completionTime"`
		Conditions     []struct {
			Type               string     `json:"type"`
			Status             string     `json:"status"`
			LastTransitionTime *time.Time `json:"lastTransitionTime"`
		} `json:"conditions"`
	} `json:"status"`
}

// CronJob is a batch/v1 CronJob.
type CronJob struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		Schedule string `json:"schedule"`
	} `json:"spec"`
	Status struct {
		LastScheduleTime   *time.Time `json:"lastScheduleTime"`
		LastSuccessfulTime *time.Time `json:"lastSuccessfulTime"`
	} `json:"status"`
}
