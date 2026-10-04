package driver

import (
	"context"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"math"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	// "github.com/hashicorp/hcl/v2/json"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
)

func (drv *Driver) CreateVolume(ctx context.Context, r *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	// drv.logentry.Infoln("Controller NodeGetInfo is called")
	drv.logentry.Infoln("Controller CreateVolume is called")

	// 复制请求供日志使用，移除其中的凭据，保留原始请求。
	logRequest := proto.Clone(r).(*csi.CreateVolumeRequest)
	logRequest.Secrets = nil

	requestJSON, err := protojson.Marshal(logRequest)

	if err != nil {
		drv.logentry.WithError(err).Warn("failed to serialize CreateVolume request")
	} else {
		drv.logentry.WithField("request", string(requestJSON)).
			Info("CreateVolume request")
	}
	// validate the CreateVolume reuest, function from ebs csi https://github.com/kubernetes-sigs/aws-ebs-csi-driver/
	if err := validateCreateVolumeRequest(r); err != nil {
		return nil, status.Error(status.Code(err), "failed to validate create volume request")
	}

	// 学习版先取第一个 Requisite 拓扑，键名与 NodeGetInfo 保持一致。
	topologies := r.GetAccessibilityRequirements().GetRequisite()
	if len(topologies) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume availability zone is required")
	}
	zone := topologies[0].GetSegments()["topology.kubernetes.io/zone"]
	if zone == "" {
		return nil, status.Error(codes.InvalidArgument, "topology is missing topology.kubernetes.io/zone")
	}

	// CSI 的容量单位是 bytes，EC2 的 Size 单位是 GiB。
	const gib int64 = 1024 * 1024 * 1024
	sizeBytes := r.GetCapacityRange().GetRequiredBytes()
	limitBytes := r.GetCapacityRange().GetLimitBytes()
	if sizeBytes < 0 || limitBytes < 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capacity must not be negative")
	}
	sizeGiB := sizeBytes / gib
	if sizeBytes%gib != 0 {
		sizeGiB++ // 向上取整，保证容量不小于请求值。
	}
	if sizeGiB == 0 {
		sizeGiB = 1 // 空卷至少分配 1 GiB。
	}
	if sizeGiB > math.MaxInt32 || (limitBytes > 0 && sizeGiB > limitBytes/gib) {
		return nil, status.Error(codes.OutOfRange, "rounded volume size exceeds capacity limit")
	}

	// 先计算容量，再转换成 AWS SDK 要求的指针类型。
	volumeInput := &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(zone),
		Size:             aws.Int32(int32(sizeGiB)),
		VolumeType:       types.VolumeTypeGp3,
	}
	drv.logentry.WithField("volumeInput", volumeInput).Debug("constructed EC2 CreateVolume input")

	createVolumeOutput, err := drv.Ec2client.CreateVolume(ctx, volumeInput)

	if err != nil {
		drv.logentry.WithError(err).Error("failed to create volume")
		return nil, status.Errorf(codes.Internal, "failed to create volume: %v", err)
	}

	// return nil, status.Errorf(codes.Unimplemented, "method CreateVolume not implemented")

	drv.logentry.WithField("createVolumeOutput", createVolumeOutput).Info("volume created successfully")

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      *createVolumeOutput.VolumeId,
			CapacityBytes: sizeGiB * gib,
			VolumeContext: map[string]string{
				"availabilityZone": zone,
			},
			AccessibleTopology: []*csi.Topology{
				{
					Segments: map[string]string{
						"topology.kubernetes.io/zone": zone,
					},
				},
			},
			// ContentSource: r.GetVolumeContentSource(),
		},
	}, nil
}
func (drv *Driver) DeleteVolume(ctx context.Context, r *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	drv.logentry.Infoln("Controller DeleteVolume is called")
	return nil, status.Errorf(codes.Unimplemented, "method DeleteVolume not implemented")
}
func (drv *Driver) ControllerPublishVolume(ctx context.Context, r *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	drv.logentry.Infoln("Controller ControllerPublishVolume is called")
	if r.GetNodeId() == "" || r.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "node ID and volume ID are required")
	}
	logRequest := proto.Clone(r).(*csi.ControllerPublishVolumeRequest)
	logRequest.Secrets = nil

	requestJSON, err := protojson.Marshal(logRequest)

	if err != nil {
		drv.logentry.WithError(err).Warn("failed to serialize ControllerPublishVolumeRequest request")
	} else {
		drv.logentry.WithField("request", string(requestJSON)).
			Info("ControllerPublishVolumeRequest request")
	}

	drv.attachMu.Lock()
	defer drv.attachMu.Unlock()
	if ctx.Err() != nil {
		return nil, status.FromContextError(ctx.Err()).Err()
	}

	// 查询请求指定的 EC2 实例及其设备映射。
	instances, err := drv.Ec2client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{r.GetNodeId()},
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to describe instance: %v", err)
	}
	if len(instances.Reservations) == 0 || len(instances.Reservations[0].Instances) == 0 {
		return nil, status.Error(codes.NotFound, "EC2 instance not found")
	}
	instance := instances.Reservations[0].Instances[0]
	device, alreadyAssigned, err := selectAttachDevice(instance.BlockDeviceMappings, r.GetVolumeId())
	if err != nil {
		return nil, err
	}

	if !alreadyAssigned {
		volumeAttachInput := &ec2.AttachVolumeInput{
			Device:     aws.String(device),
			InstanceId: aws.String(r.GetNodeId()),
			VolumeId:   aws.String(r.GetVolumeId()),
		}
		if _, err := drv.Ec2client.AttachVolume(ctx, volumeAttachInput); err != nil {
			drv.logentry.WithError(err).Error("failed to attach volume")
			return nil, status.Errorf(codes.Internal, "failed to attach volume: %v", err)
		}
	}

	// AttachVolume 返回时可能仍是 attaching，等待目标实例上的 attachment 完成，使用 k8s 的 wait.PollUntilContextTimeout 轮询，最多等待 5 分钟。
	err = wait.PollUntilContextTimeout(ctx, time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		output, err := drv.Ec2client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
			VolumeIds: []string{r.GetVolumeId()},
		})
		if err != nil {
			return false, err
		}
		for _, volume := range output.Volumes {
			for _, attachment := range volume.Attachments {
				if aws.ToString(attachment.InstanceId) == r.GetNodeId() && attachment.State == types.VolumeAttachmentStateAttached {
					return true, nil
				}
			}
		}
		return false, nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return nil, status.Errorf(codes.Internal, "failed to wait for volume attachment: %v", err)
	}

	drv.logentry.WithField("device", device).Info("volume attached successfully")
	pvInfo := map[string]string{DevicePathKey: device}
	return &csi.ControllerPublishVolumeResponse{PublishContext: pvInfo}, nil
}

