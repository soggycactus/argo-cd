package metrics

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/labels"

	argoappv1 "github.com/argoproj/argo-cd/pkg/apis/application/v1alpha1"
	applister "github.com/argoproj/argo-cd/pkg/client/listers/application/v1alpha1"
	"github.com/argoproj/argo-cd/util/git"
	"github.com/argoproj/argo-cd/util/healthz"
)

type MetricsServer struct {
	*http.Server
	syncCounter             *prometheus.CounterVec
	kubectlExecCounter      *prometheus.CounterVec
	kubectlExecPendingGauge *prometheus.GaugeVec
	k8sRequestCounter       *prometheus.CounterVec
	clusterEventsCounter    *prometheus.CounterVec
	reconcileHistogram      *prometheus.HistogramVec
	registry                *prometheus.Registry
}

const (
	// MetricsPath is the endpoint to collect application metrics
	MetricsPath = "/metrics"
)

// Follow Prometheus naming practices
// https://prometheus.io/docs/practices/naming/
var (
	descAppDefaultLabels = []string{"namespace", "name", "project"}

	descAppInfo = prometheus.NewDesc(
		"argocd_app_info",
		"Information about application.",
		append(descAppDefaultLabels, "repo", "dest_server", "dest_namespace"),
		nil,
	)
	descAppCreated = prometheus.NewDesc(
		"argocd_app_created_time",
		"Creation time in unix timestamp for an application.",
		descAppDefaultLabels,
		nil,
	)
	descAppSyncStatusCode = prometheus.NewDesc(
		"argocd_app_sync_status",
		"The application current sync status.",
		append(descAppDefaultLabels, "sync_status"),
		nil,
	)
	descAppHealthStatus = prometheus.NewDesc(
		"argocd_app_health_status",
		"The application current health status.",
		append(descAppDefaultLabels, "health_status"),
		nil,
	)
)

// NewMetricsServer returns a new prometheus server which collects application metrics
func NewMetricsServer(addr string, appLister applister.ApplicationLister, healthCheck func() error) *MetricsServer {
	mux := http.NewServeMux()
	registry := NewAppRegistry(appLister)
	mux.Handle(MetricsPath, promhttp.HandlerFor(prometheus.Gatherers{
		// contains app controller specific metrics
		registry,
		// contains process, golang and controller workqueues metrics
		prometheus.DefaultGatherer,
	}, promhttp.HandlerOpts{}))
	healthz.ServeHealthCheck(mux, healthCheck)

	syncCounter := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "argocd_app_sync_total",
			Help: "Number of application syncs.",
		},
		append(descAppDefaultLabels, "phase"),
	)
	registry.MustRegister(syncCounter)

	k8sRequestCounter := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "argocd_app_k8s_request_total",
			Help: "Number of kubernetes requests executed during application reconciliation.",
		},
		append(descAppDefaultLabels, "server", "response_code", "verb", "resource_kind", "resource_namespace"),
	)
	registry.MustRegister(k8sRequestCounter)

	kubectlExecCounter := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "argocd_kubectl_exec_total",
		Help: "Number of kubectl executions",
	}, []string{"command"})
	registry.MustRegister(kubectlExecCounter)
	kubectlExecPendingGauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "argocd_kubectl_exec_pending",
		Help: "Number of pending kubectl executions",
	}, []string{"command"})
	registry.MustRegister(kubectlExecPendingGauge)

	reconcileHistogram := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "argocd_app_reconcile",
			Help: "Application reconciliation performance.",
			// Buckets chosen after observing a ~2100ms mean reconcile time
			Buckets: []float64{0.25, .5, 1, 2, 4, 8, 16},
		},
		descAppDefaultLabels,
	)

	registry.MustRegister(reconcileHistogram)
	clusterEventsCounter := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "argocd_cluster_events_total",
		Help: "Number of processes k8s resource events.",
	}, append(descClusterDefaultLabels, "group", "kind"))
	registry.MustRegister(clusterEventsCounter)

	return &MetricsServer{
		registry: registry,
		Server: &http.Server{
			Addr:    addr,
			Handler: mux,
		},
		syncCounter:             syncCounter,
		k8sRequestCounter:       k8sRequestCounter,
		kubectlExecCounter:      kubectlExecCounter,
		kubectlExecPendingGauge: kubectlExecPendingGauge,
		reconcileHistogram:      reconcileHistogram,
		clusterEventsCounter:    clusterEventsCounter,
	}
}

func (m *MetricsServer) RegisterClustersInfoSource(ctx context.Context, source HasClustersInfo) {
	collector := &clusterCollector{infoSource: source}
	go collector.Run(ctx)
	m.registry.MustRegister(collector)
}

// IncSync increments the sync counter for an application
func (m *MetricsServer) IncSync(app *argoappv1.Application, state *argoappv1.OperationState) {
	if !state.Phase.Completed() {
		return
	}
	m.syncCounter.WithLabelValues(app.Namespace, app.Name, app.Spec.GetProject(), string(state.Phase)).Inc()
}

