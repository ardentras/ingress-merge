package ingress_merge

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"time"

	"github.com/ghodss/yaml"
	"github.com/golang/glog"
	"github.com/pkg/errors"
	"k8s.io/api/core/v1"
	networkingV1 "k8s.io/api/networking/v1beta1"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	IngressClassAnnotation = "kubernetes.io/ingress.class"
	ConfigAnnotation       = "merge.ingress.kubernetes.io/config"
	PriorityAnnotation     = "merge.ingress.kubernetes.io/priority"
	ResultAnnotation       = "merge.ingress.kubernetes.io/result"
)

const (
	NameConfigKey        = "name"
	LabelsConfigKey      = "labels"
	AnnotationsConfigKey = "annotations"
	BackendConfigKey     = "backend"
)

type Controller struct {
	MasterURL            string
	KubeconfigPath       string
	IngressClass         string
	IngressSelector      string
	ConfigMapSelector    string
	IngressWatchIgnore   []string
	ConfigMapWatchIgnore []string

	client             *kubernetes.Clientset
	ingressesIndex     cache.Indexer
	ingressesInformer  cache.Controller
	configMapsIndex    cache.Indexer
	configMapsInformer cache.Controller
	wakeCh             chan struct{}
}

func NewController() *Controller {
	return &Controller{}
}

func (c *Controller) Run(ctx context.Context) (err error) {
	clientConfig, err := clientcmd.BuildConfigFromFlags(c.MasterURL, c.KubeconfigPath)
	if err != nil {
		return err
	}

	c.client, err = kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return err
	}

	childCtx, cancel := context.WithCancel(ctx)
	defer func() {
		if err != nil {
			cancel()
		}
	}()

	if _, err = labels.Parse(c.IngressSelector); err != nil {
		return errors.Wrap(err, "Invalid Ingress selector")
	}

	c.ingressesIndex, c.ingressesInformer = cache.NewIndexerInformer(
		cache.NewFilteredListWatchFromClient(
			c.client.NetworkingV1beta1().RESTClient(),
			"ingresses",
			"",
			func(options *metaV1.ListOptions) {
				options.LabelSelector = c.IngressSelector
				duration := int64(math.MaxInt32)
				options.TimeoutSeconds = &duration
			}),
		&networkingV1.Ingress{},
		0,
		c,
		cache.Indexers{},
	)

	go c.ingressesInformer.Run(childCtx.Done())

	if _, err = labels.Parse(c.ConfigMapSelector); err != nil {
		return errors.Wrap(err, "Invalid ConfigMap selector")
	}

	c.configMapsIndex, c.configMapsInformer = cache.NewIndexerInformer(
		cache.NewFilteredListWatchFromClient(
			c.client.CoreV1().RESTClient(),
			"configmaps",
			"",
			func(options *metaV1.ListOptions) {
				options.LabelSelector = c.ConfigMapSelector
				duration := int64(math.MaxInt32)
				options.TimeoutSeconds = &duration
			}),
		&v1.ConfigMap{},
		0,
		c,
		cache.Indexers{},
	)

	go c.configMapsInformer.Run(childCtx.Done())

	glog.Infoln("Waiting for caches to sync")
	if !cache.WaitForCacheSync(childCtx.Done(), c.ingressesInformer.HasSynced, c.configMapsInformer.HasSynced) {
		return fmt.Errorf("could not sync cache")
	}

	if c.IngressSelector != "" {
		glog.V(1).Infof("Watching Ingress objects matching the following label selector: %v", c.IngressSelector)
	}

	if c.ConfigMapSelector != "" {
		glog.V(1).Infof("Watching ConfigMap objects matching the following label selector: %v", c.ConfigMapSelector)
	}

	if len(c.IngressWatchIgnore) > 0 {
		glog.V(1).Infof("Ignoring Ingress objects with the following annotations: %v", c.IngressWatchIgnore)
	}

	if len(c.ConfigMapWatchIgnore) > 0 {
		glog.V(1).Infof("Ignoring ConfigMap objects with the following annotations: %v", c.ConfigMapWatchIgnore)
	}

	c.wakeCh = make(chan struct{}, 1)

	c.Process(childCtx)

	var debounceCh <-chan time.Time
	for {
		select {
		case <-c.wakeCh:
			if debounceCh == nil {
				debounceCh = time.After(1 * time.Second)
			}
		case <-debounceCh:
			debounceCh = nil
			c.Process(childCtx)
		case <-ctx.Done():
			return nil
		}
	}
}

func (c *Controller) isIgnored(obj interface{}) bool {

	switch object := obj.(type) {
	case *networkingV1.Ingress:
		for _, val := range c.IngressWatchIgnore {
			if _, exists := object.Annotations[val]; exists {
				return true
			}
		}
	case *v1.ConfigMap:
		for _, val := range c.ConfigMapWatchIgnore {
			if _, exists := object.Annotations[val]; exists {
				return true
			}
		}
	default:
		return false
	}
	return false
}

