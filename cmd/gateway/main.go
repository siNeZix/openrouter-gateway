package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"openrouter-gateway/internal/config"
	"openrouter-gateway/internal/keys"
	"openrouter-gateway/internal/models"
	"openrouter-gateway/internal/proxy"
	"openrouter-gateway/internal/store"
	"openrouter-gateway/internal/web"
)

func main() {
	log.Println("Starting OpenRouter Free Gateway...")

	// 1. Load Config
	cfg := config.Load()
	log.Printf("Loaded configuration. Listening on %s", cfg.ListenAddr)

	// 2. Initialize DB Store
	dbStore, err := store.New(cfg.DbPath)
	if err != nil {
		log.Fatalf("Database initialization failed: %v", err)
	}
	defer dbStore.Close()
	log.Printf("SQLite database initialized at %s", cfg.DbPath)

	// 3. Initialize Key Pool (reads from DB)
	keyPool, err := keys.NewKeyPool(dbStore)
	if err != nil {
		log.Fatalf("Key pool initialization failed: %v", err)
	}
	log.Println("Key pool loaded and synchronized with database.")

	// 4. Initialize & Start Model Ranking Manager (Shir-Man API)
	rankingMgr := models.NewRankingManager(dbStore, cfg.RankingRefresh)
	rankingMgr.Start()
	log.Println("Model ranking manager started.")

	// 5. Initialize & Start Background Key Checker
	keyChecker := keys.NewKeyChecker(
		keyPool,
		cfg.KeyCheckTTL,
		cfg.KeyCheckRate,
		cfg.KeyCheckRateInterval,
		cfg.KeyCheckConcurrency,
	)
	keyChecker.Start()
	log.Println("Background key verification worker started.")

	// 6. Setup Handlers
	proxyHandler := proxy.NewProxyHandler(cfg, dbStore, keyPool, rankingMgr)
	webServer := web.NewWebServer(cfg, dbStore, rankingMgr, keyPool)

	mux := http.NewServeMux()

	// Setup Dashboard (Basic Auth protected)
	webServer.Start(mux)

	// Setup API Gateway routes (Bearer Token protected)
	mux.Handle("/v1/", proxyHandler)

	// 7. Start HTTP Server
	server := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: mux,
	}

	go func() {
		log.Printf("HTTP Server is running on %s", cfg.ListenAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server failure: %v", err)
		}
	}()

	// 8. Graceful Shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down gracefully...")

	// Stop Key Checker
	keyChecker.Stop()

	// Shutdown HTTP Server
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	log.Println("OpenRouter Free Gateway stopped.")
}
