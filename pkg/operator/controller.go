package operator

import (
	"fmt"
	"os"
	"reflect"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	kmeta "k8s.io/apimachinery/pkg/api/meta"
	metaapi "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeinformers "k8s.io/client-go/informers"
	kubeset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"

	operatorapi "github.com/openshift/api/operator/v1"
	configset "github.com/openshift/client-go/config/clientset/versioned"
	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	routeset "github.com/openshift/client-go/route/clientset/versioned"
	routeinformers "github.com/openshift/client-go/route/informers/externalversions"

	configapiv1 "github.com/openshift/api/config/v1"
	imageregistryv1 "github.com/openshift/cluster-image-registry-operator/pkg/apis/imageregistry/v1"
	regopclient "github.com/openshift/cluster-image-registry-operator/pkg/client"
	regopset "github.com/openshift/cluster-image-registry-operator/pkg/generated/clientset/versioned"
	regopinformers "github.com/openshift/cluster-image-registry-operator/pkg/generated/informers/externalversions"
	"github.com/openshift/cluster-image-registry-operator/pkg/parameters"
	"github.com/openshift/cluster-image-registry-operator/pkg/resource"
	"github.com/openshift/cluster-image-registry-operator/pkg/resource/object"
	"github.com/openshift/cluster-image-registry-operator/pkg/resource/strategy"
	"github.com/openshift/cluster-image-registry-operator/pkg/storage"
)

const (
	openshiftConfigNamespace = "openshift-config"
	kubeSystemNamespace      = "kube-system"
	workqueueKey             = "changes"
	defaultResyncDuration    = 10 * time.Minute
)

type permanentError struct {
	Err    error
	Reason string
}

func newPermanentError(reason string, err error) error {
	return permanentError{
		Err:    err,
		Reason: reason,
	}
}

func (e permanentError) Error() string {
	return e.Err.Error()
}

func NewController(kubeconfig *restclient.Config) (*Controller, error) {
	namespace, err := regopclient.GetWatchNamespace()
	if err != nil {
		klog.Fatalf("failed to get watch namespace: %s", err)
	}

	p := parameters.Globals{}

	p.Deployment.Namespace = namespace
	p.Deployment.Labels = map[string]string{"docker-registry": "default"}

	p.Pod.ServiceAccount = "registry"
	p.Container.Port = 5000

	p.Healthz.Route = "/healthz"
	p.Healthz.TimeoutSeconds = 5

	p.Service.Name = imageregistryv1.ImageRegistryName
	p.ImageConfig.Name = "cluster"
	p.CAConfig.Name = imageregistryv1.ImageRegistryCertificatesName
	p.ServiceCA.Name = "serviceca"

	listers := &regopclient.Listers{}
	c := &Controller{
		kubeconfig: kubeconfig,
		params:     p,
		generator:  resource.NewGenerator(kubeconfig, listers, &p),
		workqueue:  workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Changes"),
		listers:    listers,
	}

	// Initial event to bootstrap CR if it doesn't exist.
	c.workqueue.AddRateLimited(workqueueKey)

	return c, nil
}

type Controller struct {
	kubeconfig *restclient.Config
	params     parameters.Globals
	generator  *resource.Generator
	workqueue  workqueue.RateLimitingInterface
	listers    *regopclient.Listers
}

func (c *Controller) createOrUpdateResources(cr *imageregistryv1.Config) error {
	appendFinalizer(cr)

	err := verifyResource(cr)
	if err != nil {
		return newPermanentError("VerificationFailed", fmt.Errorf("unable to complete resource: %s", err))
	}

	err = c.generator.Apply(cr)
	if err == storage.ErrStorageNotConfigured {
		return newPermanentError("StorageNotConfigured", err)
	} else if err != nil {
		return err
	}

	return nil
}

