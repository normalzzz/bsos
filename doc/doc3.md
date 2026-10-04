# Go CSI Driver 的启动与能力发现

- 代码仓库：https://github.com/normalzzz/bsos
- 信息来源：https://github.com/viveksinghggits
- 参考项目：https://github.com/viveksinghggits/bsos
- 来源与许可说明：https://github.com/normalzzz/bsos/blob/main/NOTICE.md

要让 provisioner 调用 `CreateVolume`，先要有一个正在监听的驱动进程。本篇从 `main.go` 看这个进程如何启动，以及 sidecar 如何确认自己连接到了正确的驱动。

程序的启动顺序是：读取参数和环境变量，创建 Kubernetes 与 AWS 客户端，读取节点信息并创建 `Driver`，最后监听 Unix socket、启动 gRPC Server。这里的“能力发现”指 sidecar 连上服务后查询驱动支持哪些操作，还没有开始创建卷。

启动代码主要分在以下文件中。Service 的职责见《CSI 的三个 Service 与卷生命周期》。

https://github.com/normalzzz/bsos/blob/main/doc/doc2.md

| 文件 | 内容 |
| --- | --- |
| main.go | 读取配置、初始化客户端、创建驱动 |
| pkg/driver/Driver.go | 驱动结构、节点信息、socket 和 gRPC Server |
| pkg/driver/IdentityServer.go | 驱动身份、插件能力和就绪查询 |
| pkg/driver/ControllerServer.go | 控制端能力查询 |
| manifest/deployment.yaml | Controller Pod 与共享 socket |

https://github.com/normalzzz/bsos/blob/main/main.go

https://github.com/normalzzz/bsos/blob/main/pkg/driver/Driver.go

https://github.com/normalzzz/bsos/blob/main/pkg/driver/IdentityServer.go

https://github.com/normalzzz/bsos/blob/main/pkg/driver/ControllerServer.go

https://github.com/normalzzz/bsos/blob/main/manifest/deployment.yaml

## 启动参数

启动配置有两个来源。以 `--` 开头的是命令行参数，由 Go 的 `flag` 包读取；`AWS_REGION` 和 `NODEID` 是环境变量，由 `os.Getenv` 读取。当前代码保留了一些未用于后端操作的命令行参数，因此要按下表确认实际生效的位置。

| 配置 | 当前默认值 | 当前代码中的用途 |
| --- | --- | --- |
| `--endpoint` | `defaultvalue` | gRPC Server 的监听地址，运行时需要指定有效的 Unix socket URL |
| `--region` | `ams3` | 写入驱动的 `region` 字段，当前后端操作没有使用这个字段 |
| `--token` | 空字符串 | 为兼容旧命令保留，未用于认证，也不会输出到日志 |
| `AWS_REGION` | 由运行环境提供 | 传给 AWS SDK，决定使用的区域 |
| `NODEID` | 由运行环境提供 | Kubernetes Node 名称，用来读取 Node 对象 |

部署清单通过环境变量传入 endpoint：

```yaml
args:
- "--endpoint=$(CSI_ENDPOINT)"
env:
- name: CSI_ENDPOINT
  value: unix:///var/lib/csi/sockets/pluginproxy/bsoscsi.sock
```

Kubernetes 在容器启动时展开 `$(CSI_ENDPOINT)`，程序收到完整的 `--endpoint` 参数。

`main.go` 另外读取两个环境变量：

```go
regionid := os.Getenv("AWS_REGION")
nodename := os.Getenv("NODEID")
```

`NODEID` 在清单中来自 Pod 的 `spec.nodeName`：

```yaml
- name: NODEID
  valueFrom:
    fieldRef:
      fieldPath: spec.nodeName
```

Downward API 允许把 Pod 自己的信息注入容器。这里的 `fieldPath: spec.nodeName` 读取的是 Pod 被调度到哪个 Kubernetes 节点，再把节点名称写入 `NODEID` 环境变量。

虽然变量名是 NODEID，它保存的仍是 Kubernetes Node 名称。驱动用这个名称查询 Node 对象，然后才能从 ProviderID 取得 EC2 实例 ID；后者才是 CSI `NodeGetInfo` 返回的 `NodeId`。Pod 信息与环境变量说明：

https://kubernetes.io/docs/tasks/inject-data-application/environment-variable-expose-pod-information/

