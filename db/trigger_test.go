package db

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matryer/is"
)

// Note: these tests assume you have a postgres server listening on db:5432
// with username odk and password odk.
//
// The easiest way to ensure this is to run the tests with docker compose:
// docker compose run --rm webhook

// testServer wraps an HTTP server that can be accessed from other Docker containers
type testServer struct {
	server   *http.Server
	listener net.Listener
	URL      string
}

func createTestServer(handler http.Handler) (*testServer, error) {
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, err
	}

	server := &http.Server{
		Handler: handler,
	}

	port := listener.Addr().(*net.TCPAddr).Port

	// Try container hostname first for CI, but fallback to compose service
	// name for local runs
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "webhook" // docker-compose service name
	}

	url := fmt.Sprintf("http://%s:%d", hostname, port)


	ts := &testServer{
		server:   server,
		listener: listener,
		URL:      url,
	}

	go func() {
		_ = server.Serve(listener)
	}()

	return ts, nil
}

// Close stops the test server
func (ts *testServer) Close() error {
	return ts.server.Close()
}

func createAuditTestsTable(ctx context.Context, conn *pgxpool.Conn, is *is.I) {
	_, err := conn.Exec(ctx, `DROP TABLE IF EXISTS audits_test CASCADE;`)
	is.NoErr(err)
	auditTableCreateSql := `
		CREATE TABLE audits_test (
			id SERIAL PRIMARY KEY,
			"actorId" int,
			action varchar,
			details jsonb
		);
	`
	_, err = conn.Exec(ctx, auditTableCreateSql)
	is.NoErr(err)
}

