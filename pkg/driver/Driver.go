package driver

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	DefaultDriverName = "bsos.normalzzz.csi.dev"
	DevicePathKey     = "devicePath"
)

type InputParams struct {
	Name      string
	Endpoint  string
	Region    string
	Logger    *logrus.Logger
	Awsconfig aws.Config
	NodeName  string
	Clientset *kubernetes.Clientset
}

type Driver struct {
	csi.UnimplementedControllerServer
	csi.UnimplementedNodeServer
	csi.UnimplementedIdentityServer

	name     string
	region   string
	endpoint string

	server *grpc.Server

	logentry *logrus.Entry

	Ec2client *ec2.Client
	selfnode  core.Node
	Clientset *kubernetes.Clientset
	attachMu sync.Mutex
}

func NewDriver(input InputParams) *Driver {
	node, err := input.Clientset.CoreV1().Nodes().Get(context.TODO(), input.NodeName, metav1.GetOptions{})
	if err != nil {
		input.Logger.WithError(err).Fatalln("failed to get node info")
	}

	return &Driver{
		name:      input.Name,
		region:    input.Region,
		endpoint:  input.Endpoint,
		logentry:  input.Logger.WithField("From", "Driver"),
		Ec2client: ec2.NewFromConfig(input.Awsconfig.Copy()),
		Clientset: input.Clientset,
		selfnode:  *node,
	}

}

func (drv *Driver) Run() error {
	url, err := url.Parse(drv.endpoint)
	if err != nil {
		drv.logentry.Fatalln("endpoint could not parsed as url")
	}

	if url.Scheme != "unix" {
		drv.logentry.Fatalln("endpoint must be unix url")
	}

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

	drv.server = grpc.NewServer()

	csi.RegisterNodeServer(drv.server, drv)
	csi.RegisterControllerServer(drv.server, drv)
	csi.RegisterIdentityServer(drv.server, drv)

	drv.logentry.WithField("endpoint", drv.endpoint).Info("starting CSI gRPC server")
	return drv.server.Serve(listener)
}