## Kubernetes 和 AWS 客户端

驱动要访问两个不同的 API。Kubernetes 客户端用来读取 Node 等集群对象；AWS 客户端用来创建 EBS 卷、查询实例和附加卷。它们使用各自的地址、凭据和权限，能访问 Kubernetes 不代表就有创建 EBS 卷的权限。

### Kubernetes 配置

当前程序先尝试读取默认 kubeconfig 文件，失败后再读取 Pod 内的集群配置：

```go
config, err := clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
if err != nil {
    restConfig, err := rest.InClusterConfig()
    if err != nil {
        logentry.Fatal(err)
    }
    config = restConfig
}

clientset, err := kubernetes.NewForConfig(config)
if err != nil {
    logentry.Fatal(err)
}
```

在集群外运行时，kubeconfig 提供目标集群地址和访问身份。当前程序显式尝试默认文件路径；读取失败后，才尝试 `InClusterConfig`，使用 Pod 内的集群地址和 ServiceAccount 凭据。

ServiceAccount 是 Pod 访问 Kubernetes API 时使用的身份，RBAC 规则决定这个身份可以对哪些资源执行哪些操作。例如对 `nodes` 的 `get` 权限，允许驱动读取指定的 Node 对象。client-go 集群内配置示例：

https://github.com/kubernetes/client-go/blob/master/examples/in-cluster-client-configuration/README.md

这个驱动在启动阶段就会读取 Node，因此对应的身份需要有 `nodes` 的 `get` 权限。Controller Pod 使用的 ServiceAccount 和权限定义在 manifest/rbac.yaml 中。

https://github.com/normalzzz/bsos/blob/main/manifest/rbac.yaml

### AWS 配置

AWS 客户端的配置通过 SDK 加载：

```go
cfg, err := awsconfig.LoadDefaultConfig(context.TODO(), awsconfig.WithRegion(regionid))
if err != nil {
    logentry.Fatalln("failed to load aws config", err)
}
```

这里传入的 `regionid` 来自 `AWS_REGION`。命令行中的 `--region=ams3` 不会配置 AWS SDK 的区域。

AWS 凭据由 SDK 的默认凭据链加载，可以来自环境变量、共享配置或运行环境提供的 IAM 身份。加载配置成功不等于已经验证了创建 EBS 卷所需的权限，具体 AWS API 调用仍可能失败。AWS SDK for Go 配置说明：

https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-gosdk.html

## 创建 Driver

`main.go` 将客户端和配置传给 `NewDriver`：

```go
drv := driver.NewDriver(driver.InputParams{
    Name:      driver.DefaultDriverName,
    Region:    *region,
    Endpoint:  *endpoint,
    Logger:    logger,
    Awsconfig: cfg,
    NodeName:  nodename,
    Clientset: clientset,
})
```

`DefaultDriverName` 是 `bsos.normalzzz.csi.dev`，Identity Service 会返回这个名称。`Endpoint` 决定驱动监听的 socket，`Clientset` 和 `Awsconfig` 分别用于 Kubernetes 与 AWS 操作。

`NewDriver` 首先读取当前节点：

```go
node, err := input.Clientset.CoreV1().Nodes().Get(context.TODO(), input.NodeName, metav1.GetOptions{})
if err != nil {
    input.Logger.WithError(err).Fatalln("failed to get node info")
}
```

读取成功后，`NewDriver` 把 Node 对象保存在 `Driver.selfnode` 中，并通过 `ec2.NewFromConfig(input.Awsconfig.Copy())` 创建 EC2 客户端。以后调用 `NodeGetInfo` 时，就从这个保存的对象中读取实例 ID 和可用区。

这里的 `drv` 是一个 Go 结构体实例，保存驱动配置和客户端，CSI 方法通过它使用这些字段。创建这个结构体并不会开始接收 RPC，还需要后面的 `drv.Run()` 启动监听。

由于 Controller 和 Node 使用同一个驱动实现，两种 Pod 启动时都会读取所在节点。如果 `NODEID` 为空、节点不存在或 RBAC 不允许读取，程序会在 gRPC Server 启动前退出。

### Service 的默认实现

`Driver` 嵌入了 CSI Go 包中的三个默认实现。结构中的其他字段省略如下：

```go
type Driver struct {
    csi.UnimplementedControllerServer
    csi.UnimplementedNodeServer
    csi.UnimplementedIdentityServer

    // 其他字段省略。
}
```

