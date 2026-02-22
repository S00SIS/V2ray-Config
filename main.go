package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type Settings struct {
	Validation    ValidationSettings `json:"validation"`
	Protocols     []string           `json:"protocols"`
	ProtocolOrder []string           `json:"protocol_order"`
	Base64Links   []string           `json:"base64_links"`
	TextLinks     []string           `json:"text_links"`
	Output        OutputSettings     `json:"output"`
}

type ValidationSettings struct {
	NumWorkers             int      `json:"num_workers"`
	GlobalTimeoutSec       float64  `json:"global_timeout_sec"`
	SingboxStartTimeoutMs  int      `json:"singbox_start_timeout_ms"`
	SingboxStartIntervalMs int      `json:"singbox_start_interval_ms"`
	HTTPRequestTimeoutMs   int      `json:"http_request_timeout_ms"`
	HTTPDialTimeoutMs      int      `json:"http_dial_timeout_ms"`
	HTTPResponseTimeoutMs  int      `json:"http_response_timeout_ms"`
	PortCheckTimeoutMs     int      `json:"port_check_timeout_ms"`
	PostStartDelayMs       int      `json:"post_start_delay_ms"`
	MaxRetries             int      `json:"max_retries"`
	MinPassScore           int      `json:"min_pass_score"`
	BasePort               int      `json:"base_port"`
	TestURLs               []string `json:"test_urls"`
}

type OutputSettings struct {
	ConfigName   string `json:"config_name"`
	MainFile     string `json:"main_file"`
	ProtocolsDir string `json:"protocols_dir"`
}

var cfg Settings
var portPool chan int

var fetchHTTPClient = &http.Client{
	// Timeout is set per-request via context; this is a fallback only.
	Transport: &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       15 * time.Second,
		ResponseHeaderTimeout: 4 * time.Second,
		DisableKeepAlives:     false,
	},
}

type protoStat struct {
	mu         sync.Mutex
	tested     int
	passed     int
	parseFail  int
	startFail  int
	connFail   int
	totalLatMs int64
}

type Logger struct {
	mu         sync.Mutex
	file       *os.File
	buf        *bufio.Writer
	passed     int64
	parseFail  int64
	startFail  int64
	connFail   int64
	totalTest  int64
	protoStats map[string]*protoStat
}

var gLog *Logger

func newLogger(dir string) (*Logger, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	ts := time.Now().Format("2006-01-02_15-04-05")
	f, err := os.Create(filepath.Join(dir, "validation_"+ts+".log"))
	if err != nil {
		return nil, err
	}
	return &Logger{
		file:       f,
		buf:        bufio.NewWriterSize(f, 256*1024),
		protoStats: make(map[string]*protoStat),
	}, nil
}

func (l *Logger) writeLine(s string) {
	l.mu.Lock()
	l.buf.WriteString(s)
	l.buf.WriteByte('\n')
	l.mu.Unlock()
}

func (l *Logger) logStart(fetched, failedSrc int) {
	l.writeLine("==========================================================")
	l.writeLine("  VALIDATION RUN STARTED")
	l.writeLine(fmt.Sprintf("  Time      : %s", time.Now().Format("2006-01-02 15:04:05 MST")))
	l.writeLine(fmt.Sprintf("  Workers   : %d", cfg.Validation.NumWorkers))
	l.writeLine(fmt.Sprintf("  Timeout   : %.0fs per config", cfg.Validation.GlobalTimeoutSec))
	l.writeLine(fmt.Sprintf("  Fetched   : %d  |  FailedSrc: %d", fetched, failedSrc))
	l.writeLine("==========================================================")
	l.writeLine("")
}

func (l *Logger) logProtoStart(proto string, count int) {
	l.mu.Lock()
	if _, ok := l.protoStats[proto]; !ok {
		l.protoStats[proto] = &protoStat{}
	}
	l.mu.Unlock()
	l.writeLine(fmt.Sprintf("--- PROTOCOL: %s (%d unique) ---", strings.ToUpper(proto), count))
}

func (l *Logger) logResult(idx int64, proto, configURL string, res validationResult) {
	l.mu.Lock()
	st := l.protoStats[proto]
	if st == nil {
		st = &protoStat{}
		l.protoStats[proto] = st
	}
	l.mu.Unlock()

	st.mu.Lock()
	st.tested++
	if res.totalScore >= cfg.Validation.MinPassScore {
		st.passed++
		st.totalLatMs += res.latency.Milliseconds()
		atomic.AddInt64(&l.passed, 1)
	} else if strings.HasPrefix(res.failReason, "PARSE:") {
		st.parseFail++
		atomic.AddInt64(&l.parseFail, 1)
	} else if strings.HasPrefix(res.failReason, "SINGBOX_START:") || strings.HasPrefix(res.failReason, "START:") {
		st.startFail++
		atomic.AddInt64(&l.startFail, 1)
	} else {
		st.connFail++
		atomic.AddInt64(&l.connFail, 1)
	}
	atomic.AddInt64(&l.totalTest, 1)
	st.mu.Unlock()

	ts := time.Now().Format("15:04:05.000")
	if res.totalScore >= cfg.Validation.MinPassScore {
		l.writeLine(fmt.Sprintf("[%s] PASS  [%5d] %-6s lat=%dms  %s",
			ts, idx, proto, res.latency.Milliseconds(), truncate(configURL, 120)))
	} else {
		l.writeLine(fmt.Sprintf("[%s] FAIL  [%5d] %-6s %s  |  %s",
			ts, idx, proto, truncate(res.failReason, 80), truncate(configURL, 60)))
	}
}

func (l *Logger) logSummary(duration float64, results []configResult, failedLinks []string) {
	byProto := make(map[string]int)
	for _, r := range results {
		byProto[r.proto]++
	}

	l.writeLine("")
	l.writeLine("==========================================================")
	l.writeLine("  SUMMARY")
	l.writeLine("==========================================================")
	l.writeLine(fmt.Sprintf("  Duration    : %.2fs", duration))
	l.writeLine(fmt.Sprintf("  Total Tested: %d", atomic.LoadInt64(&l.totalTest)))
	l.writeLine(fmt.Sprintf("  Passed      : %d", atomic.LoadInt64(&l.passed)))
	l.writeLine(fmt.Sprintf("  Parse Fail  : %d", atomic.LoadInt64(&l.parseFail)))
	l.writeLine(fmt.Sprintf("  Start Fail  : %d", atomic.LoadInt64(&l.startFail)))
	l.writeLine(fmt.Sprintf("  Conn Fail   : %d", atomic.LoadInt64(&l.connFail)))
	l.writeLine("")
	l.writeLine("  Per-Protocol Breakdown:")
	l.writeLine(fmt.Sprintf("  %-6s  %6s  %6s  %7s  %9s  %9s  %9s  %8s",
		"Proto", "Tested", "Passed", "Pass%", "ParseFail", "StartFail", "ConnFail", "AvgLat"))

	for _, p := range cfg.ProtocolOrder {
		st := l.protoStats[p]
		if st == nil {
			continue
		}
		passRate := 0.0
		avgLat := int64(0)
		if st.tested > 0 {
			passRate = float64(st.passed) / float64(st.tested) * 100
		}
		if st.passed > 0 {
			avgLat = st.totalLatMs / int64(st.passed)
		}
		l.writeLine(fmt.Sprintf("  %-6s  %6d  %6d  %6.1f%%  %9d  %9d  %9d  %7dms",
			p, st.tested, st.passed, passRate, st.parseFail, st.startFail, st.connFail, avgLat))
	}

	tt := atomic.LoadInt64(&l.totalTest)
	if tt > 0 {
		overall := float64(atomic.LoadInt64(&l.passed)) / float64(tt) * 100
		l.writeLine(fmt.Sprintf("\n  Overall pass rate: %.1f%%", overall))
	}

	if len(failedLinks) > 0 {
		l.writeLine("\n  Failed Sources:")
		for _, fl := range failedLinks {
			l.writeLine("    - " + fl)
		}
	}

	l.writeLine("\n  Output Files:")
	for _, p := range cfg.ProtocolOrder {
		if n := byProto[p]; n > 0 {
			l.writeLine(fmt.Sprintf("    %-6s: %d → %s/%s.txt | %s/%s_clash.yaml | %s/%s_clash_advanced.yaml",
				p, n, cfg.Output.ProtocolsDir, p, cfg.Output.ProtocolsDir, p, cfg.Output.ProtocolsDir, p))
		}
	}
	l.writeLine(fmt.Sprintf("  Total  : %d → %s | clash.yaml | clash_advanced.yaml", len(results), cfg.Output.MainFile))
	l.writeLine("==========================================================")
}

func (l *Logger) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buf != nil {
		l.buf.Flush()
	}
	if l.file != nil {
		l.file.Close()
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

type clashBase struct {
	simple   string
	advanced string
}

var gClash clashBase

var gInputByProto = make(map[string]int)

func loadClashBase(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("clash_base.yaml: %w", err)
	}
	gClash.simple = string(data)
	return nil
}

func loadClashBaseAdvanced(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("clash_base_advanced.yaml: %w", err)
	}
	gClash.advanced = string(data)
	return nil
}

func injectClashProxies(baseContent string, proxyEntries []string, proxyNames []string) string {
	const proxiesPlaceholder = "# ---PROXIES---\n"
	const namesPlaceholder = "# ---PROXY-NAMES---\n"

	var proxyBlock strings.Builder
	for _, e := range proxyEntries {
		proxyBlock.WriteString(e)
	}

	var namesBlock strings.Builder
	for _, n := range proxyNames {
		fmt.Fprintf(&namesBlock, "      - %s\n", yamlQuote(n))
	}

	result := strings.ReplaceAll(baseContent, proxiesPlaceholder, proxyBlock.String())
	result = strings.ReplaceAll(result, namesPlaceholder, namesBlock.String())
	return result
}

func loadSettings(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &cfg)
}

func initPortPool() {
	v := cfg.Validation
	size := v.NumWorkers + 10
	portPool = make(chan int, size)
	for i := 0; i < size; i++ {
		portPool <- v.BasePort + i
	}
}

type fetchResult struct {
	url        string
	content    string
	statusCode int
	err        error
}

type validationResult struct {
	totalScore int
	latency    time.Duration
	failReason string
}

type configResult struct {
	line    string
	proto   string
	latency time.Duration
}

