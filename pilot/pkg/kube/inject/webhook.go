// Copyright 2018 Istio Authors
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

package inject

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ghodss/yaml"
	"github.com/howeyc/fsnotify"

	meshconfig "istio.io/api/mesh/v1alpha1"
	"istio.io/istio/pilot/cmd"
	"istio.io/istio/pkg/log"

	"k8s.io/api/admission/v1beta1"
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/kubernetes/pkg/apis/core/v1"
)

var (
	runtimeScheme = runtime.NewScheme()
	codecs        = serializer.NewCodecFactory(runtimeScheme)
	deserializer  = codecs.UniversalDeserializer()

	// TODO(https://github.com/kubernetes/kubernetes/issues/57982)
	defaulter = runtime.ObjectDefaulter(runtimeScheme)
)

const (
	watchDebounceDelay = 100 * time.Millisecond
)

func init() {
	_ = corev1.AddToScheme(runtimeScheme)
	_ = admissionregistrationv1beta1.AddToScheme(runtimeScheme)

	// The `v1` package from k8s.io/kubernetes/pkgp/apis/core/v1 has
	// the object defaulting functions which are not included in
	// k8s.io/api/corev1. The default functions are required by
	// runtime.ObjectDefaulter to workaround lack of server-side
	// defaulting with webhooks (see
	// https://github.com/kubernetes/kubernetes/issues/57982).
	_ = v1.AddToScheme(runtimeScheme)
}

// Webhook implements a mutating webhook for automatic proxy injection.
type Webhook struct {
	mu                     sync.RWMutex
	sidecarConfig          *Config
	sidecarTemplateVersion string
	meshConfig             *meshconfig.MeshConfig

	server     *http.Server
	meshFile   string
	configFile string
	watcher    *fsnotify.Watcher
}

func loadConfig(injectFile, meshFile string) (*Config, *meshconfig.MeshConfig, error) {
	data, err := ioutil.ReadFile(injectFile)
	if err != nil {
		return nil, nil, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil { // nolint: vetshadow
		return nil, nil, err
	}
	meshConfig, err := cmd.ReadMeshConfig(meshFile)
	if err != nil {
		return nil, nil, err
	}

	log.Infof("New configuration: sha256sum %x", sha256.Sum256(data))
	log.Infof("Policy: %v", c.Policy)
	log.Infof("Template: |\n  %v", strings.Replace(c.Template, "\n", "\n  ", -1))

	return &c, meshConfig, nil
}

// NewWebhook creates a new instance of a mutating webhook for automatic sidecar injection.
func NewWebhook(configFile, meshFile, certFile, keyFile string, port int) (*Webhook, error) {
	sidecarConfig, meshConfig, err := loadConfig(configFile, meshFile)
	if err != nil {
		return nil, err
	}
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	// watch the parent directory of the target files so we can catch
	// symlink updates of k8s ConfigMaps volumes.
	for _, file := range []string{configFile, meshFile} {
		watchDir, _ := filepath.Split(file)
		if err := watcher.Watch(watchDir); err != nil {
			return nil, fmt.Errorf("could not watch %v: %v", file, err)
		}
	}

	wh := &Webhook{
		server: &http.Server{
			Addr:      fmt.Sprintf(":%v", port),
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{pair}},
			// mtls disabled because apiserver webhook cert usage is still TBD.
		},
		sidecarConfig:          sidecarConfig,
		sidecarTemplateVersion: sidecarTemplateVersionHash(sidecarConfig.Template),
		meshConfig:             meshConfig,
		configFile:             configFile,
		meshFile:               meshFile,
		watcher:                watcher,
	}
	http.HandleFunc("/inject", wh.serveInject)
	return wh, nil
}

// Run implements the webhook server
func (wh *Webhook) Run(stop <-chan struct{}) {
	go func() {
		if err := wh.server.ListenAndServeTLS("", ""); err != nil {
			log.Errorf("ListenAndServeTLS for admission webhook returned error: %v", err)
		}
	}()
	defer wh.watcher.Close() // nolint: errcheck
	defer wh.server.Close()  // nolint: errcheck

	var timerC <-chan time.Time
	for {
		select {
		case <-timerC:
			sidecarConfig, meshConfig, err := loadConfig(wh.configFile, wh.meshFile)
			if err != nil {
				log.Errorf("update error: %v", err)
				break
			}

			version := sidecarTemplateVersionHash(sidecarConfig.Template)

			wh.mu.Lock()
			wh.sidecarConfig = sidecarConfig
			wh.sidecarTemplateVersion = version
			wh.meshConfig = meshConfig
			wh.mu.Unlock()
		case event := <-wh.watcher.Event:
			// use a timer to debounce configuration updates
			if event.IsModify() || event.IsCreate() {
				timerC = time.After(watchDebounceDelay)
			}
		case err := <-wh.watcher.Error:
			log.Errorf("Watcher error: %v", err)
		case <-stop:
			return
		}
	}
}

