package main

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	// once in queue and executed, remove or forget that item from the queue.
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
		return false
	}

	// check if the object has been deleted from k8s cluster
	// It is safer to query API server directly rather than using
	// liters, as cache may not have the deployment in some cases.
	ctx := context.Background()
	_, err = c.clientset.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	// this error says that deployment is not present, hence it is deleted.
	// we can use apimachinery errors to get if deployment is not found error.
	if apierrors.IsNotFound(err) {
		fmt.Printf("handle delete event for deployment %s\n", name)

		// delete services with particular label (label provided by the controller to resources)s
		labelSelector := fmt.Sprintf("created-by=expose-controller,deployment=%s", name)

		// ****************service deletion********************
		// list the service with the particular label.
		svcList, err := c.clientset.CoreV1().Services(ns).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})

		if err != nil {
			fmt.Printf("error %s, listing services\n", err.Error())
			return false
		}

		// delete service
		for _, svc := range svcList.Items {
			err = c.clientset.CoreV1().Services(ns).Delete(ctx, svc.Name, metav1.DeleteOptions{})
			if err != nil {
				fmt.Printf("error %s, deleting service %s\n", err.Error(), svc.Name)
			}
		}

		// *************** ingress deletion ***************
		// list all the ingress in the particular label
		ingList, err := c.clientset.NetworkingV1().Ingresses(ns).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			fmt.Printf("error %s, listing ingresses\n", err.Error())
			return false
		}

		// delete ingress
		for _, ing := range ingList.Items {
			err = c.clientset.NetworkingV1().Ingresses(ns).Delete(ctx, ing.Name, metav1.DeleteOptions{})
			if err != nil {
				fmt.Printf("error %s, deleting ingress %s\n", err.Error(), ing.Name)
				return false
			}
		}

		return true
	}

	err = c.syncDeployment(ns, name)
	if err != nil {
		// retry
		fmt.Printf("error %s, syncing deployment\n", err.Error())
		return false
	}

	return true
}

func (c *controller) syncDeployment(ns, name string) error {
	ctx := context.Background()

	// get deployment from lister without apiserver
	dep, err := c.depLister.Deployments(ns).Get(name)
	if err != nil {
		fmt.Printf("error %s, getting deployment from lister\n", err.Error())
	}

	labels := map[string]string{
		"created-by": "expose-controller",
		"deployment": name,
	}

	// create service
	// we have to modify this, to figure out the port
	// our deployment's container is listening on
	svc := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dep.Name,
			Namespace: ns,
			Labels:    labels,
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
			Labels: map[string]string{
				"created-by": "expose-controller",
				"deployment": svc.Name,
			},
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
