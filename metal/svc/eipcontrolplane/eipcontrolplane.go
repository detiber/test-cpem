package eipcontrolplane

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/packethost/packngo"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/equinix/cloud-provider-equinix-metal/metal/svc"
)

const (
	externalServiceName         = "cloud-provider-equinix-metal-kubernetes-external"
	externalServiceNamespace    = "kube-system"
	metallbAnnotation           = "metallb.universe.tf/address-pool"
	metallbDisabledtag          = "disabled-metallb-do-not-use-any-address-pool"
	checkLoopTimerSeconds       = 60
	deprecatedControlPlaneLabel = "node-role.kubernetes.io/master"
	controlPlaneLabel           = "node-role.kubernetes.io/control-plane"
)

/*
 ControlPlaneEndpointManager checks the availability of an elastic IP for
 the control plane and if it exists the reconciliation guarantees that it is
 attached to a healthy control plane.

 The general steps are:
 1. Check if the passed ElasticIP tags returns a valid Elastic IP via Equinix Metal API.
 2. If there is NOT an ElasticIP with those tags just end the reconciliation
 3. If there is an ElasticIP use the kubernetes client-go to check if it
 returns a valid response
 4. If the response returned via client-go is good we do not need to do anything
 5. If the response if wrong or it terminated it means that the device behind
 the ElasticIP is not working correctly and we have to find a new one.
 6. Ping the other control plane available in the cluster, if one of them work
 assign the ElasticIP to that device.
 7. If NO Control Planes succeed, the cluster is unhealthy and the
 reconciliation terminates without changing the current state of the system.
*/
type ControlPlaneEndpointManager struct {
	clientSet                      kubernetes.Interface
	sharedInformerFactory          informers.SharedInformerFactory
	apiServerPort                  int32 // node on which the EIP is listening
	nodeAPIServerPort              int32 // port on which the api server is listening on the control plane nodes
	eipTag                         string
	client                         *packngo.Client
	projectID                      string
	httpClient                     *http.Client
	deprecatedControlPlaneSelector labels.Selector
	controlPlaneLabelSelector      labels.Selector
	assignmentMutex                sync.Mutex
	serviceMutex                   sync.Mutex
	endpointsMutex                 sync.Mutex
}

func NewControlPlaneEndpointManager(eipTag, projectID string, client *packngo.Client, apiServerPort int32) *ControlPlaneEndpointManager {
	return &ControlPlaneEndpointManager{
		client: client,
		httpClient: &http.Client{
			Timeout: time.Second * 5,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			}},
		eipTag:        eipTag,
		projectID:     projectID,
		apiServerPort: apiServerPort,
	}
}

func (m *ControlPlaneEndpointManager) Name() string {
	return "controlPlaneEndpointManager"
}

