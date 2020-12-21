package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	podmigv1 "github.com/schrej/podmigration-operator/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/serializer"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

const (
	nodePort     = 30080
	node         = "10.43.0.100"
	pollInterval = time.Millisecond * 500
)

var (
	mc *migPodClient
	kc *kubernetes.Clientset
	// obs *migPodObserver

	evalLabels = map[string]string{
		"name": "eval",
	}
)

type result struct {
	delay         int
	memory        int
	withMigration bool
	downtime      time.Duration
}

func main() {
	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	flag.Parse()

	// use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err.Error())
	}

	kc = kubernetes.NewForConfigOrDie(config)
	mc = newMigPodClient(config, "default")

	createServiceIfNotExist()

	file, err := os.Create("results.csv")
	errPanic(err)
	defer file.Close()
	writer := csv.NewWriter(file)
	writer.Write([]string{"delay", "memory", "with migration", "downtime"})

	repeats := 10
	// delayVals := []int{0, 5, 10, 20, 30, 45, 60} //, 90, 120, 180}
	// memoryVals := []int{15_000, 20_000, 25_000, 50_000, 100_000}
	memoryVals := []int{100_000}
	// memoryVals := []int{100}

	// delayVals := []int{0, 30}
	// memoryVals := []int{0, 10000}
	// results := []result{}

	//for _, delay := range delayVals {
	delay := 0
	for _, memory := range memoryVals {
		for i := 0; i < repeats; i++ {
			log.Println("---", delay, "s delay -", memory, "MB memory - run", i+1, "of", repeats, "---")
			// errPanic(writer.Write(executeTest(delay, memory, false).csv()))
			errPanic(writer.Write(executeTest(delay, memory, true).csv()))
			writer.Flush()
			time.Sleep(time.Second)
		}
	}
	//}
	// for _, res := range results {
	// 	err := writer.Write(res.csv())
	// 	errPanic(err)
	// }
	writer.Flush()
}

func errPanic(err error) {
	if err != nil {
		panic(err)
	}
}

func executeTest(delay, memory int, withMigration bool) result {
	var mp *podmigv1.MigratingPod
	var dpl *appsv1.Deployment
	var err error
	if withMigration {
		log.Println("creating MigratingPod")
		mp, err = mc.Create(getTestMigPod(delay, memory))
		errPanic(err)
	} else {
		log.Println("creating Deployment")
		dpl, err = kc.AppsV1().Deployments("default").Create(context.TODO(), getTestDeployment(delay, memory), metav1.CreateOptions{})
		errPanic(err)
	}

	available, downtimeC := monitorDowntime()
	log.Println("waiting until web api is available")
	<-available
	log.Println("web api is available")

	log.Println("triggering migration")
	podList, err := getPods()
	errPanic(err)
	err = kc.CoreV1().Pods("default").Delete(context.TODO(), podList.Items[0].Name, metav1.DeleteOptions{})
	errPanic(err)

	downtime := <-downtimeC
	log.Println("Downtime: ", downtime.String())
	if withMigration {
		mc.Delete(mp.Name)
		waitForMPDelete()
	} else {
		kc.AppsV1().Deployments("default").Delete(context.TODO(), dpl.Name, metav1.DeleteOptions{})
		waitForDeploymentDelete()
	}
	deletePodsNow()
	waitForPodsDelete()

	return result{
		delay:         delay,
		memory:        memory,
		withMigration: withMigration,
		downtime:      downtime,
	}
}

func (r result) csv() []string {
	return []string{strconv.Itoa(r.delay), strconv.Itoa(r.memory), strconv.FormatBool(r.withMigration), strconv.FormatInt(r.downtime.Milliseconds(), 10)}
}

// func waitForState(name, state string) {
// 	log.Println("waiting for " + state + " state")
// 	ch, stop := obs.observe(name)
// 	for mp := range ch {
// 		if mp.Status.State == state {
// 			stop()
// 			return
// 		}
// 	}
// }

func waitForMPDelete() {
	log.Println("waiting for MigratingPods to be deleted")
	list := podmigv1.MigratingPodList{}
	for {
		mc.client.
			Get().
			VersionedParams(&metav1.ListOptions{LabelSelector: labels.Set(evalLabels).String()}, scheme.ParameterCodec).
			Resource("migratingpods").
			Do(context.TODO()).
			Into(&list)
		if len(list.Items) == 0 {
			log.Println("MigratingPods deleted")
			return
		}
		// log.Print(len(list.Items), " MigratingPods remain")
		time.Sleep(time.Second)
	}
}

