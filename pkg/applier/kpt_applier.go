// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package applier

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/GoogleContainerTools/kpt/pkg/live"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/klog/v2"
	"kpt.dev/configsync/pkg/api/configmanagement"
	"kpt.dev/configsync/pkg/api/configsync"
	"kpt.dev/configsync/pkg/core"
	"kpt.dev/configsync/pkg/declared"
	"kpt.dev/configsync/pkg/kinds"
	"kpt.dev/configsync/pkg/metadata"
	m "kpt.dev/configsync/pkg/metrics"
	"kpt.dev/configsync/pkg/resourcegroup"
	"kpt.dev/configsync/pkg/status"
	"kpt.dev/configsync/pkg/syncer/differ"
	"kpt.dev/configsync/pkg/syncer/metrics"
	"kpt.dev/configsync/pkg/util"
	"sigs.k8s.io/cli-utils/pkg/apis/actuation"
	"sigs.k8s.io/cli-utils/pkg/apply"
	applyerror "sigs.k8s.io/cli-utils/pkg/apply/error"
	"sigs.k8s.io/cli-utils/pkg/apply/event"
	"sigs.k8s.io/cli-utils/pkg/apply/filter"
	"sigs.k8s.io/cli-utils/pkg/common"
	"sigs.k8s.io/cli-utils/pkg/inventory"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	// maxRequestBytesStr defines the max request bytes on the etcd server.
	// It is defined in https://github.com/etcd-io/etcd/blob/release-3.4/embed/config.go#L56
	maxRequestBytesStr = "1.5M"

	// maxRequestBytes defines the max request bytes on the etcd server.
	// It is defined in https://github.com/etcd-io/etcd/blob/release-3.4/embed/config.go#L56
	maxRequestBytes = int64(1.5 * 1024 * 1024)
)

// Applier declares the Applier component in the Multi Repo Reconciler Process.
type Applier struct {
	// inventory policy for the applier.
	policy inventory.Policy
	// inventory is the inventory ResourceGroup for current Applier.
	inventory *live.InventoryResourceGroup
	// clientSetFunc is the function to create kpt clientSet.
	// Use this as a function so that the unit testing can mock
	// the clientSet.
	clientSetFunc func(client.Client, *genericclioptions.ConfigFlags, string) (*clientSet, error)
	// client get and updates RepoSync and its status.
	client client.Client
	// configFlags for creating clients
	configFlags *genericclioptions.ConfigFlags
	// errs tracks all the errors the applier encounters.
	// This field is cleared at the start of the `Applier.Apply` method
	errs status.MultiError
	// syncing indicates whether the applier is syncing.
	syncing bool
	// name and namespace of the RootSync|RepoSync object
	// for the current applier.
	syncName      string
	syncNamespace string
	// mux is an Applier-level mutext to prevent concurrent Apply() and Refresh()
	mux sync.Mutex
	// statusMode controls if the applier injects the acutation status into the
	// ResourceGroup object
	statusMode string
	// reconcileTimeout controls the reconcile and prune timeout
	reconcileTimeout time.Duration
}

// Interface is a fake-able subset of the interface Applier implements.
//
// Placed here to make discovering the production implementation (above) easier.
type Interface interface {
	// Apply updates the resource API server with the latest parsed git resource.
	// This is called when a new change in the git resource is detected. It also
	// returns a map of the GVKs which were successfully applied by the Applier.
	Apply(ctx context.Context, desiredResources []client.Object) (map[schema.GroupVersionKind]struct{}, status.MultiError)
	// Errors returns the errors encountered during apply.
	Errors() status.MultiError
	// Syncing indicates whether the applier is syncing.
	Syncing() bool
}

var _ Interface = &Applier{}

