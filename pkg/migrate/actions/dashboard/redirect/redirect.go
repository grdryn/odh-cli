package redirect

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/pflag"
	"sigs.k8s.io/yaml"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/opendatahub-io/odh-cli/pkg/migrate/action"
	"github.com/opendatahub-io/odh-cli/pkg/migrate/action/result"
	"github.com/opendatahub-io/odh-cli/pkg/resources"
	"github.com/opendatahub-io/odh-cli/pkg/util/jq"
)

const (
	actionID          = "dashboard.generate-redirect"
	actionName        = "Generate Dashboard Redirect"
	actionDescription = "Generate nginx-redirect resources to maintain backward compatibility after upgrade removes old dashboard route"

	rhodsDashboardLabel = "rhods-dashboard"
	odhDashboardLabel   = "odh-dashboard"

	maxDomainParts = 2

	msgErrNotRootRecorder      = "recorder is not a RootRecorder"
	msgStepDetectPlatform      = "Detect platform type"
	msgPlatformDetectFailed    = "Unable to detect platform type"
	msgPlatformDetected        = "Detected platform %s in namespace %s"
	msgStepDiscoverURL         = "Discover redirect URL"
	msgURLDiscoverFailed       = "Unable to discover redirect URL"
	msgURLDiscovered           = "Discovered redirect URL: %s"
	msgURLOverride             = "Using override redirect URL: %s"
	msgStepApplyResources      = "Apply nginx-redirect resources"
	msgApplyDesc               = "Apply %s"
	msgWouldApply              = "Would apply %s"
	msgUnmarshalFailed         = "Failed to unmarshal YAML: %v"
	msgApplyResourcesFailed    = "Failed to apply resources"
	msgApplyFailed             = "Failed to apply resource: %v"
	msgApplied                 = "Applied %s"
	msgDryRunSkipped           = "Dry run: resources skipped"
	msgApplyResourcesCompleted = "Successfully applied dashboard redirect resources"
)

type DashboardRedirectAction struct {
	RouteHost   string
	RedirectURL string
}

var _ action.ActionConfigurer = (*DashboardRedirectAction)(nil)

func (a *DashboardRedirectAction) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&a.RouteHost, "redirect-route-host", "",
		"Override the hostname for the dashboard redirect route (for legacy custom URLs)")
	fs.StringVar(&a.RedirectURL, "redirect-url", "",
		"Override the auto-discovered redirect destination URL")
}

func (a *DashboardRedirectAction) ID() string                { return actionID }
func (a *DashboardRedirectAction) Name() string              { return actionName }
func (a *DashboardRedirectAction) Description() string       { return actionDescription }
func (a *DashboardRedirectAction) Group() action.ActionGroup { return action.GroupMigration }

func (a *DashboardRedirectAction) CanApply(_ action.Target) bool {
	return true
}

func (a *DashboardRedirectAction) Prepare() action.Task {
	return nil
}

func (a *DashboardRedirectAction) Run() action.Task {
	return &runTask{action: a}
}

type runTask struct {
	action *DashboardRedirectAction
}

func (t *runTask) Validate(ctx context.Context, target action.Target) (*result.ActionResult, error) {
	rootRecorder, ok := target.Recorder.(action.RootRecorder)
	if !ok {
		return nil, errors.New(msgErrNotRootRecorder)
	}

	info := detectPlatform(ctx, target)
	if info.PlatformType == "" {
		target.Recorder.Child("detect-platform", msgStepDetectPlatform).Complete(result.StepFailed, msgPlatformDetectFailed)

		return rootRecorder.Build(), nil
	}
	target.Recorder.Child("detect-platform", msgStepDetectPlatform).Complete(result.StepCompleted, msgPlatformDetected, info.PlatformType, info.Namespace)

	redirectURL := t.resolveRedirectURL(ctx, target)
	if redirectURL == "" {
		target.Recorder.Child("discover-url", msgStepDiscoverURL).Complete(result.StepFailed, msgURLDiscoverFailed)

		return rootRecorder.Build(), nil
	}

	urlMsg := msgURLDiscovered
	if t.action.RedirectURL != "" {
		urlMsg = msgURLOverride
	}
	target.Recorder.Child("discover-url", msgStepDiscoverURL).Complete(result.StepCompleted, urlMsg, redirectURL)

	return rootRecorder.Build(), nil
}

