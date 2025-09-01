package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/naser-989/xray-knife/v3/pkg"
	"github.com/naser-989/xray-knife/v3/pkg/singbox"
	"github.com/naser-989/xray-knife/v3/pkg/xray"
	"github.com/oschwald/geoip2-golang"
)

// Result holds the outcome of a single proxy test.
type Result struct {
	Config    string
	SpeedMbps float64
	Country   string
}

const (
	// URL for a quick reachability check.
	sanityCheckURL = "https://pubads.g.doubleclick.net/gampad/ads?iu=/21775744923/external/single_ad_samples&sz=640x480&cust_params=sample_ct%3Dlinear&ciu_szs=300x250%2C728x90&gdfp_req=1&output=vast&unviewed_position_start=1&env=vp&correlator="
	// URL of the file to download for speed testing.
	speedTestURL = "http://cachefly.cachefly.net/10mb.test"
	// Size of the speed test file in bytes.
	speedTestFileSize = 10 * 1024 * 1024
	// URL to check the public IP address.
	ipCheckURL = "https://api.ifconfig.me/ip"
)

// geoDB holds the GeoIP database reader.
var geoDB *geoip2.Reader

// main is the entry point of the application.
func main() {
	// --- Command-line Flags ---
	urls := flag.String("urls", "", "Comma-separated list of subscription URLs (Coordinator mode)")
	timeout := flag.Duration("timeout", 10*time.Second, "Timeout for each network request")
	concurrency := flag.Int("concurrency", 50, "Number of concurrent workers to test configs (Coordinator mode)")
	geoDBPath := flag.String("geoip-db", "GeoLite2-Country.mmdb", "Path to the GeoIP MMDB file")
	outputFile := flag.String("output", "results.txt", "Name of the output file for results (Coordinator mode)")

	// --- Flags for internal worker process ---
	isWorker := flag.Bool("worker", false, "Run in worker mode (internal use)")
	workerConfig := flag.String("config", "", "Single config to test (worker mode)")

	flag.Parse()

	// --- Logic to Divert to Worker or Coordinator ---
	if *isWorker {
		// If the -worker flag is present, run in isolated worker mode
		runWorker(*workerConfig, *timeout, *geoDBPath)
		return // Worker exits after its job is done.
	}

	// Otherwise, run in the main coordinator mode
	runCoordinator(*urls, *outputFile, *geoDBPath, *concurrency, *timeout)
}

// runCoordinator is the main logic for fetching, testing, and saving configs.
func runCoordinator(urls, outputFile, geoDBPath string, concurrency int, timeout time.Duration) {
	if urls == "" {
		log.Println("Error: -urls flag is required.")
		flag.Usage()
		os.Exit(1)
	}

	log.SetOutput(os.Stderr)
	log.Println("Starting proxy tester in Coordinator mode...")

	// Fetch configurations from subscription URLs
	log.Println("Fetching configurations from subscriptions...")
	subscriptionURLs := strings.Split(urls, ",")
	allConfigs := fetchConfigsFromSubscriptions(subscriptionURLs, &http.Client{Timeout: 30 * time.Second})
	if len(allConfigs) == 0 {
		log.Fatalf("FATAL: No proxy configurations found from the provided URLs.")
	}
	log.Printf("Found %d total configs before cleanup.", len(allConfigs))

	// Clean up configs
	cleanConfigs := cleanupConfigs(allConfigs)
	if len(cleanConfigs) == 0 {
		log.Fatalf("FATAL: No valid proxy configurations after cleanup.")
	}
	log.Printf("After cleanup: %d unique configs. Starting tests with %d workers...", len(cleanConfigs), concurrency)

	// Test all configurations using subprocess workers
	results := testConfigsInSubprocesses(concurrency, cleanConfigs, timeout, geoDBPath)

	if len(results) == 0 {
		log.Println("No working proxies found.")
		return
	}

	// Sort results by speed
	sort.SliceStable(results, func(i, j int) bool {
		return results[i].SpeedMbps > results[j].SpeedMbps
	})

	log.Printf("\n--- Test Complete ---")
	log.Printf("Found %d working proxies.", len(results))

	// Write results to file
	file, err := os.Create(outputFile)
	if err != nil {
		log.Fatalf("Failed to create output file: %v", err)
	}
	defer file.Close()

	for _, result := range results {
		line := fmt.Sprintf("Country: %s, Speed: %.2f Mbps, Config: %s\n", result.Country, result.SpeedMbps, result.Config)
		file.WriteString(line)
	}

	log.Printf("Top %d fastest proxies written to %s", len(results), outputFile)
}

// runWorker executes the test for a single configuration and prints the result as JSON to stdout.
// This function runs in a separate, isolated process.
func runWorker(config string, timeout time.Duration, geoDBPath string) {
	log.SetOutput(io.Discard) // Workers should not log to stderr, they communicate via stdout

	var err error
	geoDB, err = geoip2.Open(geoDBPath)
	if err != nil {
		// If we can't open the DB, we can't proceed. Exit.
		fmt.Fprintf(os.Stderr, "worker failed to open geoip db: %v", err)
		os.Exit(1)
	}
	defer geoDB.Close()

	result, err := testSingleConfig(config, timeout)
	if err != nil {
		// A test failure is not a crash, just exit gracefully.
		// The coordinator will see the empty output and know it failed.
		os.Exit(1)
	}

	// On success, print the result as JSON to stdout for the coordinator to read.
	json.NewEncoder(os.Stdout).Encode(result)
}

