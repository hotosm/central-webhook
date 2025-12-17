package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/hotosm/central-webhook/db"
	"github.com/matryer/is"
)

// testServer wraps an HTTP server that can be accessed from other Docker containers
type testServer struct {
	server   *http.Server
	listener net.Listener
	URL      string
}

// createTestServer creates an HTTP server bound to 0.0.0.0 so it can be accessed
// from other Docker containers. Returns the server and a URL using the container hostname.
func createTestServer(handler http.Handler) (*testServer, error) {
	// Listen on all interfaces (0.0.0.0) so other containers can connect
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, err
	}

	server := &http.Server{
		Handler: handler,
	}

	// Get the actual port assigned
	port := listener.Addr().(*net.TCPAddr).Port

	// Get container hostname, fallback to service name "webhook" if hostname unavailable
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "webhook" // Use service name from docker-compose
	}

	// Construct URL using container hostname and port
	url := fmt.Sprintf("http://%s:%d", hostname, port)

	ts := &testServer{
		server:   server,
		listener: listener,
		URL:      url,
	}

	// Start the server in a goroutine
	go func() {
		_ = server.Serve(listener)
	}()

	return ts, nil
}

// Close stops the test server
func (ts *testServer) Close() error {
	return ts.server.Close()
}

func getTestDBURI() string {
	dbUri := os.Getenv("CENTRAL_WEBHOOK_DB_URI")
	if len(dbUri) == 0 {
		dbUri = "postgresql://odk:odk@db:5432/odk?sslmode=disable"
	}
	return dbUri
}

func setupTestTables(ctx context.Context, conn *pgxpool.Conn) error {
	// Create entity_defs table
	_, err := conn.Exec(ctx, `DROP TABLE IF EXISTS entity_defs CASCADE;`)
	if err != nil {
		return err
	}
	entityTableSQL := `
		CREATE TABLE entity_defs (
			id int4,
			"entityId" int4,
			"createdAt" timestamptz,
			"current" bool,
			"data" jsonb,
			"creatorId" int4,
			"label" text
		);
	`
	_, err = conn.Exec(ctx, entityTableSQL)
	if err != nil {
		return err
	}

	// Create audits table
	_, err = conn.Exec(ctx, `DROP TABLE IF EXISTS audits CASCADE;`)
	if err != nil {
		return err
	}
	auditTableSQL := `
		CREATE TABLE audits (
			"actorId" int,
			action varchar,
			details jsonb
		);
	`
	_, err = conn.Exec(ctx, auditTableSQL)
	return err
}

func cleanupTestTables(ctx context.Context, conn *pgxpool.Conn) error {
	_, err := conn.Exec(ctx, `DROP TABLE IF EXISTS entity_defs, audits CASCADE;`)
	return err
}

// TestInstallCommand tests that the install command works correctly
func TestInstallCommand(t *testing.T) {
	dbUri := getTestDBURI()
	is := is.New(t)
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx := context.Background()

	pool, err := db.InitPool(ctx, log, dbUri)
	is.NoErr(err)
	defer pool.Close()

	conn, err := pool.Acquire(ctx)
	is.NoErr(err)
	defer conn.Release()

	// Clean up any existing triggers first
	_ = db.RemoveTrigger(ctx, pool, "audits")
	// Small delay to ensure cleanup completes
	time.Sleep(100 * time.Millisecond)

	// Setup test tables
	err = setupTestTables(ctx, conn)
	is.NoErr(err)
	defer func() {
		_ = db.RemoveTrigger(ctx, pool, "audits")
		cleanupTestTables(ctx, conn)
	}()

	// Create mock webhook server
	server, err := createTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	is.NoErr(err)
	defer server.Close()

	// Test install with updateEntityUrl
	updateEntityUrl := server.URL
	err = db.CreateTrigger(ctx, pool, "audits", db.TriggerOptions{
		UpdateEntityURL: &updateEntityUrl,
	})
	is.NoErr(err)

	// Verify trigger exists
	var triggerExists bool
	err = conn.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM pg_trigger 
			WHERE tgname = 'new_audit_log_trigger'
		)
	`).Scan(&triggerExists)
	is.NoErr(err)
	is.True(triggerExists)

	// Verify function exists
	var functionExists bool
	err = conn.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM pg_proc p
			JOIN pg_namespace n ON p.pronamespace = n.oid
			WHERE p.proname = 'new_audit_log' AND n.nspname = 'public'
		)
	`).Scan(&functionExists)
	is.NoErr(err)
	is.True(functionExists)

	// Cleanup already handled in defer
}

