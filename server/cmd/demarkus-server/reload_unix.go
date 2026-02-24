//go:build !windows

package main

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/latebit/demarkus/server/internal/config"
)

func startCertReloader(cfg *config.Config, prodMode bool, logger *slog.Logger) {
	sighupChan := make(chan os.Signal, 1)
	signal.Notify(sighupChan, syscall.SIGHUP)
	go func() {
		for range sighupChan {
			if prodMode {
				if err := loadCert(cfg.TLSCert, cfg.TLSKey); err != nil {
					logger.Error("tls: certificate reload failed", "error", err)
				} else {
					logger.Info("tls: certificate reloaded", "path", cfg.TLSCert)
				}
			}
			if cfg.TokensFile != "" {
				if err := loadTokenStore(cfg.TokensFile); err != nil {
					logger.Error("auth: token reload failed", "error", err)
				} else {
					logger.Info("auth: tokens reloaded", "path", cfg.TokensFile)
				}
			}
		}
	}()
}
