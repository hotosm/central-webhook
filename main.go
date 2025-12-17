package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/hotosm/central-webhook/db"
)

func getDefaultLogger(lvl slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		AddSource: true,
		Level:     lvl,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.SourceKey {
				source, _ := a.Value.Any().(*slog.Source)
				if source != nil {
					source.Function = ""
					source.File = filepath.Base(source.File)
				}
			}
			return a
		},
	}))
}

func main() {
	ctx := context.Background()

	// Read environment variables
	defaultDbUri := os.Getenv("CENTRAL_WEBHOOK_DB_URI")
	defaultUpdateEntityUrl := os.Getenv("CENTRAL_WEBHOOK_UPDATE_ENTITY_URL")
	defaultNewSubmissionUrl := os.Getenv("CENTRAL_WEBHOOK_NEW_SUBMISSION_URL")
	defaultReviewSubmissionUrl := os.Getenv("CENTRAL_WEBHOOK_REVIEW_SUBMISSION_URL")
	defaultApiKey := os.Getenv("CENTRAL_WEBHOOK_API_KEY")
	defaultLogLevel := os.Getenv("CENTRAL_WEBHOOK_LOG_LEVEL")

	// Database connection
	var dbUri string
	flag.StringVar(&dbUri, "db", defaultDbUri, "DB URI (postgresql://{user}:{password}@{hostname}/{db}?sslmode=disable)")

	// Webhook URLs
	var updateEntityUrl string
	flag.StringVar(&updateEntityUrl, "updateEntityUrl", defaultUpdateEntityUrl, "Webhook URL for update entity events")

	var newSubmissionUrl string
	flag.StringVar(&newSubmissionUrl, "newSubmissionUrl", defaultNewSubmissionUrl, "Webhook URL for new submission events")

	var reviewSubmissionUrl string
	flag.StringVar(&reviewSubmissionUrl, "reviewSubmissionUrl", defaultReviewSubmissionUrl, "Webhook URL for review submission events")

	// API Key
	var apiKey string
	flag.StringVar(&apiKey, "apiKey", defaultApiKey, "X-API-Key header value for authenticating with webhook API")

	// Logging
	var debug bool
	flag.BoolVar(&debug, "debug", false, "Enable debug logging")

	// Check if first argument is a command (allows flags after command)
	var command string
	if len(os.Args) > 1 {
		firstArg := os.Args[1]
		if firstArg == "install" || firstArg == "uninstall" {
			command = firstArg
			// Temporarily remove command from os.Args so flag.Parse() can parse remaining flags
			originalArgs := os.Args
			os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
			flag.Parse()
			os.Args = originalArgs // Restore for potential future use
		} else {
			// Command not first, parse flags normally
			flag.Parse()
			args := flag.Args()
			if len(args) == 0 {
				fmt.Fprintf(os.Stderr, "Error: command is required. Use 'install' or 'uninstall'\n")
				fmt.Fprintf(os.Stderr, "Usage: %s <command> [flags] or %s [flags] <command>\n", os.Args[0], os.Args[0])
				flag.PrintDefaults()
				os.Exit(1)
			}
			command = args[0]
		}
	} else {
		// No arguments at all
		flag.Parse()
		args := flag.Args()
		if len(args) == 0 {
			fmt.Fprintf(os.Stderr, "Error: command is required. Use 'install' or 'uninstall'\n")
			fmt.Fprintf(os.Stderr, "Usage: %s <command> [flags] or %s [flags] <command>\n", os.Args[0], os.Args[0])
			flag.PrintDefaults()
			os.Exit(1)
		}
		command = args[0]
	}

	// Validate command
	if command != "install" && command != "uninstall" {
		fmt.Fprintf(os.Stderr, "Error: command must be either 'install' or 'uninstall', got: %s\n", command)
		os.Exit(1)
	}

	// Set logging level
	var logLevel slog.Level
	if debug {
		logLevel = slog.LevelDebug
	} else if strings.ToLower(defaultLogLevel) == "debug" {
		logLevel = slog.LevelDebug
	} else {
		logLevel = slog.LevelInfo
	}
	log := getDefaultLogger(logLevel)

	// Validate database URI
	if dbUri == "" {
		fmt.Fprintf(os.Stderr, "Error: DB URI is required\n")
		flag.PrintDefaults()
		os.Exit(1)
	}

	// For install command, validate at least one webhook URL is provided
	if command == "install" {
		if updateEntityUrl == "" && newSubmissionUrl == "" && reviewSubmissionUrl == "" {
			fmt.Fprintf(os.Stderr, "Error: At least one webhook URL is required for install command\n")
			fmt.Fprintf(os.Stderr, "  Provide at least one of: -updateEntityUrl, -newSubmissionUrl, -reviewSubmissionUrl\n")
			flag.PrintDefaults()
			os.Exit(1)
		}
	}

	// Get a connection pool
	dbPool, err := db.InitPool(ctx, log, dbUri)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: could not connect to database: %v\n", err)
		os.Exit(1)
	}
	defer dbPool.Close()

	// Execute command
	switch command {
	case "install":
		var apiKeyPtr *string
		if apiKey != "" {
			apiKeyPtr = &apiKey
		}

		var updateEntityUrlPtr *string
		if updateEntityUrl != "" {
			updateEntityUrlPtr = &updateEntityUrl
		}

		var newSubmissionUrlPtr *string
		if newSubmissionUrl != "" {
			newSubmissionUrlPtr = &newSubmissionUrl
		}

		var reviewSubmissionUrlPtr *string
		if reviewSubmissionUrl != "" {
			reviewSubmissionUrlPtr = &reviewSubmissionUrl
		}

		log.Info("Installing webhook triggers")
		err = db.CreateTrigger(ctx, dbPool, "audits", db.TriggerOptions{
			UpdateEntityURL:     updateEntityUrlPtr,
			NewSubmissionURL:    newSubmissionUrlPtr,
			ReviewSubmissionURL: reviewSubmissionUrlPtr,
			APIKey:              apiKeyPtr,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: failed to install triggers: %v\n", err)
			os.Exit(1)
		}
		log.Info("Webhook triggers installed successfully")

	case "uninstall":
		log.Info("Uninstalling webhook triggers")
		err = db.RemoveTrigger(ctx, dbPool, "audits")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: failed to uninstall triggers: %v\n", err)
			os.Exit(1)
		}
		log.Info("Webhook triggers uninstalled successfully")
	}
}