func (m *ControlPlaneEndpointManager) Init(ctx context.Context, clientSet kubernetes.Interface, sharedInformerFactory informers.SharedInformerFactory) error {
	m.clientSet = clientSet
	m.sharedInformerFactory = sharedInformerFactory

	deprecatedReq, err := labels.NewRequirement(deprecatedControlPlaneLabel, selection.Exists, nil)
	if err != nil {
		return err
	}

	m.deprecatedControlPlaneSelector = labels.NewSelector().Add(*deprecatedReq)

	req, err := labels.NewRequirement(controlPlaneLabel, selection.Exists, nil)
	if err != nil {
		return err
	}

	m.controlPlaneLabelSelector = labels.NewSelector().Add(*req)

	go m.timerLoop(ctx)

	// TODO: add a sane reconciliation timeout through a context with timeout

	sharedInformerFactory.Core().V1().Nodes().Informer().AddEventHandler(
		cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				n, _ := obj.(*v1.Node)
				return m.nodeFilter(n)
			},
			Handler: cache.ResourceEventHandlerFuncs{
				AddFunc: func(obj interface{}) {
					m.serviceMutex.Lock()
					defer m.serviceMutex.Unlock()

					n, _ := obj.(*v1.Node)
					klog.Infof("handling add, node: %s", n.Name)

					if m.nodeAPIServerPort == 0 {
						if err := m.tryDefaultAPIServerPorts(); err != nil {
							klog.Errorf("failed to default api server port configuration: %v", err)
						}
					}
				},
				UpdateFunc: func(old, obj interface{}) {
					m.assignmentMutex.Lock()
					defer m.assignmentMutex.Unlock()

					oldNode, _ := obj.(*v1.Node)
					newNode, _ := obj.(*v1.Node)
					klog.Infof("handling update, node: %s", newNode.Name)

					// If the node has transititioned to unschedulable
					if (oldNode.Spec.Unschedulable != newNode.Spec.Unschedulable) && newNode.Spec.Unschedulable {
						controlPlaneEndpoint, err := m.getControlPlaneEndpointReservation()
						if err != nil {
							klog.Errorf("failed to get the control plane endpoint for the cluster: %v", err)
							return
						}

						hasIP, err := m.nodeIsAssigned(newNode, controlPlaneEndpoint)
						if err != nil {
							klog.Errorf("failed when checking if node has the eip assignment: %v", err)
							return
						}

						selfFilter := func(nodes []*v1.Node) []*v1.Node {
							return tryFilterSelf(newNode, nodes)
						}

						if hasIP || (len(controlPlaneEndpoint.Assignments) == 0) {
							klog.Info("node has transitioned to unschedulable and is assigned the EIP, trying to migrate EIP proactively")
							if err := m.tryReassign(ctx, controlPlaneEndpoint, filterDeletingNodes, tryFilterUnschedulableNodes, selfFilter); err != nil {
								klog.Errorf("failed to reassign eip assignment: %v", err)
							}
						}
					}
				},
				DeleteFunc: func(obj interface{}) {
					m.assignmentMutex.Lock()
					defer m.assignmentMutex.Unlock()

					n, _ := obj.(*v1.Node)
					klog.Infof("handling delete, node: %s", n.Name)

					controlPlaneEndpoint, err := m.getControlPlaneEndpointReservation()
					if err != nil {
						klog.Errorf("failed to get the control plane endpoint for the cluster: %v", err)
						return
					}

					hasIP, err := m.nodeIsAssigned(n, controlPlaneEndpoint)
					if err != nil {
						klog.Errorf("failed when checking if node has the eip assignment: %v", err)
						return
					}

					selfFilter := func(nodes []*v1.Node) []*v1.Node {
						return tryFilterSelf(n, nodes)
					}

					if hasIP || (len(controlPlaneEndpoint.Assignments) == 0) {
						klog.Info("node is being deleted and is assigned the EIP, trying to migrate EIP proactively")
						if err := m.tryReassign(ctx, controlPlaneEndpoint, filterDeletingNodes, tryFilterUnschedulableNodes, selfFilter); err != nil {
							klog.Errorf("failed to reassign eip assignment: %v", err)
						}
					}
				},
			},
		},
	)

	sharedInformerFactory.Core().V1().Endpoints().Informer().AddEventHandler(
		cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				e, _ := obj.(*v1.Endpoints)
				if e.Namespace != metav1.NamespaceDefault && e.Name != "kubernetes" {
					return false
				}

				if m.eipTag == "" {
					klog.Info("controlplane endpoint manager: elastic ip tag is empty. Nothing to do")
					return false
				}

				return true
			},
			Handler: cache.ResourceEventHandlerFuncs{
				AddFunc: func(obj interface{}) {
					k8sEndpoints, _ := obj.(*v1.Endpoints)
					klog.Infof("handling add, endpoints: %s/%s", k8sEndpoints.Namespace, k8sEndpoints.Name)

					if err := m.syncEndpoints(context.TODO(), k8sEndpoints); err != nil {
						klog.Errorf("failed to sync endpoints from default/kubernetes to %s/%s: %v", externalServiceNamespace, externalServiceName, err)
						return
					}
				},
				UpdateFunc: func(_, obj interface{}) {
					k8sEndpoints, _ := obj.(*v1.Endpoints)
					klog.Infof("handling update, endpoints: %s/%s", k8sEndpoints.Namespace, k8sEndpoints.Name)

					if err := m.syncEndpoints(context.TODO(), k8sEndpoints); err != nil {
						klog.Errorf("failed to sync endpoints from default/kubernetes to %s/%s: %v", externalServiceNamespace, externalServiceName, err)
						return
					}
				},
			},
		},
	)

	sharedInformerFactory.Core().V1().Services().Informer().AddEventHandler(
		cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				s, _ := obj.(*v1.Service)
				if s.Namespace != metav1.NamespaceDefault && s.Name != "kubernetes" {
					return false
				}

				if m.eipTag == "" {
					klog.Info("controlplane endpoint manager: elastic ip tag is empty. Nothing to do")
					return false
				}

				return true
			},
			Handler: cache.ResourceEventHandlerFuncs{
				AddFunc: func(obj interface{}) {
					k8sService, _ := obj.(*v1.Service)
					klog.Infof("handling add, service: %s/%s", k8sService.Namespace, k8sService.Name)

					if err := m.syncService(context.TODO(), k8sService); err != nil {
						klog.Errorf("failed to sync service from default/kubernetes to %s/%s: %v", externalServiceNamespace, externalServiceName, err)
						return
					}
				},
				UpdateFunc: func(_, obj interface{}) {
					k8sService, _ := obj.(*v1.Service)
					klog.Infof("handling update, service: %s/%s", k8sService.Namespace, k8sService.Name)

					if err := m.syncService(context.TODO(), k8sService); err != nil {
						klog.Errorf("failed to sync service from default/kubernetes to %s/%s: %v", externalServiceNamespace, externalServiceName, err)
						return
					}
				},
			},
		},
	)

	klog.V(2).Info("controlPlaneEndpointManager.init(): enabling BGP on project")
	return nil
}