这些嵌入类型为尚未实现的方法提供返回 `Unimplemented` 的默认实现。驱动在 `IdentityServer.go`、`ControllerServer.go` 和 `NodeServer.go` 中定义自己的方法后，调用相应方法就会执行驱动代码。默认实现可在 CSI Go 包中查看：

https://pkg.go.dev/github.com/container-storage-interface/spec/lib/go/csi

默认实现让结构体具备相应接口的方法，但方法返回 `Unimplemented` 时仍不能执行操作。后面还需要分别完成“把方法注册到 gRPC Server”和“向调用方声明已支持的功能”，这两步用途不同。

## Unix socket

endpoint 是调用方连接驱动的通信地址。本例选择 Unix socket，所以 endpoint 包含 `unix://` 协议前缀和一个文件路径。容器共享这个路径所在的目录后，就能通过同一个 socket 通信。

`main.go` 最后调用 `drv.Run()`。`Run()` 解析 endpoint，并检查协议：

```go
url, err := url.Parse(drv.endpoint)
if err != nil {
    drv.logentry.Fatalln("endpoint could not parsed as url")
}

if url.Scheme != "unix" {
    drv.logentry.Fatalln("endpoint must be unix url")
}
```

当前实现只接受 `unix`。对于 `unix:///var/lib/csi/sockets/pluginproxy/bsoscsi.sock`，URL 的 scheme 为 `unix`，host 为空，path 为 `/var/lib/csi/sockets/pluginproxy/bsoscsi.sock`。

代码取出文件路径，移除该路径下已有的文件，再开始监听：

```go
grpcAddress := path.Join(url.Host, filepath.FromSlash(url.Path))
if url.Host == "" {
    grpcAddress = filepath.FromSlash(url.Path)
}

if err := os.Remove(grpcAddress); err != nil && !os.IsNotExist(err) {
    return fmt.Errorf("removiong listen address %s\n", err.Error())
}

listener, err := net.Listen(url.Scheme, grpcAddress)
if err != nil {
    return fmt.Errorf(".Listen failed %s\n", err.Error())
}
```

`net.Listen` 在这个地址上等待连接，得到的 `listener` 随后交给 gRPC Server。父目录需要事先存在，当前 `Run()` 没有创建目录的逻辑；Pod 中由卷挂载提供目录，本地运行时则要自己创建。Go net.Listen 说明：

https://pkg.go.dev/net#Listen

移除遗留 socket 是为了在重启后重新监听。当前代码会直接删除 endpoint 对应路径，因此每个实例需要独占自己的 socket 路径。

## gRPC Server

监听器创建成功后，代码建立 gRPC Server 并注册三个 Service：

```go
drv.server = grpc.NewServer()

csi.RegisterNodeServer(drv.server, drv)
csi.RegisterControllerServer(drv.server, drv)
csi.RegisterIdentityServer(drv.server, drv)

drv.logentry.WithField("endpoint", drv.endpoint).Info("starting CSI gRPC server")
return drv.server.Serve(listener)
```

`RegisterNodeServer(drv.server, drv)` 告诉 gRPC Server：“收到 Node Service 请求时，调用 `drv` 上的对应方法。”另外两个 Register 函数为 Identity 和 Controller 做相同的事。

`Serve(listener)` 开始接收连接、读取 RPC 请求并分派给这些方法。正常运行时，主程序会一直在这次调用中等待和处理请求。这里的注册发生在 Go 进程内部；向 kubelet 注册节点插件是 doc4 中的另一套流程。gRPC Go 服务端示例：

https://grpc.io/docs/languages/go/basics/#starting-the-server

`starting CSI gRPC server` 在调用 `Serve` 前打印。看到这行日志，说明客户端初始化、节点读取、socket 监听和 Service 注册已经完成；后续 RPC 日志可以确认调用方已经连到服务。

## 能力查询的实现

### GetPluginInfo

Identity Service 返回驱动名称和版本：

```go
return &csi.GetPluginInfoResponse{
    Name:          drv.name,
    VendorVersion: "v1.1",
}, nil
```

`v1.1` 是代码中写入的驱动版本。CSI Go 包依赖版本和容器镜像标签分别在依赖文件与部署清单中设置。

### GetPluginCapabilities

