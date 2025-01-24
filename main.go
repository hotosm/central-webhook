// Wrapper for the main tool functionality

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hotosm/central-webhook/db"
	"github.com/hotosm/central-webhook/parser"
	"github.com/hotosm/central-webhook/webhook"
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

func SetupWebhook(
	log *slog.Logger,
	ctx context.Context,
	dbPool *pgxpool.Pool,
	entityUrl, submissionUrl string,
) error {
	// setup the listener
	listener := db.NewListener(dbPool)
	if err := listener.Connect(ctx); err != nil {
		log.Error("error setting up listener: %v", "error", err)
		return err
	}

	// init the trigger function
	db.CreateTrigger(ctx, dbPool, "audits")

	// setup the notifier
	notifier := db.NewNotifier(log, listener)
	go notifier.Run(ctx)

	// subscribe to the 'odk-events' channel
	log.Info("listening to odk-events channel")
	sub := notifier.Listen("odk-events")

	// indefinitely listen for updates
	go func() {
		<-sub.EstablishedC()
		for {
			select {
			case <-ctx.Done():
				sub.Unlisten(ctx)
				log.Info("done listening for notifications")
				return

			case data := <-sub.NotificationC():
				eventData := string(data)
				log.Debug("got notification", "data", eventData)

				parsedData, err := parser.ParseEventJson(log, ctx, []byte(eventData))
				if err != nil {
					log.Error("Failed to parse notification", "error", err)
					continue // Skip processing this notification
				}

				if parsedData != nil {
					// Only send the request for correctly parsed (supported) events
					webhook.SendRequest(log, ctx, entityUrl, *parsedData)
				}
			}
		}
	}()

	// unsubscribe after 60s
	// go func() {
	// 	time.Sleep(3 * time.Second)
	// 	sub.Unlisten(ctx)
	// }()

	stopCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Listen for termination signals (e.g., SIGINT/SIGTERM)
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		<-c
		log.Info("Received shutdown signal")
		cancel()
	}()

	<-stopCtx.Done()
	log.Info("Application shutting down")

	return nil
}

func printStartupMsg() {
	banner := `
   ____  _____  _  __ __          __  _     _                 _    
  / __ \|  __ \| |/ / \ \        / / | |   | |               | |   
 | |  | | |  | | ' /   \ \  /\  / /__| |__ | |__   ___   ___ | | __
 | |  | | |  | |  <     \ \/  \/ / _ \ '_ \| '_ \ / _ \ / _ \| |/ /
 | |__| | |__| | . \     \  /\  /  __/ |_) | | | | (_) | (_) |   < 
  \____/|_____/|_|\_\     \/  \/ \___|_.__/|_| |_|\___/ \___/|_|\_\                                                          
	`
	fmt.Println(banner)
	fmt.Println("")
}

func main() {
	ctx := context.Background()

	// Read environment variables
	defaultDbUri := os.Getenv("CENTRAL_WEBHOOK_DB_URI")
	defaultEntityUrl := os.Getenv("CENTRAL_WEBHOOK_ENTITY_URL")
	defaultSubmissionUrl := os.Getenv("CENTRAL_WEBHOOK_SUBMISSION_URL")
	defaultLogLevel := os.Getenv("CENTRAL_WEBHOOK_LOG_LEVEL")

	var dbUri string
	flag.StringVar(&dbUri, "db", defaultDbUri, "DB host (postgresql://{user}:{password}@{hostname}/{db}?sslmode=disable)")

	var entityUrl string
	flag.StringVar(&entityUrl, "entityUrl", defaultEntityUrl, "Webhook URL for entity events")

	var submissionUrl string
	flag.StringVar(&submissionUrl, "submissionUrl", defaultSubmissionUrl, "Webhook URL for submission events")

	var debug bool
	flag.BoolVar(&debug, "debug", false, "Enable debug logging")

	flag.Parse()

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

	if dbUri == "" {
		fmt.Fprintf(os.Stderr, "DB URI is required\n")
		flag.PrintDefaults()
		os.Exit(1)
	}

	if entityUrl == "" && submissionUrl == "" {
		fmt.Fprintf(os.Stderr, "At least one of entityUrl or submissionUrl is required\n")
		flag.PrintDefaults()
		os.Exit(1)
	}

	// Get a connection pool
	dbPool, err := db.InitPool(ctx, log, dbUri)
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not connect to database: %v", err)
		os.Exit(1)
	}

	printStartupMsg()
	err = SetupWebhook(log, ctx, dbPool, entityUrl, submissionUrl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error setting up webhook: %v", err)
		os.Exit(1)
	}
}
