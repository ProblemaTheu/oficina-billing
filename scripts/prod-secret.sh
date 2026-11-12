#!/usr/bin/env bash
# Cria/atualiza o Secret billing-secrets no namespace billing-prod a partir
# do AWS Secrets Manager. Rodar uma vez depois do `terraform apply` do infra/
# (e de novo só se um segredo mudar).
#
#   ./scripts/prod-secret.sh
#
# Fontes:
#   oficina/prod/billing/db          — credenciais do RDS do Billing (infra/banco.tf)
#   oficina/prod/app                 — jwt_secret COMPARTILHADO com o OS e a Lambda
#                                      (criado pelo scripts/prod-secret.sh do oficina-app)
#   oficina/prod/billing/mercadopago — access_token e webhook_secret do MP; na 1ª
#                                      execução são pedidos no terminal (sem eco)
#   NEW_RELIC_LICENSE_KEY            — opcional, do ambiente
#
# Nada passa por argumento de linha de comando (aparece em `ps`) e nada é
# escrito em disco: o manifesto vai para o kubectl por stdin.
set -euo pipefail

REGIAO=${REGIAO:-us-east-1}
NS=${NS:-billing-prod}

ler() {
  local segredo="$1"
  aws secretsmanager get-secret-value --secret-id "$segredo" --region "$REGIAO" --query SecretString --output text 2>/dev/null
}
campo() {
  local nome="$1"
  python3 -c 'import sys,json;print(json.load(sys.stdin)[sys.argv[1]])' "$nome"
}

DB_JSON=$(ler oficina/prod/billing/db) || { echo "segredo oficina/prod/billing/db não encontrado — aplique o infra/ antes"; exit 1; }
APP_JSON=$(ler oficina/prod/app) || { echo "segredo oficina/prod/app não encontrado — rode o scripts/prod-secret.sh do oficina-app antes"; exit 1; }

if ! MP_JSON=$(ler oficina/prod/billing/mercadopago); then
  echo "==> oficina/prod/billing/mercadopago não existe; informe as credenciais de TESTE do Mercado Pago"
  read -rsp "MP access token (TEST-...): " MP_TOKEN; echo
  read -rsp "MP webhook secret (assinatura secreta do painel): " MP_SEGREDO; echo
  MP_JSON=$(MP_TOKEN="$MP_TOKEN" MP_SEGREDO="$MP_SEGREDO" python3 -c '
import json, os
print(json.dumps({"access_token": os.environ["MP_TOKEN"], "webhook_secret": os.environ["MP_SEGREDO"]}))')
  unset MP_TOKEN MP_SEGREDO
  # --secret-string por stdin (file:///dev/stdin) para não aparecer em `ps`
  printf '%s' "$MP_JSON" | aws secretsmanager create-secret --name oficina/prod/billing/mercadopago \
    --region "$REGIAO" --secret-string file:///dev/stdin >/dev/null
fi

export DB_USER DB_PASSWORD JWT_SECRET MP_ACCESS_TOKEN MP_WEBHOOK_SECRET
DB_USER=$(printf '%s' "$DB_JSON" | campo username)
DB_PASSWORD=$(printf '%s' "$DB_JSON" | campo password)
JWT_SECRET=$(printf '%s' "$APP_JSON" | campo jwt_secret)
MP_ACCESS_TOKEN=$(printf '%s' "$MP_JSON" | campo access_token)
MP_WEBHOOK_SECRET=$(printf '%s' "$MP_JSON" | campo webhook_secret)

# Manifesto em JSON gerado pelo python: valores com aspas ou `$` não quebram
# o YAML. `apply` em vez de `create`: idempotente.
python3 - <<'PY' | kubectl apply -n "$NS" -f -
import json, os
dados = {k: os.environ[k] for k in
         ["DB_USER", "DB_PASSWORD", "JWT_SECRET", "MP_ACCESS_TOKEN", "MP_WEBHOOK_SECRET"]}
if os.environ.get("NEW_RELIC_LICENSE_KEY"):
    dados["NEW_RELIC_LICENSE_KEY"] = os.environ["NEW_RELIC_LICENSE_KEY"]
print(json.dumps({
    "apiVersion": "v1", "kind": "Secret", "type": "Opaque",
    "metadata": {"name": "billing-secrets",
                 "labels": {"app.kubernetes.io/name": "billing", "app.kubernetes.io/part-of": "oficina"}},
    "stringData": dados,
}))
PY
echo "==> billing-secrets aplicado em $NS"
