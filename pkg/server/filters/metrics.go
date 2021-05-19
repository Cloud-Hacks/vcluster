package filters

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"github.com/loft-sh/vcluster/pkg/constants"
	"github.com/loft-sh/vcluster/pkg/controllers/resources/nodes"
	"github.com/loft-sh/vcluster/pkg/metrics"
	"github.com/loft-sh/vcluster/pkg/server/handler"
	requestpkg "github.com/loft-sh/vcluster/pkg/util/request"
	"github.com/prometheus/common/expfmt"
	"io/ioutil"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/client-go/rest"
	statsv1alpha1 "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	"net/http"
	"net/http/httptest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strconv"
	"strings"
)

func WithNodesProxy(h http.Handler, localManager ctrl.Manager, virtualManager ctrl.Manager, targetNamespace string) http.Handler {
	s := serializer.NewCodecFactory(virtualManager.GetScheme())
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		info, ok := request.RequestInfoFrom(req.Context())
		if !ok {
			requestpkg.FailWithStatus(w, req, http.StatusInternalServerError, fmt.Errorf("request info is missing"))
			return
		}

		if isNodesProxy(info) {
			// rewrite node port if there is one
			splitted := strings.Split(req.URL.Path, "/")
			if len(splitted) < 5 {
				responsewriters.ErrorNegotiated(kerrors.NewBadRequest("unexpected url"), s, corev1.SchemeGroupVersion, w, req)
				return
			}

			// make sure we keep the prefix and suffix
			targetNode := splitted[4]
			splittedName := strings.Split(targetNode, ":")
			if len(splittedName) == 2 || len(splittedName) == 3 {
				port := splittedName[1]
				if len(splittedName) == 3 {
					port = splittedName[2]
				}

				// delete port if it is the default one
				if port == strconv.Itoa(int(nodes.KubeletPort)) {
					if len(splittedName) == 2 {
						targetNode = splittedName[0]
					} else {
						targetNode = splittedName[0] + ":" + splittedName[1] + ":"
					}
				}
			}

			// exchange node name
			splitted[4] = targetNode
			req.URL.Path = strings.Join(splitted, "/")

			// execute the request
			_, err := handleNodeRequest(localManager.GetConfig(), virtualManager.GetClient(), targetNamespace, w, req)
			if err != nil {
				responsewriters.ErrorNegotiated(err, s, corev1.SchemeGroupVersion, w, req)
				return
			}
			return
		}

		h.ServeHTTP(w, req)
	})
}

func writeWithHeader(w http.ResponseWriter, code int, header http.Header, body []byte) {
	// delete old header
	for k := range w.Header() {
		w.Header().Del(k)
	}
	for k, v := range header {
		for _, s := range v {
			w.Header().Add(k, s)
		}
	}

	w.WriteHeader(code)
	w.Write(body)
}

func rewritePrometheusMetrics(req *http.Request, data []byte, targetNamespace string, vClient client.Client) ([]byte, error) {
	metricsFamilies, err := metrics.Decode(data)
	if err != nil {
		return nil, err
	}

	metricsFamilies, err = metrics.Rewrite(req.Context(), metricsFamilies, targetNamespace, vClient)
	if err != nil {
		return nil, err
	}

	return metrics.Encode(metricsFamilies, expfmt.Negotiate(req.Header))
}

func handleNodeRequest(localConfig *rest.Config, vClient client.Client, targetNamespace string, w http.ResponseWriter, req *http.Request) (bool, error) {
	// authorization was done here already so we will just go forward with the rewrite
	req.Header.Del("Authorization")
	h, err := handler.Handler("", localConfig, nil)
	if err != nil {
		return false, err
	}

	code, header, data, err := executeRequest(req, h)
	if err != nil {
		return false, err
	} else if code != http.StatusOK {
		writeWithHeader(w, code, header, data)
		return false, nil
	}

	// now rewrite the metrics
	newData := data
	if IsKubeletMetrics(req.URL.Path) {
		newData, err = rewritePrometheusMetrics(req, data, targetNamespace, vClient)
		if err != nil {
			return false, err
		}
	} else if IsKubeletStats(req.URL.Path) {
		newData, err = rewriteStats(req.Context(), data, targetNamespace, vClient)
		if err != nil {
			return false, err
		}
	}

	w.Header().Set("Content-Type", string(expfmt.Negotiate(req.Header)))
	w.WriteHeader(code)
	w.Write(newData)
	return true, nil
}

func rewriteStats(ctx context.Context, data []byte, targetNamespace string, vClient client.Client) ([]byte, error) {
	stats := &statsv1alpha1.Summary{}
	err := json.Unmarshal(data, stats)
	if err != nil {
		return nil, err
	}

	// rewrite pods
	newPods := []statsv1alpha1.PodStats{}
	for _, pod := range stats.Pods {
		if pod.PodRef.Namespace != targetNamespace {
			continue
		}

		// search if we can find the pod by name in the virtual cluster
		podList := &corev1.PodList{}
		err := vClient.List(ctx, podList, client.MatchingFields{constants.IndexByVName: pod.PodRef.Name})
		if err != nil {
			return nil, err
		}

		// skip the metric if the pod couldn't be found in the virtual cluster
		if len(podList.Items) == 0 {
			continue
		}

		vPod := podList.Items[0]
		pod.PodRef.Name = vPod.Name
		pod.PodRef.Namespace = vPod.Namespace
		pod.PodRef.UID = string(vPod.UID)
		newPods = append(newPods, pod)
	}
	stats.Pods = newPods

	out, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		return nil, err
	}

	return out, nil
}

func executeRequest(req *http.Request, h http.Handler) (int, http.Header, []byte, error) {
	clonedRequest := req.Clone(req.Context())
	fakeWriter := httptest.NewRecorder()
	h.ServeHTTP(fakeWriter, clonedRequest)

	// Check that the server actually sent compressed data
	var responseBytes []byte
	switch fakeWriter.Header().Get("Content-Encoding") {
	case "gzip":
		reader, err := gzip.NewReader(fakeWriter.Body)
		if err != nil {
			return 0, nil, nil, err
		}

		responseBytes, err = ioutil.ReadAll(reader)
		if err != nil {
			return 0, nil, nil, err
		}

		fakeWriter.Header().Del("Content-Encoding")
	default:
		responseBytes = fakeWriter.Body.Bytes()
	}

	return fakeWriter.Code, fakeWriter.Header(), responseBytes, nil
}

func isNodesProxy(r *request.RequestInfo) bool {
	if r.IsResourceRequest == false {
		return false
	}

	return r.APIGroup == corev1.SchemeGroupVersion.Group &&
		r.APIVersion == corev1.SchemeGroupVersion.Version &&
		r.Resource == "nodes" &&
		r.Subresource == "proxy"
}

func IsKubeletStats(path string) bool {
	return strings.HasSuffix(path, "/stats/summary")
}

func IsKubeletMetrics(path string) bool {
	return strings.HasSuffix(path, "/metrics/cadvisor") || strings.HasSuffix(path, "/metrics/probes") || strings.HasSuffix(path, "/metrics/resource") || strings.HasSuffix(path, "/metrics/resource/v1alpha1")
}
