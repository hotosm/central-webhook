package db

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// Default retry configuration
	defaultMaxRetries      = 30
	defaultInitialInterval = 1 * time.Second
	defaultMaxInterval     = 5 * time.Second
)

func InitPool(ctx context.Context, log *slog.Logger, dbUri string) (*pgxpool.Pool, error) {
	// Get retry configuration from environment or use defaults
	maxRetries := getEnvInt("CENTRAL_WEBHOOK_DB_MAX_RETRIES", defaultMaxRetries)
	initialInterval := getEnvDuration("CENTRAL_WEBHOOK_DB_RETRY_INTERVAL", defaultInitialInterval)
	maxInterval := getEnvDuration("CENTRAL_WEBHOOK_DB_MAX_RETRY_INTERVAL", defaultMaxInterval)

	var dbPool *pgxpool.Pool
	var err error
	interval := initialInterval

	log.Info("Connecting to database", "max_retries", maxRetries)

	for attempt := 0; attempt < maxRetries; attempt++ {
		// Try to create connection pool
		dbPool, err = pgxpool.New(ctx, dbUri)
		if err != nil {
			if attempt < maxRetries-1 {
				log.Info("Database connection failed, retrying", "attempt", attempt+1, "max_retries", maxRetries, "retry_in", interval, "err", err)
				time.Sleep(interval)
				// Exponential backoff: double the interval, but cap at maxInterval
				interval = min(interval*2, maxInterval)
				continue
			}
			log.Error("error connecting to DB after retries", "attempts", attempt+1, "err", err)
			return nil, fmt.Errorf("failed to connect to database after %d attempts: %w", attempt+1, err)
		}

		// Try to ping the database
		if err = dbPool.Ping(ctx); err != nil {
			dbPool.Close() // Close the pool if ping fails
			if attempt < maxRetries-1 {
				log.Info("Database ping failed, retrying", "attempt", attempt+1, "max_retries", maxRetries, "retry_in", interval, "err", err)
				time.Sleep(interval)
				// Exponential backoff: double the interval, but cap at maxInterval
				interval = min(interval*2, maxInterval)
				continue
			}
			log.Error("error pinging DB after retries", "attempts", attempt+1, "err", err)
			return nil, fmt.Errorf("failed to ping database after %d attempts: %w", attempt+1, err)
		}

		// Success!
		log.Info("Database connection established", "attempts", attempt+1)
		return dbPool, nil
	}

	// This should never be reached, but included for safety
	return nil, fmt.Errorf("failed to connect to database after %d attempts", maxRetries)
}

// getEnvInt gets an integer from environment variable or returns default
func getEnvInt(key string, defaultValue int) int {
	if val := os.Getenv(key); val != "" {
		if intVal, err := strconv.Atoi(val); err == nil {
			return intVal
		}
	}
	return defaultValue
}

// getEnvDuration gets a duration from environment variable or returns default
func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	if val := os.Getenv(key); val != "" {
		if duration, err := time.ParseDuration(val); err == nil {
			return duration
		}
	}
	return defaultValue
}

// min returns the minimum of two durations
func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