func (c *Controller) OnAdd(obj interface{}) {
	if !c.isIgnored(obj) {
		glog.V(2).Infof("Watched resource added")
		c.wakeUp()
	}
}

func (c *Controller) OnUpdate(oldObj, newObj interface{}) {
	if !c.isIgnored(oldObj) || !c.isIgnored(newObj) {
		glog.V(2).Infof("Watched resource updated")
		c.wakeUp()
	}
}

func (c *Controller) OnDelete(obj interface{}) {
	if !c.isIgnored(obj) {
		glog.V(2).Infof("Watched resource deleted")
		c.wakeUp()
	}
}

func (c *Controller) wakeUp() {
	if c.wakeCh != nil {
		c.wakeCh <- struct{}{}
	}
}

func (c *Controller) Process(ctx context.Context) {
	glog.V(1).Infof("Processing ingress resources")

	var (
		mergeMap = make(map[*v1.ConfigMap][]*networkingV1.Ingress)
		orphaned = make(map[string]*networkingV1.Ingress)
	)

	for _, ingressIface := range c.ingressesIndex.List() {
		ingress := ingressIface.(*networkingV1.Ingress)

		ingressClass := ingress.Annotations[IngressClassAnnotation]
		if ingressClass != c.IngressClass {
			if _, exists := ingress.Annotations[ResultAnnotation]; exists {
				orphaned[ingress.Namespace+"/"+ingress.Name] = ingress
			}
			continue
		}

		if priorityString, exists := ingress.Annotations[PriorityAnnotation]; exists {
			if _, err := strconv.Atoi(priorityString); err != nil {
				glog.Errorf(
					"Ingress [%s/%s] [%s] annotation must be an integer: %v",
					ingress.Namespace,
					ingress.Name,
					PriorityAnnotation,
					err,
				)
				// TODO: emit error event on ingress that priority must be integer

				continue
			}
		}

		configMapName, exists := ingress.Annotations[ConfigAnnotation]
		if !exists {
			// TODO: emit error event on ingress that no config map name is set
			glog.Errorf(
				"Ingress [%s/%s] is missing [%s] annotation",
				ingress.Namespace,
				ingress.Name,
				ConfigAnnotation,
			)
			continue
		}

		configMapIface, exists, _ := c.configMapsIndex.GetByKey(ingress.Namespace + "/" + configMapName)
		if !exists {
			// TODO: emit error event on ingress that config map does not exist
			glog.Errorf(
				"Ingress [%s/%s] needs ConfigMap [%s/%s], however it does not exist",
				ingress.Namespace,
				ingress.Name,
				ingress.Namespace,
				configMapName,
			)
			continue
		}

		configMap := configMapIface.(*v1.ConfigMap)
		mergeMap[configMap] = append(mergeMap[configMap], ingress)
	}

	glog.V(1).Infof("Collected %d ingresses to be merged", len(mergeMap))

	changed := false

	for configMap, ingresses := range mergeMap {
		sort.Slice(ingresses, func(i, j int) bool {
			var (
				a         = ingresses[i]
				b         = ingresses[j]
				priorityA = 0
				priorityB = 0
			)

			if priorityString, exits := a.Annotations[PriorityAnnotation]; exits {
				priorityA, _ = strconv.Atoi(priorityString)
			}

			if priorityString, exits := b.Annotations[PriorityAnnotation]; exits {
				priorityB, _ = strconv.Atoi(priorityString)
			}

			if priorityA > priorityB {
				return true
			} else if priorityA < priorityB {
				return false
			} else {
				return a.Name < b.Name
			}
		})

		var (
			ownerReferences []metaV1.OwnerReference
			tls             []networkingV1.IngressTLS
			rules           []networkingV1.IngressRule
		)

		for _, ingress := range ingresses {
			ownerReferences = append(ownerReferences, metaV1.OwnerReference{
				APIVersion: "extensions/v1beta1",
				Kind:       "Ingress",
				Name:       ingress.Name,
				UID:        ingress.UID,
			})

			// FIXME: merge by SecretName/Hosts?
			for _, t := range ingress.Spec.TLS {
				tls = append(tls, t)
			}

		rules:
			for _, r := range ingress.Spec.Rules {
				for _, s := range rules {
					if r.Host == s.Host {
						for _, path := range r.HTTP.Paths {
							s.HTTP.Paths = append(s.HTTP.Paths, path)
						}
						continue rules
					}
				}

				rules = append(rules, *r.DeepCopy())
			}
		}

		var (
			name        string
			labels      map[string]string
			annotations map[string]string
			backend     *networkingV1.IngressBackend
		)

		if dataName, exists := configMap.Data[NameConfigKey]; exists {
			name = dataName
		} else {
			name = configMap.Name
		}

		if dataLabels, exists := configMap.Data[LabelsConfigKey]; exists {
			if err := yaml.Unmarshal([]byte(dataLabels), &labels); err != nil {
				labels = nil
				glog.Errorf("Could not unmarshal [%s] from ConfigMap [%s/%s]: %v", LabelsConfigKey, configMap.Namespace, configMap.Name, err)
			}
		}

		if dataAnnotations, exists := configMap.Data[AnnotationsConfigKey]; exists {
			if err := yaml.Unmarshal([]byte(dataAnnotations), &annotations); err != nil {
				annotations = nil
				glog.Errorf("Could not unmarshal [%s] from ConfigMap [%s/%s]: %v", AnnotationsConfigKey, configMap.Namespace, configMap.Name, err)
			}

			if annotations[IngressClassAnnotation] == c.IngressClass {
				glog.Errorf(
					"Config [%s/%s] trying to create merged ingress of merge ingress class [%s], you have to change [%s] annotation value",
					configMap.Namespace,
					configMap.Name,
					c.IngressClass,
					IngressClassAnnotation,
				)
				continue
			}
		}

		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[ResultAnnotation] = "true"

		if dataBackend, exists := configMap.Data[BackendConfigKey]; exists {
			if err := yaml.Unmarshal([]byte(dataBackend), &backend); err != nil {
				backend = nil
				glog.Errorf("Could not unmarshal [%s] from ConfigMap [%s/%s]: %v", BackendConfigKey, configMap.Namespace, configMap.Name, err)
			}
		}

		mergedIngress := &networkingV1.Ingress{
			ObjectMeta: metaV1.ObjectMeta{
				Namespace:       configMap.Namespace,
				Name:            name,
				Labels:          labels,
				Annotations:     annotations,
				OwnerReferences: ownerReferences,
			},
			Spec: networkingV1.IngressSpec{
				Backend: backend,
				TLS:     tls,
				Rules:   rules,
			},
		}

		if existingMergedIngressIface, exists, _ := c.ingressesIndex.Get(mergedIngress); exists {
			existingMergedIngress := existingMergedIngressIface.(*networkingV1.Ingress)

			if hasIngressChanged(existingMergedIngress, mergedIngress) {
				changed = true
				ret, err := c.client.NetworkingV1beta1().Ingresses(mergedIngress.Namespace).Update(ctx, mergedIngress, metaV1.UpdateOptions{})
				if err != nil {
					glog.Errorf("Could not update ingress [%s/%s]: %v", mergedIngress.Namespace, mergedIngress.Name, err)
					continue
				}
				mergedIngress = ret
				glog.V(2).Infof("Updated merged ingress [%s/%s]", mergedIngress.Namespace, mergedIngress.Name)
			} else {
				mergedIngress = existingMergedIngress
			}

		} else {
			changed = true
			ret, err := c.client.NetworkingV1beta1().Ingresses(mergedIngress.Namespace).Create(ctx, mergedIngress, metaV1.CreateOptions{})
			if err != nil {
				glog.Errorf("Could not create ingress [%s/%s]: %v", mergedIngress.Namespace, mergedIngress.Name, err)
				continue
			}
			mergedIngress = ret
			glog.V(1).Infof("Created merged ingress [%s/%s]", mergedIngress.Namespace, mergedIngress.Name)
		}

		delete(orphaned, mergedIngress.Namespace+"/"+mergedIngress.Name)
		c.ingressesIndex.Add(mergedIngress)

		for i, ingress := range ingresses {
			if reflect.DeepEqual(ingress.Status, mergedIngress.Status) {
				continue
			}

			mergedIngress.Status.DeepCopyInto(&ingress.Status)

			changed = true
			ret, err := c.client.NetworkingV1beta1().Ingresses(ingress.Namespace).UpdateStatus(ctx, ingress, metaV1.UpdateOptions{})
			if err != nil {
				glog.Errorf("Could not update status of ingress [%s/%s]: %v", ingress.Namespace, ingress.Name, err)
				continue
			}

			ingress = ret
			ingresses[i] = ret

			glog.Infof(
				"Propagated ingress status back from [%s/%s] to [%s/%s]",
				mergedIngress.Namespace,
				mergedIngress.Name,
				ingress.Namespace,
				ingress.Name,
			)
		}
	}

	for _, ingress := range orphaned {
		changed = true
		err := c.client.NetworkingV1beta1().Ingresses(ingress.Namespace).Delete(ctx, ingress.Name, metaV1.DeleteOptions{})
		if err != nil {
			glog.Errorf("Could not delete ingress [%s/%s]: %v", ingress.Namespace, ingress.Name, err)
			continue
		}

		glog.V(1).Infof("Deleted merged ingress [%s/%s]", ingress.Namespace, ingress.Name)

		c.ingressesIndex.Delete(ingress)
	}

	if !changed {
		glog.V(1).Infof("Nothing changed")
	}
}

func hasIngressChanged(old, new *networkingV1.Ingress) bool {
	if new.Namespace != old.Namespace {
		return true
	}
	if new.Name != old.Name {
		return true
	}
	if !reflect.DeepEqual(new.Labels, old.Labels) {
		return true
	}
	if !reflect.DeepEqual(new.Annotations, old.Annotations) {
		return true
	}
	if !reflect.DeepEqual(new.OwnerReferences, old.OwnerReferences) {
		return true
	}
	if !reflect.DeepEqual(new.Spec, old.Spec) {
		return true
	}

	return false
}