// NewNamespaceApplier initializes an applier that fetches a certain namespace's resources from
// the API server.
func NewNamespaceApplier(c client.Client, configFlags *genericclioptions.ConfigFlags, namespace declared.Scope, syncName string, statusMode string, reconcileTimeout time.Duration) (*Applier, error) {
	u := newInventoryUnstructured(syncName, string(namespace), statusMode)
	// If the ResourceGroup object exists, annotate the status mode on the
	// existing object.
	if err := annotateStatusMode(context.TODO(), c, u, statusMode); err != nil {
		klog.Errorf("failed to annotate the ResourceGroup object with the status mode %s", statusMode)
		return nil, err
	}
	klog.Infof("successfully annotate the ResourceGroup object with the status mode %s", statusMode)
	inv, err := wrapInventoryObj(u)
	if err != nil {
		return nil, err
	}
	a := &Applier{
		inventory:        inv,
		client:           c,
		configFlags:      configFlags,
		clientSetFunc:    newClientSet,
		policy:           inventory.PolicyAdoptIfNoInventory,
		syncName:         syncName,
		syncNamespace:    string(namespace),
		statusMode:       statusMode,
		reconcileTimeout: reconcileTimeout,
	}
	klog.V(4).Infof("Applier %s/%s is initialized", namespace, syncName)
	return a, nil
}

// NewRootApplier initializes an applier that can fetch all resources from the API server.
func NewRootApplier(c client.Client, configFlags *genericclioptions.ConfigFlags, syncName, statusMode string, reconcileTimeout time.Duration) (*Applier, error) {
	u := newInventoryUnstructured(syncName, configmanagement.ControllerNamespace, statusMode)
	// If the ResourceGroup object exists, annotate the status mode on the
	// existing object.
	if err := annotateStatusMode(context.TODO(), c, u, statusMode); err != nil {
		klog.Errorf("failed to annotate the ResourceGroup object with the status mode %s", statusMode)
		return nil, err
	}
	klog.Infof("successfully annotate the ResourceGroup object with the status mode %s", statusMode)
	inv, err := wrapInventoryObj(u)
	if err != nil {
		return nil, err
	}
	a := &Applier{
		inventory:        inv,
		client:           c,
		configFlags:      configFlags,
		clientSetFunc:    newClientSet,
		policy:           inventory.PolicyAdoptAll,
		statusMode:       statusMode,
		reconcileTimeout: reconcileTimeout,
	}
	klog.V(4).Infof("Root applier %s is initialized and synced with the API server", syncName)
	return a, nil
}

func wrapInventoryObj(obj *unstructured.Unstructured) (*live.InventoryResourceGroup, error) {
	inv, ok := live.WrapInventoryObj(obj).(*live.InventoryResourceGroup)
	if !ok {
		return nil, errors.New("failed to create an ResourceGroup object")
	}
	return inv, nil
}

func processApplyEvent(ctx context.Context, e event.ApplyEvent, stats *applyEventStats, unknownTypeResources map[core.ID]struct{}) status.Error {
	id := idFrom(e.Identifier)
	klog.V(4).Infof("apply %v for object: %v", e.Status, id)
	stats.eventByOp[e.Status]++

	switch e.Status {
	case event.ApplyPending:
		// ignore
		return nil

	case event.ApplySuccessful:
		handleMetrics(ctx, "update", e.Error, id.WithVersion(""))
		return nil

	case event.ApplyFailed:
		switch e.Error.(type) {
		case *applyerror.UnknownTypeError:
			unknownTypeResources[id] = struct{}{}
			return ErrorForResource(e.Error, id)
		default:
			return ErrorForResource(e.Error, id)
		}

	case event.ApplySkipped:
		// Skip event always includes an error with the reason
		return handleApplySkippedEvent(e.Resource, id, e.Error)

	default:
		return ErrorForResource(fmt.Errorf("unexpected prune event status: %v", e.Status), id)
	}
}

func processWaitEvent(e event.WaitEvent, objReconciled map[core.ID]struct{}) {
	id := idFrom(e.Identifier)
	if e.Status == event.ReconcileSuccessful {
		objReconciled[id] = struct{}{}
	}
}

// handleApplySkippedEvent translates from apply skipped event into resource error.
func handleApplySkippedEvent(obj *unstructured.Unstructured, id core.ID, err error) status.Error {
	var depErr *filter.DependencyPreventedActuationError
	if errors.As(err, &depErr) {
		return SkipErrorForResource(err, id, depErr.Strategy)
	}

	var depMismatchErr *filter.DependencyActuationMismatchError
	if errors.As(err, &depMismatchErr) {
		return SkipErrorForResource(err, id, depMismatchErr.Strategy)
	}

	var policyErr *inventory.PolicyPreventedActuationError
	if errors.As(err, &policyErr) {
		// TODO: return ManagementConflictError with the conflicting manager if
		// cli-utils supports reporting the conflicting manager in
		// PolicyPreventedActuationError.
		// return SkipErrorForResource(err, id, policyErr.Strategy)
		return KptManagementConflictError(obj)
	}

	return SkipErrorForResource(err, id, actuation.ActuationStrategyApply)
}

