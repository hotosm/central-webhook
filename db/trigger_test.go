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

func createEntityDefsTable(ctx context.Context, conn *pgxpool.Conn, is *is.I) {
	_, err := conn.Exec(ctx, `DROP TABLE IF EXISTS entity_defs CASCADE;`)
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
}

type testDB struct {
	URI    string
	Pool   *pgxpool.Pool
	Conn   *pgxpool.Conn
	Log    *slog.Logger
	Ctx    context.Context
	Is     *is.I
}

func setupTestDB(t *testing.T) *testDB {
	dbUri := os.Getenv("CENTRAL_WEBHOOK_DB_URI")
	if dbUri == "" {
		dbUri = "postgresql://odk:odk@db:5432/odk?sslmode=disable"
	}

	is := is.New(t)
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx := context.Background()
	pool, err := InitPool(ctx, log, dbUri)
	is.NoErr(err)

	conn, err := pool.Acquire(ctx)
	is.NoErr(err)

	return &testDB{
		URI:  dbUri,
		Pool: pool,
		Conn: conn,
		Log:  log,
		Ctx:  ctx,
		Is:   is,
	}
}

func (tdb *testDB) Close() {
	tdb.Conn.Release()
	tdb.Pool.Close()
}

func getFunctionSQL(ctx context.Context, conn *pgxpool.Conn) (string, error) {
	var functionSQL string
	row := conn.QueryRow(ctx, `
		SELECT pg_get_functiondef(p.oid)
		FROM pg_proc p
		JOIN pg_namespace n ON p.pronamespace = n.oid
		WHERE p.proname = 'new_audit_log' AND n.nspname = 'public'
	`)
	err := row.Scan(&functionSQL)
	return functionSQL, err
}


type webhookServer struct {
	*testServer
	payload        map[string]interface{}
	headers        http.Header
	requestCount   int
	mu             sync.Mutex
	requestWG      sync.WaitGroup
	capturePayload bool
	captureHeaders bool
	is             *is.I
	expectedCount  int
}

func createWebhookServer(is *is.I, capturePayload, captureHeaders bool) (*webhookServer, error) {
	return createWebhookServerWithExpected(is, capturePayload, captureHeaders, 1)
}

func createWebhookServerWithExpected(is *is.I, capturePayload, captureHeaders bool, expectedCount int) (*webhookServer, error) {
	ws := &webhookServer{
		capturePayload: capturePayload,
		captureHeaders: captureHeaders,
		is:             is,
		expectedCount:  expectedCount,
	}
	ws.requestWG.Add(expectedCount)

	server, err := createTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws.mu.Lock()
		ws.requestCount++
		currentCount := ws.requestCount
		ws.mu.Unlock()

		if ws.captureHeaders {
			ws.headers = r.Header
		}

		if ws.capturePayload {
			defer r.Body.Close()
			body, err := io.ReadAll(r.Body)
			ws.is.NoErr(err)
			err = json.Unmarshal(body, &ws.payload)
			ws.is.NoErr(err)
		}

		w.WriteHeader(http.StatusOK)
		if currentCount <= ws.expectedCount {
			ws.requestWG.Done()
		}
	}))
	if err != nil {
		return nil, err
	}

	ws.testServer = server
	return ws, nil
}

func (ws *webhookServer) Payload() map[string]interface{} {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.payload
}

func (ws *webhookServer) Headers() http.Header {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.headers
}

func (ws *webhookServer) WaitForRequest(timeout time.Duration) error {
	done := make(chan struct{})
	go func() {
		ws.requestWG.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("timeout waiting for webhook request")
	}
}

func (ws *webhookServer) GetCount() int {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.requestCount
}

