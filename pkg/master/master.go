/*
Copyright 2014 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package master

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/rest"
	"k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/apimachinery/registered"
	"k8s.io/kubernetes/pkg/apis/extensions"
	"k8s.io/kubernetes/pkg/apiserver"
	apiservermetrics "k8s.io/kubernetes/pkg/apiserver/metrics"
	"k8s.io/kubernetes/pkg/genericapiserver"
	"k8s.io/kubernetes/pkg/healthz"
	kubeletclient "k8s.io/kubernetes/pkg/kubelet/client"
	"k8s.io/kubernetes/pkg/master/ports"
	"k8s.io/kubernetes/pkg/registry/componentstatus"
	configmapetcd "k8s.io/kubernetes/pkg/registry/configmap/etcd"
	controlleretcd "k8s.io/kubernetes/pkg/registry/controller/etcd"
	deploymentetcd "k8s.io/kubernetes/pkg/registry/deployment/etcd"
	"k8s.io/kubernetes/pkg/registry/endpoint"
	endpointsetcd "k8s.io/kubernetes/pkg/registry/endpoint/etcd"
	eventetcd "k8s.io/kubernetes/pkg/registry/event/etcd"
	expcontrolleretcd "k8s.io/kubernetes/pkg/registry/experimental/controller/etcd"
	"k8s.io/kubernetes/pkg/registry/generic"
	ingressetcd "k8s.io/kubernetes/pkg/registry/ingress/etcd"
	jobetcd "k8s.io/kubernetes/pkg/registry/job/etcd"
	limitrangeetcd "k8s.io/kubernetes/pkg/registry/limitrange/etcd"
	"k8s.io/kubernetes/pkg/registry/namespace"
	namespaceetcd "k8s.io/kubernetes/pkg/registry/namespace/etcd"
	"k8s.io/kubernetes/pkg/registry/node"
	nodeetcd "k8s.io/kubernetes/pkg/registry/node/etcd"
	pvetcd "k8s.io/kubernetes/pkg/registry/persistentvolume/etcd"
	pvcetcd "k8s.io/kubernetes/pkg/registry/persistentvolumeclaim/etcd"
	podetcd "k8s.io/kubernetes/pkg/registry/pod/etcd"
	pspetcd "k8s.io/kubernetes/pkg/registry/podsecuritypolicy/etcd"
	podtemplateetcd "k8s.io/kubernetes/pkg/registry/podtemplate/etcd"
	replicasetetcd "k8s.io/kubernetes/pkg/registry/replicaset/etcd"
	resourcequotaetcd "k8s.io/kubernetes/pkg/registry/resourcequota/etcd"
	secretetcd "k8s.io/kubernetes/pkg/registry/secret/etcd"
	"k8s.io/kubernetes/pkg/registry/service"
	etcdallocator "k8s.io/kubernetes/pkg/registry/service/allocator/etcd"
	serviceetcd "k8s.io/kubernetes/pkg/registry/service/etcd"
	ipallocator "k8s.io/kubernetes/pkg/registry/service/ipallocator"
	serviceaccountetcd "k8s.io/kubernetes/pkg/registry/serviceaccount/etcd"
	thirdpartyresourceetcd "k8s.io/kubernetes/pkg/registry/thirdpartyresource/etcd"
	"k8s.io/kubernetes/pkg/registry/thirdpartyresourcedata"
	thirdpartyresourcedataetcd "k8s.io/kubernetes/pkg/registry/thirdpartyresourcedata/etcd"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/storage"
	etcdmetrics "k8s.io/kubernetes/pkg/storage/etcd/metrics"
	etcdutil "k8s.io/kubernetes/pkg/storage/etcd/util"
	"k8s.io/kubernetes/pkg/util/sets"
	"k8s.io/kubernetes/pkg/util/wait"

	daemonetcd "k8s.io/kubernetes/pkg/registry/daemonset/etcd"
	horizontalpodautoscaleretcd "k8s.io/kubernetes/pkg/registry/horizontalpodautoscaler/etcd"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/kubernetes/pkg/registry/service/allocator"
	"k8s.io/kubernetes/pkg/registry/service/portallocator"
)

type Config struct {
	*genericapiserver.Config

	EnableCoreControllers bool
	EventTTL              time.Duration
	KubeletClient         kubeletclient.KubeletClient
	// Used to start and monitor tunneling
	Tunneler Tunneler
}

// Master contains state for a Kubernetes cluster master/api server.
type Master struct {
	*genericapiserver.GenericAPIServer

	// Map of v1 resources to their REST storages.
	v1ResourcesStorage map[string]rest.Storage

	enableCoreControllers bool
	// registries are internal client APIs for accessing the storage layer
	// TODO: define the internal typed interface in a way that clients can
	// also be replaced
	nodeRegistry              node.Registry
	namespaceRegistry         namespace.Registry
	serviceRegistry           service.Registry
	endpointRegistry          endpoint.Registry
	serviceClusterIPAllocator service.RangeRegistry
	serviceNodePortAllocator  service.RangeRegistry

	// storage for third party objects
	thirdPartyStorage storage.Interface
	// map from api path to a tuple of (storage for the objects, APIGroup)
	thirdPartyResources map[string]thirdPartyEntry
	// protects the map
	thirdPartyResourcesLock sync.RWMutex

	// Used to start and monitor tunneling
	tunneler Tunneler
}

// thirdPartyEntry combines objects storage and API group into one struct
// for easy lookup.
type thirdPartyEntry struct {
	storage *thirdpartyresourcedataetcd.REST
	group   unversioned.APIGroup
}

// New returns a new instance of Master from the given config.
// Certain config fields will be set to a default value if unset.
// Certain config fields must be specified, including:
//   KubeletClient
func New(c *Config) (*Master, error) {
	if c.KubeletClient == nil {
		return nil, fmt.Errorf("Master.New() called with config.KubeletClient == nil")
	}

	s, err := genericapiserver.New(c.Config)
	if err != nil {
		return nil, err
	}

	m := &Master{
		GenericAPIServer:      s,
		enableCoreControllers: c.EnableCoreControllers,
		tunneler:              c.Tunneler,
	}
	m.InstallAPIs(c)

	// TODO: Attempt clean shutdown?
	if m.enableCoreControllers {
		m.NewBootstrapController().Start()
	}

	return m, nil
}

func resetMetrics(w http.ResponseWriter, req *http.Request) {
	apiservermetrics.Reset()
	etcdmetrics.Reset()
	io.WriteString(w, "metrics reset\n")
}

func (m *Master) InstallAPIs(c *Config) {
	apiGroupsInfo := []genericapiserver.APIGroupInfo{}

	// Install v1 unless disabled.
	if !m.ApiGroupVersionOverrides["api/v1"].Disable {
		// Install v1 API.
		m.initV1ResourcesStorage(c)
		apiGroupInfo := genericapiserver.APIGroupInfo{
			GroupMeta: *registered.GroupOrDie(api.GroupName),
			VersionedResourcesStorageMap: map[string]map[string]rest.Storage{
				"v1": m.v1ResourcesStorage,
			},
			IsLegacyGroup:        true,
			Scheme:               api.Scheme,
			ParameterCodec:       api.ParameterCodec,
			NegotiatedSerializer: api.Codecs,
		}
		apiGroupsInfo = append(apiGroupsInfo, apiGroupInfo)
	}

	// Run the tunneler.
	healthzChecks := []healthz.HealthzChecker{}
	if m.tunneler != nil {
		m.tunneler.Run(m.getNodeAddresses)
		healthzChecks = append(healthzChecks, healthz.NamedCheck("SSH Tunnel Check", m.IsTunnelSyncHealthy))
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "apiserver_proxy_tunnel_sync_latency_secs",
			Help: "The time since the last successful synchronization of the SSH tunnels for proxy requests.",
		}, func() float64 { return float64(m.tunneler.SecondsSinceSync()) })
	}

	// TODO(nikhiljindal): Refactor generic parts of support services (like /versions) to genericapiserver.
	apiserver.InstallSupport(m.MuxHelper, m.RootWebService, healthzChecks...)
	if c.EnableProfiling {
		m.MuxHelper.HandleFunc("/resetMetrics", resetMetrics)
	}

	// Install root web services
	m.HandlerContainer.Add(m.RootWebService)

	// allGroups records all supported groups at /apis
	allGroups := []unversioned.APIGroup{}
	// Install extensions unless disabled.
	if !m.ApiGroupVersionOverrides["extensions/v1beta1"].Disable {
		m.thirdPartyStorage = c.StorageDestinations.APIGroups[extensions.GroupName].Default
		m.thirdPartyResources = map[string]thirdPartyEntry{}

		extensionResources := m.getExtensionResources(c)
		extensionsGroupMeta := registered.GroupOrDie(extensions.GroupName)
		// Update the prefered version as per StorageVersions in the config.
		storageVersion, found := c.StorageVersions[extensionsGroupMeta.GroupVersion.Group]
		if !found {
			glog.Fatalf("Couldn't find storage version of group %v", extensionsGroupMeta.GroupVersion.Group)
		}
		preferedGroupVersion, err := unversioned.ParseGroupVersion(storageVersion)
		if err != nil {
			glog.Fatalf("Error in parsing group version %s: %v", storageVersion, err)
		}
		extensionsGroupMeta.GroupVersion = preferedGroupVersion

		apiGroupInfo := genericapiserver.APIGroupInfo{
			GroupMeta: *extensionsGroupMeta,
			VersionedResourcesStorageMap: map[string]map[string]rest.Storage{
				"v1beta1": extensionResources,
			},
			OptionsExternalVersion: &registered.GroupOrDie(api.GroupName).GroupVersion,
			Scheme:                 api.Scheme,
			ParameterCodec:         api.ParameterCodec,
			NegotiatedSerializer:   api.Codecs,
		}
		apiGroupsInfo = append(apiGroupsInfo, apiGroupInfo)

		extensionsGVForDiscovery := unversioned.GroupVersionForDiscovery{
			GroupVersion: extensionsGroupMeta.GroupVersion.String(),
			Version:      extensionsGroupMeta.GroupVersion.Version,
		}
		group := unversioned.APIGroup{
			Name:             extensionsGroupMeta.GroupVersion.Group,
			Versions:         []unversioned.GroupVersionForDiscovery{extensionsGVForDiscovery},
			PreferredVersion: extensionsGVForDiscovery,
		}
		allGroups = append(allGroups, group)
	}
	if err := m.InstallAPIGroups(apiGroupsInfo); err != nil {
		glog.Fatalf("Error in registering group versions: %v", err)
	}
}

func (m *Master) initV1ResourcesStorage(c *Config) {
	storageDecorator := m.StorageDecorator()
	dbClient := func(resource string) storage.Interface { return c.StorageDestinations.Get("", resource) }

	podTemplateStorage := podtemplateetcd.NewREST(dbClient("podTemplates"), storageDecorator)

	eventStorage := eventetcd.NewREST(dbClient("events"), storageDecorator, uint64(c.EventTTL.Seconds()))
	limitRangeStorage := limitrangeetcd.NewREST(dbClient("limitRanges"), storageDecorator)

	resourceQuotaStorage, resourceQuotaStatusStorage := resourcequotaetcd.NewREST(dbClient("resourceQuotas"), storageDecorator)
	secretStorage := secretetcd.NewREST(dbClient("secrets"), storageDecorator)
	serviceAccountStorage := serviceaccountetcd.NewREST(dbClient("serviceAccounts"), storageDecorator)
	persistentVolumeStorage, persistentVolumeStatusStorage := pvetcd.NewREST(dbClient("persistentVolumes"), storageDecorator)
	persistentVolumeClaimStorage, persistentVolumeClaimStatusStorage := pvcetcd.NewREST(dbClient("persistentVolumeClaims"), storageDecorator)
	configMapStorage := configmapetcd.NewREST(dbClient("configMaps"), storageDecorator)

	namespaceStorage, namespaceStatusStorage, namespaceFinalizeStorage := namespaceetcd.NewREST(dbClient("namespaces"), storageDecorator)
	m.namespaceRegistry = namespace.NewRegistry(namespaceStorage)

	endpointsStorage := endpointsetcd.NewREST(dbClient("endpoints"), storageDecorator)
	m.endpointRegistry = endpoint.NewRegistry(endpointsStorage)

	nodeStorage := nodeetcd.NewStorage(dbClient("nodes"), storageDecorator, c.KubeletClient, m.ProxyTransport)
	m.nodeRegistry = node.NewRegistry(nodeStorage.Node)

	podStorage := podetcd.NewStorage(
		dbClient("pods"),
		storageDecorator,
		kubeletclient.ConnectionInfoGetter(nodeStorage.Node),
		m.ProxyTransport,
	)

	serviceStorage, serviceStatusStorage := serviceetcd.NewREST(dbClient("services"), storageDecorator)
	m.serviceRegistry = service.NewRegistry(serviceStorage)

	var serviceClusterIPRegistry service.RangeRegistry
	serviceClusterIPRange := m.ServiceClusterIPRange
	if serviceClusterIPRange == nil {
		glog.Fatalf("service clusterIPRange is nil")
		return
	}
	serviceClusterIPAllocator := ipallocator.NewAllocatorCIDRRange(serviceClusterIPRange, func(max int, rangeSpec string) allocator.Interface {
		mem := allocator.NewAllocationMap(max, rangeSpec)
		etcd := etcdallocator.NewEtcd(mem, "/ranges/serviceips", api.Resource("serviceipallocations"), dbClient("services"))
		serviceClusterIPRegistry = etcd
		return etcd
	})
	m.serviceClusterIPAllocator = serviceClusterIPRegistry

	var serviceNodePortRegistry service.RangeRegistry
	serviceNodePortAllocator := portallocator.NewPortAllocatorCustom(m.ServiceNodePortRange, func(max int, rangeSpec string) allocator.Interface {
		mem := allocator.NewAllocationMap(max, rangeSpec)
		etcd := etcdallocator.NewEtcd(mem, "/ranges/servicenodeports", api.Resource("servicenodeportallocations"), dbClient("services"))
		serviceNodePortRegistry = etcd
		return etcd
	})
	m.serviceNodePortAllocator = serviceNodePortRegistry

	controllerStorage, controllerStatusStorage := controlleretcd.NewREST(dbClient("replicationControllers"), storageDecorator)

	m.v1ResourcesStorage = map[string]rest.Storage{
		"pods":             podStorage.Pod,
		"pods/attach":      podStorage.Attach,
		"pods/status":      podStorage.Status,
		"pods/log":         podStorage.Log,
		"pods/exec":        podStorage.Exec,
		"pods/portforward": podStorage.PortForward,
		"pods/proxy":       podStorage.Proxy,
		"pods/binding":     podStorage.Binding,
		"bindings":         podStorage.Binding,

		"podTemplates": podTemplateStorage,

		"replicationControllers":        controllerStorage,
		"replicationControllers/status": controllerStatusStorage,
		"services":                      service.NewStorage(m.serviceRegistry, m.endpointRegistry, serviceClusterIPAllocator, serviceNodePortAllocator, m.ProxyTransport),
		"services/status":               serviceStatusStorage,
		"endpoints":                     endpointsStorage,
		"nodes":                         nodeStorage.Node,
		"nodes/status":                  nodeStorage.Status,
		"nodes/proxy":                   nodeStorage.Proxy,
		"events":                        eventStorage,

		"limitRanges":                   limitRangeStorage,
		"resourceQuotas":                resourceQuotaStorage,
		"resourceQuotas/status":         resourceQuotaStatusStorage,
		"namespaces":                    namespaceStorage,
		"namespaces/status":             namespaceStatusStorage,
		"namespaces/finalize":           namespaceFinalizeStorage,
		"secrets":                       secretStorage,
		"serviceAccounts":               serviceAccountStorage,
		"persistentVolumes":             persistentVolumeStorage,
		"persistentVolumes/status":      persistentVolumeStatusStorage,
		"persistentVolumeClaims":        persistentVolumeClaimStorage,
		"persistentVolumeClaims/status": persistentVolumeClaimStatusStorage,
		"configMaps":                    configMapStorage,

		"componentStatuses": componentstatus.NewStorage(func() map[string]apiserver.Server { return m.getServersToValidate(c) }),
	}
}

// NewBootstrapController returns a controller for watching the core capabilities of the master.
func (m *Master) NewBootstrapController() *Controller {
	return &Controller{
		NamespaceRegistry: m.namespaceRegistry,
		ServiceRegistry:   m.serviceRegistry,
		MasterCount:       m.MasterCount,

		EndpointRegistry: m.endpointRegistry,
		EndpointInterval: 10 * time.Second,

		ServiceClusterIPRegistry: m.serviceClusterIPAllocator,
		ServiceClusterIPRange:    m.ServiceClusterIPRange,
		ServiceClusterIPInterval: 3 * time.Minute,

		ServiceNodePortRegistry: m.serviceNodePortAllocator,
		ServiceNodePortRange:    m.ServiceNodePortRange,
		ServiceNodePortInterval: 3 * time.Minute,

		PublicIP: m.ClusterIP,

		ServiceIP:                 m.ServiceReadWriteIP,
		ServicePort:               m.ServiceReadWritePort,
		ExtraServicePorts:         m.ExtraServicePorts,
		ExtraEndpointPorts:        m.ExtraEndpointPorts,
		PublicServicePort:         m.PublicReadWritePort,
		KubernetesServiceNodePort: m.KubernetesServiceNodePort,
	}
}

func (m *Master) getServersToValidate(c *Config) map[string]apiserver.Server {
	serversToValidate := map[string]apiserver.Server{
		"controller-manager": {Addr: "127.0.0.1", Port: ports.ControllerManagerPort, Path: "/healthz"},
		"scheduler":          {Addr: "127.0.0.1", Port: ports.SchedulerPort, Path: "/healthz"},
	}

	for ix, machine := range c.StorageDestinations.Backends() {
		etcdUrl, err := url.Parse(machine)
		if err != nil {
			glog.Errorf("Failed to parse etcd url for validation: %v", err)
			continue
		}
		var port int
		var addr string
		if strings.Contains(etcdUrl.Host, ":") {
			var portString string
			addr, portString, err = net.SplitHostPort(etcdUrl.Host)
			if err != nil {
				glog.Errorf("Failed to split host/port: %s (%v)", etcdUrl.Host, err)
				continue
			}
			port, _ = strconv.Atoi(portString)
		} else {
			addr = etcdUrl.Host
			port = 4001
		}
		// TODO: etcd health checking should be abstracted in the storage tier
		serversToValidate[fmt.Sprintf("etcd-%d", ix)] = apiserver.Server{Addr: addr, Port: port, Path: "/health", Validate: etcdutil.EtcdHealthCheck}
	}
	return serversToValidate
}

// HasThirdPartyResource returns true if a particular third party resource currently installed.
func (m *Master) HasThirdPartyResource(rsrc *extensions.ThirdPartyResource) (bool, error) {
	_, group, err := thirdpartyresourcedata.ExtractApiGroupAndKind(rsrc)
	if err != nil {
		return false, err
	}
	path := makeThirdPartyPath(group)
	services := m.HandlerContainer.RegisteredWebServices()
	for ix := range services {
		if services[ix].RootPath() == path {
			return true, nil
		}
	}
	return false, nil
}

func (m *Master) removeThirdPartyStorage(path string) error {
	m.thirdPartyResourcesLock.Lock()
	defer m.thirdPartyResourcesLock.Unlock()
	storage, found := m.thirdPartyResources[path]
	if found {
		if err := m.removeAllThirdPartyResources(storage.storage); err != nil {
			return err
		}
		delete(m.thirdPartyResources, path)
		m.RemoveAPIGroupForDiscovery(getThirdPartyGroupName(path))
	}
	return nil
}

// RemoveThirdPartyResource removes all resources matching `path`.  Also deletes any stored data
func (m *Master) RemoveThirdPartyResource(path string) error {
	if err := m.removeThirdPartyStorage(path); err != nil {
		return err
	}

	services := m.HandlerContainer.RegisteredWebServices()
	for ix := range services {
		root := services[ix].RootPath()
		if root == path || strings.HasPrefix(root, path+"/") {
			m.HandlerContainer.Remove(services[ix])
		}
	}
	return nil
}

func (m *Master) removeAllThirdPartyResources(registry *thirdpartyresourcedataetcd.REST) error {
	ctx := api.NewDefaultContext()
	existingData, err := registry.List(ctx, nil)
	if err != nil {
		return err
	}
	list, ok := existingData.(*extensions.ThirdPartyResourceDataList)
	if !ok {
		return fmt.Errorf("expected a *ThirdPartyResourceDataList, got %#v", list)
	}
	for ix := range list.Items {
		item := &list.Items[ix]
		if _, err := registry.Delete(ctx, item.Name, nil); err != nil {
			return err
		}
	}
	return nil
}

// ListThirdPartyResources lists all currently installed third party resources
func (m *Master) ListThirdPartyResources() []string {
	m.thirdPartyResourcesLock.RLock()
	defer m.thirdPartyResourcesLock.RUnlock()
	result := []string{}
	for key := range m.thirdPartyResources {
		result = append(result, key)
	}
	return result
}

func (m *Master) addThirdPartyResourceStorage(path string, storage *thirdpartyresourcedataetcd.REST, apiGroup unversioned.APIGroup) {
	m.thirdPartyResourcesLock.Lock()
	defer m.thirdPartyResourcesLock.Unlock()
	m.thirdPartyResources[path] = thirdPartyEntry{storage, apiGroup}
	m.AddAPIGroupForDiscovery(apiGroup)
}

// InstallThirdPartyResource installs a third party resource specified by 'rsrc'.  When a resource is
// installed a corresponding RESTful resource is added as a valid path in the web service provided by
// the master.
//
// For example, if you install a resource ThirdPartyResource{ Name: "foo.company.com", Versions: {"v1"} }
// then the following RESTful resource is created on the server:
//   http://<host>/apis/company.com/v1/foos/...
func (m *Master) InstallThirdPartyResource(rsrc *extensions.ThirdPartyResource) error {
	kind, group, err := thirdpartyresourcedata.ExtractApiGroupAndKind(rsrc)
	if err != nil {
		return err
	}
	thirdparty := m.thirdpartyapi(group, kind, rsrc.Versions[0].Name)
	if err := thirdparty.InstallREST(m.HandlerContainer); err != nil {
		glog.Fatalf("Unable to setup thirdparty api: %v", err)
	}
	path := makeThirdPartyPath(group)
	groupVersion := unversioned.GroupVersionForDiscovery{
		GroupVersion: group + "/" + rsrc.Versions[0].Name,
		Version:      rsrc.Versions[0].Name,
	}
	apiGroup := unversioned.APIGroup{
		Name:     group,
		Versions: []unversioned.GroupVersionForDiscovery{groupVersion},
	}
	apiserver.AddGroupWebService(api.Codecs, m.HandlerContainer, path, apiGroup)
	m.addThirdPartyResourceStorage(path, thirdparty.Storage[strings.ToLower(kind)+"s"].(*thirdpartyresourcedataetcd.REST), apiGroup)
	apiserver.InstallServiceErrorHandler(api.Codecs, m.HandlerContainer, m.NewRequestInfoResolver(), []string{thirdparty.GroupVersion.String()})
	return nil
}

func (m *Master) thirdpartyapi(group, kind, version string) *apiserver.APIGroupVersion {
	resourceStorage := thirdpartyresourcedataetcd.NewREST(m.thirdPartyStorage, generic.UndecoratedStorage, group, kind)

	apiRoot := makeThirdPartyPath("")

	storage := map[string]rest.Storage{
		strings.ToLower(kind) + "s": resourceStorage,
	}

	optionsExternalVersion := registered.GroupOrDie(api.GroupName).GroupVersion
	internalVersion := unversioned.GroupVersion{Group: group, Version: runtime.APIVersionInternal}
	externalVersion := unversioned.GroupVersion{Group: group, Version: version}

	return &apiserver.APIGroupVersion{
		Root:                apiRoot,
		GroupVersion:        externalVersion,
		RequestInfoResolver: m.NewRequestInfoResolver(),

		Creater:   thirdpartyresourcedata.NewObjectCreator(group, version, api.Scheme),
		Convertor: api.Scheme,
		Typer:     api.Scheme,

		Mapper:                 thirdpartyresourcedata.NewMapper(registered.GroupOrDie(extensions.GroupName).RESTMapper, kind, version, group),
		Linker:                 registered.GroupOrDie(extensions.GroupName).SelfLinker,
		Storage:                storage,
		OptionsExternalVersion: &optionsExternalVersion,

		Serializer:     thirdpartyresourcedata.NewNegotiatedSerializer(api.Codecs, kind, externalVersion, internalVersion),
		ParameterCodec: api.ParameterCodec,

		Context: m.RequestContextMapper,

		MinRequestTimeout: m.MinRequestTimeout,
	}
}

// getExperimentalResources returns the resources for extensions api
func (m *Master) getExtensionResources(c *Config) map[string]rest.Storage {
	// All resources except these are disabled by default.
	enabledResources := sets.NewString("horizontalpodautoscalers", "ingresses", "jobs", "replicasets", "deployments")
	resourceOverrides := m.ApiGroupVersionOverrides["extensions/v1beta1"].ResourceOverrides
	isEnabled := func(resource string) bool {
		// Check if the resource has been overriden.
		enabled, ok := resourceOverrides[resource]
		if !ok {
			return enabledResources.Has(resource)
		}
		return enabled
	}
	storageDecorator := m.StorageDecorator()
	dbClient := func(resource string) storage.Interface {
		return c.StorageDestinations.Get(extensions.GroupName, resource)
	}

	storage := map[string]rest.Storage{}
	if isEnabled("horizontalpodautoscalers") {
		autoscalerStorage, autoscalerStatusStorage := horizontalpodautoscaleretcd.NewREST(dbClient("horizontalpodautoscalers"), storageDecorator)
		storage["horizontalpodautoscalers"] = autoscalerStorage
		storage["horizontalpodautoscalers/status"] = autoscalerStatusStorage
		controllerStorage := expcontrolleretcd.NewStorage(c.StorageDestinations.Get("", "replicationControllers"), storageDecorator)
		storage["replicationcontrollers"] = controllerStorage.ReplicationController
		storage["replicationcontrollers/scale"] = controllerStorage.Scale
	}
	if isEnabled("thirdpartyresources") {
		thirdPartyResourceStorage := thirdpartyresourceetcd.NewREST(dbClient("thirdpartyresources"), storageDecorator)
		thirdPartyControl := ThirdPartyController{
			master: m,
			thirdPartyResourceRegistry: thirdPartyResourceStorage,
		}
		go func() {
			wait.Forever(func() {
				if err := thirdPartyControl.SyncResources(); err != nil {
					glog.Warningf("third party resource sync failed: %v", err)
				}
			}, 10*time.Second)
		}()

		storage["thirdpartyresources"] = thirdPartyResourceStorage
	}

	if isEnabled("daemonsets") {
		daemonSetStorage, daemonSetStatusStorage := daemonetcd.NewREST(dbClient("daemonsets"), storageDecorator)
		storage["daemonsets"] = daemonSetStorage
		storage["daemonsets/status"] = daemonSetStatusStorage
	}
	if isEnabled("deployments") {
		deploymentStorage := deploymentetcd.NewStorage(dbClient("deployments"), storageDecorator)
		storage["deployments"] = deploymentStorage.Deployment
		storage["deployments/status"] = deploymentStorage.Status
		// TODO(madhusudancs): Install scale when Scale group issues are fixed (see issue #18528).
		// storage["deployments/scale"] = deploymentStorage.Scale
		storage["deployments/rollback"] = deploymentStorage.Rollback
	}
	if isEnabled("jobs") {
		jobStorage, jobStatusStorage := jobetcd.NewREST(dbClient("jobs"), storageDecorator)
		storage["jobs"] = jobStorage
		storage["jobs/status"] = jobStatusStorage
	}
	if isEnabled("ingresses") {
		ingressStorage, ingressStatusStorage := ingressetcd.NewREST(dbClient("ingresses"), storageDecorator)
		storage["ingresses"] = ingressStorage
		storage["ingresses/status"] = ingressStatusStorage
	}
	if isEnabled("podsecuritypolicy") {
		podSecurityPolicyStorage := pspetcd.NewREST(dbClient("podsecuritypolicy"), storageDecorator)
		storage["podSecurityPolicies"] = podSecurityPolicyStorage
	}
	if isEnabled("replicasets") {
		replicaSetStorage := replicasetetcd.NewStorage(dbClient("replicasets"), storageDecorator)
		storage["replicasets"] = replicaSetStorage.ReplicaSet
		storage["replicasets/status"] = replicaSetStorage.Status
	}

	return storage
}

// findExternalAddress returns ExternalIP of provided node with fallback to LegacyHostIP.
func findExternalAddress(node *api.Node) (string, error) {
	var fallback string
	for ix := range node.Status.Addresses {
		addr := &node.Status.Addresses[ix]
		if addr.Type == api.NodeExternalIP {
			return addr.Address, nil
		}
		if fallback == "" && addr.Type == api.NodeLegacyHostIP {
			fallback = addr.Address
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("Couldn't find external address: %v", node)
}

func (m *Master) getNodeAddresses() ([]string, error) {
	nodes, err := m.nodeRegistry.ListNodes(api.NewDefaultContext(), nil)
	if err != nil {
		return nil, err
	}
	addrs := []string{}
	for ix := range nodes.Items {
		node := &nodes.Items[ix]
		addr, err := findExternalAddress(node)
		if err != nil {
			return nil, err
		}
		addrs = append(addrs, addr)
	}
	return addrs, nil
}

func (m *Master) IsTunnelSyncHealthy(req *http.Request) error {
	if m.tunneler == nil {
		return nil
	}
	lag := m.tunneler.SecondsSinceSync()
	if lag > 600 {
		return fmt.Errorf("Tunnel sync is taking to long: %d", lag)
	}
	return nil
}
