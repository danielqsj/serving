/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package net

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	endpointsinformer "knative.dev/pkg/client/injection/kube/informers/core/v1/endpoints"
	serviceinformer "knative.dev/pkg/client/injection/kube/informers/core/v1/service"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/logging"
	"knative.dev/serving/pkg/apis/networking"
	"knative.dev/serving/pkg/apis/serving"
	revisioninformer "knative.dev/serving/pkg/client/injection/informers/serving/v1alpha1/revision"
	servinglisters "knative.dev/serving/pkg/client/listers/serving/v1alpha1"
	"knative.dev/serving/pkg/network"
	"knative.dev/serving/pkg/network/prober"
	"knative.dev/serving/pkg/queue"
	"knative.dev/serving/pkg/reconciler"
)

// revisionDestsUpdate contains the state of healthy l4 dests for talking to a revision and is the
// primary output from the RevisionBackendsManager system. If a healthy ClusterIP is found then
// ClusterIPDest will be set to non empty string and Dests will be nil. Otherwise Dests will be set
// to a slice of healthy l4 dests for reaching the revision.
type revisionDestsUpdate struct {
	Rev           types.NamespacedName
	ClusterIPDest string
	Dests         sets.String
}

const (
	probeTimeout   time.Duration = 300 * time.Millisecond
	probeFrequency time.Duration = 200 * time.Millisecond
)

// revisionWatcher watches the podIPs and ClusterIP of the service for a revision. It implements the logic
// to supply revisionDestsUpdate events on updateCh
type revisionWatcher struct {
	doneCh   <-chan struct{}
	cancel   context.CancelFunc
	rev      types.NamespacedName
	protocol networking.ProtocolType
	updateCh chan<- revisionDestsUpdate

	// Stores the list of pods that have been successfully probed.
	healthyPods sets.String
	// Stores whether the service ClusterIP has been seen as healthy
	clusterIPHealthy bool

	transport     http.RoundTripper
	destsCh       chan sets.String
	serviceLister corev1listers.ServiceLister
	logger        *zap.SugaredLogger
}

func newRevisionWatcher(ctx context.Context, rev types.NamespacedName, protocol networking.ProtocolType,
	updateCh chan<- revisionDestsUpdate, destsCh chan sets.String,
	transport http.RoundTripper, serviceLister corev1listers.ServiceLister,
	logger *zap.SugaredLogger) *revisionWatcher {
	ctx, cancel := context.WithCancel(ctx)
	return &revisionWatcher{
		doneCh:        ctx.Done(),
		cancel:        cancel,
		rev:           rev,
		protocol:      protocol,
		updateCh:      updateCh,
		healthyPods:   sets.NewString(),
		transport:     transport,
		destsCh:       destsCh,
		serviceLister: serviceLister,
		logger:        logger,
	}
}

func (rw *revisionWatcher) getK8sPrivateService() (*corev1.Service, error) {
	selector := labels.SelectorFromSet(map[string]string{
		serving.RevisionLabelKey:  rw.rev.Name,
		networking.ServiceTypeKey: string(networking.ServiceTypePrivate),
	})
	svcList, err := rw.serviceLister.Services(rw.rev.Namespace).List(selector)
	if err != nil {
		return nil, err
	}

	switch len(svcList) {
	case 0:
		return nil, fmt.Errorf("found no private services for revision %q", rw.rev.String())
	case 1:
		return svcList[0], nil
	default:
		return nil, fmt.Errorf("found multiple private services matching revision %v", rw.rev)
	}
}

func (rw *revisionWatcher) probe(ctx context.Context, dest string) (bool, error) {
	httpDest := url.URL{
		Scheme: "http",
		Host:   dest,
	}
	// NOTE: changes below may require changes to testing/roundtripper.go to make unit tests passing.
	return prober.Do(ctx, rw.transport, httpDest.String(),
		prober.WithHeader(network.ProbeHeaderName, queue.Name),
		prober.WithHeader(network.UserAgentKey, network.ActivatorUserAgent),
		prober.ExpectsBody(queue.Name),
		prober.ExpectsStatusCodes([]int{http.StatusOK}))

}

func (rw *revisionWatcher) getDest() (string, error) {
	svc, err := rw.getK8sPrivateService()
	if err != nil {
		return "", err
	}
	if svc.Spec.ClusterIP == "" {
		return "", fmt.Errorf("private service %s/%s clusterIP is nil, this should never happen", svc.ObjectMeta.Namespace, svc.ObjectMeta.Name)
	}

	svcPort, ok := getServicePort(rw.protocol, svc)
	if !ok {
		return "", fmt.Errorf("unable to find port in service %s/%s", svc.Namespace, svc.Name)
	}
	return net.JoinHostPort(svc.Spec.ClusterIP, strconv.Itoa(svcPort)), nil
}

func (rw *revisionWatcher) probeClusterIP(dest string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	return rw.probe(ctx, dest)
}

