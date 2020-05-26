package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promlog"
	"github.com/prometheus/common/promlog/flag"
	"github.com/prometheus/common/version"
	"gopkg.in/alecthomas/kingpin.v2"
)

const (
	namespace = "janus" // For Prometheus metrics.
)

func newMetric(metricName string, docString string, constLabels prometheus.Labels) *prometheus.Desc {
	return prometheus.NewDesc(prometheus.BuildFQName(namespace, "", metricName), docString, nil, constLabels)
}

var (
	currentSessions = newMetric("current_sessions", "Current number of active sessions.", nil)
	currentHandles  = newMetric("current_handles", "Current number of open plugin handles", nil)
	janusUp         = newMetric("up", "Was the last scrape successful.", nil)
)

// Exporter collects Janus stats from the given URI and exports them using
// the prometheus metrics package.
type Exporter struct {
	URI     string
	secret  string
	timeout time.Duration
	mutex   sync.RWMutex

	up           prometheus.Gauge
	totalScrapes prometheus.Counter
	logger       log.Logger
}

// NewExporter returns an initialized Exporter.
func NewExporter(uri string, secret string, timeout time.Duration, logger log.Logger) (*Exporter, error) {
	_, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}

	return &Exporter{
		URI:     uri,
		secret:  secret,
		timeout: timeout,
		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "up",
			Help:      "Was the last scrape of Janus successful.",
		}),
		totalScrapes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "exporter_total_scrapes",
			Help:      "Current total Janus scrapes.",
		}),
		logger: logger,
	}, nil
}

// Describe describes all the metrics ever exported by the Janus exporter. It
// implements prometheus.Collector.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- currentSessions
	ch <- currentHandles
	ch <- janusUp
	ch <- e.totalScrapes.Desc()
}

// Collect fetches the stats from configured Janus location and delivers them
// as Prometheus metrics. It implements prometheus.Collector.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.mutex.Lock() // To protect metrics from concurrent collects.
	defer e.mutex.Unlock()

	up := e.scrape(ch)

	ch <- prometheus.MustNewConstMetric(janusUp, prometheus.GaugeValue, up)
	ch <- e.totalScrapes
}

// JanusAdminRequest contains common fields of a janus admin request
type JanusAdminRequest struct {
	Janus       string `json:"janus"`
	Transaction string `json:"transaction"`
	AdminSecret string `json:"admin_secret,omitempty"`
	SessionID   uint64 `json:"session_id,omitempty"`
}

// JanusAdminResponder allows checking for successful reply
type JanusAdminResponder interface {
	check() error
}

// JanusAdminResponse contains common fields of a janus admin response
type JanusAdminResponse struct {
	Janus       string `json:"janus"`
	Transaction string `json:"transaction"`
	Error       *struct {
		Code   int32  `json:"code"`
		Reason string `json:"reason"`
	} `json:"error,omitempty"`
}

func (r JanusAdminResponse) check() error {
	if r.Janus == "success" {
		return nil
	}

	if r.Error != nil {
		return fmt.Errorf("Janus replied %s (code %d): %s", r.Janus, r.Error.Code, r.Error.Reason)
	}

	return fmt.Errorf("Janus replied %s", r.Janus)
}

// JanusSessions is a response to a "list_sessions" request
type JanusSessions struct {
	JanusAdminResponse
	Sessions []uint64 `json:"sessions"`
}

// JanusHandles is a response to a "list_handles" request
type JanusHandles struct {
	JanusAdminResponse
	SessionID uint64   `json:"session_id"`
	Handles   []uint64 `json:"handles"`
}

func generateTxID() string {
	buff := make([]byte, 9)
	rand.Read(buff)
	return base64.StdEncoding.EncodeToString(buff)
}

func (e *Exporter) sendRequest(pathSuffix string, body JanusAdminRequest, result interface{}) error {
	client := http.Client{
		Timeout: e.timeout,
	}

	buf := new(bytes.Buffer)
	json.NewEncoder(buf).Encode(body)
	req, _ := http.NewRequest("POST", e.URI+pathSuffix, buf)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if !(resp.StatusCode >= 200 && resp.StatusCode < 300) {
		return fmt.Errorf("HTTP status %d", resp.StatusCode)
	}

	err = json.NewDecoder(resp.Body).Decode(result)
	if err != nil {
		return err
	}

	return nil
}

