package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/russross/blackfriday/v2"
)

const (
	srcDir   = "./web"
	buildDir = "./.built"
)

type Config struct {
	Port          string `json:"port"`
	AnalyticsUser string `json:"analytics_user"`
	AnalyticsPass string `json:"analytics_pass"`
}

type Analytics struct {
	TotalViews     int
	PageViews      map[string]int
	BrowserEngines map[string]int
	Countries      map[string]int
}

var analytics = &Analytics{
	PageViews:      make(map[string]int),
	BrowserEngines: make(map[string]int),
	Countries:      make(map[string]int),
}

// Track last view time per IP+page to avoid counting rapid reloads as new views
var lastView = make(map[string]time.Time)

const viewCooldown = 10 * time.Second // Only count a view per IP+page every 10s

func loadConfig() Config {
	f, err := os.Open("config.json")
	if err != nil {
		return Config{Port: "8080"}
	}
	defer f.Close()
	var cfg Config
	if err := json.NewDecoder(f).Decode(&cfg); err != nil || cfg.Port == "" {
		return Config{Port: "8080"}
	}
	return cfg
}

func preprocessGMD(input []byte) []byte {
	// GMD syntax preprocessing:
	// Replace (abc)[clickme] with [clickme](/abc)
	re := regexp.MustCompile(`\(([^)\s]+)\)\[([^\]]+)\]`)
	return re.ReplaceAllFunc(input, func(match []byte) []byte {
		submatches := re.FindSubmatch(match)
		if len(submatches) == 3 {
			return []byte("[" + string(submatches[2]) + "](/" + string(submatches[1]) + ")")
		}
		return match
	})
}

