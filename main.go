// Copyright 2016 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"gopkg.in/alecthomas/kingpin.v2"
	"gopkg.in/yaml.v2"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"

	"github.com/prometheus/blackbox_exporter/config"
	"github.com/prometheus/blackbox_exporter/prober"
)

var (
	sc = &config.SafeConfig{
		C: &config.Config{},
	}

	configFile    = kingpin.Flag("config.file", "Blackbox exporter configuration file.").Default("blackbox.yml").String()
	listenAddress = kingpin.Flag("web.listen-address", "The address to listen on for HTTP requests.").Default(":9115").String()
	timeoutOffset = kingpin.Flag("timeout-offset", "Offset to subtract from timeout in seconds.").Default("0.5").Float64()

	Probers = map[string]prober.ProbeFn{
		"http": prober.ProbeHTTP,
		"tcp":  prober.ProbeTCP,
		"icmp": prober.ProbeICMP,
		"dns":  prober.ProbeDNS,
	}
)

func probeHandler(w http.ResponseWriter, r *http.Request, c *config.Config) {
	moduleName := r.URL.Query().Get("module")
	if moduleName == "" {
		moduleName = "http_2xx"
	}
	module, ok := c.Modules[moduleName]
	if !ok {
		http.Error(w, fmt.Sprintf("Unknown module %q", moduleName), 400)
		return
	}

	// If a timeout is configured via the Prometheus header, add it to the request.
	var timeoutSeconds float64
	if v := r.Header.Get("X-Prometheus-Scrape-Timeout-Seconds"); v != "" {
		var err error
		timeoutSeconds, err = strconv.ParseFloat(v, 64)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to parse timeout from Prometheus header: %s", err), http.StatusInternalServerError)
			return
		}
	}
	if timeoutSeconds == 0 {
		timeoutSeconds = 10
	}

	if module.Timeout.Seconds() < timeoutSeconds && module.Timeout.Seconds() > 0 {
		timeoutSeconds = module.Timeout.Seconds()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration((timeoutSeconds-*timeoutOffset)*1e9))
	defer cancel()
	r = r.WithContext(ctx)

	probeSuccessGauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "probe_success",
		Help: "Displays whether or not the probe was a success",
	})
	probeDurationGauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "probe_duration_seconds",
		Help: "Returns how long the probe took to complete in seconds",
	})
	params := r.URL.Query()
	target := params.Get("target")
	configData := params.Get("config")
	if target == "" {
		http.Error(w, "Target parameter is missing", 400)
		return
	}
	if configData != "" {
		recoverModule, err := config.RecoverConfig(configData, &module)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		module = recoverModule
	}

	prober, ok := Probers[module.Prober]
	if !ok {
		http.Error(w, fmt.Sprintf("Unknown prober %q", module.Prober), 400)
		return
	}

	start := time.Now()
	registry := prometheus.NewRegistry()
	registry.MustRegister(probeSuccessGauge)
	registry.MustRegister(probeDurationGauge)
	success := prober(ctx, target, module, registry)
	probeDurationGauge.Set(time.Since(start).Seconds())
	if success {
		probeSuccessGauge.Set(1)
	}
	h := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	h.ServeHTTP(w, r)
}

func init() {
	prometheus.MustRegister(version.NewCollector("blackbox_exporter"))
}

func main() {
	log.AddFlags(kingpin.CommandLine)
	kingpin.Version(version.Print("blackbox_exporter"))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	log.Infoln("Starting blackbox_exporter", version.Info())
	log.Infoln("Build context", version.BuildContext())

	if err := sc.ReloadConfig(*configFile); err != nil {
		log.Fatalf("Error loading config: %s", err)
	}
	log.Infoln("Loaded config file")

	hup := make(chan os.Signal)
	reloadCh := make(chan chan error)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-hup:
				if err := sc.ReloadConfig(*configFile); err != nil {
					log.Errorf("Error reloading config: %s", err)
					continue
				}
				log.Infoln("Loaded config file")
			case rc := <-reloadCh:
				if err := sc.ReloadConfig(*configFile); err != nil {
					log.Errorf("Error reloading config: %s", err)
					rc <- err
				} else {
					log.Infoln("Loaded config file")
					rc <- nil
				}
			}
		}
	}()

	http.HandleFunc("/-/reload",
		func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" {
				w.WriteHeader(http.StatusMethodNotAllowed)
				fmt.Fprintf(w, "This endpoint requires a POST request.\n")
				return
			}

			rc := make(chan error)
			reloadCh <- rc
			if err := <-rc; err != nil {
				http.Error(w, fmt.Sprintf("failed to reload config: %s", err), http.StatusInternalServerError)
			}
		})
	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		sc.Lock()
		conf := sc.C
		sc.Unlock()
		probeHandler(w, r, conf)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
    <head><title>Blackbox Exporter</title></head>
    <body>
    <h1>Blackbox Exporter</h1>
    <p><a href="/probe?target=prometheus.io&module=http_2xx">Probe prometheus.io for http_2xx</a></p>
    <p><a href="/metrics">Metrics</a></p>
    <p><a href="/config">Configuration</a></p>
    </body>
    </html>`))
	})

	http.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		sc.RLock()
		c, err := yaml.Marshal(sc.C)
		sc.RUnlock()
		if err != nil {
			log.Warnf("Error marshalling configuration: %v", err)
			http.Error(w, err.Error(), 500)
			return
		}
		w.Write(c)
	})

	log.Infoln("Listening on", *listenAddress)
	if err := http.ListenAndServe(*listenAddress, nil); err != nil {
		log.Fatalf("Error starting HTTP server: %s", err)
	}
}