func getPods() (*corev1.PodList, error) {
	return kc.CoreV1().Pods("default").List(context.TODO(), metav1.ListOptions{LabelSelector: labels.Set(evalLabels).String()})
}

func deletePodsNow() {
	pods, _ := getPods()
	now := int64(0)
	for _, pod := range pods.Items {
		kc.CoreV1().Pods("default").Delete(context.TODO(), pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &now})
	}
}

func waitForPodsDelete() {
	log.Println("waiting for Pods to be deleted")
	for {
		list, _ := getPods()
		if len(list.Items) == 0 {
			log.Println("Pods deleted")
			return
		}
		// log.Print(len(list.Items), " Pods remain")
		time.Sleep(time.Second)
	}
}

func waitForDeploymentDelete() {
	log.Println("waiting for Deployments to be deleted")
	for {
		list, _ := kc.AppsV1().Deployments("default").List(context.TODO(), metav1.ListOptions{LabelSelector: labels.Set(evalLabels).String()})
		if len(list.Items) == 0 {
			log.Println("Deployments deleted")
			return
		}
		// log.Print(len(list.Items), " Deployments remain")
		time.Sleep(time.Second)
	}
}

func monitorDowntime() (available <-chan struct{}, result <-chan time.Duration) {
	av := make(chan struct{})
	res := make(chan time.Duration)
	go func() {
		c := http.Client{}
		c.Timeout = time.Millisecond * 100
		wait := func(available bool) {
			for {
				_, err := c.Get(fmt.Sprintf("http://%s:%d", node, nodePort))
				// if err == nil {
				// 	defer r.Body.Close()
				// 	b, _ := ioutil.ReadAll(r.Body)
				// 	println(string(b))
				// }
				if (err == nil) == available {
					return
				}
				time.Sleep(pollInterval)
			}
		}
		wait(true)
		av <- struct{}{}
		wait(false)
		down := time.Now()
		wait(true)
		res <- time.Since(down)
	}()
	return av, res
}

func createServiceIfNotExist() {
	svc, _ := kc.CoreV1().Services("default").Get(context.TODO(), "eval", metav1.GetOptions{})
	if svc != nil {
		return
	}
	svc = &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "eval",
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeNodePort,
			Selector: evalLabels,
			Ports: []corev1.ServicePort{
				{Port: 8080, Protocol: corev1.ProtocolTCP, NodePort: nodePort},
			},
		},
	}
	if _, err := kc.CoreV1().Services("default").Create(context.TODO(), svc, metav1.CreateOptions{}); err != nil {
		print(err)
	}
}

func getTestMigPod(delay, memory int) *podmigv1.MigratingPod {
	return &podmigv1.MigratingPod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "eval",
			Labels: evalLabels,
		},
		Spec: podmigv1.MigratingPodSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: evalLabels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "test",
							Image:           "ghcr.io/schrej/podmigration-testapp:latest",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Ports: []corev1.ContainerPort{
								{ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
							},
							Command: []string{"/testapp", "-d", strconv.Itoa(delay), "-m", strconv.Itoa(memory)},
						},
					},
				},
			},
		},
	}
}

func getTestDeployment(delay, memory int) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "eval",
			Labels: evalLabels,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: evalLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: evalLabels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "test",
							Image:           "ghcr.io/schrej/podmigration-testapp:latest",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Ports: []corev1.ContainerPort{
								{ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
							},
							Command: []string{"/testapp", "-d", strconv.Itoa(delay), "-m", strconv.Itoa(memory)},
						},
					},
				},
			},
		},
	}
}

type migPodClient struct {
	client *rest.RESTClient
	ns     string
}

func newMigPodClient(baseConfig *rest.Config, namespace string) *migPodClient {
	err := podmigv1.AddToScheme(scheme.Scheme)
	if err != nil {
		panic(err)
	}

	config := *baseConfig
	config.ContentConfig.GroupVersion = &podmigv1.GroupVersion
	config.APIPath = "/apis"
	config.NegotiatedSerializer = serializer.NewCodecFactory(scheme.Scheme)
	config.UserAgent = rest.DefaultKubernetesUserAgent()

	client, err := rest.UnversionedRESTClientFor(&config)
	if err != nil {
		panic(err)
	}
	return &migPodClient{
		client: client,
		ns:     namespace,
	}
}