func (t *runTask) Execute(ctx context.Context, target action.Target) (*result.ActionResult, error) {
	rootRecorder, ok := target.Recorder.(action.RootRecorder)
	if !ok {
		return nil, errors.New(msgErrNotRootRecorder)
	}

	info := detectPlatform(ctx, target)
	if info.PlatformType == "" {
		target.Recorder.Child("detect-platform", msgStepDetectPlatform).Complete(result.StepFailed, msgPlatformDetectFailed)

		return rootRecorder.Build(), nil
	}
	target.Recorder.Child("detect-platform", msgStepDetectPlatform).Complete(result.StepCompleted, msgPlatformDetected, info.PlatformType, info.Namespace)

	redirectURL := t.resolveRedirectURL(ctx, target)
	if redirectURL == "" {
		target.Recorder.Child("discover-url", msgStepDiscoverURL).Complete(result.StepFailed, msgURLDiscoverFailed)

		return rootRecorder.Build(), nil
	}

	urlMsg := msgURLDiscovered
	if t.action.RedirectURL != "" {
		urlMsg = msgURLOverride
	}
	target.Recorder.Child("discover-url", msgStepDiscoverURL).Complete(result.StepCompleted, urlMsg, redirectURL)

	applyResources(ctx, target, info.Namespace, info.RouteName, redirectURL, t.action.RouteHost)

	return rootRecorder.Build(), nil
}

func (t *runTask) resolveRedirectURL(ctx context.Context, target action.Target) string {
	if t.action.RedirectURL != "" {
		return strings.TrimRight(t.action.RedirectURL, "/")
	}

	return discoverRedirectURL(ctx, target)
}

type platformInfo struct {
	PlatformType string
	Namespace    string
	RouteName    string
}

func detectPlatform(ctx context.Context, target action.Target) platformInfo {
	info := detectPlatformFromConfig(ctx, target)
	if info.PlatformType != "" {
		return info
	}

	return detectPlatformFromSubscription(ctx, target)
}

func detectPlatformFromConfig(ctx context.Context, target action.Target) platformInfo {
	configs, err := target.Client.Dynamic().Resource(resources.OdhDashboardConfig.GVR()).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil || len(configs.Items) == 0 {
		return platformInfo{}
	}

	for _, item := range configs.Items {
		anns := item.GetAnnotations()
		ns := item.GetNamespace()
		labels := item.GetLabels()

		if strings.Contains(anns["platform.opendatahub.io/type"], "OpenShift AI") || labels["app"] == rhodsDashboardLabel {
			if ns == "" {
				ns = "redhat-ods-applications"
			}

			return platformInfo{PlatformType: "rhoai", Namespace: ns, RouteName: rhodsDashboardLabel}
		} else if labels["app"] == odhDashboardLabel {
			if ns == "" {
				ns = "opendatahub"
			}

			return platformInfo{PlatformType: "odh", Namespace: ns, RouteName: odhDashboardLabel}
		}
	}

	return platformInfo{}
}

func detectPlatformFromSubscription(ctx context.Context, target action.Target) platformInfo {
	subs, err := target.Client.Dynamic().Resource(resources.Subscription.GVR()).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return platformInfo{}
	}

	for _, sub := range subs.Items {
		name, _ := jq.Query[string](&sub, ".spec.name")
		if name == "rhods-operator" {
			return platformInfo{PlatformType: "rhoai", Namespace: "redhat-ods-applications", RouteName: rhodsDashboardLabel}
		}
		if name == "opendatahub-operator" {
			return platformInfo{PlatformType: "odh", Namespace: "opendatahub", RouteName: odhDashboardLabel}
		}
	}

	return platformInfo{}
}

func discoverRedirectURL(ctx context.Context, target action.Target) string {
	links, err := target.Client.Dynamic().Resource(resources.ConsoleLink.GVR()).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, link := range links.Items {
			text, _ := jq.Query[string](&link, ".spec.text")
			if strings.Contains(text, "OpenShift AI") || strings.Contains(text, "Open Data Hub") {
				href, _ := jq.Query[string](&link, ".spec.href")
				if href != "" {
					return strings.TrimRight(href, "/")
				}
			}
		}
	}

	routes, err := target.Client.Dynamic().Resource(resources.Route.GVR()).Namespace("").List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, route := range routes.Items {
			if route.GetName() == "data-science-gateway" {
				host, _ := jq.Query[string](&route, ".spec.host")
				if host != "" {
					return "https://" + host
				}
			}
		}
	}

	return ""
}

