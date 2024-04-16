/*
Copyright 2021 The Knative Authors

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

package ingress

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"go.uber.org/zap"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"knative.dev/networking/pkg/apis/networking/v1alpha1"
	"knative.dev/networking/pkg/status"

	"knative.dev/net-gateway-api/pkg/reconciler/ingress/config"
	gatewaylisters "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1beta1"
)

func NewProbeTargetLister(logger *zap.SugaredLogger, endpointsLister corev1listers.EndpointsLister, gatewayLister gatewaylisters.GatewayLister) status.ProbeTargetLister {
	return &gatewayPodTargetLister{
		logger:          logger,
		endpointsLister: endpointsLister,
		gatewayLister:   gatewayLister,
	}
}

type gatewayPodTargetLister struct {
	logger          *zap.SugaredLogger
	endpointsLister corev1listers.EndpointsLister
	gatewayLister   gatewaylisters.GatewayLister
}

func (l *gatewayPodTargetLister) ListProbeTargets(ctx context.Context, ing *v1alpha1.Ingress) ([]status.ProbeTarget, error) {
	result := make([]status.ProbeTarget, 0, len(ing.Spec.Rules))
	for _, rule := range ing.Spec.Rules {
		eps, err := l.getRuleProbes(ctx, rule, ing.Spec.HTTPOption)
		if err != nil {
			return nil, err
		}
		result = append(result, eps...)
	}
	return result, nil
}

func (l *gatewayPodTargetLister) getRuleProbes(ctx context.Context, rule v1alpha1.IngressRule, sslOpt v1alpha1.HTTPOption) ([]status.ProbeTarget, error) {
	gatewayConfig := config.FromContext(ctx).Gateway

	var targets []status.ProbeTarget
	if service := gatewayConfig.Gateways[rule.Visibility].Service; service != nil {
		eps, err := l.endpointsLister.Endpoints(service.Namespace).Get(service.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to get endpoints: %w", err)
		}

		targets = make([]status.ProbeTarget, 0, len(eps.Subsets))
		foundTargets := 0
		for _, sub := range eps.Subsets {
			scheme := "http"
			// Istio uses "http2" for the http port
			// Contour uses "http-80" for the http port
			matchSchemes := sets.New("http", "http2", "http-80")
			if rule.Visibility == v1alpha1.IngressVisibilityExternalIP && sslOpt == v1alpha1.HTTPOptionRedirected {
				scheme = "https"
				matchSchemes = sets.New("https", "https-443")
			}
			pt := status.ProbeTarget{PodIPs: sets.New[string]()}

			portNumber := sub.Ports[0].Port
			for _, port := range sub.Ports {
				if matchSchemes.Has(port.Name) {
					// Prefer to match the name exactly
					portNumber = port.Port
					break
				}
				if port.AppProtocol != nil && matchSchemes.Has(*port.AppProtocol) {
					portNumber = port.Port
				}
			}
			pt.PodPort = strconv.Itoa(int(portNumber))

			for _, address := range sub.Addresses {
				pt.PodIPs.Insert(address.IP)
			}
			foundTargets += len(pt.PodIPs)

			pt.URLs = domainsToURL(rule.Hosts, scheme)
			targets = append(targets, pt)
		}
		if foundTargets == 0 {
			return nil, fmt.Errorf("no gateway pods available")
		}

	} else {
		gwName := gatewayConfig.Gateways[rule.Visibility].Gateway
		gw, err := l.gatewayLister.Gateways(gwName.Namespace).Get(gwName.Name)
		if apierrs.IsNotFound(err) {
			return nil, fmt.Errorf("Gateway %s does not exist: %w", gwName, err) //nolint:stylecheck
		} else if err != nil {
			return nil, err
		}

		// TODO: how do I know which listeners to probe?
		// It seems like I'd need to check each rule, and find out which listener it maps to,
		// and then add that as the target...
		//for _, l := range gw.Spec.Listeners {
		//}

		// for now, we'll only support 80 or 443
		// this way, we don't have to search for which listener to look for since
		// Knative will add a TLS listener for each ksvc when external-domain-tls (f.k.a. autoTLS) is enabled
		scheme := "http"
		podPort := "80"
		if rule.Visibility == v1alpha1.IngressVisibilityExternalIP && sslOpt == v1alpha1.HTTPOptionRedirected {
			scheme = "https"
			podPort = "443"
		}
		targets = make([]status.ProbeTarget, 0, 1)
		targets = append(targets, status.ProbeTarget{
			PodIPs:  sets.New[string](gw.Status.Addresses[0].Value),
			PodPort: podPort,
			URLs:    domainsToURL(rule.Hosts, scheme),
		})
	}

	return targets, nil
}

func domainsToURL(domains []string, scheme string) []*url.URL {
	urls := make([]*url.URL, 0, len(domains))
	for _, domain := range domains {
		url := &url.URL{
			Scheme: scheme,
			Host:   domain,
			Path:   "/",
		}
		urls = append(urls, url)
	}
	return urls
}
