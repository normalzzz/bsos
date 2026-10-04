package driver

import (
	"context"
	"strings"
	"os"
	exec "os/exec"

	"github.com/container-storage-interface/spec/lib/go/csi"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
	"fmt"
	
)

func (drv *Driver) NodeStageVolume(ctx context.Context, r *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	drv.logentry.Infoln("Node NodeStageVolume is called")

	// check the request parameters
	if r.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID is required")
	}
	if r.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "Staging target path is required")
	}
	if r.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability is required")
	}

	switch r.GetVolumeCapability().GetAccessType().(type) {
	case *csi.VolumeCapability_Block:
		return &csi.NodeStageVolumeResponse{}, nil
	}

	// mount the volume to staging target path

	sourcepath := r.GetPublishContext()[DevicePathKey]
	targetpath := r.GetStagingTargetPath()

	drv.logentry.Infof("NodeStageVolume: mount %s to %s", sourcepath, targetpath)

	// check if the source and target path exist
	if err := exec.Command("ls", "-l", sourcepath).Run(); err != nil {
		drv.logentry.WithError(err).Errorln("source path does not exist")
		return nil, status.Errorf(codes.Internal, "source path %s does not exist: %v, please validate the volume device path", sourcepath, err)
	}

	if err := exec.Command("ls", "-l", targetpath).Run(); err != nil {
		drv.logentry.WithError(err).Errorln("target path does not exist")
		err := exec.Command("mkdir", "-p", targetpath).Run()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to create target path %s: %v", targetpath, err)
		}
	}

	// Probe the device directly; never overwrite an existing filesystem or partition table.
	output, err := exec.CommandContext(ctx, "blkid", "-p", "-o", "export", sourcepath).CombinedOutput()
	if ctx.Err() != nil {
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	fsType := ""
	if err != nil {
		// blkid returns 2 when no signature is found. Other failures must not trigger formatting.
		exitErr, ok := err.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != 2 || strings.TrimSpace(string(output)) != "" {
			return nil, status.Errorf(codes.Internal, "failed to inspect device %s: %v, output: %s", sourcepath, err, strings.TrimSpace(string(output)))
		}
		if output, err := exec.CommandContext(ctx, "mkfs.ext4", sourcepath).CombinedOutput(); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to format device %s as ext4: %v, output: %s", sourcepath, err, strings.TrimSpace(string(output)))
		}
	} else {
		for _, line := range strings.Split(string(output), "\n") {
			if strings.HasPrefix(line, "PTTYPE=") {
				return nil, status.Errorf(codes.FailedPrecondition, "device %s contains a partition table; refusing to format or mount the whole device", sourcepath)
			}
			if strings.HasPrefix(line, "TYPE=") {
				fsType = strings.TrimSpace(strings.TrimPrefix(line, "TYPE="))
			}
		}
		if fsType == "" {
			return nil, status.Errorf(codes.Internal, "blkid succeeded but returned no filesystem type for device %s; ensure util-linux blkid is installed; output: %s", sourcepath, strings.TrimSpace(string(output)))
		}
	}
	if err := drv.mount(sourcepath, targetpath, fsType, nil); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to mount volume: %v", err)
	}
	// check if the volume is already staged
	return &csi.NodeStageVolumeResponse{}, nil
}


func (drv *Driver) mount(source string, target string, fsType string, options []string) error {
	mountCmd := "mount"

	if fsType == "" {
		return fmt.Errorf("fstype is not provided")
	}

	mountArgs := []string{}
	err := os.MkdirAll(target, 0777)
	if err != nil {
		return fmt.Errorf("error: %s, creating the target dir\n", err.Error())
	}
	mountArgs = append(mountArgs, "-t", fsType)

	// check of options and then append them at the end of the mount command
	if len(options) > 0 {
		mountArgs = append(mountArgs, "-o", strings.Join(options, ","))
	}

	mountArgs = append(mountArgs, source)
	mountArgs = append(mountArgs, target)

	out, err := exec.Command(mountCmd, mountArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("error %s, mounting the source %s to tar %s. Output: %s\n", err.Error(), source, target, out)
	}
	return nil

}