// TODO(https://github.com/kubernetes/kubernetes/issues/57982)
// remove this workaround once server-side defaulting is fixed.
func applyDefaultsWorkaround(initContainers, containers []corev1.Container, volumes []corev1.Volume) {
	// runtime.ObjectDefaulter only accepts top-level resources. Construct
	// a dummy pod with fields we needed defaulted.
	defaulter.Default(&corev1.Pod{
		Spec: corev1.PodSpec{
			InitContainers: initContainers,
			Containers:     containers,
			Volumes:        volumes,
		},
	})
}

// It would be great to use https://github.com/mattbaird/jsonpatch to
// generate RFC6902 JSON patches. Unfortunately, it doesn't produce
// correct patches for object removal. Fortunately, our patching needs
// are fairly simple so generating them manually isn't horrible (yet).
type rfc6902PatchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value"`
}

// JSONPatch `remove` is applied sequentially. Remove items in reverse
// order to avoid renumbering indices.
func removeContainers(containers []corev1.Container, removed []string, path string) (patch []rfc6902PatchOperation) {
	names := map[string]bool{}
	for _, name := range removed {
		names[name] = true
	}
	for i := len(containers) - 1; i >= 0; i-- {
		if _, ok := names[containers[i].Name]; ok {
			patch = append(patch, rfc6902PatchOperation{
				Op:   "remove",
				Path: fmt.Sprintf("%v/%v", path, i),
			})
		}
	}
	return patch
}
func removeVolumes(volumes []corev1.Volume, removed []string, path string) (patch []rfc6902PatchOperation) {
	names := map[string]bool{}
	for _, name := range removed {
		names[name] = true
	}
	for i := len(volumes) - 1; i >= 0; i-- {
		if _, ok := names[volumes[i].Name]; ok {
			patch = append(patch, rfc6902PatchOperation{
				Op:   "remove",
				Path: fmt.Sprintf("%v/%v", path, i),
			})
		}
	}
	return patch
}

func addContainer(target, added []corev1.Container, basePath string) (patch []rfc6902PatchOperation) {
	first := len(target) == 0
	var value interface{}
	for _, add := range added {
		value = add
		path := basePath
		if first {
			first = false
			value = []corev1.Container{add}
		} else {
			path = path + "/-"
		}
		patch = append(patch, rfc6902PatchOperation{
			Op:    "add",
			Path:  path,
			Value: value,
		})
	}
	return patch
}

func addVolume(target, added []corev1.Volume, basePath string) (patch []rfc6902PatchOperation) {
	first := len(target) == 0
	var value interface{}
	for _, add := range added {
		value = add
		path := basePath
		if first {
			first = false
			value = []corev1.Volume{add}
		} else {
			path = path + "/-"
		}
		patch = append(patch, rfc6902PatchOperation{
			Op:    "add",
			Path:  path,
			Value: value,
		})
	}
	return patch
}

// escape JSON Pointer value per https://tools.ietf.org/html/rfc6901
func escapeJSONPointerValue(in string) string {
	step := strings.Replace(in, "~", "~0", -1)
	return strings.Replace(step, "/", "~1", -1)
}

func updateAnnotation(target map[string]string, added map[string]string) (patch []rfc6902PatchOperation) {
	for key, value := range added {
		if target == nil {
			target = map[string]string{}
			patch = append(patch, rfc6902PatchOperation{
				Op:   "add",
				Path: "/metadata/annotations",
				Value: map[string]string{
					key: value,
				},
			})
		} else {
			op := "add"
			if target[key] != "" {
				op = "replace"
			}
			patch = append(patch, rfc6902PatchOperation{
				Op:    op,
				Path:  "/metadata/annotations/" + escapeJSONPointerValue(key),
				Value: value,
			})
		}
	}
	return patch
}

func createPatch(pod *corev1.Pod, prevStatus *SidecarInjectionStatus, annotations map[string]string, sic *SidecarInjectionSpec) ([]byte, error) {
	var patch []rfc6902PatchOperation

	// Remove any containers previously injected by kube-inject using
	// container and volume name as unique key for removal.
	patch = append(patch, removeContainers(pod.Spec.InitContainers, prevStatus.InitContainers, "/spec/initContainers")...)
	patch = append(patch, removeContainers(pod.Spec.Containers, prevStatus.Containers, "/spec/containers")...)
	patch = append(patch, removeVolumes(pod.Spec.Volumes, prevStatus.Volumes, "/spec/volumes")...)

	patch = append(patch, addContainer(pod.Spec.InitContainers, sic.InitContainers, "/spec/initContainers")...)
	patch = append(patch, addContainer(pod.Spec.Containers, sic.Containers, "/spec/containers")...)
	patch = append(patch, addVolume(pod.Spec.Volumes, sic.Volumes, "/spec/volumes")...)

	patch = append(patch, updateAnnotation(pod.Annotations, annotations)...)

	return json.Marshal(patch)
}

