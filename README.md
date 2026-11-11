# oficina-billing — Billing Service

**Orçamento e pagamento** da oficina, com cobrança real pelo **Mercado Pago**. Um dos três microsserviços da Fase 4 do Tech Challenge (SOAT), ao lado do [OS Service](https://github.com/ProblemaTheu/oficina-app) e do [Execution Service](https://github.com/ProblemaTheu/oficina-execution).

## O que faz — e o que não faz

| Faz | Não faz |
|---|---|
| Recebe da saga `GerarOrcamento` e **gera e envia o orçamento** por e-mail ao cliente | Não muda o status da OS — **responde** ao orquestrador do OS Service |
| Expõe a aprovação/recusa do orçamento (cliente dono ou funcionário) | Não conhece fila de mecânicos nem estoque |
| **Cobra pelo Mercado Pago** (Checkout Pro, API de Orders), **confirma** o pagamento e **estorna** quando a saga compensa | Não lê o banco de nenhum outro serviço |

## Tecnologias

| Tecnologia | Por quê |
|---|---|
| Go 1.26, chi | Mesmo ecossistema dos outros serviços |
| **PostgreSQL** (banco próprio) | Dinheiro exige ACID, unicidade (um orçamento por OS, uma cobrança por order) e transição condicional de status. **Dinheiro em centavos (`bigint`)** — sem `float` em nenhum ponto |
| **Mercado Pago — API de Orders** | Validado no sandbox em 10/11: a API de Preferences cobra mas **não estorna** com conta de teste (401); com Orders, cobrança, consulta, cancelamento e estorno funcionam |
| Amazon SQS FIFO | Comandos e respostas da saga, ordenados por `saga_id`, DLQ após 5 tentativas |
| New Relic | APM, logs com `saga_id`/`correlation_id`, trace distribuído atravessando a fila |

## Arquitetura do serviço

```
              billing-commands.fifo                                os-saga-replies.fifo
 OS Service ──────────────────────────▶ Consumidor           Relay ─────────────────────▶ OS Service
                                            │                  ▲
 Cliente / funcionário ──JWT──▶ API ────────┼──▶ Casos de uso ─┴──▶ PostgreSQL (orçamento, pagamento, inbox, outbox)
                                            │         │
 Mercado Pago ──webhook (x-signature)──▶ API          ├──▶ Mercado Pago (criar · consultar · cancelar · estornar)
                    Reconciliador (a cada N s) ───────┘
```

| Fluxo | Detalhe |
|---|---|
| **`GerarOrcamento`** | inbox + orçamento + itens + `OrcamentoGerado`, numa transação. **Um orçamento por OS** (`os_id UNIQUE`): reentrega devolve o mesmo `orcamento_id`. Preço inválido → `OrcamentoFalhou`. E-mail depois do commit |
| **Aprovar** | Cria a **order** no MP **fora** da transação (idempotency key = orçamento) e grava aprovação + cobrança pendente + `OrcamentoAprovado{link_pagamento}`. MP fora → nada gravado, o cliente tenta de novo. Segundo clique devolve o mesmo link |
| **Recusar** | `gerado → recusado` + `OrcamentoRecusado` (a saga compensa) |
| **Confirmação** | **Reconciliador** (mecanismo principal — no sandbox o MP não notifica orders de teste) e **webhook** chamam a mesma função: `GET /v1/orders/{id}` decide. `processed` → `PagamentoConfirmado`. Tentativa recusada (`action_required`) → **cancela a order** e só então `PagamentoRecusado` — senão o cliente pagaria no mesmo link depois da compensação. Só age sobre cobrança pendente, então webhook e reconciliador juntos não duplicam |
| **Pagamento tardio** | Order paga com orçamento já cancelado → **estorno imediato**, sem `PagamentoConfirmado` |
| **`EstornarPagamento`** | Estorno total com idempotency key = `message_id`; `409 order_already_refunded` = sucesso. Sem pagamento aprovado → responde mesmo assim. MP fora → a mensagem volta para a fila (DLQ após 5) |
| **`CancelarOrcamento`** | Cancela o orçamento e fecha a order pendente no MP (o link para de aceitar pagamento) |

**Webhook (`POST /billing/webhooks/mercadopago`):** valida o `x-signature` (`ts` em segundos, janela de 5 min, manifesto `id:<data.id minúsculo>;request-id:<x-request-id>;ts:<ts>;`) → consulta a order → `200`. Se a consulta falhar: `500` sem gravar nada, para o reenvio do MP ser processado de verdade.

## API

Base `/billing/v1`, JWT do OS Service (`aud=oficina-api`). Cliente (`tipo=cliente`) só vê/decide o próprio orçamento; funcionário, qualquer um.

| Método | Rota | Efeito / resposta à saga |
|---|---|---|
| GET | `/orcamentos/{id}` · `/orcamentos?os_id=` | Orçamento com itens, total e, se houver, o pagamento com o link |
| POST | `/orcamentos/{id}/aprovar` | Cria a cobrança → `OrcamentoAprovado` · devolve `pagamento.link_pagamento` |
| POST | `/orcamentos/{id}/recusar` `{"motivo": "..."}` | `OrcamentoRecusado` |
| POST | `/billing/webhooks/mercadopago` | Público, autenticado por `x-signature` |
| GET | `/health/live`, `/health/ready` | Probes (ready faz ping no banco) |

Erros: `404` · `403` cliente de outra OS · `409` status não permite · `502` Mercado Pago indisponível.

## Como rodar

O ambiente completo fica no OS Service (`docker-compose.saga.yml`), que builda este repo da pasta irmã — inclusive o **stub do Mercado Pago** (`cmd/mp-stub`, mesma imagem):

```bash
cd oficina-app && cp .env.example .env
docker compose -f docker-compose.saga.yml --profile saga up -d --build
# OS :8080 · Billing :8081 · Execution :8082 · stub MP :8090 · Mailpit :8025
# pagar no stub: POST :8090/_stub/pagar {"order_id":"ORDTST...","resultado":"processed"|"failed"}
```

Variáveis: `DB_*`, `FILA_BILLING_URL` (consome), `FILA_RESPOSTAS_URL` (responde), `JWT_SECRET`, `MP_BASE_URL` (stub no compose; `https://api.mercadopago.com` em prod), `MP_ACCESS_TOKEN`, `MP_WEBHOOK_SECRET`, `MP_RECONCILIAR_INTERVALO` (padrão `10s`), `NOTIFIER=smtp` + `SMTP_HOST`/`SMTP_PORT`, `URL_PUBLICA` (links do e-mail).

O **stub** reproduz o comportamento validado no sandbox: recusa deixa a order em `action_required/waiting_retry`; order cancelada não aceita pagamento; segundo estorno → `409`; mesma idempotency key → mesmo estorno; notifica o webhook com `x-signature` válido. Nunca é implantado em produção.

## Testes e qualidade

```bash
./scripts/cobertura.sh          # unitários com -race + gate de 80%
./scripts/test-integration.sh   # repositórios contra PostgreSQL real (Docker)
```

**Cobertura unitária: 97,9%**, no critério do Sonar — exclui `cmd/` e os adaptadores Postgres (`internal/infra/repository`, `database`), cobertos pelos testes de integração. O cliente do Mercado Pago é testado **contra o stub por HTTP** (os dois validados juntos), incluindo retry só em rede/5xx.

| Check do CI | O que valida |
|---|---|
| `Build e testes` | build, testes com `-race`, **cobertura ≥ 80%** |
| `Lint` | golangci-lint (tags `integration`, `localstack`) |
| `gitleaks` | segredos no histórico |
| `Integração (PostgreSQL)` | migrations, orçamento único por OS, transição condicional (webhook × reconciliador), outbox, inbox |
| `Integração (LocalStack)` | mensageria contra SQS FIFO real |
| `SonarCloud` | quality gate (`sonar.qualitygate.wait=true`) |

## Ambientes

Só `prod` é implantado; homologação dispensada conforme orientação do fórum (mesma decisão da Fase 3).
