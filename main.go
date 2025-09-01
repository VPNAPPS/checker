package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
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
	// Define command-line flags
	urls := flag.String("urls", "", "Comma-separated list of subscription URLs")
	timeout := flag.Duration("timeout", 10*time.Second, "Timeout for each network request")
	concurrency := flag.Int("concurrency", 50, "Number of concurrent workers to test configs")
	geoDBPath := flag.String("geoip-db", "GeoLite2-Country.mmdb", "Path to the GeoIP MMDB file")
	outputFile := flag.String("output", "results.txt", "Name of the output file for results")

	flag.Parse()

	if *urls == "" {
		log.Println("Error: -urls flag is required.")
		flag.Usage()
		os.Exit(1)
	}

	log.SetOutput(os.Stderr)
	log.Println("Starting proxy tester...")

	// Load the GeoIP database
	var err error
	geoDB, err = geoip2.Open(*geoDBPath)
	if err != nil {
		log.Fatalf("FATAL: Could not load GeoIP database from '%s'. Error: %v", *geoDBPath, err)
	}
	defer geoDB.Close()

	// Fetch configurations from subscription URLs
	log.Println("Fetching configurations from subscriptions...")
	subscriptionURLs := strings.Split(*urls, ",")
	allConfigs := fetchConfigsFromSubscriptions(subscriptionURLs, &http.Client{Timeout: 30 * time.Second})
	if len(allConfigs) == 0 {
		log.Fatalf("FATAL: No proxy configurations found from the provided URLs.")
	}
	log.Printf("Found %d total configs before cleanup.", len(allConfigs))

	// Clean up configs: remove everything after # and remove duplicates
	cleanConfigs := cleanupConfigs(allConfigs)
	if len(cleanConfigs) == 0 {
		log.Fatalf("FATAL: No valid proxy configurations after cleanup.")
	}
	log.Printf("After cleanup: %d unique configs. Starting tests with %d workers...", len(cleanConfigs), *concurrency)

	// Test all configurations
	results := testConfigs(*concurrency, cleanConfigs, *timeout)

	if len(results) == 0 {
		log.Println("No working proxies found.")
		return
	}

	// Sort results by speed in descending order
	sort.SliceStable(results, func(i, j int) bool {
		return results[i].SpeedMbps > results[j].SpeedMbps
	})

	log.Printf("\n--- Test Complete ---")
	log.Printf("Found %d working proxies.", len(results))

	// Write results to the output file
	file, err := os.Create(*outputFile)
	if err != nil {
		log.Fatalf("Failed to create output file: %v", err)
	}
	defer file.Close()

	for _, result := range results {
		line := fmt.Sprintf("Country: %s, Speed: %.2f Mbps, Config: %s\n", result.Country, result.SpeedMbps, result.Config)
		file.WriteString(line)
	}

	log.Printf("Top %d fastest proxies written to %s", len(results), *outputFile)
}



// cleanupConfigs removes everything after # and removes duplicate configurations
func cleanupConfigs(configs []string) []string {
	seen := make(map[string]bool)
	var cleaned []string
	
	for _, config := range configs {
		// Remove everything after # (including the # itself)
		if idx := strings.Index(config, "#"); idx != -1 {
			config = config[:idx]
		}
		
		// Trim whitespace
		config = strings.TrimSpace(config)
		
		// Skip empty configs
		if config == "" {
			continue
		}
		
		// Skip duplicates
		if seen[config] {
			continue
		}
		
		seen[config] = true
		cleaned = append(cleaned, config)
	}
	
	return cleaned
}

// fetchConfigsFromSubscriptions downloads and decodes proxy configurations from a list of subscription URLs.
func fetchConfigsFromSubscriptions(urls []string, client *http.Client) []string {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var allConfigs []string

	for _, url := range urls {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			log.Printf("Fetching from %s...", u)
			req, err := http.NewRequest("GET", u, nil)
			if err != nil {
				log.Printf("Failed to create request for %s: %v", u, err)
				return
			}
			// Some providers require a specific user agent
			req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/58.0.3029.110 Safari/537.36")

			resp, err := client.Do(req)
			if err != nil {
				log.Printf("Failed to fetch subscription from %s: %v", u, err)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				log.Printf("Received non-200 status code from %s: %s", u, resp.Status)
				return
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				log.Printf("Failed to read response body from %s: %v", u, err)
				return
			}

			// Attempt to decode from Base64, otherwise use as plain text
			decodedBody, err := base64.StdEncoding.DecodeString(string(body))
			var content string
			if err != nil {
				content = string(body) // Assume plain text if base64 decoding fails
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

// testConfigs orchestrates the testing of multiple proxy configurations concurrently.
func testConfigs(numWorkers int, configs []string, timeout time.Duration) []Result {
	jobs := make(chan string, len(configs))
	resultsChan := make(chan Result, len(configs))
	var wg sync.WaitGroup

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go worker(i+1, &wg, jobs, resultsChan, timeout)
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

// worker is a goroutine that processes proxy testing jobs with panic recovery.
func worker(id int, wg *sync.WaitGroup, jobs <-chan string, results chan<- Result, timeout time.Duration) {
	defer wg.Done()
	
	for config := range jobs {
		// Per-config panic recovery to skip problematic configs
		func() {
			defer func() {
				if r := recover(); r != nil {
					// Simply skip the config that caused panic - no logging needed
					return
				}
			}()
			
			var core pkg.Core
			if strings.HasPrefix(config, "hy") {
				core = singbox.NewSingboxService(false, true)
			} else {
				core = xray.NewXrayService(false, false)
			}

			proto, err := core.CreateProtocol(config)
			if err != nil || proto.Parse() != nil {
				return
			}

			httpClient, instance, err := core.MakeHttpClient(proto, timeout)
			if err != nil {
				return
			}
			defer instance.Close()

			if !checkReachability(httpClient, timeout) {
				return
			}

			speed, err := testDownloadSpeed(httpClient, timeout*3)
			if err != nil {
				return
			}

			ipCheckClient, ipCheckInstance, err := core.MakeHttpClient(proto, timeout)
			if err != nil {
				return
			}
			_, country := getIPAndCountry(ipCheckClient, timeout)
			ipCheckInstance.Close()

			if country == "" {
				return
			}

			log.Printf("[Worker %d] SUCCESS: %s | Country: %s | Speed: %.2f Mbps", id, proto.ConvertToGeneralConfig().Address, country, speed)
			results <- Result{
				Config:    config,
				SpeedMbps: speed,
				Country:   country,
			}
		}()
	}
}

// getIPAndCountry uses the provided HTTP client to determine the public IP and its country.
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

// checkReachability performs a quick HEAD request to confirm the proxy can connect to the internet.
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

// testDownloadSpeed measures the download speed by fetching a test file.
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