func (m *MetricsServer) IncKubectlExec(command string) {
	m.kubectlExecCounter.WithLabelValues(command).Inc()
}

func (m *MetricsServer) IncKubectlExecPending(command string) {
	m.kubectlExecPendingGauge.WithLabelValues(command).Inc()
}

func (m *MetricsServer) DecKubectlExecPending(command string) {
	m.kubectlExecPendingGauge.WithLabelValues(command).Dec()
}

// IncClusterEventsCount increments the number of cluster events
func (m *MetricsServer) IncClusterEventsCount(server, group, kind string) {
	m.clusterEventsCounter.WithLabelValues(server, group, kind).Inc()
}

// IncKubernetesRequest increments the kubernetes requests counter for an application
func (m *MetricsServer) IncKubernetesRequest(app *argoappv1.Application, server, statusCode, verb, resourceKind, resourceNamespace string) {
	var namespace, name, project string
	if app != nil {
		namespace = app.Namespace
		name = app.Name
		project = app.Spec.GetProject()
	}
	m.k8sRequestCounter.WithLabelValues(
		namespace, name, project, server, statusCode,
		verb, resourceKind, resourceNamespace,
	).Inc()
}

// IncReconcile increments the reconcile counter for an application
func (m *MetricsServer) IncReconcile(app *argoappv1.Application, duration time.Duration) {
	m.reconcileHistogram.WithLabelValues(app.Namespace, app.Name, app.Spec.GetProject()).Observe(duration.Seconds())
}

type appCollector struct {
	store applister.ApplicationLister
}

// NewAppCollector returns a prometheus collector for application metrics
func NewAppCollector(appLister applister.ApplicationLister) prometheus.Collector {
	return &appCollector{
		store: appLister,
	}
}

// NewAppRegistry creates a new prometheus registry that collects applications
func NewAppRegistry(appLister applister.ApplicationLister) *prometheus.Registry {
	registry := prometheus.NewRegistry()
	registry.MustRegister(NewAppCollector(appLister))
	return registry
}

// Describe implements the prometheus.Collector interface
func (c *appCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descAppInfo
	ch <- descAppCreated
	ch <- descAppSyncStatusCode
	ch <- descAppHealthStatus
}

// Collect implements the prometheus.Collector interface
func (c *appCollector) Collect(ch chan<- prometheus.Metric) {
	apps, err := c.store.List(labels.NewSelector())
	if err != nil {
		log.Warnf("Failed to collect applications: %v", err)
		return
	}
	for _, app := range apps {
		collectApps(ch, app)
	}
}

func boolFloat64(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func collectApps(ch chan<- prometheus.Metric, app *argoappv1.Application) {
	addConstMetric := func(desc *prometheus.Desc, t prometheus.ValueType, v float64, lv ...string) {
		project := app.Spec.GetProject()
		lv = append([]string{app.Namespace, app.Name, project}, lv...)
		ch <- prometheus.MustNewConstMetric(desc, t, v, lv...)
	}
	addGauge := func(desc *prometheus.Desc, v float64, lv ...string) {
		addConstMetric(desc, prometheus.GaugeValue, v, lv...)
	}

	addGauge(descAppInfo, 1, git.NormalizeGitURL(app.Spec.Source.RepoURL), app.Spec.Destination.Server, app.Spec.Destination.Namespace)

	addGauge(descAppCreated, float64(app.CreationTimestamp.Unix()))

	syncStatus := app.Status.Sync.Status
	addGauge(descAppSyncStatusCode, boolFloat64(syncStatus == argoappv1.SyncStatusCodeSynced), string(argoappv1.SyncStatusCodeSynced))
	addGauge(descAppSyncStatusCode, boolFloat64(syncStatus == argoappv1.SyncStatusCodeOutOfSync), string(argoappv1.SyncStatusCodeOutOfSync))
	addGauge(descAppSyncStatusCode, boolFloat64(syncStatus == argoappv1.SyncStatusCodeUnknown || syncStatus == ""), string(argoappv1.SyncStatusCodeUnknown))

	healthStatus := app.Status.Health.Status
	addGauge(descAppHealthStatus, boolFloat64(healthStatus == argoappv1.HealthStatusUnknown || healthStatus == ""), argoappv1.HealthStatusUnknown)
	addGauge(descAppHealthStatus, boolFloat64(healthStatus == argoappv1.HealthStatusProgressing), argoappv1.HealthStatusProgressing)
	addGauge(descAppHealthStatus, boolFloat64(healthStatus == argoappv1.HealthStatusSuspended), argoappv1.HealthStatusSuspended)
	addGauge(descAppHealthStatus, boolFloat64(healthStatus == argoappv1.HealthStatusHealthy), argoappv1.HealthStatusHealthy)
	addGauge(descAppHealthStatus, boolFloat64(healthStatus == argoappv1.HealthStatusDegraded), argoappv1.HealthStatusDegraded)
	addGauge(descAppHealthStatus, boolFloat64(healthStatus == argoappv1.HealthStatusMissing), argoappv1.HealthStatusMissing)
}
