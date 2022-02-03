package svc

import (
	"errors"
	"fmt"
	"strings"

	"github.com/packethost/packngo"
	"github.com/packethost/packngo/metadata"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"

	"github.com/equinix/cloud-provider-equinix-metal/metal/constants"
	metalerrors "github.com/equinix/cloud-provider-equinix-metal/metal/errors"
)

func NodeAddresses(device *packngo.Device) ([]v1.NodeAddress, error) {
	var addresses []v1.NodeAddress
	addresses = append(addresses, v1.NodeAddress{Type: v1.NodeHostName, Address: device.Hostname})

	var privateIP, publicIP string
	for _, address := range device.Network {
		if address.AddressFamily == int(metadata.IPv4) {
			var addrType v1.NodeAddressType
			if address.Public {
				publicIP = address.Address
				addrType = v1.NodeExternalIP
			} else {
				privateIP = address.Address
				addrType = v1.NodeInternalIP
			}
			addresses = append(addresses, v1.NodeAddress{Type: addrType, Address: address.Address})
		}
	}

	if privateIP == "" {
		return nil, errors.New("could not get at least one private ip")
	}

	if publicIP == "" {
		return nil, errors.New("could not get at least one public ip")
	}

	return addresses, nil
}

// deviceIDFromProviderID returns a device's ID from providerID.
//
// The providerID spec should be retrievable from the Kubernetes
// node object. The expected format is: equinixmetal://device-id or just device-id
func DeviceIDFromProviderID(providerID string) (string, error) {
	klog.V(2).Infof("called deviceIDFromProviderID with providerID %s", providerID)
	if providerID == "" {
		return "", errors.New("providerID cannot be empty string")
	}

	split := strings.Split(providerID, "://")
	var deviceID string
	switch len(split) {
	case 2:
		deviceID = split[1]
		if split[0] != constants.ProviderName {
			return "", fmt.Errorf("provider name from providerID should be %s, was %s", constants.ProviderName, split[0])
		}
	case 1:
		deviceID = providerID
	default:
		return "", fmt.Errorf("unexpected providerID format: %s, format should be: 'device-id' or 'equinixmetal://device-id'", providerID)
	}

	return deviceID, nil
}

func DeviceByNode(client *packngo.Client, projectID string, node *v1.Node) (*packngo.Device, error) {
	device, err := DeviceByProviderID(client, node.Spec.ProviderID)
	if err != nil {
		return nil, err
	}

	if device != nil {
		return device, nil
	}

	return deviceByName(client, projectID, types.NodeName(node.GetName()))
}

func DeviceByProviderID(client *packngo.Client, providerID string) (*packngo.Device, error) {
	if providerID != "" {
		id, err := DeviceIDFromProviderID(providerID)
		if err != nil {
			return nil, err
		}

		return deviceByID(client, id)
	}

	return nil, nil
}

// ProviderIDFromDevice returns a providerID from a device
func ProviderIDFromDevice(device *packngo.Device) string {
	return fmt.Sprintf("%s://%s", constants.ProviderName, device.ID)
}

// deviceByName returns an instance whose hostname matches the kubernetes node.Name
func deviceByName(client *packngo.Client, projectID string, nodeName types.NodeName) (*packngo.Device, error) {
	klog.V(2).Infof("called deviceByName with projectID %s nodeName %s", projectID, nodeName)
	if string(nodeName) == "" {
		return nil, errors.New("node name cannot be empty string")
	}
	devices, _, err := client.Devices.List(projectID, nil)
	if err != nil {
		return nil, err
	}

	for _, device := range devices {
		if device.Hostname == string(nodeName) {
			klog.V(2).Infof("Found device for nodeName %s", nodeName)
			klog.V(3).Infof("%#v", device)
			return &device, nil
		}
	}

	return nil, cloudprovider.InstanceNotFound
}

func deviceByID(client *packngo.Client, id string) (*packngo.Device, error) {
	klog.V(2).Infof("called deviceByID with ID %s", id)
	device, _, err := client.Devices.Get(id, nil)
	if metalerrors.IsNotFound(err) {
		return nil, cloudprovider.InstanceNotFound
	}
	return device, err
}