func createSubmissionDefsTable(ctx context.Context, conn *pgxpool.Conn, is *is.I) {
	_, err := conn.Exec(ctx, `DROP TABLE IF EXISTS submission_defs CASCADE;`)
	is.NoErr(err)
	submissionTableCreateSql := `
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
	_, err = conn.Exec(ctx, submissionTableCreateSql)
	is.NoErr(err)
}

func TestEntityTrigger(t *testing.T) {
	dbUri := os.Getenv("CENTRAL_WEBHOOK_DB_URI")
	if len(dbUri) == 0 {
		// Default
		dbUri = "postgresql://odk:odk@db:5432/odk?sslmode=disable"
	}

	is := is.New(t)
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx := context.Background()
	pool, err := InitPool(ctx, log, dbUri)
	is.NoErr(err)
	defer pool.Close()

	// Get connection and defer close
	conn, err := pool.Acquire(ctx)
	is.NoErr(err)
	defer conn.Release()

	// Create entity_defs table
	_, err = conn.Exec(ctx, `DROP TABLE IF EXISTS entity_defs CASCADE;`)
	is.NoErr(err)
	entityTableCreateSql := `
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
	_, err = conn.Exec(ctx, entityTableCreateSql)
	is.NoErr(err)

	// Create audits_test table
	createAuditTestsTable(ctx, conn, is)

	// Create audit trigger pointing at a dummy URL – we just inspect the SQL
	dummyURL := "http://example.com/webhook"
	err = CreateTrigger(ctx, pool, "audits_test", TriggerOptions{
		UpdateEntityURL: &dummyURL,
	})
	is.NoErr(err)

	// Read back the generated function and assert the entity CASE branch exists
	var functionSQL string
	row := conn.QueryRow(ctx, `
		SELECT pg_get_functiondef(p.oid)
		FROM pg_proc p
		JOIN pg_namespace n ON p.pronamespace = n.oid
		WHERE p.proname = 'new_audit_log' AND n.nspname = 'public'
	`)
	err = row.Scan(&functionSQL)
	is.NoErr(err)

	is.True(strings.Contains(functionSQL, "WHEN 'entity.update.version'"))
	is.True(strings.Contains(functionSQL, "'type', 'entity.update.version'"))
	is.True(strings.Contains(functionSQL, "'id', (NEW.details->'entity'->>'uuid')"))
	is.True(strings.Contains(functionSQL, "'data', result_data"))

	// Cleanup
	err = RemoveTrigger(ctx, pool, "audits_test")
	is.NoErr(err)
	_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS entity_defs, audits_test CASCADE;`)
}

// Test a new submission event type
func TestNewSubmissionTrigger(t *testing.T) {
	dbUri := os.Getenv("CENTRAL_WEBHOOK_DB_URI")
	if len(dbUri) == 0 {
		// Default
		dbUri = "postgresql://odk:odk@db:5432/odk?sslmode=disable"
	}

	is := is.New(t)
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx := context.Background()
	pool, err := InitPool(ctx, log, dbUri)
	is.NoErr(err)
	defer pool.Close()

	// Get connection and defer close
	conn, err := pool.Acquire(ctx)
	is.NoErr(err)
	defer conn.Release()
	_, _ = conn.Exec(ctx, `DROP FUNCTION IF EXISTS new_audit_log() CASCADE;`)

	// Create submission_defs table
	createSubmissionDefsTable(ctx, conn, is)

	// Create audits_test table
	createAuditTestsTable(ctx, conn, is)

	// Insert an submission record
	submissionInsertSql := `
		INSERT INTO submission_defs (
			id,
			"submissionId",
			xml,
			"formDefId",
			"submitterId",
			"createdAt"
		) VALUES (
		 	1,
            2,
			'<data id="xxx">',
			7,
			5,
			'2025-01-10 16:23:40.073'
		);
	`
	_, err = conn.Exec(ctx, submissionInsertSql)
	is.NoErr(err)

	// Create HTTP test server to receive webhook
	var receivedPayload map[string]interface{}
	var requestReceived sync.WaitGroup
	requestReceived.Add(1)

	server, err := createTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		is.NoErr(err)
		err = json.Unmarshal(body, &receivedPayload)
		is.NoErr(err)
		w.WriteHeader(http.StatusOK)
		requestReceived.Done()
	}))
	is.NoErr(err)
	defer server.Close()

	// Create audit trigger
	newSubmissionUrl := server.URL
	err = CreateTrigger(ctx, pool, "audits_test", TriggerOptions{
		NewSubmissionURL: &newSubmissionUrl,
	})
	is.NoErr(err)

	// Insert an audit record
	auditInsertSql := `
		INSERT INTO audits_test ("actorId", action, details)
		VALUES (5, 'submission.create', '{"submissionDefId": 1, "instanceId": "test-instance-123"}');
	`
	_, err = conn.Exec(ctx, auditInsertSql)
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

	// Validate the webhook payload
	is.True(receivedPayload != nil)
	is.Equal(receivedPayload["type"], "submission.create")
	is.Equal(receivedPayload["id"], "test-instance-123")
	is.True(receivedPayload["data"] != nil)

	data, ok := receivedPayload["data"].(map[string]interface{})
	is.True(ok)                              // Ensure data is a valid map
	is.Equal(data["xml"], `<data id="xxx">`) // Ensure `xml` has the correct value

	// Cleanup
	err = RemoveTrigger(ctx, pool, "audits_test")
	is.NoErr(err)
	_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS submission_defs, audits_test CASCADE;`)
}

