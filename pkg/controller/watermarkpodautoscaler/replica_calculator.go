package watermarkpodautoscaler

import (
	"fmt"
	"math"
	"time"

	"github.com/DataDog/watermarkpodautoscaler/pkg/apis/datadoghq/v1alpha1"

	"github.com/prometheus/client_golang/prometheus"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	metricsclient "k8s.io/kubernetes/pkg/controller/podautoscaler/metrics"
)

// ReplicaCalculatorItf interface for ReplicaCalculator
type ReplicaCalculatorItf interface {
	GetExternalMetricReplicas(currentReplicas int32, metric v1alpha1.MetricSpec, wpa *v1alpha1.WatermarkPodAutoscaler) (replicaCount int32, utilization int64, timestamp time.Time, err error)
}

// ReplicaCalculator is responsible for calculation of the number of replicas
// It contains all the needed information
type ReplicaCalculator struct {
	metricsClient metricsclient.MetricsClient
	podsGetter    v1coreclient.PodsGetter
}

// NewReplicaCalculator returns a ReplicaCalculator object reference
func NewReplicaCalculator(metricsClient metricsclient.MetricsClient, podsGetter v1coreclient.PodsGetter) *ReplicaCalculator {
	return &ReplicaCalculator{
		metricsClient: metricsClient,
		podsGetter:    podsGetter,
	}
}

// GetExternalMetricReplicas calculates the desired replica count based on a
// target metric value (as a milli-value) for the external metric in the given
// namespace, and the current replica count.
func (c *ReplicaCalculator) GetExternalMetricReplicas(currentReplicas int32, metric v1alpha1.MetricSpec, wpa *v1alpha1.WatermarkPodAutoscaler) (replicaCount int32, utilization int64, timestamp time.Time, err error) {
	metricName := metric.External.MetricName
	selector := metric.External.MetricSelector
	labelSelector, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return 0, 0, time.Time{}, err
	}
	log.Info(fmt.Sprintf("Using label selector: %v", labelSelector))

	metrics, timestamp, err := c.metricsClient.GetExternalMetric(metricName, wpa.Namespace, labelSelector)
	if err != nil {
		restrictedScaling.Delete(prometheus.Labels{"wpa_name": wpa.Name, "metric_name": metricName})
		value.Delete(prometheus.Labels{"wpa_name": wpa.Name, "metric_name": metricName})
		return 0, 0, time.Time{}, fmt.Errorf("unable to get external metric %s/%s/%+v: %s", wpa.Namespace, metricName, selector, err)
	}
	log.Info(fmt.Sprintf("Metrics from the External Metrics Provider: %v", metrics))
	averaged := 1.0

	if wpa.Spec.Algorithm == "average" {
		averaged = float64(currentReplicas)
	}
	log.Info(fmt.Sprintf("Algorithm is %s", wpa.Spec.Algorithm))

	var sum int64
	for _, val := range metrics {
		sum += val
	}
	adjustedUsage := float64(sum) / averaged
	milliAdjustedUsage := adjustedUsage / 1000
	utilization = int64(adjustedUsage)
	highMark := metric.External.HighWatermark.MilliValue()
	lowMark := metric.External.LowWatermark.MilliValue()

	log.Info(fmt.Sprintf("About to compare utilization %v vs LWM %d and HWM %d", adjustedUsage, lowMark, highMark))

	adjustedHM := float64(highMark) + wpa.Spec.Tolerance*float64(highMark)
	adjustedLM := float64(lowMark) - wpa.Spec.Tolerance*float64(lowMark)

	// We do not use the abs as we want to know if we are higher than the high mark or lower than the low mark
	switch {
	case adjustedUsage > adjustedHM:
		replicaCount = int32(math.Ceil(float64(currentReplicas) * adjustedUsage / (float64(highMark))))
		log.Info(fmt.Sprintf("Value is above highMark. Usage: %f. ReplicaCount %d", milliAdjustedUsage, replicaCount))
	case adjustedUsage < adjustedLM:
		replicaCount = int32(math.Floor(float64(currentReplicas) * adjustedUsage / (float64(lowMark))))
		log.Info(fmt.Sprintf("Value is below lowMark. Usage: %f ReplicaCount %d", milliAdjustedUsage, replicaCount))
	default:
		restrictedScaling.With(prometheus.Labels{"wpa_name": wpa.Name, "metric_name": metricName}).Set(1)
		value.With(prometheus.Labels{"wpa_name": wpa.Name, "metric_name": metricName}).Set(milliAdjustedUsage)
		log.Info(fmt.Sprintf("Within bounds of the watermarks. Value: %v is [%d; %d] Tol: +/- %v%%", adjustedUsage, lowMark, highMark, wpa.Spec.Tolerance))
		return currentReplicas, utilization, timestamp, nil
	}

	restrictedScaling.With(prometheus.Labels{"wpa_name": wpa.Name, "metric_name": metricName}).Set(0)
	value.With(prometheus.Labels{"wpa_name": wpa.Name, "metric_name": metricName}).Set(milliAdjustedUsage)

	return replicaCount, utilization, timestamp, nil
}
