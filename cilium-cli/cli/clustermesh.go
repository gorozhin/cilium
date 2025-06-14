// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cilium/cilium/cilium-cli/clustermesh"
	"github.com/cilium/cilium/cilium-cli/defaults"
	"github.com/cilium/cilium/cilium-cli/status"
)

func newCmdClusterMesh() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clustermesh",
		Short: "Multi Cluster Management",
		Long:  ``,
	}

	cmd.AddCommand(
		newCmdClusterMeshStatus(),
		newCmdClusterMeshConnectWithHelm(),
		newCmdClusterMeshDisconnectWithHelm(),
		newCmdClusterMeshEnableWithHelm(),
		newCmdClusterMeshDisableWithHelm(),
		newCmdClusterMeshPolicyDefaultClusterInspect(),
	)

	return cmd
}

func newCmdClusterMeshStatus() *cobra.Command {
	var params = clustermesh.Parameters{
		Writer: os.Stdout,
	}

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show status of ClusterMesh",
		Long:  ``,
		RunE: func(_ *cobra.Command, _ []string) error {
			params.Namespace = namespace
			params.ImpersonateAs = impersonateAs
			params.ImpersonateGroups = impersonateGroups

			if params.Output == status.OutputJSON {
				// Write status log messages to stderr to make sure they don't
				// clutter JSON output.
				params.Writer = os.Stderr
			}

			cm := clustermesh.NewK8sClusterMesh(k8sClient, params)
			if _, err := cm.Status(context.Background()); err != nil {
				fatalf("Unable to determine status:  %s", err)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&params.Wait, "wait", false, "Wait until status is successful")
	cmd.Flags().DurationVar(&params.WaitDuration, "wait-duration", 15*time.Minute, "Maximum time to wait")
	cmd.Flags().StringVarP(&params.Output, "output", "o", status.OutputSummary, "Output format. One of: json, summary")

	return cmd
}

func newCmdClusterMeshEnableWithHelm() *cobra.Command {
	var params = clustermesh.Parameters{
		Writer: os.Stdout,
	}

	cmd := &cobra.Command{
		Use:   "enable",
		Short: "Enable ClusterMesh ability in a cluster using Helm",
		Long:  ``,
		RunE: func(cmd *cobra.Command, _ []string) error {
			params.Namespace = namespace
			params.ImpersonateAs = impersonateAs
			params.ImpersonateGroups = impersonateGroups
			params.HelmReleaseName = helmReleaseName
			ctx := context.Background()
			params.EnableKVStoreMeshChanged = cmd.Flags().Changed("enable-kvstoremesh")
			if err := clustermesh.EnableWithHelm(ctx, k8sClient, params); err != nil {
				fatalf("Unable to enable ClusterMesh: %s", err)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&params.EnableKVStoreMesh, "enable-kvstoremesh", false, "Enable kvstoremesh, an extension which caches remote cluster information in the local kvstore")
	cmd.Flags().StringVar(&params.ServiceType, "service-type", "", "Type of Kubernetes service to expose control plane { LoadBalancer | NodePort }")

	return cmd
}

func newCmdClusterMeshDisableWithHelm() *cobra.Command {
	var params = clustermesh.Parameters{
		Writer: os.Stdout,
	}

	cmd := &cobra.Command{
		Use:   "disable",
		Short: "Disable ClusterMesh ability in a cluster using Helm",
		Long:  ``,
		RunE: func(_ *cobra.Command, _ []string) error {
			params.Namespace = namespace
			params.ImpersonateAs = impersonateAs
			params.ImpersonateGroups = impersonateGroups
			params.HelmReleaseName = helmReleaseName
			ctx := context.Background()
			if err := clustermesh.DisableWithHelm(ctx, k8sClient, params); err != nil {
				fatalf("Unable to disable ClusterMesh: %s", err)
			}
			return nil
		},
	}

	return cmd
}

func newCmdClusterMeshConnectWithHelm() *cobra.Command {
	var params = clustermesh.Parameters{
		Writer: os.Stdout,
	}

	cmd := &cobra.Command{
		Use:   "connect",
		Short: "Connect to a remote cluster",
		Long:  ``,
		RunE: func(_ *cobra.Command, _ []string) error {
			params.Namespace = namespace
			params.ImpersonateAs = impersonateAs
			params.ImpersonateGroups = impersonateGroups
			params.HelmReleaseName = helmReleaseName
			cm := clustermesh.NewK8sClusterMesh(k8sClient, params)
			if err := cm.ConnectWithHelm(context.Background()); err != nil {
				fatalf("Unable to connect cluster: %s", err)
			}
			return nil
		},
	}

	addCommonConnectFlags(cmd, &params)

	return cmd
}

func newCmdClusterMeshDisconnectWithHelm() *cobra.Command {
	var params = clustermesh.Parameters{
		Writer: os.Stdout,
	}

	cmd := &cobra.Command{
		Use:   "disconnect",
		Short: "Disconnect from a remote cluster",
		Run: func(_ *cobra.Command, _ []string) {
			params.Namespace = namespace
			params.ImpersonateAs = impersonateAs
			params.ImpersonateGroups = impersonateGroups
			params.HelmReleaseName = helmReleaseName
			cm := clustermesh.NewK8sClusterMesh(k8sClient, params)
			if err := cm.DisconnectWithHelm(context.Background()); err != nil {
				fatalf("Unable to disconnect clusters: %s", err)
			}
		},
	}
	cmd.Flags().StringVar(&params.ConnectionMode, "connection-mode", defaults.ClusterMeshConnectionModeBidirectional,
		fmt.Sprintf("Connection mode: %s, %s or %s", defaults.ClusterMeshConnectionModeUnicast, defaults.ClusterMeshConnectionModeBidirectional, defaults.ClusterMeshConnectionModeMesh))
	cmd.Flags().StringSliceVar(&params.DestinationContext, "destination-context", []string{}, "Comma separated list of Kubernetes configuration contexts of destination cluster")

	return cmd
}

func newCmdClusterMeshPolicyDefaultClusterInspect() *cobra.Command {
	namespace := ""
	allNamespaces := false
	output := status.OutputSummary

	cmd := &cobra.Command{
		Use:   "inspect-policy-default-local-cluster",
		Short: "List policies that would be affected by changing policy-default-local-cluster on a cluster",
		Long:  ``,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var err error
			if namespace == "" {
				if namespace, _, err = k8sClient.RESTClientGetter.ToRawKubeConfigLoader().Namespace(); err != nil {
					namespace = metav1.NamespaceDefault
				}
			}
			if allNamespaces {
				namespace = corev1.NamespaceAll
			}
			res, err := clustermesh.PolicyDefaultLocalClusterInspect(cmd.Context(), k8sClient, namespace)
			if err != nil {
				fatalf("Unable to inspect policy default local cluster: %s", err)
			}
			if err := res.OutputPolicyDefaultLocalClusterInspect(output); err != nil {
				fatalf("Unable to output inspect policy default local cluster: %s", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&namespace, "namespace", "n", namespace, "Namespace used for listing resources")
	cmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", allNamespaces, "If present, list the resources across all namespace. Namespace in current context or specified with --namespace is ignored.")
	cmd.Flags().StringVarP(&output, "output", "o", output, "Output format. One of: json, summary")

	return cmd
}

func addCommonConnectFlags(cmd *cobra.Command, params *clustermesh.Parameters) {
	cmd.Flags().StringVar(&params.ConnectionMode, "connection-mode", defaults.ClusterMeshConnectionModeBidirectional,
		fmt.Sprintf("Connection mode: %s, %s or %s", defaults.ClusterMeshConnectionModeUnicast, defaults.ClusterMeshConnectionModeBidirectional, defaults.ClusterMeshConnectionModeMesh))
	cmd.Flags().StringSliceVar(&params.DestinationContext, "destination-context", []string{}, "Comma separated list of Kubernetes configuration contexts of destination cluster")
	cmd.Flags().StringSliceVar(&params.DestinationEndpoints, "destination-endpoint", []string{}, "IP of ClusterMesh service of destination cluster")
	cmd.Flags().StringSliceVar(&params.SourceEndpoints, "source-endpoint", []string{}, "IP of ClusterMesh service of source cluster")
	cmd.Flags().IntVar(&params.Parallel, "parallel", 1, "Number of parallel connection of destination cluster")
}
