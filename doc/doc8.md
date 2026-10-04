# 测试步骤与结果记录

- 代码仓库：https://github.com/normalzzz/bsos
- 信息来源：https://github.com/viveksinghggits

仓库的 `examples` 目录提供了 StorageClass、PVC 和 Pod。这里使用这组清单检查卷的创建、附加和挂载，再向卷中写入文件，观察数据是否保留。

本文提供测试命令和状态判断依据，实际日志与截图由集群实测后补充。

排查时按卷的生命周期往后看。前一步成功，只能说明这一阶段完成：

| 观察到的结果 | 能说明什么 | 接着检查什么 |
| --- | --- | --- |
| CSINode 中出现驱动条目 | kubelet 已取得并记录节点插件信息 | PVC 是否能创建并绑定卷 |
| PVC 显示 Bound | 存储申请已经对应到一个 PV | 卷是否附加到应用节点 |
| VolumeAttachment 的 attached 为 true | attacher 已记录驱动返回的附加成功结果 | 节点设备与挂载是否正确 |
| Pod 中能读写 `/data` | 应用能够访问当前挂载的文件系统 | 重建后是否仍使用原卷并读到原文件 |

## 测试前的准备

Controller 和节点端的配置分别见《Go CSI Driver 的启动与能力发现》和《节点插件注册与 CSINode》。镜像地址、AWS 区域和 IAM 身份要按测试集群配置。

https://github.com/normalzzz/bsos/blob/main/doc/doc3.md

https://github.com/normalzzz/bsos/blob/main/doc/doc4.md

测试前需要确认以下条件，其中一部分需要先补齐代码：

| 项目 | 测试前需要确认 |
| --- | --- |
| 节点注册 | 目标节点的 CSINode 中有 `bsos.normalzzz.csi.dev`，nodeID 对应正确的 EC2 实例 |
| 可用区 | Node 的 zone 标签正确；多可用区环境需要处理当前仅取 `Requisite[0]` 的限制，可按 doc5 配置 strict topology |
| 设备路径 | 返回的 `devicePath` 在目标节点上存在，并指向本次创建的 EBS 卷；当前代码没有 NVMe 设备解析 |
| 新设备格式化 | doc7 所述格式化成功后未设置 `fsType` 的问题需要修正，不能依赖首次失败后的重试 |
| 回收 | `NodeUnpublishVolume`、`NodeUnstageVolume`、`ControllerUnpublishVolume` 和 `DeleteVolume` 还未实现，卸载和自动回收测试需要先补齐这些方法 |

这组示例按 ext4 文件系统卷使用，不覆盖裸块设备、多节点写入、扩容或快照。

修改代码后，需要让集群运行包含这些修改的镜像。本地 Go 文件的修改不会影响正在运行的驱动，测试时应记录实际镜像和代码版本。

后面的命令按章节顺序执行，`pv_name`、`volume_id`、`node_name` 等变量保存在当前 shell 中，建议使用同一个终端。中途切换终端时，要重新执行相应的取值命令。如果某一步失败，先保留该阶段的事件和日志，解决后再继续依赖它的步骤。

## 检查驱动 Pod

```bash
kubectl get deployment bsos -n kube-system
kubectl get daemonset node-plugin -n kube-system
kubectl get pods -n kube-system -l app=bsos -o wide
kubectl get pods -n kube-system -l name=node-plugin -o wide
kubectl get csinodes
kubectl get nodes -L topology.kubernetes.io/zone
```

Controller Pod 中的驱动、provisioner 和 attacher 都需要正常运行。应用可能调度到的节点上，要有已注册的节点插件。除了 Pod 的 Running 状态，还要检查 socket 调用和注册结果。

## 创建示例 PVC

在仓库根目录执行。这里把应用放在单独的 `bsos-demo` 命名空间，Controller 和节点插件仍位于 `kube-system`：

```bash
kubectl create namespace bsos-demo --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f examples/storageclass.yaml
kubectl apply -n bsos-demo -f examples/pvc.yaml
kubectl get pvc bsos-pvc -n bsos-demo
kubectl describe pvc bsos-pvc -n bsos-demo
```

StorageClass 使用 `WaitForFirstConsumer`。此时还没有使用 PVC 的 Pod，如果 PVC 的事件提示正在等待第一个使用者，那么 Pending 符合这项配置。Pending 本身不能说明原因，权限错误、供应失败等问题也可能让 PVC 停留在这个状态。保留这里的事件，再与创建 Pod 后的事件对照。延迟绑定说明：

https://kubernetes.io/docs/concepts/storage/storage-classes/#volume-binding-mode

## 创建使用卷的 Pod

examples/pod.yaml 中的 BusyBox 容器通过 `/data` 使用 `bsos-pvc`：

https://github.com/normalzzz/bsos/blob/main/examples/pod.yaml

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: bsos-demo
spec:
  containers:
    - name: busybox
      image: public.ecr.aws/docker/library/busybox:1.37
      command: ["sleep", "86400"]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: bsos-pvc