func processPruneEvent(ctx context.Context, e event.PruneEvent, stats *pruneEventStats, cs *clientSet) status.Error {
	id := idFrom(e.Identifier)
	klog.V(4).Infof("prune %v for object: %v", e.Status, id)
	stats.eventByOp[e.Status]++

	switch e.Status {
	case event.PrunePending:
		// ignore
		return nil

	case event.PruneSuccessful:
		handleMetrics(ctx, "delete", e.Error, id.WithVersion(""))
		return nil

	case event.PruneFailed:
		return PruneErrorForResource(e.Error, id)

	case event.PruneSkipped:
		if isNamespace(e.Object) && differ.SpecialNamespaces[e.Object.GetName()] {
			// the `client.lifecycle.config.k8s.io/deletion: detach` annotation is not a part of the Config Sync metadata, and will not be removed here.
			err := cs.disableObject(ctx, e.Object)
			if err != nil {
				errorMsg := "failed to remove the Config Sync metadata from %v (which is a special namespace): %v"
				klog.Errorf(errorMsg, id, err)
				return applierErrorBuilder.Wrap(fmt.Errorf(errorMsg, id, err)).Build()
			}
			klog.V(4).Infof("removed the Config Sync metadata from %v (which is a special namespace)", id)
		}
		// Skip event always includes an error with the reason
		return handlePruneSkippedEvent(e.Object, id, e.Error)

	default:
		return PruneErrorForResource(fmt.Errorf("unexpected prune event status: %v", e.Status), id)
	}
}

// handlePruneSkippedEvent translates from prune skipped event into resource error.
func handlePruneSkippedEvent(obj *unstructured.Unstructured, id core.ID, err error) status.Error {
	var depErr *filter.DependencyPreventedActuationError
	if errors.As(err, &depErr) {
		return SkipErrorForResource(err, id, depErr.Strategy)
	}

	var depMismatchErr *filter.DependencyActuationMismatchError
	if errors.As(err, &depMismatchErr) {
		return SkipErrorForResource(err, id, depMismatchErr.Strategy)
	}

	var applyDeleteErr *filter.ApplyPreventedDeletionError
	if errors.As(err, &applyDeleteErr) {
		return SkipErrorForResource(err, id, actuation.ActuationStrategyDelete)
	}

	var policyErr *inventory.PolicyPreventedActuationError
	if errors.As(err, &policyErr) {
		// For prunes, this is desired behavior, not a fatal error.
		klog.Infof("Resource object removed from inventory,  but not deleted: %v: %v", id, err)
		return nil
	}

	var namespaceErr *filter.NamespaceInUseError
	if errors.As(err, &namespaceErr) {
		return SkipErrorForResource(err, id, actuation.ActuationStrategyDelete)
	}

	var abandonErr *filter.AnnotationPreventedDeletionError
	if errors.As(err, &abandonErr) {
		// For prunes, this is desired behavior, not a fatal error.
		klog.Infof("Resource object removed from inventory, but not deleted: %v: %v", id, err)
		return nil
	}

	return SkipErrorForResource(err, id, actuation.ActuationStrategyDelete)
}

func isNamespace(obj *unstructured.Unstructured) bool {
	return obj.GetObjectKind().GroupVersionKind().GroupKind() == kinds.Namespace().GroupKind()
}

func handleMetrics(ctx context.Context, operation string, err error, gvk schema.GroupVersionKind) {
	// TODO capture the apply duration in the kpt apply library.
	start := time.Now()

	m.RecordAPICallDuration(ctx, operation, m.StatusTagKey(err), gvk, start)
	metrics.Operations.WithLabelValues(operation, gvk.Kind, metrics.StatusLabel(err)).Inc()
	m.RecordApplyOperation(ctx, operation, m.StatusTagKey(err), gvk)
}

