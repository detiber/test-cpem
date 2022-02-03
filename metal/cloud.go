package metal

import (
	"context"
	"fmt"
	"io"

	"github.com/packethost/packngo"
	"k8s.io/client-go/informers"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/component-base/version"
	"k8s.io/klog/v2"

	"github.com/equinix/cloud-provider-equinix-metal/metal/svc/device"
	"github.com/equinix/cloud-provider-equinix-metal/metal/svc/eipcontrolplane"
)

const (
	ProviderName string = "equinixmetal"
)

func init() {
	cloudprovider.RegisterCloudProvider(ProviderName, func(config io.Reader) (cloudprovider.Interface, error) {
		// by the time we get here, there is no error, as it would have been handled earlier
		metalConfig, err := getMetalConfig(config)
		// register the provider
		if err != nil {
			return nil, fmt.Errorf("provider config error: %v", err)
		}

		// report the config
		printMetalConfig(metalConfig)

		// set up our client and create the cloud interface
		client := packngo.NewClientWithAuth("cloud-provider-equinix-metal", metalConfig.AuthToken, nil)
		client.UserAgent = fmt.Sprintf("cloud-provider-equinix-metal/%s %s", version.Get(), client.UserAgent)
		cloud, err := newCloud(metalConfig, client)
		if err != nil {
			return nil, fmt.Errorf("failed to create new cloud handler: %v", err)
		}

		return cloud, nil
	})
}

type cloud struct {
	client                  *packngo.Client
	metalConfig             Config
	instancesV2             *device.InstancesV2
	controlPlaneEndpointMgr *eipcontrolplane.ControlPlaneEndpointManager
}

func newCloud(metalConfig Config, client *packngo.Client) (cloudprovider.Interface, error) {
	return &cloud{
		client:                  client,
		metalConfig:             metalConfig,
		instancesV2:             device.NewInstancesV2(client, metalConfig.ProjectID, metalConfig.AnnotationNetworkIPv4Private),
		controlPlaneEndpointMgr: eipcontrolplane.NewControlPlaneEndpointManager(metalConfig.EIPTag, metalConfig.ProjectID, client, metalConfig.APIServerPort),
	}, nil
}

// Initialize provides the cloud with a kubernetes client builder and may spawn goroutines
// to perform housekeeping activities within the cloud provider.
func (c *cloud) Initialize(clientBuilder cloudprovider.ControllerClientBuilder, stop <-chan struct{}) {
	klog.V(5).Info("called Initialize")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-stop
		cancel()
	}()

	clientset := clientBuilder.ClientOrDie("cloud-provider-equinix-metal-shared-informers")
	sharedInformer := informers.NewSharedInformerFactory(clientset, 0)

	if err := c.controlPlaneEndpointMgr.Init(ctx, clientset, sharedInformer); err != nil {
		klog.Fatalf("could not initialize %s: %v", c.controlPlaneEndpointMgr.Name, err)
	}

	sharedInformer.Start(ctx.Done())
	sharedInformer.WaitForCacheSync(ctx.Done())
}

// InstancesV2 returns an implementation of cloudprovider.InstancesV2.
func (c *cloud) InstancesV2() (cloudprovider.InstancesV2, bool) {
	klog.V(5).Info("called InstancesV2")
	return c.instancesV2, true
}

// ProviderName returns the cloud provider ID.
func (c *cloud) ProviderName() string {
	klog.V(2).Infof("called ProviderName, returning %s", ProviderName)
	return ProviderName
}

// HasClusterID returns true if a ClusterID is required and set
func (c *cloud) HasClusterID() bool {
	klog.V(5).Info("called HasClusterID")
	return true
}

// LoadBalancer returns a balancer interface. Also returns true if the interface is supported, false otherwise.
// TODO unimplemented
func (c *cloud) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	klog.V(5).Info("called LoadBalancer")
	return nil, false
}

// Instances returns an instances interface. Also returns true if the interface is supported, false otherwise.
func (c *cloud) Instances() (cloudprovider.Instances, bool) {
	klog.V(5).Info("called Instances")
	return nil, false
}

// Zones returns a zones interface. Also returns true if the interface is supported, false otherwise.
func (c *cloud) Zones() (cloudprovider.Zones, bool) {
	klog.V(5).Info("called Zones")
	return nil, false
}

// Clusters returns a clusters interface.  Also returns true if the interface is supported, false otherwise.
func (c *cloud) Clusters() (cloudprovider.Clusters, bool) {
	klog.V(5).Info("called Clusters")
	return nil, false
}

// Routes returns a routes interface along with whether the interface is supported.
func (c *cloud) Routes() (cloudprovider.Routes, bool) {
	klog.V(5).Info("called Routes")
	return nil, false
}
