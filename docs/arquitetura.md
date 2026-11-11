# Arquitetura — Billing Service

> Visão do serviço dentro da saga da Fase 4. O desenho completo do sistema (os 3 serviços, AWS, catálogo de mensagens) está em [`oficina-app/docs/planejamentos/fase-4/arquitetura.md`](https://github.com/ProblemaTheu/oficina-app/blob/main/docs/planejamentos/fase-4/arquitetura.md); as decisões, nas [ADRs 009–016](https://github.com/ProblemaTheu/oficina-app/blob/main/docs/architecture-decisions.md).

## 1. Componentes

```mermaid
flowchart LR
    qcmd[["billing-commands.fifo"]] --> cons["Consumidor<br/>infra/mensageria"]
    cons --> h["Handlers de comando<br/>infra/comandos"]
    cli["Cliente / funcionário"] -- "JWT" --> api["API HTTP<br/>infra/http"]
    mp["Mercado Pago"] -- "webhook x-signature" --> api
    h & api --> uc["Casos de uso<br/>application/cobranca"]
    rec["Reconciliador<br/>(a cada 10 s)"] --> uc
    uc --> dom["Domínio<br/>domain/orcamento<br/>(Centavos, transições)"]
    uc -- "UnitOfWork" --> pg[("PostgreSQL billing<br/>orçamentos · pagamentos<br/>outbox · inbox")]
    uc -- "API de Orders" --> mp
    uc -. "após o commit" .-> smtp["SMTP<br/>(e-mail do orçamento)"]
    relay["Relay da outbox<br/>(líder por advisory lock)"] --> pg
    relay --> qr[["os-saga-replies.fifo"]]
```

| Pacote | Papel |
|---|---|
| `internal/domain/orcamento` | Orçamento, itens, pagamento e suas transições; `Centavos` (int64) com parse/format sem `float` |
| `internal/application/cobranca` | Casos de uso (`GerarOrcamento`, `Aprovar`, `Recusar`, `ConfirmarCobranca`, `Reconciliar`, `EstornarPagamento`, `CancelarOrcamento`) e portas |
| `internal/infra/repository` | PostgreSQL: repositórios, UnitOfWork (tx no contexto), outbox e inbox |
| `internal/infra/mercadopago` | Cliente da API de Orders (retry só em erro de rede/5xx), validação de `x-signature` e o **stub** usado no compose/BDD |
| `internal/infra/mensageria` | Envelope, consumer SQS, publisher e relay (mesmo código do OS Service — ADR-013) |
| `internal/infra/comandos` | Traduz comandos da saga em chamadas aos casos de uso; classifica erros (permanente → DLQ) |
| `internal/infra/notificacao` | E-mail do orçamento (SMTP ou log) |
| `cmd/api`, `cmd/mp-stub` | Bootstrap do serviço e do stub |

## 2. Mensagens

| Consome (`billing-commands.fifo`) | Efeito | Responde (`os-saga-replies.fifo`) |
|---|---|---|
| `GerarOrcamento` | Orçamento + itens (um por OS); e-mail com o link | `OrcamentoGerado` · `OrcamentoFalhou` |
| `EstornarPagamento` | Refund total no MP (idempotente) | `PagamentoEstornado` |
| `CancelarOrcamento` | Orçamento `cancelado`; cancela a order pendente no MP | `OrcamentoCancelado` |

| Produz por ação humana / Mercado Pago | Gatilho |
|---|---|
| `OrcamentoAprovado` | `POST /orcamentos/{id}/aprovar` (cria a order no MP) |
| `OrcamentoRecusado` | `POST /orcamentos/{id}/recusar` |
| `PagamentoConfirmado` | Order `processed` (reconciliador ou webhook) |
| `PagamentoRecusado` | Tentativa recusada — **depois** de cancelar a order |

Toda resposta é gravada na `outbox` na mesma transação do efeito; toda mensagem consumida é marcada em `mensagens_processadas` na mesma transação.

## 3. Confirmação do pagamento

```mermaid
sequenceDiagram
    autonumber
    participant C as Cliente
    participant B as Billing
    participant MP as Mercado Pago
    participant OS as OS Service (saga)

    C->>B: POST /orcamentos/{id}/aprovar
    B->>MP: POST /v1/orders (X-Idempotency-Key = orçamento)
    MP-->>B: order + checkout_url
    B->>B: TX: aprovado + pagamento pendente + outbox(OrcamentoAprovado)
    B-->>C: link_pagamento
    C->>MP: paga no Checkout Pro
    par webhook (produção)
        MP->>B: POST /billing/webhooks/mercadopago (x-signature)
        B->>B: valida HMAC
    and reconciliador (sempre)
        B->>B: a cada 10 s, pagamentos pendentes
    end
    B->>MP: GET /v1/orders/{id}
    alt processed
        B->>B: TX: pendente→aprovado (condicional) + outbox(PagamentoConfirmado)
    else action_required (tentativa recusada)
        B->>MP: POST /v1/orders/{id}/cancel
        B->>B: TX: pendente→recusado + outbox(PagamentoRecusado)
    end
    B-->>OS: resposta via os-saga-replies.fifo
```

A transição `pendente → *` é um `UPDATE … WHERE status = 'pendente'`: webhook e reconciliador podem rodar juntos sem duplicar a resposta.

## 4. Modelo de dados (PostgreSQL próprio)

```mermaid
erDiagram
    orcamentos ||--|{ itens_orcamento : contem
    orcamentos ||--o| pagamentos : cobra
    orcamentos {
        uuid id PK
        uuid os_id UK "um orçamento por OS"
        uuid saga_id
        varchar status "gerado|aprovado|recusado|cancelado"
        bigint valor_total_centavos
        text diagnostico
    }
    itens_orcamento {
        uuid id PK
        uuid orcamento_id FK
        varchar tipo "servico|peca"
        int quantidade
        bigint preco_unitario_centavos
    }
    pagamentos {
        uuid id PK
        uuid orcamento_id FK,UK
        text mp_order_id UK
        text checkout_url
        varchar status "pendente|aprovado|recusado|estornado|cancelado"
        bigint valor_centavos
        text mp_refund_id
    }
    notificacoes_mp {
        text x_request_id PK
        text data_id
    }
    outbox {
        uuid id PK
        bigint seq
        jsonb envelope
        timestamptz enviado_em
    }
    mensagens_processadas {
        uuid message_id PK
    }
```

Dinheiro sempre em centavos (`bigint`). Os dados do cliente (`cliente_nome`, `cliente_email`) chegam no comando `GerarOrcamento` — o Billing não consulta o banco do OS.

## 5. Limites assumidos

- **Réplica única:** o relay tem líder por *advisory lock*, mas o reconciliador ainda não; com uma réplica não há corrida (e, se houvesse, a transição condicional impede duplicar).
- **Estorno sempre total.**
- **Sem expiração automática** do orçamento ou do pagamento (evolução na RFC-006).