func (m *ControlPlaneEndpointManager) syncEndpoints(ctx context.Context, k8sEndpoints *v1.Endpoints) error {
	m.endpointsMutex.Lock()
	defer m.endpointsMutex.Unlock()

	eps, err := m.sharedInformerFactory.Core().V1().Endpoints().Lister().Endpoints(externalServiceNamespace).Get(externalServiceName)

	switch {
	case apierrors.IsNotFound(err):
		externalEndpoints := &v1.Endpoints{
			ObjectMeta: metav1.ObjectMeta{
				Name:      externalServiceName,
				Namespace: externalServiceNamespace,
			},
			Subsets: make([]v1.EndpointSubset, 0, len(k8sEndpoints.Subsets)),
		}

		for _, subset := range k8sEndpoints.Subsets {
			externalEndpoints.Subsets = append(externalEndpoints.Subsets, *subset.DeepCopy())
		}

		_, err := m.clientSet.CoreV1().Endpoints(externalServiceNamespace).Create(ctx, externalEndpoints, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create endpoint %s/%s: %w", externalServiceNamespace, externalServiceName, err)
		}

		return nil
	case err != nil:
		return fmt.Errorf("failed to get endpoint %s/%s: %w", externalServiceNamespace, externalServiceName, err)
	default:
		externalEndpoints := eps.DeepCopy()
		externalEndpoints.Subsets = make([]v1.EndpointSubset, 0, len(k8sEndpoints.Subsets))

		for _, subset := range k8sEndpoints.Subsets {
			externalEndpoints.Subsets = append(externalEndpoints.Subsets, *subset.DeepCopy())
		}

		// TODO: replace with Apply
		_, err := m.clientSet.CoreV1().Endpoints(externalServiceNamespace).Update(ctx, externalEndpoints, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update endpoint %s/%s: %w", externalServiceNamespace, externalServiceName, err)
		}

		return nil
	}
}

