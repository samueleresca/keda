package scalers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"

	v2beta2 "k8s.io/api/autoscaling/v2beta2"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/metrics/pkg/apis/external_metrics"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kedautil "github.com/kedacore/keda/v2/pkg/util"
)

type seleniumGridScaler struct {
	metricType v2beta2.MetricTargetType
	metadata   *seleniumGridScalerMetadata
	client     *http.Client
}

type seleniumGridScalerMetadata struct {
	url            string
	browserName    string
	targetValue    int64
	browserVersion string
	unsafeSsl      bool
	scalerIndex    int
}

type seleniumResponse struct {
	Data data `json:"data"`
}

type data struct {
	Grid         grid         `json:"grid"`
	SessionsInfo sessionsInfo `json:"sessionsInfo"`
}

type grid struct {
	MaxSession int `json:"maxSession"`
}

type sessionsInfo struct {
	SessionQueueRequests []string          `json:"sessionQueueRequests"`
	Sessions             []seleniumSession `json:"sessions"`
}

type seleniumSession struct {
	ID           string `json:"id"`
	Capabilities string `json:"capabilities"`
	NodeID       string `json:"nodeId"`
}

type capability struct {
	BrowserName    string `json:"browserName"`
	BrowserVersion string `json:"browserVersion"`
}

const (
	DefaultBrowserVersion string = "latest"
)

var seleniumGridLog = logf.Log.WithName("selenium_grid_scaler")

func NewSeleniumGridScaler(config *ScalerConfig) (Scaler, error) {
	metricType, err := GetMetricTargetType(config)
	if err != nil {
		return nil, fmt.Errorf("error getting scaler metric type: %s", err)
	}

	meta, err := parseSeleniumGridScalerMetadata(config)

	if err != nil {
		return nil, fmt.Errorf("error parsing selenium grid metadata: %s", err)
	}

	httpClient := kedautil.CreateHTTPClient(config.GlobalHTTPTimeout, meta.unsafeSsl)

	return &seleniumGridScaler{
		metricType: metricType,
		metadata:   meta,
		client:     httpClient,
	}, nil
}

func parseSeleniumGridScalerMetadata(config *ScalerConfig) (*seleniumGridScalerMetadata, error) {
	meta := seleniumGridScalerMetadata{
		targetValue: 1,
	}

	if val, ok := config.TriggerMetadata["url"]; ok {
		meta.url = val
	} else {
		return nil, fmt.Errorf("no selenium grid url given in metadata")
	}

	if val, ok := config.TriggerMetadata["browserName"]; ok {
		meta.browserName = val
	} else {
		return nil, fmt.Errorf("no browser name given in metadata")
	}

	if val, ok := config.TriggerMetadata["browserVersion"]; ok && val != "" {
		meta.browserVersion = val
	} else {
		meta.browserVersion = DefaultBrowserVersion
	}

	if val, ok := config.TriggerMetadata["unsafeSsl"]; ok {
		parsedVal, err := strconv.ParseBool(val)
		if err != nil {
			return nil, fmt.Errorf("error parsing unsafeSsl: %s", err)
		}
		meta.unsafeSsl = parsedVal
	}

	meta.scalerIndex = config.ScalerIndex
	return &meta, nil
}

// No cleanup required for selenium grid scaler
func (s *seleniumGridScaler) Close(context.Context) error {
	return nil
}

func (s *seleniumGridScaler) GetMetrics(ctx context.Context, metricName string, metricSelector labels.Selector) ([]external_metrics.ExternalMetricValue, error) {
	v, err := s.getSessionsCount(ctx)
	if err != nil {
		return []external_metrics.ExternalMetricValue{}, fmt.Errorf("error requesting selenium grid endpoint: %s", err)
	}

	metric := external_metrics.ExternalMetricValue{
		MetricName: metricName,
		Value:      *resource.NewQuantity(v, resource.DecimalSI),
		Timestamp:  metav1.Now(),
	}

	return append([]external_metrics.ExternalMetricValue{}, metric), nil
}

func (s *seleniumGridScaler) GetMetricSpecForScaling(context.Context) []v2beta2.MetricSpec {
	metricName := kedautil.NormalizeString(fmt.Sprintf("seleniumgrid-%s", s.metadata.browserName))
	externalMetric := &v2beta2.ExternalMetricSource{
		Metric: v2beta2.MetricIdentifier{
			Name: GenerateMetricNameWithIndex(s.metadata.scalerIndex, metricName),
		},
		Target: GetMetricTarget(s.metricType, s.metadata.targetValue),
	}
	metricSpec := v2beta2.MetricSpec{
		External: externalMetric, Type: externalMetricType,
	}
	return []v2beta2.MetricSpec{metricSpec}
}

func (s *seleniumGridScaler) IsActive(ctx context.Context) (bool, error) {
	v, err := s.getSessionsCount(ctx)
	if err != nil {
		return false, err
	}

	return v > 0, nil
}

func (s *seleniumGridScaler) getSessionsCount(ctx context.Context) (int64, error) {
	body, err := json.Marshal(map[string]string{
		"query": "{ grid { maxSession }, sessionsInfo { sessionQueueRequests, sessions { id, capabilities, nodeId } } }",
	})

	if err != nil {
		return -1, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", s.metadata.url, bytes.NewBuffer(body))
	if err != nil {
		return -1, err
	}

	res, err := s.client.Do(req)
	if err != nil {
		return -1, err
	}

	if res.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("selenium grid returned %d", res.StatusCode)
		return -1, errors.New(msg)
	}

	defer res.Body.Close()
	b, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return -1, err
	}
	v, err := getCountFromSeleniumResponse(b, s.metadata.browserName, s.metadata.browserVersion)
	if err != nil {
		return -1, err
	}
	return v, nil
}

func getCountFromSeleniumResponse(b []byte, browserName string, browserVersion string) (int64, error) {
	var count int64
	var seleniumResponse = seleniumResponse{}

	if err := json.Unmarshal(b, &seleniumResponse); err != nil {
		return 0, err
	}

	var sessionQueueRequests = seleniumResponse.Data.SessionsInfo.SessionQueueRequests
	for _, sessionQueueRequest := range sessionQueueRequests {
		var capability = capability{}
		if err := json.Unmarshal([]byte(sessionQueueRequest), &capability); err == nil {
			if capability.BrowserName == browserName {
				if strings.HasPrefix(capability.BrowserVersion, browserVersion) {
					count++
				} else if capability.BrowserVersion == "" && browserVersion == DefaultBrowserVersion {
					count++
				}
			}
		} else {
			seleniumGridLog.Error(err, fmt.Sprintf("Error when unmarshaling session queue requests: %s", err))
		}
	}

	var sessions = seleniumResponse.Data.SessionsInfo.Sessions
	for _, session := range sessions {
		var capability = capability{}
		if err := json.Unmarshal([]byte(session.Capabilities), &capability); err == nil {
			if capability.BrowserName == browserName {
				if strings.HasPrefix(capability.BrowserVersion, browserVersion) {
					count++
				} else if browserVersion == DefaultBrowserVersion {
					count++
				}
			}
		} else {
			seleniumGridLog.Error(err, fmt.Sprintf("Error when unmarshaling sessions info: %s", err))
		}
	}

	var gridMaxSession = int64(seleniumResponse.Data.Grid.MaxSession)

	if gridMaxSession > 0 {
		count = (count + gridMaxSession - 1) / gridMaxSession
	}

	return count, nil
}
