package statsd

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/atlassian/gostatsd"
	"github.com/atlassian/gostatsd/pb"
	"github.com/atlassian/gostatsd/pkg/stats"

	"github.com/ash2k/stager/wait"
	"github.com/cenkalti/backoff"
	"github.com/golang/protobuf/proto"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"github.com/tilinna/clock"
)

const (
	defaultConsolidatorFlushInterval = 1 * time.Second
	defaultClientTimeout             = 10 * time.Second
	defaultCompress                  = true
	defaultEnableHttp2               = false
	defaultApiEndpoint               = ""
	defaultMaxRequestElapsedTime     = 30 * time.Second
	defaultMaxRequests               = 1000
	defaultNetwork                   = "tcp"
)

// HttpForwarderHandlerV2 is a PipelineHandler which sends metrics to another gostatsd instance
type HttpForwarderHandlerV2 struct {
	postId          uint64 // atomic - used for an id in logs
	messagesInvalid uint64 // atomic - messages which failed to be created
	messagesCreated uint64 // atomic - messages which were created
	messagesSent    uint64 // atomic - messages successfully sent
	messagesRetried uint64 // atomic - retries (first send is not a retry, final failure is not a retry)
	messagesDropped uint64 // atomic - final failure

	logger                logrus.FieldLogger
	apiEndpoint           string
	maxRequestElapsedTime time.Duration
	metricsSem            chan struct{}
	client                http.Client
	consolidator          *gostatsd.MetricConsolidator
	consolidatedMetrics   <-chan []*gostatsd.MetricMap
	eventWg               sync.WaitGroup
	compress              bool
}

// NewHttpForwarderHandlerV2FromViper returns a new http API client.
func NewHttpForwarderHandlerV2FromViper(logger logrus.FieldLogger, v *viper.Viper) (*HttpForwarderHandlerV2, error) {
	subViper := getSubViper(v, "http-transport")
	subViper.SetDefault("client-timeout", defaultClientTimeout)
	subViper.SetDefault("compress", defaultCompress)
	subViper.SetDefault("enable-http2", defaultEnableHttp2)
	subViper.SetDefault("api-endpoint", defaultApiEndpoint)
	subViper.SetDefault("max-requests", defaultMaxRequests)
	subViper.SetDefault("max-request-elapsed-time", defaultMaxRequestElapsedTime)
	subViper.SetDefault("consolidator-slots", v.GetInt(ParamMaxParsers))
	subViper.SetDefault("flush-interval", defaultConsolidatorFlushInterval)
	subViper.SetDefault("network", defaultNetwork)

	return NewHttpForwarderHandlerV2(
		logger,
		subViper.GetString("api-endpoint"),
		subViper.GetString("network"),
		subViper.GetInt("consolidator-slots"),
		subViper.GetInt("max-requests"),
		subViper.GetBool("compress"),
		subViper.GetBool("enable-http2"),
		subViper.GetDuration("client-timeout"),
		subViper.GetDuration("max-request-elapsed-time"),
		subViper.GetDuration("flush-interval"),
	)
}