// selectAttachDevice 复用已有卷的设备名，或从 /dev/sdf 到 /dev/sdp 中选择空闲名称。
func selectAttachDevice(mappings []types.InstanceBlockDeviceMapping, volumeID string) (string, bool, error) {
	used := map[string]bool{}
	for _, mapping := range mappings {
		if mapping.Ebs != nil && mapping.Ebs.Status == types.AttachmentStatusDetached {
			continue
		}
		device := aws.ToString(mapping.DeviceName)
		if mapping.Ebs != nil && aws.ToString(mapping.Ebs.VolumeId) == volumeID {
			if device == "" {
				return "", false, status.Error(codes.Internal, "attached volume has no device name")
			}
			if mapping.Ebs.Status == types.AttachmentStatusDetaching {
				return "", false, status.Error(codes.Aborted, "volume is being detached")
			}
			return device, true, nil
		}
		// /dev/sdf 与 /dev/xvdf 按相同槽位处理，也避开带分区后缀的名称。
		suffix := strings.TrimPrefix(device, "/dev/")
		suffix = strings.TrimPrefix(strings.TrimPrefix(suffix, "xvd"), "sd")
		used[strings.TrimRight(suffix, "0123456789")] = true
	}
	for letter := 'f'; letter <= 'p'; letter++ {
		if !used[string(letter)] {
			return "/dev/sd" + string(letter), false, nil
		}
	}
	return "", false, status.Error(codes.ResourceExhausted, "no free device name in /dev/sdf through /dev/sdp")
}

