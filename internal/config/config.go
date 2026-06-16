package config

import (
	"flag"
	"os"
	"time"
)

type Config struct {
	GatewayToken         string
	DbPath               string
	ListenAddr           string
	RankingRefresh       time.Duration
	KeyCheckTTL          time.Duration
	KeyCheckRate         int
	KeyCheckRateInterval time.Duration
	KeyCheckConcurrency  int
	MaxKeyRetries        int
	WebUsername          string
	WebPassword          string
}

func Load() *Config {
	cfg := &Config{}

	flag.StringVar(&cfg.GatewayToken, "token", getEnv("GATEWAY_TOKEN", "super-secret-gateway-token"), "Bearer token required to use the gateway")
	flag.StringVar(&cfg.DbPath, "db-path", getEnv("DB_PATH", "gateway.db"), "Path to the SQLite database")
	flag.StringVar(&cfg.ListenAddr, "listen", getEnv("LISTEN_ADDR", ":8080"), "Listen address for the gateway server")

	rankingRefreshStr := flag.String("ranking-refresh", getEnv("RANKING_REFRESH", "1h"), "Interval for refreshing Shir-Man model rankings")
	keyCheckTTLStr := flag.String("key-ttl", getEnv("KEY_CHECK_TTL", "1h"), "How long key verification remains valid")

	flag.IntVar(&cfg.KeyCheckRate, "key-check-rate", 200, "Maximum key checks per rate limit interval")
	keyCheckRateIntStr := flag.String("key-check-interval", getEnv("KEY_CHECK_INTERVAL", "1m"), "Interval for key verification rate limiting")

	flag.IntVar(&cfg.KeyCheckConcurrency, "key-check-concurrency", 5, "Number of concurrent key checker workers")
	flag.IntVar(&cfg.MaxKeyRetries, "max-retries", 5, "Maximum number of retries for 429/5xx responses with other keys")

	flag.StringVar(&cfg.WebUsername, "web-user", getEnv("WEB_USERNAME", "admin"), "Username for Web UI auth")
	flag.StringVar(&cfg.WebPassword, "web-pass", getEnv("WEB_PASSWORD", "admin"), "Password for Web UI auth")

	flag.Parse()

	var err error
	cfg.RankingRefresh, err = time.ParseDuration(*rankingRefreshStr)
	if err != nil {
		cfg.RankingRefresh = time.Hour
	}

	cfg.KeyCheckTTL, err = time.ParseDuration(*keyCheckTTLStr)
	if err != nil {
		cfg.KeyCheckTTL = time.Hour
	}

	cfg.KeyCheckRateInterval, err = time.ParseDuration(*keyCheckRateIntStr)
	if err != nil {
		cfg.KeyCheckRateInterval = time.Minute
	}

	return cfg
}

func getEnv(key, fallback string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return fallback
}
