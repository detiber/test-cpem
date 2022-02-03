package device

import (
	"context"

	"github.com/packethost/packngo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	cloudprovider "k8s.io/cloud-provider"

	"github.com/equinix/cloud-provider-equinix-metal/metal/svc"
)

type InstancesV2 struct {
	client            *packngo.Client
	k8sclient         kubernetes.Interface
	project           string
	annotationNetwork string
}

func NewInstancesV2(client *packngo.Client, projectID, annotationNetwork string) *InstancesV2 {
	return &InstancesV2{client: client, project: projectID, annotationNetwork: annotationNetwork}
}

// InstanceMetadata returns instancemetadata for the node according to the cloudprovider
func (i *InstancesV2) InstanceMetadata(ctx context.Context, node *v1.Node) (*cloudprovider.InstanceMetadata, error) {
	device, err := svc.DeviceByNode(i.client, i.project, node)
	if err != nil {
		return nil, err
	}
	nodeAddresses, err := svc.NodeAddresses(device)
	if err != nil {
		// TODO(displague) we error on missing private and public ip. is that restrictive?

		// TODO(displague) should we return the public addresses DNS name as the Type=Hostname NodeAddress type too?
		return nil, err
	}
	var p, r, z string
	if device.Plan != nil {
		p = device.Plan.Slug
	}

	// "A zone represents a logical failure domain"
	// "A region represents a larger domain, made up of one or more zones"
	//
	// Equinix Metal metros are made up of one or more facilities, so we treat
	// metros as K8s topology regions. EM facilities are then equated to zones.
	//
	// https://kubernetes.io/docs/reference/labels-annotations-taints/#topologykubernetesiozone

	if device.Facility != nil {
		z = device.Facility.Code
	}
	if device.Metro != nil {
		r = device.Metro.Code
	}

	return &cloudprovider.InstanceMetadata{
		ProviderID:    svc.ProviderIDFromDevice(device),
		InstanceType:  p,
		NodeAddresses: nodeAddresses,
		Zone:          z,
		Region:        r,
	}, nil
}

// InstanceExists returns true if the node exists in cloudprovider
func (i *InstancesV2) InstanceExists(ctx context.Context, node *v1.Node) (bool, error) {
	_, err := svc.DeviceByNode(i.client, i.project, node)
	switch {
	case err != nil && err == cloudprovider.InstanceNotFound:
		return false, nil
	case err != nil:
		return false, err
	}

	return true, nil

}

// InstanceShutdown returns true if the node is shutdown in cloudprovider
func (i *InstancesV2) InstanceShutdown(ctx context.Context, node *v1.Node) (bool, error) {
	device, err := svc.DeviceByNode(i.client, i.project, node)
	if err != nil {
		return false, err
	}

	return device.State == "inactive", nil
}
