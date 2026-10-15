// Package main adapts Traefik forwardAuth requests to registry authorization.
package main

import (
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

func main() {
	token := os.Getenv("REGISTRY_PROXY_TOKEN")
	if token == "" {
		log.Fatal("REGISTRY_PROXY_TOKEN is required")
	}
	client := &http.Client{Timeout: 8 * time.Second}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Header.Get("X-Forwarded-Host")
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		req, err := http.NewRequestWithContext(r.Context(), "GET", "http://registry:7272/internal/authorize?host="+url.QueryEscape(host), nil)
		if err != nil {
			http.Error(w, "authorization unavailable", 503)
			return
		}
		for _, h := range []string{"X-Auth-Request-User", "X-Auth-Request-Preferred-Username", "X-Auth-Request-Groups"} {
			req.Header.Set(h, r.Header.Get(h))
		}
		req.Header.Set("X-NSL-Proxy-Token", token)
		res, err := client.Do(req)
		if err != nil {
			http.Error(w, "authorization unavailable", 503)
			return
		}
		defer res.Body.Close()
		w.WriteHeader(res.StatusCode)
		_, _ = io.Copy(w, io.LimitReader(res.Body, 1024))
	})
	log.Fatal((&http.Server{Addr: ":4181", Handler: handler, ReadHeaderTimeout: 5 * time.Second}).ListenAndServe())
}
