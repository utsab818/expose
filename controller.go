package main

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	appinformers "k8s.io/client-go/informers/apps/v1"
	"k8s.io/client-go/kubernetes"
	applisters "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

type controller struct {
	// we need client set to interact with kubernetes cluster
	clientset kubernetes.Interface

	// we need lister -> component to get the resources
	depLister applisters.DeploymentLister

	// if the informer cache has been synced
	depCacheSyncd cache.InformerSynced

	// we need queue to process the tasks
	queue workqueue.RateLimitingInterface
}

func newController(clientset kubernetes.Interface, depInformer appinformers.DeploymentInformer) *controller {
	c := &controller{
		clientset:     clientset,
		depLister:     depInformer.Lister(),
		depCacheSyncd: depInformer.Informer().HasSynced,
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "expose"),
	}

	depInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.handleAdd,
			DeleteFunc: c.handleDel,
		},
	)

	return c
}

func (c *controller) run(ch <-chan struct{}) {
	fmt.Println("starting controller...")

	//wait for the informer cache to sync successfully
	if !cache.WaitForCacheSync(ch, c.depCacheSyncd) {
		fmt.Print("error waiting for cache to be synced\n")
	}

	//It calls a specific function after some duration until the channel is closed.
	go wait.Until(c.worker, 1*time.Second, ch)
	<-ch
}

func (c *controller) worker() {
	for c.processItem() {

	}
}

// call this function infinitely
func (c *controller) processItem() bool {
	item, shutdown := c.queue.Get()

	// if the shutdown is true then return false
	if shutdown {
		return false
	}

	// when synced , we do not want to process that item once again
	defer c.queue.Forget(item)

	// if the shutdown is false then we get the object from the queue
	// since the obj received from the queue is namespace and name so,
	key, err := cache.MetaNamespaceKeyFunc(item)
	if err != nil {
		fmt.Printf("error %s, getting key from cache\n", err.Error())
	}

	// get namespace and name from the key
	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		fmt.Printf("error %s, splitting key into namespace and name\n", err.Error())
	}

	err = c.syncDeployment(ns, name)
	if err != nil {
		// retry
		fmt.Printf("error %s, syncing deployment\n", err.Error())
		return false
	}

	return false
}

func (c *controller) syncDeployment(ns, name string) error {
	ctx := context.Background()

	// get deployment from lister without apiserver
	dep, err := c.depLister.Deployments(ns).Get(name)
	if err != nil {
		fmt.Printf("error %s, getting deployment from lister\n", err.Error())
	}

	// create service
	// we have to modify this, to figure out the port
	// our deployment's container is listening on
	svc := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dep.Name,
			Namespace: ns,
		},
		Spec: corev1.ServiceSpec{
			// get label for deployment to use as selector is service
			Selector: depLabels(*dep),
			Ports: []corev1.ServicePort{
				{
					Name: "http",
					Port: 80,
				},
			},
		},
	}
	s, err := c.clientset.CoreV1().Services(ns).Create(ctx, &svc, metav1.CreateOptions{})
	if err != nil {
		fmt.Printf("error %s, creating service\n", err.Error())
	}
	// create ingress
	return createIngress(ctx, c.clientset, s)
}

func createIngress(ctx context.Context, client kubernetes.Interface, svc *corev1.Service) error {
	pathType := "Prefix"
	ingress := netv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svc.Name,
			Namespace: svc.Namespace,
			Annotations: map[string]string{
				"nginx.ingress.kubernetes.io/rewrite-target": "/",
			},
		},
		Spec: netv1.IngressSpec{
			Rules: []netv1.IngressRule{
				netv1.IngressRule{
					IngressRuleValue: netv1.IngressRuleValue{
						HTTP: &netv1.HTTPIngressRuleValue{
							Paths: []netv1.HTTPIngressPath{
								netv1.HTTPIngressPath{
									Path:     fmt.Sprintf("/%s", svc.Name),
									PathType: (*netv1.PathType)(&pathType),
									Backend: netv1.IngressBackend{
										Service: &netv1.IngressServiceBackend{
											Name: svc.Name,
											Port: netv1.ServiceBackendPort{
												Number: 80,
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	_, err := client.NetworkingV1().Ingresses(svc.Namespace).Create(ctx, &ingress, metav1.CreateOptions{})
	return err
}

func depLabels(dep appsv1.Deployment) map[string]string {
	return dep.Spec.Template.Labels
}

func (c *controller) handleAdd(obj interface{}) {
	fmt.Println("handleadd was called")
	//add the object in workqueue
	c.queue.Add(obj)
}

func (c *controller) handleDel(obj interface{}) {
	fmt.Println("handledel was called")
	c.queue.Add(obj)
}