func (e *Exporter) listSessions() ([]uint64, error) {
	reqBody := JanusAdminRequest{
		Janus:       "list_sessions",
		Transaction: generateTxID(),
		AdminSecret: e.secret,
	}

	sessionList := JanusSessions{}
	err := e.sendRequest("", reqBody, &sessionList)
	if err == nil {
		err = sessionList.check()
	}

	if err != nil {
		level.Error(e.logger).Log("msg", "Cannot list sessions", "err", err)
		return nil, err
	}

	return sessionList.Sessions, nil
}

func (e *Exporter) listHandles(sessionID uint64) ([]uint64, error) {
	reqBody := JanusAdminRequest{
		Janus:       "list_handles",
		Transaction: generateTxID(),
		AdminSecret: e.secret,
		SessionID:   sessionID,
	}

	handleList := JanusHandles{}
	err := e.sendRequest("", reqBody, &handleList)
	if err == nil {
		err = handleList.check()
	}

	if err != nil {
		level.Error(e.logger).Log("msg", "Cannot list handles of session "+strconv.FormatUint(sessionID, 10), "err", err)
		return nil, err
	}

	return handleList.Handles, nil
}

func (e *Exporter) scrape(ch chan<- prometheus.Metric) (up float64) {
	e.totalScrapes.Inc()

	sessionIds, err := e.listSessions()
	if err != nil {
		return 0
	}

	ch <- prometheus.MustNewConstMetric(currentSessions, prometheus.GaugeValue, float64(len(sessionIds)))

	handleCount := 0
	for _, sessionID := range sessionIds {
		handles, err := e.listHandles(sessionID)
		if err != nil {
			continue
		}
		handleCount += len(handles)
	}

	ch <- prometheus.MustNewConstMetric(currentHandles, prometheus.GaugeValue, float64(handleCount))

	return 1
}

func main() {
	var (
		listenAddress       = kingpin.Flag("web.listen-address", "Address to listen on for web interface and telemetry.").Default(":9709").String()
		metricsPath         = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics.").Default("/metrics").String()
		janusAdminURI       = kingpin.Flag("janus.admin-uri", "Janus admin api URI.").Default("http://localhost:8890/admin").String()
		janusAdminSecretEnv = kingpin.Flag("janus.admin-secret-env", "Environment variable containing Janus admin secret.").Default("").String()
		janusTimeout        = kingpin.Flag("janus.timeout", "Timeout for trying to get stats from Janus.").Default("5s").Duration()
	)

	promlogConfig := &promlog.Config{}
	flag.AddFlags(kingpin.CommandLine, promlogConfig)
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()
	logger := promlog.New(promlogConfig)

	level.Info(logger).Log("msg", "Starting janus_exporter", "version", version.Info())
	level.Info(logger).Log("msg", "Build context", "context", version.BuildContext())

	janusAdminSecret := ""
	if *janusAdminSecretEnv != "" {
		janusAdminSecret = os.Getenv(*janusAdminSecretEnv)
	}

	exporter, err := NewExporter(*janusAdminURI, janusAdminSecret, *janusTimeout, logger)
	if err != nil {
		level.Error(logger).Log("msg", "Error creating an exporter", "err", err)
		os.Exit(1)
	}
	prometheus.MustRegister(exporter)
	prometheus.MustRegister(version.NewCollector("janus_exporter"))

	level.Info(logger).Log("msg", "Listening on address", "address", *listenAddress)
	http.Handle(*metricsPath, promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
             <head><title>Janus Exporter by mluvii.com</title></head>
             <body>
             <h1>Janus Exporter by mluvii.com</h1>
             <p><a href='` + *metricsPath + `'>Metrics</a></p>
             </body>
             </html>`))
	})
	if err := http.ListenAndServe(*listenAddress, nil); err != nil {
		level.Error(logger).Log("msg", "Error starting HTTP server", "err", err)
		os.Exit(1)
	}
}
