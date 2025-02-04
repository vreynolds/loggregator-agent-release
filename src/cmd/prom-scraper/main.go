package main

import (
	"code.cloudfoundry.org/go-loggregator/metrics"
	"code.cloudfoundry.org/loggregator-agent/cmd/prom-scraper/app"
	"code.cloudfoundry.org/loggregator-agent/pkg/scraper"
	"log"
	"os"
)

func main() {
	log := log.New(os.Stderr, "", log.LstdFlags)
	log.Printf("starting Prom Scraper...")
	defer log.Printf("closing Prom Scraper...")

	cfg := app.LoadConfig(log)

	m := metrics.NewRegistry(
		log,
		metrics.WithTLSServer(
			int(cfg.MetricsServer.Port),
			cfg.MetricsServer.CertFile,
			cfg.MetricsServer.KeyFile,
			cfg.MetricsServer.CAFile,
		),
	)

	configProvider := scraper.NewConfigProvider(cfg.ConfigGlobs, cfg.DefaultScrapeInterval, log).Configs
	app.NewPromScraper(cfg, configProvider, m, log).Run()
}