// Test a review submission event type
func TestReviewSubmissionTrigger(t *testing.T) {
	dbUri := os.Getenv("CENTRAL_WEBHOOK_DB_URI")
	if len(dbUri) == 0 {
		// Default
		dbUri = "postgresql://odk:odk@db:5432/odk?sslmode=disable"
	}

	is := is.New(t)
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx := context.Background()
	pool, err := InitPool(ctx, log, dbUri)
	is.NoErr(err)
	defer pool.Close()

	// Get connection and defer close
	conn, err := pool.Acquire(ctx)
	is.NoErr(err)
	defer conn.Release()

	// Create submission_defs table
	createSubmissionDefsTable(ctx, conn, is)

	// Create audits_test table
	createAuditTestsTable(ctx, conn, is)

	// Insert an submission record
	submissionInsertSql := `
		INSERT INTO submission_defs (
			id,
			"submissionId",
			"instanceId"
		) VALUES (
		 	1,
            2,
			'33448049-0df1-4426-9392-d3a294d638ad'
		);
	`
	_, err = conn.Exec(ctx, submissionInsertSql)
	is.NoErr(err)

	// Create HTTP test server to receive webhook
	var receivedPayload map[string]interface{}
	var requestReceived sync.WaitGroup
	requestReceived.Add(1)

	server, err := createTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		is.NoErr(err)
		err = json.Unmarshal(body, &receivedPayload)
		is.NoErr(err)
		w.WriteHeader(http.StatusOK)
		requestReceived.Done()
	}))
	is.NoErr(err)
	defer server.Close()

	// Create audit trigger
	reviewSubmissionUrl := server.URL
	err = CreateTrigger(ctx, pool, "audits_test", TriggerOptions{
		ReviewSubmissionURL: &reviewSubmissionUrl,
	})
	is.NoErr(err)

	// Insert an audit record
	auditInsertSql := `
		INSERT INTO audits_test ("actorId", action, details)
		VALUES (5, 'submission.update', '{"submissionDefId": 1, "instanceId": "33448049-0df1-4426-9392-d3a294d638ad", "reviewState": "approved"}');
	`
	_, err = conn.Exec(ctx, auditInsertSql)
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

	// Validate the webhook payload
	is.True(receivedPayload != nil)
	is.Equal(receivedPayload["type"], "submission.update")
	is.Equal(receivedPayload["id"], "33448049-0df1-4426-9392-d3a294d638ad")

	// Check reviewState present in data key
	data, ok := receivedPayload["data"].(map[string]interface{})
	is.True(ok)                               // Ensure data is a valid map
	is.Equal(data["reviewState"], "approved") // Ensure reviewState has the correct value

	// Cleanup
	err = RemoveTrigger(ctx, pool, "audits_test")
	is.NoErr(err)
	_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS submission_defs, audits_test CASCADE;`)
}

// Test an unsupported event type and ensure nothing is triggered
func TestNoTrigger(t *testing.T) {
	dbUri := os.Getenv("CENTRAL_WEBHOOK_DB_URI")
	if len(dbUri) == 0 {
		// Default
		dbUri = "postgresql://odk:odk@db:5432/odk?sslmode=disable"
	}

	is := is.New(t)
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx := context.Background()
	pool, err := InitPool(ctx, log, dbUri)
	is.NoErr(err)
	defer pool.Close()

	// Get connection and defer close
	conn, err := pool.Acquire(ctx)
	is.NoErr(err)
	defer conn.Release()

	// Create audits_test table
	createAuditTestsTable(ctx, conn, is)

	// Create HTTP test server to receive webhook
	var requestReceived sync.WaitGroup
	requestReceived.Add(1)

	server, err := createTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestReceived.Done()
		w.WriteHeader(http.StatusOK)
	}))
	is.NoErr(err)
	defer server.Close()

	// Create audit trigger
	newSubmissionUrl := server.URL
	err = CreateTrigger(ctx, pool, "audits_test", TriggerOptions{
		NewSubmissionURL: &newSubmissionUrl,
	})
	is.NoErr(err)

	// Insert an audit record with unsupported event type
	auditInsertSql := `
		INSERT INTO audits_test ("actorId", action, details)
		VALUES (1, 'invalid.event', '{"submissionDefId": 5}');
	`
	_, err = conn.Exec(ctx, auditInsertSql)
	is.NoErr(err)

	// Validate that no event was triggered for invalid event type
	// The HTTP server should not receive a request
	select {
	case <-time.After(2 * time.Second):
		// No request received - this is expected
		log.Info("no event triggered for invalid event type")
	case <-func() chan struct{} {
		done := make(chan struct{})
		go func() {
			requestReceived.Wait()
			close(done)
		}()
		return done
	}():
		// If a request was received, we failed the test
		t.Fatal("unexpected webhook request received for invalid event type")
	}

	// Cleanup - only drop audits_test since submission_defs wasn't created in this test
	err = RemoveTrigger(ctx, pool, "audits_test")
	is.NoErr(err)
	_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS audits_test CASCADE;`)
}