func TestEntityTrigger(t *testing.T) {
	tdb := setupTestDB(t)
	defer tdb.Close()

	createEntityDefsTable(tdb.Ctx, tdb.Conn, tdb.Is)
	createAuditTestsTable(tdb.Ctx, tdb.Conn, tdb.Is)

	dummyURL := "http://example.com/webhook"
	err := CreateTrigger(tdb.Ctx, tdb.Pool, "audits_test", TriggerOptions{
		UpdateEntityURL: &dummyURL,
	})
	tdb.Is.NoErr(err)

	functionSQL, err := getFunctionSQL(tdb.Ctx, tdb.Conn)
	tdb.Is.NoErr(err)

	tdb.Is.True(strings.Contains(functionSQL, "WHEN 'entity.update.version'"))
	tdb.Is.True(strings.Contains(functionSQL, "'type', 'entity.update.version'"))
	tdb.Is.True(strings.Contains(functionSQL, "'id', (NEW.details->'entity'->>'uuid')"))
	tdb.Is.True(strings.Contains(functionSQL, "'data', result_data"))

	err = RemoveTrigger(tdb.Ctx, tdb.Pool, "audits_test")
	tdb.Is.NoErr(err)
	_, _ = tdb.Conn.Exec(tdb.Ctx, `DROP TABLE IF EXISTS entity_defs, audits_test CASCADE;`)
}

func TestNewSubmissionTrigger(t *testing.T) {
	tdb := setupTestDB(t)
	defer tdb.Close()

	_, _ = tdb.Conn.Exec(tdb.Ctx, `DROP FUNCTION IF EXISTS new_audit_log() CASCADE;`)
	createSubmissionDefsTable(tdb.Ctx, tdb.Conn, tdb.Is)
	createAuditTestsTable(tdb.Ctx, tdb.Conn, tdb.Is)

	_, err := tdb.Conn.Exec(tdb.Ctx, `
		INSERT INTO submission_defs (id, "submissionId", xml, "formDefId", "submitterId", "createdAt")
		VALUES (1, 2, '<data id="xxx">', 7, 5, '2025-01-10 16:23:40.073');
	`)
	tdb.Is.NoErr(err)

	server, err := createWebhookServer(tdb.Is, true, false)
	tdb.Is.NoErr(err)
	defer server.Close()

	err = CreateTrigger(tdb.Ctx, tdb.Pool, "audits_test", TriggerOptions{
		NewSubmissionURL: &server.URL,
	})
	tdb.Is.NoErr(err)

	_, err = tdb.Conn.Exec(tdb.Ctx, `
		INSERT INTO audits_test ("actorId", action, details)
		VALUES (5, 'submission.create', '{"submissionDefId": 1, "instanceId": "test-instance-123"}');
	`)
	tdb.Is.NoErr(err)

	err = server.WaitForRequest(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}

	payload := server.Payload()
	tdb.Is.True(payload != nil)
	tdb.Is.Equal(payload["type"], "submission.create")
	tdb.Is.Equal(payload["id"], "test-instance-123")
	tdb.Is.True(payload["data"] != nil)

	data, ok := payload["data"].(map[string]interface{})
	tdb.Is.True(ok)
	tdb.Is.Equal(data["xml"], `<data id="xxx">`)

	err = RemoveTrigger(tdb.Ctx, tdb.Pool, "audits_test")
	tdb.Is.NoErr(err)
	_, _ = tdb.Conn.Exec(tdb.Ctx, `DROP TABLE IF EXISTS submission_defs, audits_test CASCADE;`)
}

func TestReviewSubmissionTrigger(t *testing.T) {
	tdb := setupTestDB(t)
	defer tdb.Close()

	createSubmissionDefsTable(tdb.Ctx, tdb.Conn, tdb.Is)
	createAuditTestsTable(tdb.Ctx, tdb.Conn, tdb.Is)

	_, err := tdb.Conn.Exec(tdb.Ctx, `
		INSERT INTO submission_defs (id, "submissionId", "instanceId")
		VALUES (1, 2, '33448049-0df1-4426-9392-d3a294d638ad');
	`)
	tdb.Is.NoErr(err)

	server, err := createWebhookServer(tdb.Is, true, false)
	tdb.Is.NoErr(err)
	defer server.Close()

	err = CreateTrigger(tdb.Ctx, tdb.Pool, "audits_test", TriggerOptions{
		ReviewSubmissionURL: &server.URL,
	})
	tdb.Is.NoErr(err)

	_, err = tdb.Conn.Exec(tdb.Ctx, `
		INSERT INTO audits_test ("actorId", action, details)
		VALUES (5, 'submission.update', '{"submissionDefId": 1, "instanceId": "33448049-0df1-4426-9392-d3a294d638ad", "reviewState": "approved"}');
	`)
	tdb.Is.NoErr(err)

	err = server.WaitForRequest(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}

	payload := server.Payload()
	tdb.Is.True(payload != nil)
	tdb.Is.Equal(payload["type"], "submission.update")
	tdb.Is.Equal(payload["id"], "33448049-0df1-4426-9392-d3a294d638ad")

	data, ok := payload["data"].(map[string]interface{})
	tdb.Is.True(ok)
	tdb.Is.Equal(data["reviewState"], "approved")

	err = RemoveTrigger(tdb.Ctx, tdb.Pool, "audits_test")
	tdb.Is.NoErr(err)
	_, _ = tdb.Conn.Exec(tdb.Ctx, `DROP TABLE IF EXISTS submission_defs, audits_test CASCADE;`)
}

