package main

import (
	"io/fs"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/russross/blackfriday/v2"
)

const (
	srcDir   = "./web"
	buildDir = "./.built"
)

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

	http.Handle("/", http.FileServer(http.Dir(buildDir)))
	log.Println("Serving on http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
