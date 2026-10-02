package platformstate

import "time"

// The Kubernetes objects platformstate reads, cut down to the fields it
// uses. The JSON names are the API's own (argoproj.io/v1alpha1
// Application, apps/v1 Deployment, v1 Pod, batch/v1 Job and CronJob,
// postgresql.cnpg.io/v1 Cluster, cert-manager.io/v1 Certificate,
// networking.k8s.io/v1 Ingress, v1 Node and events.k8s.io/v1 Event), so
// the same structs decode a list response from the API server or an
// unstructured object's content.

// ObjectMeta is the part of metadata used here.
type ObjectMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	// DeletionTimestamp is set once the object is being deleted.
	DeletionTimestamp *time.Time `json:"deletionTimestamp"`
	// Generation is bumped by every change to the object's spec.
	Generation int64 `json:"generation"`
}

// The values of a condition's status.
const (
	conditionTrue    = "True"
	conditionFalse   = "False"
	conditionUnknown = "Unknown"
)

// KubeCondition is one entry of a status.conditions list, in the shape
// Deployments, Pods, Jobs, CNPG Clusters and Certificates share.
type KubeCondition struct {
	Type               string     `json:"type"`
	Status             string     `json:"status"`
	Reason             string     `json:"reason"`
	Message            string     `json:"message"`
	LastTransitionTime *time.Time `json:"lastTransitionTime"`
}

// ArgoCDApplication is an ArgoCD Application.
type ArgoCDApplication struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		Destination struct {
			Namespace string `json:"namespace"`
		} `json:"destination"`
		// Sources are a multi-source Application's sources. Only the
		// image tag a Preview Environment's ApplicationSet sets in
		// helm.valuesObject is read.
		Sources []struct {
			Helm *struct {
				ValuesObject *struct {
					Image struct {
						Tag string `json:"tag"`
					} `json:"image"`
				} `json:"valuesObject"`
			} `json:"helm"`
		} `json:"sources"`
	} `json:"spec"`
	Status struct {
		Sync struct {
			Status string `json:"status"`
			// Revision and Revisions are the commits ArgoCD last compared
			// the cluster against: Revision for a single-source
			// Application, Revisions (one per source) for a multi-source
			// one, as every Environment is.
			Revision  string   `json:"revision"`
			Revisions []string `json:"revisions"`
		} `json:"sync"`
		Health struct {
			Status string `json:"status"`
		} `json:"health"`
		// ReconciledAt is when ArgoCD last compared the cluster with the
		// newest commit of its sources.
		ReconciledAt   *time.Time `json:"reconciledAt"`
		OperationState *struct {
			Phase      string     `json:"phase"`
			Message    string     `json:"message"`
			StartedAt  *time.Time `json:"startedAt"`
			FinishedAt *time.Time `json:"finishedAt"`
			Operation  struct {
				Sync *struct {
					Revision  string   `json:"revision"`
					Revisions []string `json:"revisions"`
				} `json:"sync"`
			} `json:"operation"`
			SyncResult *struct {
				Revision  string   `json:"revision"`
				Revisions []string `json:"revisions"`
			} `json:"syncResult"`
		} `json:"operationState"`
		// History is ArgoCD's record of its last syncs, oldest first.
		History []struct {
			ID         int64      `json:"id"`
			Revision   string     `json:"revision"`
			Revisions  []string   `json:"revisions"`
			DeployedAt *time.Time `json:"deployedAt"`
		} `json:"history"`
		// Conditions are ArgoCD's own: ComparisonError, DeletionError,
		// SyncError and the like.
		Conditions []struct {
			Type               string     `json:"type"`
			Message            string     `json:"message"`
			LastTransitionTime *time.Time `json:"lastTransitionTime"`
		} `json:"conditions"`
		Summary struct {
			ExternalURLs []string `json:"externalURLs"`
			Images       []string `json:"images"`
		} `json:"summary"`
	} `json:"status"`
}

// Container is the part of a container spec used here.
type Container struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