当前实现返回 `CONTROLLER_SERVICE` 和 `VOLUME_ACCESSIBILITY_CONSTRAINTS`。下面是 `Capabilities` 列表中声明 Controller Service 的一项：

```go
{
    Type: &csi.PluginCapability_Service_{
        Service: &csi.PluginCapability_Service{
            Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
        },
    },
},
```

这段结构层级较多，是因为 CSI 的 protobuf 定义生成了相应的 Go 包装类型。阅读时从里面看即可：最内层的枚举值是 `CONTROLLER_SERVICE`，外面的结构把这个值包装成一条插件能力记录，再放进响应的 Capabilities 列表。

第二项使用相同结构，只把最内层的值换成 `csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS`，表示卷有位置限制。

`CONTROLLER_SERVICE` 告诉 sidecar 可以继续查询 Controller Service 支持哪些操作。拓扑能力则告诉调用方需要考虑卷和节点的可访问位置；本例通过 `NodeGetInfo` 和 `CreateVolume` 传递可用区。CSI 插件能力说明：

https://github.com/container-storage-interface/spec/blob/v1.13.0/spec.md#getplugincapabilities

### Probe

当前 Probe 固定返回就绪：

```go
return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
```

`Ready` 的 Go 字段使用 protobuf 布尔包装类型，因此需要用 `wrapperspb.Bool(true)` 构造它。当前代码收到 Probe 就返回 true，没有查询 AWS 或执行设备检查，所以这次响应只能说明驱动自报就绪。

### ControllerGetCapabilities

Controller Service 通过这个方法返回支持的操作。代码将两项能力放入列表，再构造响应：

```go
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
```

`CREATE_DELETE_VOLUME` 对应创建和删除卷，`PUBLISH_UNPUBLISH_VOLUME` 对应控制端发布和解除发布。当前仓库已经声明这两项能力，但 `DeleteVolume` 和 `ControllerUnpublishVolume` 仍返回 `Unimplemented`，回收实现还需要补齐。

## Controller Pod 的共享 socket

manifest/deployment.yaml 中，`bsos`、external-provisioner 和 external-attacher 共用 `endpoint-volume`。这个卷的类型是 `emptyDir`，挂载到三个容器中的同一目录：

https://github.com/normalzzz/bsos/blob/main/manifest/deployment.yaml

```yaml
volumeMounts:
- mountPath: /var/lib/csi/sockets/pluginproxy/
  name: endpoint-volume
```

Pod 中的卷定义如下：

```yaml
volumes:
- name: endpoint-volume
  emptyDir: {}
```

`emptyDir` 提供一个属于该 Pod 的共享目录，`volumeMounts` 把它挂进各个容器。三个容器里的路径因此对应同一份目录内容：驱动在其中创建 socket 后，provisioner 和 attacher 就能连接到这个 socket。这里共享的是进程通信入口，不是应用的 EBS 数据卷。CSI Controller 部署说明：

https://kubernetes-csi.github.io/docs/deploying.html#communication-with-sidecars

| 容器 | 参数 | 使用的地址 |
| --- | --- | --- |
| bsos | `--endpoint` | `unix:///var/lib/csi/sockets/pluginproxy/bsoscsi.sock` |
| external-provisioner | `--csi-address` | `/var/lib/csi/sockets/pluginproxy/bsoscsi.sock` |
| external-attacher | `--csi-address` | `/var/lib/csi/sockets/pluginproxy/bsoscsi.sock` |

驱动通过 `url.Parse` 解析参数，需要 `unix://` 前缀。sidecar 的参数使用文件路径，两者指向同一个 socket。

这条 socket 用于 Controller Pod 内的调用。节点端使用另一条 socket，并通过 registrar 向 kubelet 注册。

## 启动和查看日志

### 在本地运行

本地运行需要 Go、能访问集群的默认 kubeconfig 文件，以及可读取 Node 的 Kubernetes 身份。当前 go.mod 声明的 Go 版本为 `1.26.0`。

https://github.com/normalzzz/bsos/blob/main/go.mod

在仓库根目录执行以下命令，将节点名称和 AWS 区域换成自己的配置：

```bash
export AWS_REGION=us-east-1
export NODEID=your-kubernetes-node-name
mkdir -p /tmp/bsos-csi
go run . --endpoint=unix:///tmp/bsos-csi/csi.sock
```

本地启动后，可以在另一终端检查 socket：