func main() {
	if err := loadSettings("settings.json"); err != nil {
		fmt.Printf("❌ Failed to load settings.json: %v\n", err)
		os.Exit(1)
	}

	if err := loadClashBase("clash_base.yaml"); err != nil {
		fmt.Printf("⚠️  clash_base.yaml: %v\n", err)
	}
	if err := loadClashBaseAdvanced("clash_base_advanced.yaml"); err != nil {
		fmt.Printf("⚠️  clash_base_advanced.yaml: %v\n", err)
	}

	var logErr error
	gLog, logErr = newLogger("logs")
	if logErr != nil {
		fmt.Printf("⚠️  Log file error: %v\n", logErr)
	}
	if gLog != nil {
		defer gLog.close()
	}

	initPortPool()

	start := time.Now()
	v := cfg.Validation
	fmt.Println("🚀 Starting V2Ray config aggregator...")
	fmt.Printf("⚙️  Workers=%d | Timeout=%.0fs | Retries=%d\n",
		v.NumWorkers, v.GlobalTimeoutSec, v.MaxRetries)

	if err := prepareOutputDirs(); err != nil {
		fmt.Printf("❌ Error creating directories: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("📡 Fetching configurations from sources...")
	allConfigs, failedLinks := fetchAll(cfg.Base64Links, cfg.TextLinks)
	fmt.Printf("📊 Total fetched: %d | Failed sources: %d\n", len(allConfigs), len(failedLinks))

	if gLog != nil {
		gLog.logStart(len(allConfigs), len(failedLinks))
	}

	fmt.Println("🔍 Validating...")
	results := validateAll(allConfigs)

	elapsed := time.Since(start).Seconds()
	fmt.Printf("\n✅ Valid configurations: %d\n", len(results))

	if gLog != nil {
		gLog.logSummary(elapsed, results, failedLinks)
	}

	writeOutputFiles(results)
	writeSummary(results, failedLinks, elapsed, len(allConfigs))
	fmt.Println("✅ Done!")
}

func prepareOutputDirs() error {
	os.RemoveAll("config")
	dirs := []string{
		"config",
		cfg.Output.ProtocolsDir,
		"config/batches/v2ray",
		"config/batches/clash",
		"config/batches/clash_advanced",
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	return nil
}

// fetchAll fetches all URLs in parallel batches of 20 with a 5s timeout each.
// base64Links are decoded after fetching; textLinks are returned as-is.
func fetchAll(base64Links, textLinks []string) ([]string, []string) {
	const batchSize    = 20
	const fetchTimeout = 5 * time.Second

	type urlJob struct {
		url      string
		isBase64 bool
	}

	var jobs []urlJob
	for _, u := range base64Links {
		jobs = append(jobs, urlJob{u, true})
	}
	for _, u := range textLinks {
		jobs = append(jobs, urlJob{u, false})
	}

	total := len(jobs)
	numBatches := (total + batchSize - 1) / batchSize
	fmt.Printf("📡 Fetching %d sources in %d batches of %d (timeout=%s per source)\n",
		total, numBatches, batchSize, fetchTimeout)

	var mu sync.Mutex
	var lines []string
	var failed []string
	var okCount, failCount int

	for batchIdx := 0; batchIdx < numBatches; batchIdx++ {
		start := batchIdx * batchSize
		end := start + batchSize
		if end > total {
			end = total
		}
		batch := jobs[start:end]

		var wg sync.WaitGroup
		results := make([]fetchResult, len(batch))

		for i, job := range batch {
			wg.Add(1)
			go func(idx int, j urlJob) {
				defer wg.Done()
				results[idx] = fetchRaw(j.url, fetchTimeout)
				if results[idx].err == nil && results[idx].statusCode == http.StatusOK && j.isBase64 {
					decoded, err := decodeBase64([]byte(results[idx].content))
					if err != nil {
						results[idx].err = err
					} else {
						results[idx].content = decoded
					}
				}
			}(i, job)
		}
		wg.Wait()

		mu.Lock()
		for _, r := range results {
			if r.err != nil || r.statusCode != http.StatusOK {
				status := "error"
				if r.statusCode > 0 {
					status = fmt.Sprintf("HTTP %d", r.statusCode)
				}
				failCount++
				failed = append(failed, fmt.Sprintf("%s (%s)", r.url, status))
				if gLog != nil {
					gLog.writeLine(fmt.Sprintf("[FETCH] FAIL  %s  status=%s", r.url, status))
				}
				continue
			}
			okCount++
			if gLog != nil {
				gLog.writeLine(fmt.Sprintf("[FETCH] OK    %s", r.url))
			}
			lines = append(lines, strings.Split(strings.TrimSpace(r.content), "\n")...)
		}
		mu.Unlock()

		fmt.Printf("  batch %3d/%d  ok=%-4d fail=%-4d  total_lines=%d\n",
			batchIdx+1, numBatches, okCount, failCount, len(lines))
	}
	return lines, failed
}

func fetchRaw(rawURL string, timeout time.Duration) fetchResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return fetchResult{url: rawURL, err: err}
	}
	resp, err := fetchHTTPClient.Do(req)
	if err != nil {
		return fetchResult{url: rawURL, err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fetchResult{url: rawURL, statusCode: resp.StatusCode, err: err}
	}
	return fetchResult{url: rawURL, statusCode: resp.StatusCode, content: string(body)}
}

func isProtocolSupported(proto string) bool {
	for _, p := range cfg.Protocols {
		if p == proto {
			return true
		}
	}
	return false
}

// failDetail holds per-protocol failure reason counts.
type failDetail struct {
	mu      sync.Mutex
	reasons map[string]int
}

func validateAll(lines []string) []configResult {
	seen := make(map[string]bool)
	byProto := make(map[string][]string)
	duplicates := 0

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, proto := range cfg.Protocols {
			if !isProtocolSupported(proto) {
				continue
			}
			if strings.HasPrefix(line, proto+"://") {
				id := coreIdentity(line, proto)
				if !seen[id] {
					seen[id] = true
					byProto[proto] = append(byProto[proto], line)
				} else {
					duplicates++
				}
				break
			}
		}
	}

	for p, lines := range byProto {
		gInputByProto[p] = len(lines)
	}

	if gLog != nil {
		gLog.writeLine(fmt.Sprintf("[DEDUP] removed=%d duplicates", duplicates))
		total := 0
		for _, p := range cfg.ProtocolOrder {
			n := len(byProto[p])
			total += n
			if n > 0 {
				gLog.writeLine(fmt.Sprintf("[DEDUP] %-6s unique=%d", p, n))
			}
		}
		gLog.writeLine(fmt.Sprintf("[DEDUP] total unique=%d", total))
		gLog.writeLine("")
	}


	// ── Per-protocol failure detail tracking ──────────────────────────────────
	protoFails := make(map[string]*failDetail)
	for _, p := range cfg.ProtocolOrder {
		protoFails[p] = &failDetail{reasons: make(map[string]int)}
	}

	// normalizeReason classifies a raw failReason into a precise, groupable key.
	// Unknown/rare reasons fall into the "OTHER" bucket.
	normalizeReason := func(reason string) string {
		return classifyFailReason(reason)
	}

	resultsCh := make(chan configResult, cfg.Validation.NumWorkers*4)
	sem := make(chan struct{}, cfg.Validation.NumWorkers)

	var testedCount int64
	var passedCount int64
	var failedParse int64
	var failedStart int64
	var failedConn int64

	// ── Sequential per-protocol processing ────────────────────────────────────
	// Each protocol is fully completed before the next one starts.
	// Within each protocol, workers run in parallel up to NumWorkers.
	go func() {
		for _, proto := range cfg.ProtocolOrder {
			protoLines := byProto[proto]
			if len(protoLines) == 0 {
				continue
			}
			if gLog != nil {
				gLog.logProtoStart(proto, len(protoLines))
			}
			protoStart := time.Now()
			fmt.Printf("\n🔵 [%s] Starting — %d configs\n", strings.ToUpper(proto), len(protoLines))

			var wg sync.WaitGroup
			var protoPassed int64
			var protoFailed int64

			for _, line := range protoLines {
				l := line
				wg.Add(1)
				sem <- struct{}{}
				go func() {
					defer wg.Done()
					defer func() { <-sem }()

					idx := atomic.AddInt64(&testedCount, 1)
					res := validate(l, proto)

					if gLog != nil {
						gLog.logResult(idx, proto, l, res)
					}

					if res.totalScore >= cfg.Validation.MinPassScore {
						atomic.AddInt64(&passedCount, 1)
						atomic.AddInt64(&protoPassed, 1)
						resultsCh <- configResult{line: l, proto: proto, latency: res.latency}
					} else {
						atomic.AddInt64(&protoFailed, 1)
						reason := res.failReason
						norm := normalizeReason(reason)
						fd := protoFails[proto]
						fd.mu.Lock()
						fd.reasons[norm]++
						fd.mu.Unlock()

						if strings.HasPrefix(reason, "PARSE:") {
							atomic.AddInt64(&failedParse, 1)
						} else if strings.HasPrefix(reason, "SINGBOX_START:") || strings.HasPrefix(reason, "START:") {
							atomic.AddInt64(&failedStart, 1)
						} else {
							atomic.AddInt64(&failedConn, 1)
						}
					}
				}()
			}
			wg.Wait()

			elapsed := time.Since(protoStart).Seconds()
			total := int64(len(protoLines))
			passRate := float64(protoPassed) / float64(total) * 100
			fmt.Printf("✅ [%s] Done — passed=%d/%d (%.1f%%) in %.1fs\n",
				strings.ToUpper(proto), protoPassed, total, passRate, elapsed)
		}
		close(resultsCh)
	}()

	var out []configResult
	for r := range resultsCh {
		out = append(out, r)
	}

	// ── Global summary line ────────────────────────────────────────────────────
	fmt.Printf("\n📊 Tested=%d | Passed=%d | ParseFail=%d | StartFail=%d | ConnFail=%d\n",
		atomic.LoadInt64(&testedCount),
		atomic.LoadInt64(&passedCount),
		atomic.LoadInt64(&failedParse),
		atomic.LoadInt64(&failedStart),
		atomic.LoadInt64(&failedConn))

	// ── Detailed per-protocol failure report ─────────────────────────────────
	printFailureReport(protoFails, byProto)

	return out
}

// printFailureReport prints a detailed statistical breakdown of failures per protocol.
// classifyFailReason maps a raw fail reason to a precise, groupable category key.
func classifyFailReason(reason string) string {
	stripANSI := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r == 0x1b { return -1 }
			return r
		}, s)
	}
	r := stripANSI(reason)

	switch {
	// ── PARSE failures ───────────────────────────────────────────────────────
	case strings.HasPrefix(r, "PARSE: base64:"):
		return "PARSE › base64 decode error"
	case strings.HasPrefix(r, "PARSE: json:"):
		return "PARSE › json decode error"
	case strings.HasPrefix(r, "PARSE: url parse:"):
		return "PARSE › url parse error"
	case strings.HasPrefix(r, "PARSE: unsupported cipher:"):
		return "PARSE › unsupported SS cipher"
	case r == "PARSE: missing @" || r == "PARSE: missing server" ||
		r == "PARSE: missing uuid" || r == "PARSE: missing password" ||
		r == "PARSE: missing port" || r == "PARSE: missing auth":
		return "PARSE › " + strings.TrimPrefix(r, "PARSE: ")
	case strings.HasPrefix(r, "PARSE: port:"):
		return "PARSE › invalid port value"
	case strings.HasPrefix(r, "PARSE: reality:"):
		return "PARSE › reality missing public key"
	case strings.HasPrefix(r, "PARSE: unknown security:"):
		return "PARSE › unknown security type"
	case strings.HasPrefix(r, "PARSE:"):
		msg := strings.TrimPrefix(r, "PARSE: ")
		if len(msg) > 48 { msg = msg[:48] + "…" }
		return "PARSE › " + msg

	// ── SINGBOX_START / START failures ───────────────────────────────────────
	case strings.HasPrefix(r, "SINGBOX_START:"), strings.HasPrefix(r, "START:"):
		body := r
		if i := strings.Index(body, ": "); i != -1 { body = body[i+2:] }
		switch {
		case strings.Contains(body, "port not open"):
			return "START › port timeout (sing-box didn't listen)"
		case strings.Contains(body, "decode config"), strings.Contains(body, "outbound"):
			if strings.Contains(body, "flow") {
				return "START › invalid flow (requires TLS)"
			}
			return "START › invalid config JSON (sing-box rejected)"
		case strings.Contains(body, "address already in use"):
			return "START › port already in use"
		case strings.Contains(body, "no such file"), strings.Contains(body, "not found"):
			return "START › sing-box binary not found"
		case strings.Contains(body, "permission denied"):
			return "START › permission denied"
		case strings.Contains(body, "method"):
			return "START › unsupported SS method"
		default:
			if len(body) > 55 { body = body[:55] + "…" }
			return "START › " + body
		}

	// ── CONN failures ────────────────────────────────────────────────────────
	case strings.HasPrefix(r, "CONN:"):
		body := strings.TrimPrefix(r, "CONN: ")
		if i := strings.Index(body, " | SINGBOX:"); i != -1 { body = body[:i] }
		switch {
		case strings.Contains(body, "context deadline exceeded"), strings.Contains(body, "context canceled"):
			return "CONN › request timed out (no response from proxy)"
		case strings.Contains(body, "connection refused"):
			return "CONN › connection refused (proxy died)"
		case strings.Contains(body, "EOF"):
			return "CONN › EOF (proxy closed connection)"
		case strings.Contains(body, "no such host"), strings.Contains(body, "lookup"):
			return "CONN › DNS resolution failed"
		case strings.Contains(body, "i/o timeout"):
			return "CONN › i/o timeout"
		case strings.Contains(body, "tls:"), strings.Contains(body, "TLS"), strings.Contains(body, "certificate"):
			return "CONN › TLS handshake failed"
		case body == "HTTP_502":
			return "CONN › HTTP 502 (proxy server rejected CONNECT)"
		case body == "HTTP_501":
			return "CONN › HTTP 501 (proxy server doesn't support CONNECT)"
		case strings.Contains(body, "HTTP_"):
			return "CONN › unexpected HTTP status: " + body
		case strings.Contains(body, "proxyconnect"):
			return "CONN › proxy CONNECT failed"
		case strings.Contains(body, "context expired"):
			return "CONN › test URL timed out (proxy dead or unreachable)"
		default:
			if len(body) > 55 { body = body[:55] + "…" }
			return "CONN › " + body
		}

	// ── FILE / internal errors ────────────────────────────────────────────────
	case strings.HasPrefix(r, "FILE:"):
		return "OTHER › temp file error"
	case r == "unknown":
		return "OTHER › unknown (no reason captured)"
	default:
		if len(r) > 55 { r = r[:55] + "…" }
		return "OTHER › " + r
	}
}

