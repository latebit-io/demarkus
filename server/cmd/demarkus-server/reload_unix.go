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
			if prodMode {
				if err := loadCert(cfg.TLSCert, cfg.TLSKey); err != nil {
					log.Printf("[ERROR] tls: certificate reload failed: %v", err)
				} else {
					log.Printf("[INFO] tls: certificate reloaded from %s", cfg.TLSCert)
				}
			}
			if cfg.TokensFile != "" {
				if err := loadTokenStore(cfg.TokensFile); err != nil {
					log.Printf("[ERROR] auth: token reload failed: %v", err)
				} else {
					log.Printf("[INFO] auth: tokens reloaded from %s", cfg.TokensFile)
				}
			}
		}
	}()
}
