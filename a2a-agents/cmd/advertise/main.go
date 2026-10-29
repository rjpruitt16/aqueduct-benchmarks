// Command advertise sits in front of a resource that doesn't speak Aquifer's
// pacing contract (vLLM) and adds X-Aqueduct-Max-Concurrent to its responses.
// Every configuration reaches the resource through it, so the extra hop costs
// all three the same.
//
// Environment: LISTEN (:8001), TARGET (http://127.0.0.1:8000), MAX_CONCURRENT
// (the resource's capacity, e.g. vLLM's --max-num-seqs).
package main

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"

	"github.com/rjpruitt16/aqueduct-benchmarks/a2a-agents/internal/advertise"
)

func main() {
	target, err := url.Parse(env("TARGET", "http://127.0.0.1:8000"))
	if err != nil {
		log.Fatalf("TARGET: %v", err)
	}
	n, err := strconv.Atoi(os.Getenv("MAX_CONCURRENT"))
	if err != nil || n < 1 {
		log.Fatalf("MAX_CONCURRENT must be the resource's capacity")
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &http.Transport{MaxIdleConnsPerHost: 256}
	addr := env("LISTEN", ":8001")
	log.Printf("advertise on %s: %s, max concurrent %d", addr, target, n)
	log.Fatal(http.ListenAndServe(addr, advertise.Wrap(proxy, n)))
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