// checkInventoryObjectSize checks the inventory object size limit.
// If it is close to the size limit 1M, log a warning.
func (a *Applier) checkInventoryObjectSize(ctx context.Context, c client.Client) {
	u := newInventoryUnstructured(a.syncName, a.syncNamespace, a.statusMode)
	err := c.Get(ctx, client.ObjectKey{Namespace: a.syncNamespace, Name: a.syncName}, u)
	if err == nil {
		size, err := getObjectSize(u)
		if err != nil {
			klog.Warningf("Failed to marshal ResourceGroup %s/%s to get its size: %s", a.syncNamespace, a.syncName, err)
		}
		if int64(size) > maxRequestBytes/2 {
			klog.Warningf("ResourceGroup %s/%s is close to the maximum object size limit (size: %d, max: %s). "+
				"There are too many resources being synced than Config Sync can handle! Please split your repo into smaller repos "+
				"to avoid future failure.", a.syncNamespace, a.syncName, size, maxRequestBytesStr)
		}
	}
}

// sync triggers a kpt live apply library call to apply a set of resources.
func (a *Applier) sync(ctx context.Context, objs []client.Object) (map[schema.GroupVersionKind]struct{}, status.MultiError) {
	cs, err := a.clientSetFunc(a.client, a.configFlags, a.statusMode)
	if err != nil {
		return nil, Error(err)
	}
	a.checkInventoryObjectSize(ctx, cs.client)

	stats := newApplyStats()
	// disabledObjs are objects for which the management are disabled
	// through annotation.
	enabledObjs, disabledObjs := partitionObjs(objs)
	if len(disabledObjs) > 0 {
		klog.Infof("%v objects to be disabled: %v", len(disabledObjs), core.GKNNs(disabledObjs))
		disabledCount, err := cs.handleDisabledObjects(ctx, a.inventory, disabledObjs)
		if err != nil {
			a.errs = status.Append(a.errs, err)
			return nil, a.errs
		}
		stats.disableObjs = disabledObjStats{
			total:     uint64(len(disabledObjs)),
			succeeded: disabledCount,
		}
	}
	klog.Infof("%v objects to be applied: %v", len(enabledObjs), core.GKNNs(enabledObjs))
	resources, toUnsErrs := toUnstructured(enabledObjs)
	if toUnsErrs != nil {
		return nil, toUnsErrs
	}

	unknownTypeResources := make(map[core.ID]struct{})
	options := apply.ApplierOptions{
		ServerSideOptions: common.ServerSideOptions{
			ServerSideApply: true,
			ForceConflicts:  true,
			FieldManager:    configsync.FieldManager,
		},
		InventoryPolicy: a.policy,
		// Leaving ReconcileTimeout and PruneTimeout unset may cause a WaitTask to wait forever.
		// ReconcileTimeout defines the timeout for a wait task after an apply task.
		// ReconcileTimeout is a task-level setting instead of an object-level setting.
		ReconcileTimeout: a.reconcileTimeout,
		// PruneTimeout defines the timeout for a wait task after a prune task.
		// PruneTimeout is a task-level setting instead of an object-level setting.
		PruneTimeout: a.reconcileTimeout,
	}

	// Reset shared mapper before each apply to invalidate the discovery cache.
	// This allows for picking up CRD changes.
	mapper, err := a.configFlags.ToRESTMapper()
	if err != nil {
		return nil, status.Append(nil, err)
	}
	meta.MaybeResetRESTMapper(mapper)

	events := cs.apply(ctx, a.inventory, resources, options)
	for e := range events {
		switch e.Type {
		case event.InitType:
			for _, ag := range e.InitEvent.ActionGroups {
				klog.Info("InitEvent", ag)
			}
		case event.ActionGroupType:
			klog.Info(e.ActionGroupEvent)
		case event.ErrorType:
			klog.V(4).Info(e.ErrorEvent)
			if util.IsRequestTooLargeError(e.ErrorEvent.Err) {
				a.errs = status.Append(a.errs, largeResourceGroupError(e.ErrorEvent.Err, idFromInventory(a.inventory)))
			} else {
				a.errs = status.Append(a.errs, Error(e.ErrorEvent.Err))
			}
			stats.errorTypeEvents++
		case event.WaitType:
			// Log WaitEvent at the verbose level of 4 due to the number of WaitEvent.
			// For every object which is skipped to apply/prune, there will be one ReconcileSkipped WaitEvent.
			// For every object which is not skipped to apply/prune, there will be at least two WaitEvent:
			// one ReconcilePending WaitEvent and one Reconciled/ReconcileFailed/ReconcileTimeout WaitEvent. In addition,
			// a reconciled object may become pending before a wait task times out.
			// Record the objs that have been reconciled.
			klog.V(4).Info(e.WaitEvent)
			processWaitEvent(e.WaitEvent, stats.objsReconciled)
		case event.ApplyType:
			logEvent := event.ApplyEvent{
				GroupName:  e.ApplyEvent.GroupName,
				Identifier: e.ApplyEvent.Identifier,
				Status:     e.ApplyEvent.Status,
				// nil Resource to reduce log noise
				Error: e.ApplyEvent.Error,
			}
			klog.V(4).Info(logEvent)
			a.errs = status.Append(a.errs, processApplyEvent(ctx, e.ApplyEvent, &stats.applyEvent, unknownTypeResources))
		case event.PruneType:
			logEvent := event.PruneEvent{
				GroupName:  e.PruneEvent.GroupName,
				Identifier: e.PruneEvent.Identifier,
				Status:     e.PruneEvent.Status,
				// nil Resource to reduce log noise
				Error: e.PruneEvent.Error,
			}
			klog.V(4).Info(logEvent)
			a.errs = status.Append(a.errs, processPruneEvent(ctx, e.PruneEvent, &stats.pruneEvent, cs))
		default:
			klog.V(4).Infof("skipped %v event", e.Type)
		}
	}

	gvks := make(map[schema.GroupVersionKind]struct{})
	for _, resource := range objs {
		id := core.IDOf(resource)
		if _, found := unknownTypeResources[id]; found {
			continue
		}
		gvks[resource.GetObjectKind().GroupVersionKind()] = struct{}{}
	}
	if a.errs == nil {
		klog.V(4).Infof("all resources are up to date.")
	}

	if stats.empty() {
		klog.V(4).Infof("The applier made no new progress")
	} else {
		klog.Infof("The applier made new progress: %s.", stats.string())
	}
	return gvks, a.errs
}