// NewHttpForwarderHandlerV2 returns a new handler which dispatches metrics over http to another gostatsd server.
func NewHttpForwarderHandlerV2(logger logrus.FieldLogger, apiEndpoint, network string, consolidatorSlots, maxRequests int, compress, enableHttp2 bool, clientTimeout, maxRequestElapsedTime time.Duration, flushInterval time.Duration) (*HttpForwarderHandlerV2, error) {
	if apiEndpoint == "" {
		return nil, fmt.Errorf("api-endpoint is required")
	}
	if consolidatorSlots <= 0 {
		return nil, fmt.Errorf("consolidator-slots must be positive")
	}
	if maxRequests <= 0 {
		return nil, fmt.Errorf("max-requests must be positive")
	}
	if clientTimeout <= 0 {
		return nil, fmt.Errorf("client-timeout must be positive")
	}
	if maxRequestElapsedTime <= 0 {
		return nil, fmt.Errorf("max-request-elapsed-time must be positive")
	}
	if flushInterval <= 0 {
		return nil, fmt.Errorf("flush-interval must be positive")
	}

	logger.WithFields(logrus.Fields{
		"api-endpoint":             apiEndpoint,
		"client-timeout":           clientTimeout,
		"compress":                 compress,
		"enable-http2":             enableHttp2,
		"max-request-elapsed-time": maxRequestElapsedTime,
		"max-requests":             maxRequests,
		"consolidator-slots":       consolidatorSlots,
		"network":                  network,
		"flush-interval":           flushInterval,
	}).Info("created HttpForwarderHandler")

	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSHandshakeTimeout: 3 * time.Second,
		TLSClientConfig: &tls.Config{
			// Can't use SSLv3 because of POODLE and BEAST
			// Can't use TLSv1.0 because of POODLE and BEAST using CBC cipher
			// Can't use TLSv1.1 because of RC4 cipher usage
			MinVersion: tls.VersionTLS12,
		},
		DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
			// replace the network with our own
			return dialer.DialContext(ctx, network, address)
		},
		MaxIdleConns:    50,
		IdleConnTimeout: 1 * time.Minute,
	}
	if !enableHttp2 {
		// A non-nil empty map used in TLSNextProto to disable HTTP/2 support in client.
		// https://golang.org/doc/go1.6#http2
		transport.TLSNextProto = map[string](func(string, *tls.Conn) http.RoundTripper){}
	}

	metricsSem := make(chan struct{}, maxRequests)
	for i := 0; i < maxRequests; i++ {
		metricsSem <- struct{}{}
	}

	ch := make(chan []*gostatsd.MetricMap)

	return &HttpForwarderHandlerV2{
		logger:                logger.WithField("component", "http-forwarder-handler-v2"),
		apiEndpoint:           apiEndpoint,
		maxRequestElapsedTime: maxRequestElapsedTime,
		metricsSem:            metricsSem,
		compress:              compress,
		consolidator:          gostatsd.NewMetricConsolidator(consolidatorSlots, flushInterval, ch),
		consolidatedMetrics:   ch,
		client: http.Client{
			Transport: transport,
			Timeout:   clientTimeout,
		},
	}, nil
}

func (hfh *HttpForwarderHandlerV2) EstimatedTags() int {
	return 0
}

func (hfh *HttpForwarderHandlerV2) DispatchMetrics(ctx context.Context, metrics []*gostatsd.Metric) {
	hfh.consolidator.ReceiveMetrics(metrics)
}

// DispatchMetricMap re-dispatches a metric map through HttpForwarderHandlerV2.DispatchMetrics
func (hfh *HttpForwarderHandlerV2) DispatchMetricMap(ctx context.Context, mm *gostatsd.MetricMap) {
	hfh.consolidator.ReceiveMetricMap(mm)
}

func (hfh *HttpForwarderHandlerV2) RunMetrics(ctx context.Context) {
	statser := stats.FromContext(ctx)

	notify, cancel := statser.RegisterFlush()
	defer cancel()

	for {
		select {
		case <-notify:
			hfh.emitMetrics(statser)
		case <-ctx.Done():
			return
		}
	}
}

func (hfh *HttpForwarderHandlerV2) emitMetrics(statser stats.Statser) {
	messagesInvalid := atomic.SwapUint64(&hfh.messagesInvalid, 0)
	messagesCreated := atomic.SwapUint64(&hfh.messagesCreated, 0)
	messagesSent := atomic.SwapUint64(&hfh.messagesSent, 0)
	messagesRetried := atomic.SwapUint64(&hfh.messagesRetried, 0)
	messagesDropped := atomic.SwapUint64(&hfh.messagesDropped, 0)

	statser.Count("http.forwarder.invalid", float64(messagesInvalid), nil)
	statser.Count("http.forwarder.created", float64(messagesCreated), nil)
	statser.Count("http.forwarder.sent", float64(messagesSent), nil)
	statser.Count("http.forwarder.retried", float64(messagesRetried), nil)
	statser.Count("http.forwarder.dropped", float64(messagesDropped), nil)
}

func (hfh *HttpForwarderHandlerV2) Run(ctx context.Context) {
	var wg wait.Group
	defer wg.Wait()
	wg.StartWithContext(ctx, hfh.consolidator.Run)

	for {
		select {
		case <-ctx.Done():
			return
		case metricMaps := <-hfh.consolidatedMetrics:
			if !hfh.acquireSem(ctx) {
				return
			}
			metricMap := mergeMaps(metricMaps)
			postId := atomic.AddUint64(&hfh.postId, 1) - 1
			go func(postId uint64) {
				hfh.postMetrics(ctx, metricMap, postId)
				hfh.releaseSem()
			}(postId)
		}
	}
}