func applyResources(ctx context.Context, target action.Target, namespace, routeName, redirectURL, routeHost string) {
	step := target.Recorder.Child("apply-redirect-resources", msgStepApplyResources)

	configMapYAML := fmt.Sprintf(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: nginx-redirect-config
  namespace: %s
  labels:
    app: nginx-redirect
data:
  redirect.conf: |
    location / {
        return 301 %s$request_uri;
    }
`, namespace, redirectURL)

	podYAML := fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: nginx-redirect
  namespace: %s
  labels:
    app: nginx-redirect
spec:
  restartPolicy: Always
  containers:
  - name: nginx
    image: registry.redhat.io/ubi9/nginx-126:latest
    command:
    - /usr/libexec/s2i/run
    ports:
    - containerPort: 8080
      protocol: TCP
    volumeMounts:
    - name: nginx-config
      mountPath: /opt/app-root/etc/nginx.default.d/redirect.conf
      subPath: redirect.conf
  volumes:
  - name: nginx-config
    configMap:
      name: nginx-redirect-config
`, namespace)

	serviceYAML := fmt.Sprintf(`
apiVersion: v1
kind: Service
metadata:
  name: nginx-redirect
  namespace: %s
  labels:
    app: nginx-redirect
spec:
  ports:
  - name: http
    port: 8080
    protocol: TCP
    targetPort: 8080
  selector:
    app: nginx-redirect
  type: ClusterIP
`, namespace)

	hostLine := ""
	if routeHost != "" {
		hostLine = fmt.Sprintf("  host: %s\n", routeHost)
	}

	routeYAML := fmt.Sprintf(`
apiVersion: route.openshift.io/v1
kind: Route
metadata:
  name: %s
  namespace: %s
  annotations:
    haproxy.router.openshift.io/hsts_header: max-age=31536000;includeSubDomains;preload
    kubernetes.io/tls-acme: "true"
  labels:
    app: nginx-redirect
spec:
%s  port:
    targetPort: http
  tls:
    insecureEdgeTerminationPolicy: Redirect
    termination: edge
  to:
    kind: Service
    name: nginx-redirect
    weight: 100
  wildcardPolicy: None
`, routeName, namespace, hostLine)

	resourcesToApply := []struct {
		desc string
		yaml string
		res  resources.ResourceType
	}{
		{"ConfigMap", configMapYAML, resources.ConfigMap},
		{"Pod", podYAML, resources.Pod},
		{"Service", serviceYAML, resources.Service},
		{"Route", routeYAML, resources.Route},
	}

	if strings.Contains(redirectURL, "rh-ai") {
		parsed, err := url.Parse(redirectURL)
		if err == nil && parsed.Hostname() != "" {
			parts := strings.SplitN(parsed.Hostname(), ".", maxDomainParts)
			if len(parts) > 1 {
				legacyHost := "data-science-gateway." + parts[1]
				legacyRouteYAML := fmt.Sprintf(`
apiVersion: route.openshift.io/v1
kind: Route
metadata:
  name: data-science-gateway-legacy
  namespace: %s
  annotations:
    haproxy.router.openshift.io/hsts_header: max-age=31536000;includeSubDomains;preload
    kubernetes.io/tls-acme: "true"
  labels:
    app: nginx-redirect
spec:
  host: %s
  port:
    targetPort: http
  tls:
    insecureEdgeTerminationPolicy: Redirect
    termination: edge
  to:
    kind: Service
    name: nginx-redirect
    weight: 100
  wildcardPolicy: None
`, namespace, legacyHost)
				resourcesToApply = append(resourcesToApply, struct {
					desc string
					yaml string
					res  resources.ResourceType
				}{"Legacy Route", legacyRouteYAML, resources.Route})
			}
		}
	}

	for _, r := range resourcesToApply {
		child := step.Child("apply-"+strings.ToLower(strings.ReplaceAll(r.desc, " ", "-")), fmt.Sprintf(msgApplyDesc, r.desc))

		if target.DryRun {
			child.Complete(result.StepSkipped, msgWouldApply, r.desc)

			continue
		}

		var obj unstructured.Unstructured
		if err := yaml.Unmarshal([]byte(r.yaml), &obj.Object); err != nil {
			child.Complete(result.StepFailed, msgUnmarshalFailed, err)
			step.Complete(result.StepFailed, msgApplyResourcesFailed)

			return
		}

		_, err := target.Client.Dynamic().Resource(r.res.GVR()).Namespace(namespace).Apply(ctx, obj.GetName(), &obj, metav1.ApplyOptions{FieldManager: "odh-cli", Force: true})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			child.Complete(result.StepFailed, msgApplyFailed, err)
			step.Complete(result.StepFailed, msgApplyResourcesFailed)

			return
		}

		child.Complete(result.StepCompleted, msgApplied, r.desc)
	}

	if target.DryRun {
		step.Complete(result.StepSkipped, msgDryRunSkipped)
	} else {
		step.Complete(result.StepCompleted, msgApplyResourcesCompleted)
	}
}
