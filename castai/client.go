//go:generate mockgen -source $GOFILE -destination ./mock/client.go . Client

package castai

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/go-resty/resty/v2"
	json "github.com/json-iterator/go"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/castai/kvisor/config"
)

const (
	headerAPIKey          = "X-API-Key" //nolint:gosec
	headerUserAgent       = "User-Agent"
	headerContentType     = "Content-Type"
	headerContentEncoding = "Content-Encoding"
	totalSendDeltaTimeout = 2 * time.Minute

	ReportTypeImagesResourcesChange = "images-resources-change"
	ReportTypeDelta                 = "delta"
	ReportTypeCis                   = "cis-report"
	ReportTypeLinter                = "linter-checks"
	ReportTypeImageMeta             = "image-metadata"
	ReportTypeCloudScan             = "cloud-scan"
)

type Client interface {
	SendLogs(ctx context.Context, req *LogEvent) error
	UpdateImageStatus(ctx context.Context, report *UpdateImagesStatusRequest) error
	SendCISReport(ctx context.Context, report *KubeBenchReport) error
	SendDeltaReport(ctx context.Context, report *Delta) error
	SendLinterChecks(ctx context.Context, checks []LinterCheck) error
	SendImageMetadata(ctx context.Context, meta *ImageMetadata) error
	SendCISCloudScanReport(ctx context.Context, report *CloudScanReport) error
	PostTelemetry(ctx context.Context, initial bool) (*TelemetryResponse, error)
	GetSyncState(ctx context.Context, filter *SyncStateFilter) (*SyncStateResponse, error)
}

func NewClient(
	apiURL, apiKey string,
	log *logrus.Logger,
	clusterID string,
	policyEnforcement bool,
	binName string,
	binVersion config.SecurityAgentVersion,
) Client {
	httpClient := newDefaultDeltaHTTPClient()
	restClient := resty.NewWithClient(httpClient)
	restClient.SetBaseURL(apiURL)
	restClient.Header.Set(headerAPIKey, apiKey)
	restClient.Header.Set(headerUserAgent, binName+"/"+binVersion.Version)
	if log.Level == logrus.TraceLevel {
		restClient.SetDebug(true)
	}

	return &client{
		apiURL:            apiURL,
		apiKey:            apiKey,
		log:               log,
		restClient:        restClient,
		httpClient:        httpClient,
		clusterID:         clusterID,
		policyEnforcement: policyEnforcement,
		binVersion:        binVersion,
	}
}

func createHTTPTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout: 5 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

func newDefaultDeltaHTTPClient() *http.Client {
	return &http.Client{
		Timeout:   2 * time.Minute,
		Transport: createHTTPTransport(),
	}
}

type client struct {
	apiURL            string
	apiKey            string
	log               *logrus.Logger
	restClient        *resty.Client
	httpClient        *http.Client
	clusterID         string
	policyEnforcement bool
	binVersion        config.SecurityAgentVersion
}

func (c *client) PostTelemetry(ctx context.Context, initial bool) (*TelemetryResponse, error) {
	req := c.restClient.R().SetContext(ctx)

	type telemetryRequest struct {
		InitialSync       bool `json:"initialSync,omitempty"`
		PolicyEnforcement bool `json:"policyEnforcement,omitempty"`
	}
	body := telemetryRequest{PolicyEnforcement: c.policyEnforcement}
	if initial {
		body.InitialSync = initial
	}
	req.SetBody(body)

	resp, err := req.Post(fmt.Sprintf("/v1/security/insights/%s/telemetry", c.clusterID))
	if err != nil {
		return nil, fmt.Errorf("sending telemetry: %w", err)
	}
	if resp.IsError() {
		return nil, fmt.Errorf("sending telemetry: request error status_code=%d body=%s", resp.StatusCode(), resp.Body())
	}

	var response TelemetryResponse
	if err := json.Unmarshal(resp.Body(), &response); err != nil {
		return nil, fmt.Errorf("unmarshal telemetry response: %w", err)
	}

	return &response, nil
}