func printFailureReport(protoFails map[string]*failDetail, byProto map[string][]string) {
	type kv struct{ key string; val int }

	const W = 78 // total report width

	hr := func(ch string) { fmt.Println(strings.Repeat(ch, W)) }

	fmt.Println()
	hr("═")
	title := "  FAILURE ANALYSIS REPORT"
	fmt.Printf("%-*s%s\n", W-len(title)-1, title, "")
	fmt.Printf("  %-*s\n", W-3, "Detailed breakdown of why each config failed, grouped by root cause.")
	hr("═")

	// Gather overall stats for the global summary table
	type protoRow struct {
		name      string
		total     int
		passed    int
		parseFail int
		startFail int
		connFail  int
		otherFail int
	}
	var rows []protoRow

	for _, proto := range cfg.ProtocolOrder {
		fd := protoFails[proto]
		if fd == nil { continue }
		total := len(byProto[proto])
		if total == 0 { continue }

		var pf, sf, cf, of int
		for key, cnt := range fd.reasons {
			switch {
			case strings.HasPrefix(key, "PARSE"):
				pf += cnt
			case strings.HasPrefix(key, "START"):
				sf += cnt
			case strings.HasPrefix(key, "CONN"):
				cf += cnt
			default:
				of += cnt
			}
		}
		totalFail := pf + sf + cf + of
		rows = append(rows, protoRow{proto, total, total - totalFail, pf, sf, cf, of})
	}

	// ── Global summary table ─────────────────────────────────────────────────
	fmt.Println()
	fmt.Printf("  %-7s %7s %7s %6s  %9s %9s %9s %8s  %s\n",
		"PROTO", "TOTAL", "PASSED", "PASS%", "PARSE✗", "START✗", "CONN✗", "OTHER✗", "PASS-RATE BAR")
	fmt.Println("  " + strings.Repeat("─", W-2))
	for _, row := range rows {
		passRate := float64(row.passed) / float64(row.total) * 100
		barLen   := int(passRate / 5)
		bar      := strings.Repeat("▓", barLen) + strings.Repeat("░", 20-barLen)
		fmt.Printf("  %-7s %7d %7d %5.1f%%  %9d %9d %9d %8d  %s\n",
			strings.ToUpper(row.name),
			row.total, row.passed, passRate,
			row.parseFail, row.startFail, row.connFail, row.otherFail,
			bar)
	}
	fmt.Println()

	// ── Per-protocol detail ──────────────────────────────────────────────────
	for _, proto := range cfg.ProtocolOrder {
		fd := protoFails[proto]
		if fd == nil { continue }
		total := len(byProto[proto])
		if total == 0 { continue }

		totalFails := 0
		for _, c := range fd.reasons { totalFails += c }
		passed   := total - totalFails
		passRate := float64(passed) / float64(total) * 100

		// Section header
		fmt.Printf("┌─ %-6s ─────────────────────────────────────────────────────────────\n",
			strings.ToUpper(proto))
		fmt.Printf("│  Total: %-6d  Passed: %-6d  Failed: %-6d  Pass rate: %.1f%%\n",
			total, passed, totalFails, passRate)

		if totalFails == 0 {
			fmt.Println("│  ✓ No failures recorded.")
			fmt.Println("└" + strings.Repeat("─", W-1))
			continue
		}

		// Group into PARSE / START / CONN / OTHER sub-sections
		sections := []struct{ prefix, label string }{
			{"PARSE", "Parse Failures  (config could not be decoded/interpreted)"},
			{"START", "Start Failures  (sing-box refused or couldn't start)"},
			{"CONN",  "Conn Failures   (proxy started but connection failed)"},
			{"OTHER", "Other / Unknown"},
		}

		for _, sec := range sections {
			var items []kv
			secTotal := 0
			for k, v := range fd.reasons {
				if strings.HasPrefix(k, sec.prefix) {
					items = append(items, kv{k, v})
					secTotal += v
				}
			}
			if len(items) == 0 { continue }
			sort.Slice(items, func(i, j int) bool { return items[i].val > items[j].val })

			secPct := float64(secTotal) / float64(totalFails) * 100
			fmt.Printf("│\n│  ▶ %s\n", sec.label)
			fmt.Printf("│    Sub-total: %d configs (%.1f%% of all failures)\n", secTotal, secPct)
			fmt.Printf("│    %-52s %7s  %6s  %s\n", "Reason", "Count", "of-sec", "Bar")
			fmt.Printf("│    %s\n", strings.Repeat("·", 72))

			for _, item := range items {
				pct    := float64(item.val) / float64(secTotal) * 100
				barLen := int(pct / 5)
				if barLen > 20 { barLen = 20 }
				bar := strings.Repeat("█", barLen)

				// Strip the prefix from the display key (e.g. "PARSE › " -> shown under PARSE section)
				displayKey := item.key
				if i := strings.Index(displayKey, " › "); i != -1 {
					displayKey = displayKey[i+3:]
				}
				if len(displayKey) > 51 { displayKey = displayKey[:51] + "…" }

				fmt.Printf("│    %-52s %7d  %5.1f%%  %s\n",
					displayKey, item.val, pct, bar)
			}
		}

		fmt.Println("└" + strings.Repeat("─", W-1))
	}

	// ── Overall totals ───────────────────────────────────────────────────────
	var grandTotal, grandPassed, grandFail int
	for _, row := range rows {
		grandTotal  += row.total
		grandPassed += row.passed
		grandFail   += row.total - row.passed
	}
	fmt.Println()
	hr("═")
	fmt.Printf("  OVERALL  Total=%-7d  Passed=%-7d  Failed=%-7d  Pass rate=%.1f%%\n",
		grandTotal, grandPassed, grandFail,
		float64(grandPassed)/float64(grandTotal)*100)
	hr("═")
	fmt.Println()
}

func validate(configURL, protocol string) validationResult {
	result := validationResult{failReason: "unknown"}

	outboundJSON, parseErr := toSingBoxOutbound(configURL, protocol)
	if parseErr != "" {
		result.failReason = "PARSE: " + parseErr
		return result
	}

	port := <-portPool
	defer func() { portPool <- port }()

	v := cfg.Validation
	fullConfig := buildSingBoxConfig(outboundJSON, port)

	configFile, err := os.CreateTemp("", "sb-*.json")
	if err != nil {
		result.failReason = "FILE: " + err.Error()
		return result
	}
	configPath := configFile.Name()
	configFile.Close()

	if err := os.WriteFile(configPath, []byte(fullConfig), 0644); err != nil {
		os.Remove(configPath)
		result.failReason = "FILE: " + err.Error()
		return result
	}
	defer os.Remove(configPath)

	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(float64(time.Second)*(v.GlobalTimeoutSec+2)))
	defer cancel()

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, singBoxPath(), "run", "-c", configPath)
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		result.failReason = "START: " + err.Error()
		return result
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	started := waitForPort(addr,
		time.Duration(v.SingboxStartTimeoutMs)*time.Millisecond,
		time.Duration(v.SingboxStartIntervalMs)*time.Millisecond,
		time.Duration(v.PortCheckTimeoutMs)*time.Millisecond,
	)

	if !started {
		killGroup(cmd)
		sbErr := extractErr(stderr.String())
		if sbErr == "" {
			sbErr = fmt.Sprintf("port not open after %dms (ctx=%v)", v.SingboxStartTimeoutMs, ctx.Err())
		}
		result.failReason = "SINGBOX_START: " + sbErr
		return result
	}

	time.Sleep(time.Duration(v.PostStartDelayMs) * time.Millisecond)

	proxyURL, _ := url.Parse("http://" + addr)
	client := &http.Client{
		Timeout: time.Duration(v.HTTPRequestTimeoutMs) * time.Millisecond,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
			DialContext: (&net.Dialer{
				Timeout:   time.Duration(v.HTTPDialTimeoutMs) * time.Millisecond,
				KeepAlive: 0,
			}).DialContext,
			MaxIdleConns:          1,
			MaxIdleConnsPerHost:   1,
			DisableKeepAlives:     true,
			ResponseHeaderTimeout: time.Duration(v.HTTPResponseTimeoutMs) * time.Millisecond,
		},
	}

	success, latency, httpErr := tryHTTP(ctx, client, v.TestURLs, v.MaxRetries)
	killGroup(cmd)

	if success {
		result.totalScore = 4
		result.latency = latency
	} else {
		sbErr := extractErr(stderr.String())
		if sbErr != "" {
			result.failReason = "CONN: " + httpErr + " | SINGBOX: " + sbErr
		} else {
			result.failReason = "CONN: " + httpErr
		}
	}
	return result
}

