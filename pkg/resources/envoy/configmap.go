// Copyright © 2019 Banzai Cloud
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package envoy

import (
	"fmt"

	envoybootstrap "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	envoycluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	envoycore "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	envoyendpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	envoylistener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	envoytcpproxy "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"github.com/ghodss/yaml"
	"github.com/go-logr/logr"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/banzaicloud/koperator/api/v1beta1"
	"github.com/banzaicloud/koperator/pkg/resources/templates"
	"github.com/banzaicloud/koperator/pkg/util"
	envoyutils "github.com/banzaicloud/koperator/pkg/util/envoy"
	kafkautils "github.com/banzaicloud/koperator/pkg/util/kafka"
)

func (r *Reconciler) configMap(log logr.Logger, extListener v1beta1.ExternalListenerConfig,
	ingressConfig v1beta1.IngressConfig, ingressConfigName, defaultIngressConfigName string) runtime.Object {
	eListenerLabelName := util.ConstructEListenerLabelName(ingressConfigName, extListener.Name)

	var configMapName string = util.GenerateEnvoyResourceName(envoyutils.EnvoyVolumeAndConfigName, envoyutils.EnvoyVolumeAndConfigNameWithScope,
		extListener, ingressConfig, ingressConfigName, r.KafkaCluster.GetName())

	configMap := &corev1.ConfigMap{
		ObjectMeta: templates.ObjectMeta(
			configMapName,
			labelsForEnvoyIngress(r.KafkaCluster.GetName(), eListenerLabelName), r.KafkaCluster),
		Data: map[string]string{"envoy.yaml": GenerateEnvoyConfig(r.KafkaCluster, extListener, ingressConfig,
			ingressConfigName, defaultIngressConfigName, log)},
	}
	return configMap
}

func generateAddressValue(kc *v1beta1.KafkaCluster, brokerId int) string {
	if kc.Spec.HeadlessServiceEnabled {
		return fmt.Sprintf("%s-%d.%s-headless.%s.svc.%s", kc.Name, brokerId, kc.Name, kc.Namespace, kc.Spec.GetKubernetesClusterDomain())
	}
	//ClusterIP services are in use
	return fmt.Sprintf("%s-%d.%s.svc.%s", kc.Name, brokerId, kc.Namespace, kc.Spec.GetKubernetesClusterDomain())
}

func generateAnyCastAddressValue(kc *v1beta1.KafkaCluster) string {
	if kc.Spec.HeadlessServiceEnabled {
		return fmt.Sprintf("%s-headless.%s.svc.%s", kc.GetName(), kc.GetNamespace(), kc.Spec.GetKubernetesClusterDomain())
	}
	//ClusterIP services are in use
	return fmt.Sprintf(
		kafkautils.AllBrokerServiceTemplate+".%s.svc.%s", kc.GetName(), kc.GetNamespace(), kc.Spec.GetKubernetesClusterDomain())
}

