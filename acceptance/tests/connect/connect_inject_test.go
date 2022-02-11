package connect

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/consul-k8s/acceptance/framework/config"
	"github.com/hashicorp/consul-k8s/acceptance/framework/consul"
	"github.com/hashicorp/consul-k8s/acceptance/framework/environment"
	"github.com/hashicorp/consul-k8s/acceptance/framework/helpers"
	"github.com/hashicorp/consul-k8s/acceptance/framework/k8s"
	"github.com/hashicorp/consul-k8s/acceptance/framework/logger"
	"github.com/hashicorp/consul/sdk/testutil/retry"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestConnectInject tests that Connect works in a default and a secure installation.
func TestConnectInject(t *testing.T) {
	cases := map[string]struct {
		clusterGen  func(*testing.T, map[string]string, environment.TestContext, *config.TestConfig, string) consul.Cluster
		releaseName string
		secure      bool
		autoEncrypt bool
	}{
		"Helm install without secure or auto-encrypt": {
			clusterGen:  consul.NewHelmCluster,
			releaseName: helpers.RandomName(),
		},
		"Helm install with secure": {
			clusterGen:  consul.NewHelmCluster,
			releaseName: helpers.RandomName(),
			secure:      true,
		},
		"Helm install with secure and auto-encrypt": {
			clusterGen:  consul.NewHelmCluster,
			releaseName: helpers.RandomName(),
			secure:      true,
			autoEncrypt: true,
		},
		"CLI install without secure or auto-encrypt": {
			clusterGen:  consul.NewCLICluster,
			releaseName: consul.CLIReleaseName,
		},
		"CLI install with secure": {
			clusterGen:  consul.NewCLICluster,
			releaseName: consul.CLIReleaseName,
			secure:      true,
		},
		"CLI install with secure and auto-encrypt": {
			clusterGen:  consul.NewCLICluster,
			releaseName: consul.CLIReleaseName,
			secure:      true,
			autoEncrypt: true,
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := suite.Config()
			ctx := suite.Environment().DefaultContext(t)

			connHelper := ConnectHelper{
				ClusterGenerator: c.clusterGen,
				Secure:           c.secure,
				AutoEncrypt:      c.autoEncrypt,
				ReleaseName:      c.releaseName,
				T:                t,
				Ctx:              ctx,
				Cfg:              cfg,
			}

			err := connHelper.Install()
			require.NoError(t, err)

			err = connHelper.TestInstallation()
			require.NoError(t, err)
		})
	}
}

// TestConnectInjectOnUpgrade tests that Connect works before and after an upgrade is performed on the cluster.
func TestConnectInjectOnUpgrade(t *testing.T) {
	type TestCase struct {
		secure      bool
		autoEncrypt bool
		helmValues  map[string]string
	}

	cases := map[string]struct {
		clusterGen       func(*testing.T, map[string]string, environment.TestContext, *config.TestConfig, string) consul.Cluster
		releaseName      string
		initial, upgrade TestCase
	}{
		"Helm upgrade changes nothing": {
			clusterGen:  consul.NewHelmCluster,
			releaseName: helpers.RandomName(),
		},
		"CLI upgrade changes nothing": {
			clusterGen:  consul.NewCLICluster,
			releaseName: consul.CLIReleaseName,
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := suite.Config()
			ctx := suite.Environment().DefaultContext(t)

			connHelper := ConnectHelper{
				ClusterGenerator: c.clusterGen,
				Secure:           c.initial.secure,
				AutoEncrypt:      c.initial.autoEncrypt,
				ReleaseName:      c.releaseName,
				T:                t,
				Ctx:              ctx,
				Cfg:              cfg,
			}

			err := connHelper.Install()
			require.NoError(t, err)

			err = connHelper.TestInstallation()
			require.NoError(t, err)

			connHelper.Secure = c.upgrade.secure
			connHelper.AutoEncrypt = c.upgrade.autoEncrypt
			connHelper.AdditionalHelmValues = c.upgrade.helmValues

			err = connHelper.Upgrade()
			require.NoError(t, err)

			err = connHelper.TestInstallation()
			require.NoError(t, err)
		})
	}
}

