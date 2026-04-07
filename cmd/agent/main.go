package main

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/siqiliu/vm-metrics-collector/internal/kafka"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	metricsv1beta1 "k8s.io/metrics/pkg/client/clientset/versioned"
)

func main() {
	// 1. Read env vars
	contextsEnv := os.Getenv("KUBE_CONTEXTS")           // "rancher-desktop" or "ctx-a,ctx-b" or ""
	intervalStr := os.Getenv("SCRAPE_INTERVAL_SECONDS") // "15"
	timeoutStr  := os.Getenv("SCRAPE_TIMEOUT_SECONDS")  // "10"
	kafkaBrokers := os.Getenv("KAFKA_BROKERS")          // "kafka:9092"
	kafkaTopic   := os.Getenv("KAFKA_TOPIC")            // "vm-metrics-ts"

	// 2. Load and merge all kubeconfig files listed in KUBECONFIG env var.
	// client-go reads KUBECONFIG automatically (supports colon-separated paths).
	// e.g. KUBECONFIG=/root/.kube/config:/root/creds/cluster-a.yaml
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	apiConfig, err := loadingRules.Load()
	if err != nil {
		log.Fatalf("failed to load kubeconfig: %v", err)
	}

	// 3. Determine which contexts to scrape
	var kubeContexts []string
	if contextsEnv == "" {
		// scrape all contexts found across all loaded kubeconfig files
		for name := range apiConfig.Contexts {
			kubeContexts = append(kubeContexts, name)
		}
	} else {
		kubeContexts = strings.Split(contextsEnv, ",")
	}

	// 4. Parse scrape interval (default 15s) and timeout (default 10s)
	interval := 15
	if intervalStr != "" {
		if v, err := strconv.Atoi(intervalStr); err == nil {
			interval = v
		}
	}
	timeout := 10
	if timeoutStr != "" {
		if v, err := strconv.Atoi(timeoutStr); err == nil {
			timeout = v
		}
	}

	// 5. Create Kafka producer once — reused across all scrapes
	producer, err := kafka.NewProducer(kafkaBrokers)
	if err != nil {
		log.Fatalf("failed to create kafka producer: %v", err)
	}
	defer producer.Close()

	log.Printf("starting agent: contexts=%v interval=%ds timeout=%ds", kubeContexts, interval, timeout)

	// 6. Scrape loop — each context runs in its own goroutine so a slow/unreachable
	// cluster does not block others from being scraped on time.
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		for _, kubeContext := range kubeContexts {
			go scrape(apiConfig, kubeContext, time.Duration(timeout)*time.Second, producer, kafkaTopic)
		}
	}
}

// scrape connects to one k8s context and logs node metrics.
// timeout bounds how long a single scrape attempt may take.
func scrape(apiConfig *clientcmdapi.Config, kubeContext string, timeout time.Duration, producer *kafka.Producer, topic string) {
	// Build a rest.Config scoped to this specific context.
	// NewNonInteractiveClientConfig picks the right cluster URL + credentials
	// from the merged apiConfig using the context name as the key.
	restConfig, err := clientcmd.NewNonInteractiveClientConfig(
		*apiConfig,
		kubeContext,
		&clientcmd.ConfigOverrides{},
		nil,
	).ClientConfig()
	if err != nil {
		log.Printf("[%s] failed to build rest config: %v", kubeContext, err)
		return
	}

	// Two clients needed:
	// - metricsClient: calls /apis/metrics.k8s.io/v1beta1/nodes → current usage
	// - k8sClient:     calls /api/v1/nodes → allocatable capacity (to compute percent)
	metricsClient, err := metricsv1beta1.NewForConfig(restConfig)
	if err != nil {
		log.Printf("[%s] failed to create metrics client: %v", kubeContext, err)
		return
	}
	k8sClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		log.Printf("[%s] failed to create k8s client: %v", kubeContext, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Fetch current resource usage per node
	nodeMetricsList, err := metricsClient.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("[%s] failed to list node metrics: %v", kubeContext, err)
		return
	}

	for _, nm := range nodeMetricsList.Items {
		// Fetch node to get allocatable capacity
		node, err := k8sClient.CoreV1().Nodes().Get(ctx, nm.Name, metav1.GetOptions{})
		if err != nil {
			log.Printf("[%s] failed to get node %s: %v", kubeContext, nm.Name, err)
			continue
		}

		allocCPU := node.Status.Allocatable[corev1.ResourceCPU]
		allocMem := node.Status.Allocatable[corev1.ResourceMemory]

		usageCPU := nm.Usage[corev1.ResourceCPU]
		usageMem := nm.Usage[corev1.ResourceMemory]

		// resource.Quantity.MilliValue() → int64 millicores (1 core = 1000m)
		// resource.Quantity.Value()      → int64 bytes
		cpuPct := float64(usageCPU.MilliValue()) / float64(allocCPU.MilliValue()) * 100
		memPct := float64(usageMem.Value()) / float64(allocMem.Value()) * 100

		log.Printf("[%s] node=%-30s cpu=%5.1f%%  mem=%5.1f%%",
			kubeContext, nm.Name, cpuPct, memPct)

		if err := producer.Publish(topic, kafka.NodeMetric{
			VmID:      kubeContext,
			Hostname:  nm.Name,
			Timestamp: time.Now().UnixNano(),
			CPUPct:    cpuPct,
			MemPct:    memPct,
		}); err != nil {
			log.Printf("[%s] failed to publish metric for node %s: %v", kubeContext, nm.Name, err)
		}
	}
}