func mergeMaps(maps []*gostatsd.MetricMap) *gostatsd.MetricMap {
	mm := gostatsd.NewMetricMap()
	for _, m := range maps {
		mm.Merge(m)
	}
	return mm
}

func (hfh *HttpForwarderHandlerV2) acquireSem(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-hfh.metricsSem:
		return true
	}
}

func (hfh *HttpForwarderHandlerV2) releaseSem() {
	hfh.metricsSem <- struct{}{} // will never block
}

func translateToProtobufV2(metricMap *gostatsd.MetricMap) *pb.RawMessageV2 {
	var pbMetricMap pb.RawMessageV2

	pbMetricMap.Gauges = map[string]*pb.GaugeTagV2{}
	for metricName, m := range metricMap.Gauges {
		pbMetricMap.Gauges[metricName] = &pb.GaugeTagV2{TagMap: map[string]*pb.RawGaugeV2{}}
		for tagsKey, metric := range m {
			pbMetricMap.Gauges[metricName].TagMap[tagsKey] = &pb.RawGaugeV2{
				Tags:     metric.Tags,
				Hostname: metric.Hostname,
				Value:    metric.Value,
			}
		}
	}

	pbMetricMap.Counters = map[string]*pb.CounterTagV2{}
	for metricName, m := range metricMap.Counters {
		pbMetricMap.Counters[metricName] = &pb.CounterTagV2{TagMap: map[string]*pb.RawCounterV2{}}
		for tagsKey, metric := range m {
			pbMetricMap.Counters[metricName].TagMap[tagsKey] = &pb.RawCounterV2{
				Tags:     metric.Tags,
				Hostname: metric.Hostname,
				Value:    metric.Value,
			}
		}
	}

	pbMetricMap.Sets = map[string]*pb.SetTagV2{}
	for metricName, m := range metricMap.Sets {
		pbMetricMap.Sets[metricName] = &pb.SetTagV2{TagMap: map[string]*pb.RawSetV2{}}
		for tagsKey, metric := range m {
			var values []string
			for key := range metric.Values {
				values = append(values, key)
			}
			pbMetricMap.Sets[metricName].TagMap[tagsKey] = &pb.RawSetV2{
				Tags:     metric.Tags,
				Hostname: metric.Hostname,
				Values:   values,
			}
		}
	}

	pbMetricMap.Timers = map[string]*pb.TimerTagV2{}
	for metricName, m := range metricMap.Timers {
		pbMetricMap.Timers[metricName] = &pb.TimerTagV2{TagMap: map[string]*pb.RawTimerV2{}}
		for tagsKey, metric := range m {
			pbMetricMap.Timers[metricName].TagMap[tagsKey] = &pb.RawTimerV2{
				Tags:        metric.Tags,
				Hostname:    metric.Hostname,
				SampleCount: metric.SampledCount,
				Values:      metric.Values,
			}
		}
	}

	return &pbMetricMap
}

func (hfh *HttpForwarderHandlerV2) postMetrics(ctx context.Context, metricMap *gostatsd.MetricMap, batchId uint64) {
	message := translateToProtobufV2(metricMap)
	hfh.post(ctx, message, batchId, "metrics", "/v2/raw")
}

