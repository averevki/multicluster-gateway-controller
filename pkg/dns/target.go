package dns

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/martinlindhe/base36"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/Kuadrant/multicluster-gateway-controller/pkg/apis/v1alpha1"
)

const (
	DefaultWeight                   = int(v1alpha1.DefaultWeight)
	DefaultGeo              GeoCode = "default"
	WildcardGeo             GeoCode = "*"
	LabelLBAttributeGeoCode         = "kuadrant.io/lb-attribute-geo-code"
)

// MultiClusterGatewayTarget represents a Gateway that is placed on multiple clusters (ClusterGateway).
type MultiClusterGatewayTarget struct {
	Gateway               *gatewayv1beta1.Gateway
	ClusterGatewayTargets []ClusterGatewayTarget
	LoadBalancing         *v1alpha1.LoadBalancingSpec
}

func NewMultiClusterGatewayTarget(gateway *gatewayv1beta1.Gateway, clusterGateways []ClusterGateway, loadBalancing *v1alpha1.LoadBalancingSpec) (*MultiClusterGatewayTarget, error) {
	mcg := &MultiClusterGatewayTarget{Gateway: gateway, LoadBalancing: loadBalancing}
	err := mcg.setClusterGatewayTargets(clusterGateways)
	return mcg, err
}

func (t *MultiClusterGatewayTarget) GetName() string {
	return fmt.Sprintf("%s-%s", t.Gateway.Name, t.Gateway.Namespace)
}

func (t *MultiClusterGatewayTarget) GetShortCode() string {
	return ToBase36hash(t.GetName())
}

// GroupTargetsByGeo groups targets based on Geo Code.
func (t *MultiClusterGatewayTarget) GroupTargetsByGeo() map[GeoCode][]ClusterGatewayTarget {
	geoTargets := make(map[GeoCode][]ClusterGatewayTarget)
	for _, target := range t.ClusterGatewayTargets {
		geoTargets[target.GetGeo()] = append(geoTargets[target.GetGeo()], target)
	}
	return geoTargets
}

func (t *MultiClusterGatewayTarget) GetDefaultGeo() GeoCode {
	if t.LoadBalancing != nil && t.LoadBalancing.Geo != nil {
		return GeoCode(t.LoadBalancing.Geo.DefaultGeo)
	}
	return DefaultGeo
}

func (t *MultiClusterGatewayTarget) GetDefaultWeight() int {
	if t.LoadBalancing != nil && t.LoadBalancing.Weighted != nil {
		return int(t.LoadBalancing.Weighted.DefaultWeight)
	}
	return DefaultWeight
}

func (t *MultiClusterGatewayTarget) setClusterGatewayTargets(clusterGateways []ClusterGateway) error {
	var cgTargets []ClusterGatewayTarget
	for _, cg := range clusterGateways {
		var customWeights []*v1alpha1.CustomWeight
		if t.LoadBalancing != nil && t.LoadBalancing.Weighted != nil {
			customWeights = t.LoadBalancing.Weighted.Custom
		}
		cgt, err := NewClusterGatewayTarget(cg, t.GetDefaultGeo(), t.GetDefaultWeight(), customWeights)
		if err != nil {
			return err
		}
		cgTargets = append(cgTargets, cgt)
	}
	t.ClusterGatewayTargets = cgTargets
	return nil
}

// ClusterGateway contains the addresses of a Gateway on a single cluster and the attributes of the target cluster.
type ClusterGateway struct {
	Cluster          metav1.Object
	GatewayAddresses []gatewayv1beta1.GatewayAddress
}

type GeoCode string

func (gc GeoCode) IsDefaultCode() bool {
	return gc == DefaultGeo
}

func (gc GeoCode) IsWildcard() bool {
	return gc == WildcardGeo
}

func NewClusterGateway(cluster metav1.Object, gatewayAddresses []gatewayv1beta1.GatewayAddress) *ClusterGateway {
	cgw := &ClusterGateway{
		Cluster:          cluster,
		GatewayAddresses: gatewayAddresses,
	}
	return cgw
}

// ClusterGatewayTarget represents a cluster Gateway with geo and weighting info calculated
type ClusterGatewayTarget struct {
	*ClusterGateway
	Geo    *GeoCode
	Weight *int
}

