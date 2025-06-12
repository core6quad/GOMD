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
	TotalViews int
	PageViews  map[string]int
}

var analytics = &Analytics{
	PageViews: make(map[string]int),
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
		user, pass, ok := r.BasicAuth()
		if !ok || user != cfg.AnalyticsUser || pass != cfg.AnalyticsPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="analytics"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		// Serve a styled HTML analytics dashboard with a chart
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`
<!DOCTYPE html>
<html>
<head>
	<title>GOMD Analytics</title>
	<script src="https://cdn.jsdelivr.net/npm/chart.js"></script>
	<style>
		body { font-family: sans-serif; background: #181c20; color: #eee; margin: 0; padding: 0; }
		.container { max-width: 700px; margin: 40px auto; background: #23272b; border-radius: 10px; padding: 32px; box-shadow: 0 2px 16px #0004; }
		h1 { text-align: center; }
		.stats { margin: 24px 0; font-size: 1.2em; }
		canvas { background: #fff; border-radius: 8px; }
		.footer { text-align: center; margin-top: 32px; color: #888; font-size: 0.9em; }
	</style>
</head>
<body>
	<div class="container">
		<h1>GOMD Analytics</h1>
		<div class="stats">
			<b>Total Views:</b> ` + itoa(analytics.TotalViews) + `
		</div>
		<canvas id="viewsChart" width="600" height="320"></canvas>
		<div class="footer">GOMD Analytics &mdash; Live stats</div>
	</div>
	<script>
			const ctx = document.getElementById('viewsChart').getContext('2d');
			const data = {
				labels: ` + pageLabelsJSON() + `,
				datasets: [{
					label: 'Page Views',
					data: ` + pageViewsJSON() + `,
					backgroundColor: 'rgba(54, 162, 235, 0.5)',
					borderColor: 'rgba(54, 162, 235, 1)',
					borderWidth: 2
				}]
			};
			new Chart(ctx, {
				type: 'bar',
				data: data,
				options: {
					scales: {
						y: { beginAtZero: true }
					}
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

// Helper to convert int to string (no strconv import needed for this small use)
func itoa(i int) string {
	return fmt.Sprintf("%d", i)
}

// Helper to generate JSON arrays for chart labels and data
func pageLabelsJSON() string {
	labels := []string{}
	for k := range analytics.PageViews {
		labels = append(labels, k)
	}
	b, _ := json.Marshal(labels)
	return string(b)
}
func pageViewsJSON() string {
	views := []int{}
	for _, k := range analytics.PageViews {
		views = append(views, k)
	}
	b, _ := json.Marshal(views)
	return string(b)
}