func (m *ControlPlaneEndpointManager) syncService(ctx context.Context, k8sService *v1.Service) error {
	m.serviceMutex.Lock()
	defer m.serviceMutex.Unlock()

	// get the target port
	existingPorts := k8sService.Spec.Ports
	if len(existingPorts) < 1 {
		return errors.New("default/kubernetes service does not have any ports defined")
	}

	// track which port the kube-apiserver actually is listening on
	m.nodeAPIServerPort = existingPorts[0].TargetPort.IntVal
	// did we set a specific port, or did we request that it just be left as is?
	if m.apiServerPort == 0 {
		m.apiServerPort = m.nodeAPIServerPort
	}

	controlPlaneEndpoint, err := m.getControlPlaneEndpointReservation()
	if err != nil {
		return fmt.Errorf("failed to get the control plane endpoint for the cluster: %w", err)
	}

	// for ease of use
	eip := controlPlaneEndpoint.Address

	var updatedService *v1.Service

	svc, err := m.sharedInformerFactory.Core().V1().Services().Lister().Services(externalServiceNamespace).Get(externalServiceName)
	switch {
	case apierrors.IsNotFound(err):
		externalService := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name: externalServiceName,
				Annotations: map[string]string{
					metallbAnnotation: metallbDisabledtag,
				},
				Namespace: externalServiceNamespace,
			},
			Spec: v1.ServiceSpec{
				Type:           v1.ServiceTypeLoadBalancer,
				LoadBalancerIP: eip,
				Ports:          make([]v1.ServicePort, 0, len(k8sService.Spec.Ports)),
			},
		}

		for _, port := range k8sService.Spec.Ports {
			externalService.Spec.Ports = append(externalService.Spec.Ports, *port.DeepCopy())
		}
		externalService.Spec.Ports[0].Port = m.apiServerPort

		var err error
		updatedService, err = m.clientSet.CoreV1().Services(externalServiceNamespace).Create(ctx, externalService, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create endpoint %s/%s: %w", externalServiceNamespace, externalServiceName, err)
		}
	case err != nil:
		return fmt.Errorf("failed to get endpoint %s/%s: %w", externalServiceNamespace, externalServiceName, err)
	default:
		externalService := svc.DeepCopy()
		externalService.Spec.Ports = make([]v1.ServicePort, 0, len(k8sService.Spec.Ports))

		for _, port := range k8sService.Spec.Ports {
			externalService.Spec.Ports = append(externalService.Spec.Ports, *port.DeepCopy())
		}
		externalService.Spec.Ports[0].Port = m.apiServerPort

		// TODO: replace with Apply
		var err error
		updatedService, err = m.clientSet.CoreV1().Services(externalServiceNamespace).Update(ctx, externalService, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update endpoint %s/%s: %w", externalServiceNamespace, externalServiceName, err)
		}
	}

	updatedService.Status = v1.ServiceStatus{
		LoadBalancer: v1.LoadBalancerStatus{
			Ingress: []v1.LoadBalancerIngress{
				{IP: eip},
			},
		},
	}

	updatedService2, err := m.clientSet.CoreV1().Services(externalServiceNamespace).UpdateStatus(context.TODO(), updatedService, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update service status: %w", err)
	}

	klog.V(5).Infof("updated service after status update: %#v", updatedService2)

	return nil
}

