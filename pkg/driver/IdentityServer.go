package driver

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	// codes "google.golang.org/grpc/codes"
	// status "google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func (drv *Driver) GetPluginInfo(ctx context.Context, r *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	drv.logentry.Infoln("Identity GetPluginInfo is called")
	return &csi.GetPluginInfoResponse{
		Name: drv.name,
		// Endpoint: drv.endpoint,
		VendorVersion: "v1.1",
	}, nil
}
func (drv *Driver) GetPluginCapabilities(ctx context.Context, r *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	drv.logentry.Infoln("Identity GetPluginCapabilities is called")
	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{
						Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
					},
				},
			},
			{
				// NodeGetInfo 返回拓扑信息时，必须声明此能力。
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{
						Type: csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS,
					},
				},
			},
		},
	}, nil
}
func (drv *Driver) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	drv.logentry.Infoln("Identity Probe is called")

	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}