// probePodIPs will probe the given target Pod IPs and will return
// the ones that are successfully probed, whether the update was a no-op, or an error.
func (rw *revisionWatcher) probePodIPs(dests sets.String) (sets.String, bool, error) {
	// Short circuit case where dests == healthyPods
	if rw.healthyPods.Equal(dests) {
		return rw.healthyPods, true /*no-op*/, nil
	}

	toProbe := sets.NewString()
	healthy := sets.NewString()
	for dest := range dests {
		if rw.healthyPods.Has(dest) {
			healthy.Insert(dest)
		} else {
			toProbe.Insert(dest)
		}
	}

	// Short circuit case where the healthy list got effectively smaller
	if toProbe.Len() == 0 {
		return healthy, false, nil
	}

	// Context used for our probe requests
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	var probeGroup errgroup.Group
	healthyDests := make(chan string, toProbe.Len())

	for dest := range toProbe {
		dest := dest // Standard Go concurrency pattern.
		probeGroup.Go(func() error {
			ok, err := rw.probe(ctx, dest)
			if ok {
				healthyDests <- dest
			}
			return err
		})
	}

	err := probeGroup.Wait()
	close(healthyDests)
	changed := len(healthyDests) > 0

	for d := range healthyDests {
		healthy.Insert(d)
	}
	return healthy, !changed, err
}

func (rw *revisionWatcher) sendUpdate(clusterIP string, dests sets.String) {
	select {
	case <-rw.doneCh:
		return
	default:
		rw.updateCh <- revisionDestsUpdate{Rev: rw.rev, ClusterIPDest: clusterIP, Dests: dests}
	}
}

// checkDests performs probing and potentially sends a dests update. It is
// assumed this method is not called concurrently.
func (rw *revisionWatcher) checkDests(dests sets.String) {
	if len(dests) == 0 {
		// We must have scaled down.
		rw.clusterIPHealthy = false
		rw.logger.Debug("ClusterIP is no longer healthy.")
		// Send update that we are now inactive (both params invalid).
		rw.sendUpdate("", nil)
		return
	}

	// First check the clusterIP. We can't cache it, since user might go rogue
	// and delete the K8s service. We'll fix it, but the cluster IP will be different.
	dest, err := rw.getDest()
	if err != nil {
		rw.logger.Errorw("Failed to determine service destination", zap.Error(err))
		return
	}

	if rw.clusterIPHealthy {
		// cluster IP is healthy and we haven't scaled down, short circuit.
		rw.logger.Debugf("ClusterIP %s already probed (backends: %d)", dest, len(dests))
		rw.sendUpdate(dest, dests)
		return
	}

	// If clusterIP is healthy send this update and we are done.
	if ok, err := rw.probeClusterIP(dest); err != nil {
		rw.logger.Errorw("Failed to probe clusterIP "+dest, zap.Error(err))
	} else if ok {
		rw.logger.Debugf("ClusterIP is successfully probed: %s (backends: %d)", dest, len(dests))
		rw.clusterIPHealthy = true
		rw.healthyPods = nil
		rw.sendUpdate(dest, dests)
		return
	}

	hs, noop, err := rw.probePodIPs(dests)
	if err != nil {
		rw.logger.Errorw("Failed probing", zap.Error(err))
		// We dont want to return here as an error still affects health states.
	}

	rw.logger.Debugf("Done probing, got %d healthy pods", len(hs))
	if !noop {
		rw.healthyPods = hs
		rw.sendUpdate("" /*clusterIP not ready yet*/, hs)
	}
}

func (rw *revisionWatcher) run(probeFrequency time.Duration) {
	defer close(rw.destsCh)

	var dests sets.String
	timer := time.NewTicker(probeFrequency)
	defer timer.Stop()

	var tickCh <-chan time.Time
	for {
		if len(dests) != 0 && !rw.clusterIPHealthy {
			tickCh = timer.C
		} else {
			tickCh = nil
		}

		select {
		case <-rw.doneCh:
			return
		case x := <-rw.destsCh:
			dests = x
		case <-tickCh:
		}

		rw.checkDests(dests)
	}
}

// revisionBackendsManager listens to revision endpoints and keeps track of healthy
// l4 dests which can be used to reach a revision
type revisionBackendsManager struct {
	ctx            context.Context
	revisionLister servinglisters.RevisionLister
	serviceLister  corev1listers.ServiceLister

	revisionWatchers    map[types.NamespacedName]*revisionWatcher
	revisionWatchersMux sync.RWMutex

	updateCh       chan revisionDestsUpdate
	transport      http.RoundTripper
	logger         *zap.SugaredLogger
	probeFrequency time.Duration
}

// NewRevisionBackendsManager returns a new RevisionBackendsManager with default
// probe time out.
func newRevisionBackendsManager(ctx context.Context, tr http.RoundTripper) *revisionBackendsManager {
	return newRevisionBackendsManagerWithProbeFrequency(ctx, tr, probeFrequency)
}