```

应用清单并观察状态：

```bash
kubectl apply -n bsos-demo -f examples/pod.yaml
kubectl get pod bsos-demo -n bsos-demo -o wide
kubectl get pvc bsos-pvc -n bsos-demo
kubectl describe pod bsos-demo -n bsos-demo
kubectl describe pvc bsos-pvc -n bsos-demo
```

从 PVC 事件查看创建卷的进度和供应错误，从 Pod 事件区分调度、附加、挂载和容器启动问题。Pod 的 spec 不要直接设置 nodeName，以免绕过延迟绑定需要的调度流程；需要限制节点时，可以通过 nodeSelector 或节点亲和性让调度器选择符合条件的节点。

接下来读取 PV 的前提是 PVC 已显示 Bound；读取节点信息的前提是 Pod 已获得节点名。Pod 在 ContainerCreating 状态时也可能已经完成调度和卷绑定，因此可以继续检查附加、挂载，而不必等到容器启动后才看这些资源。

## 记录创建出来的卷

PVC 绑定后，`spec.volumeName` 保存对应的 PV 名称。PV 的 `spec.csi.volumeHandle` 又保存驱动创建的 EBS 卷 ID。依次读取这两个字段：

```bash
pv_name=$(kubectl get pvc bsos-pvc -n bsos-demo -o jsonpath='{.spec.volumeName}')
kubectl get pv "$pv_name" -o yaml
volume_id=$(kubectl get pv "$pv_name" -o jsonpath='{.spec.csi.volumeHandle}')
printf 'PV=%s\nEBS=%s\n' "$pv_name" "$volume_id"
```

PV 应使用 `bsos.normalzzz.csi.dev`，容量满足 PVC 的 3 GiB 请求，`volumeHandle` 对应实际 EBS 卷。检查 PV 的 nodeAffinity 和 volumeAttributes 中的可用区，再与应用节点的 zone 对照。

当前示例的 StorageClass 默认使用 Delete 回收策略，而 DeleteVolume 尚未实现。测试期间可以先把本次 PV 改为 Retain，明确保留后端卷供后续检查：

```bash
kubectl patch pv "$pv_name" --type=merge \
  -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}'
```

这条命令只修改本次 PV 的回收策略。Retain 表示 PVC 删除后仍保留后端卷，之后需要另行处理；它不影响正常使用卷，也不会替代卸载和 detach 的实现。PV 回收策略说明：

https://kubernetes.io/docs/concepts/storage/persistent-volumes/#reclaiming

## 记录附加状态

```bash
node_name=$(kubectl get pod bsos-demo -n bsos-demo -o jsonpath='{.spec.nodeName}')
kubectl get csinode "$node_name" -o yaml
kubectl get node "$node_name" -L topology.kubernetes.io/zone
kubectl get volumeattachments \
  -o custom-columns='NAME:.metadata.name,PV:.spec.source.persistentVolumeName,NODE:.spec.nodeName,ATTACHED:.status.attached'
```

在输出中找到 PV 列等于 `pv_name`、NODE 列等于 `node_name` 的条目，把它的 NAME 填入下面的变量，再读取完整状态：

```bash
attachment_name=your-volumeattachment-name
kubectl get volumeattachment "$attachment_name" -o yaml
```

`status.attached` 表示驱动是否已经报告附加成功，`status.attachmentMetadata.devicePath` 是本仓库返回的设备名，`status.attachError` 记录附加错误。再核对 CSINode 中该驱动的 EC2 实例 ID，确认 AWS 中的卷确实附加到了这个实例。

有 AWS CLI 和相应只读权限时，也可以查看后端状态。先将下面的 `your-aws-region` 替换为实际测试区域，再执行命令；区域必须与驱动创建卷时使用的区域一致。命令使用 EC2 的 describe-volumes 接口：

https://docs.aws.amazon.com/cli/latest/reference/ec2/describe-volumes.html

```bash
AWS_REGION=your-aws-region
aws ec2 describe-volumes --region "$AWS_REGION" --volume-ids "$volume_id" \
  --query 'Volumes[].{ID:VolumeId,Size:Size,Zone:AvailabilityZone,State:State,Attachments:Attachments}'
```

没有 CLI 时，在 AWS 控制台按卷 ID 查看。分别记录 AWS 附加状态和节点挂载结果。

## 记录 Stage 和 Publish

找到应用所在节点上的驱动 Pod：

```bash
node_pod=$(kubectl get pods -n kube-system -l name=node-plugin \
  --field-selector "spec.nodeName=$node_name" \
  -o jsonpath='{.items[0].metadata.name}')
kubectl logs -n kube-system "$node_pod" -c node-plugin
kubectl exec -n kube-system "$node_pod" -c node-plugin -- \
  findmnt -o TARGET,SOURCE,FSTYPE,OPTIONS