func (c *client) SendLogs(ctx context.Context, req *LogEvent) error {
	resp, err := c.restClient.R().
		SetBody(req).
		SetContext(ctx).
		Post(fmt.Sprintf("/v1/security/insights/%s/log", c.clusterID))

	if err != nil {
		return fmt.Errorf("sending logs: %w", err)
	}
	if resp.IsError() {
		return fmt.Errorf("sending logs: request error status_code=%d body=%s", resp.StatusCode(), resp.Body())
	}

	return nil
}

func (c *client) UpdateImageStatus(ctx context.Context, report *UpdateImagesStatusRequest) error {
	return c.sendReport(ctx, report, ReportTypeImagesResourcesChange)
}

func (c *client) SendDeltaReport(ctx context.Context, report *Delta) error {
	return c.sendReport(ctx, report, ReportTypeDelta)
}

func (c *client) SendCISReport(ctx context.Context, report *KubeBenchReport) error {
	return c.sendReport(ctx, report, ReportTypeCis)
}

func (c *client) SendLinterChecks(ctx context.Context, checks []LinterCheck) error {
	return c.sendReport(ctx, checks, ReportTypeLinter)
}

func (c *client) SendImageMetadata(ctx context.Context, meta *ImageMetadata) error {
	return c.sendReport(ctx, meta, ReportTypeImageMeta)
}

func (c *client) SendCISCloudScanReport(ctx context.Context, report *CloudScanReport) error {
	return c.sendReport(ctx, report, ReportTypeCloudScan)
}

func (c *client) sendReport(ctx context.Context, report any, reportType string) error {
	uri, err := url.Parse(fmt.Sprintf("%s/v1/security/insights/agent/%s/%s", c.apiURL, c.clusterID, reportType))
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}

	pipeReader, pipeWriter := io.Pipe()

	go func() {
		defer func() {
			if err := pipeWriter.Close(); err != nil {
				c.log.Errorf("closing gzip pipe: %v", err)
			}
		}()

		gzipWriter := gzip.NewWriter(pipeWriter)
		defer func() {
			if err := gzipWriter.Close(); err != nil {
				c.log.Errorf("closing gzip writer: %v", err)
			}
		}()

		if err := json.NewEncoder(gzipWriter).Encode(report); err != nil {
			c.log.Errorf("compressing json: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, totalSendDeltaTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uri.String(), pipeReader)
	if err != nil {
		return fmt.Errorf("creating request for report type %s: %w", reportType, err)
	}

	req.Header.Set(headerContentType, "application/json")
	req.Header.Set(headerContentEncoding, "gzip")
	req.Header.Set(headerAPIKey, c.apiKey)
	req.Header.Set(headerUserAgent, "castai-kvisor/"+c.binVersion.Version)

	var resp *http.Response

	backoff := wait.Backoff{
		Duration: 10 * time.Millisecond,
		Factor:   1.5,
		Jitter:   0.2,
		Steps:    3,
	}
	err = wait.ExponentialBackoffWithContext(ctx, backoff, func(context.Context) (done bool, err error) {
		resp, err = c.httpClient.Do(req) //nolint:bodyclose
		if err != nil {
			c.log.Warnf("failed sending request for report %s: %v", reportType, err)
			return false, fmt.Errorf("sending request %s: %w", reportType, err)
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			c.log.Errorf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode > 399 {
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(resp.Body); err != nil {
			c.log.Errorf("failed reading error response body: %v", err)
		}
		return fmt.Errorf("%s request error status_code=%d body=%s url=%s", reportType, resp.StatusCode, buf.String(), uri.String())
	}

	return nil
}

func (c *client) GetSyncState(ctx context.Context, filter *SyncStateFilter) (*SyncStateResponse, error) {
	req := c.restClient.R().SetContext(ctx)
	req.SetBody(filter)
	resp, err := req.Post(fmt.Sprintf("/v1/security/insights/%s/sync-state", c.clusterID))
	if err != nil {
		return nil, fmt.Errorf("calling sync state: %w", err)
	}
	if resp.IsError() {
		return nil, fmt.Errorf("calling sync state: request error status_code=%d body=%s", resp.StatusCode(), resp.Body())
	}
	var response SyncStateResponse
	if err := json.Unmarshal(resp.Body(), &response); err != nil {
		return nil, fmt.Errorf("unmarshal sync state: %w", err)
	}
	return &response, nil
}
