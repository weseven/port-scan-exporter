package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
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

// portScanCollectorConfig struct to hold the configuration parameters.
type portScanCollectorConfig struct {
	// Listen address.
	ListenAddress string `yaml:"listen_address"`
	// Timeout (in ms) after which consider a port to be closed.
	PortScanTimeoutMs int `yaml:"portscan_timeout_ms"`
	// Workers to scan ports.
	PortScanWorkers int `yaml:"portscan_workers"`
	// Max parallel pod scans
	MaxParallelPodScans int `yaml:"max_parallel_pod_scans"`
	// Highest port number to scan.
	MaxPort int `yaml:"max_port"`
	// Minutes after which to rescan a pod ports
	RescanIntervalMinutes int `yaml:"rescan_interval_minutes"`
	// Expire pod info after this many minutes
	ExpireAfterMinutes int `yaml:"expire_after_minutes"`

	// Timeout in time.Duration
	PortScanTimeout time.Duration
	// Rescan interval in time.Duration
	RescanInterval time.Duration
	// Pod info expiration in time.Duration
	ExpireAfter time.Duration
}

// LoadConfig loads the configuration from the YAML file.
func loadConfig(filename string) (*portScanCollectorConfig, error) {
	configFile, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	config := &portScanCollectorConfig{}
	if err := yaml.Unmarshal(configFile, config); err != nil {
		return nil, err
	}

	if config.MaxParallelPodScans <= 0 {
		config.MaxParallelPodScans = 1
	}

	config.PortScanTimeout = time.Duration(config.PortScanTimeoutMs) * time.Millisecond
	config.RescanInterval = time.Duration(config.RescanIntervalMinutes) * time.Minute
	config.ExpireAfter = time.Duration(config.ExpireAfterMinutes) * time.Minute

	return config, nil
}

// Function to print the current config
func printConfig(config *portScanCollectorConfig) {
	log.Println("Current configuration:")
	log.Printf("\tListenAddress: %s\n", config.ListenAddress)
	log.Printf("\tPortScanTimeoutMs: %d\n", config.PortScanTimeoutMs)
	log.Printf("\tPortScanWorkers: %d\n", config.PortScanWorkers)
	log.Printf("\tMaxParallelPodScans: %d\n", config.MaxParallelPodScans)
	log.Printf("\tRescanIntervalMinutes: %d\n", config.RescanIntervalMinutes)
	log.Printf("\tExpireAfterMinutes: %d\n", config.ExpireAfterMinutes)
	log.Printf("\tMaxPort: %d\n", config.MaxPort)
}

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

// PodInfo struct
type PodInfo struct {
	// Name of the pod
	Name string
	// Namespace of the pod
	Namespace string
	// Timestamp of last port scan of the pod
	LastScanTime time.Time
	// Duration (in ms) of last scan
	LastScanDuration time.Duration
	// List of open ports of the pod
	OpenPorts []int
	// True if a scan is in progress
	Scanning bool
}

type portScanCollector struct {
	openPorts           *prometheus.GaugeVec
	podLastScanTime     *prometheus.GaugeVec
	podLastScanDuration *prometheus.GaugeVec

	// portScanCollector config
	config     *portScanCollectorConfig
	podInfoMap *sync.Map
}

func newPortScanCollector(config *portScanCollectorConfig, infoMap *sync.Map) *portScanCollector {
	return &portScanCollector{
		openPorts: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "portscan_open_ports",
				Help: "Metric has value 1 if the port specified in the port label is open for the pod.",
			},
			[]string{"portscan_namespace", "portscan_pod", "portscan_port"},
		),
		podLastScanTime: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "portscan_last_scan_time",
				Help: "Timestamp of the last portscan for the pod",
			},
			[]string{"portscan_namespace", "portscan_pod"},
		),
		podLastScanDuration: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "portscan_last_scan_duration",
				Help: "Duration (in ms) of the last portscan for the pod",
			},
			[]string{"portscan_namespace", "portscan_pod"},
		),
		config:     config,
		podInfoMap: infoMap,
	}
}

func (c *portScanCollector) Describe(ch chan<- *prometheus.Desc) {
	c.openPorts.Describe(ch)
	c.podLastScanTime.Describe(ch)
	c.podLastScanDuration.Describe(ch)
}

func (c *portScanCollector) Collect(ch chan<- prometheus.Metric) {
	// Scan the podInfoMap and gather metrics
	c.podInfoMap.Range(func(key, value interface{}) bool {
		podInfo := value.(*PodInfo)
		// Skip pods that are currently being scanned
		if podInfo.Scanning {
			return true
		}
		for _, port := range podInfo.OpenPorts {
			c.openPorts.WithLabelValues(podInfo.Namespace, podInfo.Name, strconv.Itoa(port)).Set(1)
		}
		c.podLastScanTime.WithLabelValues(podInfo.Namespace, podInfo.Name).Set(float64(podInfo.LastScanTime.Unix()))
		c.podLastScanDuration.WithLabelValues(podInfo.Namespace, podInfo.Name).Set(float64(podInfo.LastScanDuration.Milliseconds()))
		return true
	})

	c.openPorts.Collect(ch)
	c.podLastScanTime.Collect(ch)
	c.podLastScanDuration.Collect(ch)
}