func (c *Controller) sync() error {
	// Retrieve the cluster proxy configuration and set it for the currently running
	// image registry operator.
	// TODO: There must be a better way to accomplish this that is not so "black box"
	proxyConfig, err := c.listers.ProxyConfigs.Get(imageregistryv1.ClusterProxyResourceName)
	if errors.IsNotFound(err) {
		proxyConfig = &configapiv1.Proxy{}
	} else if err != nil {
		// TODO: should we report Degraded?
		return fmt.Errorf("unable to get cluster proxy configuration: %v", err)
	}

	if err := os.Setenv("NO_PROXY", proxyConfig.Status.NoProxy); err != nil {
		return fmt.Errorf("unable to set NO_PROXY env var: %v", err)
	}

	if err := os.Setenv("HTTP_PROXY", proxyConfig.Status.HTTPProxy); err != nil {
		return fmt.Errorf("unable to set HTTP_PROXY env var: %v", err)
	}

	if err := os.Setenv("HTTPS_PROXY", proxyConfig.Status.HTTPSProxy); err != nil {
		return fmt.Errorf("unable to set HTTPS_PROXY env var: %v", err)
	}

	cr, err := c.listers.RegistryConfigs.Get(imageregistryv1.ImageRegistryResourceName)
	if err != nil {
		if errors.IsNotFound(err) {
			return c.Bootstrap()
		}
		return fmt.Errorf("failed to get %q registry operator resource: %s", imageregistryv1.ImageRegistryResourceName, err)
	}
	cr = cr.DeepCopy() // we don't want to change the cached version
	prevCR := cr.DeepCopy()

	if cr.ObjectMeta.DeletionTimestamp != nil {
		err = c.finalizeResources(cr)

		if genErr := c.generator.ApplyClusterOperator(cr); genErr != nil {
			klog.Errorf("unable to apply cluster operator: %s", genErr)
		}

		return err
	}

	var applyError error
	removed := false
	switch cr.Spec.ManagementState {
	case operatorapi.Removed:
		applyError = c.RemoveResources(cr)
		removed = true
	case operatorapi.Managed:
		applyError = c.createOrUpdateResources(cr)
	case operatorapi.Unmanaged:
		// ignore
	default:
		klog.Warningf("unknown custom resource state: %s", cr.Spec.ManagementState)
	}

	deploy, err := c.listers.Deployments.Get(imageregistryv1.ImageRegistryName)
	if errors.IsNotFound(err) {
		deploy = nil
	} else if err != nil {
		return fmt.Errorf("failed to get %q deployment: %s", imageregistryv1.ImageRegistryName, err)
	} else {
		deploy = deploy.DeepCopy() // make sure we won't corrupt the cached vesrion
	}

	c.syncStatus(cr, deploy, applyError, removed)

	metadataChanged := strategy.Metadata(&prevCR.ObjectMeta, &cr.ObjectMeta)
	specChanged := !reflect.DeepEqual(prevCR.Spec, cr.Spec)
	if metadataChanged || specChanged {
		difference, err := object.DiffString(prevCR, cr)
		if err != nil {
			klog.Errorf("unable to calculate difference in %s: %s", utilObjectInfo(cr), err)
		}
		klog.Infof("object changed: %s (metadata=%t, spec=%t): %s", utilObjectInfo(cr), metadataChanged, specChanged, difference)

		if genErr := c.generator.ApplyClusterOperator(cr); genErr != nil {
			klog.Errorf("unable to apply cluster operator: %s", genErr)
		}

		client, err := regopset.NewForConfig(c.kubeconfig)
		if err != nil {
			return err
		}

		updatedCR, err := client.ImageregistryV1().Configs().Update(cr)
		if err != nil {
			if !errors.IsConflict(err) {
				klog.Errorf("unable to update %s: %s", utilObjectInfo(cr), err)
			}
			return err
		}

		// If we updated the Status field too, we'll make one more call and we
		// want it to succeed.
		cr.ResourceVersion = updatedCR.ResourceVersion
	}

	cr.Status.ObservedGeneration = cr.Generation
	statusChanged := !reflect.DeepEqual(prevCR.Status, cr.Status)
	if statusChanged {
		difference, err := object.DiffString(prevCR, cr)
		if err != nil {
			klog.Errorf("unable to calculate difference in %s: %s", utilObjectInfo(cr), err)
		}
		klog.Infof("object changed: %s (status=%t): %s", utilObjectInfo(cr), statusChanged, difference)

		if genErr := c.generator.ApplyClusterOperator(cr); genErr != nil {
			klog.Errorf("unable to apply cluster operator (cr status=%t): %s", statusChanged, genErr)
		}

		client, err := regopset.NewForConfig(c.kubeconfig)
		if err != nil {
			return err
		}

		_, err = client.ImageregistryV1().Configs().UpdateStatus(cr)
		if err != nil {
			if !errors.IsConflict(err) {
				klog.Errorf("unable to update status %s: %s", utilObjectInfo(cr), err)
			}
			return err
		}
	}

	if _, ok := applyError.(permanentError); !ok {
		return applyError
	}

	return nil
}