func NewClusterGatewayTarget(cg ClusterGateway, defaultGeoCode GeoCode, defaultWeight int, customWeights []*v1alpha1.CustomWeight) (ClusterGatewayTarget, error) {
	target := ClusterGatewayTarget{
		ClusterGateway: &cg,
	}
	target.setGeo(defaultGeoCode)
	err := target.setWeight(defaultWeight, customWeights)
	if err != nil {
		return ClusterGatewayTarget{}, err
	}
	return target, nil
}

func (t *ClusterGatewayTarget) GetGeo() GeoCode {
	return *t.Geo
}

func (t *ClusterGatewayTarget) GetWeight() int {
	return *t.Weight
}

func (t *ClusterGatewayTarget) GetName() string {
	return t.Cluster.GetName()
}

func (t *ClusterGatewayTarget) GetShortCode() string {
	return ToBase36hash(t.GetName())
}

func (t *ClusterGatewayTarget) setGeo(defaultGeo GeoCode) {
	geoCode := defaultGeo
	if geoCode == DefaultGeo {
		t.Geo = &geoCode
		return
	}
	if gc, ok := t.Cluster.GetLabels()[LabelLBAttributeGeoCode]; ok {
		geoCode = GeoCode(gc)
	}
	t.Geo = &geoCode
}

func (t *MultiClusterGatewayTarget) RemoveUnhealthyGatewayAddresses(probes []*v1alpha1.DNSHealthCheckProbe, listener gatewayv1beta1.Listener) {

	//If we have no probes we can't determine health so return unmodified
	if len(probes) == 0 {
		return
	}

	//Build a map of gateway addresses and their health status
	gwAddressHealth := map[string]bool{}
	allunhealthy := true
	for _, cgt := range t.ClusterGatewayTargets {
		for _, gwa := range cgt.GatewayAddresses {
			probe := getProbeForGatewayAddress(probes, gwa, t.Gateway.Name, string(listener.Name))
			if probe == nil {
				continue
			}
			probeHealthy := true
			if probe.Status.Healthy != nil {
				probeHealthy = *probe.Status.Healthy
			}
			if probeHealthy && probe.Spec.FailureThreshold != nil && probe.Status.ConsecutiveFailures < *probe.Spec.FailureThreshold {
				allunhealthy = false
			}
			gwAddressHealth[gwa.Value] = probeHealthy

		}
	}
	//If we have no matching probes for our current addresses, or we have no healthy probes, return unmodified
	if len(gwAddressHealth) == 0 || allunhealthy {
		return
	}

	// Remove all unhealthy addresses, we know by this point at least one of our addresses is healthy
	for _, cgt := range t.ClusterGatewayTargets {
		healthyAddresses := []gatewayv1beta1.GatewayAddress{}
		for _, gwa := range cgt.GatewayAddresses {
			if healthy, exists := gwAddressHealth[gwa.Value]; exists && healthy {
				healthyAddresses = append(healthyAddresses, gwa)
			}
		}
		cgt.GatewayAddresses = healthyAddresses
	}
}

func getProbeForGatewayAddress(probes []*v1alpha1.DNSHealthCheckProbe, gwa gatewayv1beta1.GatewayAddress, gatewayName, listenerName string) *v1alpha1.DNSHealthCheckProbe {
	for _, probe := range probes {
		if dnsHealthCheckProbeName(gwa.Value, gatewayName, listenerName) == probe.Name {
			return probe
		}
	}
	return nil
}

func dnsHealthCheckProbeName(address, gatewayName, listenerName string) string {
	return fmt.Sprintf("%s-%s-%s", address, gatewayName, listenerName)
}

func (t *ClusterGatewayTarget) setWeight(defaultWeight int, customWeights []*v1alpha1.CustomWeight) error {
	weight := defaultWeight
	for k := range customWeights {
		cw := customWeights[k]
		selector, err := metav1.LabelSelectorAsSelector(cw.Selector)
		if err != nil {
			return err
		}
		if selector.Matches(labels.Set(t.Cluster.GetLabels())) {
			customWeight := int(cw.Weight)
			weight = customWeight
			break
		}
	}
	t.Weight = &weight
	return nil
}

func ToBase36hash(s string) string {
	hash := sha256.Sum224([]byte(s))
	// convert the hash to base36 (alphanumeric) to decrease collision probabilities
	base36hash := strings.ToLower(base36.EncodeBytes(hash[:]))
	// use 6 chars of the base36hash, should be enough to avoid collisions and keep the code short enough
	return base36hash[:6]
}