```

日志中的 `Node NodeStageVolume is called` 和 `Node NodePublishVolume is called` 表示进入了相应方法，不代表 RPC 已经成功。需要对照后续错误、Pod 事件和挂载表，确认暂存目录与 Pod 目标目录都准备好。挂载表中的 TARGET 是目录，SOURCE 是挂载来源，FSTYPE 是文件系统类型，OPTIONS 是挂载选项；用日志中的路径查找对应行。

等待应用容器就绪，再查看容器中的目录：

```bash
kubectl wait -n bsos-demo --for=condition=Ready pod/bsos-demo --timeout=180s
```

180 秒是这条测试命令的等待时间。如果超时，先根据 Pod 事件排查；成功后再执行下面的命令，查看 `/data` 所在的文件系统：

```bash
kubectl exec -n bsos-demo bsos-demo -- df -h /data
```

`df` 能显示这个目录所处的文件系统，但不能单独证明它就是本次创建的 EBS 卷。需要结合前面记录的 PV 卷 ID、附加实例以及节点挂载路径，把应用目录与后端卷对应起来。

## 写入和读取文件

确认 Pod 已就绪后，写入测试文件：

```bash
kubectl exec -n bsos-demo bsos-demo -- \
  sh -c 'printf "%s\n" "bsos csi demo" > /data/hello.txt && sync'
kubectl exec -n bsos-demo bsos-demo -- cat /data/hello.txt
```

这两条命令分别在同一个 Pod 内启动写入和读取进程。读回自己写入的内容，说明当前 Pod 能访问卷中的文件；它还没有验证卸载、重新挂载或跨节点使用。

## 重建 Pod 与检查数据

重建测试需要保留 PVC，并先完成 doc7 所述卸载与解除附加实现。当前代码的清理方法返回 `Unimplemented`，删除 Pod 后可能在卷清理阶段报错；此时应记录错误，修复实现后再继续。

具备正常清理路径后，先记录旧 Pod 的 UID，再删除并重建同名 Pod。UID 是 Kubernetes 为资源分配的唯一标识，名称相同而 UID 不同，才能确认这是一个新 Pod。

```bash
old_pod_uid=$(kubectl get pod bsos-demo -n bsos-demo -o jsonpath='{.metadata.uid}')
printf '旧 Pod UID=%s\n' "$old_pod_uid"
kubectl delete pod bsos-demo -n bsos-demo
kubectl apply -n bsos-demo -f examples/pod.yaml
kubectl wait -n bsos-demo --for=condition=Ready pod/bsos-demo --timeout=180s
```

新 Pod 就绪后，再读取它的 UID 和文件：

```bash
new_pod_uid=$(kubectl get pod bsos-demo -n bsos-demo -o jsonpath='{.metadata.uid}')
printf '新 Pod UID=%s\n' "$new_pod_uid"
kubectl get pod bsos-demo -n bsos-demo -o wide
kubectl exec -n bsos-demo bsos-demo -- cat /data/hello.txt
```

记录新 Pod 的 UID、所在节点和文件内容。新旧 Pod 在同一节点时，只能验证同节点重建。跨节点测试需要在同一可用区内安排另一个合适节点，并确认旧节点已经解除挂载和附加。

只删除 Pod，保留 PVC，才能继续使用原来的 PV。删除 PVC 后重新创建同名 PVC，可能得到一个新的后端卷，不能用这种方式检查原卷中的文件是否保留。

## 结束测试后的回收

记录本次的 PVC、PV、EBS 卷 ID 和节点，避免把后端卷与别的测试混淆。清理方法已经补齐时，先停止使用卷的 Pod，检查节点卸载和解除附加，再按所选回收策略处理 PVC 和后端卷。

本次若使用 Retain，删除 PVC 后仍需要另行处理保留的 PV 和 EBS 卷。若改回 Delete，则需要已经实现 DeleteVolume，并确认后端删除结果。

资源删除时可能在等待 finalizer。finalizer 是资源上的清理标记，相关控制器完成对应处理后才会移除它，让删除继续。卸载或 detach 出错时，应先检查实际挂载和附加关系；手动移除标记只可能让 Kubernetes 资源消失，并不会代替卸载、解除附加或删除后端卷。

## 文章中保留的结果

实测后保留能说明各阶段结果的输出即可：

| 阶段 | 记录内容 |
| --- | --- |
| 测试环境 | Kubernetes 版本、节点可用区、驱动代码版本和实际镜像 |
| 节点注册 | CSINode 中的驱动条目和节点 ID |
| 创建卷 | PVC Bound、PV 卷 ID、容量、可用区，以及 CreateVolume 的实际日志 |
| 附加卷 | VolumeAttachment 状态、发布信息和 EC2 的 attachment |
| 挂载卷 | Stage/Publish 的结果、节点挂载表和 Pod 内的 `/data` |
| 文件读写 | 写入的内容与实际读回结果 |
| 重建与回收 | 新 Pod 的节点和文件内容、卸载、detach 及卷删除或保留状态 |

未完成的阶段如实记录错误和当前实现限制。截图和命令输出应对应同一次测试中的卷 ID，便于读者把各个资源和 RPC 关联起来。