func (c *Controller) eventProcessor() {
	for {
		obj, shutdown := c.workqueue.Get()
		if shutdown {
			return
		}

		klog.V(1).Infof("get event from workqueue")
		func() {
			defer c.workqueue.Done(obj)

			if _, ok := obj.(string); !ok {
				c.workqueue.Forget(obj)
				klog.Errorf("expected string in workqueue but got %#v", obj)
				return
			}

			if err := c.sync(); err != nil {
				c.workqueue.AddRateLimited(workqueueKey)
				klog.Errorf("unable to sync: %s, requeuing", err)
			} else {
				c.workqueue.Forget(obj)
				klog.Infof("event from workqueue successfully processed")
			}
		}()
	}
}

func (c *Controller) handler() cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(o interface{}) {
			if clusterOperator, ok := o.(*configapiv1.ClusterOperator); ok {
				if clusterOperator.GetName() != imageregistryv1.ImageRegistryClusterOperatorResourceName {
					return
				}
			}
			klog.V(1).Infof("add event to workqueue due to %s (add)", utilObjectInfo(o))
			c.workqueue.Add(workqueueKey)
		},
		UpdateFunc: func(o, n interface{}) {
			newAccessor, err := kmeta.Accessor(n)
			if err != nil {
				klog.Errorf("unable to get accessor for new object: %s", err)
				return
			}
			oldAccessor, err := kmeta.Accessor(o)
			if err != nil {
				klog.Errorf("unable to get accessor for old object: %s", err)
				return
			}
			if newAccessor.GetResourceVersion() == oldAccessor.GetResourceVersion() {
				// Periodic resync will send update events for all known resources.
				// Two different versions of the same resource will always have different RVs.
				return
			}
			if clusterOperator, ok := o.(*configapiv1.ClusterOperator); ok {
				if clusterOperator.GetName() != imageregistryv1.ImageRegistryClusterOperatorResourceName {
					return
				}
			}
			klog.V(1).Infof("add event to workqueue due to %s (update)", utilObjectInfo(n))
			c.workqueue.Add(workqueueKey)
		},
		DeleteFunc: func(o interface{}) {
			object, ok := o.(metaapi.Object)
			if !ok {
				tombstone, ok := o.(cache.DeletedFinalStateUnknown)
				if !ok {
					klog.Errorf("error decoding object, invalid type")
					return
				}
				object, ok = tombstone.Obj.(metaapi.Object)
				if !ok {
					klog.Errorf("error decoding object tombstone, invalid type")
					return
				}
				klog.V(4).Infof("recovered deleted object %q from tombstone", object.GetName())
			}
			if clusterOperator, ok := o.(*configapiv1.ClusterOperator); ok {
				if clusterOperator.GetName() != imageregistryv1.ImageRegistryClusterOperatorResourceName {
					return
				}
			}
			klog.V(1).Infof("add event to workqueue due to %s (delete)", utilObjectInfo(object))
			c.workqueue.Add(workqueueKey)
		},
	}
}