func compileGMDs() error {
	err := os.MkdirAll(buildDir, 0755)
	if err != nil {
		return err
	}
	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".gmd") {
			input, err := ioutil.ReadFile(path)
			if err != nil {
				return err
			}
			input = preprocessGMD(input)
			html := blackfriday.Run(input)
			rel, err := filepath.Rel(srcDir, path)
			if err != nil {
				return err
			}
			outPath := filepath.Join(buildDir, strings.TrimSuffix(rel, ".gmd")+".html")
			err = os.MkdirAll(filepath.Dir(outPath), 0755)
			if err != nil {
				return err
			}
			err = ioutil.WriteFile(outPath, html, 0644)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func cleanup() {
	os.RemoveAll(buildDir)
}

func main() {
	// Check for index.gmd
	indexPath := filepath.Join(srcDir, "index.gmd")
	if _, err := os.Stat(indexPath); err != nil {
		log.Fatalf("index.gmd not found in %s. Please create it.", srcDir)
	}

	// Check for /web/assets directory
	webAssetsPath := filepath.Join(srcDir, "assets")
	if fi, err := os.Stat(webAssetsPath); err == nil && fi.IsDir() {
		log.Fatalf("Error: Do not create an 'assets' directory inside %s. Use the top-level 'assets' directory instead.", srcDir)
	}

	// Check for /web/analytics directory
	webAnalyticsPath := filepath.Join(srcDir, "analytics")
	if fi, err := os.Stat(webAnalyticsPath); err == nil && fi.IsDir() {
		log.Fatalf("Error: Do not create an 'analytics' directory inside %s.", srcDir)
	}

	cfg := loadConfig()

	err := compileGMDs()
	if err != nil {
		log.Fatalf("Compile error: %v", err)
	}
	defer cleanup()

	// Handle Ctrl+C and SIGTERM for cleanup
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		cleanup()
		os.Exit(0)
	}()

	// Serve /assets/* from ./assets/
	http.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.Dir("assets"))))

	// Serve /favicon.ico from ./favicon.ico if present
	http.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		if _, err := os.Stat("favicon.ico"); err == nil {
			http.ServeFile(w, r, "favicon.ico")
			return
		}
		http.NotFound(w, r)
	})

	// Analytics endpoint
	http.HandleFunc("/analytics", func(w http.ResponseWriter, r *http.Request) {
		// Get memory stats
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		memMB := float64(m.Alloc) / 1024.0 / 1024.0
		// Get CPU count
		cpuCount := runtime.NumCPU()

		// Prepare browser engine data for chart
		engineLabels, engineCounts := browserEngineChartData()
		// Prepare country data for chart
		countryLabels, countryCounts := countryChartData()

		// Serve a styled HTML analytics dashboard with charts and server stats
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`
<!DOCTYPE html>
<html>
<head>
	<title>GOMD Analytics</title>
	<script src="https://cdn.jsdelivr.net/npm/chart.js"></script>
	<style>
		body { font-family: sans-serif; background: #181c20; color: #eee; margin: 0; padding: 0; }
		.container { max-width: 1200px; margin: 40px auto; background: #23272b; border-radius: 10px; padding: 32px; box-shadow: 0 2px 16px #0004; }
		h1 { text-align: center; }
		.stats { margin: 24px 0; font-size: 1.2em; }
		canvas { background: #fff; border-radius: 8px; margin-bottom: 32px; }
		.footer { text-align: center; margin-top: 32px; color: #888; font-size: 0.9em; }
		.charts { display: flex; flex-wrap: nowrap; gap: 24px; justify-content: center; }
		.chart-block { flex: 1 1 0; min-width: 0; }
		@media (max-width: 1000px) {
			.charts { flex-wrap: wrap; }
			.chart-block { min-width: 320px; }
		}
	</style>
</head>
<body>
	<div class="container">
		<h1>GOMD Analytics</h1>
		<div class="stats">
			<b>Total Views:</b> ` + itoa(analytics.TotalViews) + `<br>
			<b>CPU Cores:</b> ` + itoa(cpuCount) + `<br>
			<b>Memory Usage:</b> ` + formatFloat(memMB) + ` MB
		</div>
		<div class="charts">
			<div class="chart-block">
				<canvas id="viewsChart" width="400" height="250"></canvas>
			</div>
			<div class="chart-block">
				<canvas id="browserChart" width="400" height="250"></canvas>
			</div>
			<div class="chart-block">
				<canvas id="countryChart" width="400" height="250"></canvas>
			</div>
		</div>
		<div class="footer">GOMD Analytics &mdash; Live stats</div>
	</div>
	<script>
		const viewsCtx = document.getElementById('viewsChart').getContext('2d');
		const viewsData = {
			labels: ` + pageLabelsJSON() + `,
			datasets: [{
				label: 'Page Views',
				data: ` + pageViewsJSON() + `,
				backgroundColor: 'rgba(54, 162, 235, 0.5)',
				borderColor: 'rgba(54, 162, 235, 1)',
				borderWidth: 2
			}]
		};
		new Chart(viewsCtx, {
			type: 'bar',
			data: viewsData,
			options: {
				scales: { y: { beginAtZero: true } },
				responsive: true,
				maintainAspectRatio: false
			}
		});

		const browserCtx = document.getElementById('browserChart').getContext('2d');
		const browserData = {
			labels: ` + engineLabels + `,
			datasets: [{
				label: 'Browser Engines',
				data: ` + engineCounts + `,
				backgroundColor: [
					'rgba(255, 99, 132, 0.5)',
					'rgba(255, 205, 86, 0.5)',
					'rgba(75, 192, 192, 0.5)',
					'rgba(54, 162, 235, 0.5)',
					'rgba(153, 102, 255, 0.5)'
				],
				borderColor: [
					'rgba(255, 99, 132, 1)',
					'rgba(255, 205, 86, 1)',
					'rgba(75, 192, 192, 1)',
					'rgba(54, 162, 235, 1)',
					'rgba(153, 102, 255, 1)'
				],
				borderWidth: 2
			}]
		};
		new Chart(browserCtx, {
			type: 'pie',
			data: browserData,
			options: {
				plugins: {
					legend: { position: 'bottom' }
				},
				responsive: true,
				maintainAspectRatio: false
			}
		});

		const countryCtx = document.getElementById('countryChart').getContext('2d');
		const countryData = {
			labels: ` + countryLabels + `,
			datasets: [{
				label: 'Countries',
				data: ` + countryCounts + `,
				backgroundColor: [
					'rgba(255, 99, 132, 0.5)',
					'rgba(255, 205, 86, 0.5)',
					'rgba(75, 192, 192, 0.5)',
					'rgba(54, 162, 235, 0.5)',
					'rgba(153, 102, 255, 0.5)',
					'rgba(201, 203, 207, 0.5)'
				],
				borderColor: [
					'rgba(255, 99, 132, 1)',
					'rgba(255, 205, 86, 1)',
					'rgba(75, 192, 192, 1)',
					'rgba(54, 162, 235, 1)',
					'rgba(153, 102, 255, 1)',
					'rgba(201, 203, 207, 1)'
				],
				borderWidth: 2
			}]
		};
		new Chart(countryCtx, {
			type: 'doughnut',
			data: countryData,
			options: {
				plugins: {
					legend: { position: 'bottom' }
				},
				responsive: true,
				maintainAspectRatio: false
			}
		});
	</script>
</body>
</html>
	`))
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/" {
			path = "/index"
		}
		htmlPath := filepath.Join(buildDir, path) + ".html"
		if _, err := os.Stat(htmlPath); err == nil {
			// Analytics: count views with cooldown per IP+page
			ip, _, _ := net.SplitHostPort(r.RemoteAddr)
			key := ip + "|" + path
			now := time.Now()
			if t, ok := lastView[key]; !ok || now.Sub(t) > viewCooldown {
				analytics.TotalViews++
				analytics.PageViews[path]++
				// Browser engine detection
				engine := detectBrowserEngine(r.UserAgent())
				analytics.BrowserEngines[engine]++
				// Country detection
				country := lookupCountry(ip)
				analytics.Countries[country]++
				lastView[key] = now
			}
			http.ServeFile(w, r, htmlPath)
			return
		}
		http.NotFound(w, r)
	})

	log.Printf("Serving on http://localhost:%s\n", cfg.Port)
	log.Fatal(http.ListenAndServe(":"+cfg.Port, nil))
}