// newRevisionBackendsManagerWithProbeFrequency creates a fully spec'd RevisionBackendsManager.
func newRevisionBackendsManagerWithProbeFrequency(ctx context.Context, tr http.RoundTripper,
	probeFreq time.Duration) *revisionBackendsManager {
	rbm := &revisionBackendsManager{
		ctx:              ctx,
		revisionLister:   revisioninformer.Get(ctx).Lister(),
		serviceLister:    serviceinformer.Get(ctx).Lister(),
		revisionWatchers: make(map[types.NamespacedName]*revisionWatcher),
		updateCh:         make(chan revisionDestsUpdate),
		transport:        tr,
		logger:           logging.FromContext(ctx),
		probeFrequency:   probeFrequency,
	}
	endpointsInformer := endpointsinformer.Get(ctx)
	endpointsInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: reconciler.ChainFilterFuncs(
			reconciler.LabelExistsFilterFunc(serving.RevisionUID),
			// We are only interested in the private services, since that is
			// what is populated by the actual revision backends.
			reconciler.LabelFilterFunc(networking.ServiceTypeKey, string(networking.ServiceTypePrivate), false),
		),
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    rbm.endpointsUpdated,
			UpdateFunc: controller.PassNew(rbm.endpointsUpdated),
			DeleteFunc: rbm.endpointsDeleted,
		},
	})

	go func() {
		// updateCh can only be closed after revisionWatchers are done running
		defer close(rbm.updateCh)

		// Wait for cancellation
		<-rbm.ctx.Done()

		// Wait for all revisionWatchers to be done
		rbm.revisionWatchersMux.Lock()
		defer rbm.revisionWatchersMux.Unlock()
		for _, rw := range rbm.revisionWatchers {
			<-rw.destsCh
		}
	}()

	return rbm
}

// Returns channel where destination updates are sent to.
func (rbm *revisionBackendsManager) updates() <-chan revisionDestsUpdate {
	return rbm.updateCh
}

func (rbm *revisionBackendsManager) getRevisionProtocol(revID types.NamespacedName) (networking.ProtocolType, error) {
	revision, err := rbm.revisionLister.Revisions(revID.Namespace).Get(revID.Name)
	if err != nil {
		return "", err
	}
	return revision.GetProtocol(), nil
}

func (rbm *revisionBackendsManager) getOrCreateRevisionWatcher(rev types.NamespacedName) (*revisionWatcher, error) {
	rbm.revisionWatchersMux.Lock()
	defer rbm.revisionWatchersMux.Unlock()

	rwCh, ok := rbm.revisionWatchers[rev]
	if !ok {
		proto, err := rbm.getRevisionProtocol(rev)
		if err != nil {
			return nil, err
		}

		destsCh := make(chan sets.String)
		rw := newRevisionWatcher(rbm.ctx, rev, proto, rbm.updateCh, destsCh, rbm.transport, rbm.serviceLister, rbm.logger)
		rbm.revisionWatchers[rev] = rw
		go rw.run(rbm.probeFrequency)
		return rw, nil
	}

	return rwCh, nil
}

// endpointsUpdated is a handler function to be used by the Endpoints informer.
// It updates the endpoints in the RevisionBackendsManager if the hosts changed
func (rbm *revisionBackendsManager) endpointsUpdated(newObj interface{}) {
	// Ignore the updates when we've terminated.
	select {
	case <-rbm.ctx.Done():
		return
	default:
	}
	rbm.logger.Debugf("Endpoints updated: %#v", newObj)
	endpoints := newObj.(*corev1.Endpoints)
	revID := types.NamespacedName{endpoints.Namespace, endpoints.Labels[serving.RevisionLabelKey]}

	rw, err := rbm.getOrCreateRevisionWatcher(revID)
	if err != nil {
		rbm.logger.With(zap.Error(err)).Error("Failed to get revision watcher for revision %q", revID.String())
		return
	}
	dests := endpointsToDests(endpoints, networking.ServicePortName(rw.protocol))
	rbm.logger.Debugf("Updating Endpoints: %q (backends: %d)", revID.String(), len(dests))
	rw.destsCh <- dests
}

// deleteRevisionWatcher deletes the revision watcher for rev if it exists. It expects
// a write lock is held on revisionWatchersMux when calling.
func (rbm *revisionBackendsManager) deleteRevisionWatcher(rev types.NamespacedName) {
	if rw, ok := rbm.revisionWatchers[rev]; ok {
		rw.cancel()
		delete(rbm.revisionWatchers, rev)
	}
}

func (rbm *revisionBackendsManager) endpointsDeleted(obj interface{}) {
	// Ignore the updates when we've terminated.
	select {
	case <-rbm.ctx.Done():
		return
	default:
	}
	ep := obj.(*corev1.Endpoints)
	revID := types.NamespacedName{ep.Namespace, ep.Labels[serving.RevisionLabelKey]}

	rbm.logger.Debugf("Deleting endpoint %q", revID.String())
	rbm.revisionWatchersMux.Lock()
	defer rbm.revisionWatchersMux.Unlock()
	rbm.deleteRevisionWatcher(revID)
}