func (m *ControlPlaneEndpointManager) tryDefaultAPIServerPorts() error {
	s, err := m.sharedInformerFactory.Core().V1().Services().Lister().Services(metav1.NamespaceDefault).Get("kubernetes")
	if err != nil {
		return fmt.Errorf("failed to get default/kubernetes service: %w", err)
	}

	// get the target port
	existingPorts := s.Spec.Ports
	if len(existingPorts) < 1 {
		return errors.New("default/kubernetes service does not have any ports defined")
	}

	// track which port the kube-apiserver actually is listening on
	m.nodeAPIServerPort = existingPorts[0].TargetPort.IntVal
	// did we set a specific port, or did we request that it just be left as is?
	if m.apiServerPort == 0 {
		m.apiServerPort = m.nodeAPIServerPort
	}

	return nil
}

func (m *ControlPlaneEndpointManager) nodeFilter(n *v1.Node) bool {
	if !isControlPlaneNode(n) {
		return false
	}

	if m.apiServerPort == 0 {
		klog.Errorf("control plane apiserver port not provided or determined, skipping: %s", n.Name)
		return false
	}

	if m.eipTag == "" {
		klog.Info("control plane loadbalancer elastic ip tag is empty, skipping: %s", n.Name)
		return false
	}

	return true
}

func (m *ControlPlaneEndpointManager) getControlPlaneEndpointReservation() (*packngo.IPAddressReservation, error) {
	ipList, _, err := m.client.ProjectIPs.List(m.projectID, &packngo.ListOptions{
		Includes: []string{"assignments"},
	})
	if err != nil {
		return nil, err
	}

	controlPlaneEndpoint := svc.IPReservationByAllTags([]string{m.eipTag}, ipList)
	if controlPlaneEndpoint == nil {
		// IP NOT FOUND nothing to do here.
		return nil, fmt.Errorf("elastic IP not found. Please verify you have one with the expected tag: %s", m.eipTag)
	}

	if len(controlPlaneEndpoint.Assignments) > 1 {
		return nil, fmt.Errorf("the elastic ip %s has more than one node assigned to it and this is currently not supported. Fix it manually unassigning devices", controlPlaneEndpoint.ID)
	}

	return controlPlaneEndpoint, nil
}

func (m *ControlPlaneEndpointManager) nodeIsAssigned(node *v1.Node, ipReservation *packngo.IPAddressReservation) (bool, error) {
	for _, a := range ipReservation.Assignments {
		device, err := svc.DeviceByNode(m.client, m.projectID, node)
		if err != nil {
			return false, err
		}

		for _, ip := range device.Network {
			if ip.ID == a.ID {
				return true, nil
			}
		}
	}

	return false, nil
}

func isControlPlaneNode(node *v1.Node) bool {
	if metav1.HasLabel(node.ObjectMeta, controlPlaneLabel) || metav1.HasLabel(node.ObjectMeta, deprecatedControlPlaneLabel) {
		return true
	}

	return false
}

