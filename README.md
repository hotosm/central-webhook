# Central Webhook

<!-- markdownlint-disable -->
<p align="center">
  <em>A lightweight CLI tool for installing webhook triggers in ODK Central database.</em>
</p>
<p align="center">
  <a href="https://github.com/hotosm/central-webhook/actions/workflows/release.yml" target="_blank">
      <img src="https://github.com/hotosm/central-webhook/actions/workflows/release.yml/badge.svg" alt="Build & Release">
  </a>
  <a href="https://github.com/hotosm/central-webhook/actions/workflows/test.yml" target="_blank">
      <img src="https://github.com/hotosm/central-webhook/actions/workflows/test.yml/badge.svg" alt="Test">
  </a>
</p>

---

<!-- markdownlint-enable -->

Install PostgreSQL triggers that automatically call remote APIs on ODK Central database events:

- New submission (XML).
- Update entity (entity properties).
- Submission review (approved, hasIssues, rejected).

The `centralwebhook` tool is a simple CLI that installs or uninstalls database triggers. Once installed, the triggers use the `pgsql-http` extension to send HTTP requests directly from the database.

## Prerequisites

- ODK Central running, connecting to an accessible PostgreSQL database.
- The `pgsql-http` extension installed and enabled in your PostgreSQL database:
  ```sql
  CREATE EXTENSION http;
  ```
  > [!NOTE]
  > **Using our helper images**: We provide PostgreSQL images with the `pgsql-http` extension pre-installed:
  > - `ghcr.io/hotosm/postgres:18-http` (based on vanilla PostgreSQL 18 images)
  >
  > These images are drop-in replacements for standard PostgreSQL images and simply add the extension.
  >
  > **Installing manually**: If you don't wish to use these images, you must install the `pgsql-http` extension yourself. The extension may require superuser privileges to install. If you cannot install it yourself, ask your database administrator.
- A POST webhook endpoint on your service API, to call when the selected event occurs.

## Usage

The `centralwebhook` tool is a CLI that installs or uninstalls database triggers. After installation, the triggers run automatically whenever audit events occur in the database.

### Install Triggers

Install webhook triggers in your database:

```bash
./centralwebhook install \
    -db 'postgresql://{user}:{password}@{hostname}/{db}?sslmode=disable' \
    -updateEntityUrl 'https://your.domain.com/some/webhook' \
    -newSubmissionUrl 'https://your.domain.com/some/webhook' \
    -reviewSubmissionUrl 'https://your.domain.com/some/webhook'
```