// Test that only the related CASE statements are added to the SQL function
func TestModularSql(t *testing.T) {
	dbUri := os.Getenv("CENTRAL_WEBHOOK_DB_URI")
	if len(dbUri) == 0 {
		// Default
		dbUri = "postgresql://odk:odk@db:5432/odk?sslmode=disable"
	}

	is := is.New(t)
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx := context.Background()
	pool, err := InitPool(ctx, log, dbUri)
	is.NoErr(err)
	defer pool.Close()

	// Get connection and defer close
	conn, err := pool.Acquire(ctx)
	is.NoErr(err)
	defer conn.Release()

	// Create audits_test table
	createAuditTestsTable(ctx, conn, is)

	// Create audit trigger
	updateEntityUrl := "https://test.com"
	err = CreateTrigger(ctx, pool, "audits_test", TriggerOptions{
		UpdateEntityURL: &updateEntityUrl,
	})
	is.NoErr(err)

	// Verify the function only contains the entity.update.version CASE
	var functionSQL string
	row := conn.QueryRow(ctx, `
		SELECT pg_get_functiondef(p.oid)
		FROM pg_proc p
		JOIN pg_namespace n ON p.pronamespace = n.oid
		WHERE p.proname = 'new_audit_log' AND n.nspname = 'public'
	`)
	err = row.Scan(&functionSQL)
	is.NoErr(err)

	// Check that the expected CASE branch is present
	is.True(strings.Contains(functionSQL, "WHEN 'entity.update.version'"))
	// Ensure that other cases are not present
	is.True(!strings.Contains(functionSQL, "WHEN 'submission.create'"))
	is.True(!strings.Contains(functionSQL, "WHEN 'submission.update'"))

	// Cleanup
	err = RemoveTrigger(ctx, pool, "audits_test")
	is.NoErr(err)
	_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS audits_test CASCADE;`)
}

// Test RemoveTrigger function
func TestRemoveTrigger(t *testing.T) {
	dbUri := os.Getenv("CENTRAL_WEBHOOK_DB_URI")
	if len(dbUri) == 0 {
		// Default
		dbUri = "postgresql://odk:odk@db:5432/odk?sslmode=disable"
	}

	is := is.New(t)
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx := context.Background()
	pool, err := InitPool(ctx, log, dbUri)
	is.NoErr(err)
	defer pool.Close()

	conn, err := pool.Acquire(ctx)
	is.NoErr(err)
	defer conn.Release()

	// Create audits_test table
	createAuditTestsTable(ctx, conn, is)

	// Create trigger
	updateEntityUrl := "https://test.com"
	err = CreateTrigger(ctx, pool, "audits_test", TriggerOptions{
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

	// Remove trigger
	err = RemoveTrigger(ctx, pool, "audits_test")
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
	err = conn.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM pg_proc p
			JOIN pg_namespace n ON p.pronamespace = n.oid
			WHERE p.proname = 'new_audit_log' AND n.nspname = 'public'
		)
	`).Scan(&functionExists)
	is.NoErr(err)
	is.True(!functionExists)

	// Cleanup
	_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS audits_test CASCADE;`)
}

// Test API key is included in headers
func TestTriggerWithAPIKey(t *testing.T) {
	dbUri := os.Getenv("CENTRAL_WEBHOOK_DB_URI")
	if len(dbUri) == 0 {
		// Default
		dbUri = "postgresql://odk:odk@db:5432/odk?sslmode=disable"
	}

	is := is.New(t)
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx := context.Background()
	pool, err := InitPool(ctx, log, dbUri)
	is.NoErr(err)
	defer pool.Close()

	conn, err := pool.Acquire(ctx)
	is.NoErr(err)
	defer conn.Release()

	// Create entity_defs table
	_, err = conn.Exec(ctx, `DROP TABLE IF EXISTS entity_defs CASCADE;`)
	is.NoErr(err)
	entityTableCreateSql := `
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
	_, err = conn.Exec(ctx, entityTableCreateSql)
	is.NoErr(err)

	// Create audits_test table
	createAuditTestsTable(ctx, conn, is)

	// Insert an entity record
	entityInsertSql := `
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
	_, err = conn.Exec(ctx, entityInsertSql)
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

	// Create audit trigger with API key
	updateEntityUrl := server.URL
	apiKey := "test-api-key-12345"
	err = CreateTrigger(ctx, pool, "audits_test", TriggerOptions{
		UpdateEntityURL: &updateEntityUrl,
		APIKey:          &apiKey,
	})
	is.NoErr(err)

	// Insert an audit record to trigger the webhook
	auditInsertSql := `
		INSERT INTO audits_test ("actorId", action, details)
		VALUES (1, 'entity.update.version', '{"entityDefId": 1001, "entity": {"uuid": "test-uuid", "dataset": "test"}}');
	`
	_, err = conn.Exec(ctx, auditInsertSql)
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

	// Cleanup
	err = RemoveTrigger(ctx, pool, "audits_test")
	is.NoErr(err)
	_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS entity_defs, audits_test CASCADE;`)
}

