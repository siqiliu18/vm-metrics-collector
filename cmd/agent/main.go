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
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	metricsv1beta1 "k8s.io/metrics/pkg/client/clientset/versioned"
)

func main() {
	// 1. Read env vars
	clusterName  := os.Getenv("CLUSTER_NAME")            // World 2: "sql107" — used as vm_id label
	contextsEnv  := os.Getenv("KUBE_CONTEXTS")           // World 1: "rancher-desktop,sql107" or ""
	intervalStr  := os.Getenv("SCRAPE_INTERVAL_SECONDS") // "15"
	timeoutStr   := os.Getenv("SCRAPE_TIMEOUT_SECONDS")  // "10"
	kafkaBrokers := os.Getenv("KAFKA_BROKERS")           // "kafka:9092" or "<LB-IP>:9092"
	kafkaTopic   := os.Getenv("KAFKA_TOPIC")             // "vm-metrics-ts"

	// 2. Parse scrape interval (default 15s) and timeout (default 10s)
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

	// 3. Create Kafka producer once — reused across all scrapes
	producer, err := kafka.NewProducer(kafkaBrokers)
	if err != nil {
		log.Fatalf("failed to create kafka producer: %v", err)
	}
	defer producer.Close()

	// 4. Detect World 1 vs World 2 based on KUBECONFIG env var.
	//
	// World 2 (in-cluster): KUBECONFIG is not set — the pod's ServiceAccount token
	//   is automatically mounted at /var/run/secrets/kubernetes.io/serviceaccount/
	//   rest.InClusterConfig() reads it. One rest.Config, one cluster, CLUSTER_NAME
	//   is used as the vm_id tag.
	//
	// World 1 (kubeconfig): KUBECONFIG is set — load and merge kubeconfig files,
	//   scrape each context in its own goroutine. Context name is used as vm_id tag.
	if os.Getenv("KUBECONFIG") == "" {
		runInCluster(clusterName, interval, timeout, producer, kafkaTopic)
	} else {
		runWithKubeconfig(contextsEnv, interval, timeout, producer, kafkaTopic)
	}
}

// runInCluster is the World 2 path. Uses the pod's ServiceAccount token to
// authenticate to the local cluster API — no kubeconfig file, no VPN needed.
// CLUSTER_NAME env var is used as the vm_id tag in InfluxDB.
func runInCluster(clusterName string, interval, timeout int, producer *kafka.Producer, topic string) {
	if clusterName == "" {
		log.Fatal("CLUSTER_NAME must be set when running in-cluster (KUBECONFIG not set)")
	}

	restConfig, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("failed to build in-cluster config: %v", err)
	}

	log.Printf("starting agent (in-cluster): cluster=%s interval=%ds timeout=%ds", clusterName, interval, timeout)

	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		go scrapeWithConfig(restConfig, clusterName, time.Duration(timeout)*time.Second, producer, topic)
	}
}

// runWithKubeconfig is the World 1 path. Loads kubeconfig files listed in KUBECONFIG
// env var, scrapes each context in its own goroutine. Context name is used as vm_id.
func runWithKubeconfig(contextsEnv string, interval, timeout int, producer *kafka.Producer, topic string) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	apiConfig, err := loadingRules.Load()
	if err != nil {
		log.Fatalf("failed to load kubeconfig: %v", err)
	}

	var kubeContexts []string
	if contextsEnv == "" {
		for name := range apiConfig.Contexts {
			kubeContexts = append(kubeContexts, name)
		}
	} else {
		kubeContexts = strings.Split(contextsEnv, ",")
	}

	log.Printf("starting agent (kubeconfig): contexts=%v interval=%ds timeout=%ds", kubeContexts, interval, timeout)

	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		for _, kubeContext := range kubeContexts {
			go scrapeFromContext(apiConfig, kubeContext, time.Duration(timeout)*time.Second, producer, topic)
		}
	}
}

// scrapeFromContext builds a rest.Config for one kubeconfig context, then scrapes.
func scrapeFromContext(apiConfig *clientcmdapi.Config, kubeContext string, timeout time.Duration, producer *kafka.Producer, topic string) {
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
	scrapeWithConfig(restConfig, kubeContext, timeout, producer, topic)
}

// scrapeWithConfig is the shared scrape logic used by both World 1 and World 2.
// vmID is the label written to InfluxDB — context name in World 1, CLUSTER_NAME in World 2.
func scrapeWithConfig(restConfig *rest.Config, vmID string, timeout time.Duration, producer *kafka.Producer, topic string) {
	metricsClient, err := metricsv1beta1.NewForConfig(restConfig)
	if err != nil {
		log.Printf("[%s] failed to create metrics client: %v", vmID, err)
		return
	}
	k8sClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		log.Printf("[%s] failed to create k8s client: %v", vmID, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	nodeMetricsList, err := metricsClient.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("[%s] failed to list node metrics: %v", vmID, err)
		return
	}

	for _, nm := range nodeMetricsList.Items {
		node, err := k8sClient.CoreV1().Nodes().Get(ctx, nm.Name, metav1.GetOptions{})
		if err != nil {
			log.Printf("[%s] failed to get node %s: %v", vmID, nm.Name, err)
			continue
		}

		allocCPU := node.Status.Allocatable[corev1.ResourceCPU]
		allocMem := node.Status.Allocatable[corev1.ResourceMemory]
		usageCPU := nm.Usage[corev1.ResourceCPU]
		usageMem := nm.Usage[corev1.ResourceMemory]

		cpuPct := float64(usageCPU.MilliValue()) / float64(allocCPU.MilliValue()) * 100
		memPct := float64(usageMem.Value()) / float64(allocMem.Value()) * 100

		log.Printf("[%s] node=%-30s cpu=%5.1f%%  mem=%5.1f%%", vmID, nm.Name, cpuPct, memPct)

		if err := producer.Publish(topic, kafka.NodeMetric{
			VmID:      vmID,
			Hostname:  nm.Name,
			Timestamp: time.Now().UnixNano(),
			CPUPct:    cpuPct,
			MemPct:    memPct,
		}); err != nil {
			log.Printf("[%s] failed to publish metric for node %s: %v", vmID, nm.Name, err)
		}
	}
}
