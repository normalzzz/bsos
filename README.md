# bsos

bsos 是一个用 Go 编写的 AWS EBS CSI 驱动学习项目。代码配有 Kubernetes 部署清单和一组中文教程，说明一个卷怎样从 PVC 申请，经过创建、附加和节点挂载，最后提供给 Pod 使用。

- 代码仓库：https://github.com/normalzzz/bsos
- 信息来源：https://github.com/viveksinghggits/bsos

当前代码用于学习和实验，卷的卸载与回收尚未完成，挂载流程也有需要修正的地方。具体状态和对应代码说明见下文及教程。

## 组件如何配合

驱动名称为 `bsos.normalzzz.csi.dev`。Controller 和 Node 使用同一个 Go 程序，每个进程都提供 Identity、Controller、Node 三个 CSI Service，但分别运行在不同的 Pod 中。

| 部署位置 | 组件 | 负责的工作 |
| --- | --- | --- |
| Controller Deployment | bsos、external-provisioner、external-attacher | 根据 PVC 创建卷，通过 AWS API 将卷附加到目标 EC2 实例 |
| Node DaemonSet | bsos、node-driver-registrar | 向本机 kubelet 注册驱动，处理节点上的设备和挂载目录 |

external-provisioner 调用 `CreateVolume`，再把创建结果写入 PV。external-attacher 根据 `VolumeAttachment` 调用 `ControllerPublishVolume`。附加完成后，kubelet 调用节点插件的 `NodeStageVolume` 和 `NodePublishVolume`，准备应用需要的挂载路径。

示例 Pod 通过 PVC 申请一个 3 GiB 的文件系统卷，并在容器中的 `/data` 目录访问它。应用读写文件时，数据经过节点文件系统和块设备访问 EBS，无需为每次读写调用 CSI 接口。

## 当前实现

| 部分 | 已有代码 |
| --- | --- |
| 服务启动 | 读取配置、初始化 Kubernetes 和 AWS 客户端，通过 Unix socket 提供 gRPC 服务 |
| Identity Service | 返回驱动名称、版本和能力；Probe 固定返回就绪 |
| 创建卷 | 检查请求、换算容量、读取可用区，调用 AWS API 创建 gp3 卷 |
| 附加卷 | 选择设备名、调用 AttachVolume，并等待目标实例的附加状态 |
| 节点信息 | 从 Node 的 ProviderID 提取 EC2 实例 ID，从节点标签读取可用区 |
| 节点挂载 | 探测文件系统、按条件格式化、挂载暂存目录，再为 Pod 执行 bind mount |

尚未完成的部分和已知问题如下：

- `NodeUnpublishVolume`、`NodeUnstageVolume`、`ControllerUnpublishVolume` 和 `DeleteVolume` 仍返回 `Unimplemented`，不能依靠当前代码完成卸载和自动回收。
- 新设备执行 `mkfs.ext4` 成功后，没有设置 `fsType`，本次挂载会因文件系统类型为空而失败。具体代码和修正位置见 doc7。
- Controller 返回的是提交给 EC2 的设备名，节点端尚未实现 NVMe 设备识别，返回路径可能与实际设备路径不同。
- `CreateVolume` 只读取第一个 Requisite 拓扑，没有处理 Preferred；也没有按请求名称查找已创建的卷，重试可能重复创建 EBS 卷。
- 裸块设备流程、重复挂载处理、扩容和快照等功能尚未完成。

测试时还需要检查每次操作的结果。Pod 已部署、日志中出现 RPC 调用，都只能说明流程走到了相应位置。

## 教程

教程按概念、服务启动、节点注册、创建卷、附加和挂载的顺序展开。每篇都结合当前代码说明字段来源和调用关系。

| 文档 | 主题 | 地址 |
| --- | --- | --- |
| doc1 | CSI 简介与部署方式 | https://github.com/normalzzz/bsos/blob/main/doc/doc1.md |
| doc2 | CSI 的三个 Service 与卷生命周期 | https://github.com/normalzzz/bsos/blob/main/doc/doc2.md |
| doc3 | Go CSI Driver 的启动与能力发现 | https://github.com/normalzzz/bsos/blob/main/doc/doc3.md |
| doc4 | 节点插件注册与 CSINode | https://github.com/normalzzz/bsos/blob/main/doc/doc4.md |
| doc5 | 动态供应与 CreateVolume | https://github.com/normalzzz/bsos/blob/main/doc/doc5.md |
| doc6 | 卷附加与 VolumeAttachment | https://github.com/normalzzz/bsos/blob/main/doc/doc6.md |
| doc7 | 节点上的文件系统与挂载 | https://github.com/normalzzz/bsos/blob/main/doc/doc7.md |
| doc8 | 测试步骤与结果记录 | https://github.com/normalzzz/bsos/blob/main/doc/doc8.md |

doc8 提供测试步骤和状态判断依据，实际日志、截图及测试结果需要在自己的集群中记录。

## 目录

```text
main.go                         读取配置并启动驱动
pkg/driver/Driver.go             驱动结构、socket 与 gRPC Server
pkg/driver/IdentityServer.go     身份和插件能力查询
pkg/driver/ControllerServer.go   EBS 卷的创建与附加
pkg/driver/NodeServer.go         节点信息、文件系统与挂载
manifest/                       Controller、Node 和 RBAC 清单
examples/                       StorageClass、PVC 和应用 Pod
doc/                            中文教程
Dockerfile                      驱动镜像构建
```