func waitForPort(addr string, maxWait, interval, dialTimeout time.Duration) bool {
	elapsed := time.Duration(0)
	for elapsed < maxWait {
		time.Sleep(interval)
		elapsed += interval
		conn, err := net.DialTimeout("tcp", addr, dialTimeout)
		if err == nil {
			conn.Close()
			return true
		}
	}
	return false
}

func tryHTTP(ctx context.Context, client *http.Client, testURLs []string, maxRetries int) (bool, time.Duration, string) {
	// For proxy validation, HTTPS (CONNECT tunnel) must be used.
	// Plain HTTP through proxy uses HTTP-forward mode which sing-box proxies
	// don't support and return 502. Auto-upgrade any http:// → https://.
	// Replace http:// with https:// — plain HTTP through proxy uses HTTP-forward
	// mode which sing-box doesn't support (returns 502). HTTPS uses CONNECT tunnel.
	// Do NOT keep the http:// version: trying both wastes time and causes
	// "context expired" on the second URL when the HTTPS attempt times out.
	effectiveURLs := make([]string, 0, len(testURLs))
	seen := make(map[string]bool)
	for _, u := range testURLs {
		if strings.HasPrefix(u, "http://") {
			u = "https://" + u[len("http://"):]
		}
		if !seen[u] {
			effectiveURLs = append(effectiveURLs, u)
			seen[u] = true
		}
	}
	var lastErr string
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if ctx.Err() != nil {
			return false, 0, "context expired"
		}
		for _, testURL := range effectiveURLs {
			if ctx.Err() != nil {
				return false, 0, "context expired"
			}
			start := time.Now()
			req, err := http.NewRequestWithContext(ctx, "GET", testURL, nil)
			if err != nil {
				lastErr = err.Error()
				continue
			}
			resp, err := client.Do(req)
			if err != nil {
				// Network/tunnel error = proxy is dead or unreachable
				lastErr = shortenErr(err.Error())
				continue
			}
			latency := time.Since(start)
			code := resp.StatusCode
			resp.Body.Close()

			// With HTTPS (CONNECT tunnel): reaching here means:
			// 1) proxy tunnel was established successfully
			// 2) TLS with the target completed
			// 3) HTTP response came from the TARGET, not the proxy
			// => any of these codes = proxy is alive

			// 200/204: ideal - target fully reachable
			if code == 200 || code == 204 {
				return true, latency, ""
			}
			// 3xx redirects: target responded, proxy works
			if code == 301 || code == 302 || code == 307 || code == 308 {
				return true, latency, ""
			}
			// 400/403/404/429: target rejected our IP/request but proxy tunnel works
			if code == 400 || code == 403 || code == 404 || code == 429 {
				return true, latency, ""
			}
			// 5xx and anything else: ambiguous (could be proxy-level error), treat as fail
			lastErr = fmt.Sprintf("HTTP_%d", code)
		}
	}
	return false, 0, lastErr
}

func buildSingBoxConfig(outboundJSON string, port int) string {
	return fmt.Sprintf(`{"log":{"level":"error","timestamp":false},"dns":{"servers":[{"tag":"dns-remote","address":"https://1.1.1.1/dns-query","address_resolver":"dns-direct","strategy":"prefer_ipv4","detour":"proxy"},{"tag":"dns-direct","address":"223.5.5.5","strategy":"prefer_ipv4","detour":"direct"}],"rules":[{"outbound":"any","server":"dns-direct"}],"independent_cache":true},"inbounds":[{"type":"http","tag":"http-in","listen":"127.0.0.1","listen_port":%d}],"outbounds":[%s,{"type":"direct","tag":"direct"},{"type":"block","tag":"block"}]}`,
		port, outboundJSON)
}

func toSingBoxOutbound(configURL, protocol string) (string, string) {
	switch protocol {
	case "vmess":
		return parseVMess(configURL)
	case "vless":
		return parseVLess(configURL)
	case "trojan":
		return parseTrojan(configURL)
	case "ss":
		return parseShadowsocks(configURL)
	case "hy2":
		return parseHysteria2(configURL)
	case "hy":
		return parseHysteria(configURL)
	case "tuic":
		return parseTUIC(configURL)
	}
	return "", "unsupported protocol: " + protocol
}

func sanitizeProxyURL(raw string) string {
	// Strip spaces and control characters that break URL parsing
	raw = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, raw)
	schemeIdx := strings.Index(raw, "://")
	if schemeIdx == -1 {
		return raw
	}
	scheme := raw[:schemeIdx+3]
	rest := raw[schemeIdx+3:]
	frag := ""
	if fragIdx := strings.LastIndex(rest, "#"); fragIdx != -1 {
		frag = rest[fragIdx:]
		rest = rest[:fragIdx]
	}
	query := ""
	if queryIdx := strings.Index(rest, "?"); queryIdx != -1 {
		query = rest[queryIdx:]
		rest = rest[:queryIdx]
	}
	lastAt := strings.LastIndex(rest, "@")
	if lastAt == -1 {
		return scheme + rest + query + frag
	}
	return scheme + encodeUserInfo(rest[:lastAt]) + "@" + rest[lastAt+1:] + query + frag
}

func encodeUserInfo(s string) string {
	var buf strings.Builder
	for i := 0; i < len(s); i++ {
		b := s[i]
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') ||
			b == '-' || b == '.' || b == '_' || b == '~' || b == '!' || b == '$' ||
			b == '&' || b == '\'' || b == '(' || b == ')' || b == '*' || b == '+' ||
			b == ',' || b == ';' || b == '=' || b == ':' {
			buf.WriteByte(b)
		} else {
			fmt.Fprintf(&buf, "%%%02X", b)
		}
	}
	return buf.String()
}

// parseVMessURItoD parses vmess://uuid@host:port?params into a map
// compatible with the base64-JSON path so the rendering code is shared.
func parseVMessURItoD(data string) (map[string]interface{}, string) {
	u, err := url.Parse("vmess://" + data)
	if err != nil {
		return nil, "uri parse: " + err.Error()
	}
	uuid := u.User.Username()
	if uuid == "" {
		return nil, "missing uuid"
	}
	host := u.Hostname()
	if host == "" {
		return nil, "missing server"
	}
	portStr := u.Port()
	if portStr == "" {
		portStr = "443"
	}
	q := u.Query()
	sec := strings.ToLower(q.Get("security"))
	tlsVal := ""
	if sec == "tls" || sec == "xtls" {
		tlsVal = "tls"
	}
	d := map[string]interface{}{
		"id": uuid, "add": host, "port": portStr,
		"aid": first(q.Get("aid"), q.Get("alterId"), "0"),
		"scy": first(q.Get("encryption"), q.Get("scy"), "auto"),
		"net": first(q.Get("type"), q.Get("net"), "tcp"),
		"tls": tlsVal,
		"sni": first(q.Get("sni"), q.Get("peer"), host),
		"path": q.Get("path"),
		"host": q.Get("host"),
		"serviceName": q.Get("serviceName"),
		"fp": q.Get("fp"),
	}
	return d, ""
}

func parseVMess(raw string) (string, string) {
	data := strings.TrimPrefix(raw, "vmess://")
	// Strip fragment
	if idx := strings.LastIndex(data, "#"); idx != -1 {
		data = data[:idx]
	}
	data = strings.TrimSpace(data)

	// Detection order:
	//  1. Raw JSON  (vmess://{...})
	//  2. Base64 JSON  (most common: vmess://eyJ...)
	//  3. URI format  (vmess://uuid@host:port?...) - only if above fail

	var d map[string]interface{}

	if strings.HasPrefix(data, "{") {
		// Raw JSON
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			return "", "json: " + err.Error()
		}
	} else {
		// Try base64 candidates in order:
		//  1. Full data          (normal: vmess://eyJ...)
		//  2. data[:lastAtIdx]   (channel-suffix: vmess://eyJ...==@ChannelName)
		// For each candidate, try base64 → JSON. If both fail, fall to URI.
		var tryB64 []string
		tryB64 = append(tryB64, data)
		if lastAt := strings.LastIndex(data, "@"); lastAt > 0 {
			tryB64 = append(tryB64, data[:lastAt])
		}
		// Also try stripping from the first non-base64 character
		// (handles: eyJ...==@name, eyJ...== name, etc.)
		{
			clean := data
			for i, c := range data {
				if c != '+' && c != '/' && c != '=' &&
					c != '-' && c != '_' &&
					!(c >= 'A' && c <= 'Z') &&
					!(c >= 'a' && c <= 'z') &&
					!(c >= '0' && c <= '9') {
					clean = data[:i]
					break
				}
			}
			if clean != data && clean != "" {
				tryB64 = append(tryB64, clean)
			}
		}

		var parsed bool
		var b64Err error
		for _, candidate := range tryB64 {
			var decoded string
			decoded, b64Err = decodeBase64([]byte(candidate))
			if b64Err != nil {
				continue
			}
			var tmp map[string]interface{}
			if json.Unmarshal([]byte(decoded), &tmp) == nil {
				d = tmp
				parsed = true
				break
			}
		}
		if !parsed {
			// Fall back to URI format (vmess://uuid@host:port?params)
			atIdx := strings.Index(data, "@")
			qIdx := strings.Index(data, "?")
			if atIdx != -1 && (qIdx == -1 || atIdx < qIdx) {
				var parseErr string
				d, parseErr = parseVMessURItoD(data)
				if parseErr != "" {
					return "", parseErr
				}
			} else {
				// Report root cause
				if b64Err != nil {
					return "", "base64: " + b64Err.Error()
				}
				return "", "json: invalid vmess payload"
			}
		}
	}
	server := strings.TrimSpace(fmt.Sprintf("%v", d["add"]))
	if server == "" {
		return "", "missing server"
	}
	port, err := toPort(fmt.Sprintf("%v", d["port"]))
	if err != nil {
		return "", "port: " + err.Error()
	}
	uuid := strings.TrimSpace(fmt.Sprintf("%v", d["id"]))
	if uuid == "" {
		return "", "missing uuid"
	}
	alterId := 0
	if v, ok := d["aid"]; ok {
		switch x := v.(type) {
		case float64:
			alterId = int(x)
		case string:
			alterId, _ = strconv.Atoi(x)
		}
	}
	security := "auto"
	if s, _ := d["scy"].(string); s != "" {
		security = s
	}
	network := "tcp"
	if n, _ := d["net"].(string); n != "" {
		network = n
	}
	tls := ""
	if tlsVal, _ := d["tls"].(string); tlsVal == "tls" {
		sni := server
		if s, _ := d["sni"].(string); s != "" {
			sni = s
		} else if h, _ := d["host"].(string); h != "" {
			sni = h
		}
		tls = fmt.Sprintf(`,"tls":{"enabled":true,"insecure":true,"server_name":%q}`, sni)
	}
	return fmt.Sprintf(`{"type":"vmess","tag":"proxy","server":%q,"server_port":%d,"uuid":%q,"security":%q,"alter_id":%d%s%s}`,
		server, port, uuid, security, alterId, tls, vmessTransport(d, network)), ""
}

