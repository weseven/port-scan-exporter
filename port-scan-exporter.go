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

	"gopkg.in/yaml.v3"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Config struct to hold the configuration parameters.
type Config struct {
	ListenAddress     string `yaml:"listen_address"`
	PortScanWorkers   int    `yaml:"portscan_workers"`
	PortScanTimeoutMs int    `yaml:"portscan_timeout_ms"`
	MaxPort           int    `yaml:"max_port"`
}

// LoadConfig loads the configuration from the YAML file.
func loadConfig(filename string) (*Config, error) {
	configFile, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	config := &Config{}
	if err := yaml.Unmarshal(configFile, config); err != nil {
		return nil, err
	}

	return config, nil
}

// Highest port number to scan.
var maxPort int

// Workers scanning ports on each pod.
var portScanWorkers int

// Timeout after which consider a port to be closed.
var portScanTimeout time.Duration

// Listen address.
var listenAddress string

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
	podsPortScanned  *prometheus.Desc
	portScanDuration *prometheus.Desc
}

func newPortScanCollector() *portScanCollector {
	return &portScanCollector{
		podsPortScanned: prometheus.NewDesc(
			"portscan_pods_scanned",
			"The number of pods scanned for open ports",
			nil, nil,
		),
		portScanDuration: prometheus.NewDesc(
			"portscan_scan_duration",
			"The duration of the port scan in milliseconds",
			nil, nil,
		),
	}
}

func (c *portScanCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.podsPortScanned
	ch <- c.portScanDuration
}

func (c *portScanCollector) Collect(ch chan<- prometheus.Metric) {
	var wg sync.WaitGroup
	openPorts := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "portscan_open_ports",
		Help: "Metric has value 1 if the port specified in the port label is open for the pod.",
	}, []string{"portscan_namespace", "portscan_pod", "portscan_port"})

	podList, err := getPodList()
	if err != nil {
		log.Println("Error getting pod list:", err)
		healthy = false
		return
	}

	log.Printf("Starting port scan on %d pods...", len(podList.Items))
	start := time.Now()
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
	scanDuration := time.Since(start).Seconds()

	openPorts.Collect(ch)
	ch <- prometheus.MustNewConstMetric(c.podsPortScanned, prometheus.CounterValue, float64(len(podList.Items)))
	ch <- prometheus.MustNewConstMetric(c.portScanDuration, prometheus.CounterValue, scanDuration)
	log.Printf("Finished port scan on %d pods in %fs.", len(podList.Items), scanDuration)
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

// Scan pod for open TCP ports
func scanPorts(targetIP string, fromPort, toPort int, results chan<- scanResult) {
	for port := fromPort; port < toPort; port++ {
		target := fmt.Sprintf("%s:%d", targetIP, port)
		conn, err := net.DialTimeout("tcp", target, portScanTimeout)
		if err == nil {
			conn.Close()
			results <- scanResult{port: port, isOpen: 1}
			log.Printf("%s:%d/TCP is open\n", targetIP, port)
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
	// Load configuration from the YAML file.
	config, err := loadConfig("config.yaml")
	if err != nil {
		log.Fatalf("Error loading config: %v", err)
	}
	portScanTimeout = time.Duration(config.PortScanTimeoutMs) * time.Millisecond
	portScanWorkers = config.PortScanWorkers
	listenAddress = config.ListenAddress
	maxPort = config.MaxPort
	log.Println("Loaded configuration:")
	log.Printf("Listen address: %s\n", listenAddress)
	log.Printf("Port scan workers: %d\n", portScanWorkers)
	log.Printf("Port scan timeout: %s\n", portScanTimeout)
	log.Printf("Max port: %d\n", maxPort)

	portScanCollector := newPortScanCollector()
	prometheus.MustRegister(portScanCollector)

	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/health", healthHandler)

	// Run the exporter on a separate goroutine to keep it running in the background.
	go func() {
		err := http.ListenAndServe(listenAddress, nil)
		if err != nil {
			log.Fatalf("Error starting the port-scan-exporter: %v", err)
			healthy = false
		}
	}()
	log.Printf("port-scan-exporter is listening on %s\n", listenAddress)

	// Graceful shutdown when receiving SIGTERM signal.
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	log.Println("Received termination signal. Shutting down...")
	// Perform any cleanup or resource release tasks here.

	log.Println("port-scan-exporter has terminated.")
	os.Exit(0)
}