func TestNoTrigger(t *testing.T) {
	tdb := setupTestDB(t)
	defer tdb.Close()

	createAuditTestsTable(tdb.Ctx, tdb.Conn, tdb.Is)

	var requestReceived sync.WaitGroup
	requestReceived.Add(1)
	server, err := createTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestReceived.Done()
		w.WriteHeader(http.StatusOK)
	}))
	tdb.Is.NoErr(err)
	defer server.Close()

	err = CreateTrigger(tdb.Ctx, tdb.Pool, "audits_test", TriggerOptions{
		NewSubmissionURL: &server.URL,
	})
	tdb.Is.NoErr(err)

	_, err = tdb.Conn.Exec(tdb.Ctx, `
		INSERT INTO audits_test ("actorId", action, details)
		VALUES (1, 'invalid.event', '{"submissionDefId": 5}');
	`)
	tdb.Is.NoErr(err)

	select {
	case <-time.After(2 * time.Second):
		tdb.Log.Info("no event triggered for invalid event type")
	case <-func() chan struct{} {
		done := make(chan struct{})
		go func() {
			requestReceived.Wait()
			close(done)
		}()
		return done
	}():
		t.Fatal("unexpected webhook request received for invalid event type")
	}

	err = RemoveTrigger(tdb.Ctx, tdb.Pool, "audits_test")
	tdb.Is.NoErr(err)
	_, _ = tdb.Conn.Exec(tdb.Ctx, `DROP TABLE IF EXISTS audits_test CASCADE;`)
}

func TestModularSql(t *testing.T) {
	tdb := setupTestDB(t)
	defer tdb.Close()

	createAuditTestsTable(tdb.Ctx, tdb.Conn, tdb.Is)

	updateEntityUrl := "https://test.com"
	err := CreateTrigger(tdb.Ctx, tdb.Pool, "audits_test", TriggerOptions{
		UpdateEntityURL: &updateEntityUrl,
	})
	tdb.Is.NoErr(err)

	functionSQL, err := getFunctionSQL(tdb.Ctx, tdb.Conn)
	tdb.Is.NoErr(err)

	tdb.Is.True(strings.Contains(functionSQL, "WHEN 'entity.update.version'"))
	tdb.Is.True(!strings.Contains(functionSQL, "WHEN 'submission.create'"))
	tdb.Is.True(!strings.Contains(functionSQL, "WHEN 'submission.update'"))

	err = RemoveTrigger(tdb.Ctx, tdb.Pool, "audits_test")
	tdb.Is.NoErr(err)
	_, _ = tdb.Conn.Exec(tdb.Ctx, `DROP TABLE IF EXISTS audits_test CASCADE;`)
}

