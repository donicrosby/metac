/*
Copyright 2019 The MayaData Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package generic

import (
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"openebs.io/metac/apis/metacontroller/v1alpha1"
	metainformers "openebs.io/metac/client/generated/informers/externalversions"
	metalisters "openebs.io/metac/client/generated/listers/metacontroller/v1alpha1"
	"openebs.io/metac/config"
	"openebs.io/metac/controller/common"
	dynamicclientset "openebs.io/metac/dynamic/clientset"
	dynamicdiscovery "openebs.io/metac/dynamic/discovery"
	dynamicinformer "openebs.io/metac/dynamic/informer"
	k8s "openebs.io/metac/third_party/kubernetes"
)

// MetaController abstracts Kubernetes informers and listers
// to execute reconcile logic declared in various GenericController
// resources.
type MetaController struct {
	ResourceManager    *dynamicdiscovery.APIResourceManager
	DynClientset       *dynamicclientset.Clientset
	DynInformerFactory *dynamicinformer.SharedInformerFactory

	WatchControllers map[string]*watchController
	WorkerCount      int

	doneCh chan struct{}
}

// ConfigBasedMetaController represents a MetaController that
// is based on configs of type GenericController provided to
// this binary
type ConfigBasedMetaController struct {
	MetaController

	// Path from which metac configs will be loaded
	ConfigPath string

	// Function that fetches all generic controller instances
	// required to run Metac
	//
	// NOTE:
	//	One can use either ConfigPath or this function. ConfigPath
	// option has higher priority.
	GenericControllerAsConfigFn func() ([]*v1alpha1.GenericController, error)

	// Config instances of type GenericController required to run
	// generic meta controllers. In other words these are the
	// configurations to manage (start, stop) specific watch
	// controllers
	GenericControllerConfigs []*v1alpha1.GenericController

	// Total timeout for any condition to succeed.
	//
	// NOTE:
	//	This is currently used to load config that is required
	// to run Metac.
	WaitTimeoutForCondition time.Duration

	// Interval between retries for any condition to succeed.
	//
	// NOTE:
	// 	This is currently used to load config that is required
	// to run Metac
	WaitIntervalForCondition time.Duration
}

// ConfigBasedMetaControllerOption is a functional option to
// mutate ConfigBasedMetaController instance
//
// This follows functional options pattern
type ConfigBasedMetaControllerOption func(*ConfigBasedMetaController) error

// SetGenericControllerAsConfigFn sets the config loader function
// against the ConfigBasedMetaController instance
func SetGenericControllerAsConfigFn(fn func() ([]*v1alpha1.GenericController, error)) ConfigBasedMetaControllerOption {
	return func(c *ConfigBasedMetaController) error {
		c.GenericControllerAsConfigFn = fn
		return nil
	}
}

// SetMetaControllerConfigPath sets the config loader function
// against the ConfigBasedMetaController instance
func SetMetaControllerConfigPath(path string) ConfigBasedMetaControllerOption {
	return func(c *ConfigBasedMetaController) error {
		c.ConfigPath = path
		return nil
	}
}

// NewConfigBasedMetaController returns a new instance of
// ConfigBasedMetaController
func NewConfigBasedMetaController(
	resourceMgr *dynamicdiscovery.APIResourceManager,
	dynClientset *dynamicclientset.Clientset,
	dynInformerFactory *dynamicinformer.SharedInformerFactory,
	workerCount int,
	opts ...ConfigBasedMetaControllerOption,
) (*ConfigBasedMetaController, error) {

	obj := &ConfigBasedMetaController{
		WaitTimeoutForCondition:  30 * time.Minute,
		WaitIntervalForCondition: 1 * time.Second,
	}

	// run the options over ConfigBasedMetaController instance
	for _, o := range opts {
		err := o(obj)
		if err != nil {
			return nil, err
		}
	}

	if obj.ConfigPath == "" && obj.GenericControllerAsConfigFn == nil {
		return nil,
			errors.Errorf(
				"New config metacontroller failed: Both ConfigPath & GenericControllerAsConfig can't be empty",
			)
	}

	var gctlsAsConfig []*v1alpha1.GenericController
	var gctlsAsConfigErr error
	// NOTE: ConfigPath has higher priority to get the
	// GenericController instances as configs to run Metac
	if obj.ConfigPath != "" {
		mconfigs, err := config.New(obj.ConfigPath).Load()
		if err != nil {
			return nil, err
		}
		gctlsAsConfig, gctlsAsConfigErr = mconfigs.ListGenericControllers()

	} else {
		gctlsAsConfig, gctlsAsConfigErr = obj.GenericControllerAsConfigFn()
	}

	if gctlsAsConfigErr != nil {
		return nil, gctlsAsConfigErr
	}

	obj.GenericControllerConfigs = gctlsAsConfig
	obj.MetaController = MetaController{
		ResourceManager:    resourceMgr,
		DynClientset:       dynClientset,
		DynInformerFactory: dynInformerFactory,
		WorkerCount:        workerCount,
		WatchControllers:   make(map[string]*watchController),
	}

	return obj, nil
}

func (mc *ConfigBasedMetaController) String() string {
	return "Local GenericController"
}

// Start generic meta controller by starting watch controllers
// corresponding to the provided config
func (mc *ConfigBasedMetaController) Start() {
	mc.doneCh = make(chan struct{})

	go func() {
		defer close(mc.doneCh)
		defer utilruntime.HandleCrash()

		glog.Infof("Starting %s", mc)

		// we run this as a continuous process
		// until all the configs are loaded
		condErr := mc.wait(mc.startAllWatchControllers)
		if condErr != nil {
			glog.Fatalf("%s: Failed to start: %v", mc, condErr)
		}
	}()
}

// wait polls the condition until it's true, with a configured
// interval and timeout.
//
// The condition function returns a bool indicating whether it
// is satisfied, as well as an error which should be non-nil if
// and only if the function was unable to determine whether or
// not the condition is satisfied (for example if the check
// involves calling a remote server and the request failed).
func (mc *ConfigBasedMetaController) wait(condition func() (bool, error)) error {
	// mark the start time
	start := time.Now()
	for {
		done, err := condition()
		if err == nil && done {
			return nil
		}
		if time.Since(start) > mc.WaitTimeoutForCondition {
			return errors.Errorf(
				"%s: Wait condition timed out %s: %v", mc, mc.WaitTimeoutForCondition, err,
			)
		}
		if err != nil {
			// Log error, but keep trying until timeout.
			glog.V(4).Infof("%s: Wait condition failed: Will retry: %v", mc, err)
		} else {
			glog.V(4).Infof("%s: Waiting for condition to succeed: Will retry", mc)
		}
		time.Sleep(mc.WaitIntervalForCondition)
	}
}

// startAllWatchControllers starts all the watch controllers
// that are specified as config for this binary
func (mc *ConfigBasedMetaController) startAllWatchControllers() (bool, error) {
	// In this metacontroller, we are only responsible for
	// starting/stopping the relevant watch based controllers
	for _, conf := range mc.GenericControllerConfigs {
		key := conf.Key()
		if _, ok := mc.WatchControllers[key]; ok {
			// NOTE:
			//	One needs to be careful not to use duplicate
			// GenericController configs. Duplicate here implies
			// more than one configs having same namespace & name.

			// Already added
			continue
		}

		// watch controller i.e. a controller based on the resource
		// specified in the watch field of GenericController
		wc, err := newWatchController(
			mc.ResourceManager,
			mc.DynClientset,
			mc.DynInformerFactory,
			conf,
		)
		if err != nil {
			return false, errors.Wrapf(err, "%s: Failed to sync key %s", mc, key)
		}

		// start this watch controller
		wc.Start(mc.WorkerCount)
		mc.WatchControllers[key] = wc
	}
	return true, nil
}

// Stop stops this MetaController
func (mc *ConfigBasedMetaController) Stop() {
	glog.Infof("Shutting down %s", mc)

	// Stop metacontroller first so there's no more changes
	// to watch controllers.
	<-mc.doneCh

	// Stop all its watch controllers
	var wg sync.WaitGroup
	for _, wCtl := range mc.WatchControllers {
		wg.Add(1)
		go func(ctl *watchController) {
			defer wg.Done()
			ctl.Stop()
		}(wCtl)
	}
	// wait till all watch controllers are stopped
	wg.Wait()
}

// CRDBasedMetaController represents a MetaController that
// is based on CustomResources of GenericController applied
// to the Kubernetes cluster
type CRDBasedMetaController struct {
	MetaController

	// To list GenericController CRs
	Lister metalisters.GenericControllerLister

	// To watch GenericController CR events
	Informer cache.SharedIndexInformer

	// To enqueue & dequeue GenericController CR events
	Queue workqueue.RateLimitingInterface

	// To stop watching GenericController CR events
	stopCh chan struct{}
}

// NewCRDBasedMetaController returns a new instance of
// CRDBasedMetaController
func NewCRDBasedMetaController(
	resourceMgr *dynamicdiscovery.APIResourceManager,
	dynClientset *dynamicclientset.Clientset,
	dynInformerFactory *dynamicinformer.SharedInformerFactory,
	metaInformerFactory metainformers.SharedInformerFactory,
	workerCount int,
) *CRDBasedMetaController {

	mc := &CRDBasedMetaController{
		MetaController: MetaController{
			ResourceManager:    resourceMgr,
			DynClientset:       dynClientset,
			DynInformerFactory: dynInformerFactory,
			WorkerCount:        workerCount,
			WatchControllers:   make(map[string]*watchController),
		},
		Lister:   metaInformerFactory.Metacontroller().V1alpha1().GenericControllers().Lister(),
		Informer: metaInformerFactory.Metacontroller().V1alpha1().GenericControllers().Informer(),
		Queue: workqueue.NewNamedRateLimitingQueue(
			workqueue.DefaultControllerRateLimiter(), "CRDGCtl",
		),
	}

	mc.Informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    mc.enqueueGenericController,
		UpdateFunc: mc.updateGenericController,
		DeleteFunc: mc.enqueueGenericController,
	})

	return mc
}

// String implements Stringer interface
func (mc *CRDBasedMetaController) String() string {
	return "CRD GenericController"
}

// Start starts this MetaController
func (mc *CRDBasedMetaController) Start() {
	mc.stopCh = make(chan struct{})
	mc.doneCh = make(chan struct{})

	go func() {
		defer close(mc.doneCh)
		defer utilruntime.HandleCrash()

		glog.Infof("Starting %s", mc)
		defer glog.Infof("Shutting down %s", mc)

		if !k8s.WaitForCacheSync(mc.String(), mc.stopCh, mc.Informer.HasSynced) {
			return
		}

		// In the metacontroller, we are only responsible for starting/stopping
		// the watched resources i.e. controllers, so a single worker should be
		// enough.
		for mc.processNextWorkItem() {
		}
	}()
}

// Stop stops this MetaController
func (mc *CRDBasedMetaController) Stop() {
	// Stop metacontroller first so there's no more changes
	// to watched controllers.
	close(mc.stopCh)
	mc.Queue.ShutDown()
	<-mc.doneCh

	// Stop all its watched resources i.e. controllers
	var wg sync.WaitGroup
	for _, c := range mc.WatchControllers {
		wg.Add(1)
		go func(c *watchController) {
			defer wg.Done()
			c.Stop()
		}(c)
	}
	wg.Wait()
}

func (mc *CRDBasedMetaController) processNextWorkItem() bool {
	key, quit := mc.Queue.Get()
	if quit {
		return false
	}
	defer mc.Queue.Done(key)

	err := mc.sync(key.(string))
	if err != nil {
		utilruntime.HandleError(
			errors.Wrapf(err, "%s: Failed to sync key %s: Will re-queue", mc, key),
		)
		// requeue
		mc.Queue.AddRateLimited(key)
		return true
	}

	mc.Queue.Forget(key)
	return true
}

// sync reconciles GenericMetaController resources
func (mc *CRDBasedMetaController) sync(key string) error {
	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	glog.V(4).Infof("%s: Try sync-ing key %s", mc, key)

	ctrl, err := mc.Lister.GenericControllers(ns).Get(name)
	if apierrors.IsNotFound(err) {
		glog.V(3).Infof(
			"%s: Sync key %s ignored: No longer exist: Will stop controller: %v",
			mc, key, err,
		)

		// cleanup this GenericController instance if exists
		if c, ok := mc.WatchControllers[key]; ok {
			c.Stop()
			delete(mc.WatchControllers, key)
		}
		return nil
	}
	if err != nil {
		return err
	}

	return mc.syncGenericController(ctrl)
}

// syncGenericController is all about starting individual
// generic controller resources
func (mc *CRDBasedMetaController) syncGenericController(ctrl *v1alpha1.GenericController) error {
	if c, ok := mc.WatchControllers[ctrl.Key()]; ok {
		// The controller was already started.
		if apiequality.Semantic.DeepEqual(ctrl.Spec, c.GCtlConfig.Spec) {
			// Nothing has changed.
			return nil
		}

		// Applying desired state of GenericController resource implies
		// stop & recreate.
		c.Stop()
		delete(mc.WatchControllers, ctrl.Key())
	}

	// watched resource / controller
	wc, err := newWatchController(
		mc.ResourceManager,
		mc.DynClientset,
		mc.DynInformerFactory,
		ctrl,
	)
	if err != nil {
		return err
	}

	wc.Start(mc.WorkerCount)
	mc.WatchControllers[ctrl.Key()] = wc
	return nil
}

func (mc *CRDBasedMetaController) enqueueGenericController(obj interface{}) {
	key, err := common.KeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(
			errors.Wrapf(err, "%s: Enqueue failed: %+v", mc, obj),
		)
		return
	}

	mc.Queue.Add(key)
}

func (mc *CRDBasedMetaController) updateGenericController(old, cur interface{}) {
	mc.enqueueGenericController(cur)
}
