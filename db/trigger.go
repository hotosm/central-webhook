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
}

// Example parsed JSON
// {"action":"entity.update.version","actorId":1,"details":{"entityDefId":1001,...},"dml_action":"INSERT"}}

func CreateTrigger(ctx context.Context, dbPool *pgxpool.Pool, tableName string, opts TriggerOptions) error {
	// This trigger runs on the `audits` table by default, and creates a new event
	// in the odk-events queue when a new event is created in the table

	if tableName == "" {
		// default table (this is configurable for easier tests mainly)
		tableName = "audits"
	}

	// Create SQL trigger function dynamically, based on params
	caseStatements := ""

	if opts.UpdateEntityURL != nil {
		caseStatements += `
			WHEN 'entity.update.version' THEN
				SELECT entity_defs.data
				INTO result_data
				FROM entity_defs
				WHERE entity_defs.id = (NEW.details->>'entityDefId')::int;

				js := jsonb_set(js, '{data}', result_data, true);

				IF length(js::text) > 8000 THEN
					RAISE NOTICE 'Payload too large, truncating: %', left(js::text, 500) || '...';
					js := jsonb_set(js, '{truncated}', 'true'::jsonb, true);
					js := jsonb_set(js, '{data}', '"Payload too large. Truncated."'::jsonb, true);
				END IF;

				PERFORM pg_notify('odk-events', js::text);
		`
	}

	if opts.NewSubmissionURL != nil {
		caseStatements += `
			WHEN 'submission.create' THEN
				SELECT jsonb_build_object('xml', submission_defs.xml)
				INTO result_data
				FROM submission_defs
				WHERE submission_defs.id = (NEW.details->>'submissionDefId')::int;

				js := jsonb_set(js, '{data}', result_data, true);

				IF length(js::text) > 8000 THEN
					RAISE NOTICE 'Payload too large, truncating: %', left(js::text, 500) || '...';
					js := jsonb_set(js, '{truncated}', 'true'::jsonb, true);
					js := jsonb_set(js, '{data}', '"Payload too large. Truncated."'::jsonb, true);
				END IF;

				PERFORM pg_notify('odk-events', js::text);
		`
	}

	if opts.ReviewSubmissionURL != nil {
		caseStatements += `
			WHEN 'submission.update' THEN
				SELECT jsonb_build_object('instanceId', submission_defs."instanceId")
				INTO result_data
				FROM submission_defs
				WHERE submission_defs.id = (NEW.details->>'submissionDefId')::int;

				js := jsonb_set(js, '{data}', jsonb_build_object('reviewState', js->'details'->>'reviewState'), true);
				js := jsonb_set(js, '{details}', (js->'details')::jsonb - 'reviewState', true);
				js := jsonb_set(js, '{details}', (js->'details') || result_data, true);

				IF length(js::text) > 8000 THEN
					RAISE NOTICE 'Payload too large, truncating: %', left(js::text, 500) || '...';
					js := jsonb_set(js, '{truncated}', 'true'::jsonb, true);
					js := jsonb_set(js, '{data}', '"Payload too large. Truncated."'::jsonb, true);
				END IF;

				PERFORM pg_notify('odk-events', js::text);
		`
	}

	// default ELSE case (always included)
	caseStatements += `
		ELSE
			RETURN NEW;
	`

	// full function SQL
	createFunctionSQL := fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION new_audit_log() RETURNS trigger AS
		$$
		DECLARE
			js jsonb;
			action_type text;
			result_data jsonb;
		BEGIN
			SELECT to_jsonb(NEW.*) INTO js;
			js := jsonb_set(js, '{dml_action}', to_jsonb(TG_OP));
			action_type := NEW.action;

			CASE action_type
			%s
			END CASE;

			RETURN NEW;
		END;
		$$ LANGUAGE 'plpgsql';
	`, caseStatements)

	// SQL for dropping the existing trigger
	dropTriggerSQL := fmt.Sprintf(`
		DROP TRIGGER IF EXISTS new_audit_log_trigger
		ON %s;
	`, tableName)

	// SQL for creating the new trigger
	createTriggerSQL := fmt.Sprintf(`
		CREATE TRIGGER new_audit_log_trigger
			BEFORE INSERT OR UPDATE ON %s
			FOR EACH ROW
				EXECUTE FUNCTION new_audit_log();
	`, tableName)

	// Acquire a connection from the pool, close after all statements executed
	conn, err := dbPool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, createFunctionSQL); err != nil {
		return fmt.Errorf("failed to create function: %w", err)
	}
	if _, err := conn.Exec(ctx, dropTriggerSQL); err != nil {
		return fmt.Errorf("failed to drop trigger: %w", err)
	}
	if _, err := conn.Exec(ctx, createTriggerSQL); err != nil {
		return fmt.Errorf("failed to create trigger: %w", err)
	}

	return err
}
