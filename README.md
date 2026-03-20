# Plausible X Stack (Dokploy + GHCR + Provisioner)

This repository contains the Plausible X deployment stack: self-hosted Plausible CE plus a small Go API (`plausible-provisioner`) used to provision custom events/goals.

## Services

Defined in `compose.yml`:

- `plausible_db` (Postgres 16)
- `plausible_events_db` (ClickHouse 24.12)
- `plausible` (Plausible Community Edition v3.2.0)
- `plausible_x` (Plausible X provisioner image from GHCR)

All services run on the external Docker network `dokploy-network`.

## Repository Structure

- `compose.yml` - Full runtime stack for Dokploy
- `plausible-provisioner/main.go` - Plausible X API source code
- `plausible-provisioner/Dockerfile` - Plausible X image build
- `.github/workflows/deploy.yml` - CI/CD pipeline (build, push, deploy)

## Provisioner API

Base URL depends on your Dokploy domain mapping (recommended: separate API subdomain).

### Health check

- `GET /healthz`
- Response: `200 OK` with body `ok`

### Ensure goal

- `PUT /ensure-goal`
- Content-Type: `application/json`

Request body:

```json
{
  "domain": "example.com",
  "event_name": "signup_completed",
  "props": ["plan", "source"]
}
```

Response:

- `201` with `{"status":"created"}` when a goal was created
- `200` with `{"status":"exists"}` when it already exists
- `404` if site is not found

## CI/CD

Workflow file: `.github/workflows/deploy.yml`

On push to `main` (when relevant paths change), the workflow:

1. Builds `plausible-x` image with Buildx
2. Pushes to GitHub Container Registry (`ghcr.io`)
3. Tags image as:
   - `latest`
   - commit SHA
4. Calls Dokploy deploy webhook to redeploy

### Required GitHub Secret

Configure this repository secret:

- `DOKPLOY_WEBHOOK_URL` - Dokploy deployment webhook URL

`GITHUB_TOKEN` is provided automatically by GitHub Actions and is used to push to GHCR.

## Dokploy Setup Notes

Recommended domain mapping for Plausible X:

- `plausible-x.yourdomain.com` -> `plausible`
- `plausible-x-api.yourdomain.com` -> `plausible_x`

This keeps public analytics traffic and provisioning API traffic isolated.

## Security Notes

Current provisioner endpoints are not authenticated. If exposed publicly, add request authentication (for example, Bearer token via environment variable) before production use.

## Local Development (optional)

If you want to run only the provisioner locally:

```bash
cd plausible-provisioner
go mod tidy
DATABASE_URL="postgres://postgres:postgres@localhost:5432/plausible_db?sslmode=disable" PORT=8080 go run .
```

You will need a Postgres instance with Plausible schema/tables available.