func vmessTransport(d map[string]interface{}, network string) string {
	path := strDefault(d["path"], "/")
	host := strDefault(d["host"], "")
	svcName := strDefault(d["serviceName"], strDefault(d["path"], ""))
	return buildTransportJSON(network, path, host, svcName)
}

// singboxSupportedFlows contains vless flow values supported by sing-box.
// Others (xtls-rprx-direct, xtls-rprx-splice, etc.) cause FATAL JSON decode errors.
var singboxSupportedFlows = map[string]bool{
	"":                    true,
	"xtls-rprx-vision":   true,
	"xtls-rprx-vision-udp443": true,
}

func parseVLess(raw string) (string, string) {
	u, err := url.Parse(sanitizeProxyURL(raw))
	if err != nil {
		return "", "url parse: " + err.Error()
	}
	uuid := u.User.Username()
	if uuid == "" {
		return "", "missing uuid"
	}
	server := u.Hostname()
	if server == "" {
		return "", "missing server"
	}
	port, err := toPort(u.Port())
	if err != nil {
		return "", "port: " + err.Error()
	}
	q := u.Query()
	security := strings.ToLower(q.Get("security"))
	network := strings.ToLower(q.Get("type"))
	if network == "" {
		network = "tcp"
	}
	sni := first(q.Get("sni"), q.Get("peer"), server)
	// Filter flow: sing-box only supports xtls-rprx-vision; others cause FATAL config errors
	flow := q.Get("flow")
	if !singboxSupportedFlows[flow] {
		flow = ""
	}
	tlsJSON, tlsErr := vlessTLS(security, sni, flow, q)
	if tlsErr != "" {
		return "", tlsErr
	}
	transport := buildTransportJSON(network, first(q.Get("path"), "/"), q.Get("host"),
		first(q.Get("serviceName"), q.Get("path")))
	return fmt.Sprintf(`{"type":"vless","tag":"proxy","server":%q,"server_port":%d,"uuid":%q%s%s}`,
		server, port, uuid, tlsJSON, transport), ""
}

func vlessTLS(security, sni, flow string, q url.Values) (string, string) {
	flowJSON := ""
	if flow != "" {
		flowJSON = fmt.Sprintf(`,"flow":%q`, flow)
	}
	switch security {
	case "tls", "xtls":
		s := fmt.Sprintf(`,"tls":{"enabled":true,"insecure":true,"server_name":%q`, sni)
		if fp := q.Get("fp"); fp != "" {
			s += fmt.Sprintf(`,"utls":{"enabled":true,"fingerprint":%q}`, fp)
		}
		if alpnStr := q.Get("alpn"); alpnStr != "" {
			ab, _ := json.Marshal(strings.Split(alpnStr, ","))
			s += fmt.Sprintf(`,"alpn":%s`, ab)
		}
		return flowJSON + s + "}", ""
	case "reality":
		pbk := q.Get("pbk")
		if pbk == "" {
			return "", "reality: missing public key (pbk)"
		}
		return flowJSON + fmt.Sprintf(`,"tls":{"enabled":true,"server_name":%q,"utls":{"enabled":true,"fingerprint":%q},"reality":{"enabled":true,"public_key":%q,"short_id":%q}}`,
			sni, first(q.Get("fp"), "chrome"), pbk, q.Get("sid")), ""
	case "none", "":
		// flow requires TLS - don't include it for plaintext connections
		return "", ""
	}
	return "", "unknown security: " + security
}

func buildTransportJSON(network, path, host, grpcService string) string {
	if path == "" {
		path = "/"
	}
	switch network {
	case "ws":
		if host != "" {
			return fmt.Sprintf(`,"transport":{"type":"ws","path":%q,"headers":{"Host":%q}}`, path, host)
		}
		return fmt.Sprintf(`,"transport":{"type":"ws","path":%q}`, path)
	case "grpc":
		return fmt.Sprintf(`,"transport":{"type":"grpc","service_name":%q}`, grpcService)
	case "h2", "http":
		if host != "" {
			return fmt.Sprintf(`,"transport":{"type":"http","host":[%q],"path":%q}`, host, path)
		}
		return fmt.Sprintf(`,"transport":{"type":"http","path":%q}`, path)
	case "tcp":
		return ""
	case "httpupgrade":
		if host != "" {
			return fmt.Sprintf(`,"transport":{"type":"httpupgrade","path":%q,"host":%q}`, path, host)
		}
		return fmt.Sprintf(`,"transport":{"type":"httpupgrade","path":%q}`, path)
	case "splithttp", "xhttp":
		if host != "" {
			return fmt.Sprintf(`,"transport":{"type":"splithttp","path":%q,"host":%q}`, path, host)
		}
		return fmt.Sprintf(`,"transport":{"type":"splithttp","path":%q}`, path)
	}
	return ""
}

func parseTrojan(raw string) (string, string) {
	u, err := url.Parse(sanitizeProxyURL(raw))
	if err != nil {
		return "", "url parse: " + err.Error()
	}
	password := u.User.Username()
	if password == "" {
		return "", "missing password"
	}
	server := u.Hostname()
	if server == "" {
		return "", "missing server"
	}
	port, err := toPort(u.Port())
	if err != nil {
		return "", "port: " + err.Error()
	}
	q := u.Query()
	sni := first(q.Get("sni"), q.Get("peer"), server)
	tls := fmt.Sprintf(`,"tls":{"enabled":true,"insecure":true,"server_name":%q`, sni)
	if fp := q.Get("fp"); fp != "" {
		tls += fmt.Sprintf(`,"utls":{"enabled":true,"fingerprint":%q}`, fp)
	}
	tls += "}"
	network := strings.ToLower(q.Get("type"))
	transport := buildTransportJSON(network, first(q.Get("path"), "/"), q.Get("host"),
		first(q.Get("serviceName"), q.Get("path")))
	return fmt.Sprintf(`{"type":"trojan","tag":"proxy","server":%q,"server_port":%d,"password":%q%s%s}`,
		server, port, password, tls, transport), ""
}

// singboxSupportedSSCiphers lists ciphers supported by sing-box.
// Unsupported ciphers (rc4, rc4-md5, chacha20, bf-cfb, etc.) cause SINGBOX_START failures.
var singboxSupportedSSCiphers = map[string]bool{
	"aes-128-gcm": true, "aes-192-gcm": true, "aes-256-gcm": true,
	"aes-128-cfb": true, "aes-192-cfb": true, "aes-256-cfb": true,
	"aes-128-ctr": true, "aes-192-ctr": true, "aes-256-ctr": true,
	"chacha20-ietf-poly1305": true, "xchacha20-ietf-poly1305": true,
	"chacha20-ietf": true,
	"2022-blake3-aes-128-gcm":       true,
	"2022-blake3-aes-256-gcm":       true,
	"2022-blake3-chacha20-poly1305": true,
	"none": true, "plain": true,
}

func parseShadowsocks(raw string) (string, string) {
	trimmed := strings.TrimPrefix(raw, "ss://")
	// Strip fragment
	if idx := strings.LastIndex(trimmed, "#"); idx != -1 {
		trimmed = trimmed[:idx]
	}
	trimmed = strings.TrimSpace(trimmed)

	var method, password, server string
	var port int

	// Fast path: try url.Parse for standard ss://method:pass@host:port format.
	// This handles the case where userinfo contains `:` and is NOT base64.
	fastPathOK := false
	if fastU, err := url.Parse("ss://" + trimmed); err == nil &&
		fastU.User != nil && fastU.Hostname() != "" {
		uname := fastU.User.Username()
		pwd, hasPwd := fastU.User.Password()
		host := fastU.Hostname()
		portStr := fastU.Port()
		if portStr == "" {
			portStr = "443"
		}
		var m, p string
		if hasPwd {
			m, p = uname, pwd
		} else {
			// uname might be base64(method:pass)
			if d, derr := decodeBase64([]byte(uname)); derr == nil && strings.Contains(d, ":") {
				parts := strings.SplitN(d, ":", 2)
				m, p = parts[0], parts[1]
			}
		}
		if m != "" && host != "" {
			if pVal, perr := toPort(portStr); perr == nil {
				method, password, server, port = m, p, host, pVal
				fastPathOK = true
			}
		}
	}

	if !fastPathOK {
		atIdx := strings.LastIndex(trimmed, "@")
		if atIdx == -1 {
			decoded, err := decodeBase64([]byte(trimmed))
			if err != nil {
				decoded = trimmed
			}
			atIdx2 := strings.LastIndex(decoded, "@")
			if atIdx2 == -1 {
				return "", "missing @"
			}
			userPart := decoded[:atIdx2]
			hostPart := decoded[atIdx2+1:]
			if idx := strings.Index(hostPart, "?"); idx != -1 {
				hostPart = hostPart[:idx]
			}
			m, p, s, po, e := ssParseUserAndHost(userPart, hostPart)
			if e != "" {
				return "", e
			}
			method, password, server, port = m, p, s, po
		} else {
			userPart := trimmed[:atIdx]
			hostPart := trimmed[atIdx+1:]
			if idx := strings.Index(hostPart, "?"); idx != -1 {
				hostPart = hostPart[:idx]
			}
			m, p, s, po, e := ssParseUserAndHost(userPart, hostPart)
			if e != "" {
				return "", e
			}
			method, password, server, port = m, p, s, po
		}
	}

	method = strings.ToLower(method)
	if !singboxSupportedSSCiphers[method] {
		return "", fmt.Sprintf("unsupported cipher: %s", method)
	}
	if server == "" {
		return "", "missing server"
	}
	return fmt.Sprintf(`{"type":"shadowsocks","tag":"proxy","server":%q,"server_port":%d,"method":%q,"password":%q}`,
		server, port, method, password), ""
}