func (c *migPodClient) Create(mp *podmigv1.MigratingPod) (*podmigv1.MigratingPod, error) {
	res := podmigv1.MigratingPod{}
	err := c.client.
		Post().
		Namespace(c.ns).
		Resource("migratingpods").
		Body(mp).
		Do(context.TODO()).
		Into(&res)
	return &res, err
}

func (c *migPodClient) Get(name string) (*podmigv1.MigratingPod, error) {
	res := podmigv1.MigratingPod{}
	err := c.client.
		Get().
		Namespace(c.ns).
		Resource("migratingpods").
		Name(name).
		Do(context.TODO()).
		Into(&res)
	return &res, err
}

func (c *migPodClient) Delete(name string) (*podmigv1.MigratingPod, error) {
	res := podmigv1.MigratingPod{}
	err := c.client.
		Delete().
		Namespace(c.ns).
		Resource("migratingpods").
		Name(name).
		Do(context.TODO()).
		Into(&res)
	return &res, err
}

func (c *migPodClient) List() (*podmigv1.MigratingPodList, error) {
	res := podmigv1.MigratingPodList{}
	err := c.client.
		Get().
		Namespace(c.ns).
		Resource("migratingpods").
		Do(context.TODO()).
		Into(&res)
	return &res, err
}

// func (c *migPodClient) Watch(name string) (watch.Interface, error) {
// 	return c.client.
// 		Get().
// 		Namespace(c.ns).
// 		Resource("migratingpods").
// 		Name(name).
// 		Watch(context.TODO())
// }

// type migPodObserver struct {
// 	controller cache.Controller
// 	listener   map[string]func(*podmigv1.MigratingPod, string)
// }

// func newObserver() *migPodObserver {
// 	obs := &migPodObserver{
// 		listener: map[string]func(*podmigv1.MigratingPod, string){},
// 	}
// 	_, obs.controller = cache.NewInformer(&cache.ListWatch{
// 		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
// 			o := podmigv1.MigratingPodList{}
// 			err := mc.client.
// 				Get().
// 				Namespace("default").
// 				Resource("migratingpods").
// 				VersionedParams(&options, scheme.ParameterCodec).
// 				Do(context.TODO()).
// 				Into(&o)
// 			return &o, err
// 		},
// 		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
// 			options.Watch = true
// 			return mc.client.
// 				Get().
// 				Namespace("default").
// 				Resource("migratingpods").
// 				VersionedParams(&options, scheme.ParameterCodec).
// 				Watch(context.TODO())
// 		},
// 	}, &podmigv1.MigratingPod{}, 0, cache.ResourceEventHandlerFuncs{
// 		AddFunc: func(obj interface{}) {
// 			if mp, ok := obj.(*podmigv1.MigratingPod); ok {
// 				if f, ok := obs.listener[mp.Name]; ok {
// 					f(mp, "added")
// 				}
// 			}
// 		},
// 		UpdateFunc: func(_, newObj interface{}) {
// 			if mp, ok := newObj.(*podmigv1.MigratingPod); ok {
// 				if f, ok := obs.listener[mp.Name]; ok {
// 					f(mp, "modified")
// 				}
// 			}
// 		},
// 		DeleteFunc: func(obj interface{}) {
// 			if mp, ok := obj.(*podmigv1.MigratingPod); ok {
// 				if f, ok := obs.listener[mp.Name]; ok {
// 					f(mp, "deleted")
// 				}
// 			}
// 		},
// 	})
// 	stop := make(chan struct{})
// 	go obs.controller.Run(stop)
// 	return obs
// }

// func (o *migPodObserver) observe(name string) (<-chan *podmigv1.MigratingPod, func()) {
// 	ch := make(chan *podmigv1.MigratingPod)
// 	o.listener[name] = func(p *podmigv1.MigratingPod, _ string) {
// 		ch <- p
// 	}
// 	return ch, func() {
// 		delete(o.listener, name)
// 	}
// }

// func (o *migPodObserver) waitForDelete(name string) {
// 	ch := make(chan struct{})
// 	defer delete(o.listener, name)
// 	o.listener[name] = func(_ *podmigv1.MigratingPod, e string) {
// 		if e == "deleted" {
// 			ch <- struct{}{}
// 		}
// 	}
// 	<-ch
// }