// Verifies only one webhook is sent for duplicate entity updates
func TestDeduplicationEntityUpdate(t *testing.T) {
	dbUri := os.Getenv("CENTRAL_WEBHOOK_DB_URI")
	if len(dbUri) == 0 {
		dbUri = "postgresql://odk:odk@db:5432/odk?sslmode=disable"
	}

	is := is.New(t)
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx := context.Background()
	pool, err := InitPool(ctx, log, dbUri)
	is.NoErr(err)
	defer pool.Close()

	conn, err := pool.Acquire(ctx)
	is.NoErr(err)
	defer conn.Release()

	// Create entity_defs table
	_, err = conn.Exec(ctx, `DROP TABLE IF EXISTS entity_defs CASCADE;`)
	is.NoErr(err)
	entityTableCreateSql := `
		CREATE TABLE entity_defs (
			id int4,
			"entityId" int4,
			"data" jsonb
		);
	`
	_, err = conn.Exec(ctx, entityTableCreateSql)
	is.NoErr(err)

	// Insert entity record
	entityInsertSql := `
		INSERT INTO entity_defs (id, "entityId", "data")
		VALUES (1001, 900, '{"status": "active"}');
	`
	_, err = conn.Exec(ctx, entityInsertSql)
	is.NoErr(err)

	// Create audits_test table
	createAuditTestsTable(ctx, conn, is)

	// Track webhook calls
	var webhookCallCount int
	var mu sync.Mutex
	var requestReceived sync.WaitGroup
	requestReceived.Add(1) // Expect only 1 call

	server, err := createTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		webhookCallCount++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		if webhookCallCount == 1 {
			requestReceived.Done()
		}
	}))
	is.NoErr(err)
	defer server.Close()

	// Create trigger
	updateEntityUrl := server.URL
	err = CreateTrigger(ctx, pool, "audits_test", TriggerOptions{
		UpdateEntityURL: &updateEntityUrl,
	})
	is.NoErr(err)

	// Insert the same audit event 3 times (simulating duplicate events)
	for i := 0; i < 3; i++ {
		auditInsertSql := `
			INSERT INTO audits_test ("actorId", action, details)
			VALUES (1, 'entity.update.version', '{"entityDefId": 1001, "entity": {"uuid": "test-uuid-123"}}');
		`
		_, err = conn.Exec(ctx, auditInsertSql)
		is.NoErr(err)
	}

	// Wait for webhook(s)
	done := make(chan struct{})
	go func() {
		requestReceived.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Expected - got first webhook
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for webhook request")
	}

	// Wait a bit more to ensure no additional webhooks arrive
	time.Sleep(2 * time.Second)

	mu.Lock()
	finalCount := webhookCallCount
	mu.Unlock()

	// Should only have received 1 webhook despite 3 audit inserts
	is.Equal(finalCount, 1)

	// Cleanup
	err = RemoveTrigger(ctx, pool, "audits_test")
	is.NoErr(err)
	_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS entity_defs, audits_test CASCADE;`)
}

// Verifies only one webhook for duplicate submission creates
func TestDeduplicationSubmissionCreate(t *testing.T) {
	dbUri := os.Getenv("CENTRAL_WEBHOOK_DB_URI")
	if len(dbUri) == 0 {
		dbUri = "postgresql://odk:odk@db:5432/odk?sslmode=disable"
	}

	is := is.New(t)
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx := context.Background()
	pool, err := InitPool(ctx, log, dbUri)
	is.NoErr(err)
	defer pool.Close()

	conn, err := pool.Acquire(ctx)
	is.NoErr(err)
	defer conn.Release()

	// Create tables
	createSubmissionDefsTable(ctx, conn, is)
	createAuditTestsTable(ctx, conn, is)

	// Insert submission record
	submissionInsertSql := `
		INSERT INTO submission_defs (id, "submissionId", xml)
		VALUES (1, 2, '<data id="test"/>');
	`
	_, err = conn.Exec(ctx, submissionInsertSql)
	is.NoErr(err)

	// Track webhook calls
	var webhookCallCount int
	var mu sync.Mutex
	var requestReceived sync.WaitGroup
	requestReceived.Add(1)

	server, err := createTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		webhookCallCount++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		if webhookCallCount == 1 {
			requestReceived.Done()
		}
	}))
	is.NoErr(err)
	defer server.Close()

	// Create trigger
	newSubmissionUrl := server.URL
	err = CreateTrigger(ctx, pool, "audits_test", TriggerOptions{
		NewSubmissionURL: &newSubmissionUrl,
	})
	is.NoErr(err)

	// Insert duplicate audit events for the same instanceId
	instanceId := "dup-test-instance-456"
	for i := 0; i < 3; i++ {
		auditInsertSql := fmt.Sprintf(`
			INSERT INTO audits_test ("actorId", action, details)
			VALUES (5, 'submission.create', '{"submissionDefId": 1, "instanceId": "%s"}');
		`, instanceId)
		_, err = conn.Exec(ctx, auditInsertSql)
		is.NoErr(err)
	}

	// Wait for first webhook
	done := make(chan struct{})
	go func() {
		requestReceived.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Expected
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for webhook request")
	}

	// Wait to ensure no additional webhooks
	time.Sleep(2 * time.Second)

	mu.Lock()
	finalCount := webhookCallCount
	mu.Unlock()

	is.Equal(finalCount, 1)

	// Cleanup
	err = RemoveTrigger(ctx, pool, "audits_test")
	is.NoErr(err)
	_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS submission_defs, audits_test CASCADE;`)
}