// Retain deprecated hardcoded container and volumes names to aid in
// backwards compatible migration to the new SidecarInjectionStatus.
var (
	legacyInitContainerNames = []string{"istio-init", "enable-core-dump"}
	legacyContainerNames     = []string{"istio-proxy"}
	legacyVolumeNames        = []string{"istio-certs", "istio-envoy"}
)

func injectionStatus(pod *corev1.Pod) *SidecarInjectionStatus {
	var statusBytes []byte
	if pod.ObjectMeta.Annotations != nil {
		if value, ok := pod.ObjectMeta.Annotations[istioSidecarAnnotationStatusKey]; ok {
			statusBytes = []byte(value)
		}
	}

	// default case when injected pod has explicit status
	var status SidecarInjectionStatus
	if err := json.Unmarshal(statusBytes, &status); err == nil {
		// heuristic assumes status is valid if any of the resource
		// lists is non-empty.
		if len(status.InitContainers) != 0 ||
			len(status.Containers) != 0 ||
			len(status.Volumes) != 0 {
			return &status
		}
	}

	// backwards compatibility case when injected pod has legacy
	// status. Infer status from the list of legacy hardcoded
	// container and volume names.
	return &SidecarInjectionStatus{
		InitContainers: legacyInitContainerNames,
		Containers:     legacyContainerNames,
		Volumes:        legacyVolumeNames,
	}
}

func toAdmissionResponse(err error) *v1beta1.AdmissionResponse {
	return &v1beta1.AdmissionResponse{Result: &metav1.Status{Message: err.Error()}}
}

func (wh *Webhook) inject(ar *v1beta1.AdmissionReview) *v1beta1.AdmissionResponse {
	req := ar.Request
	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		log.Errorf("Could not unmarshal raw object: %v", err)
		return toAdmissionResponse(err)
	}

	log.Infof("AdmissionReview for Kind=%v Namespace=%v Name=%v (%v) UID=%v Rfc6902PatchOperation=%v UserInfo=%v",
		req.Kind, req.Namespace, req.Name, pod.Name, req.UID, req.Operation, req.UserInfo)
	log.Debugf("Object: %v", string(req.Object.Raw))
	log.Debugf("OldObject: %v", string(req.OldObject.Raw))

	if !injectRequired(ignoredNamespaces, wh.sidecarConfig.Policy, &pod.Spec, &pod.ObjectMeta) {
		log.Infof("Skipping %s/%s due to policy check", pod.Namespace, pod.Name)
		return &v1beta1.AdmissionResponse{
			Allowed: true,
		}
	}

	spec, status, err := injectionData(wh.sidecarConfig.Template, wh.sidecarTemplateVersion, &pod.Spec, &pod.ObjectMeta, wh.meshConfig.DefaultConfig, wh.meshConfig) // nolint: lll
	if err != nil {
		return toAdmissionResponse(err)
	}
	applyDefaultsWorkaround(spec.InitContainers, spec.Containers, spec.Volumes)
	annotations := map[string]string{istioSidecarAnnotationStatusKey: status}

	patchBytes, err := createPatch(&pod, injectionStatus(&pod), annotations, spec)
	if err != nil {
		return toAdmissionResponse(err)
	}

	log.Infof("AdmissionResponse: patch=%v\n", string(patchBytes))

	reviewResponse := v1beta1.AdmissionResponse{
		Allowed: true,
		Patch:   patchBytes,
		PatchType: func() *v1beta1.PatchType {
			pt := v1beta1.PatchTypeJSONPatch
			return &pt
		}(),
	}
	return &reviewResponse
}

func (wh *Webhook) serveInject(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		if data, err := ioutil.ReadAll(r.Body); err == nil {
			body = data
		}
	}

	// verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		log.Errorf("contentType=%s, expect application/json", contentType)
		return
	}

	var reviewResponse *v1beta1.AdmissionResponse
	ar := v1beta1.AdmissionReview{}
	if _, _, err := deserializer.Decode(body, nil, &ar); err != nil {
		log.Errorf("Could not decode AdmissionReview: %v", err)
		reviewResponse = toAdmissionResponse(err)
	} else {
		reviewResponse = wh.inject(&ar)
	}

	response := v1beta1.AdmissionReview{}
	if reviewResponse != nil {
		response.Response = reviewResponse
		response.Response.UID = ar.Request.UID
	}

	// reset the Object and OldObject, they are not needed in a response.
	ar.Request.Object = runtime.RawExtension{}
	ar.Request.OldObject = runtime.RawExtension{}

	resp, err := json.Marshal(response)
	if err != nil {
		log.Errorf("Could not encode AdmissionResponse: %v", err)
	}
	if _, err := w.Write(resp); err != nil {
		log.Errorf("Could not write AdmissionResponse: %v", err)
	}
}
