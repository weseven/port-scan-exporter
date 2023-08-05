package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Highest port number to scan.
const maxPort = 65535

// Workers scanning ports on each pod.
const portScanWorkers = 100

// Timeout after which consider a port to be closed.
const portScanTimeout = 100 * time.Millisecond

// Global variable to track the exporter's health status.
var healthy bool = true

type customCollector struct {
	podsScanned *prometheus.Desc
}

func newCustomCollector() *customCollector {
	return &customCollector{
		podsScanned: prometheus.NewDesc(
			"pods_scanned",
			"The number of pods scanned for open ports",
			nil, nil,
		),
	}
}

func (c *customCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.podsScanned
}

func (c *customCollector) Collect(ch chan<- prometheus.Metric) {
	clientConfig, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		clientConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			fmt.Println("Error building kubeconfig:", err)
			healthy = false
			return
		}
	}

	clientset, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		fmt.Println("Error creating Kubernetes client:", err)
		healthy = false
		return
	}

	podList, err := clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		fmt.Println("Error getting pod list:", err)
		healthy = false
		return
	}

	var wg sync.WaitGroup
	openPorts := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pods_open_ports",
		Help: "Metric has value 1 if the port specified in the port label is open for the pod.",
	}, []string{"namespace", "pod", "port"})

	for _, pod := range podList.Items {
		//exclude pods using Host network
		if !pod.Spec.HostNetwork {
			wg.Add(1)
			go func(podName, namespace, podIP string) {
				defer wg.Done()
				collectOpenPorts(openPorts, namespace, podName, podIP)
			}(pod.Name, pod.Namespace, pod.Status.PodIP)
		}
	}

	wg.Wait()

	openPorts.Collect(ch)
	ch <- prometheus.MustNewConstMetric(c.podsScanned, prometheus.CounterValue, float64(len(podList.Items)))
}

func collectOpenPorts(openPorts *prometheus.GaugeVec, namespace, podName, podIP string) {
	if podIP == "" {
		return
	}

	var wg sync.WaitGroup
	results := make(chan scanResult)

	wg.Add(portScanWorkers)
	portsPerWorker := maxPort / portScanWorkers

	for i := 0; i < portScanWorkers; i++ {
		fromPort := i * portsPerWorker
		toPort := (i + 1) * portsPerWorker
		if i == portScanWorkers-1 {
			toPort = maxPort
		}

		go func() {
			defer wg.Done()
			scanPorts(podIP, fromPort, toPort, results)
		}()
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	for res := range results {
		openPorts.WithLabelValues(namespace, podName, fmt.Sprintf("%d", res.port)).Set(res.isOpen)
	}
}

type scanResult struct {
	port   int
	isOpen float64
}

func scanPorts(targetIP string, fromPort, toPort int, results chan<- scanResult) {
	for port := fromPort; port < toPort; port++ {
		target := fmt.Sprintf("%s:%d", targetIP, port)
		conn, err := net.DialTimeout("tcp", target, portScanTimeout)
		if err == nil {
			conn.Close()
			results <- scanResult{port: port, isOpen: 1}
			fmt.Printf("%s:%d is open\n", targetIP, port)
		}
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	if healthy {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("port-scan-exporter is healthy.\n"))
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("port-scan-exporter is not healthy.\n"))
	}
}

func main() {
	customCollector := newCustomCollector()
	prometheus.MustRegister(customCollector)

	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/health", healthHandler)
	fmt.Println("port-scan-exporter starting on port 8080")
	http.ListenAndServe(":8080", nil)
}