// ssParseUserAndHost extracts method, password, server, port from the two halves of an SS URL.
func ssParseUserAndHost(userPart, hostPart string) (method, password, server string, port int, errMsg string) {
	// Decode userPart: may be:
	//   1. plain "method:password"
	//   2. base64("method:password")
	//   3. URL-encoded plain or base64
	//   4. "base64method:password" (rare split format)

	decodeUser := func(s string) string {
		// Try base64 decode of whole string (common SIP002 format)
		if d, err := decodeBase64([]byte(s)); err == nil && strings.Contains(d, ":") {
			return d
		}
		// Try URL-unescape first, then base64
		if unescaped, err := url.PathUnescape(s); err == nil && unescaped != s {
			if d, err2 := decodeBase64([]byte(unescaped)); err2 == nil && strings.Contains(d, ":") {
				return d
			}
			if strings.Contains(unescaped, ":") {
				return unescaped
			}
		}
		// Try base64 decode of only the part before ":" if present
		if colonIdx := strings.Index(s, ":"); colonIdx != -1 {
			prefix := s[:colonIdx]
			suffix := s[colonIdx+1:]
			if d, err := decodeBase64([]byte(prefix)); err == nil && !strings.Contains(d, ":") {
				// prefix was base64-encoded method, suffix is password
				return d + ":" + suffix
			}
		}
		return s
	}

	decoded := decodeUser(userPart)

	parts := strings.SplitN(decoded, ":", 2)
	if len(parts) != 2 || parts[0] == "" {
		return "", "", "", 0, "invalid user info"
	}
	method = strings.TrimSpace(parts[0])
	password = parts[1]

	// Parse host:port
	// Handle IPv6 in brackets
	hostPart = strings.TrimSpace(hostPart)
	var portStr string
	if strings.HasPrefix(hostPart, "[") {
		// IPv6
		closeBracket := strings.Index(hostPart, "]")
		if closeBracket == -1 {
			return "", "", "", 0, "invalid IPv6 host"
		}
		server = hostPart[1:closeBracket]
		rest := hostPart[closeBracket+1:]
		if strings.HasPrefix(rest, ":") {
			portStr = rest[1:]
		} else {
			portStr = "443"
		}
	} else {
		lastColon := strings.LastIndex(hostPart, ":")
		if lastColon == -1 {
			return "", "", "", 0, "missing port"
		}
		server = hostPart[:lastColon]
		portStr = hostPart[lastColon+1:]
	}

	// Clean portStr: strip non-digit chars (e.g., '\2}', newlines)
	if idx := strings.IndexFunc(portStr, func(r rune) bool { return r < '0' || r > '9' }); idx != -1 {
		portStr = portStr[:idx]
	}
	portStr = strings.TrimSpace(portStr)
	p, err := toPort(portStr)
	if err != nil {
		return "", "", "", 0, "port: " + err.Error()
	}
	return method, password, server, p, ""
}

func parseHysteria2(raw string) (string, string) {
	trimmed := strings.TrimPrefix(raw, "hy2://")
	if i := strings.LastIndex(trimmed, "#"); i != -1 {
		trimmed = trimmed[:i]
	}
	queryStr := ""
	if i := strings.Index(trimmed, "?"); i != -1 {
		queryStr = trimmed[i+1:]
		trimmed = trimmed[:i]
	}
	lastAt := strings.LastIndex(trimmed, "@")
	if lastAt == -1 {
		return "", "missing @"
	}
	password := trimmed[:lastAt]
	hostPort := trimmed[lastAt+1:]
	if password == "" {
		return "", "missing password"
	}
	if i := strings.Index(hostPort, "/"); i != -1 {
		hostPort = hostPort[:i]
	}
	lastColon := strings.LastIndex(hostPort, ":")
	var server string
	var port int
	if lastColon == -1 {
		// No port specified - use default 443
		server = hostPort
		port = 443
	} else {
		portCandidate := hostPort[lastColon+1:]
		// Verify it's actually a port number (not part of IPv6)
		if _, perr := toPort(portCandidate); perr == nil {
			server = hostPort[:lastColon]
			port, _ = toPort(portCandidate)
		} else if strings.HasPrefix(hostPort, "[") {
			// Pure IPv6 without port
			server = hostPort
			port = 443
		} else {
			return "", "missing port"
		}
	}
	if server == "" {
		return "", "missing server"
	}
	q, _ := url.ParseQuery(queryStr)
	return fmt.Sprintf(`{"type":"hysteria2","tag":"proxy","server":%q,"server_port":%d,"password":%q,"tls":{"enabled":true,"insecure":true,"server_name":%q}}`,
		server, port, password, first(q.Get("sni"), server)), ""
}

func parseHysteria(raw string) (string, string) {
	u, err := url.Parse(sanitizeProxyURL(raw))
	if err != nil {
		return "", "url parse: " + err.Error()
	}
	server := u.Hostname()
	if server == "" {
		return "", "missing server"
	}
	port, err := toPort(u.Port())
	if err != nil {
		return "", "port: " + err.Error()
	}
	q := u.Query()
	auth := first(q.Get("auth"), u.User.Username())
	if auth == "" {
		return "", "missing auth"
	}
	up, _ := strconv.Atoi(first(q.Get("upmbps"), "10"))
	down, _ := strconv.Atoi(first(q.Get("downmbps"), "50"))
	if up <= 0 {
		up = 10
	}
	if down <= 0 {
		down = 50
	}
	return fmt.Sprintf(`{"type":"hysteria","tag":"proxy","server":%q,"server_port":%d,"up_mbps":%d,"down_mbps":%d,"auth_str":%q,"tls":{"enabled":true,"insecure":true,"server_name":%q}}`,
		server, port, up, down, auth, first(q.Get("peer"), q.Get("sni"), server)), ""
}

func parseTUIC(raw string) (string, string) {
	u, err := url.Parse(sanitizeProxyURL(raw))
	if err != nil {
		return "", "url parse: " + err.Error()
	}
	uuid := u.User.Username()
	if uuid == "" {
		return "", "missing uuid"
	}
	password, _ := u.User.Password()
	server := u.Hostname()
	if server == "" {
		return "", "missing server"
	}
	port, err := toPort(u.Port())
	if err != nil {
		return "", "port: " + err.Error()
	}
	return fmt.Sprintf(`{"type":"tuic","tag":"proxy","server":%q,"server_port":%d,"uuid":%q,"password":%q,"tls":{"enabled":true,"insecure":true,"server_name":%q}}`,
		server, port, uuid, password, first(u.Query().Get("sni"), server)), ""
}

func coreIdentity(line, protocol string) string {
	switch protocol {
	case "vmess":
		data := strings.TrimPrefix(line, "vmess://")
		if idx := strings.LastIndex(data, "#"); idx != -1 {
			data = data[:idx]
		}
		data = strings.TrimSpace(data)
		var jsonStr string
		if strings.HasPrefix(data, "{") {
			jsonStr = data
		} else {
			decoded, err := decodeBase64([]byte(data))
			if err != nil {
				return line
			}
			jsonStr = decoded
		}
		var d struct {
			Add  string      `json:"add"`
			Port interface{} `json:"port"`
			ID   string      `json:"id"`
		}
		json.Unmarshal([]byte(jsonStr), &d)
		return fmt.Sprintf("vmess://%s:%v#%s", d.Add, d.Port, d.ID)
	default:
		u, err := url.Parse(sanitizeProxyURL(line))
		if err != nil || u.Hostname() == "" {
			return line
		}
		return fmt.Sprintf("%s://%s@%s:%s", protocol, u.User.String(), u.Hostname(), u.Port())
	}
}

func writeOutputFiles(results []configResult) {
	byProto := make(map[string][]string)
	byProtoClash := make(map[string][]string)
	byProtoClashNames := make(map[string][]string)
	var all []string
	var allClash []string
	var allClashNames []string
	protoCounters := make(map[string]int)

	for _, r := range results {
		named := renameTo(r.line, r.proto, cfg.Output.ConfigName)
		all = append(all, named)
		byProto[r.proto] = append(byProto[r.proto], named)

		protoCounters[r.proto]++
		cname := fmt.Sprintf("%s-%s-%03d", cfg.Output.ConfigName, r.proto, protoCounters[r.proto])
		if entry, ok := configToClashYAML(r.line, r.proto, cname); ok {
			allClash = append(allClash, entry)
			allClashNames = append(allClashNames, cname)
			byProtoClash[r.proto] = append(byProtoClash[r.proto], entry)
			byProtoClashNames[r.proto] = append(byProtoClashNames[r.proto], cname)
		}
	}

	writeFile(cfg.Output.MainFile, all)
	for proto, lines := range byProto {
		writeFile(filepath.Join(cfg.Output.ProtocolsDir, proto+".txt"), lines)
	}

	if gClash.simple != "" {
		writeClashConfigSimple(filepath.Join(filepath.Dir(cfg.Output.MainFile), "clash.yaml"), allClash, allClashNames)
		for proto, entries := range byProtoClash {
			writeClashConfigSimple(filepath.Join(cfg.Output.ProtocolsDir, proto+"_clash.yaml"), entries, byProtoClashNames[proto])
		}
	}
	if gClash.advanced != "" {
		writeClashConfigAdvanced(filepath.Join(filepath.Dir(cfg.Output.MainFile), "clash_advanced.yaml"), allClash, allClashNames)
		for proto, entries := range byProtoClash {
			writeClashConfigAdvanced(filepath.Join(cfg.Output.ProtocolsDir, proto+"_clash_advanced.yaml"), entries, byProtoClashNames[proto])
		}
	}

	writeBatchFiles(all, allClash, allClashNames)
}

func writeBatchFiles(allV2ray []string, allClash []string, allClashNames []string) {
	const batchSize = 500

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	shuffledV2ray := make([]string, len(allV2ray))
	copy(shuffledV2ray, allV2ray)
	rng.Shuffle(len(shuffledV2ray), func(i, j int) { shuffledV2ray[i], shuffledV2ray[j] = shuffledV2ray[j], shuffledV2ray[i] })

	type clashPair struct {
		entry string
		name  string
	}
	shuffledClash := make([]clashPair, len(allClash))
	for i := range allClash {
		shuffledClash[i] = clashPair{entry: allClash[i], name: allClashNames[i]}
	}
	rng.Shuffle(len(shuffledClash), func(i, j int) { shuffledClash[i], shuffledClash[j] = shuffledClash[j], shuffledClash[i] })

	for batchIdx := 0; batchIdx*batchSize < len(shuffledV2ray); batchIdx++ {
		start := batchIdx * batchSize
		end := start + batchSize
		if end > len(shuffledV2ray) {
			end = len(shuffledV2ray)
		}
		batch := shuffledV2ray[start:end]
		path := fmt.Sprintf("config/batches/v2ray/batch_%03d.txt", batchIdx+1)
		writeFile(path, batch)
	}

	if len(shuffledClash) > 0 {
		for batchIdx := 0; batchIdx*batchSize < len(shuffledClash); batchIdx++ {
			start := batchIdx * batchSize
			end := start + batchSize
			if end > len(shuffledClash) {
				end = len(shuffledClash)
			}
			batch := shuffledClash[start:end]
			entries := make([]string, len(batch))
			names := make([]string, len(batch))
			for i, p := range batch {
				entries[i] = p.entry
				names[i] = p.name
			}
			if gClash.simple != "" {
				pathSimple := fmt.Sprintf("config/batches/clash/batch_%03d.yaml", batchIdx+1)
				writeClashConfigSimple(pathSimple, entries, names)
			}
			if gClash.advanced != "" {
				pathAdvanced := fmt.Sprintf("config/batches/clash_advanced/batch_%03d.yaml", batchIdx+1)
				writeClashConfigAdvanced(pathAdvanced, entries, names)
			}
		}
	}
}


func writeFile(path string, lines []string) {
	f, err := os.Create(path)
	if err != nil {
		fmt.Printf("❌ Cannot write %s: %v\n", path, err)
		return
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 256*1024)
	for _, line := range lines {
		w.WriteString(line + "\n")
	}
	w.Flush()
}

func writeClashConfigSimple(path string, proxyEntries, proxyNames []string) {
	if len(proxyEntries) == 0 || gClash.simple == "" {
		return
	}
	content := injectClashProxies(gClash.simple, proxyEntries, proxyNames)
	f, err := os.Create(path)
	if err != nil {
		fmt.Printf("❌ Cannot write %s: %v\n", path, err)
		return
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 512*1024)
	defer w.Flush()
	w.WriteString(content)
}

