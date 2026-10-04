package main

import (
	// "database/sql/driver"
	"context"
	"flag"
	"fmt"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/normalzzz/bsos/pkg/driver"
	"github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"os"
	// "github.com/aws/aws-sdk-go-v2/service/ebs"
)

func main() {

	var (
		endpoint = flag.String("endpoint", "defaultvalue", "endpoint out grpc server runs on")
		region   = flag.String("region", "ams3", "region where the volume will be provisioned")
	)

	// Keep accepting the legacy flag without using or logging its value.
	flag.String("token", "", "reserved for compatibility; not used")
	flag.Parse()
	fmt.Println("flags:", *endpoint, *region)

	// create driver instance
	logger := logrus.New()
	logentry := logger.WithField("From", "main")
	regionid := os.Getenv("AWS_REGION")
	nodename := os.Getenv("NODEID")

	logger.Infoln("nodename:", nodename)

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
	cfg, err := awsconfig.LoadDefaultConfig(context.TODO(), awsconfig.WithRegion(regionid))
	if err != nil {
		logentry.Fatalln("failed to load aws config", err)
	}
	// ebsoperator := ebs.NewFromConfig(cfg)

	drv := driver.NewDriver(driver.InputParams{
		Name:      driver.DefaultDriverName,
		Region:    *region,
		Endpoint:  *endpoint,
		Logger:    logger,
		Awsconfig: cfg,
		NodeName:  nodename,
		Clientset: clientset,
	})
	// start the driver , run the grpc server

	if err := drv.Run(); err != nil {
		logentry.Fatalln("driver failed to run with error", err.Error())
	}

}