// Errors implements Interface.
// Errors returns the errors encountered during apply.
func (a *Applier) Errors() status.MultiError {
	return a.errs
}

// Syncing implements Interface.
// Syncing returns whether the applier is syncing.
func (a *Applier) Syncing() bool {
	return a.syncing
}

// Apply implements Interface.
func (a *Applier) Apply(ctx context.Context, desiredResource []client.Object) (map[schema.GroupVersionKind]struct{}, status.MultiError) {
	// Clear the `errs` field at the start.
	a.errs = nil
	// Set the `syncing` field to `true` at the start.
	a.syncing = true

	defer func() {
		// Make sure to clear the `syncing` field before `Apply` returns.
		a.syncing = false
	}()

	a.mux.Lock()
	defer a.mux.Unlock()

	// Pull the actual resources from the API server to compare against the
	// declared resources. Note that we do not immediately return on error here
	// because the Applier needs to try to do as much work as it can on each
	// cycle. We collect and return all errors at the end. Some of those errors
	// are transient and resolve in future cycles based on partial work completed
	// in a previous cycle (eg ignore an error about a CR so that we can apply the
	// CRD, then a future cycle is able to apply the CR).
	// TODO: Here and elsewhere, pass the MultiError as a parameter.
	return a.sync(ctx, desiredResource)
}

// newInventoryUnstructured creates an inventory object as an unstructured.
func newInventoryUnstructured(name, namespace, statusMode string) *unstructured.Unstructured {
	id := InventoryID(name, namespace)
	u := resourcegroup.Unstructured(name, namespace, id)
	core.SetLabel(u, metadata.ManagedByKey, metadata.ManagedByValue)
	core.SetLabel(u, metadata.SyncNamespaceLabel, namespace)
	core.SetLabel(u, metadata.SyncNameLabel, name)
	core.SetAnnotation(u, metadata.ResourceManagementKey, metadata.ResourceManagementEnabled)
	core.SetAnnotation(u, StatusModeKey, statusMode)
	return u
}

// InventoryID returns the inventory id of an inventory object.
// The inventory object generated by ConfigSync is in the same namespace as RootSync or RepoSync.
// The inventory ID is assigned as <NAMESPACE>_<NAME>.
func InventoryID(name, namespace string) string {
	return namespace + "_" + name
}
