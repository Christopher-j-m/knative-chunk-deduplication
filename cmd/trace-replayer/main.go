// Command trace-replayer fires a canonical invocation trace at a set of HTTP
// endpoints in real time, one request per invocation at its recorded offset,
// and writes one line per request with the observed latency. It is the load
// generator for the live check of the replay model.
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
	tracePath := flag.String("trace", "", "canonical trace CSV")
	urlsPath := flag.String("urls", "", "JSON map from app name to URL")
	timeout := flag.Duration("timeout", 120*time.Second, "per-request timeout")
	gateway := flag.String("gateway", "", "send every request to this base URL with the app URL's host as the Host header, for an ingress reached through a port-forward")
	flag.Parse()
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
	failed, err := replay(trace, targets, &http.Client{Timeout: *timeout}, os.Stdout)
	if err != nil {
		log.Fatal(err)
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "%d requests failed\n", failed)
		os.Exit(1)
	}
}

// loadURLs reads the app-to-URL map and checks that every app in the trace
// has a target before a single request goes out.
func loadURLs(path string, apps []string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read urls: %w", err)
	}
	urls := map[string]string{}
	if err := json.Unmarshal(raw, &urls); err != nil {
		return nil, fmt.Errorf("failed to parse urls: %w", err)
	}
	for _, app := range apps {
		if urls[app] == "" {
			return nil, fmt.Errorf("app %s has no url", app)
		}
	}
	return urls, nil
}

// target is where one app's requests go: the URL to connect to and the
// Host header the ingress routes on, which differ when the ingress is
// reached through a port-forward instead of its own name
type target struct {
	url  string
	host string
}

// resolveTargets turns the app URL map into connect targets. Without a
// gateway each app is called at its own URL; with one, every request goes
// to the gateway and carries the app URL's host so the ingress can route it.
func resolveTargets(urls map[string]string, gateway string) (map[string]target, error) {
	targets := map[string]target{}
	for app, raw := range urls {
		if gateway == "" {
			targets[app] = target{url: raw}
			continue
		}
		parsed, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("failed to parse url for app %s: %w", app, err)
		}
		targets[app] = target{url: gateway + parsed.Path, host: parsed.Host}
	}
	return targets, nil
}

// requestRecord is what one fired request leaves behind
type requestRecord struct {
	atMs      int64
	app       string
	status    int
	latencyMs int64
	sentAt    time.Time
}

// replay fires every invocation at its offset from the replay start and
// writes one CSV line per request as the responses come back. It returns
// how many requests errored or answered with a server error.
func replay(trace *sim.Trace, targets map[string]target, client *http.Client, out io.Writer) (int, error) {
	writer := csv.NewWriter(out)
	defer writer.Flush()
	if err := writer.Write([]string{"at_ms", "app", "status", "latency_ms", "sent_at_unix_ms"}); err != nil {
		return 0, fmt.Errorf("failed to write header: %w", err)
	}

	// responses arrive out of order, so one goroutine per request collects
	// its own record and the writer serialises them under a lock
	var mu sync.Mutex
	var wg sync.WaitGroup
	failed := 0
	var writeErr error
	start := time.Now()
	for _, inv := range trace.Invocations {
		time.Sleep(time.Until(dueAt(start, inv)))
		wg.Add(1)
		go func(inv sim.Invocation) {
			defer wg.Done()
			app := trace.Apps[inv.App]
			record := fire(client, targets[app], inv.AtMs, app)

			mu.Lock()
			defer mu.Unlock()
			if record.status == 0 || record.status >= 500 {
				failed++
			}
			if err := writer.Write(record.fields()); err != nil && writeErr == nil {
				writeErr = fmt.Errorf("failed to write request line: %w", err)
			}
		}(inv)
	}
	wg.Wait()
	return failed, writeErr
}

// fire sends one GET and measures it; a transport error reports status 0
func fire(client *http.Client, to target, atMs int64, app string) requestRecord {
	sent := time.Now()
	status := 0
	req, err := http.NewRequest(http.MethodGet, to.url, nil)
	if err == nil {
		if to.host != "" {
			req.Host = to.host
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
		app:       app,
		status:    status,
		latencyMs: time.Since(sent).Milliseconds(),
		sentAt:    sent,
	}
}

func (r requestRecord) fields() []string {
	return []string{
		strconv.FormatInt(r.atMs, 10),
		r.app,
		strconv.Itoa(r.status),
		strconv.FormatInt(r.latencyMs, 10),
		strconv.FormatInt(r.sentAt.UnixMilli(), 10),
	}
}

// dueAt is the wall-clock moment an invocation fires: its trace offset from
// the replay start
func dueAt(start time.Time, inv sim.Invocation) time.Time {
	return start.Add(time.Duration(inv.AtMs) * time.Millisecond)
}