// Scan TCP ports of a pod, return a list of open ports and time.Duration of the scan
func scanPodPorts(targetIP string, config *portScanCollectorConfig) (openPorts []int, scanDuration time.Duration) {
	start := time.Now()
	portsPerWorker := config.MaxPort / config.PortScanWorkers
	var wg sync.WaitGroup
	openPortsChan := make(chan int)

	wg.Add(config.PortScanWorkers)

	for i := 0; i < config.PortScanWorkers; i++ {
		fromPort := i * portsPerWorker
		toPort := (i + 1) * portsPerWorker
		if i == config.PortScanWorkers-1 {
			toPort = config.MaxPort
		}
		go func() {
			defer wg.Done()
			for port := fromPort; port < toPort; port++ {
				conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", targetIP, port), config.PortScanTimeout)
				if err == nil {
					conn.Close()
					openPortsChan <- port
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(openPortsChan)
	}()

	for openPort := range openPortsChan {
		openPorts = append(openPorts, openPort)
	}

	return openPorts, time.Since(start)
}

// TODO: non-naive implementation of health checks
// Global variable to track the exporter's health status.
var healthy bool = true

func healthHandler(w http.ResponseWriter, r *http.Request) {
	if healthy {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("port-scan-exporter is healthy.\n"))
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("port-scan-exporter is not healthy.\n"))
	}
}

// Clean up pods from the cache that have not been scanned for 12 hours
func cleanupExpiredInfo(podInfoCache *sync.Map, expiration time.Duration) {
	podInfoCache.Range(func(key, value interface{}) bool {
		podInfo := value.(*PodInfo)
		if time.Since(podInfo.LastScanTime) > expiration {
			log.Printf("Deleting expired pod %s info from cache (last scan time: %d)", key, podInfo.LastScanTime.Unix())
			podInfoCache.Delete(key)
		}
		return true
	})
}

// function that gets all the pods, starts port scanning each pod and filling the podInfoMap
func refreshPodInfo(podInfoCache *sync.Map, config *portScanCollectorConfig) {
	podList, err := getPodList()
	if err != nil {
		log.Println("Error getting pod list:", err)
		healthy = false
		return
	}

	var wg sync.WaitGroup
	semaphore := make(chan struct{}, config.MaxParallelPodScans)

	for _, pod := range podList.Items {
		//exclude pod using HostNetwork
		if !pod.Spec.HostNetwork {
			podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
			podInfoAny, found := podInfoCache.LoadOrStore(podKey, &PodInfo{LastScanTime: time.Now(), Scanning: false})
			podInfo := podInfoAny.(*PodInfo)
			// if it is a new pod, or the last scan expired and it's not currently being scanned
			if !found || (time.Since(podInfo.LastScanTime) >= config.RescanInterval && !podInfo.Scanning) {

				go func(p v1.Pod, pinfo *PodInfo) {
					wg.Add(1)
					semaphore <- struct{}{}
					defer func() {
						<-semaphore
						wg.Done()
					}()
					log.Printf("Scanning pod %s/%s", p.Namespace, p.Name)
					pinfo.Scanning = true
					pinfo.Namespace = p.Namespace
					pinfo.Name = p.Name

					openPorts, scanDuration := scanPodPorts(p.Status.PodIP, config)

					pinfo.LastScanTime = time.Now()
					pinfo.Scanning = false
					pinfo.OpenPorts = openPorts
					pinfo.LastScanDuration = scanDuration
					log.Printf("Scanned pod %s/%s in %d ms, open ports: %v", p.Namespace, p.Name, scanDuration.Milliseconds(), openPorts)
				}(pod, podInfo)
			}
		}
	}
	wg.Wait()
}

func main() {
	log.Println("Starting port-scan-exporter...")
	// Load configuration from the YAML file.
	config, err := loadConfig("config.yaml")
	if err != nil {
		log.Fatalf("Error loading config: %v", err)
	}
	printConfig(config)

	podInfoMap := new(sync.Map)

	// Background loop to clean up pods from the cache that have not been scanned for 12 hours
	go func() {
		for {
			time.Sleep(60 * time.Minute)
			cleanupExpiredInfo(podInfoMap, config.ExpireAfter)
		}
	}()

	// Background loop to scan pods and fill the podInfoMap
	go func() {
		for {
			refreshPodInfo(podInfoMap, config)
			time.Sleep(5 * time.Minute)
		}
	}()

	portScanCollector := newPortScanCollector(config, podInfoMap)
	prometheus.MustRegister(portScanCollector)

	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/health", healthHandler)

	// Run the exporter on a separate goroutine to keep it running in the background.
	go func() {
		err := http.ListenAndServe(config.ListenAddress, nil)
		if err != nil {
			log.Fatalf("Error starting the port-scan-exporter: %v", err)
			healthy = false
		}
	}()
	log.Printf("port-scan-exporter is listening on %s\n", config.ListenAddress)

	// Graceful shutdown when receiving SIGTERM signal.
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	log.Println("Received termination signal. Shutting down...")
	// Perform any cleanup or resource release tasks here.

	log.Println("port-scan-exporter has terminated.")
	os.Exit(0)
}