> [!TIP]
> Omit a webhook URL flag if you do not wish to use that particular webhook.
>
> The `-apiKey` flag is optional, see the [APIs With Authentication](#apis-with-authentication) section.

### Uninstall Triggers

Remove webhook triggers from your database:

```bash
./centralwebhook uninstall \
    -db 'postgresql://{user}:{password}@{hostname}/{db}?sslmode=disable'
```

### Environment Variables

All flags can also be provided via environment variables:

```dotenv
CENTRAL_WEBHOOK_DB_URI=postgresql://user:pass@localhost:5432/db_name?sslmode=disable
CENTRAL_WEBHOOK_UPDATE_ENTITY_URL=https://your.domain.com/some/webhook
CENTRAL_WEBHOOK_REVIEW_SUBMISSION_URL=https://your.domain.com/some/webhook
CENTRAL_WEBHOOK_NEW_SUBMISSION_URL=https://your.domain.com/some/webhook
CENTRAL_WEBHOOK_API_KEY=ksdhfiushfiosehf98e3hrih39r8hy439rh389r3hy983y
CENTRAL_WEBHOOK_LOG_LEVEL=DEBUG
```

### Via Docker

You can run the CLI tool via Docker:

```bash
docker run --rm ghcr.io/hotosm/central-webhook:latest install \
    -db 'postgresql://{user}:{password}@{hostname}/{db}?sslmode=disable' \
    -updateEntityUrl 'https://your.domain.com/some/webhook' \
    -newSubmissionUrl 'https://your.domain.com/some/webhook' \
    -reviewSubmissionUrl 'https://your.domain.com/some/webhook'
```

### Download Binary

Download the binary for your platform from the
[releases](https://github.com/hotosm/central-webhook/releases) page.

## Webhook Request Payload Examples

### Entity Update (updateEntityUrl)

```json
{
    "type": "entity.update.version",
    "id":"uuid:3c142a0d-37b9-4d37-baf0-e58876428181",
    "data": {
        "entityProperty1": "someStringValue",
        "entityProperty2": "someStringValue",
        "entityProperty3": "someStringValue"
    }
}
```

### New Submission (newSubmissionUrl)

```json
{
    "type": "submission.create",
    "id":"uuid:3c142a0d-37b9-4d37-baf0-e58876428181",
    "data": {"xml":"<?xml version='1.0' encoding='UTF-8' ?><data ...."}
}
```

### Review Submission (reviewSubmissionUrl)

```json
{
    "type":"submission.update",
    "id":"uuid:5ed3b610-a18a-46a2-90a7-8c80c82ebbe9",
    "data": {"reviewState":"hasIssues"}
}
```

## APIs With Authentication

Many APIs will not be public and require some sort of authentication.

There is an optional `-apiKey` flag that can be used to pass
an API key / token provided by the application.

This will be inserted in the `X-API-Key` request header when the trigger sends HTTP requests.

No other authentication methods are supported for now, but feel
free to open an issue (or PR!) for a proposal to support other
auth methods.

Example:

```bash
./centralwebhook install \
    -db 'postgresql://{user}:{password}@{hostname}/{db}?sslmode=disable' \
    -updateEntityUrl 'https://your.domain.com/some/webhook' \
    -apiKey 'ksdhfiushfiosehf98e3hrih39r8hy439rh389r3hy983y'
```

## Example Webhook Server

Here is a minimal FastAPI example for receiving the webhook data:

```python
from typing import Annotated, Optional

from fastapi import (
    Depends,
    Header,
)
from fastapi.exceptions import HTTPException
from pydantic import BaseModel


class OdkCentralWebhookRequest(BaseModel):
    """The POST data from the central webhook service."""

    type: OdkWebhookEvents
    # NOTE we cannot use UUID validation, as Central often passes uuid as 'uuid:xxx-xxx'
    id: str
    # NOTE we use a dict to allow flexible passing of the data based on event type
    data: dict


async def valid_api_token(
    x_api_key: Annotated[Optional[str], Header()] = None,
):
    """Check the API token is present for an active database user.

    A header X-API-Key must be provided in the request.
    """
    # Logic to validate the api key here
    return


@router.post("/webhooks/entity-status")
async def update_entity_status_in_fmtm(
    current_user: Annotated[DbUser, Depends(valid_api_token)],
    odk_event: OdkCentralWebhookRequest,
):
    """Update the status for an Entity in our app db.
    """
    log.debug(f"Webhook called with event ({odk_event.type.value})")

    if odk_event.type == OdkWebhookEvents.UPDATE_ENTITY:
        # insert state into db
    elif odk_event.type == OdkWebhookEvents.REVIEW_SUBMISSION:
        # update entity status in odk to match review state
        pass
    elif odk_event.type == OdkWebhookEvents.NEW_SUBMISSION:
        # unsupported for now
        log.debug(
            "The handling of new submissions via webhook is not implemented yet."
        )
    else:
        msg = f"Webhook was called for an unsupported event type ({odk_event.type.value})"
        log.warning(msg)
        raise HTTPException(status_code=400, detail=msg)
```

## How It Works

The tool installs PostgreSQL triggers on the `audits` table that:

1. Detect when audit events occur (entity updates, submission creates/updates)
2. Format the event data into a JSON payload with `type`, `id`, and `data` fields
3. Use the `pgsql-http` extension to send an HTTP POST request directly from the database
4. Include the `X-API-Key` header if provided during installation

The triggers run automatically after installation - no long-running service is needed.

## Development

- This package uses the standard library and a Postgres driver.
- Binary and container image distribution is automated on new **release**.

### Run The Tests

The test suite depends on a database with the `pgsql-http` extension installed. The most convenient way is to run via docker:

```bash
docker compose run --rm webhook
```