func (hfh *HttpForwarderHandlerV2) post(ctx context.Context, message proto.Message, id uint64, endpointType, endpoint string) {
	logger := hfh.logger.WithFields(logrus.Fields{
		"id":   id,
		"type": endpointType,
	})

	post, err := hfh.constructPost(ctx, logger, hfh.apiEndpoint+endpoint, message)
	if err != nil {
		atomic.AddUint64(&hfh.messagesInvalid, 1)
		logger.WithError(err).Error("failed to create request")
		return
	} else {
		atomic.AddUint64(&hfh.messagesCreated, 1)
	}

	b := backoff.NewExponentialBackOff()
	b.MaxElapsedTime = hfh.maxRequestElapsedTime

	for {
		if err = post(); err == nil {
			atomic.AddUint64(&hfh.messagesSent, 1)
			return
		}

		next := b.NextBackOff()
		if next == backoff.Stop {
			atomic.AddUint64(&hfh.messagesDropped, 1)
			logger.WithError(err).Info("failed to send, giving up")
			return
		}

		atomic.AddUint64(&hfh.messagesRetried, 1)

		timer := clock.NewTimer(ctx, next)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (hfh *HttpForwarderHandlerV2) serialize(message proto.Message) ([]byte, error) {
	buf, err := proto.Marshal(message)
	if err != nil {
		return nil, err
	}

	return buf, nil
}

// debug rendering
/*
func (hh *HttpForwarderHandlerV2) serializeText(message proto.Message) ([]byte, error) {
	buf := &bytes.Buffer{}
	err := proto.MarshalText(buf, message)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
*/

func (hfh *HttpForwarderHandlerV2) serializeAndCompress(message proto.Message) ([]byte, error) {
	raw, err := hfh.serialize(message)
	if err != nil {
		return nil, err
	}

	buf := &bytes.Buffer{}
	compressor, err := zlib.NewWriterLevel(buf, zlib.BestCompression)
	if err != nil {
		return nil, err
	}

	_, _ = compressor.Write(raw) // error is propagated through Close
	err = compressor.Close()
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func (hfh *HttpForwarderHandlerV2) constructPost(ctx context.Context, logger logrus.FieldLogger, path string, message proto.Message) (func() error /*doPost*/, error) {
	var body []byte
	var err error
	var encoding string

	if hfh.compress {
		body, err = hfh.serializeAndCompress(message)
		encoding = "deflate"
	} else {
		body, err = hfh.serialize(message)
		encoding = "identity"
	}

	if err != nil {
		return nil, err
	}

	return func() error {
		headers := map[string]string{
			"Content-Type":     "application/x-protobuf",
			"Content-Encoding": encoding,
			"User-Agent":       "gostatsd (http forwarder)",
		}
		req, err := http.NewRequest("POST", path, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("unable to create http.Request: %v", err)
		}
		req = req.WithContext(ctx)
		for header, v := range headers {
			req.Header.Set(header, v)
		}
		resp, err := hfh.client.Do(req)
		if err != nil {
			return fmt.Errorf("error POSTing: %v", err)
		}
		defer func() {
			_, _ = io.Copy(ioutil.Discard, resp.Body)
			resp.Body.Close()
		}()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			bodyStart, _ := ioutil.ReadAll(io.LimitReader(resp.Body, 512))
			logger.WithFields(logrus.Fields{
				"status": resp.StatusCode,
				"body":   string(bodyStart),
			}).Info("failed request")
			return fmt.Errorf("received bad status code %d", resp.StatusCode)
		}
		return nil
	}, nil
}

///////// Event processing

// Events are handled individually, because the context matters. If they're buffered through the consolidator, they'll
// be processed on a goroutine with a context which will be closed during shutdown.  Events should be rare enough that
// this isn't an issue.

func (hfh *HttpForwarderHandlerV2) DispatchEvent(ctx context.Context, e *gostatsd.Event) {
	hfh.eventWg.Add(1)
	go hfh.dispatchEvent(ctx, e)
}

func (hfh *HttpForwarderHandlerV2) dispatchEvent(ctx context.Context, e *gostatsd.Event) {
	postId := atomic.AddUint64(&hfh.postId, 1) - 1

	message := &pb.EventV2{
		Title:          e.Title,
		Text:           e.Text,
		DateHappened:   e.DateHappened,
		Hostname:       e.Hostname,
		AggregationKey: e.AggregationKey,
		SourceTypeName: e.SourceTypeName,
		Tags:           e.Tags,
		SourceIP:       string(e.SourceIP),
	}

	switch e.Priority {
	case gostatsd.PriNormal:
		message.Priority = pb.EventV2_Normal
	case gostatsd.PriLow:
		message.Priority = pb.EventV2_Low
	}

	switch e.AlertType {
	case gostatsd.AlertInfo:
		message.Type = pb.EventV2_Info
	case gostatsd.AlertWarning:
		message.Type = pb.EventV2_Warning
	case gostatsd.AlertError:
		message.Type = pb.EventV2_Error
	case gostatsd.AlertSuccess:
		message.Type = pb.EventV2_Success
	}

	hfh.post(ctx, message, postId, "event", "/v2/event")

	defer hfh.eventWg.Done()
}

func (hfh *HttpForwarderHandlerV2) WaitForEvents() {
	hfh.eventWg.Wait()
}
