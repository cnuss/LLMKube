//go:build e2e

/*
Copyright 2026.

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

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/defilantech/llmkube/test/utils"
)

// The multi-node prefetch e2e (#2136) needs the cluster test/e2e/kind-multinode.yaml
// describes: two workers sharing one host directory, one of them labeled as
// the GPU node. The prefetch Job must run on the download worker, write into
// a claim backed by that directory, and a serving pod pinned to the GPU
// worker must consume it read-only with no downloader. envtest has no nodes
// and the other e2e clusters have one, so nothing else exercises the
// cross-node flow the feature exists for.
const (
	multinodeTestNs = "e2e-multinode-prefetch"
	// multinodeServingLabel marks the GPU worker; the Model's prefetchAffinity
	// keeps the Job off it, as the docs' rule does.
	multinodeServingLabel = "nvidia.com/gpu.present"
	// multinodeSharedMountPath is where kind-multinode.yaml mounts the shared
	// host directory in both workers.
	multinodeSharedMountPath = "/mnt/llmkube-shared-cache"
	multinodeSharedPV        = "llmkube-e2e-shared-cache"
	multinodeSharedClaim     = "shared-model-cache"
	multinodeStorageClass    = "llmkube-e2e-shared-hostpath"
	multinodeModel           = "shared-prefetch-model"
	multinodeService         = "shared-prefetch-svc"
)

var multinodeModelServerURL = fmt.Sprintf("http://test-model-server.%s.svc.cluster.local/test-model.gguf", multinodeTestNs)

var _ = Describe("Multi-node prefetch through a shared cache claim", Ordered, func() {
	kubectl := func(args ...string) (string, error) {
		return utils.Run(exec.Command("kubectl", args...))
	}
	apply := func(manifest string) {
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(manifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
	}
	nodeIsServing := func(node string) bool {
		out, err := kubectl("get", "node", node, "-o", `jsonpath={.metadata.labels.nvidia\.com/gpu\.present}`)
		Expect(err).NotTo(HaveOccurred())
		return out == "true"
	}

	BeforeAll(func() {
		if os.Getenv("LLMKUBE_E2E_MULTINODE") != "true" {
			Skip("LLMKUBE_E2E_MULTINODE not set; the multi-node prefetch e2e needs the two-worker " +
				"kind cluster make setup-test-e2e-multinode creates")
		}

		By("checking the cluster has a serving worker and a download worker")
		serving, err := kubectl("get", "nodes", "-l", multinodeServingLabel+"=true", "-o", "name")
		Expect(err).NotTo(HaveOccurred())
		Expect(serving).NotTo(BeEmpty(), "no worker carries %s=true; create the cluster with make setup-test-e2e-multinode "+
			"(run make cleanup-test-e2e first if a single-node cluster of the same name is up)", multinodeServingLabel)
		download, err := kubectl("get", "nodes", "-l", "!"+multinodeServingLabel+",!node-role.kubernetes.io/control-plane", "-o", "name")
		Expect(err).NotTo(HaveOccurred())
		Expect(download).NotTo(BeEmpty(), "no worker without %s to run the download on", multinodeServingLabel)

		By("creating manager namespace")
		_, err = kubectl("create", "ns", namespace)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		_, err = kubectl("label", "--overwrite", "ns", namespace, "pod-security.kubernetes.io/enforce=restricted")
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("installing CRDs")
		_, err = utils.Run(exec.Command("make", "install"))
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		_, err = utils.Run(exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", projectImage)))
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")

		// The serving pods carry the PriorityClass the Helm chart ships; the
		// kustomize deploy used here does not install it, and a ReplicaSet
		// cannot create a pod that names a missing class.
		By("creating the PriorityClass the serving pods name")
		apply(`apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: llmkube-normal
value: 10000
preemptionPolicy: PreemptLowerPriority
globalDefault: false
`)

		By("creating the test namespace and the model server")
		_, err = kubectl("create", "ns", multinodeTestNs)
		Expect(err).NotTo(HaveOccurred())
		apply(fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: test-model-data
  namespace: %[1]s
binaryData:
  test-model.gguf: ZmFrZS1nZ3VmLWRhdGE=
---
apiVersion: v1
kind: Pod
metadata:
  name: test-model-server
  namespace: %[1]s
  labels:
    app: test-model-server
spec:
  containers:
  - name: nginx
    image: nginx:1.27-alpine
    ports:
    - containerPort: 80
    volumeMounts:
    - name: model-data
      mountPath: /usr/share/nginx/html
  volumes:
  - name: model-data
    configMap:
      name: test-model-data
---
apiVersion: v1
kind: Service
metadata:
  name: test-model-server
  namespace: %[1]s
spec:
  selector:
    app: test-model-server
  ports:
  - port: 80
    targetPort: 80
`, multinodeTestNs))
		Eventually(func(g Gomega) {
			out, err := kubectl("get", "pod", "test-model-server", "-n", multinodeTestNs, "-o", "jsonpath={.status.phase}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("Running"))
		}, 2*time.Minute).Should(Succeed())

		// The claim both workers reach: a static ReadWriteMany PersistentVolume
		// on the directory kind mounts into each worker. No provisioner is
		// involved, so the claim binds at once and has no node affinity.
		By("creating the shared cache claim")
		apply(fmt.Sprintf(`apiVersion: v1
kind: PersistentVolume
metadata:
  name: %[1]s
spec:
  capacity:
    storage: 2Gi
  accessModes: [ReadWriteMany]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: %[2]s
  hostPath:
    path: %[3]s
    type: DirectoryOrCreate
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %[4]s
  namespace: %[5]s
spec:
  accessModes: [ReadWriteMany]
  storageClassName: %[2]s
  volumeName: %[1]s
  resources:
    requests:
      storage: 2Gi
`, multinodeSharedPV, multinodeStorageClass, multinodeSharedMountPath, multinodeSharedClaim, multinodeTestNs))
		Eventually(func(g Gomega) {
			out, err := kubectl("get", "pvc", multinodeSharedClaim, "-n", multinodeTestNs, "-o", "jsonpath={.status.phase}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("Bound"))
		}, time.Minute).Should(Succeed())
	})

	// A failure here is a scheduling or admission question on a cluster the
	// run tears down, so the state is dumped before cleanup runs.
	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}
		for _, args := range [][]string{
			{"get", "deploy,rs,pods,pvc,jobs", "-n", multinodeTestNs, "-o", "wide"},
			{"describe", "deployment", multinodeService, "-n", multinodeTestNs},
			{"describe", "rs", "-n", multinodeTestNs},
			{"get", "events", "-n", multinodeTestNs, "--sort-by=.lastTimestamp"},
			{"get", "model", multinodeModel, "-n", multinodeTestNs, "-o", "yaml"},
			{"get", "inferenceservice", multinodeService, "-n", multinodeTestNs, "-o", "yaml"},
			{"logs", "-n", namespace, "-l", "control-plane=controller-manager", "--tail=80"},
		} {
			out, err := kubectl(args...)
			_, _ = fmt.Fprintf(GinkgoWriter, "\n==> kubectl %s (err=%v)\n%s\n", strings.Join(args, " "), err, out)
		}
	})

	AfterAll(func() {
		if os.Getenv("LLMKUBE_E2E_MULTINODE") != "true" {
			return
		}
		By("cleaning up the test namespace, the shared volume and the PriorityClass")
		_, _ = kubectl("delete", "ns", multinodeTestNs, "--ignore-not-found", "--wait=false")
		_, _ = kubectl("delete", "pv", multinodeSharedPV, "--ignore-not-found", "--wait=false")
		_, _ = kubectl("delete", "priorityclass", "llmkube-normal", "--ignore-not-found")

		By("undeploying the controller-manager")
		_, _ = utils.Run(exec.Command("make", "undeploy"))

		By("uninstalling CRDs")
		_, _ = utils.Run(exec.Command("make", "uninstall"))

		By("removing manager namespace")
		_, _ = kubectl("delete", "ns", namespace, "--ignore-not-found")
	})

	It("stages on the download worker and serves from the GPU worker with no downloader", func() {
		By("applying a Model that names the shared claim and places its prefetch off the GPU worker, and a service that asks for the staging and waits for it")
		apply(fmt.Sprintf(`apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  source: "%[3]s"
  modelCache:
    claimName: %[4]s
  prefetchAffinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
          - matchExpressions:
              - key: %[5]s
                operator: DoesNotExist
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: %[6]s
  namespace: %[2]s
spec:
  modelRef: %[1]s
  nodeSelector:
    %[5]s: "true"
  modelCache:
    claimName: %[4]s
    prefetch: true
`, multinodeModel, multinodeTestNs, multinodeModelServerURL, multinodeSharedClaim, multinodeServingLabel, multinodeService))

		By("waiting for the prefetch into the shared claim to succeed")
		entryPhase := fmt.Sprintf(`jsonpath={.status.prefetch[?(@.claimName=="%s")].phase}`, multinodeSharedClaim)
		Eventually(func(g Gomega) {
			out, err := kubectl("get", "model", multinodeModel, "-n", multinodeTestNs, "-o", entryPhase)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("Succeeded"))
		}, 4*time.Minute).Should(Succeed())
		phase, err := kubectl("get", "model", multinodeModel, "-n", multinodeTestNs, "-o", "jsonpath={.status.phase}")
		Expect(err).NotTo(HaveOccurred())
		Expect(phase).To(Equal("Ready"))

		By("checking the Job's pod ran on the download worker")
		// The Model does not prefetch itself, so the only target is the one
		// the service asked for: a claim-scoped Job, which proves the gate
		// and not the Model's own Ready.
		jobName, err := kubectl("get", "model", multinodeModel, "-n", multinodeTestNs, "-o",
			fmt.Sprintf(`jsonpath={.status.prefetch[?(@.claimName=="%s")].jobName}`, multinodeSharedClaim))
		Expect(err).NotTo(HaveOccurred())
		Expect(jobName).To(HavePrefix(multinodeModel+"-prefetch-"), "a service-requested target gets a claim-scoped Job")
		jobNode, err := kubectl("get", "pods", "-n", multinodeTestNs, "-l", "batch.kubernetes.io/job-name="+jobName,
			"-o", "jsonpath={.items[*].spec.nodeName}")
		Expect(err).NotTo(HaveOccurred())
		Expect(jobNode).NotTo(BeEmpty(), "the prefetch Job left no pod to inspect")
		for _, node := range strings.Fields(jobNode) {
			Expect(nodeIsServing(node)).To(BeFalse(), "the prefetch Job pod ran on the GPU worker %s", node)
		}

		By("checking the service created its Deployment only after the prefetch completed")
		completed, err := kubectl("get", "job", jobName, "-n", multinodeTestNs, "-o", "jsonpath={.status.completionTime}")
		Expect(err).NotTo(HaveOccurred())
		completedAt, err := time.Parse(time.RFC3339, completed)
		Expect(err).NotTo(HaveOccurred(), "job completionTime %q", completed)
		var createdAt time.Time
		Eventually(func(g Gomega) {
			out, err := kubectl("get", "deployment", multinodeService, "-n", multinodeTestNs, "-o", "jsonpath={.metadata.creationTimestamp}")
			g.Expect(err).NotTo(HaveOccurred())
			createdAt, err = time.Parse(time.RFC3339, out)
			g.Expect(err).NotTo(HaveOccurred())
		}, 2*time.Minute).Should(Succeed())
		Expect(createdAt.Before(completedAt)).To(BeFalse(),
			"Deployment created at %s, before the prefetch completed at %s", createdAt, completedAt)

		By("checking the pod template consumes the claim read-only with no downloader")
		inits, err := kubectl("get", "deployment", multinodeService, "-n", multinodeTestNs,
			"-o", "jsonpath={.spec.template.spec.initContainers[*].name}")
		Expect(err).NotTo(HaveOccurred())
		Expect(inits).To(Equal("model-verify"))
		claim, err := kubectl("get", "deployment", multinodeService, "-n", multinodeTestNs,
			"-o", `jsonpath={.spec.template.spec.volumes[?(@.name=="model-cache")].persistentVolumeClaim.claimName}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(claim).To(Equal(multinodeSharedClaim))
		readOnly, err := kubectl("get", "deployment", multinodeService, "-n", multinodeTestNs,
			"-o", `jsonpath={.spec.template.spec.volumes[?(@.name=="model-cache")].persistentVolumeClaim.readOnly}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(readOnly).To(Equal("true"))

		// The bytes the Job wrote on the download worker are read on the GPU
		// worker: model-verify finds them and exits 0. The runtime container
		// then fails on the fake GGUF, which is not under test here.
		By("waiting for the serving pod on the GPU worker to get past model-verify")
		Eventually(func(g Gomega) {
			node, err := kubectl("get", "pods", "-n", multinodeTestNs, "-l", "inference.llmkube.dev/service="+multinodeService,
				"-o", "jsonpath={.items[0].spec.nodeName}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(node).NotTo(BeEmpty())
			g.Expect(nodeIsServing(node)).To(BeTrue(), "serving pod landed on %s, not the GPU worker", node)
			state, err := kubectl("get", "pods", "-n", multinodeTestNs, "-l", "inference.llmkube.dev/service="+multinodeService,
				"-o", `jsonpath={.items[0].status.initContainerStatuses[?(@.name=="model-verify")].state.terminated.exitCode}`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(state).To(Equal("0"))
		}, 5*time.Minute).Should(Succeed())

		By("checking the service reports the prefetch as satisfied")
		cond, err := kubectl("get", "inferenceservice", multinodeService, "-n", multinodeTestNs,
			"-o", `jsonpath={.status.conditions[?(@.type=="ModelPrefetched")].status}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(cond).To(Equal("True"))
	})
})