func writeClashConfigAdvanced(path string, proxyEntries, proxyNames []string) {
	if len(proxyEntries) == 0 || gClash.advanced == "" {
		return
	}
	content := injectClashProxies(gClash.advanced, proxyEntries, proxyNames)
	f, err := os.Create(path)
	if err != nil {
		fmt.Printf("❌ Cannot write %s: %v\n", path, err)
		return
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 512*1024)
	defer w.Flush()
	w.WriteString(content)
}


func configToClashYAML(line, proto, name string) (string, bool) {
	switch proto {
	case "vmess":
		return vmessClashYAML(line, name)
	case "vless":
		return vlessClashYAML(line, name)
	case "trojan":
		return trojanClashYAML(line, name)
	case "ss":
		return ssClashYAML(line, name)
	case "hy2":
		return hy2ClashYAML(line, name)
	case "hy":
		return hyClashYAML(line, name)
	case "tuic":
		return tuicClashYAML(line, name)
	}
	return "", false
}

func vmessClashYAML(raw, name string) (string, bool) {
	data := strings.TrimPrefix(raw, "vmess://")
	if idx := strings.LastIndex(data, "#"); idx != -1 {
		data = data[:idx]
	}
	data = strings.TrimSpace(data)
	var jsonStr string
	if strings.HasPrefix(data, "{") {
		jsonStr = data
	} else {
		decoded, err := decodeBase64([]byte(data))
		if err != nil {
			return "", false
		}
		jsonStr = decoded
	}
	var d map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &d); err != nil {
		return "", false
	}
	server := strings.TrimSpace(fmt.Sprintf("%v", d["add"]))
	if server == "" {
		return "", false
	}
	port, err := toPort(fmt.Sprintf("%v", d["port"]))
	if err != nil {
		return "", false
	}
	uuid := strings.TrimSpace(fmt.Sprintf("%v", d["id"]))
	if uuid == "" {
		return "", false
	}
	alterId := 0
	if v, ok := d["aid"]; ok {
		switch x := v.(type) {
		case float64:
			alterId = int(x)
		case string:
			alterId, _ = strconv.Atoi(x)
		}
	}
	cipher := "auto"
	if s, _ := d["scy"].(string); s != "" {
		cipher = s
	}
	network := "tcp"
	if n, _ := d["net"].(string); n != "" {
		network = n
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "  - name: %s\n    type: vmess\n    server: %s\n    port: %d\n    uuid: %s\n    alterId: %d\n    cipher: %s\n    udp: true\n",
		yamlQuote(name), yamlQuote(server), port, yamlQuote(uuid), alterId, yamlQuote(cipher))
	if tlsVal, _ := d["tls"].(string); tlsVal == "tls" {
		sni := server
		if s, _ := d["sni"].(string); s != "" {
			sni = s
		} else if h, _ := d["host"].(string); h != "" {
			sni = h
		}
		fmt.Fprintf(&sb, "    tls: true\n    skip-cert-verify: true\n    servername: %s\n", yamlQuote(sni))
		if fp, _ := d["fp"].(string); fp != "" {
			fmt.Fprintf(&sb, "    client-fingerprint: %s\n", yamlQuote(fp))
		}
	}
	appendNetworkClash(&sb, network, strDefault(d["path"], "/"), strDefault(d["host"], ""),
		strDefault(d["serviceName"], strDefault(d["path"], "")))
	return sb.String(), true
}

func vlessClashYAML(raw, name string) (string, bool) {
	u, err := url.Parse(sanitizeProxyURL(raw))
	if err != nil {
		return "", false
	}
	uuid := u.User.Username()
	server := u.Hostname()
	port, err := toPort(u.Port())
	if err != nil || uuid == "" || server == "" {
		return "", false
	}
	q := u.Query()
	security := strings.ToLower(q.Get("security"))
	network := strings.ToLower(q.Get("type"))
	if network == "" {
		network = "tcp"
	}
	sni := first(q.Get("sni"), q.Get("peer"), server)
	fp := q.Get("fp")
	var sb strings.Builder
	fmt.Fprintf(&sb, "  - name: %s\n    type: vless\n    server: %s\n    port: %d\n    uuid: %s\n    udp: true\n",
		yamlQuote(name), yamlQuote(server), port, yamlQuote(uuid))
	if flow := q.Get("flow"); flow != "" {
		fmt.Fprintf(&sb, "    flow: %s\n", yamlQuote(flow))
	}
	switch security {
	case "tls", "xtls":
		fmt.Fprintf(&sb, "    tls: true\n    skip-cert-verify: true\n    servername: %s\n", yamlQuote(sni))
		if fp != "" {
			fmt.Fprintf(&sb, "    client-fingerprint: %s\n", yamlQuote(fp))
		}
		if alpn := q.Get("alpn"); alpn != "" {
			parts := strings.Split(alpn, ",")
			quoted := make([]string, len(parts))
			for i, a := range parts {
				quoted[i] = yamlQuote(strings.TrimSpace(a))
			}
			fmt.Fprintf(&sb, "    alpn: [%s]\n", strings.Join(quoted, ", "))
		}
	case "reality":
		pbk := q.Get("pbk")
		if pbk == "" {
			return "", false
		}
		fmt.Fprintf(&sb, "    tls: true\n    skip-cert-verify: false\n    servername: %s\n    client-fingerprint: %s\n    reality-opts:\n      public-key: %s\n",
			yamlQuote(sni), yamlQuote(first(fp, "chrome")), yamlQuote(pbk))
		if sid := q.Get("sid"); sid != "" {
			fmt.Fprintf(&sb, "      short-id: %s\n", yamlQuote(sid))
		}
	}
	appendNetworkClash(&sb, network, first(q.Get("path"), "/"), q.Get("host"),
		first(q.Get("serviceName"), q.Get("path")))
	return sb.String(), true
}

func trojanClashYAML(raw, name string) (string, bool) {
	u, err := url.Parse(sanitizeProxyURL(raw))
	if err != nil {
		return "", false
	}
	password := u.User.Username()
	server := u.Hostname()
	port, err := toPort(u.Port())
	if err != nil || password == "" || server == "" {
		return "", false
	}
	q := u.Query()
	sni := first(q.Get("sni"), q.Get("peer"), server)
	var sb strings.Builder
	fmt.Fprintf(&sb, "  - name: %s\n    type: trojan\n    server: %s\n    port: %d\n    password: %s\n    sni: %s\n    skip-cert-verify: true\n    udp: true\n",
		yamlQuote(name), yamlQuote(server), port, yamlQuote(password), yamlQuote(sni))
	if fp := q.Get("fp"); fp != "" {
		fmt.Fprintf(&sb, "    client-fingerprint: %s\n", yamlQuote(fp))
	}
	appendNetworkClash(&sb, strings.ToLower(q.Get("type")), first(q.Get("path"), "/"), q.Get("host"),
		first(q.Get("serviceName"), q.Get("path")))
	return sb.String(), true
}

func ssClashYAML(raw, name string) (string, bool) {
	trimmed := strings.TrimPrefix(raw, "ss://")
	if idx := strings.Index(trimmed, "#"); idx != -1 {
		trimmed = trimmed[:idx]
	}
	atIdx := strings.LastIndex(trimmed, "@")
	var userInfo, hostInfo string
	if atIdx == -1 {
		decoded, err := decodeBase64([]byte(trimmed))
		if err != nil {
			return "", false
		}
		atIdx = strings.LastIndex(decoded, "@")
		if atIdx == -1 {
			return "", false
		}
		userInfo = decoded[:atIdx]
		hostInfo = decoded[atIdx+1:]
	} else {
		userInfo = trimmed[:atIdx]
		hostInfo = trimmed[atIdx+1:]
	}
	if idx := strings.Index(hostInfo, "?"); idx != -1 {
		hostInfo = hostInfo[:idx]
	}
	decoded, err := decodeBase64([]byte(userInfo))
	if err != nil {
		decoded = userInfo
	}
	parts := strings.SplitN(decoded, ":", 2)
	if len(parts) != 2 {
		return "", false
	}
	lastColon := strings.LastIndex(hostInfo, ":")
	if lastColon == -1 {
		return "", false
	}
	portStr := hostInfo[lastColon+1:]
	if s := strings.Index(portStr, "/"); s != -1 {
		portStr = portStr[:s]
	}
	server := hostInfo[:lastColon]
	port, err := toPort(portStr)
	if err != nil || server == "" {
		return "", false
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "  - name: %s\n    type: ss\n    server: %s\n    port: %d\n    cipher: %s\n    password: %s\n    udp: true\n",
		yamlQuote(name), yamlQuote(server), port, yamlQuote(parts[0]), yamlQuote(parts[1]))
	return sb.String(), true
}

func hy2ClashYAML(raw, name string) (string, bool) {
	trimmed := strings.TrimPrefix(raw, "hy2://")
	if i := strings.LastIndex(trimmed, "#"); i != -1 {
		trimmed = trimmed[:i]
	}
	queryStr := ""
	if i := strings.Index(trimmed, "?"); i != -1 {
		queryStr = trimmed[i+1:]
		trimmed = trimmed[:i]
	}
	lastAt := strings.LastIndex(trimmed, "@")
	if lastAt == -1 {
		return "", false
	}
	password := trimmed[:lastAt]
	hostPort := trimmed[lastAt+1:]
	if password == "" {
		return "", false
	}
	if i := strings.Index(hostPort, "/"); i != -1 {
		hostPort = hostPort[:i]
	}
	lastColon := strings.LastIndex(hostPort, ":")
	if lastColon == -1 {
		return "", false
	}
	server := hostPort[:lastColon]
	port, err := toPort(hostPort[lastColon+1:])
	if err != nil || server == "" {
		return "", false
	}
	q, _ := url.ParseQuery(queryStr)
	var sb strings.Builder
	fmt.Fprintf(&sb, "  - name: %s\n    type: hysteria2\n    server: %s\n    port: %d\n    password: %s\n    sni: %s\n    skip-cert-verify: true\n    udp: true\n",
		yamlQuote(name), yamlQuote(server), port, yamlQuote(password), yamlQuote(first(q.Get("sni"), server)))
	return sb.String(), true
}

func hyClashYAML(raw, name string) (string, bool) {
	u, err := url.Parse(sanitizeProxyURL(raw))
	if err != nil {
		return "", false
	}
	server := u.Hostname()
	if server == "" {
		return "", false
	}
	port, err := toPort(u.Port())
	if err != nil {
		return "", false
	}
	q := u.Query()
	auth := first(q.Get("auth"), u.User.Username())
	if auth == "" {
		return "", false
	}
	up, _ := strconv.Atoi(first(q.Get("upmbps"), "10"))
	down, _ := strconv.Atoi(first(q.Get("downmbps"), "50"))
	if up <= 0 {
		up = 10
	}
	if down <= 0 {
		down = 50
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "  - name: %s\n    type: hysteria\n    server: %s\n    port: %d\n    auth-str: %s\n    up: %d\n    down: %d\n    sni: %s\n    skip-cert-verify: true\n    udp: true\n",
		yamlQuote(name), yamlQuote(server), port, yamlQuote(auth), up, down,
		yamlQuote(first(q.Get("peer"), q.Get("sni"), server)))
	return sb.String(), true
}