// testConfigsInSubprocesses orchestrates testing configs, each in its own process.
func testConfigsInSubprocesses(numWorkers int, configs []string, timeout time.Duration, geoDBPath string) []Result {
	jobs := make(chan string, len(configs))
	resultsChan := make(chan Result, len(configs))
	var wg sync.WaitGroup

	executablePath, err := os.Executable()
	if err != nil {
		log.Fatalf("FATAL: Could not find executable path: %v", err)
	}

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for config := range jobs {
				// Each worker process gets its own timeout context
				ctx, cancel := context.WithTimeout(context.Background(), timeout*5)
				defer cancel()

				// Prepare the command to run the executable in worker mode
				cmd := exec.CommandContext(ctx, executablePath,
					"-worker",
					"-config", config,
					"-timeout", timeout.String(),
					"-geoip-db", geoDBPath,
				)

				// Run the command and capture its stdout
				output, err := cmd.Output()
				if err != nil {
					// This indicates the worker process crashed or exited with an error.
					// This is the "catch" for the panic. We log it and move on.
					// log.Printf("[Coordinator] Worker %d failed for a config. Error: %v", workerID, err)
					continue
				}

				// If successful, decode the JSON result from the worker's stdout
				var result Result
				if err := json.Unmarshal(output, &result); err == nil {
					resultsChan <- result
				}
			}
		}(i + 1)
	}

	for _, config := range configs {
		jobs <- config
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	var finalResults []Result
	for result := range resultsChan {
		finalResults = append(finalResults, result)
	}
	return finalResults
}

// testSingleConfig now returns a result and an error, without any concurrency logic.
// It's the core testing logic that runs inside the isolated worker process.
func testSingleConfig(config string, timeout time.Duration) (Result, error) {
	var core pkg.Core
	if strings.HasPrefix(config, "hy") {
		core = singbox.NewSingboxService(false, true)
	} else {
		core = xray.NewXrayService(false, false)
	}

	proto, err := core.CreateProtocol(config)
	if err != nil || proto.Parse() != nil {
		return Result{}, fmt.Errorf("config parse error")
	}

	httpClient, instance, err := core.MakeHttpClient(proto, timeout)
	if err != nil {
		return Result{}, err
	}
	defer instance.Close()

	if !checkReachability(httpClient, timeout) {
		return Result{}, fmt.Errorf("reachability check failed")
	}

	speed, err := testDownloadSpeed(httpClient, timeout*3)
	if err != nil {
		return Result{}, err
	}

	ipCheckClient, ipCheckInstance, err := core.MakeHttpClient(proto, timeout)
	if err != nil {
		return Result{}, err
	}
	defer ipCheckInstance.Close()

	_, country := getIPAndCountry(ipCheckClient, timeout)
	if country == "" {
		return Result{}, fmt.Errorf("could not determine country")
	}

	log.Printf("SUCCESS: %s | Country: %s | Speed: %.2f Mbps", proto.ConvertToGeneralConfig().Address, country, speed)
	return Result{
		Config:    config,
		SpeedMbps: speed,
		Country:   country,
	}, nil
}

// --- Utility Functions (largely unchanged) ---

func cleanupConfigs(configs []string) []string {
	seen := make(map[string]bool)
	var cleaned []string
	for _, config := range configs {
		if idx := strings.Index(config, "#"); idx != -1 {
			config = config[:idx]
		}
		config = strings.TrimSpace(config)
		if config == "" || seen[config] {
			continue
		}
		seen[config] = true
		cleaned = append(cleaned, config)
	}
	return cleaned
}

func fetchConfigsFromSubscriptions(urls []string, client *http.Client) []string {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var allConfigs []string

	for _, url := range urls {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			req, err := http.NewRequest("GET", u, nil)
			if err != nil {
				return
			}
			req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/58.0.3029.110 Safari/537.36")
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return
			}
			decodedBody, err := base64.StdEncoding.DecodeString(string(body))
			var content string
			if err != nil {
				content = string(body)
			} else {
				content = string(decodedBody)
			}
			configs := strings.Split(content, "\n")
			mu.Lock()
			for _, config := range configs {
				trimmed := strings.TrimSpace(config)
				if trimmed != "" {
					allConfigs = append(allConfigs, trimmed)
				}
			}
			mu.Unlock()
		}(url)
	}
	wg.Wait()
	return allConfigs
}

func getIPAndCountry(client *http.Client, timeout time.Duration) (string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", ipCheckURL, nil)
	if err != nil {
		return "", ""
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()

	ipBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", ""
	}

	ipStr := strings.TrimSpace(string(ipBytes))
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return "", ""
	}

	record, err := geoDB.Country(ip)
	if err != nil {
		return ipStr, "XX"
	}
	return ipStr, record.Country.IsoCode
}

func checkReachability(client *http.Client, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "HEAD", sanityCheckURL, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 400
}

func testDownloadSpeed(client *http.Client, timeout time.Duration) (float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", speedTestURL, nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("received non-200 status: %s", resp.Status)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	if err != nil {
		return 0, err
	}
	duration := time.Since(start).Seconds()
	if duration == 0 {
		return 0, fmt.Errorf("download took zero time")
	}
	speedMbps := (float64(speedTestFileSize) * 8) / duration / 1_000_000
	return speedMbps, nil
}

