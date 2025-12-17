package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type TriggerOptions struct {
	UpdateEntityURL     *string
	NewSubmissionURL    *string
	ReviewSubmissionURL *string
	APIKey              *string
}

// CreateTrigger creates a PostgreSQL trigger that uses pg-http extension to send
// HTTP requests directly from the database when audit events occur.
func CreateTrigger(ctx context.Context, dbPool *pgxpool.Pool, tableName string, opts TriggerOptions) error {
	// Ensure pg-http extension is available
	if err := ensureHTTPExtension(ctx, dbPool); err != nil {
		return fmt.Errorf("failed to ensure pg-http extension: %w", err)
	}

	if tableName == "" {
		tableName = "audits"
	}

	// Build HTTP headers for pgsql-http
	headersSQL := `'Content-Type', 'application/json'`
	if opts.APIKey != nil {
		headersSQL += fmt.Sprintf(`, 'X-API-Key', %s`, quoteSQLString(*opts.APIKey))
	}

	caseStatements := ""

	// ---------------------------------------------------------------------
	// entity.update.version
	// ---------------------------------------------------------------------
	if opts.UpdateEntityURL != nil {
		url := quoteSQLString(*opts.UpdateEntityURL)
		caseStatements += fmt.Sprintf(`
			WHEN 'entity.update.version' THEN
				SELECT entity_defs."data"
				INTO result_data
				FROM entity_defs
				WHERE entity_defs.id = (NEW.details->>'entityDefId')::int;

				webhook_payload := jsonb_build_object(
					'type', 'entity.update.version',
					'id', (NEW.details->'entity'->>'uuid'),
					'data', result_data
				);

				PERFORM http((
					'POST',
					%s,
					http_headers(%s),
					'application/json',
					webhook_payload::text
				)::http_request);
		`, url, headersSQL)
	}

	// ---------------------------------------------------------------------
	// submission.create
	// ---------------------------------------------------------------------
	if opts.NewSubmissionURL != nil {
		url := quoteSQLString(*opts.NewSubmissionURL)
		caseStatements += fmt.Sprintf(`
			WHEN 'submission.create' THEN
				SELECT jsonb_build_object('xml', submission_defs.xml)
				INTO result_data
				FROM submission_defs
				WHERE submission_defs.id = (NEW.details->>'submissionDefId')::int;

				webhook_payload := jsonb_build_object(
					'type', 'submission.create',
					'id', (NEW.details->>'instanceId'),
					'data', result_data
				);

				PERFORM http((
					'POST',
					%s,
					http_headers(%s),
					'application/json',
					webhook_payload::text
				)::http_request);
		`, url, headersSQL)
	}

	// ---------------------------------------------------------------------
	// submission.update
	// ---------------------------------------------------------------------
	if opts.ReviewSubmissionURL != nil {
		url := quoteSQLString(*opts.ReviewSubmissionURL)
		caseStatements += fmt.Sprintf(`
			WHEN 'submission.update' THEN
				webhook_payload := jsonb_build_object(
					'type', 'submission.update',
					'id', (NEW.details->>'instanceId'),
					'data', jsonb_build_object(
						'reviewState', NEW.details->>'reviewState'
					)
				);

				PERFORM http((
					'POST',
					%s,
					http_headers(%s),
					'application/json',
					webhook_payload::text
				)::http_request);
		`, url, headersSQL)
	}

	// Default case
	caseStatements += `
		ELSE
			RETURN NEW;
	`

	createFunctionSQL := fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION new_audit_log() RETURNS trigger AS
		$$
		DECLARE
			action_type text;
			result_data jsonb;
			webhook_payload jsonb;
		BEGIN
			action_type := NEW.action;

			-- Prevent duplicate webhooks
			-- see https://github.com/hotosm/central-webhook/issues/7
			IF
				(action_type = 'submission.create' AND TG_OP != 'INSERT')
				OR
				(action_type IN ('entity.update.version', 'submission.update') AND TG_OP NOT IN ('INSERT', 'UPDATE'))
			THEN
				RETURN NEW;
			END IF;

			CASE action_type
			%s
			END CASE;

			RETURN NEW;
		EXCEPTION
			WHEN OTHERS THEN
				RAISE WARNING 'Error in webhook trigger for action %%: %%', action_type, SQLERRM;
				RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
	`, caseStatements)

	dropTriggerSQL := fmt.Sprintf(`
		DROP TRIGGER IF EXISTS new_audit_log_trigger
		ON %s;
	`, tableName)

	createTriggerSQL := fmt.Sprintf(`
		CREATE TRIGGER new_audit_log_trigger
			AFTER INSERT OR UPDATE ON %s
			FOR EACH ROW
			EXECUTE FUNCTION new_audit_log();
	`, tableName)

	conn, err := dbPool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	// Validate that the table exists before creating the trigger
	tableExists, err := checkTableExists(ctx, conn, tableName)
	if err != nil {
		return fmt.Errorf("failed to check if table exists: %w", err)
	}
	if !tableExists {
		return fmt.Errorf("table %q does not exist. This tool requires an ODK Central database with the %q table. Please verify you are connecting to the correct database and that ODK Central has been properly initialized", tableName, tableName)
	}

	if _, err := conn.Exec(ctx, createFunctionSQL); err != nil {
		return fmt.Errorf("failed to create function: %w", err)
	}
	if _, err := conn.Exec(ctx, dropTriggerSQL); err != nil {
		return fmt.Errorf("failed to drop trigger: %w", err)
	}
	if _, err := conn.Exec(ctx, createTriggerSQL); err != nil {
		return fmt.Errorf("failed to create trigger: %w", err)
	}

	return nil
}

// RemoveTrigger removes the webhook trigger from the database
func RemoveTrigger(ctx context.Context, dbPool *pgxpool.Pool, tableName string) error {
	if tableName == "" {
		tableName = "audits"
	}

	conn, err := dbPool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	// Check if table exists (optional check - we use IF EXISTS so it's not required)
	// But it's helpful to provide a clear error if the table doesn't exist
	tableExists, err := checkTableExists(ctx, conn, tableName)
	if err != nil {
		return fmt.Errorf("failed to check if table exists: %w", err)
	}
	if !tableExists {
		return fmt.Errorf("table %q does not exist. Please verify you are connecting to the correct database", tableName)
	}

	// First, drop the trigger (if it exists)
	dropTriggerSQL := fmt.Sprintf(`
		DROP TRIGGER IF EXISTS new_audit_log_trigger
		ON %s CASCADE;
	`, tableName)

	if _, err := conn.Exec(ctx, dropTriggerSQL); err != nil {
		return fmt.Errorf("failed to drop trigger: %w", err)
	}

	// Then drop the function with CASCADE to handle any remaining dependencies
	dropFunctionSQL := `
		DROP FUNCTION IF EXISTS new_audit_log() CASCADE;
	`

	if _, err := conn.Exec(ctx, dropFunctionSQL); err != nil {
		return fmt.Errorf("failed to drop function: %w", err)
	}

	return nil
}

// checkTableExists checks if a table exists in the database
func checkTableExists(ctx context.Context, conn *pgxpool.Conn, tableName string) (bool, error) {
	var exists bool
	query := `
		SELECT EXISTS (
			SELECT FROM information_schema.tables 
			WHERE table_schema = 'public' 
			AND table_name = $1
		);
	`
	err := conn.QueryRow(ctx, query, tableName).Scan(&exists)
	return exists, err
}

// ensureHTTPExtension ensures the pg-http extension is installed
func ensureHTTPExtension(ctx context.Context, dbPool *pgxpool.Pool) error {
	conn, err := dbPool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	_, err = conn.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS http;`)
	if err != nil {
		var exists bool
		checkSQL := `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'http');`
		if err := conn.QueryRow(ctx, checkSQL).Scan(&exists); err != nil {
			return fmt.Errorf("failed to check for http extension: %w", err)
		}
		if !exists {
			return fmt.Errorf("pg-http extension is not installed and cannot be created automatically")
		}
	}

	return nil
}

// quoteSQLString safely quotes a string for SQL
func quoteSQLString(s string) string {
	return fmt.Sprintf("'%s'", escapeSQLString(s))
}

// escapeSQLString escapes single quotes
func escapeSQLString(s string) string {
	result := ""
	for _, r := range s {
		if r == '\'' {
			result += "''"
		} else {
			result += string(r)
		}
	}
	return result
}
