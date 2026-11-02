// load generator against the function urls that takes a list of function invocations with timestamps and sends a request
// for each invocation and writes in a csv row that status (success or failed) and the response time
package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"nydus-cluster-cache/pkg/sim"
)

func main() {
	tracePath := flag.String("trace", "", "list of function invocations to replay")
	urlsPath := flag.String("urls", "", "JSON map from function name to its address")
	timeout := flag.Duration("timeout", 120*time.Second, "per-request timeout")
	gateway := flag.String("gateway", "", "optional address that every request has to be directed towards first => needs labeling of the corresponding app for redirection")
	flag.Parse()

	// validate required params
	if *tracePath == "" || *urlsPath == "" {
		log.Fatal("need -trace and -urls")
	}

	trace, err := sim.LoadTrace(*tracePath)
	if err != nil {
		log.Fatal(err)
	}

	urls, err := loadURLs(*urlsPath, trace.Apps)
	if err != nil {
		log.Fatal(err)
	}

	targets, err := resolveTargets(urls, *gateway)
	if err != nil {
		log.Fatal(err)
	}

	// run load generator
	failed, err := replay(trace, targets, &http.Client{Timeout: *timeout}, os.Stdout)
	if err != nil {
		log.Fatal(err)
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "%d requests failed\n", failed)
		os.Exit(1)
	}
}

// read the url json file and create a lookup map from it function name - address
func loadURLs(path string, functions []string) (map[string]string, error) {
	urlJsonFile, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("error reading the urls json: %w", err)
	}

	urls := map[string]string{}
	if err := json.Unmarshal(urlJsonFile, &urls); err != nil {
		return nil, fmt.Errorf("failed to parse urls: %w", err)
	}

	for _, currFunc := range functions {
		if urls[currFunc] == "" {
			return nil, fmt.Errorf("function %s has no url", currFunc)
		}
	}
	return urls, nil
}

type target struct {
	url  string
	host string
}

// builds for every function the target address
// => without gateway we just use the function url without any changes
func resolveTargets(urls map[string]string, gateway string) (map[string]target, error) {
	targets := map[string]target{}
	for function, funcUrl := range urls {
		if gateway == "" {
			targets[function] = target{url: funcUrl}
			continue
		}

		parsed, err := url.Parse(funcUrl)
		if err != nil {
			return nil, fmt.Errorf("failed to parse url for func %s: %w", function, err)
		}

		targets[function] = target{url: gateway + parsed.Path, host: parsed.Host}
	}
	return targets, nil
}

// metrics/result of a sent request
type requestRecord struct {
	atMs      int64     // when the func call happens from start of trace
	function  string    // function name
	status    int       // http status code
	latencyMs int64     // response time
	sentAt    time.Time // timestamp when request was sent
}

// main function to sent the requests and record the results to csv
func replay(trace *sim.Trace, targets map[string]target, client *http.Client, out io.Writer) (int, error) {
	resultFileWriter := csv.NewWriter(out)
	defer resultFileWriter.Flush()
	if err := resultFileWriter.Write([]string{"at_ms", "app", "status", "latency_ms", "sent_at_unix_ms"}); err != nil {
		return 0, fmt.Errorf("failed to write header: %w", err)
	}

	// use mutex since responses are out of order
	var mutex sync.Mutex
	var waitGrp sync.WaitGroup
	failed := 0
	var writeErr error
	start := time.Now()
	for _, currInvocation := range trace.Invocations {

		// Sleep until the function call is due
		time.Sleep(time.Until(dueAt(start, currInvocation)))
		waitGrp.Add(1)
		go func(inv sim.Invocation) {
			defer waitGrp.Done()
			// look up the function name & send request
			app := trace.Apps[inv.App]
			record := sendRequest(client, targets[app], inv.AtMs, app)

			// record failures
			mutex.Lock()
			defer mutex.Unlock()
			if record.status == 0 || record.status >= 500 {
				failed++
			}
			if err := resultFileWriter.Write(record.fields()); err != nil && writeErr == nil {
				writeErr = fmt.Errorf("failed to write request line: %w", err)
			}
		}(currInvocation)
	}
	waitGrp.Wait()
	return failed, writeErr
}

// sends one request (get) and measures response time
func sendRequest(client *http.Client, targetUrl target, atMs int64, function string) requestRecord {
	sent := time.Now()
	status := 0
	req, err := http.NewRequest(http.MethodGet, targetUrl.url, nil)

	if err == nil {
		if targetUrl.host != "" {
			req.Host = targetUrl.host
		}

		var resp *http.Response
		resp, err = client.Do(req)
		if err == nil {
			status = resp.StatusCode
			resp.Body.Close()
		}
	}

	return requestRecord{
		atMs:      atMs,
		function:  function,
		status:    status,
		latencyMs: time.Since(sent).Milliseconds(),
		sentAt:    sent,
	}
}

func (record requestRecord) fields() []string {
	return []string{
		strconv.FormatInt(record.atMs, 10),
		record.function,
		strconv.Itoa(record.status),
		strconv.FormatInt(record.latencyMs, 10),
		strconv.FormatInt(record.sentAt.UnixMilli(), 10),
	}
}

func dueAt(start time.Time, inv sim.Invocation) time.Time {
	return start.Add(time.Duration(inv.AtMs) * time.Millisecond)
}
