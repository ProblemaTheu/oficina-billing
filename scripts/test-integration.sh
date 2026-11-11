#!/usr/bin/env bash
# Roda a suíte de integração (build tag `integration`) contra um PostgreSQL
# efêmero em Docker. Não requer Go instalado na máquina — o teste também roda
# em container. Uso: ./scripts/test-integration.sh
set -euo pipefail

NETWORK=billing-it-net
PG_CONTAINER=billing-it-postgres
PG_IMAGE=postgres:15.7
GO_IMAGE=golang:1.26-alpine
REPO_DIR=$(cd "$(dirname "$0")/.." && pwd)

cleanup() {
  docker rm -f "$PG_CONTAINER" >/dev/null 2>&1 || true
  docker network rm "$NETWORK" >/dev/null 2>&1 || true
}
trap cleanup EXIT
cleanup

docker network create "$NETWORK" >/dev/null
docker run -d --name "$PG_CONTAINER" --network "$NETWORK" \
  -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=billing_it \
  "$PG_IMAGE" >/dev/null

echo "aguardando o Postgres ficar pronto..."
for _ in $(seq 1 60); do
  if docker exec "$PG_CONTAINER" pg_isready -U postgres >/dev/null 2>&1; then
    break
  fi
  sleep 0.5
done

docker run --rm --network "$NETWORK" \
  -v "$REPO_DIR":/app -w /app \
  -v tc1-gocache:/root/.cache -v tc1-gomod:/go/pkg/mod \
  -e TEST_DATABASE_URL="postgres://postgres:postgres@$PG_CONTAINER:5432/billing_it?sslmode=disable" \
  "$GO_IMAGE" go test -tags integration -count=1 -v ./internal/infra/repository/
