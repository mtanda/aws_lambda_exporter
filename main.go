package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/lambda"
	"github.com/aws/aws-sdk-go/service/lambda/lambdaiface"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/log"
)

const (
	namespace = "aws_lambda"
)

var (
	lambda_metadata = prometheus.NewDesc(
		"aws_lambda",
		"aws_lambda",
		[]string{"region"},
		nil,
	)
)

type Exporter struct {
	lambdaClient lambdaiface.LambdaAPI
	functionName string
}

func NewExporter(region string, functionName string) (*Exporter, error) {
	cfg := &aws.Config{Region: aws.String(region)}
	sess := session.Must(session.NewSession(cfg))
	return &Exporter{
		lambdaClient: lambda.New(sess),
		functionName: functionName,
	}, nil
}

var descMap = make(map[string]*prometheus.Desc)

func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	parsed, err := e.invokeLambda()
	if err != nil {
		log.Error(err)
		return
	}

	for _, mf := range parsed {
		lm := make(map[string]bool)
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				lm[l.GetName()] = true
			}
		}
		labels := []string{}
		for k := range lm {
			labels = append(labels, k)
		}
		sort.Strings(labels)
		desc := prometheus.NewDesc(
			*mf.Name,
			*mf.Help,
			labels,
			nil,
		)
		descMap[*mf.Name] = desc
		ch <- desc
	}
}

func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	parsed, err := e.invokeLambda()
	if err != nil {
		log.Error(err)
		return
	}

	for _, mf := range parsed {
		for _, m := range mf.GetMetric() {
			if desc, ok := descMap[*mf.Name]; ok {
				value := 0.0
				var valueType prometheus.ValueType
				switch *mf.Type {
				case dto.MetricType_COUNTER:
					value = *m.Counter.Value
					valueType = prometheus.CounterValue
				case dto.MetricType_GAUGE:
					value = *m.Gauge.Value
					valueType = prometheus.GaugeValue
				case dto.MetricType_SUMMARY:
					// unsupported
				case dto.MetricType_HISTOGRAM:
					// unsupported
				}
				labels := m.GetLabel()
				sort.Slice(labels, func(i, j int) bool {
					return labels[i].GetName() < labels[j].GetName()
				})
				labelValues := []string{}
				for _, l := range labels {
					labelValues = append(labelValues, l.GetValue())
				}
				m := prometheus.MustNewConstMetric(desc, valueType, value, labelValues...)
				ch <- m
			}
		}
	}
}

type lambdaResult struct {
	Result string
}

func (e *Exporter) invokeLambda() (map[string]*dto.MetricFamily, error) {
	input := &lambda.InvokeInput{
		FunctionName: aws.String(e.functionName),
	}

	result, err := e.lambdaClient.Invoke(input)
	if err != nil {
		return nil, err
	}
	if *result.StatusCode != 200 {
		return nil, fmt.Errorf("lambda invoke error: %s\n", result.FunctionError)
	}
	log.Debugf("lambda log result: %s\n", result.LogResult)

	var jsonResult lambdaResult
	err = json.Unmarshal(result.Payload, &jsonResult)
	if err != nil {
		return nil, err
	}

	var parser expfmt.TextParser
	parsed, err := parser.TextToMetricFamilies(strings.NewReader(jsonResult.Result))
	if err != nil {
		return nil, err
	}

	return parsed, nil
}

func handler(w http.ResponseWriter, r *http.Request) {
	functionName := r.URL.Query()["function_name[]"][0]
	log.Debugln("function name:", functionName)

	region, err := GetDefaultRegion()
	if err != nil {
		log.Warnln("Couldn't get region", err)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(fmt.Sprintf("Couldn't get region %s", err)))
		return
	}
	exporter, err := NewExporter(region, functionName)
	if err != nil {
		log.Warnln("Couldn't create", err)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(fmt.Sprintf("Couldn't create %s", err)))
		return
	}
	registry := prometheus.NewRegistry()
	err = registry.Register(exporter)
	if err != nil {
		log.Errorln("Couldn't register collector:", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf("Couldn't register collector: %s", err)))
		return
	}

	gatherers := prometheus.Gatherers{
		prometheus.DefaultGatherer,
		registry,
	}
	h := promhttp.InstrumentMetricHandler(
		registry,
		promhttp.HandlerFor(gatherers,
			promhttp.HandlerOpts{
				ErrorLog:      log.NewErrorLogger(),
				ErrorHandling: promhttp.ContinueOnError,
			}),
	)
	h.ServeHTTP(w, r)
}

var region string

func GetDefaultRegion() (string, error) {
	if region != "" {
		return region, nil
	}

	sess, err := session.NewSession()
	if err != nil {
		return "", err
	}
	metadata := ec2metadata.New(sess, &aws.Config{
		MaxRetries: aws.Int(0),
	})
	if metadata.Available() {
		var err error
		region, err = metadata.Region()
		if err != nil {
			return "", err
		}
	} else {
		region = os.Getenv("AWS_REGION")
		if region == "" {
			region = "us-east-1"
		}
	}

	return region, nil
}

type config struct {
	listenAddress string
	metricsPath   string
}

func main() {
	var cfg config
	flag.StringVar(&cfg.listenAddress, "web.listen-address", ":9408", "Address to listen on for web endpoints.")
	flag.StringVar(&cfg.metricsPath, "web.telemetry-path", "/metrics", "Path under which to expose metrics.")
	flag.Parse()

	http.HandleFunc(cfg.metricsPath, handler)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
			<head><title>AWS Lambda Exporter</title></head>
			<body>
			<h1>AWS Lambda Exporter</h1>
			<p><a href="` + cfg.metricsPath + `">Metrics</a></p>
			</body>
			</html>`))
	})

	log.Infoln("Listening on", cfg.listenAddress)
	err := http.ListenAndServe(cfg.listenAddress, nil)
	if err != nil {
		log.Fatal(err)
	}
}
