/*
Copyright 2022.

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

package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
	tsuruErrors "github.com/tsuru/tsuru/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/tsuru/acl-operator/api/v1alpha1"
	"github.com/tsuru/acl-operator/clients/tsuruapi"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var requeueAfter = time.Minute * 10

// ACLReconciler reconciles a ACL object
type ACLReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	TsuruAPI tsuruapi.Client
	Resolver ACLDNSResolver

	serviceCache atomic.Pointer[serviceCache]
}

//+kubebuilder:rbac:groups=extensions.tsuru.io,resources=acls,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=extensions.tsuru.io,resources=acls/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=extensions.tsuru.io,resources=acls/finalizers,verbs=update

func (r *ACLReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	acl := &v1alpha1.ACL{}
	err := r.Client.Get(ctx, req.NamespacedName, acl)
	if k8sErrors.IsNotFound(err) {
	} else if err != nil {
		l.Error(err, "could not get ACL object")
		return ctrl.Result{}, err
	}

	networkPolicy := &netv1.NetworkPolicy{}
	networkPolicyName := acl.Status.NetworkPolicy
	if networkPolicyName == "" {
		networkPolicyName = "acl-" + req.Name
	}

	err = r.Client.Get(ctx, client.ObjectKey{
		Namespace: req.Namespace,
		Name:      networkPolicyName,
	}, networkPolicy)

	if k8sErrors.IsNotFound(err) {
	} else if err != nil {
		l.Error(err, "could not get NetworkPolicy object")
		return ctrl.Result{}, err
	}

	networkPolicyHasChanges := false
	statusNeedsUpdate := false
	networkPolicy.ObjectMeta.Namespace = acl.ObjectMeta.Namespace
	networkPolicy.ObjectMeta.Name = networkPolicyName

	if len(networkPolicy.OwnerReferences) == 0 {
		networkPolicy.OwnerReferences = []v1.OwnerReference{
			*metav1.NewControllerRef(acl, acl.GroupVersionKind()),
		}

		networkPolicyHasChanges = true
	}

	if (len(networkPolicy.Spec.PolicyTypes) == 1 && networkPolicy.Spec.PolicyTypes[0] != netv1.PolicyTypeEgress) || len(networkPolicy.Spec.PolicyTypes) != 1 {
		networkPolicy.Spec.PolicyTypes = []netv1.PolicyType{
			netv1.PolicyTypeEgress,
		}
		networkPolicyHasChanges = true
	}

	podSelector := r.podSelectorForSource(acl.Spec.Source)
	if podSelector == nil {
		err = r.setUnreadyStatus(ctx, acl, "No podSelector generated by spec.source")
		return ctrl.Result{}, err
	}

	if !reflect.DeepEqual(networkPolicy.Spec.PodSelector.MatchLabels, podSelector) {
		networkPolicy.Spec.PodSelector.MatchLabels = podSelector
		networkPolicyHasChanges = true
	}

	newEgressRules := []netv1.NetworkPolicyEgressRule{}
	for _, destination := range acl.Spec.Destinations {
		egressRules, err := r.egressRulesForDestination(ctx, destination)
		// TODO: think about inconsistences, or temporarrly inconsistences
		if err != nil {
			destinationJSON, _ := json.Marshal(destination)
			l.Error(err, "could not generate egress rule for destination", "destination", string(destinationJSON))
			err = r.setUnreadyStatus(ctx, acl, "could not generate egress rule for destination "+string(destinationJSON)+", err: "+err.Error())
			return ctrl.Result{}, err
		}
		// TODO: check IPBlock Rules for translate also into kubernetes labels
		newEgressRules = append(newEgressRules, egressRules...)
	}

	err = r.fillPodSelectorByCIDR(ctx, newEgressRules)
	if err != nil {
		l.Error(err, "could not generate egress rule based on kubernetes selector", "destination")
		err = r.setUnreadyStatus(ctx, acl, "could not generate egress rule based on kubernetes selector, err: "+err.Error())
		return ctrl.Result{}, err
	}

	if len(newEgressRules) == 0 {
		err = r.setUnreadyStatus(ctx, acl, "No egress generated by spec.destinations")
		return ctrl.Result{}, err
	}

	if !reflect.DeepEqual(networkPolicy.Spec.Egress, newEgressRules) {
		networkPolicy.Spec.Egress = newEgressRules
		networkPolicyHasChanges = true
	}

	if networkPolicy.CreationTimestamp.IsZero() {
		err = r.Client.Create(ctx, networkPolicy)
		if err != nil {
			l.Error(err, "could not create NetworkPolicy object")
			return ctrl.Result{}, err
		}
		l.Info("NetworkPolicy object has been created")

		acl.Status.NetworkPolicy = networkPolicy.Name
		acl.Status.Ready = true
		acl.Status.Reason = ""
		statusNeedsUpdate = true

	} else if networkPolicyHasChanges {
		err = r.Client.Update(ctx, networkPolicy)
		if err != nil {
			l.Error(err, "could not update NetworkPolicy object")
			return ctrl.Result{}, err
		}

		l.Info("NetworkPolicy object has been updated")

		acl.Status.NetworkPolicy = networkPolicy.Name
		acl.Status.Ready = true
		acl.Status.Reason = ""
		statusNeedsUpdate = true
	}

	if statusNeedsUpdate {
		err = r.Client.Status().Update(ctx, acl)
		if err != nil {
			l.Error(err, "could not update status for ACL object")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{
		Requeue:      true,
		RequeueAfter: requeueAfter,
	}, nil
}

func (r *ACLReconciler) setUnreadyStatus(ctx context.Context, acl *v1alpha1.ACL, reason string) error {
	l := log.FromContext(ctx)

	acl.Status.Ready = false
	acl.Status.Reason = reason

	err := r.Client.Status().Update(ctx, acl)
	if err != nil {
		l.Error(err, "could not update acl status")
	}
	return err
}

func (r *ACLReconciler) podSelectorForSource(source v1alpha1.ACLSpecSource) map[string]string {
	if source.TsuruApp != "" {
		return r.podSelectorForTsuruApp(source.TsuruApp)
	}

	if source.RpaasInstance != nil {
		return r.podSelectorForRpasInstance(source.RpaasInstance)
	}

	return nil
}

func (r *ACLReconciler) egressRulesForDestination(ctx context.Context, destination v1alpha1.ACLSpecDestination) ([]netv1.NetworkPolicyEgressRule, error) {
	if destination.TsuruApp != "" {
		return r.egressRulesForTsuruApp(ctx, destination.TsuruApp)
	} else if destination.TsuruAppPool != "" {
		return r.egressRulesForTsuruAppPool(ctx, destination.TsuruAppPool)
	} else if destination.ExternalDNS != nil {
		return r.egressRulesForExternalDNS(ctx, destination.ExternalDNS)
	} else if destination.ExternalIP != nil {
		return r.egressRulesForExternalIP(ctx, destination.ExternalIP)
	} else if destination.RpaasInstance != nil {
		return r.egressRulesForRpaasInstance(ctx, destination.RpaasInstance)
	}
	return nil, nil
}

func (r *ACLReconciler) egressRulesForTsuruApp(ctx context.Context, tsuruApp string) ([]netv1.NetworkPolicyEgressRule, error) {
	l := log.FromContext(ctx)

	allErrors := &tsuruErrors.MultiError{}
	egress := []netv1.NetworkPolicyEgressRule{
		{
			To: []netv1.NetworkPolicyPeer{
				{
					PodSelector: &metav1.LabelSelector{
						MatchLabels: r.podSelectorForTsuruApp(tsuruApp),
					},
					// NamespaceSelector: nil, TODO: use namespace selector, the major advantage is to reduce number of pods processed by calico
				},
			},
		},
	}

	existingTsuruAppAddress, err := r.ensureTsuruAppAddress(ctx, tsuruApp)

	if err != nil {
		l.Error(err, "could not get TsuruAppAddress", "appName", tsuruApp)
		return nil, err
	}

	resourceEgress, errors := r.egressRulesForResourceAddressStatus(ctx, existingTsuruAppAddress.Status)
	egress = append(egress, resourceEgress...)
	for _, err := range errors {
		allErrors.Add(err)
	}

	return egress, allErrors.ToError()
}

func (r *ACLReconciler) egressRulesForResourceAddressStatus(ctx context.Context, status v1alpha1.ResourceAddressStatus) ([]netv1.NetworkPolicyEgressRule, []error) {
	errs := []error{}
	egresses := []netv1.NetworkPolicyEgressRule{}

	for _, routerIP := range status.IPs {
		addrEgresses, err := r.egressRulesForExternalIP(ctx, &v1alpha1.ACLSpecExternalIP{
			IP: routerIP,
		})

		if err != nil {
			errs = append(errs, errors.Wrapf(err, "could not generate egress rule for: %q", routerIP))
		}

		egresses = append(egresses, addrEgresses...)
	}

	return egresses, errs
}

func (r *ACLReconciler) egressRulesForTsuruAppPool(ctx context.Context, tsuruAppPool string) ([]netv1.NetworkPolicyEgressRule, error) {
	egress := []netv1.NetworkPolicyEgressRule{
		{
			To: []netv1.NetworkPolicyPeer{
				{
					PodSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"tsuru.io/app-pool": tsuruAppPool,
						},
					},
					// NamespaceSelector: nil, TODO: use namespace selector, the major advantage is to reduce number of pods processed by calico
				},
			},
		},
	}

	return egress, nil
}

func (r *ACLReconciler) egressRulesForExternalDNS(ctx context.Context, externalDNS *v1alpha1.ACLSpecExternalDNS) ([]netv1.NetworkPolicyEgressRule, error) {
	l := log.FromContext(ctx)

	if isWildCard(externalDNS.Name) {
		return nil, nil
	}

	existingDNSEntry, err := r.ensureDNSEntry(ctx, externalDNS.Name)

	if err != nil {
		l.Error(err, "could not get ACLDNSEntry", "destination", externalDNS.Name)
		return nil, err
	}

	if !existingDNSEntry.Status.Ready {
		l.Info("DNSEntry is not ready yet")
		return nil, nil
	}

	to := []netv1.NetworkPolicyPeer{}
	for _, ip := range existingDNSEntry.Status.IPs {

		var cidr string
		if strings.Contains(ip.Address, ":") {
			cidr = ip.Address + "/128"
		} else if strings.Contains(ip.Address, ".") {
			cidr = ip.Address + "/32"
		} else {
			continue
		}

		to = append(to, netv1.NetworkPolicyPeer{IPBlock: &netv1.IPBlock{
			CIDR: cidr,
		}})
	}

	egress := []netv1.NetworkPolicyEgressRule{
		{
			To:    to,
			Ports: r.ports(externalDNS.Ports),
		},
	}

	return egress, nil
}

func (r *ACLReconciler) egressRulesForExternalIP(ctx context.Context, externalIP *v1alpha1.ACLSpecExternalIP) ([]netv1.NetworkPolicyEgressRule, error) {
	var cidr string

	cidr = externalIP.IP
	if !strings.Contains(cidr, "/") {
		if strings.Contains(cidr, ":") {
			cidr = cidr + "/128"
		} else if strings.Contains(cidr, ".") {
			cidr = cidr + "/32"
		}
	}

	egress := []netv1.NetworkPolicyEgressRule{
		{
			To: []netv1.NetworkPolicyPeer{
				{
					IPBlock: &netv1.IPBlock{
						CIDR: cidr,
					},
				},
			},
			Ports: r.ports(externalIP.Ports),
		},
	}

	return egress, nil
}

func (r *ACLReconciler) egressRulesForRpaasInstance(ctx context.Context, rpaasInstance *v1alpha1.ACLSpecRpaasInstance) ([]netv1.NetworkPolicyEgressRule, error) {
	l := log.FromContext(ctx)

	allErrors := &tsuruErrors.MultiError{}
	egress := []netv1.NetworkPolicyEgressRule{
		{
			To: []netv1.NetworkPolicyPeer{
				{
					PodSelector: &metav1.LabelSelector{
						MatchLabels: r.podSelectorForRpasInstance(rpaasInstance),
					},
					// NamespaceSelector: nil, TODO: use namespace selector, the major advantage is to reduce number of pods processed by calico
				},
			},
		},
	}

	existingRpaasInstanceAddress, err := r.ensureRpaasInstanceAddress(ctx, rpaasInstance)

	if err != nil {
		l.Error(err, "could not get RpaasInstanceAddress",
			"rpaasInstance", rpaasInstance.Instance,
			"rpaasService", rpaasInstance.ServiceName,
		)
		return nil, err
	}

	resourceEgress, errors := r.egressRulesForResourceAddressStatus(ctx, existingRpaasInstanceAddress.Status)
	egress = append(egress, resourceEgress...)
	for _, err := range errors {
		allErrors.Add(err)
	}

	return egress, allErrors.ToError()
}

func (r *ACLReconciler) ensureDNSEntry(ctx context.Context, host string) (*v1alpha1.ACLDNSEntry, error) {
	l := log.FromContext(ctx)

	existingDNSEntry := &v1alpha1.ACLDNSEntry{}

	resourceName := validResourceName(host)
	err := r.Client.Get(ctx, types.NamespacedName{
		Name: resourceName,
	}, existingDNSEntry)

	if k8sErrors.IsNotFound(err) {
		dnsEntry := &v1alpha1.ACLDNSEntry{
			ObjectMeta: metav1.ObjectMeta{
				Name: resourceName,
			},
			Spec: v1alpha1.ACLDNSEntrySpec{
				Host: host,
			},
		}

		err = r.Client.Create(ctx, dnsEntry)
		if err != nil {
			l.Error(err, "could not create ACLDNSEntry object")
			return nil, err
		}

		subReconciler := &ACLDNSEntryReconciler{
			Client:   r.Client,
			Scheme:   r.Scheme,
			Resolver: r.Resolver,
		}

		_, err = subReconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name: dnsEntry.Name,
			},
		})

		if err != nil {
			l.Error(err, "could not sub-reconcicle DNSEntry", "dnsEntryName", resourceName)
			return nil, err
		}

		err = r.Client.Get(ctx, types.NamespacedName{
			Name: resourceName,
		}, existingDNSEntry)
		return existingDNSEntry, err
	} else if err != nil {
		l.Error(err, "could not get ACLDNSEntry", "dnsEntryName", resourceName)
		return nil, err
	}

	return existingDNSEntry, nil
}

func (r *ACLReconciler) ensureTsuruAppAddress(ctx context.Context, appName string) (*v1alpha1.TsuruAppAddress, error) {
	l := log.FromContext(ctx)

	existingTsuruAppAddress := &v1alpha1.TsuruAppAddress{}
	resourceName := validResourceName(appName)
	err := r.Client.Get(ctx, types.NamespacedName{
		Name: resourceName,
	}, existingTsuruAppAddress)

	if k8sErrors.IsNotFound(err) {
		tsuruAppAddress := &v1alpha1.TsuruAppAddress{
			ObjectMeta: metav1.ObjectMeta{
				Name: resourceName,
			},
			Spec: v1alpha1.TsuruAppAddressSpec{
				Name: resourceName,
			},
		}

		err = r.Client.Create(ctx, tsuruAppAddress)
		if err != nil {
			l.Error(err, "could not create ACLDNSEntry object")
			return nil, err
		}

		subReconciler := &TsuruAppAddressReconciler{
			Client:   r.Client,
			Scheme:   r.Scheme,
			Resolver: r.Resolver,
			TsuruAPI: r.TsuruAPI,
		}

		_, err = subReconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name: tsuruAppAddress.Name,
			},
		})

		if err != nil {
			l.Error(err, "could not sub-reconcicle TsuruAppAddress", "tsuruAppName", resourceName)
			return nil, err
		}

		err = r.Client.Get(ctx, types.NamespacedName{
			Name: resourceName,
		}, existingTsuruAppAddress)
		return existingTsuruAppAddress, err
	} else if err != nil {
		l.Error(err, "could not get TsuruAppAddress", "tsuruAppName", resourceName)
		return nil, err
	}

	return existingTsuruAppAddress, nil
}

func (r *ACLReconciler) ensureRpaasInstanceAddress(ctx context.Context, rpaasInstance *v1alpha1.ACLSpecRpaasInstance) (*v1alpha1.RpaasInstanceAddress, error) {
	l := log.FromContext(ctx)

	existingRpaasInstanceAddress := &v1alpha1.RpaasInstanceAddress{}
	resourceName := validResourceName(rpaasInstance.ServiceName + "-" + rpaasInstance.Instance)
	err := r.Client.Get(ctx, types.NamespacedName{
		Name: resourceName,
	}, existingRpaasInstanceAddress)

	if k8sErrors.IsNotFound(err) {
		rpaasInstanceAddress := &v1alpha1.RpaasInstanceAddress{
			ObjectMeta: metav1.ObjectMeta{
				Name: resourceName,
			},
			Spec: v1alpha1.RpaasInstanceAddressSpec{
				ServiceName: rpaasInstance.ServiceName,
				Instance:    rpaasInstance.Instance,
			},
		}

		err = r.Client.Create(ctx, rpaasInstanceAddress)
		if err != nil {
			l.Error(err, "could not create RpaasInstanceAddress object")
			return nil, err
		}

		subReconciler := &RpaasInstanceAddressReconciler{
			Client:   r.Client,
			Scheme:   r.Scheme,
			Resolver: r.Resolver,
			TsuruAPI: r.TsuruAPI,
		}

		_, err = subReconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name: rpaasInstanceAddress.Name,
			},
		})

		if err != nil {
			l.Error(err, "could not sub-reconcicle RpaasInstanceAddress", "name", resourceName)
			return nil, err
		}

		err = r.Client.Get(ctx, types.NamespacedName{
			Name: resourceName,
		}, existingRpaasInstanceAddress)
		return existingRpaasInstanceAddress, err
	} else if err != nil {
		l.Error(err, "could not get RpaasInstanceAddress", "name", resourceName)
		return nil, err
	}

	return existingRpaasInstanceAddress, nil
}

func (r *ACLReconciler) ports(p []v1alpha1.ProtoPort) []netv1.NetworkPolicyPort {
	result := []netv1.NetworkPolicyPort{}
	for _, port := range p {
		var protocol *corev1.Protocol
		if port.Protocol != "" {
			p := corev1.Protocol(strings.ToUpper(port.Protocol))
			protocol = &p
		}

		portNumber := intstr.FromInt(int(port.Number))
		result = append(result, netv1.NetworkPolicyPort{
			Protocol: protocol,
			Port:     &portNumber,
		})
	}
	return result
}

func (r *ACLReconciler) podSelectorForTsuruApp(tsuruApp string) map[string]string {
	return map[string]string{
		"tsuru.io/app-name": tsuruApp,
	}
}

func (r *ACLReconciler) podSelectorForRpasInstance(rpaasInstance *v1alpha1.ACLSpecRpaasInstance) map[string]string {
	return map[string]string{
		"rpaas.extensions.tsuru.io/instance-name": rpaasInstance.Instance,
		"rpaas.extensions.tsuru.io/service-name":  rpaasInstance.ServiceName,
	}
}

func (r *ACLReconciler) getServiceCache() *serviceCache {
	s := r.serviceCache.Load()
	if s == nil {
		s = &serviceCache{
			Client: r.Client,
		}
		r.serviceCache.Store(s)
	}

	return s
}

func (r *ACLReconciler) fillPodSelectorByCIDR(ctx context.Context, rules []netv1.NetworkPolicyEgressRule) error {
	serviceCache := r.getServiceCache()
	for i, egressRule := range rules {
		newDestinations := []netv1.NetworkPolicyPeer{}

	toLoop:
		for _, to := range egressRule.To {
			if to.IPBlock != nil {
				if strings.HasSuffix(to.IPBlock.CIDR, "/32") || strings.HasSuffix(to.IPBlock.CIDR, "/128") {
					ip := strings.Split(to.IPBlock.CIDR, "/")[0]

					svc, err := serviceCache.GetByIP(ctx, ip)
					if err != nil {
						return err
					}

					if svc == nil {
						continue toLoop
					}

					newDestinations = append(newDestinations, netv1.NetworkPolicyPeer{
						PodSelector: &metav1.LabelSelector{
							MatchLabels: svc.Spec.Selector,
						},
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"name": svc.Namespace, // we have a common practice to add name of namespace as a label
							},
						},
					})
				}
			}
		}

		rules[i].To = append(rules[i].To, newDestinations...)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ACLReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ACL{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 4, RecoverPanic: true}).
		Complete(r)
}

func isWildCard(name string) bool {
	return name != "" && name[0] == '.'
}

func validResourceName(name string) string {
	if errs := validation.IsDNS1123Subdomain(name); len(errs) == 0 {
		return name
	}

	truncatedName := regexp.MustCompile("[^a-z0-9.-]").ReplaceAllString(name, "-")
	truncatedName = regexp.MustCompile("^[^a-z0-9]+").ReplaceAllString(truncatedName, "")

	digest := sha256String(name)[:10]

	const maxChars = 253

	if len(truncatedName) < maxChars-11 {
		return truncatedName + "-" + digest
	}

	return truncatedName[:maxChars-11] + "-" + digest
}

func sha256String(str string) string {
	hash := sha256.New()
	fmt.Fprint(hash, str)
	return fmt.Sprintf("%x", hash.Sum(nil))
}