// Anything calling this function should be wrapped by a lock on m.assignmentMutex
func (m *ControlPlaneEndpointManager) reassign(ctx context.Context, nodes []*v1.Node, ip *packngo.IPAddressReservation, eipURL string) error {
	klog.V(2).Info("controlPlaneEndpoint.reassign")
	// must have figured out the node port first, or nothing to do
	if m.nodeAPIServerPort == 0 {
		return errors.New("control plane node apiserver port not yet determined, cannot reassign, will try again on next loop")
	}

	// TODO: sort nodes, with deleted last, unschedulable in the middle, schedulable first, each ordered by creation time, newest first
	for _, node := range nodes {
		device, err := svc.DeviceByNode(m.client, m.projectID, node)
		if err != nil {
			return err
		}

		nodeAddresses, err := svc.NodeAddresses(device)
		if err != nil {
			return err
		}

		// I decided to iterate over all the addresses assigned to the node to avoid network misconfiguration
		// The first one for example is the node name, and if the hostname is not well configured it will never work.
		for _, a := range nodeAddresses {
			if a.Type == "Hostname" {
				klog.V(2).Infof("skipping address check of type %s: %s", a.Type, a.Address)
				continue
			}
			healthCheckAddress := fmt.Sprintf("https://%s:%d/healthz", a.Address, m.nodeAPIServerPort)
			if healthCheckAddress == eipURL {
				klog.V(2).Infof("skipping address check for EIP on this node: %s", eipURL)
				continue
			}
			klog.Infof("healthcheck node %s", healthCheckAddress)
			req, err := http.NewRequest("GET", healthCheckAddress, nil)
			if err != nil {
				klog.Errorf("healthcheck failed for node %s. err \"%s\"", node.Name, err)
				continue
			}
			resp, err := m.httpClient.Do(req)

			if err != nil {
				if err != nil {
					klog.Errorf("http client error during healthcheck. err \"%s\"", err)
				}
				continue
			}

			// We have a healthy node, this is the candidate to receive the EIP
			if resp.StatusCode == http.StatusOK {
				deviceID, err := svc.DeviceIDFromProviderID(svc.ProviderIDFromDevice(device))
				if err != nil {
					return err
				}
				if len(ip.Assignments) == 1 {
					if _, err := m.client.DeviceIPs.Unassign(ip.Assignments[0].ID); err != nil {
						return err
					}
				}
				if _, _, err := m.client.DeviceIPs.Assign(deviceID, &packngo.AddressStruct{
					Address: ip.Address,
				}); err != nil {
					return err
				}
				klog.Infof("control plane endpoint assigned to new device %s", node.Name)
				return nil
			}
			klog.Infof("will not assign control plane endpoint to new device %s: returned http code %d", node.Name, resp.StatusCode)
		}
	}
	return errors.New("ccm didn't find a good candidate for IP allocation. Cluster is unhealthy")
}

func (m *ControlPlaneEndpointManager) timerLoop(ctx context.Context) {
	m.sharedInformerFactory.WaitForCacheSync(ctx.Done())

	klog.V(5).Infof("timerLoop(): starting loop")

	for {
		select {
		case <-time.After(checkLoopTimerSeconds * time.Second):
			klog.V(5).Infof("timerLoop(): timer triggered")
			if err := m.performHealthCheck(ctx); err != nil {
				klog.Errorf("failed to perform periodic health check")
			}
		case <-ctx.Done():
			return
		}
	}
}

func (m *ControlPlaneEndpointManager) healthURLFromControlPlaneEndpoint(controlPlaneEndpoint *packngo.IPAddressReservation) string {
	return fmt.Sprintf("https://%s:%d/healthz", controlPlaneEndpoint.Address, m.apiServerPort)
}

func filterDeletingNodes(nodes []*v1.Node) []*v1.Node {
	var liveNodes []*v1.Node
	for i := range nodes {
		if nodes[i].DeletionTimestamp.IsZero() {
			liveNodes = append(liveNodes, nodes[i])
		}
	}

	return liveNodes
}

func tryFilterUnschedulableNodes(nodes []*v1.Node) []*v1.Node {
	var schedulableNodes []*v1.Node
	for i := range nodes {
		if nodes[i].Spec.Unschedulable {
			continue
		}

		schedulableNodes = append(schedulableNodes, nodes[i])
	}

	if len(schedulableNodes) > 0 {
		return schedulableNodes
	}

	return nodes
}

func tryFilterSelf(self *v1.Node, nodes []*v1.Node) []*v1.Node {
	var remainingNodes []*v1.Node

	for i := range nodes {
		if nodes[i].Name != self.Name {
			remainingNodes = append(remainingNodes, nodes[i])
		}
	}

	if len(remainingNodes) > 0 {
		return remainingNodes
	}

	return nodes
}

type nodeFitler func([]*v1.Node) []*v1.Node

// Anything calling this function should be wrapped by a lock on m.assignmentMutex
func (m *ControlPlaneEndpointManager) tryReassignAllAvailable(ctx context.Context, controlPlaneEndpoint *packngo.IPAddressReservation) error {
	return m.tryReassign(ctx, controlPlaneEndpoint, filterDeletingNodes, tryFilterUnschedulableNodes)
}

