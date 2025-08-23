package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
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
	sanityCheckURL = "https://googleads.g.doubleclick.net/mads/static/mad/sdk/native/production/sdk-core-v40-impl.html"
	// URL of the file to download for speed testing.
	speedTestURL = "http://cachefly.cachefly.net/10mb.test"
	// Size of the speed test file in bytes.
	speedTestFileSize = 10 * 1024 * 1024
	// URL to check the public IP address.
	ipCheckURL = "https://api.ifconfig.me/ip"
)

// geoDB holds the GeoIP database reader.
var geoDB *geoip2.Reader

// testConfigs orchestrates the testing of multiple proxy configurations concurrently.
// It distributes the work among a specified number of worker goroutines.
func testConfigs(numWorkers int, configs []string, timeout time.Duration) []Result {
	// Create channels for jobs and results.
	jobs := make(chan string, len(configs))
	resultsChan := make(chan Result, len(configs))
	var wg sync.WaitGroup

	// Start the worker goroutines.
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go worker(i+1, &wg, jobs, resultsChan, timeout)
	}

	// Send all configs to the jobs channel.
	for _, config := range configs {
		jobs <- config
	}
	close(jobs)

	// Wait for all workers to finish, then close the results channel.
	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	// Collect all results from the results channel.
	var finalResults []Result
	for result := range resultsChan {
		finalResults = append(finalResults, result)
	}
	return finalResults
}

// worker is a goroutine that processes proxy testing jobs.
// It receives configurations from the jobs channel and sends results to the results channel.
func worker(id int, wg *sync.WaitGroup, jobs <-chan string, results chan<- Result, timeout time.Duration) {
	defer wg.Done()
	for config := range jobs {
		var core pkg.Core
		// Determine the core service (singbox or xray) based on the config prefix.
		if strings.HasPrefix(config, "hy") {
			core = singbox.NewSingboxService(false, true)
		} else {
			core = xray.NewXrayService(false, false)
		}

		// Parse the configuration string.
		proto, err := core.CreateProtocol(config)
		if err != nil || proto.Parse() != nil {
			continue // Skip invalid configs.
		}

		// Create an HTTP client that routes traffic through the proxy.
		httpClient, instance, err := core.MakeHttpClient(proto, timeout)
		if err != nil {
			continue
		}
		defer instance.Close()

		// 1. Perform a sanity check to see if the proxy is reachable.
		if !checkReachability(httpClient, timeout) {
			continue
		}
		log.Printf("[Worker %d] Sanity check PASSED for %s", id, proto.ConvertToGeneralConfig().Address)

		// 2. Test the download speed.
		speed, err := testDownloadSpeed(httpClient, timeout*3) // Use a longer timeout for speed test.
		if err != nil {
			continue
		}
		log.Printf("[Worker %d] Speed test PASSED for %s | Speed: %.2f Mbps", id, proto.ConvertToGeneralConfig().Address, speed)

		// 3. Get the country of the proxy server.
		ipCheckClient, ipCheckInstance, err := core.MakeHttpClient(proto, timeout)
		if err != nil {
			continue
		}
		_, country := getIPAndCountry(ipCheckClient, timeout)
		ipCheckInstance.Close()

		if country == "" {
			log.Printf("[Worker %d] Geo-location FAILED for %s", id, proto.ConvertToGeneralConfig().Address)
			continue
		}
		log.Printf("[Worker %d] SUCCESS: %s | Country: %s", id, proto.ConvertToGeneralConfig().Address, country)

		// Send the successful result to the results channel.
		results <- Result{
			Config:    config,
			SpeedMbps: speed,
			Country:   country,
		}
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

	// Look up the country using the GeoIP database.
	record, err := geoDB.Country(ip)
	if err != nil {
		return ipStr, "XX" // Return "XX" for unknown country.
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

	// A status code in the 2xx or 3xx range indicates success.
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

	// Discard the downloaded content, as we only need the time it took.
	_, err = io.Copy(io.Discard, resp.Body)
	if err != nil {
		return 0, err
	}
	duration := time.Since(start).Seconds()
	if duration == 0 {
		return 0, fmt.Errorf("download took zero time")
	}

	// Calculate speed in Megabits per second (Mbps).
	speedMbps := (float64(speedTestFileSize) * 8) / duration / 1_000_000
	return speedMbps, nil
}