// Verify only one webhook per reviewState transition
func TestDeduplicationSubmissionUpdate(t *testing.T) {
	dbUri := os.Getenv("CENTRAL_WEBHOOK_DB_URI")
	if len(dbUri) == 0 {
		dbUri = "postgresql://odk:odk@db:5432/odk?sslmode=disable"
	}

	is := is.New(t)
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx := context.Background()
	pool, err := InitPool(ctx, log, dbUri)
	is.NoErr(err)
	defer pool.Close()

	conn, err := pool.Acquire(ctx)
	is.NoErr(err)
	defer conn.Release()

	// Create tables
	createSubmissionDefsTable(ctx, conn, is)
	createAuditTestsTable(ctx, conn, is)

	// Track webhook calls - need to expect 2 total
	var webhookCallCount int
	var mu sync.Mutex
	var allWebhooksReceived sync.WaitGroup
	allWebhooksReceived.Add(2) // Expect 2 webhooks total

	server, err := createTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		webhookCallCount++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		allWebhooksReceived.Done()
	}))
	is.NoErr(err)
	defer server.Close()

	// Create trigger
	reviewSubmissionUrl := server.URL
	err = CreateTrigger(ctx, pool, "audits_test", TriggerOptions{
		ReviewSubmissionURL: &reviewSubmissionUrl,
	})
	is.NoErr(err)

	// Insert duplicate audit events for same instanceId + reviewState
	instanceId := "test-instance-789"
	for i := 0; i < 3; i++ {
		auditInsertSql := fmt.Sprintf(`
			INSERT INTO audits_test ("actorId", action, details)
			VALUES (5, 'submission.update', '{"instanceId": "%s", "reviewState": "approved"}');
		`, instanceId)
		_, err = conn.Exec(ctx, auditInsertSql)
		is.NoErr(err)
	}

	// Give first webhook time to arrive
	time.Sleep(1 * time.Second)

	mu.Lock()
	firstCount := webhookCallCount
	mu.Unlock()

	// Should have received exactly 1 webhook for the 3 duplicate inserts
	is.Equal(firstCount, 1)

	// Now insert with different reviewState - should trigger another webhook
	auditInsertSql := fmt.Sprintf(`
		INSERT INTO audits_test ("actorId", action, details)
		VALUES (5, 'submission.update', '{"instanceId": "%s", "reviewState": "rejected"}');
	`, instanceId)
	_, err = conn.Exec(ctx, auditInsertSql)
	is.NoErr(err)

	// Wait for both webhooks
	done := make(chan struct{})
	go func() {
		allWebhooksReceived.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Expected - different reviewState should trigger new webhook
	case <-time.After(5 * time.Second):
		mu.Lock()
		currentCount := webhookCallCount
		mu.Unlock()
		t.Fatalf("timeout waiting for second webhook request (received %d webhooks)", currentCount)
	}

	mu.Lock()
	finalCount := webhookCallCount
	mu.Unlock()

	// Should have 2 webhooks total (one per unique reviewState)
	is.Equal(finalCount, 2)

	// Cleanup
	err = RemoveTrigger(ctx, pool, "audits_test")
	is.NoErr(err)
	_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS submission_defs, audits_test CASCADE;`)
}