// Test the endpoints controller cleans up force-killed pods.
func TestConnectInject_CleanupKilledPods(t *testing.T) {
	cases := []struct {
		secure      bool
		autoEncrypt bool
	}{
		{false, false},
		{true, false},
		{true, true},
	}

	for _, c := range cases {
		name := fmt.Sprintf("secure: %t; auto-encrypt: %t", c.secure, c.autoEncrypt)
		t.Run(name, func(t *testing.T) {
			cfg := suite.Config()
			ctx := suite.Environment().DefaultContext(t)

			helmValues := map[string]string{
				"connectInject.enabled":        "true",
				"global.tls.enabled":           strconv.FormatBool(c.secure),
				"global.tls.enableAutoEncrypt": strconv.FormatBool(c.autoEncrypt),
				"global.acls.manageSystemACLs": strconv.FormatBool(c.secure),
			}

			releaseName := helpers.RandomName()
			consulCluster := consul.NewHelmCluster(t, helmValues, ctx, cfg, releaseName)

			consulCluster.Create(t)

			logger.Log(t, "creating static-client deployment")
			k8s.DeployKustomize(t, ctx.KubectlOptions(t), cfg.NoCleanupOnFailure, cfg.DebugDirectory, "../fixtures/cases/static-client-inject")

			logger.Log(t, "waiting for static-client to be registered with Consul")
			consulClient := consulCluster.SetupConsulClient(t, c.secure)
			retry.Run(t, func(r *retry.R) {
				for _, name := range []string{"static-client", "static-client-sidecar-proxy"} {
					instances, _, err := consulClient.Catalog().Service(name, "", nil)
					r.Check(err)

					if len(instances) != 1 {
						r.Errorf("expected 1 instance of %s", name)
					}
				}
			})

			ns := ctx.KubectlOptions(t).Namespace
			pods, err := ctx.KubernetesClient(t).CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{LabelSelector: "app=static-client"})
			require.NoError(t, err)
			require.Len(t, pods.Items, 1)
			podName := pods.Items[0].Name

			logger.Logf(t, "force killing the static-client pod %q", podName)
			var gracePeriod int64 = 0
			err = ctx.KubernetesClient(t).CoreV1().Pods(ns).Delete(context.Background(), podName, metav1.DeleteOptions{GracePeriodSeconds: &gracePeriod})
			require.NoError(t, err)

			logger.Log(t, "ensuring pod is deregistered")
			retry.Run(t, func(r *retry.R) {
				for _, name := range []string{"static-client", "static-client-sidecar-proxy"} {
					instances, _, err := consulClient.Catalog().Service(name, "", nil)
					r.Check(err)

					for _, instance := range instances {
						if strings.Contains(instance.ServiceID, podName) {
							r.Errorf("%s is still registered", instance.ServiceID)
						}
					}
				}
			})
		})
	}
}

// Test that when Consul clients are restarted and lose all their registrations,
// the services get re-registered and can continue to talk to each other.
func TestConnectInject_RestartConsulClients(t *testing.T) {
	cfg := suite.Config()
	ctx := suite.Environment().DefaultContext(t)

	helmValues := map[string]string{
		"connectInject.enabled": "true",
	}

	releaseName := helpers.RandomName()
	consulCluster := consul.NewHelmCluster(t, helmValues, ctx, cfg, releaseName)

	consulCluster.Create(t)

	logger.Log(t, "creating static-server and static-client deployments")
	k8s.DeployKustomize(t, ctx.KubectlOptions(t), cfg.NoCleanupOnFailure, cfg.DebugDirectory, "../fixtures/cases/static-server-inject")
	if cfg.EnableTransparentProxy {
		k8s.DeployKustomize(t, ctx.KubectlOptions(t), cfg.NoCleanupOnFailure, cfg.DebugDirectory, "../fixtures/cases/static-client-tproxy")
	} else {
		k8s.DeployKustomize(t, ctx.KubectlOptions(t), cfg.NoCleanupOnFailure, cfg.DebugDirectory, "../fixtures/cases/static-client-inject")
	}

	logger.Log(t, "checking that connection is successful")
	if cfg.EnableTransparentProxy {
		k8s.CheckStaticServerConnectionSuccessful(t, ctx.KubectlOptions(t), "http://static-server")
	} else {
		k8s.CheckStaticServerConnectionSuccessful(t, ctx.KubectlOptions(t), "http://localhost:1234")
	}

	logger.Log(t, "restarting Consul client daemonset")
	k8s.RunKubectl(t, ctx.KubectlOptions(t), "rollout", "restart", fmt.Sprintf("ds/%s-consul-client", releaseName))
	k8s.RunKubectl(t, ctx.KubectlOptions(t), "rollout", "status", fmt.Sprintf("ds/%s-consul-client", releaseName))

	logger.Log(t, "checking that connection is still successful")
	if cfg.EnableTransparentProxy {
		k8s.CheckStaticServerConnectionSuccessful(t, ctx.KubectlOptions(t), "http://static-server")
	} else {
		k8s.CheckStaticServerConnectionSuccessful(t, ctx.KubectlOptions(t), "http://localhost:1234")
	}
}
