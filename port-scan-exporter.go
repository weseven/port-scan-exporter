package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// TODO: move configs to a yaml file, add config to include hostNetwork pods?

// Highest port number to scan.
const maxPort = 65535

// Workers scanning ports on each pod.
const portScanWorkers = 100

// Timeout after which consider a port to be closed.
const portScanTimeout = 100 * time.Millisecond

// Global variable to track the exporter's health status.
var healthy bool = true

// Get a list of pods on the cluster.
func getPodList() (*v1.PodList, error) {
	clientConfig, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		clientConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			log.Println("Error building kubeconfig:", err)
			healthy = false
			return nil, err
		}
	}

	clientset, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		log.Println("Error creating Kubernetes client:", err)
		healthy = false
		return nil, err
	}

	podList, err := clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		log.Println("Error getting pod list:", err)
		healthy = false
		return nil, err
	}
	return podList, nil
}

type portScanCollector struct {
	podsPortScanned *prometheus.Desc
}

func newPortScanCollector() *portScanCollector {
	return &portScanCollector{
		podsPortScanned: prometheus.NewDesc(
			"pods_port_scanned",
			"The number of pods scanned for open ports",
			nil, nil,
		),
	}
}

func (c *portScanCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.podsPortScanned
}

func (c *portScanCollector) Collect(ch chan<- prometheus.Metric) {
	var wg sync.WaitGroup
	openPorts := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pods_open_ports",
		Help: "Metric has value 1 if the port specified in the port label is open for the pod.",
	}, []string{"namespace", "pod", "port"})

	podList, err := getPodList()
	if err != nil {
		log.Println("Error getting pod list:", err)
		healthy = false
		return
	}

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
	ch <- prometheus.MustNewConstMetric(c.podsPortScanned, prometheus.CounterValue, float64(len(podList.Items)))
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

// Results of a port scan
type scanResult struct {
	port   int     // Port number
	isOpen float64 // 1 if the port is open.
}

func scanPorts(targetIP string, fromPort, toPort int, results chan<- scanResult) {
	for port := fromPort; port < toPort; port++ {
		target := fmt.Sprintf("%s:%d", targetIP, port)
		conn, err := net.DialTimeout("tcp", target, portScanTimeout)
		if err == nil {
			conn.Close()
			results <- scanResult{port: port, isOpen: 1}
			log.Printf("%s:%d is open\n", targetIP, port)
		}
	}
}

// TODO: non-naive implementation of health checks
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
	log.Println("Starting port-scan-exporter...")

	portScanCollector := newPortScanCollector()
	prometheus.MustRegister(portScanCollector)

	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/health", healthHandler)

	// Run the exporter on a separate goroutine to keep it running in the background.
	go func() {
		err := http.ListenAndServe(":8080", nil)
		if err != nil {
			log.Fatalf("Error starting the port-scan-exporter: %v", err)
			healthy = false
		}
	}()
	log.Println("port-scan-exporter is listening on :8080")

	// Graceful shutdown when receiving SIGTERM signal.
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	log.Println("Received termination signal. Shutting down...")
	// Perform any cleanup or resource release tasks here.

	log.Println("port-scan-exporter has terminated.")
	os.Exit(0)
}
