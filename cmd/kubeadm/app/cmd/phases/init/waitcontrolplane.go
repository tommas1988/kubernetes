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

package phases

import (
	"fmt"
	"io"
	"net/http"
	"text/template"
	"time"

	"github.com/lithammer/dedent"
	"github.com/pkg/errors"

	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"k8s.io/kubernetes/cmd/kubeadm/app/cmd/phases/workflow"
	"k8s.io/kubernetes/cmd/kubeadm/app/util/apiclient"
	dryrunutil "k8s.io/kubernetes/cmd/kubeadm/app/util/dryrun"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm"
	"k8s.io/kubernetes/cmd/kubeadm/app/phases/controlplane"
	"k8s.io/kubernetes/pkg/probe"
	httpprobe "k8s.io/kubernetes/pkg/probe/http"
)

var (
	kubeletFailTempl = template.Must(template.New("init").Parse(dedent.Dedent(`
	Unfortunately, an error has occurred:
		{{ .Error }}

	This error is likely caused by:
		- The kubelet is not running
		- The kubelet is unhealthy due to a misconfiguration of the node in some way (required cgroups disabled)

	If you are on a systemd-powered system, you can try to troubleshoot the error with the following commands:
		- 'systemctl status kubelet'
		- 'journalctl -xeu kubelet'

	Additionally, a control plane component may have crashed or exited when started by the container runtime.
	To troubleshoot, list all containers using your preferred container runtimes CLI.
	Here is one example how you may list all running Kubernetes containers by using crictl:
		- 'crictl --runtime-endpoint {{ .Socket }} ps -a | grep kube | grep -v pause'
		Once you have found the failing container, you can inspect its logs with:
		- 'crictl --runtime-endpoint {{ .Socket }} logs CONTAINERID'
	`)))
)

// NewWaitControlPlanePhase is a hidden phase that runs after the control-plane and etcd phases
func NewWaitControlPlanePhase() workflow.Phase {
	phase := workflow.Phase{
		Name:   "wait-control-plane",
		Run:    runWaitControlPlanePhase,
		Hidden: true,
	}
	return phase
}

func runWaitControlPlanePhase(c workflow.RunData) error {
	data, ok := c.(InitData)
	if !ok {
		return errors.New("wait-control-plane phase invoked with an invalid data struct")
	}

	// If we're dry-running, print the generated manifests.
	// TODO: think of a better place to move this call - e.g. a hidden phase.
	if data.DryRun() {
		if err := dryrunutil.PrintFilesIfDryRunning(true /* needPrintManifest */, data.ManifestDir(), data.OutputWriter()); err != nil {
			return errors.Wrap(err, "error printing files on dryrun")
		}
	}

	// waiter holds the apiclient.Waiter implementation of choice, responsible for querying the API server in various ways and waiting for conditions to be fulfilled
	klog.V(1).Infoln("[wait-control-plane] Waiting for the API server to be healthy")

	client, err := data.Client()
	if err != nil {
		return errors.Wrap(err, "cannot obtain client")
	}

	timeout := data.Cfg().ClusterConfiguration.APIServer.TimeoutForControlPlane.Duration
	waiter, err := newControlPlaneWaiter(data.DryRun(), timeout, client, data.OutputWriter())
	if err != nil {
		return errors.Wrap(err, "error creating waiter")
	}

	fmt.Printf("[wait-control-plane] Waiting for the kubelet to boot up the control plane as static Pods from directory %q. This can take up to %v\n", data.ManifestDir(), timeout)

	prober := getControlPlaneComponentStartupProber(data.Cfg())
	if err := waiter.WaitForKubeletAndFunc(func() error {
		start := time.Now()
		return wait.PollImmediate(10*time.Second, timeout, func() (bool, error) {
			if started := prober.isStartup(); !started {
				return false, nil
			} else {
				fmt.Printf("[wait-control-plane] All control plane components are startup after %f seconds\n", time.Since(start).Seconds())
				return true, nil
			}
		})
	}); err != nil {
		context := struct {
			Error  string
			Socket string
		}{
			Error:  fmt.Sprintf("%v", err),
			Socket: data.Cfg().NodeRegistration.CRISocket,
		}

		kubeletFailTempl.Execute(data.OutputWriter(), context)
		return errors.New("couldn't initialize a Kubernetes cluster")
	}

	return nil
}

// newControlPlaneWaiter returns a new waiter that is used to wait on the control plane to boot up.
func newControlPlaneWaiter(dryRun bool, timeout time.Duration, client clientset.Interface, out io.Writer) (apiclient.Waiter, error) {
	if dryRun {
		return dryrunutil.NewWaiter(), nil
	}

	return apiclient.NewKubeWaiter(client, timeout, out), nil
}

type startupResult bool

type startupProber []func(ch chan<- startupResult)

// getControlPlaneComponentStartupProber get all control plane component startup probes
func getControlPlaneComponentStartupProber(cfg *kubeadm.InitConfiguration) startupProber {
	specs := controlplane.GetStaticPodSpecs(&cfg.ClusterConfiguration, &cfg.LocalAPIEndpoint, nil)
	prober := startupProber{}
	for name, podSpec := range specs {
		for _, container := range podSpec.Spec.Containers {
			p := container.StartupProbe
			// Currently control plane component only use http probe
			if p.HTTPGet != nil {
				req, err := httpprobe.NewRequestForHTTPGetAction(p.HTTPGet, &container, "", "kubeadm")
				if err != nil {
					klog.V(2).ErrorS(err, "[wait-control-plane] Failed to create control plane component startup probe request", "component", name)
					continue
				}

				timeout := time.Duration(p.TimeoutSeconds) * time.Second
				f := func(req *http.Request, component string) func(chan<- startupResult) {
					return func(ch chan<- startupResult) {
						prober := httpprobe.New(false)
						result, msg, err := prober.Probe(req, timeout)
						if result != probe.Success {
							err = errors.New(msg)
						}

						if err != nil {
							klog.V(2).ErrorS(err, "[wait-control-plane] Failed to startup control plane component", "component", component)
							ch <- false
						} else {
							ch <- true
						}
					}
				}(req, name)

				prober = append(prober, f)
			}
		}
	}

	return prober
}

// isStartup returns true if all startup probes success
func (p startupProber) isStartup() bool {
	ch := make(chan startupResult, len(p))

	for _, r := range p {
		go r(ch)
	}

	for range p {
		if result := <-ch; !result {
			return false
		}
	}

	return true
}
