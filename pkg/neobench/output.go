package neobench

import (
	"fmt"
	"github.com/codahale/hdrhistogram"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type ProgressReport struct {
	Section      string
	Step         string
	Completeness float64
}

type Result struct {
	// Targeted database
	DatabaseName string
	Scenario     string

	FailedByErrorGroup map[string]FailureGroup

	// Results by script
	Scripts map[string]*ScriptResult
}

func NewResult(databaseName, scenario string) Result {
	return Result{
		DatabaseName:       databaseName,
		Scenario:           scenario,
		FailedByErrorGroup: make(map[string]FailureGroup),
		Scripts:            make(map[string]*ScriptResult),
	}
}

func (r *Result) TotalSucceeded() (n int64) {
	for _, s := range r.Scripts {
		n += s.Succeeded
	}
	return
}

func (r *Result) TotalFailed() (n int64) {
	for _, s := range r.Scripts {
		n += s.Failed
	}
	return
}

func (r *Result) TotalRate() (n float64) {
	for _, s := range r.Scripts {
		n += s.Rate
	}
	return
}

func (r *Result) Add(res WorkerResult) {
	for _, workerScriptResult := range res.Scripts {
		combinedScriptResult := r.Scripts[workerScriptResult.ScriptName]
		if combinedScriptResult == nil {
			r.Scripts[workerScriptResult.ScriptName] = &ScriptResult{
				ScriptName: workerScriptResult.ScriptName,
				Latencies:  hdrhistogram.Import(workerScriptResult.Latencies.Export()),
				Rate:       workerScriptResult.Rate,
				Succeeded:  workerScriptResult.Succeeded,
				Failed:     workerScriptResult.Failed,
			}
		} else {
			combinedScriptResult.Rate += workerScriptResult.Rate
			combinedScriptResult.Succeeded += workerScriptResult.Succeeded
			combinedScriptResult.Failed += workerScriptResult.Failed
			combinedScriptResult.Latencies.Merge(workerScriptResult.Latencies)
		}
	}
	for name, group := range res.FailedByErrorGroup {
		existing, found := r.FailedByErrorGroup[name]
		if found {
			r.FailedByErrorGroup[name] = FailureGroup{
				Count:        existing.Count + group.Count,
				FirstFailure: existing.FirstFailure,
			}
		} else {
			r.FailedByErrorGroup[name] = group
		}
	}
}

// Result for one script; normally a workload is just one script, but we allow workloads to be made up of
// lots of scripts as well, with a weighted random mix of them. We report results per-script, since latencies
// between different scripts will mean totally different things.
type ScriptResult struct {
	ScriptName string
	// Rate is scripts executed per second, both succeeded and failed
	// TODO should this just count succeeded? That creates confusing effects with how the workload paces itself tho..
	Rate      float64
	Failed    int64
	Succeeded int64
	Latencies *hdrhistogram.Histogram
}

type Output interface {
	// scenario is a string describing the flags you'd need to pass to neobench to run an equivalent load
	BenchmarkStart(databaseName, url, scenario string)
	// Called if running in --init mode, eg. we are doing dataset population for one of the built-in workloads
	ReportInitProgress(report ProgressReport)
	// Called at interval set by --progress <interval>
	ReportWorkloadProgress(completeness float64, checkpoint Result)
	// Called at workload completion if running in Throughput mode; this is the final result
	ReportThroughput(result Result)
	// Called at workload completion if running in Latency mode; this is the final result
	ReportLatency(result Result)
	// Called if the workload or setup fails
	Errorf(format string, a ...interface{})
}

// Creates the output specified by name; if prometheusAddress is set, also starts
// that as an output, returning an output that publishes to both
// TODO(jake): Maybe this would be nicer with `name` a comma-separated list, eg. csv,prometheus
func InitOutput(name, prometheusAddress string) (Output, error) {
	if name == "auto" {
		fi, _ := os.Stdout.Stat()
		if fi.Mode()&os.ModeCharDevice == 0 {
			name = "csv"
		} else {
			name = "interactive"
		}
	}

	var output Output
	if name == "interactive" {
		output = &InteractiveOutput{
			ErrStream: os.Stderr,
			OutStream: os.Stdout,
		}
	} else if name == "csv" {
		output = &CsvOutput{
			ErrStream: os.Stderr,
			OutStream: os.Stdout,
		}
	} else {
		return nil, fmt.Errorf("unknown output format: %s, supported formats are 'auto', 'interactive' and 'csv'", name)
	}

	if prometheusAddress != "" {
		InitPrometheus(prometheusAddress)
		output = &CombinedOutput{
			delegates: []Output{output, NewPrometheusOutput()},
		}
	}

	return output, nil
}

type InteractiveOutput struct {
	ErrStream io.Writer
	OutStream io.Writer
	// Used to rate-limit progress reporting
	LastProgressReport ProgressReport
	LastProgressTime   time.Time
}

func (o *InteractiveOutput) BenchmarkStart(databaseName, url, scenario string) {
	if databaseName == "" {
		databaseName = "<default>"
	}
	_, err := fmt.Fprintf(o.ErrStream,
		"Starting workload on database %s against %s\n"+
			"Scenario: %s\n", databaseName, url, scenario)
	if err != nil {
		panic(err)
	}
}

func (o *InteractiveOutput) ReportWorkloadProgress(completeness float64, checkpoint Result) {
	_, err := fmt.Fprintf(o.ErrStream, "[%.02f%%] %.02f tps / %d failures\n", completeness*100, checkpoint.TotalRate(), checkpoint.TotalFailed())
	if err != nil {
		panic(err)
	}
}

func (o *InteractiveOutput) ReportInitProgress(report ProgressReport) {
	now := time.Now()
	if report.Section == o.LastProgressReport.Section && report.Step == o.LastProgressReport.Step && now.Sub(o.LastProgressTime).Seconds() < 10 {
		return
	}
	o.LastProgressReport = report
	o.LastProgressTime = now
	_, err := fmt.Fprintf(o.ErrStream, "[%s][%s] %.02f%%\n", report.Section, report.Step, report.Completeness*100)
	if err != nil {
		panic(err)
	}
}

func (o *InteractiveOutput) ReportThroughput(result Result) {
	s := strings.Builder{}

	s.WriteString("== Results ==\n")
	s.WriteString(fmt.Sprintf("Scenario: %s\n", result.Scenario))
	s.WriteString(fmt.Sprintf("%d successful transactions, %d failed. (Total of %.3f per second)\n", result.TotalSucceeded(), result.TotalFailed(), result.TotalRate()))
	s.WriteString("\n")
	for _, script := range result.Scripts {
		s.WriteString(fmt.Sprintf("  [%s]: %.03f total transactions per second\n", script.ScriptName, script.Rate))
	}
	s.WriteString("\n")
	writeErrorReport(result, &s)

	_, err := fmt.Fprintf(o.OutStream, s.String())
	if err != nil {
		panic(err)
	}
}

func (o *InteractiveOutput) ReportLatency(result Result) {
	s := strings.Builder{}

	s.WriteString("== Results ==\n")

	s.WriteString(fmt.Sprintf("Scenario: %s\n", result.Scenario))
	s.WriteString(fmt.Sprintf("%d successful transactions, %d failed. (Total of %.3f per second)\n", result.TotalSucceeded(), result.TotalFailed(), result.TotalRate()))

	if result.TotalSucceeded() > 0 {
		for _, workload := range result.Scripts {
			s.WriteString("\n")
			s.WriteString(fmt.Sprintf("-- Script: %s --\n\n", workload.ScriptName))
			summarizeLatency(workload, &s, "  ")
		}
	}
	s.WriteString("\n")
	writeErrorReport(result, &s)

	_, err := fmt.Fprint(o.OutStream, s.String())
	if err != nil {
		panic(err)
	}
}

func summarizeLatency(script *ScriptResult, s *strings.Builder, indent string) {
	histo := script.Latencies
	lines := []string{
		fmt.Sprintf("%d successful transactions, %d failed. (Total of %.3f per second)\n", script.Succeeded, script.Failed, script.Rate),
		fmt.Sprintf("Max: %.3fms, Min: %.3fms, Mean: %.3fms, Stddev: %.3f\n\n",
			float64(histo.Max())/1000.0, float64(histo.Min())/1000.0, histo.Mean()/1000.0, histo.StdDev()/1000.0),
		fmt.Sprintf("Latency distribution:\n"),
		fmt.Sprintf("  P00.000: %.03fms\n", float64(histo.Min())/1000.0),
		fmt.Sprintf("  P25.000: %.03fms\n", float64(histo.ValueAtQuantile(25))/1000.0),
		fmt.Sprintf("  P50.000: %.03fms\n", float64(histo.ValueAtQuantile(50))/1000.0),
		fmt.Sprintf("  P75.000: %.03fms\n", float64(histo.ValueAtQuantile(75))/1000.0),
		fmt.Sprintf("  P95.000: %.03fms\n", float64(histo.ValueAtQuantile(95))/1000.0),
		fmt.Sprintf("  P99.000: %.03fms\n", float64(histo.ValueAtQuantile(99))/1000.0),
		fmt.Sprintf("  P99.999: %.03fms\n", float64(histo.ValueAtQuantile(99.999))/1000.0),
	}
	for _, line := range lines {
		s.WriteString(indent)
		s.WriteString(line)
	}
}

func writeErrorReport(result Result, s *strings.Builder) {
	s.WriteString(fmt.Sprintf("Error stats:\n"))
	if result.TotalFailed() == 0 {
		s.WriteString(fmt.Sprintf("  No errors!\n"))
	} else {
		s.WriteString(fmt.Sprintf("  Failed transactions: %d (%.3f %%)\n", result.TotalFailed(), 100*float64(result.TotalFailed())/float64(result.TotalFailed()+result.TotalSucceeded())))
		s.WriteString(fmt.Sprintf("\n"))
		s.WriteString(fmt.Sprintf("  Causes:\n"))
		for name, info := range result.FailedByErrorGroup {
			s.WriteString(fmt.Sprintf("    %s: %d failures\n", name, info.Count))
			s.WriteString(fmt.Sprintf("      (ex: %s)\n", info.FirstFailure))
		}
	}
}

func (o *InteractiveOutput) Errorf(format string, a ...interface{}) {
	_, err := fmt.Fprintf(o.ErrStream, "ERROR: %s\n", fmt.Sprintf(format, a...))
	if err != nil {
		panic(err)
	}
}

// Writes simple progress to stderr, and then a result for easy import into eg. a spreadsheet or other app
// in CSV format to stdout
type CsvOutput struct {
	ErrStream io.Writer
	OutStream io.Writer
	// Used to rate-limit progress reporting
	LastProgressReport ProgressReport
	LastProgressTime   time.Time
}

func (o *CsvOutput) BenchmarkStart(databaseName, url, scenario string) {
	if databaseName == "" {
		databaseName = "<default>"
	}
	_, err := fmt.Fprintf(o.ErrStream,
		"Starting workload on database %s against %s\n"+
			"Scenario: %s\n", databaseName, url, scenario)
	if err != nil {
		panic(err)
	}

	columnNames := make([]string, 0, len(csvColumns))
	for _, col := range csvColumns {
		columnNames = append(columnNames, col.name)
	}
	_, err = fmt.Fprintf(o.OutStream, "%s\n", strings.Join(columnNames, ","))
	if err != nil {
		panic(err)
	}
}

func (o *CsvOutput) ReportInitProgress(report ProgressReport) {
	now := time.Now()
	if report.Section == o.LastProgressReport.Section && report.Step == o.LastProgressReport.Step && now.Sub(o.LastProgressTime).Seconds() < 10 {
		return
	}
	o.LastProgressReport = report
	o.LastProgressTime = now
	_, err := fmt.Fprintf(o.ErrStream, "[%s][%s] %.02f%%\n", report.Section, report.Step, report.Completeness*100)
	if err != nil {
		panic(err)
	}
}

func (o *CsvOutput) ReportWorkloadProgress(completeness float64, checkpoint Result) {
	_, err := fmt.Fprintf(o.ErrStream, "[workload] %.02f%% done\n", completeness*100)
	if err != nil {
		panic(err)
	}
	o.ReportLatency(checkpoint)
}

func (o *CsvOutput) ReportThroughput(result Result) {
	columns := []string{"script", "succeeded", "failed", "transactions_per_second"}

	s := strings.Builder{}
	separator := ","
	s.WriteString(strings.Join(columns, separator))
	s.WriteString("\n")

	for _, script := range result.Scripts {
		row := []float64{
			float64(script.Succeeded),
			float64(script.Failed),
			script.Rate,
		}
		s.WriteString(fmt.Sprintf("\"%s\",", script.ScriptName))
		for i, cell := range row {
			if i > 0 {
				s.WriteString(separator)
			}
			s.WriteString(fmt.Sprintf("%.03f", cell))
		}
		s.WriteString("\n")
	}

	if _, err := fmt.Fprint(o.OutStream, s.String()); err != nil {
		panic(err)
	}

	if result.TotalFailed() > 0 {
		s.Reset()
		writeErrorReport(result, &s)
		if _, err := fmt.Fprint(o.ErrStream, s.String()); err != nil {
			panic(err)
		}
	}
}

func (o *CsvOutput) ReportLatency(result Result) {
	o.writeLatencyRow(result)
}

func (o *CsvOutput) writeLatencyRow(result Result) {
	s := strings.Builder{}

	for _, script := range result.Scripts {
		for i, col := range csvColumns {
			if i != 0 {
				s.WriteString(",")
			}
			s.WriteString(col.value(result, script))
		}
		s.WriteString("\n")
	}

	_, err := fmt.Fprint(o.OutStream, s.String())
	if err != nil {
		panic(err)
	}

	if result.TotalFailed() > 0 {
		s.Reset()
		writeErrorReport(result, &s)
		if _, err := fmt.Fprint(o.ErrStream, s.String()); err != nil {
			panic(err)
		}
	}
}

func fmtFloat(v interface{}) string {
	switch v.(type) {
	case int64:
		return fmt.Sprintf("%.3f", float64(v.(int64)))
	case float64:
		return fmt.Sprintf("%.3f", v.(float64))
	}
	return fmt.Sprintf("%v?", v)
}

var csvColumns = []struct {
	name  string
	value func(r Result, s *ScriptResult) string
}{
	{"db", func(r Result, s *ScriptResult) string { return fmt.Sprintf("\"%s\"", r.DatabaseName) }},
	{"script", func(r Result, s *ScriptResult) string { return fmt.Sprintf("\"%s\"", s.ScriptName) }},
	{"rate", func(r Result, s *ScriptResult) string { return fmtFloat(s.Rate) }},
	{"succeeded", func(r Result, s *ScriptResult) string { return fmtFloat(s.Latencies.TotalCount()) }},
	{"failed", func(r Result, s *ScriptResult) string { return fmtFloat(s.Failed) }},
	{"mean", func(r Result, s *ScriptResult) string { return fmtFloat(s.Latencies.Mean() / 1000.0) }},
	{"stdev", func(r Result, s *ScriptResult) string { return fmtFloat(s.Latencies.StdDev()) }},
	{"p0", func(r Result, s *ScriptResult) string { return fmtFloat(float64(s.Latencies.Min()) / 1000.0) }},
	{"p25", func(r Result, s *ScriptResult) string {
		return fmtFloat(float64(s.Latencies.ValueAtQuantile(25)) / 1000.0)
	}},
	{"p50", func(r Result, s *ScriptResult) string {
		return fmtFloat(float64(s.Latencies.ValueAtQuantile(50)) / 1000.0)
	}},
	{"p75", func(r Result, s *ScriptResult) string {
		return fmtFloat(float64(s.Latencies.ValueAtQuantile(75)) / 1000.0)
	}},
	{"p99", func(r Result, s *ScriptResult) string {
		return fmtFloat(float64(s.Latencies.ValueAtQuantile(99)) / 1000.0)
	}},
	{"p99999", func(r Result, s *ScriptResult) string {
		return fmtFloat(float64(s.Latencies.ValueAtQuantile(99.999)) / 1000.0)
	}},
	{"p100", func(r Result, s *ScriptResult) string { return fmtFloat(float64(s.Latencies.Max()) / 1000.0) }},
}

func (o *CsvOutput) Errorf(format string, a ...interface{}) {
	_, err := fmt.Fprintf(o.ErrStream, "ERROR: %s\n", fmt.Sprintf(format, a...))
	if err != nil {
		panic(err)
	}
}

// Call once at app init; starts the prometheus http endpoint
func InitPrometheus(addr string) {
	http.Handle("/metrics", promhttp.Handler())
	go func() {
		err := http.ListenAndServe(addr, nil)
		if err != nil {
			panic(errors.Wrap(err, "prometheus http server failed"))
		}
	}()
}

type PrometheusOutput struct {
	totalSucceededCounter prometheus.Counter
	totalFailedCounter    prometheus.Counter
}

func NewPrometheusOutput() *PrometheusOutput {
	return &PrometheusOutput{
		totalSucceededCounter: promauto.NewCounter(prometheus.CounterOpts{
			Name: "neobench_successful_transactions_total",
			Help: "The total number of successful transactions",
		}),
		totalFailedCounter: promauto.NewCounter(prometheus.CounterOpts{
			Name: "neobench_failed_transactions_total",
			Help: "The total number of failed transactions",
		}),
	}
}

func (p *PrometheusOutput) BenchmarkStart(databaseName, url, scenario string) {
}

func (p *PrometheusOutput) ReportInitProgress(report ProgressReport) {
}

func (p *PrometheusOutput) ReportWorkloadProgress(completeness float64, checkpoint Result) {
	p.totalSucceededCounter.Add(float64(checkpoint.TotalSucceeded()))
	p.totalFailedCounter.Add(float64(checkpoint.TotalFailed()))
}

func (p *PrometheusOutput) ReportThroughput(result Result) {
}

func (p *PrometheusOutput) ReportLatency(result Result) {
}

func (p *PrometheusOutput) Errorf(format string, a ...interface{}) {
}

var _ Output = &PrometheusOutput{}

// Combines multiple output mechanisms; we use this to eg. both write to stdout and publish to prometheus
type CombinedOutput struct {
	delegates []Output
}

func (c *CombinedOutput) BenchmarkStart(databaseName, url, scenario string) {
	for _, d := range c.delegates {
		d.BenchmarkStart(databaseName, url, scenario)
	}
}

func (c *CombinedOutput) ReportInitProgress(report ProgressReport) {
	for _, d := range c.delegates {
		d.ReportInitProgress(report)
	}
}

func (c *CombinedOutput) ReportWorkloadProgress(completeness float64, checkpoint Result) {
	for _, d := range c.delegates {
		d.ReportWorkloadProgress(completeness, checkpoint)
	}
}

func (c *CombinedOutput) ReportThroughput(result Result) {
	for _, d := range c.delegates {
		d.ReportThroughput(result)
	}
}

func (c *CombinedOutput) ReportLatency(result Result) {
	for _, d := range c.delegates {
		d.ReportLatency(result)
	}
}

func (c *CombinedOutput) Errorf(format string, a ...interface{}) {
	for _, d := range c.delegates {
		d.Errorf(format, a)
	}
}

var _ Output = &CombinedOutput{}