// TestUninstallCommand tests that the uninstall command works correctly
func TestUninstallCommand(t *testing.T) {
	dbUri := getTestDBURI()
	is := is.New(t)
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx := context.Background()

	pool, err := db.InitPool(ctx, log, dbUri)
	is.NoErr(err)
	defer pool.Close()

	conn, err := pool.Acquire(ctx)
	is.NoErr(err)
	defer conn.Release()

	// Setup test tables
	err = setupTestTables(ctx, conn)
	is.NoErr(err)
	defer func() {
		cleanupTestTables(ctx, conn)
	}()

	// Create mock webhook server
	server, err := createTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	is.NoErr(err)
	defer server.Close()

	// Install trigger first
	updateEntityUrl := server.URL
	err = db.CreateTrigger(ctx, pool, "audits", db.TriggerOptions{
		UpdateEntityURL: &updateEntityUrl,
	})
	is.NoErr(err)

	// Verify trigger exists
	var triggerExists bool
	err = conn.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM pg_trigger 
			WHERE tgname = 'new_audit_log_trigger'
		)
	`).Scan(&triggerExists)
	is.NoErr(err)
	is.True(triggerExists)

	// Uninstall trigger
	err = db.RemoveTrigger(ctx, pool, "audits")
	is.NoErr(err)

	// Verify trigger is removed
	err = conn.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM pg_trigger 
			WHERE tgname = 'new_audit_log_trigger'
		)
	`).Scan(&triggerExists)
	is.NoErr(err)
	is.True(!triggerExists)

	// Verify function is removed
	var functionExists bool
	err = conn.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM pg_proc p
			JOIN pg_namespace n ON p.pronamespace = n.oid
			WHERE p.proname = 'new_audit_log' AND n.nspname = 'public'
		)
	`).Scan(&functionExists)
	is.NoErr(err)
	is.True(!functionExists)
}

// TestE2EInstallAndTrigger tests end-to-end: install trigger, insert audit record, verify webhook is called
func TestE2EInstallAndTrigger(t *testing.T) {
	dbUri := getTestDBURI()
	is := is.New(t)
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx := context.Background()

	pool, err := db.InitPool(ctx, log, dbUri)
	is.NoErr(err)
	defer pool.Close()

	conn, err := pool.Acquire(ctx)
	is.NoErr(err)
	defer conn.Release()

	// Clean up any existing triggers first
	_ = db.RemoveTrigger(ctx, pool, "audits")
	// Small delay to ensure cleanup completes
	time.Sleep(100 * time.Millisecond)

	// Setup test tables
	err = setupTestTables(ctx, conn)
	is.NoErr(err)
	defer func() {
		_ = db.RemoveTrigger(ctx, pool, "audits")
		cleanupTestTables(ctx, conn)
	}()

	// Insert an entity record
	entityInsertSQL := `
		INSERT INTO public.entity_defs (
			id, "entityId","createdAt","current","data","creatorId","label"
		) VALUES (
		 	1001,
			900,
			'2025-01-10 16:23:40.073',
			true,
			'{"status": "0", "task_id": "26", "version": "1"}',
			5,
			'Task 26 Feature 904487737'
		);
	`
	_, err = conn.Exec(ctx, entityInsertSQL)
	is.NoErr(err)

	// Create HTTP test server to receive webhook
	var receivedPayload map[string]interface{}
	var receivedHeaders http.Header
	var requestReceived sync.WaitGroup
	requestReceived.Add(1)

	server, err := createTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		receivedHeaders = r.Header
		body, err := io.ReadAll(r.Body)
		is.NoErr(err)
		err = json.Unmarshal(body, &receivedPayload)
		is.NoErr(err)
		w.WriteHeader(http.StatusOK)
		requestReceived.Done()
	}))
	is.NoErr(err)
	defer server.Close()

	// Install trigger
	updateEntityUrl := server.URL
	err = db.CreateTrigger(ctx, pool, "audits", db.TriggerOptions{
		UpdateEntityURL: &updateEntityUrl,
	})
	is.NoErr(err)

	// Insert an audit record to trigger the webhook
	auditInsertSQL := `
		INSERT INTO audits ("actorId", action, details)
		VALUES (1, 'entity.update.version', '{"entityDefId": 1001, "entity": {"uuid": "test-uuid-123", "dataset": "test"}}');
	`
	_, err = conn.Exec(ctx, auditInsertSQL)
	is.NoErr(err)

	// Wait for HTTP request to be received (with timeout)
	done := make(chan struct{})
	go func() {
		requestReceived.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Request received successfully
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for webhook request")
	}

	// Validate the webhook payload
	is.True(receivedPayload != nil)
	is.Equal(receivedPayload["type"], "entity.update.version")
	is.Equal(receivedPayload["id"], "test-uuid-123")
	is.True(receivedPayload["data"] != nil)

	// Check nested JSON value for status in data
	data, ok := receivedPayload["data"].(map[string]interface{})
	is.True(ok)
	is.Equal(data["status"], "0")

	// Validate headers
	is.Equal(receivedHeaders.Get("Content-Type"), "application/json")
}

// TestE2EInstallWithAPIKey tests end-to-end with API key authentication
func TestE2EInstallWithAPIKey(t *testing.T) {
	dbUri := getTestDBURI()
	is := is.New(t)
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx := context.Background()

	pool, err := db.InitPool(ctx, log, dbUri)
	is.NoErr(err)
	defer pool.Close()

	conn, err := pool.Acquire(ctx)
	is.NoErr(err)
	defer conn.Release()

	// Clean up any existing triggers first
	_ = db.RemoveTrigger(ctx, pool, "audits")
	// Small delay to ensure cleanup completes
	time.Sleep(100 * time.Millisecond)

	// Setup test tables
	err = setupTestTables(ctx, conn)
	is.NoErr(err)
	defer func() {
		_ = db.RemoveTrigger(ctx, pool, "audits")
		cleanupTestTables(ctx, conn)
	}()

	// Insert an entity record
	entityInsertSQL := `
		INSERT INTO public.entity_defs (
			id, "entityId","createdAt","current","data","creatorId","label"
		) VALUES (
		 	1001,
			900,
			'2025-01-10 16:23:40.073',
			true,
			'{"status": "0"}',
			5,
			'Test Entity'
		);
	`
	_, err = conn.Exec(ctx, entityInsertSQL)
	is.NoErr(err)

	// Create HTTP test server to receive webhook
	var receivedHeaders http.Header
	var requestReceived sync.WaitGroup
	requestReceived.Add(1)

	server, err := createTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header
		w.WriteHeader(http.StatusOK)
		requestReceived.Done()
	}))
	is.NoErr(err)
	defer server.Close()

	// Install trigger with API key
	updateEntityUrl := server.URL
	apiKey := "test-api-key-12345"
	err = db.CreateTrigger(ctx, pool, "audits", db.TriggerOptions{
		UpdateEntityURL: &updateEntityUrl,
		APIKey:          &apiKey,
	})
	is.NoErr(err)

	// Insert an audit record to trigger the webhook
	auditInsertSQL := `
		INSERT INTO audits ("actorId", action, details)
		VALUES (1, 'entity.update.version', '{"entityDefId": 1001, "entity": {"uuid": "test-uuid", "dataset": "test"}}');
	`
	_, err = conn.Exec(ctx, auditInsertSQL)
	is.NoErr(err)

	// Wait for HTTP request to be received
	done := make(chan struct{})
	go func() {
		requestReceived.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Request received successfully
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for webhook request")
	}

	// Validate API key header
	is.Equal(receivedHeaders.Get("X-API-Key"), "test-api-key-12345")
	is.Equal(receivedHeaders.Get("Content-Type"), "application/json")
}

// TestE2EInstallMultipleWebhooks tests installing multiple webhook types
func TestE2EInstallMultipleWebhooks(t *testing.T) {
	dbUri := getTestDBURI()
	is := is.New(t)
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx := context.Background()

	pool, err := db.InitPool(ctx, log, dbUri)
	is.NoErr(err)
	defer pool.Close()

	conn, err := pool.Acquire(ctx)
	is.NoErr(err)
	defer conn.Release()

	// Clean up any existing triggers first
	_ = db.RemoveTrigger(ctx, pool, "audits")
	// Small delay to ensure cleanup completes
	time.Sleep(100 * time.Millisecond)

	// Setup test tables
	err = setupTestTables(ctx, conn)
	is.NoErr(err)
	defer func() {
		_ = db.RemoveTrigger(ctx, pool, "audits")
		cleanupTestTables(ctx, conn)
		conn.Exec(ctx, `DROP TABLE IF EXISTS submission_defs CASCADE;`)
	}()

	// Create submission_defs table
	_, err = conn.Exec(ctx, `DROP TABLE IF EXISTS submission_defs CASCADE;`)
	is.NoErr(err)
	submissionTableSQL := `
		CREATE TABLE submission_defs (
			id int4,
			"submissionId" int4,
			"instanceId" uuid,
			xml text,
			"formDefId" int4,
			"submitterId" int4,
			"createdAt" timestamptz
		);
	`
	_, err = conn.Exec(ctx, submissionTableSQL)
	is.NoErr(err)

	// Insert test data
	entityInsertSQL := `
		INSERT INTO public.entity_defs (
			id, "entityId","createdAt","current","data","creatorId","label"
		) VALUES (
		 	1001,
			900,
			'2025-01-10 16:23:40.073',
			true,
			'{"status": "0"}',
			5,
			'Test Entity'
		);
	`
	_, err = conn.Exec(ctx, entityInsertSQL)
	is.NoErr(err)

	submissionInsertSQL := `
		INSERT INTO submission_defs (
			id, "submissionId", xml, "formDefId", "submitterId", "createdAt"
		) VALUES (
		 	1,
            2,
			'<data id="xxx">',
			7,
			5,
			'2025-01-10 16:23:40.073'
		);
	`
	_, err = conn.Exec(ctx, submissionInsertSQL)
	is.NoErr(err)

	// Create HTTP test servers
	var entityPayload map[string]interface{}
	var submissionPayload map[string]interface{}
	var entityReceived sync.WaitGroup
	var submissionReceived sync.WaitGroup
	entityReceived.Add(1)
	submissionReceived.Add(1)

	entityServer, err := createTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		is.NoErr(err)
		err = json.Unmarshal(body, &entityPayload)
		is.NoErr(err)
		w.WriteHeader(http.StatusOK)
		entityReceived.Done()
	}))
	is.NoErr(err)
	defer entityServer.Close()

	submissionServer, err := createTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		is.NoErr(err)
		err = json.Unmarshal(body, &submissionPayload)
		is.NoErr(err)
		w.WriteHeader(http.StatusOK)
		submissionReceived.Done()
	}))
	is.NoErr(err)
	defer submissionServer.Close()

	// Install triggers with multiple webhook URLs
	updateEntityUrl := entityServer.URL
	newSubmissionUrl := submissionServer.URL
	err = db.CreateTrigger(ctx, pool, "audits", db.TriggerOptions{
		UpdateEntityURL:  &updateEntityUrl,
		NewSubmissionURL: &newSubmissionUrl,
	})
	is.NoErr(err)

	// Trigger entity update
	entityAuditSQL := `
		INSERT INTO audits ("actorId", action, details)
		VALUES (1, 'entity.update.version', '{"entityDefId": 1001, "entity": {"uuid": "test-uuid-entity", "dataset": "test"}}');
	`
	_, err = conn.Exec(ctx, entityAuditSQL)
	is.NoErr(err)

	// Trigger submission create
	submissionAuditSQL := `
		INSERT INTO audits ("actorId", action, details)
		VALUES (5, 'submission.create', '{"submissionDefId": 1, "instanceId": "test-instance-123"}');
	`
	_, err = conn.Exec(ctx, submissionAuditSQL)
	is.NoErr(err)

	// Wait for both webhooks
	done := make(chan struct{})
	go func() {
		entityReceived.Wait()
		submissionReceived.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Both requests received successfully
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for webhook requests")
	}

	// Validate entity webhook
	is.True(entityPayload != nil)
	is.Equal(entityPayload["type"], "entity.update.version")
	is.Equal(entityPayload["id"], "test-uuid-entity")

	// Validate submission webhook
	is.True(submissionPayload != nil)
	is.Equal(submissionPayload["type"], "submission.create")
	is.Equal(submissionPayload["id"], "test-instance-123")
}