func (drv *Driver) ControllerUnpublishVolume(ctx context.Context, r *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	drv.logentry.Infoln("Controller ControllerUnpublishVolume is called")
	return nil, status.Errorf(codes.Unimplemented, "method ControllerUnpublishVolume not implemented")
}
func (drv *Driver) ValidateVolumeCapabilities(ctx context.Context, r *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	drv.logentry.Infoln("Controller ValidateVolumeCapabilities is called")
	return nil, status.Errorf(codes.Unimplemented, "method ValidateVolumeCapabilities not implemented")
}
func (drv *Driver) ListVolumes(ctx context.Context, r *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	drv.logentry.Infoln("Controller ListVolumes is called")
	return nil, status.Errorf(codes.Unimplemented, "method ListVolumes not implemented")
}
func (drv *Driver) ControllerListVolumeHealth(ctx context.Context, r *csi.ControllerListVolumeHealthRequest) (*csi.ControllerListVolumeHealthResponse, error) {
	drv.logentry.Infoln("Controller ControllerListVolumeHealth is called")
	return nil, status.Errorf(codes.Unimplemented, "method ControllerListVolumeHealth not implemented")
}
func (drv *Driver) ControllerGetVolumeHealth(ctx context.Context, r *csi.ControllerGetVolumeHealthRequest) (*csi.ControllerGetVolumeHealthResponse, error) {
	drv.logentry.Infoln("Controller ControllerGetVolumeHealth is called")
	return nil, status.Errorf(codes.Unimplemented, "method ControllerGetVolumeHealth not implemented")
}
func (drv *Driver) GetCapacity(ctx context.Context, r *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	drv.logentry.Infoln("Controller GetCapacity is called")
	return nil, status.Errorf(codes.Unimplemented, "method GetCapacity not implemented")
}
func (drv *Driver) ControllerGetCapabilities(ctx context.Context, r *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	drv.logentry.Infoln("Controller ControllerGetCapabilities is called")
	caps := []*csi.ControllerServiceCapability{}

	for _, c := range []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
	} {
		caps = append(caps, &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: c,
				},
			},
		})
	}

	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: caps,
	}, nil
}
func (drv *Driver) CreateSnapshot(ctx context.Context, r *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	drv.logentry.Infoln("Controller CreateSnapshot is called")
	return nil, status.Errorf(codes.Unimplemented, "method CreateSnapshot not implemented")
}
func (drv *Driver) DeleteSnapshot(ctx context.Context, r *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	drv.logentry.Infoln("Controller DeleteSnapshot is called")
	return nil, status.Errorf(codes.Unimplemented, "method DeleteSnapshot not implemented")
}
func (drv *Driver) ListSnapshots(ctx context.Context, r *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	drv.logentry.Infoln("Controller ListSnapshots is called")
	return nil, status.Errorf(codes.Unimplemented, "method ListSnapshots not implemented")
}
func (drv *Driver) GetSnapshot(ctx context.Context, r *csi.GetSnapshotRequest) (*csi.GetSnapshotResponse, error) {
	drv.logentry.Infoln("Controller GetSnapshot is called")
	return nil, status.Errorf(codes.Unimplemented, "method GetSnapshot not implemented")
}
func (drv *Driver) ControllerExpandVolume(ctx context.Context, r *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	drv.logentry.Infoln("Controller ControllerExpandVolume is called")
	return nil, status.Errorf(codes.Unimplemented, "method ControllerExpandVolume not implemented")
}
func (drv *Driver) ControllerGetVolume(ctx context.Context, r *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {
	drv.logentry.Infoln("Controller ControllerGetVolume is called")
	return nil, status.Errorf(codes.Unimplemented, "method ControllerGetVolume not implemented")
}
func (drv *Driver) ControllerModifyVolume(ctx context.Context, r *csi.ControllerModifyVolumeRequest) (*csi.ControllerModifyVolumeResponse, error) {
	drv.logentry.Infoln("Controller ControllerModifyVolume is called")
	return nil, status.Errorf(codes.Unimplemented, "method ControllerModifyVolume not implemented")
}

func validateCreateVolumeRequest(req *csi.CreateVolumeRequest) error {
	volName := req.GetName()
	if len(volName) == 0 {
		return status.Error(codes.InvalidArgument, "Volume name not provided")
	}

	volCaps := req.GetVolumeCapabilities()
	if len(volCaps) == 0 {
		return status.Error(codes.InvalidArgument, "Volume capabilities not provided")
	}

	if !isValidVolumeCapabilities(volCaps) {
		return status.Error(codes.InvalidArgument, "Volume capabilities not supported")
	}

	return nil
}

func isValidVolumeCapabilities(volcap []*csi.VolumeCapability) bool {
	for _, c := range volcap {
		accessMode := c.GetAccessMode().GetMode()

		//nolint:exhaustive
		// 支持单节点挂载或者支持块设备的多节点挂载， 这部分 capacity 有 accessmode 来配置
		switch accessMode {
		case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER:
			return true

		case csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
			if isBlock(c) {
				return true
			} else {
				klog.Infoln("isValidCapability: access mode is only supported for block devices", "accessMode", accessMode)
				return false
			}

		default:
			klog.Infoln("isValidCapability: access mode is not supported", "accessMode", accessMode)
			return false
		}

	}
	return false
}

func isBlock(v *csi.VolumeCapability) bool {
	_, isBlk := v.GetAccessType().(*csi.VolumeCapability_Block)
	return isBlk
}