func GenerateEnvoyConfig(kc *v1beta1.KafkaCluster, elistener v1beta1.ExternalListenerConfig, ingressConfig v1beta1.IngressConfig,
	ingressConfigName, defaultIngressConfigName string, log logr.Logger) string {
	adminConfig := envoybootstrap.Admin{
		Address: &envoycore.Address{
			Address: &envoycore.Address_SocketAddress{
				SocketAddress: &envoycore.SocketAddress{
					Address: "0.0.0.0",
					PortSpecifier: &envoycore.SocketAddress_PortValue{
						PortValue: uint32(ingressConfig.EnvoyConfig.GetEnvoyAdminPort()),
					},
				},
			},
		},
	}

	var listeners []*envoylistener.Listener
	var clusters []*envoycluster.Cluster

	for _, brokerId := range util.GetBrokerIdsFromStatusAndSpec(kc.Status.BrokersState, kc.Spec.Brokers, log) {
		brokerConfig, err := kafkautils.GatherBrokerConfigIfAvailable(kc.Spec, brokerId)
		if err != nil {
			log.Error(err, "could not determine brokerConfig")
			continue
		}
		if util.ShouldIncludeBroker(brokerConfig, kc.Status, brokerId, defaultIngressConfigName, ingressConfigName) {
			// TCP_Proxy filter configuration
			tcpProxy := &envoytcpproxy.TcpProxy{
				StatPrefix: fmt.Sprintf("broker_tcp-%d", brokerId),
				ClusterSpecifier: &envoytcpproxy.TcpProxy_Cluster{
					Cluster: fmt.Sprintf("broker-%d", brokerId),
				},
			}
			pbstTcpProxy, err := anypb.New(tcpProxy)
			if err != nil {
				log.Error(err, "could not marshall envoy tcp_proxy config")
				return ""
			}
			listeners = append(listeners, &envoylistener.Listener{
				Address: &envoycore.Address{
					Address: &envoycore.Address_SocketAddress{
						SocketAddress: &envoycore.SocketAddress{
							Address: "0.0.0.0",
							PortSpecifier: &envoycore.SocketAddress_PortValue{
								PortValue: uint32(elistener.ExternalStartingPort + int32(brokerId)),
							},
						},
					},
				},
				FilterChains: []*envoylistener.FilterChain{
					{
						Filters: []*envoylistener.Filter{
							{
								Name: wellknown.TCPProxy,
								ConfigType: &envoylistener.Filter_TypedConfig{
									TypedConfig: pbstTcpProxy,
								},
							},
						},
					},
				},
			})

			clusters = append(clusters, &envoycluster.Cluster{
				Name:                 fmt.Sprintf("broker-%d", brokerId),
				ConnectTimeout:       &durationpb.Duration{Seconds: 1},
				ClusterDiscoveryType: &envoycluster.Cluster_Type{Type: envoycluster.Cluster_STRICT_DNS},
				LbPolicy:             envoycluster.Cluster_ROUND_ROBIN,
				// disable circuit breakingL:
				// https://www.envoyproxy.io/docs/envoy/latest/faq/load_balancing/disable_circuit_breaking
				CircuitBreakers: &envoycluster.CircuitBreakers{
					Thresholds: []*envoycluster.CircuitBreakers_Thresholds{
						{
							Priority:           envoycore.RoutingPriority_DEFAULT,
							MaxConnections:     &wrapperspb.UInt32Value{Value: 1_000_000_000},
							MaxPendingRequests: &wrapperspb.UInt32Value{Value: 1_000_000_000},
							MaxRequests:        &wrapperspb.UInt32Value{Value: 1_000_000_000},
							MaxRetries:         &wrapperspb.UInt32Value{Value: 1_000_000_000},
						},
						{
							Priority:           envoycore.RoutingPriority_HIGH,
							MaxConnections:     &wrapperspb.UInt32Value{Value: 1_000_000_000},
							MaxPendingRequests: &wrapperspb.UInt32Value{Value: 1_000_000_000},
							MaxRequests:        &wrapperspb.UInt32Value{Value: 1_000_000_000},
							MaxRetries:         &wrapperspb.UInt32Value{Value: 1_000_000_000},
						},
					},
				},
				LoadAssignment: &envoyendpoint.ClusterLoadAssignment{
					ClusterName: fmt.Sprintf("broker-%d", brokerId),
					Endpoints: []*envoyendpoint.LocalityLbEndpoints{{
						LbEndpoints: []*envoyendpoint.LbEndpoint{{
							HostIdentifier: &envoyendpoint.LbEndpoint_Endpoint{
								Endpoint: &envoyendpoint.Endpoint{
									Address: &envoycore.Address{
										Address: &envoycore.Address_SocketAddress{
											SocketAddress: &envoycore.SocketAddress{
												Protocol: envoycore.SocketAddress_TCP,
												Address:  generateAddressValue(kc, brokerId),
												PortSpecifier: &envoycore.SocketAddress_PortValue{
													PortValue: uint32(elistener.ContainerPort),
												},
											},
										},
									},
								},
							},
						}},
					}},
				},
			})
		}
	}
	// Create an any cast broker access point

	// TCP_Proxy filter configuration
	tcpProxy := &envoytcpproxy.TcpProxy{
		StatPrefix: envoyutils.AllBrokerEnvoyConfigName,
		ClusterSpecifier: &envoytcpproxy.TcpProxy_Cluster{
			Cluster: envoyutils.AllBrokerEnvoyConfigName,
		},
	}
	pbstTcpProxy, err := anypb.New(tcpProxy)
	if err != nil {
		log.Error(err, "could not marshall envoy tcp_proxy config")
		return ""
	}
	listeners = append(listeners, &envoylistener.Listener{
		Address: &envoycore.Address{
			Address: &envoycore.Address_SocketAddress{
				SocketAddress: &envoycore.SocketAddress{
					Address: "0.0.0.0",
					PortSpecifier: &envoycore.SocketAddress_PortValue{
						PortValue: uint32(elistener.GetAnyCastPort()),
					},
				},
			},
		},
		FilterChains: []*envoylistener.FilterChain{
			{
				Filters: []*envoylistener.Filter{
					{
						Name: wellknown.TCPProxy,
						ConfigType: &envoylistener.Filter_TypedConfig{
							TypedConfig: pbstTcpProxy,
						},
					},
				},
			},
		},
	})

	clusters = append(clusters, &envoycluster.Cluster{
		Name:                 envoyutils.AllBrokerEnvoyConfigName,
		ConnectTimeout:       &durationpb.Duration{Seconds: 1},
		ClusterDiscoveryType: &envoycluster.Cluster_Type{Type: envoycluster.Cluster_STRICT_DNS},
		LbPolicy:             envoycluster.Cluster_ROUND_ROBIN,
		// disable circuit breakingL:
		// https://www.envoyproxy.io/docs/envoy/latest/faq/load_balancing/disable_circuit_breaking
		CircuitBreakers: &envoycluster.CircuitBreakers{
			Thresholds: []*envoycluster.CircuitBreakers_Thresholds{
				{
					Priority:           envoycore.RoutingPriority_DEFAULT,
					MaxConnections:     &wrapperspb.UInt32Value{Value: 1_000_000_000},
					MaxPendingRequests: &wrapperspb.UInt32Value{Value: 1_000_000_000},
					MaxRequests:        &wrapperspb.UInt32Value{Value: 1_000_000_000},
					MaxRetries:         &wrapperspb.UInt32Value{Value: 1_000_000_000},
				},
				{
					Priority:           envoycore.RoutingPriority_HIGH,
					MaxConnections:     &wrapperspb.UInt32Value{Value: 1_000_000_000},
					MaxPendingRequests: &wrapperspb.UInt32Value{Value: 1_000_000_000},
					MaxRequests:        &wrapperspb.UInt32Value{Value: 1_000_000_000},
					MaxRetries:         &wrapperspb.UInt32Value{Value: 1_000_000_000},
				},
			},
		},
		LoadAssignment: &envoyendpoint.ClusterLoadAssignment{
			ClusterName: envoyutils.AllBrokerEnvoyConfigName,
			Endpoints: []*envoyendpoint.LocalityLbEndpoints{{
				LbEndpoints: []*envoyendpoint.LbEndpoint{{
					HostIdentifier: &envoyendpoint.LbEndpoint_Endpoint{
						Endpoint: &envoyendpoint.Endpoint{
							Address: &envoycore.Address{
								Address: &envoycore.Address_SocketAddress{
									SocketAddress: &envoycore.SocketAddress{
										Protocol: envoycore.SocketAddress_TCP,
										Address:  generateAnyCastAddressValue(kc),
										PortSpecifier: &envoycore.SocketAddress_PortValue{
											PortValue: uint32(elistener.ContainerPort),
										},
									},
								},
							},
						},
					},
				}},
			}},
		},
	})

	config := envoybootstrap.Bootstrap_StaticResources{
		Listeners: listeners,
		Clusters:  clusters,
	}
	generatedConfig := envoybootstrap.Bootstrap{
		Admin:           &adminConfig,
		StaticResources: &config,
	}
	marshaller := &protojson.MarshalOptions{}
	marshalledProtobufConfig, err := marshaller.Marshal(&generatedConfig)
	if err != nil {
		log.Error(err, "could not marshall envoy config")
		return ""
	}

	marshalledConfig, err := yaml.JSONToYAML(marshalledProtobufConfig)
	if err != nil {
		log.Error(err, "could not convert config from Json to Yaml")
		return ""
	}
	return string(marshalledConfig)
}