根目录的 `request.json`、`CreateVolume_call.json`、`ControllerPublishVolume_call.json` 和 `request.txt` 是经过脱敏的请求与日志样例。`CreateVolume_call.json` 包含一个请求数组，阅读或调用时需要取出单个对象；其中未携带拓扑的请求会被当前驱动拒绝。

## 部署前的配置

本地编译需要 Go 1.26.0 或更高版本，具体依赖见 `go.mod`。集群实验需要运行在 EC2 Linux 节点上的 Kubernetes、可用的 kubectl 和镜像构建工具，以及允许所需 EC2/EBS 操作的 AWS 身份。

当前清单使用示例值，部署前需要逐项调整：

| 配置 | 位置 | 需要调整的内容 |
| --- | --- | --- |
| 镜像地址 | `manifest/deployment.yaml`、`manifest/node-plugin.yaml` | 将所有 `registry.example.com` 地址替换为集群可拉取的驱动与 sidecar 镜像 |
| AWS 身份 | `manifest/rbac.yaml`、`manifest/rbac-node.yaml` 及运行环境 | IAM ARN 使用全零示例账号，需要替换实际分区、账号和角色；使用其他身份方式时调整或移除角色注解 |
| AWS 区域 | 两份工作负载清单中驱动容器的 `env` | 补充实际 `AWS_REGION`，当前清单没有设置这个变量 |
| 节点信息 | Kubernetes Node | 确认 `spec.providerID` 包含 EC2 实例 ID，`topology.kubernetes.io/zone` 标签正确 |
| 可用区选择 | external-provisioner 的参数 | 多可用区测试先按 doc5 处理拓扑限制；其中给出了 `--strict-topology` 的配置方式 |

`AWS_REGION` 应添加到 Controller 的 `bsos` 容器和 DaemonSet 的 `node-plugin` 容器中。下面是合并到各自 `env` 列表的片段，区域需按环境替换：

```yaml
- name: AWS_REGION
  value: us-east-1
```

程序中的 `--region` 是尚未用于后端操作的旧参数，AWS SDK 实际使用环境变量 `AWS_REGION`。`NODEID` 虽然名为 ID，当前保存的是 Kubernetes 节点名称，由部署清单通过 `spec.nodeName` 注入。`--token` 只为兼容旧命令保留，不用于认证，也不会输出到启动日志。

驱动通过 ServiceAccount 访问 Kubernetes，通过 AWS SDK 获取 AWS 凭据，两类权限需要分别配置。节点插件还需要清单中的宿主机设备挂载、privileged 权限和双向挂载传播，具体用途见 doc4、doc7。

## 构建和部署

下面的命令在仓库根目录执行。先将 `BSOS_IMAGE` 改为自己有推送权限的镜像地址：

```bash
BSOS_IMAGE=registry.example.com/bsos:dev
docker build -t "$BSOS_IMAGE" .
docker push "$BSOS_IMAGE"
```

镜像架构需要与运行节点一致。构建完成后，把 Controller 和 Node 清单中的驱动镜像都改为这个地址，并完成上面的区域、身份和 sidecar 镜像配置，再应用清单：

```bash
kubectl apply -f manifest/rbac.yaml
kubectl apply -f manifest/rbac-node.yaml
kubectl apply -f manifest/deployment.yaml
kubectl apply -f manifest/node-plugin.yaml

kubectl rollout status deployment/bsos -n kube-system
kubectl rollout status daemonset/node-plugin -n kube-system
```

查看驱动 Pod 和节点注册结果：

```bash
kubectl get pods -n kube-system -l app=bsos -o wide
kubectl get pods -n kube-system -l name=node-plugin -o wide
kubectl logs deployment/bsos -n kube-system -c bsos
kubectl get csinodes -o yaml
```

在目标节点的 CSINode 中查找 `spec.drivers` 下的 `bsos.normalzzz.csi.dev` 条目，核对节点 ID。仓库目前没有 `CSIDriver` 清单；它与节点注册产生的 CSINode 用途不同，配置说明和示例见 doc4。

## 测试卷

先按 doc6、doc7 处理设备路径和首次格式化问题，再进行挂载实验。以下命令会申请实际 EBS 卷，应在已配置好的测试环境中执行。

示例使用 `bsos-demo` 命名空间，与教程保持一致：

```bash
kubectl create namespace bsos-demo --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f examples/storageclass.yaml
kubectl apply -n bsos-demo -f examples/pvc.yaml
kubectl apply -n bsos-demo -f examples/pod.yaml

kubectl get pvc,pod -n bsos-demo
kubectl describe pvc bsos-pvc -n bsos-demo
kubectl describe pod bsos-demo -n bsos-demo
```

StorageClass 使用 `WaitForFirstConsumer`，需要有 Pod 使用 PVC 才会触发这组示例的卷供应。PVC 显示 Bound 表示完成卷绑定，之后还要检查附加和节点挂载。

确认应用 Pod 已就绪后，可以检查容器中的挂载目录：

```bash
kubectl exec -n bsos-demo bsos-demo -- df -h /data
```

卷 ID、VolumeAttachment、节点挂载表和文件读写结果需要一起核对，完整操作见 doc8。删除和卸载方法尚未实现，删除 Pod 或 PVC 不能作为后端卷已回收的依据；测试卷的保留和后续处理也见 doc8。