func tuicClashYAML(raw, name string) (string, bool) {
	u, err := url.Parse(sanitizeProxyURL(raw))
	if err != nil {
		return "", false
	}
	uuid := u.User.Username()
	password, _ := u.User.Password()
	server := u.Hostname()
	port, err := toPort(u.Port())
	if err != nil || uuid == "" || server == "" {
		return "", false
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "  - name: %s\n    type: tuic\n    server: %s\n    port: %d\n    uuid: %s\n    password: %s\n    sni: %s\n    skip-cert-verify: true\n    udp: true\n    congestion-controller: bbr\n    reduce-rtt: true\n",
		yamlQuote(name), yamlQuote(server), port, yamlQuote(uuid), yamlQuote(password),
		yamlQuote(first(u.Query().Get("sni"), server)))
	return sb.String(), true
}

func appendNetworkClash(sb *strings.Builder, network, path, host, grpcService string) {
	if path == "" {
		path = "/"
	}
	switch network {
	case "ws":
		fmt.Fprintf(sb, "    network: ws\n    ws-opts:\n      path: %s\n", yamlQuote(path))
		if host != "" {
			fmt.Fprintf(sb, "      headers:\n        Host: %s\n", yamlQuote(host))
		}
	case "grpc":
		fmt.Fprintf(sb, "    network: grpc\n    grpc-opts:\n      grpc-service-name: %s\n", yamlQuote(grpcService))
	case "h2", "http":
		fmt.Fprintf(sb, "    network: h2\n    h2-opts:\n      path: %s\n", yamlQuote(path))
		if host != "" {
			fmt.Fprintf(sb, "      host: [%s]\n", yamlQuote(host))
		}
	case "httpupgrade":
		fmt.Fprintf(sb, "    network: httpupgrade\n    httpupgrade-opts:\n      path: %s\n", yamlQuote(path))
		if host != "" {
			fmt.Fprintf(sb, "      host: %s\n", yamlQuote(host))
		}
	case "splithttp", "xhttp":
		fmt.Fprintf(sb, "    network: splithttp\n    splithttp-opts:\n      path: %s\n", yamlQuote(path))
		if host != "" {
			fmt.Fprintf(sb, "      host: %s\n", yamlQuote(host))
		}
	}
}

func strDefault(v interface{}, def string) string {
	if v == nil {
		return def
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return def
	}
	return s
}

func renameTo(config, protocol, newName string) string {
	switch protocol {
	case "vmess":
		data := strings.TrimPrefix(config, "vmess://")
		if idx := strings.LastIndex(data, "#"); idx != -1 {
			data = data[:idx]
		}
		data = strings.TrimSpace(data)
		var jsonStr string
		if strings.HasPrefix(data, "{") {
			jsonStr = data
		} else {
			decoded, err := decodeBase64([]byte(data))
			if err != nil {
				return config
			}
			jsonStr = decoded
		}
		var d map[string]interface{}
		if err := json.Unmarshal([]byte(jsonStr), &d); err != nil {
			return config
		}
		d["ps"] = newName
		keys := make([]string, 0, len(d))
		for k := range d {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var buf bytes.Buffer
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kj, _ := json.Marshal(k)
			vj, _ := json.Marshal(d[k])
			buf.Write(kj)
			buf.WriteByte(':')
			buf.Write(vj)
		}
		buf.WriteByte('}')
		return "vmess://" + base64.StdEncoding.EncodeToString(buf.Bytes())
	default:
		if idx := strings.Index(config, "#"); idx != -1 {
			return config[:idx] + "#" + url.PathEscape(newName)
		}
		return config + "#" + url.PathEscape(newName)
	}
}

func countBatchFiles(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() {
			count++
		}
	}
	return count
}

func min500(batchIdx, total int) int {
	start := (batchIdx - 1) * 500
	if start >= total {
		return 0
	}
	end := start + 500
	if end > total {
		return total - start
	}
	return end - start
}



func writeSummary(results []configResult, failedLinks []string, duration float64, originalTotal int) {
	byProtoOut := make(map[string]int)
	for _, r := range results {
		byProtoOut[r.proto]++
	}

	f, err := os.Create("README.md")
	if err != nil {
		return
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()

	repoBase := "https://github.com/Delta-Kronecker/V2ray-Config/raw/refs/heads/main"

	w.WriteString("## Statistics\n\n")

	w.WriteString("### Per-Protocol Input & Output\n\n")
	fmt.Fprintf(w, "| Protocol | Tested (unique) | valid | Pass Rate |\n|---|---|---|---|\n")
	totalIn := 0
	totalOut := 0
	for _, p := range cfg.ProtocolOrder {
		in := gInputByProto[p]
		out := byProtoOut[p]
		totalIn += in
		totalOut += out
		rate := 0.0
		if in > 0 {
			rate = float64(out) / float64(in) * 100
		}
		fmt.Fprintf(w, "| %s | %d | %d | %.1f%% |\n", strings.ToUpper(p), in, out, rate)
	}
	overallRate := 0.0
	if totalIn > 0 {
		overallRate = float64(totalOut) / float64(totalIn) * 100
	}
	fmt.Fprintf(w, "| **Total** | **%d** | **%d** | **%.1f%%** |\n\n", totalIn, totalOut, overallRate)

	fmt.Fprintf(w, "| Metric | Value |\n|---|---|\n")
	fmt.Fprintf(w, "| Raw fetched lines | %d |\n", originalTotal)
	fmt.Fprintf(w, "| Unique after dedup | %d |\n", totalIn)
	fmt.Fprintf(w, "| Valid configs | %d |\n", len(results))
	fmt.Fprintf(w, "| Processing time | %.2fs |\n\n", duration)

	w.WriteString("---\n\n")
	w.WriteString("## Main Files\n\n")

	w.WriteString("### V2ray — All Configs\n\n")
	fmt.Fprintf(w, "| File | Link |\n|---|---|\n")
	fmt.Fprintf(w, "| All configs (txt) | [all_configs.txt](%s/config/all_configs.txt) |\n\n", repoBase)

	w.WriteString("### V2ray — By Protocol\n\n")
	fmt.Fprintf(w, "| Protocol | Count | Link |\n|---|---|---|\n")
	for _, p := range cfg.ProtocolOrder {
		if n := byProtoOut[p]; n > 0 {
			fmt.Fprintf(w, "| %s | %d | [%s.txt](%s/config/protocols/%s.txt) |\n",
				strings.ToUpper(p), n, p, repoBase, p)
		}
	}
	w.WriteString("\n")

	w.WriteString("### Clash \n\n")
	fmt.Fprintf(w, "Groups: **PROXY** (selector) → **Load-Balance** · **Auto** · **Fallback**\n\n")
	fmt.Fprintf(w, "| File | Link |\n|---|---|\n")
	fmt.Fprintf(w, "| clash.yaml (all protocols) | [clash.yaml](%s/config/clash.yaml) |\n", repoBase)
	for _, p := range cfg.ProtocolOrder {
		if byProtoOut[p] > 0 {
			fmt.Fprintf(w, "| %s_clash.yaml | [%s_clash.yaml](%s/config/protocols/%s_clash.yaml) |\n",
				p, p, repoBase, p)
		}
	}
	w.WriteString("\n")

	w.WriteString("---\n\n")
	w.WriteString("## Batch Files — Random 500-Config Groups\n\n")
	w.WriteString("> Each file contains 500 randomly selected configs from all protocols.\n\n")

	v2rayBatches := countBatchFiles("config/batches/v2ray")
	clashBatches := countBatchFiles("config/batches/clash")

	w.WriteString("### V2ray Batches\n\n")
	fmt.Fprintf(w, "| Batch | Count | Link |\n|---|---|---|\n")
	for i := 1; i <= v2rayBatches; i++ {
		cnt := min500(i, len(results))
		fmt.Fprintf(w, "| Batch %03d | %d | [batch_%03d.txt](%s/config/batches/v2ray/batch_%03d.txt) |\n",
			i, cnt, i, repoBase, i)
	}
	w.WriteString("\n")

	w.WriteString("### Clash Batches\n\n")
	fmt.Fprintf(w, "| Batch | Link |\n|---|---|\n")
	for i := 1; i <= clashBatches; i++ {
		fmt.Fprintf(w, "| Batch %03d | [batch_%03d.yaml](%s/config/batches/clash/batch_%03d.yaml) |\n",
			i, i, repoBase, i)
	}
	w.WriteString("\n")

	w.WriteString("---\n\n")
	w.WriteString("## 🔥 Keep This Project Going!\n\n")
	w.WriteString("If you're finding this useful, please show your support:\n\n")
	w.WriteString("⭐ **Star the repository on GitHub**\n\n")
	w.WriteString("⭐ **Star our [Telegram posts](https://t.me/DeltaKroneckerGithub)** \n\n")
	w.WriteString("Your stars fuel our motivation to keep improving!\n")
}



func decodeBase64(encoded []byte) (string, error) {
	// Strip all whitespace variants (space, tab, CR, LF)
	s := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, string(encoded))

	// Normalize: strip existing padding to get clean raw string
	stripped := strings.TrimRight(s, "=")

	// Build padded version (multiple of 4)
	padded := stripped
	if r := len(padded) % 4; r != 0 {
		padded += strings.Repeat("=", 4-r)
	}

	// Try with padding: StdEncoding (+/) then URLEncoding (-_)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding} {
		if b, err := enc.DecodeString(padded); err == nil {
			return string(b), nil
		}
	}
	// Try without padding: RawStdEncoding (+/) then RawURLEncoding (-_)
	for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(stripped); err == nil {
			return string(b), nil
		}
	}
	_, err := base64.RawURLEncoding.DecodeString(stripped)
	return "", err
}

func toPort(s string) (int, error) {
	s = strings.TrimSpace(s)
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 || n > 65535 {
		return 0, fmt.Errorf("invalid port %q", s)
	}
	return n, nil
}

func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func singBoxPath() string {
	for _, p := range []string{"./sing-box", "/usr/local/bin/sing-box"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "sing-box"
}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
			syscall.Kill(-pgid, syscall.SIGKILL)
		}
		cmd.Process.Kill()
	}
	cmd.Wait()
}

func extractErr(stderr string) string {
	var errs []string
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		// Skip warn/info/debug lines
		if strings.Contains(lower, "warn") || strings.Contains(lower, "deprecated") {
			continue
		}
		if strings.Contains(lower, `"level":"info"`) || strings.Contains(lower, `"level":"debug"`) ||
			strings.Contains(lower, "level=info") || strings.Contains(lower, "level=debug") {
			continue
		}
		if len(line) > 120 {
			line = line[:120] + "..."
		}
		errs = append(errs, line)
		if len(errs) >= 3 {
			break
		}
	}
	return strings.Join(errs, " | ")
}

func shortenErr(s string) string {
	s = strings.ReplaceAll(s, `"`, "")
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

func yamlQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