func TestRemoveTrigger(t *testing.T) {
	tdb := setupTestDB(t)
	defer tdb.Close()

	createAuditTestsTable(tdb.Ctx, tdb.Conn, tdb.Is)

	updateEntityUrl := "https://test.com"
	err := CreateTrigger(tdb.Ctx, tdb.Pool, "audits_test", TriggerOptions{
		UpdateEntityURL: &updateEntityUrl,
	})
	tdb.Is.NoErr(err)

	triggerExists, err := checkTriggerExists(tdb.Ctx, tdb.Conn)
	tdb.Is.NoErr(err)
	tdb.Is.True(triggerExists)

	functionExists, err := checkFunctionExists(tdb.Ctx, tdb.Conn)
	tdb.Is.NoErr(err)
	tdb.Is.True(functionExists)

	err = RemoveTrigger(tdb.Ctx, tdb.Pool, "audits_test")
	tdb.Is.NoErr(err)

	triggerExists, err = checkTriggerExists(tdb.Ctx, tdb.Conn)
	tdb.Is.NoErr(err)
	tdb.Is.True(!triggerExists)

	functionExists, err = checkFunctionExists(tdb.Ctx, tdb.Conn)
	tdb.Is.NoErr(err)
	tdb.Is.True(!functionExists)

	_, _ = tdb.Conn.Exec(tdb.Ctx, `DROP TABLE IF EXISTS audits_test CASCADE;`)
}

func TestTriggerWithAPIKey(t *testing.T) {
	tdb := setupTestDB(t)
	defer tdb.Close()

	createEntityDefsTable(tdb.Ctx, tdb.Conn, tdb.Is)
	createAuditTestsTable(tdb.Ctx, tdb.Conn, tdb.Is)

	_, err := tdb.Conn.Exec(tdb.Ctx, `
		INSERT INTO entity_defs (id, "entityId", "createdAt", "current", "data", "creatorId", "label")
		VALUES (1001, 900, '2025-01-10 16:23:40.073', true, '{"status": "0"}', 5, 'Test Entity');
	`)
	tdb.Is.NoErr(err)

	server, err := createWebhookServer(tdb.Is, false, true)
	tdb.Is.NoErr(err)
	defer server.Close()

	apiKey := "test-api-key-12345"
	err = CreateTrigger(tdb.Ctx, tdb.Pool, "audits_test", TriggerOptions{
		UpdateEntityURL: &server.URL,
		APIKey:          &apiKey,
	})
	tdb.Is.NoErr(err)

	_, err = tdb.Conn.Exec(tdb.Ctx, `
		INSERT INTO audits_test ("actorId", action, details)
		VALUES (1, 'entity.update.version', '{"entityDefId": 1001, "entity": {"uuid": "test-uuid", "dataset": "test"}}');
	`)
	tdb.Is.NoErr(err)

	err = server.WaitForRequest(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}

	headers := server.Headers()
	tdb.Is.Equal(headers.Get("X-API-Key"), "test-api-key-12345")
	tdb.Is.Equal(headers.Get("Content-Type"), "application/json")

	err = RemoveTrigger(tdb.Ctx, tdb.Pool, "audits_test")
	tdb.Is.NoErr(err)
	_, _ = tdb.Conn.Exec(tdb.Ctx, `DROP TABLE IF EXISTS entity_defs, audits_test CASCADE;`)
}