// Anything calling this function should be wrapped by a lock on m.assignmentMutex
func (m *ControlPlaneEndpointManager) tryReassign(ctx context.Context, controlPlaneEndpoint *packngo.IPAddressReservation, filters ...nodeFitler) error {
	controlPlaneHealthURL := m.healthURLFromControlPlaneEndpoint(controlPlaneEndpoint)
	nodesLister := m.sharedInformerFactory.Core().V1().Nodes().Lister()

	klog.V(5).Infof("tryReassignAllAvailable(): listing nodes with controlplane selector")
	controlPlaneNodes, err := nodesLister.List(m.controlPlaneLabelSelector)
	if err != nil {
		return fmt.Errorf("failed to list control plane nodes having the control plane label: %w", err)
	}

	klog.V(5).Infof("tryReassignAllAvailable(): listing nodes with deprecated controlplane selector")
	nodesWithDeprecatedLabel, err := nodesLister.List(m.deprecatedControlPlaneSelector)
	if err != nil {
		return fmt.Errorf("failed to list control plane nodes having the deprecated control plane label: %w", err)
	}

	controlPlaneNodes = append(controlPlaneNodes, nodesWithDeprecatedLabel...)
	klog.V(5).Infof("tryReassignAllAvailable(): found controlplane nodes: %#v", controlPlaneNodes)

	potentialNodes := controlPlaneNodes

	for _, f := range filters {
		potentialNodes = f(controlPlaneNodes)
	}

	if err := m.reassign(ctx, potentialNodes, controlPlaneEndpoint, controlPlaneHealthURL); err != nil {
		return fmt.Errorf("failed to assign the control plane endpoint: %w", err)
	}

	return nil
}

func (m *ControlPlaneEndpointManager) performHealthCheck(ctx context.Context) error {
	klog.V(5).Infof("performHealthCheck(): performing health check")

	klog.V(5).Infof("performHealthCheck(): trying to acquire assignmentMutex lock")
	m.assignmentMutex.Lock()

	defer func() {
		klog.V(5).Infof("performHealthCheck(): releasing assignmentMutex lock")
		m.assignmentMutex.Unlock()
	}()

	klog.V(5).Infof("performHealthCheck(): assignmentMutex lock acquired")

	controlPlaneEndpoint, err := m.getControlPlaneEndpointReservation()
	if err != nil {
		return fmt.Errorf("failed to get the control plane endpoint for the cluster: %w", err)
	}

	if len(controlPlaneEndpoint.Assignments) == 0 {
		klog.Info("performHealthCheck(): no control plane IP assignment found, trying to assign to an available controlplane node")

		return m.tryReassignAllAvailable(ctx, controlPlaneEndpoint)
	}

	controlPlaneHealthURL := m.healthURLFromControlPlaneEndpoint(controlPlaneEndpoint)
	klog.Infof("performHealthCheck(): checking control plane health through elastic ip %s", controlPlaneHealthURL)

	req, err := http.NewRequest("GET", controlPlaneHealthURL, nil)
	// we should not have an error constructing the request
	if err != nil {
		return fmt.Errorf("error constructing GET request for %s: %w", controlPlaneHealthURL, err)
	}

	resp, err := m.httpClient.Do(req)
	// if there was no error, ensure we close
	if err == nil && resp.Body != nil {
		defer resp.Body.Close()
	}

	if err != nil || resp.StatusCode != http.StatusOK {
		if err != nil {
			klog.Errorf("http client error during healthcheck, will try to reassign to a healthy node. err \"%s\"", err)
		}

		klog.Info("performHealthCheck(): health check through elastic ip failed, trying to reassign to an available controlplane node")
		return m.tryReassignAllAvailable(ctx, controlPlaneEndpoint)
	}

	return nil
}