// Helper to convert int to string
func itoa(i int) string {
	return fmt.Sprintf("%d", i)
}

// Helper to format float with 1 decimal
func formatFloat(f float64) string {
	return fmt.Sprintf("%.1f", f)
}

// Helper to generate JSON arrays for chart labels and data
func pageLabelsJSON() string {
	labels := []string{}
	for k := range analytics.PageViews {
		labels = append(labels, k)
	}
	sort.Strings(labels)
	b, _ := json.Marshal(labels)
	return string(b)
}
func pageViewsJSON() string {
	labels := []string{}
	for k := range analytics.PageViews {
		labels = append(labels, k)
	}
	sort.Strings(labels)
	views := []int{}
	for _, k := range labels {
		views = append(views, analytics.PageViews[k])
	}
	b, _ := json.Marshal(views)
	return string(b)
}

// Browser engine detection (very basic)
func detectBrowserEngine(ua string) string {
	ua = strings.ToLower(ua)
	switch {
	case strings.Contains(ua, "webkit") && strings.Contains(ua, "chrome"):
		return "Blink"
	case strings.Contains(ua, "webkit"):
		return "WebKit"
	case strings.Contains(ua, "gecko") && strings.Contains(ua, "firefox"):
		return "Gecko"
	case strings.Contains(ua, "trident") || strings.Contains(ua, "msie"):
		return "Trident"
	default:
		return "Other"
	}
}

// For browser engine chart
func browserEngineChartData() (string, string) {
	type kv struct {
		Key   string
		Value int
	}
	var sorted []kv
	for k, v := range analytics.BrowserEngines {
		sorted = append(sorted, kv{k, v})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })
	labels := []string{}
	counts := []int{}
	for _, kv := range sorted {
		labels = append(labels, kv.Key)
		counts = append(counts, kv.Value)
	}
	lb, _ := json.Marshal(labels)
	cb, _ := json.Marshal(counts)
	return string(lb), string(cb)
}

// Country lookup cache to avoid repeated API calls
var countryCache = make(map[string]string)

func lookupCountry(ip string) string {
	if ip == "" {
		return "Unknown"
	}
	if c, ok := countryCache[ip]; ok {
		if c == "" {
			return "Unknown"
		}
		return c
	}
	// Use ip-api.com for free IP geolocation
	resp, err := http.Get("http://ip-api.com/json/" + ip + "?fields=countryCode")
	if err != nil {
		countryCache[ip] = "Unknown"
		return "Unknown"
	}
	defer resp.Body.Close()
	body, _ := ioutil.ReadAll(resp.Body)
	var result struct {
		CountryCode string `json:"countryCode"`
	}
	if err := json.Unmarshal(body, &result); err != nil || result.CountryCode == "" {
		countryCache[ip] = "Unknown"
		return "Unknown"
	}
	countryCache[ip] = result.CountryCode
	return result.CountryCode
}

// For country chart
func countryChartData() (string, string) {
	type kv struct {
		Key   string
		Value int
	}
	var sorted []kv
	// Always include "Unknown" if present
	for k, v := range analytics.Countries {
		if k == "" {
			sorted = append(sorted, kv{"Unknown", v})
		} else {
			sorted = append(sorted, kv{k, v})
		}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })
	labels := []string{}
	counts := []int{}
	for _, kv := range sorted {
		labels = append(labels, kv.Key)
		counts = append(counts, kv.Value)
	}
	lb, _ := json.Marshal(labels)
	cb, _ := json.Marshal(counts)
	return string(lb), string(cb)
}