```bash
ls -l /tmp/bsos-csi/csi.sock
```

看到 socket 文件，只能确认已经建立了本地通信入口。没有调用方时，服务会等待请求，不会自动打印 Identity 方法的调用日志；需要 sidecar 或其他 CSI 客户端连接后发出相应 RPC。

### 在集群中运行

Dockerfile 将 Go 程序编译为 `/app/bsos`，作为容器入口。运行镜像还安装了 `util-linux` 和 `e2fsprogs`，供后续节点挂载操作使用。

https://github.com/normalzzz/bsos/blob/main/Dockerfile

构建和推送镜像时，将下面的示例镜像地址替换为自己的仓库地址：

```bash
docker build -t registry.example.com/bsos:doc3 .
docker push registry.example.com/bsos:doc3
```

部署前，将 `manifest/deployment.yaml` 中的驱动镜像改为构建出的镜像，并确认两个 sidecar 镜像也能被集群拉取。清单中的 `registry.example.com` 是脱敏后的示例地址，需要替换为实际可用的镜像仓库。

当前 Deployment 没有显式设置 `AWS_REGION`，需要在 `bsos` 容器的 `env` 中补充区域。下面的片段需要合并到已有环境变量中：

```yaml
- name: AWS_REGION
  value: us-east-1
```

`us-east-1` 是示例区域，运行时应换成自己的区域。`manifest/rbac.yaml` 中的 ServiceAccount 使用全零账号的示例 IAM 角色 ARN，需要按实际分区、账号和角色配置；使用其他 AWS 身份方式时，相应调整或移除该注解。

配置准备好后，在仓库根目录应用清单：

```bash
kubectl apply -f manifest/rbac.yaml
kubectl apply -f manifest/deployment.yaml
kubectl rollout status deployment/bsos -n kube-system
kubectl logs deployment/bsos -n kube-system -c bsos
```

下面按已有运行记录展示启动日志的形式。时间已替换为占位值，`flags` 行按当前代码省略 token；这段内容用于说明调用过程：

```text
time="<TIMESTAMP>" level=info msg="starting CSI gRPC server" From=Driver endpoint="unix:///var/lib/csi/sockets/pluginproxy/bsoscsi.sock"
flags: unix:///var/lib/csi/sockets/pluginproxy/bsoscsi.sock ams3
time="<TIMESTAMP>" level=info msg="Identity Probe is called" From=Driver
time="<TIMESTAMP>" level=info msg="Identity GetPluginInfo is called" From=Driver
time="<TIMESTAMP>" level=info msg="Identity GetPluginCapabilities is called" From=Driver
time="<TIMESTAMP>" level=info msg="Controller ControllerGetCapabilities is called" From=Driver
```

`flags` 一行来自 `main.go`，打印 endpoint 和 region，不打印 token。这里的 `ams3` 是旧命令行参数的默认值，不代表 AWS SDK 使用的区域。标准输出和日志输出在收集后可能交错，不能仅凭这一行的位置判断程序执行顺序。

其余日志说明驱动收到了就绪、身份和能力查询。这些查询通常由 Controller Pod 中的 sidecar 发起，当前日志未记录具体调用方。不同 sidecar 会分别查询，也可能重复调用，顺序不固定。

启动阶段可以分别查看三个容器的日志：

```bash
kubectl logs deployment/bsos -n kube-system -c bsos
kubectl logs deployment/bsos -n kube-system -c external-provisioner
kubectl logs deployment/bsos -n kube-system -c external-attacher
```

| 现象 | 检查位置 |
| --- | --- |
| `endpoint must be unix url` | `--endpoint` 是否传入完整的 Unix socket URL |
| `failed to get node info` | `NODEID`、Node 对象、kubeconfig 或 ServiceAccount 的 Node 读取权限 |
| `.Listen failed` | socket 父目录、写入权限，以及是否有其他进程占用路径 |
| sidecar 无法连接 socket | 地址是否对应同一个文件、共享卷是否挂载、驱动是否仍在运行 |
| ImagePullBackOff | 驱动与 sidecar 的镜像地址，以及集群拉取权限 |

这段日志验证的是 Controller Pod 中的服务启动和 RPC 查询。节点端是否注册到 kubelet，需要查看 registrar 的注册结果和 CSINode；创建、附加和挂载卷则需要对应的资源与 RPC 结果。