func TestDeduplicationEntityUpdate(t *testing.T) {
	tdb := setupTestDB(t)
	defer tdb.Close()

	_, err := tdb.Conn.Exec(tdb.Ctx, `
		DROP TABLE IF EXISTS entity_defs CASCADE;
		CREATE TABLE entity_defs (id int4, "entityId" int4, "data" jsonb);
		INSERT INTO entity_defs (id, "entityId", "data") VALUES (1001, 900, '{"status": "active"}');
	`)
	tdb.Is.NoErr(err)
	createAuditTestsTable(tdb.Ctx, tdb.Conn, tdb.Is)

	server, err := createWebhookServer(tdb.Is, false, false)
	tdb.Is.NoErr(err)
	defer server.Close()

	err = CreateTrigger(tdb.Ctx, tdb.Pool, "audits_test", TriggerOptions{
		UpdateEntityURL: &server.URL,
	})
	tdb.Is.NoErr(err)

	for i := 0; i < 3; i++ {
		_, err = tdb.Conn.Exec(tdb.Ctx, `
			INSERT INTO audits_test ("actorId", action, details)
			VALUES (1, 'entity.update.version', '{"entityDefId": 1001, "entity": {"uuid": "test-uuid-123"}}');
		`)
		tdb.Is.NoErr(err)
	}

	err = server.WaitForRequest(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(2 * time.Second)
	tdb.Is.Equal(server.GetCount(), 1)

	err = RemoveTrigger(tdb.Ctx, tdb.Pool, "audits_test")
	tdb.Is.NoErr(err)
	_, _ = tdb.Conn.Exec(tdb.Ctx, `DROP TABLE IF EXISTS entity_defs, audits_test CASCADE;`)
}

func TestDeduplicationSubmissionCreate(t *testing.T) {
	tdb := setupTestDB(t)
	defer tdb.Close()

	createSubmissionDefsTable(tdb.Ctx, tdb.Conn, tdb.Is)
	createAuditTestsTable(tdb.Ctx, tdb.Conn, tdb.Is)

	_, err := tdb.Conn.Exec(tdb.Ctx, `
		INSERT INTO submission_defs (id, "submissionId", xml)
		VALUES (1, 2, '<data id="test"/>');
	`)
	tdb.Is.NoErr(err)

	server, err := createWebhookServer(tdb.Is, false, false)
	tdb.Is.NoErr(err)
	defer server.Close()

	err = CreateTrigger(tdb.Ctx, tdb.Pool, "audits_test", TriggerOptions{
		NewSubmissionURL: &server.URL,
	})
	tdb.Is.NoErr(err)

	instanceId := "dup-test-instance-456"
	for i := 0; i < 3; i++ {
		_, err = tdb.Conn.Exec(tdb.Ctx, fmt.Sprintf(`
			INSERT INTO audits_test ("actorId", action, details)
			VALUES (5, 'submission.create', '{"submissionDefId": 1, "instanceId": "%s"}');
		`, instanceId))
		tdb.Is.NoErr(err)
	}

	err = server.WaitForRequest(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(2 * time.Second)
	tdb.Is.Equal(server.GetCount(), 1)

	err = RemoveTrigger(tdb.Ctx, tdb.Pool, "audits_test")
	tdb.Is.NoErr(err)
	_, _ = tdb.Conn.Exec(tdb.Ctx, `DROP TABLE IF EXISTS submission_defs, audits_test CASCADE;`)
}

func TestDeduplicationSubmissionUpdate(t *testing.T) {
	tdb := setupTestDB(t)
	defer tdb.Close()

	createSubmissionDefsTable(tdb.Ctx, tdb.Conn, tdb.Is)
	createAuditTestsTable(tdb.Ctx, tdb.Conn, tdb.Is)

	server, err := createWebhookServerWithExpected(tdb.Is, false, false, 2)
	tdb.Is.NoErr(err)
	defer server.Close()

	err = CreateTrigger(tdb.Ctx, tdb.Pool, "audits_test", TriggerOptions{
		ReviewSubmissionURL: &server.URL,
	})
	tdb.Is.NoErr(err)

	instanceId := "test-instance-789"
	for i := 0; i < 3; i++ {
		_, err = tdb.Conn.Exec(tdb.Ctx, fmt.Sprintf(`
			INSERT INTO audits_test ("actorId", action, details)
			VALUES (5, 'submission.update', '{"instanceId": "%s", "reviewState": "approved"}');
		`, instanceId))
		tdb.Is.NoErr(err)
	}

	time.Sleep(1 * time.Second)
	tdb.Is.Equal(server.GetCount(), 1)

	_, err = tdb.Conn.Exec(tdb.Ctx, fmt.Sprintf(`
		INSERT INTO audits_test ("actorId", action, details)
		VALUES (5, 'submission.update', '{"instanceId": "%s", "reviewState": "rejected"}');
	`, instanceId))
	tdb.Is.NoErr(err)

	err = server.WaitForRequest(5 * time.Second)
	if err != nil {
		t.Fatalf("timeout waiting for second webhook request (received %d webhooks)", server.GetCount())
	}

	tdb.Is.Equal(server.GetCount(), 2)

	err = RemoveTrigger(tdb.Ctx, tdb.Pool, "audits_test")
	tdb.Is.NoErr(err)
	_, _ = tdb.Conn.Exec(tdb.Ctx, `DROP TABLE IF EXISTS submission_defs, audits_test CASCADE;`)
}
