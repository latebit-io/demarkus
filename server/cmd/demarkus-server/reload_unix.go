//go:build !windows

package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/latebit/demarkus/server/internal/config"
)

func startCertReloader(cfg *config.Config, prodMode bool) {
	sighupChan := make(chan os.Signal, 1)
	signal.Notify(sighupChan, syscall.SIGHUP)
	go func() {
		for range sighupChan {
			if !prodMode {
				log.Printf("[WARN] tls: certificate reload not supported in dev mode")
				continue
			}
			if err := loadCert(cfg.TLSCert, cfg.TLSKey); err != nil {
				log.Printf("[ERROR] tls: certificate reload failed: %v", err)
			} else {
				log.Printf("[INFO] tls: certificate reloaded from %s", cfg.TLSCert)
			}
		}
	}()
}