func (c *Controller) Run(stopCh <-chan struct{}) error {
	defer c.workqueue.ShutDown()

	kubeClient, err := kubeset.NewForConfig(c.kubeconfig)
	if err != nil {
		return err
	}

	routeClient, err := routeset.NewForConfig(c.kubeconfig)
	if err != nil {
		return err
	}

	configClient, err := configset.NewForConfig(c.kubeconfig)
	if err != nil {
		return err
	}

	regopClient, err := regopset.NewForConfig(c.kubeconfig)
	if err != nil {
		return err
	}

	configInformerFactory := configinformers.NewSharedInformerFactory(configClient, defaultResyncDuration)
	kubeInformerFactory := kubeinformers.NewSharedInformerFactoryWithOptions(kubeClient, defaultResyncDuration, kubeinformers.WithNamespace(c.params.Deployment.Namespace))
	openshiftConfigKubeInformerFactory := kubeinformers.NewSharedInformerFactoryWithOptions(kubeClient, defaultResyncDuration, kubeinformers.WithNamespace(openshiftConfigNamespace))
	kubeSystemKubeInformerFactory := kubeinformers.NewSharedInformerFactoryWithOptions(kubeClient, defaultResyncDuration, kubeinformers.WithNamespace(kubeSystemNamespace))
	regopInformerFactory := regopinformers.NewSharedInformerFactory(regopClient, defaultResyncDuration)
	routeInformerFactory := routeinformers.NewSharedInformerFactoryWithOptions(routeClient, defaultResyncDuration, routeinformers.WithNamespace(c.params.Deployment.Namespace))

	var informers []cache.SharedIndexInformer
	for _, ctor := range []func() cache.SharedIndexInformer{
		func() cache.SharedIndexInformer {
			informer := kubeInformerFactory.Apps().V1().Deployments()
			c.listers.Deployments = informer.Lister().Deployments(c.params.Deployment.Namespace)
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := kubeInformerFactory.Apps().V1().DaemonSets()
			c.listers.DaemonSets = informer.Lister().DaemonSets(c.params.Deployment.Namespace)
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := kubeInformerFactory.Core().V1().Services()
			c.listers.Services = informer.Lister().Services(c.params.Deployment.Namespace)
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := kubeInformerFactory.Core().V1().Secrets()
			c.listers.Secrets = informer.Lister().Secrets(c.params.Deployment.Namespace)
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := kubeInformerFactory.Core().V1().ConfigMaps()
			c.listers.ConfigMaps = informer.Lister().ConfigMaps(c.params.Deployment.Namespace)
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := kubeInformerFactory.Core().V1().ServiceAccounts()
			c.listers.ServiceAccounts = informer.Lister().ServiceAccounts(c.params.Deployment.Namespace)
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := routeInformerFactory.Route().V1().Routes()
			c.listers.Routes = informer.Lister().Routes(c.params.Deployment.Namespace)
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := kubeInformerFactory.Rbac().V1().ClusterRoles()
			c.listers.ClusterRoles = informer.Lister()
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := kubeInformerFactory.Rbac().V1().ClusterRoleBindings()
			c.listers.ClusterRoleBindings = informer.Lister()
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := openshiftConfigKubeInformerFactory.Core().V1().ConfigMaps()
			c.listers.OpenShiftConfig = informer.Lister().ConfigMaps(openshiftConfigNamespace)
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := configInformerFactory.Config().V1().Images()
			c.listers.ImageConfigs = informer.Lister()
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := configInformerFactory.Config().V1().ClusterOperators()
			c.listers.ClusterOperators = informer.Lister()
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := configInformerFactory.Config().V1().Proxies()
			c.listers.ProxyConfigs = informer.Lister()
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := regopInformerFactory.Imageregistry().V1().Configs()
			c.listers.RegistryConfigs = informer.Lister()
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := kubeSystemKubeInformerFactory.Core().V1().ConfigMaps()
			c.listers.InstallerConfigMaps = informer.Lister().ConfigMaps(kubeSystemNamespace)
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := configInformerFactory.Config().V1().Infrastructures()
			c.listers.Infrastructures = informer.Lister()
			return informer.Informer()
		},
	} {
		informer := ctor()
		informer.AddEventHandler(c.handler())
		informers = append(informers, informer)
	}

	configInformerFactory.Start(stopCh)
	kubeInformerFactory.Start(stopCh)
	openshiftConfigKubeInformerFactory.Start(stopCh)
	kubeSystemKubeInformerFactory.Start(stopCh)
	regopInformerFactory.Start(stopCh)
	routeInformerFactory.Start(stopCh)

	klog.Info("waiting for informer caches to sync")
	for _, informer := range informers {
		if ok := cache.WaitForCacheSync(stopCh, informer.HasSynced); !ok {
			return fmt.Errorf("failed to wait for caches to sync")
		}
	}

	go wait.Until(c.eventProcessor, time.Second, stopCh)

	klog.Info("started events processor")
	<-stopCh
	klog.Info("shutting down events processor")

	return nil
}