// Deployment is an apps/v1 Deployment.
type Deployment struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		// Replicas is nil when the manifest leaves it to the default, 1.
		Replicas *int `json:"replicas"`
		Selector struct {
			MatchLabels map[string]string `json:"matchLabels"`
		} `json:"selector"`
		Template struct {
			Spec struct {
				Containers []Container `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration int64           `json:"observedGeneration"`
		Replicas           int             `json:"replicas"`
		UpdatedReplicas    int             `json:"updatedReplicas"`
		ReadyReplicas      int             `json:"readyReplicas"`
		AvailableReplicas  int             `json:"availableReplicas"`
		Conditions         []KubeCondition `json:"conditions"`
	} `json:"status"`
}

// ContainerState is a container's state or last state: at most one of
// its fields is set.
type ContainerState struct {
	Waiting *struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"waiting"`
	Running *struct {
		StartedAt *time.Time `json:"startedAt"`
	} `json:"running"`
	Terminated *struct {
		Reason   string `json:"reason"`
		ExitCode int    `json:"exitCode"`
	} `json:"terminated"`
}

// Pod is a v1 Pod.
type Pod struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		Containers []Container `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase             string          `json:"phase"`
		Conditions        []KubeCondition `json:"conditions"`
		ContainerStatuses []struct {
			Name         string         `json:"name"`
			Image        string         `json:"image"`
			Ready        bool           `json:"ready"`
			RestartCount int            `json:"restartCount"`
			State        ContainerState `json:"state"`
			LastState    ContainerState `json:"lastState"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

// Job is a batch/v1 Job.
type Job struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		Template struct {
			Spec struct {
				Containers []Container `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		StartTime      *time.Time      `json:"startTime"`
		CompletionTime *time.Time      `json:"completionTime"`
		Conditions     []KubeCondition `json:"conditions"`
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

// PostgresCluster is a CloudNativePG postgresql.cnpg.io/v1 Cluster: an
// Environment's Postgres.
type PostgresCluster struct {
	Metadata ObjectMeta `json:"metadata"`
	Status   struct {
		// Phase is CNPG's summary, such as "Cluster in healthy state" or
		// "Setting up primary".
		Phase string `json:"phase"`
		// Conditions include Ready, LastBackupSucceeded and
		// ContinuousArchiving.
		Conditions []KubeCondition `json:"conditions"`
	} `json:"status"`
}

// Certificate is a cert-manager.io/v1 Certificate: the certificate of a
// custom domain.
type Certificate struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		DNSNames []string `json:"dnsNames"`
	} `json:"spec"`
	Status struct {
		Conditions []KubeCondition `json:"conditions"`
		// NotAfter is when the certificate in use expires; nil before
		// the first one is issued.
		NotAfter *time.Time `json:"notAfter"`
		// LastFailureTime is set while the latest issuance has failed.
		LastFailureTime *time.Time `json:"lastFailureTime"`
	} `json:"status"`
}

// Ingress is a networking.k8s.io/v1 Ingress. Only its metadata is read:
// the annotation that puts it behind Itema login.
type Ingress struct {
	Metadata ObjectMeta `json:"metadata"`
}

// Node is a v1 Node, which k3s runs everything on.
type Node struct {
	Metadata ObjectMeta `json:"metadata"`
	Status   struct {
		// Conditions include Ready, which the kubelet keeps; Unknown when
		// it has stopped reporting.
		Conditions []KubeCondition `json:"conditions"`
		NodeInfo   struct {
			// KubeletVersion is the node's Kubernetes, such as
			// v1.36.4+k3s1: k3s's version.
			KubeletVersion string `json:"kubeletVersion"`
		} `json:"nodeInfo"`
	} `json:"status"`
}

// Event is an events.k8s.io/v1 Event, such as the Deploy gate's
// DeployAccepted and DeployRefused.
type Event struct {
	Metadata ObjectMeta `json:"metadata"`
	// Reason is the machine-readable reason, such as DeployAccepted.
	Reason string `json:"reason"`
	// Note is the human-readable description.
	Note string `json:"note"`
	// Type is Normal or Warning.
	Type string `json:"type"`
	// EventTime is when it happened; it may be null on Events converted
	// from core/v1, which have deprecatedLastTimestamp instead.
	EventTime               *time.Time `json:"eventTime"`
	DeprecatedLastTimestamp *time.Time `json:"deprecatedLastTimestamp"`
	// Regarding is the object it is about.
	Regarding struct {
		Kind      string `json:"kind"`
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"regarding"`
}

// Time is when the Event happened: its eventTime, else its
// deprecatedLastTimestamp, else its creation.
func (e Event) Time() time.Time {
	switch {
	case e.EventTime != nil && !e.EventTime.IsZero():
		return *e.EventTime
	case e.DeprecatedLastTimestamp != nil && !e.DeprecatedLastTimestamp.IsZero():
		return *e.DeprecatedLastTimestamp
	}
	return e.Metadata.CreationTimestamp
}

// conditionOf is the condition of type t in conditions, if there is one.
func conditionOf(conditions []KubeCondition, t string) (KubeCondition, bool) {
	for _, c := range conditions {
		if c.Type == t {
			return c, true
		}
	}
	return KubeCondition{}, false
}
