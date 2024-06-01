package main

import (
	"flag"
	"fmt"
	"time"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	kubeconfig := flag.String("kubeconfig", "C:\\Users\\utsab\\.kube\\config", "location to your kubeconfig file")
	flag.Parse()
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		fmt.Printf("error %s, building config from flags\n", err.Error())
		config, err = rest.InClusterConfig()
		if err != nil {
			fmt.Printf("error %s, getting inclusterconfig\n", err.Error())
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Printf("error %s, creating clientset\n", err.Error())
	}

	// we need to create informer
	informers := informers.NewSharedInformerFactory(clientset, 10*time.Minute)

	// start the informer
	ch := make(chan struct{})
	c := newController(clientset, informers.Apps().V1().Deployments())
	informers.Start(ch)
	c.run(ch)

	fmt.Println(informers)
}	