func (drv *Driver) NodeUnstageVolume(ctx context.Context, r *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	drv.logentry.Infoln("Node NodeUnstageVolume is called")
	return nil, status.Errorf(codes.Unimplemented, "method NodeUnstageVolume not implemented")
}
func (drv *Driver) NodePublishVolume(ctx context.Context, r *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	drv.logentry.Infoln("Node NodePublishVolume is called")

		options := []string{"bind"}
	if r.Readonly {
		options = append(options, "ro")
	}

	// get req.VolumeCaps and make sure that you handle request for block mode as well
	// here we are just handling request for filesystem mode
	// in case of block mode, the source is going to be the device dir where volume was attached form ControllerPubVolume RPC

	fsType := "ext4"
	if r.VolumeCapability.GetMount().FsType != "" {
		fsType = r.VolumeCapability.GetMount().FsType
	}

	source := r.StagingTargetPath
	target := r.TargetPath

	// we want to run mount -t fstype source target -o bind,ro

	err := drv.mount(source, target, fsType, options)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("Error %s, mounting the volume from staging dir to target dir", err.Error()))
	}

	return &csi.NodePublishVolumeResponse{}, nil
	// return nil, status.Errorf(codes.Unimplemented, "method NodePublishVolume not implemented")
}
func (drv *Driver) NodeUnpublishVolume(ctx context.Context, r *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	drv.logentry.Infoln("Node NodeUnpublishVolume is called")
	return nil, status.Errorf(codes.Unimplemented, "method NodeUnpublishVolume not implemented")
}
func (drv *Driver) NodeGetVolumeStats(ctx context.Context, r *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	drv.logentry.Infoln("Node NodeGetVolumeStats is called")
	return nil, status.Errorf(codes.Unimplemented, "method NodeGetVolumeStats not implemented")
}
func (drv *Driver) NodeGetVolumeHealth(ctx context.Context, r *csi.NodeGetVolumeHealthRequest) (*csi.NodeGetVolumeHealthResponse, error) {
	drv.logentry.Infoln("Node NodeGetVolumeHealth is called")
	return nil, status.Errorf(codes.Unimplemented, "method NodeGetVolumeHealth not implemented")
}
func (drv *Driver) NodeGetStorageHealth(ctx context.Context, r *csi.NodeGetStorageHealthRequest) (*csi.NodeGetStorageHealthResponse, error) {
	drv.logentry.Infoln("Node NodeGetStorageHealth is called")
	return nil, status.Errorf(codes.Unimplemented, "method NodeGetStorageHealth not implemented")
}
func (drv *Driver) NodeExpandVolume(ctx context.Context, r *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	drv.logentry.Infoln("Node NodeExpandVolume is called")
	return nil, status.Errorf(codes.Unimplemented, "method NodeExpandVolume not implemented")
}
func (drv *Driver) NodeGetCapabilities(ctx context.Context, r *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	drv.logentry.Infoln("Node NodeGetCapabilities is called")
	// in this function, we need to advertise that this node server supports STAGE_UNSTAGE_VOLUME: https://github.com/container-storage-interface/spec/blob/master/spec.md#node-service-rpc

	// return nil, status.Errorf(codes.Unimplemented, "method NodeGetCapabilities not implemented")

	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
					},
				},
			},
		},
	}, nil
}
func (drv *Driver) NodeGetInfo(ctx context.Context, r *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	drv.logentry.Infoln("Node NodeGetInfo is called")

	// 从当前 EC2 节点的元数据服务获取实例 ID 和可用区。
	// metadata := imds.New(imds.Options{})
	// doc, err := metadata.GetInstanceIdentityDocument(ctx, &imds.GetInstanceIdentityDocumentInput{})
	// if err != nil {
	// 	return nil, status.Errorf(codes.Internal, "failed to get EC2 instance metadata: %v", err)
	// }
	// if doc.InstanceID == "" || doc.AvailabilityZone == "" {
	// 	return nil, status.Error(codes.Internal, "EC2 instance metadata is missing instance ID or availability zone")
	// }

	// ProviderID 形如 aws:///us-east-1c/i-xxx，最后一段就是实例 ID。
	parts := strings.Split(drv.selfnode.Spec.ProviderID, "/")
	instanceID := parts[len(parts)-1]
	if !strings.HasPrefix(instanceID, "i-") || len(instanceID) <= 2 {
		return nil, status.Error(codes.Internal, "Node ProviderID does not contain an EC2 instance ID")
	}

	return &csi.NodeGetInfoResponse{
		NodeId: instanceID,
		MaxVolumesPerNode: 0,
		AccessibleTopology: &csi.Topology{
			Segments: map[string]string{
				"topology.kubernetes.io/zone": drv.selfnode.Labels["topology.kubernetes.io/zone"],
			},
		},
	}, nil
}
