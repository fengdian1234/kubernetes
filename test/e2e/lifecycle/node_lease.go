/*
Copyright 2018 The Kubernetes Authors.

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

package lifecycle

import (
	"fmt"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2elog "k8s.io/kubernetes/test/e2e/framework/log"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
)

var _ = SIGDescribe("[Disruptive]NodeLease", func() {
	f := framework.NewDefaultFramework("node-lease-test")
	var systemPodsNo int32
	var c clientset.Interface
	var ns string
	var group string

	ginkgo.BeforeEach(func() {
		c = f.ClientSet
		ns = f.Namespace.Name
		systemPods, err := e2epod.GetPodsInNamespace(c, ns, map[string]string{})
		gomega.Expect(err).To(gomega.BeNil())
		systemPodsNo = int32(len(systemPods))
		if strings.Index(framework.TestContext.CloudConfig.NodeInstanceGroup, ",") >= 0 {
			e2elog.Failf("Test dose not support cluster setup with more than one MIG: %s", framework.TestContext.CloudConfig.NodeInstanceGroup)
		} else {
			group = framework.TestContext.CloudConfig.NodeInstanceGroup
		}
	})

	ginkgo.Describe("NodeLease deletion", func() {
		var skipped bool

		ginkgo.BeforeEach(func() {
			skipped = true
			framework.SkipUnlessProviderIs("gce", "gke", "aws")
			framework.SkipUnlessNodeCountIsAtLeast(2)
			skipped = false
		})

		ginkgo.AfterEach(func() {
			if skipped {
				return
			}

			ginkgo.By("restoring the original node instance group size")
			if err := framework.ResizeGroup(group, int32(framework.TestContext.CloudConfig.NumNodes)); err != nil {
				e2elog.Failf("Couldn't restore the original node instance group size: %v", err)
			}
			// In GKE, our current tunneling setup has the potential to hold on to a broken tunnel (from a
			// rebooted/deleted node) for up to 5 minutes before all tunnels are dropped and recreated.
			// Most tests make use of some proxy feature to verify functionality. So, if a reboot test runs
			// right before a test that tries to get logs, for example, we may get unlucky and try to use a
			// closed tunnel to a node that was recently rebooted. There's no good way to framework.Poll for proxies
			// being closed, so we sleep.
			//
			// TODO(cjcullen) reduce this sleep (#19314)
			if framework.ProviderIs("gke") {
				ginkgo.By("waiting 5 minutes for all dead tunnels to be dropped")
				time.Sleep(5 * time.Minute)
			}
			if err := framework.WaitForGroupSize(group, int32(framework.TestContext.CloudConfig.NumNodes)); err != nil {
				e2elog.Failf("Couldn't restore the original node instance group size: %v", err)
			}

			if err := e2enode.WaitForReadyNodes(c, framework.TestContext.CloudConfig.NumNodes, 10*time.Minute); err != nil {
				e2elog.Failf("Couldn't restore the original cluster size: %v", err)
			}
			// Many e2e tests assume that the cluster is fully healthy before they start.  Wait until
			// the cluster is restored to health.
			ginkgo.By("waiting for system pods to successfully restart")
			err := e2epod.WaitForPodsRunningReady(c, metav1.NamespaceSystem, systemPodsNo, 0, framework.PodReadyBeforeTimeout, map[string]string{})
			gomega.Expect(err).To(gomega.BeNil())
		})

		ginkgo.It("node lease should be deleted when corresponding node is deleted", func() {
			leaseClient := c.CoordinationV1().Leases(v1.NamespaceNodeLease)
			err := e2enode.WaitForReadyNodes(c, framework.TestContext.CloudConfig.NumNodes, 10*time.Minute)
			gomega.Expect(err).To(gomega.BeNil())

			ginkgo.By("verify node lease exists for every nodes")
			originalNodes := framework.GetReadySchedulableNodesOrDie(c)
			framework.ExpectEqual(len(originalNodes.Items), framework.TestContext.CloudConfig.NumNodes)

			gomega.Eventually(func() error {
				pass := true
				for _, node := range originalNodes.Items {
					if _, err := leaseClient.Get(node.ObjectMeta.Name, metav1.GetOptions{}); err != nil {
						e2elog.Logf("Try to get lease of node %s, but got error: %v", node.ObjectMeta.Name, err)
						pass = false
					}
				}
				if pass {
					return nil
				}
				return fmt.Errorf("some node lease is not ready")
			}, 1*time.Minute, 5*time.Second).Should(gomega.BeNil())

			targetNumNodes := int32(framework.TestContext.CloudConfig.NumNodes - 1)
			ginkgo.By(fmt.Sprintf("decreasing cluster size to %d", targetNumNodes))
			err = framework.ResizeGroup(group, targetNumNodes)
			gomega.Expect(err).To(gomega.BeNil())
			err = framework.WaitForGroupSize(group, targetNumNodes)
			gomega.Expect(err).To(gomega.BeNil())
			err = e2enode.WaitForReadyNodes(c, framework.TestContext.CloudConfig.NumNodes-1, 10*time.Minute)
			gomega.Expect(err).To(gomega.BeNil())
			targetNodes := framework.GetReadySchedulableNodesOrDie(c)
			framework.ExpectEqual(len(targetNodes.Items), int(targetNumNodes))

			ginkgo.By("verify node lease is deleted for the deleted node")
			var deletedNodeName string
			for _, originalNode := range originalNodes.Items {
				originalNodeName := originalNode.ObjectMeta.Name
				for _, targetNode := range targetNodes.Items {
					if originalNodeName == targetNode.ObjectMeta.Name {
						continue
					}
				}
				deletedNodeName = originalNodeName
				break
			}
			gomega.Expect(deletedNodeName).NotTo(gomega.Equal(""))
			gomega.Eventually(func() error {
				if _, err := leaseClient.Get(deletedNodeName, metav1.GetOptions{}); err == nil {
					return fmt.Errorf("node lease is not deleted yet for node %q", deletedNodeName)
				}
				return nil
			}, 1*time.Minute, 5*time.Second).Should(gomega.BeNil())

			ginkgo.By("verify node leases still exist for remaining nodes")
			gomega.Eventually(func() error {
				for _, node := range targetNodes.Items {
					if _, err := leaseClient.Get(node.ObjectMeta.Name, metav1.GetOptions{}); err != nil {
						return err
					}
				}
				return nil
			}, 1*time.Minute, 5*time.Second).Should(gomega.BeNil())
		})
	})
})